#!/usr/bin/env bash
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# Jazzer (libFuzzer for the JVM) on the Java client's own code around the core: each target in
# java/opamp/src/test/java (ClientFuzz, CoreFuzz), one after another, for SECONDS each. A target's
# corpus starts from its SEEDS and every input under src/test/resources/fuzz/<target>/, and lives
# under target/shim-fuzz/java/<target>/ (with its log and what it found). A crash fails the run:
# copy the input it wrote (crash-*) into src/test/resources/fuzz/<target>/, name it in that
# directory's index file and commit both, and every mvn test runs it from then on (FuzzInputsTest).
#
#   scripts/java_fuzz.sh SECONDS [TARGET...]
#
# As every fuzzer here: an input that runs longer than FUZZ_TIMEOUT seconds (default 20) or needs more
# than FUZZ_RSS_MB of memory (default 2048) fails as a crash does. Only the client's own classes are
# instrumented (dev.pydantic.opamp.*): not the core's, compiled from its module, nor the protobufs.
set -euo pipefail

seconds=${1:?usage: java_fuzz.sh SECONDS [TARGET...]}
shift
timeout=${FUZZ_TIMEOUT:-20}
rss_mb=${FUZZ_RSS_MB:-2048}
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root/java/opamp"

targets=("$@")
[ ${#targets[@]} -gt 0 ] || targets=(ClientFuzz CoreFuzz)

mvn -q -B -Pjazzer test-compile dependency:build-classpath -Dmdep.outputFile=target/jazzer.classpath \
	-Dmdep.includeScope=test
cp="target/test-classes:target/classes:$(cat target/jazzer.classpath)"

failed=()
for t in "${targets[@]}"; do
	work="$root/target/shim-fuzz/java/$t"
	mkdir -p "$work/corpus" "$work/artifacts"
	java -cp "$cp" dev.pydantic.opamp.FuzzInputsTest "$t" "$work/corpus"
	echo "== java $t, ${seconds}s"
	# Every target runs, whatever one before found.
	java -Xmx1g -cp "$cp" com.code_intelligence.jazzer.Jazzer "--target_class=dev.pydantic.opamp.$t" \
		'--instrumentation_includes=dev.pydantic.opamp.*' "--reproducer_path=$work/artifacts" \
		"-artifact_prefix=$work/artifacts/" "-max_total_time=$seconds" "-timeout=$timeout" \
		"-rss_limit_mb=$rss_mb" "$work/corpus" > "$work/fuzz.log" 2>&1 ||
		{ echo "::error::fuzz target java $t failed"; tail -n 80 "$work/fuzz.log"; failed+=("$t"); }
	grep -E '^Done [0-9]+ runs' "$work/fuzz.log" | tail -n 1 || true
done
if [ ${#failed[@]} -gt 0 ]; then
	printf 'failed: %s\n' "${failed[@]}" >&2
	exit 1
fi
