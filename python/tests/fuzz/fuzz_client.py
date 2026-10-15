# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Fuzz target (Atheris): the Python client's own code around the core, fed by a server whose every
answer is the fuzzer's and an application whose callback calls the client as the fuzzer says.

One input is one run of a real Client on the native core (its C interface: opamp_client._native),
on a seeded event loop with a virtual clock (tests/_sched_loop.py), in either mode:

- Python mode: the executor's aiohttp path over a fake session (responses in pieces, resets,
  error statuses with any headers, package downloads into the state file's directory; WebSocket
  upgrades taken or refused, messages, closes and drops);
- Rust mode: the core's own HTTP and WebSocket over _streams.py's connections, on an in-memory
  network, the server's bytes arriving in pieces (or any bytes at all, status line included),
  package files spliced or written by the streams' file writer.

The server answers each request with any bytes, a ServerToAgent built from the bytes (remote
configs, packages with files it serves, connection settings, commands, custom messages, errors,
a new instance UID) and perhaps damaged, an error status, or a reset. Each notification the
callback hears makes it call the client: statuses, remote config and package answers, custom
messages, stop. Then the application stops the client. What must hold, whatever came:

- the client never stops on its own with an error (a CoreError is this shell feeding the core
  something it refuses, or a trap; anything else is a bug here): the callback never raises;
- callbacks one at a time, on the loop's thread, none before start returned, none after stopped;
  stopped once, last; stop returns within stop_timeout (plus a margin), raising nothing;
- install_package names a file under the state file's downloads directory, there as the callback
  runs, whose SHA-256 is the offer's content hash when it has one;
- every answer the server gave reaches the core, unless the core gave up on its effect first;
- a heartbeat interval is never negative; nothing touches the loop from another thread; no
  temporary state file is left once stop has returned.

    uv run --with atheris python tests/fuzz/fuzz_client.py [libFuzzer flags] [corpus]

scripts/py_fuzz.sh runs it with the repository's time and memory limits; the seeds below and every
input under crashes/fuzz_client/ run with every pytest (tests/test_fuzz_inputs.py)."""

from __future__ import annotations

import asyncio
import base64
import contextvars
import hashlib
import os
import shutil
import struct
import sys
import tempfile
import threading
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(HERE), str(HERE.parent)]

try:
    import atheris
except ImportError:  # the seeds' replay under pytest: no instrumentation
    atheris = None

if atheris is not None and __name__ == "__main__":
    with atheris.instrument_imports(include=["opamp_client"]):
        import opamp_client._client  # noqa: F401

import aiohttp  # noqa: E402
from aiohttp import WSMessage, WSMsgType  # noqa: E402
import yarl  # noqa: E402
from multidict import CIMultiDict  # noqa: E402

import opamp_client._client as _client_mod  # noqa: E402
import opamp_client._core as _core_mod  # noqa: E402
import opamp_client._executor as _executor_mod  # noqa: E402
from _fuzzlib import CALLS, CAPABILITIES, CAPABILITY, Reader, Server, call, pieces  # noqa: E402
from _sched_loop import Network, SeededLoop  # noqa: E402
from _sched_world import FakeContent  # noqa: E402
from opamp_client import Client, Config, Notification, Proxy, Status, opamp_pb2 as pb  # noqa: E402
from opamp_client._executor import downloads_dir  # noqa: E402
from opamp_client._proto.opamp.core.v1 import core_pb2 as corepb  # noqa: E402

UNIX0 = 1_790_000_000_000
STOP_S = 5.0
MARGIN_S = 1.0
RUN_S = 600.0  # virtual seconds the client runs for, at most, before the application stops it
UID = bytes([0x46] * 16)
TAG = "X-Answer"  # Python mode: the header that names each answer the server gave


class Problem(AssertionError):
    """A property broken: the fuzzer's crash."""


