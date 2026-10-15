# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Tests for what torture.py hands the drivers it runs in a container.

    python3 -m unittest conformance/torture/test_torture.py
"""
from __future__ import annotations

import re
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "runner"))
import run  # noqa: E402


def driver_env() -> set[str]:
    """The variables torture.py sets for a driver: those of its environment and of each launch."""
    src = (HERE / "torture.py").read_text()
    start = src.index("env = dict(os.environ,")
    end = src.index('return {"driver"', start)
    return set(re.findall(r"\b((?:DRIVER|OPAMP)_[A-Z_]+)\b", src[start:end]))


def passed_in(spec: dict) -> set[str]:
    """The variables a `docker run` command passes into its container (`-e NAME`)."""
    cmd = spec["run"]
    return {cmd[i + 1] for i, a in enumerate(cmd[:-1]) if a == "-e" and "=" not in cmd[i + 1]}


class DockerDrivers(unittest.TestCase):
    def test_the_variables_are_found(self):
        self.assertTrue({"DRIVER_RUN_S", "DRIVER_CHANGES_AT_S", "DRIVER_FIRST_CHANGE", "DRIVER_STATE_FILE"}
                        <= driver_env())

    def test_a_driver_that_torture_crashes_gets_every_variable(self):
        # Docker passes a variable into the container only if `-e` names it. A driver torture
        # restarts after a crash that never sees DRIVER_FIRST_CHANGE starts its statuses over at 0,
        # and status_never_goes_back fails. Nightly 2026-10-14: every java-core, java-core-rust,
        # dotnet-core and dotnet-core-rust torture run.
        wanted = driver_env()
        crashed = 0
        for name, spec in run.load_clients(None).items():
            if not spec.get("docker") or spec.get("upstream") or "instance_uid_kept" not in spec.get("features", []):
                continue
            crashed += 1
            with self.subTest(client=name):
                self.assertEqual(sorted(wanted - passed_in(spec)), [])
        self.assertGreater(crashed, 0)


if __name__ == "__main__":
    unittest.main()
