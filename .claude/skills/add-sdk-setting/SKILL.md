---
name: add-sdk-setting
description: >
  Use when adding, changing or removing a remote setting (a Logfire SDK policy control or one of
  its fields) that the SDK integrations carry out, or when changing which integrations carry one
  out live or at the next start. Covers the policy contract, the host each integration declares,
  the code, tests (unit, shared vectors, torture) and runnable example in every integration, the
  examples/settings cookbook, the try.py end-to-end checks, and the docs, all in one PR.
---

# Add or change an SDK setting

## The owner's rules (they decide the shape of the PR)

- **One PR, every integration.** A setting ships with code, a runnable example and thorough tests
  in each integration in the same PR: Python (`python/otel`), Node.js (`node/otel`), Go
  (`go/opampotel`), the Java agent extension (`java/opamp-javaagent`), the .NET plugin
  (`dotnet/Pydantic.OpAmp.AutoInstrumentation`) and .NET `WithOpAmp()`
  (`dotnet/Pydantic.OpAmp.Extensions.Hosting`). Those are the six integrations of
  [docs/sdk-settings.md](../../../docs/sdk-settings.md#where-each-setting-applies). Some docs
  mention a Rust integration; it is not in this repository, so don't add or describe one.
- **Settings control the application's telemetry**: what is sampled, exported, redacted, capped,
  the application's log records by level. Never the SDK's own diagnostic logging or log level.
- **Prefer a control every integration carries out the same way.** Where one can't, it doesn't
  declare it (see the host below), so it never answers APPLIED for something not in force. Write
  the limit in the "Where each setting applies" table.
- Never skip, weaken or quarantine a test to land it.

## The contract

- The server sends one file, `config_map["logfire-sdk-policy.json"]` (`application/json`), with
  `"schema": "logfire-sdk-policy/v1"`, and only to an agent whose description advertises
  `logfire.opamp.config_format` = `logfire-sdk-policy/v1`. It sends an override only for a
  control the agent lists in `logfire.opamp.supported_controls`.
- A control is `{"mode": "local"}` (the SDK's own value) or `{"mode": "override", ...fields}`.
  `head_sampling` and `signal_exports` are required; any other control left out is local. Unknown
  fields, wrong types, a duplicate key, a missing or extra file: FAILED, nothing changes.
- An override this integration can't carry out where it runs is **FAILED**, never APPLIED and
  never "set aside": `<control>: this SDK cannot apply the requested override` (with the field
  for a multi-field control: `log_levels: scopes: ...`). Failure reasons start with the control,
  or `config_map:`, `schema:`, `policy:`, `can't save the settings`. A server sorts failures by
  these prefixes, so keep them.
- `elastic` is a compatibility format between Elastic's own (EDOT) SDKs and a server that chooses
  to support them. It is never our format: don't read, write, document or test it in our SDKs,
  examples or cookbook. If your base still has flat `elastic` documents in `examples/settings/`
  and an "elastic" section in `docs/sdk-settings.md`, the move to the policy has not landed on
  that base: check `git log origin/main --oneline -- docs/sdk-settings.md` and build on the
  policy, not on the old format.
- Each control field maps to an internal **setting name** (`head_sampling.ratio` is
  `sampling_rate`, `log_rate_limit.records_per_second` is `send_logs_per_second`, ...). Hosts,
  inventories and tests use the setting names. The mapping table is in docs/sdk-settings.md
  ("The controls"); keep it in step.

## Where the rules live

Parsing, validation, the effective config, the decision and `supported_controls` all come from
the **settings layer**: `opamp_sdk_settings.wasm`, shipped with the core release that
`opamp-core.lock.json` pins, with its interface `proto/opamp/sdk/v1/settings.proto` and the
shared vectors `testdata/sdk-settings/*.json`. `scripts/fetch_core.py fetch` places all of them;
they are gitignored. Never edit them here.

So a **new control or field needs a core release that has it first**:

```sh
python3 scripts/fetch_core.py check                    # what is in place is what the lock pins
grep -n '<setting_name>' proto/opamp/sdk/v1/settings.proto testdata/sdk-settings/settings.json
```

No hit: the layer doesn't know it yet; take the release that does with
[`core-bump`](../core-bump/SKILL.md) (a lock-only PR) before this work, or in the same PR if the
owner says so. Then regenerate each language's types:

```sh
python3 scripts/gen_sdk_settings.py           # writes Go, Node, Python, Java, .NET settings types
python3 scripts/gen_sdk_settings.py --check   # CI's check: "generated files are up to date"
```

What stays in each language is carrying it out (a sampler or gate that reads it), declaring it,
and testing it.

## Live or next start

- **Live**: in force at once, in the running process, through the sampler or the exporter gates.
- **Next start**: saved, in force after a restart. It needs a state directory; without one a
  policy that changes it is FAILED (`instrumentations: takes effect at the next start, which needs
  somewhere to keep state`). The effective config shows it local until the restart, and the
  decision lists it under `pending_restart`.
- The host decides per integration: a next-start setting listed under `live` is live everywhere
  (Go and Node.js list `deactivate_instrumentations` and `deactivate_all_instrumentations` there,
  because they can switch an instrumentation's tracer off at once). A live setting can't be listed
  under `next_start` (the layer refuses the host).

## The host: declare only what you carry out

Each integration passes a `Host{live, next_start}` to the layer. A setting left out is not
carried out, and its control is left out of `supported_controls`. Declare a signal's settings
only where every exporter of that signal goes through the integration's gate, and the sampler's
settings only where the integration's sampler is in force. When the application adds its own
processor, provider or sampler (`check_truth`'s setups), the integration narrows the host.

| Integration | Where the host is built | Carries it out |
|---|---|---|
| Python | `python/otel/src/pydantic_opamp_otel/__init__.py` (`settings_pb2.Host(live=..., next_start=...)`, `LOG_GATE`) | same file, `_match.py` |
| Node.js | `node/otel/src/index.ts` (`hostFromEnv`) | same file |
| Go | `go/opampotel/agent.go` (`host()`, `carried`) | `go/opampotel/controls.go`, `caps.go`, `levels.go`, `redact.go`, `match.go` |
| Java agent | `java/opamp-javaagent/.../javaagent/Extension.java` (`LIVE`, `NEXT_START`) | `Controls.java`, `LiveSampler.java`, `Redaction.java`, `TokenBucket.java` |
| .NET plugin | `dotnet/Pydantic.OpAmp.AutoInstrumentation/Agent.cs` (`new Host { ... }`) | `dotnet/shared/Controls.cs`, `Sampling.cs`, `Matching.cs`, `OtlpRewriter.cs` |
| .NET `WithOpAmp` | `dotnet/Pydantic.OpAmp.Extensions.Hosting/OpAmpAgent.cs`, `dotnet/shared/SdkAgent.cs` | `dotnet/shared/` |

## Find the files a real change touched

The fastest template is an existing setting of the same kind. A live log-gate setting:

```sh
git grep -l send_logs_per_second -- . ':!*.wasm'
```

lists about 60 files: every integration's host and gate, its tests, generated types, the wrappers'
tests, every `try.py`, `examples/integrations.py` and its tests, the cookbook file and README, and
six docs pages. For a whole cross-SDK change read the latest such PR on main:

```sh
git log --oneline origin/main -- examples/settings docs/sdk-settings.md
git show --stat 2a14a08   # #21: every integration declares only what reaches the telemetry
```

## Steps

1. **Layer.** Confirm the fetched layer knows the setting (above). Read its rows in
   `testdata/sdk-settings/*.json`: those are the cases every language must agree on.
2. **Decide per integration**: live, next start, or not carried out (and why). Write that row of
   "Where each setting applies" in `docs/sdk-settings.md` first; it is the spec for the rest.
3. **Carry it out and declare it** in each integration (table above).
4. **Tests in each integration**, each a test that fails without the change:
   - applied and in force; `local` puts the SDK's own value back; an override the host can't
     carry out is FAILED with the prefix above; the effective config reports it;
   - the shared vectors still pass (`go/opampotel/vectors_test.go`, Java `VectorsTest`,
     `AgreesWithLayerTest`, and each wrapper's settings test);
   - the inventory lists it where declared (`go/opampotel/inventory_test.go`, `InventoryTest.java`,
     `node/otel/test/inventory.test.ts`, `python/otel/tests/test_inventory.py`, .NET
     `InventoryTests.cs`);
   - the **torture test** flips it under load. Add the control to each one's document generator
     (Python's `change()` in `python/otel/tests/test_concurrency.py`): Go
     `TestSettingsFlippedWhileTelemetryFlows` (`go/opampotel/torture_test.go`), Node
     `node/otel/test/settings.torture.test.ts`, Java `ControlsTortureTest`, .NET
     `ConcurrencyTests.cs`.
5. **Cookbook.** Add `examples/settings/<use>.json` (a whole policy) and a README section that
   shows it byte for byte in a json block. `examples/test_settings_cookbook.py` reads the controls
   table in `docs/sdk-settings.md` and fails when a control (live or next start) has no example
   file or the README doesn't show a file exactly.
6. **End to end.** In `examples/integrations.py`, add the setting to `check_signals` (measured at
   the telemetry sink, not by trusting APPLIED), or to a new check function; add its row to the
   matching table in `examples/README.md`; cover the new harness logic in
   `examples/test_integrations.py`. A new check goes in each `examples/<integration>/try.py`
   `CHECKS` dict, and in exactly one of that integration's `checks:` lists in the workflow that
   runs the integration checks (examples/README.md): say in the PR body which. If a setup can take
   the setting out of the integration's hands, add it to that setup's expected set in try.py
   (`EXTENSION_PROCESSOR` in `examples/javaagent/try.py`, for one) so `check_truth` holds it.
7. **Docs per client**: `docs/sdk-settings.md` (controls table, mapping, where it applies, host),
   `docs/python-otel.md`, `node-otel.md`, `go-otel.md`, `javaagent.md`, `dotnet-otel.md`;
   `docs/available-components.md` if the inventory changes; `docs/app-settings.md` for
   `app_settings`.
8. **Local checks**, below. Then one PR (see [`steward`](../steward/SKILL.md) before pushing).

## Local checks (each run here; .NET as CI runs it)

```sh
python3 -m unittest examples/test_integrations.py examples/test_settings_cookbook.py   # ~5 s
(cd python && uv run --python 3.13 pytest -q tests/test_sdk_settings.py)                # 35 tests, <1 s
(cd python/otel && uv run --python 3.13 pytest -q)                                      # 506 tests, ~1 min
(cd python/otel && uv run --python 3.13 pytest -q tests/test_concurrency.py)            # torture, ~5 s
(cd go && go test -count=1 ./opamp/sdk/)
(cd go/opampotel && go vet ./... && go test -race ./...)                                     # own module, ~1.5 min
(cd go/opampotel && go test -race -count=1 -run TestSettingsFlippedWhileTelemetryFlows .)
(cd node/opamp && npm ci --no-audit --no-fund && npm run build)                         # node/otel needs it
(cd node/otel && npm ci --no-audit --no-fund && npm exec -- tsc -p . && node --test dist/test/settings.test.js)
mvn -q -B -DskipTests install -f java/opamp/pom.xml                                     # ~1 min, first
mvn -q -B test -f java/opamp-javaagent/pom.xml -Dtest=ControlsTest                      # or ControlsTortureTest
# .NET (as CI runs it; no dotnet here, CI is the only .NET build):
#   cd dotnet && dotnet test Pydantic.OpAmp.AutoInstrumentation.Tests --nologo
#   cd dotnet && dotnet test Pydantic.OpAmp.Extensions.Hosting.Tests --nologo
#   cd dotnet && dotnet test Pydantic.OpAmp.AutoInstrumentation.Tests --nologo --filter FullyQualifiedName~ConcurrencyTests
python3 examples/python-otel/try.py --list    # the checks; likewise node-otel, go-otel, javaagent, dotnet-otel
```

Running a check (`python3 examples/python-otel/try.py signals`, `--jobs 4 signals setup app`)
needs the reference OpAMP server's binary in `OPAMP_EXAMPLES_SERVER`; without it the check stops
with "the integration checks need their reference OpAMP server ... set OPAMP_EXAMPLES_SERVER".
This repository's CI does not run them, so say in the PR body which checks you ran end to end,
and which you could not. Java and .NET checks use Docker, or a local runtime with `--local`.

## Gotchas

- `scripts/gen_sdk_settings.py` has no `--help`: any argument other than `--check` still
  regenerates into the tree. Run `git status` after it.
- `proto/`, `spec/` and `testdata/sdk-settings/` are the core's, fetched and gitignored. A change
  there is a core change, never a hand edit.
- `go/opampotel` is its own module: run its tests from `go/opampotel`, not `go/`.
- `node/otel` loads the built `node/opamp`: `npm run build` there first, or tests fail on imports.
- The Java agent tests need the client installed into the local Maven repository first
  (`mvn -q -B -DskipTests install -f java/opamp/pom.xml`).
- A matcher that sees spans (rules, redaction) sees what the span has at start: `http.route` is
  added later by most instrumentations, so it is not a matcher. Each language matches with its
  own small matcher, held to the layer's by the vectors and the layer's `Evaluate` call.
- `debug` is in `supported_controls` only when the integration passes a clock (`now_unix_ms` not
  0) to `Describe`, `Load`, `Decide` and `Refresh`. Pass the same host and clock to all of them.
- A gate that covers only some exporters must not be declared: Python's `send_metrics` is
  declared only for the `otlp` exporter, Go's only for push readers.
- Two checks that can run at once under `--jobs` must name their runs apart (their files share
  `target/integrations/<integration>/`); `check_app_settings` uses `app7` and `app8` for that.
- APPLIED is not proof. `check_signals` and `check_truth` count what reaches the sink; a new
  setting gets a measured end-to-end assertion, not only a status check.
