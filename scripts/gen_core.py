#!/usr/bin/env python3
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Generate the language clients' interface types, and, where the core's sources are, build the
core's WebAssembly modules they load.

    scripts/gen_core.py                # everything
    scripts/gen_core.py --check        # fail if anything checked in is out of date (CI)

- The spec's messages and proto/opamp/core/v1/core.proto -> each client's types, with protoc:
  Go (protoc-gen-go, core.proto only: the messages come from opamp-go), TypeScript (protoc-gen-es,
  from node/opamp's dev dependencies: run `npm ci` there first), Python (the protoc of protobuf 6,
  through uvx), Java and C# (built in).
- Where the core's sources are (a Cargo.toml at the root): the core's module, copied into each
  client that ships it, and the adversary's (testdata/core), the hostile stand-in for the core that
  every SDK's harness test loads (spec/shell-contract.md), never shipped. Both are checked in.
  Without the sources they are the core's release, which scripts/fetch_core.py puts in place and
  checks, and this generates and checks the types alone.
"""
from __future__ import annotations

import filecmp
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
# Whether the core's sources are here to build its modules from.
CORE_SOURCE = (ROOT / "Cargo.toml").is_file()
GO_MODULE = "github.com/pydantic/opamp-clients/go"
# The module is built in a pinned container, so it is the same bytes on every machine: the same
# compiler, the same host (build scripts are compiled for it, and their output paths carry a hash of
# it), the same paths. Needs Docker; on an arm64 machine the container runs emulated.
RUST_IMAGE = "rust:1.95.0"
WASM_TARGET = ROOT / "target" / "wasm-build"
WASM = WASM_TARGET / "wasm32-unknown-unknown" / "release" / "opamp_core_wasm.wasm"
ADVERSARY = WASM_TARGET / "wasm32-unknown-unknown" / "release" / "opamp_core_adversary.wasm"
ADVERSARY_TARGET = "testdata/core/opamp_core_adversary.wasm"
NODE = ROOT / "node" / "opamp"
PROTOS = ["opamp/v1/opamp.proto", "opamp/v1/anyvalue.proto", "opamp/core/v1/core.proto"]
PY = "python/src/opamp_client/_proto"
# protoc 31.1 (protobuf 6.31.1), from grpcio-tools.
PY_PROTOC = ["uvx", "--quiet", "--from", "grpcio-tools==1.75.0", "python", "-m", "grpc_tools.protoc"]
PY_PACKAGES = ["", "/opamp", "/opamp/v1", "/opamp/core", "/opamp/core/v1"]
JAVA = "java/opamp/src/main/java"
DOTNET = "dotnet/Pydantic.OpAmp.Client/Generated"
# Every generated file, relative to the repository root.
TARGETS = [
    "go/opamp/internal/core/opamp_core.wasm",
    "node/opamp/core/opamp_core.wasm",
    "python/src/opamp_client/opamp_core.wasm",
    "java/opamp/src/main/wasm/opamp_core.wasm",
    "dotnet/Pydantic.OpAmp.Client/opamp_core.wasm",
    ADVERSARY_TARGET,
    "go/opamp/internal/corepb/core.pb.go",
    *(f"node/opamp/src/gen/{p.replace('.proto', '_pb.ts')}" for p in PROTOS),
    *(f"{PY}{d}/__init__.py" for d in PY_PACKAGES),
    *(f"{PY}/{p.replace('.proto', suffix)}" for p in PROTOS for suffix in ("_pb2.py", "_pb2.pyi")),
    f"{JAVA}/opamp/proto/v1/Opamp.java",
    f"{JAVA}/opamp/proto/v1/Anyvalue.java",
    f"{JAVA}/dev/pydantic/opamp/core/CoreProto.java",
    f"{DOTNET}/Opamp.cs",
    f"{DOTNET}/Anyvalue.cs",
    f"{DOTNET}/Core.cs",
]


def run(cmd: list[str], **kw) -> None:
    print("+", " ".join(cmd), flush=True)
    subprocess.run(cmd, check=True, cwd=ROOT, **kw)


def protoc(*args: str) -> None:
    run(["protoc", "-I", "spec/proto", "-I", "proto", *args])


def build_wasm(packages: tuple[str, ...] = ("opamp-core-wasm", "opamp-core-adversary")) -> None:
    # Named volumes keep the toolchain and the crates between runs (Docker fills an empty one from
    # the image). --remap-path-prefix keeps the paths out of the module as well. v0 mangling puts
    # each generic instance's arguments in its name, which the core's stack check reads to bound a
    # recursion by the type it walks. One build per package: built together, their dependencies'
    # features would be unified, and the core's bytes would depend on the adversary's.
    flags = "--remap-path-prefix=/usr/local/cargo=cargo --remap-path-prefix=/work=. -C symbol-mangling-version=v0"
    for package in packages:
        # The core's shadow stack: 512 KiB, not wasm-ld's default 1 MiB. The core's stack check
        # bounds what it can need under that budget, and fails if the module reserves less.
        stack = " -C link-arg=-zstack-size=524288" if package == "opamp-core-wasm" else ""
        # linux/amd64 on every machine, the one place an emulated image is used on purpose: the
        # module's bytes depend on the host the compiler runs on (its code, not only symbol names),
        # and CI's runners and --check build on amd64.
        run(["docker", "run", "--rm", "--platform", "linux/amd64", "-v", f"{ROOT}:/work", "-w", "/work",
             "-v", "opamp-clients-rustup:/usr/local/rustup", "-v", "opamp-clients-cargo:/usr/local/cargo/registry",
             "-e", f"CARGO_TARGET_DIR=/work/{WASM_TARGET.relative_to(ROOT)}", "-e", f"RUSTFLAGS={flags}{stack}",
             RUST_IMAGE, "cargo", "build", "-q", "--locked", "-p", package, "--release",
             "--target", "wasm32-unknown-unknown"])


def fresh_wasm_target() -> None:
    """Empty the modules' target directory, so that --check builds them from the sources alone, as
    the core's release does: a reused one changes the modules' bytes. CI's
    rust-cache keeps target/, and a job that restored it failed this check with no core change.
    Removed in the container, since what it built there is root's."""
    run(["docker", "run", "--rm", "--platform", "linux/amd64", "-v", f"{ROOT}:/work", RUST_IMAGE,
         "rm", "-rf", f"/work/{WASM_TARGET.relative_to(ROOT)}"])


