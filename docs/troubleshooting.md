# Troubleshooting

What to look at when an application, a host or a cluster doesn't show up on the OpAMP server,
or a policy doesn't do what you expect. Start with the client's own log: every integration says
when it connects, and says once, with what to check, when it can't.

## Where each integration logs

| Integration | Where | INFO lines shown |
|---|---|---|
| Python ([python-otel.md](python-otel.md#is-it-working)) | the `pydantic_opamp_otel` logger; warnings reach stderr | with a handler at INFO |
| Go ([go-otel.md](go-otel.md#is-it-working)) | `slog`'s default logger | at slog's level |
| Node.js ([node-otel.md](node-otel.md#is-it-working)) | OpenTelemetry's `diag` logger; warnings reach stderr | with `OTEL_LOG_LEVEL=info` |
| Java ([javaagent.md](javaagent.md#is-it-working)) | the `java.util.logging` logger `dev.pydantic.opamp.javaagent.Extension` | at INFO |
| .NET ([dotnet-otel.md](dotnet-otel.md#is-it-working)) | standard error; `WithOpAmp()` logs to the application's logger | with `OTEL_LOG_LEVEL=info` |
| Supervisor ([collector.md](collector.md)) | Linux: the journal (`journalctl -u pydantic-opamp-supervisor`); macOS: `/opt/pydantic-opamp/var/log/supervisor.log`; Windows: the Application event log | always |

The first connection logs:

```
OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance <uid>
```

If you see nothing at all, check that the integration is loaded (the package installed, the
agent extension or plugin named) and that `OTEL_OPAMP_ENDPOINT` is set: without it every
integration runs as upstream's, unmanaged, and says nothing.

## It can't reach the server

The warning names the endpoint, what went wrong, and what to check. It comes once per run of
failures; the client keeps trying in the background, and the SDK keeps its current settings
meanwhile. Once it's back you see `connected to ... again`.

| The warning says | Means | Do |
|---|---|---|
| `(HTTP 401)` or `(HTTP 403)`: `the server refused this agent's credentials: check OTEL_OPAMP_HEADERS` | the key is wrong, expired or revoked | Check the header is `Authorization=Bearer%20<key>`, with the space percent-encoded. Get a new key from the server if it was revoked or has expired. |
| `(HTTP 404)`: `nothing serves OpAMP at that path: check OTEL_OPAMP_ENDPOINT` | the host answered, but not at that path | Use the endpoint exactly as the server gives it (it usually ends in `/v1/opamp`). |
| `(connection refused)`, a DNS error, or a timeout: `check OTEL_OPAMP_ENDPOINT and that the server is up` | nothing answered | Check the host and port, and that the network (a firewall, an egress policy) lets the process reach it. Behind a proxy, set `HTTPS_PROXY` ([sdks.md](sdks.md) says how each language reads it). |
| a certificate error | the server's certificate isn't trusted | Behind a TLS-inspecting proxy, give the proxy's CA (each client's TLS options, or the Supervisor's `--ca-file`). |
| at start: `has no scheme, so it is taken as http://` | `opamp.example.com/v1/opamp` was given | Write the scheme: `https://` or `wss://`. Over plain HTTP the key goes in the clear. |

The warning names what you configured: with the endpoint or headers given in code, it says so
instead of naming the variable.

## Connected, but no telemetry arrives

The OpAMP connection and the telemetry are separate: the key that connects the agent doesn't
send telemetry. Check the SDK's own exporter settings (`OTEL_EXPORTER_OTLP_ENDPOINT`, its
headers and protocol). For Logfire, the exporter needs a write token and the region's endpoint
([guides/logfire.md](guides/logfire.md#where-the-telemetry-goes)).

A policy can also stop what is sent: `signal_exports` off for a signal, a log level, or head
sampling at 0. The instance's effective config, on the server, shows the policy in force.

## A policy is FAILED

The SDK answers every policy APPLIED or FAILED, and a FAILED policy changes nothing. The reason
names the control:

| The reason | Means | Do |
|---|---|---|
| `<control>: this SDK cannot apply the requested override` | this integration doesn't carry the control out here, often because something sends telemetry past its gates: a console exporter, an exporter or processor the application added itself, another extension | Remove what bypasses the gates, or leave that control `local` in the policy. Each integration's page says what counts. |
| `instrumentations: takes effect at the next start, which needs somewhere to keep state` | no state directory | Give `OTEL_OPAMP_STATE_DIR` (or drop `none`). |
| a value's problem, such as `head_sampling: ratio must be a number from 0 to 1` | the policy is malformed | Fix the policy. [sdk-settings.md](sdk-settings.md) has every control and its limits. |
| from the Supervisor: `receiver "filelog" reads "/etc/shadow", which ... allowed_log_paths doesn't allow` | the collector config breaks the host's policy | Change the config, or widen the policy on the host ([what the server can do](../go/opampsupervisor/packaging/README.md#what-the-server-can-do-to-this-host)). The server can't widen it. |
| from the Supervisor: a config holding `logfire-sdk-policy.json` | an SDK policy sent to a collector | Send a collector its YAML. SDK policies are for SDKs. |

`instrumentations` takes effect at the next start in Python, Java and the .NET plugin (at once
in Go and Node.js; `WithOpAmp()` refuses it): the policy is APPLIED, and the effective config
shows the change pending until the process restarts.

## The server shows too many instances

Each instance is one process, named by its instance UID, which the state directory keeps.

- **A new instance at every restart:** the state directory doesn't survive restarts (a
  container's own filesystem, an `emptyDir`), or is `none`. Mount a volume for it. Until then,
  disconnected instances pile up; delete them on the server.
- **Two processes as one instance:** they share a state directory. Give each process its own.
  Without `OTEL_OPAMP_STATE_DIR`, each integration already gives each running copy of a program
  its own slot ([default-state-dir.md](default-state-dir.md)).
- **Hosts cloned from one image:** the image holds a Supervisor's state directory. Keep it out of
  images and enroll each host on first boot.

## The core can't be loaded

Python and .NET load the core as a native library. On a platform without one
([supported-versions.md](supported-versions.md#platforms)), or when it can't be loaded, the
integration says so once and the application runs unmanaged, on its own settings. Python says:

```
OpAMP: the core can't be loaded on linux-riscv64; the SDK runs unmanaged: ...
```

The application and its telemetry carry on. Run on a supported platform to have it managed.

## Keys that expire

SDKs and the Supervisor take a key of their own on their first connection and move to a new one
before it expires, as long as their state directory survives restarts. An instance that loses
its state starts over from the key it was configured with: if that one has expired or was
revoked, it can't connect (HTTP 401).

The collector `opamp` extension from upstream's contrib release and the Operator's OpAMP bridge
don't take new keys from the server. Their key works until it expires (in Logfire, 90 days),
and then they disconnect: replace the key before then and restart them
([guides/logfire.md](guides/logfire.md#replacing-a-key)).

## Turning it off

`OTEL_OPAMP_DISABLED=true` and a restart turns OpAMP off in any integration: no client starts,
and the SDK runs on its own settings. One warning says so. `OTEL_SDK_DISABLED=true` turns it off
too.

## Still stuck

Open an issue at https://github.com/pydantic/opamp-clients/issues with the integration and its
version, the runtime, the warning as logged (it never contains the key), and for a FAILED
policy, the reason. Report a security problem privately: [SECURITY.md](../SECURITY.md).
