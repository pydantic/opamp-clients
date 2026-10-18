# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT

import asyncio

import pytest
from _servers import MODES
from aiohttp import WSMsgType, web

from pydantic_opamp_client import Client, Config, Notification, Status, anyvalue_pb2, opamp_pb2 as pb


def expected_sequence(got) -> list[int]:
    """The sequence numbers a run should have: 0 until the server has answered a message (those
    sent before its first answer: RULE-CLIENT-20), then 1, 2, ..."""
    first = next((i for i, m in enumerate(got) if m.sequence_num != 0), len(got))
    zeros = max(1, first) if first < len(got) else len(got)
    return [0 if i < zeros else i - zeros + 1 for i in range(len(got))]

UID = bytes([0x42] * 16)
UID_TEXT = "42424242-4242-4242-4242-424242424242"
DESCRIPTION = pb.AgentDescription(identifying_attributes=[
    anyvalue_pb2.KeyValue(key="service.name", value=anyvalue_pb2.AnyValue(string_value="test"))])


async def serve(app: web.Application) -> tuple[web.AppRunner, int]:
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    return runner, site._server.sockets[0].getsockname()[1]


async def wait_for(cond, seconds: float = 10) -> None:
    for _ in range(int(seconds * 100)):
        if cond():
            return
        await asyncio.sleep(0.01)


@pytest.mark.parametrize("mode", MODES)
def test_http_reports_then_says_goodbye_after_the_request_in_flight(mode: str) -> None:
    async def run() -> None:
        got: list[pb.AgentToServer] = []
        headers = []

        async def handle(req: web.Request) -> web.Response:
            headers.append(req.headers)
            msg = pb.AgentToServer.FromString(await req.read())
            got.append(msg)
            if len(got) == 2:  # hold a poll, so stop comes while it is in flight
                await asyncio.sleep(0.2)
            reply = pb.ServerToAgent(instance_uid=msg.instance_uid).SerializeToString()
            return web.Response(body=reply, content_type="application/x-protobuf")

        app = web.Application()
        app.router.add_post("/v1/opamp", handle)
        runner, port = await serve(app)
        notes: list[Notification] = []
        client = Client.start(Config(mode=mode, endpoint=f"http://127.0.0.1:{port}/v1/opamp", instance_uid=UID,
                                     heartbeat_interval_ms=50, headers={"Authorization": "Bearer t"}),
                              Status(description=DESCRIPTION), notes.append)
        await wait_for(lambda: len(got) >= 2)
        await client.stop()
        await runner.cleanup()

        assert got[0].HasField("agent_description"), "the first message is a full report"
        assert got[-1].HasField("agent_disconnect"), "the last message says goodbye"
        assert [m.sequence_num for m in got] == expected_sequence(got)
        for h in headers:
            assert h["Content-Type"] == "application/x-protobuf"
            assert h["OpAMP-Instance-UID"] == UID_TEXT
            assert h["Authorization"] == "Bearer t"
        assert notes[-1].kind == "stopped"

    asyncio.run(run())


@pytest.mark.parametrize("mode", MODES)
def test_websocket_answers_a_close_frame_reconnects_and_says_goodbye(mode: str) -> None:
    async def run() -> None:
        conns: list[list[pb.AgentToServer]] = []
        upgrades = []

        async def handle(req: web.Request) -> web.WebSocketResponse:
            upgrades.append(req.headers)
            ws = web.WebSocketResponse()
            await ws.prepare(req)
            msgs: list[pb.AgentToServer] = []
            conns.append(msgs)
            async for m in ws:
                if m.type != WSMsgType.BINARY:
                    continue
                assert m.data[0] == 0, "the WebSocket header"
                msg = pb.AgentToServer.FromString(m.data[1:])
                msgs.append(msg)
                await ws.send_bytes(b"\0" + pb.ServerToAgent(instance_uid=msg.instance_uid).SerializeToString())
                if len(conns) == 1:  # the server closes the first connection after its first message
                    await ws.close(code=1001)
            return ws

        app = web.Application()
        app.router.add_get("/v1/opamp", handle)
        runner, port = await serve(app)
        notes: list[Notification] = []
        client = Client.start(Config(mode=mode, endpoint=f"ws://127.0.0.1:{port}/v1/opamp", instance_uid=UID),
                              Status(description=DESCRIPTION), notes.append)
        await wait_for(lambda: len(conns) >= 2 and len(conns[1]) >= 1)
        await client.stop()
        await runner.cleanup()

        assert len(conns) == 2, "reconnected after the Close frame"
        for msgs in conns:
            assert msgs[0].HasField("agent_description"), "each connection starts with a full report"
        assert conns[1][-1].HasField("agent_disconnect"), "goodbye on stop"
        for h in upgrades:
            assert h["OpAMP-Instance-UID"] == UID_TEXT
        assert [n.kind for n in notes].count("connected") == 2
        assert notes[-1].kind == "stopped"

    asyncio.run(run())


