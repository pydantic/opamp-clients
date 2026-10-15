#!/usr/bin/env python3
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Generate the language clients' types for the SDK settings layer, and, where the layer's
sources are, build its WebAssembly module.

    scripts/gen_sdk_settings.py            # everything
    scripts/gen_sdk_settings.py --check    # fail if anything checked in is out of date (CI)

- proto/opamp/sdk/v1/settings.proto -> each client's types.
- Where the sources are (a Cargo.toml at the root): the module (opamp_sdk_settings.wasm), copied
  into each client that ships it, so it is checked in. Built in the same pinned container as the
  client core's (scripts/gen_core.py), so it is the same bytes on every machine. Without the
  sources it is the core's release, which scripts/fetch_core.py puts in place and checks, and this
  generates and checks the types alone.

The layer is separate from the client core: its own module and interface, so either changes
without the other.
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
# Whether the layer's sources are here to build its module from.
CORE_SOURCE = (ROOT / "Cargo.toml").is_file()
RUST_IMAGE = "rust:1.95.0"
WASM_TARGET = ROOT / "target" / "wasm-build"
WASM = WASM_TARGET / "wasm32-unknown-unknown" / "release" / "opamp_sdk_settings_wasm.wasm"
PROTO = "opamp/sdk/v1/settings.proto"
JAVA = "java/opamp/src/main/java"
PY = "python/src/opamp_client/_proto"
# The protoc of protobuf 6, as scripts/gen_core.py uses, so the code runs on protobuf 6 and 7.
PY_PROTOC = ["uvx", "--quiet", "--from", "grpcio-tools==1.75.0", "python", "-m", "grpc_tools.protoc"]
PY_PACKAGES = ["/opamp/sdk", "/opamp/sdk/v1"]
NODE = ROOT / "node" / "opamp"
GO_MODULE = "github.com/pydantic/opamp-clients/go"
DOTNET = "dotnet/Pydantic.OpAmp.Client"
# Every generated file, relative to the repository root. A language joins when its integration
# does.
TARGETS = [
    "java/opamp/src/main/wasm/opamp_sdk_settings.wasm",
    f"{JAVA}/dev/pydantic/opamp/sdk/SettingsProto.java",
    "python/src/opamp_client/opamp_sdk_settings.wasm",
    f"{PY}/opamp/sdk/v1/settings_pb2.py",
    f"{PY}/opamp/sdk/v1/settings_pb2.pyi",
    *(f"{PY}{d}/__init__.py" for d in PY_PACKAGES),
    "node/opamp/core/opamp_sdk_settings.wasm",
    "node/opamp/src/gen/opamp/sdk/v1/settings_pb.ts",
    "go/opamp/sdk/opamp_sdk_settings.wasm",
    "go/opamp/sdk/settingspb/settings.pb.go",
    f"{DOTNET}/opamp_sdk_settings.wasm",
    f"{DOTNET}/Generated/Settings.cs",
]


def run(cmd: list[str]) -> None:
    print("+", " ".join(cmd), flush=True)
    subprocess.run(cmd, check=True, cwd=ROOT)


def build_wasm() -> None:
    flags = "--remap-path-prefix=/usr/local/cargo=cargo --remap-path-prefix=/work=."
    # linux/amd64 on every machine, the one place an emulated image is used on purpose: the
    # module's bytes depend on the host the compiler runs on (its code, not only symbol names),
    # and CI's runners and --check build on amd64.
    run(["docker", "run", "--rm", "--platform", "linux/amd64", "-v", f"{ROOT}:/work", "-w", "/work",
         "-v", "opamp-clients-rustup:/usr/local/rustup", "-v", "opamp-clients-cargo:/usr/local/cargo/registry",
         "-e", f"CARGO_TARGET_DIR=/work/{WASM_TARGET.relative_to(ROOT)}", "-e", f"RUSTFLAGS={flags}",
         RUST_IMAGE, "cargo", "build", "-q", "--locked", "-p", "opamp-sdk-settings-wasm", "--release",
         "--target", "wasm32-unknown-unknown"])


def fresh_wasm_target() -> None:
    """Empty the module's target directory, so that --check builds it from the sources alone, as
    scripts/gen_core.py's fresh_wasm_target says."""
    run(["docker", "run", "--rm", "--platform", "linux/amd64", "-v", f"{ROOT}:/work", RUST_IMAGE,
         "rm", "-rf", f"/work/{WASM_TARGET.relative_to(ROOT)}"])


def generated() -> list[str]:
    """The targets this generates here: the module only where the layer's sources are."""
    return [t for t in TARGETS if CORE_SOURCE or not t.endswith(".wasm")]


def build(out: Path) -> None:
    """Generate every target under `out` (at the same relative paths)."""
    if CORE_SOURCE:
        build_wasm()
        for t in TARGETS:
            if t.endswith(".wasm"):
                (out / t).parent.mkdir(parents=True, exist_ok=True)
                shutil.copy(WASM, out / t)
    (out / JAVA).mkdir(parents=True, exist_ok=True)
    run(["protoc", "-I", "spec/proto", "-I", "proto", f"--java_out=lite:{out / JAVA}", PROTO])

    # Python: under the client's package, so the imports are rewritten from `opamp.` to it.
    py = out / PY
    py.mkdir(parents=True, exist_ok=True)
    run([*PY_PROTOC, "-I", "spec/proto", "-I", "proto", f"--python_out={py}", f"--pyi_out={py}", PROTO])
    for d in PY_PACKAGES:
        (out / f"{PY}{d}/__init__.py").write_text("")
    # Go: the spec's messages are opamp-go's (their go_package says so).
    with tempfile.TemporaryDirectory() as tmp:
        run(["protoc", "-I", "spec/proto", "-I", "proto", f"--go_out={tmp}", f"--go_opt=module={GO_MODULE}", PROTO])
        go = out / "go/opamp/sdk/settingspb/settings.pb.go"
        go.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy(Path(tmp) / "opamp" / "sdk" / "settingspb" / "settings.pb.go", go)

    # C#: the spec's messages are the client's own (scripts/gen_core.py generates them).
    (out / DOTNET / "Generated").mkdir(parents=True, exist_ok=True)
    run(["protoc", "-I", "spec/proto", "-I", "proto", f"--csharp_out={out / DOTNET / 'Generated'}", PROTO])

    # TypeScript, with node/opamp's protoc-gen-es (run `npm ci` there first).
    plugin = NODE / "node_modules" / ".bin" / "protoc-gen-es"
    if not plugin.exists():
        sys.exit(f"{plugin} is missing: run `npm ci` in {NODE.relative_to(ROOT)}")
    gen = out / "node/opamp/src/gen"
    gen.mkdir(parents=True, exist_ok=True)
    run(["protoc", "-I", "spec/proto", "-I", "proto", f"--plugin=protoc-gen-es={plugin}", f"--es_out={gen}",
         "--es_opt=target=ts,import_extension=js", PROTO])

    package = PY.removeprefix("python/src/").replace("/", ".")
    for f in (py / "opamp" / "sdk").rglob("*_pb2.py*"):
        f.write_text(re.sub(r"^from opamp\.", f"from {package}.opamp.", f.read_text(), flags=re.M))


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
        print("out of date (run scripts/gen_sdk_settings.py):", *stale, sep="\n  ")
        return 1
    print("generated files are up to date")
    return 0


if __name__ == "__main__":
    sys.exit(main())
