# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The shell's schedules: a Hypothesis state machine driving one client, in Python mode
(aiohttp, faked: tests/_sched_world.py) and in Rust mode (the core's own HTTP and WebSocket, over
the in-memory network), on a seeded event loop (tests/_sched_loop.py) whose task order comes from
a seed and whose clock is virtual. The real core runs, tapped; the server is the test's.

Each step is one of:

- the server settles something the client asked of it, at a turn the step picks: a connection
  attempt (accepted or refused), a request (answered, in pieces, with an offer: a remote config,
  a custom message, a package, a heartbeat interval; an error status; or a reset), a WebSocket
  upgrade (accepted, refused with a status, reset), the client's Close frame (answered or reset),
  a package download (the file, a bad file, a 404, cut off mid-body, stalled mid-body until a
  later step sends the rest or resets, a reset); a quarter of what the client gives up on finishes
  anyway (a connection, response or file that comes after its cancel);
- the server pushes on an open WebSocket, closes it, or drops it;
- the application calls the client: from the loop, from another thread (it must be
  marshalled), or from inside its callback; or its callback raises on its next call;
- the state file's writer: the next save fails, or takes a while (virtual time);
- time passes (timeouts fire: the core cancels);
- stop, from outside or from inside the callback.

After each step, with the loop idle, it checks spec/shell-contract.md:

- RULE-SHELL-01, -02: callbacks one at a time, on the loop's thread, none before start returned,
  none after stopped; stopped last, once; a callback that raised stops the client with that
  error, and stop raises it; custom messages and remote configs in the order offered;
- RULE-SHELL-03: the core is stepped on the loop's thread only, never re-entered, and nothing
  touches the loop from another thread;
