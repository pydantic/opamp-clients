#!/usr/bin/env python3
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The native core for every platform it ships to: the libraries the .NET package carries, and the
Python wheels (pydantic-opamp-core, one per platform holding just the library, and
pydantic-opamp-client, pure Python), on one Linux x64 machine, then checked and run where they will
run.

    scripts/native_dist.py build    # dist/native: runtimes/<rid>/native/, wheels/
    scripts/native_dist.py pack     # dist/native/nuget: Pydantic.OpAmp.Client with every runtime, and
                                    # Pydantic.OpAmp.Extensions.Hosting on it
    scripts/native_dist.py check    # each binary is the format and machine its name says
    scripts/native_dist.py smoke    # install and run them in Docker: glibc and musl, x64 and arm64

build packs the libraries the core's release carries, which `scripts/fetch_core.py fetch
--all-platforms` puts in dist/native/runtimes (glibc 2.17, so the manylinux wheels and the .NET
libraries run on any distribution since). macOS and Windows binaries are checked here but run
nowhere: CI has only Linux runners. Arm64 runs under QEMU.
"""
from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
import tempfile
import time
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DIST = ROOT / "dist" / "native"
PYTHON = ROOT / "python"
sys.path.insert(0, str(PYTHON / "core"))
import build_backend  # noqa: E402  (python/core: writes pydantic-opamp-core's wheels)


@dataclass(frozen=True)
class Target:
    rid: str  # .NET's runtime identifier
    rust: str  # the Rust target (with zigbuild's glibc version, if any)
    tool: str  # zigbuild or xwin
    plat: str  # pydantic-opamp-core's wheel's platform tag
    lib: str  # the library's file name
    magic: tuple[str, str]  # what `file` says of it: the format, and the machine


ELF = "ELF 64-bit LSB shared object"
TARGETS = [
    Target("linux-x64", "x86_64-unknown-linux-gnu.2.17", "zigbuild", "manylinux_2_17_x86_64.manylinux2014_x86_64",
           "libopamp_native.so", (ELF, "x86-64")),
    Target("linux-arm64", "aarch64-unknown-linux-gnu.2.17", "zigbuild", "manylinux_2_17_aarch64.manylinux2014_aarch64",
           "libopamp_native.so", (ELF, "aarch64")),
    Target("linux-musl-x64", "x86_64-unknown-linux-musl", "zigbuild", "musllinux_1_2_x86_64", "libopamp_native.so",
           (ELF, "x86-64")),
    Target("linux-musl-arm64", "aarch64-unknown-linux-musl", "zigbuild", "musllinux_1_2_aarch64", "libopamp_native.so",
           (ELF, "aarch64")),
    # Rust's own deployment targets: macOS 10.12 on x64, 11 on arm64.
    Target("osx-x64", "x86_64-apple-darwin", "zigbuild", "macosx_10_12_x86_64", "libopamp_native.dylib",
           ("Mach-O 64-bit", "x86_64")),
    Target("osx-arm64", "aarch64-apple-darwin", "zigbuild", "macosx_11_0_arm64", "libopamp_native.dylib",
           ("Mach-O 64-bit", "arm64")),
    Target("win-x64", "x86_64-pc-windows-msvc", "xwin", "win_amd64", "opamp_native.dll", ("PE32+ executable", "x86-64")),
    Target("win-arm64", "aarch64-pc-windows-msvc", "xwin", "win_arm64", "opamp_native.dll", ("PE32+ executable", "Aarch64")),
]
# OPAMP_NATIVE_TARGETS=linux-x64,linux-musl-x64: only those (a machine that can't fetch the MSVC
# CRT, say). CI builds them all.
def only(targets: list[Target], rids: str) -> list[Target]:
    """The targets named, refusing a name that isn't one (else a typo builds and checks nothing)."""
    wanted = rids.split(",")
    if unknown := sorted(set(wanted) - {t.rid for t in targets}):
        sys.exit(f"OPAMP_NATIVE_TARGETS: no target {', '.join(unknown)}")
    return [t for t in targets if t.rid in wanted]


if rids := os.environ.get("OPAMP_NATIVE_TARGETS"):
    TARGETS = only(TARGETS, rids)


def run(cmd: list[str], **kw) -> None:
    print("+", " ".join(cmd), flush=True)
    subprocess.run(cmd, check=True, **kw)


def fetched_libs(runtimes: Path) -> list[tuple[Target, Path]]:
    """Every target's library where scripts/fetch_core.py fetch --all-platforms put it."""
    libs = [(t, runtimes / t.rid / "native" / t.lib) for t in TARGETS]
    if missing := [str(lib.relative_to(ROOT)) for _, lib in libs if not lib.is_file()]:
        sys.exit("no " + ", ".join(missing) + ": run scripts/fetch_core.py fetch --all-platforms")
    return libs


def build() -> None:
    for d in ("wheels", "nuget"):
        shutil.rmtree(DIST / d, ignore_errors=True)
    libs = fetched_libs(DIST / "runtimes")
    (DIST / "wheels").mkdir(parents=True)
    for t, lib in libs:
        print("+ wheel", build_backend.wheel(lib, t.plat, DIST / "wheels").name, flush=True)
    # The client: pure Python, one wheel for every platform (its dependency on the core is a name
    # and a version; pip finds the core's wheel for the machine).
    run([sys.executable, "-m", "pip", "wheel", "--quiet", "--no-deps", "-w", str(DIST / "wheels"), str(PYTHON)])


def file_says(path: Path) -> str:
    return subprocess.run(["file", "-b", str(path)], check=True, capture_output=True, text=True).stdout.strip()


BINARY = (".so", ".dylib", ".dll", ".pyd", ".wasm")
# What goes with the core wherever it is shipped: its license, and the notices beside it.
CORE_LICENSE = ("LICENSE.txt", "THIRD_PARTY_NOTICES.txt")


def check() -> None:
    """Each library, in the runtimes and in its core wheel, is the format and machine its target
    says; every target has both, with the core's license and notices; the client's wheel is
    Python alone; and nothing carries the cores' sources."""
    import zipfile

    problems = []
    wheels = sorted((DIST / "wheels").glob("*.whl"))
    for t in TARGETS:
        lib = DIST / "runtimes" / t.rid / "native" / t.lib
        if not lib.exists():
            problems.append(f"{t.rid}: no {lib.name}")
        elif not all(m in (says := file_says(lib)) for m in t.magic):
            problems.append(f"{t.rid}: {lib.name} is {says!r}, not {t.magic}")
        elif "not stripped" in says:
            problems.append(f"{t.rid}: {lib.name} has its symbol table")
        found = [w for w in wheels if w.name.startswith("pydantic_opamp_core-") and w.name.endswith(f"-py3-none-{t.plat}.whl")]
        if len(found) != 1:
            problems.append(f"{t.rid}: {len(found)} core wheels for {t.plat}")
            continue
        with zipfile.ZipFile(found[0]) as z, tempfile.TemporaryDirectory() as d:
            libs = [n for n in z.namelist() if not n.startswith("pydantic_opamp_core-")]
            if libs != [f"pydantic_opamp_core/{t.lib}"]:
                problems.append(f"{found[0].name}: holds {libs}, not pydantic_opamp_core/{t.lib} alone")
                continue
            licenses = {n.split("/", 2)[2] for n in z.namelist() if n.split("/")[0].endswith(".dist-info")
                        and n.split("/")[1] == "licenses"}
            problems += [f"{found[0].name}: lacks .dist-info/licenses/{f}" for f in CORE_LICENSE if f not in licenses]
            if not all(m in (says := file_says(Path(z.extract(libs[0], d)))) for m in t.magic):
                problems.append(f"{found[0].name}: {libs[0]} is {says!r}, not {t.magic}")
    clients = [w for w in wheels if w.name.startswith("pydantic_opamp_client-")]
    if [w.name.endswith("-py3-none-any.whl") for w in clients] != [True]:
        problems.append(f"client wheels {[w.name for w in clients]}, not one for every platform")
    for w in clients:
        with zipfile.ZipFile(w) as z:
            problems += [f"{w.name}: ships {n}" for n in z.namelist() if n.endswith(BINARY)]
    # .NET the same way: the core's package holds every target's library, with the core's license
    # (its license file) and notices, and nothing else; the client's holds no binary but its
    # assembly, and asks for that core package.
    for name in ("Pydantic.OpAmp.Core.Native", "Pydantic.OpAmp.Client", "Pydantic.OpAmp.Extensions.Hosting"):
        if (n := len(list((DIST / "nuget").glob(f"{name}.*.nupkg")))) != 1:
            problems.append(f"{n} {name} packages, not 1")
    for p in (DIST / "nuget").glob("Pydantic.OpAmp.Core.Native.*.nupkg"):
        with zipfile.ZipFile(p) as z:
            got = sorted(n for n in z.namelist() if not nuget_meta(n))
            spec = z.read(next(n for n in z.namelist() if n.endswith(".nuspec"))).decode()
        want = sorted([*CORE_LICENSE, *(f"runtimes/{t.rid}/native/{t.lib}" for t in TARGETS)])
        if got != want:
            problems.append(f"{p.name}: holds {got}, not {want}")
        if f'<license type="file">{CORE_LICENSE[0]}</license>' not in spec:
            problems.append(f"{p.name}: its license isn't {CORE_LICENSE[0]}")
    for p in (DIST / "nuget").glob("Pydantic.OpAmp.Client.*.nupkg"):
        with zipfile.ZipFile(p) as z:
            problems += [f"{p.name}: ships {n}" for n in z.namelist() if n.startswith("runtimes/") or (n.endswith(BINARY) and not n.startswith("lib/"))]
            problems += [f"{p.name}: lacks lib/{tfm}/Pydantic.OpAmp.Client.dll" for tfm in FRAMEWORKS
                         if f"lib/{tfm}/Pydantic.OpAmp.Client.dll" not in z.namelist()]
            spec = z.read(next(n for n in z.namelist() if n.endswith(".nuspec"))).decode()
        if f'id="Pydantic.OpAmp.Core.Native" version="[{core_version()}]"' not in spec:
            problems.append(f"{p.name}: doesn't depend on Pydantic.OpAmp.Core.Native {core_version()} exactly")
    # The hosting package: its own assembly (and its docs) alone, on the client's package. What it
    # shares with the plugin (dotnet/shared) is compiled into it, never shipped as source.
    for p in (DIST / "nuget").glob("Pydantic.OpAmp.Extensions.Hosting.*.nupkg"):
        assemblies = [f"lib/{tfm}/Pydantic.OpAmp.Extensions.Hosting" for tfm in FRAMEWORKS]
        with zipfile.ZipFile(p) as z:
            payload = [n for n in z.namelist() if not nuget_meta(n)]
            spec = z.read(next(n for n in z.namelist() if n.endswith(".nuspec"))).decode()
        problems += [f"{p.name}: ships {n}" for n in payload
                     if n not in [f"{a}.{ext}" for a in assemblies for ext in ("dll", "xml")]]
        problems += [f"{p.name}: lacks {a}.dll" for a in assemblies if f"{a}.dll" not in payload]
        if 'id="Pydantic.OpAmp.Client"' not in spec:
            problems.append(f"{p.name}: doesn't depend on Pydantic.OpAmp.Client")
    # The cores ship compiled only: no source distribution, and no wheel or package holds Rust
    # sources or manifests.
    problems += [f"{p.name}: a source distribution" for p in (DIST / "wheels").glob("*.tar.gz")]
    for p in [*wheels, *(DIST / "nuget").glob("*.nupkg")]:
        with zipfile.ZipFile(p) as z:
            problems += [f"{p.name}: ships {n}" for n in z.namelist() if n.endswith((".rs", "Cargo.toml", "Cargo.lock"))]
    for p in problems:
        print(p, file=sys.stderr)
    if problems:
        sys.exit(1)
    print(f"{len(TARGETS)} libraries and {len(wheels)} wheels are what their names say")


# The frameworks each .NET package ships an assembly for: .NET 8 and .NET 10, the supported LTS
# releases (docs/supported-versions.md). Packed on an SDK older than 10, a package holds net8.0
# alone, and check refuses it.
FRAMEWORKS = ("net8.0", "net10.0")


def nuget_meta(name: str) -> bool:
    """Whether NuGet itself writes `name` into every package: anything else is the package's payload."""
    return (name == "[Content_Types].xml" or name.startswith(("_rels/", "package/services/metadata/"))
            or ("/" not in name and name.endswith(".nuspec")))


CORE_NATIVE = ROOT / "dotnet" / "Pydantic.OpAmp.Core.Native"


def core_version() -> str:
    """Pydantic.OpAmp.Core.Native's version, which the client's package asks for exactly."""
    import xml.etree.ElementTree as ET

    return ET.parse(CORE_NATIVE / "Pydantic.OpAmp.Core.Native.csproj").getroot().findtext("PropertyGroup/Version")


def pack() -> None:
    """Pydantic.OpAmp.Core.Native with every platform's library, then the client on it, and the
    hosting package on the client."""
    nuget = DIST / "nuget"
    run(["dotnet", "pack", str(CORE_NATIVE), "-c", "Release", "--nologo", "-p:IsPackable=true",
         f"-p:OpAmpNativeRuntimes={DIST / 'runtimes'}", "-o", str(nuget)])
    for project in ("Pydantic.OpAmp.Client", "Pydantic.OpAmp.Extensions.Hosting"):
        run(["dotnet", "pack", str(ROOT / "dotnet" / project), "-c", "Release", "--nologo",
             "-p:IsPackable=true", f"-p:OpAmpCorePackage={core_version()}", f"-p:RestoreAdditionalProjectSources={nuget}",
             "-o", str(nuget)])


# Images to run each on: (platform, Python image, .NET SDK image). QEMU runs the arm64 ones.
SMOKE = [
    ("linux/amd64", "python:3.13-slim", "mcr.microsoft.com/dotnet/sdk:8.0"),
    ("linux/amd64", "python:3.13-alpine", "mcr.microsoft.com/dotnet/sdk:8.0-alpine"),
    ("linux/arm64", "python:3.13-slim", "mcr.microsoft.com/dotnet/sdk:8.0"),
    ("linux/arm64", "python:3.13-alpine", "mcr.microsoft.com/dotnet/sdk:8.0-alpine"),
]


# A container still running after this long has hung (under QEMU the slowest take about six
# minutes): it is stopped and fails with what it printed, instead of holding the job to its limit.
SMOKE_LIMIT_S = 15 * 60


def run_limited(cmd: list[str], limit: float, on_timeout: Callable[[], object] | None = None) -> tuple[int, str]:
    """`cmd`'s exit code and output, stdout and stderr together; past `limit` seconds it is killed,
    `on_timeout` runs, and it fails with what it printed and a line saying so."""
    with subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True) as p:
        try:
            out, _ = p.communicate(timeout=limit)
            return p.returncode, out
        except subprocess.TimeoutExpired:
            p.kill()
            out, _ = p.communicate()
            if on_timeout:
                on_timeout()
            return 124, f"{out or ''}still running after {limit:g} s: stopped\n"


def pinned(images: list[tuple[str, str]], docker=subprocess.run) -> dict[tuple[str, str], str]:
    """Each (platform, image) pulled for its platform and named by its local ID, one after
    another. A tag names one image locally: pulled for amd64 and arm64 at once (as the
    containers started in parallel did), one pull took the tag from under the other, and a run
    found the other platform's image ("does not match the specified platform", exit 125)."""
    ids = {}
    for platform, image in dict.fromkeys(images):
        docker(["docker", "pull", "-q", "--platform", platform, image], check=True, stdout=subprocess.DEVNULL)
        got = docker(["docker", "image", "inspect", "--format", "{{.Id}}", image],
                     check=True, capture_output=True, text=True)
        ids[(platform, image)] = got.stdout.strip()
    return ids


def smoke_cmds(ids: dict[tuple[str, str], str], version: str) -> list[list[str]]:
    """The smoke runs: in each image (by its ID, `pinned`), the wheel pip picks and the package
    NuGet resolves. .NET under QEMU runs with W^X off (DOTNET_EnableWriteXorExecute=0, the usual
    workaround for the runtime under emulation): with it on, the arm64 runtime crashed QEMU itself
    ("QEMU internal SIGSEGV", exit 139) or hung, on three runs in a row."""
    cmds = []
    for platform, py, dotnet in SMOKE:
        py, dotnet = ids[(platform, py)], ids[(platform, dotnet)]
        emulated = platform != "linux/amd64"
        cmds.append(["docker", "run", "--rm", "--platform", platform, "-v", f"{DIST / 'wheels'}:/wheels:ro",
                     "-v", f"{PYTHON / 'tests' / 'native_smoke.py'}:/smoke.py:ro", py, "sh", "-c",
                     "pip install -q --no-index --find-links /wheels pydantic-opamp-client 2>/dev/null"
                     " || pip install -q --find-links /wheels pydantic-opamp-client; python /smoke.py"])
        cmds.append(["docker", "run", "--rm", "--platform", platform, "-v", f"{DIST / 'nuget'}:/feed:ro",
                     "-v", f"{ROOT / 'dotnet' / 'smoke'}:/src:ro", "-e", "DOTNET_CLI_TELEMETRY_OPTOUT=1",
                     "-e", "DOTNET_NOLOGO=1",
                     *(["-e", "DOTNET_EnableWriteXorExecute=0"] if emulated else []), dotnet, "sh", "-c",
                     "cp -r /src /app && cd /app && dotnet nuget add source /feed -n local >/dev/null"
                     f" && dotnet run -c Release -p:OpAmpVersion={version}"])
    return cmds


def smoke() -> None:
    """Install the wheel pip picks, and the package NuGet resolves, in each image, and run them.

    Every container at once, each with its own output, shown in order once all are done: one after
    another they were eight minutes of a twenty-minute job, most of it QEMU's, on idle cores."""
    nupkg = next((DIST / "nuget").glob("Pydantic.OpAmp.Client.*.nupkg"))
    version = nupkg.name.removeprefix("Pydantic.OpAmp.Client.").removesuffix(".nupkg")
    ids = pinned([(platform, image) for platform, py, dotnet in SMOKE for image in (py, dotnet)])
    cmds = smoke_cmds(ids, version)

    def one(i: int, cmd: list[str]) -> tuple[list[str], int, str, float]:
        began = time.monotonic()
        name = f"opamp-smoke-{os.getpid()}-{i}"
        cmd = [*cmd[:2], "--name", name, *cmd[2:]]
        code, out = run_limited(cmd, SMOKE_LIMIT_S, on_timeout=lambda: subprocess.run(
            ["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        return cmd, code, out, time.monotonic() - began

    with ThreadPoolExecutor(len(cmds)) as pool:
        results = list(pool.map(one, range(len(cmds)), cmds))
    failed = 0
    for cmd, code, out, secs in results:
        print("+", " ".join(cmd), f"({secs:.0f} s, exit {code})", flush=True)
        print(out, end="", flush=True)
        failed += code != 0
    if failed:
        sys.exit(f"{failed} of {len(results)} smoke runs failed")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("step", choices=["build", "check", "pack", "smoke"])
    {"build": build, "check": check, "pack": pack, "smoke": smoke}[p.parse_args().step]()


if __name__ == "__main__":
    main()
