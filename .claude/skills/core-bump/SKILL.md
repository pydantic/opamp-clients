---
name: core-bump
description: >
  Use when moving the clients to a new core release (a core/vX.Y.Z release appeared, a lock-bump
  PR is red with HTTP 404, or an SDK change needs behavior a newer core has). Covers editing
  opamp-core.lock.json with scripts/fetch_core.py lock/fetch/check, what else a bump PR may
  contain (the module copies fetch rewrites, regenerated types from gen_core.py and
  gen_sdk_settings.py), the adversary module in testdata/core, and which tests to run per SDK.
---

# Move the clients to a new core

The core is the state machine every client wraps: it decides what to send, what to save and
what to report, and each SDK supplies transport, storage and callbacks around it. It arrives
compiled, as this repository's `core/vX.Y.Z` releases, and `opamp-core.lock.json` pins one release
and the sha256 of every file in it. Sessions here never build or release a core; they move the
lock. Background: [docs/how-it-works.md](../../../docs/how-it-works.md) ("Where the core comes
from") and [AGENTS.md](../../../AGENTS.md) ("The core"). Public-repo rules and pre-push checks:
the [`steward`](../steward/SKILL.md) skill.

## 1. What a release holds, and where fetch puts it

| Release file | Placed at (`MODULES`, `TREE_PLACES` in `scripts/fetch_core.py`) |
|---|---|
| `opamp_core.wasm` | `go/opamp/internal/core/`, `node/opamp/core/`, `python/src/pydantic_opamp_client/`, `java/opamp/src/main/wasm/`, `dotnet/Pydantic.OpAmp.Client/` (checked in) |
| `opamp_sdk_settings.wasm` | `go/opamp/sdksettings/` and the same four other places (checked in) |
| `opamp_core_adversary.wasm` | `testdata/core/` (checked in; never shipped) |
| `native-<rid>-<lib>` | this machine's in `target/release/`; with `--all-platforms` every one in `dist/native/runtimes/<rid>/native/` |
| `tool-linux-x64-opamp-tap` | `target/release/opamp-tap` |
| `opamp-core-tree.tar.gz` | `spec/proto/`, `proto/`, `spec/shell-contract.md`, `tests/shell-tables/`, `testdata/sdk-settings/` (gitignored) |

The adversary module is a hostile stand-in for the core: every SDK's harness test
(`go/opamp/adversary_*_test.go`, `python/tests/test_adversary_properties.py`,
`node/opamp` `adversary.*.test`, `java/.../AdversaryRawTest.java`, `AdversaryPropertiesTest.java`,
`dotnet/...Tests/AdversaryPropertyTests.cs`) runs the client against it and expects the client to
survive and report a misbehaving core. Override its path with `OPAMP_ADVERSARY_WASM`. It changes
with every release, so it is part of every bump's diff; `fetch_core.py check` holds it to the lock
and `PUBLIC_ALLOWLIST` names it.

## 2. The bump

```bash
python3 scripts/fetch_core.py lock 0.5.0          # downloads the release, checks every file against
                                                  # its opamp-core.json, writes opamp-core.lock.json
python3 scripts/fetch_core.py fetch --all-platforms
python3 scripts/fetch_core.py check               # "the core in the tree is 0.5.0 (...), as opamp-core.lock.json pins"
python3 scripts/gen_core.py                       # regenerate types if the .proto files changed
python3 scripts/gen_sdk_settings.py
python3 scripts/gen_core.py --check && python3 scripts/gen_sdk_settings.py --check   # "generated files are up to date"
git status --short
```
Timings here: `lock` ~1 s, `fetch` ~3 s, `fetch --all-platforms` ~4 s (cached in `.opamp-core/<version>/`
after the first download). Re-running `lock` on the pinned version leaves the lock byte-identical.

A lock for a version not yet released fails with
`GET .../releases/tags/core/v9.9.9: HTTP 404 (the rate limit: set GH_TOKEN)`: the message blames
the rate limit, but 404 means the release doesn't exist yet. Set `GH_TOKEN` only for a real 403/429.

A **prerelease** (`core/vX.Y.Z-dev.N`, a GitHub prerelease) is locked the same way
(`lock 0.10.0-dev.1`). It lets main take a core change before the core's release, so an SDK change
that needs it can merge now. `registries.yml` and the Supervisor's `release.yml` refuse to publish
while the lock pins one, and a Go tag must not go on such a commit (the `sdk-release` skill): move
the lock to the release first.

`--from DIR` locks or fetches from a directory of release files (a candidate handed to you); CI
uses it when called with a candidate (`.github/actions/core`, `CORE_ARTIFACT`).

## 3. What the PR may contain

AGENTS.md and the README say a core move "changes only the lock". In practice the bump PR is the
lock plus everything the lock deterministically produces, and nothing hand-written:

- `opamp-core.lock.json`;
- the module copies `fetch` rewrites (the ten `.wasm` files above plus `testdata/core/opamp_core_adversary.wasm`);
- regenerated types when the `.proto` files changed: `go/opamp/internal/corepb/`,
  `go/opamp/sdksettings/settingspb/`, `node/opamp/src/gen/`, `python/src/pydantic_opamp_client/_proto/`,
  `java/opamp/src/main/java/` (`CoreProto.java`, `SettingsProto.java`), `dotnet/Pydantic.OpAmp.Client/Generated/`.
  CI's `core` job runs `gen_core.py --check` and `gen_sdk_settings.py --check` on the locked
  release, so a bump that changes protos and omits these is red.

Core 0.4.0's bump (#9) carried exactly those plus a `docs/sdk-settings.md` section describing the
new behavior. SDK code that adopts new core behavior is a separate change, or one PR that says so
(#24 carries the bump and every SDK's adoption together, because main can't pass on either alone).

Generation needs `protoc` 34.1 (CI pins `arduino/setup-protoc` `version: "34.1"`: generated code
records it), `protoc-gen-go` v1.36.11, `npm ci` in `node/opamp` (for `protoc-gen-es`), and `uvx`
(Python's types come from `grpcio-tools==1.75.0`). `gen_core.py` and `gen_sdk_settings.py` ignore
`--help` and regenerate in place.

## 4. Order

1. The core release exists (check `gh api 'repos/pydantic/opamp-clients/releases/tags/core%2Fv0.5.0' --jq '[.assets[].name]'`;
   `gh release view` uses GraphQL and gets 403 in sessions).
2. The bump PR, green. Merge needs the maintainer's own words in the thread; squash only.
3. SDK changes that need the new core, rebased on the bump (they can't pass on the old core).

## 5. Tests to run locally

All after `fetch` (each SDK loads the fetched core). Representative runs, with times measured here:

| SDK | Command | Notes |
|---|---|---|
| Go | `cd go && go test ./opamp/ -run Adversary -count=1` | ~1m45 incl. module downloads; CI: `go test -race` per shard |
| Go integration | `cd go/opampotel && go vet ./... && go test -race ./...` (as CI runs it) | its own module: run from `go/opampotel` |
| Python | `cd python && uv run --python 3.13 pytest -q tests/test_adversary_properties.py tests/test_sdk_settings.py` | 66 passed, 1 skipped, ~4 min |
| Node.js | `cd node/opamp && npm ci && npm exec -- tsc -p . && node --test dist/test/adversary*.test.js` | 8 pass, ~3.5 min; full: `npm test` |
| Java | `cd java/opamp && mvn -q -B package -Dtest=AdversaryRawTest -Dsurefire.failIfNoSpecifiedTests=false` | ~70 s |
| .NET | `cd dotnet && dotnet test Pydantic.OpAmp.Client.Tests --nologo` (as CI runs it; also `Pydantic.OpAmp.Extensions.Hosting.Tests`) | the .NET SDK download is refused in the sandbox: CI only |
| Collector | `cd go/opampextension && go vet ./... && go test -race ./...`; same in `go/opampsupervisor` (as CI runs it) | |
| Scripts | `python3 -m unittest scripts/test_native_dist.py scripts/test_sdk_release.py scripts/test_redact_mutants.py scripts/test_collector_release.py scripts/test_public_check.py` | 48 tests, ~2 s |

When the release changes `testdata/sdk-settings/` or `tests/shell-tables/`, run each SDK's
settings and shell-table tests too (`python/tests/test_shell_tables.py`, `test_sdk_settings.py`,
and their counterparts). Packaging: `python3 scripts/native_dist.py build` and `check` after
`fetch --all-platforms` (see [`sdk-release`](../sdk-release/SKILL.md)).

## 6. Before pushing

```bash
python3 scripts/public_check.py    # "everything here is allowed to be public"
```
plus the steward skill's grep. Describe the new core in the PR body only by what changes for the
clients (a new field, a check it now makes, a value it now refuses), never by where or how it was
made. Release notes for SDKs pick up the behavior changes (e.g. 0.4.0: a negative duration is now
an error at start in Go, Node.js and Java).

## Gotchas

- `fetch_core.py check` before any `fetch` lists every tree file as "not the one X has" and ends
  `run scripts/fetch_core.py fetch`. With a stale `dist/native` it fails until
  `fetch --all-platforms` runs again.
- `.opamp-core/`, `.opamp-core-candidate/`, `target/`, `dist/` and the tree's paths are gitignored:
  never force-add them.
- The lock's `commit` field is the build's commit as the release's manifest names it; copy it as
  `lock` writes it, don't edit it.
- CI caches `.opamp-core` keyed on the lock's hash, so a bump always downloads fresh.
- On a core candidate (CI called with `core-artifact`), the `core` job generates types instead of
  checking them and prints `git status`: that list is what the bump PR will change.
