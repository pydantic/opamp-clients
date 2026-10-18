# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Carries out the core's effects with aiohttp and reports what happened as inputs, each naming
the effect it belongs to. It decides nothing about OpAMP and keeps no time of its own: the core
builds every request and reads every response, gives each effect its deadline, and cancels what
it gives up on (Effect.cancel), after which nothing more about it is reported."""

from __future__ import annotations

import asyncio
import collections
import concurrent.futures
import hashlib
import os
import secrets
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from urllib.parse import urljoin, urlsplit

import aiohttp
import yarl
from multidict import CIMultiDict

from ._files import REAL, Files, write_all
from ._proto.opamp.core.v1 import core_pb2 as corepb
from ._transport import Transport

Deliver = Callable[[corepb.Input], None]


def _headers(hs) -> CIMultiDict[str]:
    """The headers of a request the core asks for, as aiohttp takes them. The core has checked
    them already: every header list it asks for meets the rule an offered header meets, or it
    doesn't ask."""
    return CIMultiDict((h.name, h.value) for h in hs)


def _from_headers(hs) -> list[corepb.Header]:
    return [corepb.Header(name=lossy(k), value=lossy(v)) for k, v in hs.items()]


def lossy(s: str) -> str:
    """`s` as UTF-8, what isn't replaced by U+FFFD, as core mode's own HTTP reader takes header
    values (String::from_utf8_lossy). aiohttp keeps the bytes of a header that aren't UTF-8 as lone
    surrogates (surrogateescape), as Python does those of the environment and of file names; a
    protobuf string refuses them, and building the input raised: every string the client hands the
    core from outside it (what a server sent, the environment, an error's text) goes through this."""
    try:
        s.encode("utf-8")
        return s
    except UnicodeEncodeError:
        pass
    try:
        raw = s.encode("utf-8", "surrogateescape")  # the bytes as they came
    except UnicodeEncodeError:
        raw = s.encode("utf-8", "surrogatepass")  # a lone surrogate of another origin
    return raw.decode("utf-8", "replace")


async def _no_redirects(request: aiohttp.ClientRequest, handler) -> aiohttp.ClientResponse:
    """Stop a WebSocket upgrade at a 3xx: ws_connect follows redirects and has no switch for it.
    (The POST has allow_redirects=False.) The core takes a 3xx as an error."""
    response = await handler(request)
    if 300 <= response.status < 400 and request.headers.get("Upgrade", "").lower() == "websocket":
        raise aiohttp.WSServerHandshakeError(request.request_info, (), status=response.status,
                                             message="redirected", headers=response.headers)
    return response


def _url(url: str) -> yarl.URL:
    """`url` (one the core checked) as aiohttp takes it: as it is written, sent byte for byte. A
    plain string would be requoted by yarl (%41 sent as A, %3D as =, %e1 as %E1), another URL
    to a server that reads its target as written (tests/shell-tables/urls.json)."""
    return yarl.URL(url, encoded=True)


def _unix_ms() -> int:
    return time.time_ns() // 1_000_000


def save_state(path: str, data: bytes) -> None:
    """Replace what is saved at `path` with `data`, all or nothing: a new file, synced, renamed
    over the old one. Readable by the agent's user only."""
    _save(REAL, path, data)


def _save(files: Files, path: str, data: bytes) -> None:
    """save_state, through `files` (the real file system, journaled with the core tap on). The
    recipe is docs/state-file.md's."""
    tmp = path + ".tmp"
    try:
        # A new file, never one left behind (which could be anyone's, with any mode).
        try:
            files.remove(tmp)
        except FileNotFoundError:
            pass
        fd = files.create(tmp)
        try:
            # All of it: a write may take only part (a disk filling up), and the rest follows.
            write_all(files, fd, data)
            files.sync(fd)
        finally:
            files.close(fd)
        files.rename(tmp, path)
        # The rename itself must reach the disk (directories can't be opened on Windows).
        files.sync_dir(os.path.dirname(path) or ".")
    except OSError as e:
        try:
            files.remove(tmp)
        except OSError:
            pass
        raise OSError(f"saving {path}: {e}") from e


