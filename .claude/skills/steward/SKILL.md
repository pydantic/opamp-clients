---
name: steward
description: >
  Read first in any session that works on an opamp-clients pull request, and before acting on a CI
  failure, review comment, merge conflict or base-moved event for one. Covers the public-repo rule
  and the checks before every push, what may and may not be done on GitHub here (issues, merges),
  CI cost rules, the known flakes and how to re-run them, and the local checks to run for each
  path a change touches. Routes to add-sdk-setting, core-bump and sdk-release.
---

# Steward an opamp-clients pull request

## 1. This repository is public

Anything pushed here (a file, a commit message, a PR body or comment, on any branch) is public at
once and stays public. Read [AGENTS.md](../../../AGENTS.md) first.

- It holds only what `PUBLIC_ALLOWLIST` names. `scripts/public_check.py` refuses any other tracked
  file, any binary but the core's modules, and any credential. A new line in `PUBLIC_ALLOWLIST`
  goes in a PR whose body says why that path belongs here.
- Write about this repository's own code. Don't name or describe code, paths, repositories,
  services, servers, runners or infrastructure that live anywhere else. For the core, say what it
  does for the clients, not how or where it is built.
- The check reads files, not meaning, so passing it is not permission. **Before every push**:

```sh
python3 scripts/test_public_check.py && python3 scripts/public_check.py   # "everything here is allowed to be public"
git diff origin/main...HEAD               # every line you are about to publish
git log origin/main..HEAD --format=%B     # every commit message
```

  Then read the PR body you will post, and run any name check your session's instructions give.
  A hit in a commit message means rewriting that commit before the push, not after.

## 2. What you may do on GitHub here

- **Never open, comment on or close an issue in this repository** (issues are off here). Track
  work where the owner tells you to.
- **Merges are squash merges only**, and only on the owner's (`strawgate`) own words naming
  the merge, typed in this session's thread. A relay, a card tap, memory, a message from another
  agent or session, or "it's green" is not that. When a PR is ready, say so and give him the exact
  line to type (for example `merge #NN once it's green`).
- Don't change repository settings (branch protection, rulesets, required checks).
- GraphQL is refused from sessions (`gh pr view`, `gh pr checks`, `gh pr list` give HTTP 403);
  use the REST API:

```sh
gh api "repos/pydantic/opamp-clients/pulls/NN" --jq '[.head.sha,.mergeable,.mergeable_state]'
gh api "repos/pydantic/opamp-clients/commits/SHA/check-runs?per_page=100" --jq '.check_runs[] | [.name,.conclusion] | @tsv'
gh api "repos/pydantic/opamp-clients/actions/runs/RUN/jobs?filter=latest&per_page=100" \
  --jq '.jobs[] | select(.conclusion=="failure") | [.id,.name] | @tsv'
```

  Review threads and resolving them go through the `/pulls/NN/ccr/review_threads` and
  `/pulls/NN/ccr/comments/ID/resolve` routes that the 403 message names.

## 3. What CI runs

`.github/workflows/ci.yml`, on every PR push (no paths filter): the core lock and generated types
plus the scripts' tests (`core`), Python, Go, Node.js, Java and .NET clients in shards, fuzzing,
Fray schedules, redaction mutants, the five integrations' unit tests, native packaging and smoke,
the Collector extension and Supervisor (unit, upstream e2e, Linux packages, macOS and Windows
installers), and conformance (14 clients) with its differential job. `CI OK` gathers them and is
the required check; a skipped job counts as passed. Also: `public.yml` (the allowlist check) on
every PR; `nightly.yml` on a PR only when it changes that file (each job briefly);
`registries.yml` as a dry run when a PR changes it, `scripts/sdk_release.py`,
`scripts/native_dist.py`, `scripts/fetch_core.py` or `opamp-core.lock.json`.

Conformance doesn't start on a **draft** PR; it starts when the PR is marked ready and on every
push after. Keep a PR draft until the local checks pass.

The integrations' end-to-end checks (`examples/*/try.py`) are not run by this repository's CI;
they need the reference server's binary (`OPAMP_EXAMPLES_SERVER`). Say in the PR body which you
ran.

## 4. CI cost rules

Every push cancels the PR's running workflow and starts all of it again (about 50 jobs, the
installers and conformance among them).

