#!/usr/bin/env python3
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""An SDK release, as the Registries workflow makes it: every library at one version, packed for
its registry, and checked before anything is uploaded.

    scripts/sdk_release.py stamp 0.2.0    # every manifest at 0.2.0, ours pinned to each other
    scripts/sdk_release.py pack           # dist/sdk: what native_dist.py doesn't make
    scripts/sdk_release.py check          # dist/sdk and dist/native: all there, no cores' sources

The native core's wheels and NuGet packages come from scripts/native_dist.py (build, pack) first.
Go needs nothing packed (the module proxy serves the go/vX.Y.Z tag). Rust isn't published: the
client crate builds the core from source, which must not ship.
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
import tarfile
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DIST = ROOT / "dist" / "sdk"
NATIVE = ROOT / "dist" / "native"

VERSION = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$")

PYPROJECTS = ["python/pyproject.toml", "python/core/pyproject.toml", "python/otel/pyproject.toml"]
PACKAGE_JSONS = ["node/opamp/package.json", "node/otel/package.json"]
POMS = ["java/opamp/pom.xml", "java/opamp-javaagent/pom.xml"]
CSPROJS = ["dotnet/Pydantic.OpAmp.Client/Pydantic.OpAmp.Client.csproj",
           "dotnet/Pydantic.OpAmp.Core.Native/Pydantic.OpAmp.Core.Native.csproj",
           "dotnet/Pydantic.OpAmp.AutoInstrumentation/Pydantic.OpAmp.AutoInstrumentation.csproj"]

# What a package of ours must never hold: the cores' Rust sources and manifests.
SOURCE = (".rs", "Cargo.toml", "Cargo.lock")


def sub_once(pattern: str, repl: str, text: str, where: str) -> str:
    """text with pattern's one match replaced; a manifest that no longer matches fails loudly."""
    new, n = re.subn(pattern, repl, text, count=1, flags=re.M)
    if n != 1:
        sys.exit(f"sdk_release.py: {where}: no match for {pattern!r}")
    return new


def stamp(version: str, root: Path = ROOT) -> None:
    """Every manifest at version, and each of our packages asking for the others at exactly it:
    a release never mixes versions. Drops npm's private flag, which guards a checkout, not this."""
    if not VERSION.match(version):
        sys.exit(f"sdk_release.py: {version} isn't a version like 0.2.0")
    for rel in PYPROJECTS:
        p = root / rel
        text = sub_once(r'^version = "[^"]*"$', f'version = "{version}"', p.read_text(), rel)
        # Ours, in dependencies (indented list items, never the project's `name`): the client's
        # exact core, otel's exact client.
        text = re.sub(r'^(\s+)"(pydantic-opamp-(?:core|client))(?:==[^"]*)?"', rf'\1"\2=={version}"', text,
                      flags=re.M)
        p.write_text(text)
    names = [json.loads((root / rel).read_text())["name"] for rel in PACKAGE_JSONS]
    for rel in PACKAGE_JSONS:
        p = root / rel
        pkg = json.loads(p.read_text())
        pkg["version"] = version
        pkg.pop("private", None)
        for deps in ("dependencies", "peerDependencies", "optionalDependencies"):
            for name in pkg.get(deps, {}):
                if name in names:
                    pkg[deps][name] = version
        p.write_text(json.dumps(pkg, indent=2) + "\n")
    for rel in POMS:
        p = root / rel
        text = p.read_text()
        # The project's own version is the first after its artifactId; then ours in dependencies.
        text = sub_once(r"(</artifactId>\s*<version>)[^<]*(</version>)", rf"\g<1>{version}\g<2>", text, rel)
        text = re.sub(r"(<groupId>dev\.pydantic</groupId>\s*<artifactId>[^<]*</artifactId>\s*<version>)[^<]*",
                      rf"\g<1>{version}", text)
        p.write_text(text)
    for rel in CSPROJS:
        p = root / rel
        p.write_text(sub_once(r"<Version>[^<]*</Version>", f"<Version>{version}</Version>", p.read_text(), rel))


