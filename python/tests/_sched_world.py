# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The world a schedule test runs the client in: an OpAMP server whose every answer is the
test's to decide, when it decides, reached through

- Platform mode: a fake aiohttp session and WebSocket (``FakeSession``, given to the client as its
  ``_sessions``), as aiohttp behaves: a response body comes in pieces and ``read(n)`` returns what
  has come, a total timeout, a close handshake that waits for the server's Close frame;
- Core mode: the seeded loop's in-memory network (``_sched_loop.Network``), with a real aiohttp
  server (its low-level ``web.Server``) on each connection, so the core's own HTTP/1.1 and
  WebSocket meet a real peer.

Everything the client asks of the server waits in ``World.ops`` (a connection attempt, a request,
a WebSocket upgrade, a client's Close frame, a package download) until the test decides it; an
open WebSocket's server end is a ``ServerConn``, through which the test pushes, closes or drops."""

from __future__ import annotations

import asyncio
import hashlib
from collections import deque
from dataclasses import dataclass, field
from typing import Any

import aiohttp
from aiohttp import WSMessage, WSMsgType, web
from multidict import CIMultiDict

from _sched_loop import Attempt, MemTransport, Network, SeededLoop
from pydantic_opamp_client import opamp_pb2 as pb

CAPABILITY = "io.example.schedules"
OFFERS = ["none", "config", "custom", "package", "heartbeat"]


def package_body(n: int, good: bool = True) -> bytes:
    body = bytes((n * 7 + i) % 251 for i in range(1500 + 300 * (n % 5)))
    return body if good else body[:-1] + bytes([body[-1] ^ 1])


def cut(data: bytes, cuts: list[int]) -> list[bytes]:
    """`data` in pieces, cut at the given offsets (taken modulo its length)."""
    if not data:
        return []
    at = sorted({c % len(data) for c in cuts} - {0})
    return [data[a:b] for a, b in zip([0, *at], [*at, len(data)])]


@dataclass(eq=False)
class Op:
    """Something the client asked of the server, waiting for the test's decision."""

    kind: str  # connect, post, ws_open, ws_close, download
    decision: asyncio.Future
    url: str = ""
    body: bytes = b""
    conn: ServerConn | None = None
    attempt: Attempt | None = None
    transport: MemTransport | None = None  # Core mode: the server's end it came on
    gone: bool = False  # the client gave up on it

    @property
    def waiting(self) -> bool:
        if self.gone or self.decision.done():
            return False
        if self.attempt is not None:
            return self.attempt.waiting
        return self.transport is None or not self.transport.lost

    def decide(self, how: tuple) -> None:
        if self.attempt is not None:
            self.attempt.decide(how[0])
        elif not self.decision.done():
            self.decision.set_result(how)


@dataclass(eq=False)
class ServerConn:
    """The server's end of an open WebSocket."""

    world: World
    heard: deque = field(default_factory=deque)  # the client's messages, not yet answered
    ended: bool = False  # closed or dropped, by either side
    closing: bool = False  # the server sent its Close frame
    impl: Any = None  # the mode's own: a FakeWS, or (core mode) the aiohttp WebSocketResponse

    def hear(self, data: bytes) -> None:
        self.heard.append(pb.AgentToServer.FromString(data[1:]))

    async def push(self, offer: str) -> None:
        uid = self.heard.popleft().instance_uid if self.heard else b""
        await self.world.mode_push(self, b"\0" + self.world.reply(uid, offer).SerializeToString())


class World:
    """The server, and what it offered: custom messages and remote configs are numbered in the
    order offered."""

    def __init__(self, loop: SeededLoop, mode: str) -> None:
        self.loop, self.mode = loop, mode
        self.ops: list[Op] = []
        self.conns: list[ServerConn] = []
        self.customs = self.configs = self.packages = 0
        self.late = 0  # results that came after the client gave up on them
        self.heard: list[pb.AgentToServer] = []  # every message, in arrival order
        self.fakes: list[FakeWS] = []  # Platform mode: every WebSocket made
        self.requests: list[_Exchange] = []  # Platform mode: every request and download
        self.tasks: set[asyncio.Task] = set()  # the server's own
        self.network: Network | None = None
        if mode == "core":
            self.network = Network(loop, self._serve)
            loop.network = self.network

    # -- what is waiting

    def waiting(self) -> list[Op]:
        ops = [op for op in self.ops if op.waiting]
        if self.network is not None:
            known = {op.attempt for op in ops if op.attempt is not None}
            for a in self.network.attempts:
                if a.waiting and a not in known:
                    op = Op("connect", a.decision, url=f"{a.host}:{a.port}", attempt=a)
                    self.ops.append(op)
                    ops.append(op)
        order = {op: i for i, op in enumerate(self.ops)}
        return sorted(ops, key=order.__getitem__)

    def open_conns(self) -> list[ServerConn]:
        return [c for c in self.conns if not c.ended]

    def park(self, kind: str, **kw: Any) -> Op:
        op = Op(kind, self.loop.create_future(), **kw)
        self.ops.append(op)
        return op

    #: What a request, upgrade or download the client gave up on comes to when it finishes anyway.
    LATE = {"post": ("ok", 200, "none", []), "ws_open": ("ok",), "download": ("ok", 200, True, [])}

    async def decided(self, op: Op, kind: str, timed_out=lambda: False) -> tuple:
        """The server's decision on `op`. Cancelled meanwhile: given up on, mostly; but some (the
        seed's choice, a quarter) finish anyway, at once, as a response already on its way does:
        the client gets a result it gave up on."""
        late = self.loop.rng.random() < 0.25
        try:
            return await asyncio.shield(op.decision)
        except asyncio.CancelledError:
            if not late or timed_out():
                raise
            self.late += 1
            return self.LATE[kind]
        finally:
            op.gone = True

    def spawn(self, coro) -> None:
        task = self.loop.create_task(coro)
        self.tasks.add(task)
        task.add_done_callback(self.tasks.discard)

    # -- OpAMP

    def reply(self, uid: bytes, offer: str) -> pb.ServerToAgent:
        r = pb.ServerToAgent(instance_uid=uid)
        if offer == "config":
            self.configs += 1
            h = b"c%d" % self.configs
            r.remote_config.config_hash = h
            r.remote_config.config.config_map[""].body = h
        elif offer == "custom":
            self.customs += 1
            r.custom_message.CopyFrom(pb.CustomMessage(capability=CAPABILITY, type="n", data=str(self.customs).encode()))
        elif offer == "package":
            self.packages += 1
            n = self.packages
            r.packages_available.CopyFrom(pb.PackagesAvailable(all_packages_hash=b"ph%d" % n, packages={
                "pkg": pb.PackageAvailable(type=pb.PackageType_TopLevel, version=str(n), hash=b"h%d" % n,
                                           file=pb.DownloadableFile(download_url=f"http://files.test/p{n}",
                                                                    content_hash=hashlib.sha256(package_body(n)).digest()))}))
        elif offer == "heartbeat":
            r.connection_settings.opamp.heartbeat_interval_seconds = 1 + self.loop.rng.randrange(30)
        return r

    def answer(self, body: bytes, offer: str) -> bytes:
        msg = pb.AgentToServer.FromString(body)
        self.heard.append(msg)
        return self.reply(msg.instance_uid, offer).SerializeToString()

    # -- the modes' own

    async def mode_push(self, conn: ServerConn, data: bytes) -> None:
        if conn.ended:
            return
        if self.mode == "platform":
            conn.impl.deliver(WSMessage(WSMsgType.BINARY, data, None))
        else:
            try:
                await conn.impl.send_bytes(data)
            except Exception:  # noqa: BLE001 - it went meanwhile
                pass

    def server_close(self, conn: ServerConn, code: int) -> None:
        if conn.ended or conn.closing:
            return
        conn.closing = True
        if self.mode == "platform":
            conn.impl.deliver(WSMessage(WSMsgType.CLOSE, code, ""))
        else:
            self.spawn(_close_quietly(conn.impl, code))

    def drop(self, conn: ServerConn) -> None:
        if conn.ended:
            return
        conn.ended = True
        if self.mode == "platform":
            conn.impl.dropped(ConnectionResetError("reset by the server"))
        else:
            transport = conn.impl._req.transport
            if transport is not None:
                transport.abort()

    # -- core mode: an aiohttp server on each in-memory connection

    def _serve(self, host: str, port: int) -> asyncio.BaseProtocol:
        if not hasattr(self, "_server"):
            self._server = web.Server(self._handle, loop=self.loop)
        return self._server()

    async def _handle(self, req: web.BaseRequest) -> web.StreamResponse:
        transport = req.transport
        if req.method == "POST":
            body = await req.read()
            op = self.park("post", url=str(req.url), body=body, transport=transport)
            how = await op.decision
            if how[0] == "fail":
                return _abort(req)
            _, status, offer, cuts = how
            data = self.answer(body, offer) if status == 200 else b""
            return await _write(req, status, data, cuts)
        if req.headers.get("Upgrade", "").lower() == "websocket":
            op = self.park("ws_open", url=str(req.url), transport=transport)
            how = await op.decision
            if how[0] == "fail":
                return _abort(req)
            if how[0] == "status":
                return web.Response(status=how[1])
            ws = web.WebSocketResponse(autoclose=False, autoping=True)
            await ws.prepare(req)
            conn = ServerConn(self, impl=ws)
            self.conns.append(conn)
            try:
                async for m in ws:
                    if m.type == WSMsgType.BINARY:
                        conn.hear(m.data)
                        self.heard.append(conn.heard[-1])
                    elif m.type == WSMsgType.CLOSE:
                        if conn.closing:  # the client answered ours
                            await ws.close()
                            break
                        cop = self.park("ws_close", conn=conn, transport=transport)
                        how = await cop.decision
                        if how[0] == "ack":
                            await ws.close(code=1000)
                        else:
                            transport.abort()  # type: ignore[union-attr]
                        break
            finally:
                conn.ended = True
            return ws
        # A package file.
        op = self.park("download", url=str(req.url), transport=transport)
        how = await op.decision
        if how[0] == "fail":
            return _abort(req)
        n = int(req.path.rsplit("p", 1)[-1] or 0)
        if how[0] in ("fail_mid", "stall"):
            body = package_body(n, how[0] == "fail_mid" or how[1])
            resp = web.StreamResponse(status=200)
            resp.content_length = len(body)
            await resp.prepare(req)
            await resp.write(body[:700])
            if how[0] == "fail_mid":
                return _abort(req)
            # The rest when the test says (or never).
            rest = self.park("body", url=str(req.url), transport=transport)
            if (await rest.decision)[0] != "rest":
                return _abort(req)
            await resp.write(body[700:])
            await resp.write_eof()
            return resp
        _, status, good, cuts = how
        return await _write(req, status, package_body(n, good) if status == 200 else b"missing", cuts)


async def _close_quietly(ws: web.WebSocketResponse, code: int) -> None:
    try:
        await ws.close(code=code)
    except Exception:  # noqa: BLE001
        pass


def _abort(req: web.BaseRequest) -> web.StreamResponse:
    if req.transport is not None:
        req.transport.abort()
    raise asyncio.CancelledError  # nothing more to send: the connection is gone


async def _write(req: web.BaseRequest, status: int, data: bytes, cuts: list[int]) -> web.StreamResponse:
    resp = web.StreamResponse(status=status)
    resp.content_length = len(data)
    await resp.prepare(req)
    for piece in cut(data, cuts):
        await resp.write(piece)
        await asyncio.sleep(0)
    await resp.write_eof()
    return resp


# -- platform mode: a fake aiohttp


_MORE = object()  # FakeContent.feed: more to come


class FakeContent:
    """A response body as aiohttp's StreamReader gives it: read(n) waits for something, then
    returns what has come, up to n bytes."""

    def __init__(self, loop: SeededLoop) -> None:
        self._loop = loop
        self._buf: deque[bytes] = deque()
        self._eof = False
        self._exc: BaseException | None = None
        self._waiter: asyncio.Future | None = None

    def feed(self, pieces: list[bytes], end: BaseException | None | object = None) -> None:
        """Deliver `pieces`, one per turn, then the end (or `end`, raised to the reader; or, for
        _MORE, nothing yet)."""
        if not pieces:
            if end is not _MORE:
                self._end(end)  # type: ignore[arg-type]
            return
        self._buf.append(pieces[0])
        self._wake()
        self._loop.call_soon(self.feed, pieces[1:], end)

    def _end(self, end: BaseException | None) -> None:
        if end is None:
            self._eof = True
        else:
            self._exc = end
        self._wake()

    def _wake(self) -> None:
        if self._waiter is not None and not self._waiter.done():
            self._waiter.set_result(None)

    async def read(self, n: int = -1) -> bytes:
        while not self._buf and not self._eof and self._exc is None:
            self._waiter = self._loop.create_future()
            await self._waiter
        if not self._buf and self._exc is not None:
            raise self._exc
        out = bytearray()
        while self._buf and (n < 0 or len(out) < n):
            chunk = self._buf.popleft()
            take = len(chunk) if n < 0 else min(len(chunk), n - len(out))
            out += chunk[:take]
            if take < len(chunk):
                self._buf.appendleft(chunk[take:])
        return bytes(out)

    def iter_chunked(self, n: int):
        content = self

        class _It:
            def __aiter__(self):
                return self

            async def __anext__(self) -> bytes:
                data = await content.read(n)
                if not data:
                    raise StopAsyncIteration
                return data

        return _It()


class FakeResponse:
    def __init__(self, loop: SeededLoop, status: int, headers: dict[str, str]) -> None:
        self.status = status
        self.headers = CIMultiDict(headers)
        self.content = FakeContent(loop)


class _Exchange:
    """One request or download (``async with session.post(...) as resp``), with aiohttp's total
    timeout around all of it."""

    def __init__(self, session: FakeSession, kind: str, url: str, body: bytes, timeout: aiohttp.ClientTimeout) -> None:
        self.session, self.kind, self.url, self.body = session, kind, url, body
        # aiohttp's total timeout is the core's deadline: half the time a millisecond later, so
        # that the core's cancel comes first (and a result may still come after it).
        total = timeout.total if timeout is not None else None
        self.total = total + (0.001 if session.world.loop.rng.random() < 0.5 else 0.0) if total else None
        self.op: Op | None = None
        self.rest: Op | None = None  # the rest of a body that stalled, when the test says
        self.finished = False  # the client is done with it (released, or given up on)
        self._tm: asyncio.Timeout | None = None

    async def __aenter__(self) -> FakeResponse:
        world = self.session.world
        world.requests.append(self)
        if self.total:
            self._tm = asyncio.timeout(self.total)
            await self._tm.__aenter__()
        try:
            self.op = world.park(self.kind, url=self.url, body=self.body)
            how = await world.decided(self.op, self.kind, lambda: self._tm is not None and self._tm.expired())
            if how[0] == "fail":
                raise aiohttp.ClientConnectionError("connection reset by the server")
            loop = world.loop
            if self.kind == "post":
                _, status, offer, cuts = how
                data = world.answer(self.body, offer) if status == 200 else b""
                resp = FakeResponse(loop, status, {"Content-Length": str(len(data))})
                resp.content.feed(cut(data, cuts))
                return resp
            n = int(self.url.rsplit("p", 1)[-1] or 0)
            if how[0] == "fail_mid":
                resp = FakeResponse(loop, 200, {})
                resp.content.feed([package_body(n)[:700]], aiohttp.ClientPayloadError("the connection closed mid-body"))
                return resp
            if how[0] == "stall":
                body = package_body(n, how[1])
                resp = FakeResponse(loop, 200, {})
                resp.content.feed([body[:700]], _MORE)
                self.rest = world.park("body", url=self.url)
                world.spawn(self._rest(resp.content, body[700:]))
                return resp
            _, status, good, cuts = how
            data = package_body(n, good) if status == 200 else b"missing"
            resp = FakeResponse(loop, status, {})
            resp.content.feed(cut(data, cuts))
            return resp
        except BaseException as e:
            await self._exit(type(e), e)
            raise

    async def __aexit__(self, et, e, tb) -> bool | None:
        return await self._exit(et, e)

    async def _rest(self, content: FakeContent, rest: bytes) -> None:
        assert self.rest is not None
        how = await self.rest.decision
        if how[0] == "rest":
            content.feed([rest])
        else:
            content._end(aiohttp.ClientPayloadError("the connection closed mid-body"))

    async def _exit(self, et, e) -> bool | None:
        self.finished = True
        if self.rest is not None:
            self.rest.gone = True
        if self._tm is not None:
            tm, self._tm = self._tm, None
            return await tm.__aexit__(et, e, None)
        return None


class _Drop:
    def __init__(self, ws: FakeWS) -> None:
        self._ws = ws

    def close(self) -> None:
        self._ws.close_connection()


class FakeWS:
    """A client WebSocket as aiohttp's ClientWebSocketResponse behaves with autoclose=False and
    no close timeout."""

    def __init__(self, world: World, conn: ServerConn) -> None:
        self.world, self.conn = world, conn
        self._inbox: deque[WSMessage] = deque()
        self._waiter: asyncio.Future | None = None
        self._closed = False  # our Close frame went, or the connection is gone
        self._closing = False  # the server's Close frame came
        self.close_code: int | None = None
        self._exc: BaseException | None = None
        self.gone = False  # the connection itself is closed
        self._response = _Drop(self)  # aiohttp: ws._response.close() drops the connection

    # The server's side.

    def deliver(self, msg: WSMessage) -> None:
        if self.gone:
            return
        self._inbox.append(msg)
        if self._waiter is not None and not self._waiter.done():
            self._waiter.set_result(None)

    def dropped(self, exc: BaseException) -> None:
        if self.gone:
            return
        self._exc = self._exc or exc
        self.deliver(WSMessage(WSMsgType.ERROR, exc, None))
        self.gone = True
        self.conn.ended = True

    # aiohttp's.

    def close_connection(self) -> None:
        if not self.gone:
            self.gone = True
            self.conn.ended = True
            if self._waiter is not None and not self._waiter.done():
                self._waiter.set_result(None)

    def exception(self) -> BaseException | None:
        return self._exc

    async def _next(self) -> WSMessage:
        while not self._inbox:
            if self.gone:
                return WSMessage(WSMsgType.CLOSED, None, None)
            self._waiter = self.world.loop.create_future()
            await self._waiter
        return self._inbox.popleft()

    async def receive(self) -> WSMessage:
        if self._closed and self.gone:
            return WSMessage(WSMsgType.CLOSED, None, None)
        msg = await self._next()
        if msg.type == WSMsgType.CLOSE:
            self._closing = True
            self.close_code = msg.data
        elif msg.type in (WSMsgType.ERROR, WSMsgType.CLOSED):
            self._closed = True
            self.close_code = 1006
        return msg

    async def send_bytes(self, data: bytes) -> None:
        if self.gone or self._closed:
            raise aiohttp.ClientConnectionResetError("Cannot write to closing transport")
        self.conn.hear(data)
        self.world.heard.append(self.conn.heard[-1])
        await asyncio.sleep(0)

    async def close(self, *, code: int = 1000, message: bytes = b"") -> bool:
        if self._waiter is not None and not self._waiter.done() and not self._closing:
            self.deliver(WSMessage(WSMsgType.CLOSING, None, None))
            await asyncio.sleep(0)
        if self._closed:
            return False
        self._closed = True
        if self.gone:
            self.close_code = 1006
            self._exc = self._exc or ConnectionResetError("the connection is gone")
            return True
        if self._closing:  # the server's Close came first: ours answers it
            self.close_connection()
            return True
        op = self.world.park("ws_close", conn=self.conn)
        try:
            how = await op.decision
        except BaseException:
            op.gone = True
            self.close_code = 1006
            self.close_connection()
            raise
        if how[0] == "ack":
            self.close_code = 1000
        else:
            self.close_code = 1006
            self._exc = ConnectionResetError("dropped during the closing handshake")
        self.close_connection()
        return True


class FakeSession:
    """What the client's executor takes for an aiohttp ClientSession."""

    def __init__(self, world: World, kind: str) -> None:
        self.world, self.kind = world, kind
        self.closed = False

    def post(self, url: str, *, data: bytes, headers, timeout, allow_redirects: bool) -> _Exchange:
        return _Exchange(self, "post", url, bytes(data), timeout)

    def get(self, url: str, *, headers, timeout, allow_redirects: bool) -> _Exchange:
        return _Exchange(self, "download", url, b"", timeout)

    async def ws_connect(self, url: str, **kwargs) -> FakeWS:
        world = self.world
        op = world.park("ws_open", url=url)
        how = await world.decided(op, "ws_open")
        if how[0] == "fail":
            raise aiohttp.ClientConnectionError("connection refused")
        if how[0] == "status":
            raise aiohttp.WSServerHandshakeError(None, (), status=how[1], message="refused", headers=CIMultiDict())
        conn = ServerConn(world)
        ws = FakeWS(world, conn)
        conn.impl = ws
        world.conns.append(conn)
        world.fakes.append(ws)
        return ws

    async def close(self) -> None:
        self.closed = True
