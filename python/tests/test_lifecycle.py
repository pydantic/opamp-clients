# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The client's lifetime around what it starts: stop during a WebSocket handshake, a save that
takes long, calls from another thread. Each of these failed before the client carried out the
core's cancels, saved beside the loop and marshalled calls onto it."""

from __future__ import annotations

import asyncio
import threading
import time

from aiohttp import web

from opamp_client import Client, Config, Status, _executor, opamp_pb2 as pb
from opamp_client._core import Core

UID = bytes([0x42] * 16)


async def _serve(app: web.Application) -> tuple[web.AppRunner, int]:
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    return runner, site._server.sockets[0].getsockname()[1]  # type: ignore[union-attr]


def _answering(got: list[pb.AgentToServer]) -> web.Application:
    async def handle(req: web.Request) -> web.Response:
        msg = pb.AgentToServer.FromString(await req.read())
        got.append(msg)
        return web.Response(body=pb.ServerToAgent(instance_uid=msg.instance_uid).SerializeToString(),
                            content_type="application/x-protobuf")

    app = web.Application()
    app.router.add_post("/v1/opamp", handle)
    return app


# Stop while the WebSocket handshake is under way. The server holds the upgrade for 2 s, then
# accepts it: stop returns, and the connection is closed, not left reading.
# covers: RULE-CLIENT-16
def test_stop_during_a_websocket_handshake_returns_and_leaves_nothing_open() -> None:
    async def run() -> None:
        ended = asyncio.Event()

        async def handle(req: web.Request) -> web.WebSocketResponse:
            ws = web.WebSocketResponse()
            try:
                await asyncio.sleep(2)
                await ws.prepare(req)
                async for _ in ws:
                    pass
            finally:
                ended.set()
            return ws

        app = web.Application()
        app.router.add_get("/v1/opamp", handle)
        runner, port = await _serve(app)
        try:
            client = Client.start(Config(mode="python", endpoint=f"ws://127.0.0.1:{port}/v1/opamp", instance_uid=UID), Status())
            await asyncio.sleep(0.3)
            await asyncio.wait_for(client.stop(), 5)
            await asyncio.wait_for(ended.wait(), 5)
        finally:
            await runner.cleanup()

    asyncio.run(run())


# A save that takes 2 s holds up neither start, nor the heartbeats.
def test_a_slow_save_holds_up_neither_start_nor_heartbeats(tmp_path, monkeypatch) -> None:
    real = _executor._save

    def slow(files, path: str, data: bytes) -> None:
        time.sleep(2)
        real(files, path, data)

    monkeypatch.setattr(_executor, "_save", slow)

    async def run() -> None:
        got: list[pb.AgentToServer] = []
        runner, port = await _serve(_answering(got))
        try:
            t0 = time.monotonic()
            client = Client.start(Config(mode="python", endpoint=f"http://127.0.0.1:{port}/v1/opamp", heartbeat_interval_ms=100,
                                         state_file=str(tmp_path / "state")), Status())
            assert time.monotonic() - t0 < 1, "start waited for the save"
            await asyncio.sleep(1)
            assert len(got) >= 3, f"{len(got)} messages in 1 s while the save was writing"
            await asyncio.wait_for(client.stop(), 10)
        finally:
            await runner.cleanup()

    asyncio.run(run())


# A call from another thread reaches the core on the loop's thread, never on its own.
def test_calls_from_another_thread_step_the_core_on_the_loop(monkeypatch) -> None:
    threads: set[int] = set()
    real = Core.step

    def step(self, now_ms, input_):
        threads.add(threading.get_ident())
        return real(self, now_ms, input_)

    monkeypatch.setattr(Core, "step", step)

    async def run() -> None:
        client = Client.start(Config(mode="python", endpoint="http://127.0.0.1:1/v1/opamp", instance_uid=UID), Status())
        await asyncio.to_thread(client.set_health, pb.ComponentHealth(healthy=False))
        await asyncio.sleep(0.05)
        await asyncio.wait_for(client.stop(), 10)
        assert threads == {threading.get_ident()}

    asyncio.run(run())
