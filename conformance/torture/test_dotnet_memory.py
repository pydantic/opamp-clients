# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Tests for how resources_bounded judges a .NET client's memory (dotnet_memory.py).

    python3 -m unittest conformance/torture/test_dotnet_memory.py
"""
from __future__ import annotations

import sys
import unittest
from pathlib import Path
from typing import Callable

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "runner"))
sys.path.insert(0, str(HERE))
from invariants import INVARIANTS, Plan, Sample  # noqa: E402
from dotnet_memory import DotnetSample, gc_committed_mb, is_dotnet, live_heap_mb  # noqa: E402

resources_bounded = INVARIANTS["resources_bounded"]

# The nightly's shape: an hour, warm-up over at the end of the first quiet window (120 s).
PLAN = Plan(quiet=[(45.0, 120.0)], check_s=20, poll_s=2, storm_per_10s=40)
MINUTES = 60


def series(rss: Callable[[float], float], heap: Callable[[float], float] | None,
           nongc: Callable[[float], float] | None) -> list[Sample]:
    """A sample every 5 s, as torture takes them: the container's memory and PIDs every time, and
    (a .NET client) the GC heap live after a forced GC and the non-GC memory once a minute. Each
    function takes the minute."""
    out: list[Sample] = []
    for i in range(1, MINUTES * 12 + 1):
        t = 5.0 * i
        m = t / 60
        if heap is None:
            out.append(Sample(t, rss(m), None, 30))
        else:
            probed = i % 12 == 0
            out.append(DotnetSample(t, rss(m), None, 30, heap(m) if probed else None,
                                    nongc(m) if probed else None))
    return out


def gen0_garbage(m: float) -> float:
    """The container's memory in the night that failed (dotnet-core http, Nightly run
    38112161610, go server): 65 MB for the first third, 90 MB for the last. The runtime sizes
    gen0 from the L3 cache, a budget this client's garbage takes most of the hour to fill: gen0
    is hardly ever collected, and its pages fill with dead objects."""
    return 65 + 25 * min(1.0, max(0.0, (m - 15) / 25))


def live_set(m: float) -> float:
    """The GC heap live after a forced full GC: 3 to 5 MB all hour."""
    return 3 + (int(m) * 7) % 3


def warming(m: float) -> float:
    """Non-GC memory (the container's less what the GC has committed): the runtime, the native
    core, JIT-compiled code growing over the first 15 minutes, then level."""
    return 38 + 6 * min(1.0, m / 15)


class DotnetMemoryTests(unittest.TestCase):
    def judge(self, samples: list[Sample]):
        return resources_bounded(None, PLAN, samples)

    def test_gen0_garbage_is_not_a_leak(self):
        # The container's memory grows past the limit, with dead objects gen0 hasn't collected;
        # the GC heap live after a forced GC and the non-GC memory after warm-up do not grow.
        r = self.judge(series(gen0_garbage, live_set, warming))
        self.assertEqual(r.status, "pass", r.detail)

    def test_a_live_heap_that_grows_is_a_leak(self):
        # 0.8 MB a minute kept alive (a cache that is never trimmed, a list of pending sends).
        r = self.judge(series(gen0_garbage, lambda m: 3 + 0.8 * m, warming))
        self.assertEqual(r.status, "fail", r.detail)
        self.assertIn("GC heap", r.detail)

    def test_a_live_heap_leak_the_container_hides(self):
        # The same leak in a client whose gen0 is collected now and then: the survivors move into
        # memory the gen0 budget had already committed and touched, so the container's memory
        # rises only 80 -> 95 MB, under the old limit (100 MB). Judging it alone misses the leak.
        def heap(m: float) -> float:
            return 3 + 0.8 * m

        def container(m: float) -> float:
            return 80 + 15 * m / MINUTES

        self.assertEqual(self.judge(series(container, None, None)).status, "pass")
        r = self.judge(series(container, heap, warming))
        self.assertEqual(r.status, "fail", r.detail)
        self.assertIn("GC heap", r.detail)

    def test_non_gc_memory_that_grows_after_warm_up_is_a_leak(self):
        # 1 MB a minute of native memory (the core's, a socket's, a handle's) from minute 20 on:
        # the GC heap is level.
        def leaking(m: float) -> float:
            return warming(m) + 1.0 * max(0.0, m - 20)

        r = self.judge(series(lambda m: 70 + 1.0 * max(0.0, m - 20), live_set, leaking))
        self.assertEqual(r.status, "fail", r.detail)
        self.assertIn("non-GC", r.detail)

    def test_non_gc_memory_that_grows_from_the_start_is_a_leak(self):
        # The same leak from the first minute: warm-up doesn't hide it.
        r = self.judge(series(lambda m: 80, live_set, lambda m: 38 + 1.0 * m))
        self.assertEqual(r.status, "fail", r.detail)
        self.assertIn("non-GC", r.detail)

    def test_threads_are_still_judged(self):
        s = series(lambda m: 80, live_set, warming)
        for x in s[len(s) // 2:]:
            x.threads = 60
        r = self.judge(s)
        self.assertEqual(r.status, "fail", r.detail)
        self.assertIn("threads", r.detail)

    def test_other_clients_are_judged_as_before(self):
        # The same container memory from a client with no GC heap to look into: it fails, as it did.
        r = self.judge(series(gen0_garbage, None, None))
        self.assertEqual((r.status, r.detail), ("fail", "memory grew from 65 MB to 90 MB"))
        r = self.judge(series(lambda m: 50 + 0.1 * m, None, None))
        self.assertEqual((r.status, r.detail), ("pass", "memory 51 -> 55 MB"))

    def test_a_dotnet_client_that_could_not_be_probed_is_judged_as_before(self):
        # The probe never answered (the tools couldn't be installed, or the container was frozen):
        # no heap figures, so the container's memory is judged, as for any other client.
        r = self.judge(series(gen0_garbage, lambda m: None, lambda m: None))
        self.assertEqual((r.status, r.detail), ("fail", "memory grew from 65 MB to 90 MB"))


# dotnet-gcdump report's head (dotnet-gcdump 10.0.745401, a .NET 8 process).
REPORT = """\
      2,663,468  GC Heap bytes
          2,597  GC Heap objects

   Object Bytes     Count  Type
         32,792         1  System.Byte[][] (Bytes > 10K)  [System.Private.CoreLib.dll]
"""

# dotnet-counters collect --format json, against .NET 8 (EventCounters), cut to two counters.
COUNTERS_8 = """\
{ "TargetProcess": "dotnet", "StartTime": "2026-10-11T08:57:01.1640686+00:00", "Events": [\
{ "timestamp": "2026-10-11T08:57:03.4244751+00:00",  "provider": "System.Runtime",  "name": "Working Set (MB)",  \
"tags": "",  "counterType": "Metric",  "meterTags": "",  "instrumentTags": "",  "value": 128.548864 },\
{ "timestamp": "2026-10-11T08:57:03.4265215+00:00",  "provider": "System.Runtime",  "name": "GC Committed Bytes (MB)",  \
"tags": "",  "counterType": "Metric",  "meterTags": "",  "instrumentTags": "",  "value": 94.916608 },\
{ "timestamp": "2026-10-11T08:57:04.3650749+00:00",  "provider": "System.Runtime",  "name": "GC Committed Bytes (MB)",  \
"tags": "",  "counterType": "Metric",  "meterTags": "",  "instrumentTags": "",  "value": 95.768576 }] }
"""

# The same against .NET 9 or later, whose System.Runtime is a Meter (bytes).
COUNTERS_9 = """\
{ "TargetProcess": "dotnet", "StartTime": "2026-10-11T08:57:01+00:00", "Events": [\
{ "timestamp": "2026-10-11T08:57:03+00:00",  "provider": "System.Runtime",  "name": "dotnet.gc.last_collection.memory.committed_size (By)",  \
"tags": "",  "counterType": "Metric",  "meterTags": "",  "instrumentTags": "",  "value": 52428800 }] }
"""


class ParseTests(unittest.TestCase):
    def test_live_heap_from_the_report(self):
        self.assertAlmostEqual(live_heap_mb(REPORT), 2663468 / 2**20)

    def test_no_report_no_heap(self):
        self.assertIsNone(live_heap_mb("[ERROR] System.ServerException: Unable to connect"))
        self.assertIsNone(live_heap_mb(""))

    def test_gc_committed_from_event_counters(self):
        self.assertAlmostEqual(gc_committed_mb(COUNTERS_8), 95.768576 * 1e6 / 2**20)

    def test_gc_committed_from_the_runtime_meter(self):
        self.assertAlmostEqual(gc_committed_mb(COUNTERS_9), 50.0)

    def test_no_counters_no_committed(self):
        self.assertIsNone(gc_committed_mb(""))
        self.assertIsNone(gc_committed_mb('{ "TargetProcess": "dotnet", "Events": [] }'))
        # Cut short (the session stopped mid-write): no figure rather than a guess.
        self.assertIsNone(gc_committed_mb(COUNTERS_8[:200]))

    def test_dotnet_clients(self):
        self.assertTrue(is_dotnet(["docker", "run", "--rm", "mcr.microsoft.com/dotnet/runtime:8.0", "dotnet",
                                   "/src/out/Driver.dll"]))
        self.assertFalse(is_dotnet(["{bin}/driver-go"]))
        self.assertFalse(is_dotnet(["docker", "run", "--rm", "maven:3.9", "java", "-cp", "/src", "Driver"]))


if __name__ == "__main__":
    unittest.main()
