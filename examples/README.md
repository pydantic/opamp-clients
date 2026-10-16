# SDK application examples

The OpenTelemetry SDK integrations run against a reference OpAMP server that plays each check's
scenario, with a telemetry sink that receives the exported spans, metrics and logs. Where the
server's sources are (a Cargo.toml at the root) `examples/integrations.py` builds it; elsewhere
`OPAMP_EXAMPLES_SERVER` names its binary, and without one the checks stop and say so. The Go
conformance server is not part of these integration examples.

Run one integration:

```sh
python3 examples/python-otel/try.py   # or node-otel, go-otel, dotnet-otel, javaagent
```

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
inventory must leave ours out); it lists the instrumentations the run switches off, a traces exporter the `send_*`
switch reaches, and when `sampling_rate`, `send_traces` and `deactivate_instrumentations` apply.
To see an application's inventory yourself, run it against the reference server and read the
`components` of the server log's `a2s` lines: after `just sdk-integration python`, in
`target/integrations/python-otel/run1.jsonl` (each language's directory is its example's name).

Each integration then runs `check_signals`: two more runs with metrics and logs exported too
(every application writes a log line per request), which prove every remote setting end to end:

| Setting | What the run checks |
|---|---|
| `send_metrics`, `send_logs` | no metrics or log records arrive once `false`; they come back once left out; spans go on |
| `send_logs_level` | at `warn`, the application's INFO log lines stop arriving (about no records per request, against about one before); left out again, they come back |
| `sampling_rate` | at 0.25, some of the application's requests are traced but not all; at 0, none (once a debug window is over) |
| `debug_until` | with `sampling_rate` 0 and a time 10 s ahead (the server fills it in as it sends the offer), the requests are traced until then and not after, though the server sends nothing more |
| `sampling_rules` | at `sampling_rate` 1, a rule of rate 0 for client spans: none of the application's requests is traced |
| `send_logs_level_by_scope` | the application's loggers (`app*`, and Rust's target) `off`: about no log records per request; left out again, they come back |
| `mask_attributes` | on `url.full` and `http.url`: spans come again, each URL `[REDACTED]` in the sink, none shown |
| `drop_attributes` | on the same keys, in another case: spans arrive, none with a URL |
| `sampling_traces_per_second`, `send_logs_per_second` | at 1: no more than about a span and a log record a second, of some five requests a second |
| a key that is no setting (EDOT's `logging_level`) | sent beside `sampling_rate` 0.25: APPLIED, and the effective config the agent reports names it as ignored in its `elastic.set_aside` entry, beside the settings in force; left out again, no longer named |
| export health (no setting) | the agent's health carries `exporters`, a child per signal, the application's own `healthy` kept; it counts trace exports that succeeded and log records dropped on purpose |
| `opamp_polling_interval` | at the next start the agent checks in at the offered interval, not its environment's |
| `deactivate_all_instrumentations` | at the next start, no spans while the application makes requests |

Last, `check_setup` runs the application twice against a server that requires a bearer token,
to prove the local connection settings:

| Setting | What the run checks |
|---|---|
| `OTEL_OPAMP_HEADERS` | with the wrong token every request is refused, the application is traced all the same, and the agent's warning names `OTEL_OPAMP_HEADERS`; with the right one the agent is accepted |
| `OTEL_OPAMP_HEARTBEAT_INTERVAL` | the accepted agent checks in as often as it says |

The Python, Go, Node.js and Rust integrations also have an example application that gives the
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

A new check goes into one of its integration's `checks:` lists in the CI workflow that runs
them. Each run prints how long each check took, for keeping those jobs even.

Every integration lets the application read its own settings ([docs/app-settings.md](../docs/app-settings.md)),
checked by `check_app_settings` (the check `app`): its example application reads the flag
`example.feature`, requests `/feature` while it is `on` and `/changed` once per change it is told of.

| Setting | What the run checks |
|---|---|
| `custom` | none of `/feature` before the first config (the default); `/feature` once the flag is `on`, still after a bad map is FAILED naming `custom.example.feature`, none once `custom` is left out, again from a map in a string; `/changed` three times (on, off, on), not for the FAILED config or the same map sent again; after a restart with no offer, `/feature` from the start |

Logs and scenario files are saved under `target/integrations/<integration>/`.
The OpAMP and telemetry listeners use independent ports. Each run stops its server and app.
The examples currently use the `elastic` settings format. The Logfire policy adapter is
separate work.

The programs in `go/`, `node/`, `python/`, `java/` and `dotnet/` also show how to
connect an OpAMP client to an SDK manually. `try.py` exercises those lower-level examples
against the Go conformance server.

[settings/](settings/README.md) has ready-made remote settings documents, one per common use.
