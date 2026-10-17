# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The Atheris targets (tests/fuzz/fuzz_*.py) on their seeds and on every input kept under
tests/fuzz/crashes/<target>/: what a fuzzer found once is run with every pytest (scripts/py_fuzz.sh
runs the targets under libFuzzer)."""

from __future__ import annotations

import importlib
import sys
from pathlib import Path

import pytest

FUZZ = Path(__file__).resolve().parent / "fuzz"
TARGETS = sorted(p.stem for p in FUZZ.glob("fuzz_*.py"))


def _target(name: str):
    if str(FUZZ) not in sys.path:
        sys.path.insert(0, str(FUZZ))
    return importlib.import_module(name)


def _inputs() -> list:
    cases = []
    for name in TARGETS:
        seeds = len(_target(name).SEEDS)
        cases += [pytest.param(name, i, id=f"{name}-seed-{i}") for i in range(seeds)]
        crashes = FUZZ / "crashes" / name
        if crashes.is_dir():
            cases += [pytest.param(name, p.name, id=f"{name}-{p.name}") for p in sorted(crashes.iterdir()) if p.is_file()]
    return cases


def test_there_are_targets():
    assert TARGETS == ["fuzz_client", "fuzz_native"]


@pytest.mark.parametrize(("name", "which"), _inputs())
def test_the_target_holds_on(name: str, which: int | str) -> None:
    target = _target(name)
    data = target.SEEDS[which] if isinstance(which, int) else (FUZZ / "crashes" / name / which).read_bytes()
    target.one(data)
