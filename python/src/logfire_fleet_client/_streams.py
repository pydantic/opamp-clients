# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Core mode: the core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5 itself,
and this carries out only what needs the platform: TCP connections, TLS on them (Python's ssl,
with the operator's CAs, certificate and versions), name resolution (only for a SOCKS5 proxy
with resolve_locally: otherwise the proxy resolves), the proxy urllib's convention picks for a
URL. It decides nothing: every byte it writes, the core wrote, and the core keeps every time (it
closes a stream it gives up on). As node/opamp/src/streams.ts does for Node.

One registry (`_InFlight`) owns everything under way: each stream (its transport, and the task
connecting it or starting its TLS), each name being resolved, and each timer (a closed stream's
linger). A close, from the core or at shutdown, ends what it names at once; a connection or a TLS
handshake that completes after its stream was closed is aborted as it arrives, and reported to no
one.

Backpressure: the core writes without hearing back (it writes one request or message at a
  time, plus answers to the peer's pings and closes). While a transport's write buffer is over
  its high-water mark (asyncio's pause_writing), the stream reads nothing more, so a peer that
  doesn't read hears nothing more to answer; and a write that finds more than MAX_UNSENT bytes
  still unsent fails the stream (stream_failed: the core reconnects, as for any lost
  connection) rather than buffering without end.
"""

from __future__ import annotations

import asyncio
import socket
from collections import deque
from collections.abc import Callable, Coroutine
from typing import Any

from ._executor import lossy
from ._proto.opamp.core.v1 import core_pb2 as corepb
from ._transport import Transport

Deliver = Callable[[corepb.Input], None]


def proxy_choice(transport: Transport) -> corepb.ProxyChoice:
    """What core mode tells the core about proxies, once (Config.proxy): the configured proxy (its
    URL as given, credentials and all, its CONNECT headers, resolve_locally), none with
    Proxy.DIRECT, or else the environment's variables, from which the core picks each
    connection's proxy by the rule every SDK's core mode shares. (The environment's bytes that
    aren't UTF-8 are lone surrogates here: lossy.)"""
    r = transport.fixed
    if r is not None:
        return corepb.ProxyChoice(url=lossy(r.url), resolve_locally=r.resolve_locally,
                                  headers=[corepb.Header(name=lossy(k), value=lossy(v)) for k, v in r.headers.items()])
    if transport.configured:
        return corepb.ProxyChoice(direct=True)
    return corepb.ProxyChoice(environment=[corepb.Header(name=k, value=lossy(v))
                                           for k, v in transport.core_environment.items()])

#: How long a closed stream has to flush what was written before it is aborted.
LINGER_S = 2.0
#: Bytes written to a stream and not yet taken by the peer beyond which the next write fails it.
MAX_UNSENT = 16 << 20
#: How long shutdown waits for the tasks under way (connects, handshakes) before it returns.
SHUTDOWN_S = 5.0


def _failed(id_: int, detail: str) -> corepb.Input:
    return corepb.Input(stream_failed=corepb.Failed(id=id_, detail=lossy(detail)))


def _detail(e: BaseException | None, default: str) -> str:
    return (str(e) or type(e).__name__) if e is not None else default


def _abort(t: asyncio.BaseTransport) -> None:
    """Abort `t`. One asyncio has already closed may raise (a selector transport that finished a
    graceful close has no loop left): it is gone, which is all an abort asks."""
    try:
        t.abort()  # type: ignore[attr-defined]
    except (AttributeError, RuntimeError):
        pass


class _Stream(asyncio.Protocol):
    """One stream: the protocol on its transport (TCP, then TLS on it). Until the core has heard
    the stream is ready (opened, or TLS up), what arrives waits here, in order."""

    def __init__(self, owner: Streams, id_: int) -> None:
        self.owner = owner
        self.id = id_
        self.transport: asyncio.Transport | None = None
        #: Connecting or in a TLS handshake: events wait in `early`.
        self.pending = True
        self.early: deque[tuple[str, Any]] = deque()
        #: The task connecting it or starting its TLS.
        self.task: asyncio.Task | None = None
        #: Why it is not reading: a full write buffer.
        self.holds = 0
        self.blocked = False
        #: Written while pending: sent once it is ready.
        self.unsent: list[bytes] = []
        #: Its transport has closed (asyncio said connection_lost).
        self.gone = False

    # asyncio.Protocol: each event is the stream's while it is in the registry; after, nothing.

    def connection_made(self, transport: asyncio.BaseTransport) -> None:
        self.transport = transport  # type: ignore[assignment]
        if not self.owner._mine(self):
            _abort(transport)

    def data_received(self, data: bytes) -> None:
        self._event("data", data)

    def eof_received(self) -> bool | None:
        self._event("eof", None)
        return None  # close our side too

    def connection_lost(self, exc: Exception | None) -> None:
        self.gone = True
        self.owner._in.lingering.discard(self)
        self._event("lost", exc)

    def pause_writing(self) -> None:
        if self.owner._mine(self) and not self.pending and not self.blocked:
            self.blocked = True
            self.hold()

    def resume_writing(self) -> None:
        if self.blocked:
            self.blocked = False
            self.release()

    def _event(self, kind: str, value: Any) -> None:
        if not self.owner._mine(self):
            return
        if self.pending:
            self.early.append((kind, value))
        else:
            self.owner._event(self, kind, value)

    def ready(self, transport: asyncio.Transport, said: corepb.Input) -> None:
        """Connected, or TLS is up on `transport`: tell the core, then what came meanwhile."""
        self.transport = transport
        self.pending = False
        for data in self.unsent:
            transport.write(data)
        self.unsent.clear()
        self.owner._deliver(said)
        while self.early and self.owner._mine(self) and not self.pending:
            kind, value = self.early.popleft()
            self.owner._event(self, kind, value)

    def hold(self) -> None:
        """Stop reading (for one more reason)."""
        self.holds += 1
        if self.holds == 1 and self.transport is not None:
            self.transport.pause_reading()

    def release(self) -> None:
        """One reason fewer not to read: read again if it was the last."""
        if self.holds == 0:
            return
        self.holds -= 1
        if self.holds == 0 and self.transport is not None and not self.transport.is_closing():
            self.transport.resume_reading()


class _InFlight:
    """Everything under way, by owner: streams by id, names being resolved by stream id, tasks
    (connects, TLS handshakes, resolutions), timers, and closed transports still
    flushing. `close` and `shutdown` end what is here; nothing under way is anywhere else."""

    def __init__(self) -> None:
        self.streams: dict[int, _Stream] = {}
        self.resolving: dict[int, asyncio.Task] = {}
        self.tasks: set[asyncio.Task] = set()
        self.timers: set[asyncio.TimerHandle] = set()
        #: Streams closed with what was written still going (LINGER_S at most).
        self.lingering: set[_Stream] = set()
        self.closed = False

    def spawn(self, coro: Coroutine[Any, Any, Any]) -> asyncio.Task:
        task = asyncio.get_running_loop().create_task(coro)
        self.tasks.add(task)
        task.add_done_callback(self.tasks.discard)
        return task

    def later(self, delay: float, fn: Callable[[], None]) -> None:
        handle: asyncio.TimerHandle

        def run() -> None:
            self.timers.discard(handle)
            fn()

        handle = asyncio.get_running_loop().call_later(delay, run)
        self.timers.add(handle)

    def close(self, s: _Stream, abort: bool) -> None:
        """Stream `s` is no longer anyone's: stop its task, and close its transport (abort: at
        once; otherwise once what was written has gone, or LINGER_S)."""
        if self.streams.get(s.id) is s:
            del self.streams[s.id]
        s.early.clear()
        s.unsent.clear()
        connecting = s.task is not None and not s.task.done()
        if s.task is not None:
            s.task.cancel()
        t = s.transport
        if t is None or s.gone:
            return
        if abort or connecting or s.pending or t.is_closing():
            # Given up on (or never ready: nothing written has anywhere to go).
            _abort(t)
            return
        self.lingering.add(s)
        t.close()

        def linger_over() -> None:
            if s in self.lingering:
                self.lingering.discard(s)
                _abort(t)

        self.later(LINGER_S, linger_over)

    def stop_all(self) -> None:
        """Close every stream at once, stop every resolution and connect, fire no timer."""
        self.closed = True
        for s in list(self.streams.values()):
            self.close(s, abort=True)
        for task in self.resolving.values():
            task.cancel()
        self.resolving.clear()
        for h in self.timers:
            h.cancel()
        self.timers.clear()
        for s in self.lingering:
            if s.transport is not None:
                _abort(s.transport)
        self.lingering.clear()


class Streams:
    """Carries out core mode's effects: `do` takes those that are its own."""

    def __init__(self, deliver: Deliver, transport: Transport, *,
                 connect: Callable[..., Coroutine[Any, Any, tuple[asyncio.BaseTransport, Any]]] | None = None,
                 start_tls: Callable[..., Coroutine[Any, Any, asyncio.BaseTransport | None]] | None = None,
                 getaddrinfo: Callable[..., Coroutine[Any, Any, list[Any]]] | None = None) -> None:
        self._deliver = deliver
        self._transport = transport
        loop = asyncio.get_running_loop()
        # The loop's, unless a test stands in for them.
        self._connect = connect or loop.create_connection
        self._start_tls = start_tls or loop.start_tls
        self._getaddrinfo = getaddrinfo or loop.getaddrinfo
        self._in = _InFlight()

    def do(self, e: corepb.Effect) -> bool:
        """Carry out `e` if it is one of core mode's; False if it isn't."""
        kind = e.WhichOneof("kind")
        if kind == "proxy_query":
            q = e.proxy_query
            r = self._transport.proxy_for(q.url)
            self._deliver(corepb.Input(proxy_answer=corepb.ProxyAnswer(
                id=q.id, proxy=lossy(r.url) if r else "",
                headers=[corepb.Header(name=lossy(k), value=lossy(v))
                         for k, v in (r.headers if r else {}).items()],
                resolve_locally=r.resolve_locally if r else False)))
        elif kind == "stream_resolve":
            self._resolve(e.stream_resolve.id, e.stream_resolve.host)
        elif kind == "stream_open":
            o = e.stream_open
            self._open(o.id, o.host, o.port)
        elif kind == "stream_start_tls":
            t = e.stream_start_tls
            self._tls(t.id, t.host, t.server_origin)
        elif kind == "stream_write":
            self._write(e.stream_write.id, e.stream_write.data)
        elif kind == "stream_close":
            c = e.stream_close
            if (task := self._in.resolving.pop(c.id, None)) is not None:
                task.cancel()
            if (s := self._in.streams.get(c.id)) is not None:
                self._in.close(s, c.abort)
        else:
            return False
        return True

    # Streams.

    def _mine(self, s: _Stream) -> bool:
        return self._in.streams.get(s.id) is s

    def _resolve(self, id_: int, host: str) -> None:
        async def run() -> None:
            try:
                infos = await self._getaddrinfo(host, None, type=socket.SOCK_STREAM)
            except Exception as e:  # noqa: BLE001 - a name that can't be resolved, however
                if self._in.resolving.pop(id_, None) is task:
                    self._deliver(_failed(id_, _detail(e, "no address")))
                return
            if self._in.resolving.get(id_) is not task:
                return
            del self._in.resolving[id_]
            addresses: list[str] = []
            for info in infos:
                a = str(info[4][0]).split("%", 1)[0]
                if a not in addresses:
                    addresses.append(a)
            self._deliver(corepb.Input(stream_resolved=corepb.StreamResolved(id=id_, addresses=addresses)))

        if self._in.closed:
            return
        task = self._in.spawn(run())
        self._in.resolving[id_] = task

    def _open(self, id_: int, host: str, port: int) -> None:
        if self._in.closed:
            return
        if (old := self._in.streams.get(id_)) is not None:
            self._in.close(old, abort=True)
        s = _Stream(self, id_)
        self._in.streams[id_] = s

        async def run() -> None:
            try:
                transport, _ = await self._connect(lambda: s, host, port)
            except Exception as e:  # noqa: BLE001 - whatever it is, the stream is gone
                if self._mine(s):
                    self._in.close(s, abort=True)
                    self._deliver(_failed(id_, _detail(e, "connecting failed")))
                return
            if not self._mine(s):
                # Closed while it connected: it goes at once.
                _abort(transport)
                return
            s.ready(transport, corepb.Input(stream_opened=corepb.Id(id=id_)))  # type: ignore[arg-type]

        s.task = self._in.spawn(run())

    def _tls(self, id_: int, host: str, server_origin: bool) -> None:
        s = self._in.streams.get(id_)
        if s is None or s.transport is None or s.pending:
            return
        raw = s.transport
        # From now on the bytes are TLS's, not the core's; and the holds were the raw transport's.
        s.pending = True
        s.holds = 0
        s.blocked = False

        async def run() -> None:
            try:
                # The client certificate only at the server's own origin; the CAs everywhere.
                ctx = self._transport.ssl if server_origin else self._transport.elsewhere_ssl
                tls = await self._start_tls(raw, s, ctx, server_hostname=host)
                if tls is None:
                    raise ConnectionError("the connection closed during the TLS handshake")
            except Exception as e:  # noqa: BLE001 - whatever it is, the stream is gone
                if self._mine(s):
                    self._in.close(s, abort=True)
                    self._deliver(_failed(id_, _detail(e, "the TLS handshake failed")))
                return
            if not self._mine(s):
                _abort(tls)
                return
            s.ready(tls, corepb.Input(stream_tls_ready=corepb.Id(id=id_)))  # type: ignore[arg-type]

        s.task = self._in.spawn(run())

    def _write(self, id_: int, data: bytes) -> None:
        s = self._in.streams.get(id_)
        if s is None:
            return
        if s.transport is None or s.pending:
            s.unsent.append(data)  # (the core waits for the stream to be ready: kept, in order)
            return
        unsent = s.transport.get_write_buffer_size()
        if unsent > MAX_UNSENT:
            # The peer has stopped reading: give up on it rather than buffer without end.
            self._in.close(s, abort=True)
            self._deliver(_failed(id_, f"the peer has not taken the last {unsent} bytes written to it"))
            return
        s.transport.write(data)

    def _event(self, s: _Stream, kind: str, value: Any) -> None:
        """What arrived on `s`, which the core has heard is ready."""
        if kind == "data":
            self._deliver(corepb.Input(stream_data=corepb.WsMessage(id=s.id, data=bytes(value))))
        elif kind == "eof":
            self._deliver(corepb.Input(stream_eof=corepb.Id(id=s.id)))
        else:
            # Gone, with an error or without (after an end, or a reset): the core must hear it.
            self._in.close(s, abort=True)
            self._deliver(_failed(s.id, _detail(value, "the connection closed")))

    async def shutdown(self, timeout: float | None = None) -> None:
        """Drop every stream and resolution: waits for the tasks under way `timeout` seconds at
        most (None: SHUTDOWN_S)."""
        if timeout is None:
            timeout = SHUTDOWN_S
        self._in.stop_all()
        loop = asyncio.get_running_loop()
        deadline = loop.time() + timeout
        while self._in.tasks and (left := deadline - loop.time()) > 0:
            await asyncio.wait(list(self._in.tasks), timeout=left)