- RULE-SHELL-04: every input names an effect the core issued, of its kind, still waited on;
  each answered once; a connection's messages only while open, its end once; Rust mode's streams
  likewise (opened, TLS, data, end, resolution, the proxy's answer);
- RULE-SHELL-06: nothing about an effect after its cancel (or a stream after its close); a
  connection, transport, request or download the core has given up on is released by the time
  the loop is idle, and a cancelled or discarded download's file is gone;
- RULE-SHELL-07: no input after the core said stopped; stop returns within stop_timeout plus a
  margin (virtual time), and then nothing is open, no task of the client's is left, nothing new
  starts, and the state file's writer has nothing in flight unless stop took its whole bound;
- RULE-SHELL-09: a store is answered stored only once its data landed (or a later store's),
  and saves land in the order asked.

Runs: ``HYPOTHESIS_EXAMPLES`` per mode (default 100; nightly 5000); a failure prints the steps (the loop's seed
is the first step's) and ``replay: OPAMP_SCHED_SEED=...``, which replays the run; or
``OPAMP_SCHED_LOOP_SEED`` pins the loop's seed alone. ``OPAMP_SCHED_NO_SHRINK=1`` reports the
first failing run as found, unshrunk (quicker, longer)."""

from __future__ import annotations

import asyncio
import os
import random
import shutil
import tempfile
import threading
from collections import Counter, deque
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field

import hypothesis.core
import pytest
from hypothesis import HealthCheck, Phase, settings, strategies as st
from hypothesis import note as _note
from hypothesis.errors import InvalidArgument
from hypothesis.stateful import RuleBasedStateMachine, initialize, invariant, precondition, rule, run_state_machine_as_test

import opamp_client._client as _client_mod
import opamp_client._core as _core_mod
import opamp_client._executor as _executor
from _sched_loop import SeededLoop
from _sched_world import CAPABILITY, OFFERS, FakeSession, World
from opamp_client import Client, Config, Notification, Status, anyvalue_pb2, opamp_pb2 as pb
from opamp_client._proto.opamp.core.v1 import core_pb2 as corepb

UID = bytes([0x5A] * 16)
CAPABILITIES = (pb.AgentCapabilities_AcceptsRemoteConfig | pb.AgentCapabilities_ReportsRemoteConfig
                | pb.AgentCapabilities_AcceptsPackages | pb.AgentCapabilities_ReportsPackageStatuses)
STOP_S = 5.0  # the default stop_timeout_ms
MARGIN_S = 1.0
UNIX0 = 1_790_000_000_000
METHODS = ["set_health", "set_description", "send_custom_message", "answer_config", "package_installed"]

CURRENT: ScheduleMachine | None = None
#: What the runs did, summed (printed at the end: evidence that the steps reach what they should).
STATS: Counter[str] = Counter()


def note(text: str) -> None:
    """Hypothesis's note, in a Hypothesis test; nothing outside one (a replay)."""
    try:
        _note(text)
    except InvalidArgument:
        pass


class Boom(Exception):
    """What the callback raises when told to."""


def description(n: int) -> pb.AgentDescription:
    return pb.AgentDescription(non_identifying_attributes=[
        anyvalue_pb2.KeyValue(key="n", value=anyvalue_pb2.AnyValue(int_value=n))])


# -- the tap: every step of the core, checked against the effects rules


@dataclass
class Tracked:
    what: str  # request, connection, download, store
    state: str = "live"  # live, open, peer_closed, closing, ended, cancelled
    data: bytes = b""


class Monitor:
    """RULE-SHELL-03, -04, -06, -07 and -09 at the core's boundary."""

    RESULTS = {"http_response": "request", "http_failed": "request", "ws_failed": "connection",
               "stored": "store", "store_failed": "store", "download_response": "download",
               "downloaded": "download", "download_failed": "download"}

    def __init__(self, machine: ScheduleMachine) -> None:
        self.m = machine
        self.problems: list[str] = []
        self.effects: dict[int, Tracked] = {}
        self.streams: dict[int, str] = {}  # opening, tls, open, closed, failed
        self.resolving: dict[int, str] = {}  # live, done, closed
        self.proxies: dict[int, bool] = {}  # answered
        self.discarded: set[int] = set()  # downloads whose file must be gone
        self.depth = 0
        self.stopped = False
        self.steps = 0
        self.trace: list[str] = []

    def bad(self, rule: str, what: str) -> None:
        self.problems.append(f"{rule}: {what} (step {self.steps})")

    def before(self, input_: corepb.Input) -> None:
        self.steps += 1
        kind = input_.WhichOneof("kind")
        self.trace.append(kind)
        loop = self.m.loop
        if threading.get_ident() != loop._thread_id:
            self.bad("RULE-SHELL-03", f"{kind} stepped the core on thread {threading.current_thread().name}")
        if self.depth:
            self.bad("RULE-SHELL-03", f"{kind} re-entered the core during a step")
        if self.stopped:
            self.bad("RULE-SHELL-07", f"{kind} after the core said stopped")
            return
        if self.m.client is not None and self.m.client._finished:
            self.bad("RULE-SHELL-07", f"{kind} after the client ended")
            return
        if kind in self.RESULTS:
            self._answer(getattr(input_, kind).id, self.RESULTS[kind], kind, input_)
        elif kind == "ws_opened":
            self._conn(input_.ws_opened.id, kind, {"live": "open"})
        elif kind == "ws_message":
            self._conn(input_.ws_message.id, kind, {"open": "open"})
        elif kind == "ws_closed":
            self._conn(input_.ws_closed.id, kind, {"open": "peer_closed", "closing": "ended"})
        elif kind == "ws_lost":
            self._conn(input_.ws_lost.id, kind, {"open": "ended", "peer_closed": "ended", "closing": "ended"})
        elif kind == "proxy_answer":
            id_ = input_.proxy_answer.id
            if self.proxies.get(id_) is not False:
                self.bad("RULE-SHELL-04", f"proxy_answer {id_}: not asked, or answered twice")
            self.proxies[id_] = True
        elif kind == "stream_resolved":
            self._resolved(input_.stream_resolved.id, kind)
        elif kind in ("stream_opened", "stream_tls_ready", "stream_data", "stream_eof", "stream_spliced",
                      "stream_failed"):
            self._stream(getattr(input_, kind).id, kind)

    def _cancelled(self, id_: int, kind: str) -> bool:
        t = self.effects.get(id_)
        if t is not None and t.state == "cancelled":
            self.bad("RULE-SHELL-06", f"{kind} {id_} after its cancel")
            return True
        return False

    def _answer(self, id_: int, what: str, kind: str, input_: corepb.Input) -> None:
        if self._cancelled(id_, kind):
            return
        t = self.effects.get(id_)
        if t is None or t.what != what:
            self.bad("RULE-SHELL-04", f"{kind} {id_}: no {what} of that id was issued")
            return
        if t.state != "live":
            self.bad("RULE-SHELL-04", f"{kind} {id_}: answered twice ({t.state})")
            return
        t.state = "ended"
        if kind == "stored":
            self.m.writer.check_stored(self, id_, t.data)

    def _conn(self, id_: int, kind: str, moves: dict[str, str]) -> None:
        if self._cancelled(id_, kind):
            return
        t = self.effects.get(id_)
        if t is None or t.what != "connection":
            self.bad("RULE-SHELL-04", f"{kind} {id_}: no connection of that id")
        elif t.state not in moves:
            self.bad("RULE-SHELL-04", f"{kind} {id_}: the connection is {t.state}")
        else:
            t.state = moves[t.state]

    def _resolved(self, id_: int, kind: str) -> None:
        state = self.resolving.get(id_)
        if state != "live":
            self.bad("RULE-SHELL-06" if state == "closed" else "RULE-SHELL-04", f"{kind} {id_}: resolution {state}")
        self.resolving[id_] = "done"

    def _stream(self, id_: int, kind: str) -> None:
        state = self.streams.get(id_)
        if state == "closed":
            self.bad("RULE-SHELL-06", f"{kind} {id_} after the core closed the stream")
            return
        allowed = {"stream_opened": {"opening"}, "stream_tls_ready": {"tls"},
                   "stream_failed": {"opening", "tls", "open"}}.get(kind, {"open"})
        if state not in allowed:
            self.bad("RULE-SHELL-04", f"{kind} {id_}: the stream is {state}")
            return
        self.streams[id_] = {"stream_opened": "open", "stream_tls_ready": "open", "stream_failed": "failed"}.get(kind, state)

    def after(self, out: corepb.Output) -> None:
        for e in out.effects:
            kind = e.WhichOneof("kind")
            v = getattr(e, kind)
            if kind in ("http_request", "ws_open", "download", "store"):
                what = {"http_request": "request", "ws_open": "connection", "download": "download", "store": "store"}[kind]
                self.effects[v.id] = Tracked(what, data=bytes(v.data) if kind == "store" else b"")
                if kind == "store":
                    self.m.writer.issued.append((v.id, bytes(v.data)))
            elif kind == "download_done":
                self.effects[v.id] = Tracked("download")
            elif kind == "ws_close":
                t = self.effects.get(v.id)
                if t is not None and t.state in ("open", "peer_closed"):
                    t.state = "closing"
            elif kind == "cancel":
                t = self.effects.get(v.id)
                if t is not None:
                    if t.what == "download":
                        self.discarded.add(v.id)
                    t.state = "cancelled"
                elif self.m.mode == "rust":
                    self.discarded.add(v.id)  # a download of Rust mode's, before its download_done
            elif kind == "discard":
                self.discarded.add(v.id)
            elif kind == "proxy_query":
                self.proxies[v.id] = False
            elif kind == "stream_resolve":
                self.resolving[v.id] = "live"
            elif kind == "stream_open":
                self.streams[v.id] = "opening"
            elif kind == "stream_start_tls":
                if self.streams.get(v.id) == "open":
                    self.streams[v.id] = "tls"
            elif kind == "stream_close":
                self.streams[v.id] = "closed"
                if v.id in self.resolving:
                    self.resolving[v.id] = "closed"
        if out.stopped:
            self.stopped = True

    def live(self, what: str, states=("live",)) -> int:
        return sum(1 for t in self.effects.values() if t.what == what and t.state in states)


class TappedCore(_core_mod.Core):
    """The real core, with a fixed seed and wall clock (a run is its seeds), checked at each step."""

    def __init__(self, config: corepb.Config, *, _module: bytes | None = None) -> None:
        config.seed = 0x5EED
        config.entropy = bytes(range(32))
        config.unix_ms = UNIX0
        super().__init__(config, _module=_module)
        assert CURRENT is not None
        self.monitor = CURRENT.monitor

    def step(self, now_ms: int, input_: corepb.Input) -> corepb.Output:
        m = self.monitor
        m.before(input_)
        m.depth += 1
        try:
            out = self._call(self._step, corepb.Step(now_ms=now_ms, input=input_, unix_ms=UNIX0 + now_ms),
                             _core_mod._STEP_IN)
        finally:
            m.depth -= 1
        m.after(out)
        return out


# -- the state file's writer


class Writer:
    """Stands in for save_state, on the client's writer thread: lands, fails, or takes a while
    (virtual time), as the steps say."""

    def __init__(self, loop: SeededLoop) -> None:
        self.loop = loop
        self.plans: deque[tuple[str, float]] = deque()
        self.issued: list[tuple[int, bytes]] = []  # the core's stores, in order
        self.landed: list[bytes] = []
        self.lock = threading.Lock()
        self.outstanding = 0  # submitted to the writer's thread and not over

    def save(self, path: str, data: bytes) -> None:
        how, dt = self.plans.popleft() if self.plans else ("ok", 0.0)
        if how == "slow":
            self.loop.thread_sleep(dt)
        if how == "fail":
            raise OSError("injected: the disk is full")
        self.landed.append(bytes(data))

    def check_stored(self, mon: Monitor, id_: int, data: bytes) -> None:
        # RULE-SHELL-09: stored means saved: that store's data, or a later one's, is what landed last.
        later = [d for i, d in self.issued if i >= id_]
        if not self.landed or self.landed[-1] not in later:
            mon.bad("RULE-SHELL-09", f"stored {id_}, but the file holds {self.landed[-1:]!r}")

    def check_order(self, mon: Monitor) -> None:
        it = iter(d for _, d in self.issued)
        if not all(any(d == x for x in it) for d in self.landed):
            mon.bad("RULE-SHELL-09", "saves landed out of the order asked")


class CountingSaver(ThreadPoolExecutor):
    """The state file's writer thread, counting the work in flight on it."""

    def __init__(self, writer: Writer) -> None:
        super().__init__(max_workers=1, thread_name_prefix="opamp-save")
        self.writer = writer

    def submit(self, fn, /, *args, **kwargs):
        w = self.writer
        ran = []

        def work():
            ran.append(True)
            try:
                return fn(*args, **kwargs)
            finally:
                with w.lock:
                    w.outstanding -= 1

        with w.lock:
            w.outstanding += 1
        f = super().submit(work)

        def cancelled(f) -> None:
            if f.cancelled() and not ran:
                with w.lock:
                    w.outstanding -= 1

        f.add_done_callback(cancelled)
        return f


# -- the application


@dataclass
class App:
    returned: bool = False
    notes: list[Notification] = field(default_factory=list)
    active: int = 0
    problems: list[str] = field(default_factory=list)
    inside: deque = field(default_factory=deque)  # what the next callback does
    throw_next: bool = False
    thrown: Boom | None = None
    thrown_before_stopped: bool = False
    described: int = 0

    @property
    def stopped(self) -> list[Notification]:
        return [n for n in self.notes if n.kind == "stopped"]


class ScheduleMachine(RuleBasedStateMachine):
    mode = "python"

    def __init__(self) -> None:
        super().__init__()
        global CURRENT
        CURRENT = self
        self.loop: SeededLoop | None = None
        self.client: Client | None = None
        self.app = App()
        self.monitor = Monitor(self)
        self.dir = tempfile.mkdtemp(prefix="opamp-sched-")
        self.state = os.path.join(self.dir, "state")
        self.stop_task: asyncio.Task | None = None
        self.stop_at: float | None = None
        self.stop_took: float | None = None
        self.stop_returned = False
        self.ops_at_stop = 0
        self.attempts_at_stop = 0
        self.steps = 0  # steps run since start (stop and a raising callback wait for a few)
        self.stopped_at: float | None = None  # when the application heard stopped

    # -- plumbing

    def _run(self, k: int, action) -> None:
        """Do `action` on the loop at turn `k` from now, then run until idle."""
        loop = self.loop
        assert loop is not None

        def at(n: int) -> None:
            if n:
                loop.call_soon(at, n - 1)
            else:
                action()

        async def go() -> None:
            loop.call_soon(at, k)
            await loop.idle()

        loop.run_until_complete(go())

    def _advance(self, seconds: float) -> None:
        self.loop.run_until_complete(self.loop.idle(seconds))  # type: ignore[union-attr]

    def _call(self, what: str) -> None:
        c = self.client
        assert c is not None
        if what == "set_health":
            c.set_health(pb.ComponentHealth(healthy=True, status=str(self.monitor.steps)))
        elif what == "set_description":
            self.app.described += 1
            c.set_description(description(self.app.described))
        elif what == "send_custom_message":
            c.send_custom_message(pb.CustomMessage(capability=CAPABILITY, type="t", data=b"x"))
        elif what == "answer_config":
            configs = [n for n in self.app.notes if n.kind == "remote_config"]
            h = configs[-1].remote_config.config_hash if configs else b"none"
            c.set_remote_config_status(pb.RemoteConfigStatus(last_remote_config_hash=h,
                                                             status=pb.RemoteConfigStatuses_APPLIED))
        elif what == "package_installed":
            pkgs = [n for n in self.app.notes if n.kind == "install_package"]
            if pkgs:
                c.package_installed(pkgs[-1].package_name, pkgs[-1].package.hash)
            else:
                c.package_removed("pkg")

    def _notify(self, n: Notification) -> None:
        app = self.app
        app.active += 1
        try:
            if app.active > 1:
                app.problems.append(f"RULE-SHELL-01: two callbacks at once ({n.kind})")
            if threading.get_ident() != self.loop._thread_id:  # type: ignore[union-attr]
                app.problems.append(f"RULE-SHELL-03: a callback off the loop's thread ({n.kind})")
            if not app.returned:
                app.problems.append(f"RULE-SHELL-01: a callback before start returned ({n.kind})")
            if app.stopped:
                app.problems.append(f"RULE-SHELL-01: a callback after stopped ({n.kind})")
            if n.kind == "stopped" and self.stopped_at is None:
                self.stopped_at = self.loop.time()  # type: ignore[union-attr]
            if n.kind == "bad_message":
                # The server sends nothing malformed: what the core heard isn't what was sent.
                app.problems.append(f"RULE-SHELL-05: bad_message ({n.detail}): the server sent no such thing")
            app.notes.append(n)
            while app.inside:
                what = app.inside.popleft()
                if what == "stop":
                    self._request_stop()
                else:
                    self._call(what)
            if app.throw_next:
                app.throw_next = False
                e = Boom(f"raised on {n.kind}")
                if app.thrown is None:
                    app.thrown, app.thrown_before_stopped = e, n.kind != "stopped"
                raise e
        finally:
            app.active -= 1

    def _request_stop(self) -> None:
        if self.stop_task is None:
            loop = self.loop
            self.stop_at = loop.time()  # type: ignore[union-attr]
            self.stop_task = loop.create_task(self.client.stop())  # type: ignore[union-attr]

            def done(_t) -> None:
                self.stop_took = loop.time() - self.stop_at  # type: ignore[union-attr, operator]

            self.stop_task.add_done_callback(done)

    def _stop_error(self) -> BaseException | None:
        t = self.stop_task
        assert t is not None and t.done()
        return None if t.cancelled() else t.exception()

    def _sessions(self, kind: str) -> FakeSession:
        return FakeSession(self.world, kind)

    # -- the steps

    @initialize(seed=st.integers(0, 2**32 - 1), transport=st.sampled_from(["http", "ws"]),
                tls=st.booleans(), heartbeat=st.sampled_from([500, 5000, 30_000]),
                timeouts=st.sampled_from([1000, 30_000]))
    def start(self, seed: int, transport: str, tls: bool, heartbeat: int, timeouts: int) -> None:
        if (pinned := os.environ.get("OPAMP_SCHED_LOOP_SEED")) is not None:
            seed = int(pinned)
        note(f"loop seed {seed}")
        self.loop = SeededLoop(seed)
        self.world = World(self.loop, self.mode)
        self.writer = Writer(self.loop)
        self.saver = CountingSaver(self.writer)
        scheme = transport + ("s" if tls and self.mode == "rust" else "")
        config = Config(endpoint=f"{scheme}://opamp.test/v1/opamp", instance_uid=UID, capabilities=CAPABILITIES,
                        heartbeat_interval_ms=heartbeat, request_timeout_ms=timeouts, connect_timeout_ms=timeouts,
                        download_timeout_ms=timeouts, state_file=self.state, accept_packages=True, mode=self.mode)
        status = Status(description=description(0), custom_capabilities=pb.CustomCapabilities(capabilities=[CAPABILITY]))

        def go() -> None:
            self.client = Client.start(config, status, self._notify,
                                       _sessions=self._sessions if self.mode == "python" else None)
            self.app.returned = True

        self._run(0, go)

    @precondition(lambda self: self.client is not None and not self.stop_returned)
    @rule(data=st.data())
    def server_settles(self, data) -> None:
        # (One argument: Hypothesis tries the rules with fewer arguments first, and this one
        # should come up often.)
        STATS["rule server_settles"] += 1
        i = data.draw(st.integers(0, 7), label="which")
        k = data.draw(st.integers(0, 6), label="at turn")
        # Nothing asked yet: time passes until something is (a heartbeat, a retry), within a minute.
        for _ in range(240):
            if self.world.waiting() or self.stop_returned:
                break
            self._advance(0.25)
        ops = self.world.waiting()
        if not ops:
            return
        op = ops[i % len(ops)]
        cuts = st.lists(st.integers(0, 4000), max_size=4)
        if op.kind == "connect":
            how = (data.draw(st.sampled_from(["accept", "accept", "accept", "refuse"]), label="connect"),)
        elif op.kind == "post":
            how = data.draw(st.one_of(
                st.tuples(st.just("ok"), st.sampled_from([200, 200, 200, 503]), st.sampled_from(OFFERS), cuts),
                st.just(("fail",))), label="post")
        elif op.kind == "ws_open":
            how = data.draw(st.sampled_from([("ok",), ("ok",), ("ok",), ("status", 401), ("status", 503), ("fail",)]),
                            label="upgrade")
        elif op.kind == "ws_close":
            how = data.draw(st.sampled_from([("ack",), ("ack",), ("drop",)]), label="close")
        elif op.kind == "body":
            how = data.draw(st.sampled_from([("rest",), ("rest",), ("fail",)]), label="the rest of the body")
        else:
            how = data.draw(st.one_of(
                st.tuples(st.just("ok"), st.sampled_from([200, 200, 404]), st.booleans(), cuts),
                st.sampled_from([("fail",), ("fail_mid",), ("stall", True), ("stall", False)])), label="download")
        note(f"server: {op.kind} {op.url} (which={i}, at turn {k}) -> {how!r}")
        STATS[f"server {op.kind} {how[0]}"] += 1
        self._run(k, lambda: op.decide(how))

    @precondition(lambda self: self.client is not None and not self.stop_returned)
    @rule(data=st.data())
    def server_settles_more(self, data) -> None:
        """The same again: the server's answers are most of what can happen, so twice the weight."""
        self.server_settles(data)

    @precondition(lambda self: bool(self.world.open_conns()))
    @rule(i=st.integers(0, 3), k=st.integers(0, 6), what=st.sampled_from(["push", "push", "push", "close", "drop"]),
          offer=st.sampled_from(OFFERS))
    def server_on_websocket(self, i: int, k: int, what: str, offer: str) -> None:
        STATS["rule server_on_websocket"] += 1
        conns = self.world.open_conns()
        conn = conns[i % len(conns)]
        STATS[f"websocket {what}"] += 1
        if what == "push":
            self._run(k, lambda: self.world.spawn(conn.push(offer)))
        elif what == "close":
            self._run(k, lambda: self.world.server_close(conn, 1000))
        else:
            self._run(k, lambda: self.world.drop(conn))

    @precondition(lambda self: self.client is not None)
    @rule(seconds=st.sampled_from([0.001, 0.1, 1.0, 5.0, 31.0]))
    def time_passes(self, seconds: float) -> None:
        STATS["rule time_passes"] += 1
        self._advance(seconds)

    @precondition(lambda self: self.client is not None)
    @rule(what=st.sampled_from(METHODS), where=st.sampled_from(["loop", "thread", "callback"]), k=st.integers(0, 6))
    def application_calls(self, what: str, where: str, k: int) -> None:
        STATS["rule application_calls"] += 1
        STATS[f"call {where}"] += 1
        if where == "callback":
            self.app.inside.append(what)
            self._advance(0)
        elif where == "loop":
            self._run(k, lambda: self._call(what))
        else:
            # A direct call from another thread, while the loop runs (the loop's thread
            # waits for it here, so where it lands in the schedule is the seed's).
            def from_thread() -> None:
                t = threading.Thread(target=self._call, args=(what,), name="app-thread")
                t.start()
                t.join()

            self._run(k, from_thread)

    @precondition(lambda self: self.client is not None and not self.stop_returned and self.steps >= 6)
    @rule(often=st.integers(0, 3))
    def callback_raises_next(self, often: int) -> None:
        # Mostly not: a callback that raises ends the client, and the rest of the run with it.
        STATS["rule callback_raises_next"] += 1
        if often == 0:
            self.app.throw_next = True
        self._advance(0)

    @precondition(lambda self: self.client is not None)
    @rule(how=st.sampled_from(["fail", "slow", "slow"]), seconds=st.sampled_from([0.5, 3.0, 20.0]))
    def next_save(self, how: str, seconds: float) -> None:
        STATS["rule next_save"] += 1
        STATS[f"save {how}"] += 1
        self.writer.plans.append((how, seconds))

    @precondition(lambda self: self.client is not None and self.stop_task is None and self.steps >= 6)
    @rule(where=st.sampled_from(["outside", "outside", "callback"]), k=st.integers(0, 6))
    def stop(self, where: str, k: int) -> None:
        STATS["rule stop"] += 1
        if where == "callback":
            self.app.inside.append("stop")
            self._advance(0)
            return
        self._run(k, self._request_stop)
        self._advance(STOP_S + MARGIN_S)

    # -- the model

    @invariant()
    def the_contract_holds(self) -> None:
        if self.client is None:
            return
        STATS["checks"] += 1
        self.steps += 1
        STATS["checks with the server waited on"] += bool(self.world.waiting())
        loop, app, mon, world = self.loop, self.app, self.monitor, self.world
        assert loop is not None
        assert not loop.wrong_thread, f"RULE-SHELL-03: touched the loop from another thread: {loop.wrong_thread}"
        assert not app.problems, app.problems
        assert not mon.problems, mon.problems
        self.writer.check_order(mon)
        assert not mon.problems, mon.problems
        stopped = app.stopped
        assert len(stopped) <= 1, f"RULE-SHELL-01: stopped {len(stopped)} times"
        if stopped:
            assert app.notes[-1].kind == "stopped", "RULE-SHELL-01: stopped is the last notification"
            if app.thrown is not None and app.thrown_before_stopped:
                assert stopped[0].error is app.thrown, f"RULE-SHELL-02: stopped with {stopped[0].error!r}, the callback raised {app.thrown!r}"
            elif app.thrown is None:
                assert stopped[0].error is None, f"RULE-SHELL-02: stopped with {stopped[0].error!r}, nothing raised"
        customs = [int(n.custom_message.data) for n in app.notes if n.kind == "custom_message"]
        assert customs == sorted(set(customs)), f"RULE-SHELL-01: custom messages out of order: {customs}"
        configs = [int(n.remote_config.config_hash[1:]) for n in app.notes if n.kind == "remote_config"]
        assert configs == sorted(set(configs)), f"RULE-SHELL-01: remote configs out of order: {configs}"

        # RULE-SHELL-06: what the core gave up on is released by now.
        if self.mode == "python":
            live_ws = [f for f in world.fakes if not f.gone]
            conns = mon.live("connection", ("open", "peer_closed", "closing"))
            assert len(live_ws) <= conns, f"RULE-SHELL-06: {len(live_ws)} WebSockets open, the core has {conns}"
            for kind, what in (("post", "request"), ("download", "download")):
                going = [r for r in world.requests if r.kind == kind and not r.finished]
                live = mon.live(what)
                assert len(going) <= live, f"RULE-SHELL-06: {len(going)} {kind}s under way, the core waits on {live}"
        else:
            assert world.network is not None
            open_ = world.network.open()
            streams = sum(1 for s in mon.streams.values() if s in ("opening", "tls", "open"))
            assert len(open_) <= streams, f"RULE-SHELL-06: {len(open_)} connections open, the core has {streams} streams"
        client = self.client
        for id_ in mon.discarded:
            path = client._x.download_path(id_)
            assert not os.path.exists(path), f"RULE-SHELL-06: download {id_}'s file is still there"

        # RULE-SHELL-07: stop is bounded, and leaves nothing.
        if self.stop_task is not None:
            assert self.stop_at is not None
            if loop.time() >= self.stop_at + STOP_S + MARGIN_S - 1e-6:
                assert self.stop_task.done(), f"RULE-SHELL-07: stop hasn't returned {STOP_S + MARGIN_S} s after it was called"
            if self.stop_task.done() and not self.stop_returned:
                self._stop_returned()
        if app.stopped and app.thrown is not None and self.stop_task is None:
            # The client stopped on its own (a callback raised): stop gives that error.
            self._run(0, self._request_stop)
            self._advance(STOP_S + MARGIN_S)
            self.the_contract_holds()
        if self.stop_returned:
            assert len(world.ops) == self.ops_at_stop, f"RULE-SHELL-07: asked the server for {world.ops[self.ops_at_stop:]} after stop"
            if world.network is not None:
                assert len(world.network.attempts) == self.attempts_at_stop, "RULE-SHELL-07: connected after stop"

    def _stop_returned(self) -> None:
        self.stop_returned = True
        app, mon, world = self.app, self.monitor, self.world
        took = self.stop_took
        assert took is not None
        note(f"stop took {took:.3f} s (virtual)")
        assert took <= STOP_S + MARGIN_S, f"RULE-SHELL-07: stop took {took:.3f} s"
        err = self._stop_error()
        want = app.thrown  # whatever the callback raised first (on stopped too)
        assert err is want, f"RULE-SHELL-02: stop gave {err!r}, expected {want!r}"
        assert len(app.stopped) == 1, f"RULE-SHELL-01: stopped {len(app.stopped)} times once stop returned"
        # The core said stopped, or a callback raised and the client left it.
        assert mon.stopped or app.thrown is not None, "RULE-SHELL-07: stop returned before the core said stopped"
        if self.mode == "python":
            assert not [f for f in world.fakes if not f.gone], "RULE-SHELL-07: a WebSocket open after stop"
            assert not [r for r in world.requests if not r.finished], "RULE-SHELL-07: a request under way after stop"
        else:
            assert not world.network.open(), f"RULE-SHELL-07: {world.network.open()} open after stop"  # type: ignore[union-attr]
        left = [t for t in asyncio.all_tasks(self.loop) if not t.done() and "opamp_client" in t.get_coro().cr_code.co_filename]
        assert not left, f"RULE-SHELL-07: the client's tasks are left after stop: {left}"
        # The client's end began at stop(), or earlier if it ended on its own (a callback raised).
        assert self.stop_at is not None
        began = min(self.stop_at, self.stopped_at if self.stopped_at is not None else self.stop_at)
        if self.stop_at + took - began < STOP_S - 1e-6:
            with self.writer.lock:
                outstanding = self.writer.outstanding
            assert outstanding == 0, (f"RULE-SHELL-07: the client's end took {self.stop_at + took - began:.3f} s, "
                                      "and left a save in flight")
        self.ops_at_stop = len(world.ops)
        if world.network is not None:
            self.attempts_at_stop = len(world.network.attempts)

    def teardown(self) -> None:
        global CURRENT
        STATS["runs"] += 1
        STATS.update(f"input {k}" for k in self.monitor.trace)
        STATS.update(f"notified {n.kind}" for n in self.app.notes)
        if self.loop is not None:
            STATS["late results"] += self.world.late + (self.world.network.late if self.world.network else 0)
            STATS["loop reorders"] += self.loop.reorders
            STATS["loop thread jobs"] += self.loop.jobs
        if self.stop_took is not None:
            STATS["stops"] += 1
            STATS["stops within 1 s"] += self.stop_took <= 1.0
        try:
            if self.client is not None and self.stop_task is None:
                self._run(0, self._request_stop)
                self._advance(STOP_S + MARGIN_S)
                self.the_contract_holds()
        finally:
            try:
                if self.loop is not None:
                    for op in self.world.ops:
                        if op.waiting:
                            op.decide(("fail",))
                    for conn in self.world.open_conns():
                        self.world.drop(conn)
                    self.loop.finish()
            finally:
                if hasattr(self, "saver"):
                    self.writer.plans.clear()
                    self.saver.shutdown(wait=True, cancel_futures=True)
                shutil.rmtree(self.dir, ignore_errors=True)
                CURRENT = None


class PythonMode(ScheduleMachine):
    mode = "python"


class RustMode(ScheduleMachine):
    mode = "rust"


@pytest.fixture
def tapped(monkeypatch):
    monkeypatch.setattr(_client_mod, "Core", TappedCore)
    # The executor saves through its file system (_save); the writer stands in for the whole save.
    monkeypatch.setattr(_executor, "_save", lambda files, path, data: CURRENT.writer.save(path, data))  # type: ignore[union-attr]
    monkeypatch.setattr(_executor, "_saver", lambda path: CURRENT.saver)  # type: ignore[union-attr]


# covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-06, RULE-SHELL-07, RULE-SHELL-09
def explore(machine: type[ScheduleMachine], monkeypatch, examples: int | None = None) -> None:
    seed = os.environ.get("OPAMP_SCHED_SEED")
    seed = int(seed) if seed else hypothesis.core.global_force_seed
    if seed is None:
        seed = random.randrange(2**32)
    monkeypatch.setattr(hypothesis.core, "global_force_seed", seed)
    runs = examples or int(os.environ.get("HYPOTHESIS_EXAMPLES", "100"))
    print(f"schedules ({machine.mode} mode): OPAMP_SCHED_SEED={seed}, {runs} examples", flush=True)
    STATS.clear()
    try:
        run_state_machine_as_test(machine, settings=settings(
            max_examples=runs, stateful_step_count=30, deadline=None, database=None, print_blob=True, derandomize=False,
            phases=[Phase.explicit, Phase.generate] + ([] if os.environ.get("OPAMP_SCHED_NO_SHRINK") else [Phase.shrink]),
            suppress_health_check=list(HealthCheck)))
    except BaseException:
        print(f"replay: OPAMP_SCHED_SEED={seed} HYPOTHESIS_EXAMPLES={runs}", flush=True)
        raise
    finally:
        print("what the runs did:", ", ".join(f"{k}: {v}" for k, v in sorted(STATS.items())), flush=True)


@pytest.mark.parametrize("machine", [PythonMode, RustMode], ids=["python", "rust"])
def test_the_shell_contract_holds_over_explored_schedules(machine, tapped, monkeypatch) -> None:
    explore(machine, monkeypatch)


# -- what these runs found, replayed (a fix makes each pass; each failed before it)


class Drawn:
    """Stands in for Hypothesis's data in a replay: hands out the values given, in order."""

    def __init__(self, *values) -> None:
        self.values = list(values)

    def draw(self, _strategy, label: str | None = None):
        return self.values.pop(0)


def replay(machine: type[ScheduleMachine], steps) -> None:
    m = machine()
    try:
        for name, kwargs in steps:
            getattr(m, name)(**kwargs)
            m.the_contract_holds()
    finally:
        m.teardown()


def test_found_a_response_in_pieces_reaches_the_core_whole(tapped) -> None:
    # Python mode, loop seed 0: an HTTP response whose body came in two pieces reached the core
    # as its first piece alone (resp.content.read(n) returns what has come), and the core said
    # bad_message.
    # covers: RULE-SHELL-05
    replay(PythonMode, [
        ("start", dict(seed=0, transport="http", tls=False, heartbeat=500, timeouts=1000)),
        ("server_settles", dict(data=Drawn(0, 0, ("ok", 200, "custom", [100])))),
    ])


@pytest.mark.parametrize("machine", [PythonMode, RustMode], ids=["python", "rust"])
def test_found_stop_is_bounded_by_one_stop_timeout_with_a_save_writing(machine, tapped) -> None:
    # Python mode, OPAMP_SCHED_SEED=21 (loop seed 0): a save still writing when the core said
    # stopped held stop up for a second stop_timeout (the core's 5 s for the goodbye, then 5 s
    # more for the writer): 10 s, not 5 plus a margin.
    # covers: RULE-SHELL-07
    replay(machine, [
        ("start", dict(seed=0, transport="http", tls=False, heartbeat=30_000, timeouts=30_000)),
        ("next_save", dict(how="slow", seconds=20.0)),
        *([("server_settles", dict(data=Drawn(0, 0, "accept")))] if machine.mode == "rust" else []),
        ("server_settles", dict(data=Drawn(0, 0, ("ok", 200, "config", [])))),
        ("stop", dict(where="outside", k=0)),
    ])


def test_found_a_download_finished_after_its_cancel_leaves_no_file(tapped) -> None:
    # Python mode, OPAMP_SCHED_SEED=21 (loop seed 1 here): a download whose response came as stop
    # cancelled it (the cancel didn't land in its task: here the fake's, as a library that
    # swallows a cancel would) wrote its whole file after its cancel had deleted it, and the file
    # stayed.
    # covers: RULE-SHELL-06
    replay(PythonMode, [
        ("start", dict(seed=1, transport="http", tls=False, heartbeat=30_000, timeouts=30_000)),
        ("server_settles", dict(data=Drawn(0, 0, ("ok", 200, "package", [])))),
        ("stop", dict(where="outside", k=0)),
    ])


def test_found_stopped_is_the_last_notification(tapped) -> None:
    # Python mode, OPAMP_SCHED_SEED=1 (loop seed 0): stop while a save that answers a package's
    # download was writing; the save landed, and the core's output for `stored` held stopped and
    # then install_package (the core emits a notification after stopped). The shell passed them
    # on in that order: the application heard install_package after stopped.
    # covers: RULE-SHELL-01
    replay(PythonMode, [
        ("start", dict(seed=0, transport="http", tls=False, heartbeat=500, timeouts=1000)),
        ("next_save", dict(how="fail", seconds=0.5)),
        ("next_save", dict(how="slow", seconds=0.5)),
        ("server_settles", dict(data=Drawn(0, 0, ("ok", 200, "none", [])))),
        ("server_settles", dict(data=Drawn(0, 0, ("ok", 200, "package", [])))),
        ("server_settles", dict(data=Drawn(1, 0, ("ok", 200, True, [])))),
        ("server_settles", dict(data=Drawn(0, 0, ("ok", 200, "none", [])))),
        ("server_settles", dict(data=Drawn(0, 0, ("fail",)))),
    ])
