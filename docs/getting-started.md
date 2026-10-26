# Getting started

This page takes you from nothing to an application, a host or a cluster that an OpAMP server
sees and configures. You need an OpAMP server: [Logfire's Fleet](guides/logfire.md#logfire-fleet),
or any server that speaks [OpAMP](https://opentelemetry.io/docs/specs/opamp/). The examples use
`https://opamp.example.com/v1/opamp`; use your server's endpoint.

## What to connect

| You want the server to see and configure | Use | Guide |
|---|---|---|
| An application instrumented with OpenTelemetry (sampling, what it sends, log levels, redaction, scrubbing) | this repository's integration for its language | [below](#an-application) |
| An application on the Logfire SDK for Python | `configure_logfire()` in place of `logfire.configure()` | [guides/logfire.md](guides/logfire.md#the-logfire-sdk-for-python) |
| A host or VM: a collector the server configures | the Supervisor, from its installer | [below](#a-host) |
| A collector you already run, configured by your own tooling | the collector's `opamp` extension, reporting only | [collector.md](collector.md#a-collector-with-the-extension) |
| A Kubernetes cluster | the OpenTelemetry Operator's OpAMP bridge (upstream's) | [guides/logfire.md](guides/logfire.md#a-kubernetes-cluster) |

Check [supported-versions.md](supported-versions.md) for the runtimes and platforms each one
needs.

## An application

### 1. Get an endpoint and a key

The server gives you two things: its OpAMP endpoint, and a key the agent sends as a bearer token.
In Logfire, **Fleet → Connect an application** shows both
([guides/logfire.md](guides/logfire.md#an-application)). The key is shown once: store it in your
secret manager.

The key that connects an application is not a telemetry write token. Telemetry goes where the
SDK sends it, with its own credentials (for Logfire, a write token), and never through the OpAMP
server.

### 2. Install the integration

| Language | Integration |
|---|---|
| Python | `pydantic-opamp-otel`, under `opentelemetry-instrument` ([python-otel.md](python-otel.md)) |
| Go | `opampotel.Setup` in place of the SDK's setup ([go-otel.md](go-otel.md)) |
| Node.js | `node --import @pydantic/opamp-otel/register` ([node-otel.md](node-otel.md)) |
| Java | the extension jar beside the OpenTelemetry Java agent ([javaagent.md](javaagent.md)) |
| .NET | the plugin for automatic instrumentation, or `WithOpAmp()` in code ([dotnet-otel.md](dotnet-otel.md)) |

Until the packages are on PyPI, npm, Maven Central and NuGet, each page says how to install from
this repository.

### 3. Point it at the server

Every integration reads the same variables:

```bash
OTEL_SERVICE_NAME=checkout
OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp
OTEL_OPAMP_HEADERS="Authorization=Bearer%20<key>"
OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp
```

- `OTEL_OPAMP_ENDPOINT`: `https://` polls, `wss://` keeps a connection open. Give the scheme:
  an endpoint without one is refused (taken as `http://`, the key would go in the clear), and the
  SDK runs unmanaged.
- `OTEL_OPAMP_HEADERS`: as `OTEL_EXPORTER_OTLP_HEADERS`, values percent-encoded (`%20` for the
  space after `Bearer`).
- `OTEL_OPAMP_STATE_DIR`: where the agent keeps its identity and the key the server issues it,
  readable by the application's user only. Give each process its own directory, on a disk that
  outlives a restart. Without it the integration keeps one of the user's own
  ([default-state-dir.md](default-state-dir.md)).

Then start the application as you do today, with its usual exporter settings.

### 4. Check it connected

The integration logs one line when it first reaches the server:

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

and a warning, with what to check, when it can't. Where each language logs it, and what each
warning means, is in [troubleshooting.md](troubleshooting.md). The server lists the instance,
named by `service.name`, with its health and what it runs.

### 5. Send it a policy

The server configures an SDK with a Logfire SDK policy: a JSON document where every control
is `local` (the application's own value) unless the policy overrides it.
[examples/settings](../examples/settings/README.md) has a policy for each common job: sample a
quarter of traces, quiet noisy loggers, redact secrets, cap the volume, debug for half an hour.
[sdk-settings.md](sdk-settings.md) has every control.

The SDK answers each policy APPLIED, or FAILED with the reason, and changes nothing when it
fails. An SDK that can't carry out a control the policy overrides refuses the whole policy, and
names the control.

### 6. Keys after the first start

The key you gave can be one many instances share. A server that issues keys (Logfire's does)
gives each instance a key of its own on its first connection. The SDK checks the new key works,
keeps it in the state directory, and uses it from then on, across restarts. Before that key
expires, the server offers a new one, and the SDK moves to it the same way. Nothing for you to
do, as long as the state directory survives restarts
([connection-settings.md](connection-settings.md)).

### 7. Taking it off the server

Set `OTEL_OPAMP_DISABLED=true` and restart: no OpAMP client starts, and the SDK runs on its own
settings, whatever the code or other variables say. Use it in an incident, with no deploy.
Removing `OTEL_OPAMP_ENDPOINT` does the same.

To stop an instance connecting at all, revoke its key on the server. In Logfire, deleting the
installation in Fleet revokes its keys and closes its connections.

## A host

The Supervisor runs a collector on the host and lets the server configure it, within a policy
the host sets. The server can't widen that policy: by default it may use a short list of
components, send telemetry only to its own host, and read no log files
([what the server can do to this host](../go/opampsupervisor/packaging/README.md#what-the-server-can-do-to-this-host)).

- **Logfire:** **Fleet → Connect host** gives a one-line install command for Linux, macOS or
  Windows, which installs the Supervisor and its collector as a service and enrolls the host
  ([guides/logfire.md](guides/logfire.md#a-host)).
- **Any server:** install the Supervisor from its
  [release](https://github.com/pydantic/opamp-clients/releases) (deb, rpm, `.pkg` or `.msi`),
  then enroll the host with your server's endpoint and enrollment key:

  ```sh
  echo "$ENROLLMENT_TOKEN" | sudo pydantic-opamp-enroll --endpoint wss://opamp.example.com/v1/opamp
  ```

  [The packages' README](../go/opampsupervisor/packaging/README.md) covers each platform, and
  the options that widen the policy (`--exporter-endpoint`, `--log-path`).

Like an SDK, the Supervisor takes a key of its own and renews it, so the enrollment key is only
needed once. Install each host separately, and keep the Supervisor's state directory out of
machine images: it holds the host's identity and key.

## Next

- [troubleshooting.md](troubleshooting.md): when something doesn't connect or a policy fails.
- [supported-versions.md](supported-versions.md): runtimes, OpenTelemetry versions and
  platforms.
- [how-it-works.md](how-it-works.md): how the clients and the core fit together.
