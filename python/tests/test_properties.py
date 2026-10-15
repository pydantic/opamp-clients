# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Properties of the client around the core, over random plans (Hypothesis): the application's
calls, some made from inside the notification callback (so they reach the client while it is
still carrying out the step that called back), others from the event loop, over either
transport. Whatever the plan: every custom message reaches the server once and in order, the
server ends with the last description, sequence numbers run without a gap, and the goodbye is
the last message."""

import asyncio
import os

import pytest
from _servers import MODES
from aiohttp import WSMsgType, web
from hypothesis import given, settings, strategies as st

from opamp_client import Client, Config, Notification, Status, anyvalue_pb2, opamp_pb2 as pb


def expected_sequence(got) -> list[int]:
    """The sequence numbers a run should have: 0 until the server has answered a message (those
    sent before its first answer: RULE-CLIENT-20), then 1, 2, ..."""
    first = next((i for i, m in enumerate(got) if m.sequence_num != 0), len(got))
    zeros = max(1, first) if first < len(got) else len(got)
    return [0 if i < zeros else i - zeros + 1 for i in range(len(got))]

UID = bytes([0x42] * 16)
CAPABILITY = "io.example.tick"


def description(n: int) -> pb.AgentDescription:
    return pb.AgentDescription(non_identifying_attributes=[
        anyvalue_pb2.KeyValue(key="n", value=anyvalue_pb2.AnyValue(int_value=n))])


def described(msg: pb.AgentToServer) -> int | None:
    if not msg.HasField("agent_description"):
        return None
    return msg.agent_description.non_identifying_attributes[0].value.int_value


async def serve(app: web.Application) -> tuple[web.AppRunner, int]:
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    return runner, site._server.sockets[0].getsockname()[1]


# A step of the plan: a new description or a custom message, called from the callback or not.
steps = st.lists(st.tuples(st.sampled_from(["describe", "send"]), st.booleans()), min_size=1, max_size=12)


@pytest.mark.parametrize("mode", MODES)
@settings(max_examples=int(os.environ.get("HYPOTHESIS_EXAMPLES", "25")), deadline=None)
@given(plan=steps, ws=st.booleans())
def test_calls_from_anywhere_arrive_once_and_in_order(plan: list[tuple[str, bool]], ws: bool, mode: str) -> None:
    async def run() -> None:
        got: list[pb.AgentToServer] = []

        def reply(msg: pb.AgentToServer) -> bytes:
            got.append(msg)
            # Every answer carries a custom message, so the application is called back often.
            tick = pb.CustomMessage(capability=CAPABILITY, type="tick", data=b"")
            return pb.ServerToAgent(instance_uid=msg.instance_uid, custom_message=tick).SerializeToString()

        async def http(req: web.Request) -> web.Response:
            body = reply(pb.AgentToServer.FromString(await req.read()))
            return web.Response(body=body, content_type="application/x-protobuf")

        async def websocket(req: web.Request) -> web.WebSocketResponse:
            conn = web.WebSocketResponse()
            await conn.prepare(req)
            async for m in conn:
                if m.type == WSMsgType.BINARY:
                    await conn.send_bytes(b"\0" + reply(pb.AgentToServer.FromString(m.data[1:])))
            return conn

        app = web.Application()
        app.router.add_post("/v1/opamp", http)
        app.router.add_get("/v1/opamp", websocket)
        runner, port = await serve(app)

        done = 0  # the steps of the plan carried out
        sent: list[int] = []
        last_description = 0
        errors: list[BaseException] = []
        client: Client | None = None

        def carry_out() -> None:
            nonlocal done, last_description
            what, _ = plan[done]
            done += 1
            if what == "describe":
                last_description = done
                client.set_description(description(done))
            else:
                sent.append(done)
                client.send_custom_message(pb.CustomMessage(capability=CAPABILITY, type="t", data=str(done).encode()))

        def notify(n: Notification) -> None:
            # The steps due from the callback, called from inside it.
            if n.kind == "stopped" and n.error is not None:
                errors.append(n.error)
            try:
                while client is not None and done < len(plan) and plan[done][1]:
                    carry_out()
            except BaseException as e:  # noqa: BLE001
                errors.append(e)

        scheme = "ws" if ws else "http"
        client = Client.start(Config(mode=mode, endpoint=f"{scheme}://127.0.0.1:{port}/v1/opamp", instance_uid=UID,
                                     heartbeat_interval_ms=20),
                              Status(description=description(0),
                                     custom_capabilities=pb.CustomCapabilities(capabilities=[CAPABILITY])),
                              notify)
        for _ in range(2000):
            if done == len(plan):
                break
            if not plan[done][1]:
                carry_out()
            await asyncio.sleep(0.002)
        assert done == len(plan), "the plan stalled"

        def delivered() -> list[int]:
            return [int(m.custom_message.data) for m in got if m.HasField("custom_message")
                    and m.custom_message.type == "t"]

        descriptions = lambda: [d for m in got if (d := described(m)) is not None]  # noqa: E731
        for _ in range(1000):
            if delivered() == sent and descriptions() and descriptions()[-1] == last_description:
                break
            await asyncio.sleep(0.005)
        await client.stop()
        await runner.cleanup()

        assert not errors, errors
        assert delivered() == sent, f"custom messages {delivered()}, sent {sent}"
        assert descriptions()[-1] == last_description, "the server has the last description"
        assert [m.sequence_num for m in got] == expected_sequence(got), "0 until the first answer, then no gap"
        assert got[-1].HasField("agent_disconnect"), "the goodbye is last"

    asyncio.run(run())
