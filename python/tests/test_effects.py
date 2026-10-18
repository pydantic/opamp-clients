# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Effects end when the core says: each kind of cancel drops what it names at once and nothing
more about it is reported; a close is reported when its handshake is done; stop returns when the
core says it has stopped."""

from __future__ import annotations

import asyncio
import struct
import time

from aiohttp import WSMsgType, web

from pydantic_opamp_client import Client, Config, Status
from pydantic_opamp_client._core import Core
from pydantic_opamp_client._executor import Executor
from pydantic_opamp_client._proto.opamp.core.v1 import core_pb2 as corepb

UID = bytes([0x42] * 16)


class Silent:
    """Accepts connections and reads what comes, answering nothing; counts the connections the
    client has closed."""

    def __init__(self) -> None:
        self.accepted = 0
        self.closed = 0
        self.port = 0
        self._server: asyncio.Server | None = None

    async def start(self) -> Silent:
        self._server = await asyncio.start_server(self._serve, "127.0.0.1", 0)
        self.port = self._server.sockets[0].getsockname()[1]
        return self

    async def _serve(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        self.accepted += 1
        try:
            while await reader.read(65536):
                pass
        except ConnectionError:
            pass
        self.closed += 1
        writer.close()

    async def close(self) -> None:
        assert self._server is not None
        self._server.close()


async def until(cond, seconds: float = 5) -> bool:
    for _ in range(int(seconds * 100)):
        if cond():
            return True
        await asyncio.sleep(0.01)
    return cond()


class WsServer:
    """A WebSocket server. `answer_close`: answer the client's Close frame (else it is read and
    ignored). `close_first`: send a Close frame (1001) once open. Counts connections ended."""

    def __init__(self, answer_close: bool = True, close_first: bool = False) -> None:
        self.answer_close, self.close_first = answer_close, close_first
        self.ended = 0
        self.got_close = asyncio.Event()
        self.port = 0
        self._runner: web.AppRunner | None = None

    async def _ws(self, req: web.Request) -> web.WebSocketResponse:
        ws = web.WebSocketResponse(autoclose=self.answer_close)
        await ws.prepare(req)
        try:
            if self.close_first:
                await ws.close(code=1001)
            while True:
                m = await ws.receive()
                if m.type == WSMsgType.CLOSE and not self.answer_close:
                    self.got_close.set()
                    # Never answered (receiving again would answer it): wait until the client
                    # drops the connection.
                    while req.transport is not None and not req.transport.is_closing():
                        await asyncio.sleep(0.01)
                    break
                if m.type in (WSMsgType.CLOSE, WSMsgType.CLOSING, WSMsgType.CLOSED, WSMsgType.ERROR):
                    break
        finally:
            self.ended += 1
        return ws

    async def start(self) -> WsServer:
        app = web.Application()
        app.router.add_get("/v1/opamp", self._ws)
        self._runner = web.AppRunner(app, shutdown_timeout=0.1)
        await self._runner.setup()
        site = web.TCPSite(self._runner, "127.0.0.1", 0)
        await site.start()
        self.port = site._server.sockets[0].getsockname()[1]  # type: ignore[union-attr]
        return self

    async def close(self) -> None:
        assert self._runner is not None
        await self._runner.cleanup()


def _kinds(got: list[corepb.Input]) -> list[str]:
    return [i.WhichOneof("kind") for i in got]


def test_a_cancelled_request_is_aborted_and_never_reported() -> None:
    async def run() -> None:
        server = await Silent().start()
        got: list[corepb.Input] = []
        x = Executor(got.append)
        try:
            x.do(corepb.Effect(http_request=corepb.HttpRequest(
                id=1, url=f"http://127.0.0.1:{server.port}/v1/opamp", body=b"x", timeout_ms=30_000,
                max_response_bytes=1 << 20)))
            assert await until(lambda: server.accepted == 1)
            x.do(corepb.Effect(cancel=corepb.Id(id=1)))
            assert await until(lambda: server.closed == 1), "the request is aborted"
            await asyncio.sleep(0.1)
            assert got == []
        finally:
            await x.close()
            await server.close()

    asyncio.run(run())


def test_a_cancelled_connecting_is_aborted_and_never_reported() -> None:
    async def run() -> None:
        server = await Silent().start()  # takes the upgrade and never answers it
        got: list[corepb.Input] = []
        x = Executor(got.append)
        try:
            x.do(corepb.Effect(ws_open=corepb.WsOpen(id=1, url=f"ws://127.0.0.1:{server.port}/v1/opamp",
                                                     max_message_bytes=1 << 20)))
            assert await until(lambda: server.accepted == 1)
            x.do(corepb.Effect(cancel=corepb.Id(id=1)))
            assert await until(lambda: server.closed == 1), "the handshake is aborted"
            await asyncio.sleep(0.1)
            assert got == []
        finally:
            await x.close()
            await server.close()

    asyncio.run(run())


def _open(x: Executor, port: int, id_: int = 1) -> None:
    x.do(corepb.Effect(ws_open=corepb.WsOpen(id=id_, url=f"ws://127.0.0.1:{port}/v1/opamp", max_message_bytes=1 << 20)))


def test_a_cancelled_open_connection_is_dropped_and_never_reported() -> None:
    async def run() -> None:
        server = await WsServer().start()
        got: list[corepb.Input] = []
        x = Executor(got.append)
        try:
            _open(x, server.port)
            assert await until(lambda: _kinds(got) == ["ws_opened"])
            x.do(corepb.Effect(cancel=corepb.Id(id=1)))
            assert await until(lambda: server.ended == 1), "the connection is dropped"
            await asyncio.sleep(0.1)
            assert _kinds(got) == ["ws_opened"], "nothing after the cancel, not even ws_lost"
        finally:
            await x.close()
            await server.close()

    asyncio.run(run())


def test_a_cancelled_closing_connection_is_dropped_and_never_reported() -> None:
    async def run() -> None:
        server = await WsServer(answer_close=False).start()
        got: list[corepb.Input] = []
        x = Executor(got.append)
        try:
            _open(x, server.port)
            assert await until(lambda: _kinds(got) == ["ws_opened"])
            x.do(corepb.Effect(ws_close=corepb.Id(id=1)))
            await asyncio.wait_for(server.got_close.wait(), 5)
            await asyncio.sleep(0.2)
            assert _kinds(got) == ["ws_opened"], "the handshake waits, with no timeout of the client's own"
            x.do(corepb.Effect(cancel=corepb.Id(id=1)))
            assert await until(lambda: server.ended == 1), "the connection is dropped"
            await asyncio.sleep(0.1)
            assert _kinds(got) == ["ws_opened"]
        finally:
            await x.close()
            await server.close()

    asyncio.run(run())


def test_ws_close_is_reported_when_its_handshake_is_done() -> None:
    async def run() -> None:
        server = await WsServer().start()
        got: list[corepb.Input] = []
        x = Executor(got.append)
        try:
            _open(x, server.port)
            assert await until(lambda: _kinds(got) == ["ws_opened"])
            x.do(corepb.Effect(ws_send=corepb.WsMessage(id=1, data=b"\0")))
            x.do(corepb.Effect(ws_close=corepb.Id(id=1)))
            assert await until(lambda: len(got) == 2)
            assert got[1].ws_closed == corepb.WsClosed(id=1, code=1000)
            await asyncio.sleep(0.1)
            assert len(got) == 2
        finally:
            await x.close()
            await server.close()

    asyncio.run(run())


def test_the_servers_close_is_reported_then_answered_when_the_core_says() -> None:
    async def run() -> None:
        server = await WsServer(close_first=True).start()
        got: list[corepb.Input] = []
        x = Executor(got.append)
        try:
            _open(x, server.port)
            assert await until(lambda: len(got) == 2)
            assert got[1].ws_closed == corepb.WsClosed(id=1, code=1001)
            x.do(corepb.Effect(ws_close=corepb.Id(id=1)))
            assert await until(lambda: len(got) == 3)
            assert got[2].WhichOneof("kind") == "ws_closed" and got[2].ws_closed.id == 1
            assert await until(lambda: server.ended == 1)
        finally:
            await x.close()
            await server.close()

    asyncio.run(run())


def test_a_result_queued_before_its_cancel_never_reaches_the_core(tmp_path, monkeypatch) -> None:
    """A save is answered at once; if the core cancels it before the answer is fed, the answer is
    dropped: nothing about a cancelled id reaches the core. The file is saved whole all the same."""
    seen: list[corepb.Input] = []

    def step(self, now_ms, input_):
        seen.append(input_)
        if input_.WhichOneof("kind") == "stop":
            return corepb.Output(stopped=True)
        if input_.WhichOneof("kind") == "start":
            return corepb.Output(effects=[corepb.Effect(store=corepb.Store(id=5, data=b"saved"))])
        if input_.WhichOneof("kind") == "timer":
            return corepb.Output(effects=[corepb.Effect(cancel=corepb.Id(id=5))])
        return corepb.Output()

    monkeypatch.setattr(Core, "step", step)
    state = tmp_path / "state"

    async def run() -> None:
        client = Client.start(Config(mode="platform", endpoint="http://127.0.0.1:1/v1/opamp", instance_uid=UID, state_file=str(state)),
                              Status())
        await until(lambda: state.exists())
        await asyncio.sleep(0.05)
        # The answer is queued (a step is under way) when the cancel comes.
        client._draining = True
        client._queue.append(corepb.Input(stored=corepb.Id(id=5)))
        client._draining = False
        client._feed(corepb.Input(timer=corepb.Empty()))
        await client.stop()

    asyncio.run(run())
    assert state.read_bytes() == b"saved"
    kinds = [i.WhichOneof("kind") for i in seen]
    assert "timer" in kinds and "stored" not in kinds[kinds.index("timer"):], kinds


def _tap_records(path) -> list[tuple[int, bytes]]:
    data, out = path.read_bytes(), []
    while data:
        kind, n, _ = struct.unpack_from("<BIQ", data)
        out.append((kind, data[13:13 + n]))
        data = data[13 + n:]
    return out


# covers: RULE-CLIENT-16
def test_stop_returns_when_the_core_says_and_steps_no_more(tmp_path, monkeypatch) -> None:
    """A request the server never answers: stop keeps stepping timers until the core, at its
    stop timeout, cancels it and says it has stopped; the request is aborted, and the core is
    stepped no more."""
    tap = tmp_path / "tap"
    monkeypatch.setenv("OPAMP_CORE_TAP", str(tap))
    monkeypatch.setattr("pydantic_opamp_client._core._taps", 0)

    async def run() -> None:
        server = await Silent().start()
        client = Client.start(Config(mode="platform", endpoint=f"http://127.0.0.1:{server.port}/v1/opamp", instance_uid=UID,
                                     stop_timeout_ms=300), Status())
        assert await until(lambda: server.accepted == 1)
        t0 = time.monotonic()
        await asyncio.wait_for(client.stop(), 5)
        took = time.monotonic() - t0
        assert 0.25 < took < 2, took
        assert await until(lambda: server.closed == server.accepted), "the request is aborted"
        await server.close()

    asyncio.run(run())
    records = _tap_records(tap)
    assert [k for k, _ in records[:2]] == [1, 2]
    kinds = [k for k, _ in records[2:]]
    assert kinds == [3, 4] * (len(kinds) // 2)
    steps = [corepb.Step.FromString(b) for k, b in records if k == 3]
    replies = [corepb.Reply.FromString(b) for k, b in records if k == 4]
    stop_at = next(i for i, s in enumerate(steps) if s.input.WhichOneof("kind") == "stop")
    assert steps[stop_at + 1:], "stepped on after stop (timers) until the core said"
    assert [r.output.stopped for r in replies].index(True) == len(replies) - 1, "nothing after stopped"
    cancelled = [e.cancel.id for r in replies for e in r.output.effects if e.WhichOneof("kind") == "cancel"]
    assert cancelled, "the core cancelled the request at its stop timeout"


def test_a_handshake_that_never_answers_is_given_up_and_tried_again() -> None:
    async def run() -> None:
        server = await Silent().start()
        client = Client.start(Config(mode="platform", endpoint=f"ws://127.0.0.1:{server.port}/v1/opamp", instance_uid=UID,
                                     connect_timeout_ms=300), Status())
        try:
            assert await until(lambda: server.accepted >= 2, 10), "a second attempt"
            assert server.closed >= 1, "the first was aborted"
        finally:
            await asyncio.wait_for(client.stop(), 5)
            await server.close()

    asyncio.run(run())



def test_what_aiohttp_refuses_still_ends_the_effect(tmp_path) -> None:
    """A header aiohttp refuses (CR/LF in a value), or a max_message_bytes over 2^32 - 1, raised in
    the effect's task, which then ended with nothing reported: the core waited on it until its
    deadline (adversary: headers_odd seed 2, limit_huge seed 1). And a max_message_bytes of 0 went
    to aiohttp as is, where 0 means no limit: a message over it was delivered (limit_zero seed 1)."""
    async def run() -> None:
        async def answer(_req: web.Request) -> web.Response:
            return web.Response(body=b"x")

        async def push(req: web.Request) -> web.WebSocketResponse:
            ws = web.WebSocketResponse()
            await ws.prepare(req)
            await ws.send_bytes(b"12345")
            await asyncio.sleep(1)
            return ws

        app = web.Application()
        app.router.add_post("/h", answer)
        app.router.add_get("/d", answer)
        app.router.add_get("/w", push)
        runner = web.AppRunner(app, shutdown_timeout=0.1)
        await runner.setup()
        site = web.TCPSite(runner, "127.0.0.1", 0)
        await site.start()
        base = f"127.0.0.1:{site._server.sockets[0].getsockname()[1]}"  # type: ignore[union-attr]
        got: list[corepb.Input] = []
        x = Executor(got.append, str(tmp_path / "state"))
        odd = [corepb.Header(name="X-Odd", value="a\r\nb")]
        try:
            x.do(corepb.Effect(http_request=corepb.HttpRequest(
                id=1, url=f"http://{base}/h", headers=odd, timeout_ms=5000, max_response_bytes=100)))
            x.do(corepb.Effect(ws_open=corepb.WsOpen(id=2, url=f"ws://{base}/w", headers=odd, max_message_bytes=100)))
            x.do(corepb.Effect(download=corepb.Download(
                id=3, url=f"http://{base}/d", headers=odd, max_bytes=100, timeout_ms=5000)))
            x.do(corepb.Effect(ws_open=corepb.WsOpen(id=4, url=f"ws://{base}/w", max_message_bytes=2**40)))
            x.do(corepb.Effect(ws_open=corepb.WsOpen(id=5, url=f"ws://{base}/w", max_message_bytes=0)))
            assert await until(lambda: len(got) >= 7, 5), got
            by = {}
            for i in got:
                kind = i.WhichOneof("kind")
                by.setdefault(getattr(i, kind).id, []).append(kind)
            assert by[1] == ["http_failed"]
            assert by[2] == ["ws_failed"]
            assert by[3] == ["download_failed"]
            assert by[4] == ["ws_opened", "ws_message"], "a huge limit is no limit"
            assert by[5] == ["ws_opened", "ws_lost"], "a limit of 0 takes no message"
        finally:
            await x.close(1)
            await runner.cleanup()

    asyncio.run(run())


def test_a_response_body_in_pieces_is_read_whole() -> None:
    # Found by test_schedules.py: resp.content.read(n) returns what has come so far, and a
    # body that came in two pieces reached the core cut short (a bad_message).
    async def run() -> None:
        body = bytes(range(256)) * 8

        async def handle(req: web.Request) -> web.StreamResponse:
            await req.read()
            resp = web.StreamResponse(headers={"Content-Length": str(len(body))})
            await resp.prepare(req)
            await resp.write(body[:1000])
            await asyncio.sleep(0.2)
            await resp.write(body[1000:])
            return resp

        app = web.Application()
        app.router.add_post("/", handle)
        runner = web.AppRunner(app)
        await runner.setup()
        site = web.TCPSite(runner, "127.0.0.1", 0)
        await site.start()
        port = site._server.sockets[0].getsockname()[1]  # type: ignore[union-attr]
        got: list[corepb.Input] = []
        x = Executor(got.append)
        try:
            for limit, kind in ((len(body), "http_response"), (len(body) - 1, "http_failed")):
                got.clear()
                x.do(corepb.Effect(http_request=corepb.HttpRequest(
                    id=limit, url=f"http://127.0.0.1:{port}/", body=b"x", max_response_bytes=limit, timeout_ms=5000)))
                assert await until(lambda: bool(got))
                assert _kinds(got) == [kind], got
                if kind == "http_response":
                    assert got[0].http_response.body == body, len(got[0].http_response.body)
        finally:
            await x.close(1)
            await runner.cleanup()

    asyncio.run(run())


def test_what_the_server_sends_that_isnt_utf8_still_reaches_the_core(tmp_path) -> None:
    """Found by tests/fuzz/fuzz_client.py: aiohttp keeps the bytes of a header that aren't UTF-8 as
    lone surrogates (surrogateescape), which a protobuf string refuses. A response with one was
    reported as a failed request (a server that sends one, a Latin-1 Server or Set-Cookie say, was
    never heard); a refused upgrade with one, or a download's redirect to such a Location, raised in
    the effect's task, which ended with nothing reported. Each reaches the core as core mode's own
    HTTP reader takes it: what isn't UTF-8 replaced by U+FFFD (String::from_utf8_lossy)."""
    async def run() -> None:
        async def serve(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
            head = await reader.readuntil(b"\r\n\r\n")
            length = next((int(line.split(b":")[1]) for line in head.split(b"\r\n")
                           if line.lower().startswith(b"content-length:")), 0)
            await reader.readexactly(length)
            path = head.split(b" ")[1]
            if path == b"/h":
                writer.write(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nServer: caf\xe9\r\n\r\nok")
            elif path == b"/w":
                writer.write(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nX-Why: \xff\xfe no\r\n\r\n")
            else:
                writer.write(b"HTTP/1.1 302 Found\r\nContent-Length: 0\r\nLocation: /caf\xe9/p\r\n\r\n")
            await writer.drain()
            writer.close()

        server = await asyncio.start_server(serve, "127.0.0.1", 0)
        base = f"127.0.0.1:{server.sockets[0].getsockname()[1]}"
        got: list[corepb.Input] = []
        x = Executor(got.append, str(tmp_path / "state"))
        try:
            x.do(corepb.Effect(http_request=corepb.HttpRequest(
                id=1, url=f"http://{base}/h", timeout_ms=5000, max_response_bytes=100)))
            x.do(corepb.Effect(ws_open=corepb.WsOpen(id=2, url=f"ws://{base}/w", max_message_bytes=100)))
            x.do(corepb.Effect(download=corepb.Download(id=3, url=f"http://{base}/d", max_bytes=100, timeout_ms=5000)))
            assert await until(lambda: len(got) >= 3, 5), got
            by = {getattr(i, i.WhichOneof("kind")).id: i for i in got}
            assert by[1].WhichOneof("kind") == "http_response", by[1]
            assert ("server", "caf�") in [(h.name.lower(), h.value) for h in by[1].http_response.headers]
            assert by[2].WhichOneof("kind") == "ws_failed" and by[2].ws_failed.status == 403, by[2]
            assert ("x-why", "�� no") in [(h.name.lower(), h.value) for h in by[2].ws_failed.headers]
            assert by[3].WhichOneof("kind") == "download_response", by[3]
            assert by[3].download_response.location == f"http://{base}/caf�/p"
        finally:
            await x.close(1)
            server.close()

    asyncio.run(run())
