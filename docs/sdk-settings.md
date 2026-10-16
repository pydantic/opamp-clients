# SDK settings

An OpAMP server configures an OpenTelemetry SDK by sending it **settings**: how much to sample,
what to send, which instrumentations to run. This page is the contract: what the server sends,
what each setting does, and how an SDK answers. The rules live in one place, the SDK settings
layer, a compiled module ([the layer](#the-layer)) every language runs. So the same document gets
the same answer, and the same error message, in every language.

To put an SDK under a server without writing code, use a language's integration:

| Language | Integration | How it plugs in |
|---|---|---|
| Java | the Java agent extension, [javaagent.md](javaagent.md) | `-Dotel.javaagent.extensions=...` on the upstream OpenTelemetry Java agent |
| Python | `pydantic-opamp-otel`, [python-otel.md](python-otel.md) | installed next to `opentelemetry-instrument`: a distro, a configurator and the SDK's OpAMP hook |
| Node.js | `@pydantic/opamp-otel`, [node-otel.md](node-otel.md) | `node --import @pydantic/opamp-otel/register`, in place of upstream's `register` |
| Go | `github.com/pydantic/opamp-clients/go/otel`, [go-otel.md](go-otel.md) | `opampotel.Setup(ctx)` in place of the application's own SDK setup |
| .NET | `Pydantic.OpAmp.AutoInstrumentation`, [dotnet-otel.md](dotnet-otel.md) | a plugin for upstream's automatic instrumentation (`OTEL_DOTNET_AUTO_PLUGINS`) |

To wire a client to an SDK by hand instead, see [sdks.md](sdks.md).

## The document

The settings are the remote config's `elastic` entry (`AgentConfigMap.config_map["elastic"]`):
a JSON object of values. This is the format of Elastic's central configuration, which the EDOT
SDKs read and Kibana and o11yfleet send, so the same server configures EDOT and upstream SDKs
alike.

```json
{"sampling_rate": "0.25", "send_logs": "false", "deactivate_instrumentations": "jdbc,kafka-clients"}
```

[examples/settings](../examples/settings/README.md) has a document for each common use (cut trace
volume, trace everything while debugging, stop metrics and logs, quiet the logs, switch
instrumentations off),
each checked by the layer's tests.

- Values may be strings, numbers or booleans: `"0.25"` and `0.25` are the same setting. EDOT
  Java refuses numbers, so servers send strings. `sampling_rules` is the one structured setting:
  a JSON array, sent as it is or encoded in a string.
- A setting left out means the SDK's own value, as configured locally. A remote config with no
  `elastic` entry, one meant for something else say, leaves every setting the SDK's own.
- A `null` is a setting left out, and so is an empty list of instrumentations.
- The whole document is applied, or none of it. If any setting is wrong, or a key is sent twice
  (JSON parsers disagree on which one wins), the config is answered FAILED, naming that setting,
  and nothing changes.
- The reference server reads a desired config named `elastic` with this same layer, as an SDK
  that carries out every setting would on the server's clock, and refuses the scenario with the
  layer's own reason (`check_sdk_settings`), so a document every SDK would answer FAILED is never
  offered. A setting no SDK knows isn't refused, and a debug window already over is a document
  SDKs apply without debug mode. It also refuses `opamp_polling_interval` beside a
  `heartbeat_interval_s`: the heartbeat offer would win while the SDK runs.

## The settings

| Setting | Value | Takes effect |
|---|---|---|
| `sampling_rate` | 0 to 1: the share of new traces sampled; a span whose parent was sampled (in this process or by the caller) is sampled whatever the rate | at once |
| `sampling_rules` | a JSON array of rules (or one in a string): for a new trace, the first rule its root span matches decides the rate, before `sampling_rate`; see [sampling rules](#sampling-rules) | at once |
| `send_traces` | `true` or `false`: export traces | at once |
| `send_metrics` | `true` or `false`: export metrics | at once |
| `send_logs` | `true` or `false`: export logs | at once |
| `send_logs_level` | `trace`, `debug`, `info`, `warn` (or `warning`), `error` or `fatal`, any case: export only the application's log records of that severity or above (at least SeverityNumber 1, 5, 9, 13, 17 or 21); left out, every severity | at once |
| `send_logs_level_by_scope` | comma-separated `pattern=level`: a level, or `off`, for the log records of the scopes a pattern (an exact scope name, or a prefix ending in `*`) matches; see [log levels per scope](#log-levels-per-scope) | at once |
| `drop_attributes` | comma-separated globs on attribute keys, ignoring ASCII case: remove the span, span event, span link and log record attributes they match, at export; see [redaction](#redaction) | at once |
| `mask_attributes` | as `drop_attributes`, but replace the value with `[REDACTED]`; a key both match is dropped | at once |
| `sampling_traces_per_second` | a number, more than 0 and at most 1000000 (decimals too: `0.5` is one every 2 s): the most traces a second this process starts or joins; see [rate caps](#rate-caps) | at once |
| `send_logs_per_second` | as `sampling_traces_per_second`: the most log records a second exported | at once |
| `debug_until` | an RFC 3339 time, at most 24 h ahead of the instance's clock: debug mode until then; see [debug mode](#debug-mode) | at once, until then |
| `debug_sampling_rate` | as `sampling_rate`, in debug mode in place of it and the rules; left out, 1 | at once, until `debug_until` |
| `debug_send_logs_level` | as `send_logs_level`, in debug mode in place of it and the levels per scope; left out, `debug` | at once, until `debug_until` |
| `deactivate_instrumentations` | comma-separated instrumentation names (lowercase letters, digits, `.`, `_`, `-`) | at the next start |
| `deactivate_all_instrumentations` | `true` or `false` | at the next start |
| `opamp_polling_interval` | a duration: `500ms`, `30s`, `2m`; from 1 s to 24 h | at the next start |
| `custom` | a JSON object of the application's own keys to values (or one in a string): not the SDK's, handed to the application to read; see [app-settings.md](app-settings.md) | at once |

Settings that take effect **at once** are carried out while the SDK runs: a sampler and exporters
that can be swapped (with the limits in [where each setting applies](#where-each-setting-applies)). `send_traces`, `send_metrics` and `send_logs` stop exports, not the telemetry
itself. Turned off, nothing more is sent, but an export already on its way still arrives. Turned
back on, what the SDK's batch processors still held from while it was off (what they collected
since their last export) goes out with the next exports. `send_logs_level` drops the log records
below it at export in the same way, from the next record on; the records are still made. A record
with no severity (SeverityNumber 0, unspecified) can't be judged, so it is exported whatever the
level. The others decide how the SDK starts: most SDKs can't remove an
instrumentation from a running process. (Go and Node.js can: an instrumentation's tracer
comes from a provider the integration hands out, so each switches them off at once; see
[the host](#the-host).) They are saved and put in force at the next start, which
needs somewhere to keep state (a state directory). Without one, a config that changes them is
answered FAILED; one that only changes live settings is fine.

Settings the layer doesn't carry out, EDOT's `logging_level` and `infer_spans` say, or any key
it doesn't know, are **ignored, not refused**, whatever their value: the same document goes to
every language, and a setting one SDK lacks shouldn't stop the others. So is a known setting this
SDK doesn't carry out, in this process ([the host](#the-host)): it is **set aside**, never in
force and never reported in force. The layer names the two apart (`ignored`: unknown keys;
`unsupported`: known settings set aside), and the server sees both: every integration reports
them beside the settings in force in its effective config ([the answer](#the-answer)). The
integrations also log each, at `info`, so with the default log level nothing shows there.
`send_logs_level` filters the application's exported log records, nothing more: `logging_level`,
EDOT's level for the agent's own diagnostic logging, stays ignored, and **no integration changes
the SDK's own log level**.

## Settings that match telemetry

Some settings match spans or log records by a pattern. A pattern is a **glob**: `*` matches any
run of characters, none included, and every other character is itself (`?`, `.` and `[` too).
Matching is by Unicode characters, so a language's UTF-8, UTF-16 or JavaScript strings all agree,
and case-sensitive unless a setting says otherwise. A pattern is at most 256 bytes. There are no
regular expressions: their dialects differ between languages, and every language has to match
the same way. The layer reads and checks these settings; each language matches with a small
matcher of its own (too often to call into the layer), held to the layer's by the shared test
vectors in `testdata/sdk-settings` and by the layer's `Evaluate` call.

### Sampling rules

`sampling_rules` sets the rate per kind of trace, first match wins:

```json
{
  "sampling_rate": "0.1",
  "sampling_rules": [
    {"rate": 0, "kind": "server", "path": "/health*"},
    {"rate": 1, "name": "checkout *"},
    {"rate": 0.5, "attributes": {"tenant.id": "acme", "db.system": "post*"}}
  ]
}
```

The same rules in a string, which is what a server that only sends strings (Kibana, o11yfleet's
renderer) can send:

```json
{"sampling_rules": "[{\"rate\": 0, \"kind\": \"server\", \"path\": \"/health*\"}]"}
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
  which the SDK's own trace-ID ratio sampler applies; if none matches, `sampling_rate` does; if
  that isn't set either, the application's own sampler. A span with a parent follows its parent
  whatever the rules say, as with `sampling_rate`: a sampled caller is still sampled under a rule
  of rate 0. With no rules and no `sampling_rate`, the application's own sampler decides every
  span, children too, as it did before these settings.
- A rule sees only what the span has when it starts. **`http.route` isn't a matcher**: most
  instrumentations add it after the span starts (name it under `attributes` where it is set at
  start), and HTTP server spans start named `GET`, so match them by `path` and `kind`, not by
  `name`.
- Up to 100 rules and 32 attributes a rule. An unknown field (`route` included), a missing or
  out-of-range `rate`, a bad `kind`, a pattern over 256 bytes or a field sent twice is FAILED,
  naming the rule: `sampling_rules[2].rate: 7 is not between 0 and 1`. An empty array is the
  setting left out.
- The effective config reports the rules as compact JSON, each rule's fields sorted and a rate of
  0 or 1 as an integer, so the same rules sent another way are no change.

### Log levels per scope

`send_logs_level_by_scope` sets the level per logger, by the scope name a log record is exported
under:

```json
{
  "send_logs_level": "warn",
  "send_logs_level_by_scope": "com.acme.db*=debug, urllib3*=error, noisy.lib=off"
}
```

Here records of `com.acme.db` and every scope whose name starts with it are exported from
`debug` up, `urllib3*` from `error`, none at all of `noisy.lib`, and every other scope's from
`warn`.

- A pattern is an exact scope name or a prefix ending in `*` (`*` alone: every scope). A prefix is
  by characters, not by dotted segments: `com.acme*` matches `com.acmesoft`. Names may not hold
  `,`, `=` or whitespace, and match case-sensitively.
- Levels are `send_logs_level`'s, in any case, plus `off`. A scope's level may be lower than
  `send_logs_level` as well as higher.
- A record's threshold is the level of the exact entry for its scope, else of the longest prefix
  that matches, else `send_logs_level`; with none of them, every record is exported. A record
  with no severity (0) is kept, except under `off`, which drops every record of its scope.
  `send_logs: false` drops them all whatever the levels.
- The scope is the name the record is exported under, which is what a scope *is* in each SDK:
  Python's and Java's logger name, .NET's ILogger category; in Node.js
  the bridge's package (`@opentelemetry/instrumentation-pino`), not the app's logger; in Go the
  name passed to `otelslog.NewLogger`. The empty scope name only `*` matches.
- Up to 256 entries. A pattern sent twice, a bad name or a bad level is FAILED; an empty list is
  the setting left out. The effective config reports the entries sorted by pattern, each level by
  its lowercase name: `*=info,com.acme.db=warn,com.acme.db*=debug`.
- A host that doesn't carry out `send_logs_level` can't carry this out either: the layer sets it
  aside whenever the host doesn't declare `send_logs_level`, even where it declares this one.

### Redaction

`drop_attributes` and `mask_attributes` keep attributes out of what is exported:

```json
{
  "drop_attributes": "http.request.header.authorization, http.request.header.cookie",
  "mask_attributes": "*password*, *token*, user.email"
}
```

Here the two headers are removed, and an attribute whose key holds `password` or `token`, or is
`user.email`, is exported as `"[REDACTED]"`. This follows Elastic APM's `sanitize_field_names`, in
pattern style and mask text, applied to OpenTelemetry attributes.

- Each is comma-separated globs on attribute keys, matched **ignoring ASCII case**
  (`Authorization` matches `authorization`; `É` doesn't match `é`). `*` alone matches every key.
- `drop_attributes` removes the attribute; `mask_attributes` replaces its value with the string
  `[REDACTED]`, whatever its type (a masked integer becomes a string). A key both match is
  dropped. A drop doesn't change `dropped_attributes_count`.
- They apply at export, through the integration's own gates, to span attributes, span event
  attributes, span link attributes and log record attributes. The application's own span or
  record isn't changed: another exporter (one the gate doesn't cover) sees the original.
- Out of scope: resource and scope attributes, span names, log bodies (map bodies too), and
  **metrics**, in every language. Removing or masking a data point's attribute would merge
  distinct series into one, so the exported data would be wrong.
- Up to 128 patterns each, each at most 256 bytes. An empty list is the setting left out. The
  effective config reports the patterns lowercase, without repeats, sorted:
  `*password*,*token*,user.email`.

### Rate caps

`sampling_traces_per_second` and `send_logs_per_second` are safety valves: whatever the rates
and levels say, no more than this many a second.

```json
{"sampling_rate": "1", "sampling_traces_per_second": "50", "send_logs_per_second": "200"}
```

- Each is a number, more than 0 and at most 1000000; decimals are allowed (`0.5` is one every
  2 s). 0 is refused: to send none, use `sampling_rate` 0 or `send_logs` false.
- Each is a token bucket, the same algorithm in every language (pinned by
  `testdata/caps.json`): it starts full, refills at n a second on a monotonic clock, and holds at
  most a second's worth (at least one token).
- **Traces, not spans.** The trace cap is in the sampler, after the rules and the rate have said
  to sample. A root span takes a token, and so does a span whose remote parent was sampled: a
  cap has to hold behind a traced caller, unlike `sampling_rate`. Without a token the span is
  dropped. A child of a span sampled in this process is always sampled and takes none, since
  refusing it would break its trace.
- **Logs.** The log cap is at the log gate, after the levels: of a batch, the first records the
  tokens allow go on and the rest are dropped. With the SDKs' default batch delay (1 s) that lets
  n records a second through; a longer `OTEL_BLRP_SCHEDULE_DELAY` makes the cap stricter.
- Changing n starts a full bucket; leaving the setting out removes the cap at once. The caps stay
  in force in debug mode.

## Debug mode

For a while, everything: `debug_until` puts an instance in debug mode until a time, and it ends on
its own, whether the server can still reach the instance or not.

```json
{"sampling_rate": "0.05", "send_logs_level": "warn", "debug_until": "2026-10-14T15:30:00Z"}
```

```json
{"debug_until": "2026-10-14T15:30:00Z", "debug_sampling_rate": "0.5", "debug_send_logs_level": "info"}
```

- Until then, `debug_sampling_rate` (1 if left out) is the sampling rate, and the sampling rules
  are set aside; `debug_send_logs_level` (`debug` if left out) is the log level, and the levels
  per scope are set aside. Everything else stays: the `send_*` switches (a `send_logs` false
  still sends nothing), redaction, and the rate caps, which keep debug mode from flooding the
  backend.
- **A time, not a duration.** The server sends the same config again on every reconnect, and an
  instance loads the saved document at every restart: a duration would start again each time.
  A time ends when it says, however often it is sent. To end debug mode early, send a config
  without `debug_until`; to extend it, a later time.
- The instance's own clock decides. A time more than 24 h ahead of it is refused (FAILED: the
  server's clock, or this one, is far off). A time at or before now is applied but over: the
  settings are as without it, and it isn't reported in force. Times are RFC 3339 with a `Z` or an
  offset (`2026-10-14T17:30:00+02:00`), a fraction allowed; no leap seconds.
- `debug_sampling_rate` and `debug_send_logs_level` without `debug_until` are refused: they would
  do nothing. They are checked like `sampling_rate` and `send_logs_level`, and set aside where the
  integration doesn't declare those.
- The effective config reports all three while debug mode lasts, as sent, and none of them after.
  Each decision says when to ask again (`refresh_at_unix_ms`): the integration runs a timer and,
  when it fires, calls `Refresh` and puts in force what it returns, and reports the new effective
  config. A restart before the end is still in debug mode, after it, not.
- An integration that doesn't give the layer the time (`now_unix_ms` 0), or doesn't declare
  them (see [the host](#the-host)), sets them aside.

## Export health

Always on, with no setting: it is reporting, not behaviour. Each integration counts what its
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
  exporter's error) or `off` (its `send_*` is false: off on purpose, not failing). The next
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
or off, the log levels, the log cap, redaction, and the counts export health reports). A setting
reaches the telemetry that goes through them, and nothing else.

| Setting | Python | Node.js | Go | .NET plugin | .NET `WithOpAmp` | Java agent |
|---|---|---|---|---|---|---|
| `sampling_rate` | at once | at once ³ | at once | at once | at once | at once |
| `send_traces`, `send_logs` | at once ¹ | at once ¹ | at once | at once ¹ | at once ¹ | at once ¹ |
| `send_logs_level` | at once ¹ | at once ¹ | at once | at once ¹ | at once ¹ | at once ¹ |
| `sampling_rules` | at once ⁴ ⁵ | at once ³ ⁵ | at once ⁵ | at once ⁴ ⁵ | at once ⁴ ⁵ | at once ⁵ |
| `send_logs_level_by_scope` | at once ¹ ⁴, per logger | at once ¹ ⁴, per logs API logger or bridge | at once, per `otelslog` logger | at once ¹ ⁴, per `ILogger` category | at once ¹ ⁴, per `ILogger` category | at once ¹, per logger |
| `drop_attributes`, `mask_attributes` | at once ¹ ⁴ | at once ¹ ⁴ | at once | at once ¹ ⁴, in the OTLP bytes | at once ¹ ⁴, in the OTLP bytes | at once ¹ |
| `sampling_traces_per_second`, `send_logs_per_second` | at once ¹ ⁴, per process | at once ¹ ³ ⁴ | at once | at once ¹ ⁴ | at once ¹ ⁴ | at once ¹ |
| `debug_until`, `debug_sampling_rate`, `debug_send_logs_level` | at once, until then ⁴ | at once, until then ⁴ | at once, until then | at once, until then ⁴ | at once, until then ⁴ | at once, until then |
| [export health](#export-health) (no setting) | reported ⁶ | reported ⁶ | reported ⁶ | reported ⁶ | reported ⁶ | reported ⁶ |
| `send_metrics` | at once, `otlp` only ¹ | at once ¹ | at once, push only ¹ | at once ¹ | at once ¹ | at once, Prometheus too |
| `deactivate_instrumentations`, `deactivate_all_instrumentations` | next start | at once ² | at once | next start | not carried out | next start |
| `opamp_polling_interval` | next start | next start | next start | next start | next start | next start |
| `logging_level`, `infer_spans` | ignored | ignored | ignored | ignored | ignored | ignored |
| `custom` ([app-settings.md](app-settings.md)) | at once, read by the application | at once, read by the application | at once, read by the application | at once, read by the application | at once, read by the application | at once, read by the application |

¹ Only for the exporters the integration builds: OTLP and console (and Java's Prometheus reader).
Another exporter, a span or log processor passed in or added later, or another agent extension's
processor goes on exporting. Where the integration sees such an exporter when it builds the SDK,
it doesn't declare that signal's settings, so they are set aside, named as not carried out:
Python for exporters it doesn't build and span or log processors or metric exporters passed to
`configure()`; Node.js for exporters it doesn't build, and from the moment the application
registers a global tracer, logger or meter provider of its own in place of the SDK's; the .NET plugin and `WithOpAmp` whenever
one of a signal's exporters can't be stopped (not OTLP, beside another exporter, or, in
`WithOpAmp`, with a path or an `HttpClientFactory` of its own); the Java agent under a
configuration file for a span or log processor that is neither batch nor simple, or a metric
reader neither periodic nor pull. A processor added after the SDK is built isn't seen: the
setting is still answered APPLIED. (In Node.js the SDK's providers take no processor once built.
A provider the application builds and never registers isn't seen either.)

² For the instrumentations loaded at start: the server switches them off and on again, but one
the `OTEL_NODE_*_INSTRUMENTATIONS` variables (or upstream's defaults) leave out isn't loaded, and
a switched-off one stays loaded, recording nothing ([node-otel.md](node-otel.md#switching-instrumentations-off)).

³ In Node.js the sampler samples the spans of the SDK's tracer provider only. Once the
application registers a global tracer provider of its own, its spans go past the sampler, so
`sampling_rate`, `sampling_rules`, `sampling_traces_per_second` and `debug_sampling_rate` are set
aside from then on ([node-otel.md](node-otel.md#a-provider-of-the-applications-own)).

⁴ Declared to the layer (see [the host](#the-host)) only where the integration carries it out,
as every setting is; elsewhere it is set aside: named as not carried out, never reported in force,
and the rest of the config still APPLIED. Python declares every one only where its configurator
built the SDK (not under another configurator or `OTEL_CONFIG_FILE`). Node.js declares
`debug_until` always, and `sampling_rules`, `sampling_traces_per_second` and
`debug_sampling_rate` unless the application registers a tracer provider of its own (footnote ³), the log ones (`send_logs_level_by_scope`,
`send_logs_per_second`, `debug_send_logs_level`) only where it builds the log exporters, and
redaction only where it builds both the span and the log exporters. The .NET plugin and
`WithOpAmp` declare the log ones where every logs exporter is the gated OTLP one, redaction where
every span and log record exported goes through a gate (traces and logs each gated, or not
exported), `sampling_rules` and `sampling_traces_per_second` where `sampling_rate` is carried
out, and debug mode always (its rate and level set aside where `sampling_rate` and
`send_logs_level` are). Go declares every one, always; the Java agent every one but where
footnote ¹ says.

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
- **The rate replaces the application's sampler** while one is set (`OTEL_TRACES_SAMPLER`, even
  `always_off`); left out again, the SDK's own sampler is back.
- **Turned off means dropped at export.** Spans, metrics and logs are still made while a `send_*`
  setting is false (a span is still sampled, so services downstream still trace it); they are
  only dropped at export. Cumulative metrics carry the counts made meanwhile into the next export.
- **An SDK the application builds on its own isn't reached.** Python (`opamp_otel.configure()`)
  and .NET (`WithOpAmp()`) let an application keep its own setup; in Node.js and Go the
  integration's setup has to be the application's whole SDK setup, and in Java the agent's SDK.
  Node.js sets aside what a provider the application registers as the global one takes past it,
  but can't see one the application never registers.
- **Declarative configuration (`OTEL_CONFIG_FILE`) builds the SDK from the file.** The Python
  integration still joins the fleet and switches instrumentations at the next start, but the
  file's sampler and exporters aren't its own: `sampling_rate` and the `send_*` settings are
  ignored. The Java agent extension takes the configuration file's model instead and carries out
  every setting as without it ([javaagent.md](javaagent.md)).

## The answer

For each remote config the SDK answers with a `RemoteConfigStatus`: APPLIED or FAILED, the
config's hash, and on FAILED the reason (`sampling_rate: 7 is not between 0 and 1`). The reason
quotes at most 64 characters of a value or key, then `… (N bytes)`, so a huge value sent wrong
doesn't come back as a huge reason. It then
reports its **effective config**: the `elastic` entry again, with the settings in force.

**The client keeps the config applied.** A config is answered in two steps, the same in every
language. The layer first decides what to keep (`Decide` returns a `Plan`: the document to keep,
or nothing when the config is refused), and then gives the answer (`Commit`). With a state
directory the integrations save nothing themselves: the client keeps the config the application
answered APPLIED in its own saved state, and tells the server APPLIED only once that save has
landed. Until then, and while a save fails (the client tries again), the server hears APPLYING.
So the integration commits it as saved. With no state directory there is nowhere to keep it: a
config that changes a next-start setting is refused (`… take effect at the next start, which needs
somewhere to keep state`), and one that changes only live settings is APPLIED. A save that fails,
for a host that makes one itself, refuses the config (`can't save the settings: …`) and changes
nothing. The integration never rewrites the answer: each language's binding runs the two steps,
so the integration makes one call.

- Live settings show as applied: as sent, but `send_logs_level` by its lowercase name (`warn` for
  `WARNING`), `sampling_rules` as canonical JSON, `send_logs_level_by_scope` sorted, and the
  redaction patterns lowercase and sorted.
- Next-start settings show as they were at start, until the restart. A server that compares the
  effective config with what it sent sees them pending, which they are. One sent another way but
  meaning the same (`30s` for `30000ms`, `b,a` for `a,b`) is no change: it isn't pending, and
  shows as sent.
- Ignored and set-aside settings never show in the `elastic` entry. They are named in an entry
  of their own beside it, `elastic.set_aside` (`application/json`), so the server can tell a
  setting this SDK won't carry out from one still pending or not yet applied:

  ```json
  {"ignored":["infer_spans","logging_level"],"unsupported":["send_metrics"]}
  ```

  `ignored` lists the keys that are no setting (another SDK's, or misspelt), `unsupported` the
  settings this integration doesn't carry out where it runs ([the host](#the-host)), each sorted.
  The entry is left out when nothing was set aside. Like the `elastic` entry, it is about the
  document in force: after a FAILED it still names what that document set aside, and a restart
  reports it again. The status says nothing of either: `RemoteConfigStatus.error_message` is
  FAILED's alone, and a document with settings set aside is APPLIED. The layer builds both entries
  (`Load`, `Commit` and `Refresh` each return the whole effective config), so every language
  reports them the same way. The reference server's log shows an effective config's JSON entries
  on its `a2s` line, as `effective`.
- **On the server.** The reference server keeps, for each agent while it knows it, the `elastic`
  entry in force (up to 16 KiB), what was set aside, and the reason for the last config it
  answered FAILED (until an APPLIED). `App::explain` shows them: `sdk_settings` has `in_force`,
  `ignored`, `unsupported` and `not_in_force`, and `config.refused` has the hash and reason.
  `not_in_force` lists the settings of the document wanted that aren't in force as wanted and
  weren't set aside: not applied yet, or a next-start setting waiting for a restart. The wanted
  document is read by the layer, as an SDK that carries out every setting would report it
  applied, so a level sent as `WARNING` and in force as `warn` counts as in force. When what an
  agent sets aside changes, the log says so once, with a `settings_set_aside` event (`uid`,
  `ignored`, `unsupported`). A restarted server, or one that takes an agent over from another,
  keeps none of this, and an SDK sends its effective config only when it changes: so the server
  asks for the full state in its reply to the agent's next message that leaves the effective config
  out, and learns these again from the answer. It asks until the effective config comes, or a full
  report without it says the agent has none to send, and no more. An SDK on plain HTTP that
  the server offers no heartbeat polls at the `opamp_polling_interval` in force: the server lets
  it be silent for three of those before it counts it disconnected, not three of its default
  30 s.

## The layer

The layer is the rules as pure functions, compiled to `opamp_sdk_settings.wasm`, which each
language's client ships; it comes with the core, pinned by hash
([how-it-works.md](how-it-works.md#where-the-core-comes-from)), and `scripts/gen_sdk_settings.py`
generates each language's types for it. It is separate from the client core: its own module and
its own interface, `proto/opamp/sdk/v1/settings.proto`. The module keeps nothing between calls. The
integration keeps the settings documents it is handed and passes them back.

| Call | When | Returns |
|---|---|---|
| `Load{core_state, saved, host, now_unix_ms}` | at start, before the SDK is built, with the client's saved state and the document kept before ([at start](#at-start)) | the settings to start with (next-start ones included), the document in force, the effective config, and a warning if the saved document was unreadable (the SDK then starts with its own); when to ask again (`refresh_at_unix_ms`) |
| `Decide{config, current, at_start, host, now_unix_ms}` | a remote config arrived | a `Plan`: the document to save before answering (none when the config is refused) |
| `Commit{decide, saved, error}` | once the plan's document is saved, or its save failed, or there is nowhere to save it ([the answer](#the-answer)) | the `RemoteConfigStatus` to send, the settings to put in force (only when APPLIED), the document now in force, the effective config, the next-start settings pending a restart, and when to ask again |
| `Refresh{current, at_start, host, now_unix_ms}` | at the time a `Load`, `Commit` or `Refresh` said to ask again (`refresh_at_unix_ms`, 0 for never): the end of debug mode | the settings to put in force now, the effective config, when to ask again, and a warning if the document in force no longer reads on this clock (one moved back a day, say: the SDK's own settings are then in force) |
| `Health{counts, previous, now_unix_ms, settings}` | every 30 s, from the `Refresh` timer, with what the gates counted since start, the report the last call answered, and the settings in force | whether to send now, and the `exporters` entry to put in the agent's health (when not sending, the one last sent): see [export health](#export-health) |
| `Describe{resource}` | at start | the agent description: `service.name`, `service.namespace` and `service.instance.id` identify the instance; the rest of the resource describes it |
| `Components{inventory, host, now_unix_ms}` | once the SDK is built, and when what it lists changes | the instance's inventory as its available components, for `set_available_components`: the integration's and SDK's versions, its instrumentations and exporters, and the settings the host carries out ([available-components.md](available-components.md#what-the-sdk-integrations-report)) |
| `Evaluate{settings, host, input}` | in tests only | what the layer's reference matchers make of one input under a settings document: how a span is sampled, whether a log record passes `send_logs` and the levels, what redaction leaves of a list of attributes, what a rate cap lets through. Each language's tests hold its own matchers to it |

### At start

The settings to start with are those of the last config applied. The client keeps that config in
its saved state (the state file in the state directory, beside the last config received, which it
hands over again at start): it saves it when the application answers APPLIED, and reports APPLIED
only once that save has landed. `Load` takes the state as the client saved it (`core_state`) and
reads the config's `elastic` entry from it. A config applied without an `elastic` entry is every
setting the SDK's own. So a config the application refused, received after the one applied, is
never what a restart starts from.

With no config applied in that state (none saved, a state saved by an earlier client that didn't
keep one, or one that doesn't read), `Load` reads `saved`: the document the integration kept
itself before (`sdk-settings.json` in the state directory). So the settings carry over from an
earlier release until a config is applied. The integrations no longer write that file.

The config applied survives an endpoint change. The client drops the config received from the
old server (the new server's config wins) but keeps the applied one, as it keeps packages
installed: its settings are in force on the agent, and stay so until the new server's config is
applied. A new server then sees in the effective config what the agent actually runs.

### The host

An integration **declares** every setting it carries out, with a `Host` passed to `Load`,
`Decide` and `Refresh`, from what it built: a setting it leaves out is set aside, so a host never
answers APPLIED for a setting it doesn't carry out. Beside it each of them takes `now_unix_ms`,
the instance's clock in milliseconds since the Unix epoch, which only debug mode reads; the layer
reads no clock of its own.

- `live`: the settings it carries out at once. A next-start setting listed here is then live in
  every way: put in force by the decision, reported in force at once, never pending, and it needs
  no state directory. The Go and Node.js integrations list `deactivate_instrumentations`
  and `deactivate_all_instrumentations` here.
- `next_start`: the next-start settings it carries out at the next start
  (`deactivate_instrumentations`, `deactivate_all_instrumentations`, `opamp_polling_interval`).
  A live setting can't be listed here.
- A setting in neither is set aside: not checked, never in force, never reported in force, and
  named in the decision's `unsupported` list (unknown keys go in `ignored`). Each integration
  declares a signal's sending and what its gate holds only where every exporter of the signal is
  gated, and the sampler's settings only where its sampler is in force ([where each setting
  applies](#where-each-setting-applies), footnotes ¹ and ⁴). `send_logs_level_by_scope` and
  `debug_send_logs_level` are set aside without `send_logs_level`, and `debug_sampling_rate`
  without `sampling_rate`, declared or not; debug mode also needs the clock. `custom`, the
  application's own settings ([app-settings.md](app-settings.md)), is declared where the
  application can read them.
- A host that names what is no setting (a misspelling, say) is refused: the call is an error,
  naming it. So is one that lists a live setting in `next_start`.

With no host, nothing is declared and every setting is set aside. The rules stay the layer's: an
integration only says what its SDK can do.

Every integration also keeps its OpAMP client's own requests out of the application's telemetry.
The client runs with instrumentation suppressed, as OpenTelemetry's own exporters do (in Go,
where instrumentation wraps a transport, with an HTTP transport of its own), so its requests and
its goodbye are never traced. The end-to-end check fails if one is. This is built around the
platform's HTTP stack, so every integration asks for its client's platform mode by name
([sdks.md](sdks.md#rust-mode-and-platform-mode)) rather than the clients' default, Rust mode.

What stays in each language is what touches its SDK:

- a sampler and exporters it can swap;
- how it switches instrumentations off, at start or while it runs;
- where it keeps state;
- its hooks into the SDK or agent.

The Java agent extension, for example, is its controls (a live sampler, three gated
exporters and a gated pull metric reader), a mapping from the next-start settings to agent properties
(or, under declarative configuration, to the configuration model), and the agent's extension hooks.

## How it is checked

- The layer is checked on its own before it is released: every setting and every refusal, and
  properties over generated documents (any bytes are read or refused with a reason; a valid
  document is carried out or named as ignored; an answer is all or nothing, with the effective
  config and the pending settings exactly as described above; the same next-start settings sent
  another way are no change).
- `testdata/sdk-settings/*.json`: the shared test vectors, each a settings document, an input and
  the outcome: `settings.json` (how documents are read and refused), `sampling.json` (how a span
  is sampled), `logs.json` (whether a log record passes the levels), `redact.json` (what
  redaction leaves of a list of attributes), `caps.json` (what a rate cap lets through, event by
  event), `debug.json` (debug mode over time: `Load`, `Decide` and `Refresh` in turn, and what the
  effective config names as set aside through them), `saving.json` (the answer from what became
  of the save: saved, failed, nowhere to save; the integrations, whose saves are the client's, run
  its saved and nowhere cases), `start.json` (the document `Load` starts from: the config applied
  in the client's saved state, or the one kept before),
  `components.json` (what the inventory lists,
  [available-components.md](available-components.md#what-the-sdk-integrations-report)),
  `health.json` (export health call after call: what is reported, and when it is sent),
  `custom.json` (the application's own settings: the maps taken and refused, and the typed
  reads). The layer runs every one, and through `Evaluate`; each language's wrapper and
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
  `check_signals` sends each setting that matches telemetry to each integration in turn, and
  checks what arrives at the sink: a debug window that ends on the agent's timer, a sampling rule,
  the application's loggers off by scope, a masked and a dropped URL, both rate caps, and export
  health; and that an EDOT setting sent beside them is named as set aside in the effective config
  the agent reports ([examples/README.md](../examples/README.md)).
