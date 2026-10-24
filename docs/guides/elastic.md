# Guide: Elastic's OpenTelemetry distributions under an OpAMP server

Elastic ships its own builds of the OpenTelemetry SDKs, the EDOT distributions. This guide puts an
application on one of them under an OpAMP server with this repository's integrations, so the
server's [Logfire SDK policy](../sdk-settings.md) samples, gates and redacts its telemetry like any
other SDK's. The telemetry can still go to Elastic, or anywhere else OTLP goes.

| Distribution | Works with | How | Checked with |
|---|---|---|---|
| EDOT Java | [the Java agent extension](../javaagent.md) | the extension, given to EDOT's agent | EDOT Java 1.14.0, every check of the Java agent's |
| EDOT .NET | [the .NET plugin](../dotnet-otel.md) | both plugins, EDOT's first | EDOT .NET 1.6.1, every check of the plugin's |
| EDOT Python | [the Python package](../python-otel.md) | EDOT's instrumentations, this package's distro and configurator | EDOT Python 1.18.0, every check of the package's |
| EDOT Node.js | not yet | EDOT and this package each start a NodeSDK of their own | |

"Every check" is the integration's whole `try.py` run with `--edot`
([How it is checked](#how-it-is-checked)).

## Before you start: one OpAMP client per process

Each EDOT distribution has an OpAMP client of its own, which reads Elastic's central
configuration (`ELASTIC_OTEL_OPAMP_ENDPOINT`). It is off unless that variable is set. Leave it
off: this repository's integration is then the process's only OpAMP client, and the one whose
settings it carries out. Running both makes two agents of one process, each changing the SDK
behind the other's back.

Everything else about the distribution stays as Elastic ships it: its instrumentations, its
resource attributes (`telemetry.distro.name` is `elastic`), its exporters' defaults.

## Where the telemetry goes

The integrations change nothing about where the SDK exports. To send to Elastic, give the SDK
Elastic's OTLP endpoint and an API key, as Elastic documents:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=https://<your deployment>.ingest.<region>.elastic.cloud:443
OTEL_EXPORTER_OTLP_HEADERS="Authorization=ApiKey%20<key>"
```

To send to Logfire instead, see [logfire.md](logfire.md#where-the-telemetry-goes). Either way the
OpAMP server is given apart, with `OTEL_OPAMP_ENDPOINT` and `OTEL_OPAMP_HEADERS`.

## Java

Give EDOT's agent the extension, as you would the upstream agent:

```bash
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
java -javaagent:elastic-otel-javaagent.jar \
     -Dotel.javaagent.extensions=opamp-javaagent-extension.jar \
     -jar checkout.jar
```

`elastic-otel-javaagent.jar` is `co.elastic.otel:elastic-otel-javaagent` from Maven Central;
the extension jar is built as [javaagent.md](../javaagent.md#use-it) says.

Every control is carried out, EDOT's own additions included:

- **EDOT's sampler.** EDOT samples with a sampler of its own by default
  (`experimental_composite_parentbased_traceidratio`). The extension wraps it as it wraps any
  sampler named by `otel.traces.sampler`: `head_sampling` local is EDOT's sampler, an override is
  the policy's ratio.
- **EDOT's span processors.** EDOT adds processors of its own: span stack traces, and its
  profiler integration chained in front of the exporting processor the agent builds. None of
  them exports, and the spans still leave through the extension's gate.
- **EDOT's span logger.** EDOT adds a span logger to every SDK, which writes spans to the agent's
  log once Elastic's central configuration turns debug logging on. With EDOT's OpAMP client off
  nothing can, so the extension leaves it alone. With `ELASTIC_OTEL_OPAMP_ENDPOINT` set, it counts
  as an exporter past the gate. The trace gate's controls (`signal_exports.traces`,
  `attribute_redaction`, `scrubbing`) are then set aside, and the agent logs a WARNING naming it.

EDOT's agent writes its own log, and the extension's lines in it, to standard output.

## .NET

Reference EDOT's package in place of upstream's `OpenTelemetry.AutoInstrumentation`. EDOT
1.6.1 is built on automatic instrumentation 1.17.0, the version the plugin is compiled
against:

```xml
<PackageReference Include="Elastic.OpenTelemetry.AutoInstrumentation" Version="1.6.1" />
<ProjectReference Include="path/to/dotnet/Pydantic.OpAmp.AutoInstrumentation/Pydantic.OpAmp.AutoInstrumentation.csproj" />
```

List both plugins, EDOT's first, and start the application with `_instrument.sh`:

```bash
OTEL_DOTNET_AUTO_PLUGINS="Elastic.OpenTelemetry.AutoInstrumentationPlugin, Elastic.OpenTelemetry.AutoInstrumentation:Pydantic.OpAmp.AutoInstrumentation.Plugin, Pydantic.OpAmp.AutoInstrumentation" \
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
./_instrument.sh dotnet Checkout.dll
```

EDOT's `instrument.sh` sets `OTEL_DOTNET_AUTO_PLUGINS` to EDOT's plugin alone, whatever it was,
and then runs `_instrument.sh`. Started through `instrument.sh`, the application runs without
this plugin, unmanaged. `_instrument.sh` does the rest of what `instrument.sh` does and keeps the
plugins you give it.

## Python

EDOT Python is a distro and a configurator, and so is this package. Only one of each runs:
choose this package's, and keep EDOT's instrumentations, sampler and resource detectors.

```bash
pip install elastic-opentelemetry ./python ./python/otel
edot-bootstrap --action=install     # the instrumentations for what the application uses

OTEL_PYTHON_DISTRO=opamp \
OTEL_PYTHON_CONFIGURATOR=opamp \
OTEL_TRACES_SAMPLER=experimental_composite_parentbased_traceidratio \
OTEL_EXPERIMENTAL_RESOURCE_DETECTORS=process_runtime,os,telemetry_distro,otel \
OTEL_SERVICE_NAME=checkout \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
opentelemetry-instrument python app.py
```

Without `OTEL_PYTHON_DISTRO` and `OTEL_PYTHON_CONFIGURATOR`, which of the two runs is left to the
order Python finds them in. With EDOT's configurator, the instance still joins the fleet, but a
policy that overrides `head_sampling`, `signal_exports.traces` or `signal_exports.logs` is refused
([python-otel.md](../python-otel.md#use-it)).

EDOT's distro also sets some defaults of its own, which this package's distro doesn't. Set the
ones you want yourself:

| Variable | EDOT's default |
|---|---|
| `OTEL_TRACES_SAMPLER` | `experimental_composite_parentbased_traceidratio` (above) |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` |
| `OTEL_EXPERIMENTAL_RESOURCE_DETECTORS` | `process_runtime,os,telemetry_distro,service_instance,containerid`, the cloud detectors, then `otel` |
| `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` | `DELTA` |
| `OTEL_METRICS_EXEMPLAR_FILTER` | `always_off` |
| `OTEL_PYTHON_TRACER_CONFIGURATOR` | `updatable_tracer_configurator`, which EDOT's central configuration drives |

Leave `service_instance` out of the resource detectors: the package gives `service.instance.id`
the agent's instance UID, which the server knows the instance by. Both distros export every
signal over OTLP/gRPC unless told otherwise.

## Node.js

Not yet. `node --import @elastic/opentelemetry-node` starts EDOT's NodeSDK, and
`node --import @pydantic/opamp-otel/register` starts this package's. A process can run only one:
the second one's providers are refused as the global ones. To manage a Node.js application with a
Logfire SDK policy, start it with this package's register. It runs the upstream
auto-instrumentations, which EDOT Node.js also runs, without EDOT's additions.

## How it is checked

The Java agent's, the .NET plugin's and the Python package's `try.py` take `--edot`. That
runs every check of the integration with the EDOT distribution in place: the same server, the same
policies, the same telemetry checks, and each must pass as it does upstream.

```sh
python3 examples/javaagent/try.py --edot
python3 examples/dotnet-otel/try.py --edot
python3 examples/python-otel/try.py --edot
```

[examples/README.md](../../examples/README.md) has what the checks are and what they need.
