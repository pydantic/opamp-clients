# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""With no UID of the application's the core makes one (a UUIDv7) and the client says which;
request_instance_uid asks the server for one."""

from __future__ import annotations

import asyncio
import time
import uuid

import pytest
from _servers import MODES
from aiohttp import web

from pydantic_opamp_client import Client, Config, Notification, Status, opamp_pb2 as pb


def test_with_no_uid_chosen_the_core_makes_one_and_the_client_says_which(tmp_path):
    async def run() -> None:
        uids = []
        for _ in range(2):
            client = Client.start(Config(mode="platform", endpoint="http://127.0.0.1:1/v1/opamp"), Status())
            uids.append(client.instance_uid)
            await client.stop()
        a, b = uids
        assert a != b
        for u in (a, b):
            parsed = uuid.UUID(bytes=u)
            assert parsed.version == 7
            assert parsed.variant == uuid.RFC_4122
            assert abs(time.time_ns() // 1_000_000 - int.from_bytes(u[:6], "big")) < 5_000

        # The application's own, as given; and with a state file, the one made is kept.
        chosen = bytes([9] * 16)
        client = Client.start(Config(mode="platform", endpoint="http://127.0.0.1:1/v1/opamp", instance_uid=chosen), Status())
        assert client.instance_uid == chosen
        await client.stop()
        state = str(tmp_path / "state")
        first = Client.start(Config(mode="platform", endpoint="http://127.0.0.1:1/v1/opamp", state_file=state), Status())
        await asyncio.sleep(0.05)  # the first save
        await first.stop()
        again = Client.start(Config(mode="platform", endpoint="http://127.0.0.1:1/v1/opamp", state_file=state), Status())
        assert again.instance_uid == first.instance_uid
        await again.stop()

    asyncio.run(run())


# request_instance_uid reaches the core: the client asks until the server assigns a UID, then
# uses it; without the option it never asks.
# covers: OPAMP-UID-04
@pytest.mark.parametrize("mode", MODES)
@pytest.mark.parametrize("on", [True, False])
def test_request_instance_uid_asks_the_server(on: bool, mode: str) -> None:
    async def run() -> None:
        assigned = bytes([0x77] * 16)
        got: list[pb.AgentToServer] = []

        async def handle(req: web.Request) -> web.Response:
            msg = pb.AgentToServer.FromString(await req.read())
            got.append(msg)
            reply = pb.ServerToAgent(instance_uid=msg.instance_uid)
            if msg.flags & pb.AgentToServerFlags_RequestInstanceUid:
                reply.agent_identification.new_instance_uid = assigned
            return web.Response(body=reply.SerializeToString(), content_type="application/x-protobuf")

        app = web.Application()
        app.router.add_post("/v1/opamp", handle)
        runner = web.AppRunner(app)
        await runner.setup()
        site = web.TCPSite(runner, "127.0.0.1", 0)
        await site.start()
        port = site._server.sockets[0].getsockname()[1]
        notes: list[Notification] = []
        client = Client.start(Config(mode=mode, endpoint=f"http://127.0.0.1:{port}/v1/opamp", heartbeat_interval_ms=50,
                                     request_instance_uid=on), Status(), notes.append)
        for _ in range(1000):
            if len(got) >= 3:
                break
            await asyncio.sleep(0.01)
        await client.stop()
        await runner.cleanup()

        assert bool(got[0].flags & 1) == on, "the first message asks for a UID"
        assert all(m.flags & 1 == 0 for m in got[1:]), "later messages don't ask"
        if on:
            assert got[-1].instance_uid == assigned, "the assigned UID is used"
            assert any(n.kind == "instance_uid_changed" for n in notes), "the application hears of it"
        else:
            assert got[-1].instance_uid != assigned

    asyncio.run(run())