def generated() -> list[str]:
    """The targets this generates here: the modules only where the core's sources are."""
    return [t for t in TARGETS if CORE_SOURCE or not t.endswith(".wasm")]


def build(out: Path) -> None:
    """Generate every target under `out` (at the same relative paths)."""
    if CORE_SOURCE:
        build_wasm()
        for t in TARGETS:
            if t.endswith(".wasm"):
                (out / t).parent.mkdir(parents=True, exist_ok=True)
                shutil.copy(ADVERSARY if t == ADVERSARY_TARGET else WASM, out / t)

    # Go: only the core's messages; the spec's come from opamp-go.
    with tempfile.TemporaryDirectory() as tmp:
        protoc(f"--go_out={tmp}", f"--go_opt=module={GO_MODULE}", "opamp/core/v1/core.proto")
        go = out / "go/opamp/internal/corepb/core.pb.go"
        go.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy(Path(tmp) / "opamp" / "internal" / "corepb" / "core.pb.go", go)

    # TypeScript.
    plugin = NODE / "node_modules" / ".bin" / "protoc-gen-es"
    if not plugin.exists():
        sys.exit(f"{plugin} is missing: run `npm ci` in {NODE.relative_to(ROOT)}")
    gen = out / "node/opamp/src/gen"
    gen.mkdir(parents=True, exist_ok=True)
    protoc(f"--plugin=protoc-gen-es={plugin}", f"--es_out={gen}", "--es_opt=target=ts,import_extension=js", *PROTOS)

    # Python: generated under the package, so the imports are rewritten from `opamp.v1` to it. With
    # the protoc of protobuf 6 (PY_PROTOC), so the code runs on protobuf 6 and 7: OpenTelemetry's
    # Python packages before 1.43 require protobuf below 7.
    py = out / PY
    py.mkdir(parents=True, exist_ok=True)
    run([*PY_PROTOC, "-I", "spec/proto", "-I", "proto", f"--python_out={py}", f"--pyi_out={py}", *PROTOS])
    for d in PY_PACKAGES:
        (out / f"{PY}{d}/__init__.py").write_text("")
    package = PY.removeprefix("python/src/").replace("/", ".")
    for f in (out / PY).rglob("*_pb2.py*"):
        f.write_text(re.sub(r"^from opamp\.", f"from {package}.opamp.", f.read_text(), flags=re.M))

    # Java and C#.
    (out / JAVA).mkdir(parents=True, exist_ok=True)
    protoc(f"--java_out=lite:{out / JAVA}", *PROTOS)
    (out / DOTNET).mkdir(parents=True, exist_ok=True)
    protoc(f"--csharp_out={out / DOTNET}", *PROTOS)


def main() -> int:
    if "--check" not in sys.argv:
        build(ROOT)
        return 0
    if CORE_SOURCE:
        fresh_wasm_target()
    with tempfile.TemporaryDirectory() as tmp:
        build(Path(tmp))
        stale = [t for t in generated()
                 if not (ROOT / t).exists() or not filecmp.cmp(Path(tmp) / t, ROOT / t, shallow=False)]
    if stale:
        print("out of date (run scripts/gen_core.py):", *stale, sep="\n  ")
        return 1
    print("generated files are up to date")
    return 0


if __name__ == "__main__":
    sys.exit(main())
