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
| Python | `logfire-fleet-otel`, [python-otel.md](python-otel.md) | installed next to `opentelemetry-instrument`: a distro, a configurator and the SDK's OpAMP hook |
| Node.js | `@pydantic/logfire-fleet-otel`, [node-otel.md](node-otel.md) | `node --import @pydantic/logfire-fleet-otel/register`, in place of upstream's `register` |
| Go | `github.com/pydantic/opamp-clients/go/opampotel`, [go-otel.md](go-otel.md) | `opampotel.Setup(ctx)` in place of the application's own SDK setup |
| .NET | `Logfire.Fleet.AutoInstrumentation`, [dotnet-otel.md](dotnet-otel.md) | a plugin for upstream's automatic instrumentation (`OTEL_DOTNET_AUTO_PLUGINS`) |

To wire a client to an SDK by hand instead, see [sdks.md](sdks.md).

## The policy

The policy is a Logfire SDK policy: the remote config's only file, `logfire-sdk-policy.json`
(`AgentConfigMap.config_map["logfire-sdk-policy.json"]`, content type `application/json`). It is
one JSON object. Its `schema` is `logfire-sdk-policy/v1`. A control is `{"mode": "local"}`, which
is the SDK's own value as configured locally, or `{"mode": "override", ...}` with the fields of the
override. Every policy sends `head_sampling` and `signal_exports`, and the shortest policy is those
two alone:

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

