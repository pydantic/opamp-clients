# opamp-client (Java)

An [OpAMP](https://github.com/open-telemetry/opamp-spec) client for Java 17 and later, over plain
HTTP or WebSocket, with TLS and through HTTP or SOCKS5 proxies.

Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
agent_disconnect, and the requests and responses themselves) is the shared core, a
WebAssembly module run with [Chicory](https://chicory.dev), a runtime written
in plain Java: nothing native. The build compiles the module to JVM classes, so the jar carries
classes rather than WebAssembly and nothing is interpreted or compiled when the client runs. By
default the core speaks HTTP and WebSocket itself too ([Rust mode](#rust-mode-and-java-mode)), and
this library only gives it the JDK's sockets and TLS, so the bytes on the wire are the same as
every other language's client here. In Java mode the library carries out the core's requests with
the JDK's own `java.net.http.HttpClient` instead. Either way its only dependencies are Chicory and
protobuf-javalite.

```java
CompletableFuture<OpampClient> started = new CompletableFuture<>();
OpampClient client = OpampClient.start(
    OpampClient.Config.builder("wss://opamp.example.com/v1/opamp", instanceUid)
        .header("Authorization", "Bearer ...")
        .build(),
    OpampClient.Status.of(description),
    n -> {
      if (n instanceof Notification.RemoteConfig r) {
        // apply r.config(), then started.join().setRemoteConfigStatus(...)
      }
    });
started.complete(client);
// ...
client.stop().join(); // sends agent_disconnect; done when the core says it has stopped
```

The transport follows the endpoint's scheme: `http(s)://` polls, `ws(s)://` keeps one connection
open. Give the agent's status to `start` so the first message reports it in full. Notifications
come on the client's thread, the first after `start` has returned. The callback can't name
`client` (Java doesn't let a lambda use the variable it initialises), so it takes the client from
a future completed once `start` returns.

`Config.builder(endpoint)`, with no instance UID, lets the core make one: a UUIDv7 from the clock
and the platform's `SecureRandom`, kept across restarts with a state file. `client.instanceUid()`
says which UID is in use.

## Timeouts and stop

How long anything may take is the core's: a request (`requestTimeout`, 30 s), a WebSocket to open
(`connectTimeout`, 30 s), a download (`downloadTimeout`, 10 minutes), and stop with its goodbye and
closing handshake (`stopTimeout`, 5 s). When one passes, the core cancels the effect and the
client aborts it at once; the client has no timeouts of its own. `stop()` completes when the core
says it has stopped: nothing the client started is still running then. A save already writing
finishes whole; a client started on the same state file waits for it.

## Downloads

Package files are downloaded beside the state file, through the same TLS settings and proxy as
OpAMP, and follow no redirect: whatever the status, the body goes to the file and the response to
the core, a redirect with its `Location` resolved by `java.net.URI` and that URL's origin. The core
decides whether to follow it (`docs/packages.md`, "Redirects").

## TLS and proxies

`https://` and `wss://` endpoints use TLS 1.2 or 1.3 and trust the system's CAs (the JVM's default
`SSLContext`). `tls(...)` changes that: the CAs to trust, a client certificate for servers that
ask for one, and the versions allowed. `proxy(...)` sends everything through a proxy. Both apply
to OpAMP over HTTP and WebSocket and to package downloads alike, in both modes.

```java
OpampClient.Config.builder("wss://opamp.example.com/v1/opamp")
    .tls(Tls.builder()
        .trustedCa(Files.readString(Path.of("/etc/agent/ca.pem")))
        .clientCertificate(Files.readString(Path.of("/etc/agent/agent.pem")),
            Files.readString(Path.of("/etc/agent/agent-key.pem")))
        .build())
    .proxy("http://user:password@proxy.example.com:3128")
    .build();
```

- The CAs given replace the system's. Add `includeSystemRoots(true)` to trust both.
- The client certificate's PEM holds the certificate, then any intermediates. The key may be
  PKCS #8 (`BEGIN PRIVATE KEY`), PKCS #1 RSA (`BEGIN RSA PRIVATE KEY`) or SEC 1 EC (`BEGIN EC
  PRIVATE KEY`), and not encrypted: decrypt an encrypted one with
  `openssl pkcs8 -topk8 -nocrypt -in key.pem`.
- `minVersion` and `maxVersion` narrow the TLS versions. Nothing older than 1.2 is allowed.
- The key must be the certificate's: one that doesn't match is refused.
- Certificates and keys are read when given, and the proxy URL by `build()`, so mistakes fail there.

`proxy(...)` takes `http://`, `https://`, `socks5://` and `socks5h://` proxies, each with
`user:password@` if it needs them. The core speaks to the proxy itself: every connection through
an HTTP proxy is a CONNECT tunnel (`http://` endpoints too), with the URL's credentials as Basic
`Proxy-Authorization`, and no JVM property is needed for them. Through SOCKS5 the proxy resolves
the server's name, unless `proxyResolveLocally(true)`.

With no proxy configured, the client takes the proxy from the environment by the rule every SDK
follows in Rust mode (`tests/shell-tables/README.md`, "Rust mode's proxy rule"): `HTTP_PROXY` for
`http://` and `ws://`, `HTTPS_PROXY` for `https://` and `wss://`, `ALL_PROXY` for either when its
own is unset (upper case first, an empty value unset); `NO_PROXY` lists names (`example.com`
covers its subdomains, `*` every name), addresses and CIDR networks (`10.0.0.0/8`), never
resolving a name; and under CGI (`REQUEST_METHOD` set) no proxy comes from the environment. It
does not read the JDK's proxy settings: `http.proxyHost`, `https.proxyHost`, `http.nonProxyHosts`,
`java.net.useSystemProxies` and a `ProxySelector` the application installed are
[Java mode](#rust-mode-and-java-mode)'s. `directConnection()` ignores the environment and
connects straight to the server. It can't be combined with `proxy(...)`.

## Rust mode and Java mode

`mode(...)` says who speaks HTTP and WebSocket. The client runs in **Rust mode** unless the
application asks for **platform mode**: by naming it in the mode option, or by handing the client
an HTTP client of its own, where the SDK takes one (an application that brings its own client has
asked for the platform's stack). An option only one mode honours is never silently ignored: it
selects that mode, or the client refuses to start with an error that says what to set instead.

Here platform mode is `mode(OpampClient.Mode.JAVA)`, and `httpClient(...)` selects it too.
`build()` refuses `mode(Mode.RUST)` with an `httpClient`, and in Java mode `proxyResolveLocally`,
`rustSplice(false)` and a proxy URL that isn't `http://`.

In Rust mode (`mode(OpampClient.Mode.RUST)`, or no `mode`) the core speaks HTTP/1.1 (chunked,
keep-alive), WebSocket, CONNECT and SOCKS5 itself (inside the same WebAssembly module), over byte streams this library opens. The client then only opens TCP
connections (blocking sockets, `TCP_NODELAY`), starts TLS on them (the JDK's `SSLEngine`, with the
`tls(...)` settings: the certificate is verified for the server's name, or for its address when
the endpoint is an IP literal; TLS to an `https://` proxy carries TLS to the server inside it),
resolves names for a SOCKS5 proxy told to (`proxyResolveLocally`), picks the proxy, and writes
package files.

- `rustSplice(false)` passes package bytes through the core instead of from the socket straight
  to the file (where the response says how long the body is).
- Stop is bounded: the core's stop (its goodbye and closing handshake), then every stream closed
  at once and the files not complete deleted, within what is left of the stop timeout.

Choose Java mode to bring your own `java.net.http.HttpClient`, or when the connection must be
made by the JDK's HTTP client: with the JVM's proxy settings, say.

```java
OpampClient.Config.builder("wss://opamp.example.com/v1/opamp")
    .mode(OpampClient.Mode.JAVA)
    .build();
```

What differs there:

- Proxies are HTTP proxies only: `http://[user:password@]host[:port]`. The JDK's HTTP client has
  no SOCKS, no `https://` proxies and no CONNECT headers of your own, and the client refuses
  those URLs. `http://` requests go to the proxy in absolute form; `https://`, `ws://` and
  `wss://` go through a CONNECT tunnel. The URL's credentials go to the proxy with every request,
  as Basic `Proxy-Authorization`.
- With no proxy configured, the client uses the platform's proxy settings, as the JDK does:
  `ProxySelector.getDefault()`, which reads `http.proxyHost`, `https.proxyHost`,
  `http.nonProxyHosts` and `java.net.useSystemProxies`, and not the environment's `HTTP_PROXY`.
  `directConnection()` ignores them.
- The JDK refuses to send Basic credentials through a CONNECT tunnel unless the JVM runs with

  ```
  -Djdk.http.auth.tunneling.disabledSchemes=
  ```

  It is the JVM's setting, read once when the first HTTP client is built, so the client doesn't
  change it for you. Without it, a proxy that needs credentials answers 407 to every connection
  but plain `http://`, and the client logs a warning at start.
- `httpClient(...)` takes your own `java.net.http.HttpClient`. It is used as it is for OpAMP and
  downloads, and replaces `tls`, `proxy` and `directConnection`: giving both is an error. It must
  not follow redirects.

## The state file

With a state file (`stateFile(path)`) the client keeps what it needs across restarts: accepted
connection settings, the instance UID, the last remote config, telemetry settings and packages.
It holds secrets: the credentials of connection settings the server offered (bearer tokens in
`Authorization` headers) and the private keys of telemetry settings' client certificates.
The client writes it for the agent's user alone: mode 0600 on Linux and macOS, and on Windows
an access-control list that allows only that user. Package files downloaded beside it
(`<state file>.downloads`) are protected the same way.
Keep it on a local disk, in a directory only the agent's user can read: not on a shared or
network path, and not in backups or images copied anywhere less protected.

## Development

```
../../scripts/fetch_core.py fetch   # first: the core's release (the modules, the adversary)
mvn package                   # compile and test
mvn -Pfray test -Dfray.only=true   # Fray's schedule exploration (x86-64 Linux, a JDK 25: below)
../../scripts/gen_core.py     # after the core's .proto files change: regenerates the types
```

The `*FrayTest` classes explore thread schedules with [Fray](https://github.com/cmu-pasta/fray) over a
fake network (`FakeHttp`; in Rust mode, streams in memory, `MemoryNet`): the in-flight registry and
executor (`RegistryFrayTest`; Rust mode's: `StreamsFrayTest`), and the client with its real core
(`ShellFrayTest`, its scenarios in both modes). They run only in the `fray` profile, in a JVM Fray builds by
`jlink` from a JDK 25 (`JDK25_HOME`, or one it downloads); its agent exists for x86-64 Linux and
macOS on arm64. `OPAMP_FRAY_RUNS` sets the schedules per scenario and `OPAMP_FRAY_BUDGET_S` the
seconds each may take; a failure prints the plan seed and Fray's recording, which
`OPAMP_FRAY_SEED=<seed> OPAMP_FRAY_REPLAY=<recording>` replays (with `-Dfray.test=Class#method`).
`ConnLincheckTest` (Lincheck's model checker, in the normal run) checks a connection's send and
Close chain, and `StreamLincheckTest` a Rust-mode stream's write queue; `OPAMP_LINCHECK_ITERATIONS`
sets their scenarios.

The conformance suite runs this client through `conformance/drivers/java-core`, in both modes:
`java-core` (Java mode) and `java-core-rust` (Rust mode, `OPAMP_JAVA_MODE=rust`).

What the client promises around its callback (when it is called, one at a time and never before `start` returns, what a callback that throws does, which calls are allowed from the callback and from other threads, stop from the callback) is [the SDK contract](../../docs/sdk-contract.md), the same for every SDK here. The precise rules, around the core too, are [the shell contract](../../spec/shell-contract.md) (it lists the breaks known today). The callback model test and the adversary harness in this client's tests check it.
