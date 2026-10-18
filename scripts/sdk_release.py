#!/usr/bin/env python3
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""An SDK release, as the Registries workflow makes it: every library at one version, packed for
its registry, and checked before anything is uploaded.

    scripts/sdk_release.py stamp 0.2.0    # every manifest at 0.2.0, ours pinned to each other
    scripts/sdk_release.py pack           # dist/sdk: what native_dist.py doesn't make
    scripts/sdk_release.py check          # dist/sdk and dist/native: all there, no cores' sources
    scripts/sdk_release.py bundle DIR ZIP # the Maven Central bundle of DIR's poms and jars, signed

The native core's wheels and NuGet packages come from scripts/native_dist.py (build, pack) first.
Go needs nothing packed (the module proxy serves the go/vX.Y.Z tag).
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import tarfile
import xml.etree.ElementTree as ET
import zipfile
from collections.abc import Callable
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DIST = ROOT / "dist" / "sdk"
NATIVE = ROOT / "dist" / "native"

VERSION = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$")

PYPROJECTS = ["python/pyproject.toml", "python/core/pyproject.toml", "python/otel/pyproject.toml"]
PACKAGE_JSONS = ["node/opamp/package.json", "node/otel/package.json"]
POMS = ["java/opamp/pom.xml", "java/opamp-javaagent/pom.xml"]
# The jars a pom builds beside its main one, by classifier: the agent extension's app-settings jar
# is what applications compile against (dev.pydantic.opamp.AppSettings).
CLASSIFIERS = {"java/opamp-javaagent/pom.xml": ["app-settings"]}
# The poms whose jar shades its dependencies in: what Central gets is the pom the shade plugin
# reduces (target/dependency-reduced-pom.xml), without the dependencies the jar already holds.
SHADED = {"java/opamp-javaagent/pom.xml"}
# What Central requires of every pom (central.sonatype.org/publish/requirements), as paths under
# <project>, and the jars beside each artifact's own.
POM_REQUIRED = ["name", "description", "url", "licenses/license/name", "developers/developer/name",
                "scm/url", "scm/connection"]
CENTRAL_JARS = ["", "-sources", "-javadoc"]
POM_NS = {"m": "http://maven.apache.org/POM/4.0.0"}
CSPROJS = ["dotnet/Pydantic.OpAmp.Client/Pydantic.OpAmp.Client.csproj",
           "dotnet/Pydantic.OpAmp.Core.Native/Pydantic.OpAmp.Core.Native.csproj",
           "dotnet/Pydantic.OpAmp.AutoInstrumentation/Pydantic.OpAmp.AutoInstrumentation.csproj",
           "dotnet/Pydantic.OpAmp.Extensions.Hosting/Pydantic.OpAmp.Extensions.Hosting.csproj"]

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
    tarballs, the Maven jars (with their poms, sources and javadoc jars, and the app-settings jar)
    and the .NET plugin."""
    for sub in ("wheels", "npm", "maven", "nuget"):
        (DIST / sub).mkdir(parents=True, exist_ok=True)
    run([sys.executable, "-m", "pip", "wheel", "--quiet", "--no-deps", "-w", str(DIST / "wheels"), "python/otel"], ROOT)
    for rel in ("node/opamp", "node/otel"):
        run(["npm", "ci", "--no-audit", "--no-fund"], ROOT / rel)
        run(["npm", "run", "build"], ROOT / rel)
        run(["npm", "pack", "--pack-destination", str(DIST / "npm")], ROOT / rel)
    # The release profile: the sources and javadoc jars Central requires beside each jar.
    run(["mvn", "-B", "-q", "-DskipTests", "-Prelease", "install", "-f", "java/opamp/pom.xml"], ROOT)
    run(["mvn", "-B", "-q", "-DskipTests", "-Prelease", "package", "-f", "java/opamp-javaagent/pom.xml"], ROOT)
    copy_maven(ROOT, DIST / "maven")
    run(["dotnet", "pack", CSPROJS[2], "-c", "Release", "--nologo", "-p:IsPackable=true", "-o", str(DIST / "nuget")],
        ROOT)


def copy_maven(root: Path, dest: Path) -> None:
    """What the poms under root built, into dest: each artifact's jar (the shaded one, for the
    agent extension), its sources and javadoc, its classified jars, and the pom Central gets."""
    for rel in POMS:
        target = (root / rel).parent / "target"
        pom = (target / "dependency-reduced-pom.xml" if rel in SHADED else root / rel).read_text()
        _, artifact, version = coordinates(pom, rel)
        name = f"{artifact}-{version}"
        for jar in [*(f"{name}{c}.jar" for c in CENTRAL_JARS), *(f"{name}-{c}.jar" for c in CLASSIFIERS.get(rel, []))]:
            (dest / jar).write_bytes((target / jar).read_bytes())
        (dest / f"{name}.pom").write_text(pom)


def coordinates(pom: str, where: str) -> tuple[str, str, str]:
    """The pom's own groupId, artifactId and version (not its parent's or a dependency's)."""
    project = ET.fromstring(pom)
    found = [project.findtext(f"m:{k}", namespaces=POM_NS) for k in ("groupId", "artifactId", "version")]
    if not all(found):
        sys.exit(f"sdk_release.py: {where}: no groupId, artifactId and version")
    return found[0].strip(), found[1].strip(), found[2].strip()


