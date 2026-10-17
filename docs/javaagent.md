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
| `otel.opamp.headers` | Headers for every request, as `OTEL_EXPORTER_OTLP_HEADERS`: `name=value,...`, values percent-encoded (`+` is a plus). A header whose value has an escape it can't decode, or that HTTP can't carry, is left out, with a warning naming the header. The enrollment key goes here. |
| `otel.opamp.state.dir` | Where to keep state across restarts: the instance UID, the settings for the next start, the client's own state (written readable by the application's user only). Without it, the extension keeps one of the user's own, one per JVM of the program running at once ([default-state-dir.md](default-state-dir.md)); `none` turns that off. A directory named here is one per process: two JVMs given the same one would share an instance UID and overwrite each other's state. With `none`, settings that take effect at the next start are refused; live ones still apply. With `none`, or a directory that can't be written (checked at start), each start is a new instance; in a directory that can't be written, a settings change fails, as it can't be saved, as in every language. |
| `otel.opamp.heartbeat.interval` | How often to check in until the server offers an interval (default 30 s): milliseconds, or `500ms`, `30s`, `2m`. A value it can't read is warned about and the default used; longer than a day is a day; it never stops the SDK. |

The core reads these, by the same rules in every language: the endpoint, the headers, the interval, which state directory, and the instance UID kept there. The extension keeps only the I/O: reading the settings, making and locking the state directory's slot, reading and writing its `instance-uid` file, and logging the warnings, which name the system property.

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
  the extension's gate, so that signal's gate controls are set aside, as under a configuration
  file. For traces these are `signal_exports.traces` and `attribute_redaction`. For logs they are
  `signal_exports.logs`, `log_levels` (`level` and `scopes`), `log_rate_limit`, `debug`
  (`log_level`) and `attribute_redaction`. For metrics it is `signal_exports.metrics`. The agent
  logs a WARNING that names what was added and what is set aside. The inventory lists it,
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
  extension's, `head_sampling`, `sampling_rules`, `trace_rate_limit` and `debug`
  (`sampling_ratio`) are set aside, with a WARNING. A sampler set through
  `otel.traces.sampler` or a sampler customizer is still the one the extension wraps.

