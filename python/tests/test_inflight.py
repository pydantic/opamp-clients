# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""InFlight's invariants over seeded random interleavings of starts, results, late connections,
cancels and close: a result after its cancel is dropped, an effect's last word is delivered once, a
connection ready after its cancel is dropped at once, nothing starts after close, and close is
bounded even by work that won't end."""

from __future__ import annotations

import asyncio
import random
import time

import pytest

from opamp_client._executor import InFlight
from opamp_client._proto.opamp.core.v1 import core_pb2 as corepb

IDS = range(1, 6)


class FakeConn:
    def __init__(self) -> None:
        self.dropped = False

    def drop(self) -> None:
        self.dropped = True


async def _explore(seed: int) -> None:
    rng = random.Random(seed)
    log: list[tuple[str, int]] = []
    inflight = InFlight(lambda i: log.append(("deliver", i.http_failed.id)))
    gates: dict[int, asyncio.Future] = {}
    conns: dict[int, FakeConn] = {}
    began: set[int] = set()
    cancelled: set[int] = set()

    async def work(id_: int) -> None:
        began.add(id_)
        await gates[id_]
        inflight.finish(id_, corepb.Input(http_failed=corepb.Failed(id=id_)))

    for _ in range(rng.randrange(5, 40)):
        op, id_ = rng.choice(["start", "result", "cancel", "adopt", "tick", "tick"]), rng.choice(IDS)
        if op == "start" and id_ not in gates:
            gates[id_] = asyncio.get_running_loop().create_future()
            inflight.start(id_, work(id_))
        elif op == "result" and id_ in gates and not gates[id_].done():
            gates[id_].set_result(None)
        elif op == "cancel" and id_ in gates:  # the core cancels only what it asked for
            inflight.cancel(id_)
            cancelled.add(id_)
            log.append(("cancel", id_))
        elif op == "adopt" and id_ in gates and id_ not in conns:
            # A connection that opens now: kept only if its effect is still under way.
            conn = conns[id_] = FakeConn()
            under_way = inflight.adopt(id_, conn, asyncio.sleep(3600))
            assert conn.dropped == (not under_way), (seed, id_)
        elif op == "tick":
            await asyncio.sleep(0)

    t0 = time.monotonic()
    await inflight.close(0.5)
    assert time.monotonic() - t0 < 1, seed

    # Nothing starts after close.
    gates[99] = asyncio.get_running_loop().create_future()
    inflight.start(99, work(99))
    await asyncio.sleep(0)
    assert 99 not in began, seed
    # Every connection is dropped by now.
    assert all(c.dropped for c in conns.values()), seed

    delivered = [i for kind, i in log if kind == "deliver"]
    assert len(delivered) == len(set(delivered)), (seed, log)  # once
    for n, (kind, i) in enumerate(log):
        if kind == "cancel":
            assert ("deliver", i) not in log[n:], (seed, log)  # nothing after its cancel
    # A result that came and wasn't cancelled first was delivered.
    for i in delivered:
        assert gates[i].done()


@pytest.mark.parametrize("seed", range(300))
def test_inflight_invariants_over_seeded_interleavings(seed: int) -> None:
    asyncio.run(_explore(seed))


def test_close_is_bounded_by_work_that_wont_end() -> None:
    async def run() -> None:
        inflight = InFlight(lambda _i: None)
        let_go = False

        async def stubborn() -> None:
            while not let_go:
                try:
                    await asyncio.sleep(3600)
                except asyncio.CancelledError:
                    pass

        inflight.start(1, stubborn())
        await asyncio.sleep(0)
        (task,) = [t for t in asyncio.all_tasks() if t is not asyncio.current_task()]
        t0 = time.monotonic()
        await inflight.close(0.2)
        assert 0.15 < time.monotonic() - t0 < 1
        let_go = True
        task.cancel()
        await task

    asyncio.run(run())
