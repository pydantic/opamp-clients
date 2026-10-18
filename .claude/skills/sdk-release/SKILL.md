---
name: sdk-release
description: >
  Use when releasing anything from this repository: the Go modules (go/vX.Y.Z and the nested
  modules' tags), a Supervisor release (go/opampsupervisor/vX.Y.Z: GitHub Release archives with
  checksums and provenance), the collector extension, or the SDK packages for PyPI, npm, Maven
  Central and NuGet (sdk/vX.Y.Z). Covers tag naming, who pushes tags, scripts/sdk_release.py,
  native_dist.py, collector_release.py, the release.yml and registries.yml workflows, and how to
  check a release once it is out.
---

# Release the SDKs, the Supervisor and the collector extension

Every release here starts from a tag on `main`, and **only the maintainer (`strawgate`) pushes
tags**: sessions get HTTP 403 on `git push` of a tag and on `POST git/refs`.
Never work around it. Prepare the commit, then give the maintainer the exact commands. Merges here are squash
merges only, and a merge or release needs the maintainer's own words in the thread that does it. Never open
an issue in this repository; track release work where the steward skill says. Public-repo rules:
[`steward`](../steward/SKILL.md). Core releases are not made here: see [`core-bump`](../core-bump/SKILL.md).

## 1. What each tag releases

| Tag | Workflow | What gets published |
|---|---|---|
| `go/vX.Y.Z` | none | the client module `github.com/pydantic/opamp-clients/go`, served by proxy.golang.org |
| `go/opampotel/vX.Y.Z`, `go/opampextension/vX.Y.Z` | none | those nested modules (the extension is used through an ocb manifest) |
| `go/opampsupervisor/vX.Y.Z` | `release.yml` | the module, plus a GitHub Release: six archives (linux, darwin, windows x amd64, arm64), `SHA256SUMS`, a provenance attestation per archive |
| `sdk/vX.Y.Z` | `registries.yml` | PyPI, npm, Maven Central, NuGet, each only once `SDK_REGISTRIES` names it |
| `core/vX.Y.Z` | not here | the core release the lock pins; never cut by a session in this repository |

The collector extension has no artifact of its own: it ships as the `go/opampextension` module
and inside each Supervisor archive's `otelcol`. There are no installers or hosted package
repositories yet: the deb, rpm, `.pkg` and `.msi` under `go/opampsupervisor/packaging/` are built
and tested in CI, not published.

## 2. Go modules

1. In a PR, make every nested `go.mod` (`go/opampotel`, `go/opampextension`, `go/opampsupervisor`)
   `require github.com/pydantic/opamp-clients/go vX.Y.Z`, the version about to be tagged. Keep each
   `replace ... => ../`: consumers ignore it, but a `v0.0.0` require breaks them. See
   [docs/sdk-registries.md](../../../docs/sdk-registries.md) (Go row). Squash-merge on the maintainer's line.
2. Hand the maintainer the tags for the merge commit, at most three per push (GitHub sends no push event
   for more), the client first and the Supervisor alone:
   ```bash
   git fetch origin main
   for t in go go/opampotel go/opampextension go/opampsupervisor; do git tag "$t/vX.Y.Z" <merge-sha>; done
   git push origin go/vX.Y.Z go/opampotel/vX.Y.Z go/opampextension/vX.Y.Z
   git push origin go/opampsupervisor/vX.Y.Z      # alone: it triggers release.yml
   ```
3. Check that the proxy serves them (each prints Path, Version, Time):
   ```bash
   for m in go go/opampotel go/opampextension go/opampsupervisor; do
     GOFLAGS=-mod=mod go list -m -json github.com/pydantic/opamp-clients/$m@vX.Y.Z; done
   ```
   The proxy caches a tag for good: a wrong tag is fixed by the next version, never by moving it.
   `go install` of the Supervisor stays refused (its `go.mod` has a `replace`), which is why it
   ships as archives.

## 3. Supervisor release (release.yml)

