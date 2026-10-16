# OpenTelemetry .NET automatic instrumentation, under an OpAMP server

`dotnet/Pydantic.OpAmp.AutoInstrumentation` is a plugin for upstream's
[automatic instrumentation](https://github.com/open-telemetry/opentelemetry-dotnet-instrumentation).
With it, the SDK that automatic instrumentation sets up joins an OpAMP server's fleet and takes
its settings from the server: sampling, what to send, which instrumentations run
([sdk-settings.md](sdk-settings.md)). The application needs no code, and the SDK and the
instrumentations stay the upstream ones.

It plugs in through upstream's plugin API (`IPlugin`, `ITelemetryPlugin`, and the OTLP exporter
options hooks). Checked with automatic instrumentation 1.17.0 (OpenTelemetry .NET 1.19.1) on
.NET 8, Linux.

Automatic instrumentation has an OpAMP client of its own (`OTEL_DOTNET_AUTO_OPAMP_ENABLED`, off by
default). Leave it off. The plugin brings ours: it keeps its instance UID and state across
restarts, answers the server's requests for a full report, and runs the same core as every other
client here.

## Use it

Add the plugin to the application next to the automatic instrumentation package (from the
repository, until it is published), and name it in `OTEL_DOTNET_AUTO_PLUGINS`:

```xml
<PackageReference Include="OpenTelemetry.AutoInstrumentation" Version="1.17.0" />
<ProjectReference Include="path/to/dotnet/Pydantic.OpAmp.AutoInstrumentation/Pydantic.OpAmp.AutoInstrumentation.csproj" />
```

```bash
OTEL_DOTNET_AUTO_PLUGINS="Pydantic.OpAmp.AutoInstrumentation.Plugin, Pydantic.OpAmp.AutoInstrumentation" \
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
./instrument.sh dotnet Checkout.dll
```

| Variable | |
|---|---|
| `OTEL_OPAMP_ENDPOINT` | The OpAMP server: `http(s)://.../v1/opamp` polls, `ws(s)://...` keeps a connection open. Without it the plugin does nothing. |
| `OTEL_OPAMP_HEADERS` | Headers for every request, as `OTEL_EXPORTER_OTLP_HEADERS`: `name=value,...`, values percent-encoded. The enrollment key goes here. |
| `OTEL_OPAMP_STATE_DIR` | Where to keep state across restarts: the instance UID, the settings for the next start, the client's own state (readable by the application's user only). Without it, the plugin keeps one of the user's own, one per process of the program running at once ([default-state-dir.md](default-state-dir.md); the program is the entry assembly's name), unless `OTEL_RESOURCE_ATTRIBUTES` has a `service.instance.id`; `none` turns that off, and then each start is a new instance and settings that take effect at the next start are refused. One process per directory named here (below). |
| `OTEL_OPAMP_HEARTBEAT_INTERVAL` | How often to check in until the server offers an interval (default 30 s): milliseconds, or `30s`, `2m`. A value it can't read is the default, with a warning (`OpAMP: OTEL_OPAMP_HEARTBEAT_INTERVAL unreadable, using the default: ...`). |

The plugin is compiled against the exact OpenTelemetry versions automatic instrumentation 1.17
ships, as upstream requires of plugins. Upstream tests plugins referenced by the application, as
above; copying one into the automatic instrumentation home directory also works, but its
dependencies (Google.Protobuf, and the native core for the platform) must then go there too, and must
not conflict with the ones automatic instrumentation ships.

## What it does

- **Joins the fleet** with one instance UID: the OpAMP instance UID, and `service.instance.id` on
  every signal's resource (upstream otherwise gives traces and metrics different random ones).
  With a state directory it is kept across restarts, and a UID the server assigns
  (`new_instance_uid`) is saved there for the next start (the running process keeps the
  `service.instance.id` it started with: its resource is built already). Without one, or where the UID
  can't be saved in it, a `service.instance.id` given in `OTEL_RESOURCE_ATTRIBUTES` is used if it
  is a UUID: the same at every start.