def maven_problems(maven: Path) -> list[str]:
    """What Maven Central would refuse in maven: a pom without the metadata it requires, a
    snapshot, or an artifact without its jar, sources jar or javadoc jar."""
    problems = []
    for pom in sorted(maven.glob("*.pom")):
        project = ET.fromstring(pom.read_text())
        _, artifact, version = coordinates(pom.read_text(), pom.name)
        problems += [f"{pom.name}: no {path}" for path in POM_REQUIRED
                     if not (project.findtext("m:" + path.replace("/", "/m:"), namespaces=POM_NS) or "").strip()]
        if version.endswith("-SNAPSHOT"):
            problems.append(f"{pom.name}: a snapshot")
        problems += [f"{pom.name}: no {artifact}-{version}{c}.jar" for c in CENTRAL_JARS
                     if not (maven / f"{artifact}-{version}{c}.jar").is_file()]
    return problems


def gpg_sign(path: Path) -> bytes:
    """An armored detached signature of path, by the key in gpg's keyring (the maven environment's
    MAVEN_GPG_PRIVATE_KEY, imported), its passphrase from MAVEN_GPG_PASSPHRASE."""
    cmd = ["gpg", "--batch", "--yes", "--armor", "--detach-sign", "--output", "-"]
    passphrase = os.environ.get("MAVEN_GPG_PASSPHRASE", "")
    if passphrase:
        cmd += ["--pinentry-mode", "loopback", "--passphrase-fd", "0"]
    done = subprocess.run([*cmd, str(path)], input=passphrase.encode(), capture_output=True, check=False)
    if done.returncode != 0:
        sys.exit(f"sdk_release.py: gpg can't sign {path.name}: {done.stderr.decode(errors='replace').strip()}")
    return done.stdout


def bundle(maven: Path, out: Path, sign: Callable[[Path], bytes] | None = gpg_sign) -> list[str]:
    """The Central Portal's upload bundle of maven's poms and jars, at out: each in the repository
    layout (dev/pydantic/<artifactId>/<version>/), with its .md5 and .sha1, and its .asc unless sign
    is None (a dry run, which Central would refuse). Refuses what Central would (maven_problems).
    Returns the bundle's names."""
    problems = maven_problems(maven)
    poms = sorted(maven.glob("*.pom"))
    if not poms:
        problems.append(f"no poms in {maven}")
    if problems:
        sys.exit("sdk_release.py: " + "; ".join(problems))
    names = []
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
        for pom in poms:
            group, artifact, version = coordinates(pom.read_text(), pom.name)
            base = f"{group.replace('.', '/')}/{artifact}/{version}/"
            for f in [pom, *sorted(maven.glob(f"{artifact}-{version}*.jar"))]:
                data = f.read_bytes()
                entries = {f.name: data, f"{f.name}.md5": hashlib.md5(data).hexdigest().encode(),
                           f"{f.name}.sha1": hashlib.sha1(data).hexdigest().encode()}
                if sign is not None:
                    entries[f"{f.name}.asc"] = sign(f)
                for name, content in entries.items():
                    z.writestr(base + name, content)
                    names.append(base + name)
    return names


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
            "Maven poms": list(dist.glob("maven/*.pom")),
            "Maven jars": list(dist.glob("maven/*.jar")),
            "NuGet packages": [*native.glob("nuget/*.nupkg"), *dist.glob("nuget/*.nupkg")]}
    # Two Maven artifacts, each with its jar, sources and javadoc, and the app-settings jar.
    minimum = {"PyPI wheels": 3, "npm tarballs": 2, "Maven poms": 2, "Maven jars": 7, "NuGet packages": 4}
    problems = [f"{len(got)} {kind}, not at least {minimum[kind]}" for kind, got in want.items()
                if len(got) < minimum[kind]]
    problems += maven_problems(dist / "maven")
    problems += [f"{p.name}: a source distribution" for p in [*native.glob("wheels/*.tar.gz"), *dist.glob("wheels/*.tar.gz")]]
    for p in sorted(p for kind, got in want.items() if kind != "Maven poms" for p in got):
        problems += [f"{p.name}: ships {n}" for n in members(p) if n.endswith(SOURCE)]
    return problems


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("stamp").add_argument("version")
    sub.add_parser("pack")
    sub.add_parser("check")
    b = sub.add_parser("bundle")
    b.add_argument("maven", type=Path, help="the poms and jars (dist/sdk/maven)")
    b.add_argument("out", type=Path, help="the bundle to write")
    b.add_argument("--unsigned", action="store_true", help="no signatures: a dry run's bundle, which Central refuses")
    args = p.parse_args()
    if args.cmd == "stamp":
        stamp(args.version)
    elif args.cmd == "pack":
        pack()
    elif args.cmd == "bundle":
        names = bundle(args.maven, args.out, None if args.unsigned else gpg_sign)
        print(f"{args.out}: {len(names)} files" + (", unsigned" if args.unsigned else ""))
    else:
        problems = check()
        for line in problems:
            print(line, file=sys.stderr)
        if problems:
            sys.exit(1)
        print("every registry's packages are there, and none carries the cores' sources")


if __name__ == "__main__":
    main()
