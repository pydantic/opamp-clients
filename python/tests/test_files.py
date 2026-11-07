# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The file operations of saves: journaled in the core tap (kind 5) and
held to what the checker (opamp-tap's file records) asks of them; and, through the seam that
stands in for the file system (``_files.Files``), faults injected into them: an error, a write
cut short then a full disk, a slow operation, a rename that lands with a directory sync that
fails. Whatever
the fault: the client keeps heartbeating, every save is answered once, stop is bounded, the state
file is whole afterwards (a version saved, or the one before) and a temporary file left over never
makes a later save fail.

The fault runs are seeded: ``OPAMP_FILE_FAULT_SEEDS`` (8) of them, from
``OPAMP_FILE_FAULT_BASE`` (0); ``OPAMP_FILE_FAULT_SEED=S`` replays one. With
``target/release/opamp-tap`` there (``scripts/fetch_core.py fetch`` puts it there), every tap is checked
by it too."""

from __future__ import annotations

import asyncio
import errno
import hashlib
import json
import os
import random
import struct
import subprocess
import threading
import time
import warnings
from collections import Counter
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

import pytest
from _servers import MODES
from aiohttp import web

from logfire_fleet_client import Client, Config, Status, opamp_pb2 as pb
from logfire_fleet_client import _executor
from logfire_fleet_client._executor import _save, read_state
from logfire_fleet_client._files import REAL, Files, Journal
from logfire_fleet_client._proto.opamp.core.v1 import core_pb2 as corepb
from logfire_fleet_client._transport import Proxy

ROOT = Path(__file__).resolve().parents[2]  # the repository
OPAMP_TAP = ROOT / "target" / "release" / "opamp-tap"
FILE = 5


# The tap.

@dataclass
class Tap:
    """A tap's records, decoded as far as these tests need: the file operations, the saves the
    core asked for, and their answers, in the order they were written."""

    events: list[tuple] = field(default_factory=list)  # ("file", op) | ("store", id, data) | (input kind, id)

    @property
    def ops(self) -> list[dict]:
        return [e[1] for e in self.events if e[0] == "file"]

    @property
    def stores(self) -> list[tuple[int, bytes]]:
        return [(e[1], e[2]) for e in self.events if e[0] == "store"]

    def answers(self) -> list[tuple[str, int]]:
        return [(e[0], e[1]) for e in self.events if e[0] in ("stored", "store_failed")]

    def inputs(self, kind: str) -> list[int]:
        return [e[1] for e in self.events if e[0] == kind]

    def index(self, pred: Callable[[tuple], bool]) -> int:
        return next(i for i, e in enumerate(self.events) if pred(e))


def read_tap(path: str) -> Tap:
    """Decode the tap at `path` (a record cut short at its end, still being written, is left out)."""
    data = Path(path).read_bytes() if os.path.exists(path) else b""
    tap = Tap()
    at = 0
    while at + 13 <= len(data):
        kind, n, _ = struct.unpack_from("<BIQ", data, at)
        if at + 13 + n > len(data):
            break
        body = data[at + 13:at + 13 + n]
        at += 13 + n
        if kind == FILE:
            tap.events.append(("file", json.loads(body)))
        elif kind == 3:
            i = corepb.Step.FromString(body).input
            k = i.WhichOneof("kind")
            if k is not None and hasattr(getattr(i, k), "id"):
                tap.events.append((k, getattr(i, k).id))
        elif kind == 4:
            for e in corepb.Reply.FromString(body).output.effects:
                if e.WhichOneof("kind") == "store":
                    tap.events.append(("store", e.store.id, bytes(e.store.data)))
    return tap


def tap_path(tmp_path: Path, name: str = "core.tap") -> Path:
    """The file a core tapped to OPAMP_CORE_TAP=tmp_path/name writes: the name itself for a
    process's first core, name.<pid>.<n> for later ones (one core per name here)."""
    found = sorted(tmp_path.glob(name + "*"), key=lambda p: p.stat().st_mtime)
    assert found, f"no tap at {tmp_path / name}"
    return found[-1]


def opamp_tap(path: Path) -> None:
    """Check the tap with opamp-tap, if it is there: exit 0, or the test fails."""
    if not OPAMP_TAP.is_file():
        warnings.warn(f"{OPAMP_TAP} is missing (scripts/fetch_core.py fetch puts it there): "
                      f"the tap at {path} goes unchecked", stacklevel=2)
        return
    r = subprocess.run([str(OPAMP_TAP), str(path)], capture_output=True, text=True, timeout=120)
    assert r.returncode == 0, f"opamp-tap {path}: exit {r.returncode}\n{r.stdout}\n{r.stderr}"


async def until(what: str, done: Callable[[], bool], seconds: float = 20) -> None:
    deadline = time.monotonic() + seconds
    while not done():
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out waiting for {what}")
        await asyncio.sleep(0.01)


# A server: a new remote config in each of its first `configs` answers (each one a save), and a
# package on offer if it has one; it holds the first download of the file until released, if
# asked to.

class Server:
    def __init__(self, configs: int = 0) -> None:
        self.configs = configs
        self.arrivals: list[float] = []  # time.monotonic() of each message
        self.port = 0
        self._runner: web.AppRunner | None = None

    @property
    def endpoint(self) -> str:
        return f"http://127.0.0.1:{self.port}/v1/opamp"

    async def _opamp(self, req: web.Request) -> web.Response:
        msg = pb.AgentToServer.FromString(await req.read())
        self.arrivals.append(time.monotonic())
        reply = pb.ServerToAgent(instance_uid=msg.instance_uid)
        if not msg.HasField("agent_disconnect"):
            n = min(len(self.arrivals), self.configs)
            if n:
                body = f"config {n}".encode()
                file = pb.AgentConfigObject(body=body, content_type="text/plain")
                reply.remote_config.CopyFrom(pb.AgentRemoteConfig(
                    config=pb.AgentConfigMap(config_map={"": file}), config_hash=hashlib.sha256(body).digest()))
        return web.Response(body=reply.SerializeToString(), content_type="application/x-protobuf")

    async def start(self) -> Server:
        app = web.Application()
        app.router.add_post("/v1/opamp", self._opamp)
        self._runner = web.AppRunner(app)
        await self._runner.setup()
        site = web.TCPSite(self._runner, "127.0.0.1", 0)
        await site.start()
        self.port = site._server.sockets[0].getsockname()[1]  # type: ignore[union-attr]
        return self

    async def close(self) -> None:
        if self._runner is not None:
            await self._runner.cleanup()


CAPS = pb.AgentCapabilities_AcceptsRemoteConfig


def config(server: Server, state: str, mode: str, **kw) -> Config:
    return Config(endpoint=server.endpoint, state_file=state, mode=mode, capabilities=CAPS, proxy=Proxy.DIRECT,
                  heartbeat_interval_ms=kw.pop("heartbeat_interval_ms", 100), **kw)


# The save's recipe, journaled.

def test_a_save_is_journaled_step_by_step_and_a_leftover_temporary_file_removed_first(tmp_path) -> None:
    ops: list[dict] = []
    state = str(tmp_path / "state.bin")
    files = Journal(REAL, lambda b: ops.append(json.loads(b)), state)
    _save(files, state, b"one")
    Path(state + ".tmp").write_bytes(b"left by a crash")
    _save(files, state, b"two")
    assert Path(state).read_bytes() == b"two"
    save = [{"op": "create", "path": "state.bin.tmp"},
            {"op": "write", "path": "state.bin.tmp", "data": None},
            {"op": "sync", "path": "state.bin.tmp"},
            {"op": "rename", "from": "state.bin.tmp", "to": "state.bin"},
            # Windows can't sync a directory (Files.sync_dir), so none is journaled there.
            *([] if os.name == "nt" else [{"op": "syncdir", "path": "."}])]

    def with_data(data: bytes) -> list[dict]:
        return [dict(o, data=data.hex()) if o["op"] == "write" else o for o in save]

    assert ops == [{"op": "state", "path": "state.bin"}, *with_data(b"one"),
                   {"op": "remove", "path": "state.bin.tmp"}, *with_data(b"two")]


# A file system that fails as told.

class Faulty(Files):
    """The real file system, but for one call: the `nth` (from 0) of operation `op` on a path
    `mine` says is in play, which fails as `kind` says:

    - ``eio``: it fails (EIO) and does nothing (a close closes, then fails);
    - ``half``: a write writes half of what it was given and says so, and the next write to that
      file fails (ENOSPC), as write(2) does on a disk that fills up (other operations: eio);
    - ``slow``: it takes `slow_s` seconds, then is done;
    - ``lands``: it is done, then the next directory sync (it is done too) fails (EIO): a rename
      that lands while its save reports failure.
    """

    def __init__(self, op: str, nth: int, kind: str, mine: Callable[[str], bool], slow_s: float = 1.0) -> None:
        self.op, self.nth, self.kind, self.mine, self.slow_s = op, nth, kind, mine, slow_s
        self.calls: Counter[str] = Counter()
        self.fired: str | None = None
        self.slow: list[tuple[float, float]] = []  # when slow calls began and ended
        self._lock = threading.Lock()
        self._paths: dict[int, str] = {}
        self._full: set[int] = set()  # files whose next write fails (ENOSPC)
        self._fail_syncdir = False

    def __str__(self) -> str:
        return f"{self.kind} on {self.op} #{self.nth} ({'fired: ' + self.fired if self.fired else 'never reached'})"

    def _hit(self, op: str, path: str | None) -> bool:
        if path is None or not self.mine(path):
            return False
        with self._lock:
            n = self.calls[op]
            self.calls[op] += 1
            if op == self.op and n == self.nth and self.fired is None:
                self.fired = f"{op} {os.path.basename(path)}"
                return True
        return False

    def _do(self, op: str, path: str | None, fn: Callable[[], object]):
        if not self._hit(op, path):
            return fn()
        if self.kind == "slow":
            t0 = time.monotonic()
            time.sleep(self.slow_s)
            r = fn()
            self.slow.append((t0, time.monotonic()))
            return r
        if self.kind == "lands":
            r = fn()
            self._fail_syncdir = True
            return r
        raise OSError(errno.EIO, f"injected: {op} failed")

    def create(self, path: str) -> int:
        fd = self._do("create", path, lambda: REAL.create(path))
        assert isinstance(fd, int)
        self._paths[fd] = path
        return fd

    def write(self, fd: int, bufs) -> int:
        if fd in self._full:
            raise OSError(errno.ENOSPC, "injected: no space left on device")
        path = self._paths.get(fd)
        if self.kind == "half" and self._hit("write", path):
            data = b"".join(bytes(b) for b in bufs)
            n = REAL.write(fd, [data[:max(1, len(data) // 2)]])
            self._full.add(fd)
            return n
        return self._do("write", path, lambda: REAL.write(fd, bufs))  # type: ignore[return-value]

    def sync(self, fd: int) -> None:
        self._do("sync", self._paths.get(fd), lambda: REAL.sync(fd))

    def close(self, fd: int) -> None:
        path = self._paths.pop(fd, None)
        self._full.discard(fd)
        if self.kind in ("eio", "half") and self._hit("close", path):
            REAL.close(fd)
            raise OSError(errno.EIO, "injected: close failed")
        self._do("close", path, lambda: REAL.close(fd))

    def rename(self, src: str, dst: str) -> None:
        self._do("rename", src, lambda: REAL.rename(src, dst))

    def sync_dir(self, path: str) -> bool:
        if self._fail_syncdir:
            self._fail_syncdir = False
            REAL.sync_dir(path)
            raise OSError(errno.EIO, "injected: the directory sync failed after the rename")
        return self._do("syncdir", path, lambda: REAL.sync_dir(path))  # type: ignore[return-value]

    def remove(self, path: str) -> None:
        self._do("remove", path, lambda: REAL.remove(path))


def test_a_write_cut_short_is_followed_by_the_rest_or_the_save_fails(tmp_path) -> None:
    """write(2) may take part of what it is given: the rest follows, or (the disk is full) the
    save fails. It never renames half a state over the whole one."""
    state = str(tmp_path / "state")
    save_state = _executor.save_state
    save_state(state, b"before")
    with pytest.raises(OSError, match="no space left"):
        _save(Faulty("write", 0, "half", lambda p: True), state, b"a whole new state")
    assert Path(state).read_bytes() == b"before"
    assert not os.path.exists(state + ".tmp")


def test_a_write_cut_short_once_is_written_in_full(tmp_path) -> None:
    class Short(Files):
        """Each write takes at most 3 bytes."""

        def write(self, fd, bufs) -> int:
            return REAL.write(fd, [bytes(bufs[0])[:3]])

    state = str(tmp_path / "state")
    _save(Short(), state, b"a whole new state")
    assert Path(state).read_bytes() == b"a whole new state"


# The journal of a client's saves, in its tap.

@pytest.mark.parametrize("mode", MODES)
def test_a_clients_saves_are_journaled_in_its_tap(mode: str, tmp_path, monkeypatch) -> None:
    monkeypatch.setenv("OPAMP_CORE_TAP", str(tmp_path / "core.tap"))
    state = str(tmp_path / "state.bin")

    async def run() -> None:
        server = await Server(configs=3).start()
        try:
            client = Client.start(config(server, state, mode), Status())
            tap = tap_path(tmp_path)
            # The core takes in what comes while a save is under way at once, so the three configs
            # (coming one a message, fast) may make as few as two saves.
            await until("two saves", lambda: len([a for a in read_tap(str(tap)).answers() if a[0] == "stored"]) >= 2)
            await asyncio.wait_for(client.stop(), 10)
        finally:
            await server.close()

        t = read_tap(str(tap))
        assert t.ops[0] == {"op": "state", "path": "state.bin"}
        # Each save: a new temporary file, its data written and synced, renamed over the state
        # file, the directory synced; stored only after that.
        for id_, data in t.stores:
            if ("stored", id_) not in t.answers():
                continue
            at = t.index(lambda e, id_=id_, data=data: e == ("store", id_, data))
            done = t.index(lambda e, id_=id_: e == ("stored", id_))
            save = [e[1] for e in t.events[at:done] if e[0] == "file"]
            assert save == [{"op": "create", "path": "state.bin.tmp"},
                            {"op": "write", "path": "state.bin.tmp", "data": data.hex()},
                            {"op": "sync", "path": "state.bin.tmp"},
                            {"op": "rename", "from": "state.bin.tmp", "to": "state.bin"},
                            # Windows can't sync a directory (Files.sync_dir): none is journaled there.
                            *([] if os.name == "nt" else [{"op": "syncdir", "path": "."}])], f"save {id_}"
        assert Path(state).read_bytes() == [d for i, d in t.stores if ("stored", i) in t.answers()][-1]
        opamp_tap(tap)

    asyncio.run(run())


# Faults in saves.

def _seeds(default: int) -> list[int]:
    if one := os.environ.get("OPAMP_FILE_FAULT_SEED"):
        return [int(one)]
    base = int(os.environ.get("OPAMP_FILE_FAULT_BASE", "0"))
    return list(range(base, base + int(os.environ.get("OPAMP_FILE_FAULT_SEEDS", str(default)))))


SAVE_KINDS = ["eio", "half", "slow", "lands"]
SAVE_OPS = ["remove", "create", "write", "sync", "close", "rename", "syncdir"]
STOP_S = 2.0  # the clients' stop_timeout_ms
SLOW_S = (0.5, 2.0)


class Said:
    """A description, said as it is when a failure says it."""

    def __init__(self, say: Callable[[], str]) -> None:
        self._say = say

    def __str__(self) -> str:
        return self._say()

    __format__ = lambda self, _spec: str(self)  # noqa: E731


def _replay(seed: int) -> str:
    return f"seed {seed} (replay: OPAMP_FILE_FAULT_SEED={seed} pytest {__file__})"


@pytest.mark.parametrize("seed", _seeds(8))
def test_a_fault_in_a_save_is_survived_answered_once_and_leaves_the_state_whole(seed: int, tmp_path,
                                                                                 monkeypatch) -> None:
    rng = random.Random(seed)
    kind = SAVE_KINDS[seed % len(SAVE_KINDS)]
    mode = MODES[(seed // len(SAVE_KINDS)) % len(MODES)]
    op = "rename" if kind == "lands" else "write" if kind == "half" else rng.choice(SAVE_OPS)
    leftover = rng.random() < 0.5 or op == "remove"
    state = str(tmp_path / "state.bin")
    tmp = state + ".tmp"
    nth = 0 if op == "remove" and leftover else rng.randrange(3)
    if op == "remove" and not leftover:
        nth = 0  # the remove of a save that failed before
    faulty = Faulty(op, nth, kind, lambda p: p in (state, tmp, str(tmp_path)), rng.uniform(*SLOW_S))
    what = Said(lambda: f"{_replay(seed)}: {mode} mode, {faulty}, a leftover temporary file: {leftover}")
    if leftover:
        Path(tmp).write_bytes(b"left by a crash")
    monkeypatch.setenv("OPAMP_CORE_TAP", str(tmp_path / "core.tap"))

    async def run() -> None:
        server = await Server(configs=6).start()
        try:
            client = Client.start(config(server, state, mode, stop_timeout_ms=int(STOP_S * 1000)), Status(),
                                  _files=faulty)
            tap = tap_path(tmp_path)

            def settled() -> bool:
                t = read_tap(str(tap))
                answers = t.answers()
                failed = [n for n, (k, _) in enumerate(answers) if k == "store_failed"]
                if faulty.fired is None or (failed and not any(k == "stored" for k, _ in answers[failed[-1] + 1:])):
                    # More saves, until the fault is reached, and one after it is stored.
                    server.configs = len(server.arrivals) + 1
                    return False
                # Every save asked for answered, the configs all in, and none for a while.
                return (len(server.arrivals) > server.configs + 2 and len(answers) == len(t.stores) >= 2
                        and time.monotonic() - server.arrivals[server.configs - 1] > 0.5)

            await until(f"the saves to settle ({what})", settled, 30)
            t0 = time.monotonic()
            await client.stop()
            took = time.monotonic() - t0
            assert took < STOP_S + SLOW_S[1] + 3, f"{what}: stop took {took:.1f} s"
        finally:
            await server.close()

        assert faulty.fired is not None, f"{what}: the fault was never reached"
        t = read_tap(str(tap))
        stores = t.stores
        answers = t.answers()
        # Every save gets exactly one answer.
        assert sorted(i for _, i in answers) == sorted(i for i, _ in stores), f"{what}: {stores} {answers}"
        # The client kept heartbeating while a save was slow.
        for begun, ended in faulty.slow:
            during = [a for a in server.arrivals if begun < a < ended]
            assert during, f"{what}: no message in the {ended - begun:.1f} s a save took"
        # A temporary file left over (by a crash, or a failed save) never fails a later save:
        # every save after the fault is stored.
        failed = [n for n, (k, _) in enumerate(answers) if k == "store_failed"]
        if kind == "slow":
            assert not failed, f"{what}: {answers}"
        else:
            assert len(failed) <= 1 and all(k == "stored" for k, _ in answers[failed[0] + 1 if failed else 0:]), (
                f"{what}: {answers}")
            assert answers[-1][0] == "stored", f"{what}: {answers}"
        # The state file is whole: a version saved, no older than the last stored.
        content = await asyncio.to_thread(read_state, state)
        datas = [d for _, d in stores]
        assert content in datas, f"{what}: the state file holds {len(content)} bytes that are no save"
        last_stored = max(n for n, (i, _) in enumerate(stores) if ("stored", i) in answers)
        assert max(n for n, d in enumerate(datas) if d == content) >= last_stored, f"{what}: an older save"
        assert not os.path.exists(tmp), f"{what}: the temporary file is left"
        opamp_tap(tap)

        # The next client starts from it.
        server2 = await Server().start()
        try:
            again = Client.start(config(server2, state, mode), Status())
            await again.stop()
        finally:
            await server2.close()

    asyncio.run(run())


# On macOS fsync(2) hands the data to the drive, which may keep it in its cache: only
# fcntl(F_FULLFSYNC) asks the drive to write it out (fsync(2) on macOS says so, as do Python's
# os.fsync docs). RULE-SHELL-09 says a save answered stored is on disk. Rust's std
# (File::sync_all), Go (File.Sync), libuv (Node's fsyncSync) and OpenJDK (FileChannel.force) use
# F_FULLFSYNC there, and so does this shell's `Files`, for the temporary file and for the
# directory. Played on any host by making it look like macOS and noting the calls; F_FULLFSYNC is
# 51 in <sys/fcntl.h>.
F_FULLFSYNC = 51


@pytest.mark.skipif(os.name == "nt", reason="no fcntl on Windows")
def test_on_macos_a_save_and_its_directory_are_flushed_with_f_fullfsync(tmp_path, monkeypatch) -> None:
    import fcntl
    import sys

    calls: list[str] = []
    real_fcntl = fcntl.fcntl

    def fake_fcntl(fd: int, cmd: int, arg: int = 0) -> int:
        if cmd == F_FULLFSYNC:
            calls.append("F_FULLFSYNC")
            return 0
        return real_fcntl(fd, cmd, arg)

    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setattr(fcntl, "F_FULLFSYNC", F_FULLFSYNC, raising=False)
    monkeypatch.setattr(fcntl, "fcntl", fake_fcntl)
    monkeypatch.setattr(os, "fsync", lambda fd: calls.append("fsync"))

    fd = REAL.create(str(tmp_path / "f"))
    try:
        REAL.sync(fd)
    finally:
        REAL.close(fd)
    assert REAL.sync_dir(str(tmp_path))
    assert calls == ["F_FULLFSYNC", "F_FULLFSYNC"]


@pytest.mark.skipif(os.name == "nt", reason="no fcntl on Windows")
def test_on_macos_a_file_system_without_f_fullfsync_is_flushed_with_fsync(tmp_path, monkeypatch) -> None:
    # Some file systems (network mounts) refuse F_FULLFSYNC: then plain fsync, as libuv and OpenJDK
    # do, rather than a save that fails.
    import errno
    import fcntl
    import sys

    calls: list[str] = []
    real_fcntl = fcntl.fcntl

    def fake_fcntl(fd: int, cmd: int, arg: int = 0) -> int:
        if cmd == F_FULLFSYNC:
            calls.append("F_FULLFSYNC")
            raise OSError(errno.ENOTSUP, "Operation not supported")
        return real_fcntl(fd, cmd, arg)

    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setattr(fcntl, "F_FULLFSYNC", F_FULLFSYNC, raising=False)
    monkeypatch.setattr(fcntl, "fcntl", fake_fcntl)
    monkeypatch.setattr(os, "fsync", lambda fd: calls.append("fsync"))

    fd = REAL.create(str(tmp_path / "f"))
    try:
        REAL.sync(fd)
    finally:
        REAL.close(fd)
    assert REAL.sync_dir(str(tmp_path))
    assert calls == ["F_FULLFSYNC", "fsync", "F_FULLFSYNC", "fsync"]


def test_elsewhere_a_save_and_its_directory_are_flushed_with_fsync(tmp_path, monkeypatch) -> None:
    import sys

    calls: list[str] = []
    monkeypatch.setattr(sys, "platform", "linux")
    monkeypatch.setattr(os, "fsync", lambda fd: calls.append("fsync"))

    fd = REAL.create(str(tmp_path / "f"))
    try:
        REAL.sync(fd)
    finally:
        REAL.close(fd)
    REAL.sync_dir(str(tmp_path))
    assert calls == (["fsync"] if os.name == "nt" else ["fsync", "fsync"])


# Windows: a directory can't be flushed, so a save's rename is written through (MoveFileEx with
# MOVEFILE_WRITE_THROUGH: it returns once the rename is on the disk). Played on any host with a
# stand-in for MoveFileExW.
def test_on_windows_a_saves_rename_is_written_through(tmp_path) -> None:
    from logfire_fleet_client import _windows

    calls: list[tuple[str, str, int]] = []

    def move(src: str, dst: str, flags: int) -> bool:
        calls.append((src, dst, flags))
        return True

    _windows.replace("a.tmp", "a", move)
    assert calls == [("a.tmp", "a", _windows.MOVEFILE_REPLACE_EXISTING | _windows.MOVEFILE_WRITE_THROUGH)]
    with pytest.raises(OSError):
        _windows.replace("a.tmp", "a", lambda *_: False)


@pytest.mark.skipif(os.name != "nt", reason="Windows access-control lists")
def test_on_windows_a_save_and_the_tap_allow_the_user_alone(tmp_path, monkeypatch) -> None:
    from logfire_fleet_client import _core, _windows

    user = _windows.user_sid()
    assert _windows.user_only() == f"D:P(A;;FA;;;{user})"

    # A save: its temporary file the user's only from the start, and so the state file it becomes.
    state = str(tmp_path / "state.bin")
    _executor._save(REAL, state, b"one")
    _executor._save(REAL, state, b"two")
    assert Path(state).read_bytes() == b"two"
    assert _windows.dacl(state) == _windows.canonical(_windows.user_only())
    fd = REAL.create(state + ".tmp")
    REAL.close(fd)
    with pytest.raises(FileExistsError):
        REAL.create(state + ".tmp")  # made new: never one already there

    # The core tap: appended to, the user's only.
    tap = tmp_path / "core.tap"
    monkeypatch.setenv("OPAMP_CORE_TAP", str(tap))
    for _ in range(2):
        monkeypatch.setattr(_core, "_taps", 0)  # the first core's tap: the path itself
        fd = _core._open_tap()
        assert fd is not None
        os.write(fd, b"ab")
        os.close(fd)
    assert tap.read_bytes() == b"abab"
    assert _windows.dacl(str(tap)) == _windows.canonical(_windows.user_only())
