#!/usr/bin/env python3
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""This repository is public, so it holds only what PUBLIC_ALLOWLIST names. CI runs this on every
PR and every push to main.

    scripts/public_check.py

It refuses:
- a tracked file that no line of PUBLIC_ALLOWLIST matches;
- a binary (ELF, Mach-O, PE, WebAssembly), except the core's modules at the paths
  scripts/fetch_core.py puts them, whose bytes `fetch_core.py check` holds to the lock;
- a credential, or a private key outside a test directory (where they are made-up fixtures).

PUBLIC_ALLOWLIST is one pattern a line (# comments): `dir/` allows everything under dir/, and
anything else is a glob matched against the whole path (`*` stays within a directory).
"""
from __future__ import annotations

import fnmatch
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))
import fetch_core  # noqa: E402

ALLOWLIST = ROOT / "PUBLIC_ALLOWLIST"
MAGIC = (b"\x7fELF", b"\xfe\xed\xfa\xce", b"\xfe\xed\xfa\xcf", b"\xce\xfa\xed\xfe", b"\xcf\xfa\xed\xfe",
         b"\xca\xfe\xba\xbe", b"MZ", b"\0asm")
MODULE_PATHS = {p for dests in fetch_core.MODULES.values() for p in dests}
SECRETS = re.compile(rb"gh[opsur]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,}|AKIA[0-9A-Z]{16}"
                     rb"|xox[abposr]-[A-Za-z0-9-]{10,}|pylf_v[0-9]_[A-Za-z0-9_]{8,}"
                     rb"|-----BEGIN [A-Z ]*PRIVATE KEY-----")
TEST_PATH = re.compile(r"(^|/)(tests?|testdata|fixtures)/")


def patterns(text: str) -> list[str]:
    return [p for line in text.splitlines() if (p := line.split("#", 1)[0].strip())]


def matches(rel: str, pattern: str) -> bool:
    if pattern.endswith("/"):
        return rel.startswith(pattern)
    parts, globs = rel.split("/"), pattern.split("/")
    return len(parts) == len(globs) and all(fnmatch.fnmatchcase(p, g) for p, g in zip(parts, globs))


def allowed(rel: str, allow: list[str]) -> bool:
    return any(matches(rel, p) for p in allow)


def problems(rel: str, data: bytes, allow: list[str]) -> list[str]:
    """What is wrong with publishing `data` at `rel` (a path relative to the repository's root)."""
    if not allowed(rel, allow):
        return [f"{rel}: not in PUBLIC_ALLOWLIST"]
    if data.startswith(MAGIC) and not (data.startswith(b"\0asm") and rel in MODULE_PATHS):
        return [f"{rel}: a binary"]
    for hit in SECRETS.finditer(data):
        if not (TEST_PATH.search(rel) and hit.group().startswith(b"-----BEGIN")):
            return [f"{rel}: holds a credential or private key"]
    return []


def tracked() -> list[str]:
    out = subprocess.run(["git", "ls-files", "-z"], cwd=ROOT, check=True, capture_output=True).stdout
    return sorted(p for p in out.decode().split("\0") if p)


def main() -> int:
    allow = patterns(ALLOWLIST.read_text())
    found = [x for rel in tracked() if (ROOT / rel).is_file() for x in problems(rel, (ROOT / rel).read_bytes(), allow)]
    if found:
        print(*found, sep="\n")
        return 1
    print("everything here is allowed to be public")
    return 0


if __name__ == "__main__":
    sys.exit(main())
