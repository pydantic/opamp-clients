# OpenTelemetry for Go, under an OpAMP server

`go/otel` is `github.com/pydantic/opamp-clients/go/otel`, package `opampotel`. `Setup` builds the
OpenTelemetry SDK the `OTEL_*` variables describe, sets it as the global providers, and puts it
under an OpAMP server's fleet: sampling, what to send and which instrumentations run follow the
server's settings ([sdk-settings.md](sdk-settings.md)).

Go has no agent to attach at run time: an application instruments itself (otelhttp, otelgrpc, a
log bridge) and sets up the SDK in `main`. `Setup` takes the place of that setup. The
instrumentations and the SDK stay the upstream ones. Checked with `go.opentelemetry.io/otel`
1.46 and contrib 0.71; it needs Go 1.25 or later.

## Use it

```go
import "github.com/pydantic/opamp-clients/go/otel"

func main() {
	sdk, err := opampotel.Setup(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer sdk.Shutdown(context.Background()) // goodbye to the server, then flush

	http.DefaultTransport = otelhttp.NewTransport(http.DefaultTransport)
	// ...
}
```

Then run the application with the server's address:

```bash
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
./checkout
```

| Variable | |
|---|---|
| `OTEL_OPAMP_ENDPOINT` | The OpAMP server: `http(s)://.../v1/opamp` polls, `ws(s)://...` keeps a connection open. Without it, `Setup` builds the same SDK, unmanaged. |
| `OTEL_OPAMP_HEADERS` | Headers for every request, as `OTEL_EXPORTER_OTLP_HEADERS`: `name=value,...`, values percent-encoded. The enrollment key goes here. A header HTTP can't carry warns and is left out. |
| `OTEL_OPAMP_STATE_DIR` | Where to keep state across restarts: the instance UID, the settings for the next start, the client's own state (written readable by the application's user only). Without it, the package keeps one of the user's own, one per process of the program running at once ([default-state-dir.md](default-state-dir.md)); `none` turns that off, and then each start is a new instance and a setting that takes effect at the next start (`opamp_polling_interval`) is refused. A directory named here belongs to one process: two processes given the same one are the same instance. |
| `OTEL_OPAMP_HEARTBEAT_INTERVAL` | How often to check in until the server offers an interval (default 30 s): milliseconds, or `30s`, `2m`. A value it can't read is the default, with a warning. |

### In code

`Setup` takes the same four as options. An option given wins over its variable; the rest still
come from the variables. An endpoint given in code turns the package on without
`OTEL_OPAMP_ENDPOINT`, and headers given in code replace `OTEL_OPAMP_HEADERS` (values as they
are, not percent-encoded). An option left blank (an empty string or map, a zero interval) is as
if not given, so the application's own flags or settings can be passed as they are and fall back
to the variables when unset. A value that can't be used warns and is left out or is the
default, as in the variables: a negative interval, a header HTTP can't carry.

```go
sdk, err := opampotel.Setup(ctx,
	opampotel.WithEndpoint("https://opamp.example.com/v1/opamp"),
	opampotel.WithHeaders(map[string]string{"Authorization": "Bearer " + key}),
	opampotel.WithStateDir("/var/lib/checkout/opamp"), // or "none"
	opampotel.WithHeartbeatInterval(30*time.Second),
)
```

[examples/go-otel/main.go](../examples/go-otel/main.go) does it from its own flags
(`-opamp`, `-state-dir`, `-heartbeat`) and a key in `CHECKOUT_OPAMP_KEY`, and
[try.py](../examples/go-otel/try.py) runs every check against it that way too, with no
`OTEL_OPAMP_*` set. The same, on pkg.go.dev: `ExampleSetup_inCode` and `ExampleSetup_fromFlags`
in [example_test.go](../go/otel/example_test.go).

The SDK is what [autoexport](https://pkg.go.dev/go.opentelemetry.io/contrib/exporters/autoexport)
and the SDK's own variables make it: the exporter each `OTEL_*_EXPORTER` names (OTLP by default,
over `http/protobuf` unless `OTEL_EXPORTER_OTLP_PROTOCOL` says `grpc`), the sampler
`OTEL_TRACES_SAMPLER` names, the propagators `OTEL_PROPAGATORS` names, and the resource from
`OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` plus the host.

