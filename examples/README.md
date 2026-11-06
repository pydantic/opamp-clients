# SDK application examples

New to Fleet? Start with [fleet-quickstart](fleet-quickstart/README.md): a small app in Python,
Node.js and Go that you connect to Logfire Fleet and change from a policy while it runs. The rest
of this page covers the integration checks.

The OpenTelemetry SDK integrations run against a reference OpAMP server that plays each check's
scenario, with a telemetry sink that receives the exported spans, metrics and logs: the
conformance suite's Go server ([conformance/server](../conformance/server)) as the SDK fixture
(`fixture.go`), which the checks build with `go build` the first time they need it.
`OPAMP_EXAMPLES_SERVER` names another binary to use in its place, one that takes the same flags,
scenarios and log.

Run one integration:

```sh
python3 examples/python-otel/try.py   # or node-otel, go-otel, dotnet-otel, javaagent
```

The Java agent's, the .NET plugin's and the Python package's also take `--edot`: the same checks
with Elastic's distribution of the SDK in place ([docs/guides/elastic.md](../docs/guides/elastic.md)).

Node.js runs both CommonJS and ESM applications. Java and .NET require Docker. Go examples also
require Go, Node.js examples require Node.js, and Python examples use uv.

Each integration proves that traces stop and start again while it runs, invalid-config
rejection, effective configuration reporting, identity persistence, instrumentation changes
after restart, and clean shutdown. It checks actual exported spans and application requests.
The OpAMP client's own requests must not produce spans.

