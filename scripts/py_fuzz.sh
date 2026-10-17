#!/usr/bin/env bash
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# Atheris (libFuzzer for Python) on the Python client's own code around the core: each target in
# python/tests/fuzz/ (fuzz_*.py), one after another, for SECONDS each. A target's corpus starts
# from its seeds (the SEEDS in its file) and every input under python/tests/fuzz/crashes/<target>/,
# and lives under target/shim-fuzz/python/<target>/ (with its log and what it found). A crash fails
# the run: copy the input it wrote (crash-*) into crashes/<target>/ and commit it, and every pytest
# runs it from then on (tests/test_fuzz_inputs.py).
#
#   scripts/py_fuzz.sh SECONDS [TARGET...]
#
# As every fuzzer here: an input that runs longer than FUZZ_TIMEOUT seconds (default 20) or needs more
# than FUZZ_RSS_MB of memory (default 2048) fails as a crash does.
set -euo pipefail

seconds=${1:?usage: py_fuzz.sh SECONDS [TARGET...]}
shift
timeout=${FUZZ_TIMEOUT:-20}
rss_mb=${FUZZ_RSS_MB:-2048}
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root/python"

targets=("$@")
if [ ${#targets[@]} -eq 0 ]; then
	for f in tests/fuzz/fuzz_*.py; do
		targets+=("$(basename "$f" .py)")
	done
fi

failed=()
for t in "${targets[@]}"; do
	work="$root/target/shim-fuzz/python/$t"
	mkdir -p "$work/corpus" "$work/artifacts"
	# The seeds, as files of the corpus; and every crash kept.
	uv run --python 3.13 python -c '
import importlib, sys
sys.path[:0] = ["tests/fuzz", "tests"]
for i, s in enumerate(importlib.import_module(sys.argv[1]).SEEDS):
    open(f"{sys.argv[2]}/seed-{i}", "wb").write(s)' "$t" "$work/corpus"
	if [ -d "tests/fuzz/crashes/$t" ]; then cp "tests/fuzz/crashes/$t"/* "$work/corpus/"; fi
	echo "== python $t, ${seconds}s"
	# Every target runs, whatever one before found.
	uv run --python 3.13 --with atheris==3.1.0 python "tests/fuzz/$t.py" \
		"-artifact_prefix=$work/artifacts/" "-max_total_time=$seconds" "-timeout=$timeout" \
		"-rss_limit_mb=$rss_mb" "$work/corpus" > "$work/fuzz.log" 2>&1 ||
		{ echo "::error::fuzz target python $t failed"; tail -n 80 "$work/fuzz.log"; failed+=("$t"); }
	grep -E '^(#[0-9]+.*DONE|Done [0-9]+ runs)' "$work/fuzz.log" | tail -n 1 || true
done
if [ ${#failed[@]} -gt 0 ]; then
	printf 'failed: %s\n' "${failed[@]}" >&2
	exit 1
fi
