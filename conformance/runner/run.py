#!/usr/bin/env python3
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Run conformance scenarios against OpAMP clients and check the recorded traces.

    conformance/runner/run.py                          # every client, scenario and transport
    conformance/runner/run.py --client go --scenario close   # filters (substring match)
    conformance/runner/run.py --client =go                   # exactly that client
    conformance/runner/run.py --list                   # show clients, scenarios and checks
    conformance/runner/run.py --server rust            # the clients against the Rust server
    conformance/runner/run.py --jobs 4                 # up to four runs at once

The scenarios run against the Go server (conformance/server) unless --server rust picks the Rust
reference server, which is built from the core's sources and so is there only where they are
(OPAMP_CORE_ROOT: a checkout with the core's sources, where what comes from them is built and runs
from its target/). A scenario lists the servers it runs on under
"servers" (default: go); only the Go server injects faults, serves TLS or runs a proxy. A client
whose clients.toml table says `source = true` is built from those sources too, and is left out
where they aren't.

Output goes to conformance/out/: per run, the server log, the driver log and the results;
results.json and a summary table for the whole invocation.

Every client of ours runs its core with OPAMP_CORE_TAP set: each driver process records every call
into its core in the run's directory (core-<n>.tap, one per driver run), and opamp-tap (the core's
tap checker) checks them after the scenario, a MUST of its own (core_tap): every effect's lifetime
keeps the core's effects contract (spec/shell-contract.md), every reply is the core's, and every
save the client answered stored was on the disk (the file operations it records, on a disk that can
crash at any point).

