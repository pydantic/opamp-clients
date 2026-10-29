# The OpenTelemetry Collector

Two pieces put an OpenTelemetry Collector under an OpAMP server on this repository's Go client
([go.md](go.md)), in platform mode: net/http and gorilla/websocket carry the requests, so the
Collector's TLS settings and the environment's proxies apply as they do to its other clients.

| | What it is | What the server can do |
|---|---|---|
| The `opamp` extension, [`go/opampextension`](../go/opampextension/) | The Collector's `opamp` extension with the client underneath replaced. The component type, the config and the custom-message API (`opampcustommessages`) are upstream's | see the collector: its description, effective config, health and available components; exchange custom messages with its components; restart it; issue it new OpAMP credentials |
| The Supervisor, [`go/opampsupervisor`](../go/opampsupervisor/) | A fork of upstream's `opampsupervisor` at `cmd/opampsupervisor/v0.164.1`, on this client. It runs any vendor's collector as a child process | everything above, plus configure the collector (remote config, within the policy the host sets: [what the server can do](../go/opampsupervisor/packaging/README.md#what-the-server-can-do-to-this-host)), and install a new collector executable (packages) |

The extension alone reports the collector it runs in. To have the server change what the
collector runs, run it under the Supervisor, which configures it with the extension. A collector's remote config is its YAML: the Logfire SDK
policy is for SDKs, and the Supervisor answers FAILED to a remote config that holds one
(`logfire-sdk-policy.json`).

What each adds to upstream's, and every option, is in its README: the
[extension's](../go/opampextension/README.md) and the
[Supervisor's](../go/opampsupervisor/README.md). In both, a state file keeps the instance UID and
the credentials the server issued across restarts, and `state_key` encrypts it at rest
([state-file.md](state-file.md#encrypted-at-rest-go), [connection-settings.md](connection-settings.md)).

## A collector with the extension

Build one with [ocb](https://opentelemetry.io/docs/collector/custom-collector/), the
Collector's builder. Put this extension in the manifest in place of upstream's: a collector can
have only one `opamp` extension.

```yaml
dist:
  name: otelcol-opamp
  output_path: ./otelcol-opamp
receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.164.0
exporters:
  - gomod: go.opentelemetry.io/collector/exporter/otlphttpexporter v0.164.0
extensions:
  - gomod: github.com/pydantic/opamp-clients/go/opampextension v0.2.0
providers:
  - gomod: go.opentelemetry.io/collector/confmap/provider/envprovider v1.70.0
  - gomod: go.opentelemetry.io/collector/confmap/provider/fileprovider v1.70.0
```

```bash
go run go.opentelemetry.io/collector/cmd/builder@v0.164.0 --config manifest.yaml
```

The extension is built against the Collector's v0.164.0 modules (v1.70.0 for the stable ones), so
take the other components at that version, and ocb with them.

To build from a checkout instead of a tag, add these to the manifest, with the checkout's path.

```yaml
replaces:
  - github.com/pydantic/opamp-clients/go => /path/to/checkout/go
  - github.com/pydantic/opamp-clients/go/opampextension => /path/to/checkout/go/opampextension
```

The collector's config is upstream's extension config, plus `state_file`, `state_key` and
`capabilities.accepts_opamp_connection_settings`; the [README](../go/opampextension/README.md#what-it-adds)
has an example.

## The Supervisor

### Packages

The packages install the Supervisor, a collector for it to run (built with ocb from
[collector.yaml](../go/opampsupervisor/packaging/collector.yaml), with this extension) and a
service, and enroll the host with one command, `pydantic-opamp-enroll`:

| Platform | Package | Service |
|---|---|---|
| Linux | `pydantic-opamp-supervisor`, a deb and an rpm, from signed apt and yum repositories | a systemd unit; the token and the state key sealed with `systemd-creds` |
| macOS | a `.pkg` | a LaunchDaemon; the state key in the System Keychain |
| Windows | an `.msi` | a Windows service; the state key protected with DPAPI |

None is published yet: build them with the scripts in
[packaging/](../go/opampsupervisor/packaging/). Each `go/opampsupervisor/vX.Y.Z` tag publishes
archives instead (see [Release archives](#release-archives)). The
[packaging README](../go/opampsupervisor/packaging/README.md) covers installing, enrolling,
running a collector without a server, building, signing and publishing, for
[Linux](../go/opampsupervisor/packaging/README.md#linux),
[macOS](../go/opampsupervisor/packaging/README.md#macos) and
[Windows](../go/opampsupervisor/packaging/README.md#windows).

### Release archives

Each `go/opampsupervisor/vX.Y.Z` tag has a
[GitHub Release](https://github.com/pydantic/opamp-clients/releases) with an archive for Linux,
macOS and Windows on amd64 and arm64: `opampsupervisor`, `otelcol` (the collector above, with this
extension), the licenses and a README. `SHA256SUMS` and a build provenance attestation
(`gh attestation verify <file> --repo pydantic/opamp-clients`) check a download.

```bash
curl -fsSLO https://github.com/pydantic/opamp-clients/releases/download/go%2Fopampsupervisor%2Fv0.8.0/pydantic-opamp-supervisor_0.8.0_linux_amd64.tar.gz
tar -xzf pydantic-opamp-supervisor_0.8.0_linux_amd64.tar.gz
cd pydantic-opamp-supervisor_0.8.0_linux_amd64
./opampsupervisor --config supervisor.yaml
```

### Container image

Each release also publishes `ghcr.io/pydantic/opamp-supervisor:<version>` for amd64 and arm64:
the Supervisor and this collector, configured with environment variables, with its state on a
volume. The [packaging README](../go/opampsupervisor/packaging/README.md#container) has the
variables and how to run it.

```bash
docker run -d -e OPAMP_ENDPOINT=wss://opamp.example.com/v1/opamp -e OPAMP_TOKEN="$ENROLLMENT_TOKEN" \
  -v opamp-state:/var/lib/pydantic-opamp ghcr.io/pydantic/opamp-supervisor:0.8.0
```

### From source

```bash
cd go/opampsupervisor
go build -o opampsupervisor .
./opampsupervisor --config supervisor.yaml
```

The config is upstream's. The [README](../go/opampsupervisor/README.md#running-it) has an example,
and lists what changes: issued credentials verified and kept, certificate checks that stay on, a
policy that limits what a remote config may run, and collector executables installed as packages,
checked with Cosign.

## Tests

```bash
scripts/fetch_core.py fetch
(cd go/opampextension && go test -race ./...)
(cd go/opampsupervisor && go test -race ./...)
```

The Supervisor's end-to-end suite (upstream's) runs against a collector built with this
extension, which it looks for in `bin/` at the repository's root:

```bash
cd go/opampsupervisor
mkdir -p ../../bin
go run go.opentelemetry.io/collector/cmd/builder@v0.164.0 --config testdata/collector/manifest.yaml
cp ../../bin/e2e-collector/otelcontribcol "../../bin/otelcontribcol_$(go env GOOS)_$(go env GOARCH)"
go test -tags=e2e -timeout 20m .
```