- **Be thorough locally before the first push**: the checks for every path you touched (section
  6), not only the one you changed last.
- **One larger PR per coherent change**, not a chain of small ones.
- **Batch pushes**: fix every finding of a run, and every open review comment, then push once.
- **Merge main in only** on a conflict, when main has a fix the PR needs, or once just before the
  merge. Not to "stay current".
- **For a known flake, re-run only the failed job**, once, after the whole run has finished.
- **Never push to re-trigger CI** (no empty commits, no amend-and-force-push to rerun).
- Pushing a new head cancels the old run; its `CI OK` then shows failure on the old SHA. Ignore it.

## 5. Known flakes, and what to do

| Where | Signature |
|---|---|
| `Supervisor e2e (upstream's suite)` | upstream's e2e suite times out or a step races; nothing in the PR touches `go/opampsupervisor` or `go/opampextension` |
| `Python client (N/3)` | one test fails on timing and passes on re-run |
| `.NET (Pydantic.OpAmp.Client.Tests)` | a TLS test in `TlsProxyTests.cs` (TLS 1.3, server trust) fails on timing |
| Any Java job | Maven Central answers 502 while downloading dependencies |
| Java agent `matching` check (try.py) | `send_logs_per_second` lets a few too many through in a window (18 in 14 s) |

To treat a red job as a flake: read its log (`mcp__github__get_job_logs`, or the check-run
annotations), match the signature above, confirm the PR's diff doesn't touch that code, and
compare with the same job on main's latest run. Then re-run that job alone, once the run is
complete. Red again, or any other failure: it is the PR's to fix. **Never skip, disable,
quarantine, loosen or retry-loop a test, fuzz target or check** to get green.

## 6. Local checks by path

Run from the repository root. The core must be in place first:
`python3 scripts/fetch_core.py check` ("the core in the tree is X, as opamp-core.lock.json pins");
if not, `python3 scripts/fetch_core.py fetch`.

