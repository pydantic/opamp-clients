# The packages

Releases are archives for now, not installers: each `go/opampsupervisor/vX.Y.Z` tag gets a
[GitHub Release](https://github.com/pydantic/opamp-clients/releases) with the Supervisor and the
collector for Linux, macOS and Windows on amd64 and arm64 (see [Releases](#releases)). The
installers below are built and tested in CI but not published yet.

There are three: a deb and an rpm for Linux, a [.pkg for macOS](#macos), and an
[.msi for Windows](#windows). Each installs the Supervisor and a collector for it to run, and
enrolls with one command.

## Linux

`pydantic-opamp-supervisor` (deb and rpm) installs everything a host needs to be managed over
OpAMP:

- the Supervisor (this fork);
- a collector for it to run, built with ocb from [collector.yaml](collector.yaml), with this
  repository's opamp extension;
- a systemd unit;
- `pydantic-opamp-enroll`.

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
  `accepts_opamp_connection_settings` and `storage.state_key: systemd-creds`.
- **Starts the service.** It enables and starts `pydantic-opamp-supervisor.service`. Pass
  `--no-start` to skip this step.

systemd decrypts both credentials for the service alone, into
`/run/credentials/pydantic-opamp-supervisor.service/`. The service runs as `pydantic-opamp`.

Once the server issues the agent its own credentials, the Supervisor uses them from then on:
- it verifies them by a connection the server accepts;
- it keeps them sealed in `/var/lib/pydantic-opamp/opamp-client.state`;
- it uses them after every restart.

The enrollment token in the config is then only a fallback.

Other options:
- `--token-file PATH` reads the token from a file instead of standard input.
- `--ca-file PATH` sets the CA bundle to trust for the server.
- `--plaintext` allows a `ws://` or `http://` endpoint.

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
the six archives as the tag's GitHub Release, with `SHA256SUMS` and a build provenance attestation
for each. It needs no secrets. Run it by hand, with the version, for a tag already pushed: GitHub
sends no push event when more than three tags are pushed at once.

Check a download with either:

```sh
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify pydantic-opamp-supervisor_0.1.0_linux_amd64.tar.gz --repo pydantic/opamp-clients
```

## macOS

The `.pkg` installs everything under `/opt/pydantic-opamp`:

- `bin/` holds the Supervisor, the collector and `pydantic-opamp-enroll`;
- `etc/` and `var/` hold the config, the token and the storage, all readable by root alone;
- `/Library/LaunchDaemons/dev.pydantic.opamp-supervisor.plist` is the LaunchDaemon. It runs as
  root and logs to `/opt/pydantic-opamp/var/log/supervisor.log`.

```sh
sudo installer -pkg pydantic-opamp-supervisor-0.1.0-darwin-arm64.pkg -target /
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
msiexec /i pydantic-opamp-supervisor-0.1.0-windows-amd64.msi /qn
$env:ENROLLMENT_TOKEN | & "$env:ProgramFiles\pydantic-opamp\pydantic-opamp-enroll.ps1" -Endpoint wss://opamp.example.com/v1/opamp
```

It takes `-TokenFile`, `-CaFile`, `-Plaintext` and `-NoStart`, like the Linux one's options. It
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

## Tests

- [otelcol_configure_test.go](otelcol_configure_test.go) does the same for
  `pydantic-otelcol-configure`, and [otelcol_validate_test.go](otelcol_validate_test.go)
  (`-tags=repo`) checks that the bundled collector takes the config it installs.
- [enroll_test.go](enroll_test.go) runs `pydantic-opamp-enroll` with stand-ins for `systemd-creds` and `systemctl`. It checks what the script writes, what it refuses, and that the Supervisor loads the config it writes.
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

CI runs them all, on Linux (with a throwaway signing key), macOS and Windows, and keeps the
packages as artifacts.
