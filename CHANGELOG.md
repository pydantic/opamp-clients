# Changelog

What each release of this repository's packages changes for the people who run them. Every
release is a tag: `go/opampsupervisor/vX.Y.Z` (the Supervisor and its installers),
`go/<module>/vX.Y.Z` (the Go modules), and, once the SDKs are on their registries, `sdk/vX.Y.Z`.
The core every package carries is a `core/vX.Y.Z` release of this repository, pinned in
`opamp-core.lock.json`.

How a version moves: each package follows [Semantic Versioning](https://semver.org/). Before 1.0 a
minor release may break you, and says how under **Breaking**. A fix ships in a new release of the
latest version only; there are no backports ([SECURITY.md](SECURITY.md)).

How to add to it: a pull request that changes what a user of a package sees (behavior, options,
defaults, supported versions, the core it carries, a security fix) adds a line under
**Unreleased**, in the group it belongs to (**Breaking**, **Added**, **Changed**, **Fixed**,
**Security**), naming the package and the pull request. A release moves those lines under its tag
and date. Tests, CI and docs alone don't need a line.

## Unreleased

### Changed

- Go: `go/opampotel` needs Go 1.26 or later (Go 1.25 is out of support), to take golang.org/x/net
  0.60.0.
- Collector: the `logfirepolicy` processor no longer head-samples; sample in the SDKs (#77).

### Security

- Dependencies with known advisories updated: golang.org/x/net 0.60.0 (`go/opampotel`, the
  examples, the conformance server), google.golang.org/grpc 1.83.1 and OpenTelemetry Go 1.45.0
  (the Go example, the conformance server), aiohttp 3.14.5 and OpenTelemetry Python 1.45.1
  (Python), ip-address 10.7.4 (Node.js), jackson-databind 2.22.3 (Java tests). Every lockfile is
  now checked against OSV on every change and nightly (`scripts/deps_scan.py`).

## Supervisor 0.4.0 and the Go modules 0.2.0 (2026-10-22)

`go/opampsupervisor/v0.4.0`, `go/v0.2.0`, `go/opampotel/v0.2.0`, `go/opampextension/v0.2.0`,
`go/logfirepolicyprocessor/v0.2.0`, on core 0.10.0.

### Added

- Scrubbing: the Logfire SDK's default scrubbing patterns, plus globs, in every SDK integration,
  set by the server's policy (#71).
- Collector: the `logfirepolicy` processor carries out the Logfire SDK policy on what goes through
  the collector; the Supervisor sends it the policy (#63).

### Changed

- The Supervisor drops capability filters the core now applies itself (#74).
- Every integration and driver declares the capability bits it reports (#68).

### Fixed

- Supervisor: the host's OTLP environment variables are kept from the collector (#70).
- Collector extension: the health start time is kept (#70).

## Supervisor 0.3.0 (2026-10-21)

`go/opampsupervisor/v0.3.0`, on core 0.9.0.

### Fixed

- The config the Supervisor starts on is answered on the new collector's health; a failed first
  start is tried again (#66).
- Bound credentials are permitted in remote configs, and resumed offers are acknowledged (#60).
- A config sent as the Supervisor starts is kept (#57).
- The collector starts on what the OpAMP client resumes (#53, #54).
- No install over an update a start couldn't undo; APPLIED only for a running collector; a failed
  restart is tried again (#51).
- Safe own telemetry, no secrets in the effective config, and collector updates that survive a
  crash (#50); only what the running collector loaded and was healthy on is reported (#49).

## Earlier

`go/v0.1.0` (2026-10-16) and `go/opampsupervisor/v0.2.0` (2026-10-18) were the first tags. The
old `go/otel/v0.1.0` tag is of a module path that no longer exists (it is `go/opampotel` now).
