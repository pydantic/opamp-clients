#!/usr/bin/env bash
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
#
# In the LazyFS container (run.sh): the repository (mounted read-only at /src) copied to /work,
# the drivers built natively, and the crash scenarios run with every state file on a LazyFS mount
# (conformance/runner/run.py --lazyfs): LAZYFS_SEEDS seeds from LAZYFS_SEED_BASE, each a real
# crash of every crashed run, then, with LAZYFS_SAVES=1, the scenarios that save, once, on LazyFS.
set -uo pipefail
mkdir -p /work /lazyfs /cache/target /results
tar -C /src --exclude=./.git --exclude=./target --exclude=node_modules --exclude=.venv --exclude=bin \
  --exclude=obj --exclude=out -cf - . | tar -C /work -xf -
cd /work
ln -s /cache/target target
# Without the core's sources (no Cargo.toml), the builds take what scripts/fetch_core.py put in
# target/release (the native library, opamp-tap), which the copy above leaves out.
if [ ! -f Cargo.toml ] && [ -d /src/target/release ]; then
  mkdir -p target/release && cp -a /src/target/release/. target/release/
fi
# Caches in /cache (a volume run.sh names): a second run builds little.
export GOMODCACHE=/cache/gomod GOCACHE=/cache/gobuild npm_config_cache=/cache/npm UV_CACHE_DIR=/cache/uv \
  UV_PROJECT_ENVIRONMENT=/cache/venv NUGET_PACKAGES=/cache/nuget MAVEN_OPTS="-Dmaven.repo.local=/cache/m2"
[ -d /opt/cargo ] && mkdir -p /cache/cargo && ln -sfn /cache/cargo /opt/cargo/registry

status=0
run() {
  python3 conformance/runner/run.py --lazyfs /lazyfs --clients-override conformance/lazyfs/native.toml \
    --client "$LAZYFS_CLIENTS" --jobs "$LAZYFS_JOBS" --strict "$@" || status=1
}
rm -f conformance/out/results.json
run --scenario =crash-mid-save,=crash-mid-download --crash-seed "$LAZYFS_SEED_BASE" --crash-seeds "$LAZYFS_SEEDS"
cp conformance/out/results.json /results/crash-results.json 2>/dev/null
if [ "${LAZYFS_SAVES:-1}" = 1 ]; then
  rm -f conformance/out/results.json
  run --scenario =remote-config-restart,=uid-assigned-restart,=request-uid-restart,=credential-offer,=packages,=restart-mid-download
  cp conformance/out/results.json /results/saves-results.json 2>/dev/null
fi
# Every run's logs and taps, for a failure's replay (the seed is in its directory's name).
tar -C conformance/out --exclude=./bin -cf - . | tar -C /results -xf -
python3 - <<'PY'
import json, pathlib
for name in ("crash-results.json", "saves-results.json"):
    p = pathlib.Path("/results", name)
    if not p.exists():
        continue
    records = json.loads(p.read_text())
    crashes = sum(1 for r in records if r.get("crash_seed") is not None)
    bad = [f"{r['client']}/{r['transport']} {r['scenario']}"
           + (f" seed {r['crash_seed']}" if r.get("crash_seed") is not None else "") + f": {k}: {v['detail']}"
           for r in records for k, v in r["results"].items() if v["status"] in ("fail", "error") and v["level"] == "MUST"]
    print(f"{name}: {len(records)} runs ({crashes} with crashes), {len(bad)} MUST failures")
    for b in bad:
        print("  " + b)
PY
chown -R "${HOST_IDS:-0:0}" /results 2>/dev/null
exit $status
