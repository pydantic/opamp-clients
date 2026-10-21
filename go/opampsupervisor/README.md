# OpAMP Supervisor for the OpenTelemetry Collector, on the shared core

A downstream fork of upstream's
[`opampsupervisor`](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/cmd/opampsupervisor)
at `cmd/opampsupervisor/v0.163.0`, with its OpAMP client replaced: upstream's runs on opamp-go;
this one runs on [`go/opamp`](../opamp), the shared core in platform mode (net/http and
gorilla/websocket, so the configured TLS settings and the environment's proxies apply as before).
It supervises any vendor's collector, as upstream's does: the collector needs the `opamp`
extension, upstream's or [ours](../opampextension). The config is upstream's; what differs is
below, and everything after the next heading is upstream's README.

## Running it

On Linux, the [package](packaging/README.md) brings the Supervisor, a collector with our
extension and a systemd unit, and enrolls the host in one step:

```sh
echo "$ENROLLMENT_TOKEN" | sudo pydantic-opamp-enroll --endpoint wss://opamp.example.com/v1/opamp
```

It writes a remote config policy: the server's configs may use the components a Logfire config
needs, send only to the OpAMP server's host (or the `--exporter-endpoint`s given), read no file
(or only the `--log-path`s given), and reference only locally bound credential environment names instead of arbitrary
`${env:...}` or `${file:...}`
([what the server can do](packaging/README.md#what-the-server-can-do-to-this-host)).

The [macOS .pkg](packaging/README.md#macos) and the [Windows .msi](packaging/README.md#windows)
do the same with a LaunchDaemon and a Windows service.

Or build and run it yourself:

```sh
go build -o opampsupervisor .
./opampsupervisor --config supervisor.yaml
```

```yaml
server:
  endpoint: wss://opamp.example.com/v1/opamp
  headers:
    Authorization: "Bearer ${env:OPAMP_ENROLLMENT_TOKEN}"
capabilities:
  accepts_remote_config: true
  reports_effective_config: true
  reports_health: true
  accepts_opamp_connection_settings: true
storage:
  directory: /var/lib/opampsupervisor
  state_key: auto
agent:
  executable: /usr/bin/otelcol-contrib
  remote_config_policy:
    allowed_components:
      receivers: [otlp, hostmetrics]
      processors: [batch]
      exporters: [otlphttp]
    allowed_exporter_endpoints: ["https://logfire-us.pydantic.dev"]
```

## What changes

- **Bound credentials remain usable under the remote config policy.** A remote config can reference
  exactly the environment names in the local `agent.connection_env` bindings. Other environment
  names, nested references, defaults, and file providers remain restricted by the policy.
- **Resumed configs are acknowledged again.** The agent loop reports its existing outcome for a
  repeated config hash without restarting the Collector. This lets the client complete a resumed
  offer before it delivers another config.

- **Issued credentials are verified and kept.** With `accepts_opamp_connection_settings`, the
  server may issue new OpAMP connection settings (per-agent credentials replacing an enrollment
  token, say). The core tries them, keeps them only once the server has accepted a connection
  with them, goes back to the old ones otherwise, and keeps them in
  `<storage.directory>/opamp-state` (0600), so a restarted Supervisor connects with the
  issued credentials, not the ones in its config
  ([docs/connection-settings.md](../../docs/connection-settings.md)). Upstream's kept them in
  memory only, and "verified" them by restarting its client.
- **The OpAMP client's state is the only state.** The client keeps the instance UID, the remote
  config last received, the one the collector last applied, a refused one's FAILED status and
  the own-telemetry settings in `opamp-state`, and says what it resumes before it connects: the
  collector starts on that. Upstream's kept copies of its own (`persistent_state.yaml`,
  `last_recv_remote_config.dat`, `last_working_remote_config.dat`,
  `last_recv_own_telemetry_config.dat`), which are no longer written: upstream's code that
  saves and loads them is kept, and `supervisor/client_state.go` serves it the client's state
  instead, read before the bootstrap. The collector
  starts on the config last received, or, if the collector refused it, on the one it last
  applied: a refused config isn't run again at start, with or without
  `automatic_config_rollback` (upstream's ran it again without). `agent.instance_id` wins over
  a UID kept for another one ([docs/instance-uid.md](../../docs/instance-uid.md)).
- **The issued credentials can be encrypted at rest.** `storage.state_key` seals the client's
  state file with AES-256-GCM, its key from the OS: `systemd-creds` (a `LoadCredentialEncrypted=`
  credential, bound to the host and its TPM), `keychain` (macOS), `dpapi` (Windows), `file:PATH`
  or `auto` ([docs/state-file.md](../../docs/state-file.md#encrypted-at-rest-go)). A key it can't
  find stops the client at the start. The collector's config files in the storage directory
  aren't sealed: the collector reads them.
- **Certificate checks stay on.** Upstream's turned certificate verification off
  (`InsecureSkipVerify`) for an offer without a certificate, a plain token rotation, and then
  sent the rotated token to a server whose certificate nothing trusted. Here the configured TLS
  settings apply to every connection.
- **An `auth` extension runs on every HTTP request, and at every WebSocket upgrade** (a
  refreshed token is used from the next one). It can't be combined with
  `accepts_opamp_connection_settings`, since it would overwrite the headers the server issued.
- **The remote config can be limited** (`agent.remote_config_policy`), and the packages limit
  it: to the component types listed for each kind and the confmap providers listed; the
  exporters (and the collector's own telemetry) to the endpoints listed, with no proxy or TLS
  setting of their own; and `filelog` receivers to the paths listed. So a server can't point a
  `filelog` receiver at any file the collector reads, `${file:...}` a secret into an exporter,
  or send what it collects anywhere. A config refused is FAILED, with the reason, and never
  runs. The policy is read with confmap, as the config is merged (a `receivers::filelog` key is a
  receiver). Without a policy the Supervisor says at start that the remote config is
  unrestricted. Upstream has no limit. What the packaged policy allows, how to widen it on the
  host, and what a server can still do:
  [packaging/README.md](packaging/README.md#what-the-server-can-do-to-this-host).
- **Remote config statuses are about the last config received.** After an automatic rollback
  (`automatic_config_rollback`) the server keeps the FAILED status of the config that failed;
  upstream's also reported the restored one APPLIED, under that config's hash. The effective
  config says what runs.
- **APPLIED means the running collector is healthy on the config.** It is reported only when, at
  the end of the apply window, the collector runs and reported healthy on a connection made
  since it was given the config. An exit in the window, or as it ends, is FAILED, and so is an
  exit after APPLIED. A collector the restart backoff can't start is tried again on the backoff;
  it stayed down until the next config. The loop's decisions on an exit, a restart and the
  window's end are one pure step (`supervisor/apply_step.go`), tested rule by rule.
- **The Logfire SDK policy reaches the collector.** A server sends it in the
  `logfire-sdk-policy.json` entry of the config map, as it sends it to an SDK, beside the
  collector's YAML or alone. The Supervisor doesn't merge it into the config as YAML (upstream's
  did, and the collector refused the result): the SDK settings layer checks it, as the SDKs' does,
  and the collector's [`logfirepolicy` processor](../logfirepolicyprocessor/README.md), configured
  with it, goes first in every traces, metrics and logs pipeline, local or remote, so the
  collector redacts, filters by level, switches signals and head-samples as an SDK under the same
  policy does. A policy that overrides a control the collector can't carry out (`sampling_rules`,
  the rate caps, `debug`, ...) is FAILED with the layer's reason, and never runs. The agent
  description carries `logfire.opamp.config_format` and `logfire.opamp.supported_controls` only
  while the collector lists the processor among its available components; without it, only a
  policy with every control local is taken, and changes nothing.
- A custom message is handed to the client, which queues it: `ErrCustomMessagePending` never
  happens, and a message it can't send after all is logged.
- **`server` settings that can't apply are refused.** An endpoint with a user name or password,
  and `server.tls.insecure` with an `https` or `wss` endpoint (upstream then dropped the client
  certificate, `min_version` and `server_name`), fail validation. So does
  `server.tls.insecure_skip_verify` with `accepts_opamp_connection_settings`: the credentials the
  server issues would go to any server.
- **The effective config is the one the collector reports.** Upstream sent the merged config it
  wrote until the collector reported, with every secret in it; this one sends none until then,
  and an empty one while a server's empty config map runs nothing.
- Health, the effective config, the remote config status and the available components reach the
  server only with their capability, as before.
- With `accepts_packages` the Supervisor installs the collector executable the server offers,
  checked with Cosign, and puts the previous one back if the new one isn't healthy: upstream
  refuses the capability. When the server stops offering it, the configured executable runs
  again. A .zip is read as well as a .tar.gz, and the executable may sit in the archive's top
  directory, as in this repository's release archives. See the specification's
  [Collector Executable Updates Flow](specification/README.md#collector-executable-updates-flow).
  An update stays marked unproven until the new collector reports healthy (on the bootstrap's
  config when the server's config runs nothing), and a start that finds the mark undoes it, so
  a crash during the health wait can't cost the rollback target. A shutdown during the wait, or
  a rollback that can't put the previous executable back (the configured one then runs), isn't
  reported: the core asks again at the next start. While a start can't undo an unproven update,
  nothing more is installed: the core's request isn't answered until a start has undone it.
  A collector executable that can't be removed when it is no longer offered is reported failed,
  with why; it is removed again only once the server offers it and drops it again.
  `agent.package.verifier.type` must be set,
  to `none` to skip signature checks; an empty one turned them off.
- **The local OpAMP server takes the collector only.** The server the collector's opamp
  extension connects to, and the one the bootstrap starts, take a request only with a bearer
  token the Supervisor makes at each start and writes into the extension config
  (`server.ws.headers`; the extension redacts it in the effective config it reports, and the
  Supervisor leaves it out of what it passes on). The collector's messages are taken only
  with the Supervisor's instance UID. One collector is connected at a time, from its connection
  to its close: a request that fails to decode, or a failed upgrade, doesn't keep later ones out
  (409), and the server's custom messages go to the collector connected now.
- **A Restart command is carried out on the agent loop**, as a new config is: the client (its
  heartbeats, the server's messages) doesn't wait for the collector to stop, a Restart in the
  server's first reply waits for the loop, and with no config to run the collector stays
  stopped.
- A bootstrap that finds another instance UID reports the agent unhealthy, and fails.

Every file from upstream keeps its license header (Apache-2.0, [LICENSE](LICENSE)); the import
paths were rewritten to this module's. The files changed beyond that say what changed in their
header. These files are new, and are this repository's:

- `supervisor/coreclient.go`, `agent_update.go`, `apply_step.go`, `client_state.go`, `startup.go`, `sealed_storage.go`, `telemetry_env.go`,
  `localserver.go`, `sdk_policy.go`, `config/remote_config_policy.go` and `verifier/cosign.go`, and the tests
  beside them (with `supervisor/remote_config_refusal_test.go`, `restart_command_test.go` and
  `supervisor/config/core_test.go`, and the Cosign release fixture in `verifier/testdata/`);
- `e2e_packages_test.go`, `e2e_policy_test.go`, `e2e_sdk_policy_test.go` and `e2e_state_test.go`, and their configs in `testdata/`
  (`testdata/collector/manifest.yaml`, `testdata/supervisor/supervisor_auth_bearer_ws.yaml`,
  `supervisor_packages.yaml`, `supervisor_policy.yaml` and `supervisor_sdk_policy.yaml`);
- everything under `packaging/`.

Upstream's `supervisor/packages.go` (and its test config `supervisor_packages_cap.yaml`) is
removed: installing packages is `agent_update.go`'s. So are upstream's `Makefile`, `Dockerfile` and
`metadata.yaml`, which need contrib's build: `internal/metadata`, `generated_package_test.go` and
`documentation.md` are checked in as upstream's mdatagen wrote them.

The fork is kept close to upstream so that upstream's changes merge in: a change here goes in a
new file, with a small hook in upstream's code where it must, rather than rewriting upstream's
functions ([AGENTS.md](../../AGENTS.md#the-supervisor-and-collector-extension-forks)).
`scripts/collector_release.py diverged` (run by CI) lists every file that differs from upstream's
at the pinned release and refuses one whose header doesn't say what changed, or a missing
upstream file nobody explained; `scripts/collector_release.py port vX.Y.Z` three-way merges
upstream's changes since the pinned release into this directory.

## Upstream's README


<!-- status autogenerated section -->
| Status        |           |
| ------------- |-----------|
| Stability     | [alpha]: metrics   |
| Issues        | [![Open issues](https://img.shields.io/github/issues-search/open-telemetry/opentelemetry-collector-contrib?query=is%3Aissue%20is%3Aopen%20label%3Acmd%2Fopampsupervisor%20&label=open&color=orange&logo=opentelemetry)](https://github.com/open-telemetry/opentelemetry-collector-contrib/issues?q=is%3Aopen+is%3Aissue+label%3Acmd%2Fopampsupervisor) [![Closed issues](https://img.shields.io/github/issues-search/open-telemetry/opentelemetry-collector-contrib?query=is%3Aissue%20is%3Aclosed%20label%3Acmd%2Fopampsupervisor%20&label=closed&color=blue&logo=opentelemetry)](https://github.com/open-telemetry/opentelemetry-collector-contrib/issues?q=is%3Aclosed+is%3Aissue+label%3Acmd%2Fopampsupervisor) |
| [Code Owners](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/CONTRIBUTING.md#becoming-a-code-owner)    | [@evan-bradley](https://www.github.com/evan-bradley), [@atoulme](https://www.github.com/atoulme), [@tigrannajaryan](https://www.github.com/tigrannajaryan), [@douglascamata](https://www.github.com/douglascamata), [@dpaasman00](https://www.github.com/dpaasman00) |

[alpha]: https://github.com/open-telemetry/opentelemetry-collector/blob/main/docs/component-stability.md#alpha
<!-- end autogenerated section -->

This is an implementation of an OpAMP Supervisor that runs a Collector instance using configuration provided from an OpAMP server. This implementation
is following a design specified [here](./specification/README.md).
The design is still undergoing changes, and as such this implementation may change as well.

Binary and container images for the Supervisor are available under tags starting with `cmd/opampsupervisor` [here](https://github.com/open-telemetry/opentelemetry-collector-releases/tags).

## More information

If you'd like to learn more about OpAMP, see the
[OpAMP specification](https://github.com/open-telemetry/opamp-spec/blob/main/specification.md#open-agent-management-protocol).
For production-ready client and server implementations in Go, see the
[OpAMP Go repository](https://github.com/open-telemetry/opamp-go).

If you have questions or would like to participate, we'd love to hear from you.
There is a regular SIG meeting, see the
[meeting notes](https://docs.google.com/document/d/19WA5-ex8rNFIBIyVb5VqMXfWNmUQwppGhN8zBeNG0f4)
for more information. You can also get in touch with us in the
[#otel-opamp](https://cloud-native.slack.com/archives/C02J58HR58R) channel on
the CNCF Slack workspace.

## Installing the Supervisor

### Binary

Binaries are available as downloadable assets from [OpenTelemetry Collector releases](https://github.com/open-telemetry/opentelemetry-collector-releases/releases) with tags using the prefix `cmd/opampsupervisor`. You can download the file containing the binary for your OS and architecture, and install it on your machine manually.

#### Linux/macOS

Replace `<VERSION>` with your desired Supervisor version, and `<OS>` and `<ARCH>` with your platform-specific information:

```sh
curl --proto '=https' --tlsv1.2 -fL -o opampsupervisor \
"https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/cmd%2Fopampsupervisor%2Fv<VERSION>/opampsupervisor_<VERSION>_<OS>_<ARCH>"
chmod +x opampsupervisor
```

#### Windows

Replace `<VERSION>` with your desired Supervisor version:

```sh
Invoke-WebRequest -Uri "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/cmd%2Fopampsupervisor%2Fv<VERSION>/opampsupervisor_<VERSION>_windows_amd64.exe" -OutFile "opampsupervisor.exe"
Unblock-File -Path "opampsupervisor.exe"
```

### Container image

Container images are available for download from [GitHub Container registry](https://github.com/open-telemetry/opentelemetry-collector-releases/pkgs/container/opentelemetry-collector-releases%2Fopentelemetry-collector-opampsupervisor):

```sh
docker pull ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-opampsupervisor
```

## Using the Supervisor

To use the Supervisor, you will need four things:

1. A Supervisor binary, which can be obtained through the [installation](#installing-the-supervisor) step above.
2. A Collector binary that you would like to control through the Supervisor.
3. A Supervisor config file. See examples [here](./examples/).
4. A running OpAMP server.

To test the Supervisor with an example server, download the
[opamp-go](https://github.com/open-telemetry/opamp-go) repository, and run the
OpAMP example server in the `internal/examples/server` directory.

   ```shell
   git clone git@github.com:open-telemetry/opamp-go.git
   cd opamp-go/internal/examples/server
   go run .
   ```

Visit [localhost:4321](http://localhost:4321) to verify that the server is running.

In a separate directory, create a Supervisor configuration file named `supervisor.yaml`. Copy one of the existing [examples](./examples/) or create a new configuration. The `agent::executable` should be set to the path of your Collector binary:

```yaml
agent:
  executable: <COLLECTOR_BINARY>
```

Start the Supervisor:

```sh
./opampsupervisor --config=supervisor.yaml
```

The supervisor's own log encoding is configured under `telemetry.logs.encoding`.
Use `console` for plain-text logs with ISO8601 timestamps. Leave the field
unset (or set it to `json`) to keep the default JSON output.

```yaml
telemetry:
  logs:
    encoding: console
```

The supervisor can also configure resource detection for its own telemetry
using the experimental `telemetry.resource.detection/development` field.

```yaml
telemetry:
  resource:
    detection/development:
      detectors:
        - host: {}
        - process: {}
```

Supported detector entries are `container`, `host`, `process`, and `service`.

Do not set the `OTEL_EXPERIMENTAL_CONFIG_FILE` environment variable for the
Supervisor process. `xotelconf.NewSDK` uses that file to configure the SDK and
it supersedes the configuration passed by the Supervisor. If the variable is
set (for example to control the Collector's resource config), it can also
override the Supervisor's own resource detection and telemetry resource
settings. The Supervisor logs a warning at startup when the variable is set
while `telemetry.resource.detection/development` is configured.

The Supervisor's own OTLP exporters take their settings only from its
`telemetry` config. `OTEL_EXPORTER_OTLP_*` environment variables (endpoint,
insecure, certificates, headers and the rest) are ignored for them and still
reach the Collector. Without this, an `http://` endpoint or
`OTEL_EXPORTER_OTLP_INSECURE=true` in the environment would stop the Supervisor
from starting with an `https` endpoint in its config. To take a value from the
environment, use `${env:NAME}` in the config.

Visit [localhost:4321](http://localhost:4321) again to verify that your Collector appears in the Agents list.

## Collector crash log snippets

The Supervisor can include the tail of the Collector's logs in health and remote config failure messages when the Collector process exits unexpectedly. This is disabled by default because Collector logs may contain sensitive data. To enable it, set `agent::collector_crash_log_snippet_kib` to the maximum number of log KiB to include. A value of `0` disables the feature. `4` is a reasonable starting point. The maximum value is `1024` (1 MiB).

```yaml
agent:
  collector_crash_log_snippet_kib: 4
```

## Persistent data storage

The supervisor persists some data to disk in order to mantain state between restarts. The directory where this data is stored may be specified via the supervisor configuration:

```yaml
storage:
  directory: "/path/to/storage/dir"
```

By default, the supervisor will use `/var/lib/otelcol/supervisor` on posix systems, and `%ProgramData%/Otelcol/Supervisor` on Windows.

This directory will be created on supervisor startup if it does not exist.

(Changed here: what is kept between restarts is `opamp-state`, the OpAMP client's state: the
instance UID, the remote configs, the own-telemetry settings and any connection settings the
server issued. It is written readable by the Supervisor's user only, and sealed with
`storage.state_key` if set. Upstream's `persistent_state.yaml` and `.dat` files are gone.)

## Healthcheck

The Supervisor can be configured to expose a healthcheck endpoint that can be used to determine whether the Supervisor is running and healthy. This can be configured in the Supervisor configuration file:

```yaml
healthcheck:
  endpoint: localhost:13133
```

At the moment this healtcheck checks that the following conditions are met:

- The Supervisor is able to persist state to disk.
- The Supervisor is able to generate the agent's configuration.

For more details on the healthcheck configuration, see the see the [full list of `ServerConfig` options](https://github.com/open-telemetry/opentelemetry-collector/tree/main/config/confighttp).

Note that the healthceck endpoint is not enabled by default. To enable it, you must explicitly set at least the `endpoint` field in the configuration.

## Startup Fallback Configuration

The Supervisor supports a startup fallback configuration mechanism that provides resilience when the OpAMP server is unreachable at startup and there's no previous configuration state persisted in disk. This is useful for ensuring the Collector can start with a known-good configuration during network outages or server maintenance. When the Supervisor successfully connects to the OpAMP server, the regular configuration (indicated by `agent::config_files`) is restored and any potential remote configuration received from the OpAMP server is applied.

If, when the Supervisor starts, there's previous configuration state persisted in disk, the Supervisor will use it to start the Collector.

### Configuration Options

The following options can be configured under the `agent` section:

```yaml
agent:
  startup_fallback_configs:
  - /path/to/fallback_config.yaml
```

| Option | Description |
|--------|-------------|
| `startup_fallback_configs` | List of paths to startup fallback configuration files to use. If more than one path is specified, they are merged in order. Together, these must form a complete, standalone Collector configuration. |

### Important Notes

- The startup fallback configuration is a set of **standalone configuration files**. It is intentionally not merged with the `agent::config_files` setting. This ensures predictable behavior without dependencies on other configuration files.

- The OpAMP extension and "own telemetry" configuration are automatically added to the fallback configuration. This maintains the Collector's own telemetry flowing and the communication between Supervisor and Collector.

- The Supervisor will continue attempting to reconnect to the OpAMP server even while using the fallback configuration.

### Example

See [examples/supervisor_fallback.yaml](./examples/supervisor_fallback.yaml) for a complete example configuration with fallback enabled.

## Automatic Remote Configuration Rollback

The Supervisor supports automatic rollback when a remote configuration received from
the OpAMP backend causes the Collector to fail to start correctly. This feature is
**disabled by default** and can be enabled in the Supervisor configuration file:

```yaml
agent:
  automatic_config_rollback: true
```

When enabled, the Supervisor will watch the health of the Collector after applying remote
configurations received from the OpAMP backend and it will keep the last working one,
besides the last received. If a remote configurations causes the Collector
to fail to start correctly, the Supervisor will automatically swap to the last working one
and restart the Collector. (Changed here: the FAILED status of the configuration that failed
stays reported; upstream reported a new "APPLIED" status for the last working one. The OpAMP
client keeps the last working one, as the config last applied; after a restart the collector
starts on it rather than on the one that failed, whether or not rollback is enabled.)

## Status

The OpenTelemetry OpAMP Supervisor is intended to be the reference
implementation of an OpAMP Supervisor, and as such will support all OpAMP
capabilities. Additionally, it follows a design document for the features it
intends to support.

For a list of open issues related to the Supervisor, see [these issues](https://github.com/open-telemetry/opentelemetry-collector-contrib/issues?q=is%3Aopen+is%3Aissue+label%3Acmd%2Fopampsupervisor).

**Key**:

✅: Fully implemented

⚠️: Implemented with caveats

### OpAMP capabilities

| OpAMP capability               | Status                                                                           |
|--------------------------------|----------------------------------------------------------------------------------|
| AcceptsRemoteConfig            | ✅                                                                               |
| ReportsEffectiveConfig         | ✅                                                                               |
| AcceptsPackages                | ✅ (the collector executable only)                                               |
| ReportsPackageStatuses         | ✅                                                                               |
| ReportsOwnTraces               | ✅                                                                               |
| ReportsOwnMetrics              | ✅                                                                               |
| ReportsOwnLogs                 | ✅                                                                               |
| AcceptsOpAMPConnectionSettings | ✅                                                                               |
| AcceptsOtherConnectionSettings | ✅                                                                               |
| AcceptsRestartCommand          | ✅                                                                               |
| ReportsHealth                  | ⚠️                                                                               |
| ReportsStatus                  | <https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/38729> |
| ReportsRemoteConfig            | ✅                                                                               |
| ReportsAvailableComponents     | ✅                                                                               |

### Supervisor specification features

| Feature                                                            | Status                                                                           |
|--------------------------------------------------------------------|----------------------------------------------------------------------------------|
| Offers Supervisor configuration including configuring capabilities | ✅                                                                               |
| Starts and stops a Collector using remote configuration            | ✅                                                                               |
| Communicates with OpAMP extension running in the Collector         | ✅                                                                               |
| Updates the Collector binary                                       | <https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/33947> |
| Configures the Collector to report it's own metrics over OTLP      | ✅                                                                               |
| Configures the Collector to report it's own logs over OTLP         | ✅                                                                               |
| Sanitization or restriction of Collector config                    | <https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/24310> |
