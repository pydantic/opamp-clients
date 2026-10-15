#!/usr/bin/env python3
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Torture one OpAMP client for a long time, on a seeded schedule of faults, and check it held up.

    uv run --no-project --with psutil conformance/torture/torture.py --client go-core --transport ws
    ... --minutes 120 --seed 42          # a long run; the same seed replays the same schedule
    ... --server rust                    # against the Rust server instead of the Go one

The run is a series of epochs. Each starts with a chaotic stretch — the server refuses,
rejects, throttles, closes, drops connections, restarts and forgets the agent, offers new UIDs
and configs; a proxy in front of it adds latency, splits writes into tiny pieces, stalls and
resets connections; the driver process is frozen (SIGSTOP) now and then, and a client of ours is
killed and started again on its state file; and the application keeps changing its status — then
a quiet stretch in which the client must recover. invariants.py
says what must hold, and for a client of ours opamp-tap checks its core tap against the effects'
contract (core_tap, as in run.py). Output goes to conformance/out/torture/<client>-<transport>-<seed>/.

With --server rust (where it is built: run.py's SERVERS) the server under test is the Rust one, and
it decides for itself: the schedule keeps the network, the pauses and the status changes, but of
the server faults only the events a real server has (restart, Close frame, TCP drop), and the
operator changes the desired config. The server then must end every quiet window knowing the
agent runs the config wanted at its start.
"""
from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import queue
import random
import signal
import socket
import struct
import subprocess
import sys
import threading
import time
import tomllib
from dataclasses import asdict, dataclass, field
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "runner"))
sys.path.insert(0, str(HERE))
from invariants import SERVER_CHECKS, Plan, Sample, evaluate  # noqa: E402
import dotnet_memory  # noqa: E402
import jvm_memory  # noqa: E402
from run import (OUT, ROOT, SERVERS, TAP_CHECKER, baseline_file, build, check_taps, core_taps,  # noqa: E402
                 expand, kill, load_clients, read_jsonl, said, saved_state, tap_env, wait_for)
from checks import Trace  # noqa: E402

# Known failures of the upstream clients under torture, beyond the conformance baseline.
BASELINE = HERE / "baseline.toml"
# Checks of the server: ours to pass whichever client it ran with.
OF_THE_SERVER = set(SERVER_CHECKS) | {"server_config_converges"}

# Seconds after a crash in which the new process's resources aren't sampled: it is warming up.
RESTART_SETTLE_S = 30.0

try:
    import psutil
except ImportError:  # resources are then not checked
    psutil = None


# ---------------------------------------------------------------------------------------------
# The schedule

@dataclass
class Schedule:
    seed: int
    duration_s: float
    server_faults: list[dict] = field(default_factory=list)
    proxy: list[dict] = field(default_factory=list)  # {"kind", "start", "end", ...}
    pauses: list[tuple[float, float]] = field(default_factory=list)
    changes_at_s: list[float] = field(default_factory=list)
    quiet: list[tuple[float, float]] = field(default_factory=list)
    # When the driver is killed (SIGKILL, or `docker kill`) and started again on its state file.
    crashes: list[float] = field(default_factory=list)
    # The Rust server: the desired config's body over time, from 0 (None: no config).
    desired: list[tuple[float, str | None]] = field(default_factory=list)


def config_hash(body: str | None) -> str | None:
    return None if body is None else hashlib.sha256(body.encode()).hexdigest()


def make_schedule(seed: int, minutes: float, epoch_s: float, chaos_s: float, transport: str,
                  can_pause: bool, server: str = "go", can_crash: bool = False) -> Schedule:
    """Everything that will happen, from the seed alone. Times are seconds since the server
    started (the driver starts a moment later, which only shifts its changes a little).

    The crashes come from a generator of their own, so a seed's other faults are the same with
    them or without."""
    rng = random.Random(seed)
    crash_rng = random.Random(f"crash/{seed}")
    s = Schedule(seed=seed, duration_s=minutes * 60)
    if server == "rust":
        s.desired.append((0.0, f"torture: {seed}\n"))
    epochs = max(1, int(s.duration_s // epoch_s))
    s.duration_s = epochs * epoch_s
    ws = transport == "ws"
    for e in range(epochs):
        t0 = e * epoch_s
        end_chaos = t0 + chaos_s

        def window(max_len: float) -> tuple[float, float]:
            a = rng.uniform(t0, end_chaos - 2)
            return a, min(end_chaos, a + rng.uniform(1, max_len))

        for _ in range(rng.randint(2, 5)):
            if server == "rust":
                kind = rng.choice(["restart", "close", "drop", "desire", "desire", "overload"])
            else:
                kind = rng.choice(["refuse_http", "reject", "unavailable", "full_state", "assign_uid", "offer_config",
                                   "restart", "close", "drop"])
            a, b = window(15)
            if kind == "desire":
                body = None if rng.random() < 0.15 else f"torture: {seed}-{e}-{rng.randint(0, 1 << 30)}\n"
                s.desired.append((a, body))
                s.server_faults.append({"action": "desire", "at_s": a,
                                        "config": None if body is None else {"body": body}})
            elif kind == "refuse_http":
                s.server_faults.append({"action": "http_status", "window_s": [a, b],
                                        "status": rng.choice([429, 503]), "retry_after": str(rng.randint(1, 5))})
            elif kind == "reject":
                s.server_faults.append({"action": "reject", "window_s": [a, b], "p": 0.3,
                                        "status": rng.choice([413, 400])})
            elif kind == "unavailable":
                s.server_faults.append({"action": "unavailable", "window_s": [a, b], "p": 0.3,
                                        "retry_after_s": rng.randint(1, 5)})
            elif kind == "full_state":
                s.server_faults.append({"action": "report_full_state", "window_s": [a, b], "p": 0.5})
            elif kind == "assign_uid":
                s.server_faults.append({"action": "assign_uid", "window_s": [a, b], "p": 0.2, "uid": "v7"})
            elif kind == "offer_config":
                s.server_faults.append({"action": "offer_config", "window_s": [a, b], "p": 0.3,
                                        "body": f"torture: {seed}-{e}-{rng.randint(0, 1 << 30)}\n"})
            elif kind == "overload":
                # The Rust server's own overload: everything is turned away for a while.
                s.server_faults.append({"action": "unavailable", "at_s": a, "for_s": b - a,
                                        "retry_after_s": rng.randint(1, 5)})
            elif kind == "restart":
                s.server_faults.append({"action": "restart", "at_s": a})
            elif ws:
                s.server_faults.append({"action": "close_frame" if kind == "close" else "tcp_drop", "at_s": a,
                                        "code": 1001})
        for _ in range(rng.randint(1, 4)):
            kind = rng.choice(["latency", "slice", "stall", "reset", "refuse"])
            a, b = window(10)
            ev = {"kind": kind, "start": a, "end": b}
            if kind == "latency":
                ev["ms"] = rng.choice([50, 200, 800, 2000])
            elif kind == "slice":
                ev["bytes"], ev["delay_ms"] = rng.choice([1, 7, 64]), rng.choice([0, 5, 30])
            elif kind == "reset":
                ev["end"] = a
            s.proxy.append(ev)
        if can_pause and rng.random() < 0.5:
            a = rng.uniform(t0, end_chaos - 12)
            s.pauses.append((a, a + rng.uniform(2, 10)))
        # Status changes during the chaos only, so each quiet window has one status to reach.
        t = t0 + rng.uniform(0, 5)
        while t < end_chaos - 3:
            s.changes_at_s.append(round(t, 2))
            t += rng.uniform(2, 12)
        # A crash in half the chaotic stretches, early enough that the driver is back (a JVM in a
        # container takes seconds to start) well before the quiet one.
        if can_crash and crash_rng.random() < 0.5 and chaos_s > 20:
            s.crashes.append(round(crash_rng.uniform(t0 + 5, end_chaos - 12), 2))
        s.quiet.append((end_chaos, t0 + epoch_s))
    s.desired.sort(key=lambda d: d[0])
    return s


# ---------------------------------------------------------------------------------------------
# A proxy that misbehaves on schedule

def _rst(w: asyncio.StreamWriter) -> None:
    """Close with a TCP RST rather than a FIN."""
    sock = w.get_extra_info("socket")
    if sock is not None:
        try:
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
        except OSError:
            pass
    w.transport.abort()


class Proxy:
    def __init__(self, target: tuple[str, int], schedule: list[dict], t0: float, log):
        self.target, self.schedule, self.t0, self.log = target, schedule, t0, log
        self.conns: set[tuple[asyncio.StreamWriter, asyncio.StreamWriter]] = set()

    def now(self) -> float:
        return time.monotonic() - self.t0

    def active(self, kind: str) -> dict | None:
        t = self.now()
        ev = next((e for e in self.schedule if e["kind"] == kind and e["start"] <= t < e["end"]), None)
        if ev is not None and not ev.get("hit"):
            ev["hit"] = True  # logged once: the fault touched real traffic
            self.log(f"proxy_{kind}", start=ev["start"])
        return ev

    async def serve(self, host: str, ready: "queue.Queue[int]") -> None:
        self.loop = asyncio.get_running_loop()
        srv = await asyncio.start_server(self.handle, host, 0)
        ready.put(srv.sockets[0].getsockname()[1])
        for ev in self.schedule:
            if ev["kind"] == "reset":
                asyncio.get_running_loop().call_later(max(0.0, ev["start"] - self.now()), self.reset_all)
        async with srv:
            await srv.serve_forever()

    def reset_all(self, event: str = "proxy_reset") -> None:
        self.log(event, conns=len(self.conns))
        for a, b in list(self.conns):
            _rst(a)
            _rst(b)

    def drop_dead(self) -> None:
        """From another thread, once the driver is killed: reset its connections, and with them
        whatever of its messages the proxy still holds (a latency, a stall), so that nothing the
        dead process sent reaches the server after its crash is noted."""
        asyncio.run_coroutine_threadsafe(self._drop_dead(), self.loop).result(timeout=10)

    async def _drop_dead(self) -> None:
        self.reset_all("crash_reset")

    async def handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        if self.active("refuse"):
            self.log("proxy_refused")
            _rst(writer)
            return
        try:
            ur, uw = await asyncio.open_connection(*self.target)
        except OSError:
            _rst(writer)
            return
        pair = (writer, uw)
        self.conns.add(pair)
        try:
            await asyncio.gather(self.pipe(reader, uw), self.pipe(ur, writer))
        finally:
            self.conns.discard(pair)
            for w in pair:
                w.close()

    async def pipe(self, r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
        try:
            while data := await r.read(65536):
                while self.active("stall"):
                    await asyncio.sleep(0.05)
                if lat := self.active("latency"):
                    await asyncio.sleep(lat["ms"] / 1000)
                if sl := self.active("slice"):
                    for i in range(0, len(data), sl["bytes"]):
                        w.write(data[i:i + sl["bytes"]])
                        await w.drain()
                        await asyncio.sleep(sl["delay_ms"] / 1000)
                else:
                    w.write(data)
                    await w.drain()
            w.write_eof()
        except (OSError, RuntimeError):
            pass


# ---------------------------------------------------------------------------------------------
# Running it

class Logger:
    def __init__(self, path: Path):
        self.f, self.lock = open(path, "w"), threading.Lock()

    def __call__(self, event: str, **fields) -> None:
        with self.lock:
            self.f.write(json.dumps({"event": event, "wall": time.time(), **fields}) + "\n")
            self.f.flush()


def sample(driver_pid: int, container: str | None) -> tuple[float, int | None, int | None] | None:
    if container:
        out = subprocess.run(["docker", "stats", "--no-stream", "--format", "{{.MemUsage}}|{{.PIDs}}", container],
                             capture_output=True, text=True).stdout.strip()
        if "|" not in out:
            return None
        mem, pids = out.split("|")
        num, unit = mem.split("/")[0].strip()[:-3], mem.split("/")[0].strip()[-3:]
        try:
            mb = float(num) * {"KiB": 1 / 1024, "MiB": 1, "GiB": 1024}.get(unit, 1)
            return mb, None, int(pids)
        except ValueError:  # a container stopping or gone ("--"): no figures, and the sampler goes on
            return None
    if psutil is None:
        return None
    try:
        root = psutil.Process(driver_pid)
        procs = [root, *root.children(recursive=True)]
        rss = sum(p.memory_info().rss for p in procs) / 2**20
        fds = sum(p.num_fds() for p in procs) if hasattr(root, "num_fds") else None
        threads = sum(p.num_threads() for p in procs)
        return rss, fds, threads
    except (psutil.Error, OSError):
        return None


def torture(client: str, spec: dict, transport: str, sched: Schedule, poll_s: float, out_dir: Path,
            server_name: str = "go") -> dict:
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / "schedule.json").write_text(json.dumps(asdict(sched), indent=1))
    scenario_file = out_dir / "scenario.json"
    scenario = {"faults": sched.server_faults}
    if sched.desired:
        scenario["desired_config"] = {"body": sched.desired[0][1]}
    scenario_file.write_text(json.dumps({"server": scenario}, indent=1))
    server_log, driver_log, driver_err = out_dir / "server.jsonl", out_dir / "driver.jsonl", out_dir / "driver.stderr"
    log = Logger(out_dir / "harness.jsonl")
    docker = bool(spec.get("docker"))
    # (Installed before the clock starts: the first run downloads them.)
    dotnet_tools = None
    if docker and dotnet_memory.is_dotnet(expand(spec["run"])):
        dotnet_tools = OUT / "dotnet-diag"
        if not dotnet_memory.install_tools(dotnet_tools, expand(["{nuget}"])[0]):
            log("dotnet_probe_unavailable", reason="the diagnostic tools could not be installed")
            dotnet_tools = None

    server = subprocess.Popen(SERVERS[server_name] + ["-listen", "127.0.0.1:0", "-scenario", str(scenario_file),
                               "-log", str(server_log), "-seed", str(sched.seed)],
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    ready = json.loads(server.stdout.readline())
    t0 = time.monotonic()
    server_port = int(ready["addr"].rsplit(":", 1)[1])

    port_q: "queue.Queue[int]" = queue.Queue()
    proxy = Proxy(("127.0.0.1", server_port), sched.proxy, t0, log)
    threading.Thread(target=lambda: asyncio.run(proxy.serve("0.0.0.0" if docker else "127.0.0.1", port_q)),
                     daemon=True).start()
    proxy_port = port_q.get(timeout=10)

    host = "host.docker.internal" if docker else "127.0.0.1"
    # With crashes, the client keeps its state in a file and makes its own instance UID, kept there
    # (DRIVER_UID none: a UID the driver chose afresh each start would win over the saved one).
    state = out_dir / "state"
    state.unlink(missing_ok=True)
    env = dict(os.environ,
               OPAMP_ENDPOINT=f"{'ws' if transport == 'ws' else 'http'}://{host}:{proxy_port}/v1/opamp",
               DRIVER_POLL_S=str(poll_s),
               DRIVER_UID="none" if sched.crashes else "random")
    if sched.crashes:
        env["DRIVER_STATE_FILE"] = f"/out/{state.relative_to(OUT)}" if docker else str(state)
    # A client of ours taps its core (run.py's core_tap): checked against the contract after the run.
    tapped = not spec.get("upstream")
    for old in core_taps(out_dir):
        old.unlink()
    container = None
    # A JVM in a container is probed for its heap after a GC and its non-heap memory (jvm_memory.py).
    jvm = docker and jvm_memory.is_jvm(expand(spec["run"]))
    if docker:
        container = f"opamp-torture-{client}-{transport}-{sched.seed}"

    def launch(n: int, dout, derr) -> dict:
        """Run n of the driver (0, then one per crash), from now to the schedule's end: the status
        changes still to come, starting from the last one the application made."""
        elapsed = time.monotonic() - t0
        run_s = max(1.0, sched.duration_s - elapsed)
        run_env = env | {
            "DRIVER_RUN_S": str(run_s),
            "DRIVER_CHANGES_AT_S": ",".join(str(max(0.0, round(c - elapsed, 2))) for c in sched.changes_at_s
                                            if n == 0 or c > elapsed),
            "DRIVER_FIRST_CHANGE": str(changes_said(driver_log)),
        }
        if tapped:
            run_env["OPAMP_CORE_TAP"] = tap_env(out_dir, n, docker)
        cmd = expand(spec["run"])
        if docker:
            cmd = cmd[:2] + ["--name", container] + (jvm_memory.RUN_ARGS if jvm else []) + cmd[2:]
        return {"driver": subprocess.Popen(cmd, cwd=ROOT / spec.get("cwd", "."), stdout=dout, stderr=derr,
                                           env=run_env, start_new_session=True),
                "run_s": run_s}

    # A .NET client in a container is probed for its GC heap live after a forced GC and its
    # non-GC memory (dotnet_memory.py), by tools copied into the container: its own command is
    # left as it is. (A new probe for each run: the tools go into each new container.)
    def dotnet_probe():
        return dotnet_memory.Probe(container, dotnet_tools) if docker and dotnet_tools else None

    runner_events: list[dict] = []

    def note(event: str, **fields) -> None:
        runner_events.append({"event": event, "wall": time.time(), **fields})

    samples: list[Sample] = []
    stop = threading.Event()
    with open(driver_log, "w") as dout, open(driver_err, "w") as derr:
        # The driver now running, and when it was launched: a crash replaces both.
        cur = {**launch(0, dout, derr), "launched": time.monotonic(), "dotnet": dotnet_probe()}

        def signal_driver(sig: int) -> None:
            try:
                os.killpg(cur["driver"].pid, sig)
            except (ProcessLookupError, PermissionError):
                pass  # killed by a crash meanwhile

        def pauses() -> None:
            for a, b in sched.pauses:
                if stop.wait(max(0.0, a - (time.monotonic() - t0))):
                    return
                log("pause", seconds=round(b - a, 1))
                if container:
                    subprocess.run(["docker", "pause", container], capture_output=True)
                else:
                    signal_driver(signal.SIGSTOP)
                stop.wait(b - a)
                if container:
                    subprocess.run(["docker", "unpause", container], capture_output=True)
                else:
                    signal_driver(signal.SIGCONT)
                log("resume")

        def sampler() -> None:
            next_probe = time.monotonic() + jvm_memory.PROBE_EVERY_S
            while not stop.wait(5):
                # A process just started after a crash is warming up: its figures say nothing of
                # growth.
                if time.monotonic() - cur["launched"] < RESTART_SETTLE_S and cur["launched"] > launched:
                    continue
                driver, dotnet = cur["driver"], cur["dotnet"]
                got = sample(driver.pid, container)
                if not got:
                    continue
                at, rss = round(time.monotonic() - t0, 1), round(got[0], 1)
                if dotnet:
                    # After the container's figures, so the tools' own processes aren't in them.
                    heap = nongc = None
                    if time.monotonic() >= next_probe:
                        next_probe += dotnet_memory.PROBE_EVERY_S
                        probed = dotnet()
                        # The container's memory again, after the GC, less what the GC has
                        # committed after it.
                        after = sample(driver.pid, container) if probed else None
                        if probed and after:
                            heap, nongc = probed[0], round(after[0] - probed[1], 1)
                    samples.append(dotnet_memory.DotnetSample(at, rss, got[1], got[2], heap, nongc))
                    continue
                if not jvm:
                    samples.append(Sample(at, rss, got[1], got[2]))
                    continue
                # After the container's figures, so jcmd's own JVM isn't in them.
                probed = None
                if time.monotonic() >= next_probe:
                    next_probe += jvm_memory.PROBE_EVERY_S
                    probed = jvm_memory.probe(container, rss)
                samples.append(jvm_memory.JvmSample(at, rss, got[1], got[2], *(probed or (None, None))))

        launched = cur["launched"]
        threads = [threading.Thread(target=pauses, daemon=True), threading.Thread(target=sampler, daemon=True)]
        for th in threads:
            th.start()
        # A driver runs DRIVER_RUN_S from its own start, and a JVM in a container on a loaded host
        # can take most of a minute to get there (a pause may land in it, too): the minute of grace
        # counts from when it says it started, not from its launch.
        # (wait_for's deadline is wall time, asked each time round: time.time(), not this clock.)
        started_by = time.time() + sched.duration_s + 60
        wait_for(cur["driver"], driver_log, "started", 0, lambda: started_by)
        deadline = launched + sched.duration_s + 60
        if cur["driver"].poll() is None and said(driver_log, "started"):
            log("driver_started", after_s=round(time.monotonic() - launched, 1))
            deadline = max(deadline, time.monotonic() + cur["run_s"] + 60)
        crashed: list[int] = []
        for n, at in enumerate(sched.crashes):
            driver = cur["driver"]
            while driver.poll() is None and time.monotonic() - t0 < at:
                time.sleep(0.05)
            if driver.poll() is not None:
                note("crash_missed", run=n, at_s=at, exit=driver.returncode)
                break
            if container:
                subprocess.run(["docker", "unpause", container], capture_output=True)  # if a pause is on
            kill(driver, container)
            if container:
                subprocess.run(["docker", "rm", "-f", container], capture_output=True)
            proxy.drop_dead()
            time.sleep(0.2)  # what the server had read by then, it has logged
            note("crashed", run=n, at_s=round(time.monotonic() - t0, 2), seed=sched.seed)
            crashed.append(n)
            # What the crash left of the state file: a process crash loses nothing the client wrote,
            # so the file must hold the last save its core was told was stored, or a later one.
            proc = subprocess.run([str(TAP_CHECKER), "judge", str(state), str(out_dir / f"core-{n}.tap")],
                                  capture_output=True, text=True)
            try:
                left = json.loads(proc.stdout)
            except json.JSONDecodeError:
                left = {"error": (proc.stderr or proc.stdout).strip()}
            content = state.read_bytes() if state.exists() else None
            note("crash_state", run=n, **left,
                 sha256=hashlib.sha256(content).hexdigest() if content is not None else None)
            cur.update(launch(n + 1, dout, derr), launched=time.monotonic(), dotnet=dotnet_probe())
            # As for the first run, with a minute more for the new process to start.
            deadline = max(deadline, time.monotonic() + cur["run_s"] + 120)
        try:
            exit_code = cur["driver"].wait(timeout=max(0.0, deadline - time.monotonic()))
        except subprocess.TimeoutExpired:
            signal_driver(signal.SIGCONT)
            signal_driver(signal.SIGKILL)
            exit_code = None
        stop.set()
        if container:
            subprocess.run(["docker", "rm", "-f", container], capture_output=True)
    # A run started after a crash: the state it started from (its core's Config.saved_state).
    for n in crashed:
        content = saved_state(out_dir / f"core-{n + 1}.tap")
        note("restarted", run=n + 1, sha256=hashlib.sha256(content).hexdigest() if content else None,
             bytes=len(content) if content is not None else None)
    (out_dir / "runner.jsonl").write_text("".join(json.dumps(e) + "\n" for e in runner_events))
    state.unlink(missing_ok=True)
    time.sleep(0.5)
    server.send_signal(signal.SIGTERM)
    try:
        server.wait(timeout=10)
    except subprocess.TimeoutExpired:
        server.kill()

    events = sorted(read_jsonl(server_log, "server") + read_jsonl(driver_log, "driver")
                    + read_jsonl(out_dir / "harness.jsonl", "harness") + read_jsonl(out_dir / "runner.jsonl", "runner"),
                    key=lambda e: e["wall"])
    trace = Trace(events, {"params": {}}, transport, exit_code)
    results = evaluate(trace, plan_for(sched, poll_s), samples, core_taps(out_dir) if tapped else [])
    if tapped:
        # A run the harness crashed may end its tap in a call cut off.
        r = check_taps(core_taps(out_dir), killed=[out_dir / f"core-{n}.tap" for n in crashed])
        results["core_tap"] = {"status": r.status, "detail": r.detail, "level": "MUST"}
    record = {"client": client, "transport": transport, "server": server_name, "seed": sched.seed,
              "minutes": sched.duration_s / 60,
              "driver_exit": exit_code, "results": results, "exercised": exercised(events),
              "samples": [asdict(s) for s in samples]}
    (out_dir / "result.json").write_text(json.dumps(record, indent=1))
    return record


def changes_said(driver_log: Path) -> int:
    """The last status change the drivers have said they made (0: none), for the next run to
    start from."""
    try:
        lines = driver_log.read_text(errors="replace").splitlines()
    except OSError:
        return 0
    last = 0
    for line in lines:
        if '"status_change"' in line:
            try:
                last = max(last, int(json.loads(line)["change"]))
            except (ValueError, KeyError, TypeError):
                pass
    return last


def plan_for(sched: Schedule, poll_s: float) -> Plan:
    """What the invariants need to know of a schedule."""
    stalls = [(p["start"], p["end"]) for p in sched.proxy if p["kind"] == "stall"]
    return Plan(quiet=sched.quiet, check_s=20, poll_s=poll_s, storm_per_10s=40,
                desired=[(at, config_hash(body)) for at, body in sched.desired],
                deaf=merged(stalls + [tuple(p) for p in sched.pauses]),
                slow=sorted((p["start"], p["end"]) for p in sched.proxy if slows(p)))


def merged(windows: list[tuple[float, float]]) -> list[tuple[float, float]]:
    """The windows, overlapping ones joined: a stall that begins before another (or a pause) ends
    leaves the client deaf from the first's start to the second's end, and `deaf_through` asks for
    one window that covers the whole wait."""
    out: list[tuple[float, float]] = []
    for lo, hi in sorted(windows):
        if out and lo <= out[-1][1]:
            out[-1] = (out[-1][0], max(out[-1][1], hi))
        else:
            out.append((lo, hi))
    return out


def slows(p: dict) -> bool:
    """A proxy fault that delays data by seconds: a latency of 800 ms or more on every read (with a
    heartbeat every 2 s and status changes, reads queue behind it), or pieces sent with a pause
    between (a kilobyte at 1 byte per 30 ms takes half a minute). 50 and 200 ms latencies, and
    pieces sent back to back, only add a little to each message."""
    return (p["kind"] == "latency" and p["ms"] >= 800) or (p["kind"] == "slice" and p["delay_ms"] > 0)


def exercised(events: list[dict]) -> dict[str, int]:
    """How often each kind of fault actually hit the client (not just how often it was planned)."""
    counts: dict[str, int] = {}

    def bump(k: str) -> None:
        counts[k] = counts.get(k, 0) + 1

    for e in events:
        ev = e["event"]
        if ev in ("restart", "close_frame_sent", "tcp_drop", "request_rejected", "pause", "desired", "crashed") \
                or ev.startswith("proxy_"):
            bump(ev)
        elif ev == "s2a":
            if e.get("error"):
                bump(e["error"])
            if e.get("http_status", 200) != 200:
                bump(f"http_{e['http_status']}")
            for k in ("assign_uid", "offer_config"):
                if e.get(k):
                    bump(k)
            if e.get("full_state_reason"):
                bump(f"report_full_state_{e['full_state_reason']}")
    return dict(sorted(counts.items()))


def known_failures(client: str, transport: str, server: str, upstream: bool,
                   baseline: Path = BASELINE) -> dict[str, str]:
    """The checks a client is known to fail, each with where that is recorded.

    For an upstream client: what the conformance baseline of the same server lists as failing or
    flaky, and what `baseline` lists, except checks of the server. Those, and anything of a client
    of ours, are excused only by an entry of `baseline` that gives its reason: a bug of ours,
    owned and tracked elsewhere."""
    known: dict[str, str] = {}
    if upstream:
        conformance = baseline_file(server)
        if conformance.exists():
            entry = tomllib.loads(conformance.read_text()).get(f"{client}/{transport}", {})
            for name in entry.get("fail", []) + entry.get("flaky", []):
                known[name] = str(conformance.relative_to(ROOT))
    entry = tomllib.loads(baseline.read_text()).get(server, {}).get(f"{client}/{transport}", {})
    reason = entry.get("reason", "").strip()
    where = baseline.relative_to(ROOT) if baseline.is_relative_to(ROOT) else baseline
    for name in entry.get("known", []):
        known[name] = f"{where}: {reason}" if reason else str(where)
    return {k: v for k, v in known.items()
            if (reason and k in entry.get("known", [])) or (upstream and k not in OF_THE_SERVER)}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--client", required=True)
    ap.add_argument("--transport", choices=["http", "ws"], required=True)
    ap.add_argument("--seed", type=int, default=None, help="default: from the clock (printed)")
    ap.add_argument("--minutes", type=float, default=10)
    ap.add_argument("--epoch-s", type=float, default=120, help="one chaotic stretch plus one quiet stretch")
    ap.add_argument("--chaos-s", type=float, default=45)
    ap.add_argument("--poll-s", type=float, default=2)
    ap.add_argument("--server", default="go", choices=sorted(SERVERS), help="the server the client talks to")
    ap.add_argument("--crashes", action=argparse.BooleanOptionalAction, default=True,
                    help="kill the driver now and then and start it again on its state file (clients of ours "
                         "that keep their instance UID)")
    args = ap.parse_args()

    clients = load_clients(None)
    if args.client not in clients:
        sys.exit(f"no client {args.client} here (clients.toml; one built from the core's sources needs them)")
    spec = clients[args.client]
    if args.transport not in spec["transports"]:
        sys.exit(f"{args.client} does not support {args.transport}")
    seed = args.seed if args.seed is not None else int(time.time())
    build(clients, [args.client], args.server)
    can_crash = (args.crashes and not spec.get("upstream")
                 and "instance_uid_kept" in spec.get("features", []))
    sched = make_schedule(seed, args.minutes, args.epoch_s, args.chaos_s, args.transport, can_pause=True,
                          server=args.server, can_crash=can_crash)
    print(f"torture {args.client}/{args.transport} on the {args.server} server, seed {seed}: "
          f"{sched.duration_s / 60:.0f} min, "
          f"{len(sched.server_faults)} server faults, {len(sched.proxy)} network faults, "
          f"{len(sched.pauses)} pauses, {len(sched.crashes)} crashes, {len(sched.changes_at_s)} status changes",
          flush=True)
    prefix = "" if args.server == "go" else f"{args.server}-server/"
    out_dir = OUT / "torture" / f"{prefix}{args.client}-{args.transport}-{seed}"
    record = torture(args.client, spec, args.transport, sched, args.poll_s, out_dir, args.server)
    known = known_failures(args.client, args.transport, args.server, bool(spec.get("upstream")))
    bad = False
    for name, r in record["results"].items():
        mark = {"pass": "ok  ", "skip": "-   "}.get(r["status"], "FAIL" if r["level"] == "MUST" else "warn")
        if mark == "FAIL" and name in known:
            mark = "known"
        bad |= mark == "FAIL"
        print(f"{mark:4} {name:28} {r['detail']}" + (f" (known: {known[name]})" if mark == "known" else ""))
    print("faults that hit: " + ", ".join(f"{k} {v}" for k, v in record["exercised"].items()))
    print(f"logs in {out_dir.relative_to(ROOT)}; replay with --seed {seed}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
