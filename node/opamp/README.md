# @pydantic/opamp-client

An [OpAMP](https://github.com/open-telemetry/opamp-spec) client for Node.js (22 or later), over
plain HTTP or WebSocket.

Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
agent_disconnect, and the requests and responses themselves) is the shared OpAMP
client core, run in Node's own WebAssembly engine: nothing native to install. By default
the core speaks HTTP and WebSocket itself too ([core mode](#core-mode-and-platform-mode)), and this
package only gives it Node's TCP and TLS sockets, so the bytes on the wire are the same as every
other language's client here. In platform mode the package carries out the core's requests with
[`undici`](https://github.com/nodejs/undici)'s `fetch` and the
[`ws`](https://github.com/websockets/ws) package instead (SOCKS5 proxies through the
[`socks`](https://github.com/JoshGlazebrook/socks) package).

```ts
import { AgentDescriptionSchema, Client, create } from "@pydantic/opamp-client";

const client = Client.start(
  { endpoint: "wss://opamp.example.com/v1/opamp", instanceUid, headers: { Authorization: "Bearer ..." } },
  {
    description: create(AgentDescriptionSchema, {
      identifyingAttributes: [{ key: "service.name", value: { value: { case: "stringValue", value: "my-agent" } } }],
    }),
  },
  (n) => {
    if (n.kind === "remoteConfig") {
      // apply n.remoteConfig, then report with client.setRemoteConfigStatus(...)
    }
  },
);

// ...
await client.stop(); // sends agent_disconnect, then closes
```

The transport follows the endpoint's scheme: `http(s)://` polls, `ws(s)://` keeps one connection
open. Give the agent's status to `start` so the first message reports it in full.

Without `instanceUid` the core makes one (a UUIDv7, from the clock and Node's CSPRNG) and, with a
`stateFile`, keeps it across restarts. `client.instanceUid` is the one in use.

The client keeps no time of its own. The core gives every request, connection, closing
handshake, download and save its deadline (`requestTimeoutMs`, `connectTimeoutMs`,
`downloadTimeoutMs`, `stopTimeoutMs`) and cancels what passes it; the client then drops it at once
(a request or a download aborted, the download's file deleted, a connection closed, even one whose
handshake completes after the cancel) and reports nothing more about it. `stop()` sends the
goodbye if it can and resolves when the core says it has stopped, within `stopTimeoutMs` (5 s by
default); nothing the client started is still running then.

With `acceptPackages` (and a `stateFile`) the client downloads the files the server offers. It
follows no redirect itself: it writes whatever response comes to the file and reports its status,
and for a redirect the `Location` resolved by the WHATWG URL parser (`new URL(location, url)`).
The core decides whether to follow it (`docs/packages.md`, "Redirects").

The client doesn't keep Node.js running (its timers and connection are unref'd, as the other
clients run on daemon threads): the application's own work does. A program whose only work is
the client keeps itself going, and calls `stop()` when it ends: from `stop()` until the client
has stopped, the client does hold Node.js, so the goodbye is sent before the program exits. (A
connection holds Node.js only while it is being opened and until the server first answers on it;
in platform mode, while a request is in flight.)

## TLS and proxies

`tls` and `proxy` apply to everything the client sends, in both modes: plain HTTP, WebSocket and
package downloads.

```ts
import { readFileSync } from "node:fs";

const client = Client.start(
  {
    endpoint: "wss://opamp.internal:4320/v1/opamp",
    tls: {
      ca: readFileSync("/etc/agent/ca.pem", "utf8"), // trust this CA, not Node's
      cert: readFileSync("/etc/agent/client.pem", "utf8"), // present this certificate
      key: readFileSync("/etc/agent/client-key.pem", "utf8"),
    },
    proxy: { url: "http://user:password@proxy.internal:3128" },
  },
  status,
);
```

- `tls.ca`: the CA certificates (PEM) to verify the server with. They replace the CAs Node trusts
  by default. Set `includeSystemCas: true` to trust both. Without `ca`, Node's defaults apply.
- `tls.cert` and `tls.key`: a client certificate and its private key (PEM, the key not
  encrypted), presented in every handshake. Give both or neither.
- `tls.minVersion` and `tls.maxVersion`: `"TLSv1.2"` or `"TLSv1.3"`. The client never uses
  anything older than TLS 1.2.
- `proxy.url`: `http://`, `https://`, `socks5://` or `socks5h://`, with `user:password@` when the
  proxy needs them (percent-encode special characters); `host:port` with no scheme is an
  `http://` proxy, in both modes, as the core reads a proxy the server offers and as curl and
  Go read `HTTP_PROXY`. An HTTP proxy gets a CONNECT for every connection, `http://` endpoints
  included; `proxy.headers` go with each CONNECT. The proxy resolves the server's name, for
  `socks5://` and `socks5h://` alike, as opamp-go, the OpenTelemetry Collector and browsers do
  (curl, unlike them, resolves it itself for `socks5://`). `socks4://`, `socks4a://` and any
  other scheme throw at `start`: the client never goes direct instead. An `https://` proxy's
  certificate is checked against `tls.ca` too.
- `proxy.resolveLocally`: resolve the server's name on this host and give a SOCKS5 proxy its
  address, for a proxy that can't resolve the names the agent can. Default `false`. SOCKS5 only:
  with an `http://` or `https://` proxy it throws. Only the application sets it.

Without `proxy`, the proxy comes from the environment, by the rule every SDK's core mode shares
(`tests/shell-tables/README.md`, its proxy rule, put in place by
`scripts/fetch_core.py fetch`):
`HTTP_PROXY` for `http://` and `ws://`, `HTTPS_PROXY` for `https://` and `wss://`, else
`ALL_PROXY`; the upper-case name first; `NO_PROXY` names (and their subdomains), `*`, addresses
and CIDR networks; nothing under CGI (`REQUEST_METHOD` set: there `HTTP_PROXY` may be a request's
`Proxy` header, httpoxy). They are read whatever `NODE_USE_ENV_PROXY` says. The value goes to
the core as it is (trimmed): a scheme it can't use fails the connection rather than going direct,
at connection time rather than at `start`. `proxy: { resolveLocally: true }` without a `url`
applies to a SOCKS5 proxy the environment names. [platform mode](#core-mode-and-platform-mode) reads the
environment by Node's convention instead.

`proxy: false` goes direct, whatever the environment says.

The client checks these settings at `start`, and throws if it can't use them.

## The state file

With a state file (`stateFile`) the client keeps what it needs across restarts: accepted
connection settings, the instance UID, the last remote config, telemetry settings and packages.
It holds secrets: the credentials of connection settings the server offered (bearer tokens in
`Authorization` headers) and the private keys of telemetry settings' client certificates.
The client writes it readable by the agent's user alone (mode 0600; downloaded packages go in a
0700 directory beside it, `<state file>.downloads`). On Windows Node sets no access-control
list: the file takes its directory's permissions.
Keep it on a local disk, in a directory only the agent's user can read: not on a shared or
network path, and not in backups or images copied anywhere less protected.

Saves are off the event loop (in both modes), one at a time, in the order the core asks for
them: a new temporary file (`<state file>.tmp`, never one left behind), written, synced, renamed
over the state file, and the directory synced; only then does the core hear it is saved. A slow
disk holds up neither heartbeats nor `stop`, which doesn't wait for a save already writing; a
client started next on the same state file in the same process waits for it (and for a stopped
client's downloads to delete their files) before it reads the file. A download's file is made
new, never written over one already there. With `OPAMP_CORE_TAP` set, every file operation is
recorded in the tap too, for the `opamp-tap` checker to check against a disk that can crash. A
tap holds the headers sent (the enrollment key) and the credentials saved, so a new one is the
user's only.

## Core mode and platform mode

`mode` says who speaks HTTP and WebSocket. The client runs in **core mode** unless the
application asks for **platform mode**: by naming it in the mode option, or by handing the client
an HTTP client of its own, where the SDK takes one (an application that brings its own client has
asked for the platform's stack). An option only one mode honours is never silently ignored: it
selects that mode, or the client refuses to start with an error that says what to set instead.

Here platform mode is `mode: "platform"`. This client takes no HTTP client of the application's, so
the mode option is the only way to choose it. `spliceDownloads: false` is core mode's: with
`mode: "platform"` it throws.

In core mode (`mode: "core"`, or no `mode`) the core speaks HTTP/1.1, WebSocket, CONNECT and
SOCKS5 itself, over Node's TCP and TLS sockets: Node opens connections,
starts TLS with the options above, picks the proxy, and writes package files.

- Package files are written off the event loop, in order, and hashed over exactly the bytes
  written; a file is synced before the core hears it is complete. A file the core discards or
  cancels while a write is in flight is deleted once that write is over, and nothing more is said
  about it. With `spliceDownloads` (the default) body bytes go from the socket to the file without
  passing through the core, wherever the response says how many there are.
- A socket that brings a file's bytes faster than they can be written is paused while more than
  4 MiB wait, and resumed at 1 MiB.
- The core has no "written" signal from a stream, and needs none: it writes one request or
  message at a time. What a peer could make the client buffer is answers to what it sends while
  it reads nothing (WebSocket pings, say). So while a socket's write buffer is full the client
  reads nothing more from it, and a write that finds more than 16 MiB still unsent fails the
  stream (the core reconnects, as for any lost connection).

Choose platform mode (`mode: "platform"`) when the connection must be made by Node's own stack. What
differs there:

- The environment's proxies are Node's convention, not the shared rule. With
  `NODE_USE_ENV_PROXY=1` or `--use-env-proxy` (Node 22.21 or later, 24.5 or later), the client
  takes the proxy from `HTTP_PROXY` for `http://` and `ws://`, and from `HTTPS_PROXY` for
  `https://` and `wss://`. Lower-case names win over upper-case ones. They are read as
  `proxy.url` is (a scheme the client can't use throws at `start`), and
  `proxy: { resolveLocally: true }` without a `url` applies to them. `NO_PROXY` sends hosts
  direct: `*`, a host or `host:port`, a suffix (`.example.com` or `*.example.com`), or an IPv4
  range (`10.0.0.1-10.0.0.9`). Node 24.0 to 24.4 have only `NODE_USE_ENV_PROXY`, with any value.
  Otherwise the client goes direct: without the switch, platform mode reads no proxy from the
  environment at all, and `ALL_PROXY` never. Under CGI the environment's proxies are ignored
  here too.
- HTTP and WebSocket are undici's and ws's: their connection pooling and their headers' order
  and case, not the core's bytes.

## Development

```
npm ci
npm test                    # compile, then node --test
../../scripts/gen_core.py   # after changing the core: rebuilds core/opamp_core.wasm and src/gen
```

The conformance suite runs this client through `conformance/drivers/node`.

What the client promises around its callback (when it is called, one at a time and never before `start` returns, what a callback that throws does, which calls are allowed from the callback and from other threads, stop from the callback) is [the SDK contract](../../docs/sdk-contract.md), the same for every SDK here. The precise rules, around the core too, are the shell contract, `spec/shell-contract.md` (put in place by `scripts/fetch_core.py fetch`; it lists the breaks known today). The callback model test and the adversary harness in this client's tests check it, each in platform mode and in core mode.
