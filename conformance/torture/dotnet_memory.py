# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""A .NET client's memory, as resources_bounded judges it.

A container's memory is a poor measure of a .NET process's. The runtime sizes gen0's budget from
the L3 cache, and a client that allocates little takes most of an hour to fill it: gen0 is
hardly ever collected, and the container's memory climbs with dead objects (dotnet-core-platform http,
Nightly run 38112161610: 65 -> 90 MB against the Go server, and it failed against ours;
dotnet-counters and dotnet-gcdump found no leak). So for a .NET client, once a minute, the
harness forces a full, blocking GC in the client's container and reads what is still live, and
the memory the GC hasn't committed. It judges, as jvm_memory.py does a JVM's:

- the GC heap live after the forced GC, from the end of warm-up, by the rule the container's
  memory gets for any other client: the last third may not be above max(first third x 1.25,
  first third + 20 MB);
- the non-GC memory (the container's less what the GC has committed) by the same rule, from a
  third of the way into the run on: that first third is the runtime's own warm-up (JIT-compiled
  code, type loading).

The probe runs inside the client's container, as the driver's user, and changes nothing of how
the driver runs (no GC setting, no other runtime setting): the runtime's diagnostics port, open by
default, is all it uses. Microsoft's diagnostic tools are installed once per run (pinned, in the
.NET SDK image, under conformance/out/dotnet-diag), copied into the container, and run on the
container's own runtime:

- `dotnet-gcdump collect -p 1` asks the runtime (over EventPipe) for a heap dump, which it takes by
  inducing a full blocking GC and walking what survived it; `dotnet-gcdump report`'s total is
  those objects' bytes: the live heap, whatever the GC chose to compact;
- `dotnet-counters collect` then reads the GC's committed bytes as of that GC.

