# pydantic-opamp-client

An [OpAMP](https://github.com/open-telemetry/opamp-spec) client for Python (asyncio), over plain
HTTP or WebSocket, with TLS and through HTTP or SOCKS5 proxies.

Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
agent_disconnect, and the requests and responses themselves) is the shared core,
run natively: a native library built from the same source as
the WebAssembly module the Go, Node.js and Java clients run, and held to it byte for byte
(`tests/test_native.py`). It comes in its own package, `pydantic-opamp-core` (`core/`: a wheel per
platform holding the library alone, for Linux (glibc and musl), macOS and Windows on x64 and
arm64), and this package, pure Python, loads it through its C interface with `ctypes`
(`opamp_client._native`), on every CPython from 3.11, free-threaded included. The core ships
compiled only: there is no source distribution.
By default the core speaks HTTP and WebSocket itself too ([Rust mode](#rust-mode-and-python-mode)),
and Python only moves its bytes, so the bytes on the wire are the same as every other language's
client here. In Python mode this package carries out the core's requests with
[aiohttp](https://docs.aiohttp.org) instead.

```python
from opamp_client import Client, Config, Status, anyvalue_pb2, opamp_pb2

async def main() -> None:
    description = opamp_pb2.AgentDescription(identifying_attributes=[
        anyvalue_pb2.KeyValue(key="service.name", value=anyvalue_pb2.AnyValue(string_value="my-agent"))])

    def notify(n):
        if n.kind == "remote_config":
            ...  # apply n.remote_config, then client.set_remote_config_status(...)

    client = Client.start(
        Config(endpoint="wss://opamp.example.com/v1/opamp",
               headers={"Authorization": "Bearer ..."}),
        Status(description=description),
        notify,
    )
    ...
    await client.stop()  # sends agent_disconnect, then closes
```

The transport follows the endpoint's scheme: `http(s)://` polls, `ws(s)://` keeps one connection
open. Give the agent's status to `start` so the first message reports it in full.

Without `instance_uid` the core makes one (a UUIDv7, from the clock and the operating system's
randomness) and, with a `state_file`, keeps it across restarts. `client.instance_uid` is the one
in use.

The client keeps no time of its own. The core gives every request, connection, closing
handshake, download and save its deadline (`request_timeout_ms`, `connect_timeout_ms`,
`download_timeout_ms`, `stop_timeout_ms`) and cancels what passes it; the client then drops it at
once and reports nothing more about it. `stop()` sends the goodbye if it can and returns when the
core says it has stopped, within `stop_timeout_ms` (5 s by default); nothing the client started is
still running then. Saves run on a thread beside the event loop, one at a time; a client starting
on the same state file reads it once they have landed.

The client's methods may be called from any thread: a call from another thread is carried out on
the event loop. `stop()` too: awaited on another thread's loop, it is carried out on the
client's.

## Packages

With `accept_packages` (and a `state_file`) the client downloads the files the server offers.
It follows no redirect itself: it writes whatever response comes to the file and reports its
status, and for a redirect the `Location` resolved with `urllib.parse.urljoin`. The core decides
whether to follow it (`docs/packages.md`, "Redirects").

## TLS and proxies

`tls` and `proxy` in `Config` say how to reach the server. They apply to every request, every
WebSocket and every package download, in both modes.

```python
from opamp_client import TLS, Proxy

Config(
    endpoint="wss://opamp.internal:4320/v1/opamp",
    tls=TLS(ca_file="/etc/agent/ca.pem",          # trust this CA instead of the system's
            cert_file="/etc/agent/agent.pem",     # present this certificate (mutual TLS)
            key_file="/etc/agent/agent-key.pem"),
    proxy=Proxy("http://agent:s3cret@proxy.internal:3128", headers={"X-Tenant": "t1"}),
)
```

Without `tls` the client trusts the system's CAs and uses TLS 1.2 or later. With `ca_file` or
`ca_pem` it trusts those CAs alone; add `system_cas=True` to trust the system's as well.
`min_version` and `max_version` take `ssl.TLSVersion` values; below TLS 1.2 is refused (for
either), and so is a `min_version` above the `max_version`. The
same CAs check an `https://` proxy's certificate.

The certificate and key can be files (`cert_file`, `key_file`) or PEM in memory (`cert_pem`,
`key_pem`). Python's `ssl` module loads them from files only. So the client writes PEM to a
new temporary file, readable by the agent's user alone (mode 0600), loads it and deletes it at
once. `key_password` unlocks an encrypted key; without it an encrypted key is refused (never
asked for on the terminal).

`tls` also takes an `ssl.SSLContext`, used as it is. Use one for anything `TLS` doesn't cover,
such as the operating system's own trust store through
[truststore](https://pypi.org/project/truststore/):
`tls=truststore.SSLContext(ssl.PROTOCOL_TLS_CLIENT)`.

The system's CAs are OpenSSL's. Python from python.org on macOS has none until you run its
`Install Certificates.command`. Without them, give the CAs yourself (`ca_file=certifi.where()`,
say) or use truststore.

`proxy` is a `Proxy` or just its URL:

- `http://host:port`: an HTTP proxy. Every connection is a CONNECT tunnel, for `http://` and
  `ws://` endpoints too. `headers` go with each CONNECT.
- `https://host:port`: the same, with TLS to the proxy.
- `socks5://host:port`: a SOCKS5 proxy. The proxy resolves the server's name: the client
  sends it the name, not an address. curl and requests resolve `socks5://` names locally; this
  client, like browsers, opamp-go and the OpenTelemetry Collector, does not.
- `socks5h://host:port`: the same.

`Proxy(url, resolve_locally=True)` makes the client resolve the name itself and send the SOCKS5
proxy the address, for `socks5://` and `socks5h://` alike (for a proxy that can't resolve
names). It is an error with an HTTP proxy. SOCKS4 (`socks4://`, `socks4a://`) and any other
scheme are refused.

Credentials go in the URL as `user:password@`, percent-encoded. An HTTP proxy gets them as
Basic credentials; a SOCKS5 proxy as its username and password. They never appear in errors.

A proxy you configure is used for every connection, whatever the host. `NO_PROXY` doesn't
apply to it.

Without `proxy` the client uses the environment's proxies, by the rule every SDK's Rust mode
shares ([tests/shell-tables](../tests/shell-tables/README.md), "Rust mode's proxy rule"):
`HTTP_PROXY` is for `http://` and `ws://` URLs, `HTTPS_PROXY` for `https://` and `wss://`,
`ALL_PROXY` for either when its own is unset; the upper-case name first. `NO_PROXY` lists hosts
to reach directly (`*` for all): names (and the names under them), IP addresses and networks in
CIDR notation (`10.0.0.0/8`), IPv6 ones too, but no `name:port` entries. Under CGI
(`REQUEST_METHOD` set) no variable is read at all. A value may be any proxy URL above,
`socks5://` included (the proxy resolves names, as above); a bare `host:port` is an HTTP proxy.
The value goes to the core as it is (trimmed): a scheme it can't use fails the connection rather
than going direct, at connection time rather than at start. The client reads the environment
once, at start. It matches each URL on its own: the server's, and each package download's. It
doesn't read the operating system's proxy settings, or `.netrc`.
[Python mode](#rust-mode-and-python-mode) reads the environment as `urllib` does instead.

`proxy=Proxy.DIRECT` goes straight to the server, whatever the environment says.

Settings that can't work (a missing file, a bad key, a proxy URL without a host) fail in
`Client.start`, before anything is sent.

## The state file

With a state file (`state_file`) the client keeps what it needs across restarts: accepted
connection settings, the instance UID, the last remote config, telemetry settings and packages.
It holds secrets: the credentials of connection settings the server offered (bearer tokens in
`Authorization` headers) and the private keys of telemetry settings' client certificates.
The client writes it readable by the agent's user alone (mode 0600; downloaded packages go in a
0700 directory beside it, `<state file>.downloads`). On Windows Python sets no access-control
list: the file takes its directory's permissions.
Keep it on a local disk, in a directory only the agent's user can read: not on a shared or
network path, and not in backups or images copied anywhere less protected.

## Rust mode and Python mode

`Config.mode` says who speaks HTTP and WebSocket. The client runs in **Rust mode** unless the
application asks for **platform mode**: by naming it in the mode option, or by handing the client
an HTTP client of its own, where the SDK takes one (an application that brings its own client has
asked for the platform's stack). An option only one mode honours is never silently ignored: it
selects that mode, or the client refuses to start with an error that says what to set instead.

Here platform mode is `Config(mode="python")`. This client takes no HTTP client of the
application's, so the mode option is the only way to choose it. `rust_splice=False` is Rust
mode's: with `mode="python"` it is a `ValueError`.

In Rust mode (`mode="rust"`, the default) the core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5
itself (the core's own HTTP stack, in the same library), over asyncio's TCP connections and
TLS. Python opens connections, starts TLS with the `tls` settings above (an `https://` proxy's,
then the server's inside it), resolves the server's name for a SOCKS5 proxy with
`resolve_locally`, picks the proxy, runs the core's timer and writes package files. Every byte on
the wire is the core's, and the core keeps every deadline: the connect, each handshake, each
request, stop. aiohttp isn't used.

- One registry holds everything under way: each stream, the task connecting it or starting its
  TLS, each name being resolved, each file's writes and each timer. When the core closes a stream
  (or gives up on it), it goes at once; a connection or TLS handshake that completes after that
  is aborted as it arrives, and the core hears nothing of it. All of `stop()` takes one stop
  timeout (5 s): the core's own stop, then the registry letting everything go, waiting for a file
  write in flight only for what is left of it.
- Package files are written in a worker thread, in order, and hashed over exactly the bytes
  written; a file is synced before the core hears it is complete. A file the core discards or
  cancels while a write is in flight is deleted once that write is over, and nothing more is said
  about it. With `rust_splice` (the default) body bytes go from the socket to the file without
  passing through the core, wherever the response says how many there are.
- A connection that brings a file's bytes faster than they can be written stops reading while
  more than 4 MiB wait, and reads again at 1 MiB.
- The core has no "written" signal from a stream, and needs none: it writes one request or
  message at a time. What a peer could make the client buffer is answers to what it sends while
  it reads nothing (WebSocket pings, say). So while a connection's write buffer is over asyncio's
  high-water mark the client reads nothing more from it, and a write that finds more than 16 MiB
  still unsent fails the stream (the core reconnects, as for any lost connection).

Choose Python mode (`mode="python"`) when the connection must be made by aiohttp. What differs
there:

- The environment's proxies are read as Python's `urllib` reads them, not by the shared rule:
  lower-case names work too, and win over upper-case ones; `NO_PROXY` also takes `name:port`
  entries; under CGI only the upper-case `HTTP_PROXY` is ignored. A proxy the environment names
  that can't work (SOCKS4, a URL without a host) fails in `Client.start`.
- HTTP and WebSocket are aiohttp's: its connection pooling and its headers' order and case, not
  the core's bytes.

The conformance suite runs both: `python-core` (Python mode) and `python-core-rust` (Rust mode;
`OPAMP_PYTHON_MODE=rust` for the driver).

## Development

```
../scripts/fetch_core.py fetch  # the core's library and modules, as the lock pins them
uv run pytest                   # the tests (uv builds core/ from the fetched library)
../scripts/gen_core.py          # after a new core: regenerates _proto
../scripts/native_dist.py       # the core's wheels for every platform, and this one (CI's native job)
```

With `OPAMP_CORE_TAP=<path>` set, every call into the core is appended to a file for the effects
checker (`target/release/opamp-tap <tap files...>`, which `fetch_core.py fetch` puts there).

The conformance suite runs this client through `conformance/drivers/python-core`.

What the client promises around its callback (when it is called, one at a time and never before `start` returns, what a callback that throws does, which calls are allowed from the callback and from other threads, stop from the callback) is [the SDK contract](../docs/sdk-contract.md), the same for every SDK here. The precise rules, around the core too, are [the shell contract](../spec/shell-contract.md) (it lists the breaks known today). The callback model test and the adversary harness in this client's tests check it, each in Python mode and in Rust mode.
