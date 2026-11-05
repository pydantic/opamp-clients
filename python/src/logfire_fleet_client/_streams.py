# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Core mode: the core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5 itself,
and this carries out only what needs the platform: TCP connections, TLS on them (Python's ssl,
with the operator's CAs, certificate and versions), name resolution (only for a SOCKS5 proxy
with resolve_locally: otherwise the proxy resolves), the proxy urllib's convention picks for a
URL, and the files of downloads. It decides nothing: every byte it writes, the core wrote, and the
core keeps every time (it closes a stream it gives up on, and cancels a download's file). As
node/opamp/src/streams.ts does for Node.

One registry (`_InFlight`) owns everything under way: each stream (its transport, and the task
connecting it or starting its TLS), each name being resolved, each file's writes, and each
timer (a closed stream's linger). A close, from the core or at shutdown, ends what it names at
once; a connection or a TLS handshake that completes after its stream was closed is aborted as
it arrives, and reported to no one. Shutdown waits for file work at most SHUTDOWN_S.

Download files are written off the event loop (a worker thread), one write at a time per file,
in the order the bytes came: what is queued meanwhile goes in the next write, and is hashed as
it is written. Each download ends here once, answering the shell's download_done (synced, then
downloaded; or download_failed if a write failed: what came after is dropped, and the file
deleted), or not at all when the core discards or cancels it first: then the file goes once the
write in flight is over, and nothing more is said about it.

Backpressure, both ways:
- Socket to file. Once more than HIGH_WATER bytes wait for a file's writes, the stream that
  brought the last of them stops reading (its spliced bytes, or the data the core turned into
  download_data just then), until they are down to LOW_WATER. Nothing is dropped or reordered:
  the kernel's window stops the server.
- Core to socket. The core writes without hearing back (it writes one request or message at a
  time, plus answers to the peer's pings and closes). While a transport's write buffer is over
  its high-water mark (asyncio's pause_writing), the stream reads nothing more, so a peer that
  doesn't read hears nothing more to answer; and a write that finds more than MAX_UNSENT bytes
  still unsent fails the stream (stream_failed: the core reconnects, as for any lost
  connection) rather than buffering without end.
"""

from __future__ import annotations

import asyncio
import hashlib
import os
import socket
from collections import deque
from collections.abc import Callable, Coroutine
from dataclasses import dataclass, field
from typing import Any

from ._executor import download_path, lossy
from ._files import REAL, Files
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
#: The most buffers one file write takes.
MAX_WRITEV = 256
#: Bytes waiting for a file's writes beyond which the stream they come from stops reading...
HIGH_WATER = 4 << 20
#: ...until they are down to this.
LOW_WATER = 1 << 20
#: Bytes written to a stream and not yet taken by the peer beyond which the next write fails it.
MAX_UNSENT = 16 << 20
#: How long shutdown waits for file work (a write in flight, a deletion) before it returns.
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
        #: Why it is not reading: how many files it waits for, and a full write buffer.
        self.holds = 0
        self.blocked = False
        #: Its next bytes go to a download's file: which download, and how many are left.
        self.splice: list[int] | None = None
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
    (connects, TLS handshakes, resolutions, file work), timers, and closed transports still
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
        s.splice = None
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


@dataclass(eq=False)
class _File:
    """A download's file."""

    id: int
    path: str
    fd: int | None = None
    #: Bytes waiting for the next write, in order.
    queue: deque[bytes] = field(default_factory=deque)
    #: How many bytes are queued or being written.
    unwritten: int = 0
    #: Streams not reading until the bytes waiting are down to LOW_WATER.
    paused: set[_Stream] = field(default_factory=set)
    #: Of the bytes written (touched only by the write in flight).
    hash: Any = field(default_factory=hashlib.sha256)
    #: A write is running (the pump): only it touches fd and hash.
    busy: bool = False
    #: The file was made (by this: only then is it this one's to delete).
    made: bool = False
    #: download_done came: sync, close and answer once everything queued is written.
    done: bool = False
    #: Discarded or cancelled (or shut down): delete it, say nothing.
    gone: bool = False
    #: A write failed: the file is deleted, and download_done is answered with this.
    error: str | None = None


def _open_file(files: Files, path: str) -> int:
    """A download's file, made new (never one already there, a crashed run's say), in its
    directory, made if need be: both this user's only."""
    files.mkdir(os.path.dirname(path))
    return files.create(path)


def _write_all(files: Files, fd: int, bufs: list[bytes], digest: Any) -> None:
    """Write `bufs` in full, hashing each byte once it is written (in a worker thread)."""
    views = [memoryview(b) for b in bufs]
    while views:
        n = files.write(fd, views[:MAX_WRITEV])
        if n <= 0:
            raise OSError("the file took none of the bytes written to it")
        while views and n > 0:
            v = views[0]
            if n >= len(v):
                digest.update(v)
                n -= len(v)
                views.pop(0)
            else:
                digest.update(v[:n])
                views[0] = v[n:]
                n = 0


def _sync_close(files: Files, fd: int) -> None:
    try:
        files.sync(fd)
    finally:
        files.close(fd)


def _discard(files: Files, fd: int | None, path: str | None) -> None:
    """Close `fd`, if open, and delete `path`, if given (a file this made)."""
    if fd is not None:
        try:
            files.close(fd)
        except OSError:
            pass
    if path is not None:
        try:
            files.remove(path)
        except OSError:
            pass


class Streams:
    """Carries out core mode's effects: `do` takes those that are its own."""

    def __init__(self, deliver: Deliver, transport: Transport, state_file: str | None, *, client: str = "",
                 connect: Callable[..., Coroutine[Any, Any, tuple[asyncio.BaseTransport, Any]]] | None = None,
                 start_tls: Callable[..., Coroutine[Any, Any, asyncio.BaseTransport | None]] | None = None,
                 getaddrinfo: Callable[..., Coroutine[Any, Any, list[Any]]] | None = None,
                 files: Files | None = None) -> None:
        self._deliver = deliver
        #: The file system download files go through (journaled with the core tap on).
        self._fs = files or REAL
        #: The downloads whose file was made, answered and not yet deleted (a discard deletes it).
        self._answered: set[int] = set()
        self._transport = transport
        self._state_file = state_file
        #: The client's name for its download files (Executor.client): one client never touches
        #: another's.
        self._client = client
        loop = asyncio.get_running_loop()
        # The loop's, unless a test stands in for them.
        self._connect = connect or loop.create_connection
        self._start_tls = start_tls or loop.start_tls
        self._getaddrinfo = getaddrinfo or loop.getaddrinfo
        self._in = _InFlight()
        self._files: dict[int, _File] = {}
        #: The stream whose bytes the core is being given (download_data then came from it).
        self._reading: _Stream | None = None

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
        elif kind == "stream_splice":
            sp = e.stream_splice
            if (s := self._in.streams.get(sp.id)) is not None:
                s.splice = [sp.download, sp.bytes]
        elif kind == "download_data":
            self._append(e.download_data.id, e.download_data.data, self._reading)
        elif kind == "download_done":
            f = self._file(e.download_done.id)
            f.done = True
            self._pump(f)
        elif kind in ("discard", "cancel"):
            # A download's file the core is done with, or gave up on (cancel: its stream is closed
            # already). A save's cancel is the Executor's: this says it isn't only this one's.
            id_ = getattr(e, kind).id
            f = self._files.get(id_)
            if f is not None:
                f.gone = True  # deleted once the write in flight is over
                self._pump(f)
            elif id_ in self._answered and self._state_file is not None and not self._in.closed:
                # Answered already: only its file is left.
                self._answered.discard(id_)
                path = download_path(self._state_file, self._client, id_)
                self._in.spawn(asyncio.to_thread(_discard, self._fs, None, path))
            return kind == "discard"
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
            data = memoryview(value)
            # While a splice lasts: hearing one done, the shell may ask for the next, and the bytes
            # already read after the first are then that one's (what the core asks in answer to an
            # input about a stream applies to the very next bytes: RULE-SHELL-05).
            while s.splice is not None:
                # Body bytes: straight to the file, past the core.
                download, left = s.splice
                n = min(left, len(data))
                self._append(download, bytes(data[:n]), s)
                s.splice[1] -= n
                data = data[n:]
                if s.splice[1] == 0:
                    s.splice = None
                    self._from(s, corepb.Input(stream_spliced=corepb.Id(id=s.id)))
                # (Hearing the splice done, the shell may have closed the stream: nothing more.)
                if not data or not self._mine(s):
                    return
            self._from(s, corepb.Input(stream_data=corepb.WsMessage(id=s.id, data=bytes(data))))
        elif kind == "eof":
            self._deliver(corepb.Input(stream_eof=corepb.Id(id=s.id)))
        else:
            # Gone, with an error or without (after an end, or a reset): the core must hear it.
            self._in.close(s, abort=True)
            self._deliver(_failed(s.id, _detail(value, "the connection closed")))

    def _from(self, s: _Stream, i: corepb.Input) -> None:
        """Deliver `i`, what `s` read: the download_data the core makes of it came from `s`."""
        self._reading = s
        try:
            self._deliver(i)
        finally:
            self._reading = None

    # Files.

    def _file(self, id_: int) -> _File:
        f = self._files.get(id_)
        if f is None:
            f = _File(id_, download_path(self._state_file or "", self._client, id_))
            self._files[id_] = f
        return f

    def _append(self, id_: int, data: bytes, src: _Stream | None) -> None:
        """Queue `data` for the end of download `id_`'s file (made at the first); `src`: the
        stream it came from, held while too much waits."""
        f = self._file(id_)
        # After a failure (or a discard), the bytes go nowhere.
        if f.error is not None or f.gone or not data:
            return
        f.queue.append(bytes(data))
        f.unwritten += len(data)
        if src is not None and f.unwritten > HIGH_WATER and src not in f.paused:
            f.paused.add(src)
            src.hold()
        self._pump(f)

    @property
    def unwritten(self) -> int:
        """Bytes waiting for files' writes, all of them (for tests)."""
        return sum(f.unwritten for f in self._files.values())

    def _unpause(self, f: _File) -> None:
        for s in f.paused:
            s.release()
        f.paused.clear()

    def _pump(self, f: _File) -> None:
        """Carry `f` forward (unless that is under way): write what is queued; then, if it is done,
        sync and answer; if it is gone or failed, delete it."""
        if f.busy:
            return
        f.busy = True

        def again(_task: asyncio.Task) -> None:
            f.busy = False
            # What came while it finished (a download_done or a discard the answer led to).
            if self._files.get(f.id) is f and (f.gone or f.done or (f.error is None and f.queue)):
                self._pump(f)

        self._in.spawn(self._run(f)).add_done_callback(again)

    async def _run(self, f: _File) -> None:
        try:
            while not f.gone and f.error is None and (f.queue or (f.done and f.fd is None)):
                if f.fd is None:
                    f.fd = await asyncio.to_thread(_open_file, self._fs, f.path)
                    f.made = True
                    continue
                bufs = [f.queue.popleft() for _ in range(min(MAX_WRITEV, len(f.queue)))]
                await asyncio.to_thread(_write_all, self._fs, f.fd, bufs, f.hash)
                f.unwritten -= sum(len(b) for b in bufs)
                if f.unwritten <= LOW_WATER:
                    self._unpause(f)
            if f.gone or f.error is not None or not f.done:
                if f.gone or f.error is not None:
                    await self._remove(f)
                return
            # Complete: durable before the core hears of it.
            fd, f.fd = f.fd, None
            assert fd is not None
            await asyncio.to_thread(_sync_close, self._fs, fd)
            if f.gone:
                await self._remove(f)
                return
            del self._files[f.id]
            self._answered.add(f.id)
            self._tell(corepb.Input(downloaded=corepb.Downloaded(id=f.id, sha256=f.hash.digest())))
        except OSError as e:
            f.error = f"writing {f.path}: {e}"
            await self._remove(f)

    async def _remove(self, f: _File) -> None:
        """Close and delete a file that failed or is gone, then answer download_done if it came."""
        # Nothing more is written: the streams it held read on (into nothing, until they end).
        f.queue.clear()
        f.unwritten = 0
        self._unpause(f)
        fd, f.fd = f.fd, None
        await asyncio.to_thread(_discard, self._fs, fd, f.path if f.made else None)
        if f.gone:
            if self._files.get(f.id) is f:
                del self._files[f.id]
        elif f.done:
            del self._files[f.id]
            self._tell(corepb.Input(download_failed=corepb.Failed(id=f.id, detail=lossy(f.error or ""))))

    def _tell(self, i: corepb.Input) -> None:
        """Report `i`, unless this has shut down."""
        if not self._in.closed:
            self._deliver(i)

    async def shutdown(self, timeout: float | None = None) -> None:
        """Drop every stream and resolution, and every file not complete (once its write in
        flight is over): waits for file work `timeout` seconds at most (None: SHUTDOWN_S)."""
        if timeout is None:
            timeout = SHUTDOWN_S
        self._in.stop_all()
        for f in self._files.values():
            f.gone = True
            self._pump(f)
        loop = asyncio.get_running_loop()
        deadline = loop.time() + timeout
        while self._in.tasks and (left := deadline - loop.time()) > 0:
            await asyncio.wait(list(self._in.tasks), timeout=left)
