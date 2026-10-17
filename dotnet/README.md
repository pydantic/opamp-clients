# Pydantic.OpAmp.Client (.NET)

An [OpAMP](https://github.com/open-telemetry/opamp-spec) client for .NET 8 and later, over plain
HTTP or WebSocket.

Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
agent_disconnect, and the requests and responses themselves) is the shared core, run
natively: a library built from the same source as the WebAssembly module the Go, Node.js and Java clients run, called through `LibraryImport`, and
held to that module byte for byte (`NativeCoreTests`).
By default the core speaks HTTP and WebSocket itself too ([Rust mode](#rust-mode-and-net-mode)),
and this library only gives it .NET's sockets and `SslStream`, so the bytes on the wire are the
same as every other language's client here. In .NET mode the library carries out the core's
requests with `HttpClient` and `ClientWebSocket` instead.

To put automatic instrumentation under a server without writing code, use the plugin next to it,
`Pydantic.OpAmp.AutoInstrumentation` ([docs/dotnet-otel.md](../docs/dotnet-otel.md)). An SDK set up
in code takes one call, `services.AddOpenTelemetry().WithOpAmp()`, from
`Pydantic.OpAmp.Extensions.Hosting` (the same document). To decide
what an SDK does with the server's settings by hand, `Pydantic.OpAmp.Client.Sdk.SdkSettings`
runs the shared SDK settings layer ([docs/sdk-settings.md](../docs/sdk-settings.md)).

```csharp
var client = OpAmpClient.Start(
    new OpAmpClientOptions
    {
        Endpoint = new Uri("wss://opamp.example.com/v1/opamp"),
        Headers = new Dictionary<string, string> { ["Authorization"] = "Bearer ..." },
    },
    new OpAmpStatus { Description = description },
    (c, n) =>
    {
        if (n is OpAmpNotification.RemoteConfig r)
        {
            // apply r.Config, then c.SetRemoteConfigStatus(...)
        }
    });
// ...
await client.StopAsync(); // sends agent_disconnect, then closes
```

The transport follows the endpoint's scheme, as the core reads it: `http(s)://` polls, `ws(s)://`
keeps one connection open. Give the agent's status to `Start` so the first message reports it in
full. Notifications come on the client's loop, never before `Start` returns, with the client to
answer through: a remote config saved before a restart is handed over as the client starts, so
answer through the `c` the callback is given rather than the variable `Start` assigns.

With no `InstanceUid`, the core makes one (a UUIDv7, from the clock and 32 bytes of the
operating system's randomness) and, with a state file, keeps it across restarts.
`client.InstanceUid` says which UID is in use.

The client keeps no time of its own. How long a request, a WebSocket connection being opened, a
closing handshake, a download and stop may take is the core's (`RequestTimeout`,
`ConnectTimeout`, `StopTimeout`, `DownloadTimeout`): when one runs out, the core cancels the effect
and the client aborts it at once, and reports nothing more about it. `StopAsync` ends when the
core says it has stopped (within `StopTimeout`, 5 s by default, after waiting for the goodbye and
the closing handshake); then everything still open is closed, and work the core cancelled is let
finish within that time again. A save already writing at stop finishes, whole or not at all, and a
client started on the same state file waits for it.

Package downloads follow no redirect. Whatever the status, the body goes to the file, hashed,
and the core is told the status and, for a redirect, the `Location` resolved with
`new Uri(requested, location)` and its origin: the core decides whether to follow it.

Set `OPAMP_CORE_TAP=<path>` to record every call into the core (for `opamp-tap`): the first
core in the process writes `<path>`, each later one `<path>.<pid>.<n>`. A tap holds the headers
sent (the enrollment key) and the credentials saved, so a new one is the user's only.

The native core comes in its own package, `Pydantic.OpAmp.Core.Native`, which this one depends
on: the library alone for Linux (glibc and musl, so Alpine too), macOS and Windows, on x64 and
arm64 (`runtimes/<rid>/native`), about 2 MB each, with no assembly and no source. This package,
`Pydantic.OpAmp.Client`, holds only the client's assembly.

## TLS and proxies

`Tls` sets the CAs to trust, a client certificate and the TLS versions. `Proxy` sets a proxy.
Both apply to every connection the client makes, in both modes: requests, the WebSocket and
package downloads.

```csharp
var options = new OpAmpClientOptions
{
    Endpoint = new Uri("wss://opamp.example.com/v1/opamp"),
    Tls = new OpAmpTlsOptions
    {
        CaPem = File.ReadAllText("ca.pem"),
        ClientCertPem = File.ReadAllText("agent.pem"),
        ClientKeyPem = File.ReadAllText("agent-key.pem"),
    },
    Proxy = new Uri("http://user:password@proxy.example.com:3128"),
};
```

- `CaPem` holds one or more CA certificates. They replace the system's CAs. Set
  `IncludeSystemCaCerts` to trust both. Without `CaPem` the system's CAs are used.
- The server's certificate is always checked, and so is its name (SNI unless the host is an
  address). There is no way to turn that off.
- `ClientCertPem` and `ClientKeyPem` go together. The key is PKCS #8, PKCS #1 or SEC 1, not
  encrypted. Certificates after the first in `ClientCertPem` are sent as its chain.
- The client uses TLS 1.2 and 1.3. `MinVersion` and `MaxVersion` narrow that to one of them.
  Cipher suites can't be chosen: .NET doesn't support that on Windows.
- `Proxy` is `http://`, `https://`, `socks5://` or `socks5h://`. Credentials go in the URL,
  percent-encoded. An HTTP proxy always gets a CONNECT tunnel, plain `http://` included. The
  client can't add headers of its own to the CONNECT request. The TLS settings apply to an
  `https://` proxy too.
- A SOCKS5 proxy is given the server's host name and resolves it, with `socks5://` and
  `socks5h://` alike. Unlike curl, where `socks5://` resolves the name locally.
  `ProxyUseLocalResolver` has the client resolve the name instead and give the proxy an address;
  with an HTTP proxy it is an error.
- `socks4://` and `socks4a://` are refused, and so is any other scheme: a proxy the client can't
  use is an error, never a direct connection.
- Without `Proxy` the proxy comes from the environment by the rule every SDK's Rust mode follows:
  [tests/shell-tables/README.md](../tests/shell-tables/README.md), "Rust mode's proxy rule"
  (`HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` and `NO_PROXY`, the upper-case name first; NO_PROXY
  names, addresses and CIDR networks; nothing under CGI). `ProxyUseLocalResolver` then applies
  to a SOCKS5 proxy the environment names. It does not read the system's proxy settings on
  Windows and macOS, nor `HttpClient.DefaultProxy`: those are
  [.NET mode](#rust-mode-and-net-mode)'s. Set `DirectConnection` to go direct instead. It can't
  be combined with `Proxy`.
- Package downloads use the same settings. With `CaPem` set, a download from another server
  (where a redirect points, say) needs that server's CA in `CaPem`, or `IncludeSystemCaCerts`.
- Bad settings throw `ArgumentException` from `Start`.

.NET checks the server's certificate once the TLS handshake is done, not during it. A server
the client can't verify sees the handshake complete, then the connection close. The client
sends it nothing: no request, no upgrade, not a byte of application data. The conformance
suite checks both: `rejects_untrusted_server` passes, and `refuses_in_handshake` (a SHOULD)
warns.

## Rust mode and .NET mode

`Mode` says who speaks HTTP and WebSocket. The client runs in **Rust mode** unless the
application asks for **platform mode**: by naming it in the mode option, or by handing the client
an HTTP client of its own, where the SDK takes one (an application that brings its own client has
asked for the platform's stack). An option only one mode honours is never silently ignored: it
selects that mode, or the client refuses to start with an error that says what to set instead.

Here platform mode is `Mode = OpAmpMode.DotNet`, and setting `HttpClient` selects it too. `Start`
throws for `Mode = OpAmpMode.Rust` with an `HttpClient`, and in .NET mode for
`ProxyUseLocalResolver` and `RustSplice = false`.

In Rust mode (`Mode = OpAmpMode.Rust`, or no `Mode`) the shared core speaks HTTP/1.1, WebSocket,
CONNECT and SOCKS5 itself (in the same native library). .NET then only
opens TCP connections (`Socket`, TCP_NODELAY), starts TLS on them (`SslStream`, TLS inside TLS
for an `https://` proxy), resolves names when asked to, answers which proxy to use, and writes
package files; every byte on the wire is the core's.

- `RustSplice` (default true): package bodies of 16 KiB or more go from the socket to the file
  without passing through the core.
- A connection the core closes keeps 2 s at most to flush what was written and hear the server
  end it; `StopAsync` resets whatever is still open, so it still ends within `StopTimeout`.

Choose .NET mode to pass your own `HttpClient`, or when the connection must be made by .NET's
HTTP stack: with the system's proxy settings, say.

```csharp
var options = new OpAmpClientOptions
{
    Endpoint = new Uri("https://opamp.example.com/v1/opamp"),
    Mode = OpAmpMode.DotNet,
};
```

What differs there:

- Without `Proxy` the client uses the system's proxy settings (`HttpClient.DefaultProxy`:
  `HTTPS_PROXY` and the like, and on Windows and macOS the system's own), for requests, the
  WebSocket and downloads alike. `DirectConnection` goes direct instead.
- The environment's proxy URLs (`http_proxy`, `https_proxy`, `all_proxy`, in either case) are
  held to the same schemes, as is a `WebProxy` the application sets as `HttpClient.DefaultProxy`.
  One exception: .NET doesn't read `socks5h://` there (it would connect directly), so it is an
  error too. Write it as `socks5://`, which has the proxy resolve names just the same, or set
  `Proxy`. The system's own proxy settings on Windows and macOS aren't checked.
- An HTTP proxy gets plain `http://` requests in absolute form and a CONNECT tunnel for
  everything else (`https://`, `ws://`, `wss://`).
- `ProxyUseLocalResolver` is an error: .NET's SOCKS5 always sends the name, and an HTTP proxy is
  always given it.
- You can pass your own `HttpClient`. It then carries everything, the WebSocket and downloads
  included, so set TLS and the proxy on its handler. It can't be combined with `Tls`, `Proxy` or
  `DirectConnection`.

## The state file

With a state file (`StateFile`) the client keeps what it needs across restarts: accepted
connection settings, the instance UID, the last remote config, telemetry settings and packages.
It holds secrets: the credentials of connection settings the server offered (bearer tokens in
`Authorization` headers) and the private keys of telemetry settings' client certificates.
The client writes it for the agent's user alone: mode 0600 on Linux and macOS, and on Windows
an access-control list that allows only that user, with nothing inherited from the directory.
Package files downloaded beside it (`<state file>.downloads`, named for the client that
downloads them) are protected the same way. Saves run beside the client's loop, one at a time.
Keep it on a local disk, in a directory only the agent's user can read: not on a shared or
network path, and not in backups or images copied anywhere less protected.

## Development

```
../scripts/fetch_core.py fetch             # first: the core's release (the native library in target/release, the modules)
dotnet test Pydantic.OpAmp.Client.Tests
../scripts/gen_core.py                      # after the core's .proto files change: regenerates the types
../scripts/native_dist.py                   # the package, with every platform's native core (CI's native job)
```

The conformance suite runs this client through `conformance/drivers/dotnet-core`, in both modes:
`dotnet-core` (.NET mode) and `dotnet-core-rust` (Rust mode, `OPAMP_DOTNET_MODE=rust`). The tests
run Rust mode over an in-memory network (`MemNet`, with `MemServer` at its far end) as well as
real sockets.

What the client promises around its callback (when it is called, one at a time and never before `start` returns, what a callback that throws does, which calls are allowed from the callback and from other threads, stop from the callback) is [the SDK contract](../docs/sdk-contract.md), the same for every SDK here. The precise rules, around the core too, are [the shell contract](../spec/shell-contract.md) (it lists the breaks known today). The callback model test and the adversary harness in this client's tests check it.
