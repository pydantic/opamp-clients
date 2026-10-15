# OpenTelemetry auto-instrumentation for Python, under an OpAMP server

`python/otel` is `pydantic-opamp-otel`, a package for `opentelemetry-instrument`. With it
installed, the SDK that auto-instrumentation sets up joins an OpAMP server's fleet and takes its
settings from the server: sampling, what to send, which instrumentations run
([sdk-settings.md](sdk-settings.md)). The application needs no code, and the SDK and the
instrumentations stay the upstream ones.

It plugs in through upstream's extension points, including the SDK's OpAMP hook
(`_opentelemetry_opamp`, experimental, since `opentelemetry-sdk` 1.43). Checked with
`opentelemetry-sdk` 1.45 and Python 3.13; it needs 3.11 or later.

## Use it

Install it next to the instrumentations the application needs (from the repository, until it is
published):

```bash
pip install ./python ./python/otel opentelemetry-instrumentation-flask
```

Then run the application under `opentelemetry-instrument` as usual, with the server's address:

```bash
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
opentelemetry-instrument python app.py
```

| Variable | |
|---|---|
| `OTEL_OPAMP_ENDPOINT` | The OpAMP server: `http(s)://.../v1/opamp` polls, `ws(s)://...` keeps a connection open. Without it the package does nothing beyond the upstream distro's defaults. |
| `OTEL_OPAMP_HEADERS` | Headers for every request, as `OTEL_EXPORTER_OTLP_HEADERS`: `name=value,...`, values percent-encoded. The enrollment key goes here. |
| `OTEL_OPAMP_STATE_DIR` | Where to keep state across restarts: the instance UID, the settings for the next start, the client's own state (written readable by the application's user only). Without it, the package keeps one of the user's own, one per process of the program running at once ([default-state-dir.md](default-state-dir.md)); `none` turns that off, and then each start is a new instance and settings that take effect at the next start are refused. A directory named here belongs to one process: two processes given the same one are the same instance. |
| `OTEL_OPAMP_HEARTBEAT_INTERVAL` | How often to check in until the server offers an interval (default 30 s): milliseconds, or `30s`, `2m`. A value it can't read is warned about and the default used; it never stops the application. |

It is a distro (`opentelemetry_distro`) and a configurator (`opentelemetry_configurator`), both
named `opamp`. If another distro is installed too (`opentelemetry-distro`, say), pick this one
with `OTEL_PYTHON_DISTRO=opamp` and `OTEL_PYTHON_CONFIGURATOR=opamp`. Like the upstream distro, it
defaults every signal's exporter to OTLP (`OTEL_TRACES_EXPORTER=otlp` and so on, over gRPC unless
`OTEL_EXPORTER_OTLP_PROTOCOL` says otherwise).