### Is it working?

The package says so through `slog`'s default logger. When the client first reaches the server it
logs, at INFO, `opamp: connected to https://opamp.example.com/v1/opamp; this SDK is managed as
checkout, instance <uid>`. When it can't, it logs one WARN per run of failures, with what to check:

```
opamp: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused this agent's
credentials: check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). Retrying in the background;
the SDK keeps its current settings meanwhile
```

and at INFO, `connected to ... again` once it's back. The warning names what to check as it was
given: with `WithHeaders`, it says `check WithHeaders`, and with `WithEndpoint`, `check
WithEndpoint`. The endpoint is logged without user info, a query string or a fragment. An endpoint without a scheme gets a warning at start: it is taken as `http://`.

## What it does

- **Joins the fleet** with one instance UID: the OpAMP instance UID, and `service.instance.id` in
  the telemetry. With a state directory it is kept across restarts. Without one, or where the UID
  can't be saved in it, a `service.instance.id` given in `OTEL_RESOURCE_ATTRIBUTES` is used if it
  is a UUID: the same at every start. A new UID
  the server assigns (`new_instance_uid`) is the client's at once and, kept in the state
  directory, the telemetry's from the next start (the resource is made once).
- **Takes a key of its own.** An agent enrolled with a shared key (the enrollment key in
  `OTEL_OPAMP_HEADERS`, or `WithHeaders`) is offered a key of its own by a server that hands them
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
  `service.instance.id` identify it; the rest describes it. Attributes keep their types, arrays
  as arrays.
