# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The callback contract (spec/shell-contract.md, RULE-SHELL-01 to -03 and -07), as a model test:
random runs of a Hypothesis state machine against the real core and a local server. Each
step is one of: start (over HTTP or WebSocket, with a state file holding a saved remote config or
a fresh one, the callback set to raise on its k-th call or not), an application call from outside
(on the loop), from another thread or from inside the callback, stop from outside or requested
from inside the callback, or the server offering a remote config, a custom message or a heartbeat
interval. After each step the notifications seen so far are checked against the model:

- callbacks one at a time, none before `start` returned, none after `stopped`;
- `stopped` at most once (exactly once once `stop` has returned), and last; with the error when a
  callback raised before it;
- a callback that raised stops the client: `stopped` comes, and `stop` raises that error every
  time it is called;
- `stop` returns within its bound (from outside, or requested from inside the callback);
- the order the model can predict: the saved remote config first, the server's custom messages
  and remote configs in the order offered, the descriptions set in the order they were set.

A call from another thread is made directly from that thread; the client carries it out on the
loop. The separate test at the end does the same alone.

Each test runs in both modes: Python mode (aiohttp), and Rust mode (the core's own HTTP and
WebSocket, over asyncio's connections), against the same server.

Runs: ``OPAMP_MODEL_RUNS`` per mode (default 30); replay a failing run's seed (printed) with
``OPAMP_MODEL_SEED`` (or pytest's ``--hypothesis-seed``), and ``-k python`` or ``-k rust`` for its
mode: a failure prints the whole line."""

from __future__ import annotations

import asyncio
import gc
import os
import random
import tempfile
import threading
import time
import warnings
from collections import deque

import hypothesis.core
import pytest
from aiohttp import WSMsgType, web
from hypothesis import HealthCheck, Phase, settings, strategies as st
from hypothesis.stateful import RuleBasedStateMachine, invariant, precondition, rule, run_state_machine_as_test

from opamp_client import Client, Config, Notification, Status, anyvalue_pb2, opamp_pb2 as pb

UID = bytes([0x42] * 16)
CAPABILITY = "io.example.model"
CAPABILITIES = pb.AgentCapabilities_AcceptsRemoteConfig | pb.AgentCapabilities_ReportsRemoteConfig
SAVED_HASH = b"saved"
STOP_BOUND = 5 + 2  # the request timeout (the goodbye in flight), plus a margin
SETTLE = 0.03  # how long the loop runs after each step
METHODS = ["set_health", "set_description", "send_custom_message", "answer_config"]
MODES = ["python", "rust"]


class Boom(Exception):
    """What the callback raises on its k-th call."""


def description(n: int) -> pb.AgentDescription:
    return pb.AgentDescription(non_identifying_attributes=[
        anyvalue_pb2.KeyValue(key="n", value=anyvalue_pb2.AnyValue(int_value=n))])


class Server:
    """An OpAMP server over HTTP and WebSocket that sends what the steps offer (once each), and
    records the descriptions it hears. A WebSocket idle for 3 s is closed: a connection that opens
    after stop then ends, so that stop returns."""

    IDLE_CLOSE = 3.0

    def __init__(self) -> None:
        self.configs = 0
        self.customs = 0
        self.pending: dict[str, object] = {}
        self.heartbeat = 0  # the last interval offered, seconds
        self.descriptions: list[int] = []
        self.conns: set[web.WebSocketResponse] = set()
        self.pushes: set[asyncio.Future] = set()
        self.port = 0
        self._runner: web.AppRunner | None = None

    def offer_config(self, hash_: bytes | None = None) -> None:
        if hash_ is None:
            self.configs += 1
            hash_ = b"c%d" % self.configs
        self.pending["config"] = hash_
        self._push()

    def offer_custom(self) -> None:
        self.customs += 1
        self.pending.setdefault("customs", [])
        # One custom message per answer: the rest wait for the next.
        self.pending["customs"].append(self.customs)  # type: ignore[union-attr]
        self._push()

    def offer_heartbeat(self) -> None:
        # 1 s, then 0 (over HTTP: back to the configured interval; over WebSocket: off).
        self.heartbeat = 0 if self.heartbeat else 1
        self.pending["heartbeat"] = self.heartbeat
        self._push()

    def _reply(self, msg: pb.AgentToServer) -> pb.ServerToAgent:
        if msg.HasField("agent_description"):
            self.descriptions.append(msg.agent_description.non_identifying_attributes[0].value.int_value)
        return self._offers(msg.instance_uid)

    def _offers(self, uid: bytes) -> pb.ServerToAgent:
        r = pb.ServerToAgent(instance_uid=uid)
        if (h := self.pending.pop("config", None)) is not None:
            r.remote_config.config_hash = h  # type: ignore[assignment]
            r.remote_config.config.config_map[""].body = h  # type: ignore[assignment]
        if (customs := self.pending.get("customs")):
            n = customs.pop(0)  # type: ignore[union-attr]
            r.custom_message.CopyFrom(pb.CustomMessage(capability=CAPABILITY, type="n", data=str(n).encode()))
            if not customs:
                del self.pending["customs"]
        if (s := self.pending.pop("heartbeat", None)) is not None:
            r.connection_settings.opamp.heartbeat_interval_seconds = s  # type: ignore[assignment]
        return r

    def _push(self) -> None:
        """Over WebSocket, offers go out at once."""
        for ws in list(self.conns):
            if not ws.closed:
                task = asyncio.ensure_future(ws.send_bytes(b"\0" + self._offers(UID).SerializeToString()))
                self.pushes.add(task)
                task.add_done_callback(self.pushes.discard)

    async def _http(self, req: web.Request) -> web.Response:
        msg = pb.AgentToServer.FromString(await req.read())
        return web.Response(body=self._reply(msg).SerializeToString(), content_type="application/x-protobuf")

    async def _ws(self, req: web.Request) -> web.WebSocketResponse:
        ws = web.WebSocketResponse()
        await ws.prepare(req)
        self.conns.add(ws)
        try:
            while True:
                try:
                    m = await ws.receive(timeout=self.IDLE_CLOSE)
                except asyncio.TimeoutError:
                    await ws.close()
                    break
                if m.type == WSMsgType.BINARY:
                    reply = self._reply(pb.AgentToServer.FromString(m.data[1:]))
                    await ws.send_bytes(b"\0" + reply.SerializeToString())
                elif m.type in (WSMsgType.CLOSE, WSMsgType.CLOSING, WSMsgType.CLOSED, WSMsgType.ERROR):
                    break
        finally:
            self.conns.discard(ws)
        return ws

    async def start(self) -> None:
        app = web.Application()
        app.router.add_post("/v1/opamp", self._http)
        app.router.add_get("/v1/opamp", self._ws)
        self._runner = web.AppRunner(app)
        await self._runner.setup()
        site = web.TCPSite(self._runner, "127.0.0.1", 0)
        await site.start()
        self.port = site._server.sockets[0].getsockname()[1]  # type: ignore[union-attr]

    async def close(self) -> None:
        if self.pushes:
            await asyncio.wait(list(self.pushes))
        for ws in list(self.conns):
            await ws.close()
        if self._runner is not None:
            await self._runner.cleanup()


class Run:
    """One client, from start until stop has returned, and what its callback saw."""

    def __init__(self, ws: bool, saved: bool, throw_at: int | None, again: bool) -> None:
        self.ws, self.saved = ws, saved
        self.client: Client | None = None
        self.returned = False  # start has returned
        self.notes: list[Notification] = []
        self.calls = 0
        self.active = 0
        self.problems: list[str] = []
        self.throw_at, self.again = throw_at, again
        self.thrown: Boom | None = None
        self.thrown_before_stopped = False
        self.inside: deque[str] = deque()  # what the next callback does
        self.stops: list[asyncio.Future] = []  # stops requested from inside the callback
        self.stop_returned = False
        self.described = 0

    @property
    def stopped(self) -> list[Notification]:
        return [n for n in self.notes if n.kind == "stopped"]


class CallbackModel(RuleBasedStateMachine):
    mode = "python"  # the client's: PythonMode's, or RustMode's

    def __init__(self) -> None:
        super().__init__()
        self.loop = asyncio.new_event_loop()
        self.server = Server()
        self.loop.run_until_complete(self.server.start())
        self.dir = tempfile.mkdtemp(prefix="opamp-model-")
        self.files = 0
        self.saved: dict[bool, bytes] = {}  # the state file holding a remote config, per transport
        self.run: Run | None = None
        self.descriptions = 0
        self.unhandled: list[object] = []
        self.loop.set_exception_handler(lambda _loop, ctx: self.unhandled.append(ctx.get("exception") or ctx))

    # -- plumbing

    def _do(self, coro):
        return self.loop.run_until_complete(coro)

    def _endpoint(self, ws: bool) -> str:
        return f"{'ws' if ws else 'http'}://127.0.0.1:{self.server.port}/v1/opamp"

    def _config(self, ws: bool, state_file: str) -> Config:
        return Config(mode=self.mode, endpoint=self._endpoint(ws), instance_uid=UID, capabilities=CAPABILITIES,
                      heartbeat_interval_ms=50, request_timeout_ms=5000, state_file=state_file)

    def _status(self) -> Status:
        return Status(description=description(0),
                      custom_capabilities=pb.CustomCapabilities(capabilities=[CAPABILITY]))

    def _state_file(self, data: bytes | None) -> str:
        self.files += 1
        path = os.path.join(self.dir, f"state{self.files}")
        if data is not None:
            with open(path, "wb") as f:
                f.write(data)
        return path

    async def _prime(self, ws: bool) -> bytes:
        """A state file holding a remote config: one the server offered, saved by a client."""
        path = self._state_file(None)
        got = asyncio.Event()
        self.server.offer_config(SAVED_HASH)
        client = Client.start(self._config(ws, path), self._status(),
                              lambda n: got.set() if n.kind == "remote_config" else None)
        await asyncio.wait_for(got.wait(), 5)
        await asyncio.wait_for(client.stop(), STOP_BOUND)
        self.server.pending.clear()
        with open(path, "rb") as f:
            return f.read()

    def _call(self, run: Run, what: str) -> None:
        """An application call (from the loop's thread or another)."""
        c = run.client
        assert c is not None
        if what == "set_health":
            c.set_health(pb.ComponentHealth(healthy=True, status=str(time.monotonic())))
        elif what == "set_description":
            # Numbered when made: the server must hear them in that order.
            self.descriptions += 1
            run.described = self.descriptions
            c.set_description(description(self.descriptions))
        elif what == "send_custom_message":
            c.send_custom_message(pb.CustomMessage(capability=CAPABILITY, type="t", data=b"x"))
        else:
            configs = [n for n in run.notes if n.kind == "remote_config"]
            h = configs[-1].remote_config.config_hash if configs else b"none"
            c.set_remote_config_status(pb.RemoteConfigStatus(
                last_remote_config_hash=h, status=pb.RemoteConfigStatuses_APPLIED))

    def _notify(self, run: Run, n: Notification) -> None:
        run.calls += 1
        run.active += 1
        try:
            if run.active > 1:
                run.problems.append(f"two callbacks at once ({n.kind}, call {run.calls})")
            if not run.returned:
                run.problems.append(f"a callback before start returned ({n.kind})")
            if run.stopped:
                run.problems.append(f"a callback after stopped ({n.kind}, error {n.error!r})")
            run.notes.append(n)
            while run.inside:
                what = run.inside.popleft()
                if what == "stop":
                    # Requested, not waited for: that would deadlock (RULE-SHELL-03).
                    run.stops.append(asyncio.ensure_future(run.client.stop()))  # type: ignore[union-attr]
                else:
                    self._call(run, what)
            if run.throw_at is not None and (run.calls == run.throw_at or (run.again and run.calls > run.throw_at)):
                e = Boom(f"raised on call {run.calls} ({n.kind})")
                if run.thrown is None:
                    run.thrown = e
                    run.thrown_before_stopped = not run.stopped
                raise e
        finally:
            run.active -= 1

    async def _settle(self, run: Run | None) -> None:
        await asyncio.sleep(SETTLE)
        if run is not None and run.thrown is not None and run.thrown_before_stopped:
            # A callback that raised stops the client: stopped comes, soon.
            for _ in range(200):
                if run.stopped:
                    break
                await asyncio.sleep(0.01)
            else:
                run.problems.append(f"no stopped within 2 s of the callback raising {run.thrown!r}")

    async def _nudge(self, run: Run, until) -> None:
        """Have the server send something (so the callback is called), until `until()`."""
        for i in range(300):
            if until() or run.stopped:
                return
            if i % 10 == 0:
                self.server.offer_custom()
            await asyncio.sleep(0.01)

    async def _await_stop(self, run: Run, stop) -> None:
        """Wait for a stop, with its bound; it gives the error a callback raised, or nothing."""
        begun = time.monotonic()
        try:
            await asyncio.wait_for(stop, STOP_BOUND)
            err: BaseException | None = None
        except asyncio.TimeoutError:
            run.problems.append(f"stop didn't return within {STOP_BOUND} s")
            return
        except BaseException as e:  # noqa: BLE001
            err = e
        took = time.monotonic() - begun
        if took > 1.5 and run.ws:
            print(f"stop took {took:.1f} s over WebSocket: a connection that opened after stop held it "
                  "until the server closed it")
        if err is not run.thrown:
            run.problems.append(f"stop gave {err!r}, expected {run.thrown!r}")

    # -- the steps

    @precondition(lambda self: self.run is None or self.run.stop_returned)
    @rule(ws=st.booleans(), saved=st.booleans(), throw_at=st.none() | st.integers(1, 6), again=st.booleans())
    def start(self, ws: bool, saved: bool, throw_at: int | None, again: bool) -> None:
        async def go() -> None:
            if saved and ws not in self.saved:
                self.saved[ws] = await self._prime(ws)
            run = Run(ws, saved, throw_at, again)
            self.server.descriptions.clear()
            path = self._state_file(self.saved[ws] if saved else None)
            run.client = Client.start(self._config(ws, path), self._status(), lambda n: self._notify(run, n))
            run.returned = True
            self.run = run
            await self._settle(run)

        self._do(go())

    @precondition(lambda self: self.run is not None)
    @rule(what=st.sampled_from(METHODS))
    def call_outside(self, what: str) -> None:
        run = self.run
        assert run is not None

        async def go() -> None:
            self._call(run, what)  # after stop too: it does nothing
            await self._settle(run)

        self._do(go())

    @precondition(lambda self: self.run is not None)
    @rule(what=st.sampled_from(METHODS))
    def call_from_another_thread(self, what: str) -> None:
        # Called directly from the thread; the client carries it out on the loop.
        run = self.run
        assert run is not None

        async def go() -> None:
            t = threading.Thread(target=lambda: self._call(run, what))
            t.start()
            await asyncio.to_thread(t.join)
            await self._settle(run)

        self._do(go())

    @precondition(lambda self: self.run is not None and not self.run.stop_returned)
    @rule(what=st.sampled_from(METHODS))
    def call_inside(self, what: str) -> None:
        run = self.run
        assert run is not None
        run.inside.append(what)

        async def go() -> None:
            await self._nudge(run, lambda: not run.inside)
            await self._settle(run)

        self._do(go())

    @precondition(lambda self: self.run is not None and not self.run.stop_returned)
    @rule(k=st.integers(1, 3), again=st.booleans())
    def raise_on_a_later_call(self, k: int, again: bool) -> None:
        run = self.run
        assert run is not None
        if run.throw_at is None:
            run.throw_at, run.again = run.calls + k, again

        async def go() -> None:
            await self._nudge(run, lambda: run.calls >= run.throw_at)  # type: ignore[operator]
            await self._settle(run)

        self._do(go())

    @precondition(lambda self: self.run is not None and not self.run.stop_returned)
    @rule()
    def stop_outside(self) -> None:
        # covers: RULE-SHELL-07
        run = self.run
        assert run is not None

        async def go() -> None:
            await self._await_stop(run, run.client.stop())  # type: ignore[union-attr]
            await self._stop_returned(run)

        self._do(go())

    @precondition(lambda self: self.run is not None and not self.run.stop_returned)
    @rule()
    def stop_inside(self) -> None:
        # covers: RULE-SHELL-03, RULE-SHELL-07
        run = self.run
        assert run is not None
        run.inside.append("stop")

        async def go() -> None:
            await self._nudge(run, lambda: bool(run.stops))
            if run.stops:
                await self._await_stop(run, asyncio.shield(run.stops[0]))
            else:
                # The client stopped (a callback raised) before a callback could ask: stop from
                # outside instead.
                run.inside.clear()
                await self._await_stop(run, run.client.stop())  # type: ignore[union-attr]
            await self._stop_returned(run)

        self._do(go())

    async def _stop_returned(self, run: Run) -> None:
        run.stop_returned = True
        await self._settle(None)
        if len(run.stopped) != 1:
            run.problems.append(f"stopped {len(run.stopped)} times once stop returned")
        # Every later stop gives the same.
        await self._await_stop(run, run.client.stop())  # type: ignore[union-attr]

    @precondition(lambda self: self.run is not None)
    @rule(what=st.sampled_from(["config", "two_configs", "custom", "heartbeat"]))
    def server_offers(self, what: str) -> None:
        run = self.run

        async def go() -> None:
            if what == "two_configs":
                # The second arrives while the application has the first; the application answers the
                # first from inside a later callback, and the core hands the second over in the step
                # of that answer: a callback must not run inside the callback (RULE-SHELL-01, -03).
                configs = lambda: sum(n.kind == "remote_config" for n in run.notes)  # noqa: E731
                before = configs()
                self.server.offer_config()
                for _ in range(100):
                    if configs() > before or run.stopped:
                        break
                    await asyncio.sleep(0.01)
                self.server.offer_config()
                for _ in range(150):
                    if "config" not in self.server.pending or run.stopped:
                        break
                    await asyncio.sleep(0.01)
                await asyncio.sleep(0.05)
                run.inside.append("answer_config")
                await self._nudge(run, lambda: not run.inside)
            else:
                getattr(self.server, f"offer_{what}")()
            await self._settle(run)

        self._do(go())

    # -- the model

    @invariant()
    def the_contract_holds(self) -> None:
        run = self.run
        if run is None:
            return
        assert not run.problems, run.problems
        assert not self.unhandled, f"unhandled on the loop: {self.unhandled}"
        stopped = run.stopped
        assert len(stopped) <= 1, f"stopped {len(stopped)} times: {[n.error for n in stopped]}"
        if stopped:
            assert run.notes[-1].kind == "stopped", "stopped is the last notification"
            if run.thrown is not None and run.thrown_before_stopped:
                assert stopped[0].error is run.thrown, f"stopped with {stopped[0].error!r}, the callback raised {run.thrown!r}"
            elif run.thrown is None:
                assert stopped[0].error is None, f"stopped with {stopped[0].error!r}, nothing raised"
        if run.saved and run.notes:
            first = run.notes[0]
            assert first.kind == "remote_config" and first.remote_config.config_hash == SAVED_HASH, (
                f"the saved remote config first, not {first.kind}")
        customs = [int(n.custom_message.data) for n in run.notes if n.kind == "custom_message"]
        assert customs == sorted(set(customs)), f"custom messages in the order sent: {customs}"
        configs = [n.remote_config.config_hash for n in run.notes if n.kind == "remote_config"
                   and n.remote_config.config_hash != SAVED_HASH]
        numbers = [int(h[1:]) for h in configs]
        assert numbers == sorted(set(numbers)), f"remote configs in the order offered: {configs}"
        heard = self.server.descriptions
        assert heard == sorted(heard), f"descriptions in the order they were set: {heard}"

    def teardown(self) -> None:
        run = self.run

        async def go() -> None:
            if run is not None and not run.stop_returned:
                try:
                    await asyncio.wait_for(run.client.stop(), STOP_BOUND)  # type: ignore[union-attr]
                except BaseException:  # noqa: BLE001 - checked by the steps, not here
                    pass
            await self.server.close()

        try:
            self._do(go())
        finally:
            self.loop.close()


class PythonMode(CallbackModel):
    mode = "python"


class RustMode(CallbackModel):
    mode = "rust"


# covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-07
@pytest.mark.parametrize("machine", [PythonMode, RustMode], ids=MODES)
def test_the_callback_contract_holds_over_random_runs(machine: type[CallbackModel], monkeypatch) -> None:
    seed = os.environ.get("OPAMP_MODEL_SEED")
    seed = int(seed) if seed else hypothesis.core.global_force_seed
    if seed is None:
        seed = random.randrange(2**32)
    monkeypatch.setattr(hypothesis.core, "global_force_seed", seed)
    runs = int(os.environ.get("OPAMP_MODEL_RUNS", "30"))
    try:
        run_state_machine_as_test(machine, settings=settings(
            max_examples=runs, stateful_step_count=12, deadline=None, database=None, print_blob=True, derandomize=False,
            phases=[Phase.explicit, Phase.generate], suppress_health_check=list(HealthCheck)))
    except BaseException:
        print(f"replay: OPAMP_MODEL_SEED={seed} OPAMP_MODEL_RUNS={runs} uv run --python 3.13 pytest -q "
              f"tests/test_callback_properties.py -k {machine.mode}")
        raise


@pytest.mark.parametrize("mode", MODES)
def test_a_call_from_another_thread_is_safe(mode: str) -> None:
    # covers: RULE-SHELL-03
    async def run() -> None:
        server = Server()
        await server.start()
        notes: list[Notification] = []
        with tempfile.TemporaryDirectory() as d:
            client = Client.start(Config(mode=mode, endpoint=f"http://127.0.0.1:{server.port}/v1/opamp", instance_uid=UID,
                                         heartbeat_interval_ms=60_000, state_file=os.path.join(d, "s")),
                                  Status(description=description(0)), notes.append)
            for _ in range(500):  # the first exchange is over: nothing in flight
                if server.descriptions:
                    break
                await asyncio.sleep(0.01)
            await asyncio.sleep(0.05)
            # A direct call from another thread: the request it makes is started off the loop.
            await asyncio.to_thread(client.set_description, description(1))
            for _ in range(200):
                if server.descriptions[-1:] == [1] or any(n.kind != "connected" for n in notes):
                    break
                await asyncio.sleep(0.01)
            try:
                await asyncio.wait_for(client.stop(), STOP_BOUND)
                err = None
            except Exception as e:  # noqa: BLE001
                err = e
            await server.close()
        assert err is None, f"the client stopped with {err!r}"
        assert 1 in server.descriptions, "the call reached the server"

    try:
        asyncio.run(run())
    finally:
        # The request the call made off the loop was never started: its coroutine goes here, not
        # with a later test's warnings.
        with warnings.catch_warnings():
            warnings.simplefilter("ignore", RuntimeWarning)
            gc.collect()