If another configurator builds the SDK all the same, the instance still joins the fleet, but the
sampler and the trace and log exporters aren't the package's: `sampling_rate`, `send_traces` and
`send_logs` are ignored rather than answered as done ([the host](sdk-settings.md#the-host)), the
package logs a warning, and `service.instance.id` is whatever that configurator gives.

### Is it working?

The package says so on the `opamp_otel` logger. When the client first reaches the server it logs,
at INFO, `OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout,
instance <uid>`. When it can't, it logs one WARNING per run of failures, with what to check:

```
OpAMP: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused this agent's
credentials: check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). Retrying in the background;
the SDK keeps its current settings meanwhile
```

and at INFO, `connected to ... again` once it's back. The warning names what to check as it was
given: with the headers given to `configure()`, it says `check the headers given to configure()`,
and likewise for the endpoint. The endpoint is logged without user info or a query string. An
endpoint without a scheme gets a warning at start: it is taken as `http://`. Warnings reach stderr even
when the only handler on the root logger is OpenTelemetry's (with
`OTEL_PYTHON_LOGGING_AUTO_INSTRUMENTATION_ENABLED=true`), where Python would otherwise only export
them as log records; a handler of the application's own shows them instead, once.

## In code: `configure()`

An application that sets its SDK up in code, rather than under `opentelemetry-instrument`
(Locust, say, whose gevent rules it out), gets the same with one call in place of that setup:

```python
import opamp_otel

opamp_otel.configure()
# then the application's own instrumentors, as before:
RequestsInstrumentor().instrument()
```

It sets the SDK up as `opentelemetry-instrument` would, from the same variables: the tracer, meter
and logger providers the `OTEL_*` variables describe (OTLP over gRPC by default, as the distro
above), set as the global ones, with the live sampler, the gated exporters and the instance UID as
`service.instance.id`, and the client started. The server comes from the `OTEL_OPAMP_*` variables
above, or from the application, as `configure()`'s own arguments:

```python
opamp_otel.configure(
    endpoint="https://opamp.example.com/v1/opamp",
    headers={"Authorization": f"Bearer {settings.opamp_key}"},
)
```

An argument given wins over its variable; one left out, `None` or blank comes from the variable,
and with neither it is the default:

| Argument | In place of | Takes |
|---|---|---|
| `endpoint` | `OTEL_OPAMP_ENDPOINT` | A URL, as the variable. Enough on its own to put the SDK under the server; with no endpoint from either it is upstream's distro and configurator, unmanaged. |
| `headers` | `OTEL_OPAMP_HEADERS` | A mapping of names to values, sent as given (not percent-decoded). It replaces the variable's headers, it isn't merged with them: `{}` is none at all. |
| `state_dir` | `OTEL_OPAMP_STATE_DIR` | A directory (`str` or a path), or `"none"` for none, not even the default one. |
| `heartbeat_interval` | `OTEL_OPAMP_HEARTBEAT_INTERVAL` | Seconds (`0.5`, `30`), a `timedelta`, or a string as the variable (`"30s"`, `"2m"`, `"1500"` for milliseconds). |

A value it can't use is warned about on the `opamp_otel` logger and is the default, as a variable
it can't read is: an endpoint that isn't a string leaves the SDK unmanaged, headers that aren't
all strings are none (the warning doesn't show them), a `state_dir` that isn't a path is the
default one, and an interval that isn't a positive duration (`0`, `-1`, `True`, NaN, `"soon"`) is
30 s. An endpoint whose scheme the client doesn't speak (not `http(s)://` or `ws(s)://`) is warned
about at start, as one without a scheme is. None of it raises. Its other keyword arguments are
upstream's configurator's (`sampler`, which a remote rate replaces while one is set,
`resource_attributes`, `span_processors`, and so on). Span processors given here, or added to the
provider later, aren't the package's: `send_traces` doesn't stop them. Call it once, before the application's instrumentors.

[examples/python-otel/in_code.py](../examples/python-otel/in_code.py) is a whole application set
up this way, the server taken from its own variables; `examples/python-otel/try.py` runs it
against the reference server with no `OTEL_OPAMP_*` variable set.

What it can't carry out: the instrumentation switches (`deactivate_instrumentations`,
`deactivate_all_instrumentations`), since it instruments nothing and the application names its
instrumentors in code. They are ignored rather than answered as done, like the rest above that a
process can't carry out.

## What it does

- **Joins the fleet** with one instance UID: the OpAMP instance UID, and `service.instance.id` in
  the telemetry (the configurator hands it to the SDK; it is not put in the environment, so a child
  process doesn't inherit it). With a state directory it is kept across restarts, and a UID the
  server assigns is saved there: the running SDK keeps its `service.instance.id`, and the next
  start uses the server's UID for both. Without a state directory, or where the UID can't be saved
  in it, a `service.instance.id` given in `OTEL_RESOURCE_ATTRIBUTES` is used if it is a UUID: the
  same at every start.
- **Takes a key of its own.** An agent enrolled with a shared key (the enrollment key in
  `OTEL_OPAMP_HEADERS`, or the headers given to `configure()`) is offered a key of its own by a
  server that hands them out ([connection-settings.md](connection-settings.md)). With a state
  directory the agent says it takes OpAMP connection settings (`AcceptsOpAMPConnectionSettings`,
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
  | `send_traces`, `send_logs` | every trace and log exporter the package sets up from `OTEL_TRACES_EXPORTER` and `OTEL_LOGS_EXPORTER`, wrapped in the SDK's export processors: `false` drops what would be exported. Processors passed to `configure()` or added later aren't stopped. |
  | `send_logs_level` | live, at the same gated log exporters: a record below the level's SeverityNumber is dropped at its next export, one with no severity (0) is kept, and left out every record goes. |
  | `sampling_rules`, `send_logs_level_by_scope`, `drop_attributes`, `mask_attributes`, `sampling_traces_per_second`, `send_logs_per_second`, `debug_until` (with `debug_sampling_rate`, `debug_send_logs_level`) | live, in the same sampler and gates: see [the newer settings](#the-newer-settings). |
  | `send_metrics` | the OTLP metric exporter, swapped for a gated one (metrics have no export-processor hook): `otlp` in `OTEL_METRICS_EXPORTER` becomes `opamp`, the same exporter the protocol variables pick, stoppable. Only the name `otlp` is: with no `otlp` among the metric exporters (`otlp_proto_http` named directly, say), nothing can stop them and the setting is ignored rather than answered as done ([the host](sdk-settings.md#the-host)). Nor with `otlp` beside another (`otlp,prometheus`): only the OTLP one could stop, so the setting is ignored too. |
  | `deactivate_instrumentations` | at the next start, added to `OTEL_PYTHON_DISABLED_INSTRUMENTATIONS`. Names are the instrumentations' entry points: `flask`, `requests`, `urllib`, and so on. |
  | `deactivate_all_instrumentations` | at the next start, `OTEL_PYTHON_DISABLED_INSTRUMENTATIONS=*`. |
  | `opamp_polling_interval` | at the next start, the interval it checks in at, until the server offers one. |

  `logging_level` and `infer_spans` (EDOT settings) aren't carried out, so the server can't change a
  log level; the package logs them at `info`, which the default level hides.

  With declarative configuration (`OTEL_CONFIG_FILE`), the file builds the SDK: the instance
  joins the fleet (the configurator starts the client, as upstream skips its OpAMP hook) and the
  instrumentation switches work, but the sampler and exporters are the file's, so
  `sampling_rate` and the `send_*` settings are ignored rather than answered as done, and the
  package warns.
- **Doesn't trace itself.** The OpAMP client runs with instrumentation suppressed, so its requests
  (and its goodbye) never show up as the application's spans, whichever HTTP instrumentations are
  installed.
- **Answers** each remote config APPLIED or FAILED, and reports the settings in force as its
  effective config, with the settings it set aside or didn't know named beside them in an
  `elastic.set_aside` entry ([sdk-settings.md](sdk-settings.md#the-answer)).
- **Says goodbye** (`agent_disconnect`) when the process exits: once the main thread is done,
  before the SDK flushes, and while host names can still be resolved (with threading's exit hooks,
  which run before `atexit`'s). The client runs on a thread of its own, so the application needs
  no event loop.
- **Takes forked workers along.** A process forked after the SDK is built (gunicorn's, Celery's
  or uWSGI's workers, `multiprocessing` with `fork`) is a worker of the parent, not an instance
  of its own. It drops its copy of the parent's client without a word to the server, and follows
  the live settings the parent is given: at its first fork the parent writes them to a file of
  its own (a new `0700` directory under the temporary directory) and rewrites it on each change,
  and each child reads it within a second. Next-start settings reach a child as they reach the
  parent, which it forked from. The parent stays the one instance, and every child's telemetry
  keeps the parent's resource, `service.instance.id` included. When the parent stops it removes
  the file, and its children keep the settings in force.

The rules are the SDK settings layer's (`opamp_client.sdk`), the same in every language. The
package only carries them out on the Python SDK.

## The newer settings

Each is carried out in the package's live sampler and gated exporters, so only where its
configurator built the SDK: there the package declares them to the layer ([the
host](sdk-settings.md#the-host)). Under another configurator, or `OTEL_CONFIG_FILE`, it doesn't,
and they are set aside: named as not carried out, never reported in force. The rules for each are
[sdk-settings.md](sdk-settings.md#settings-that-match-telemetry)'s; what follows is where they
apply in Python. Each config below is the remote config's `elastic` entry, ready to send.
`examples/python-otel/try.py` checks each one against the reference server and its sink.

### Sampling rules

```json
{"sampling_rate": "0.25",
 "sampling_rules": [{"rate": 0, "kind": "server", "path": "/health*"},
                    {"rate": 0, "kind": "client", "attributes": {"url.full": "*/health"}},
                    {"rate": 1, "name": "checkout *"}]}
```

No traces of health checks, served or made, every `checkout ...` trace, and a quarter of the
rest. In the live sampler (`opamp_otel.LiveSampler`), for a span with no parent only: the first
rule that matches sets the rate, applied by the SDK's `TraceIdRatioBased`; with none,
`sampling_rate`; with neither, the configured sampler (`OTEL_TRACES_SAMPLER`). A span with a
parent follows it. What a rule sees is what the instrumentation passes when it starts the span:

- ASGI and WSGI servers (and Flask, Django, FastAPI over them) start with `http.target`, the path
  and query (the default, old semantic conventions), or `url.path` with
  `OTEL_SEMCONV_STABILITY_OPT_IN=http`: `path` matches either. Flask and FastAPI also set
  `http.route` at start, so it can be named under `attributes`; Django doesn't. Server spans start
  named by the method, or `GET /path` for some: match them by `path` and `kind`.
- Client instrumentations (urllib, requests, httpx) start with `http.url` (or `url.full`) and no
  path: match the URL under `attributes`, as above.
- A double or an array attribute never matches; an integer matches by its decimal text.

### Log levels per logger

```json
{"send_logs_level": "warn", "send_logs_level_by_scope": "myapp.db*=debug, urllib3*=error, noisy.lib=off"}
```

At the log gate, beside `send_logs_level`. A record's scope is the Python logger it was logged on:
the logging handler exports each record under its logger's name (`myapp.db.pool`). So
`myapp.db*` covers `myapp.db` and every logger under it, and `noisy.lib=off` drops all of that
logger's records, even those with no severity. Only what is exported is filtered: the logger's
own level still decides what is logged at all, and the package never changes it.

### Redaction

```json
{"drop_attributes": "http.request.header.authorization, *password*", "mask_attributes": "enduser.id, user.email"}
```

At the span and log gates, after the levels and the cap: span attributes, span event attributes,
span link attributes and log record attributes. A span with something to redact is exported as a
new `ReadableSpan`, and a log record as a copy with a copy of its `LogRecord`: the span or record
the application made is shared with every other processor, and stays as it was, so an exporter
the package doesn't gate (a span processor passed to `configure()`, say) sees the original. What
nothing matches goes on unchanged. Metrics, resource and scope attributes, span names and log
bodies are left alone.

### Rate caps

```json
{"sampling_rate": "1", "sampling_traces_per_second": "50", "send_logs_per_second": "200"}
```

Traces: in the live sampler, after the rules and the rate; a new trace, or one a caller sampled,
takes a token, and a span of a trace already sampled in this process never does, nor one sampled
under a caller that didn't sample (the application's own sampler may sample one when only a cap is
set). Logs: at the log gate after the levels; a batch keeps its first records while tokens last.
Each process has its own buckets, so a gunicorn worker is capped on its own. With the default
`OTEL_BLRP_SCHEDULE_DELAY` (1 s) the log cap lets n records a second through; a longer delay makes
it stricter.

### Debug mode

```json
{"sampling_rate": "0.05", "send_logs_level": "warn",
 "debug_until": "2026-10-14T15:30:00Z", "debug_sampling_rate": "1", "debug_send_logs_level": "debug"}
```

Send your own time, at most 24 h ahead. The layer folds debug mode into the settings it hands
over; the package runs one timer (a daemon thread, `opamp-timer`) for the time the layer says to
ask again, on the monotonic clock. When it fires it asks the layer, puts the result in force
(forked workers follow, through the relay) and reports the new effective config: debug mode ends
whether or not the server can be reached. A restart before the end, with a state directory, is
still in debug mode.

### Export health

No setting: always on. Each gate counts its exports after the exporter's own retries (a
`FAILURE` result, or an exception with its text), the items in failed exports, and what it drops
on purpose (`send_*` false, a level, a cap). The OTLP metric exporter is counted by data point,
and only when it is the gated one (`otlp`). The agent's health goes with its first message and is
looked at again every 30 s, on the same timer: sent at once when a signal starts or stops
failing, at most a minute apart while only the counters move. `healthy` stays true (the
application is running); the exporters are under `component_health_map["exporters"]`
([sdk-settings.md](sdk-settings.md#export-health)). The Python exporters log why an export failed
but don't return it, so a `FAILURE` reads "the exporter answered FAILURE (its own log says why)".
Forked workers' exports are counted in the workers, which don't report: under gunicorn, health
shows the master's exports only.

## The application's own settings

The server may send the application settings of its own in `custom` (a feature flag, a
verbosity), which the package hands on rather than reads ([app-settings.md](app-settings.md)):

```python
from opamp_otel import app_settings

if app_settings.get("checkout.new_flow", "off") == "on":
    ...
unsubscribe = app_settings.on_change(lambda settings, changed: print("changed:", sorted(changed)))
```

`get(key, default)` is the value in force or the default, never an error; `get_bool`, `get_int`
and `get_float` read one typed; `snapshot()` is every value in force. A callback gets the map in
force and the keys that changed, once per change, on the OpAMP client's thread; one that raises
is logged and the client goes on. Every integration path declares `custom`, whoever built the
SDK, and a forked worker follows its parent's values. With a state directory the values in force
are saved and loaded again at the next start, before the application's code runs.

## Inventory

No setting: always on. Once the SDK is built, the agent reports what it runs as its available
components, in the shape every language sends
([available-components.md](available-components.md#what-the-sdk-integrations-report)):

- `sdk`: `pydantic-opamp-otel` and its version, `python`, and the `opentelemetry-sdk` version.
- `instrumentations`: every instrumentation `opentelemetry-instrument` can run, switched off or
  not, under its entry point's name (`flask`, `requests`, `aiohttp-client`), which is the name
  `OTEL_PYTHON_DISABLED_INSTRUMENTATIONS`, and so `deactivate_instrumentations`, takes. Each has
  its distribution's version. One whose library isn't installed is left out, as
  `opentelemetry-instrument` skips it. Under `configure()` the list is the same, though nothing
  there reads the switch.
- `exporters`: read off the SDK's tracer, meter and logger providers, so what was built, however
  it was. Each is named by its entry point, as `OTEL_*_EXPORTER` names it (`otlp_proto_grpc`,
  `otlp_proto_http` and the package's gated `opamp` are all `otlp`; `console`, `prometheus`,
  `zipkin`), or by its type when no entry point names it (`InMemorySpanExporter`). `gated` is
  whether the package's gate wraps it (the trace and log exporters its configurator built, and the
  OTLP metric exporter) and the host declares its signal's `send_*` setting. An exporter another
  configurator or `OTEL_CONFIG_FILE` built, every trace exporter when a span processor given to
  `configure()` exports past the gate (and every log exporter, likewise for a log record
  processor), and every metric exporter when one beside `otlp` makes `send_metrics` ignored: each
  is `gated: false`, as the settings left undeclared say. An exporter set to `none` isn't listed.
- `settings`: the settings this process carries out, from the same host its decisions use.

It is sent once: nothing it lists changes while the process runs.

## How it is checked

- Unit tests (`python/otel/tests`) cover these:
  - the controls change sampling and each signal's exports on a real SDK;
  - the next-start settings become the instrumentation switch;
  - the answers, saving and reporting follow the layer;
  - settings another configurator's SDK can't carry out are ignored, not answered as done;
  - `configure()` sets an SDK up from the variables under the server (spans stop when the server
    says so, the metric exporter is the gated one, one UID), is the SDK's own setup without an
    endpoint, and ignores the instrumentation switches;
  - each of `configure()`'s server arguments, given in code, as a variable, both (code wins) or
    neither (the default); the forms each takes; blank as not given; and each value it can't use
    warned about and the default, never raised. End to end, the headers, state directory and
    interval given in code reach the server and the variables' don't;
  - when the server can't be reached, the warning names what gave the endpoint and the headers:
    the argument given to `configure()`, else the variable;
  - the UID is kept across starts, a UID the server assigns is used at the next one, and the UID
    is not left in the environment for child processes;
  - the goodbye reaches a server named by host at exit, and a forked child drops the parent's
    client while the parent carries on;
  - a forked worker follows a live setting the parent is given after the fork, and keeps what's
    in force when the parent is gone;
  - without an endpoint nothing changes;
  - the application's own settings (`tests/test_app_settings.py`): reads before any config, after
    one and once a key is left out; the typed reads against the shared vectors; a callback told
    once per change and never for the same map, in order, one at a time; a callback that raises;
    unsubscribing; readers and subscribers on other threads; a bad map FAILED with nothing changed;
    restarts with and without a state directory; forked workers; the shared vectors through the
    layer and the agent;
  - the newer settings (`tests/test_next_settings.py`): every shared vector in
    `testdata/sdk-settings` that applies, through the package's own sampler, gates
    and agent (`sampling`, `logs`, `redact`, `caps` with an injected clock, `debug` through
    decisions, restarts and refreshes, `health` through the agent's counters); generated rules,
    scope levels and keys against the layer's `Evaluate`; and each setting's edge cases from
    sdk-settings.md, live changes and reverting included;
  - the inventory (`tests/test_inventory.py`): the instrumentations are the entry points
    `opentelemetry-instrument` matches, at their versions, a missing library's left out; the
    exporters are read off real providers, gated only behind the package's gates, a duplicate
    ungated one making the name ungated; the settings are the agent's host's; and under
    `opentelemetry-instrument` and `configure()`, the components reach a test server in full,
    with their hash. The client's `components` call runs the shared vectors
    (`python/tests/test_sdk_settings.py`).
- End to end: `python3 examples/python-otel/try.py` (CI's `python-otel` job), the checks every
  integration passes ([javaagent.md](javaagent.md#how-it-is-checked), `examples/integrations.py`).
  A program with no OpenTelemetry code runs under `opentelemetry-instrument`, and its urllib
  requests are traced; then the same program set up in code (`in_code.py`), the server given to
  `configure()` and no `OTEL_OPAMP_*` variable set, passes the same checks, but for the
  instrumentation switch, which it ignores: its spans go on at the next start. Its `setup-in-code`
  check gives `configure()` the wrong token and then the right one, each time with the other token
  in `OTEL_OPAMP_HEADERS`. The token given in code wins both times. The wrong token's warning names
  the headers given to `configure()`, not `OTEL_OPAMP_HEADERS`.
