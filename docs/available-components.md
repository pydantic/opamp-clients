# Available components

What the agent can run: a collector's receivers, processors and exporters, say, or the
instrumentations an application has loaded. The server keeps a catalogue of component sets and
asks for a set only when it hasn't seen it.

## What the spec says

- The agent reports `available_components` if and only if it declares
  `ReportsAvailableComponents` (OPAMP-COMP-01), always with a hash (OPAMP-COMP-02) that is the
  same for any agent with the same set (OPAMP-COMP-03).
- The first message carries the hash alone (OPAMP-COMP-04). When the server sets
  `ReportAvailableComponents` in a ServerToAgent, the agent sends the components in full.

## What the client does

- The application gives its components once at start (or later, when they change) and leaves the
  hash empty. The core computes it: FNV-1a over a canonical encoding, with ids sorted at every
  level, so the same set always has the same hash whatever order a map holds it in. An
  application that has a hash of its own can give it instead.
- The client declares `ReportsAvailableComponents` once there are components.
- A report carries the hash alone, like any other status field: in the first message on a
  connection, in full reports, and when the components change.
- Asked (`ReportAvailableComponents`), the client sends them in full in the next message, once,
  even if unchanged and even if a server rejected them before. Components too large to send in
  full under `max_send_bytes` go as the hash, and the application hears `StatusTooLarge` with
  `available_components` (RULE-CLIENT-09).

## What the server does

- It asks an agent for its set when it doesn't hold the set that agent itself sent under the hash
  it reports. A hash is the agent's own word, and the client's is no digest (FNV-1a can be made to
  collide), so a set another agent sent under the same hash, forged or colliding, never stands for
  this agent's: each agent is asked for its own set once, and not again while the server holds it.
- It keeps each set once, by its own SHA-256 of the set's encoding (the hash left out), however
  many agents sent it. The inventory lists each agent on the set it sent itself; until it has
  sent one, on its hash alone. Two different sets under one hash are two entries, each with the
  agents that sent it, and the server logs `components_conflict`.

## In each client

| Client | At start | Later |
|---|---|---|
| Go | `Status.AvailableComponents` | `Client.SetAvailableComponents` |
| Node.js | `status.availableComponents` | `client.setAvailableComponents` |
| Python | `Status.available_components` | `client.set_available_components` |
| Java | `OpampClient.Status` (the fifth component) | `client.setAvailableComponents` |
| .NET | `OpAmpStatus.AvailableComponents` | `client.SetAvailableComponents` |

## How it is checked

- The core is checked on its own before it is released: the components as a status field on
  every random run (the hash alone unless asked, in full exactly once when asked, cut to the hash
  when too big, a request lifting a rejection), and the hash (the same set in any order, any
  difference changes it).
- The conformance scenario `available-components`: the server asks once, and every client over
  both transports sends the hash first, the components in full after the request, and the hash
  again after that.

## What the SDK integrations report

Each OpenTelemetry integration (Python, Node.js, Go, .NET, Java) reports its **inventory**
as its available components: what it runs, so a server can see across its fleet which services
are on an old SDK, which can run an instrumentation, or which export through something the
server can't switch off. It is reporting, always on, with no setting.

