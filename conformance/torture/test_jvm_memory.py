# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Tests for how resources_bounded judges a JVM's memory (jvm_memory.py).

    python3 -m unittest conformance/torture/test_jvm_memory.py
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
from jvm_memory import JvmSample, is_jvm, parse_jcmd  # noqa: E402

resources_bounded = INVARIANTS["resources_bounded"]

# The nightly's shape: an hour, warm-up over at the end of the first quiet window (120 s).
PLAN = Plan(quiet=[(45.0, 120.0)], check_s=20, poll_s=2, storm_per_10s=40)
MINUTES = 60


def series(rss: Callable[[float], float], heap: Callable[[float], float] | None,
           nonheap: Callable[[float], float] | None) -> list[Sample]:
    """A sample every 5 s, as torture takes them: the container's memory and PIDs every time, and
    (a JVM) the heap after a forced GC and the non-heap memory once a minute. Each function takes
    the minute."""
    out: list[Sample] = []
    for i in range(1, MINUTES * 12 + 1):
        t = 5.0 * i
        m = t / 60
        if heap is None:
            out.append(Sample(t, rss(m), None, 30))
        else:
            probed = i % 12 == 0
            out.append(JvmSample(t, rss(m), None, 30, heap(m) if probed else None,
                                 nonheap(m) if probed else None))
    return out


def eden_touched(m: float) -> float:
    """The container's memory in a night that failed (java-core-platform http, 2026-10-10): 115 MB for the
    first third, 152 MB for the last, as already-committed heap pages are touched for the first
    time while eden's high-water mark climbs. The committed heap is flat all hour."""
    return 115 + 37 * min(1.0, max(0.0, (m - 20) / 20))


def live_set(m: float) -> float:
    """The heap after a young or full GC: 9 to 14 MB all hour."""
    return 9 + (int(m) * 7) % 6


def warming(m: float) -> float:
    """Non-heap memory: JIT code 8 -> 22 MB and metaspace 15 -> 18 MB over the first 20 minutes,
    then level."""
    return 75 + 17 * min(1.0, m / 20)


class JvmMemoryTests(unittest.TestCase):
    def judge(self, samples: list[Sample]):
        return resources_bounded(None, PLAN, samples)

    def test_jvm_warm_up_is_not_a_leak(self):
        # The container's memory grows past the limit; the heap still in use after a GC and the
        # non-heap memory after warm-up do not grow.
        r = self.judge(series(eden_touched, live_set, warming))
        self.assertEqual(r.status, "pass", r.detail)

    def test_a_live_heap_that_grows_is_a_leak(self):
        # 0.8 MB a minute kept alive: the old generation grows into pages eden already touched, so
        # the container's memory hardly moves; the heap after a forced GC does.
        r = self.judge(series(lambda m: 140, lambda m: 10 + 0.8 * m, warming))
        self.assertEqual(r.status, "fail", r.detail)
        self.assertIn("heap", r.detail)

    def test_non_heap_that_grows_after_warm_up_is_a_leak(self):
        # 1.2 MB a minute of native memory from minute 20 on (a direct buffer, a thread, a class
        # loader never freed): the heap is flat.
        def leaking(m: float) -> float:
            return warming(m) + 1.2 * max(0.0, m - 20)

        r = self.judge(series(lambda m: 115 + 1.2 * max(0.0, m - 20), live_set, leaking))
        self.assertEqual(r.status, "fail", r.detail)
        self.assertIn("non-heap", r.detail)

    def test_non_heap_that_grows_from_the_start_is_a_leak(self):
        # The same leak from the first minute: warm-up doesn't hide it.
        def leaking(m: float) -> float:
            return 75 + 1.2 * m

        r = self.judge(series(lambda m: 140, live_set, leaking))
        self.assertEqual(r.status, "fail", r.detail)
        self.assertIn("non-heap", r.detail)

    def test_threads_are_still_judged(self):
        s = series(lambda m: 140, live_set, warming)
        for x in s[len(s) // 2:]:
            x.threads = 60
        r = self.judge(s)
        self.assertEqual(r.status, "fail", r.detail)
        self.assertIn("threads", r.detail)

    def test_a_non_jvm_client_is_judged_as_before(self):
        # The same container memory from a client with no heap to look into: it fails, as it did.
        r = self.judge(series(eden_touched, None, None))
        self.assertEqual((r.status, r.detail), ("fail", "memory grew from 115 MB to 152 MB"))
        r = self.judge(series(lambda m: 50 + 0.1 * m, None, None))
        self.assertEqual((r.status, r.detail), ("pass", "memory 51 -> 55 MB"))

    def test_a_jvm_that_could_not_be_probed_is_judged_as_before(self):
        # jcmd never answered (the JVM wasn't up, or the container was frozen): no heap figures,
        # so the container's memory is judged, as for any other client.
        r = self.judge(series(eden_touched, lambda m: None, lambda m: None))
        self.assertEqual((r.status, r.detail), ("fail", "memory grew from 115 MB to 152 MB"))


G1 = """\
1:
Command executed successfully
 garbage-first heap   total 260096K, used 12034K [0x0000000704800000, 0x0000000800000000)
  region size 2048K, 0 young (0K), 0 survivors (0K)
 Metaspace       used 17410K, committed 17664K, reserved 1114112K
  class space    used 2089K, committed 2216K, reserved 1048576K

Native Memory Tracking:

(Omitting categories weighting less than 1KB)

Total: reserved=5649808KB, committed=356004KB
       malloc: 15984KB #43026
       mmap:   reserved=5633824KB, committed=195636KB

-                 Java Heap (reserved=4120576KB, committed=260096KB)
                            (mmap: reserved=4120576KB, committed=260096KB, peak=260096KB)

-                     Class (reserved=1048820KB, committed=1460KB)
"""

SERIAL = """\
1:
Command executed successfully
 def new generation   total 9792K, used 1024K [0x00000000fe000000, 0x00000000feaa0000, 0x0000000100000000)
  eden space 8704K,   0% used [0x00000000fe000000, 0x00000000fe000000, 0x00000000fe880000)
 tenured generation   total 21888K, used 9216K [0x00000000feaa0000, 0x00000000ff000000, 0x0000000100000000)
 Metaspace       used 9410K, committed 9664K, reserved 1114112K
  class space    used 1089K, committed 1216K, reserved 1048576K
Native memory tracking is not enabled
"""


class ParseTests(unittest.TestCase):
    def test_g1_with_native_memory_tracking(self):
        used, committed, nonheap = parse_jcmd(G1)
        self.assertAlmostEqual(used, 12034 / 1024)
        self.assertAlmostEqual(committed, 260096 / 1024)
        self.assertAlmostEqual(nonheap, (356004 - 260096) / 1024)

    def test_serial_without_native_memory_tracking(self):
        used, committed, nonheap = parse_jcmd(SERIAL)
        self.assertAlmostEqual(used, (1024 + 9216) / 1024)
        self.assertAlmostEqual(committed, (9792 + 21888) / 1024)
        self.assertIsNone(nonheap)

    def test_no_heap_no_figures(self):
        self.assertIsNone(parse_jcmd("Unable to open socket file /tmp/.java_pid1"))

    def test_jvm_clients(self):
        self.assertTrue(is_jvm(["docker", "run", "--rm", "maven:3.9", "java", "-cp", "/src", "Driver"]))
        self.assertFalse(is_jvm(["{bin}/driver-go"]))
        self.assertFalse(is_jvm(["docker", "run", "mcr.microsoft.com/dotnet/runtime:8.0", "dotnet", "/src/out/Driver.dll"]))


if __name__ == "__main__":
    unittest.main()