class FixedCore(_core_mod.Core):
    """The real core with a fixed seed, entropy and wall clock: the same input is the same run."""

    def __init__(self, config: corepb.Config, *, _module: bytes | None = None) -> None:
        config.seed = 0x5EED
        config.entropy = bytes(range(32))
        config.unix_ms = UNIX0
        super().__init__(config, _module=_module)

    #: The run whose answers it notes (Python mode tags each with an X-Answer header).
    run: Run | None = None

    def step(self, now_ms: int, input_: corepb.Input) -> corepb.Output:
        kind = input_.WhichOneof("kind")
        if self.run is not None and kind in ("http_response", "ws_failed"):
            for h in getattr(input_, kind).headers:
                if h.name.lower() == TAG.lower():
                    self.run.reached.add(h.value)
        out = self._call(self._step, corepb.Step(now_ms=now_ms, input=input_, unix_ms=UNIX0 + now_ms),
                         _core_mod._STEP_IN)
        if self.run is not None:
            self.run.cancelled.update(e.cancel.id for e in out.effects if e.WhichOneof("kind") == "cancel")
        return out


# The effect whose task is running (Python mode): what the fake session tags its answers with.
EFFECT: contextvars.ContextVar[int] = contextvars.ContextVar("effect", default=-1)


def _named(method):
    async def run(self, effect):
        EFFECT.set(effect.id)
        return await method(self, effect)

    return run


_executor_mod.Executor._post = _named(_executor_mod.Executor._post)
_executor_mod.Executor._open = _named(_executor_mod.Executor._open)


# -- Python mode: a fake aiohttp session


def _aiohttp_headers(headers: list[tuple[bytes, bytes]]) -> CIMultiDict[str]:
    """Headers as aiohttp's parser hands them over: decoded as UTF-8, what isn't kept as lone
    surrogates (surrogateescape)."""
    return CIMultiDict((k.decode("utf-8", "surrogateescape"), v.decode("utf-8", "surrogateescape"))
                       for k, v in headers)


class _Response:
    def __init__(self, loop: SeededLoop, status: int, headers: list[tuple[bytes, bytes]]) -> None:
        self.status = status
        self.headers = _aiohttp_headers(headers)
        self.content = FakeContent(loop)


class _FakeWS:
    """A client WebSocket as aiohttp's behaves (autoclose off): each message the client sends is
    answered as the server says, with a message, its Close frame, or the connection dropped."""

    def __init__(self, run: Run) -> None:
        self.run = run
        self._inbox: list[WSMessage] = []
        self._waiter: asyncio.Future | None = None
        self.close_code: int | None = None
        self._exc: BaseException | None = None
        self._closed = False
        self._response = self  # aiohttp's ws._response.close(): drop the connection

    def _deliver(self, msg: WSMessage) -> None:
        self._inbox.append(msg)
        if self._waiter is not None and not self._waiter.done():
            self._waiter.set_result(None)

    def close(self, *args, **kwargs):  # noqa: D401 - both _response.close() and ws.close(code=)
        if args or kwargs:
            return self._close(*args, **kwargs)
        self._drop(ConnectionResetError("dropped"))
        return None

    def _drop(self, e: BaseException) -> None:
        if not self._closed:
            self._closed = True
            self._exc = self._exc or e
            self.close_code = 1006
            self._deliver(WSMessage(WSMsgType.ERROR, e, None))

    def exception(self) -> BaseException | None:
        return self._exc

    async def receive(self) -> WSMessage:
        while not self._inbox:
            if self._closed:
                return WSMessage(WSMsgType.CLOSED, None, None)
            self._waiter = asyncio.get_running_loop().create_future()
            await self._waiter
        msg = self._inbox.pop(0)
        if msg.type == WSMsgType.CLOSE:
            self.close_code = msg.data
        return msg

    async def send_bytes(self, data: bytes) -> None:
        if self._closed:
            raise aiohttp.ClientConnectionResetError("Cannot write to closing transport")
        await asyncio.sleep(0)
        how, what = self.run.server.ws_answer(bytes(data))
        if how == "drop":
            self._drop(ConnectionResetError("reset by the server"))
        elif how == "close":
            self._deliver(WSMessage(WSMsgType.CLOSE, what, ""))
        else:
            self._deliver(WSMessage(WSMsgType.BINARY, what, None))

    async def _close(self, *, code: int = 1000, message: bytes = b"") -> bool:
        if self._closed:
            return False
        self._closed = True
        self.close_code = code
        return True


