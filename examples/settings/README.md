# Remote settings, by example

Each file here is a whole settings document, ready to send as the remote config's `elastic`
entry (`application/json`) to any of the six SDK integrations. [docs/sdk-settings.md](../../docs/sdk-settings.md)
defines every setting; [where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)
has the limits per language. The settings layer's own tests check that every file here is
applied as written, and that between them they use every setting, and that the one with keys that
are no setting here shows what an instance reports of them.

A document is the whole of what the server wants: a setting left out goes back to the SDK's own
value, as configured locally. So each document says everything that should differ from the
SDK's own, not only what changes.

## Cut trace volume: `sample-a-quarter.json`

```json
{"sampling_rate": "0.25"}
```

A quarter of new traces are sampled, at once. A request that arrives already sampled (a
`traceparent` from a sampled caller) stays sampled, so a service behind a traced caller is cut
far less than this.

## Trace everything while you debug: `sample-everything.json`

```json
{"sampling_rate": "1"}
```

Every new trace is sampled, at once, whatever the SDK's own sampler says. Send
`back-to-defaults.json` when you're done.

## Keep traces, stop metrics and logs: `traces-only.json`

```json
{"send_metrics": "false", "send_logs": "false"}
```

Metrics and log records stop being exported, at once (a batch already on its way still arrives).
Traces carry on.

## Quiet the application's logs to warnings and errors: `quiet-logs.json`

```json
{"send_logs_level": "warn"}
```

Only the application's log records at `warn` or above (SeverityNumber 13 and up) are exported, at
once: the records below are still made, just not sent, and a record with no severity is exported
whatever the level. The levels are `trace`, `debug`, `info`, `warn`, `error` and `fatal`. It
filters what is exported; it doesn't change the SDK's own log level. It reaches the log exporters
the integration builds, as `send_logs` does ([where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)).

## Stop a service exporting anything: `stop-exporting.json`

```json
{"send_traces": "false", "send_metrics": "false", "send_logs": "false"}
```

Nothing more is exported, at once. The application runs as before, and the agent stays
connected, so the next document can turn it all back on.

## Switch off noisy instrumentations: `drop-noisy-instrumentations.json`

```json
{"deactivate_instrumentations": "jdbc,kafka-clients"}
```

The named instrumentations are off from the next start (in Go, Node.js and Rust, at once). Names
are each language's own:

| Language | Names | The example app's HTTP client |
|---|---|---|
| Python | entry points: `flask`, `requests`, `urllib` | `urllib` |
| Node.js | upstream's short ones: `http`, `express`, `pg` | `http,net,dns` |
| Go | the instrumentation's: `otelhttp` | `otelhttp` |
| Rust | the `tracing` target: `reqwest_tracing` | `reqwest_tracing` |
| Java | the agent's: `jdbc`, `spring-webmvc`, `java-http-client` | `java-http-client` |
| .NET | upstream's, lowercase: `httpclient`, `aspnetcore`, `sqlclient` | `httpclient` |

The same document goes to every language: a name a language doesn't have switches nothing off
there. A next-start setting needs a state directory to be kept in; without one, the document is
answered FAILED and nothing changes.

## Switch every instrumentation off: `no-instrumentation.json`

```json
{"deactivate_all_instrumentations": "true"}
```

No instrumentation runs from the next start (in Go, Node.js and Rust, at once).

## Check in less often: `poll-less-often.json`

```json
{"opamp_polling_interval": "5m"}
```

The agent checks in every 5 minutes from the next start, whatever `OTEL_OPAMP_HEARTBEAT_INTERVAL`
says. From 1 s to 24 h.

## Skip health checks, keep every checkout: `skip-health-checks.json`

```json
{"sampling_rate": "0.25", "sampling_rules": [{"rate": 0, "kind": "server", "path": "/health*"}, {"rate": 1, "name": "POST /checkout*"}]}
```

For a new trace, the first rule its root span matches sets the rate, at once: no health-check
traces, every checkout, and a quarter of the rest (`sampling_rate`). A rule's `name` and `path`
are globs (`*` is any run of characters); `kind` is `server`, `client`, `internal`, `producer` or
`consumer`. A span with a sampled parent is still sampled, rules or not. A rule matches what a
span has when it starts, which differs by instrumentation: an HTTP client span starts without its
URL in Go and on .NET 8, for one ([where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)).