All of this is decided before the client starts, so the extension never declares these controls
([the host](sdk-settings.md#the-host)). A policy that overrides one is refused, FAILED, and the
error names the control and field (`signal_exports.traces: this SDK cannot apply the requested
override`). A saved policy that overrides one is not used at start, and the SDK starts with its
own settings. The SDK's builders don't expose their processors and readers, so the
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
  The description also names the policy format the extension reads
  (`logfire.opamp.config_format`) and the controls this agent carries out
  (`logfire.opamp.supported_controls`).
- **Takes settings** from the Logfire SDK policy, the remote config's only file
  (`logfire-sdk-policy.json`). Each control is `{"mode": "local"}`, the agent's own value, or an
  override:

  | Control | How |
  |---|---|
  | `head_sampling` | a sampler in place of the agent's (`otel.traces.sampler`, whatever it names). A span whose parent was sampled, here or by the caller, is sampled at any ratio, 0 included. Local: the agent's own sampler. |
  | `signal_exports` (`traces`, `metrics`, `logs`) | the agent's exporters, wrapped. `enabled: false` drops what would be exported. It is lost, not held, and spans are still made. A pull reader (`otel.metrics.exporter=prometheus`) is wrapped too. With the metrics export off, a scrape gets nothing. |
  | `log_levels` (`level`) | the same log exporters, at once. A record below that severity is dropped at export, one at it or above is exported, and one with no severity (SeverityNumber 0) is always exported. Local: every severity. |
  | `log_levels` (`scopes`), `attribute_redaction`, `sampling_rules`, `trace_rate_limit`, `log_rate_limit`, `debug` | at once. See [the settings that match telemetry](#the-settings-that-match-telemetry). |
  | `instrumentations` (`disabled`) | at the next start, `otel.instrumentation.<name>.enabled=false`. Names are the agent's: `jdbc`, `spring-webmvc` (every version) or `spring-webmvc-6.0` (one), `java-http-client`, and so on. `_` reads as `-`. |
  | `instrumentations` (`all_disabled`) | at the next start, `otel.instrumentation.common.default-enabled=false`. The application's own spans, metrics and logs (through the OpenTelemetry API) stop too. |
  | `app_settings` | at once, to the application ([below](#the-applications-own-settings)). |

  A policy that overrides a control this agent does not carry out is refused whole (FAILED), and
  the error names the control. A field the policy format does not know is refused too.
- **Doesn't trace itself.** The OpAMP client runs with instrumentation suppressed (its own
  HTTP client runs every task that way), so its requests never show up as the application's spans.
- **Answers** each remote config APPLIED or FAILED, and reports the policy in force as its
  effective config, one `logfire-sdk-policy.json` file ([sdk-settings.md](sdk-settings.md#the-answer)).
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
its gate holds, are not declared. The agent logs a warning, and a policy that overrides them is
refused. What each means is in
[sdk-settings.md](sdk-settings.md#settings-that-match-telemetry). Here is where the agent carries
it out, and what it sees. Each example below shows the
controls a policy (`logfire-sdk-policy/v1`) overrides. Every control left out of an example is
local.

### Sampling rules

```json
{
  "head_sampling": {"mode": "override", "ratio": 0.1},
  "sampling_rules": {"mode": "override", "rules": [
    {"rate": 0, "kind": "server", "path": "/health*"},
    {"rate": 0, "kind": "client", "attributes": {"url.full": "*/health*"}},
    {"rate": 1, "name": "checkout *"}
  ]}
}
```

- In the extension's sampler, which is in place of the agent's (`LiveSampler`). For a new trace
  the first rule its root span matches decides the rate, applied by the SDK's trace-ID ratio
  sampler; then `head_sampling`; then the agent's own sampler. A span with a parent follows it.
- A rule sees what the agent's instrumentation sets when it starts the span. HTTP server spans
  start with `url.path`, `http.request.method` and the kind `server`, and are named `GET`:
  `http.route` is mostly added later, so match `path`, not the route or the name. HTTP client
  spans start with `url.full`, `server.address` and `server.port`, so match those under
  `attributes`. Spans the application starts itself have their own name.
- The sampler `otel.traces.sampler` names (the agent's, or another extension's) is what the
  rules and the ratio fall back to. It decides every span while both controls are local. A
  sampler another extension sets in place of the extension's sets the sampling controls aside
  ([what another extension adds](#what-another-extension-adds)).

### Log levels per scope

```json
{"log_levels": {"mode": "override", "level": "warn",
                "scopes": {"com.acme.db*": "debug", "org.apache.kafka*": "error", "noisy.lib": "off"}}}
```

- At the log gate, beside `level`. The scope is the logger's name: the agent's
  logback, log4j and java.util.logging bridges name the scope after the logger, so
  `com.acme.db*` is every logger under `com.acme.db`.
- The logging framework's own level still decides what is logged at all: a `debug` here can't
  export what logback was told to drop. It only drops more, or lets through what
  `level` would have dropped.

### Redaction

```json
{"attribute_redaction": {"mode": "override",
                         "drop": ["http.request.header.authorization", "http.request.header.cookie"],
                         "mask": ["*password*", "*token*", "user.email"]}}
```

- At the span and log gates: span attributes, span event attributes (an exception event stays
  one), span link attributes and log record attributes. Exported spans and records are new
  objects over the application's, so another exporter, or a span processor of another extension,
  sees the original. A dropped attribute isn't counted as dropped by the SDK's limits.
- Not metrics, resources, scopes, span names or log bodies (sdk-settings.md says why).

### Rate caps

```json
{"head_sampling": {"mode": "override", "ratio": 1},
 "trace_rate_limit": {"mode": "override", "traces_per_second": 50},
 "log_rate_limit": {"mode": "override", "records_per_second": 200}}
```

- Traces: in the sampler, after the rules and the rate. A sampled root span, or a span whose
  remote parent was sampled, takes a token; a child of a span sampled in this JVM never does, so
  a trace is whole or not there at all.
- Logs: at the log gate, after the levels: of each batch, the first records the bucket allows.
  The agent's batch delay is 1 s (`otel.blrp.schedule.delay`); a longer one makes the cap stricter.

### Debug mode

```json
{"head_sampling": {"mode": "override", "ratio": 0.05},
 "log_levels": {"mode": "override", "level": "warn"},
 "debug": {"mode": "override", "until": "2026-10-14T15:30:00Z", "sampling_ratio": 1, "log_level": "debug"}}
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

The server may send the application settings of its own in the policy's `app_settings` control
(a feature flag, a verbosity, a limit), which the extension hands on rather than reads
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
put in place, the extension says so (a WARNING) and every read is its default. The extension then
does not carry out `app_settings`, so a policy that overrides it is refused (FAILED), not answered
APPLIED for nothing.

The extension carries out `app_settings` (it declares `custom` in `Host.live`) when the bridge is
in place. The values go in force
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
  for it, so the names are the ones the `instrumentations` control takes: `spring-webmvc` and
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
  metric reader serves nothing while the metrics export is off. Under declarative configuration
  the SDK is built from a configuration file through the extension's hooks, with the file's
  sampler and exporters inside the controls, the UID in the resource, and the next-start
  settings in the distribution.
- The application's own settings (`AppSettingsTest`, `AppSettingsExtensionTest`): reads before
  any config, without the agent and once a key is left out; `custom.json`'s `typed` vectors and
  their edges; every `custom.json` case through the extension's own `Decide`, and the hosts that
  do not carry it out, which refuse it; a listener told once per change and never for the same map (sent again,
  another way, beside another setting, at the end of debug mode), in order, one at a time; an
  exception caught and logged with the others still told; unsubscribing, also from inside a
  listener; readers and subscribers on other threads, and documents applied from several threads
  going in whole; a FAILED config changing nothing; restarts with and without a state directory;
  a failed setup putting back what was in force; several class loaders on one bridge, one that is
  gone no longer held; and the bridge's jar loaded by a class loader that sees the JDK alone.
- What another extension adds (`OutsideTest`), on an SDK built through another extension's
  customizers and then the extension's: a span processor, a log record processor and a metric
  reader of its own each set aside their signal's controls, a policy that overrides one refused,
  and are listed ungated; a span processor that only starts spans, and another extension's wrapper
  around the agent's own processors, set nothing aside; a sampler set in place of the extension's
  sets the sampler's controls aside, while one set through the sampler customizer is still wrapped.
- The inventory (`InventoryTest`) of an SDK built through the extension's hooks: the
  names the `instrumentations` control takes, versions where embedded, each exporter as configured
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
  1. The server sends a sampling ratio: APPLIED, and spans arrive.
  2. It switches the traces export off: APPLIED, and no span is exported while it holds.
  3. It sends a ratio of 7: FAILED, naming `head_sampling`, and nothing changes.
  4. It sends a ratio of 1 with the traces export local: APPLIED, and spans come again.
  5. It overrides `instrumentations`: APPLIED, and the instrumentation stays on until the
     restart.
  6. After the restart: the same instance UID (16 bytes), the effective config reported, and the
     HTTP instrumentation off, so no spans although the program makes requests while the agent is
     connected.

  7. The settings that match telemetry, one after the other, each checked in what the agent
     exports (through a recording proxy on its way to the reference sink): a rule of rate 0 for
     the program's `/health` checks; the scope `app.noisy` at `off`; a dropped and a masked attribute; and caps
     of one trace and one log record a second.
  8. Debug mode with `head_sampling` at 0: spans until `until`, none after, and the effective
     config reported at the end, the server sending nothing.
  9. The proxy refuses trace exports for a minute: the agent sends its health once they fail, and
     again once they succeed.

  Then `check_truth`, once for each setup a second extension (`examples/javaagent/Setups.java`)
  makes, named in `EXAMPLE_SETUP`. The server offers one policy per control. A control
  is set aside when the policy is FAILED as an override the extension can't carry out, or APPLIED
  with that override reported `local` in the effective config. Every other override must show in
  the telemetry. `extension-processor` adds a span processor, a log record processor and a metric
  reader exporting to the same sink: their signals' gate controls are set aside, and the
  sampler's are carried out. `extension-sampler` sets a sampler that samples every trace:
  `head_sampling`, `sampling_rules` and `trace_rate_limit` are set aside, and the gates' controls
  are carried out.

  Then `check_app_settings` ([app-settings.md](app-settings.md#how-it-is-checked)): the program
  reads `example.feature` with `AppSettings`, which the server turns on and off; a bad map is
  FAILED and changes nothing, the same map again tells nothing, and the next start has the saved
  value from the start.

  `--local` builds and runs with this host's Maven and JDK in place of Docker (`MAVEN_ARGS` is
  passed to Maven). Runs 1 and 2 end with `agent_disconnect`. Each check that nothing arrives
  also checks that the program made requests to trace in that window, so it can't pass for want of them. Each runner
  keeps its builds, logs and state in `target/integrations/<runner>`.