# Found by the differential, in the Rust client (remote-config-restart-ws): after a restart the core
# hands the saved remote config to the application again, in the step that starts connecting, and
# reports it APPLYING until the application answers. What the callback asks before it returns
# reaches the core before anything the client takes in next: here the call is fed by the drain that
# is running the callback, before the loop can run anything else (the connection's opening
# included), so the first message says APPLIED, and none says APPLYING.
# covers: RULE-SHELL-10
@pytest.mark.parametrize("mode", MODES)
def test_a_config_answered_in_the_callback_at_a_restart_is_never_reported_applying(mode: str, tmp_path) -> None:
    applying, applied = pb.RemoteConfigStatuses_APPLYING, pb.RemoteConfigStatuses_APPLIED
    offer = pb.AgentRemoteConfig(
        config_hash=b"c1", config=pb.AgentConfigMap(config_map={"": pb.AgentConfigObject(body=b"x")}))

    def statuses(got: list[pb.AgentToServer]) -> list[int]:
        return [m.remote_config_status.status for m in got
                if m.HasField("remote_config_status") and m.remote_config_status.last_remote_config_hash == b"c1"]

    async def run() -> None:
        got: list[pb.AgentToServer] = []
        offers = [offer]

        async def handle(req: web.Request) -> web.WebSocketResponse:
            ws = web.WebSocketResponse()
            await ws.prepare(req)
            async for m in ws:
                if m.type != WSMsgType.BINARY:
                    continue
                msg = pb.AgentToServer.FromString(m.data[1:])
                got.append(msg)
                reply = pb.ServerToAgent(instance_uid=msg.instance_uid)
                if offers:
                    reply.remote_config.CopyFrom(offers.pop())
                await ws.send_bytes(b"\0" + reply.SerializeToString())
            return ws

        app = web.Application()
        app.router.add_get("/v1/opamp", handle)
        # (One server for both lives: the core uses a saved config only on the endpoint it came from.)
        runner, port = await serve(app)

        async def life() -> list[pb.AgentToServer]:
            got.clear()
            clients: list[Client] = []

            def notify(n: Notification) -> None:
                # Answered before the callback returns. (Never before start has returned: the client is there.)
                if n.kind == "remote_config":
                    clients[0].set_remote_config_status(
                        pb.RemoteConfigStatus(last_remote_config_hash=n.remote_config.config_hash, status=applied))

            clients.append(Client.start(
                Config(mode=mode, endpoint=f"ws://127.0.0.1:{port}/v1/opamp", instance_uid=UID,
                       state_file=str(tmp_path / "state"), capabilities=pb.AgentCapabilities_AcceptsRemoteConfig),
                Status(description=DESCRIPTION), notify))
            await wait_for(lambda: applied in statuses(got))
            await clients[0].stop()
            return list(got)

        # The first life: the server offers the config, the application applies it, and it is
        # saved. (Offered by the server, a config is APPLYING in the message that goes before the
        # application is told.)
        assert statuses(await life()) == [applying, applied], "the first life"
        # The second, on what the first saved.
        second = await life()
        await runner.cleanup()
        assert second[0].remote_config_status.last_remote_config_hash == b"c1", "the first message reports on the saved config"
        assert second[0].remote_config_status.status == applied, "the first message after the restart"
        assert applying not in statuses(second), "APPLYING reported after the restart"

    asyncio.run(run())


def test_a_request_that_cant_be_made_at_all_is_a_failed_request(monkeypatch) -> None:
    # At interpreter exit the resolver's thread pool takes no more work: aiohttp raises RuntimeError
    # rather than a ClientError. The core hears the request failed, and stop still returns.
    import aiohttp

    def post(*args, **kwargs):
        raise RuntimeError("cannot schedule new futures after interpreter shutdown")

    monkeypatch.setattr(aiohttp.ClientSession, "post", post)

    async def run() -> None:
        unhandled = []
        asyncio.get_running_loop().set_exception_handler(lambda _loop, ctx: unhandled.append(ctx))
        notes: list[Notification] = []
        client = Client.start(Config(mode="platform", endpoint="http://opamp.invalid/v1/opamp", instance_uid=UID),
                              Status(description=DESCRIPTION), notes.append)
        await wait_for(lambda: any(n.kind == "connect_failed" for n in notes), 5)
        await asyncio.wait_for(client.stop(), 5)
        assert [n.detail for n in notes if n.kind == "connect_failed"][0].startswith("cannot schedule")
        assert unhandled == []

    asyncio.run(run())


# covers: OPAMP-SIZE-02, OPAMP-SIZE-08
@pytest.mark.parametrize("mode", MODES)
def test_max_send_bytes_reaches_the_core(mode: str) -> None:
    async def run() -> None:
        got: list[pb.AgentToServer] = []

        async def handle(req: web.Request) -> web.Response:
            msg = pb.AgentToServer.FromString(await req.read())
            got.append(msg)
            reply = pb.ServerToAgent(instance_uid=msg.instance_uid).SerializeToString()
            return web.Response(body=reply, content_type="application/x-protobuf")

        app = web.Application()
        app.router.add_post("/v1/opamp", handle)
        runner, port = await serve(app)
        big = pb.AgentDescription(identifying_attributes=[
            anyvalue_pb2.KeyValue(key="big", value=anyvalue_pb2.AnyValue(string_value="x" * 4096))])
        notes: list[Notification] = []
        client = Client.start(Config(mode=mode, endpoint=f"http://127.0.0.1:{port}/v1/opamp", instance_uid=UID,
                                     max_send_bytes=1024),
                              Status(description=big), notes.append)
        await wait_for(lambda: len(got) >= 1)
        await client.stop()
        await runner.cleanup()

        assert not got[0].HasField("agent_description"), "a description too large is left out"
        # The field's name is the core's (StatusTooLarge.name): the client keeps no table of its own.
        assert any(n.kind == "status_too_large" and n.field == "agent_description" for n in notes), notes

    asyncio.run(run())