Open files and threads are judged as for any other client. A client the probe never answered
(the tools couldn't be installed, or the container was frozen) is judged by its container's
memory, as before.
"""
from __future__ import annotations

import json
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "runner"))
from checks import Result  # noqa: E402
from jvm_memory import judge_probed  # noqa: E402

# Microsoft's diagnostic tools, pinned; installed with the image the drivers are built with.
TOOLS_VERSION = "10.0.745401"
TOOLS = ("dotnet-gcdump", "dotnet-counters")
SDK_IMAGE = "mcr.microsoft.com/dotnet/sdk:8.0"
# Where they are copied to in the client's container (its /tmp is the driver user's HOME).
IN_CONTAINER = "/tmp/opamp-dotnet-diag"
# How often the harness probes the client. Each probe is a full GC.
PROBE_EVERY_S = 60


@dataclass
class DotnetSample:
    """A Sample (invariants.py) of a .NET client, with what a probe read, when one did."""

    t: float
    rss_mb: float
    fds: int | None
    threads: int | None
    heap_mb: float | None = None  # the GC heap live after a forced full GC
    nonheap_mb: float | None = None  # the container's memory less what the GC has committed


def is_dotnet(run: list[str]) -> bool:
    """The client's driver command runs `dotnet`."""
    return "dotnet" in run


def live_heap_mb(report: str) -> float | None:
    """The bytes of the objects a heap dump found (dotnet-gcdump report's "GC Heap bytes"), in MB."""
    m = re.search(r"^\s*([\d,]+)\s+GC Heap bytes", report, re.M)
    return int(m[1].replace(",", "")) / 2**20 if m else None


def gc_committed_mb(counters: str) -> float | None:
    """The GC's committed memory, in MB, from `dotnet-counters collect --format json`: the last
    value of .NET 8's gc-committed EventCounter (in units of 10^6 bytes) or of .NET 9's
    dotnet.gc.last_collection.memory.committed_size (bytes)."""
    try:
        events = json.loads(counters)["Events"]
    except (ValueError, KeyError, TypeError):
        return None
    got = None
    for e in events:
        name = e.get("name", "")
        if name.startswith("GC Committed Bytes"):
            got = e["value"] * 1e6 / 2**20
        elif name.startswith("dotnet.gc.last_collection.memory.committed_size"):
            got = e["value"] / 2**20
    return got


def _dll(tools_dir: Path, tool: str) -> Path | None:
    found = sorted(tools_dir.glob(f".store/{tool}/{TOOLS_VERSION}/{tool}/{TOOLS_VERSION}/tools/net8.0/any/{tool}.dll"))
    return found[0] if found else None


def install_tools(tools_dir: Path, nuget: str) -> bool:
    """Install the tools under `tools_dir` (once: kept for later runs); whether they are there."""
    if all(_dll(tools_dir, t) for t in TOOLS):
        return True
    tools_dir.mkdir(parents=True, exist_ok=True)
    script = " ; ".join(f"dotnet tool install {t} --version {TOOLS_VERSION} --tool-path /diag" for t in TOOLS)
    try:
        subprocess.run(["docker", "run", "--rm", "--network", "host", "-v", f"{tools_dir}:/diag",
                        "-v", f"{nuget}:/root/.nuget", SDK_IMAGE, "sh", "-c", script],
                       capture_output=True, timeout=600)
    except (subprocess.TimeoutExpired, OSError):
        return False
    return all(_dll(tools_dir, t) for t in TOOLS)


class Probe:
    """The probe of one client's container: copies the tools in on first use."""

    def __init__(self, container: str, tools_dir: Path):
        self.container = container
        self.tools_dir = tools_dir
        self.copied = False

    def _script(self) -> str:
        gcdump, counters = (f"{IN_CONTAINER}/{_dll(self.tools_dir, t).relative_to(self.tools_dir / '.store')}"
                            for t in TOOLS)
        return (f'dotnet "{gcdump}" collect -p 1 -o /tmp/opamp-probe.gcdump >/dev/null 2>&1'
                f' && dotnet "{gcdump}" report /tmp/opamp-probe.gcdump 2>/dev/null | head -n 2;'
                " rm -f /tmp/opamp-probe.gcdump; echo '--- counters';"
                f' dotnet "{counters}" collect -p 1'
                " --counters 'System.Runtime[gc-committed,dotnet.gc.last_collection.memory.committed_size]'"
                " --refresh-interval 1 --format json -o /tmp/opamp-probe.json --duration 00:00:00:05 >/dev/null 2>&1;"
                " cat /tmp/opamp-probe.json 2>/dev/null; rm -f /tmp/opamp-probe.json")

    def __call__(self, timeout: float = 60) -> tuple[float, float] | None:
        """Force a full GC in the container's runtime (PID 1: the driver's `docker run ... dotnet`),
        and read the GC heap still live and the GC's committed memory, in MB."""
        try:
            if not self.copied:
                if not all(_dll(self.tools_dir, t) for t in TOOLS):
                    return None
                cp = subprocess.run(["docker", "cp", f"{self.tools_dir / '.store'}/.", f"{self.container}:{IN_CONTAINER}"],
                                    capture_output=True, timeout=timeout)
                if cp.returncode != 0:
                    return None
                self.copied = True
            out = subprocess.run(["docker", "exec", self.container, "sh", "-c", self._script()],
                                 capture_output=True, text=True, timeout=timeout).stdout
        except (subprocess.TimeoutExpired, OSError):
            return None
        report, _, counters = out.partition("--- counters")
        heap, committed = live_heap_mb(report), gc_committed_mb(counters.strip())
        if heap is None or committed is None:
            return None
        return round(heap, 1), round(committed, 1)


def dotnet_resources_bounded(samples: list, warm: float) -> Result | None:
    """resources_bounded for a .NET client (see the module's doc); None if it isn't one, or was
    probed fewer than six times after warm-up."""
    return judge_probed([x for x in samples if isinstance(x, DotnetSample)], samples, warm,
                        heap="GC heap live after a forced GC", heap_short="GC heap live after a GC",
                        other="non-GC memory", other_short="non-GC")
