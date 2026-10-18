# Remote settings, by example

Each file here is a whole Logfire SDK policy. Send it as the remote config's only file,
`logfire-sdk-policy.json` (`application/json`), to any of the six SDK integrations.
[docs/sdk-settings.md](../../docs/sdk-settings.md#the-policy) describes the policy and what each
control does.
[Where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies) has the limits
per language. `examples/test_settings_cookbook.py` checks three things. Every file here is a whole
policy: the first four controls alone where only those are overridden, else every control. This README shows each file as it
is. Between them, the files override every control.

A policy is the whole of what the server wants. Each control is in it as `local` or as
`override`, or is left out, which is `local`. A `local` control is the SDK's own value, as
configured locally. Thus each policy
states everything that should differ from the SDK's own, not only what changes. A field that the
policy version does not name is refused, and so is an override that the SDK cannot carry out. A
refused policy changes nothing.

Every policy sends `head_sampling` and `signal_exports`. The other controls may be left out. A
server sends an override only to an SDK that lists its control in its
`logfire.opamp.supported_controls` attribute.

## Cut trace volume: `sample-a-quarter.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "override", "ratio": 0.25},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  }
}
```

A quarter of new traces are sampled, at once. A request that arrives already sampled (a
`traceparent` from a sampled caller) stays sampled. Thus a service behind a traced caller is cut
far less than this.

## Trace everything while you debug: `sample-everything.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "override", "ratio": 1},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  }
}
```

Every new trace is sampled, at once, whatever the SDK's own sampler says. Send
`back-to-defaults.json` when you are done.

## Keep traces, stop metrics and logs: `traces-only.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "local"},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "override", "enabled": false},
    "logs": {"mode": "override", "enabled": false}
  }
}
```

Metrics and log records stop being exported, at once. A batch already on its way still arrives.
Traces carry on.

## Quiet the application's logs to warnings and errors: `quiet-logs.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "local"},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "local"},
  "trace_rate_limit": {"mode": "local"},
  "log_levels": {"mode": "override", "level": "warn"},
  "log_rate_limit": {"mode": "local"},
  "attribute_redaction": {"mode": "local"},
  "debug": {"mode": "local"},
  "app_settings": {"mode": "local"},
  "instrumentations": {"mode": "local"}
}
```

Only the application's log records at `warn` or above (SeverityNumber 13 and up) are exported, at
once. The records below are still made, but they are not sent. A record with no severity is
exported whatever the level. The levels are `trace`, `debug`, `info`, `warn`, `error` and
`fatal`. The level filters what is exported. It does not change the SDK's own log level. It
reaches the log exporters that the integration builds, as `signal_exports.logs` does
([where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)).

## Stop a service exporting anything: `stop-exporting.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "local"},
  "signal_exports": {
    "traces": {"mode": "override", "enabled": false},
    "metrics": {"mode": "override", "enabled": false},
    "logs": {"mode": "override", "enabled": false}
  }
}
```

Nothing more is exported, at once. The application runs as before, and the agent stays
connected. Thus the next policy can turn it all back on.

## Switch off noisy instrumentations: `drop-noisy-instrumentations.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "local"},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "local"},
  "trace_rate_limit": {"mode": "local"},
  "log_levels": {"mode": "local"},
  "log_rate_limit": {"mode": "local"},
  "attribute_redaction": {"mode": "local"},
  "debug": {"mode": "local"},
  "app_settings": {"mode": "local"},
  "instrumentations": {"mode": "override", "disabled": ["jdbc", "kafka-clients"]}
}
```

The named instrumentations are off from the next start. In Go, Node.js and Rust, they are off at
once. Names are each language's own:

| Language | Names | The example app's HTTP client |
|---|---|---|
| Python | entry points: `flask`, `requests`, `urllib` | `urllib` |
| Node.js | upstream's short ones: `http`, `express`, `pg` | `http`, `net`, `dns` |
| Go | the instrumentation's: `otelhttp` | `otelhttp` |
| Rust | the `tracing` target: `reqwest_tracing` | `reqwest_tracing` |
| Java | the agent's: `jdbc`, `spring-webmvc`, `java-http-client` | `java-http-client` |
| .NET | upstream's, lowercase: `httpclient`, `aspnetcore`, `sqlclient` | `httpclient` |

The same policy goes to every language. A name that a language does not have switches nothing
off there. A next-start control needs a state directory to be kept in. Without one, the SDK
answers the policy FAILED and nothing changes.

## Switch every instrumentation off: `no-instrumentation.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "local"},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "local"},
  "trace_rate_limit": {"mode": "local"},
  "log_levels": {"mode": "local"},
  "log_rate_limit": {"mode": "local"},
  "attribute_redaction": {"mode": "local"},
  "debug": {"mode": "local"},
  "app_settings": {"mode": "local"},
  "instrumentations": {"mode": "override", "all_disabled": true}
}
```

No instrumentation runs from the next start. In Go, Node.js and Rust, none runs from now.

## Skip health checks, keep every checkout: `skip-health-checks.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "override", "ratio": 0.25},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "override", "rules": [{"rate": 0, "kind": "server", "path": "/health*"}, {"rate": 1, "name": "POST /checkout*"}]},
  "trace_rate_limit": {"mode": "local"},
  "log_levels": {"mode": "local"},
  "log_rate_limit": {"mode": "local"},
  "attribute_redaction": {"mode": "local"},
  "debug": {"mode": "local"},
  "app_settings": {"mode": "local"},
  "instrumentations": {"mode": "local"}
}
```

For a new trace, the first rule that its root span matches sets the rate, at once. Thus there are
no health-check traces, every checkout is sampled, and a quarter of the rest are sampled
(`head_sampling`). The `name` and `path` of a rule are globs, where `*` is any run of characters.
The `kind` is `server`, `client`, `internal`, `producer` or `consumer`. A span with a sampled
parent is still sampled, rules or not. A rule matches what a span has when it starts, and that
differs by instrumentation. For example, an HTTP client span starts without its URL in Go and on
.NET 8 ([where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)).

## Quiet the noisy loggers, hear more from one: `quiet-noisy-loggers.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "local"},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "local"},
  "trace_rate_limit": {"mode": "local"},
  "log_levels": {"mode": "override", "level": "warn", "scopes": {"com.acme.payments*": "debug", "urllib3": "error", "noisy.lib": "off"}},
  "log_rate_limit": {"mode": "local"},
  "attribute_redaction": {"mode": "local"},
  "debug": {"mode": "local"},
  "app_settings": {"mode": "local"},
  "instrumentations": {"mode": "local"}
}
```

Log records are exported from `warn` up, at once. But they are exported from `debug` up for the
scopes (loggers) whose name starts with `com.acme.payments`, from `error` up for `urllib3`, and
not at all for `noisy.lib`. An exact name wins over a prefix, and a longer prefix wins over a
shorter one. A scope is the logger of each language. It is the logger name in Python and Java,
the `ILogger` category in .NET, the `tracing` target in Rust, the name of the `otelslog` logger in
Go, and the logs API logger (or the package of the bridge) in Node.js. Like the level, the scopes
reach the log exporters that the integration builds. An SDK that cannot carry them out refuses
the policy ([where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)).

## Keep secrets out of the backend: `redact-secrets.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "local"},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "local"},
  "trace_rate_limit": {"mode": "local"},
  "log_levels": {"mode": "local"},
  "log_rate_limit": {"mode": "local"},
  "attribute_redaction": {"mode": "override", "mask": ["user.email", "enduser.id"], "drop": ["*password*", "*token*", "http.request.header.authorization"]},
  "debug": {"mode": "local"},
  "app_settings": {"mode": "local"},
  "instrumentations": {"mode": "local"}
}
```

At export, the SDK removes the span and log attributes whose keys match `drop`. It replaces the
value with `[REDACTED]` for the keys that match `mask`. The globs ignore case. This applies at
once. Resource attributes, span names, log bodies and metrics stay as they are. Only an
integration whose own exporters see every span and log record carries this out. Elsewhere the SDK
refuses the policy
([where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)).

## Cap the volume: `cap-volume.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "override", "ratio": 1},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "local"},
  "trace_rate_limit": {"mode": "override", "traces_per_second": 50},
  "log_levels": {"mode": "local"},
  "log_rate_limit": {"mode": "override", "records_per_second": 200},
  "attribute_redaction": {"mode": "local"},
  "debug": {"mode": "local"},
  "app_settings": {"mode": "local"},
  "instrumentations": {"mode": "local"}
}
```

Every trace is sampled, but no more than 50 new traces and 200 log records a second leave each
instance, at once. This is a safety valve for a traffic spike or a logging loop. The log cap is at
the log exporters that the integration builds, as the log level is. Each process has its own
buckets.

## Debug for half an hour: `debug-for-half-an-hour.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "override", "ratio": 0.05},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "local"},
  "trace_rate_limit": {"mode": "local"},
  "log_levels": {"mode": "override", "level": "warn"},
  "log_rate_limit": {"mode": "local"},
  "attribute_redaction": {"mode": "local"},
  "debug": {"mode": "override", "until": "2026-10-14T12:30:00Z", "sampling_ratio": 1, "log_level": "debug"},
  "app_settings": {"mode": "local"},
  "instrumentations": {"mode": "local"}
}
```

Until 12:30 UTC, every trace is sampled and every log record from `debug` up is exported. Rules
and levels per scope do not apply for that time. Then each instance goes back to 5 % and `warn`
on its own, even if the server cannot reach it. Send it with your own time, as an RFC 3339 time
at most 24 h ahead. Sending it again (as a server does on every reconnect) does not extend it,
but a later time does. Every integration ends it on a timer of its own, server or no server.

## Flip a feature flag of the application's own: `flip-a-feature-flag.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "local"},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  },
  "sampling_rules": {"mode": "local"},
  "trace_rate_limit": {"mode": "local"},
  "log_levels": {"mode": "local"},
  "log_rate_limit": {"mode": "local"},
  "attribute_redaction": {"mode": "local"},
  "debug": {"mode": "local"},
  "app_settings": {"mode": "override", "values": {"checkout.new_flow": "on", "example.log_level": "debug"}},
  "instrumentations": {"mode": "local"}
}
```

These values are not settings of the SDK but of the application. The SDK hands `app_settings` to
the application. The application reads a key with a default, for example
`pydantic_opamp_otel.app_settings.get("checkout.new_flow", "off")` in Python, and it can be told when the
values change. The values are in force at once. When the control is `local` again, each key is
back to the default of the application ([docs/app-settings.md](../../docs/app-settings.md)). Only
an integration that lets the application read them takes them. Elsewhere the SDK refuses the
policy.

## Back to the SDK's own settings: `back-to-defaults.json`

```json
{
  "schema": "logfire-sdk-policy/v1",
  "head_sampling": {"mode": "local"},
  "signal_exports": {
    "traces": {"mode": "local"},
    "metrics": {"mode": "local"},
    "logs": {"mode": "local"}
  }
}
```

Every control is `local`, those left out included, so every setting is the SDK's own again. Live
settings change at once, and next-start settings change at the next start. Before any policy is
applied, the effective config reports this policy with every control listed.
