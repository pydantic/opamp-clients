#!/usr/bin/env bash
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
#
# Go's native fuzzing of the Go client's own code around the core (go/opamp, its core package and
# the SDK settings layer): every Fuzz test, one after another, for SECONDS each. A crash fails the
# run, and the input that caused it is written under that package's testdata/fuzz/<test>/: commit
# it, and every go test runs it from then on.
#
#   scripts/go_fuzz.sh SECONDS [WORKERS] [TEST...]
#
# WORKERS is go test's -parallel (default: one per CPU); TEST names the tests to run (default: all).
set -euo pipefail

seconds=${1:?usage: go_fuzz.sh SECONDS [WORKERS] [TEST...]}
workers=${2:-}
shift
[ $# -gt 0 ] && shift
cd "$(dirname "$0")/../go"

# Go's fuzz coordinator (internal/fuzz.CoordinateFuzzing) stops on its -fuzztime context's Done
# and keeps the error unless it equals its child context's, but a context closes Done before it
# cancels its children: a stop that lands in between fails a run that found nothing with a bare
# "context deadline exceeded" (about one run in six of this script when the deadline lands
# while a worker is in a slow input). A crash always writes its input, so the failure is the
# engine's only when nothing was written and that line is all the test said.
deadline_race() { # TEST OUTPUT
	awk -v t="$1" '
		$0 ~ "^--- FAIL: " t " \\(" { inside = 1; next }
		inside && /^FAIL([ \t]|$)/ { inside = 0; next }
		inside { said[n++] = $0 }
		END { exit !(n == 1 && said[0] ~ /^[ \t]+context deadline exceeded$/) }
	' "$2"
}

out=$(mktemp)
trap 'rm -f "$out"' EXIT
failed=()
for pkg in ./opamp ./opamp/internal/core ./opamp/sdk; do
	tests=$(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true)
	[ -n "$tests" ] || { echo "no fuzz tests in $pkg" >&2; exit 1; }
	for t in $tests; do
		if [ $# -gt 0 ] && ! printf '%s\n' "$@" | grep -qx "$t"; then
			continue
		fi
		echo "== $pkg $t, ${seconds}s"
		corpus="$pkg/testdata/fuzz/$t"
		before=$(ls "$corpus" 2>/dev/null || true)
		# Every test runs, whatever one before found.
		if go test -run='^$' -fuzz="^${t}\$" -fuzztime="${seconds}s" ${workers:+-parallel=$workers} "$pkg" 2>&1 | tee "$out"; then
			continue
		fi
		if [ "$(ls "$corpus" 2>/dev/null || true)" = "$before" ] && deadline_race "$t" "$out"; then
			echo "::notice::$pkg $t: ran its ${seconds}s and found nothing; go's own stop reported the deadline as a failure"
			continue
		fi
		failed+=("$pkg $t")
	done
done
if [ ${#failed[@]} -gt 0 ]; then
	printf 'failed: %s\n' "${failed[@]}" >&2
	exit 1
fi