## Quiet the noisy loggers, hear more from one: `quiet-noisy-loggers.json`

```json
{"send_logs_level": "warn", "send_logs_level_by_scope": "com.acme.payments*=debug,urllib3=error,noisy.lib=off"}
```

Log records are exported from `warn` up, at once, but from `debug` up for the scopes (loggers)
whose name starts with `com.acme.payments`, from `error` up for `urllib3`, and none for
`noisy.lib`. An exact name wins over a prefix, and a longer prefix over a shorter one. A scope
is each language's logger: Python's and Java's logger name, .NET's `ILogger` category, Rust's
`tracing` target, the `otelslog` logger's name in Go, the logs API logger (or the bridge's
package) in Node.js. Like `send_logs_level`, it reaches the log exporters the integration builds;
where it can't be carried out, it is ignored rather than answered as done ([where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)).

## Keep secrets out of the backend: `redact-secrets.json`

```json
{"drop_attributes": "*password*,*token*,http.request.header.authorization", "mask_attributes": "user.email,enduser.id"}
```

At export, the span and log attributes whose keys match (globs, ignoring case) are removed, or
have their value replaced by `[REDACTED]`, at once. Resource attributes, span names, log
bodies and metrics are left alone. Only where every span and log record goes through the
integration's own exporters is it carried out; elsewhere it is ignored rather than half done
([where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)).

## Cap the volume: `cap-volume.json`

```json
{"sampling_rate": "1", "sampling_traces_per_second": "50", "send_logs_per_second": "200"}
```

Every trace is sampled, but no more than 50 new traces and 200 log records a second leave each
instance, at once: a safety valve for a traffic spike or a logging loop. The log cap is at the
log exporters the integration builds, as `send_logs_level` is; each process has its own buckets.

## Debug for half an hour: `debug-for-half-an-hour.json`

```json
{"sampling_rate": "0.05", "send_logs_level": "warn", "debug_until": "2026-10-14T12:30:00Z", "debug_sampling_rate": "1", "debug_send_logs_level": "debug"}
```

Until 12:30 UTC every trace is sampled and every log record from `debug` up is exported, rules
and levels per scope set aside; then each instance goes back to 5 % and `warn` on its own, even
if the server can't reach it. Send it with your own time: at most 24 h ahead, as an RFC 3339
time. Sending it again (as a server does on every reconnect) doesn't extend it; a later time does.
Every integration ends it on a timer of its own, server or no server.

## Flip a feature flag of the application's own: `flip-a-feature-flag.json`

```json
{"custom": {"checkout.new_flow": "on", "example.log_level": "debug"}}
```

Not a setting of the SDK's but of the application's: the SDK hands `custom` to the application,
which reads a key with a default (`opamp_otel.app_settings.get("checkout.new_flow", "off")` in
Python) and can be told when the map changes. The values are in force at once, and left out
again each key is back to the application's default
([docs/app-settings.md](../../docs/app-settings.md)). Only an integration that lets the
application read them takes them; elsewhere `custom` is ignored.

## One document for EDOT and these SDKs alike, and what each set aside: `edot-and-upstream-alike.json`

```json
{"sampling_rate": "0.25", "logging_level": "warn", "infer_spans": "false"}
```

`logging_level` and `infer_spans` are EDOT's settings, not these SDKs': they are ignored, not
refused, and the document is answered APPLIED with a quarter of new traces sampled. So the server
can see what didn't take, each instance names what it set aside in its effective config, as an
entry of its own beside the `elastic` entry of the settings in force. Here its
`elastic.set_aside` entry is:

```json
{"ignored":["infer_spans","logging_level"],"unsupported":[]}
```

`ignored` names keys that are no setting here (another SDK's, or misspelt); `unsupported` names
settings this integration doesn't carry out where it runs, such as `send_metrics` beside an
exporter it doesn't gate ([where each setting applies](../../docs/sdk-settings.md#where-each-setting-applies)).
With nothing set aside, the entry is left out.

## Back to the SDK's own settings: `back-to-defaults.json`

```json
{}
```

Every setting is the SDK's own again: live ones at once, next-start ones at the next start.