class _Exchange:
    def __init__(self, run: Run, kind: str, url: str, body: bytes) -> None:
        self.run, self.kind, self.url, self.body = run, kind, url, body

    async def __aenter__(self) -> _Response:
        await asyncio.sleep(0)
        server = self.run.server
        how, status, headers, data, at = server.answer(self.body) if self.kind == "post" else server.download(self.url)
        if how != "ok":
            raise aiohttp.ClientConnectionError("connection reset by the server")
        if self.kind == "post":
            headers = [*headers, (TAG.encode(), self.run.tag())]
        resp = _Response(self.run.loop, status, headers)
        resp.content.feed(pieces(data, at))
        return resp

    async def __aexit__(self, et, e, tb) -> None:
        return None


class _Session:
    def __init__(self, run: Run) -> None:
        self.run = run

    def post(self, url, *, data, headers, timeout, allow_redirects) -> _Exchange:
        return _Exchange(self.run, "post", str(url), bytes(data))

    def get(self, url, *, headers, timeout, allow_redirects) -> _Exchange:
        return _Exchange(self.run, "download", str(url), b"")

    async def ws_connect(self, url, **kwargs):
        await asyncio.sleep(0)
        how, status, headers = self.run.server.upgrade()
        if how == "reset":
            raise aiohttp.ClientConnectionError("connection reset by the server")
        if how == "status":
            info = aiohttp.RequestInfo(yarl.URL(str(url)), "GET", CIMultiDict(), yarl.URL(str(url)))
            raise aiohttp.WSServerHandshakeError(info, (), status=status, message="Invalid response status",
                                                 headers=_aiohttp_headers([*headers, (TAG.encode(), self.run.tag())]))
        return _FakeWS(self.run)

    async def close(self) -> None:
        pass


# -- Rust mode: a raw HTTP/1.1 server on each in-memory connection


