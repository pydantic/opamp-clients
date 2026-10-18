# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The seeded loop (tests/_sched_loop.py) runs the real thing (the risk: aiohttp makes tasks,
handles and threads of its own, and the loop must own them all): a real client, over a real
aiohttp session in platform mode and asyncio's own connections in core mode, against a real local
server, with saves to a real state file, under shuffled task order and a virtual clock. Then the
loop's own promises: virtual time moves only when nothing can run, never past a thread at work;
threads' results arrive in submission order; and a seed is a schedule."""

from __future__ import annotations

import asyncio
import os
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import pytest

from _sched_loop import SeededLoop, Stuck
from _servers import MODES, OpampServer, run_client


@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("transport", ["http", "ws"])
@pytest.mark.parametrize("seed", range(1, 1 + int(os.environ.get("OPAMP_SCHED_LOOP_SEEDS", "2"))))
def test_a_real_client_runs_on_the_seeded_loop(mode: str, transport: str, seed: int, tmp_path) -> None:
    loop = SeededLoop(seed, auto=True, io_grace=0.02)
    before: set[asyncio.Task] = set()

    async def run() -> None:
        before.update(asyncio.all_tasks())
        server = await OpampServer().start()
        try:
            t0 = loop.time()
            notes = await asyncio.wait_for(
                run_client(server, server.endpoint(transport), messages=4, mode=mode,
                           state_file=str(tmp_path / "state")), 60)
            assert loop.time() > t0, "virtual time moved"
        finally:
            await server.close()
        assert [n.kind for n in notes][-1:] == ["stopped"], [n.kind for n in notes]
        assert len(server.arrivals) >= 4
        # Nothing the client started is left.
        left = [t for t in asyncio.all_tasks() - before if t is not asyncio.current_task() and not t.done()]
        assert not left, left

    try:
        loop.run_until_complete(run())
        assert not loop.wrong_thread, loop.wrong_thread
        assert loop.reorders > 0, "the seed reordered tasks"
        assert loop.jobs > 0, "the state file's writes went through the loop's threads"
        assert loop.busy == 0
    finally:
        loop.finish()


def test_virtual_time_waits_for_threads_and_their_results_come_in_order() -> None:
    loop = SeededLoop(7)
    order: list[str] = []

    async def run() -> None:
        def slow(name: str, real: float, virtual: float) -> str:
            time.sleep(real)  # real work: the clock must not move meanwhile
            if virtual:
                loop.thread_sleep(virtual)
            return name

        t0 = loop.time()
        timer = loop.create_future()
        loop.call_later(0.5, timer.set_result, None)
        # Submitted a, b (one worker: a then b); a's real work is slower than anything.
        fa = loop.run_in_executor(None, slow, "a", 0.05, 0.0)
        fb = loop.run_in_executor(None, slow, "b", 0.0, 2.0)
        for f in (fa, fb, timer):
            f.add_done_callback(lambda f: order.append(f.result() or "timer"))
        await loop.idle(advance=10)
        assert order == ["a", "timer", "b"], order
        assert loop.time() == t0 + 10

    try:
        loop.run_until_complete(run())
    finally:
        loop.finish()


def test_work_queued_behind_a_sleeping_thread_does_not_hold_the_clock() -> None:
    loop = SeededLoop(5)
    one = ThreadPoolExecutor(max_workers=1)  # as the client's state-file writer

    async def run() -> None:
        t0 = loop.time()
        a = loop.run_in_executor(one, loop.thread_sleep, 3.0)
        b = loop.run_in_executor(one, lambda: loop.time())
        await loop.idle(advance=1)
        assert not a.done() and not b.done(), "b waits behind a, asleep until 3 s"
        await loop.idle(advance=4)
        assert a.done() and b.done()
        assert b.result() == t0 + 3, "b ran when a woke"

    try:
        loop.run_until_complete(run())
    finally:
        loop.finish()
        one.shutdown()


def test_a_call_from_another_thread_is_recorded() -> None:
    loop = SeededLoop(3)

    async def run() -> None:
        t = threading.Thread(target=lambda: loop.call_soon(lambda: None))
        t.start()
        t.join()
        await loop.idle()

    try:
        loop.run_until_complete(run())
    finally:
        loop.finish()
    assert len(loop.wrong_thread) == 1 and "call_soon" in loop.wrong_thread[0], loop.wrong_thread


def test_a_seed_is_a_schedule() -> None:
    def trace(seed: int) -> list[str]:
        loop = SeededLoop(seed)
        seen: list[str] = []

        async def worker(name: str) -> None:
            for i in range(4):
                seen.append(f"{name}{i}")
                await asyncio.sleep(0)

        async def run() -> None:
            tasks = [loop.create_task(worker(n)) for n in "abcd"]
            await loop.idle()
            assert all(t.done() for t in tasks)

        try:
            loop.run_until_complete(run())
        finally:
            loop.finish()
        return seen

    assert trace(11) == trace(11)
    assert len({tuple(trace(s)) for s in range(10)}) > 1, "different seeds, different orders"


def test_idle_with_nothing_to_wait_for_is_stuck() -> None:
    loop = SeededLoop(1)
    try:
        with pytest.raises(Stuck):
            loop.run_until_complete(loop.create_future())
    finally:
        loop.close()