The first run also checks the **inventory** each integration reports as its available components
([available-components.md](../docs/available-components.md#what-the-sdk-integrations-report)):
it is sent in full when the server asks; it names our package and the OpenTelemetry SDK with their
versions (Go's example builds our package from this checkout, which has no version, so there the
inventory must leave ours out); it lists the instrumentations the run switches off, a traces exporter the traces export control
reaches, and when the settings behind `head_sampling`, `signal_exports.traces` and
`instrumentations` apply (`sampling_rate`, `send_traces` and `deactivate_instrumentations`).
To see an application's inventory yourself, run it against the reference server and read the
`components` of the server log's `a2s` lines: after `python3 examples/python-otel/try.py first`, in
`target/integrations/python-otel/app-py/run1.jsonl` (each language's directory is its example's
name; Python's and Node.js's have one more, for the application).

Each offer is a remote config whose only file is a Logfire SDK policy, `logfire-sdk-policy.json`
([docs/sdk-settings.md](../docs/sdk-settings.md)): the first four controls alone where only
`head_sampling` and `signal_exports` are overridden, else every control. Every control a step does
not name is local, the SDK's own value.

Each integration then runs `check_signals`: two more runs with metrics and logs exported too
(every application writes a log line per request), which prove every control of the policy end
to end:

| Control | What the run checks |
|---|---|
| `signal_exports.metrics`, `signal_exports.logs` | no metrics or log records arrive once `enabled` is `false`; they come back once local; spans go on |
| `log_levels` `level` | at `warn`, the application's INFO log lines stop arriving (about no records per request, against about one before); local again, they come back |
| `head_sampling` | at 0.25, some of the application's requests are traced but not all; at 0, none (once a debug window is over) |
| `debug` | with `head_sampling` 0 and `until` 10 s ahead (the server fills it in as it sends the offer), the requests are traced until then and not after, though the server sends nothing more |
| `sampling_rules` | at `head_sampling` 1, a rule of rate 0 for client spans: none of the application's requests is traced |
| `log_levels` `scopes` | the application's loggers (`app*`, and Rust's target) `off`: about no log records per request; local again, they come back |
| `attribute_redaction` `mask` | on `url.full` and `http.url`: spans come again, each URL `[REDACTED]` in the sink, none shown |
| `attribute_redaction` `drop` | on the same keys, in another case: spans arrive, none with a URL |
| `scrubbing` `defaults` | with the mask: the note the probe carries beside its URL (`example.note`, "the password is p4ss") arrives `[Scrubbed due to 'password']` on its span events, links and log records |
| `scrubbing` `patterns` | with the drop, `*P4SS` alone: the note arrives `[Scrubbed due to '*p4ss']` (the pattern, lowercase, not the defaults); local again, the note and the URL as they are |
| `trace_rate_limit`, `log_rate_limit` | at 1: no more than about a span and a log record a second, of some five requests a second |
| the effective config | after `head_sampling` 0.25, the agent reports that policy, every control listed, as the only file; after the log level is added, that policy |
| export health (no control) | the agent's health carries `exporters`, a child per signal, the application's own `healthy` kept; it counts trace exports that succeeded and log records dropped on purpose |
| `instrumentations` `all_disabled` | at the next start, no spans while the application makes requests |

An integration that cannot carry out a control refuses the whole policy: the agent answers
FAILED, names the control, and changes nothing. For example, an application that instruments
itself in code answers the switch of an instrumentation with `instrumentations: disabled: this SDK
cannot apply the requested override`, and its spans go on.

Last, `check_setup` runs the application twice against a server that requires a bearer token,
to prove the local connection settings:

| Setting | What the run checks |
|---|---|
| `OTEL_OPAMP_HEADERS` | with the wrong token every request is refused, the application is traced all the same, and the agent's warning names `OTEL_OPAMP_HEADERS`; with the right one the agent is accepted |
| `OTEL_OPAMP_HEARTBEAT_INTERVAL` | the accepted agent checks in as often as it says |

The Python, Go and Node.js integrations also have an example application that gives the
server in code, and they run `check_setup` against it too, as the check `setup-in-code`. There the
token is given in code, and `OTEL_OPAMP_HEADERS` holds the other token: the right one when the code
gives the wrong one, and the wrong one when the code gives the right one. The code wins both
times. With the wrong token, the agent's warning names the headers given in code, not
`OTEL_OPAMP_HEADERS`. With the right token, the agent is accepted.

Then `check_rotation` (the check `rotation`) runs the application against the same kind of server,
whose operator rotates the agent's key and then moves it to the server's second listener, and
starts it once more with the first endpoint still configured:

| What the operator does | What the run checks |
|---|---|
| rotates the key the agent took | the agent takes the second key: the server confirms it, refusing no request |
| moves the agent to another port, with a third key | the agent takes it there, refusing no request, opens no connection to the first port after it, and is traced throughout |
| (the next start) | the agent goes straight to the second port on the key it was given there, takes no new key, and none of its requests is refused |

Then `check_outage` (the check `outage`) takes the OpAMP server away and starts the application
while it is away. The telemetry goes to a sink that stays up throughout; the OpAMP servers come and
go at one endpoint:

| What happens | What the run checks |
|---|---|
| server A offers a policy that masks the URL | APPLIED; spans arrive with each URL `[REDACTED]` |
| server A stops, and nothing listens for 15 s | the application goes on making requests, and its spans still arrive masked |
| server B, a new server that knows nothing of the agent, comes up at the same endpoint | the agent comes back with the same UID, sends its whole state (its description) before any offer, and reports the masking policy it kept as its effective config |
| server B offers a policy that drops the URL | APPLIED; spans arrive with no URL |
| the next start, with nothing at the endpoint | the application starts and makes its requests, its spans without a URL from the first: it runs on the policy it saved, with no word from a server |
| server C comes up there | the agent reports the same UID, and the dropping policy as its effective config |

The Python integration also runs it on the Logfire SDK (`outage-logfire`: `logfire_app.py`, set up
with `configure_logfire()`, its spans sent where the Logfire SDK sends them) and on an SDK the
application set up itself (`outage-manage`: `logfire.configure()` then `manage()`), and Node.js on
its own NodeSDK then `manage()` (`outage-manage`: `manage.mjs`) and on the Logfire JS SDK
(`outage-logfire`: `logfire/logfire-js.mjs`, `logfire.configure()` then `manage()`, its spans sent
where the Logfire SDK sends them). The .NET integration also runs it on an SDK set up in code with
`WithOpAmp()` (`outage-hosted`: `dotnet-otel/Hosted/`, no automatic instrumentation).

Each check is independent of the others (its own server, state directory and logs), so one can
run alone, by name, or several side by side. A check waits on its scenario's clock (one message a
second), not on the machine, so with `--jobs` they take about as long as the longest of them:

```sh
python3 examples/python-otel/try.py --list                    # its checks: first, signals, setup, ...
python3 examples/python-otel/try.py signals setup             # those two alone
python3 examples/python-otel/try.py --jobs 4 signals setup next  # up to four at once, after one build
```