def run(cmd: list[str], cwd: Path) -> None:
    print("+", " ".join(cmd), flush=True)
    subprocess.run(cmd, check=True, cwd=cwd)


def pack() -> None:
    """The libraries native_dist.py doesn't make, into dist/sdk: the Python otel wheel, the npm
    tarballs, the Maven jars (with their poms) and the .NET plugin."""
    for sub in ("wheels", "npm", "maven", "nuget"):
        (DIST / sub).mkdir(parents=True, exist_ok=True)
    run([sys.executable, "-m", "pip", "wheel", "--quiet", "--no-deps", "-w", str(DIST / "wheels"), "python/otel"], ROOT)
    for rel in ("node/opamp", "node/otel"):
        run(["npm", "ci", "--no-audit", "--no-fund"], ROOT / rel)
        run(["npm", "run", "build"], ROOT / rel)
        run(["npm", "pack", "--pack-destination", str(DIST / "npm")], ROOT / rel)
    run(["mvn", "-B", "-q", "-DskipTests", "install", "-f", "java/opamp/pom.xml"], ROOT)
    run(["mvn", "-B", "-q", "-DskipTests", "package", "-f", "java/opamp-javaagent/pom.xml"], ROOT)
    for rel in POMS:
        pom = (ROOT / rel).read_text()
        found = re.search(r"</groupId>\s*<artifactId>([^<]*)</artifactId>\s*<version>([^<]*)</version>", pom)
        if found is None:
            sys.exit(f"sdk_release.py: {rel}: no artifactId and version")
        name = f"{found[1]}-{found[2]}"
        # The jar Maven publishes, beside its pom (the shaded one, for the agent extension).
        (DIST / "maven" / f"{name}.jar").write_bytes(((ROOT / rel).parent / "target" / f"{name}.jar").read_bytes())
        (DIST / "maven" / f"{name}.pom").write_text(pom)
    run(["dotnet", "pack", CSPROJS[2], "-c", "Release", "--nologo", "-p:IsPackable=true", "-o", str(DIST / "nuget")],
        ROOT)


def members(path: Path) -> list[str]:
    if path.suffix == ".tgz":
        with tarfile.open(path) as t:
            return t.getnames()
    with zipfile.ZipFile(path) as z:
        return z.namelist()


def check(dist: Path = DIST, native: Path = NATIVE) -> list[str]:
    """What's wrong with the release in dist and native: a registry missing its packages, or a
    package holding the cores' sources (or, for Python, being a source distribution)."""
    want = {"PyPI wheels": [*native.glob("wheels/*.whl"), *dist.glob("wheels/*.whl")],
            "npm tarballs": list(dist.glob("npm/*.tgz")),
            "Maven jars": list(dist.glob("maven/*.jar")),
            "NuGet packages": [*native.glob("nuget/*.nupkg"), *dist.glob("nuget/*.nupkg")]}
    minimum = {"PyPI wheels": 3, "npm tarballs": 2, "Maven jars": 2, "NuGet packages": 3}
    problems = [f"{len(got)} {kind}, not at least {minimum[kind]}" for kind, got in want.items()
                if len(got) < minimum[kind]]
    problems += [f"{p.name}: a source distribution" for p in [*native.glob("wheels/*.tar.gz"), *dist.glob("wheels/*.tar.gz")]]
    for p in sorted(p for got in want.values() for p in got):
        problems += [f"{p.name}: ships {n}" for n in members(p) if n.endswith(SOURCE)]
    return problems


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("stamp").add_argument("version")
    sub.add_parser("pack")
    sub.add_parser("check")
    args = p.parse_args()
    if args.cmd == "stamp":
        stamp(args.version)
    elif args.cmd == "pack":
        pack()
    else:
        problems = check()
        for line in problems:
            print(line, file=sys.stderr)
        if problems:
            sys.exit(1)
        print("every registry's packages are there, and none carries the cores' sources")


if __name__ == "__main__":
    main()
