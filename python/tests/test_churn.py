# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""A client started and stopped again and again in one process leaves nothing behind: an agent
that reconfigures its client runs for months, starting a new one each time. Each test churns
clients through one way of stopping, in both modes and over both transports, then counts what the
process holds against what it held after a warm-up: the native core's sessions, threads, file
descriptors, the loop's tasks, and the Python objects allocated. A leak of one of anything per
client shows as growth with the number of clients; what a warm-up fills once (aiohttp's caches,
asyncio's thread pool) does not.

The server runs on a loop of its own, in its own thread: what it holds (held upgrades, open
connections it has yet to see end) is not the client's loop's to count."""

from __future__ import annotations

import asyncio
import contextlib
import gc
import os
import sys
import threading
from collections.abc import Awaitable, Callable, Iterator
from dataclasses import dataclass

import pytest
from _servers import MODES, UID, OpampServer, wait_for

from pydantic_opamp_client import Client, Config, Notification, Status, _native, opamp_pb2 as pb

# Clients started before counting (caches, pools and lazily made objects fill up), then counted.
WARM = 5
CYCLES = 30

# What a count may grow by over CYCLES clients and still be noise: a thread pool's thread starting
# late, a connection the server has yet to see end. A leak of one per client is CYCLES.
SLACK = {"native sessions": 0, "threads": 3, "fds": 6, "tasks": 2}
# Python's allocated blocks move by a few hundred on their own (caches, free lists, the server's
# record of what arrived); even a stopped client kept alive is 60 (platform mode) to 350 (core
# mode), and CYCLES of them are more.
BLOCKS_SLACK = 1500

FDS = "/proc/self/fd"

REMOTE = pb.AgentRemoteConfig(
    config=pb.AgentConfigMap(config_map={"": pb.AgentConfigObject(body=b"x: 1", content_type="text/yaml")}),
    config_hash=b"h1")


@dataclass
class Counts:
    native: int
    threads: int
    fds: int
    tasks: int
    blocks: int

    def by_name(self) -> dict[str, int]:
        return {"native sessions": self.native, "threads": self.threads, "fds": self.fds, "tasks": self.tasks}


class Sessions:
    """The native core's sessions alive, of those made since: opamp_core_new and opamp_core_free
    watched. The library keeps no count of its own; its two entry points are what the client
    calls. (By handle: a session of an earlier test may be freed while this one runs.)"""

    def __init__(self, monkeypatch: pytest.MonkeyPatch) -> None:
        self._handles: set[int] = set()
        self._lock = threading.Lock()
        new, free = _native._lib().opamp_core_new, _native._lib().opamp_core_free

        def counted_new():
            handle = new()
            with self._lock:
                self._handles.add(handle)
            return handle

        def counted_free(handle) -> None:
            with self._lock:
                self._handles.discard(handle)
            free(handle)

        monkeypatch.setattr(_native._lib(), "opamp_core_new", counted_new)
        monkeypatch.setattr(_native._lib(), "opamp_core_free", counted_free)

    @property
    def live(self) -> int:
        with self._lock:
            return len(self._handles)


@contextlib.contextmanager
def served(**knobs) -> Iterator[OpampServer]:
    """An OpampServer on a loop of its own, in its own thread."""
    loop = asyncio.new_event_loop()
    thread = threading.Thread(target=loop.run_forever, name="churn-server", daemon=True)
    thread.start()
    server = asyncio.run_coroutine_threadsafe(OpampServer(**knobs).start(), loop).result(10)
    try:
        yield server
    finally:
        asyncio.run_coroutine_threadsafe(server.close(), loop).result(10)
        loop.call_soon_threadsafe(loop.stop)
        thread.join(10)
        loop.close()


def counts(sessions: Sessions) -> Counts:
    gc.collect()
    me = asyncio.current_task()
    return Counts(
        native=sessions.live,
        threads=threading.active_count(),
        fds=len(os.listdir(FDS)) if os.path.isdir(FDS) else 0,
        tasks=sum(1 for t in asyncio.all_tasks() if t is not me),
        blocks=sys.getallocatedblocks(),
    )


async def churn(sessions: Sessions, cycle: Callable[[], Awaitable[None]]) -> None:
    """Run `cycle` WARM times, count, CYCLES times more, and count again: nothing has grown. What
    winds down after a stop (a save's thread ending, the server closing its end) is given a few
    seconds to; a leak never comes back down."""
    for _ in range(WARM):
        await cycle()
    await asyncio.sleep(0.2)
    before = counts(sessions)
    for _ in range(CYCLES):
        await cycle()
    # Before anything is collected: each client freed its session as it stopped, not when the
    # garbage collector got to it (a client is in reference cycles; the collector doesn't count
    # the library's memory, and an application may run with it off).
    assert sessions.live == 0, f"{sessions.live} native sessions alive with every client stopped"

    def settled() -> bool:
        after = counts(sessions)
        return all(after.by_name()[k] <= v + SLACK[k] for k, v in before.by_name().items()) \
            and after.blocks <= before.blocks + BLOCKS_SLACK

    await wait_for(settled, 5)
    after = counts(sessions)
    for name, was in before.by_name().items():
        now = after.by_name()[name]
        assert now <= was + SLACK[name], f"{name}: {was} after the warm-up, {now} after {CYCLES} more clients"
    assert after.blocks <= before.blocks + BLOCKS_SLACK, \
        f"Python's allocated blocks: {before.blocks} after the warm-up, {after.blocks} after {CYCLES} more clients"


def config(mode: str, endpoint: str, **more) -> Config:
    return Config(mode=mode, endpoint=endpoint, instance_uid=UID, heartbeat_interval_ms=50, **more)


@pytest.mark.parametrize("transport", ["http", "ws"])
@pytest.mark.parametrize("mode", MODES)
def test_clients_stopped_right_after_start_leave_nothing_behind(monkeypatch, mode: str, transport: str) -> None:
    # Stopped before the loop has run once: the first request or upgrade is cancelled before it
    # has a socket, or as it gets one.
    sessions = Sessions(monkeypatch)
    with served() as server:
        async def cycle() -> None:
            client = Client.start(config(mode, server.endpoint(transport)), Status())
            await asyncio.wait_for(client.stop(), 10)

        asyncio.run(churn(sessions, cycle))


@pytest.mark.parametrize("mode", MODES)
def test_clients_stopped_during_the_websocket_handshake_leave_nothing_behind(monkeypatch, mode: str) -> None:
    # The server holds each upgrade longer than the client lives: the dial is given up on with the
    # connection open and the upgrade sent, unanswered.
    sessions = Sessions(monkeypatch)
    with served(hold_upgrade=0.3) as server:
        async def cycle() -> None:
            client = Client.start(config(mode, server.endpoint("ws")), Status())
            await asyncio.sleep(0.03)
            await asyncio.wait_for(client.stop(), 10)

        asyncio.run(churn(sessions, cycle))


@pytest.mark.parametrize("transport", ["http", "ws"])
@pytest.mark.parametrize("mode", MODES)
def test_clients_stopped_while_connected_leave_nothing_behind(monkeypatch, mode: str, transport: str) -> None:
    # Stopped once the server has answered and the next heartbeat (or poll) is out: the goodbye
    # goes out, and over WebSocket the closing handshake runs.
    sessions = Sessions(monkeypatch)
    with served() as server:
        async def cycle() -> None:
            had = len(server.arrivals)
            client = Client.start(config(mode, server.endpoint(transport)), Status())
            await wait_for(lambda: len(server.arrivals) >= had + 2)
            assert len(server.arrivals) >= had + 2, "not connected"
            await asyncio.wait_for(client.stop(), 10)

        asyncio.run(churn(sessions, cycle))


@pytest.mark.parametrize("transport", ["http", "ws"])
@pytest.mark.parametrize("mode", MODES)
def test_clients_stopped_from_the_remote_config_callback_leave_nothing_behind(monkeypatch, mode: str,
                                                                            transport: str) -> None:
    # The callback answers the remote config and starts the stop itself: the stop is fed while the
    # answer is still on its way out, the step that told the callback barely over.
    sessions = Sessions(monkeypatch)
    with served(remote_config=REMOTE) as server:
        async def cycle() -> None:
            stopping: list[asyncio.Task] = []

            def notify(n: Notification) -> None:
                if n.kind == "remote_config":
                    client.set_remote_config_status(pb.RemoteConfigStatus(
                        last_remote_config_hash=n.remote_config.config_hash,
                        status=pb.RemoteConfigStatuses_APPLIED))
                    stopping.append(asyncio.get_running_loop().create_task(client.stop()))

            client = Client.start(config(mode, server.endpoint(transport),
                                         capabilities=pb.AgentCapabilities_AcceptsRemoteConfig), Status(), notify)
            await wait_for(lambda: bool(stopping))
            assert stopping, "no remote config"
            await asyncio.wait_for(stopping[0], 10)

        asyncio.run(churn(sessions, cycle))


@pytest.mark.parametrize("transport", ["http", "ws"])
@pytest.mark.parametrize("mode", MODES)
def test_clients_on_one_state_file_leave_nothing_behind(monkeypatch, tmp_path, mode: str, transport: str) -> None:
    # One state file for every client, as an agent restarting its client has: each saves (its
    # first answer, a remote config) on the file's thread, and the next reads what it saved.
    sessions = Sessions(monkeypatch)
    state = str(tmp_path / "state")
    with served(remote_config=REMOTE) as server:
        async def cycle() -> None:
            told = asyncio.Event()

            def notify(n: Notification) -> None:
                if n.kind == "remote_config":
                    told.set()

            client = Client.start(config(mode, server.endpoint(transport), state_file=state,
                                         capabilities=pb.AgentCapabilities_AcceptsRemoteConfig), Status(), notify)
            await asyncio.wait_for(told.wait(), 10)
            await asyncio.wait_for(client.stop(), 10)

        asyncio.run(churn(sessions, cycle))


@pytest.mark.parametrize("transport", ["http", "ws"])
@pytest.mark.parametrize("mode", MODES)
def test_clients_whose_callback_throws_stop_themselves_and_leave_nothing_behind(monkeypatch, mode: str,
                                                                              transport: str) -> None:
    # The callback raises on the remote config: the client stops on its own, mid-connection, with
    # no stop called, and stop() raises the callback's error.
    sessions = Sessions(monkeypatch)

    class Bug(Exception):
        pass

    with served(remote_config=REMOTE) as server:
        async def cycle() -> None:
            stopped = asyncio.Event()

            def notify(n: Notification) -> None:
                if n.kind == "stopped":
                    stopped.set()
                elif n.kind == "remote_config":
                    raise Bug()

            client = Client.start(config(mode, server.endpoint(transport),
                                         capabilities=pb.AgentCapabilities_AcceptsRemoteConfig), Status(), notify)
            await asyncio.wait_for(stopped.wait(), 10)
            with pytest.raises(Bug):
                await asyncio.wait_for(client.stop(), 10)

        asyncio.run(churn(sessions, cycle))
