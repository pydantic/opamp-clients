# Using the clients with the OpenTelemetry SDKs

> To put an SDK under a server without writing code, use a language's integration instead:
> [sdk-settings.md](sdk-settings.md) lists them (the Java agent extension first,
> [javaagent.md](javaagent.md)), and describes the settings a server sends. This page shows the
> wiring by hand, for an application that builds its own SDK.

An application instrumented with an OpenTelemetry SDK can run one of these OpAMP clients next to
it. The server then knows the application: what it is, what it runs with, whether it is healthy.
The server can also change its configuration while it runs. The SDK and the OpAMP client don't
know about each other: the application connects them. This page shows how, in every language.

Each language has a complete program in [`examples/`](../examples). Each one:

- describes itself from the SDK's resource;
- samples traces at a ratio the server can change;
- answers every remote config with APPLIED or FAILED;
- sends its traces where the server says (telemetry settings), with the credentials it gives;
- says goodbye when it is interrupted.

`python3 examples/try.py <language> http|ws` builds one and runs it against the conformance
suite's reference server. The server offers a valid config, then an invalid one, and tells the
example to send its traces to the server's own OTLP/HTTP receiver with a token. The run passes
when the configs are answered, the server hears APPLIED for the telemetry settings, and every span
export arrives with the token and the SDK's resource. Every example passes over both transports.

| Language | Example | SDK used |
|---|---|---|
| Go | [examples/go](../examples/go/main.go) | `go.opentelemetry.io/otel/sdk` 1.38 |
| Node.js | [examples/node](../examples/node/example.mjs) | `@opentelemetry/sdk-trace-node` 2.1 |
| Python | [examples/python](../examples/python/example.py) | `opentelemetry-sdk` 1.38 |
| Java | [examples/java](../examples/java/src/main/java/Example.java) | `io.opentelemetry:opentelemetry-sdk` 1.54 |
| .NET | [examples/dotnet](../examples/dotnet/Program.cs) | `OpenTelemetry` 1.19 |

Each also uses its SDK's OTLP/HTTP trace exporter: `otlptracehttp`,
`@opentelemetry/exporter-trace-otlp-proto`, `opentelemetry-exporter-otlp-proto-http`,
`opentelemetry-exporter-otlp` and `OpenTelemetry.Exporter.OpenTelemetryProtocol`.

## What goes where

| OpAMP | From the SDK | How |
|---|---|---|
| Instance UID | `service.instance.id` | Generate one UUID and use it for both, so the fleet view and the telemetry name the same instance. Prefer UUIDv7 where the language has it (Go, Python 3.14 and later). With a state file the client keeps a UID the server assigns across restarts, and tells you at start (`InstanceUidChanged`); or pass none and let the client make one up and keep it. To have the server assign it, turn on `request_instance_uid`: the UID you start with is temporary until then, so name the instance by the UID `InstanceUidChanged` tells you; with a state file the assigned UID is kept, without one every start asks again and gets a new UID ([instance-uid.md](instance-uid.md)). |
| Agent description | the resource | Identifying: `service.name`, `service.namespace`, `service.instance.id`. Non-identifying: everything else (host, OS, `telemetry.sdk.*`, `deployment.environment.name`). |
| Effective config | what the application runs with | Report it at start, and again after applying a remote config. |
| Remote config | the application applies it | Parse it, change the SDK, then answer with `RemoteConfigStatus`, APPLIED or FAILED, and the config's hash. The client hands each config over once and one at a time, and with a state file hands the last one over again at start ([remote-config.md](remote-config.md)). |
| Telemetry settings | the SDK's exporters | Declare `ReportsOwnMetrics`, `ReportsOwnTraces` and `ReportsOwnLogs` for the signals you export. On `ConnectionSettings`, point each exporter at `own_metrics`, `own_traces` or `own_logs`: `destination_endpoint` is the full OTLP/HTTP URL (`.../v1/metrics`), with its `headers`; an empty one switches that signal off. Rebuild the exporter and swap it in, then answer with `ConnectionSettingsStatus`, APPLIED or FAILED, and the settings' hash. You get all the settings every time, not only what changed, and with a state file again at start ([telemetry-settings.md](telemetry-settings.md)). |
| Available components | what the application can run: its instrumentations, say | Give them at start (`available_components`), with no hash: the client computes it, sends it, and sends the full list when the server asks ([available-components.md](available-components.md)). |
| Custom messages | what the application and its server agree on | Declare the custom capabilities (`custom_capabilities`); messages for them arrive as `CustomMessage`, and `send_custom_message` sends one back, at least once ([custom-messages.md](custom-messages.md)). |
| Packages | extensions the application loads | With a state file and `accept_packages`, install each `InstallPackage` from the file it names (checking its signature is yours), then answer; remove each `RemovePackage`. Both must be safe to do twice: after a crash the client asks again ([packages.md](packages.md)). |
| Health | the application's own view | `healthy: true` at start; report changes with `setHealth`. |
| Stop | before the SDK shuts down | Stop the OpAMP client first, so the server hears the agent go (`agent_disconnect`), then shut the SDK down so it flushes. |

