#!/usr/bin/env python3
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Drop each of the SDKs' redactions, one at a time, and check their noninterference tests catch it.

    scripts/redact_mutants.py            # every mutant of scripts/redact-mutants.toml
    scripts/redact_mutants.py proxy      # those whose name contains "proxy"
    scripts/redact_mutants.py --check    # no tests: every mutant applies, exactly once

Each mutant is applied to a copy of the repository and its SDK's test runs there: Python's
test_redaction.py, Go's redact_test.go, .NET's RedactionTests.cs. Each test runs on the unmutated
copy first, and must pass there: a test that fails anyway catches nothing. Fails, naming them, if a
mutant survives. Python and .NET take the native core scripts/fetch_core.py fetch put in
target/release, so run that first.
"""
from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
MUTANTS = ROOT / "scripts" / "redact-mutants.toml"
# The fetched core is in the repository's target/release, which a copy (git's files only) lacks.
NATIVE_LIB = ROOT / "target" / "release" / "libopamp_native.so"
# Each test, as run from the copy's root.
TESTS = {
    "python": ["uv", "run", "--quiet", "--project", "python", "--python", "3.13", "pytest", "-q", "-p", "no:cacheprovider",
               "python/tests/test_redaction.py"],
    "go": ["go", "-C", "go", "test", "-count=1", "./opamp", "-run",
           "^(TestSecretsChangeNothingShown|TestWhatIsNotSecretIsStillShown|TestEveryFieldIsPrinted)$"],
    "dotnet": ["dotnet", "test", "dotnet/Pydantic.OpAmp.Client.Tests", "--nologo", "-v", "quiet",
               f"-p:OpAmpNativeLib={NATIVE_LIB}",
               "--filter", "FullyQualifiedName~Pydantic.OpAmp.Client.Tests.RedactionTests"],
}


def mutants(names: list[str] | None = None) -> list[dict[str, str]]:
    found = tomllib.loads(MUTANTS.read_text())["mutant"]
    return [m for m in found if not names or any(n in m["name"] for n in names)]


def apply(mutant: dict[str, str], root: Path) -> None:
    """Put `mutant` into the copy at `root`. Raises ValueError unless its `find` is there once."""
    path = root / mutant["file"]
    text = path.read_text()
    count = text.count(mutant["find"])
    if count != 1:
        raise ValueError(f"{mutant['name']}: `find` matches {count} times in {mutant['file']}, not once")
    path.write_text(text.replace(mutant["find"], mutant["replace"]))


def check(found: list[dict[str, str]]) -> list[str]:
    """What is wrong with the mutants as written: unknown tests, finds that don't match once."""
    problems = []
    names = [m["name"] for m in found]
    problems += [f"{n}: named twice" for n in sorted({n for n in names if names.count(n) > 1})]
    for m in found:
        if m.get("test") not in TESTS:
            problems.append(f"{m['name']}: test {m.get('test')!r} is not one of {sorted(TESTS)}")
        if m["find"] == m["replace"]:
            problems.append(f"{m['name']}: `replace` is `find`")
        path = ROOT / m["file"]
        if not path.is_file():
            problems.append(f"{m['name']}: no file {m['file']}")
        elif (count := path.read_text().count(m["find"])) != 1:
            problems.append(f"{m['name']}: `find` matches {count} times in {m['file']}, not once")
    return problems


def copy_repo(to: Path) -> None:
    """The files git knows of, committed or not (but not ignored)."""
    listed = subprocess.run(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"], cwd=ROOT,
                            check=True, capture_output=True).stdout.split(b"\0")
    for name in filter(None, listed):
        source = ROOT / name.decode()
        if source.is_file():
            target = to / name.decode()
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target)


def passes(test: str, root: Path) -> tuple[bool, str]:
    # Python's core wheel is built from CARGO_TARGET_DIR/release (python/core/build_backend.py): the
    # repository's, where the fetched core is.
    env = {**os.environ, "CARGO_TARGET_DIR": str(ROOT / "target")}
    run = subprocess.run(TESTS[test], cwd=root, env=env, capture_output=True, text=True)
    return run.returncode == 0, (run.stdout + run.stderr)[-2000:]


def main(argv: list[str]) -> int:
    names = [a for a in argv if not a.startswith("--")]
    found = mutants(names)
    problems = check(mutants())
    if problems:
        print("\n".join(problems))
        return 1
    if "--check" in argv:
        print(f"{len(found)} mutants, each applies once")
        return 0
    needs = {m["test"] for m in found} & {"python", "dotnet"}
    if needs and not NATIVE_LIB.is_file():
        print(f"{NATIVE_LIB} is missing, which {' and '.join(sorted(needs))} take: run scripts/fetch_core.py fetch")
        return 1
    survived = []
    with tempfile.TemporaryDirectory(prefix="redact-mutants-") as tmp:
        for test in sorted({m["test"] for m in found}):
            clean = Path(tmp) / "clean"
            if not clean.exists():
                copy_repo(clean)
            ok, out = passes(test, clean)
            if not ok:
                print(f"the {test} test fails with no mutant: it can catch nothing\n{out}")
                return 1
        for m in found:
            copy = Path(tmp) / m["name"]
            copy_repo(copy)
            apply(m, copy)
            ok, _ = passes(m["test"], copy)
            print(f"{'SURVIVED' if ok else 'caught  '} {m['name']}: {m['shows']}", flush=True)
            if ok:
                survived.append(m["name"])
            shutil.rmtree(copy)
    if survived:
        print(f"{len(survived)} of {len(found)} survived: {', '.join(survived)}")
        return 1
    print(f"all {len(found)} caught")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