The integration lists what it built; the SDK settings layer
([sdk-settings.md](sdk-settings.md#the-layer), its `Components` call) adds the settings it
carries out and builds the one shape every language sends:

| Component | What it holds |
|---|---|
| `sdk` | metadata `integration.name` and `integration.version` (our package), `telemetry.sdk.language` and `telemetry.sdk.version` (the OpenTelemetry SDK's) |
| `instrumentations` | one sub-component per instrumentation that applies (one whose library the application has: see each integration's page), switched off or not, under the name that the policy's `instrumentations` control takes, with metadata `version` when known, and `scope` when known: the OpenTelemetry instrumentation scope its telemetry carries (its tracer's, meter's or logger's name), so a server can tell which instrumentations send anything |
| `exporters` | one sub-component per signal (`traces`, `metrics`, `logs`) with an exporter, and under it one per exporter by its identity: its name as `OTEL_*_EXPORTER` names it (`otlp`, `console`, `prometheus`, `zipkin`) or its type, and `@host:port` where it sends somewhere known (`otlp@api.honeycomb.io:443`). Metadata: the boolean `gated`, whether the policy governs it (the integration's gate is in front of it: signal export, levels, caps, redaction), and `destination` (`scheme://host:port`) with a destination. Never the path, query or credentials. One not gated is ungoverned: reported, never a reason to refuse a policy ([sdk-settings.md](sdk-settings.md#where-each-setting-applies)). A global provider of the application's own (Node.js, Go, .NET) is listed as `app_provider`, not gated |
| `settings` | one sub-component per setting it carries out, by the setting's name behind the policy's controls ([sdk-settings.md](sdk-settings.md#the-controls)), with metadata `applies`: `at once` or `next start`. A setting it cannot carry out in this process is left out |

The exporters come from the layer too, the same in every language: its `Exporters` call takes
each exporter the integration found on its SDK's providers as a fact (its signal, what it is known
as, whether the gate is in front of it, its endpoint when it holds one, and whether it was built
from the `OTEL_*` variables) and the process's `OTEL_EXPORTER_*` variables, and decides the name it
is listed under (`OtlpGrpcSpanExporter`, `otlp_proto_http` and `otlp` are all `otlp`), where it
sends (its own endpoint, else what the variables say, else the default for its protocol), which
signals are governed, and one warning for each ungoverned exporter, which the integration logs
once. Walking the providers stays each language's, since that is its SDK's object model; it runs
at start and when the providers change, never per span. Checked by
`testdata/sdk-settings/exporters.json`, through each language's wrapper.

For example, a Python service with Flask and a Prometheus reader:

```json
{
  "sdk": {"integration.name": "opamp-otel", "integration.version": "0.4.0",
          "telemetry.sdk.language": "python", "telemetry.sdk.version": "1.45.1"},
  "instrumentations": {"flask": {"version": "0.56b0", "scope": "opentelemetry.instrumentation.flask"},
                       "requests": {"version": "0.56b0", "scope": "opentelemetry.instrumentation.requests"}},
  "exporters": {"traces": {"otlp@collector:4318": {"gated": true, "destination": "http://collector:4318"}},
                "metrics": {"otlp@collector:4318": {"gated": true, "destination": "http://collector:4318"},
                            "prometheus": {"gated": false}}},
  "settings": {"sampling_rate": {"applies": "at once"},
               "deactivate_instrumentations": {"applies": "next start"}, "...": {}}
}
```

- The same inventory in any order gives the same components, and so the same hash: services that
  run the same thing share one set, which a server can store once.
- A name listed twice is one sub-component: an instrumentation's versions joined, sorted, by
  `,` (a version with a `,` in it is left out, since it would read as several), and its scopes
  the same way; an exporter `gated` only if every exporter of that identity is.
- Which instrumentations each integration lists: those that apply, never every one a bundle
  ships. Python, the instrumentors whose library is installed (those `opentelemetry-instrument`
  would run); Node.js, each once it has patched a module the application loaded; Go, each scope
  once it is taken; the Java agent, each module whose library a class loader of the application's
  has; the .NET plugin, each once its library's assembly is loaded; `WithOpAmp()`, those the
  application added in code. One that applies later is added, and the inventory sent again.
- Where each integration's scopes come from: Python, the module its entry point names
  (`opentelemetry.instrumentation.requests`, the name upstream's instrumentations get their
  tracers by); Node.js, the instrumentation's `instrumentationName`
  (`@opentelemetry/instrumentation-http`); Go, the whole scope name it was taken under
  (`go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`); the Java agent,
  `io.opentelemetry.<name>` for the name whose version the module embeds (none embedded: none);
  .NET, a table of the ActivitySource or Meter each instrumentation's telemetry comes from
  (`System.Net.Http`, `Microsoft.AspNetCore`; none for a logging bridge, whose logs are under the
  application's categories), and `WithOpAmp()` the instrumentation's assembly name for one the
  table doesn't have.
- Limits, so a set stays small: names, versions and scopes of up to 128 bytes (a longer name is
  left out, a longer version or scope cut), 16 versions and 16 scopes an instrumentation (past
  them, the first, sorted), 1024
  instrumentations and 64 exporters a signal (past them, the first by name).
- The integration calls `Components` once its SDK is built, and again when what it lists
  changes, and gives the answer to its client (`set_available_components`). The client then sends
  the hash, and the components in full when the server asks.
- Checked by the shared vectors, `testdata/sdk-settings/components.json` (the layer
  runs them, each language's wrapper through its own), and
  property tests: any order is the same bytes, and nothing is listed that wasn't given, or that the
  host doesn't carry out.
