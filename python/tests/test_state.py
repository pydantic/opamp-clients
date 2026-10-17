# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The state file: saved atomically and privately, and every Store answered."""

from __future__ import annotations

import asyncio
import os
import stat
import threading
import time

import pytest

from opamp_client import Client, Config, Status
from opamp_client import _executor as executor_module
from opamp_client._executor import Executor, read_state, save_state
from opamp_client._proto.opamp.core.v1 import core_pb2 as corepb


def test_saves_replace_what_was_saved_privately(tmp_path):
    path = str(tmp_path / "state.bin")
    save_state(path, b"one")
    save_state(path, b"two")
    with open(path, "rb") as f:
        assert f.read() == b"two"
    assert not os.path.exists(path + ".tmp")
    if os.name != "nt":
        assert stat.S_IMODE(os.stat(path).st_mode) == 0o600


def test_a_leftover_temporary_file_is_replaced_not_reused(tmp_path):
    path = str(tmp_path / "state.bin")
    with open(path + ".tmp", "wb") as f:
        f.write(b"stale, readable by all")
    os.chmod(path + ".tmp", 0o644)
    save_state(path, b"new")
    with open(path, "rb") as f:
        assert f.read() == b"new"
    if os.name != "nt":
        assert stat.S_IMODE(os.stat(path).st_mode) == 0o600


def test_a_store_is_answered(tmp_path):
    async def run() -> None:
        got: list[corepb.Input] = []
        store = lambda id_: corepb.Effect(store=corepb.Store(id=id_, data=b"x"))  # noqa: E731
        for id_, path in ((7, str(tmp_path / "state.bin")), (8, str(tmp_path / "missing" / "state.bin")), (9, None)):
            x = Executor(got.append, path)
            x.do(store(id_))
            for _ in range(500):
                if len(got) == id_ - 6:
                    break
                await asyncio.sleep(0.01)
            await x.close()
        assert [(i.WhichOneof("kind"), getattr(i, i.WhichOneof("kind")).id) for i in got] == [
            ("stored", 7), ("store_failed", 8), ("store_failed", 9)]
        assert "state.bin" in got[1].store_failed.detail

    asyncio.run(run())


def test_saves_land_in_order_beside_the_loop_and_a_cancelled_one_is_never_reported(tmp_path, monkeypatch):
    """One save at a time, in the order asked: a cancelled one still writing lands whole before
    the next, and one not yet started never lands. Reading the state waits for them."""
    real = executor_module._save
    landed: list[bytes] = []

    def slow(files, path: str, data: bytes) -> None:
        time.sleep(0.3)
        real(files, path, data)
        landed.append(data)

    monkeypatch.setattr(executor_module, "_save", slow)
    path = str(tmp_path / "state.bin")

    async def run() -> None:
        got: list[corepb.Input] = []
        x = Executor(got.append, path)
        x.do(corepb.Effect(store=corepb.Store(id=1, data=b"one")))
        await asyncio.sleep(0.1)  # writing
        x.do(corepb.Effect(cancel=corepb.Id(id=1)))
        x.do(corepb.Effect(store=corepb.Store(id=2, data=b"two")))
        x.do(corepb.Effect(cancel=corepb.Id(id=2)))  # not started: never written
        x.do(corepb.Effect(store=corepb.Store(id=3, data=b"three")))
        t0 = time.monotonic()
        await asyncio.sleep(0.05)
        assert time.monotonic() - t0 < 0.2, "the loop was held up"
        # Another client starting on the file reads it once the saves before have landed.
        assert await asyncio.to_thread(read_state, path) == b"three"
        await asyncio.sleep(0.05)
        await x.close()
        assert landed == [b"one", b"three"]
        assert [(i.WhichOneof("kind"), getattr(i, i.WhichOneof("kind")).id) for i in got] == [("stored", 3)]

    asyncio.run(run())


@pytest.mark.parametrize("mode", ["python", "rust"])
def test_an_unreadable_state_file_stops_the_client_starting(mode, tmp_path):
    async def run() -> None:
        with pytest.raises(OSError):
            Client.start(Config(mode=mode, endpoint="http://127.0.0.1:1", instance_uid=bytes(16), state_file=str(tmp_path)),
                         Status())

    asyncio.run(run())


@pytest.mark.parametrize("mode", ["python", "rust"])
def test_clients_on_many_state_files_leave_no_saving_threads_behind(mode, tmp_path):
    """A thread saves to a state file while there is work for it, then ends: a process that runs
    clients on one state file after another (the callback model test runs thousands) keeps no
    thread per file it ever used. (One thread per file, kept for good, made the nightly callback
    model's thousands of runs lose their runner.)"""
    def savers() -> int:
        return sum(t.name.startswith("opamp-save") for t in threading.enumerate())

    before = savers()

    async def run() -> None:
        for i in range(20):
            client = Client.start(Config(mode=mode, endpoint="http://127.0.0.1:1", instance_uid=bytes(16),
                                         state_file=str(tmp_path / f"state{i}")), Status())
            await asyncio.wait_for(client.stop(), 10)

    asyncio.run(run())
    for _ in range(200):
        if savers() <= before:
            break
        time.sleep(0.01)
    assert savers() <= before, f"{savers() - before} saving threads left behind by 20 clients"
