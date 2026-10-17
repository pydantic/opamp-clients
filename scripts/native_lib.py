#!/usr/bin/env python3
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The native core for Linux, for a .NET build that runs in Docker: the conformance suite's .NET
driver, examples/dotnet-otel.

    scripts/native_lib.py      # prints the library's path, relative to the repository

The library has to be a Linux one of the host's architecture whatever the host is (a Mac
building for a Linux container, say). The .NET build is told where it is (-p:OpAmpNativeLib=...,
see dotnet/Pydantic.OpAmp.Client's project file) instead of taking its own, which its image hasn't
got.

It is the library the core's release carries for Linux on this architecture (glibc 2.17, so it runs
in those images), which `scripts/fetch_core.py fetch --all-platforms` puts in dist/native/runtimes,
copied to where the conformance clients look.
"""
from __future__ import annotations

import platform
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
LIB = Path("target") / "native-linux" / "release" / "libopamp_native.so"


def build() -> Path:
    """The fetched Linux library for this architecture, copied to where the conformance clients
    look for it; its path relative to ROOT."""
    machine = platform.machine()
    arch = {"x86_64": "x64", "amd64": "x64", "aarch64": "arm64", "arm64": "arm64"}.get(machine.lower())
    if arch is None:
        sys.exit(f"no native core for Linux on {machine}: the core's release has x64 and arm64")
    lib = Path("dist") / "native" / "runtimes" / f"linux-{arch}" / "native" / "libopamp_native.so"
    if not (ROOT / lib).is_file():
        sys.exit(f"no {lib}: run scripts/fetch_core.py fetch --all-platforms")
    (ROOT / LIB).parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(ROOT / lib, ROOT / LIB)
    return LIB


if __name__ == "__main__":
    print(build())