On a `go/opampsupervisor/v*` tag push, or `workflow_dispatch` with `version` for a tag already
pushed (the fix when the push carried more than three tags and no event fired). Each matrix job
checks out the tag, takes `packaging/archive.sh` from the workflow's own commit (so v0.1.0, which
predates the script, still builds), and runs it; the release job writes `SHA256SUMS`, attests, and
creates the release (or `--clobber`s the assets on a rerun). Build one archive locally (from the
script's header; not run in the sandbox):
```bash
VERSION=0.1.0 GOOS=linux GOARCH=amd64 go/opampsupervisor/packaging/archive.sh   # -> packaging/dist/
```
Check a published one (done for 0.1.0: checksum OK, attestation verifies, `otelcol components`
lists the `opamp` extension):
```bash
gh api 'repos/pydantic/opamp-clients/releases/tags/go%2Fopampsupervisor%2Fv0.1.0' --jq '[.assets[].name]'
curl -fsSLO https://github.com/pydantic/opamp-clients/releases/download/go%2Fopampsupervisor%2Fv0.1.0/SHA256SUMS
curl -fsSLO https://github.com/pydantic/opamp-clients/releases/download/go%2Fopampsupervisor%2Fv0.1.0/pydantic-opamp-supervisor_0.1.0_linux_amd64.tar.gz
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify pydantic-opamp-supervisor_0.1.0_linux_amd64.tar.gz --repo pydantic/opamp-clients
tar -xzf pydantic-opamp-supervisor_0.1.0_linux_amd64.tar.gz
./pydantic-opamp-supervisor_0.1.0_linux_amd64/otelcol components | grep -A1 'name: opamp'
```
Use REST (`gh api`): `gh release view` and `gh release download` go through GraphQL and get 403
in sessions. The archives are ~42-48 MB each.

## 4. Before moving the collector pins

The extension and Supervisor are forks of contrib's `extension/opampextension` and
`cmd/opampsupervisor` at one release; ocb needs every component on that release.
```bash
python3 scripts/collector_release.py pins                  # "every pin names contrib v0.163.0" (offline)
python3 scripts/collector_release.py latest --report-only  # "contrib v0.163.0 is the newest release" (~1 s)
```
`latest` without `--report-only` fails when contrib is newer and prints the upstream files changed
since our release: port that diff, then move every pin together (the `go.mod` requires under `go/`,
the ocb manifests, each `builder@vX` in CI, `archive.sh` and docs/collector.md), in one PR.

## 5. SDK packages (registries.yml)

Nothing has been released this way yet (no `sdk/*` tag); every manifest is at `0.1.0`. Before the
first one, the registry setup in [docs/sdk-registries.md](../../../docs/sdk-registries.md) must exist:
PyPI and npm trusted publishers (environment `pypi`, `npm`), NuGet's `NUGET_API_KEY` in the
`nuget` environment, and the repository variable `SDK_REGISTRIES` (e.g. `pypi npm nuget`). Maven
Central isn't wired up: its job fails if enabled. Sessions can't read Actions variables (403), so
ask the maintainer what `SDK_REGISTRIES` holds rather than assuming.

What the workflow runs, in order (a PR touching it, the scripts or the lock runs it all at 0.0.0
as a dry run that uploads nothing):
```bash
python3 scripts/fetch_core.py fetch --all-platforms   # every platform's native library
python3 scripts/sdk_release.py stamp X.Y.Z            # every manifest at X.Y.Z, ours pinned exactly
python3 scripts/native_dist.py build                  # dist/native: runtimes/<rid>/native/, wheels/ (~8 s here)
python3 scripts/native_dist.py pack                   # dist/native/nuget (needs dotnet)
python3 scripts/native_dist.py check
python3 scripts/sdk_release.py pack                   # dist/sdk: otel wheel, npm tarballs, Maven jars, .NET plugin
python3 scripts/sdk_release.py check                  # all there, no sdist, no core sources
twine check --strict dist/native/wheels/*.whl dist/sdk/wheels/*.whl
```
Locally, `native_dist.py build` works after `fetch --all-platforms`; `check` then fails with
`0 Pydantic.OpAmp.Core.Native packages, not 1` until `pack` has run, which needs the .NET SDK
(refused in the sandbox: CI only). Don't run `stamp` in a working tree you'll commit from: it
rewrites tracked manifests (and drops npm's `private` flag). Its behavior is covered by the tests.
`native_dist.py smoke` installs and runs the packages in Docker.

Release: merge what ships, then the maintainer pushes `git push origin sdk/vX.Y.Z` alone. Go has nothing to
upload (section 2), and the Rust client is not published.

## 6. Tests

```bash
python3 -m unittest scripts/test_sdk_release.py scripts/test_collector_release.py \
  scripts/test_native_dist.py scripts/test_redact_mutants.py scripts/test_public_check.py   # 48 tests, ~2 s
```

## Gotchas

- More than three tags in one push fires no workflow. The Go v0.1.0 tags went up four at once, so
  the Supervisor's release had to be run by hand with `version: 0.1.0`.
- A release is public at once, notes included. Notes describe this repository's code only.
- Registries keep every version for good, and a name on a registry can't be taken back: every
  scope or prefix (`pydantic-opamp-*`, `@pydantic/opamp-*`, `dev.pydantic`, `Pydantic.OpAmp.*`) must
  be ours first.
- The bundled collector reports an empty `version` (`collector.yaml`'s dist has none) and lists the
  extension as `v0.0.0` (its local replace).
- Workflow runs, environments and variables: read-only REST at best; dispatching a workflow is a
  release step that needs the maintainer's in-thread words.