- **Takes a key of its own.** An agent enrolled with a shared key (the enrollment key in
  `OTEL_OPAMP_HEADERS`, or `OpAmp:Headers`) is offered a key of its own by a server that hands them
  out ([connection-settings.md](connection-settings.md)). With a state directory the agent says it
  takes OpAMP connection settings (`AcceptsOpAMPConnectionSettings`,
  `ReportsConnectionSettingsStatus`) and takes the offer: it tries the new key (APPLYING), keeps it
  once the server answers with it (APPLIED; the server may then refuse the shared key for it), and
  goes back to the key it had if the server doesn't (FAILED, saying why). The key is kept in the
  state directory, readable by the application's user only, and the next start comes back with it;
  changing the endpoint or the headers it is given starts again from them. An offer may carry an
  interval to check in at (the server's heartbeat interval), which holds until the next start.
  Without a state directory a restart would lose the key, so the agent doesn't say it takes one.
- **Describes itself** from the SDK's resource: `service.name`, `service.namespace` and
  `service.instance.id` identify it; the rest describes it.
- **Takes settings** from the remote config's `elastic` entry:

  | Setting | How |
  |---|---|
  | `sampling_rate` | a sampler in place of the configured one (`OTEL_TRACES_SAMPLER`); a span whose parent was sampled, here or by the caller, is sampled at any rate, 0 included. Left out: the configured sampler. |
  | `send_traces`, `send_metrics`, `send_logs` | each signal's OTLP exporter sends through a gate: `false` answers each export with success without sending it, so nothing leaves and nothing is retried. |
  | `send_logs_level` | at once, at the OTLP logs exporter's gate: each export goes out without its log records below the level (a record with no severity is kept), and one with none left is answered with success without leaving. Left out: every severity. |
  | `deactivate_instrumentations` | at the next start, `OTEL_DOTNET_AUTO_{TRACES,METRICS,LOGS}_<NAME>_INSTRUMENTATION_ENABLED=false`. Names are upstream's, lowercase: `httpclient`, `aspnetcore`, `sqlclient`, and so on. Upstream's have no punctuation, so `.`, `-` and `_` are dropped: `http_client` and `asp-net-core` are `httpclient` and `aspnetcore`. |
  | `deactivate_all_instrumentations` | at the next start, every instrumentation's variable set to false (one set to true would otherwise win). |
  | `opamp_polling_interval` | at the next start, the interval it checks in at, until the server offers one. |

  The newer settings (rules, per-scope levels, redaction, rate caps, debug mode) and export
  health are [below](#the-newer-settings).
  `logging_level` and `infer_spans` (EDOT settings) aren't carried out, so the server can't change a
  log level; the plugin logs them at `OTEL_LOG_LEVEL=info`.
- **Doesn't trace itself.** The OpAMP client runs inside a `SuppressInstrumentationScope`, as the
  SDK's own exporters do, so its requests (and its goodbye) never show up as the application's
  spans.
- **Answers** each remote config APPLIED or FAILED, and reports the settings in force as its
  effective config, with the settings it set aside or didn't know named beside them in an
  `elastic.set_aside` entry ([sdk-settings.md](sdk-settings.md#the-answer)).
- **Says goodbye** (`agent_disconnect`) when the process exits.
- **Never holds the application up.** If the plugin can't start, it says why on standard error
  and the SDK runs unmanaged.

The rules are the SDK settings layer's (`Pydantic.OpAmp.Client.Sdk`), the same in every
language. The plugin only carries them out on the .NET SDK.

### What it can't carry out, and says so

A setting the plugin can't carry out in this process isn't declared to the layer, so it is set aside, rather than
answered APPLIED and not done ([the host](sdk-settings.md#the-host)):

- `send_traces`, `send_metrics`, `send_logs`, for a signal whose exporter isn't OTLP (console,
  Zipkin, Prometheus), or is OTLP beside another (`otlp,console`): only the OTLP one could be
  stopped, and a setting carried out by half isn't carried out, so it is set aside too.
  The gate replaces the OTLP exporter's HTTP client. An exporter given a CA or a client
  certificate (`OTEL_EXPORTER_OTLP_CERTIFICATE`, `..._CLIENT_CERTIFICATE`, `..._CLIENT_KEY`, PEM
  files; the SDK reads no per-signal ones) builds them into its own client, so the gate's client
  carries the same. Files it can't use (missing, unreadable, an encrypted key, a certificate
  without its key) leave every exporter as it is, and none of the three is carried out.
  `send_logs_level` is held at the logs gate, so it is carried out exactly where `send_logs` is.
- A signal some exporter of which another plugin added. A plugin named in
  `OTEL_DOTNET_AUTO_PLUGINS` beside ours can add a processor or an exporter to the providers
  automatic instrumentation builds (in `AfterConfigureTracerProvider`, `TracerProviderInitialized`
  or `ConfigureLogsOptions(OpenTelemetryLoggerOptions)`, say). The plugin reads what each built
  provider exports through, as `WithOpAmp()` does ([below](#in-code-withopamp)). Any exporter
  that isn't the gated OTLP one sends past the gate, so that signal's settings are set aside:
  its `send_*`, for logs the levels and the log cap too, and redaction for traces or logs. It
  reads the tracer and meter providers as it joins the fleet. Automatic instrumentation builds a
  logger provider for each logger factory the application creates, later. The plugin adds a
  processor of its own to each, which exports nothing. On the provider's first log record, before
  any exporter has it, the plugin reads that provider. If a setting in force is set aside then,
  the plugin reports the effective config again. A logger provider the application disposes
  exports nothing more, and is no reason from then on.
- `send_traces`, `send_metrics`, the sampling settings and redaction, when automatic
  instrumentation leaves the SDK to the application (`OTEL_DOTNET_AUTO_SETUP_SDK=false`). It then
  builds no tracer or meter provider, and the application's own aren't the plugin's. The logs
  are still automatic instrumentation's, and their settings are carried out.
- `sampling_rate`, when the application runs with no tracer provider (traces off).
- With file-based configuration (`OTEL_EXPERIMENTAL_FILE_BASED_CONFIGURATION_ENABLED=true`), the
  instrumentation switches and the exporter gates: the file is read before any plugin runs, and
  its exporters' TLS settings can't be kept. Sampling still works: the plugin reads the sampler the
  file configures from the SDK's builder (an internal property) and wraps it, or, if that ever
  can't be read, doesn't carry `sampling_rate` out.

What the plugin can't see: a tracer or meter provider the application builds itself beside
automatic instrumentation's (`Sdk.CreateTracerProviderBuilder()`). It listens to the same
instrumentations, with its own sampler and exporters, and .NET has no way to list the listeners
of an `ActivitySource`. Its telemetry goes on whatever the server says, and the settings stay
answered APPLIED. Add exporters through a plugin instead, or set the SDK up in code with
`WithOpAmp()`.

### One process per state directory

A child process the application starts inherits its environment: `OTEL_OPAMP_STATE_DIR`, and the
instrumentation variables the plugin set at start. Two processes on one state directory would be
one instance to the server (one UID) while each ran and saved its own settings. So the plugin
locks the directory (a `lock` file in it, locked by the operating system while the process runs,
never inherited, gone when the process ends). A process that finds the lock taken, a child say,
runs **unmanaged**: the plugin says so on standard error and leaves the SDK alone. It doesn't
join the fleet under a new UID either, which would add an instance to the fleet for every child
started. The switches the parent set still hold in the child. To manage such processes too, give
each its own `OTEL_OPAMP_STATE_DIR`. A state directory that can't be locked (not writable) is
treated the same way. The default state directory (no `OTEL_OPAMP_STATE_DIR`) has a slot per
process instead, each locked the same way: a child takes the next free slot, and an identity of
its own.

### Is it working?

The plugin says so on standard error. When the client first connects it says `OpAMP: connected
to wss://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>` (at
`OTEL_LOG_LEVEL=info`, as all it has to say that isn't a warning): when a WebSocket opens, or the
first HTTP poll is answered. When it can't, it warns once per
run of failures, with what to check:

```
OpAMP: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused this agent's
credentials: check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). Retrying in the background;
the SDK keeps its current settings meanwhile
```

and `connected to ... again` once it's back; each failure in between only at
`OTEL_LOG_LEVEL=debug`. The endpoint is said without user info, a query or a fragment. An endpoint
without a scheme gets a warning at start (one that isn't a URL at all stops the plugin, which
says so). `WithOpAmp()` (below) says the same on the application's logger, at Information,
Warning and Debug, naming `OpAmp:Endpoint` and `OpAmp:Headers` instead.

## The newer settings

Each is carried out at once, in the plugin and in `WithOpAmp()` alike (the same code), and declared
to the layer ([the host](sdk-settings.md#the-host)) only where it is carried out: elsewhere it is
set aside, never answered APPLIED and not done. What each means is
[sdk-settings.md](sdk-settings.md)'s; here is where .NET holds it. Each document below goes in the
remote config's `elastic` entry as it is.

### `sampling_rules`

```json
{"sampling_rate": "1",
 "sampling_rules": [{"rate": 0, "kind": "server", "path": "/health*"},
                    {"rate": 0.1, "name": "GET /api/*"}]}
```

In the live sampler, before `sampling_rate`: a new trace's root span takes the first rule it
matches, else `sampling_rate`, else the application's sampler; a span with a parent follows it. A
rule's rate is applied by the SDK's `TraceIdRatioBasedSampler`, so a trace is all or nothing.
Rules match what a span has when it starts, and on .NET 8 that is little:

- ASP.NET Core's request span reaches the sampler with no attributes, its path among them. So for
  a new trace's request the sampler records it, and the ASP.NET Core instrumentation's `Filter`,
  which is given the `HttpContext` as the request starts, decides it with the request's
  `url.path`, `http.request.method`, `url.scheme`, `server.address` and `server.port`: rules,
  rate, the application's sampler and the trace cap, as the sampler would. Turned down, the
  request is not recorded, and its children and outgoing calls follow it unsampled. A `Filter`
  the application set runs first, and its `false` wins. The plugin sets it through
  `IConfigureTracesOptions<AspNetCoreTraceInstrumentationOptions>`; `WithOpAmp()` post-configures
  the options when the application has the instrumentation (by reflection: the package doesn't
  make every application an ASP.NET Core one).
- An `HttpClient` span on .NET 8 starts with no attributes either (it is named
  `System.Net.Http.HttpRequestOut` then): only a rule with neither `path` nor `attributes` can
  match it, by `kind: client` say.
- A span of the application's own `ActivitySource` has the tags it was started with.

Declared wherever the sampler is the plugin's (not where `sampling_rate` is ignored).

### `send_logs_level_by_scope`

```json
{"send_logs_level": "warn", "send_logs_level_by_scope": "Checkout.Db*=debug,Microsoft.*=error,Noisy.Library=off"}
```

At the OTLP logs exporter's gate, beside `send_logs_level`: a record's scope is its `ILogger`
category, and the entry for exactly that category, else the longest prefix (`name*`) that matches
it, sets its level (`off`: none); categories are matched as written, case and all. Declared where `send_logs_level` is carried out:
every logs exporter is the gated OTLP one.

### `drop_attributes`, `mask_attributes`

```json
{"drop_attributes": "user.email,secret.*", "mask_attributes": "http.request.header.authorization,enduser.id"}
```

At the traces and logs gates, in the OTLP bytes as they leave (http/protobuf or gRPC, gzip or
not): a span's attributes, its events' and its links', and a log record's attributes. A dropped
key goes; a masked one keeps its key with the value `[REDACTED]`. Another exporter, or a processor,
sees the telemetry as it was: nothing shared is changed. A log record's body (its message
template) is left alone. An export the gate can't read (another encoding) isn't sent while either
is set: what can't be read can't be redacted. Declared only where every span and every log
record exported goes through a gate (traces and logs each gated, or not exported at all).

### `sampling_traces_per_second`, `send_logs_per_second`

```json
{"sampling_traces_per_second": "50", "send_logs_per_second": "200"}
```

Token buckets on the process's monotonic clock, full at first, refilled at the rate up to a
second's worth. The trace cap is in the sampler, after the rules and the rate: a root span the
sampler kept, or a span whose remote parent was sampled, takes a token, and without one the
trace is dropped; a local child takes none (an ASP.NET Core request takes its token in the
`Filter`). The log cap is at the logs gate, after the levels: an export keeps its first records,
as many as there are tokens. A cap left out is gone; one sent again at the same rate keeps its
bucket. Both stay in force in debug mode. Declared with the sampler and with the logs gate.

### `debug_until`, `debug_sampling_rate`, `debug_send_logs_level`

```json
{"sampling_rate": "0.05", "debug_until": "2026-10-14T17:30:00Z", "debug_sampling_rate": "1", "debug_send_logs_level": "debug"}
```

The layer decides with the instance's clock and folds the window into the settings it hands
over; the agent runs one timer (`System.Threading.Timer`) for the end the layer gives
(`refresh_at_unix_ms`), as a deadline on the monotonic clock: the wall clock jumping while the
process runs doesn't move it. When it goes off the agent calls `Refresh`, puts the result in
force and reports the effective config: debug mode ends without the server. A later remote config
re-arms it (a generation counter makes the old timer's call do nothing). Declared always; the debug
rate and level are ignored where `sampling_rate` and `send_logs_level` are.

### Export health

No setting: the agent's health carries `component_health_map["exporters"]`
([export health](sdk-settings.md#export-health)), the top-level `healthy` staying the
application's (`WithOpAmp()`'s health checks). The gate counts each export attempt: the OTLP
exporter's retry is a new request through it, so a retry that later succeeds briefly shows
`failing`. An HTTP status, gRPC's `grpc-status` (read from the trailers), or an exception (its
message is the `last_error`). Records the levels and the log cap drop, and traces the trace cap
refuses, are `items.filtered`; an export redaction can't read, and so doesn't send, is a failed
one. The agent asks the layer at start (the first report goes with the first message) and every
30 s on the same timer, and sends when the layer says to.

### Inventory

No setting either: once the SDK is built, the agent reports what it runs as its available
components ([what the SDK integrations report](available-components.md#what-the-sdk-integrations-report)),
and the server is sent their hash, and them in full when it asks. It lists:

- `sdk`: the package (`Pydantic.OpAmp.AutoInstrumentation`, or `Pydantic.OpAmp.Extensions.Hosting`
  for `WithOpAmp()`) and its version, `telemetry.sdk.language` `dotnet`, and the OpenTelemetry
  SDK's version.
- `instrumentations`: under the names `deactivate_instrumentations` takes, upstream's lowercase
  (`httpclient`, `aspnetcore`, `netruntime`). The plugin lists every instrumentation automatic
  instrumentation has on .NET, switched off or not (ASP.NET and WCF services, .NET Framework's
  alone, aren't), at automatic instrumentation's version, which they ship with. `WithOpAmp()`
  lists those the application added to its providers, by their packages
  (`OpenTelemetry.Instrumentation.Http` is `httpclient`; another's name after
  `OpenTelemetry.Instrumentation.`, without its punctuation), at each package's version.
- `exporters`: for each signal, the plugin's from `OTEL_{TRACES,METRICS,LOGS}_EXPORTER` (`otlp`,
  `console`, `zipkin`, `prometheus`; `none`, a signal switched off with
  `OTEL_DOTNET_AUTO_{SIGNAL}_ENABLED=false`, and a name automatic instrumentation doesn't know
  list none), and `WithOpAmp()`'s read from the built providers, by those names or, for one they
  don't name, its type (`InMemoryExporter`, or a reader or processor it can't see into). `gated` is
  whether the signal's `send_*` setting is carried out ([above](#what-it-cant-carry-out-and-says-so)):
  beside an exporter that can't be stopped, the OTLP one is not gated either. With file-based
  configuration the exporters are the file's, which the plugin can't read, so none is listed.
- `settings`: the ones it carries out, from the same host it answers by.

It is reported as the agent joins the fleet. It is reported again only if an exporter that isn't
gated shows up later (another plugin's logger provider, or a processor added to a built provider
under `WithOpAmp()`): then the signal's exporters are no longer `gated`, and its settings are no
longer listed.

## In code: `WithOpAmp()`

An application that sets its SDK up in code (`services.AddOpenTelemetry()`) gets the same with one
call, from `dotnet/Pydantic.OpAmp.Extensions.Hosting`:

```csharp
builder.Services.AddOpenTelemetry()
    .WithTracing(t => t.AddAspNetCoreInstrumentation().AddOtlpExporter())
    .WithMetrics(m => m.AddAspNetCoreInstrumentation().AddOtlpExporter())
    .WithOpAmp();
// or: builder.AddPydanticOpAmp();
```

The server comes from the configuration's `OpAmp` section (`OpAmp:Endpoint`,
`OpAmp:Headers:<name>`, `OpAmp:StateDirectory`, `OpAmp:HeartbeatInterval`), else from the
variables above (`OTEL_OPAMP_ENDPOINT` and the rest); `WithOpAmp(o => ...)` wins over both. With
no endpoint it changes nothing. Without a state directory named, it keeps one of the user's own
by default, as the plugin does; `none` turns that off. A value it can't read never fails the host: a heartbeat interval
it can't read, or a negative one, is the default, with a warning through the application's logger (`OpAMP:
OpAmp:HeartbeatInterval unreadable, using the default: ...`); an endpoint that isn't a URL leaves
the application running unmanaged, with an error saying why (`OpAMP: the client did not start;
the SDK runs unmanaged: OpAmp:Endpoint is not a URL`). It does what the plugin does, with the same code
(`dotnet/shared`, compiled into both): one instance UID (kept in the state directory, and the
resource's `service.instance.id`, over one the application set before `WithOpAmp()`), the live sampler in place of the
application's while a rate is set (the application's again when it is left out), and every OTLP
exporter gated, its signal told by each request's path
(`/v1/traces`, or gRPC's `TraceService/Export`; an endpoint with a path of its own isn't gated).
A hosted service starts the client once the SDK is built and says goodbye as the host stops. The
application's health checks, if it has any, are the agent's health, reported when they change.

What it can't carry out: the instrumentation switches (the application names its
instrumentations in code); `send_traces`, `send_metrics`, `send_logs` and `send_logs_level` when the OTLP CA or
client certificate files (`OTEL_EXPORTER_OTLP_CERTIFICATE` and the rest) can't be used, as for the
plugin (a gate carries the TLS they set up); `sampling_rate`
with no tracer provider, or when a sampler is set after `WithOpAmp()` (it says so: call it last).
A signal's `send_*` setting (for logs, `send_logs_level` with it) is ignored too, like other settings it can't carry out, when any of
the signal's exporters can't be stopped: one that isn't OTLP (console, in-memory, Prometheus), an
OTLP exporter given an endpoint with a path of its own, and one given an `HttpClientFactory` of
its own. Which these are is read from the providers once they are built (from the SDK's internals,
as the sampler is); a processor or reader it can't see into counts as one that can't be stopped.
It doesn't matter where in the builder the exporter is added, before `WithOpAmp()` or after.
The providers are read again before each remote config is decided, and every 30 s on the health
timer. So a processor the application adds to a built provider (`tracerProvider.AddProcessor`,
`loggerProvider.AddProcessor`) sets its signal's settings aside from the next decision on. A
config already in force is decided again within 30 s, and the effective config is reported again.
A tracer or meter provider the application builds on its own (`Sdk.CreateTracerProviderBuilder()`)
isn't under `WithOpAmp()`, and isn't seen.
Tests: `dotnet/Pydantic.OpAmp.Extensions.Hosting.Tests`, a host against an OpAMP server and a
collector on loopback.

## The application's own settings

The server may send the application settings of its own in `custom` (a feature flag, a
verbosity), which the plugin and `WithOpAmp()` hand on rather than read
([app-settings.md](app-settings.md)). The application reads them through `OpAmpAppSettings`
(namespace `Pydantic.OpAmp`):

```csharp
using Pydantic.OpAmp;

if (OpAmpAppSettings.Get("checkout.new_flow", "off") == "on")
{
    // ...
}

var limit = OpAmpAppSettings.GetInt64("checkout.max_items", 50);

var changes = new ConcurrentQueue<IReadOnlySet<string>>();
using var subscription = OpAmpAppSettings.OnChange((settings, changed) => changes.Enqueue(changed));
// The work is done on the application's own thread, from the queue. Disposing it unsubscribes.
```

`Get(key, defaultValue)` is the value in force, exactly as sent, or `defaultValue`, never an
exception: before the first config, without an OpAMP server, with neither the plugin nor
`WithOpAmp()` there, and when the config in force leaves the key out. `GetBool`, `GetInt64` and
`GetDouble` read one typed, by the rules every language shares, and are the default for anything
else. `Snapshot()` is every value in force, an `IReadOnlyDictionary<string, string>` in key order
that no later change alters. A handler given to `OnChange` gets that map and the keys added,
changed or removed (an `IReadOnlySet<string>`, enumerated in ordinal order), once per change, in
order, one at a time, on the OpAMP client's thread: keep it quick. An exception it throws is caught
and logged as an error (`OpAMP: an application settings handler threw ...`); the other handlers
are still called and the config is still APPLIED. Disposing what `OnChange` returns unsubscribes;
disposing it again does nothing. Any thread may read, subscribe and unsubscribe at any time, and a
reader sees the whole of one map or the whole of the next.

`OpAmpAppSettings` lives in `Pydantic.OpAmp.Client`, the assembly the plugin and the hosting
package already both depend on, so it is one type whichever of them fills it, and an application
that references either has it; it is a small static class with no dependency of its own, and with
nothing attached it answers every read with its default and never calls a handler. Both declare
`custom` always, and put it in force after the SDK's settings, under the same lock, from a load, an
APPLIED config or a refresh alike: a handler sees the SDK's settings of the same document already
in force. A config whose `custom` is wrong is FAILED and changes nothing.

With a state directory the values in force are saved and loaded again at the next start: the
plugin loads them as automatic instrumentation starts, before the application's `Main`, so they are
in force from its first line whether the server can be reached or not, and a handler it adds is
told of the server's changes from then on, not of those. `WithOpAmp()` loads them when the SDK is
built. A plugin that fails as it starts, after loading them, puts back the values that were in
force before and stops its agent, so it never puts them back later. `examples/dotnet-otel/Program.cs`
reads the flag `example.feature` and requests `/changed` from its main loop for each change it is
told of.

## How it is checked

- Unit tests (`dotnet/Pydantic.OpAmp.AutoInstrumentation.Tests`):
  - the sampler and the gate, on the real SDK types;
  - the next-start settings become the instrumentation variables;
  - the answers, saving and reporting follow the layer;
  - what the plugin can't carry out is ignored;
  - the UID is kept across starts, and a UID the server assigns is the next start's;
  - a second process on the same state directory runs unmanaged;
  - by default (no state directory named) the UID is kept across restarts, a second process at
    once gets a slot and a UID of its own, another service another UID; `none`, a
    `service.instance.id` in `OTEL_RESOURCE_ATTRIBUTES`, no home, one that can't be written and
    every slot taken each mean no state directory;
  - stopping never throws, even when the client stopped on an error;
  - instrumentation names map to upstream's variables whatever their punctuation;
  - the sampler the builder was given is read from it (the internal property);
  - the shared vectors (`testdata/sdk-settings`) through the real code: `sampling.json`
    through the policy and the sampler, `caps.json` through the sampler and the logs gate on an
    injected clock, `logs.json` and `redact.json` through the gate's rewriter, `debug.json` and
    `health.json` through the agent and its timer on injected clocks;
  - the ASP.NET Core `Filter` against a real Kestrel application, as automatic instrumentation's
    initializer sets it up (and under `WithOpAmp()` in the hosting tests): a path rule drops the
    whole trace, a remote parent decides, the application's `Filter` runs first, the cap counts;
  - the shared vectors `components.json` through the client's `SdkSettings.Components`, and the
    inventory each integration builds: names `deactivate_instrumentations` takes, versions, each
    exporter's name and `gated` agreeing with the `send_*` it carries out (none, duplicates, one it
    doesn't know, file-based configuration), sent by the client to a server that asks for it in
    full;
  - live changes and reverting: rules, levels, caps and redaction come and go, a stale debug timer
    does nothing, the gate counts each attempt by its status;
  - telemetry past the gates (`OwnExportersTests`): another plugin's span exporter sets the traces
    settings aside, and the plugin's own gated exporter alone sets nothing aside; another plugin's
    log processor sets the logs settings aside once its provider handles a record, with a config
    already in force, and its provider disposed is no reason any more;
    `OTEL_DOTNET_AUTO_SETUP_SDK=false` sets traces and metrics aside but not logs. Under
    `WithOpAmp()` (the hosting tests), a processor added to a built tracer provider sets traces
    aside at the next decision, and one added to the logger provider sets logs aside at the next
    check, reported to the server;
  - the application's own settings (`AppSettingsTests`): reads before any config and once a key is
    left out; the typed reads against `custom.json`'s `typed` vectors and their edges; every
    `custom.json` case through the .NET wrapper's `Decide` and `Controls`, and through the plugin's
    agent; a handler told once per change and never for the same map (sent again, another way, by a
    Refresh at the end of debug mode), in order, one at a time; an exception caught and logged;
    unsubscribing, also from inside a handler; readers and subscribers on other threads, and each
    document's SDK settings and custom values in force together; a bad map FAILED with nothing
    changed; restarts with and without a state directory; a plugin that fails as it starts putting
    the values back; and `OpAmpAppSettings` itself filled by the plugin and by `WithOpAmp()` (the
    hosting tests).
- End to end: `python3 examples/dotnet-otel/try.py` (CI's `dotnet-otel` job), the checks every
  integration passes ([javaagent.md](javaagent.md#how-it-is-checked), `examples/integrations.py`).
  A program with no OpenTelemetry code runs under `instrument.sh`, and its HttpClient requests
  are traced. The OpAMP client's requests go through HttpClient too, so the check that they
  aren't traced fails if the suppression is taken out. Then the newer settings, each against the
  reference server, its telemetry through a recording proxy (runs 7 to 9 in the script): a path
  rule for ASP.NET Core requests, a scope level, redaction, the caps and their removal, a debug
  window ending on the agent's timer, and export health through a minute of refused exports. Last,
  `check_app_settings` ([app-settings.md](app-settings.md#how-it-is-checked)): the server turns
  `example.feature` on and off, a bad map is FAILED and changes nothing, the same map again tells
  nothing, and the next start has the saved value from the start. Then `check_truth`
  (`examples/integrations.py`) under two setups (`examples/dotnet-otel/Setups.cs`): each setting
  must show in the telemetry or be named as set aside, and the set-aside ones must be exactly
  those the setup takes from the plugin. `truth-plugin-exporters`: a plugin of the application's
  own adds a span exporter and a log processor over OTLP. `truth-own-sdk`: with
  `OTEL_DOTNET_AUTO_SETUP_SDK=false`, the application builds its own tracer and meter providers.
  `--local` runs it with this host's .NET SDK and native core instead of Docker.