# One thread saves to each state file, for every client in the process: saves land one at a
# time, in the order asked, beside the event loop (a slow disk holds up neither heartbeats nor
# stop), and a client starting on the file reads it once the saves before it have landed. The
# thread lasts while the file has work waiting, then ends: a process that runs clients on many
# files keeps no thread for each (the nightly callback model's thousands of runs ran out of them).
_savers: dict[str, collections.deque] = {}  # the work waiting, per file that has a thread
_savers_lock = threading.Lock()


class _Saver(concurrent.futures.Executor):
    """The work for one state file, carried out in order by the file's thread."""

    def __init__(self, key: str) -> None:
        self._key = key

    def submit(self, fn, /, *args, **kwargs) -> concurrent.futures.Future:
        future: concurrent.futures.Future = concurrent.futures.Future()
        with _savers_lock:
            work = _savers.get(self._key)
            if work is None:
                work = _savers[self._key] = collections.deque()
                threading.Thread(target=_save_all, args=(self._key, work), name="opamp-save").start()
            work.append((future, fn, args, kwargs))
        return future


def _save_all(key: str, work: collections.deque) -> None:
    """Carry out a file's work, one at a time, and end when there is none: the next work starts
    a new thread, after this one's last has landed."""
    while True:
        with _savers_lock:
            if not work:
                if _savers.get(key) is work:
                    del _savers[key]
                return
            future, fn, args, kwargs = work.popleft()
        if not future.set_running_or_notify_cancel():
            continue  # cancelled before it started: never carried out
        try:
            result = fn(*args, **kwargs)
        except BaseException as e:  # noqa: BLE001 - the caller's, given to the caller
            future.set_exception(e)
        else:
            future.set_result(result)


def _saver(path: str) -> concurrent.futures.Executor:
    return _Saver(os.path.abspath(path))


def _forked() -> None:
    """In a forked child the savers' threads are gone: new ones start as needed."""
    global _savers_lock
    _savers_lock = threading.Lock()
    _savers.clear()


if hasattr(os, "register_at_fork"):
    os.register_at_fork(after_in_child=_forked)


def read_state(path: str | None) -> bytes:
    """What is saved at `path` (nothing if there is no file yet), once every save to it already
    asked for has landed."""
    if path is None:
        return b""

    def read() -> bytes:
        try:
            with open(path, "rb") as f:
                return f.read()
        except FileNotFoundError:
            # No file yet; but no directory for it is a setting that can't work: every save
            # would fail, unseen, and the agent would be a new one after each restart.
            if not os.path.isdir(os.path.dirname(path) or "."):
                raise
            return b""

    return _saver(path).submit(read).result()


def clear_downloads(files: Files, state_file: str) -> concurrent.futures.Future:
    """Delete the downloads a run before left (the core downloads again what it still needs),
    on the state file's saver: beside the loop, and before any save of the client starting. The
    deletion, which a download waits for before making its file (`AfterClear`)."""
    def clear() -> None:
        try:
            files.clear(downloads_dir(state_file))
        except OSError:
            pass  # what is left is never reused: download files are made new

    return _saver(state_file).submit(clear)


def downloads_dir(state_file: str) -> str:
    """Where package files are downloaded, beside the state file."""
    return state_file + ".downloads"


def download_path(state_file: str, client: str, id_: int) -> str:
    """The file of download `id_` of client `client` (ids start again in every client: one client
    never touches another's files)."""
    return os.path.join(downloads_dir(state_file), f"{client}-{id_}")


# The ports the url crate takes as a scheme's default (the Rust client reports those as 0).
_DEFAULT_PORTS = {"http": 80, "https": 443, "ws": 80, "wss": 443, "ftp": 21}


def location(requested: str, given: str) -> tuple[str, corepb.Origin | None]:
    """Where a redirect points: its Location `given` resolved against the URL `requested` by
    urllib.parse, and that URL's origin (port 0 for the scheme's default). A Location that doesn't
    resolve goes as given, with no origin: the core refuses it."""
    given = lossy(given)
    try:
        url = urljoin(requested, given)
        u = urlsplit(url)
        port = u.port
    except ValueError:
        return given, None
    scheme = u.scheme.lower()
    if port is None or port == _DEFAULT_PORTS.get(scheme):
        port = 0
    return url, corepb.Origin(scheme=scheme, host=u.hostname or "", port=port)


