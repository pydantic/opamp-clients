# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""A JVM client's memory, as resources_bounded judges it.

A container's memory is a poor measure of a JVM's. The heap is committed up front, and the
container's memory follows eden's high-water mark as those pages are touched for the first time;
JIT-compiled code and metaspace grow while the JVM warms up. java-core-platform grew 115 -> 152 MB in an
hour (nightly, seed 20261010) with the committed heap flat at 254 MB, 9-14 MB live after a GC, and
JIT code and metaspace level after 20 minutes. So for a JVM, once a minute, the harness forces a
GC in the client's container and reads the heap still in use, and the non-heap memory the JVM
has committed (native memory tracking: everything but the Java heap). It judges:

- the heap after a GC, from the end of warm-up, by the rule the container's memory gets for any
  other client: the last third may not be above max(first third x 1.25, first third + 20 MB);
- the non-heap memory by the same rule, from a third of the way into the run on: that first
  third is the JVM's own warm-up (JIT code, metaspace, which level off after it).

Open files and threads are judged as for any other client. A client whose JVM could not be probed
(not in a container, or jcmd never answered) is judged by its container's memory, as before.
"""
from __future__ import annotations

import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from statistics import median

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "runner"))
from checks import Result  # noqa: E402

# Added to a JVM client's `docker run`: native memory tracking, for the non-heap figure. (Summary
# tracking costs a few bytes per allocation.)
RUN_ARGS = ["-e", "JAVA_TOOL_OPTIONS=-XX:NativeMemoryTracking=summary"]
# How often the harness probes the JVM. Each probe is a full GC, and starts a JVM for jcmd.
PROBE_EVERY_S = 60
# One jcmd for all three: force a GC, then read the heap and the native memory.
JCMD = "GC.run\nGC.heap_info\nVM.native_memory summary scale=KB\n"


@dataclass
class JvmSample:
    """A Sample (invariants.py) of a JVM client, with what a probe read, when one did."""

    t: float
    rss_mb: float
    fds: int | None
    threads: int | None
    heap_mb: float | None = None  # the heap still in use after a forced GC
    nonheap_mb: float | None = None  # committed memory other than the Java heap


def is_jvm(run: list[str]) -> bool:
    """The client's driver command runs `java`."""
    return "java" in run


def parse_jcmd(out: str) -> tuple[float, float, float | None] | None:
    """The heap in use and committed, and the non-heap memory committed (None without native
    memory tracking), in MB, from GC.heap_info and VM.native_memory; None if there is no heap."""
    # G1: one "garbage-first heap total ..K, used ..K"; Serial and Parallel: one line a generation.
    gens = re.findall(r"^\s*(?!Metaspace|class space)[\w -]+?\s+total (\d+)K, used (\d+)K", out, re.M)
    if not gens:
        return None
    committed = sum(int(c) for c, _ in gens) / 1024
    used = sum(int(u) for _, u in gens) / 1024
    total = re.search(r"^Total: reserved=\d+KB, committed=(\d+)KB", out, re.M)
    heap = re.search(r"Java Heap \(reserved=\d+KB, committed=(\d+)KB\)", out)
    nonheap = (int(total[1]) - int(heap[1])) / 1024 if total and heap else None
    return used, committed, nonheap


def probe(container: str, rss_mb: float, timeout: float = 30) -> tuple[float, float] | None:
    """Force a GC in the container's JVM (PID 1: the driver's `docker run ... java`), and read the
    heap still in use and the non-heap memory. Without native memory tracking, the non-heap memory
    is the container's less the committed heap."""
    try:
        out = subprocess.run(["docker", "exec", "-i", "-e", "JAVA_TOOL_OPTIONS=", container, "jcmd", "1", "-f", "/dev/stdin"],
                             input=JCMD, capture_output=True, text=True, timeout=timeout).stdout
    except (subprocess.TimeoutExpired, OSError):
        return None
    got = parse_jcmd(out)
    if got is None:
        return None
    used, committed, nonheap = got
    return round(used, 1), round(nonheap if nonheap is not None else rss_mb - committed, 1)


def _growth(values: list[float]) -> tuple[float, float]:
    third = max(1, len(values) // 3)
    return median(values[:third]), median(values[-third:])


def _grew(before: float, after: float) -> bool:
    return after > max(before * 1.25, before + 20)


def jvm_resources_bounded(samples: list, warm: float) -> Result | None:
    """resources_bounded for a JVM (see the module's doc); None if it isn't one, or was probed
    fewer than six times after warm-up."""
    return judge_probed([x for x in samples if isinstance(x, JvmSample)], samples, warm,
                        heap="heap in use after a GC", heap_short="heap after a GC",
                        other="non-heap memory", other_short="non-heap")


def judge_probed(probes: list, samples: list, warm: float, heap: str, heap_short: str, other: str,
                 other_short: str) -> Result | None:
    """resources_bounded for a runtime probed for its heap after a GC (`heap_mb`) and its other
    memory (`nonheap_mb`): `probes`, of all the `samples`. None if it was probed fewer than six
    times after warm-up. (jvm_memory.py, dotnet_memory.py.)"""
    s = [x for x in samples if x.t >= warm]
    probed = [x for x in probes if x.t >= warm and x.heap_mb is not None]
    if len(probed) < 6:
        return None
    before, after = _growth([x.heap_mb for x in probed])
    if _grew(before, after):
        return Result("fail", f"{heap} grew from {before:.0f} MB to {after:.0f} MB")
    detail = f"{heap_short} {before:.0f} -> {after:.0f} MB"
    rt_warm = warm + (probed[-1].t - warm) / 3
    nonheap = [x.nonheap_mb for x in probed if x.t >= rt_warm and x.nonheap_mb is not None]
    if len(nonheap) >= 6:
        b, a = _growth(nonheap)
        if _grew(b, a):
            return Result("fail", f"{other} grew from {b:.0f} MB to {a:.0f} MB after warm-up "
                                  f"({rt_warm:.0f}s)")
        detail += f", {other_short} {b:.0f} -> {a:.0f} MB after warm-up"
    for name in ("fds", "threads"):
        vals = [getattr(x, name) for x in s if getattr(x, name) is not None]
        if len(vals) >= 6:
            b, a = _growth(vals)
            if a > b + 20:
                return Result("fail", f"{name} grew from {b:.0f} to {a:.0f}")
    c_before, c_after = _growth([x.rss_mb for x in s])
    return Result("pass", f"{detail} (container {c_before:.0f} -> {c_after:.0f} MB)")