The capabilities to declare are `AcceptsRemoteConfig`. The client adds `ReportsStatus` and
`ReportsHeartbeat` itself, and the `Reports*` bit for each status field you set.

## Changing the SDK while it runs

SDKs are built once, so the application keeps a handle on anything the server may change. In the
examples that is a sampler that delegates to another it can swap: the tracer provider gets
`ParentBased(live)`, and a remote config replaces what `live` delegates to.

| Language | The live sampler |
|---|---|
| Go | implements `sdktrace.Sampler`; holds an `atomic.Pointer[sdktrace.Sampler]` |
| Node.js | an object with `shouldSample` and `toString`; the SDK's `Sampler` is an interface |
| Python | subclasses `Sampler`; `should_sample` and `get_description` delegate |
| Java | implements `Sampler`; a `volatile Sampler` field |
| .NET | subclasses `Sampler`; a `volatile Sampler` field |

The same pattern works for anything else you want the server to control: a log level, a feature
flag, an exporter's endpoint. You wire those by hand: the integrations
([sdk-settings.md](sdk-settings.md)) don't carry out a log level.

### Where the telemetry goes

The examples declare `ReportsOwnTraces`. When the server says where traces go
(`ConnectionSettings`, with `own_traces`), they build an OTLP/HTTP exporter for the endpoint (the
full URL, `.../v1/traces`) and headers, put it in place, and answer APPLIED with the settings'
hash (FAILED, with the reason, if the exporter can't be built). The settings always hold
everything, not only what changed, and with a state file come again at start.

| Language | Swapping the exporter |
|---|---|
| Go | a `sdktrace.SpanExporter` holding an `atomic.Pointer`, under the batcher; the old exporter is shut down |
| Node.js | an object with `export`, `shutdown` and `forceFlush`, under a `BatchSpanProcessor` |
| Python | subclasses `SpanExporter`, under a `BatchSpanProcessor` |
| Java | implements `SpanExporter`; a `volatile` field, under a `BatchSpanProcessor` |
| .NET | a new `TracerProvider` with the new exporter; the old one is disposed (which flushes it) |

The remote config's format is between the application and its server. The examples take
`{"sampling_ratio": 0.25}` from the config map's `""` entry. Anything else is answered FAILED
with the reason, and a missing key counts too: a default of 0 would silently stop sampling.

## Pitfalls

Running the examples turned these up:

- **A client can stop on its own**: a bug, in the client or in your callback. You hear `Stopped` with the
  error at once, and `stop` returns or throws it too. Don't ignore it: nothing keeps a Node
  process alive once the client has stopped, so it would exit 0 unmanaged. The examples print
  it and exit 1.
- **Notifications arrive on the client's thread or task.** Keep the callback quick. It may ask
  for `stop`, but must not wait for it: the client stops once the callback has returned. Go's
  `Stop` called in the callback returns at once ([sdk-contract.md](sdk-contract.md)). The Go
  example hands notifications to a goroutine of its own through a channel.
- **Go: `atomic.Value` panics** if a later value has a different concrete type.
  `TraceIDRatioBased(1)` and `TraceIDRatioBased(0.25)` do, so hold the sampler in an
  `atomic.Pointer[sdktrace.Sampler]`.
- **Node: build messages with `create()`.** protobuf-es v2 won't encode a plain object pushed
  into a repeated field of a message (`d.identifyingAttributes.push(...)`).
- **.NET: an exporter takes the resource from its provider**, through a property only the SDK
  sets, so an exporter swapped in under a delegating exporter sends no resource. Build a new
  provider instead. (Go, Node, Python and Java take the resource from each span.)
- **Node: `exporter-trace-otlp-http` sends JSON.** Use `exporter-trace-otlp-proto` for
  protobuf, OTLP/HTTP's default and what most receivers expect first.
- **Python: protobuf 6 or 7.** The client's generated code is protobuf 6's, so it runs on either.
  OpenTelemetry's Python packages before 1.43 require protobuf below 7.
- **Python: `uuid.uuid7()` is Python 3.14 and later.** The example uses `uuid4()`; any 16 bytes
  that are not all zero are a valid instance UID.

## Rust mode and platform mode

Each of the five SDKs (Go, Node.js, Python, Java, .NET) can reach the server two ways. In **Rust mode** the shared module speaks HTTP, WebSocket, CONNECT and SOCKS5 itself, and
the language only opens TCP connections, starts TLS with its own library and moves the bytes, so
the bytes on the wire are the same from every language. In **platform mode** the language's own
HTTP and WebSocket stack carries out the core's requests.

The client runs in **Rust mode** unless the application asks for **platform mode**: by naming it
in the mode option, or by handing the client an HTTP client of its own, where the SDK takes one
(an application that brings its own client has asked for the platform's stack). An option only
one mode honours is never silently ignored: it selects that mode, or the client refuses to start
with an error that says what to set instead.

| Language | Platform mode, by name | An HTTP client of the application's | Refused at start |
|---|---|---|---|
| Go | `Mode: opamp.ModeGo` | `Config.HTTPClient`: selects Go mode | `ModeRust` with an `HTTPClient`; `NoRustSplice` in Go mode; a `TLS.Config` with a `ServerName` in Rust mode |
| Node.js | `mode: "node"` | takes none | `rustSplice: false` in Node mode |
| Python | `Config(mode="python")` | takes none | `rust_splice=False` in Python mode |
| Java | `mode(OpampClient.Mode.JAVA)` | `httpClient(...)`: selects Java mode | `Mode.RUST` with an `httpClient`; in Java mode `proxyResolveLocally`, `rustSplice(false)` and a proxy that isn't `http://` |
| .NET | `Mode = OpAmpMode.DotNet` | `HttpClient`: selects .NET mode | `OpAmpMode.Rust` with an `HttpClient`; in .NET mode `ProxyUseLocalResolver` and `RustSplice = false` |

The examples name no mode, so they run in Rust mode. The integrations of
[sdk-settings.md](sdk-settings.md) ask for platform mode by name: keeping the client's own
requests out of the application's telemetry is built around the platform's HTTP stack there.

What an application that sets nothing gets, and where platform mode differs:

- **TLS** is the platform's library in both modes, from the same settings: the system's CAs by
  default (as the language's TLS library finds them), or the CAs, client certificate and versions
  the application gives.
- **The proxy**, when none is configured, comes from the environment by one rule in every
  language (`tests/shell-tables/README.md`, put in place by `scripts/fetch_core.py fetch`,
  "Rust mode's proxy rule"):
  `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` and `NO_PROXY`, the upper-case name first, nothing
  under CGI, and `localhost` like any other host. Platform mode keeps each platform's own
  convention instead: Go's `net/http` (never a proxy for `localhost`, no `ALL_PROXY`), Node's
  (the environment only with `NODE_USE_ENV_PROXY=1` or `--use-env-proxy`), Python's `urllib`
  (lower-case names first, `name:port` entries), the JDK's `ProxySelector` (`https.proxyHost` and
  the other system properties, not the environment), .NET's `HttpClient.DefaultProxy` (the
  environment, or on Windows and macOS the system's settings). An application that relied on one
  of those sets the proxy in the client's options, or chooses platform mode.
- **Through an HTTP proxy** every connection is a CONNECT tunnel, to `http://` endpoints too. Go,
  Java and .NET's platform modes send `http://` requests to the proxy in absolute form.
- **HTTP** is HTTP/1.1. Go's platform mode speaks HTTP/2 to a server that offers it.

Each client's README (for Go, its package documentation) has the rest: its options in both
modes, and what its platform mode differs in.

## Credentials and state

To let the server give each instance its own credentials, give the client a state file and turn
on `accept_connection_settings` (see [connection-settings.md](connection-settings.md), which
lists the option in each language). The examples leave this out; a comment in each shows where a
bootstrap `Authorization` header goes.

The core checks every header list it sends against one rule, whether the headers were configured,
offered by the server, or added by an authentication extension. Go's `Config.AuthHeader`, which
the Supervisor's auth extensions use, hands the core a token it renews. The core sets the token
over the header of the same name on each later request and connection, and never saves it. A list
the rule refuses fails that attempt, and nothing is sent.

## Each client

Each client has a page of its own: how to install it, start and stop it, its options, and its
threads: [Python](python.md), [Go](go.md), [Node.js](node.md), [Java](java.md) and
[.NET](dotnet.md). The OpenTelemetry Collector's extension and Supervisor are in
[collector.md](collector.md).

The conformance suite also runs the upstream OpAMP clients (opamp-go, and the Java, .NET and
Python contrib clients); their known results are in
[conformance/baseline.toml](../conformance/baseline.toml).
