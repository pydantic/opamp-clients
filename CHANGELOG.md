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

### Breaking

- Go: the `go/logfirepolicyprocessor` module is gone; its last release is v0.2.0 (#80).
- Go, Python, Node.js, Java, .NET: an OpAMP endpoint without a scheme (`opamp.example.com/v1/opamp`)
  is refused. OpAMP stays off, the SDK runs unmanaged, and the core's warning names the setting.
  Add `https://` or `wss://`. On core 0.12.0, which the clients now pin (#92).

### Added

- .NET: `Pydantic.OpAmp.Client`, `Pydantic.OpAmp.Extensions.Hosting` and the automatic
  instrumentation plugin ship for .NET 10 as well as .NET 8 (`net8.0` and `net10.0`), and their
  tests run on both. Building from source needs the .NET 10 SDK (#86).
- Docs: a getting-started guide, the supported runtimes and platforms
  (`docs/supported-versions.md`), and a troubleshooting page (#86).
- Python (`pydantic-opamp-otel`): `configure_logfire()`, the Logfire SDK under the server, in place of
  `logfire.configure()`: the live sampler as its head sampler, and its exporters to Logfire behind
  the package's gates, so the SDK settings apply to what reaches Logfire (`logfire` extra).
- Go: an install's or a removal's error can say how it failed: `opamp.FailedForNow(err)` (an
  outage or a timeout: not reported failed, and tried again after a wait) or
  `opamp.FailedLeavingUnknown(err)` (reported failed, and what the agent has is uncertain), with
  `opamp.IsFailedForNow` and `opamp.IsFailedLeavingUnknown` to read them. A plain error fails the
  version for good, as before (#81).
- Supervisor: a container image, `ghcr.io/pydantic/opamp-supervisor`, for amd64 and arm64, with
  the collector, configured with environment variables and keeping its state on a volume; each
  Supervisor release publishes it, signed with Cosign (#93).
- Supervisor: `opampsupervisor status` prints how the running Supervisor is: the OpAMP
  connection (with what to check when it can't connect), the collector's health and the last
  remote config's status, from a status file it keeps in its storage directory. It exits 0 when
  all is well, so scripts and probes can use it (#93).

### Fixed

- .NET (`Pydantic.OpAmp.Extensions.Hosting`, the automatic instrumentation plugin): a health
  report given while the agent was starting was lost until the next one (30 s on with the default
  health check publisher); the first message now carries it (#86).
- Python: on Windows, the core tap (`OPAMP_CORE_TAP`) wrote its records in text mode, which broke
  them (#86).
- Python and .NET: on Windows, a save's rename is written through to the disk (`MoveFileEx` with
  `MOVEFILE_WRITE_THROUGH`), since a directory can't be flushed there: a save answered stored
  survives a power cut (#86).
- .NET: on Windows, a proxy variable the client refuses (as `ALL_PROXY=socks4://...`) is named as
  it was set in the error, not in lower case (#86).
- Java agent extension: under Elastic's distribution of the agent (EDOT Java), describing the
  SDK's sampler no longer overflows the stack when EDOT logs the SDK at start (EDOT's default
  sampler's own description never returns), and EDOT's span logger no longer sets the trace
  gate's controls aside while EDOT's own OpAMP client is off. Every check of the extension now
  passes under EDOT Java 1.14 (#83).

### Changed

- Go, Python, Node.js, Java, .NET: a state file saved by a newer core is left as it was (that run
  saves nothing), and the client says so once with the new `state_from_newer_version`
  notification, which every integration, the collector extension and the Supervisor log as a
  warning (#92).
- Node.js (`@pydantic/opamp-otel`): the register runs the instrumentations' loader hook for
  `import` in the application's thread (`module.registerHooks`) on Node.js 22.22.3, 24.11.1, 25.1.0,
  26 and later, instead of on a loader thread with a V8 isolate of its own: about 7 MB less
  resident memory for a small HTTP app, with the same instrumentations. Older Node.js keeps the
  loader thread (#91).
- Go: `go/opampotel` needs Go 1.26 or later (Go 1.25 is out of support), to take golang.org/x/net
  0.60.0.
- Supervisor: a server it can't reach is logged once a run of failures, naming what to check
  (`server.endpoint`, `server.headers` or `server.auth`, by the server's answer) and the endpoint
  without credentials; the retries go at debug, and getting through again is logged once. Before,
  every retry logged an error (#93).

### Security

- Python: on Windows, state files, downloads, the downloads directory and the core tap are made
  with an access-control list allowing the agent's user alone, as the .NET and Java clients make
  them; a mode of 0600 meant nothing there, so they took the directory's inherited list (#86).
- Go, Python, Node.js, Java, .NET (platform mode): the client certificate configured for the OpAMP
  server is presented only when a package file is at the server's own origin; a file anywhere else
  is fetched without it (and, in Go, without skipping verification). It was presented to every
  download host (#81).
- Go, Python, Node.js, Java, .NET (core mode): the same in core mode. Each TLS
  handshake the core asks for says whether it is with the server's own origin; only then is the
  client certificate presented (and, in Go, verification skipped where it is for the server). A
  file elsewhere, and TLS to an https:// proxy, get neither; the CAs still apply. On core
  0.11.0, which the clients now pin (#87).
- Node.js: undici 7.30.1, for GHSA-xjj6-f33q-929c, GHSA-4qv8-2p9j-2685 and GHSA-f72v-6w45-q472
  (#81).
- Dependencies with known advisories updated: golang.org/x/net 0.60.0 (`go/opampotel`, the
  examples, the conformance server), google.golang.org/grpc 1.83.1 and OpenTelemetry Go 1.45.0
  (the Go example, the conformance server), aiohttp 3.14.5 and OpenTelemetry Python 1.45.1
  (Python), ip-address 10.7.4 (Node.js), jackson-databind 2.22.3 (Java tests). Every lockfile is
  now checked against OSV on every change and nightly (`scripts/deps_scan.py`).

## Supervisor 0.5.0 (2026-10-25)

`go/opampsupervisor/v0.5.0`, on core 0.11.0. The Go modules are not released with it.

### Breaking

- Collector: the `logfirepolicy` processor (`go/logfirepolicyprocessor`) is gone, and collectors
  no longer take the Logfire SDK policy: a collector's remote config is its YAML. The Supervisor
  answers FAILED to a remote config holding `logfire-sdk-policy.json`, no longer puts the
  processor in the collector's pipelines, and no longer sets `logfire.opamp.config_format` or
  `logfire.opamp.supported_controls` in the agent description. The packaged collector doesn't
  carry the processor (#80).

### Changed

- Supervisor: hosts enrolled with `pydantic-opamp-enroll` (Linux, macOS, Windows) accept the server's
  restart command (`accepts_restart_command`), so the server can restart the host's collector
  (#88).
- Supervisor: a collector package whose signature Cosign can't check for now (Sigstore's trusted
  root or the transparency log unreachable) is tried again after a wait instead of being reported
  failed for good, and a rollback that can't put the previous executable back is reported failed
  (what the host has unknown) instead of not being reported at all (#81).

### Security

- The client certificate configured for the OpAMP server is presented only on TLS with the
  server's own origin; a package file elsewhere, and TLS to an https:// proxy, get neither (it was
  presented to every download host). The CAs still apply (#87).

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
