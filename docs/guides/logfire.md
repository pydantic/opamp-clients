# Guide: the Logfire SDKs and Logfire under an OpAMP server

This guide is for an application that sends its telemetry to [Logfire](https://logfire.pydantic.dev/docs/)
and that should take its settings from an OpAMP server: sampling, what to send, log levels,
redaction and scrubbing, the [Logfire SDK policy](../sdk-settings.md). There are two ways in.

| The application uses | How it joins the server | Languages |
|---|---|---|
| the Logfire SDK | `pydantic_opamp_otel.configure_logfire()` in place of `logfire.configure()` | Python |
| an OpenTelemetry SDK, exporting to Logfire over OTLP | this repository's integration for the language | Python, Go, Node.js, Java, .NET |

The Logfire SDKs for JavaScript and Rust have no integration yet: [below](#the-other-logfire-sdks).

In Logfire the OpAMP server is Fleet, in each project: [Logfire Fleet](#logfire-fleet) says how
to connect an application, a host or a Kubernetes cluster to it.

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

## Logfire Fleet

Fleet is the OpAMP server in each Logfire project. It lists every application, host and cluster
connected to the project, with its health and what it runs, and sends each its policy: an SDK
policy to applications, collector YAML to hosts and clusters. Each thing you connect is an
installation; each process of it is an instance.

### An application

1. On the project's Fleet page, choose **Connect an application**. Name it, optionally give an
   environment, and pick the **SDK policy to apply**: a new policy starts from the SDK's own
   settings, and **Just monitor this application** connects it with no policy.
2. Choose how long the connection key lasts (up to a year): the application keeps it, and Fleet
   doesn't renew it. **Create connection** shows the OpAMP endpoint and the runtime key. The key
   is shown once: store it in your secret manager as `LOGFIRE_OPAMP_TOKEN`.
3. Start the application with its integration (above), the endpoint and the key:

   ```bash
   OTEL_OPAMP_ENDPOINT=<the OpAMP endpoint Fleet shows>
   OTEL_OPAMP_HEADERS="Authorization=Bearer%20${LOGFIRE_OPAMP_TOKEN}"
   OTEL_OPAMP_STATE_DIR=/var/lib/checkout/opamp
   ```

   Give each process its own state directory, so its identity and the key Fleet issues it
   survive restarts. The telemetry still needs its write token ([above](#where-the-telemetry-goes)).
4. The dialog moves from **Waiting for connection** to **Connected** when the application
   reports. Check Live view to see its telemetry arrive.

Edit the policy in Fleet and publish it: every connected instance gets it, and answers APPLIED or
FAILED ([troubleshooting.md](../troubleshooting.md#a-policy-is-failed)).

### A host

1. Choose **Connect host**. Pick the policy, which says what the host's collector collects, and
   optionally a host name (left blank, Fleet uses the host's own).
2. **Create enrollment key**, then choose the host's operating system. Fleet shows one command,
   with the enrollment key in it:

   ```bash
   uvx logfire-cli --beta --base-url <...> --org <org> collector install \
     --project <project> --enrollment-token <enrollment key> --yes
   ```

   Run it on the host (an administrator PowerShell on Windows; `uvx` comes with
   [uv](https://docs.astral.sh/uv/getting-started/installation/)). It installs the Supervisor and
   its collector as a service and enrolls the host. No Logfire sign-in is needed on the host.
3. The host shows up in Fleet. Its collector runs the policy, within what the host allows
   ([what the server can do to this host](../../go/opampsupervisor/packaging/README.md#what-the-server-can-do-to-this-host)).

The enrollment key expires on the date the dialog shows, and can be revoked in Fleet's settings
to stop new enrollments. Hosts already enrolled have keys of their own, which they renew.

### A Kubernetes cluster

Fleet connects to a cluster through the OpenTelemetry Operator's OpAMP bridge, upstream's. The
bridge applies the Collector resources in the cluster's Fleet policy and reports their health.

1. Install cert-manager and the
   [OpenTelemetry Operator](https://github.com/open-telemetry/opentelemetry-operator). On GKE
   Autopilot, install cert-manager with Helm and its leader election in its own namespace
   (`--set global.leaderElection.namespace=cert-manager`).
2. Choose **Connect Kubernetes**. Name the cluster, give the namespaces its collectors run in,
   pick a policy (or **Just monitor this cluster**), and what to collect. On GKE Autopilot,
   leave **Container logs** off: Autopilot refuses the host path it reads.
3. Fleet shows the runtime key and the commands that store it as the `logfire-opamp` Secret and
   apply the bridge. Save the key: it is shown once.
4. Publish the policy. The bridge creates the collectors, and Fleet shows the installation
   connected with its workloads.

Edit the collectors in Fleet and publish: a resource the policy leaves out is deleted. Each
collector pod counts toward the organization's collector allowance; the bridge doesn't.

### A collector you already run

A collector configured by your own tooling can report to Fleet without Fleet changing it: give it
an `opamp` extension (upstream's, or [this repository's](../collector.md#a-collector-with-the-extension))
with Fleet's endpoint and a runtime key as its bearer token. Its YAML stays yours.

### Replacing a key

A host's Supervisor moves to a new key on its own before its key expires. The rest keep the
key they were given: an application's lasts as long as you chose when you connected it (up to a
year), and the Operator's bridge's and upstream's `opamp` extension's last 90 days. Fleet warns
on the key's page in the last 14 days. To replace one:

1. Open the key in Fleet and choose **Issue a replacement key** (for an application, choose how
   long it lasts). Keep the new key.
2. Replace the key where the old one was (for an application, `OTEL_OPAMP_HEADERS`; for the
   bridge, the `logfire-opamp` Secret), and restart. A restarted bridge is refused for about 30
   seconds, until its old connection drops.
3. Once everything uses the new key, revoke the old one.

### Removing an installation

Deleting an installation in Fleet revokes its keys, and any write token issued with it, and
closes its connections. The telemetry it already sent stays in the project. A disconnected
instance's record can be deleted on its own.

## The other Logfire SDKs

The Logfire SDK for JavaScript (`@pydantic/logfire-node`) starts a NodeSDK of its own, and so
does this repository's Node.js integration; a process runs one. For now, start a Node.js
application with the integration's register and export to Logfire over OTLP, as above. The
Logfire SDK for Rust has no integration here.
