# SDK settings

An OpAMP server configures an OpenTelemetry SDK by sending it a **policy**: how much to sample,
what to send, which instrumentations to run. This page is the contract: what the server sends,
what each control does, and how an SDK answers. The rules live in one place, the SDK settings
layer, a compiled module ([the layer](#the-layer)) that every language runs. Thus the same policy
gets the same answer, and the same error message, in every language.

To put an SDK under a server without writing code, use a language's integration:

| Language | Integration | How it plugs in |
|---|---|---|
| Java | the Java agent extension, [javaagent.md](javaagent.md) | `-Dotel.javaagent.extensions=...` on the upstream OpenTelemetry Java agent |
| Python | `pydantic-opamp-otel`, [python-otel.md](python-otel.md) | installed next to `opentelemetry-instrument`: a distro, a configurator and the SDK's OpAMP hook |
| Node.js | `@pydantic/opamp-otel`, [node-otel.md](node-otel.md) | `node --import @pydantic/opamp-otel/register`, in place of upstream's `register` |
| Go | `github.com/pydantic/opamp-clients/go/otel`, [go-otel.md](go-otel.md) | `opampotel.Setup(ctx)` in place of the application's own SDK setup |
| .NET | `Pydantic.OpAmp.AutoInstrumentation`, [dotnet-otel.md](dotnet-otel.md) | a plugin for upstream's automatic instrumentation (`OTEL_DOTNET_AUTO_PLUGINS`) |

To wire a client to an SDK by hand instead, see [sdks.md](sdks.md).

## The policy

The policy is a Logfire SDK policy: the remote config's only file, `logfire-sdk-policy.json`
(`AgentConfigMap.config_map["logfire-sdk-policy.json"]`, content type `application/json`). It is
one JSON object. Its `schema` names its version, and every control of that version is in it. A
control is `{"mode": "local"}`, which is the SDK's own value as configured locally, or
`{"mode": "override", ...}` with the fields of the override.

Version 1, `logfire-sdk-policy/v1`, has two controls, `head_sampling` and `signal_exports`:

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "override", "ratio": 0.25},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "override", "enabled": false}
  }
}
```

The version 2 draft, `logfire-sdk-policy/v2-draft`, keeps every rule of version 1 and adds a
control for each other setting that the SDKs carry out:

```json
{
  "schema": "logfire-sdk-policy/v2-draft",
  "head_sampling": {"mode": "override", "ratio": 0.25},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "override", "rules": [{"path": "/health*", "rate": 0}]},
  "trace_rate_limit": {"mode": "local"},
  "log_levels": {"mode": "override", "level": "warn", "scopes": {"sqlalchemy.engine": "error"}},
  "log_rate_limit": {"mode": "local"},
  "attribute_redaction": {"mode": "override", "mask": ["*token*"], "drop": ["http.request.header.cookie"]},
  "debug": {"mode": "local"},
  "app_settings": {"mode": "override", "values": {"checkout.new_flow": true}},
  "instrumentations": {"mode": "local"},
  "polling_interval": {"mode": "local"}
}
```

A server sends version 2 only to an SDK that lists `logfire-sdk-policy/v2-draft` in its
`logfire.opamp.config_formats` attribute ([the description](#the-description)).
[examples/settings](../examples/settings/README.md) has a policy for each common use (cut trace
volume, trace everything while debugging, stop metrics and logs, quiet the logs, switch
instrumentations off). Version 1 is used where it is enough.

- The policy is the whole of what the server wants. A control left `local` is the SDK's own
  value, so a policy states everything that should differ from the SDK's own, not only what
  changes.
- Every control of the version is required. A missing control, a field that the version does not
  name, a control of version 2 in a version 1 policy, a bad value, or a field sent twice (JSON
  parsers disagree on which one wins) refuses the policy. The answer is FAILED, the reason starts
  with the control (`head_sampling: ratio must be a number from 0 to 1`), and nothing changes.
- The whole policy is applied, or none of it. This includes an override that this SDK does not
  carry out where it runs ([the host](#the-host)): the SDK refuses the policy with
  `signal_exports.metrics: this SDK cannot apply the requested override` in version 1, or with
  the field as well in version 2 (`log_levels: scopes: this SDK cannot apply the requested
  override`). Thus a server never hears APPLIED for an override that is not in force.
- The policy must be the only file of the config. A config without it is refused
  (`config_map: the config has no logfire-sdk-policy.json file`), and so is a config with another
  file beside it (`config_map: logfire-sdk-policy.json must be the only file, and the config has
  2`).

## The controls

| Control | Override fields | Takes effect |
|---|---|---|
| `head_sampling` | `ratio`, 0 to 1: the share of new traces sampled. A span whose parent was sampled (in this process or by the caller) is sampled whatever the ratio. | at once |
| `signal_exports.traces` | `enabled`, `true` or `false`: export traces | at once |
| `signal_exports.metrics` | `enabled`: export metrics | at once |
| `signal_exports.logs` | `enabled`: export logs | at once |
| `sampling_rules` | `rules`, an array of at most 100 rules: for a new trace, the first rule that its root span matches decides the rate, before `head_sampling`. See [sampling rules](#sampling-rules). | at once |
| `trace_rate_limit` | `traces_per_second`, more than 0 and at most 1000000 (`0.5` is one every 2 s): the most traces a second that this process starts or joins. See [rate caps](#rate-caps). | at once |
| `log_levels` | `level`, one of `trace`, `debug`, `info`, `warn`, `error` or `fatal`: export only the application's log records of that severity or above (at least SeverityNumber 1, 5, 9, 13, 17 or 21). `scopes`, an object of scope patterns to a level or `off`. See [log levels per scope](#log-levels-per-scope). At least one of the two. | at once |
| `log_rate_limit` | `records_per_second`, as `traces_per_second`: the most log records a second exported | at once |
| `attribute_redaction` | `drop` and `mask`, arrays of globs on attribute keys, ignoring ASCII case: remove the attributes that `drop` matches, and replace the value with `[REDACTED]` for those that `mask` matches, at export. See [redaction](#redaction). At least one of the two. | at once |
| `debug` | `until`, an RFC 3339 time at most 24 h ahead of the instance's clock: debug mode until then. Optionally `sampling_ratio` (as `ratio`, 1 if left out) and `log_level` (as `level`, `debug` if left out). See [debug mode](#debug-mode). | at once, until `until` |
| `app_settings` | `values`, an object of at most 128 of the application's own keys to strings, numbers or booleans: not the SDK's, handed to the application to read. See [app-settings.md](app-settings.md). | at once |
| `instrumentations` | `disabled`, an array of instrumentation names. `all_disabled`, `true` or `false`. At least one of the two. | at the next start |
| `polling_interval` | `milliseconds`, an integer from 1000 to 86400000 | at the next start |

A glob is `*` and literal characters only. An array or object that a field takes cannot be empty.
A string is taken as sent: a name, a pattern, a level or a time with spaces round it is refused.
Levels are read in any case, and `warning` is `warn`.

Inside the layer, each control field sets a **setting**, and an integration declares the settings
that it carries out by these names ([the host](#the-host), and the inventory in
[available-components.md](available-components.md#what-the-sdk-integrations-report)):

| Control field | Setting |
|---|---|
| `head_sampling` `ratio` | `sampling_rate` |
| `signal_exports.traces`, `.metrics`, `.logs` `enabled` | `send_traces`, `send_metrics`, `send_logs` |
| `sampling_rules` `rules` | `sampling_rules` |
| `trace_rate_limit` `traces_per_second` | `sampling_traces_per_second` |
| `log_levels` `level`, `scopes` | `send_logs_level`, `send_logs_level_by_scope` |
| `log_rate_limit` `records_per_second` | `send_logs_per_second` |
| `attribute_redaction` `drop`, `mask` | `drop_attributes`, `mask_attributes` |
| `debug` `until`, `sampling_ratio`, `log_level` | `debug_until`, `debug_sampling_rate`, `debug_send_logs_level` |
| `app_settings` `values` | `custom` |
| `instrumentations` `disabled`, `all_disabled` | `deactivate_instrumentations`, `deactivate_all_instrumentations` |
| `polling_interval` `milliseconds` | `opamp_polling_interval` |

Controls that take effect **at once** are carried out while the SDK runs: a sampler and exporters
that can be swapped (with the limits in [where each setting applies](#where-each-setting-applies)).
`signal_exports` stops exports, not the telemetry itself. Turned off, nothing more is sent, but an
export already on its way still arrives. Turned back on, the SDK's batch processors send what they
still held from while it was off (what they collected since their last export) with the next
exports. The log level drops the log records below it at export in the same way, from the next
record on. The records are still made. A record with no severity (SeverityNumber 0, unspecified)
cannot be judged, so it is exported whatever the level.

The next-start controls decide how the SDK starts. Most SDKs cannot remove an instrumentation
from a running process. Go and Node.js can, because an instrumentation's tracer comes from a
provider that the integration hands out. Thus each of them switches instrumentations off at once
([the host](#the-host)). The SDK keeps a next-start control and puts it in force at the next
start. That needs somewhere to keep state (a state directory). Without one, a policy that changes
a next-start control is refused (`polling_interval: takes effect at the next start, which needs
somewhere to keep state`). A policy that changes only live controls is fine.

The log level filters the application's exported log records, nothing more. **No integration
changes the SDK's own log level**, or the level of its own diagnostic logging.

## Settings that match telemetry

Some controls match spans or log records by a pattern. A pattern is a **glob**: `*` matches any
run of characters, none included, and every other character is itself (`?`, `.` and `[` too).
Matching is by Unicode characters, so a language's UTF-8, UTF-16 or JavaScript strings all agree,
and case-sensitive unless a control says otherwise. A pattern is at most 256 bytes. There are no
regular expressions: their dialects differ between languages, and every language has to match
the same way. The layer reads and checks these controls. Each language matches with a small
matcher of its own, because it matches too often to call into the layer. The shared test vectors
in `testdata/sdk-settings` and the layer's `Evaluate` call hold each matcher to the layer's.

### Sampling rules

`sampling_rules` sets the rate per kind of trace, first match wins. These are the two controls
of a version 2 policy that it is about:

```json
{
  "head_sampling": {"mode": "override", "ratio": 0.1},
  "sampling_rules": {"mode": "override", "rules": [
    {"rate": 0, "kind": "server", "path": "/health*"},
    {"rate": 1, "name": "checkout *"},
    {"rate": 0.5, "attributes": {"tenant.id": "acme", "db.system": "post*"}}
  ]}
}
```

- A rule has a `rate`, from 0 to 1 (a JSON number), and any of these matchers, all of which must
  match: `name`, a glob on the name the span starts with; `kind`, one of `server`, `client`,
  `internal`, `producer` or `consumer` (any case); `path`, a glob on `url.path`, or else on
  `http.target` with its query removed; and `attributes`, a map from an exact attribute key to a
  glob on its value. A rule with no matchers matches every span.
- An attribute's value is matched by its text: a string's own, an integer's decimal (`200`
  matches `"2*"`), `true` or `false`. A double or an array never matches (languages print floats
  differently), and neither does a missing attribute.
- Rules decide only new traces. For a root span, the first rule that matches decides the rate,
  which the SDK's own trace-ID ratio sampler applies. If none matches, `head_sampling` does. If
  that is local too, the application's own sampler does. A span with a parent follows its parent
  whatever the rules say, as with `head_sampling`: a sampled caller is still sampled under a rule
  of rate 0. With both controls local, the application's own sampler decides every span,
  children too, as it did before any policy.
- A rule sees only what the span has when it starts. **`http.route` isn't a matcher**: most
  instrumentations add it after the span starts (name it under `attributes` where it is set at
  start), and HTTP server spans start named `GET`, so match them by `path` and `kind`, not by
  `name`.
- Up to 100 rules and 32 attributes a rule. An unknown field (`route` included), a missing or
  out-of-range `rate`, a bad `kind`, a pattern over 256 bytes or a field sent twice is FAILED,
  naming the rule: `sampling_rules: rules[2].rate: 7 is not between 0 and 1`. An empty array is
  refused.
- The effective config reports each rule's fields sorted and a rate of 0 or 1 as an integer, so
  the same rules sent another way are no change.

### Log levels per scope

The `scopes` of `log_levels` set the level per logger, by the scope name that a log record is
exported under:

```json
{"log_levels": {"mode": "override", "level": "warn",
                "scopes": {"com.acme.db*": "debug", "urllib3*": "error", "noisy.lib": "off"}}}
```

Here records of `com.acme.db` and every scope whose name starts with it are exported from
`debug` up, `urllib3*` from `error`, none at all of `noisy.lib`, and every other scope's from
`warn`.

- A pattern is an exact scope name or a prefix ending in `*` (`*` alone: every scope). A prefix is
  by characters, not by dotted segments: `com.acme*` matches `com.acmesoft`. Names may not hold
  `,`, `=` or whitespace, and match case-sensitively.
- Levels are those of `level`, in any case, plus `off`. A scope's level may be lower than
  `level` as well as higher.
- A record's threshold is the level of the exact entry for its scope, else of the longest prefix
  that matches, else `level`. With none of them, every record is exported. A record with no
  severity (0) is kept, except under `off`, which drops every record of its scope. The logs export
  off drops them all whatever the levels.
- The scope is the name the record is exported under, which is what a scope *is* in each SDK:
  Python's and Java's logger name, .NET's ILogger category; in Node.js
  the bridge's package (`@opentelemetry/instrumentation-pino`), not the app's logger; in Go the
  name passed to `otelslog.NewLogger`. The empty scope name only `*` matches.
- Up to 256 entries. A pattern sent twice, a bad name or a bad level is FAILED, and so is an
  empty object. The effective config reports the entries sorted by pattern, each level by its
  lowercase name.
- A host that does not carry out the level cannot carry out the scopes either. Where the host does
  not declare `send_logs_level`, the layer refuses a policy that overrides `scopes`
  (`log_levels: scopes: this SDK cannot apply the requested override`), even where the host
  declares `send_logs_level_by_scope`.

### Redaction

`attribute_redaction` keeps attributes out of what is exported:

```json
{"attribute_redaction": {"mode": "override",
                         "drop": ["http.request.header.authorization", "http.request.header.cookie"],
                         "mask": ["*password*", "*token*", "user.email"]}}
```

Here the two headers are removed, and an attribute whose key holds `password` or `token`, or is
`user.email`, is exported as `"[REDACTED]"`.

- Each is an array of globs on attribute keys, matched **ignoring ASCII case**
  (`Authorization` matches `authorization`; `É` doesn't match `é`). `*` alone matches every key.
- `drop` removes the attribute. `mask` replaces its value with the string `[REDACTED]`, whatever
  its type (a masked integer becomes a string). A key that both match is dropped. A drop does not
  change `dropped_attributes_count`.
- They apply at export, through the integration's own gates, to span attributes, span event
  attributes, span link attributes and log record attributes. The application's own span or
  record isn't changed: another exporter (one the gate doesn't cover) sees the original.
- Out of scope: resource and scope attributes, span names, log bodies (map bodies too), and
  **metrics**, in every language. Removing or masking a data point's attribute would merge
  distinct series into one, so the exported data would be wrong.
- Up to 128 patterns each, each at most 256 bytes, with no comma. An empty array is refused. The
  effective config reports the patterns lowercase, without repeats, sorted.

### Rate caps

`trace_rate_limit` and `log_rate_limit` are safety valves: whatever the rates and levels say, no
more than this many a second.

```json
{
  "head_sampling": {"mode": "override", "ratio": 1},
  "trace_rate_limit": {"mode": "override", "traces_per_second": 50},
  "log_rate_limit": {"mode": "override", "records_per_second": 200}
}
```

- Each is a number, more than 0 and at most 1000000. Decimals are allowed (`0.5` is one every
  2 s). 0 is refused. To send none, use `head_sampling` 0 or the logs export off.
- Each is a token bucket, the same algorithm in every language (pinned by
  `testdata/caps.json`): it starts full, refills at n a second on a monotonic clock, and holds at
  most a second's worth (at least one token).
- **Traces, not spans.** The trace cap is in the sampler, after the rules and the rate have said
  to sample. A root span takes a token, and so does a span whose remote parent was sampled: a
  cap has to hold behind a traced caller, unlike `head_sampling`. Without a token the span is
  dropped. A child of a span sampled in this process is always sampled and takes none, since
  refusing it would break its trace.
- **Logs.** The log cap is at the log gate, after the levels: of a batch, the first records the
  tokens allow go on and the rest are dropped. With the SDKs' default batch delay (1 s) that lets
  n records a second through; a longer `OTEL_BLRP_SCHEDULE_DELAY` makes the cap stricter.
- Changing n starts a full bucket. Putting the control back to local removes the cap at once. The
  caps stay in force in debug mode.

## Debug mode

For a while, everything: the `debug` control puts an instance in debug mode until a time, and it
ends on its own, whether the server can still reach the instance or not.

```json
{
  "head_sampling": {"mode": "override", "ratio": 0.05},
  "log_levels": {"mode": "override", "level": "warn"},
  "debug": {"mode": "override", "until": "2026-10-14T15:30:00Z"}
}
```

```json
{"debug": {"mode": "override", "until": "2026-10-14T15:30:00Z", "sampling_ratio": 0.5, "log_level": "info"}}
```

- Until then, `sampling_ratio` (1 if left out) is the sampling rate, and the sampling rules do not
  apply. `log_level` (`debug` if left out) is the log level, and the levels per scope do not
  apply. Everything else stays: the signal exports (the logs export off still sends nothing),
  redaction, and the rate caps, which keep debug mode from flooding the backend.
- **A time, not a duration.** The server sends the same config again on every reconnect, and an
  instance loads the saved policy at every restart: a duration would start again each time.
  A time ends when it says, however often it is sent. To end debug mode early, send a policy with
  `debug` local. To extend it, send a later time.
- The instance's own clock decides. A time more than 24 h ahead of it is refused (FAILED: the
  server's clock, or this one, is far off). A time at or before now is applied but over: the
  settings are as without it, and the effective config reports `debug` local. Times are
  RFC 3339 with a `Z` or an offset (`2026-10-14T17:30:00+02:00`), a fraction allowed, and no leap
  seconds.
- `sampling_ratio` and `log_level` are checked like `ratio` and `level`. Where the integration
  does not declare `sampling_rate` or `send_logs_level`, a policy that overrides the one that
  depends on it is refused (`debug: log_level: this SDK cannot apply the requested override`).
- The effective config reports the override while debug mode lasts, as sent, and `debug` local
  after it.
  Each decision says when to ask again (`refresh_at_unix_ms`): the integration runs a timer and,
  when it fires, calls `Refresh` and puts in force what it returns, and reports the new effective
  config. A restart before the end is still in debug mode, after it, not.
- An integration that does not give the layer the time (`now_unix_ms` 0), or does not declare
  `debug_until` (see [the host](#the-host)), refuses a policy that overrides `debug`
  (`debug: until: this SDK cannot apply the requested override`).

## Export health

Always on, with no control: it is reporting, not behaviour. Each integration counts what its
gates see, and the agent's health (`ComponentHealth`) carries it under
`component_health_map["exporters"]`. The top-level `healthy` stays the application's (as with
.NET hosting's health checks), so a failing exporter doesn't make the service look down.

```json
{"exporters": {"healthy": false, "status": "failing", "last_error": "traces: connection refused",
  "component_health_map": {
    "traces":  {"healthy": false, "status": "failing", "last_error": "connection refused",
                "attributes": {"exports.ok": 3, "exports.failed": 1, "items.failed": 512, "items.filtered": 0}},
    "metrics": {"healthy": true, "status": "ok", "attributes": {"exports.ok": 4, "...": 0}},
    "logs":    {"healthy": true, "status": "off", "attributes": {"exports.ok": 0, "...": 0}}}}}
```

- One child per signal the gates export: `traces`, `metrics`, `logs`. `status` is `ok`,
  `failing` (its most recent export failed, after the exporter's own retries; `last_error` is the
  exporter's error) or `off` (its export is off: off on purpose, not failing). The next
  successful export clears a failure. The `exporters` entry is unhealthy while any child is, and
  `off` when every one is.
- `attributes`, integers cumulative since start: `exports.ok`, `exports.failed`, `items.failed`
  (spans, metric points or log records in failed exports), and `items.filtered`: dropped on
  purpose by a level, a cap or redaction, which is never unhealthy. Records dropped by a full
  queue aren't counted.
- **When it is sent.** The layer's `Health` call builds the entry and says whether to send it:
  at once when a `healthy` or `status` changes, at most every 60 s while only the counters move,
  and not at all while nothing changes. The integration calls it from the same timer as `Refresh`,
  every 30 s.
- **On the server.** The server core raises a health change when the condition (`healthy`,
  `status`, `last_error`) of the agent or of any component under it changes, so an exporter going
  `failing` is one; counters and times moving are not. The reference server keeps the whole report
  and shows each component's condition in its explanation of the agent.

## Where each setting applies

None of this needs OpAMP support in the upstream SDK. Each integration builds the upstream SDK (or
hooks into it as it starts) with a sampler and exporters of its own inside: a sampler it can
change (the rate, the rules, the trace cap, debug mode) and a gate in front of each exporter (on
or off, the log levels, the log cap, redaction, and the counts export health reports). A control
reaches the telemetry that goes through them, and nothing else.

| Control | Python | Node.js | Go | .NET plugin | .NET `WithOpAmp` | Java agent |
|---|---|---|---|---|---|---|
| `head_sampling` | at once | at once ³ | at once ⁴ | at once | at once | at once ⁴ |
| `signal_exports.traces`, `.logs` | at once ¹ | at once ¹ | at once ¹ | at once ¹ | at once ¹ | at once ¹ |
| `log_levels` `level` | at once ¹ | at once ¹ | at once ¹ | at once ¹ | at once ¹ | at once ¹ |
| `sampling_rules` | at once ⁴ ⁵ | at once ³ ⁵ | at once ⁴ ⁵ | at once ⁴ ⁵ | at once ⁴ ⁵ | at once ⁴ ⁵ |
| `log_levels` `scopes` | at once ¹ ⁴, per logger | at once ¹ ⁴, per logs API logger or bridge | at once ¹ ⁴, per `otelslog` logger | at once ¹ ⁴, per `ILogger` category | at once ¹ ⁴, per `ILogger` category | at once ¹, per logger |
| `attribute_redaction` | at once ¹ ⁴ | at once ¹ ⁴ | at once ¹ ⁴ | at once ¹ ⁴, in the OTLP bytes | at once ¹ ⁴, in the OTLP bytes | at once ¹ |
| `trace_rate_limit`, `log_rate_limit` | at once ¹ ⁴, per process | at once ¹ ³ ⁴ | at once ¹ ⁴ | at once ¹ ⁴ | at once ¹ ⁴ | at once ¹ |
| `debug` | at once, until then ⁴ | at once, until then ⁴ | at once, until then ⁴ | at once, until then ⁴ | at once, until then ⁴ | at once, until then |
| [export health](#export-health) (no control) | reported ⁶ | reported ⁶ | reported ⁶ | reported ⁶ | reported ⁶ | reported ⁶ |
| `signal_exports.metrics` | at once, `otlp` only ¹ | at once ¹ | at once, push only ¹ | at once ¹ | at once ¹ | at once, Prometheus too |
| `instrumentations` | next start | at once ² | at once ⁴ | next start | refused | next start |
| `polling_interval` | next start | next start | next start | next start | next start | next start |
| `app_settings` ([app-settings.md](app-settings.md)) | at once, read by the application | at once, read by the application | at once, read by the application | at once, read by the application | at once, read by the application | at once, read by the application |

¹ Only for the exporters the integration builds: OTLP and console (and Java's Prometheus reader).
Another exporter, a span or log processor passed in or added later, or another agent extension's
processor goes on exporting. Where the integration sees such an exporter when it builds the SDK,
it does not declare that signal's settings. Thus it refuses a policy that overrides them, and it
logs each signal as not carried out:
Python for exporters it doesn't build, span or log processors or metric exporters passed to
`configure()`, and, from then on, a span or log processor added to its providers later; Node.js for
exporters it doesn't build, and from the moment the application registers a global tracer, logger
or meter provider of its own in place of the SDK's; Go for a metrics exporter it doesn't gate, and
for a global provider the application sets in place of `Setup`'s, from when it is set (the package
looks before each decision and once a second, [go-otel.md](go-otel.md#where-the-server-cant-reach)); the .NET plugin and `WithOpAmp` whenever
one of a signal's exporters can't be stopped (not OTLP, beside another exporter, another
plugin's, or, in `WithOpAmp`, with a path or an `HttpClientFactory` of its own), and the .NET
plugin for traces and metrics under `OTEL_DOTNET_AUTO_SETUP_SDK=false`; the Java agent for a span
or log record processor or a metric reader another agent extension adds through the autoconfigure
customizers, and under a configuration file for a span or log processor that is neither batch nor
simple, or a metric reader neither periodic nor pull. .NET reads the built providers again, before
each decision and every 30 s, and the plugin reads each logger provider on its first log record.
Where the host narrows while the SDK runs, the overrides of that signal's controls in the policy
in force are set aside ([the host](#the-host)): each is reported `local` in the effective config,
which is reported again, and the rest of the policy stays in force. Elsewhere a processor added
after the SDK is built is not seen: the policy is still answered APPLIED. In Node.js the SDK's
providers take no processor once built, and Go's `Setup` hands out no SDK provider, so nothing
can be added to one. A provider a Node.js or Go application builds and never registers or sets as
global isn't seen either.

² For the instrumentations loaded at start: the server switches them off and on again, but one
the `OTEL_NODE_*_INSTRUMENTATIONS` variables (or upstream's defaults) leave out isn't loaded, and
a switched-off one stays loaded, recording nothing ([node-otel.md](node-otel.md#switching-instrumentations-off)).

³ In Node.js the sampler samples the spans of the SDK's tracer provider only. Once the
application registers a global tracer provider of its own, its spans go past the sampler, so
`head_sampling`, `sampling_rules`, `trace_rate_limit` and `debug` (`sampling_ratio`) are set
aside from then on ([node-otel.md](node-otel.md#a-provider-of-the-applications-own)).

⁴ Declared to the layer (see [the host](#the-host)) only where the integration carries it out,
as every control is. Elsewhere the SDK refuses a policy that overrides it, and nothing changes.
Python declares every one only where its configurator built the SDK (not under another
configurator or `OTEL_CONFIG_FILE`). Node.js declares `debug` (`until`) always, and
`sampling_rules`, `trace_rate_limit` and `debug` (`sampling_ratio`) unless the application
registers a tracer provider of its own (footnote ³), the log ones (`log_levels` (`scopes`),
`log_rate_limit`, `debug` (`log_level`)) only where it builds the log exporters, and
`attribute_redaction` only where it builds both the span and the log exporters. The .NET plugin
and `WithOpAmp` declare the log ones where every logs exporter is the gated OTLP one,
`attribute_redaction` where every span and log record exported goes through a gate (traces and
logs each gated, or not exported), `sampling_rules` and `trace_rate_limit` where `head_sampling`
is carried out, and debug mode always (its `sampling_ratio` and `log_level` refused where
`head_sampling` and `log_levels` (`level`) are not carried out). Go declares every one until the
application sets a global provider of its own in place of `Setup`'s. Then it no longer carries
out what that signal needs, and sets those overrides aside: for the tracer provider the sampler's
controls, `signal_exports.traces`, `attribute_redaction` and `instrumentations`; for the meter
provider `signal_exports.metrics`; for the logger provider the log controls and
`attribute_redaction`. The Java agent declares every one but where footnote ¹ says. It also does
not declare the sampler's controls (`head_sampling`, `sampling_rules`, `trace_rate_limit` and
`debug` (`sampling_ratio`)) where another agent extension sets a sampler of its own in place of
the extension's.

⁵ A rule sees what a span has when it starts, which differs by instrumentation
([sampling rules](#sampling-rules)). HTTP server spans start with their path in every language
in Python, Node.js, Go and Java (match them by `kind` and `path`; `http.route` mostly comes
later). HTTP client
spans start with their URL in Python, Node.js and Java, to match under
`attributes`; in Go (`otelhttp`'s transport) and on .NET 8 (`HttpClient`) with no attributes, so
only their name and `kind` can match. On .NET 8 an ASP.NET Core request span starts with none
either: the integration decides a new trace's request in the ASP.NET Core instrumentation's
`Filter`, with its path, method, scheme and host, as the sampler would.

⁶ Each integration counts at its own gates (the exporters ¹ covers) and sends the `exporters`
entry from its 30 s timer, the top-level `healthy` staying the application's. Not counted:
records a full batch queue drops (everywhere); a Prometheus reader (Go, Java), which exports
nothing through a gate, so its child stays `ok` at zero; in Python, a metric exporter that isn't
the gated `otlp` one, and forked workers' exports (they don't report). In .NET each attempt
through the gate counts, so an OTLP retry that then succeeds shows `failing` for a while.

What else to know:

- **A sampled caller decides.** The rate is for new traces. A request that arrives with a sampled
  `traceparent` is traced at any rate, 0 included, so a service behind callers that already trace
  sees little change.
- **The ratio replaces the application's sampler** while `head_sampling` is overridden (whatever
  `OTEL_TRACES_SAMPLER` says, even `always_off`). Local again, the SDK's own sampler is back.
- **Turned off means dropped at export.** Spans, metrics and logs are still made while a signal's
  export is off (a span is still sampled, so services downstream still trace it). They are only
  dropped at export. Cumulative metrics carry the counts made meanwhile into the next export.
- **An SDK the application builds on its own isn't reached.** Python (`opamp_otel.configure()`)
  and .NET (`WithOpAmp()`) let an application keep its own setup; in Node.js and Go the
  integration's setup has to be the application's whole SDK setup, and in Java the agent's SDK.
  Node.js and Go set aside what a provider the application registers or sets as the global one
  takes past them. A provider it builds and never registers or sets as global can't be seen: the
  server may hear APPLIED for a setting its telemetry doesn't follow. The Java agent extension
  can't see an `OpenTelemetrySdk` the application builds and uses itself: that SDK's telemetry
  escapes every setting, and the extension still answers for the agent's SDK. In .NET, a tracer or
  meter provider the application builds beside the managed one listens to the same
  instrumentations. The integration can't see it, so the settings stay answered APPLIED while its
  telemetry goes on.
- **Declarative configuration (`OTEL_CONFIG_FILE`) builds the SDK from the file.** The Python
  integration still joins the fleet and switches instrumentations at the next start, but the
  file's sampler and exporters are not its own. Thus it refuses a policy that overrides
  `head_sampling` or `signal_exports`. The Java agent extension takes the configuration file's
  model instead and carries out every control as without it ([javaagent.md](javaagent.md)).

## The answer

For each remote config the SDK answers with a `RemoteConfigStatus`: APPLIED or FAILED, the
config's hash, and on FAILED the reason (`head_sampling: ratio must be a number from 0 to 1`). The
reason starts with the control it is about, or with `config_map:`, `schema:` or `policy:` when
the policy as a whole is wrong. It quotes at most 64 characters of a value or key, then
`… (N bytes)`, so a huge value sent wrong does not come back as a huge reason. The SDK then
reports its **effective config**: the policy in force, as the only file, `logfire-sdk-policy.json`
(`application/json`).

**The client keeps the config applied.** A config is answered in two steps, the same in every
language. The layer first decides what to keep (`Decide` returns a `Plan`: the policy to keep, or
nothing when the config is refused). Then it gives the answer (`Commit`). With a state directory
the integrations save nothing themselves. The client keeps the config that the application
answered APPLIED in its own saved state, and tells the server APPLIED only once that save has
landed. Until then, and while a save fails (the client tries again), the server hears APPLYING.
Thus the integration commits it as saved. With no state directory there is nowhere to keep it. A
policy that changes a next-start control is refused, and one that changes only live controls is
APPLIED. A save that fails, for a host that makes one itself, refuses the config
(`can't save the settings: …`) and changes nothing. The integration never rewrites the answer.
Each language's binding runs the two steps, so the integration makes one call.

- The effective config uses the schema of the policy in force. Before any policy is applied, it
  is the version 1 policy with every control local.
- A version 1 policy is written compact, its keys in the order of the schema. A version 2 policy
  is written compact, its keys sorted.
- Live controls show as applied: a level by its lowercase name (`warn` for `WARNING`), the rules,
  the scopes and the redaction patterns in their canonical form, and the values of `app_settings`
  as strings (`"25"` for `25`).
- A next-start control shows local until the restart, because the SDK started with its own value.
  A server that compares the effective config with what it sent sees the change pending, which it
  is. The decision also lists the settings pending a restart (`pending_restart`, for example
  `opamp_polling_interval`).
- A refused policy changes nothing, so the effective config still reports the policy in force
  before it. A restart reports the policy in force again.
- The layer builds the effective config (`Load`, `Commit` and `Refresh` each return the whole of
  it), so every language reports it the same way. The reference server's log shows an effective
  config's JSON files on its `a2s` line, as `effective`.

## The description

`Describe` builds the agent description. `service.name`, `service.namespace` and
`service.instance.id` identify the instance, and the rest of the resource describes it. Beside
them, the layer adds three attributes for the server:

- `logfire.opamp.config_format`: `logfire-sdk-policy/v1`, the exact format of version 1.
- `logfire.opamp.config_formats`: the schemas that the SDK reads, `logfire-sdk-policy/v1` and
  `logfire-sdk-policy/v2-draft`.
- `logfire.opamp.supported_controls`: the controls that this host carries out, in the order
  `head_sampling`, `signal_exports.traces`, `signal_exports.metrics`, `signal_exports.logs`,
  `sampling_rules`, `trace_rate_limit`, `log_levels`, `log_rate_limit`, `attribute_redaction`,
  `debug`, `app_settings`, `instrumentations`, `polling_interval`. A control is listed when the
  host declares the settings behind it ([the host](#the-host)). `debug` is listed only with a
  clock (`now_unix_ms` not 0). With no host, the list is empty.

Thus every integration passes the same `host` and `now_unix_ms` to `Describe` as to `Load`.

## The layer

The layer is the rules as pure functions, compiled to `opamp_sdk_settings.wasm`, which each
language's client ships. It comes with the core, pinned by hash
([how-it-works.md](how-it-works.md#where-the-core-comes-from)), and `scripts/gen_sdk_settings.py`
generates each language's types for it. It is separate from the client core: its own module and
its own interface, `proto/opamp/sdk/v1/settings.proto`. The module keeps nothing between calls.
The integration keeps the policies it is handed and passes them back.

| Call | When | Returns |
|---|---|---|
| `Load{core_state, saved, host, now_unix_ms}` | at start, before the SDK is built, with the client's saved state and the policy kept before ([at start](#at-start)) | the settings to start with (next-start ones included), the policy in force, the effective config, and a warning if the saved policy could not be used (the SDK then starts with its own); when to ask again (`refresh_at_unix_ms`) |
| `Decide{config, current, at_start, host, now_unix_ms}` | a remote config arrived | a `Plan`: the policy to save before answering (none when the config is refused) |
| `Commit{decide, saved, error}` | once the plan's policy is saved, or its save failed, or there is nowhere to save it ([the answer](#the-answer)) | the `RemoteConfigStatus` to send, the settings to put in force (only when APPLIED), the policy now in force, the effective config, the next-start settings pending a restart, and when to ask again |
| `Refresh{current, at_start, host, now_unix_ms}` | at the time a `Load`, `Commit` or `Refresh` said to ask again (`refresh_at_unix_ms`, 0 for never): the end of debug mode | the settings to put in force now, the effective config, when to ask again, and a warning if the policy in force can no longer be used (one moved back a day, say). An override that the host no longer declares is set aside instead: reported `local`, the rest of the policy still in force ([the host](#the-host)) |
| `Health{counts, previous, now_unix_ms, settings}` | every 30 s, from the `Refresh` timer, with what the gates counted since start, the report the last call answered, and the settings in force | whether to send now, and the `exporters` entry to put in the agent's health (when not sending, the one last sent): see [export health](#export-health) |
| `Describe{resource, host, now_unix_ms}` | at start | the agent description, with the policy formats and the supported controls ([the description](#the-description)) |
| `Components{inventory, host, now_unix_ms}` | once the SDK is built, and when what it lists changes | the instance's inventory as its available components, for `set_available_components`: the integration's and SDK's versions, its instrumentations and exporters, and the settings the host carries out ([available-components.md](available-components.md#what-the-sdk-integrations-report)) |
| `Evaluate{settings, host, input}` | in tests only | what the layer's reference matchers make of one input under a policy: how a span is sampled, whether a log record passes the logs export and the levels, what redaction leaves of a list of attributes, what a rate cap lets through. Each language's tests hold its own matchers to it |

### At start

The settings to start with are those of the last config applied. The client keeps that config in
its saved state (the state file in the state directory, beside the last config received, which it
hands over again at start). It saves the config when the application answers APPLIED, and
reports APPLIED only once that save has landed. `Load` takes the state as the client saved it
(`core_state`) and reads the policy of the config from it. Thus a config that the application
refused, received after the one applied, is never what a restart starts from.

With no config applied in that state (none saved, a state saved by an earlier client that did not
keep one, or one that does not read), `Load` reads `saved`: the policy that the integration kept
itself before (`sdk-settings.json` in the state directory). Thus the settings carry over from an
earlier release until a config is applied. The integrations no longer write that file.

A saved policy that overrides a control the host does not declare now (the application changed
its exporters, say) is not used. `Load` warns (`saved SDK settings ignored: …`) and the SDK starts
with its own settings, every control local, until the server sends a policy that it can carry
out.

The config applied survives an endpoint change. The client drops the config received from the
old server (the new server's config wins), but it keeps the applied one, as it keeps packages
installed. Its settings are in force on the agent, and stay so until the new server's config is
applied. A new server then sees in the effective config what the agent actually runs.

### The host

An integration **declares** every setting that it carries out, with a `Host` passed to `Load`,
`Decide`, `Refresh` and `Describe`, from what it built. A policy that overrides a control whose
setting is not declared is refused, so a host never answers APPLIED for an override that it does
not carry out. Beside the host, each call takes `now_unix_ms`, the instance's clock in
milliseconds since the Unix epoch, which only debug mode reads. The layer reads no clock of its
own.

- `live`: the settings that it carries out at once. A next-start setting listed here is then
  live in every way: put in force by the decision, reported in force at once, never pending, and
  it needs no state directory. The Go and Node.js integrations list
  `deactivate_instrumentations` and `deactivate_all_instrumentations` here.
- `next_start`: the next-start settings that it carries out at the next start
  (`deactivate_instrumentations`, `deactivate_all_instrumentations`, `opamp_polling_interval`).
  A live setting cannot be listed here.
- A setting in neither is not carried out. A policy that overrides its control is refused with
  `<control>: this SDK cannot apply the requested override` (version 1), or
  `<control>: <field>: this SDK cannot apply the requested override` (version 2), and nothing
  changes. Each integration declares a signal's sending and what its gate holds only where every
  exporter of the signal is gated, and the sampler's settings only where its sampler is in force
  ([where each setting applies](#where-each-setting-applies), footnotes ¹ and ⁴).
  `send_logs_level_by_scope` and `debug_send_logs_level` count as not declared without
  `send_logs_level`, and `debug_sampling_rate` without `sampling_rate`. Debug mode also needs the
  clock. `custom`, the setting behind `app_settings` ([app-settings.md](app-settings.md)), is
  declared where the application can read it.
- A host can narrow while the SDK runs, when the application sends telemetry past the SDK's
  gates (another plugin's exporter, a global provider of the application's own, another
  extension's processor or sampler). From then on the integration passes the narrower host. A
  policy offered from then on that overrides a control the SDK no longer carries out is refused
  whole, FAILED, and the error names the control and field
  (`signal_exports.traces: this SDK cannot apply the requested override`). In the policy already
  in force, each override the SDK no longer carries out is set aside: it is reported `local` in
  the effective config (the SDK's own value is in force), and the rest of the policy stays in
  force.
- A host that names what is no setting (a misspelling, say) is refused: the call is an error,
  naming it. So is one that lists a live setting in `next_start`.

With no host, nothing is declared, and every override is refused. The rules stay the layer's: an
integration only says what its SDK can do.

Every integration also keeps its OpAMP client's own requests out of the application's telemetry.
The client runs with instrumentation suppressed, as OpenTelemetry's own exporters do (in Go,
where instrumentation wraps a transport, with an HTTP transport of its own). Thus its requests
and its goodbye are never traced. The end-to-end check fails if one is. This is built around the
platform's HTTP stack, so every integration asks for its client's platform mode by name
([sdks.md](sdks.md#rust-mode-and-platform-mode)) rather than the clients' default, Rust mode.

What stays in each language is what touches its SDK:

- a sampler and exporters it can swap;
- how it switches instrumentations off, at start or while it runs;
- where it keeps state;
- its hooks into the SDK or agent.

The Java agent extension, for example, is its controls (a live sampler, three gated exporters
and a gated pull metric reader), a mapping from the next-start settings to agent properties (or,
under declarative configuration, to the configuration model), and the agent's extension hooks.

## How it is checked

- The layer is checked on its own before it is released: every control and every refusal, and
  properties over generated policies (any bytes are read or refused with a reason; an answer is
  all or nothing, with the effective config and the pending settings exactly as described above;
  the same next-start control sent another way is no change).
- `testdata/sdk-settings/*.json`: the shared test vectors, each a policy, an input and the
  outcome: `settings.json` (how policies are read and refused), `sampling.json` (how a span is
  sampled), `logs.json` (whether a log record passes the levels), `redact.json` (what redaction
  leaves of a list of attributes), `caps.json` (what a rate cap lets through, event by event),
  `debug.json` (debug mode over time: `Load`, `Decide` and `Refresh` in turn, and the exact
  effective config through them), `saving.json` (the answer from what became of the save: saved,
  failed, nowhere to save; the integrations, whose saves are the client's, run its saved and
  nowhere cases), `start.json` (the policy `Load` starts from: the config applied in the client's
  saved state, or the one kept before, and whether `Load` warns),
  `components.json` (what the inventory lists,
  [available-components.md](available-components.md#what-the-sdk-integrations-report)),
  `health.json` (export health call after call: what is reported, and when it is sent),
  `custom.json` (the application's own settings: the values taken and refused, and the typed
  reads). The layer runs every one, and through `Evaluate`. Each language's wrapper and
  integration tests run them too.
- Random vectors: cases in the formats of `sampling.json`, `logs.json`, `redact.json` and
  `caps.json`, answered by the layer, drawn where languages part ways (case outside ASCII, the
  Kelvin sign and the long s, characters past the BMP, a leading U+FEFF, regex and path
  punctuation, overlapping rules and nested scope prefixes, clocks that jump and go back). Each
  language's vector tests run them in place of testdata's when `OPAMP_SETTINGS_VECTORS` names
  their directory.
- Each language: its wrapper through its WebAssembly runtime (`SdkSettingsTest`, `test_sdk_settings.py`,
  `sdk.test.ts`, `go/opamp/sdk`, `SdkSettingsTests`),
  and its integration end to end against an OpAMP server, with the same checks for every
  integration (`examples/integrations.py`; [javaagent.md](javaagent.md#how-it-is-checked)). Its
  `check_signals` sends each control that matches telemetry to each integration in turn, and
  checks what arrives at the sink: a debug window that ends on the agent's timer, a sampling rule,
  the application's loggers off by scope, a masked and a dropped URL, both rate caps, and export
  health. It also checks that the effective config that the agent reports is the policy in force,
  version 1 and then version 2 ([examples/README.md](../examples/README.md)). Its `check_truth`
  runs the integration under the setups where a control may not reach the telemetry (a processor
  added once the SDK is built, say), and offers one version 2 policy per control. A control counts
  as set aside when the policy is FAILED as an override the SDK can't carry out, or APPLIED with
  that override reported `local` in the effective config. Every other override must show in the
  telemetry, and the controls set aside must be the ones that setup is known to take out of the
  integration's hands.
