# The OpenTelemetry Collector

Two pieces put an OpenTelemetry Collector under an OpAMP server on this repository's Go client
([go.md](go.md)), in Go mode: net/http and gorilla/websocket carry the requests, so the
Collector's TLS settings and the environment's proxies apply as they do to its other clients.

| | What it is | What the server can do |
|---|---|---|
| The `opamp` extension, [`go/opampextension`](../go/opampextension/) | The Collector's `opamp` extension with the client underneath replaced. The component type, the config and the custom-message API (`opampcustommessages`) are upstream's | see the collector: its description, effective config, health and available components; exchange custom messages with its components; restart it; issue it new OpAMP credentials |
| The Supervisor, [`go/opampsupervisor`](../go/opampsupervisor/) | A fork of upstream's `opampsupervisor` at `cmd/opampsupervisor/v0.163.0`, on this client. It runs any vendor's collector as a child process | everything above, plus configure the collector (remote config), and install a new collector executable (packages) |

The extension alone reports the collector it runs in. To have the server change what the
collector runs, run it under the Supervisor, which configures it with the extension.

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
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.163.0
exporters:
  - gomod: go.opentelemetry.io/collector/exporter/otlphttpexporter v0.163.0
extensions:
  - gomod: github.com/pydantic/opamp-clients/go/opampextension v0.1.0
providers:
  - gomod: go.opentelemetry.io/collector/confmap/provider/envprovider v1.69.0
  - gomod: go.opentelemetry.io/collector/confmap/provider/fileprovider v1.69.0
```

```bash
go run go.opentelemetry.io/collector/cmd/builder@v0.163.0 --config manifest.yaml
```

The extension is built against the Collector's v0.163.0 modules (v1.69.0 for the stable ones), so
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

The Packages workflow publishes them for each `go/opampsupervisor/vX.Y.Z` tag. Until the first,
build them with the scripts in [packaging/](../go/opampsupervisor/packaging/). The
[packaging README](../go/opampsupervisor/packaging/README.md) covers installing, enrolling,
running a collector without a server, building, signing and publishing, for
[Linux](../go/opampsupervisor/packaging/README.md#linux),
[macOS](../go/opampsupervisor/packaging/README.md#macos) and
[Windows](../go/opampsupervisor/packaging/README.md#windows).

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
go run go.opentelemetry.io/collector/cmd/builder@v0.163.0 --config testdata/collector/manifest.yaml
cp ../../bin/e2e-collector/otelcontribcol "../../bin/otelcontribcol_$(go env GOOS)_$(go env GOARCH)"
go test -tags=e2e -timeout 20m .
```