class _RawServer(asyncio.Protocol):
    """Reads requests (Content-Length bodies) and writes the server's answers, in pieces; takes a
    WebSocket upgrade as the server says, and then answers each binary frame the client sends."""

    def __init__(self, run: Run, host: str) -> None:
        self.run, self.host = run, host
        self.buf = b""
        self.t: asyncio.Transport | None = None
        self.busy = False
        self.ws = False  # upgraded: frames from here on
        self.frags = b""

    def connection_made(self, transport) -> None:
        self.t = transport

    def data_received(self, data: bytes) -> None:
        self.buf += data
        self._next()

    def _next(self) -> None:
        while not self.busy and self.t is not None and not self.t.is_closing():
            if not (self._frame() if self.ws else self._request()):
                return

    def _request(self) -> bool:
        end = self.buf.find(b"\r\n\r\n")
        if end < 0:
            return False
        head = self.buf[:end].decode("latin-1").split("\r\n")
        fields = {}
        for line in head[1:]:
            name, _, value = line.partition(":")
            fields[name.strip().lower()] = value.strip()
        length = int(fields.get("content-length") or 0)
        if len(self.buf) < end + 4 + length:
            return False
        body, self.buf = self.buf[end + 4:end + 4 + length], self.buf[end + 4 + length:]
        target = head[0].split(" ")[1] if " " in head[0] else "/"
        server = self.run.server
        if fields.get("upgrade", "").lower() == "websocket":
            how, status, headers = server.upgrade()
            if how == "reset":
                self.t.abort()
                return False
            if how == "status":
                self._send(_head(status, headers, b""), [], False)
                return True
            accept = base64.b64encode(hashlib.sha1((fields.get("sec-websocket-key", "") +
                                                    "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest())
            self.ws = True
            self._send(b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                       b"Sec-WebSocket-Accept: " + accept + b"\r\n\r\n", [], False)
            return True
        if head[0].startswith("POST"):
            how, status, headers, data, at = server.answer(body)
        else:
            how, status, headers, data, at = server.download(f"http://{self.host}{target}")
        if how == "reset":
            self.t.abort()
            return False
        if how == "garbage":
            self._send(data, [], True)
            return True
        top = _head(status, headers, data)
        self._send(top + data, [a + len(top) for a in at], False)
        return True

    def _frame(self) -> bool:
        """One frame from the client (masked), answered."""
        b = self.buf
        if len(b) < 2:
            return False
        n, at = b[1] & 0x7F, 2
        if n == 126:
            if len(b) < 4:
                return False
            n, at = struct.unpack(">H", b[2:4])[0], 4
        elif n == 127:
            if len(b) < 10:
                return False
            n, at = struct.unpack(">Q", b[2:10])[0], 10
        masked = b[1] & 0x80
        if len(b) < at + (4 if masked else 0) + n:
            return False
        mask = b[at:at + 4] if masked else b"\0\0\0\0"
        at += 4 if masked else 0
        payload = bytes(c ^ mask[i % 4] for i, c in enumerate(b[at:at + n]))
        self.buf = b[at + n:]
        op = b[0] & 0x0F
        if op == 8:  # the client's Close: answered, and the connection closed
            self._send(bytes([0x88, len(payload[:2])]) + payload[:2], [], True)
            return True
        if op == 9:  # ping
            self._send(_frame(0xA, payload), [], False)
            return True
        if op not in (0, 2):
            return True
        self.frags += payload
        if not b[0] & 0x80:
            return True
        message, self.frags = self.frags, b""
        how, what = self.run.server.ws_answer(message)
        if how == "drop":
            self.t.abort()
            return False
        if how == "close":
            self._send(_frame(0x8, struct.pack(">H", what) if what else b""), [], False)
        elif how == "raw":
            self._send(what, [], False)  # any bytes: frames or not
        else:
            self._send(_frame(0x2, what), [], False)
        return True

    def _send(self, raw: bytes, at: list[int], close: bool) -> None:
        self.busy = True
        self._write(pieces(raw, at), close)

    def _write(self, rest: list[bytes], close: bool) -> None:
        if self.t is None or self.t.is_closing():
            return
        if rest:
            self.t.write(rest[0])
            asyncio.get_running_loop().call_soon(self._write, rest[1:], close)
            return
        self.busy = False
        if close:
            self.t.close()
        else:
            self._next()

    def eof_received(self) -> bool:
        return False

    def connection_lost(self, exc) -> None:
        self.t = None


def _head(status: int, headers: list[tuple[bytes, bytes]], data: bytes) -> bytes:
    lines = [f"HTTP/1.1 {status} X".encode(), f"Content-Length: {len(data)}".encode(),
             *(k + b": " + v for k, v in headers)]
    return b"\r\n".join(lines) + b"\r\n\r\n"


def _frame(op: int, payload: bytes) -> bytes:
    n = len(payload)
    size = bytes([n]) if n < 126 else b"\x7e" + struct.pack(">H", n) if n < 65536 else b"\x7f" + struct.pack(">Q", n)
    return bytes([0x80 | op]) + size + payload


# -- one run


class Run:
    def __init__(self, data: bytes, tmp: str) -> None:
        r = self.r = Reader(data)
        flags = r.byte()
        self.mode = "rust" if flags & 1 else "python"
        self.loop = SeededLoop(r.u32(), auto=True)
        self.server = Server(r)
        self.state = os.path.join(tmp, "state")
        more = r.byte()
        self.config = Config(
            endpoint="ws://opamp.test/v1/opamp" if more & 2 else "http://opamp.test/v1/opamp",
            instance_uid=UID if flags & 2 else None,
            capabilities=CAPABILITIES, heartbeat_interval_ms=[100, 1000, 30_000, 0][flags >> 2 & 3],
            request_timeout_ms=[1000, 0][flags >> 4 & 1], state_file=self.state, accept_packages=True,
            accept_connection_settings=bool(flags & 32), request_instance_uid=bool(flags & 64),
            download_timeout_ms=5000, max_package_bytes=[0, 1000][flags >> 7], mode=self.mode, proxy=Proxy.DIRECT,
            rust_splice=bool(more & 1) or self.mode == "python")
        self.status = Status(
            description=pb.AgentDescription(), custom_capabilities=pb.CustomCapabilities(capabilities=[CAPABILITY]))
        self.client: Client | None = None
        self.returned = False
        self.active = 0
        self.notes: list[Notification] = []
        self.seen: dict[str, Notification] = {}
        self.stop_task: asyncio.Task | None = None
        self.problems: list[str] = []
        self.tagged: dict[str, int] = {}  # Python mode: the answers the server gave (their effect's id)
        self.reached: set[str] = set()  # ...those that reached the core
        self.cancelled: set[int] = set()  # the effects the core gave up on
        self.tasks: list[asyncio.Task] = []  # every task made on the loop
        loop, tasks = self.loop, self.tasks
        make = loop.create_task

        def create_task(coro, **kwargs):
            task = make(coro, **kwargs)
            tasks.append(task)
            return task

        loop.create_task = create_task
        if self.mode == "rust":
            self.loop.network = Network(self.loop, lambda host, port: _RawServer(self, host), accept_all=True)

    def problem(self, what: str) -> None:
        self.problems.append(what)

    def tag(self) -> bytes:
        """A new answer's tag, for the effect being carried out (Python mode)."""
        t = str(len(self.tagged) + 1)
        self.tagged[t] = EFFECT.get()
        return t.encode()

    def notify(self, n: Notification) -> None:
        self.active += 1
        try:
            if self.active > 1:
                self.problem(f"two callbacks at once ({n.kind})")
            if threading.get_ident() != self.loop._thread_id:
                self.problem(f"a callback off the loop's thread ({n.kind})")
            if not self.returned:
                self.problem(f"a callback before start returned ({n.kind})")
            if any(m.kind == "stopped" for m in self.notes):
                self.problem(f"a callback after stopped ({n.kind})")
            self.notes.append(n)
            self.seen[n.kind] = n
            if n.kind == "stopped" and n.error is not None:
                self.problem(f"the client stopped on its own: {n.error!r}")
            if n.kind == "heartbeat_interval" and n.heartbeat_interval_ms < 0:
                self.problem(f"heartbeat interval {n.heartbeat_interval_ms}")
            if n.kind == "install_package":
                self.check_package(n)
            if n.kind == "stopped":
                return
            r = self.r
            for _ in range(r.byte() % 4):
                if call(r, self.client, r.pick(CALLS), self.seen) == "stop" and self.stop_task is None:
                    self.stop_task = self.loop.create_task(self.client.stop())
        finally:
            self.active -= 1

    def check_package(self, n: Notification) -> None:
        where = downloads_dir(self.state) + os.sep
        if not n.path.startswith(where) or os.sep in n.path[len(where):]:
            self.problem(f"install_package {n.package_name!r}: {n.path!r} is not in {where!r}")
            return
        try:
            with open(n.path, "rb") as f:
                got = f.read()
        except OSError as e:
            self.problem(f"install_package {n.package_name!r}: its file can't be read: {e}")
            return
        want = n.package.file.content_hash if n.package is not None else b""
        if want and hashlib.sha256(got).digest() != want:
            self.problem(f"install_package {n.package_name!r}: the file isn't the one offered ({len(got)} bytes)")

    async def main(self) -> None:
        loop = self.loop
        self.client = Client.start(self.config, self.status, self.notify,
                                   _sessions=(lambda _kind: _Session(self)) if self.mode == "python" else None)
        self.returned = True
        begun = loop.time()
        while not self.r.empty and self.stop_task is None and not self.client._finished:
            if loop.time() - begun > RUN_S:
                break
            await asyncio.sleep(0.5)
        if self.stop_task is None:
            self.stop_task = loop.create_task(self.client.stop())
        asked = loop.time()
        try:
            await asyncio.wait_for(asyncio.shield(self.stop_task), STOP_S + MARGIN_S)
        except TimeoutError:
            self.problem(f"stop took over {STOP_S + MARGIN_S} s")
            return
        except Exception as e:  # noqa: BLE001 - stop raised: the client stopped on its own
            self.problem(f"stop raised {e!r}")
        took = loop.time() - asked
        if took > STOP_S + MARGIN_S:
            self.problem(f"stop took {took} s")
        stops = [n for n in self.notes if n.kind == "stopped"]
        if len(stops) != 1 or self.notes[-1].kind != "stopped":
            self.problem(f"stopped told {len(stops)} times, last {self.notes[-1].kind if self.notes else None}")
        if os.path.exists(self.state + ".tmp"):
            self.problem("a temporary state file left behind")
        if loop.wrong_thread:
            self.problem(f"the loop touched from another thread: {loop.wrong_thread[:3]}")
        # Every effect's task ends by reporting (or by a cancel): one that raised left its effect
        # unanswered.
        for t in self.tasks:
            if t.done() and not t.cancelled() and t.exception() is not None:
                self.problem(f"a task raised {t.exception()!r} ({t.get_coro().__qualname__})")
        # An answer the server gave reaches the core, unless the core gave up on its effect.
        if lost := [t for t, id_ in self.tagged.items() if t not in self.reached and id_ not in self.cancelled]:
            self.problem(f"answers the server gave never reached the core: {lost}")


def one(data: bytes) -> None:
    tmp = tempfile.mkdtemp(prefix="opamp-fuzz-")
    saved = _client_mod.Core
    _client_mod.Core = FixedCore
    run = FixedCore.run = Run(data, tmp)
    try:
        run.loop.run_until_complete(run.main())
    finally:
        _client_mod.Core = saved
        FixedCore.run = None
        try:
            run.loop.finish()
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
    if run.problems:
        raise Problem(f"{run.mode} mode, {run.server.answers} answers: " + "; ".join(run.problems))


def TestOneInput(data: bytes) -> None:  # noqa: N802 - Atheris's name
    one(data)


def _seed(*parts: bytes) -> bytes:
    return b"".join(parts)


def _u16(n: int) -> bytes:
    return bytes([n & 0xFF, n >> 8])


# Inputs that reach the deeper paths at once: a remote config answered, a package offered,
# downloaded and installed, connection settings offered, raw bytes, a damaged message, a reset.
SEEDS = [
    _seed(b"\x00", b"\0\0\0\0"),
    _seed(b"\x01", b"\1\0\0\0"),
    # Python mode: a remote config (parts bit 2), answered from the callback.
    _seed(b"\x04", b"\2\0\0\0", b"\5", _u16(2), b"\2ab", b"\1", b"\0", b"\4conf", b"\0", b"\0", b"\1", b"\3", b"\1"),
    # Rust mode: a package (bit 4), its file served, installed and answered.
    _seed(b"\x05", b"\3\0\0\0", b"\1", b"\5", _u16(4), b"\0", b"\1", b"\0", b"\0", b"\1", b"1", b"\2h1", b"\1", b"\0",
          b"\5hello", b"\0", b"\0", b"\0", b"\0", b"\0", b"\0", b"\1", b"\4\1\1\0"),
    # Connection settings offered (bit 8), with a new endpoint.
    _seed(b"\x25", b"\4\0\0\0", b"\0", b"\6", _u16(8), b"\2cs", b"\1", b"\1", b"\1", b"\0", b"\0", b"\0", b"\0"),
    # Raw bytes, a reset, a damaged message, an error status with Retry-After.
    _seed(b"\x00", b"\5\0\0\0", b"\1", _u16(5), b"\x0a\x03abc", b"\2", b"\x09", _u16(0x3ff), b"\0" * 40, b"\3",
          b"\7", b"\1", b"\1"),
    _seed(b"\x01", b"\6\0\0\0", b"\1", b"\4", b"\x10HTTP/1.1 200 OK\r\n", b"\1", _u16(3), b"\xff\xff\xff"),
]


def main() -> None:
    if atheris is None:
        sys.exit("atheris isn't installed: uv run --with atheris python tests/fuzz/fuzz_client.py")
    atheris.Setup(sys.argv, TestOneInput)
    atheris.Fuzz()


if __name__ == "__main__":
    main()
