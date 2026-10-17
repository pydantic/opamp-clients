#!/usr/bin/env bash
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# SharpFuzz (libFuzzer for .NET) on the .NET client's own code around the core: each target in
# dotnet/Pydantic.OpAmp.Client.Fuzz/Targets (ClientFuzz, CoreFuzz), one after another, for SECONDS
# each. The client's assembly is instrumented (sharpfuzz), not the protobufs nor the native core.
# A target's corpus starts from its seeds and every input under
# dotnet/Pydantic.OpAmp.Client.Fuzz/crashes/<target>/, and lives under target/shim-fuzz/dotnet/<target>/
# (with its log and what it found). A crash fails the run: copy the input it wrote (crash-*) into
# crashes/<target>/ and commit it, and every dotnet test runs it from then on (FuzzInputsTests).
#
#   scripts/dotnet_fuzz.sh SECONDS [TARGET...]
#
# As every fuzzer here: an input that runs longer than FUZZ_TIMEOUT seconds (default 20) or needs more
# than FUZZ_RSS_MB of memory (default 2048: the .NET process's GC heap is capped there too) fails as
# a crash does. Needs clang with libFuzzer (libclang-rt-dev), to build libfuzzer-dotnet from its
# pinned source; LIBFUZZER_DOTNET names one built already instead. OPAMP_NATIVE_LIB names a native
# core to use (else the project takes the one it does for the tests).
set -euo pipefail

seconds=${1:?usage: dotnet_fuzz.sh SECONDS [TARGET...]}
shift
timeout=${FUZZ_TIMEOUT:-20}
rss_mb=${FUZZ_RSS_MB:-2048}
root=$(cd "$(dirname "$0")/.." && pwd)
out="$root/target/shim-fuzz/dotnet"
project="$root/dotnet/Pydantic.OpAmp.Client.Fuzz"
mkdir -p "$out"

targets=("$@")
[ ${#targets[@]} -gt 0 ] || targets=(ClientFuzz CoreFuzz)

# libfuzzer-dotnet (github.com/Metalnem/libfuzzer-dotnet): libFuzzer driving a .NET process.
lf=${LIBFUZZER_DOTNET:-$out/libfuzzer-dotnet}
if [ ! -x "$lf" ]; then
	commit=bd39d4e88d715ab460a929943645be2a186cde52
	sum=90f019e2e9ad3a0b93c7ecc2c5afb2fbfc8b5aab6aac51c7e0d349ec79354f36
	curl -fsSL --retry 3 -o "$out/libfuzzer-dotnet.cc" \
		"https://raw.githubusercontent.com/Metalnem/libfuzzer-dotnet/$commit/libfuzzer-dotnet.cc"
	echo "$sum  $out/libfuzzer-dotnet.cc" | sha256sum -c --quiet
	clang -fsanitize=fuzzer -O1 -o "$lf" "$out/libfuzzer-dotnet.cc"
fi

# sharpfuzz, the instrumenter.
[ -x "$out/tools/sharpfuzz" ] ||
	dotnet tool install SharpFuzz.CommandLine --version 2.3.0 --tool-path "$out/tools" > "$out/tools.log"

# Built afresh, then the client's assembly instrumented in a copy (instrumenting twice fails).
native=()
[ -z "${OPAMP_NATIVE_LIB:-}" ] || native=("-p:OpAmpNativeLib=$OPAMP_NATIVE_LIB")
dotnet build "$project" -c Release --nologo -v quiet -o "$out/build" "${native[@]}"
rm -rf "$out/run"
cp -r "$out/build" "$out/run"
"$out/tools/sharpfuzz" "$out/run/Pydantic.OpAmp.Client.dll"
exe="$out/run/Pydantic.OpAmp.Client.Fuzz"

failed=()
for t in "${targets[@]}"; do
	work="$out/$t"
	mkdir -p "$work/corpus" "$work/artifacts"
	"$out/build/Pydantic.OpAmp.Client.Fuzz" seeds "$t" "$work/corpus" # instrumented, it runs only under the fuzzer
	if [ -d "$project/crashes/$t" ]; then cp "$project/crashes/$t"/* "$work/corpus/"; fi
	echo "== dotnet $t, ${seconds}s"
	# Every target runs, whatever one before found.
	DOTNET_GCHeapHardLimit=$(printf '0x%x' $((rss_mb * 1024 * 1024))) \
		"$lf" "--target_path=$exe" "--target_arg=$t" \
		"-artifact_prefix=$work/artifacts/" "-max_total_time=$seconds" "-timeout=$timeout" \
		"-rss_limit_mb=$rss_mb" "$work/corpus" > "$work/fuzz.log" 2>&1 ||
		{ echo "::error::fuzz target dotnet $t failed"; tail -n 80 "$work/fuzz.log"; failed+=("$t"); }
	grep -E '^(#[0-9]+.*DONE|Done [0-9]+ runs)' "$work/fuzz.log" | tail -n 1 || true
done
if [ ${#failed[@]} -gt 0 ]; then
	printf 'failed: %s\n' "${failed[@]}" >&2
	exit 1
fi