- **Takes settings** from the remote config's `elastic` entry, **every one but the polling
  interval at once**, for the SDK `Setup` builds (an application that keeps its own SDK setup, or
  sets other global providers after `Setup`, isn't managed):

  | Setting | How |
  |---|---|
  | `sampling_rate` | a sampler in place of the configured one; a span whose parent was sampled, here or by the caller, is sampled at any rate, 0 included. Left out: the configured sampler. `OTEL_TRACES_SAMPLER=jaeger_remote` or `xray` isn't built: those are the default sampler. |
  | `sampling_rules` | the same sampler: for a new trace, the first rule its root span matches decides the rate, before `sampling_rate` ([below](#sampling-rules)). |
  | `sampling_traces_per_second` | the same sampler, last: a token bucket on the traces started or joined ([below](#rate-caps)). |
  | `send_traces`, `send_metrics`, `send_logs` | each signal's exporter, gated: `false` drops what would be exported. Not a Prometheus reader, or any reader while `OTEL_METRICS_PRODUCERS` is set ([below](#where-the-server-cant-reach)). |
  | `send_logs_level` | the log exporter's gate again: from the next export, a record below that severity is dropped; one with no severity (0) is kept. Left out: every severity. |
  | `send_logs_level_by_scope` | the same gate, a level per scope: the name given to the log bridge ([below](#log-levels-per-scope)). |
  | `send_logs_per_second` | the same gate, after the levels: a token bucket on the records exported ([below](#rate-caps)). |
  | `drop_attributes`, `mask_attributes` | the span and log exporters' gates: attributes removed or masked as they are exported ([below](#redaction)). |
  | `debug_until`, `debug_sampling_rate`, `debug_send_logs_level` | until that time, the debug rate and level in place of the rate, the rules and the levels; it ends on a timer, with or without the server ([below](#debug-mode)). |
  | `deactivate_instrumentations` | from then on, the named instrumentations' tracers start no span, and their metrics and log records aren't exported. |
  | `deactivate_all_instrumentations` | the same, for every instrumentation. |
  | `opamp_polling_interval` | at the next start, the interval it checks in at, until the server offers one. |

  `logging_level` and `infer_spans` (EDOT settings) aren't carried out, so the server can't change a
  log level; the package logs them.
- **Doesn't trace itself.** The OpAMP client has an HTTP transport of its own, so an application
  that wraps `http.DefaultTransport` in otelhttp doesn't trace the client's requests.
- **Answers** each remote config APPLIED or FAILED, and reports the settings in force as its
  effective config, with the settings it set aside or didn't know named beside them in an
  `elastic.set_aside` entry ([sdk-settings.md](sdk-settings.md#the-answer)).
- **Says goodbye** (`agent_disconnect`) in `Shutdown`, and lets a config being applied finish
  (as long as `Shutdown`'s context allows), before the SDK flushes.
- **Never holds the application up.** If the settings can't be loaded or the client can't start,
  it logs why and the SDK runs unmanaged. `Setup` fails only for an exporter the `OTEL_*`
  variables describe that can't be built, and then leaves the global providers as they were.

The rules are the SDK settings layer's (`go/opamp/sdk`, in wazero), the same in every language.
The package only carries them out on the Go SDK.

### Switching instrumentations off

Go instrumentations take their tracer from the global provider, by scope name, or from the
provider of the span in the context (otelhttp's Transport does), so `Setup` can switch one off
while the application runs: every span the provider returns gives it back as its
`TracerProvider`, and a switched-off instrumentation's tracer hands back the parent's span
context instead of starting a span. The trace goes on through the instrumentations still on, and
propagation doesn't change. An instrumentation is named by its scope name's last path element:
`otelhttp` for `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`, `otelgrpc`,
`otelsql`. A name without a path (`checkout`) is a scope name as is. A full scope name can't be
typed (the settings take no `/`), and a few instrumentations' scopes end in something other than
their package: otelhttptrace's is `httptrace`.

Its instruments were made when the application started and go on recording; their data is left
out of each push export (a Prometheus reader still serves them). A log bridge's records are left
out the same way, by the scope its logger is given, which is the name the application picks, not
the bridge's package (`otelslog`).

Go draws no line between instrumentation and the application's own tracing: the application's
own tracer is a scope like any other. `deactivate_all_instrumentations` switches off every scope,
the application's own too. To keep the application's own spans, list the instrumentations.

### Where the server can't reach

- A metrics exporter autoexport builds a reader for that isn't a push exporter (`prometheus`),
  or any metrics exporter while `OTEL_METRICS_PRODUCERS` is set, is autoexport's own: an SDK
  reader can't be wrapped outside the SDK. `send_metrics` can't stop it: the package warns, and
  the setting is ignored rather than answered as done ([the host](sdk-settings.md#the-host)).
- An SDK the application builds itself, or global providers it sets after `Setup`: none of the
  settings reach them.
- A tracer taken from a provider other than the global one (`sdk.TracerProvider` is the global
  one) or a span it started isn't switched.

### Inventory

The package reports what the instance runs as its available components, the same shape in every
language ([available-components.md](available-components.md#what-the-sdk-integrations-report)):
this package (`github.com/pydantic/opamp-clients/go/otel`) and its version, `telemetry.sdk.language`
`go` and the SDK's version (`sdk.Version()`), the instrumentations, the exporters, and the
settings it carries out. For example, with otelhttp, a log bridge and a Prometheus reader:

```json
{"instrumentations": {"otelhttp": {"version": "0.71.0"}, "checkout.db": {}},
 "exporters": {"traces": {"otlp": {"gated": true}}, "metrics": {"prometheus": {"gated": false}},
               "logs": {"otlp": {"gated": true}}}}
```

- **Instrumentations** are the scopes that have taken a tracer, a meter or a logger from the
  global providers `Setup` set, switched off or not, each under the name
  `deactivate_instrumentations` takes ([above](#switching-instrumentations-off)): its scope name's
  last path element. Go instrumentations are libraries the application wires in, so one is listed
  once it takes its first tracer, meter or logger, often after `Setup` has returned; the list is
  then sent again. The application's own scopes are listed too, being scopes like any other. The
  version is the one the instrumentation gives (`WithInstrumentationVersion`; otelhttp's is its
  own), else that of the Go module its scope is in, from the binary's build info
  (`runtime/debug.ReadBuildInfo`); none for a module built from a checkout or replaced by a
  directory. Our own version comes from build info the same way. At most 4096 scope names are
  kept, the first by name, each with at most 16 versions, the first sorted, so an application
  that names a tracer per request doesn't grow the list and every instance lists the same set.
- **Exporters** are the one `Setup` built for each signal, named as `OTEL_*_EXPORTER` names it
  (`otlp` when unset); `none` lists none. `gated` is whether its gate wraps it: always for spans and
  logs, and for metrics exactly when `send_metrics` is carried out, so a Prometheus reader, or any
  reader while `OTEL_METRICS_PRODUCERS` is set, is `gated: false` ([above](#where-the-server-cant-reach)).
- **Settings** are those of the host the layer is given at `Load` and `Decide`, with the same clock.

## The settings that match telemetry

What each setting means is [sdk-settings.md](sdk-settings.md#settings-that-match-telemetry)'s,
the same in every language. This is where the package carries each out on the Go SDK, and what
to send. Each one is declared to the layer ([the host](sdk-settings.md#the-host)), in every configuration `Setup` builds,
so the layer puts it in force and reports it; each takes effect at once and reverts at once when
left out. The documents below are the `elastic` entry of a remote config, as the server sends it.

### Sampling rules

```json
{
  "sampling_rate": "0.1",
  "sampling_rules": [
    {"rate": 0, "kind": "server", "path": "/health*"},
    {"rate": 0, "kind": "client", "name": "GET /health*"},
    {"rate": 1, "name": "checkout *"}
  ]
}
```

In the live sampler (`LiveSampler.ShouldSample`), from the `SamplingParameters` the SDK hands
it: the name the span starts with, its kind, and the attributes given at start. For a root span
the first rule that matches decides, by the SDK's own `TraceIDRatioBased`; else `sampling_rate`;
else the sampler `OTEL_TRACES_SAMPLER` configured. With rules or a rate set, a span with a parent
follows it (`ParentBased`), so traces stay whole. What Go instrumentation gives at start:

- otelhttp's handler (server spans) gives `url.path` and the method at start. It names the span
  `GET`, or `GET /users/{id}` when the request already has its `http.ServeMux` pattern (the
  handler wraps the mux's handler, not the mux), and the route is otherwise set later. Match
  server spans by `path` and `kind`.
- otelhttp's transport (client spans) gives nothing at start: it adds `url.full` and the rest
  after the span has started, too late for a rule, and names the span `HTTP GET`. To sample
  outgoing calls by where they go, name them by it, as the second rule expects:
  `otelhttp.NewTransport(rt, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path }))`
  (examples/go-otel does).
- A span the application starts itself: whatever it passes with `trace.WithAttributes` at
  `Start` (not what it sets later) and its own name.

### Log levels per scope

```json
{"send_logs_level": "warn", "send_logs_level_by_scope": "checkout.db*=debug, noisy=off"}
```

In the log exporter's gate, next to `send_logs_level`: a record's level is the entry for the
scope it is exported under, else `send_logs_level`'s. In Go that scope is the name the
application gives its log bridge, `otelslog.NewLogger("checkout.db")`, one per bridge logger, not
one per slog logger or per `With` group. Above, `checkout.db` and `checkout.dbpool` export from
debug up, `noisy` nothing at all, and every other bridge from warn.

### Redaction

```json
{"drop_attributes": "http.request.header.authorization, http.request.header.cookie",
 "mask_attributes": "*password*, *token*, user.email"}
```

In the span and log exporters' gates. A span's attributes, its events' and its links' are
redacted on a view of the SDK's span (a type that embeds `sdktrace.ReadOnlySpan` and gives the
redacted ones), so the span processor and any other exporter still see the original. A log
record is redacted on a clone (`Record.Clone`, then `SetAttributes`). Masked values are the
string `[REDACTED]`, whatever their type. Resource and scope attributes, span names, log bodies
and metrics are left alone, as everywhere.

One Go difference: the log SDK can only set a record's attributes all at once, which also sets
its count of attributes dropped by the SDK's limits to 0. A record that had attributes dropped by
`OTEL_LOGRECORD_ATTRIBUTE_COUNT_LIMIT` and that redaction changes is exported as if none were.
Spans keep their count.

### Rate caps

```json
{"sampling_traces_per_second": "50", "send_logs_per_second": "200"}
```

`sampling_traces_per_second` is a token bucket in the live sampler, after the rules and the rate
said to sample: a root span takes a token, and so does a span joining a caller's sampled trace
(a remote parent); without a token it is dropped, children and all. A child of a span sampled in
this process takes none and stays sampled, so a trace is never cut in two.

`send_logs_per_second` is a token bucket in the log exporter's gate, after the levels, on Go's
monotonic clock: of each batch, the first records the tokens allow go on and the rest are
dropped. A bucket holds a second's worth (at least one record) and starts full. The batch
processor exports every second by default (`OTEL_BLRP_SCHEDULE_DELAY`), so 200 lets 200 records
a second through; a longer delay makes the cap stricter. The same cap sent again (a reconnect, a
restart) keeps its bucket; a different one starts a full bucket; left out, there is no cap from
the next export.

### Debug mode

```json
{"sampling_rate": "0.05", "send_logs_level": "warn",
 "debug_until": "2026-10-14T15:30:00Z", "debug_sampling_rate": "1", "debug_send_logs_level": "debug"}
```

The layer decides everything; the package passes it the time (`LoadAt`, `DecideAt`, with
`time.Now()`) and runs one timer (`time.AfterFunc`). Until `debug_until` the settings the layer
hands over have the debug rate and level folded in (the rules and the levels per scope set
aside; redaction, the caps and the `send_*` switches as sent). Each answer says when to ask
again (`refresh_at_unix_ms`); the timer is armed for it, in place of any earlier one, next to the
decision and under the same lock, and a timer a newer decision replaced does nothing. When it
goes off the package calls `Refresh`, puts what it returns in force, and reports the effective
config, which no longer has `debug_until`: the server sees debug mode end, but isn't needed for
it. The wait is on Go's monotonic clock, so a wall clock moved back while it runs doesn't make
it longer. A restart before the end, with the saved document, is still in debug mode until the
same time; without a state directory the server sends the config again, and the window, being
a time, is the same.

### Export health

No setting: always on. Each gate counts, after the exporter's own retries, the exports that
succeeded and failed (with the exporter's error), the spans, metric data points or log records
in failed ones, and the items it dropped on purpose (`send_*` false, a switched-off
instrumentation, a level, a cap). The agent's health at start, and then every 30 s on the same
timer as debug mode, carries what the layer's `Health` call makes of them, under
`component_health_map["exporters"]`, with one child each for `traces`, `metrics` and `logs`:

```json
{"healthy": true, "component_health_map": {"exporters": {"healthy": false, "status": "failing",
  "last_error": "traces: connection refused", "component_health_map": {
    "traces": {"healthy": false, "status": "failing", "last_error": "connection refused",
               "attributes": {"exports.ok": 3, "exports.failed": 1, "items.failed": 512, "items.filtered": 0}},
    "metrics": {"healthy": true, "status": "ok"}, "logs": {"healthy": true, "status": "off"}}}}}
```

The top-level `healthy` stays the application's, true while it runs. The layer says when to send:
at once when a status changes, at most every 60 s while only the counters move, never while
nothing changes. Records a full batch queue drops never reach a gate, so they aren't counted
(the Go SDK keeps that count private). A metrics reader that isn't gated (Prometheus) isn't
counted either: its child stays `ok` at zero.

## The application's own settings

The server may send the application settings of its own in `custom` (a feature flag, a
verbosity), which the package hands on rather than reads ([app-settings.md](app-settings.md)):

```go
if opampotel.AppSetting("checkout.new_flow", "off") == "on" {
	// ...
}
limit := opampotel.AppSettingInt("checkout.max_items", 50)

changes := make(chan []string, 16)
remove := opampotel.OnAppSettingsChange(func(settings map[string]string, changed []string) {
	select {
	case changes <- changed: // the work is done on the application's own goroutine
	default:
	}
})
defer remove()
```

`AppSetting(key, def)` is the value in force or `def`, never an error: before the first config,
before `Setup`, without an OpAMP server and when the config in force leaves the key out.
`AppSettingBool`, `AppSettingInt` (`int64`) and `AppSettingFloat` (`float64`) read one typed, by
the rules every language shares, and are `def` for anything else; `AppSettings()` is every value
in force, a copy of the caller's own. A function given to `OnAppSettingsChange` gets the values in
force (its own copy) and the keys added, changed or removed (sorted), once per change, in order,
one at a time, on the OpAMP client's goroutine: keep it quick, and don't call `SDK.Shutdown` from
it. A panic in it is recovered and logged as an error; the others are still called and the config
is still APPLIED. The function it returns removes it. Any goroutine may read, subscribe and
unsubscribe at any time, and a reader sees the whole of one map or the whole of the next.

`Setup` declares `custom` always. With a state directory the values in force are saved and loaded
again at the next start inside `Setup`, before it returns: from the application's first line
after `Setup` they are in force, whether the server can be reached or not, and a function added
after `Setup` is told of the server's changes from then on, not of those. `examples/go-otel/main.go`
reads the flag `example.feature` and requests `/changed` from its own loop for each change it is
told of.

## How it is checked

- Unit tests (`go/otel`, with `-race`):
  - the sampler, the gates and the switches on a real SDK, with settings applied while spans start;
  - a switched-off instrumentation keeps the trace going, and is switched off too when it takes
    its tracer from the span in the context;
  - the answers, saving and reporting follow the layer;
  - the UID is kept across starts, a UID the server assigns too;
  - options given in code win over the variables, headers replace them, and an endpoint in code
    alone turns the package on; each option against its variable (neither, either, both, blank),
    and a value that can't be used warns and is the default, in code or in the variables;
  - without an endpoint nothing changes, an unreachable server or an unreadable heartbeat
    interval holds nothing up, and a failed `Setup` leaves the globals alone;
  - `Shutdown` waits for a config being applied;
  - connecting, failing and recovering are each logged once, without the endpoint's credentials;
    the warning names what gave the endpoint and the headers: `WithEndpoint` and `WithHeaders`
    when given in code, else the variables;
  - the shared vectors (`testdata/sdk-settings`), each through this package's own
    code, the settings documents through the layer as its host declares them:
    `sampling.json` and `caps.json` through the sampler, `logs.json`, `caps.json` and
    `redact.json` through the span and log gates, `debug.json` and `health.json` through the
    agent and its timer on a fake clock; and the sampler, the levels, redaction and the log cap
    against the layer's own matchers (`Evaluate`) on generated inputs;
  - the inventory a typical setup reports reaches a server, with its hash: each name one
    `deactivate_instrumentations` takes, versions given or from build info, a Prometheus reader
    `gated: false` while `send_metrics` is left out, the settings the host's; a scope taken
    later is sent again, once; no instrumentation, every exporter `none`, and duplicates;
  - each setting's edge cases from [sdk-settings.md](sdk-settings.md), live changes and
    reverting, and a second, ungated exporter seeing the application's own telemetry;
  - the application's own settings (`appsettings_test.go`): reads before any config, without a
    server and once a key is left out; the typed reads against `custom.json`'s `typed` vectors and
    their edges; every `custom.json` case through the layer and `Controls`; a function told once
    per change and never for the same map (sent again, another way, by a Refresh at the end of
    debug mode), in order, one at a time, each with its own copy; a panic recovered and logged;
    unsubscribing, also from inside a function; readers and subscribers on other goroutines; a bad
    map FAILED with nothing changed; restarts with and without a state directory, through `Setup`
    too; and the examples (`example_appsettings_test.go`).
- End to end: `python3 examples/go-otel/try.py` (CI's `go-otel` job), the checks every
  integration passes ([javaagent.md](javaagent.md#how-it-is-checked), `examples/integrations.py`),
  twice: the server given by `OTEL_OPAMP_*`, then in code, with none of them set.
  The second time, `setup-in-code` gives `WithHeaders` the wrong token and then the right one,
  each time with the other token in `OTEL_OPAMP_HEADERS`. The token given in code wins both times.
  The wrong token's warning names `WithHeaders`, not `OTEL_OPAMP_HEADERS`.
  The example wraps `http.DefaultTransport` in otelhttp, so the check that the client's own
  requests aren't traced has something to catch; it fails if the client shares that transport.
  Its `check_settings` then checks the settings that match telemetry against the reference
  server and its telemetry sink: a sampling rule leaving out `/health`, a scope switched off, a
  dropped and a masked attribute, both caps, a debug window that ends with the server sending
  nothing more, and health sent again after trace exports start to fail. Last,
  `check_app_settings` ([app-settings.md](app-settings.md#how-it-is-checked)): the server turns
  `example.feature` on and off, a bad map is FAILED and changes nothing, the same map again tells
  nothing, and the next start has the saved value from the start.