Under `--jobs`, each check runs in a process of its own and its output is printed whole when it
ends. Two checks of one integration that run at once must not name their runs alike: the runs'
files share the integration's directory (`check_app_settings` names its runs `app7` and `app8`
for that reason).

A new check goes into its integration's `try.py`, in `CHECKS`, under a name of its own. Each run
prints how long each check took.

CI runs every check of each integration on every pull request that is not a draft (the job
"…, end to end"), four at once, besides the checks' own tests (`examples/test_integrations.py`,
`examples/test_settings_cookbook.py`) and each integration's unit tests.

Every integration lets the application read its own settings ([docs/app-settings.md](../docs/app-settings.md)),
checked by `check_app_settings` (the check `app`): its example application reads the flag
`example.feature`, requests `/feature` while it is `on` and `/changed` once per change it is told of.

| Control | What the run checks |
|---|---|
| `app_settings` | none of `/feature` before the first policy (the default); `/feature` once the flag is `on`, still after a policy with a bad value is FAILED naming `app_settings`, none once `app_settings` is local, again once it is `on` again; `/changed` three times (on, off, on), not for the FAILED policy or the same values sent again; after a restart with no offer, `/feature` from the start |

`check_truth` (the checks `truth-<setup>`) runs an integration under a setup that adds telemetry
paths of its own: something the application does to the SDK beside the integration, named in
`EXAMPLE_SETUP`. The server offers one setting a document, each beside `sampling_rate` 1 and left
out again by the next: `send_traces`, `send_metrics`, `send_logs`, `sampling_rate` 0,
`send_logs_level`, `send_logs_level_by_scope`, `sampling_rules`, `mask_attributes`,
`drop_attributes` and both rate caps (and `deactivate_all_instrumentations` where it is live). Each
must either show at the sink, measured as `check_signals` measures it, or be named as set aside in
the effective config the agent reports. An APPLIED setting that is neither fails. The policy
governs every exporter the integration's gates are in front of, so only a sampler of another's
sets settings aside, and they must be the ones its try.py expects. What the setup exports past the
gates goes to a second sink (`EXAMPLE_SECOND_SINK`): it must arrive there, and the inventory and
export health must report it ungoverned, and nothing else. Where a signal's every exporter is past
the gates, its settings need only be APPLIED and in force.

| Integration | Setup | What the application does | Past the gates (second sink) | Set aside |
|---|---|---|---|---|
| Python | `late-processor` | once the SDK is built, adds a span and a log record batch processor (gated, to the first sink) and a span processor of its own kind that exports each span itself | that processor's traces | nothing |
| Go | `global-tracer` | sets a tracer provider of its own as the global one after `Setup` | every span (`app_provider`) | nothing |
| Go | `global-meter` | sets a meter provider of its own as the global one after `Setup` | every metric (`app_provider`) | nothing |
| Go | `global-logger` | sets a logger provider of its own as the global one after `Setup` | every log record (`app_provider`) | nothing |
| Node.js | `own-tracer-provider` | disables the SDK's global tracer provider and registers one of its own, tracing each request with it too | its own spans (`app_provider`); the http instrumentation's stay governed | nothing |
| Node.js | `own-logger-provider` | the same for the logger provider | every log record (`app_provider`) | nothing |
| Node.js | `own-meter-provider` | the same for the meter provider, counting each request with it | its own metrics (`app_provider`) | nothing |
| Java agent | `extension-processor` | a second agent extension (`Setups.java`) adds a span processor, a log record processor and a metric reader | all three | nothing |
| Java agent | `extension-sampler` | a second agent extension sets a sampler of its own in place of the extension's | nothing | the sampler's settings |
| .NET plugin | `plugin-exporters` | a plugin of the application's own, listed after the integration's, adds an OTLP span exporter and a log record processor | both | nothing |
| .NET plugin | `own-sdk` | sets `OTEL_DOTNET_AUTO_SETUP_SDK=false` and builds its own tracer and meter providers | every span and metric (`app_provider`); the logs stay governed | the sampler's settings |

Logs and scenario files are saved under `target/integrations/<integration>/`.
The OpAMP and telemetry listeners use independent ports. Each run stops its server and app.

The programs in `go/`, `node/`, `python/`, `java/` and `dotnet/` also show how to
connect an OpAMP client to an SDK manually. `try.py` exercises those lower-level examples
against the Go conformance server.

[settings/](settings/README.md) has ready-made Logfire SDK policies, one per common use.