A scenario's driver can be crashed ("crash": {"runs": [n...], "from_s": a, "to_s": b}): run n is
killed (SIGKILL, or docker kill) at a moment picked from the scenario's seed, a to b seconds after
the driver said it started (b: the run's end); then `opamp-tap crash` writes what a crash of the machine could have left of the state file's
directory (from the run's tap: unflushed data lost, in part or whole), and the next run starts on
that. The moment and the crash come from --crash-seed if given (a replay; the result prints the
seed); --crash-seeds N runs each crash scenario on N seeds from there (or from the scenario's).
With "overlap": true, the next run starts as soon as the one before says it stopped,
while it lingers (DRIVER_LINGER_S): a restart in process on the same state file.

With --lazyfs DIR, every run's state file is on a LazyFS mount of its own under DIR (Linux with
FUSE: the container of conformance/lazyfs/), and a crash is real: the runner kills the driver,
LazyFS drops every byte not flushed (or, in a scenario whose crash says "tear": true, on a third of
the seeds, tears the first or second write of the state's temporary file and dies: the crash is
then), the file system is mounted again, and `opamp-tap judge` says what the
state file holds then, for crash_state_consistent (conformance/runner/lazyfs.py).
--clients-override FILE merges FILE's tables over clients.toml's (there: the drivers run natively).

--differential compares the clients' taps of each scenario and transport after the runs
(conformance/differential/shells.py; opamp-tap diff): what each told its core should differ
only in timing, or by a difference conformance/differential/shells.toml allows with its reason.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import time
import threading
import tomllib
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from checks import CHECKS, TAP_CHECK, Result, Trace  # noqa: E402
import lazyfs  # noqa: E402

CONF = Path(__file__).resolve().parent.parent
ROOT = CONF.parent
OUT = CONF / "out"
BIN = OUT / "bin"


def core_root(env: dict[str, str] = os.environ) -> Path:
    """Where the core's sources are: OPAMP_CORE_ROOT if set (a checkout with the core's sources),
    else the root, which has none. What is built from them is built there, and runs from its
    target/."""
    given = env.get("OPAMP_CORE_ROOT")
    if not given:
        return ROOT
    root = Path(given).resolve()
    if not (root / "Cargo.toml").is_file():
        sys.exit(f"OPAMP_CORE_ROOT={given}: no Cargo.toml there")
    return root


CORE_ROOT = core_root()
# Whether the core's sources are here (OPAMP_CORE_ROOT): what is built from them (the Rust server,
# the clients marked `source`, opamp-tap) runs only then; elsewhere opamp-tap is the one the core's
# release carries (scripts/fetch_core.py fetch puts it in the root's target/).
CORE_SOURCE = CORE_ROOT != ROOT
TAP_CHECKER = (CORE_ROOT if CORE_SOURCE else ROOT) / "target" / "release" / "opamp-tap"


def sh(cmd: list[str], cwd: Path) -> None:
    proc = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    if proc.returncode != 0:
        sys.exit(f"build failed in {cwd}: {' '.join(cmd)}\n{proc.stdout}{proc.stderr}")


def expand(parts: list[str]) -> list[str]:
    # {user}: a container runs as the runner's own user, so what it writes under out/ (a state
    # file, a private downloads directory) stays readable by the runner and by artifact uploads.
    user = f"{os.getuid()}:{os.getgid()}" if hasattr(os, "getuid") else "0:0"
    # {m2}, {nuget}: the Docker volumes Maven's and NuGet's caches are kept in, for a run that
    # keeps its own (CONFORMANCE_M2_VOLUME, CONFORMANCE_NUGET_VOLUME); {maven}: the image the JVM
    # drivers build and run in, for a run that names its own (CONFORMANCE_MAVEN_IMAGE: one of the
    # host's architecture, say, where the shared tag is another's).
    m2 = os.environ.get("CONFORMANCE_M2_VOLUME") or "opamp-clients-m2"
    nuget = os.environ.get("CONFORMANCE_NUGET_VOLUME") or "opamp-clients-nuget"
    maven = os.environ.get("CONFORMANCE_MAVEN_IMAGE") or "maven:3.9-eclipse-temurin-21"
    # {core}: where the core's sources are (CORE_ROOT), for a client built from them.
    return [p.format(bin=BIN, root=ROOT, core=CORE_ROOT, conf=CONF, user=user, m2=m2, nuget=nuget, maven=maven)
            for p in parts]


SERVERS = {"go": [str(BIN / "server")]}
if CORE_SOURCE:
    # The suite's checks read every message and reply from the log.
    SERVERS["rust"] = [str(CORE_ROOT / "target" / "release" / "opamp-server"), "-log-messages"]


def load_clients(override: Path | None) -> dict:
    """clients.toml, with the tables of `override` (--clients-override) merged over its own, less
    the clients built from the core's sources where they aren't here."""
    clients = tomllib.loads((CONF / "clients.toml").read_text())
    if override is not None:
        for name, table in tomllib.loads(override.read_text()).items():
            clients[name] = {**clients.get(name, {}), **table}
    return {name: c for name, c in clients.items() if CORE_SOURCE or not c.get("source")}


def need_docker(clients: dict, wanted: list[str]) -> list[str]:
    """The clients of `wanted` whose driver runs in a container (docker) or whose build needs one
    (native: the core built for Linux in Docker, scripts/native_lib.py)."""
    return [c for c in wanted if clients[c].get("docker") or clients[c].get("native")]


def build(clients: dict, wanted: list[str], server: str) -> None:
    BIN.mkdir(parents=True, exist_ok=True)
    if server == "rust":
        sh(["cargo", "build", "--release", "-p", "opamp-server"], CORE_ROOT)
    else:
        sh(["go", "build", "-o", str(BIN / "server"), "."], CONF / "server")
    # opamp-tap: built beside its source; elsewhere the core's release carries it.
    if any(not clients[name].get("upstream") for name in wanted):
        if CORE_SOURCE:
            sh(["cargo", "build", "--release", "-p", "opamp-tap"], CORE_ROOT)
        elif not TAP_CHECKER.exists():
            sys.exit(f"no {TAP_CHECKER.relative_to(ROOT)}: run scripts/fetch_core.py fetch")
    # native: the client's build in Docker takes the native core built for Linux (scripts/native_lib.py).
    if any(clients[name].get("native") for name in wanted):
        sh([sys.executable, str(ROOT / "scripts" / "native_lib.py")], ROOT)
    # A client built from the core's sources is built where they are.
    for name in wanted:
        b = clients[name].get("build")
        if b:
            sh(expand(b["cmd"]), (CORE_ROOT if clients[name].get("source") else ROOT) / b["cwd"])


def read_jsonl(path: Path, source: str) -> list[dict]:
    events = []
    if not path.exists():
        return events
    for line in path.read_text(errors="replace").splitlines():
        try:
            e = json.loads(line)
        except json.JSONDecodeError:
            e = {"event": "output", "line": line}
        if not isinstance(e, dict):
            e = {"event": "output", "line": line}
        e["source"] = source
        events.append(e)
    # Lines without a timestamp (stray output) sort next to the previous event.
    last = 0.0
    for e in events:
        last = e.setdefault("wall", last)
    return events


def core_taps(run_dir: Path) -> list[Path]:
    """The core taps a run's drivers wrote: core-<n>.tap, and core-<n>.tap.<pid>.<k> for a
    process's later cores."""
    return sorted(run_dir.glob("core-*.tap*"))


def tap_env(run_dir: Path, n: int, docker: bool) -> str:
    """OPAMP_CORE_TAP for driver run `n`, as the driver sees it (containers see conformance/out
    as /out)."""
    path = run_dir / f"core-{n}.tap"
    return f"/out/{path.relative_to(OUT)}" if docker else str(path)


def check_taps(taps: list[Path], checker: Path = TAP_CHECKER, killed: list[Path] = ()) -> Result:
    """opamp-tap on a run's core taps: every effect's lifetime against the contract. `killed`:
    the taps of runs the runner killed (crash scenarios), which may end in a call cut off."""
    if not taps:
        return Result("fail", "no core tap was written: the client ran without OPAMP_CORE_TAP, or its core never "
                              "started")
    marks = [a for k in killed for a in ("--killed", str(k))]
    proc = subprocess.run([str(checker), "--quiet", *marks, *map(str, taps)], capture_output=True, text=True)
    lines = [ln.strip() for ln in proc.stdout.splitlines() if ln.strip()]
    if proc.returncode == 0:
        return Result("pass", lines[-1] if lines else "")
    broken = [ln.replace(str(OUT) + "/", "") for ln in lines if not ln.startswith("FAIL")][:6]
    return Result("fail", "the core tap breaks the effects' contract: "
                  + "; ".join(broken or lines[:6] or [proc.stderr.strip()]))


def saved_state(tap: Path) -> bytes | None:
    """The saved state a tap's first core started from (Config.saved_state, field 14 of the
    Config in the first record), or None if the tap has no init."""
    try:
        data = tap.read_bytes()
    except OSError:
        return None
    if len(data) < 13 or data[0] != 1:
        return None
    length = int.from_bytes(data[1:5], "little")
    config, i = data[13:13 + length], 0

    def varint() -> int:
        nonlocal i
        shift = value = 0
        while i < len(config):
            b = config[i]
            i += 1
            value |= (b & 0x7F) << shift
            shift += 7
            if b < 0x80:
                break
        return value

    found = b""
    while i < len(config):
        key = varint()
        field, wire = key >> 3, key & 7
        if wire == 0:
            varint()
        elif wire == 1:
            i += 8
        elif wire == 5:
            i += 4
        elif wire == 2:
            n = varint()
            if field == 14:
                found = config[i:i + n]
            i += n
        else:
            return None
    return found


def crash_moment(seed: int, n: int, from_s: float, run_s: float) -> float:
    """When run n is crashed: from_s to run_s, picked by the seed."""
    frac = int(hashlib.sha256(f"{seed}/{n}".encode()).hexdigest()[:8], 16) / 0xFFFFFFFF
    return from_s + frac * max(0.0, run_s - from_s)


def kill(driver: subprocess.Popen, container: str | None) -> None:
    """Kill a driver, and its container if it runs in one (killing `docker run` leaves it)."""
    if container:
        subprocess.run(["docker", "kill", container], capture_output=True)
    try:
        os.killpg(driver.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        # Gone already (the container's kill ended `docker run`; macOS says EPERM for a group
        # whose leader has exited and not been reaped).
        pass
    try:
        driver.wait(timeout=30)
    except subprocess.TimeoutExpired:
        pass


def said(driver_log: Path, event: str) -> int:
    """How many `event` events (started, stopped) the drivers have written so far."""
    try:
        return sum(1 for line in driver_log.read_text(errors="replace").splitlines() if f'"{event}"' in line)
    except OSError:
        return 0


def event_wall(driver_log: Path, event: str, k: int) -> float | None:
    """The wall time of the k-th (from 1) `event` (started, stopped) the drivers wrote, if there
    is one."""
    try:
        lines = driver_log.read_text(errors="replace").splitlines()
    except OSError:
        return None
    seen = 0
    for line in lines:
        if f'"{event}"' not in line:
            continue
        try:
            e = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(e, dict) and e.get("event") == event:
            seen += 1
            if seen == k:
                return e.get("wall")
    return None


#: How long a driver may take to say it started: a JVM in a container on a loaded machine takes
#: 25 s (CI, where a run counted from its process's start was killed after its stop for that).
START_S = 120.0
#: How long a driver may take to end once it said stopped and lingered; and, until it says stopped,
#: past its run, counted from when it said it started.
EXIT_S = 30.0


def driver_deadline(driver_log: Path, before: dict[str, int], spawned: float, run_s: float,
                    linger: float) -> float:
    """The wall time by which a driver run must have ended, from what it has said so far (its
    events after `before`'s counts): EXIT_S after its stop and linger, once it said stopped; else
    EXIT_S after its run and linger, counted from when it said started; else START_S more, from
    when it was spawned. A slow start doesn't eat the time it has to stop and end."""
    stopped = event_wall(driver_log, "stopped", before["stopped"] + 1)
    if stopped is not None:
        return stopped + linger + EXIT_S
    started = event_wall(driver_log, "started", before["started"] + 1)
    if started is not None:
        return started + run_s + linger + EXIT_S
    return spawned + START_S + run_s + linger + EXIT_S


def wait_driver(driver: subprocess.Popen, deadline) -> int | None:
    """Wait for a driver to end by `deadline()` (wall time, read again as the driver says more): its
    exit code, or None if it is still running then (the caller kills it)."""
    while True:
        code = driver.poll()
        if code is not None:
            return code
        if time.time() >= deadline():
            return None
        time.sleep(0.05)


def watch_exit(driver: subprocess.Popen, exits: dict[int, float], n: int) -> None:
    """Note when driver run n's process ends (wall time), on a thread of its own: how long a driver
    takes to exit after it said stopped is measured from it."""
    def wait() -> None:
        driver.wait()
        exits[n] = time.time()
    threading.Thread(target=wait, daemon=True).start()


def wait_for(driver: subprocess.Popen, driver_log: Path, event: str, before: int, deadline) -> None:
    """Wait until the driver has said `event` once more than `before`, exited, or `deadline()`
    (wall time)."""
    while time.time() < deadline() and driver.poll() is None and said(driver_log, event) <= before:
        time.sleep(0.05)


CRASH_SEED: int | None = None
#: A driver that takes longer than this to exit after it said stopped (beyond its linger) is named
#: in the run's line.
SLOW_EXIT_S = 5.0
#: --lazyfs: where each run's LazyFS mount goes (None: the state file is on the disk as it is).
LAZYFS_DIR: Path | None = None


def torn_write(seed: int, n: int) -> tuple[int, int] | None:
    """On LazyFS, whether run n's crash is a torn write of the state's temporary file rather than a
    kill (a third of them, by the seed): which write to it (1 or 2) and which half is kept."""
    v = int(hashlib.sha256(f"{seed}/{n}/torn".encode()).hexdigest()[:8], 16)
    return (1 + (v >> 8) % 2, 1 + (v >> 16) % 2) if v % 3 == 0 else None


def run_dir_of(client: str, name: str, transport: str, server_name: str, crash_seed: int | None) -> Path:
    leaf = f"{name}-{transport}" + (f"-seed{crash_seed}" if crash_seed is not None else "")
    return (OUT if server_name == "go" else OUT / f"{server_name}-server") / client / leaf


def lazyfs_base(run_dir: Path) -> Path:
    """Where run_dir's LazyFS mount goes: a name of its own, as LazyFS misreads its arguments
    when a path holds "-o" (credential-offer)."""
    assert LAZYFS_DIR is not None
    return LAZYFS_DIR / hashlib.sha256(str(run_dir.relative_to(OUT)).encode()).hexdigest()[:16]


def run_safely(client: str, spec: dict, scenario: dict, transport: str, server_name: str,
               crash_seed: int | None = None) -> dict:
    """run_one; a run the runner itself couldn't carry out (LazyFS wouldn't mount, say) is an
    error of that run's, not the end of all of them."""
    try:
        return run_one(client, spec, scenario, transport, server_name, crash_seed)
    except Exception as e:  # noqa: BLE001
        run_dir = run_dir_of(client, scenario["name"], transport, server_name, crash_seed)
        if LAZYFS_DIR is not None:
            lazyfs.Mount(lazyfs_base(run_dir)).stop()
        c = CHECKS["driver_survives"]
        return {"client": client, "scenario": scenario["name"], "transport": transport, "server": server_name,
                "driver_exit": None, "seconds": 0.0, **({"crash_seed": crash_seed} if crash_seed is not None else {}),
                "results": {"driver_survives": {"status": "error", "detail": f"the runner failed: {type(e).__name__}: {e}",
                                                "level": c.level, "covers": c.covers, "subject": c.subject}}}


def run_one(client: str, spec: dict, scenario: dict, transport: str, server_name: str,
            crash_seed: int | None = None) -> dict:
    name = scenario["name"]
    run_dir = run_dir_of(client, name, transport, server_name, crash_seed)
    run_dir.mkdir(parents=True, exist_ok=True)
    scenario_file = run_dir / "scenario.json"
    scenario_file.write_text(json.dumps(scenario, indent=1))
    server_log, driver_log, driver_err = run_dir / "server.jsonl", run_dir / "driver.jsonl", run_dir / "driver.stderr"

    listen = "0.0.0.0:0" if spec.get("docker") else "127.0.0.1:0"
    server_cfg = scenario.get("server", {})
    server_args = ["-listen", listen, "-scenario", str(scenario_file), "-log", str(server_log)]
    if server_cfg.get("tls"):
        # Only the Go server takes this flag; scenarios with TLS run on it alone.
        server_args += ["-certs", str(run_dir)]
    server = subprocess.Popen(SERVERS[server_name] + server_args,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    ready = json.loads(server.stdout.readline())
    port = ready["addr"].rsplit(":", 1)[1]
    # A scenario can name the host (say localhost: a cookie jar ignores cookies from an IP address).
    host = "host.docker.internal" if spec.get("docker") else scenario.get("host", "127.0.0.1")
    scheme = "ws" if transport == "ws" else "http"
    tls_files = ready.get("tls") or {}
    if tls_files:
        scheme += "s"

    def driver_path(path: str | None) -> str:
        """A file the server wrote, as the driver sees it: containers see conformance/out as /out."""
        if not path:
            return ""
        return f"/out/{Path(path).relative_to(OUT)}" if spec.get("docker") else path

    proxy_url = ""
    if ready.get("proxy"):
        proxy = server_cfg["proxy"]
        auth = proxy.get("auth")
        creds = f"{auth['user']}:{auth['pass']}@" if auth else ""
        proxy_scheme = "socks5" if proxy["kind"] == "socks5" else "http"
        proxy_url = f"{proxy_scheme}://{creds}{host}:{ready['proxy'].rsplit(':', 1)[1]}"
    driver_cfg = scenario.get("driver", {})
    # State the client keeps across restarts: in the run's directory, which containers see as /out;
    # or on a LazyFS mount of the run's own.
    fs = None
    if LAZYFS_DIR is not None:
        base = lazyfs_base(run_dir)
        if base.exists():
            lazyfs.Mount(base).stop()  # a mount a run before left, if any
            shutil.rmtree(base)
        for log in ("lazyfs.log", "lazyfs.out"):
            (run_dir / log).unlink(missing_ok=True)
        fs = lazyfs.Mount(base, logs=run_dir)
        fs.start()
        state = fs.mnt / "state.bin"
    else:
        state = run_dir / "state.bin"
    state.unlink(missing_ok=True)
    env = dict(os.environ,
               OPAMP_ENDPOINT=f"{scheme}://{host}:{port}/v1/opamp",
               DRIVER_POLL_S=str(driver_cfg.get("poll_s", 2)),
               DRIVER_UID=str(driver_cfg.get("uid", "random")),
               DRIVER_CHANGES_AT_S=",".join(str(x) for x in driver_cfg.get("changes_at_s", [])),
               DRIVER_TOKEN=driver_cfg.get("token", ""),
               DRIVER_ACCEPT_CONNECTION_SETTINGS="1" if driver_cfg.get("accept_connection_settings") else "",
               DRIVER_OWN_TELEMETRY="1" if driver_cfg.get("own_telemetry") else "",
               DRIVER_COMPONENTS="1" if driver_cfg.get("components") else "",
               DRIVER_CUSTOM="1" if driver_cfg.get("custom") else "",
               DRIVER_PACKAGES="1" if driver_cfg.get("packages") else "",
               DRIVER_REQUEST_UID="1" if driver_cfg.get("request_uid") else "",
               DRIVER_THROW_ON_CONFIG="1" if driver_cfg.get("throw_on_config") else "",
               # A scenario whose package files are served over TLS names their CA (the core feature).
               DRIVER_CA_FILE=driver_path(tls_files.get("ca_file") or ready.get("download_ca_file")),
               DRIVER_CLIENT_CERT=driver_path(tls_files.get("client_cert")),
               DRIVER_CLIENT_KEY=driver_path(tls_files.get("client_key")),
               DRIVER_PROXY=proxy_url,
               DRIVER_LINGER_S=str(driver_cfg.get("linger_s", "")),
               DRIVER_MAX_SEND_BYTES=str(driver_cfg.get("max_send_bytes", "")),
               DRIVER_DESCRIPTION_PAD=str(driver_cfg.get("description_pad", "")),
               DRIVER_STATE_FILE=f"/out/{state.relative_to(OUT)}" if spec.get("docker") else str(state))
    # The driver runs once, or once per entry of "runs" (seconds each): a restart in between.
    runs = driver_cfg.get("runs", [scenario["duration_s"]])
    tapped = not spec.get("upstream")
    for old in core_taps(run_dir):
        old.unlink()
    crash = driver_cfg.get("crash") or {}
    if crash_seed is None:
        crash_seed = CRASH_SEED if CRASH_SEED is not None else int(scenario.get("seed", 1))
    runner_log = run_dir / "runner.jsonl"
    runner_events: list[dict] = []

    def note(event: str, **fields) -> None:
        runner_events.append({"event": event, "wall": time.time(), **fields})

    started = time.monotonic()
    exit_code: int | None = 0
    # Drivers still running: (process, container, run index, deadline: wall time, read again).
    running: list[tuple[subprocess.Popen, str | None, int, float]] = []
    crashed: set[int] = set()
    # Per run: how many stopped the drivers had said before it started, and when its process ended.
    stops_before: dict[int, int] = {}
    exits: dict[int, float] = {}
    with open(driver_log, "w") as out, open(driver_err, "w") as err:
        for n, run_s in enumerate(runs):
            # Each driver process taps its core to a file of its own.
            tap = {"OPAMP_CORE_TAP": tap_env(run_dir, n, bool(spec.get("docker")))} if tapped else {}
            cmd = expand(spec["run"])
            container = None
            if spec.get("docker") and cmd[:2] == ["docker", "run"]:
                # Named, so a kill reaches the container: killing `docker run` leaves it running.
                container = re.sub(r"[^a-zA-Z0-9_.-]", "-", f"opamp-conf-{os.getpid()}-{client}-{name}-{transport}-{n}")
                cmd = cmd[:2] + ["--name", container] + cmd[2:]
            before = {e: said(driver_log, e) for e in ("started", "stopped")}
            tear = (torn_write(crash_seed, n) if fs is not None and crash.get("tear") and n in crash.get("runs", [])
                    else None)
            if tear is not None:
                # Armed before the driver starts: the write it names may come at once.
                fs.tear(Path(f"{state}.tmp"), *tear)
            driver = subprocess.Popen(cmd, cwd=ROOT / spec.get("cwd", "."), stdout=out, stderr=err,
                                      env=env | {"DRIVER_RUN_S": str(run_s)} | tap, start_new_session=True)
            stops_before[n] = before["stopped"]
            watch_exit(driver, exits, n)
            linger = float(driver_cfg.get("linger_s", 0) or 0)
            # Read again as the driver says more: a slow start doesn't eat its time to stop and end.
            deadline = (lambda b=dict(before), at=time.time(), r=float(run_s), w=linger:
                        driver_deadline(driver_log, b, at, r, w))
            if n in crash.get("runs", []):
                # From when the driver says it started: a JVM in a container takes seconds to.
                wait_for(driver, driver_log, "started", before["started"], deadline)
                at = crash_moment(crash_seed, n, float(crash.get("from_s", 0.5)), float(crash.get("to_s", run_s)))
                crash_at = time.monotonic() + at
                # Until the moment, or a torn write has crashed LazyFS (the crash is then).
                code = driver.poll()
                while code is None and time.monotonic() < crash_at and (fs is None or fs.alive()):
                    time.sleep(0.01)
                    code = driver.poll()
                if code is not None:
                    note("crash_missed", run=n, at_s=round(at, 2), exit=code)
                    if exit_code == 0:
                        exit_code = code
                    if tear is not None:
                        fs.reboot()  # the torn write it was armed for never came: disarmed
                else:
                    kill(driver, container)
                    crashed.add(n)
                    if fs is not None:
                        # A real crash: LazyFS drops what wasn't flushed (or tore a write and died,
                        # dropping it all), then the machine comes back on the same disk.
                        how = "torn" if not fs.alive() else "clear-cache" if fs.clear_cache() else "clear-cache-unanswered"
                        fs.reboot()
                        note("crashed", run=n, at_s=round(at, 2), seed=crash_seed, lazyfs=how,
                             torn=list(tear) if how == "torn" and tear else None)
                        proc = subprocess.run([str(TAP_CHECKER), "judge", str(state), str(run_dir / f"core-{n}.tap")],
                                              capture_output=True, text=True)
                    else:
                        note("crashed", run=n, at_s=round(at, 2), seed=crash_seed)
                        # What a crash of the machine could have left: from the run's tap, picked by
                        # the seed, written where the next run starts from.
                        proc = subprocess.run([str(TAP_CHECKER), "crash", "--seed", str(crash_seed * 1000 + n), "--dir",
                                               str(run_dir), str(run_dir / f"core-{n}.tap")], capture_output=True,
                                              text=True)
                    try:
                        left = json.loads(proc.stdout)
                    except json.JSONDecodeError:
                        left = {"error": (proc.stderr or proc.stdout).strip()}
                    content = state.read_bytes() if state.exists() else None
                    note("crash_state", run=n, **left,
                         sha256=hashlib.sha256(content).hexdigest() if content is not None else None)
                continue
            if driver_cfg.get("overlap") and n + 1 < len(runs):
                # The next run starts once this one says it stopped (it lingers on, in process).
                wait_for(driver, driver_log, "stopped", before["stopped"], deadline)
                note("next_run", after=n)
                running.append((driver, container, n, deadline))
                continue
            running.append((driver, container, n, deadline))
            for d, c, k, ends_by in running:
                code = wait_driver(d, ends_by)
                if code is None:
                    if fs is not None:
                        # What a driver that won't end waits on (a file system call that hangs?).
                        ps = subprocess.run(["ps", "-eLo", "pid,ppid,lwp,stat,wchan:32,args"], capture_output=True, text=True)
                        (run_dir / "hung.txt").write_text(ps.stdout)
                    kill(d, c)
                # The first run that fails is the one that counts.
                if exit_code == 0:
                    exit_code = code
            running = []
        for d, c, _, ends_by in running:
            code = wait_driver(d, ends_by)
            if code is None:
                kill(d, c)
            if exit_code == 0:
                exit_code = code
    # How long each driver took to end after it said stopped, beyond DRIVER_LINGER_S: its runtime
    # shutting down (and, for a driver in a container, the container's). Something the client left
    # running past stop shows here first.
    time.sleep(0.1)  # the exit watchers note the last exits
    lags: dict[int, float] = {}
    for n in sorted(exits):
        said_at = event_wall(driver_log, "stopped", stops_before[n] + 1) if n not in crashed else None
        if said_at is not None:
            lags[n] = round(exits[n] - said_at - float(driver_cfg.get("linger_s", 0) or 0), 2)
            note("exited", run=n, after_stopped_s=lags[n])
    # A run started after a crash: the state it started from (its core's Config.saved_state).
    for n in sorted(crashed):
        if n + 1 < len(runs):
            content = saved_state(run_dir / f"core-{n + 1}.tap")
            note("restarted", run=n + 1,
                 sha256=hashlib.sha256(content).hexdigest() if content else None,
                 bytes=len(content) if content is not None else None)
    runner_log.write_text("".join(json.dumps(e) + "\n" for e in runner_events))
    elapsed = time.monotonic() - started
    # A container writes the state as root, readable by root only: the results upload couldn't
    # read it. The directory is ours, so it can go.
    state.unlink(missing_ok=True)
    if fs is not None:
        fs.stop()
    time.sleep(0.5)
    server.send_signal(signal.SIGTERM)
    try:
        server.wait(timeout=10)
    except subprocess.TimeoutExpired:
        server.kill()

    events = sorted(read_jsonl(server_log, "server") + read_jsonl(driver_log, "driver")
                    + read_jsonl(runner_log, "runner"), key=lambda e: e["wall"])
    trace = Trace(events, scenario, transport, exit_code)
    results = {}
    for check_name in scenario["checks"]:
        c = CHECKS[check_name]
        if transport not in c.transports:
            continue
        try:
            r = c.fn(trace)
        except Exception as exc:  # a checker bug must not look like a client pass
            r = Result("error", f"{type(exc).__name__}: {exc}")
        results[check_name] = {"status": r.status, "detail": r.detail, "level": c.level, "covers": c.covers,
                               "subject": c.subject}
    if tapped:
        c = CHECKS[TAP_CHECK]
        r = check_taps(core_taps(run_dir), killed=[run_dir / f"core-{n}.tap" for n in sorted(crashed)])
        results[TAP_CHECK] = {"status": r.status, "detail": r.detail, "level": c.level, "covers": c.covers,
                              "subject": c.subject}
    record = {"client": client, "scenario": name, "transport": transport, "server": server_name,
              "driver_exit": exit_code, **({"crash_seed": crash_seed} if crash else {}),
              **({"lazyfs": True} if fs is not None else {}),
              "seconds": round(elapsed, 1), "exit_after_stopped_s": max(lags.values(), default=None),
              "results": results}
    (run_dir / "result.json").write_text(json.dumps(record, indent=1))
    return record


def cell(r: dict) -> str:
    if r["status"] == "fail":
        return "FAIL" if r["level"] == "MUST" else "warn"
    return {"pass": "ok", "skip": "-", "error": "ERR"}[r["status"]]


RANK = {"": -1, "-": -1, "ok": 0, "warn": 2, "FAIL": 3, "ERR": 4}


def worst_cells(records: list[dict]) -> dict[tuple[str, str, str], str]:
    """(check, client, transport) -> the worst cell across the scenarios that ran the check."""
    worst: dict[tuple[str, str, str], str] = {}
    for r in records:
        for name, v in r["results"].items():
            key = (name, r["client"], r["transport"])
            c = cell(v)
            if RANK[c] > RANK[worst.get(key, "")]:
                worst[key] = c
    return worst


BASELINE = CONF / "baseline.toml"


def baseline_file(server: str) -> Path:
    return BASELINE if server == "go" else CONF / f"baseline-{server}-server.toml"


def write_baseline(records: list[dict], path: Path) -> None:
    """Record each upstream client's current failures and warnings, per transport. Checks marked
    flaky by hand stay so."""
    worst = worst_cells(records)
    old = tomllib.loads(path.read_text()) if path.exists() else {}
    lines = ["# Known results of the upstream clients, written by `run.py --write-baseline`.",
             "# The runner compares every run against this file: a new failure is a regression, and a",
             "# listed failure that now passes means the baseline is out of date. A check listed as",
             "# flaky (by hand) races inside the client, so either result is expected.", ""]
    for client, transport in sorted({(c, t) for _, c, t in worst}):
        flaky = old.get(f"{client}/{transport}", {}).get("flaky", [])
        cells = {n: v for (n, c, t), v in worst.items() if (c, t) == (client, transport) and n not in flaky}
        fails = sorted(n for n, v in cells.items() if v == "FAIL")
        warns = sorted(n for n, v in cells.items() if v == "warn")
        lines += [f'["{client}/{transport}"]', f"fail = {json.dumps(fails)}", f"warn = {json.dumps(warns)}"]
        if flaky:
            lines.append(f"flaky = {json.dumps(sorted(flaky))}")
        lines.append("")
    path.write_text("\n".join(lines))


def compare_baseline(records: list[dict], path: Path) -> list[str]:
    """Differences between this run and the baseline, for the client/transport pairs that ran."""
    if not path.exists():
        return []
    base = tomllib.loads(path.read_text())
    worst = worst_cells(records)
    out = []
    for (name, client, transport), got in sorted(worst.items()):
        entry = base.get(f"{client}/{transport}", {"fail": [], "warn": []})
        want = "FAIL" if name in entry["fail"] else "warn" if name in entry["warn"] else "ok"
        if got == want or got in ("", "-") or name in entry.get("flaky", []):
            continue
        if RANK[got] > RANK[want]:
            out.append(f"regression  {client}/{transport} {name}: {want} -> {got}")
        else:
            out.append(f"improvement {client}/{transport} {name}: {want} -> {got} (update the baseline)")
    return out


def print_matrix(records: list[dict]) -> None:
    """One row per check, one column per client and transport: the worst result across scenarios."""
    columns = sorted({(r["client"], r["transport"]) for r in records})
    worst = worst_cells(records)
    names = sorted({k[0] for k in worst})
    print(f"\n{'check':38} {'level':6} " + " ".join(f"{c}/{t}"[:10].ljust(10) for c, t in columns))
    for name in names:
        print(f"{name:38} {CHECKS[name].level:6} "
              + " ".join(worst.get((name, c, t), "").ljust(10) for c, t in columns))


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--client", default="", help="comma-separated substrings of client names (=name: exactly)")
    ap.add_argument("--scenario", default="", help="comma-separated substrings of scenario names")
    ap.add_argument("--transport", default="", help="http or ws")
    ap.add_argument("--server", default="go", choices=sorted(SERVERS), help="the server the clients talk to")
    ap.add_argument("--list", action="store_true")
    ap.add_argument("--strict", action="store_true",
                    help="exit 1 on a MUST failure for a client we own, or any difference from the baseline")
    ap.add_argument("--write-baseline", action="store_true", help="record this run's results as the baseline")
    ap.add_argument("--crash-seed", type=int, default=None,
                    help="replay a crash scenario with this seed (its result names the seed it ran with)")
    ap.add_argument("--crash-seeds", type=int, default=1,
                    help="run each crash scenario on this many seeds, from --crash-seed or the scenario's own")
    ap.add_argument("--lazyfs", type=Path, default=None, metavar="DIR",
                    help="state files on LazyFS mounts under DIR, and real crashes (conformance/lazyfs/)")
    ap.add_argument("--clients-override", type=Path, default=None, metavar="FILE",
                    help="merge FILE's client tables over clients.toml's")
    ap.add_argument("--differential", action="store_true",
                    help="after the runs, compare the clients' taps of each scenario and transport "
                         "(conformance/differential/shells.py)")
    ap.add_argument("--jobs", type=int, default=int(os.environ.get("CONFORMANCE_JOBS", "1")),
                    help="runs at once (default 1, or $CONFORMANCE_JOBS). Each run has its own server, port and "
                         "output directory, and spends most of its time waiting on the scenario's clock")
    args = ap.parse_args()
    global CRASH_SEED, LAZYFS_DIR
    CRASH_SEED = args.crash_seed

    clients = load_clients(args.clients_override)
    if args.lazyfs is not None:
        why = lazyfs.available()
        if why is not None:
            print(f"LazyFS can't run here: {why}", file=sys.stderr)
            return 3
        LAZYFS_DIR = args.lazyfs.resolve()
    scenarios = [json.loads(p.read_text()) | {"name": p.stem} for p in sorted((CONF / "scenarios").glob("*.json"))]
    for s in scenarios:
        unknown = [c for c in s["checks"] if c not in CHECKS]
        if unknown:
            sys.exit(f"scenario {s['name']}: unknown checks {unknown}")
        # TLS and the proxies are the Go server's (conformance/server/tls.go, proxy.go).
        if {"tls", "proxy"} & set(s.get("server", {})) and s.get("servers", ["go"]) != ["go"]:
            sys.exit(f"scenario {s['name']}: tls and proxy run on the Go server only")
    match = lambda name, filt: not filt or any(  # noqa: E731
        f[1:] == name if f.startswith("=") else f in name for f in filt.split(","))
    if args.list:
        print("clients:  ", ", ".join(f"{k} ({'/'.join(v['transports'])})" for k, v in clients.items()))
        print("scenarios:", ", ".join(s["name"] for s in scenarios))
        for c in CHECKS.values():
            print(f"  {c.name:38} {c.level:6} {c.subject:6} {','.join(c.transports):8} {' '.join(c.covers)}")
        return 0

    wanted = [c for c in clients if match(c, args.client)]
    if LAZYFS_DIR is not None and need_docker(clients, wanted):
        sys.exit("--lazyfs: the drivers must be built and run here, where the mount is, without Docker "
                 "(--clients-override conformance/lazyfs/native.toml): " + ", ".join(need_docker(clients, wanted)))
    build(clients, wanted, args.server)
    planned = []
    for s in scenarios:
        if not match(s["name"], args.scenario) or args.server not in s.get("servers", ["go"]):
            continue
        for client in wanted:
            if not set(s.get("needs", [])) <= set(clients[client].get("features", [])):
                continue
            for transport in s["transports"]:
                if transport not in clients[client]["transports"] or (args.transport and transport != args.transport):
                    continue
                if (s.get("driver", {}).get("crash") and (args.crash_seeds > 1 or LAZYFS_DIR is not None)):
                    first = args.crash_seed if args.crash_seed is not None else int(s.get("seed", 1))
                    planned += [(client, s, transport, first + i) for i in range(args.crash_seeds)]
                else:
                    planned.append((client, s, transport, None))
    records = []
    # Results come back, and print, in the planned order whatever --jobs is.
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        for (client, s, transport, seed), rec in zip(planned, pool.map(
                lambda p: run_safely(p[0], clients[p[0]], p[1], p[2], args.server, p[3]), planned)):
            records.append(rec)
            bad = {k: v for k, v in rec["results"].items() if v["status"] in ("fail", "error")}
            worst = max((RANK[cell(v)] for v in bad.values()), default=0)
            mark = {0: "ok  ", 2: "warn"}.get(worst, "FAIL")
            seeded = f" seed {seed}" if seed is not None else ""
            lag = rec.get("exit_after_stopped_s")
            slow = f" (exited {lag:.1f}s after stopped)" if lag is not None and lag > SLOW_EXIT_S else ""
            print(f"{mark} {client:8} {s['name']:28} {transport:4} {rec['seconds']:5.1f}s{seeded}{slow}"
                  + "".join(f"\n       {cell(v)} {k}: {v['detail']}" for k, v in bad.items()), flush=True)

    results_name = "results.json" if args.server == "go" else f"results-{args.server}-server.json"
    (OUT / results_name).write_text(json.dumps(records, indent=1))
    print_matrix(records)
    baseline = baseline_file(args.server)
    # A check of the server is ours to pass whichever client it ran with; checks of the client
    # follow the baseline for upstream clients.
    upstream = [dict(r, results={k: v for k, v in r["results"].items() if v["subject"] == "client"})
                for r in records if clients[r["client"]].get("upstream")]
    if args.write_baseline:
        write_baseline(upstream, baseline)
        print(f"\nwrote {baseline.relative_to(ROOT)}")
    diffs = compare_baseline(upstream, baseline)
    for d in diffs:
        print(d)
    base = tomllib.loads(baseline.read_text()) if baseline.exists() else {}
    differ = []
    if args.differential and args.server == "go":
        sys.path.insert(0, str(CONF / "differential"))
        import shells  # noqa: E402
        ours = [c for c in wanted if not clients[c].get("upstream")]
        lines, found = shells.compare(OUT, shells.REFERENCE, ours, args.scenario, shells.load_rules(), TAP_CHECKER)
        print("\ndifferential (conformance/differential/shells.py):")
        print("\n".join(lines))
        differ = [d for d in found if not d["allowed"]]
        print(f"{len(found)} differences, {len(found) - len(differ)} allowed, {len(differ)} not")
    return 1 if args.strict and (must_failures(records, clients, base) or diffs or differ) else 0


def must_failures(records: list[dict], clients: dict, base: dict) -> list[str]:
    """The MUST failures that are ours: any check of a client we own, and any check of the server.

    A race inside an upstream client, marked flaky in the baseline by hand, can fail a check of the
    server too (python/http: a message after the goodbye reuses a sequence number, and the goodbye
    after it is out of sequence, so the server doesn't honour it). That isn't ours to fix."""
    out = []
    for r in records:
        upstream = clients[r["client"]].get("upstream")
        flaky = base.get(f"{r['client']}/{r['transport']}", {}).get("flaky", []) if upstream else []
        for name, v in r["results"].items():
            if (v["status"] in ("fail", "error") and v["level"] == "MUST"
                    and (v["subject"] == "server" or not upstream) and name not in flaky):
                out.append(f"{r['client']}/{r['transport']} {r['scenario']} {name}")
    return out


if __name__ == "__main__":
    sys.exit(main())
