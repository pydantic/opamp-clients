# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""An effect this client doesn't know stops it, and stop() says why."""

from __future__ import annotations

import asyncio

import pytest

from pydantic_opamp_client import Client, Config, Notification, Status
from pydantic_opamp_client._core import Core
from pydantic_opamp_client._proto.opamp.core.v1 import core_pb2 as corepb


def test_an_unknown_effect_stops_the_client(monkeypatch):
    # A newer core: its effect has a kind this client has no case for.
    monkeypatch.setattr(Core, "step", lambda self, now, input_: corepb.Output(effects=[corepb.Effect()]))

    async def run() -> None:
        notes: list[Notification] = []
        client = Client.start(Config(mode="platform", endpoint="http://127.0.0.1:1", instance_uid=bytes([1] * 16)), Status(),
                              notes.append)
        # It failed while starting: the application hears once start has returned, without stop().
        assert notes == []
        await asyncio.sleep(0)
        assert [n.kind for n in notes] == ["stopped"]
        assert "older than its core" in str(notes[0].error)
        with pytest.raises(RuntimeError, match="older than its core"):
            await client.stop()

    asyncio.run(run())


def test_a_callback_that_throws_stops_the_client_which_says_so_once():
    async def run() -> None:
        notes: list[Notification] = []

        def notify(n: Notification) -> None:
            notes.append(n)
            raise ValueError("the application's bug")

        client = Client.start(Config(mode="platform", endpoint="http://127.0.0.1:1", instance_uid=bytes([1] * 16)), Status(),
                              notify)
        # The refused connection is told, the callback raises, and Stopped follows (raised at too).
        for _ in range(500):
            if any(n.kind == "stopped" for n in notes):
                break
            await asyncio.sleep(0.01)
        stopped = [n for n in notes if n.kind == "stopped"]
        assert len(stopped) == 1 and "the application's bug" in str(stopped[0].error)
        with pytest.raises(ValueError, match="the application's bug"):
            await client.stop()

    asyncio.run(run())


def test_a_stalled_save_is_told_as_the_core_says():
    # The core tells of a save that hangs, and of its end: the application hears both, as they are.
    from pydantic_opamp_client._client import _notification

    def told(stalled: bool, waited_ms: int) -> Notification | None:
        n = corepb.Notification(save_stalled=corepb.SaveStalled(stalled=stalled, waited_ms=waited_ms))
        return _notification(n, lambda _: "")

    assert told(True, 30_000) == Notification("save_stalled", stalled=True, waited_ms=30_000)
    assert told(False, 2**64 - 1) == Notification("save_stalled", stalled=False, waited_ms=2**64 - 1)
