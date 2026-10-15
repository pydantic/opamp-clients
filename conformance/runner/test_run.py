# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Tests for run.py's choice of clients, and for scenarios timed against the core's backoff.

    python3 -m unittest conformance/runner/test_run.py
"""
from __future__ import annotations

import json
import re
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import run  # noqa: E402

LAZYFS = run.CONF / "lazyfs"


def lazyfs_clients() -> list[str]:
    """The clients conformance/lazyfs/run.sh runs by default (LAZYFS_CLIENTS)."""
    script = (LAZYFS / "run.sh").read_text()
    default = re.search(r'LAZYFS_CLIENTS="\$\{LAZYFS_CLIENTS:-([^}]*)\}"', script)
    assert default, "run.sh names its default clients"
    return [c.lstrip("=") for c in default.group(1).split(",")]


class LazyFSClients(unittest.TestCase):
    def test_the_lazyfs_container_needs_no_docker_for_its_clients(self):
        # The LazyFS container has no Docker: a driver that runs in a container, or a build that
        # needs one (a native core built for Linux in Docker, scripts/native_lib.py), can't run there.
        # Nightly 2026-10-11: dotnet-core's native build called docker in the container and failed.
        clients = run.load_clients(LAZYFS / "native.toml")
        # (The Rust client, built from the core's sources, only where they are.)
        wanted = [c for c in lazyfs_clients() if c in clients]
        self.assertIn("dotnet-core", wanted)
        self.assertEqual(run.need_docker(clients, wanted), [])

    def test_a_client_built_or_run_in_docker_is_named(self):
        clients = run.load_clients(None)
        self.assertIn("dotnet-core", run.need_docker(clients, ["dotnet-core"]))
        self.assertIn("java-core", run.need_docker(clients, ["java-core"]))
        self.assertEqual(run.need_docker(clients, [c for c in ("rust", "go-core") if c in clients]), [])


def kernel_backoff_ms() -> tuple[int, int]:
    """The core's default backoff (its Config's, in proto/opamp/core/v1/core.proto): initial and max."""
    src = (run.ROOT / "proto" / "opamp" / "core" / "v1" / "core.proto").read_text()
    initial = re.search(r"backoff_initial_ms = \d+;.*default (\d+)", src)
    most = re.search(r"backoff_max_ms = \d+;.*default (\d+)", src)
    assert initial and most, "core.proto's Config gives the default backoff"
    return int(initial.group(1)), int(most.group(1))


def latest_comeback_s(window_end_s: float, initial_ms: int, max_ms: int) -> float:
    """The latest a client backing off as the core does (backoff_ms: wait n is half to all of
    min(max, initial * 2^n)) can first be let in, every attempt from 0 up to window_end_s refused:
    after n refused waits the last refused attempt is before the window's end and no later than the
    n longest waits allow, and the next wait is at most the whole of wait n. n runs while n waits at
    their shortest still end inside the window."""
    base = lambda n: min(max_ms, initial_ms * 2 ** n) / 1000  # noqa: E731
    latest, shortest, longest, n = 0.0, 0.0, 0.0, 0
    while shortest < window_end_s:
        latest = max(latest, min(window_end_s, longest) + base(n))
        shortest += base(n) / 2
        longest += base(n)
        n += 1
    return latest


class BackoffCovered(unittest.TestCase):
    def test_redirect_runs_past_the_latest_comeback(self):
        # redirects_not_followed needs the client back after the redirects. The fault window is
        # timed from the driver's first request, the run from the driver's start: a run of 30 s
        # against a comeback as late as 31 s failed about 1% of the time (dotnet-core-rust, ws).
        scenario = json.loads((run.CONF / "scenarios" / "redirect.json").read_text())
        (fault,) = [f for f in scenario["server"]["faults"] if f["action"] == "http_status"]
        self.assertEqual(fault["window_s"][0], 0)
        latest = latest_comeback_s(fault["window_s"][1], *kernel_backoff_ms())
        self.assertEqual(latest, 31.0)
        # Room for round trips and a first request that comes after the driver's clock starts.
        self.assertGreaterEqual(scenario["duration_s"], latest + 5)

    def test_latest_comeback(self):
        # 0.5 + 1 + 2 + 4 = 7.5 s for four waits at their shortest: a fifth attempt can be refused
        # just before 15 s, and wait up to 16 s.
        self.assertEqual(latest_comeback_s(15, 1000, 60000), 31.0)
        self.assertEqual(latest_comeback_s(7, 1000, 60000), 15.0)  # 7 s: three waits, then 8 s
        self.assertEqual(latest_comeback_s(15, 1000, 4000), 19.0)  # capped at 4 s


if __name__ == "__main__":
    unittest.main()
