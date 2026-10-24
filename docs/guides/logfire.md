# Guide: the Logfire SDKs and Logfire under an OpAMP server

This guide is for an application that sends its telemetry to [Logfire](https://logfire.pydantic.dev/docs/)
and that should take its settings from an OpAMP server: sampling, what to send, log levels,
redaction and scrubbing, the [Logfire SDK policy](../sdk-settings.md). There are two ways in.

| The application uses | How it joins the server | Languages |
|---|---|---|
| the Logfire SDK | `pydantic_opamp_otel.configure_logfire()` in place of `logfire.configure()` | Python |
| an OpenTelemetry SDK, exporting to Logfire over OTLP | this repository's integration for the language | Python, Go, Node.js, Java, .NET |

The Logfire SDKs for JavaScript and Rust have no integration yet: [below](#the-other-logfire-sdks).

## The Logfire SDK for Python

Install the package with its `logfire` extra, and call `configure_logfire()` with the arguments
the application gives `logfire.configure()` today:

```python
import logfire
import pydantic_opamp_otel

pydantic_opamp_otel.configure_logfire(service_name="checkout", console=False)
logfire.instrument_httpx()  # the Logfire SDK's instrumentations, as before
```

```bash
LOGFIRE_TOKEN=<write token> \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
python app.py
```

The Logfire SDK still sends to Logfire with its own token, region and options. The package puts
its sampler in front of the Logfire SDK's head sampling, and its gates in front of the exporters
to Logfire. Without `OTEL_OPAMP_ENDPOINT` (or `endpoint=`), `configure_logfire()` is
`logfire.configure()`.

Pass `console=False` (or set `LOGFIRE_CONSOLE=false`). The console shows every span and log as
it is, past the gates, so with it on the server can still sample but a policy that overrides
`signal_exports`, the log controls, `attribute_redaction` or `scrubbing` is refused. The same
goes for span processors, log record processors or metric readers given in the arguments, and for
the OTLP exporters the Logfire SDK adds for `OTEL_EXPORTER_OTLP_*ENDPOINT`.

[python-otel.md](../python-otel.md#the-logfire-sdk-configure_logfire) has the whole reference:
which Logfire SDK versions, what it does differently from `logfire.configure()`, and what is
set aside when.

## An OpenTelemetry SDK, exporting to Logfire

Every integration here works with any OTLP destination, and Logfire is one. The application keeps
its OpenTelemetry SDK and instrumentations, and the integration's guide shows how to start it
under the server:

| Language | Integration |
|---|---|
| Python | [python-otel.md](../python-otel.md) (`opentelemetry-instrument`) |
| Go | [go-otel.md](../go-otel.md) |
| Node.js | [node-otel.md](../node-otel.md) (`node --import @pydantic/opamp-otel/register`) |
| Java | [javaagent.md](../javaagent.md) (the Java agent and the extension) |
| .NET | [dotnet-otel.md](../dotnet-otel.md) (automatic instrumentation and the plugin) |

### Where the telemetry goes

Give the SDK Logfire's OTLP endpoint for your project's region and a write token, over HTTP:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=https://logfire-us.pydantic.dev   # or https://logfire-eu.pydantic.dev
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS="Authorization=<write token>"
```

Set the protocol: the Python integration defaults to OTLP over gRPC, as upstream's distro does,
and the protocol named once holds for every language. The OpAMP server is given apart, with `OTEL_OPAMP_ENDPOINT` and `OTEL_OPAMP_HEADERS`,
and the telemetry never goes through it.

For example, a Java service:

```bash
OTEL_SERVICE_NAME=checkout \
OTEL_EXPORTER_OTLP_ENDPOINT=https://logfire-us.pydantic.dev \
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf \
OTEL_EXPORTER_OTLP_HEADERS="Authorization=<write token>" \
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp \
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<enrollment key>" \
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp \
java -javaagent:opentelemetry-javaagent.jar \
     -Dotel.javaagent.extensions=opamp-javaagent-extension.jar \
     -jar checkout.jar
```

An application on one of Elastic's distributions can do the same: [elastic.md](elastic.md).

## Sending it a policy

The server sends each SDK a Logfire SDK policy, `logfire-sdk-policy.json`. Every control is
`local` (the SDK's own value) unless the policy overrides it, so a policy names only what it
changes. [examples/settings](../../examples/settings/README.md) has one for each common use:
sample a quarter of traces, quiet noisy loggers, redact secrets, cap the volume, debug for half
an hour, and back to the defaults. [sdk-settings.md](../sdk-settings.md) has every control and
what an SDK answers.

An SDK that can't carry out a control the policy overrides refuses the whole policy: it answers
FAILED, names the control, and changes nothing. Each integration's page says which controls it
carries out, and when it sets one aside.

## The other Logfire SDKs

The Logfire SDK for JavaScript (`@pydantic/logfire-node`) starts a NodeSDK of its own, and so
does this repository's Node.js integration; a process runs one. For now, start a Node.js
application with the integration's register and export to Logfire over OTLP, as above. The
Logfire SDK for Rust has no integration here.
