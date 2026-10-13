#!/usr/bin/env python3
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""What may be published in pydantic/opamp-clients, which is public: nothing of the core's source,
no binary but the core's modules where the clients load them, no credential. CI runs it on every PR,
and pydantic/opamp runs its copy on what moves here.

    public_check.py                    # every file git tracks here
    public_check.py --list FILE        # only the paths FILE names (a git pathspec a line)

It refuses:
- the core's source: Rust (.rs, Cargo.toml, Cargo.lock, rust-toolchain), the models (.tla), and
  anything under rust/, fuzz/, tla/ or tests/wasm_core/;
- a reference into a core crate's source (rust/opamp-*/src/...): it points at code that isn't
  public;
- a binary (ELF, Mach-O, PE, WebAssembly), except each module at the paths fetch_core.py puts it
  (Go embeds it, so it is committed), whose bytes fetch_core.py check holds to the lock;
- a credential, or a private key outside a test directory (where they are made-up fixtures).
"""
from __future__ import annotations

import argparse
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import fetch_core  # noqa: E402

SOURCE_NAMES = {"Cargo.toml", "Cargo.lock", "rust-toolchain", "rust-toolchain.toml"}
SOURCE_SUFFIXES = {".rs", ".tla"}
SOURCE_DIRS = ("rust/", "fuzz/", "tla/", "tests/wasm_core/")
CORE_REF = re.compile(rb"rust/opamp-[a-z0-9-]+/src/[A-Za-z0-9_/]+")
MAGIC = (b"\x7fELF", b"\xfe\xed\xfa\xce", b"\xfe\xed\xfa\xcf", b"\xce\xfa\xed\xfe", b"\xcf\xfa\xed\xfe",
         b"\xca\xfe\xba\xbe", b"MZ", b"\0asm")
MODULE_PATHS = {p for dests in fetch_core.MODULES.values() for p in dests}
SECRETS = re.compile(rb"gh[opsur]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,}|AKIA[0-9A-Z]{16}"
                     rb"|xox[abposr]-[A-Za-z0-9-]{10,}|pylf_v[0-9]_[A-Za-z0-9_]{8,}"
                     rb"|-----BEGIN [A-Z ]*PRIVATE KEY-----")
# Where a private key is a made-up test fixture (tests/shell-tables/tls/gen.sh and the like).
TEST_PATH = re.compile(r"(^|/)(tests?|fixtures)/")


def problems(rel: str, data: bytes) -> list[str]:
    """What is wrong with publishing `data` at `rel` (a path relative to the repository's root)."""
    name = rel.rsplit("/", 1)[-1]
    if name in SOURCE_NAMES or Path(rel).suffix in SOURCE_SUFFIXES or rel.startswith(SOURCE_DIRS):
        return [f"{rel}: the core's source"]
    if data.startswith(MAGIC) and not (data.startswith(b"\0asm") and rel in MODULE_PATHS):
        return [f"{rel}: a binary"]
    if data.startswith(b"\0asm"):
        return []
    out = []
    if CORE_REF.search(data):
        lines = [i for i, line in enumerate(data.splitlines(), 1) if CORE_REF.search(line)]
        out.append(f"{rel}: names the core's source (line {', '.join(map(str, lines))})")
    for hit in SECRETS.finditer(data):
        if not (TEST_PATH.search(rel) and hit.group().startswith(b"-----BEGIN")):
            out.append(f"{rel}: holds a credential or private key")
            break
    return out


def tracked(root: Path, paths: list[str]) -> list[str]:
    out = subprocess.run(["git", "ls-files", "-z", "--", *paths], cwd=root, check=True,
                         capture_output=True).stdout.decode()
    return sorted(p for p in out.split("\0") if p)


def check(root: Path, paths: list[str]) -> list[str]:
    found = []
    for rel in tracked(root, paths):
        if (root / rel).is_file():
            found += problems(rel, (root / rel).read_bytes())
    return found


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--list", type=Path, help="check only the paths this file names, one a line (# comments)")
    p.add_argument("--root", type=Path, default=Path.cwd())
    args = p.parse_args()
    paths = []
    if args.list:
        paths = [line.split("#", 1)[0].strip() for line in args.list.read_text().splitlines()]
        paths = [x for x in paths if x]
    if found := check(args.root, paths):
        print(*found, sep="\n")
        return 1
    print("nothing here that shouldn't be public")
    return 0


if __name__ == "__main__":
    sys.exit(main())