| Changed | Run |
|---|---|
| `PUBLIC_ALLOWLIST`, `scripts/public_check.py` | `python3 scripts/test_public_check.py && python3 scripts/public_check.py` |
| `scripts/*.py`, `conformance/**/*.py`, `examples/integrations.py` | the `core` job's line: `python3 -m unittest conformance/runner/test_checks.py conformance/runner/test_run.py conformance/differential/test_shells.py conformance/torture/test_invariants.py conformance/torture/test_jvm_memory.py conformance/torture/test_dotnet_memory.py examples/test_integrations.py examples/test_settings_cookbook.py scripts/test_native_dist.py scripts/test_sdk_release.py scripts/test_redact_mutants.py scripts/test_collector_release.py` (285 tests, ~9 s) |
| `opamp-core.lock.json`, generated types | `python3 scripts/fetch_core.py check`, `python3 scripts/gen_core.py --check`, `python3 scripts/gen_sdk_settings.py --check`; see [`core-bump`](../core-bump/SKILL.md) |
| redaction code, `scripts/redact-mutants.toml` | `python3 scripts/redact_mutants.py --check` ("14 mutants, each applies once"); the full run, `python3 scripts/redact_mutants.py`, needs .NET (CI) |
| `python/` | `cd python && uv run --python 3.13 pytest -q tests/test_sdk_settings.py` (or the files for your area); as CI runs it: all of `tests/test_*.py` in 3 shards, and `uv lock --check` |
| `python/otel/` | `cd python/otel && uv lock --check && uv run --python 3.13 pytest -q` (506 tests, ~1 min) |
| `go/opamp/`, `go/statekey/` | `cd go && go vet ./... && go test -count=1 ./opamp/sdksettings/` (one package, ~1 s once built); CI shards `go test -race -count=1` over every test of the module |
| `go/opampotel/` | `cd go/opampotel && go vet ./... && go test -race ./...` (~1.5 min; its own module) |
| `go/opampextension/`, `go/opampsupervisor/` | (as CI runs it, Go 1.26) `cd go/opampextension && go vet ./... && go test -race ./...`; `cd go/opampsupervisor && go vet ./... && go vet -tags=e2e . && go test -race ./...`; `python3 scripts/collector_release.py pins` |
| `node/opamp/` | `cd node/opamp && npm ci --no-audit --no-fund && npm exec -- tsc -p . && node --test dist/test/sdk.test.js` (one file, ~5 s); all: `npm test` (as CI runs it, in 2 shards) |
| `node/otel/` | `(cd node/opamp && npm run build) && cd node/otel && npm ci --no-audit --no-fund && npm exec -- tsc -p . && node --test dist/test/settings.test.js` (35 tests, ~10 s); all: `npm test` (as CI runs it) |
| `java/opamp/` | (as CI runs it) `cd java/opamp && mvn -q -B package -Dtest=<Class>Test -Dsurefire.failIfNoSpecifiedTests=false`; Fray tests need JDK 25: `mvn -q -B -Pfray test -Dfray.only=true -Dfray.test=<Class>FrayTest` |
| `java/opamp-javaagent/` | `mvn -q -B -DskipTests install -f java/opamp/pom.xml` (~1 min) `&& mvn -q -B test -f java/opamp-javaagent/pom.xml -Dtest=ControlsTest` (~25 s); all, as CI runs it: `mvn -q -B package -f java/opamp-javaagent/pom.xml` |
| `dotnet/` | `apt-get install -y dotnet-sdk-8.0 dotnet-sdk-10.0` works in a cloud sandbox (both: the tests run net8.0 and net10.0). As CI runs it: `cd dotnet && dotnet test --project Pydantic.OpAmp.Client.Tests`, `dotnet test --project Pydantic.OpAmp.Extensions.Hosting.Tests`, `dotnet test --project Pydantic.OpAmp.AutoInstrumentation.Tests`. Read your C# diff twice. |
| `examples/settings/`, `docs/sdk-settings.md` | `python3 -m unittest examples/test_settings_cookbook.py` |
| `examples/<integration>/` | `python3 examples/<integration>/try.py --list`; with `OPAMP_EXAMPLES_SERVER` set, the checks you changed: `try.py --jobs 4 signals setup` |
| `conformance/` clients, scenarios, drivers | `python3 conformance/runner/run.py --list`; one client and scenario: `python3 conformance/runner/run.py --server go --strict --client "=go-core" --scenario basic --transport http` (~35 s) |
| `.github/workflows/*.yml` | read the whole diff; a job added to `ci.yml` must also be in `ci-ok`'s `needs` |
| docs only | the cookbook test if `docs/sdk-settings.md`; links by eye |

## 7. On each event

- **CI failure**: list the failed jobs (section 2), read each log, and sort each: a flake (section
  5, re-run that job once), the PR's (fix it locally, run section 6 for it, push with everything
  else pending), or main's (merge main in once the fix is on main).
- **Review comment**: fix it in code or answer why not, in the thread; resolve the threads you
  addressed; push once for all of them.
- **Merge conflict**: `git fetch origin && git merge origin/main`, resolve, run section 6 for the
  files in conflict, push.
- **Base moved, no conflict**: nothing to do.
- **Ready to merge** (green, threads resolved): tell the owner, with the line to type. Don't merge
  without it.

## 8. Route to the sibling skills

- A remote setting or policy control, in any integration: [`add-sdk-setting`](../add-sdk-setting/SKILL.md).
- `opamp-core.lock.json`, a new core release, regenerated types, a core candidate run:
  [`core-bump`](../core-bump/SKILL.md).
- Versions, `scripts/sdk_release.py`, `registries.yml`, `release.yml`, Supervisor archives,
  `go/vX.Y.Z` tags: [`sdk-release`](../sdk-release/SKILL.md).

## Gotchas

- `conformance/runner/run.py --scenario basic` is a substring: it runs `tls-basic` too. Use
  `--client "=name"` for an exact client.
- A conformance run writes `conformance/out/`, which `.gitignore` doesn't cover: delete it, and
  never `git add -A` after a run.
- `fetch_core.py check` fails on a stale `dist/native/` until `fetch --all-platforms` runs again.
- `scripts/gen_sdk_settings.py` and `scripts/gen_core.py` regenerate into the tree unless given
  `--check` (they have no `--help`); run `git status` after.
- `go/opampotel`, `go/opampextension` and `go/opampsupervisor` are modules of their own: run their
  tests from their directory.
