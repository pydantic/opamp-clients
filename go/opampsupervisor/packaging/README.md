# The packages

Each `go/opampsupervisor/vX.Y.Z` tag gets a
[GitHub Release](https://github.com/pydantic/opamp-clients/releases) with portable archives and
native installers for Linux, macOS and Windows on amd64 and arm64 (see [Releases](#releases)),
and a [container image](#container).

The installers are a deb and an rpm for Linux, a [.pkg for macOS](#macos), and an
[.msi for Windows](#windows). Each installs the Supervisor and a collector for it to run.
The enrollment helper enrolls it with one command.

The bundled Collector includes host metrics, file logs, Windows event logs, macOS unified logs,
and file storage. Windows event logs and macOS unified logs run on their native operating systems.
Configure `storage: file_storage` on file and event log receivers to keep their positions across
restarts. Enable the extension under `service.extensions`. macOS unified logs do not have a saved
cursor. Bound their startup history with `max_log_age`.

The packages include these components from version 0.2.0. Version 0.1.0 did not include the
Windows and macOS log receivers or the file storage extension.

The release installers are unsigned. The macOS package is not notarized. GitHub build
provenance and the checksums verify their source and contents, but do not replace platform
code signing. `SHA256SUMS` covers the archives. `SHA256SUMS.installers` covers the installers.

## Linux

`pydantic-opamp-supervisor` (deb and rpm) installs everything a host needs to be managed over
OpAMP:

- the Supervisor (this fork);
- a collector for it to run, built with ocb from [collector.yaml](collector.yaml), with this
  repository's opamp extension;
- a systemd unit;
- `pydantic-opamp-enroll`.

Download the deb or rpm for your architecture from the GitHub Release. On Debian or Ubuntu,
install the local deb with `sudo apt install ./pydantic-opamp-supervisor_0.7.0_amd64.deb`.
On RHEL or Fedora, install the unsigned local rpm with
`sudo dnf install --nogpgcheck ./pydantic-opamp-supervisor-0.7.0-1.x86_64.rpm`.
The host needs systemd 250 or later. Verify the download against `SHA256SUMS.installers` first.

Once a signed apt and yum repository is published (none is yet, see [Publishing](#publishing)),
install it from there. `$REPO_URL` is where it's served:

```sh
# Debian, Ubuntu
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL "$REPO_URL/pydantic-opamp.gpg" | sudo tee /etc/apt/keyrings/pydantic-opamp.gpg >/dev/null
curl -fsSL "$REPO_URL/pydantic-opamp.sources" | sudo tee /etc/apt/sources.list.d/pydantic-opamp.sources >/dev/null
sudo apt-get update && sudo apt-get install pydantic-opamp-supervisor

# RHEL, Fedora, Amazon Linux: dnf asks to import the key on the first install.
sudo curl -fsSL -o /etc/yum.repos.d/pydantic-opamp.repo "$REPO_URL/pydantic-opamp.repo"
sudo dnf install pydantic-opamp-supervisor
```

Enrolling is one command. The token is read from standard input, never from the command line:

```sh
echo "$ENROLLMENT_TOKEN" | sudo pydantic-opamp-enroll --endpoint wss://opamp.example.com/v1/opamp
```

`pydantic-opamp-enroll` does four things:

- **Seals the token.** It encrypts the token to the host with `systemd-creds`, into
  `/etc/pydantic-opamp/enroll-token.cred`. That uses the TPM if the host has one.
- **Makes the state key, once.** It writes `state-key.cred`, the key the Supervisor seals its
  state file with. Running the command again keeps it, since a new key couldn't open the old
  state.
- **Writes the config.** `/etc/pydantic-opamp/supervisor.yaml` turns on
  `accepts_remote_config`, `accepts_opamp_connection_settings` and
  `storage.state_key: systemd-creds` and `agent.automatic_config_rollback` (a remote config the
  Collector fails on is replaced by the last one that ran healthy, and stays FAILED), and limits what the server's remote config may do (see
  [What the server can do to this host](#what-the-server-can-do-to-this-host)).
- **Starts the service and waits for it to connect.** It enables and starts
  `pydantic-opamp-supervisor.service`, then waits up to 30 seconds for the Supervisor to connect
  (`opampsupervisor status --wait`). If it can't, for example because the host doesn't trust the
  server's certificate (pass `--ca-file`), the command prints the Supervisor's status with what to
  check and exits 1. The service keeps retrying. Pass `--no-start` to skip this step.

With `--accept-packages`, the config also lets the server update the collector (see
[Collector updates from the server](#collector-updates-from-the-server)).

systemd decrypts both credentials for the service alone, into
`/run/credentials/pydantic-opamp-supervisor.service/`. The service runs as `pydantic-opamp`.

Once the server issues the agent its own credentials, the Supervisor uses them from then on:
- it verifies them by a connection the server accepts;
- it keeps them sealed in `/var/lib/pydantic-opamp/opamp-state`;
- it uses them after every restart.

The enrollment token in the config is then only a fallback.

Other options:
- `--token-file PATH` reads the token from a file instead of standard input.
- `--ca-file PATH` sets the CA bundle to trust for the server.
- `--plaintext` allows a `ws://` or `http://` endpoint.
- `--restricted` limits what the server's configs may do ([the restricted
  policy](#the-restricted-policy)). The default is no: any config the server sends runs.
- `--exporter-endpoint URL`, with `--restricted`, sets where the server's configs may send
  telemetry (repeat it for more). The default is the `--endpoint`'s host and port, over https.
- `--log-path PATH`, with `--restricted`, lets the server's configs read the logs in a file or a
  directory (repeat it for more). The default is none.
- `--host-logs`, with `--restricted`, lets the server's configs read the host's system logs. The
  default is no.

### Telemetry credentials for managed Collectors

The enrollment helpers can supply a separate telemetry token to a managed
Collector. This support is unreleased. Pass `--telemetry-token-stdin` on Linux
or macOS, or `-TelemetryTokenStdin` on Windows. Standard input must contain the
enrollment token on the first line and the telemetry token on the second line.
Terminate both lines with a newline. Do not put either token in arguments.

The Supervisor supplies `LOGFIRE_TOKEN` to its child Collector. A remote policy
can use `${env:LOGFIRE_TOKEN}` in an exporter header. The Supervisor configuration
contains only file references. Linux seals the telemetry token with
`systemd-creds` and loads it through a service drop-in. macOS and Windows store
it in the same private directory as the enrollment token. Both credentials
remain available after a service restart.

Without this option, the helper keeps any existing telemetry token. To rotate
the telemetry token, run the helper with this option and both credentials again.
`--token-file` cannot be combined with the two-token input mode.

### Unmanaged

The package can also run its collector on its own, with no OpAMP server, from a config you give
it:

```sh
echo "$TOKEN" | sudo pydantic-otelcol-configure --config otelcol.yaml
```

`pydantic-otelcol-configure` seals the token with `systemd-creds` into
`/etc/pydantic-opamp/otelcol-token.cred`, and installs the config as
`/etc/pydantic-opamp/otelcol.yaml`. It then turns the Supervisor off and starts
`pydantic-otelcol.service`; `--no-start` skips that step. The config reads the token as
`${file:/run/credentials/pydantic-otelcol.service/otelcol-token}`, where systemd decrypts it for
that service alone. The two services conflict, so only one runs: `pydantic-opamp-enroll` turns
the unmanaged collector off in the same way.

Removing the package stops both services. It keeps `/etc/pydantic-opamp` and the storage directory, so a reinstall picks up where it left off.

### Building

```sh
VERSION=0.1.0 ARCH=amd64 go/opampsupervisor/packaging/build.sh   # or ARCH=arm64
```

This builds both binaries without cgo, then the deb and the rpm into `packaging/dist/`, using nfpm and [nfpm.yaml](nfpm.yaml).

With `SIGNING_KEY_FILE` (an armored OpenPGP secret key; `SIGNING_KEY_PASSPHRASE` if it has one)
the rpm is signed with it, for dnf's `gpgcheck`.

### Publishing

[repo.sh](repo.sh) adds what `build.sh` made to an apt and a yum repository in `REPO_DIR`, and
signs both with the same key:

```sh
REPO_DIR=public REPO_URL=https://packages.example.com SIGNING_KEY_FILE=key.asc go/opampsupervisor/packaging/repo.sh
```

- **apt:** `deb/` holds `dists/stable`, signed as `InRelease` and `Release.gpg`, over `pool/`.
- **yum:** `rpm/` holds the rpms and `repodata/`, with `repomd.xml.asc` for `repo_gpgcheck`.
- **The key:** `pydantic-opamp.asc`, and `pydantic-opamp.gpg` for apt's `Signed-By`.
- **The sources:** `pydantic-opamp.sources` and `pydantic-opamp.repo`, pointing at `REPO_URL`.

Packages already in `REPO_DIR` stay. Run it on the published copy, then serve `REPO_DIR` at
`REPO_URL` as static files, so earlier releases stay installable. It needs `gpg`, `apt-utils`
and `createrepo_c`.

[publish.sh](publish.sh) does that for a bucket, with `gcloud storage`: GCS (`gs://`, with Google
credentials), or S3 and any S3-compatible store (`s3://`, with AWS credentials; set
`CLOUDSDK_STORAGE_S3_ENDPOINT_URL` for one other than S3). It fetches the published packages, adds
these with `repo.sh` and uploads the result: the packages first, then the indexes that list them,
then their signatures, so a machine updating mid-way never sees an index whose packages aren't
there yet. Packages are cached for good, indexes not at all. It refuses two things that would
break every machine already using the repositories: signing with a key other than the published
one, and replacing a published package with different contents.

```sh
REPO_BUCKET=gs://pydantic-opamp-packages REPO_URL=https://packages.pydantic.dev SIGNING_KEY_FILE=key.asc \
  go/opampsupervisor/packaging/publish.sh
```

Nothing runs it yet: no repository is hosted, and there is no signing key.

### Releases

[archive.sh](archive.sh) builds one release archive into `packaging/dist/`: `opampsupervisor` and
`otelcol` (built with ocb from [collector.yaml](collector.yaml)), the licenses and a README, as a
`.tar.gz` for Linux and macOS and a `.zip` for Windows. Both binaries are cross-built without cgo,
so any machine builds every target:

```sh
VERSION=0.1.0 GOOS=darwin GOARCH=arm64 go/opampsupervisor/packaging/archive.sh
```

The [Release workflow](../../../.github/workflows/release.yml) runs it for each
`go/opampsupervisor/vX.Y.Z` tag, for linux, darwin and windows on amd64 and arm64, and publishes
the six archives and eight installers as the tag's GitHub Release. Each file has a build
provenance attestation. `SHA256SUMS` covers the six archives, and `SHA256SUMS.installers` covers
the eight installers. The installers build on a matching OS and architecture. All source comes
from the release tag. The workflow needs no signing secrets.

Run it by hand, with the version, for a tag already pushed. GitHub sends no push event when
more than three tags are pushed at once. Select `installers-only` to add installers to an
existing release. This mode keeps its archives and `SHA256SUMS`. A rerun replaces the installer
assets and their checksums.

From version 0.2.0, each archive is also signed with Cosign (keyless, from the
workflow's GitHub identity), as
`<archive>.sigstore.json`: what a Supervisor that accepts packages checks before it installs a
collector the server offers.

Check a download with either:

```sh
sha256sum --check --ignore-missing SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS.installers
gh attestation verify pydantic-opamp-supervisor_0.7.0_linux_amd64.tar.gz --repo pydantic/opamp-clients
cosign verify-blob --bundle pydantic-opamp-supervisor_0.7.0_linux_amd64.tar.gz.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/pydantic/opamp-clients/\.github/workflows/release\.yml@' \
  pydantic-opamp-supervisor_0.7.0_linux_amd64.tar.gz
```

## macOS

The `.pkg` installs everything under `/opt/pydantic-opamp`:

- `bin/` holds the Supervisor, the collector and `pydantic-opamp-enroll`;
- `etc/` and `var/` hold the config, the token and the storage, all readable by root alone;
- `/Library/LaunchDaemons/dev.pydantic.opamp-supervisor.plist` is the LaunchDaemon. It runs as
  root and logs to `/opt/pydantic-opamp/var/log/supervisor.log`.

```sh
sudo installer -pkg pydantic-opamp-supervisor-0.7.0-darwin-arm64.pkg -target /
echo "$ENROLLMENT_TOKEN" | sudo /opt/pydantic-opamp/bin/pydantic-opamp-enroll --endpoint wss://opamp.example.com/v1/opamp
```

`pydantic-opamp-enroll` takes the Linux one's options. macOS has no systemd credentials, so
there are two differences:
- the enrollment token is kept in `etc/enroll-token`, readable by root alone;
- the Supervisor seals its state with a key it keeps in the System Keychain (`state_key: keychain`).

Build it on a Mac with `VERSION=0.1.0 ARCH=arm64 go/opampsupervisor/packaging/macos/build.sh`
(or `ARCH=amd64`). Three settings sign it, and each is optional:
- `CODESIGN_IDENTITY`, a Developer ID Application identity, signs the binaries;
- `INSTALLER_IDENTITY`, a Developer ID Installer identity, signs the package;
- `NOTARY_PROFILE`, a `notarytool` keychain profile, notarizes and staples it.

Unsigned, Gatekeeper refuses the package when it's opened from Finder. `installer` on the
command line still installs it.

## Windows

The `.msi` installs the Supervisor, the collector and `pydantic-opamp-enroll.ps1` in
`Program Files\pydantic-opamp`. It also installs the `pydantic-opamp-supervisor` service, which
runs as LocalSystem, logs to the Application event log, and stays stopped until enrolled. As an
administrator:

```powershell
msiexec /i pydantic-opamp-supervisor-0.7.0-windows-amd64.msi /qn
$env:ENROLLMENT_TOKEN | & "$env:ProgramFiles\pydantic-opamp\pydantic-opamp-enroll.ps1" -Endpoint wss://opamp.example.com/v1/opamp
```

It takes `-TokenFile`, `-CaFile`, `-Plaintext`, `-Restricted`, `-ExporterEndpoint`, `-LogPath`, `-HostLogs`, `-AcceptPackages` and `-NoStart`,
like the Linux one's options (give several endpoints or paths as a list:
`-LogPath 'C:\ProgramData\app\logs','D:\logs'`). It
writes to `ProgramData\pydantic-opamp`, which only SYSTEM and Administrators can read:
- the config;
- the token;
- the storage, whose state key DPAPI protects for SYSTEM (`state_key: dpapi`).

It then sets the service to start automatically, and starts it. Uninstalling removes the
service and keeps `ProgramData\pydantic-opamp`.

Build it on Windows with WiX 5 (`dotnet tool install --global wix --version 5.0.2`):
`go\opampsupervisor\packaging\windows\build.ps1 -Version 0.1.0`. The version is numbers only.
`-CertificateThumbprint` signs the executables and the `.msi` with `signtool`, using that
certificate from the machine's store.

## Container

The image holds the Supervisor and the collector, from the Linux release archives, on distroless
(no shell), running as a non-root user (65532). Each Supervisor release publishes it for amd64
and arm64 as `ghcr.io/pydantic/opamp-supervisor:<version>`, signed with Cosign and attested like
the archives. Environment variables configure it ([container/supervisor.yaml](container/supervisor.yaml)).
It runs any config the server sends. To restrict it, run the image with
`--config /etc/pydantic-opamp/supervisor-restricted.yaml`
([container/supervisor-restricted.yaml](container/supervisor-restricted.yaml)), the enroll
scripts' `--restricted` policy:

| Variable | What it is |
|---|---|
| `OPAMP_ENDPOINT` | the OpAMP server, as `wss://host/v1/opamp` or `https://host/v1/opamp` (required) |
| `OPAMP_TOKEN` | the enrollment token (required for the first start) |
| `OPAMP_EXPORTER_ENDPOINTS` | restricted only: where the server's configs may send telemetry, a YAML list of `scheme://host[:port]`; Logfire's US and EU regions by default |
| `OPAMP_LOG_PATHS` | restricted only: the files and directories the server's configs may read logs from, a YAML list; none by default |
| `OPAMP_STATE_KEY` | `file:PATH`, a 32-byte key (raw or base64) the issued credentials are sealed with, readable by the container's user only; none by default |
| `LOGFIRE_TOKEN` | a telemetry token for the collector, when the server doesn't issue one |

```sh
docker run -d --name opamp-supervisor \
  -e OPAMP_ENDPOINT=wss://opamp.example.com/v1/opamp -e OPAMP_TOKEN="$ENROLLMENT_TOKEN" \
  -v opamp-state:/var/lib/pydantic-opamp -p 4318:4318 \
  ghcr.io/pydantic/opamp-supervisor:0.7.0
```

Mount a volume at `/var/lib/pydantic-opamp`. It keeps the agent's instance UID, the credentials
the server issued and the last config, so a new container on it is the same agent and uses the
issued credentials. Without one, every container is a new agent that enrolls again with
`OPAMP_TOKEN`. On Kubernetes that means a StatefulSet with a volume claim, or a Deployment of
one replica with a persistent volume.

`supervisor-restricted.yaml`'s policy is the packages' [restricted policy](#the-restricted-policy),
with the variables above in place of the enroll script's options. The server can't install a collector in the container
(`accepts_packages` is off): a new collector comes with a new image. To change anything else,
mount your own config at `/etc/pydantic-opamp/supervisor.yaml`: one that reads the token from a
mounted secret with `${file:...}`, say, rather than from the environment. The collector's OTLP receiver,
when the server's config turns it on, listens in the container: publish its ports as you would
any collector's.

`docker exec <container> /usr/lib/pydantic-opamp/opampsupervisor status --config
/etc/pydantic-opamp/supervisor.yaml` says how it is: connected or not (and what to check), the
collector's health and the last remote config's status. It exits 0 when all is well, so it can
serve as an exec probe.

Build it from archives `archive.sh` made, for one architecture (loaded into the local Docker) or
both (`PUSH=1`, pushed as `IMAGE`):

```sh
VERSION=0.1.0 GOOS=linux GOARCH=amd64 go/opampsupervisor/packaging/archive.sh
VERSION=0.1.0 ARCHES=amd64 go/opampsupervisor/packaging/container/build.sh
```

## What the server can do to this host

Every enroll script turns on `accepts_remote_config`, so the OpAMP server configures the
collector. That includes anyone who holds the server's operator credentials, or who takes the
server over. The collector can read files and environment variables and send data anywhere.

By default that is all allowed: any config the server sends runs, with any component the
collector has, sending anywhere. That is what lets a server deploy any pipeline, to any
exporter. The collector runs as `pydantic-opamp` on Linux, and as root on macOS and LocalSystem
on Windows, so it reads what that user can.

With `--restricted` (`-Restricted` on Windows), each script also writes a policy,
`agent.remote_config_policy` in the Supervisor's config. The server can't change it: it is in
the local config, and a remote config can't touch that.

The Supervisor refuses a remote config that breaks the policy, the whole config. It reports the
config FAILED to the server with the reason, for example `the remote config: receiver "filelog"
reads "/etc/shadow", which agent::remote_config_policy::allowed_log_paths doesn't allow`. The
collector keeps the config it had.

### The restricted policy

| Rule | Default | Widen it with |
|---|---|---|
| `allowed_components` | receivers `otlp`, `host_metrics`, `file_log`, `nop`; processors `batch`, `memory_limiter`, `resource`, `resource_detection`; exporters `otlp_grpc`, `otlp_http`, `debug`, `nop`; extension `health_check` (and the older names `hostmetrics`, `filelog`, `resourcedetection`, `otlp` and `otlphttp`) | an edit to the config |
| `allowed_providers` | none, except environment names explicitly bound by `agent.connection_env`; no other `${env:...}` or `${file:...}` | an edit to the config (not recommended) |
| `allowed_exporter_endpoints` | the OpAMP endpoint's host and port, over https (http with `--plaintext`) | `--exporter-endpoint` (`-ExporterEndpoint` on Windows) |
| `allowed_log_paths` | none | `--log-path` (`-LogPath` on Windows); `--host-logs` adds `/var/log` on Linux |
| `allowed_storage_directories` | none | `--host-logs` (`-HostLogs` on Windows) adds the Supervisor's `collector` directory: `/var/lib/pydantic-opamp/collector` on Linux, `ProgramData\pydantic-opamp\var\collector` on Windows |

What each rule checks in a remote config:
- **Components.** Each receiver, processor, exporter, connector and extension must have a type
  in the list. A kind that isn't listed allows none.
  The Windows event log and macOS unified log receivers and the `file_storage` extension
  are not in the default list. They read the host's system logs and write files, so a server's
  config can turn them on only where the host's operator allowed it: `--host-logs` adds them
  where the system has them (`windows_event_log` and `file_storage` on Windows,
  `macos_unified_logging` on macOS, `file_storage` on Linux).
- **System logs.** A Windows event log receiver's `remote` (other hosts' logs, with their
  credentials) and a macOS unified logging receiver's `archive_path` (an archive read from any
  path) are refused: the local config can set them, the server can't.
- **Storage.** A `file_storage` extension must name its `directory`, and it and any
  `compaction::directory` must be an allowed storage directory or lie under one.
- **Providers.** Remote configs may reference exactly the environment names bound by the local
  `agent.connection_env` configuration. For example, a binding to `LOGFIRE_TOKEN` permits
  `${env:LOGFIRE_TOKEN}` and `${LOGFIRE_TOKEN}`. Other environment variables, nested references,
  and default values remain blocked. `$${...}` is a literal `$` and is allowed. The local config
  still uses providers, such as `${file:...}` for the enrollment token.
- **Destinations.** Each exporter's `endpoint` and `*_endpoint` setting must be an `http://` or
  `https://` URL with an allowed scheme, host and port. The host's case and the scheme's default
  port don't matter, and any path is allowed (a gRPC `otlp` exporter takes none). A value with
  a `$` is refused. So are `proxy_url`, `ca_file`, `ca_pem`, `server_name_override`,
  `insecure` and `insecure_skip_verify`: the local config can set them, the server can't. The
  collector's own telemetry (`service::telemetry`) must use the same endpoints, and may write its
  logs only to `stdout` and `stderr`.
- **Log files.** Each path a `file_log` (or `filelog`) receiver includes must be an allowed file, or lie under
  an allowed directory. A path is compared as written, case and all. It must be absolute, and
  can't hold `..`, braces, `$` or (except on Windows) a backslash. `delete_after_read` is
  refused.

The Supervisor reads the config as it merges it, with confmap: a key spelled
`receivers::filelog`, an anchor or a merge key is checked like any other.

Why no log files in it: host logs often hold secrets and personal data, and only the host's
operator knows which files are fit to send. `--host-logs` is that operator's choice, made on the
command line they run: Logfire's host monitoring configs read the system logs, and need it.
Either way, on Linux the collector runs as `pydantic-opamp`: add that user to the group that can
read the log files (`adm` on Debian and Ubuntu) for it to read the ones only that group can.

### Widening it

Only on the host. Run the enroll script again with the options above, which rewrites the config
and keeps the state key:

```sh
echo "$ENROLLMENT_TOKEN" | sudo pydantic-opamp-enroll --endpoint wss://opamp.example.com/v1/opamp --restricted \
  --exporter-endpoint https://logfire-us.pydantic.dev --log-path /var/log/myapp
```

Or edit `agent.remote_config_policy` in the config and restart the Supervisor:

```yaml
agent:
  remote_config_policy:
    allowed_exporter_endpoints: ["https://logfire-us.pydantic.dev", "https://logfire-eu.pydantic.dev"]
    allowed_log_paths: ["/var/log/myapp", "/var/log/nginx/access.log"]
```

An allowed directory is trusted as a whole. The collector follows a symlink in it, even one
that points out of it, so allow only directories that only trusted users can write. On macOS
and Windows the collector runs as root and LocalSystem, so it can read an allowed file whatever
the file's permissions.

### What the server can't do

- Read a file outside the allowed log paths, or an environment variable.
- Send telemetry anywhere but the allowed endpoints, or through a proxy, or turn off the TLS
  checks an `https://` endpoint gets.
- Write or delete a file: the bundled collector has no file exporter, its own logs go only to
  the standard output, and `delete_after_read` is refused.
- Run a component that isn't listed, or change the policy.

### What the server can still do

- Choose what the allowed components collect and send to the allowed endpoints. That includes
  host metrics (with process names and command lines, from `hostmetrics`' process scraper), the
  allowed log files, and what applications send to the OTLP receiver.
- Have the OTLP receiver or `health_check` listen on any address and port of the host. Others
  on the network can then send telemetry through it.
- Stop telemetry, and restart the collector.
- Issue the agent new OpAMP connection settings (`accepts_opamp_connection_settings`): new
  credentials, or another OpAMP server. The policy stays the one in the local config.

A remote config holds its own credentials (an exporter's `Authorization` header, say), because
it can't read them from the host. The Supervisor keeps the configs it received sealed with the
state key; the merged config the collector reads, in the storage directory, isn't sealed.

### Collector updates from the server

Off by default. With `--accept-packages` (`-AcceptPackages` on Windows), the enroll script turns
on `accepts_packages`, and the server can offer a collector for this host to run in place of the
installed one. The Supervisor installs it only if this repository's release workflow signed it:
the archive's Cosign signature (`<archive>.sigstore.json` on each release) must carry that
workflow's certificate and be in Sigstore's public transparency log. The accepted signer is in
the local config (`agent.package.verifier`), and the server can't change it. Before it stops the
running collector, the Supervisor checks that the new one runs. If the new one doesn't report
healthy within 30 seconds, the previous one goes back.

The collector the server installed lives in the storage directory, and runs while the server
offers it. A package upgrade (apt, dnf, the `.pkg` or the `.msi`) still updates the installed
collector, but that one runs only once the server stops offering a collector. The Supervisor
then deletes the server's copy and runs the installed collector again. Package upgrades always
update the Supervisor itself: the server can't.

On Linux the collector runs as `pydantic-opamp`. On macOS and Windows it runs as root and
LocalSystem, so there a collector the server offers runs with those rights, once its signature
checks out.

## Tests

- [otelcol_configure_test.go](otelcol_configure_test.go) does the same for
  `pydantic-otelcol-configure`, and [otelcol_validate_test.go](otelcol_validate_test.go)
  (`-tags=repo`) checks that the bundled collector takes the config it installs.
- [enroll_test.go](enroll_test.go) runs `pydantic-opamp-enroll` with stand-ins for `systemd-creds` and `systemctl`. It checks what the script writes, what it refuses, and that the Supervisor loads the config it writes. It also checks that the policy it writes allows a Logfire config and refuses the file and environment reads and the other destinations.
- [repo_test.go](repo_test.go) (`-tags=repo`, with the `SIGNING_KEY_FILE` the build used) runs
  `repo.sh` and installs from what it makes:
  - apt installs from it with its key alone, keeping an earlier release, and refuses a changed
    index or another key;
  - dnf, in an AlmaLinux 9 container, imports the key and installs, and refuses a repository
    signed by another key;
  - the rpm's own signature checks out with the key.
- [publish_test.go](publish_test.go) (`-tags=repo` too, with `gcloud`, `fake-gcs-server`,
  `moto_server` and the `aws` CLI) runs `publish.sh` against a GCS bucket fake-gcs-server serves
  and an S3 bucket moto serves, both over HTTP. apt installs from its URL, keeping
  the earlier release already there; the packages go up before the indexes, the indexes before
  their signatures; and another key or a changed package is refused before anything is uploaded.
- [enroll_macos_test.go](enroll_macos_test.go) and [enroll_windows_test.go](enroll_windows_test.go) do the
  same for the macOS and Windows enroll scripts.
- [install_test.go](install_test.go) (`-tags=install`, as root or an administrator, on a throwaway
  machine) is the end-to-end test, on all three systems. It:
  1. installs the package: from a repository `repo.sh` made (`OPAMP_REPO`) with apt on Linux, the
     `.pkg` (`OPAMP_PKG`) on macOS, the `.msi` (`OPAMP_MSI`) on Windows;
  2. enrolls with a server that issues a token;
  3. checks that the token is used and that the state file is sealed;
  4. restarts the Supervisor and checks it connects with the issued token.
- [container_test.go](container_test.go) (`-tags=container`, with Docker and the image in
  `OPAMP_IMAGE`) does the same for the container image: a container enrolls, its state in the
  volume is sealed, and a new container on the volume connects with the issued token.

CI runs them all, on Linux (with a throwaway signing key), macOS and Windows, and keeps the
packages as artifacts.

## Remove the installed Collector

Run `pydantic-opamp-uninstall` as root on Linux, or
`/opt/pydantic-opamp/bin/pydantic-opamp-uninstall` as root on macOS.
On Windows, run `pydantic-opamp-uninstall.ps1` from the installed program directory
in an Administrator terminal. These helpers stop the service and remove the native
package. They do not contact the OpAMP server or revoke any remote credentials.

Use `--dry-run` (`-DryRun` on Windows) to preview the operation. The default keeps
configuration, credentials, downloaded packages, and log read positions for a
reinstall. It retains a protected recovery helper at `/etc/pydantic-opamp/uninstall`
on Linux, `/opt/pydantic-opamp/etc/uninstall` on macOS, or
`ProgramFiles\pydantic-opamp\uninstall.ps1` on Windows. Run that helper to repeat
removal after the native package is gone.

Use `--purge` (`-Purge` on Windows) to also delete the package's configuration and
state directories, including the recovery helper. A purge deletes the macOS
System Keychain item for `/opt/pydantic-opamp/var/opamp-state` only.
Custom storage directories, system log files, package repository settings, and
shared system users stay untouched. A service-stop or package-removal failure
aborts the purge and keeps local data.