class _Conn:
    """A WebSocket connection, open or closing: a reader reporting what arrives, a writer carrying
    out the sends and the close in order."""

    def __init__(self, ws: aiohttp.ClientWebSocketResponse, limit: int) -> None:
        self.ws = ws
        self.limit = limit  # the largest message taken, in bytes (0: none at all)
        self.outbox: asyncio.Queue[bytes | None] = asyncio.Queue()  # data to send; None: close
        self.closing = False  # the core said ws_close

    def drop(self) -> None:
        """Close the socket at once, without a closing handshake."""
        self.ws._response.close()  # aiohttp's own abort: the connection is closed, not reused


@dataclass
class _Entry:
    tasks: list[asyncio.Task] = field(default_factory=list)
    conn: _Conn | None = None
    on_cancel: Callable[[], None] | None = None  # what else a cancel undoes (a download's file)


class InFlight:
    """The effects under way, by id: the one place that decides whether what happened reaches the
    core. Everything here runs on the event loop, so each check-and-remove is atomic.

    - Every effect under way (a request, a connection being opened, an open or closing
      connection, a download, a save) has one entry, holding its tasks and, for a connection,
      its socket.
    - ``cancel`` removes the entry and aborts its work at once: its tasks are cancelled, its
      socket dropped, a download's file deleted.
    - Only ``report`` and ``finish`` deliver, and only while the entry is there; ``finish``
      removes it with the same check, so an effect's final result is delivered once, and
      nothing about it after a cancel.
    - A resource ready after its entry has gone (a connection that opens after its cancel, or
      after close) is dropped at once (``adopt``).
    - After ``close`` nothing registers: new work is dropped before it starts.
    - ``close`` cancels everything, then waits for the tasks to end, within a bound.
    """

    def __init__(self, deliver: Deliver) -> None:
        self._deliver = deliver
        self._entries: dict[int, _Entry] = {}
        self._closed = False

    def start(self, id_: int, coro, on_cancel: Callable[[], None] | None = None) -> None:
        """Run `coro` as effect `id_`."""
        if self._closed:
            coro.close()
            return
        entry = self._entries[id_] = _Entry(on_cancel=on_cancel)
        entry.tasks.append(asyncio.get_running_loop().create_task(coro))

    def report(self, id_: int, input_: corepb.Input) -> bool:
        """Deliver something effect `id_` has to say, if it is still under way."""
        if id_ not in self._entries:
            return False
        self._deliver(input_)
        return True

    def finish(self, id_: int, input_: corepb.Input) -> bool:
        """Deliver effect `id_`'s last word, if it is still under way, and forget it (dropping
        its socket, if it has one: whatever ended it, it is done). False: it had gone."""
        entry = self._entries.pop(id_, None)
        if entry is None:
            return False
        self._stop(entry, cancel=False)
        self._deliver(input_)
        return True

    def adopt(self, id_: int, conn: _Conn, *coros) -> bool:
        """Connection `id_` has opened: keep it, and run `coros` (its reader and writer) as part
        of it. Opened after its cancel or after close: dropped at once."""
        entry = self._entries.get(id_)
        if entry is None or self._closed:
            conn.drop()
            for coro in coros:
                coro.close()
            return False
        entry.conn = conn
        loop = asyncio.get_running_loop()
        entry.tasks += [loop.create_task(coro) for coro in coros]
        return True

    def conn(self, id_: int) -> _Conn | None:
        entry = self._entries.get(id_)
        return entry.conn if entry is not None else None

    def cancel(self, id_: int) -> None:
        """Give up on effect `id_` at once: nothing more about it is delivered."""
        entry = self._entries.pop(id_, None)
        if entry is not None:
            self._stop(entry, cancel=True)

    @staticmethod
    def _stop(entry: _Entry, cancel: bool) -> None:
        current = asyncio.current_task()
        for task in entry.tasks:
            if task is not current:
                task.cancel()
        if entry.conn is not None:
            entry.conn.drop()
        if cancel and entry.on_cancel is not None:
            entry.on_cancel()

    async def close(self, timeout: float | None) -> None:
        """Take nothing more, cancel everything, and wait up to `timeout` seconds for it to end."""
        self._closed = True
        tasks = [t for e in self._entries.values() for t in e.tasks]
        for id_ in list(self._entries):
            self.cancel(id_)
        if tasks:
            await asyncio.wait(tasks, timeout=timeout)


