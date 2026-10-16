# The OpenTelemetry Java agent, under an OpAMP server

`java/opamp-javaagent` is an extension for the upstream
[OpenTelemetry Java agent](https://github.com/open-telemetry/opentelemetry-java-instrumentation).
With it, the agent joins an OpAMP server's fleet and takes its SDK settings from the server:
sampling and what to send while it runs, which instrumentations run at the next start
([sdk-settings.md](sdk-settings.md)). The
application needs no code (unless it reads [settings of its own](#the-applications-own-settings)),
and the agent stays the upstream one.

Built and tested against agent 2.31.1 (SDK 1.66). It needs Java 17 or later.

## Use it

Build the extension jar (Maven, from the repository root):

```bash
mvn -q -B -DskipTests install -f java/opamp/pom.xml
mvn -q -B -DskipTests package -f java/opamp-javaagent/pom.xml
```

That gives `java/opamp-javaagent/target/opamp-javaagent-extension-0.1.0-SNAPSHOT.jar`, one jar
with the OpAMP client and its dependencies inside, relocated so nothing collides with the
application's. Then start the application with the agent and the extension:

```bash
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
java -javaagent:opentelemetry-javaagent.jar \
     -Dotel.javaagent.extensions=opamp-javaagent-extension.jar \
     -jar checkout.jar
```

| Setting (system property, or the environment variable in capitals) | |
|---|---|
| `otel.opamp.endpoint` | The OpAMP server: `http(s)://.../v1/opamp` polls, `ws(s)://...` keeps a connection open. Without it the extension does nothing. |
| `otel.opamp.headers` | Headers for every request, as `OTEL_EXPORTER_OTLP_HEADERS`: `name=value,...`, values percent-encoded (`+` is a plus). A value with an escape it can't decode is sent as written, with a warning naming the header. The enrollment key goes here. |
| `otel.opamp.state.dir` | Where to keep state across restarts: the instance UID, the settings for the next start, the client's own state (written readable by the application's user only). Without it, the extension keeps one of the user's own, one per JVM of the program running at once ([default-state-dir.md](default-state-dir.md)); `none` turns that off. A directory named here is one per process: two JVMs given the same one would share an instance UID and overwrite each other's state. With `none`, settings that take effect at the next start are refused; live ones still apply. With `none`, or a directory that can't be written (checked at start), each start is a new instance; in a directory that can't be written, a settings change fails, as it can't be saved, as in every language. |
| `otel.opamp.heartbeat.interval` | How often to check in until the server offers an interval (default 30 s). A value it can't read is warned about and the default used; it never stops the SDK. |

Everything else is the agent's usual configuration. The settings the server sends override the
agent's own for as long as they are sent: withdraw a setting and the agent's own value comes
back. They reach the sampler and exporters the agent builds from its `otel.*` configuration, and
nothing else. Where the agent's SDK sends telemetry another way, the extension sets aside the
settings it can't carry out, so they are never answered as done
([what another extension adds](#what-another-extension-adds)).

With declarative configuration (`OTEL_CONFIG_FILE`, `otel.config.file`) the agent builds its SDK
from the file and runs none of the autoconfigure hooks, so the extension takes the file's model
instead, before the SDK is built from it. The `otel.opamp.*` settings above still come from
system properties or the environment, not the file. The sampler the file names and the exporter
of each `batch` or `simple` processor and each `periodic` or `pull` reader are wrapped in an
`opamp` component of the extension's, which builds what the file named and puts the same
controls around it. The instance UID goes into `resource.attributes`, and the next-start settings
go into `distribution.javaagent.instrumentation`, where the agent reads which instrumentations
run: `disabled` (with `_` for `-`, as the agent compares names) and `default_enabled: false`.
Every setting is carried out as without the file. A processor or reader the file names by a
component of its own isn't covered, as an extension's isn't. The model's classes change from
one SDK release to the next, so the extension is built against the agent's own (1.65.0-alpha for
agent 2.31.1). If an endpoint is set but the agent built its SDK without calling either hook, the
extension logs a WARNING saying it won't connect, rather than nothing.

### What another extension adds

Another agent extension can change the SDK through the same autoconfigure customizers. The
extension's customizers run last, and it reads what the SDK is built with:

- **A processor or reader of its own.** A tracer, logger or meter provider customizer can add a
  span processor, a log record processor or a metric reader. What it exports doesn't go through
  the extension's gate, so that signal's gate settings are set aside, as under a configuration
  file. For traces these are `send_traces`, `drop_attributes` and `mask_attributes`. For logs they
  are `send_logs`, `send_logs_level`, `send_logs_level_by_scope`, `send_logs_per_second`,
  `debug_send_logs_level` and the two redaction settings. For metrics it is `send_metrics`. The
  agent logs a WARNING that names what was added and what is set aside. The inventory lists it,
  ungated. A span processor that isn't told when a span ends (`isEndRequired()` false) isn't
  counted: the agent adds such processors itself, and one that exported would send spans before
  they end.
- **What is left alone.** A processor or reader the agent built from `otel.*.exporter` stays
  gated, also when another extension's processor customizer wrapped it. A span processor that
  isn't told when a span ends can't export, so it changes nothing: the agent's thread details and
  baggage processors are two. The agent's baggage log record processor only adds attributes. Any
  other log record processor counts as one that exports, as the extension can't tell what it does.
  So does the span processor the agent adds for `otel.javaagent.debug`, which logs every span.
- **A sampler of its own.** A tracer provider customizer can set a sampler in place of the
  extension's. Once the SDK is built, the extension checks its sampler. If it isn't the
  extension's, `sampling_rate`, `sampling_rules`, `sampling_traces_per_second` and
  `debug_sampling_rate` are set aside, with a WARNING. A sampler set through
  `otel.traces.sampler` or a sampler customizer is still the one the extension wraps.

All of this is decided before the client starts, so the first effective config already names
these settings as set aside. The SDK's builders don't expose their processors and readers, so the
extension reads them reflectively. If it can't, it logs a WARNING and sees nothing added.

The extension can't see an SDK the application builds itself. An application that makes its own
`OpenTelemetrySdk` and uses it directly sends that SDK's telemetry past every setting, and the
extension still answers for the agent's SDK.

### Is it working?

The extension says so on its `java.util.logging` logger, `dev.pydantic.opamp.javaagent.Extension`.
When the client first reaches the server it logs, at INFO, `OpAMP: connected to
https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>`. When it
can't, it logs one WARNING per run of failures, with what to check:

```
OpAMP: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused this agent's
credentials: check otel.opamp.headers / OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). Retrying
in the background; the SDK keeps its current settings meanwhile
```

and at INFO, `connected to ... again` once it's back. The endpoint is logged without user info, a
query string or a fragment. An endpoint without a scheme gets a warning at start: it is taken as `http://`.

## What it does

- **Joins the fleet** with one instance UID: the OpAMP instance UID, and `service.instance.id` in
  the telemetry, so the fleet view and the traces name the same instance. With a state directory
  it is kept across restarts. A UID the server assigns is kept too: the client uses it at once,
  and it is `service.instance.id` from the next start (the resource is built once). Without a
  state directory, or where the UID can't be saved in it, a `service.instance.id` given is used if
  it is a UUID: the same at every start.
- **Takes a key of its own.** An agent enrolled with a shared key (the enrollment key in
  `otel.opamp.headers` or `OTEL_OPAMP_HEADERS`) is offered a key of its own by a server that hands
  them out ([connection-settings.md](connection-settings.md)). With a state directory the agent
  says it takes OpAMP connection settings (`AcceptsOpAMPConnectionSettings`,
  `ReportsConnectionSettingsStatus`) and takes the offer: it tries the new key (APPLYING), keeps it
  once the server answers with it (APPLIED; the server may then refuse the shared key for it), and
  goes back to the key it had if the server doesn't (FAILED, saying why). The key is kept in the
  state directory, readable by the application's user only, and the next start comes back with it;
  changing the endpoint or the headers it is given starts again from them. An offer may carry an
  interval to check in at (the server's heartbeat interval), which holds until the next start.
  Without a state directory a restart would lose the key, so the agent doesn't say it takes one.
- **Describes itself** from the agent's resource: `service.name`, `service.namespace` and
  `service.instance.id` identify it. Host, process, runtime and `telemetry.distro.*` describe it.
- **Takes settings** from the remote config's `elastic` entry:

  | Setting | How |
  |---|---|
  | `sampling_rate` | a sampler in place of the agent's (`otel.traces.sampler`, whatever it names); a span whose parent was sampled, here or by the caller, is sampled at any rate, 0 included. Left out: the agent's own sampler. |
  | `send_traces`, `send_metrics`, `send_logs` | the agent's exporters, wrapped: `false` drops what would be exported (it is lost, not held; spans are still made). A pull reader (`otel.metrics.exporter=prometheus`) is wrapped too: with `send_metrics: false` a scrape gets nothing. |
  | `send_logs_level` | the same log exporters, at once: a record below that severity is dropped at export, one at it or above is exported, and one with no severity (SeverityNumber 0) is always exported. Left out: every severity. |
  | `send_logs_level_by_scope`, `drop_attributes`, `mask_attributes`, `sampling_rules`, `sampling_traces_per_second`, `send_logs_per_second`, `debug_until`, `debug_sampling_rate`, `debug_send_logs_level` | at once: see [the settings that match telemetry](#the-settings-that-match-telemetry). |
  | `deactivate_instrumentations` | at the next start, `otel.instrumentation.<name>.enabled=false`. Names are the agent's: `jdbc`, `spring-webmvc` (every version) or `spring-webmvc-6.0` (one), `java-http-client`, and so on; `_` reads as `-`. |
  | `deactivate_all_instrumentations` | at the next start, `otel.instrumentation.common.default-enabled=false`. The application's own spans, metrics and logs (through the OpenTelemetry API) stop too. |
  | `opamp_polling_interval` | at the next start, the interval it checks in at, until the server offers one. |

  `logging_level` and `infer_spans` (EDOT settings) aren't carried out, so the server can't change a
  log level; the extension logs them.
- **Doesn't trace itself.** The OpAMP client runs with instrumentation suppressed (its own
  HTTP client runs every task that way), so its requests never show up as the application's spans.
- **Answers** each remote config APPLIED or FAILED, and reports the settings in force as its
  effective config, with the settings it set aside or didn't know named beside them in an
  `elastic.set_aside` entry ([sdk-settings.md](sdk-settings.md#the-answer)).
- **Says goodbye** (`agent_disconnect`) when the JVM stops (waiting up to 5 s), from a shutdown
  hook of its own. The agent's SDK flushes from its own hook meanwhile: the JVM runs shutdown hooks
  in no set order, so the goodbye isn't ordered before the flush.

The rules (what a document means, what to answer, what to report) are the SDK settings layer's,
the same in every language. The extension only carries them out on the agent.

## The settings that match telemetry

The extension declares each of these to the layer ([the host](sdk-settings.md#the-host)), under
autoconfigure and declarative configuration alike: both put the same sampler and gates in. Under a
configuration file, a span or log processor that is neither batch nor simple, or a metric reader
neither periodic nor pull, is a component the extension can't gate: that signal's sending, and what
its gate holds, aren't declared, and the agent logs a warning. What each means is in
[sdk-settings.md](sdk-settings.md#settings-that-match-telemetry); here is where the agent carries
it out, and what it sees. Each example is a whole `elastic` document, as the server sends it.

### Sampling rules

```json
{
  "sampling_rate": "0.1",
  "sampling_rules": [
    {"rate": 0, "kind": "server", "path": "/health*"},
    {"rate": 0, "kind": "client", "attributes": {"url.full": "*/health*"}},
    {"rate": 1, "name": "checkout *"}
  ]
}
```

- In the extension's sampler, which is in place of the agent's (`LiveSampler`). For a new trace
  the first rule its root span matches decides the rate, applied by the SDK's trace-ID ratio
  sampler; then `sampling_rate`; then the agent's own sampler. A span with a parent follows it.
- A rule sees what the agent's instrumentation sets when it starts the span. HTTP server spans
  start with `url.path`, `http.request.method` and the kind `server`, and are named `GET`:
  `http.route` is mostly added later, so match `path`, not the route or the name. HTTP client
  spans start with `url.full`, `server.address` and `server.port`, so match those under
  `attributes`. Spans the application starts itself have their own name.
- The sampler `otel.traces.sampler` names (the agent's, or another extension's) is what the
  rules and the rate fall back to, and what decides every span while neither is set. A sampler
  another extension sets in place of the extension's sets the rules aside
  ([what another extension adds](#what-another-extension-adds)).

### Log levels per scope

```json
{"send_logs_level": "warn", "send_logs_level_by_scope": "com.acme.db*=debug, org.apache.kafka*=error, noisy.lib=off"}
```

- At the log gate, beside `send_logs_level`. The scope is the logger's name: the agent's
  logback, log4j and java.util.logging bridges name the scope after the logger, so
  `com.acme.db*` is every logger under `com.acme.db`.
- The logging framework's own level still decides what is logged at all: a `debug` here can't
  export what logback was told to drop. It only drops more, or lets through what
  `send_logs_level` would have dropped.

### Redaction

```json
{"drop_attributes": "http.request.header.authorization, http.request.header.cookie", "mask_attributes": "*password*, *token*, user.email"}
```

- At the span and log gates: span attributes, span event attributes (an exception event stays
  one), span link attributes and log record attributes. Exported spans and records are new
  objects over the application's, so another exporter, or a span processor of another extension,
  sees the original. A dropped attribute isn't counted as dropped by the SDK's limits.
- Not metrics, resources, scopes, span names or log bodies (sdk-settings.md says why).

### Rate caps

```json
{"sampling_rate": "1", "sampling_traces_per_second": "50", "send_logs_per_second": "200"}
```

- Traces: in the sampler, after the rules and the rate. A sampled root span, or a span whose
  remote parent was sampled, takes a token; a child of a span sampled in this JVM never does, so
  a trace is whole or not there at all.
- Logs: at the log gate, after the levels: of each batch, the first records the bucket allows.
  The agent's batch delay is 1 s (`otel.blrp.schedule.delay`); a longer one makes the cap stricter.

### Debug mode

```json
{"sampling_rate": "0.05", "send_logs_level": "warn", "debug_until": "2026-10-14T15:30:00Z", "debug_sampling_rate": "1", "debug_send_logs_level": "debug"}
```

- The extension passes the layer the JVM's wall clock, and runs one daemon timer
  (`opamp-settings-timer`) for the time the layer says to ask again. When it fires it asks the
  layer what is in force, puts it in force and reports the effective config, whether the server
  is reachable or not. The time is waited on a monotonic clock: setting the wall clock back
  doesn't stretch the window.
- With a state directory, a restart before the end is still in debug mode; without one, debug
  mode comes back when the server sends the config again, with the same end.

### Export health

No setting: always on. Each gate counts its exporter's results, after the OTLP exporter's own
retries, and what it dropped on purpose. Every 30 s, on the same timer, the layer says whether
to send; the agent health then carries `component_health_map["exporters"]`, one child per signal,
while the top-level `healthy` stays true. A failure whose exporter gives no exception (the OTLP
exporters log the HTTP status instead) has no `last_error` of its own; the agent's log has it.
A pull reader (Prometheus) exports nothing, so nothing is counted for it.

## The application's own settings

The server may send the application settings of its own in `custom` (a feature flag, a
verbosity, a limit), which the extension hands on rather than reads
([app-settings.md](app-settings.md)). The application reads them with
`dev.pydantic.opamp.AppSettings`, from the extension's second jar,
`opamp-javaagent-extension-<version>-app-settings.jar` (the `app-settings` classifier: four
classes, nothing else), which it compiles against and runs with:

```java
import dev.pydantic.opamp.AppSettings;

if (AppSettings.get("checkout.new_flow", "off").equals("on")) {
  // ...
}
long limit = AppSettings.getLong("checkout.max_items", 50);

BlockingQueue<Set<String>> changes = new LinkedBlockingQueue<>();
AutoCloseable subscription = AppSettings.onChange((settings, changed) -> changes.add(changed));
// ... the application's own thread takes the changes and does the work
```

`get(key, def)` is the value in force or `def`, never an exception: before the first config,
without the agent, the extension or an OpAMP server, and when the config in force leaves the key
out. `getBoolean`, `getLong` and `getDouble` read one typed, by the rules every language shares,
and are `def` for anything else; `snapshot()` is every value in force, an unmodifiable map no
later change alters. A listener given to `onChange` gets the values in force (the same map) and
the keys added, changed or removed (sorted), once per change, in order, one at a time, on the
OpAMP client's thread: keep it quick. An exception it throws is caught and logged (SEVERE, on
`dev.pydantic.opamp.AppSettings`); the others are still called and the config is still APPLIED.
Closing what `onChange` returns removes it. Any thread may read, subscribe and unsubscribe at any
time, and a reader sees the whole of one map or the whole of the next.

**How the extension reaches the application.** The agent loads the extension in a class loader
of its own, which the application's class loaders can't see, and the application's classes are
out of the extension's sight too. The one class loader every other reaches is the bootstrap, where
the agent itself keeps what it shares with instrumented code. So with an endpoint configured, the
extension writes one class of its own, `dev.pydantic.opamp.internal.AppSettingsBridge` (the JDK's
types alone in it), into a jar in a temporary directory of the agent user's and appends that to
the bootstrap class path, through the `Instrumentation` the agent keeps. The application's
`AppSettings` finds it with `Class.forName(name, true, null)` as it is loaded, whatever its own
class loader delegates to (a servlet container's child-first loader too), and from then on the
two speak in maps, sets and a `BiConsumer`. Each class loader that has `AppSettings` (each web
application in an application server) gets its own copy, all filled from the one bridge, which
holds them weakly, so undeploying an application frees its class loader. If the bridge can't be
put in place the extension says so (a WARNING), every read is its default, and `custom` isn't
declared, so the server's are set aside and named as unsupported rather than answered APPLIED for
nothing.

The extension declares `custom` (`Host.live`) when the bridge is in place. The values go in force
after the SDK's settings of the same document, under the same lock, at the start (the saved
settings, from the state directory), on each APPLIED config and on a refresh (the end of debug
mode), so a listener sees the SDK's settings already in force. With a state directory the values
in force at exit are in force again before the application's `main` runs, whether the server can
be reached or not; a listener the application adds is told of the server's changes from then on.
If the extension can't put its controls into the SDK (it then runs unmanaged), the values go back
to those in force before. `examples/javaagent/App.java` reads the flag `example.feature` and
requests `/changed` from its own loop for each change it is told of.

## Inventory

No setting: always on. Once the agent has built its SDK, the extension reports what it runs as its
available components ([available-components.md](available-components.md#what-the-sdk-integrations-report)),
built by the SDK settings layer as in every language:

- `sdk`: the extension's Maven coordinates, `dev.pydantic:opamp-javaagent-extension`, and its
  version; `telemetry.sdk.language` `java` and the version of the SDK the agent bundles.
- `instrumentations`: every instrumentation module the agent can run (its own and any
  extension's), switched off or not, under each name `otel.instrumentation.<name>.enabled` takes
  for it, so the names are the ones `deactivate_instrumentations` takes: `spring-webmvc` and
  `spring-webmvc-6.0`, `jdbc`, `java-http-client`. The version is the one the agent embeds for that
  instrumentation (`2.31.1-alpha`, the version its telemetry's scope carries); a module that embeds
  none has no version.
- `exporters`: each exporter the SDK was built with, by signal, as `otel.traces.exporter`,
  `otel.metrics.exporter` and `otel.logs.exporter` name it (`otlp` when unset, nothing for `none`,
  `console`, `prometheus`, or the name another library's exporter is configured by). Every one is
  `gated`: it is inside the extension's gate. Under declarative configuration the file's exporters
  are listed the same way (`otlp_http` and `otlp_grpc` as `otlp`, `prometheus/development` as
  `prometheus`), and a processor of a kind of its own, which the controls don't reach, by its name
  with `gated` false. A processor or metric reader another extension adds is listed with `gated`
  false too: a batch or simple processor by its exporter's class (`OtlpHttpSpanExporter`), any
  other processor or reader by its own class.
- `settings`: every setting above, `at once` or `next start`.

The agent builds its SDK once and declares the same settings while it runs, so the inventory is
sent once, at start: the hash in the first message, the components in full when the server asks.

## How it is checked

- Unit tests (`java/opamp-javaagent/src/test`): the controls change sampling and exports on a
  real SDK, and the next-start settings become agent properties. The extension answers, saves
  and reports as the layer decides: a FAILED changes nothing; without a state directory, a
  next-start change is refused and a live one applied; with one it can't write, a change fails; the UID is kept
  across starts, and so is one the server assigns; saved files are the agent user's only. A pull
  metric reader serves nothing while `send_metrics` is `false`. Under declarative configuration
  the SDK is built from a configuration file through the extension's hooks, with the file's
  sampler and exporters inside the controls, the UID in the resource, and the next-start
  settings in the distribution.
- The application's own settings (`AppSettingsTest`, `AppSettingsExtensionTest`): reads before
  any config, without the agent and once a key is left out; `custom.json`'s `typed` vectors and
  their edges; every `custom.json` case through the extension's own `Decide`, and the hosts that
  don't declare it; a listener told once per change and never for the same map (sent again,
  another way, beside another setting, at the end of debug mode), in order, one at a time; an
  exception caught and logged with the others still told; unsubscribing, also from inside a
  listener; readers and subscribers on other threads, and documents applied from several threads
  going in whole; a FAILED config changing nothing; restarts with and without a state directory;
  a failed setup putting back what was in force; several class loaders on one bridge, one that is
  gone no longer held; and the bridge's jar loaded by a class loader that sees the JDK alone.
- What another extension adds (`OutsideTest`), on an SDK built through another extension's
  customizers and then the extension's: a span processor, a log record processor and a metric
  reader of its own each set aside their signal's settings, named as unsupported, and are listed
  ungated; a span processor that only starts spans, and another extension's wrapper around the
  agent's own processors, set nothing aside; a sampler set in place of the extension's sets the
  sampler's settings aside, while one set through the sampler customizer is still wrapped.
- The inventory (`InventoryTest`) of an SDK built through the extension's hooks: the
  names `deactivate_instrumentations` takes, versions where embedded, each exporter as configured
  (`none`, another library's, a processor of a file's own kind ungated), and the client sending
  the hash first and the components in full when a test server asks.
- The shared test vectors (`testdata/sdk-settings`), each case through the agent's own
  sampler, gates and extension: sampling, log levels, redaction, rate caps (the clock injected),
  debug mode over time and export health. Generated settings and inputs are compared with the
  layer's reference matchers (its `Evaluate` call), and each setting's edge cases (live changes,
  withdrawal, a second exporter that sees the original) run on a real SDK.
- End to end: `python3 examples/javaagent/try.py` (CI's `javaagent` job). A program with no
  OpenTelemetry code runs under agent 2.31.1 and the extension, against the reference server.
  The agent traces the program's HTTP requests.
  1. The server sends a sampling rate: APPLIED, and spans arrive.
  2. It sends `send_traces: false`: APPLIED, and no span is exported while it holds.
  3. It sends a rate of 7: FAILED, naming `sampling_rate`, and nothing changes.
  4. It sends a rate of 1 with `send_traces` left out: APPLIED, and spans come again.
  5. It sends `deactivate_instrumentations`: APPLIED, and the instrumentation stays on until the
     restart.
  6. After the restart: the same instance UID (16 bytes), the effective config reported, and the
     HTTP instrumentation off, so no spans although the program makes requests while the agent is
     connected.

  7. The settings that match telemetry, one after the other, each checked in what the agent
     exports (through a recording proxy on its way to the reference sink): a rule of rate 0 for
     the program's `/health` checks; `app.noisy=off`; a dropped and a masked attribute; and caps
     of one trace and one log record a second.
  8. Debug mode with `sampling_rate` 0: spans until `debug_until`, none after, and the effective
     config reported at the end, the server sending nothing.
  9. The proxy refuses trace exports for a minute: the agent sends its health once they fail, and
     again once they succeed.

  Then `check_truth`, once for each setup a second extension (`examples/javaagent/Setups.java`)
  makes, named in `EXAMPLE_SETUP`. The server offers one setting a document, and each must show in
  the telemetry or be named as set aside, never neither. `extension-processor` adds a span
  processor, a log record processor and a metric reader exporting to the same sink: their
  signals' gate settings are set aside, and the sampler's are carried out. `extension-sampler`
  sets a sampler that samples every trace: `sampling_rate`, `sampling_rules` and
  `sampling_traces_per_second` are set aside, and the gates' settings are carried out.

  Then `check_app_settings` ([app-settings.md](app-settings.md#how-it-is-checked)): the program
  reads `example.feature` with `AppSettings`, which the server turns on and off; a bad map is
  FAILED and changes nothing, the same map again tells nothing, and the next start has the saved
  value from the start.

  `--local` builds and runs with this host's Maven and JDK in place of Docker (`MAVEN_ARGS` is
  passed to Maven). Runs 1 and 2 end with `agent_disconnect`. Each check that nothing arrives
  also checks that the program made requests to trace in that window, so it can't pass for want of them. Each runner
  keeps its builds, logs and state in `target/integrations/<runner>`.
