#!/usr/bin/env python3
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Generate the language clients' types for the SDK settings layer.

    scripts/gen_sdk_settings.py            # everything
    scripts/gen_sdk_settings.py --check    # fail if anything checked in is out of date (CI)

proto/opamp/sdk/v1/settings.proto -> each client's types. The layer's module
(opamp_sdk_settings.wasm) is the core's release: scripts/fetch_core.py puts it in place and checks
it.

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
PROTO = "opamp/sdk/v1/settings.proto"
JAVA = "java/opamp/src/main/java"
PY = "python/src/pydantic_opamp_client/_proto"
# The protoc of protobuf 6, as scripts/gen_core.py uses, so the code runs on protobuf 6 and 7.
PY_PROTOC = ["uvx", "--quiet", "--from", "grpcio-tools==1.75.0", "python", "-m", "grpc_tools.protoc"]
PY_PACKAGES = ["/opamp/sdk", "/opamp/sdk/v1"]
NODE = ROOT / "node" / "opamp"
GO_MODULE = "github.com/pydantic/opamp-clients/go"
DOTNET = "dotnet/Pydantic.OpAmp.Client"
# Every generated file, relative to the repository root. A language joins when its integration
# does.
TARGETS = [
    f"{JAVA}/dev/pydantic/opamp/sdk/SettingsProto.java",
    f"{PY}/opamp/sdk/v1/settings_pb2.py",
    f"{PY}/opamp/sdk/v1/settings_pb2.pyi",
    *(f"{PY}{d}/__init__.py" for d in PY_PACKAGES),
    "node/opamp/src/gen/opamp/sdk/v1/settings_pb.ts",
    "go/opamp/sdksettings/settingspb/settings.pb.go",
    f"{DOTNET}/Generated/Settings.cs",
]


def run(cmd: list[str]) -> None:
    print("+", " ".join(cmd), flush=True)
    subprocess.run(cmd, check=True, cwd=ROOT)


def build(out: Path) -> None:
    """Generate every target under `out` (at the same relative paths)."""
    (out / JAVA).mkdir(parents=True, exist_ok=True)
    run(["protoc", "-I", "spec/proto", "-I", "proto", f"--java_out=lite:{out / JAVA}", PROTO])

    # Python: under the client's package, so the imports are rewritten from `opamp.` to it.
    py = out / PY
    py.mkdir(parents=True, exist_ok=True)
    run([*PY_PROTOC, "-I", "spec/proto", "-I", "proto", f"--python_out={py}", f"--pyi_out={py}", PROTO])
    for d in PY_PACKAGES:
        (out / f"{PY}{d}/__init__.py").write_text("")
    # Go: the spec's messages are opamp-go's (their go_package says so). protoc writes the file
    # where the proto's go_package (.../go/opamp/sdk/settingspb) puts it; it is kept beside the
    # settings layer's package, go/opamp/sdksettings, and imported from there.
    with tempfile.TemporaryDirectory() as tmp:
        run(["protoc", "-I", "spec/proto", "-I", "proto", f"--go_out={tmp}", f"--go_opt=module={GO_MODULE}", PROTO])
        go = out / "go/opamp/sdksettings/settingspb/settings.pb.go"
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
    args = sys.argv[1:]
    if {"-h", "--help"} & set(args):
        print(__doc__)
        return 0
    if set(args) - {"--check"}:
        print(__doc__, file=sys.stderr)
        return 2
    if "--check" not in args:
        build(ROOT)
        return 0
    with tempfile.TemporaryDirectory() as tmp:
        build(Path(tmp))
        stale = [t for t in TARGETS
                 if not (ROOT / t).exists() or not filecmp.cmp(Path(tmp) / t, ROOT / t, shallow=False)]
    if stale:
        print("out of date (run scripts/gen_sdk_settings.py):", *stale, sep="\n  ")
        return 1
    print("generated files are up to date")
    return 0


if __name__ == "__main__":
    sys.exit(main())
