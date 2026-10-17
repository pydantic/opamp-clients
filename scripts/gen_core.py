#!/usr/bin/env python3
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Generate the language clients' interface types from the core's interface.

    scripts/gen_core.py                # everything
    scripts/gen_core.py --check        # fail if anything checked in is out of date (CI)

- The spec's messages and proto/opamp/core/v1/core.proto -> each client's types, with protoc:
  Go (protoc-gen-go, core.proto only: the messages come from opamp-go), TypeScript (protoc-gen-es,
  from node/opamp's dev dependencies: run `npm ci` there first), Python (the protoc of protobuf 6,
  through uvx), Java and C# (built in).

The core's modules, and the adversary's (testdata/core), are the core's release:
scripts/fetch_core.py puts them in place and checks them.
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
GO_MODULE = "github.com/pydantic/opamp-clients/go"
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


def build(out: Path) -> None:
    """Generate every target under `out` (at the same relative paths)."""
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
    with tempfile.TemporaryDirectory() as tmp:
        build(Path(tmp))
        stale = [t for t in TARGETS
                 if not (ROOT / t).exists() or not filecmp.cmp(Path(tmp) / t, ROOT / t, shallow=False)]
    if stale:
        print("out of date (run scripts/gen_core.py):", *stale, sep="\n  ")
        return 1
    print("generated files are up to date")
    return 0


if __name__ == "__main__":
    sys.exit(main())