Every other control ([the controls](#the-controls)) may be sent too, and a control left out is
`local`:

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "override", "ratio": 0.25},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "override", "rules": [{"path": "/health*", "rate": 0}]},
  "log_levels": {"mode": "override", "level": "warn", "scopes": {"sqlalchemy.engine": "error"}},
  "attribute_redaction": {"mode": "override", "mask": ["*token*"], "drop": ["http.request.header.cookie"]},
  "app_settings": {"mode": "override", "values": {"checkout.new_flow": true}}
}
```

[examples/settings](../examples/settings/README.md) has a policy for each common
use (cut trace volume, trace everything while debugging, stop metrics and logs, quiet the logs,
switch instrumentations off).

- The policy is the whole of what the server wants. A control left `local` is the SDK's own
  value, so a policy states everything that should differ from the SDK's own, not only what
  changes.
- `head_sampling` and `signal_exports` (with all three signals) are required. A missing one, a
  field or control that the policy does not name, a bad value, or a field sent twice (JSON
  parsers disagree on which one wins) refuses the policy. The answer is FAILED, the reason starts
  with the control (`head_sampling: ratio must be a number from 0 to 1`), and nothing changes.
- The whole policy is applied, or none of it. This includes an override that this SDK does not
  carry out where it runs ([the host](#the-host)): the SDK refuses the policy with
  `signal_exports.metrics: this SDK cannot apply the requested override`, naming the field as
  well where a control has several (`log_levels: scopes: this SDK cannot apply the requested
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
| `scrubbing` | `defaults`, `true`: scrub what the Logfire SDK scrubs by default. `patterns`, an array of globs matched against a whole key or string value, ignoring ASCII case. Scrubs span, event, link and log attributes, and log bodies, at export. See [scrubbing](#scrubbing). At least one of the two. | at once |

The policy has no control for how often the SDK checks in: the server's OpAMP heartbeat offer
(`OpAMPConnectionSettings.heartbeat_interval_seconds`) sets that, at once.

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
| `scrubbing` `defaults`, `patterns` | `scrub_defaults`, `scrub_patterns` |

Controls that take effect **at once** are carried out while the SDK runs: a sampler and exporters
that can be swapped (with the limits in [where each setting applies](#where-each-setting-applies)).
`signal_exports` stops exports, not the telemetry itself. Turned off, nothing more is sent, but an
export already on its way still arrives. Turned back on, the SDK's batch processors send what they
still held from while it was off (what they collected since their last export) with the next
exports. The log level drops the log records below it at export in the same way, from the next
record on. The records are still made. A record with no severity (SeverityNumber 0, unspecified)
cannot be judged, so it is exported whatever the level.

The next-start control, `instrumentations`, decides how the SDK starts. Most SDKs cannot remove
an instrumentation from a running process. Go and Node.js can, because an instrumentation's tracer
comes from a provider that the integration hands out. Thus each of them switches instrumentations off at once
([the host](#the-host)). Elsewhere the SDK keeps the control and puts it in force at the next
start. That needs somewhere to keep state (a state directory). Without one, a policy that changes
it is refused (`instrumentations: takes effect at the next start, which needs somewhere to keep
state`). A policy that changes only live controls is fine.

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
of a policy that it is about:

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
- Out of scope: resource and scope attributes, span names, log bodies (map bodies too;
  [scrubbing](#scrubbing) reaches them), and **metrics**, in every language. Removing or masking a data point's attribute would merge
  distinct series into one, so the exported data would be wrong.
- Up to 128 patterns each, each at most 256 bytes, with no comma. An empty array is refused. The
  effective config reports the patterns lowercase, without repeats, sorted.

### Scrubbing

`scrubbing` scrubs values, as the Logfire SDK's own scrubbing does, where redaction only goes by
keys:

```json
{"scrubbing": {"mode": "override", "defaults": true, "patterns": ["*iban*", "sk_live_*"]}}
```

Here an attribute whose key or value looks secret to the Logfire SDK (`password`, `api_key`,
`session`, `jwt` and the rest of its default patterns) is exported as
`"[Scrubbed due to '<what matched>']"`, and so is one whose key or whole string value is like
`*iban*` or `sk_live_*`.

- `defaults: true` is the Logfire SDK's default patterns and safe keys (logfire 5.2.0): `password`,
  `passwd`, `mysql_pwd`, `secret`, `auth` (not `author` or `authors`), `credential`,
  `private key`, `api key`, `session`, `cookie`, `social security`, `credit card`,
  `logfire token` (each pair with at most one of `.`, `_`, space or `-` between, or none), a
  Logfire token's prefix (`pylf_v1_`), and `csrf`, `xsrf`, `jwt` and `ssn` as words of their own.
  They match anywhere in a key or a value, ignoring case as Python does (`ſ` is `s`, the Kelvin
  sign is `k`, `İ` and `ı` are `i`). A word boundary is ASCII: only ASCII letters, digits and `_`
  are word characters. A value that is all the match (`password` alone) says nothing secret and
  is kept, as the Logfire SDK keeps it.
- `patterns` are globs, as in redaction, matched ignoring ASCII case against a whole key or a
  whole string value. The default patterns name the match first: with both, a key or value they
  match names what they matched, and the patterns are tried after.
- A key that matches has its value replaced by the string, whatever its type (a list becomes one
  string). Otherwise its string value is scrubbed if it matches, each string in a list one by
  one, and each member of a map as an attribute of its own. A safe key's value is never touched,
  at any depth: `exception.message`, `db.statement`, `url.full`, `http.target`,
  `gen_ai.input.messages` and the rest of the Logfire SDK's list, each matched exactly, case and
  all.
- The Logfire SDK renders a span's arguments into its message (`logfire.msg`, a safe key). Where
  redaction or scrubbing changes an argument the message template (`logfire.msg_template`) names,
  the message is exported as its template, which holds no value (Python and Node.js, where the
  Logfire SDK runs).
- It runs after redaction, on what redaction keeps: a dropped attribute is gone and a masked one
  stays `[REDACTED]`. It reaches what redaction does (span, span event, span link and log record
  attributes) and **log bodies** too, a string body or a map body's members. The application's
  own span or record isn't changed. It adds to the SDK's own scrubbing: a Logfire SDK that
  scrubs on its own still does.
- An override needs `defaults: true` or patterns, or both. `defaults: false` adds no defaults; it
  does not turn off the SDK's own scrubbing. Up to 128 patterns, each at most 256 bytes, with no
  comma, named lowercase in the replacement. An empty array is refused. The effective config
  reports the patterns lowercase, without repeats, sorted, and the first of them in that order
  names a value that several match. The SDK does not add Logfire's `logfire.scrubbed` attribute.
- The matchers are each language's own, held to the layer's by the shared vectors (`scrub.json`)
  and its `Evaluate` call, which takes a log body too. They differ from the Logfire SDK where
  its regular expressions can't be matched the same in every language: a word boundary is ASCII
  only, a key that matches with a list value gives one string where the Logfire SDK gives a list
  of one, and a JSON string is one string value, scrubbed whole.

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
  The layer's [agent flow](#the-agent-flow) ends it: it asks to be woken at the end, on the
  monotonic clock (a wall clock moved back or forward doesn't move it), then puts in force the
  settings without debug and reports the new effective config. A config refused after the wall
  clock passed the end ends it too. A restart before the end is still in debug mode, after it, not.
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
- **When it is sent.** The [agent flow](#the-agent-flow) builds the entry and sends it: at once
  when a `healthy` or `status` changes (new exporters included), at most every 60 s while only the
  counters move, and not at all while nothing changes. It looks every 30 s, on the wake it asks
  for, with the counts the integration gives it.
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
| `scrubbing` | at once ¹ ⁴ | at once ¹ ⁴ | at once ¹ ⁴ | at once ¹ ⁴, in the OTLP bytes | at once ¹ ⁴, in the OTLP bytes | at once ¹ |
| `trace_rate_limit`, `log_rate_limit` | at once ¹ ⁴, per process | at once ¹ ³ ⁴ | at once ¹ ⁴ | at once ¹ ⁴ | at once ¹ ⁴ | at once ¹ |
| `debug` | at once, until then ⁴ | at once, until then ⁴ | at once, until then ⁴ | at once, until then ⁴ | at once, until then ⁴ | at once, until then |
| [export health](#export-health) (no control) | reported ⁶ | reported ⁶ | reported ⁶ | reported ⁶ | reported ⁶ | reported ⁶ |
| `signal_exports.metrics` | at once, push exporters ¹ | at once ¹ | at once, push only ¹ | at once ¹ | at once ¹ | at once, Prometheus too |
| `instrumentations` | next start | at once ² | at once ⁴ | next start | refused | next start |
| `app_settings` ([app-settings.md](app-settings.md)) | at once, read by the application | at once, read by the application | at once, read by the application | at once, read by the application | at once, read by the application | at once, read by the application |

A collector doesn't take the policy: its remote config is its YAML, and the
[Supervisor](collector.md) answers FAILED to a remote config that holds a policy.

¹ The policy governs every exporter the SDK exports through, not just the ones the integration
builds: manage in Logfire, send anywhere. Each integration puts its gate in front of every
exporter it can reach on the SDK's providers (the SDK's own, console included, a processor passed
in or added later, another plugin's OTLP exporter in .NET), and one decision per span or log
record holds for every exporter it reaches, with one rate cap budget per process. What a gate
can't be put in front of is never a reason to refuse a policy: it is reported to the server as
ungoverned, listed with `gated: false` in the [inventory](available-components.md) and with the
status `ungoverned` (healthy all the same) in [export health](#export-health), and the integration
logs it. The policy is APPLIED and stays in force on everything governed. Reported ungoverned:
Python, a processor of the application's own that holds an exporter itself, a pulled metric
reader (Prometheus), or providers that aren't the SDK's; Node.js, a pulled reader, and from the
moment the application registers a global tracer, logger or meter provider of its own in place
of the SDK's (`app_provider`); Go, a metrics reader autoexport keeps (Prometheus, producers), and a
global provider the application sets in place of `Setup`'s (`app_provider`, seen before each
decision and once a second, [go-otel.md](go-otel.md#where-the-server-cant-reach)); the .NET
plugin and `WithOpAmp`, every exporter that isn't the gated OTLP one (the gate is the OTLP
exporter's HTTP handler: console, Zipkin, Prometheus, another plugin's exporter of another kind,
or in `WithOpAmp` one with a path or an `HttpClientFactory` of its own), and the plugin's traces
and metrics under `OTEL_DOTNET_AUTO_SETUP_SDK=false` (`app_provider`); the Java agent, a span or
log record processor or a metric reader another agent extension adds through the autoconfigure
customizers. .NET reads the built providers again, before each decision and every 30 s, and the
plugin reads each logger provider on its first log record. A provider a Node.js or Go application
builds and never registers or sets as global isn't seen.

² For the instrumentations loaded at start: the server switches them off and on again, but one
the `OTEL_NODE_*_INSTRUMENTATIONS` variables (or upstream's defaults) leave out isn't loaded, and
a switched-off one stays loaded, recording nothing ([node-otel.md](node-otel.md#switching-instrumentations-off)).

³ In Node.js the sampler samples the spans of the SDK's tracer provider only. A global tracer
provider the application registers of its own is reported ungoverned, and the sampler's controls
stay in force on the SDK's spans ([node-otel.md](node-otel.md#a-provider-of-the-applications-own)).

⁴ Declared to the layer (see [the host](#the-host)) only where the integration carries it out,
as every control is. Elsewhere the SDK refuses a policy that overrides it, and nothing changes.
Every integration declares the gates' controls always (footnote ¹). The sampler's controls
(`head_sampling`, `sampling_rules`, `trace_rate_limit` and `debug` (`sampling_ratio`)) are the one
exception: they are not declared where the integration's sampler isn't the one in force. Python
leaves them undeclared where another configurator or `OTEL_CONFIG_FILE` built the SDK; the .NET
plugin and `WithOpAmp` where they couldn't wrap the sampler (under
`OTEL_DOTNET_AUTO_SETUP_SDK=false`, say); the Java agent where another agent extension sets a
sampler of its own in place of the extension's, and from then on an override of them already in
force is set aside.

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
nothing through a gate, so it is reported `ungoverned`; in Python, a metric exporter that is
an ungoverned one, and forked workers' exports (they don't report). In .NET each attempt
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
- **An SDK the application builds on its own isn't reached.** Python (`logfire_fleet_otel.configure()`)
  and .NET (`WithOpAmp()`) let an application keep its own setup; in Node.js and Go the
  integration's setup has to be the application's whole SDK setup, and in Java the agent's SDK.
  Node.js and Go report a provider the application registers or sets as the global one as
  ungoverned. A provider it builds and never registers or sets as global can't be seen: the
  server may hear APPLIED for a setting its telemetry doesn't follow. The Java agent extension
  can't see an `OpenTelemetrySdk` the application builds and uses itself: that SDK's telemetry
  escapes every setting, and the extension still answers for the agent's SDK. In .NET, a tracer or
  meter provider the application builds beside the managed one listens to the same
  instrumentations. The integration can't see it, so the settings stay answered APPLIED while its
  telemetry goes on.
- **Declarative configuration (`OTEL_CONFIG_FILE`) builds the SDK from the file.** The Python
  integration still joins the fleet and switches instrumentations at the next start, but the
  file's sampler is not its own. Thus it refuses a policy that overrides `head_sampling` (and the
  sampler's other controls), while its gates go in front of the file's exporters as in front of
  any other. The Java agent extension takes the configuration file's
  model instead and carries out every control as without it ([javaagent.md](javaagent.md)).

## The answer

For each remote config the SDK answers with a `RemoteConfigStatus`: APPLIED or FAILED, the
config's hash, and on FAILED the reason (`head_sampling: ratio must be a number from 0 to 1`). The
reason starts with the control it is about, or with `config_map:`, `schema:` or `policy:` when
the policy as a whole is wrong. It quotes at most 64 characters of a value or key, then
`… (N bytes)`, so a huge value sent wrong does not come back as a huge reason. The SDK then
reports its **effective config**: the policy in force, as the only file, `logfire-sdk-policy.json`
(`application/json`).

**The client keeps the config applied.** With a state directory the integrations save nothing
themselves. The client keeps the config that the application answered APPLIED in its own saved
state, and tells the server APPLIED only once that save has landed. Until then, and while a save
fails (the client tries again), the server hears APPLYING. With no state directory there is
nowhere to keep it: a policy that changes a next-start control is refused, and one that changes
only live controls is APPLIED. The integration never rewrites the answer: it hands the flow the
remote config and sends the status the flow gives back.

- The effective config lists every control, those the policy in force left out as `local`.
  Before any policy is applied, every control is local. It is written compact, its keys sorted.
- Live controls show as applied: a level by its lowercase name (`warn` for `WARNING`), the rules,
  the scopes and the redaction patterns in their canonical form, and the values of `app_settings`
  as strings (`"25"` for `25`).
- A next-start control shows local until the restart, because the SDK started with its own value.
  A server that compares the effective config with what it sent sees the change pending, which it
  is. The decision also lists the settings pending a restart (`pending_restart`, for example
  `deactivate_instrumentations`).
- A refused policy changes nothing, so the effective config still reports the policy in force
  before it. A restart reports the policy in force again.
- The layer builds the effective config (the flow gives the whole of it with every change), so
  every language reports it the same way. The reference server's log shows an effective
  config's JSON files on its `a2s` line, as `effective`.

## The description

`Describe` builds the agent description. `service.name`, `service.namespace` and
`service.instance.id` identify the instance, and the rest of the resource describes it. Beside
them, the layer adds two attributes for the server:

- `logfire.opamp.config_format`: `logfire-sdk-policy/v1`, the policy that the SDK reads.
- `logfire.opamp.supported_controls`: the controls that this host carries out, in the order
  `head_sampling`, `signal_exports.traces`, `signal_exports.metrics`, `signal_exports.logs`,
  `sampling_rules`, `trace_rate_limit`, `log_levels`, `log_rate_limit`, `attribute_redaction`,
  `debug`, `app_settings`, `instrumentations`, `scrubbing`. A control is listed when the
  host declares the settings behind it ([the host](#the-host)). `debug` is listed only with a
  clock (`now_unix_ms` not 0). With no host, the list is empty.

Thus every integration passes the same `host` and `now_unix_ms` to `Describe` as to the flow's
start.

## The layer

The layer is the rules as pure functions, compiled to `opamp_sdk_settings.wasm`, which each
language's client ships. It comes with the core, pinned by hash
([how-it-works.md](how-it-works.md#where-the-core-comes-from)), and `scripts/gen_sdk_settings.py`
generates each language's types for it. It is separate from the client core: its own module and
its own interface, `proto/opamp/sdk/v1/settings.proto`. The module keeps nothing between calls.
The integration keeps the agent's state it is handed and passes it back.

| Call | When | Returns |
|---|---|---|
| `AgentStart{host, state_dir, core_state, endpoint, instance_uid, hints, now_unix_ms, now_mono_ms}` | at start, before the SDK is built, with the client's saved state ([at start](#at-start)) | an `AgentOutput`: the agent's state, the effects to carry out (the settings to start with first) and when to wake it ([the agent flow](#the-agent-flow)) |
| `AgentStep{agent, now_unix_ms, now_mono_ms, input}` | for each thing that happens: a remote config, a connection or a failed one, the timer, new exporters, a narrower host | an `AgentOutput` |
| `Describe{resource, host, now_unix_ms}` | at start | the agent description, with the policy formats and the supported controls ([the description](#the-description)) |
| `Components{inventory, host, now_unix_ms}` | once the SDK is built, and when what it lists changes | the instance's inventory as its available components, for `set_available_components`: the integration's and SDK's versions, its instrumentations and exporters, and the settings the host carries out ([available-components.md](available-components.md#what-the-sdk-integrations-report)) |
| `Evaluate{settings, host, input}` | in tests only | what the layer's reference matchers make of one input under a policy: how a span is sampled, whether a log record passes the logs export and the levels, what redaction leaves of a list of attributes, what a rate cap lets through. Each language's tests hold its own matchers to it |

### The agent flow

What an integration does around the rules is the same in every language, so it is the layer's
too: start from the config the client kept applied, answer each remote config, put the settings in
force, end debug mode on time, send export health, and say whether the server can be reached. The
integration is a loop. It tells the flow what happened and carries out what the flow says.

- **State.** Each `AgentOutput` carries `agent`, the flow's state as bytes. The integration keeps
  it and passes it back with the next step, and never reads it.
- **Clocks.** Each call takes the wall clock (`now_unix_ms`, which only debug mode's time reads)
  and a monotonic one (`now_mono_ms`, milliseconds from any start the integration picks, which
  times the wakes). The flow reads no clock of its own.
- **Inputs.** `remote_config`; `connected` and `connect_failed` (the endpoint in use, and the HTTP
  status or 0); `instance_uid_changed` (one the server assigned), `uid_not_saved` (the
  integration couldn't keep it) and `state_from_newer_version` (the client's saved state came
  from a newer client); `stopped` (on its own, with why); `built` (the SDK is built: its
  `service.name` and exporters) and `exporters` (they changed); `host` (it narrowed,
  [the host](#the-host)); `timer`, at the wake asked for. `exporters` and `timer` carry what the
  gates counted since start.
- **Effects,** carried out in order: `apply` (put these settings in force), `effective` (give the
  client this effective config), `status` (send this `RemoteConfigStatus`), `health` (send this
  agent health, with the `exporters` entry), `save_uid` (keep this instance UID for the next
  start; if that fails, step `uid_not_saved`), and `log` (say this line, at its level).
- **Wake.** `wake_at_mono_ms` is when to step `timer` (0: only on an input). Each output replaces
  the last one's wake, so the integration arms one timer.

The lines it says are the same in every language: the first connection, a failure with the
setting to check (`hints`, from the core's Setup), a connection back, a new instance UID that
couldn't be kept.

### At start

The settings to start with are those of the last config applied. The client keeps that config in
its saved state (the state file in the state directory, beside the last config received, which it
hands over again at start). It saves the config when the application answers APPLIED, and
reports APPLIED only once that save has landed. `AgentStart` takes the state as the client saved it
(`core_state`) and reads the policy of the config from it. Thus a config that the application
refused, received after the one applied, is never what a restart starts from.

With no config applied in that state (none saved, or one that does not read), the SDK starts with
its own settings, every control local.

A saved policy that overrides a control the host does not declare now (the application changed
its exporters, say) is not used. The flow warns (`saved SDK settings ignored: …`) and the SDK
starts with its own settings, every control local, until the server sends a policy that it can
carry out.

The config applied survives an endpoint change. The client drops the config received from the
old server (the new server's config wins), but it keeps the applied one, as it keeps packages
installed. Its settings are in force on the agent, and stay so until the new server's config is
applied. A new server then sees in the effective config what the agent actually runs.

### The host

An integration **declares** every setting that it carries out, with a `Host` given to the flow's
start (and a `host` step when it narrows) and to `Describe`, from what it built. A policy that overrides a control whose
setting is not declared is refused, so a host never answers APPLIED for an override that it does
not carry out. Beside the host, each call takes `now_unix_ms`, the instance's clock in
milliseconds since the Unix epoch, which only debug mode reads. The layer reads no clock of its
own.

- `live`: the settings that it carries out at once. A next-start setting listed here is then
  live in every way: put in force by the decision, reported in force at once, never pending, and
  it needs no state directory. The Go and Node.js integrations list
  `deactivate_instrumentations` and `deactivate_all_instrumentations` here.
- `next_start`: the next-start settings that it carries out at the next start
  (`deactivate_instrumentations`, `deactivate_all_instrumentations`). A live setting cannot be
  listed here.
- A setting in neither is not carried out. A policy that overrides its control is refused with
  `<control>: this SDK cannot apply the requested override`, or
  `<control>: <field>: this SDK cannot apply the requested override` where the control has
  several fields, and nothing changes. Each integration declares a signal's sending and what its
  gate holds always (an exporter it can't gate is reported ungoverned, not undeclared), and the
  sampler's settings only where its sampler is in force
  ([where each setting applies](#where-each-setting-applies), footnotes ¹ and ⁴).
  `send_logs_level_by_scope` and `debug_send_logs_level` count as not declared without
  `send_logs_level`, and `debug_sampling_rate` without `sampling_rate`. Debug mode also needs the
  clock. `custom`, the setting behind `app_settings` ([app-settings.md](app-settings.md)), is
  declared where the application can read it.
- A host can narrow while the SDK runs, when another sampler takes the place of the SDK's
  (another agent extension's, in Java). Telemetry sent past the gates doesn't narrow it: that is
  reported ungoverned. From then on the integration passes the narrower host. A
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
([sdks.md](sdks.md#core-mode-and-platform-mode)) rather than the clients' default, core mode.

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
  `debug.json` (debug mode over time: a start, remote configs and the timer in turn, and the exact
  effective config through them), `saving.json` (the answer from what became of the save: saved,
  failed, nowhere to save; the integrations, whose saves are the client's, run its saved and
  nowhere cases), `start.json` (the policy the flow starts from: the config applied in the
  client's saved state, and whether it warns),
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
  `sdk.test.ts`, `go/opamp/sdksettings`, `SdkSettingsTests`),
  and its integration end to end against an OpAMP server, with the same checks for every
  integration (`examples/integrations.py`; [javaagent.md](javaagent.md#how-it-is-checked)). Its
  `check_signals` sends each control that matches telemetry to each integration in turn, and
  checks what arrives at the sink: a debug window that ends on the agent's timer, a sampling rule,
  the application's loggers off by scope, a masked and a dropped URL, both rate caps, and export
  health. It also checks that the effective config that the agent reports is the policy in force,
  the first four controls alone and then every control ([examples/README.md](../examples/README.md)). Its `check_truth`
  runs the integration under the setups that add telemetry paths of their own (a processor added
  once the SDK is built, another extension's exporter, a global provider of the application's
  own), and offers one policy per control. A control counts as set aside when the policy is
  FAILED as an override the SDK can't carry out, or APPLIED with that override reported `local`
  in the effective config. Every other override must show in the telemetry, and only a sampler of
  another's may set controls aside. What the setup sends past the gates goes to a second sink: it
  must arrive there and be reported ungoverned in the inventory and export health.