class _DownloadFile:
    """A download's file, written beside the loop (in worker threads: a slow disk holds up
    neither heartbeats nor the application), one operation at a time, in order. Made new, and
    deleted only if it was made. Safe to cancel: the download's task may be cancelled while an
    operation runs; the operation still ends in its thread, noting what it did here (the file's
    descriptor, whether it was made), and `discard` waits for it before closing and deleting the
    file. Each operation is journaled (with the tap on) in its thread, before the task goes on:
    before the core hears of the download's result."""

    def __init__(self, files: Files, path: str) -> None:
        self.files, self.path = files, path
        self.fd: int | None = None
        self.made = False
        self.gone = False
        self._op: asyncio.Future | None = None  # the operation in flight, or the last

    async def _run(self, fn: Callable[[], None]) -> None:
        if self.gone:
            raise OSError("the download's file was discarded")
        op = self._op = asyncio.ensure_future(asyncio.to_thread(fn))
        # Its error is the task's, or (the task cancelled) no one's.
        op.add_done_callback(lambda f: f.cancelled() or f.exception())
        await asyncio.shield(op)

    async def open(self) -> None:
        def run() -> None:
            self.files.mkdir(os.path.dirname(self.path))
            # Made new: never a file already there (left by a run that crashed, say).
            self.fd = self.files.create(self.path)
            self.made = True

        await self._run(run)

    async def write(self, chunk: bytes) -> None:
        fd = self.fd
        assert fd is not None
        await self._run(lambda: write_all(self.files, fd, chunk))

    async def finish(self) -> None:
        """Every byte is written: flush the file to the disk (durable before the core hears of
        it), and close it."""
        def run() -> None:
            fd = self.fd
            assert fd is not None
            try:
                self.files.sync(fd)
            finally:
                self.fd = None
                self.files.close(fd)

        await self._run(run)

    async def discard(self) -> None:
        """Once the operation in flight is over: close the file, and delete it if it was made."""
        self.gone = True
        if self._op is not None:
            await asyncio.wait([self._op])
        fd, self.fd = self.fd, None
        path = self.path if self.made else None
        self.made = False
        await asyncio.to_thread(_discard_file, self.files, fd, path)


def _discard_file(files: Files, fd: int | None, path: str | None) -> None:
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


