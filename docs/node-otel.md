# OpenTelemetry auto-instrumentation for Node.js, under an OpAMP server

`node/otel` is `@pydantic/opamp-otel`, upstream's auto-instrumentation `register` under an OpAMP
server. It sets up what `@opentelemetry/auto-instrumentations-node/register` sets up: NodeSDK,
every auto-instrumentation, configured by the `OTEL_*` variables. The SDK then joins the server's
fleet and takes its settings from it: sampling, what to send and which instrumentations are on,
while it runs ([sdk-settings.md](sdk-settings.md)). It reaches the SDK this
package sets up. A provider the application builds on its own isn't managed: where the application
registers it as the global one, the package sets aside the settings it can no longer carry out
([below](#a-provider-of-the-applications-own)). The application needs no code. It works for CommonJS and ES modules alike:
the register installs the instrumentations' loader hook for `import`, which upstream leaves to a
separate `--experimental-loader` flag. The hook runs in the application's thread where Node.js
can run it there (22.22.3, 24.11.1, 25.1.0, 26 and later), and on a loader thread of its own
before that, which costs about 7 MB more.

Checked with `@opentelemetry/sdk-node` 0.222, `auto-instrumentations-node` 0.80 and Node.js 22 and
later.

## Use it

Build it in a checkout of this repository (until it is published), and install it into the
application:

```bash
OPAMP=/path/to/opamp   # this repository
(cd $OPAMP/node/opamp && npm ci && npm run build) && (cd $OPAMP/node/otel && npm ci && npm run build)
npm install $OPAMP/node/otel   # in the application's directory
```

Then start the application with it instead of upstream's `register`:

```bash
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
node --import @pydantic/opamp-otel/register app.js
```

| Variable | |
|---|---|
| `OTEL_OPAMP_DISABLED` | `true` turns OpAMP off: no client starts, whatever else is given (the endpoint given to `register` too), and the SDK runs on its own settings. This is the way to take an application off the server in an incident: set it and restart, no deploy. One warning says so (`OpAMP: off, as OTEL_OPAMP_DISABLED=true says: no OpAMP client is started, whatever the code gives`). `false` or blank leaves it to the rest; any other value (`1`, `yes`) is taken as `true`, and the warning says how to write it. |
| `OTEL_SDK_DISABLED` | OpenTelemetry's own: `true` turns OpAMP off too, read as `OTEL_OPAMP_DISABLED` is (here `1` is taken as `true` as well). What the SDK does with it is upstream's (it reads only `true` as off). |
| `OTEL_OPAMP_ENDPOINT` | The OpAMP server: `http(s)://.../v1/opamp` polls, `ws(s)://...` keeps a connection open. Without it, or with OpAMP off (above), this is upstream's `register`. |
| `OTEL_OPAMP_HEADERS` | Headers for every request, as `OTEL_EXPORTER_OTLP_HEADERS`: `name=value,...`, values percent-encoded. The enrollment key goes here. |
| `OTEL_OPAMP_STATE_DIR` | Where to keep state across restarts: the instance UID, the settings for the next start, the client's own state (written readable by the application's user only). Without it, the package keeps one of the user's own, one per process of the program running at once ([default-state-dir.md](default-state-dir.md)); `none` turns that off, and then each start is a new instance. A directory named here belongs to one process: two processes given the same one are the same instance. |
| `OTEL_OPAMP_HEARTBEAT_INTERVAL` | How often to check in until the server offers an interval (default 30 s): milliseconds, or `30s`, `2m`. A value it can't read is warned about and the default used; longer than a day is a day; it never stops the application. |

The core reads these (and the options below), by the same rules in every language: whether OpAMP is off, the endpoint, the headers, the interval, which state directory, and the instance UID kept there. The package keeps only the I/O: reading the variables, making and locking the state directory's slot, reading and writing its `instance-uid` file, and logging the warnings.

### In code

The same four can be given to `register`, called before the application loads what it
instruments. What is given in code wins over its variable; what isn't still comes from the
variables. Headers given replace `OTEL_OPAMP_HEADERS` whole, and `stateDir: "none"` turns the
default state directory off, as the variable's `none`.

```js
import { register } from "@pydantic/opamp-otel";

const { shutdown } = register({
  endpoint: "https://opamp.example.com/v1/opamp",
  headers: { Authorization: `Bearer ${process.env.ENROLLMENT_KEY}` },
  heartbeatIntervalMs: 30_000,
});
```

Called this way, the shutdown and the loader hook for `import` are the application's to arrange;
`node --import @pydantic/opamp-otel/register` does both, and reads only the variables. A blank
`endpoint` or `stateDir` is not given (its variable applies). A value it can't use never stops the
application: one of the wrong type (as plain JavaScript may pass, or a `heartbeatIntervalMs` that
isn't a finite number) is warned about and not used; a `heartbeatIntervalMs` of 0 or less is warned
about and the default, a fraction of a millisecond is cut off, and longer than a day is a day.
`register` again once `shutdown` has finished (a test harness, a reload) is the same instance:
the client starts again, with the same state directory and instance UID.

[examples/node-otel/in-code.mjs](../examples/node-otel/in-code.mjs) is a whole program that takes
the server from its own settings, run with plain `node` and no `OTEL_OPAMP_*` variable;
`python3 examples/node-otel/try.py` runs it against the reference server, as CI does.

As upstream's `register`, it shuts down on SIGTERM and when Node.js exits normally. The OpAMP
client doesn't keep Node.js running: when the application's work is done, it exits as it would
without the register, after the goodbye. On a SIGTERM, an application with its own SIGTERM
listener still decides when to exit; one without ends as Node.js would have ended it (by the
signal), once the goodbye is said and the telemetry flushed. Upstream's `register` leaves such a
process running.

## What it does

- **Joins the fleet** with one instance UID: the OpAMP instance UID, and `service.instance.id` in
  the telemetry. With a state directory it is kept across restarts, and a UID the server assigns
  is the one the next start uses (this run's telemetry keeps the one it started with). The UID is
  handed to the SDK's resource, not put in the environment, so a child process doesn't take it.
  Without a state directory, or where the UID can't be saved in it, a `service.instance.id` given
  in `OTEL_RESOURCE_ATTRIBUTES` is used if it is a UUID: the same at every start.
- **Takes a key of its own.** An agent enrolled with a shared key (the enrollment key in
  `OTEL_OPAMP_HEADERS`, or the headers given to `register()`) is offered a key of its own by a
  server that hands them out ([connection-settings.md](connection-settings.md)). With a state
  directory the agent says it takes OpAMP connection settings (`AcceptsOpAMPConnectionSettings`,
  `ReportsConnectionSettingsStatus`) and takes the offer: it tries the new key (APPLYING), keeps it
  once the server answers with it (APPLIED; the server may then refuse the shared key for it), and
  goes back to the key it had if the server doesn't (FAILED, saying why). The key is kept in the
  state directory, readable by the application's user only, and the next start comes back with it;
  changing the endpoint or the headers it is given starts again from them. An offer may carry an
  interval to check in at (the server's heartbeat interval), which holds until the next start.
  Without a state directory a restart would lose the key, so the agent doesn't say it takes one.
- **Describes itself** from the SDK's resource, detected once (the same detectors as upstream's
  `register`) and handed to NodeSDK. `service.name`, `service.namespace` and
  `service.instance.id` identify it, and the rest describes it. The description also names the
  policy format the package reads (`logfire.opamp.config_format`)
  and the controls this process carries out (`logfire.opamp.supported_controls`).
- **Reports its inventory**: this package's and the SDK's versions, the instrumentations it loaded
  and the exporters it built, as its available components ([below](#inventory)).
- **Takes settings** from the Logfire SDK policy, the remote config's only file
  (`logfire-sdk-policy.json`). Each control is `{"mode": "local"}`, the SDK's own value, or an
  override:

  | Control | How |
  |---|---|
  | `head_sampling` | a sampler in place of the one `OTEL_TRACES_SAMPLER` names. A span whose parent was sampled, here or by the caller, is sampled at any ratio, 0 included. Local: that sampler. |
  | `sampling_rules`, `trace_rate_limit` | the same sampler: a rate per kind of new trace, and at most this many traces a second ([below](#sampling-rules)). |
  | `signal_exports.traces`, `signal_exports.metrics`, `signal_exports.logs` | each signal's exporters this package builds (OTLP and `console`, below), wrapped. `enabled: false` drops what would be exported. Spans are still made and sampled, so services downstream still trace them. |
  | `log_levels` (`level`) | the same log exporters, wrapped. A record whose severity number is below the level's is dropped from the next export on, and one without a severity number is kept. Local: every severity. |
  | `log_levels` (`scopes`) | the same log exporters: a level per scope, over `level` ([below](#log-levels-per-scope)). |
  | `log_rate_limit` | the same log exporters: at most this many records a second ([below](#rate-caps)). |
  | `attribute_redaction` | the span and log exporters: attributes removed or masked at export ([below](#redaction)). |
  | `scrubbing` | the same exporters: values that look secret replaced at export ([below](#scrubbing)). |
  | `debug` | until a time, another sampling ratio and log level, then back on its own ([below](#debug-mode)). |
  | `instrumentations` (`disabled`) | from then on, the named instrumentations' tracers start no span, and their metrics and log records are not exported ([below](#switching-instrumentations-off)). Names are upstream's short ones: `http`, `express`, `pg`, and so on. `http`, `net` and `dns` all trace an outgoing HTTP request. |
  | `instrumentations` (`all_disabled`) | the same, for every instrumentation the package loaded. The application's own tracers are not switched. |
  | `app_settings` | to the application, at once. See [the application's own settings](#the-applications-own-settings). |

  A policy that overrides a control this process does not carry out is refused whole (FAILED),
  and the error names the control. A field the policy format does not know is refused too.
- **Doesn't trace itself.** The OpAMP client runs with tracing suppressed, so its requests (and its
  goodbye) never show up as the application's spans.
- **Answers** each remote config APPLIED or FAILED, and reports the policy in force as its
  effective config, one `logfire-sdk-policy.json` file ([sdk-settings.md](sdk-settings.md#the-answer)).
- **Says goodbye** (`agent_disconnect`) on shutdown, before the SDK flushes.

NodeSDK builds its exporters from the environment internally, so this package builds the ones it
knows the same way and wraps them: OTLP over `grpc`, `http/json` or `http/protobuf`, `console`
and `none`. It reads the variables as NodeSDK 0.222 does: an empty variable is an unset one, a
name listed twice is one exporter, `none` is read as NodeSDK reads it (for traces, `none` among
other names means the default `otlp` alone), and `OTEL_METRIC_EXPORT_INTERVAL`,
`OTEL_METRIC_EXPORT_TIMEOUT`, `OTEL_BSP_*` and `OTEL_BLRP_*` fall back to their defaults when not a
number above 0, and an OTLP protocol it doesn't know is `http/protobuf`, with a warning. A signal configured with anything else (`zipkin`, `prometheus`) is left to NodeSDK
and cannot be stopped by the server. The package logs it, and refuses a policy that overrides
that signal's `signal_exports` (and for logs, `log_levels`) ([the host](sdk-settings.md#the-host)). What the package logs goes
to the logger `OTEL_LOG_LEVEL` sets up, from the start. Without `OTEL_LOG_LEVEL`, OpenTelemetry's
`diag` logger shows nothing, so the package's own warnings and errors (`OpAMP: ...`) go to the
console (stderr) instead, and its INFO lines aren't shown.

The rules are the SDK settings layer's (`@pydantic/opamp-client/sdk`), the same in every
language. The package only carries them out on the Node.js SDK.

### Switching instrumentations off

The package loads every instrumentation the variables load (`OTEL_NODE_ENABLED_INSTRUMENTATIONS`,
`OTEL_NODE_DISABLED_INSTRUMENTATIONS`, upstream's defaults), whatever the server says, and once
NodeSDK has started hands each one a tracer provider of its own. While the instrumentation is
switched off, its tracer starts no span: it hands back a span that only carries the parent's span
context, so the trace goes on through the instrumentations still on, and propagation doesn't
change. Switched on again, its next span is recorded. The switch is read at each span, so it takes
effect at once, and the saved policy switches the same instrumentations off from the next start.

- **Loaded, not removed.** A switched-off instrumentation's patches stay in place; they only record
  nothing. To keep an instrumentation from loading at all, use `OTEL_NODE_DISABLED_INSTRUMENTATIONS`.
- **Only what the variables load.** One they leave out (`fs` and `host-metrics` are off by
  default) isn't loaded, so the server can't switch it on: it can only switch instrumentations off.
  A name that matches no loaded instrumentation is answered APPLIED and changes nothing; the
  package logs it at `info`.
- **Metrics and log records** are left out at export. An instrumentation's instruments were made at
  start and go on recording; with cumulative temporality, the counts made while it was off come
  back in the totals once it is on again. A log bridge's records (`pino`, `bunyan`) are left out by
  their scope, the instrumentation's name. Only the exporters this package builds leave them out:
  a `prometheus` reader still serves them.

### A provider of the application's own

The package's sampler and gates are inside the SDK's providers. What the application sends through
a tracer, logger or meter provider it builds itself goes past them.

- **Registered as the global one.** The API holds one global provider per signal, and refuses
  another until the first is disabled (`trace.disable()`, `logs.disable()`, `metrics.disable()`).
  When the application then registers its own (`trace.setGlobalTracerProvider`,
  `provider.register()`, `logs.setGlobalLoggerProvider`, `metrics.setGlobalMeterProvider`), the
  package sees it at once. It warns, and no longer carries out the controls that provider takes
  past it. In the policy in force, each of their overrides is set aside: reported `local` in the
  effective config, which the package reports to the server at once, with the inventory's
  settings. The rest of the policy stays in force. A policy offered from then on that overrides
  one is refused (`head_sampling: this SDK cannot apply the requested override`). A provider the
  application registered before `register()` is seen the same way, as the SDK's is refused.

  | The application's own | Set aside from then on |
  |---|---|
  | tracer provider | `head_sampling`, `sampling_rules`, `trace_rate_limit`, `debug` (`sampling_ratio`), `signal_exports.traces`, `attribute_redaction`, `scrubbing` |
  | logger provider | `signal_exports.logs`, `log_levels` (`level` and `scopes`), `log_rate_limit`, `debug` (`log_level`), `attribute_redaction`, `scrubbing` |
  | meter provider | `signal_exports.metrics` |

- **The instrumentations keep the SDK's providers.** Each got its providers when NodeSDK started.
  So their spans are still sampled and gated, and the server still switches them. But a control
  that no longer reaches all of the application's telemetry isn't answered as carried out. The
  controls stay set aside until the next start.
- **Not registered, not seen.** A provider the application builds and uses directly
  (`provider.getTracer("app")`) goes past the package unseen, and a policy that overrides its
  controls is still answered APPLIED. The same holds for one registered through another copy of
  `@opentelemetry/api` or `@opentelemetry/api-logs`: every copy shares the one global, but the
  package watches its own copy's setters. npm installs one copy where the versions agree.
- **No processor added later.** The SDK's providers take no span or log record processor once
  they are built, so nothing joins them past the gates.

### Is it working?

The package says so on the logger `OTEL_LOG_LEVEL` sets up; without it, its warnings go to stderr
and its INFO lines aren't shown (`OTEL_LOG_LEVEL=info` shows them). When the client first reaches
the server it logs, at INFO, `OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is
managed as checkout, instance <uid>`. When it can't, it logs one WARNING per run of failures, with
what to check:

```
OpAMP: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused this agent's
credentials: check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). Retrying in the background;
the SDK keeps its current settings meanwhile
```

and at INFO, `connected to ... again` once it's back. The warning names what to check as it was
given: with the headers given to `register()`, it says `check the headers given to register()`,
and likewise for the endpoint. The endpoint is logged without user info or a query string. An
endpoint without a scheme is refused at start: no OpAMP client starts, the SDK runs unmanaged, and the warning says why.

## The settings that match telemetry

These controls act on what the sampler and the exporters this package builds see (OTLP and
`console`). [sdk-settings.md](sdk-settings.md#settings-that-match-telemetry) has their contract.
The package declares each to the layer ([the host](sdk-settings.md#the-host)) only where it
carries it out. Thus a policy that overrides a control the package cannot carry out is FAILED,
never APPLIED. Each example below shows the
controls a policy (`logfire-sdk-policy/v1`) overrides. Every control left out of an example is
local.

### Sampling rules

`sampling_rules` sets the sampling rate per kind of new trace, before `head_sampling`. The first
match wins:

```json
{"head_sampling": {"mode": "override", "ratio": 0.1},
 "sampling_rules": {"mode": "override",
                    "rules": [{"rate": 0, "kind": "server", "path": "/health*"}, {"rate": 1, "name": "checkout *"},
                              {"rate": 0.5, "attributes": {"tenant.id": "acme"}}]}}
```

- In the package's sampler, for a span with no parent: the first rule it matches decides the rate,
  applied by the SDK's `TraceIdRatioBasedSampler`; if none matches, `head_sampling`; if that is not
  set either, the sampler `OTEL_TRACES_SAMPLER` names. A span with a parent follows it, sampled or
  not, here or in the caller, whatever the rules say.
- A rule sees what the span has when it starts. The `http` instrumentation starts a server span
  named `GET` (the method), with `url.path` and the other request attributes; Express adds
  `http.route` later, so the sampler never sees it. Match incoming requests by `kind` and `path`,
  not `name` or `http.route`. An outgoing request's span (`kind` `client`) has no path, only
  `url.full`: match it under `attributes`, as `{"rate": 0, "kind": "client", "attributes":
  {"url.full": "*/health*"}}`.
- An attribute matches by its text: a string's own, an integer's decimal, `true` or `false`. In
  JavaScript a number that is an integer (`1.0` too) is an integer, as OTLP exports it; another
  number, or an array, never matches.

`trace_rate_limit` caps the traces this process starts or joins, after the rules and the
rate have said to sample:

```json
{"head_sampling": {"mode": "override", "ratio": 1},
 "trace_rate_limit": {"mode": "override", "traces_per_second": 50}}
```

- A root span the sampler sampled takes a token, and so does a span whose remote parent (a traced
  caller) was sampled; without a token it isn't sampled, so its children aren't either. A child of
  a span sampled in this process is always sampled and takes none: the cap refuses whole traces.
  Nor does a span sampled under a remote parent that wasn't (the application's own sampler may sample
  one when only a cap is set).
- The same bucket as `log_rate_limit` ([rate caps](#rate-caps)), on its own.
- Both are declared whatever the exporters: the sampler is always this package's. But it samples
  only the SDK's spans: once the application registers a tracer provider of its own, the
  sampler's controls are set aside ([above](#a-provider-of-the-applications-own)).

### Log levels per scope

`log_levels` `scopes` sets a level per log scope, on top of `level`:

```json
{"log_levels": {"mode": "override", "level": "warn",
                "scopes": {"@opentelemetry/instrumentation-pino": "error", "app*": "debug", "noisy": "off"}}}
```

- The scope is the name a record is exported under. For a record the application emits through
  the logs API it is the logger's name (`logs.getLogger("app")`); for pino, winston and bunyan it
  is the bridge's package (`@opentelemetry/instrumentation-pino`), not the application's logger
  name, which becomes an attribute. So in Node.js a level is per logs API logger, or per bridge.
- A record's level is its scope's exact entry, else the longest prefix entry (`app*`), else
  `level`. `off` drops every record of the scope, severity 0 too. A scope's level may be
  lower than `level` as well as higher.
- Filtered at the log exporters' wrapper, from the next export on, after `signal_exports.logs`
  and before a rate cap. Where a log exporter is left to NodeSDK, the package does not declare
  `log_levels`, so a policy that overrides it is FAILED.

### Redaction

`attribute_redaction` keeps attributes out of what is exported:

```json
{"attribute_redaction": {"mode": "override",
                         "drop": ["http.request.header.authorization", "http.request.header.cookie"],
                         "mask": ["*password*", "*token*", "user.email"]}}
```

- Globs on attribute keys, ignoring ASCII case. A key `drop` matches is removed. A key only
  `mask` matches is exported as `"[REDACTED]"`, whatever its type.
- Span attributes, span event and span link attributes, and log record attributes, at the span and
  log exporters' wrappers. The application's span or record isn't changed: the wrapper exports an
  object over it with the redacted attributes, so a processor or exporter the application added
  itself still sees the original. Names, log bodies, resource and scope attributes, and metrics
  are left alone.
- Declared only when both the span and the log exporters are this package's (OTLP, `console` or
  `none`). With either left to NodeSDK, a policy that overrides `attribute_redaction` is FAILED,
  so the control is never half carried out. The same holds once the application registers a
  tracer or logger provider of its own, and an override already in force is then set aside.

### Scrubbing

```json
{"scrubbing": {"mode": "override", "defaults": true, "patterns": ["*@*", "card.*"]}}
```

The Logfire SDK's scrubbing, its matchers ported character for character and held to the core's
by the shared vectors. `defaults: true` takes its default patterns (`password`, `secret`, `auth`,
`api key`, `session`, `cookie`, `jwt` and the others), matched anywhere in a key or a string;
`patterns` are globs matched, ignoring ASCII case, against a whole key or a whole string. An
attribute whose key matches is exported with the string `[Scrubbed due to '<match>']` in place
of its value, whatever its type. Otherwise its string values are scrubbed: a string, each string
in a list, and each member of a map, as an attribute of its own. The Logfire SDK's safe keys
(`http.url`, `db.statement`, `exception.message` and the others) are kept as they are.
[sdk-settings.md](sdk-settings.md#scrubbing) has the rules in full.

- At the same wrappers, on what redaction keeps (a masked value is not scrubbed): span, span
  event, span link and log record attributes, and log record bodies (a string, or the strings in
  an array or a plain object). The application's span or record isn't changed. Names, resource
  and scope attributes, and metrics are left alone.
- Declared, and set aside, exactly as `attribute_redaction` is.

### Rate caps

`log_rate_limit` is a safety valve: whatever the levels say, no more log records a second
than this. `trace_rate_limit` is its traces' counterpart ([above](#sampling-rules)).

```json
{"log_rate_limit": {"mode": "override", "records_per_second": 200}}
```

- A token bucket on `performance.now()`: full to start, refilled at n a second, holding at most a
  second's worth (at least one record). Decimals are allowed: `0.5` is a record every 2 s.
- At the log exporters' wrapper, after the levels (a record they drop takes no token): of each
  batch, the first records the tokens allow go on, the rest are dropped. With the default batch
  delay (`OTEL_BLRP_SCHEDULE_DELAY`, 1 s) that is n records a second; a longer delay makes the cap
  stricter.
- A new n starts a full bucket. The same n sent again keeps the bucket it has. With the control
  local, there is no cap from the next export on. It stays in force in debug mode.

### Debug mode

`debug` turns everything up until a time, and the package turns it back down on its own, the
server reachable or not:

```json
{"head_sampling": {"mode": "override", "ratio": 0.05},
 "log_levels": {"mode": "override", "level": "warn"},
 "debug": {"mode": "override", "until": "2026-10-14T15:30:00Z", "sampling_ratio": 1, "log_level": "debug"}}
```

- Until then the sampler samples at `sampling_ratio` (1 if left out), and the rules do not apply.
  The log gate exports from `log_level` (`debug` if left out), and the levels per scope do not
  apply. The signal exports, redaction and the rate caps stay as they are.
- The layer decides, against this process's clock (`Date.now()`): it folds the debug settings in,
  and says when to ask again. The package runs one timer for that time (unref'd, so it never keeps
  the process alive), waited for on Node's monotonic timer clock; when it fires it asks the layer,
  puts in force what it answers, and reports the new effective config. A newer remote config
  re-arms it; a timer armed before that does nothing.
- A time more than 24 h ahead is FAILED; one already past is applied but over. The same document
  sent again (a reconnect) keeps its end; a restart before the end is still in debug mode (with a
  state directory), after it, not.
- The package carries out `debug` `log_level` only where it builds the log exporters, `until`
  everywhere, and `sampling_ratio` where it carries out `head_sampling`. Elsewhere a policy that
  sets `log_level` or `sampling_ratio` is FAILED.

### Export health

No setting: the package counts what its exporter wrappers see and reports it in the agent's
health, under `component_health_map["exporters"]`, with a child for `traces`, `metrics` and `logs`
([sdk-settings.md](sdk-settings.md#export-health)). Nothing to send from the server; to see it,
read the agent's health:

```json
{"healthy": true, "component_health_map": {"exporters": {"healthy": false, "status": "failing",
  "last_error": "traces: connect ECONNREFUSED 127.0.0.1:4318", "component_health_map": {"traces": {...}}}}}
```

- Each export's result is counted as the exporter's callback gives it, after the OTLP exporter's
  own retries: `exports.ok`, `exports.failed`, `items.failed` (spans, metric data points or log
  records), and `items.filtered`, those dropped on purpose (a signal's exports off, a switched-off
  instrumentation, a level, a cap). A signal is `failing` while its last export failed, `off`
  while its exports are off.
- The top-level `healthy` stays `true`: an exporter that can't reach its backend doesn't make the
  application look down.
- Looked at every 30 s on the agent's one timer (the same as debug mode's end); the layer says
  when to send: at once on a change of status, at most every 60 s while only the counters move.
  A change of status is logged at `info` (`OpAMP: export health: failing (...)`).
- Records a full batch queue drops aren't counted: the SDK only logs them.

## The application's own settings

The server may also send settings of the application's own in the policy's `app_settings`
control (a feature flag, a log verbosity, a limit), which the package hands on rather than reads. The contract (what the server
sends, what is refused, when a callback is called) is [app-settings.md](app-settings.md); in
Node.js the application reads them through `appSettings`:

```js
import { appSettings } from "@pydantic/opamp-otel";
// or, without loading the rest of the package: "@pydantic/opamp-otel/app-settings"
// CommonJS: const { appSettings } = require("@pydantic/opamp-otel");

if (appSettings.get("checkout.new_flow", "off") === "on") {
  // ...
}
const limit = appSettings.getInt("checkout.limit", 100);

const unsubscribe = appSettings.onChange((settings, changed) => {
  // Quick: hand longer work to the application's own loop.
  if (changed.includes("example.log_level")) logger.level = settings["example.log_level"] ?? "info";
});
```

- `get(key, def)` is the value in force, or `def` while there is none: before the first config
  (and no saved one), when the policy in force leaves the key out, and without
  `OTEL_OPAMP_ENDPOINT`. Always a string (or `undefined` with no default given), never an error,
  whatever the key; a key named like one of Object's own (`constructor`, `toString`) is only ever
  a value the server sent.
- `getBool(key, def)`, `getInt(key, def)` and `getFloat(key, def)` read the value typed, by the
  rules every language shares (`true`/`false` in any ASCII case; a decimal integer; a JSON
  number; spaces, tabs, CR and LF round it ignored), and are `def` for anything else. `getInt`
  takes only safe integers (±2^53 - 1): a value beyond them can't be held exactly in a number, so
  it is `def`, where the other languages read the whole 64-bit range.
- `snapshot()` is every value in force, as one frozen object (no prototype) that no later change
  alters.
- `onChange(callback)` returns a function that unsubscribes (calling it again does nothing). The
  callback gets the frozen map now in force and the keys added, changed or removed, sorted, once
  per change, in order, each callback in the order it was added: when a config is applied, at the
  timer's Refresh, at start from the saved settings (before the application's code runs, so an
  application is not told of those: read the snapshot). The same map again, another control
  changing, debug mode's start and end, and a FAILED config tell nothing. A callback that throws,
  or returns a promise that rejects, is logged as an error (on the console without
  `OTEL_LOG_LEVEL`), and the other callbacks are still called and the config still APPLIED. One
  added or removed while a change is told takes effect from the next change. A callback runs as
  part of the OpAMP client's own work, which isn't traced: requests it starts aren't either, so
  hand the work to the application's own code, as the examples do.
- A change is logged at `info`: `OpAMP: application settings changed: <keys>`.
- The package carries out `app_settings` always, whatever builds the exporters. With a state
  directory the map is saved with the rest of the policy and in force again from the start of the next run,
  whether the server can be reached or not; without one, each start begins from the defaults.
- One map per process: the register's and the application's imports share it whichever way each
  loads the package (it is kept on `globalThis`). A worker thread has its own, which nothing
  fills: read the settings on the main thread and pass them on.

## Inventory

No setting: the package reports what the instance runs as its available components, so a
server can see across its fleet which services are on an old SDK, which can run an instrumentation,
and which export through something it can't switch off
([available-components.md](available-components.md#what-the-sdk-integrations-report)). The client
sends the hash at start, and the components in full when the server asks.

```json
{"sdk": {"integration.name": "@pydantic/opamp-otel", "integration.version": "0.1.0",
         "telemetry.sdk.language": "nodejs", "telemetry.sdk.version": "2.11.0"},
 "instrumentations": {"express": {"version": "0.70.0"}, "http": {"version": "0.222.0"}, "...": {}},
 "exporters": {"traces": {"otlp": {"gated": true}},
               "metrics": {"otlp": {"gated": false}, "prometheus": {"gated": false}}},
 "settings": {"sampling_rate": {"applies": "at once"}, "deactivate_instrumentations": {"applies": "at once"}, "...": {}}}
```

- `sdk`: this package's name and version, from its `package.json`, and the SDK's
  `telemetry.sdk.language` and `telemetry.sdk.version`, as the resource has them.
- `instrumentations`: every instrumentation the variables loaded (`OTEL_NODE_ENABLED_INSTRUMENTATIONS`,
  `OTEL_NODE_DISABLED_INSTRUMENTATIONS`), switched off or not, under the short name
  the `instrumentations` control takes (`http` for `@opentelemetry/instrumentation-http`), with its
  version. One not loaded isn't listed: the server can't switch it on.
- `exporters`: each signal's exporters, named as `OTEL_*_EXPORTER` names them, once each, with
  `none` read as NodeSDK reads it. Those this package builds are `gated`; a signal it leaves to
  NodeSDK (a `prometheus` or `zipkin` exporter among them) is listed as NodeSDK builds it, every
  exporter `gated: false`. Its `send_*` setting is not carried out, so it is not under `settings`.
  A name NodeSDK does not know builds nothing, and is not listed.
- `settings`: the settings the package carries out, by the layer's names for them, as it declares
  them to the layer. The instrumentations are switched `at once`. The log settings are there only
  where the package builds the log exporters.
- Worked out once, as the client starts. Only `settings` can change while the application runs:
  a provider of the application's own takes its settings out, and the components are sent again.

## How it is checked

- Unit tests (`node/otel/test`) cover these:
  - the controls change sampling and each signal's exports on a real SDK;
  - upstream's `http` and `undici` instrumentations, as `getNodeAutoInstrumentations` loads them,
    stop and start recording spans as remote configs switch them, and a trace goes on from
    `fetch` (`undici`, on) through a server whose `http` instrumentation is off to the
    application's own span; a switched-off instrumentation's metrics and log records aren't
    exported;
  - the answers, saving and reporting follow the layer, and the description names the policy
    formats and the controls the host carries out;
  - the UID is kept across starts, a UID the server assigns is the next start's, and neither goes
    into the environment;
  - the exporters, metric readers, log batching and sampler are built from the variables as
    NodeSDK builds them;
  - a stop before the client has started means it never starts;
  - connecting, failing and recovering are each logged once, without the endpoint's secrets, and
    a failure reaches stderr without `OTEL_LOG_LEVEL`; the warning names what gave the endpoint
    and the headers: the option given to `register()`, else the variable;
  - the server given in code, a table of every option: in code only, in its variable only, both
    (code wins), neither (the default); blank in code is not given; headers in code replace
    `OTEL_OPAMP_HEADERS` whole; `stateDir: "none"` turns the variable's and the default directory
    off; a value it can't use (no scheme, a negative, NaN or fractional interval, a wrong type)
    warns and never throws;
  - in a real process (`test/register.test.ts`): a program whose work is done exits after the
    goodbye; a SIGTERM ends it by the signal after the goodbye, unless it has its own listener;
    `register()` given the server in code, with no `OTEL_OPAMP_*` variable, polls at its interval
    with its headers and says goodbye, wins over the variables when both are set, and runs on with
    warnings when given values it can't use.
  - the settings that match telemetry (`test/settings.test.ts`): the shared test vectors
    (`testdata/sdk-settings`) through this package's own sampler, gates and agent
    (`sampling.json`, `logs.json`, `redact.json`, `caps.json` on an injected clock, `debug.json`
    step by step with the timer, `health.json` call by call), and each setting's edge cases on a
    real SDK: live changes and reverting, a second exporter seeing the original, the OTLP
    serializer's output;
  - the application's own settings (`test/app-settings.test.ts`): `custom.json`'s cases through
    the layer and through the agent (what the application reads, and the effective config), its
    typed reads (beyond the safe integers, the default); reads before any config and with keys
    named like Object's own; once per change and never for the same map, however it is sent;
    `app_settings` local; a FAILED config and a failed save changing nothing; the timer's Refresh and
    debug mode telling nothing; callbacks in order, ones that throw or reject, unsubscribing and
    subscribing from inside a callback; restarts with and without a state directory; one map for
    the register and the application, by `import` or `require`;
  - a provider of the application's own (`test/providers.test.ts`): a tracer, logger or meter
    provider registered once NodeSDK has started sets exactly its settings aside, reported at
    once with the inventory's settings, and the configs decided after; one the API refuses
    changes nothing; one registered before NodeSDK starts is seen as the SDK's is refused;
  - the inventory (`test/inventory.test.ts`): a typical setup's components; every instrumentation
    `getNodeAutoInstrumentations` loads listed with its version, and switched off by the name
    listed; no instrumentations, exporters set to `none`, repeated and unknown ones; an exporter
    gated exactly where its `send_*` is carried out; and in a real process, the hash at start and
    the components in full when the server asks.
- End to end: `python3 examples/node-otel/try.py` (CI's `node-otel` job), the checks every
  integration passes (`examples/integrations.py`). A program with no OpenTelemetry code runs
  under the register, and its `http` requests are traced; once the server switches `http`, `net`
  and `dns` off, they aren't, while it goes on making them. Then `in-code.mjs` passes the shared
  `check()` runs with the server given to `register()` in code and no `OTEL_OPAMP_*` variable set.
  Its `setup-in-code` check gives `register()` the wrong token and then the right one, each time
  with the other token in `OTEL_OPAMP_HEADERS`. The token given in code wins both times. The wrong
  token's warning names the headers given to `register()`, not `OTEL_OPAMP_HEADERS`. Then
  the settings that match telemetry, against the reference server, with the program exporting
  OTLP/JSON to a sink in `try.py` that reads every attribute: a rule of rate 0 for `/health`
  requests, a level per scope, redaction on spans and log records, both caps, a debug window that
  ends with the server stopped, and export health sent when the sink refuses traces. And
  `check_app_settings` (the checks `app-cjs` and `app-mjs`): the programs read the flag
  `example.feature` (`appSettings`), which the server turns on, sends a bad map for (FAILED, still
  on), leaves out (off), sends in a string (on) and sends again (nothing to tell); after a restart
  with no offer it is on from the start. Last, `check_truth` (the checks `truth-own-*`): app.cjs
  registers a tracer, logger or meter provider of its own (`EXAMPLE_SETUP`, setups.cjs), exporting
  to the same sink. The server offers one policy per control. A control is set aside
  when the policy is FAILED as an override the package can't carry out, or APPLIED with that
  override reported `local` in the effective config. Every other override must show in the
  telemetry, and the controls set aside must be exactly those that provider takes past the
  package.