class Executor:
    """Carries out effects, one kind of I/O each; what becomes of their results is InFlight's."""

    def __init__(self, deliver: Deliver, state_file: str | None = None, transport: Transport | None = None,
                 files: Files | None = None, *,
                 sessions: Callable[[str], aiohttp.ClientSession] | None = None) -> None:
        self._state_file = state_file
        #: The file system saves and download files go through (journaled with the core tap on).
        self._files = files or REAL
        #: The downloads' files, by id, until deleted (a failure, a cancel, a discard).
        self._dfiles: dict[int, _DownloadFile] = {}
        #: Deletions of download files under way: close waits for them (within its bound).
        self._file_work: set[asyncio.Task] = set()
        # TLS and the proxy, for requests, connections and downloads alike.
        self._transport = transport or Transport()
        # Tests only: makes the sessions instead ("opamp": requests and connections,
        # "download": package files).
        self._sessions = sessions
        self._session: aiohttp.ClientSession | None = None
        self._fetcher: aiohttp.ClientSession | None = None  # package downloads
        self._inflight = InFlight(deliver)
        # This client's name for its download files (core mode's Streams names them so too).
        self.client = secrets.token_hex(6)
        #: The URL of the OpAMP request or connection whose outcome came last (None: none yet).
        self.used: str | None = None

    def download_path(self, id_: int) -> str:
        """The file of download `id_`."""
        return download_path(self._state_file or "", self.client, id_)

    def _http(self) -> aiohttp.ClientSession:
        if self._session is None and self._sessions is not None:
            self._session = self._sessions("opamp")
        if self._session is None:
            # The core says what to send: no User-Agent or Accept-Encoding of aiohttp's own, and
            # nothing decompressed (the core asks for none). trust_env stays off: the transport
            # picks the proxy (the environment's included), and .netrc is not read. No timeout of
            # aiohttp's own: a request carries the core's, and the core cancels the rest.
            self._session = aiohttp.ClientSession(connector=self._transport.connector(),
                                                  skip_auto_headers=("User-Agent", "Accept-Encoding"),
                                                  auto_decompress=False, middlewares=(_no_redirects,),
                                                  timeout=aiohttp.ClientTimeout())
        return self._session

    def do(self, effect: corepb.Effect) -> None:
        kind = effect.WhichOneof("kind")
        if kind == "http_request":
            self._inflight.start(effect.http_request.id, self._post(effect.http_request))
        elif kind == "ws_open":
            self._inflight.start(effect.ws_open.id, self._open(effect.ws_open))
        elif kind == "ws_send":
            conn = self._inflight.conn(effect.ws_send.id)
            if conn is not None and not conn.closing:  # else it is going, and says so
                conn.outbox.put_nowait(effect.ws_send.data)
        elif kind == "ws_close":
            conn = self._inflight.conn(effect.ws_close.id)
            if conn is not None and not conn.closing:
                conn.closing = True
                conn.outbox.put_nowait(None)
        elif kind == "download":
            id_ = effect.download.id
            self._inflight.start(id_, self._download(effect.download), on_cancel=lambda: self._remove(id_))
        elif kind == "discard":
            self._remove(effect.discard.id)
        elif kind == "store":
            self._inflight.start(effect.store.id, self._store(effect.store))
        elif kind == "cancel":
            # Abort a request, a connection being opened or a download (deleting its file); drop
            # a connection, open or closing; stop waiting for a save (one already writing lands
            # whole, one not yet started never does). Nothing more about it is reported.
            self._inflight.cancel(effect.cancel.id)

    def _remove(self, id_: int) -> asyncio.Task | None:
        """Delete download `id_`'s file (if this made it), beside the loop, once the operation
        on it in flight is over: the deletion, if there is one."""
        f = self._dfiles.pop(id_, None)
        if f is None:
            return None
        task = asyncio.get_running_loop().create_task(f.discard())
        self._file_work.add(task)
        task.add_done_callback(self._file_work.discard)
        return task

    async def _store(self, s: corepb.Store) -> None:
        """Save beside the loop, all or nothing, after the saves asked for before."""
        try:
            if self._state_file is None:
                raise OSError("no state file")
            await asyncio.get_running_loop().run_in_executor(_saver(self._state_file), _save, self._files,
                                                             self._state_file, s.data)
        except Exception as e:  # noqa: BLE001 - whatever it is, the save has failed
            self._inflight.finish(s.id, corepb.Input(store_failed=corepb.Failed(id=s.id, detail=lossy(str(e)))))
        else:
            self._inflight.finish(s.id, corepb.Input(stored=corepb.Id(id=s.id)))

    def _outcome(self, url: str, id_: int, input_: corepb.Input) -> None:
        """Hand the core the outcome of a request or a connection to url, having noted url as the
        latest one tried: a notification of the outcome finds it in place. An outcome the core no
        longer waits for (its effect was cancelled) leaves the URL noted before."""
        before, self.used = self.used, url
        if not self._inflight.finish(id_, input_):
            self.used = before

    async def _post(self, r: corepb.HttpRequest) -> None:
        def failed(detail: str) -> corepb.Input:
            return corepb.Input(http_failed=corepb.Failed(id=r.id, detail=lossy(detail)))

        limit = r.max_response_bytes
        try:
            timeout = aiohttp.ClientTimeout(total=r.timeout_ms / 1000 or None)
            async with self._http().post(_url(r.url), data=r.body, headers=_headers(r.headers), timeout=timeout,
                                         allow_redirects=False) as resp:
                # read(n) returns what has come, up to n bytes: read on until the end (or one
                # byte past the limit).
                body = bytearray()
                while len(body) <= limit and (piece := await resp.content.read(limit + 1 - len(body))):
                    body += piece
                if len(body) > limit:
                    self._outcome(r.url, r.id, failed(f"response over {limit} bytes"))
                    return
                self._outcome(r.url, r.id, corepb.Input(http_response=corepb.HttpResponse(
                    id=r.id, status=resp.status, headers=_from_headers(resp.headers), body=bytes(body),
                    unix_ms=_unix_ms())))
        # Whatever aiohttp raises (a socket error, a header it refuses, a request it can't make at
        # all, as at interpreter exit), the core hears the request failed, rather than waiting on
        # it. (A cancel is a CancelledError: not an Exception.)
        except Exception as e:  # noqa: BLE001
            self._outcome(r.url, r.id, failed(str(e) or type(e).__name__))

    async def _open(self, o: corepb.WsOpen) -> None:
        try:
            # No timeout of aiohttp's own, for the handshake or the close: the core keeps them.
            # aiohttp takes 0 as no limit, and nothing over 2^32 - 1: it gets 1 for 0 (the reader
            # holds the rest to the core's limit exactly), and at most 2^32 - 1.
            size = min(max(o.max_message_bytes, 1), 2**32 - 1)
            ws = await self._http().ws_connect(_url(o.url), headers=_headers(o.headers), max_msg_size=size,
                                               autoclose=False, autoping=True, timeout=aiohttp.ClientWSTimeout())
        except aiohttp.WSServerHandshakeError as e:
            self._outcome(o.url, o.id, corepb.Input(ws_failed=corepb.WsFailed(
                id=o.id, status=e.status, headers=_from_headers(e.headers or {}), detail=lossy(str(e)),
                unix_ms=_unix_ms())))
            return
        except Exception as e:  # noqa: BLE001 - whatever it is, the open has failed
            self._outcome(o.url, o.id, corepb.Input(ws_failed=corepb.WsFailed(
                id=o.id, detail=lossy(str(e) or type(e).__name__), unix_ms=_unix_ms())))
            return
        conn = _Conn(ws, o.max_message_bytes)
        if self._inflight.adopt(o.id, conn, self._read(o.id, conn), self._write(o.id, conn)):
            self.used = o.url
            self._inflight.report(o.id, corepb.Input(ws_opened=corepb.Id(id=o.id)))

    async def _read(self, id_: int, c: _Conn) -> None:
        """Report what arrives until the server's Close frame, or until the connection goes."""
        while True:
            msg = await c.ws.receive()
            if c.closing:  # our close is under way: the writer reports how it ends
                return
            if msg.type == aiohttp.WSMsgType.BINARY:
                if len(msg.data) > c.limit:
                    detail = f"a message of {len(msg.data)} bytes, over {c.limit}"
                    self._inflight.finish(id_, corepb.Input(ws_lost=corepb.Failed(id=id_, detail=lossy(detail))))
                    return
                self._inflight.report(id_, corepb.Input(ws_message=corepb.WsMessage(id=id_, data=msg.data)))
            elif msg.type == aiohttp.WSMsgType.CLOSE:
                # The server's Close frame: answered when the core says (ws_close).
                self._inflight.report(id_, corepb.Input(ws_closed=corepb.WsClosed(id=id_, code=msg.data or 0)))
                return
            elif msg.type in (aiohttp.WSMsgType.TEXT, aiohttp.WSMsgType.PING, aiohttp.WSMsgType.PONG):
                continue  # text means nothing to OpAMP
            else:  # gone without a Close frame
                detail = str(c.ws.exception() or "connection dropped")
                self._inflight.finish(id_, corepb.Input(ws_lost=corepb.Failed(id=id_, detail=lossy(detail))))
                return

    async def _write(self, id_: int, c: _Conn) -> None:
        """Send what the core says, in order, then close: the closing handshake takes as long as
        it takes (the core cancels one that takes too long)."""
        while (data := await c.outbox.get()) is not None:
            try:
                await c.ws.send_bytes(data)
            except Exception as e:  # noqa: BLE001 - whatever it is, the connection is lost
                detail = lossy(str(e) or type(e).__name__)
                self._inflight.finish(id_, corepb.Input(ws_lost=corepb.Failed(id=id_, detail=detail)))
                return
        await c.ws.close(code=1000)
        code = c.ws.close_code
        if c.ws.exception() is None and code is not None and code != 1006:
            self._inflight.finish(id_, corepb.Input(ws_closed=corepb.WsClosed(id=id_, code=code)))
        else:
            detail = str(c.ws.exception() or "connection dropped while closing")
            self._inflight.finish(id_, corepb.Input(ws_lost=corepb.Failed(id=id_, detail=lossy(detail))))

    async def _download(self, d: corepb.Download) -> None:
        """GET a package file into its path, whatever the status, hashing it as it goes; on
        failure nothing is left. No redirect is followed: the core decides."""
        try:
            status, sha256, where = await self._fetch(d)
        except Exception as e:  # noqa: BLE001 - whatever it is, the download has failed
            # Nothing left of it before the core hears (a cancel meanwhile lets it finish).
            if (removing := self._remove(d.id)) is not None:
                await asyncio.shield(removing)
            self._inflight.finish(d.id, corepb.Input(download_failed=corepb.Failed(
                id=d.id, detail=lossy(str(e) or type(e).__name__))))
            return
        response = corepb.DownloadResponse(id=d.id, status=status, sha256=sha256)
        if where is not None:
            response.location, origin = where
            if origin is not None:
                response.location_origin.CopyFrom(origin)
        if not self._inflight.finish(d.id, corepb.Input(download_response=response)):
            # Cancelled (or closed) while it finished: its cancel deleted the file before it was
            # written; nothing is to be left of it.
            self._remove(d.id)

    async def _fetch(self, d: corepb.Download) -> tuple[int, bytes, tuple[str, corepb.Origin | None] | None]:
        path = self.download_path(d.id)
        if self._fetcher is None and self._sessions is not None:
            self._fetcher = self._sessions("download")
        if self._fetcher is None:
            self._fetcher = aiohttp.ClientSession(connector=self._transport.connector(), timeout=aiohttp.ClientTimeout())
        timeout = aiohttp.ClientTimeout(total=d.timeout_ms / 1000 or None)
        async with self._fetcher.get(_url(d.url), headers=_headers(d.headers), timeout=timeout,
                                     allow_redirects=False) as resp:
            where = None
            if 300 <= resp.status < 400 and (given := resp.headers.get("Location")) is not None:
                where = location(d.url, given)
            # Its file is known before it is made: a cancel from now on deletes it.
            f = self._dfiles[d.id] = _DownloadFile(self._files, path)
            await f.open()
            digest = hashlib.sha256()
            size = 0
            async for chunk in resp.content.iter_chunked(64 * 1024):
                size += len(chunk)
                if size > d.max_bytes:
                    raise aiohttp.ClientError(f"file over {d.max_bytes} bytes")
                digest.update(chunk)
                await f.write(chunk)
            await f.finish()
            return resp.status, digest.digest(), where

    async def close(self, timeout: float | None = None, *, release: float | None = None) -> None:
        """Release everything: whatever is still under way is dropped, reported to no one, and a
        save already writing lands. What was given up on (requests, connections, downloads and
        the deletion of their files, once the file operation in flight is over) is waited for
        `release` seconds at most (None: `timeout`); a save still writing, `timeout` seconds at
        most (None: for ever)."""
        loop = asyncio.get_running_loop()
        if release is None:
            release = timeout
        saves_by = loop.time() + timeout if timeout is not None else None
        released_by = loop.time() + release if release is not None else None
        await self._inflight.close(release)
        for session in (self._session, self._fetcher):
            if session is not None:
                await session.close()
        if self._file_work:
            # Download files cancelled: deleted once their operation in flight is over.
            left = None if released_by is None else max(0.0, released_by - loop.time())
            await asyncio.wait(list(self._file_work), timeout=left)
        if self._state_file is not None:
            settled = loop.run_in_executor(_saver(self._state_file), lambda: None)
            left = None if saves_by is None else max(0.0, saves_by - loop.time())
            try:
                await asyncio.wait_for(settled, left)
            except asyncio.TimeoutError:
                pass
