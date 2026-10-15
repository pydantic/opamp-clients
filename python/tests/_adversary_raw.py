# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The adversary's raw server: what the
streams of a Rust mode run connect to. A stream is bytes, not HTTP, so the adversary tells the
server what to do in the first line it writes:

    opamp-raw tag=7 send=1000 chunk=100 gap=5 end=fin

The server answers "ok\\n", then does what the line says, and keeps for each connection that said
who it was (tag) how many bytes it read after the line, whether each was the pattern's (byte i is
i % 251), and how the connection ended; the adversary asks for that on another stream (ask=TAG).
One server per run, on a port of its own, so every connection is that run's, those that never say
who they are too. As go/opamp/adversary_raw_test.go, the reference, on the loop the harness's
other server and every seed's client share."""

from __future__ import annotations

import asyncio
import socket
import ssl
import struct
import time
from dataclasses import dataclass

#: The longest first line the server reads.
LINE = 256
#: The most one read takes.
READ = 64 << 10
#: The marker is answered within this of being sent (the harness's MARKER_DEADLINE).
MARK_DEADLINE = 30.0

_CYCLE = bytes(range(251))
#: The pattern from any offset, for as long as one read is.
_PATTERN = _CYCLE * (READ // 251 + 2)


def body(size: int) -> bytes:
    """`size` bytes of the body pattern (byte i is i % 251)."""
    return (_CYCLE * (size // 251 + 1))[:size]


@dataclass(eq=False)
class RawConn:
    """What the server knows of one connection."""

    accepted: float  # time.monotonic() when the server took it
    said: bool = False  # its first line came
    got: int = 0  # bytes read after the line
    bad: bool = False  # one of them wasn't the pattern's
    end: str = ""  # "": open; fin: the peer closed its side; reset: it failed; closed: the server ended it
    writer: asyncio.StreamWriter | None = None


class _Tls:
    """TLS on a connection that began in the clear, as its server (the server's side of a
    STARTTLS): Python's ssl through memory, so that the bytes under it stay the connection's own to
    end (a FIN, a reset) as the line says."""

    def __init__(self, context: ssl.SSLContext, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        self._reader, self._writer = reader, writer
        self._in, self._out = ssl.MemoryBIO(), ssl.MemoryBIO()
        self._tls = context.wrap_bio(self._in, self._out, server_side=True)
        self._ended = False

    async def _flush(self) -> None:
        if data := self._out.read():
            self._writer.write(data)
            await self._writer.drain()

    async def _more(self) -> None:
        """What TLS has to say, then more of what the peer sent (its end is the handshake's or the
        record's too: TLS says which)."""
        await self._flush()
        if self._ended:
            raise ConnectionError("the connection ended under TLS")
        if data := await self._reader.read(READ):
            self._in.write(data)
        else:
            self._ended = True
            self._in.write_eof()

    async def handshake(self) -> None:
        while True:
            try:
                self._tls.do_handshake()
                break
            except ssl.SSLWantReadError:
                await self._more()
        await self._flush()

    async def read(self) -> bytes:
        """The next bytes inside TLS; none: the peer closed it (close_notify). A connection that
        ends without that raises (ssl.SSLError)."""
        while True:
            try:
                data = self._tls.read(READ)
            except ssl.SSLWantReadError:
                await self._more()
                continue
            await self._flush()
            return data

    async def write(self, data: bytes) -> None:
        self._tls.write(data)
        await self._flush()

    def close(self) -> None:
        """Say the end (close_notify), as far as it can still be said."""
        try:
            self._tls.unwrap()
        except (ssl.SSLError, OSError):
            pass
        if (data := self._out.read()) and not self._writer.is_closing():
            self._writer.write(data)


class RawServer:
    """One run's raw server. `context`: what tls=1 starts TLS with (None: it can't)."""

    def __init__(self, context: ssl.SSLContext | None = None) -> None:
        self._tls = context
        self.port = 0
        self._server: asyncio.Server | None = None
        #: Set once the run's stop has returned: read=0 connections read on.
        self._released = asyncio.Event()
        #: Set, and made anew, each time a connection says who it is or ends.
        self._changed = asyncio.Event()
        #: Every connection, but the harness's marker.
        self._conns: set[RawConn] = set()
        #: Those that said who they were.
        self._tags: dict[int, RawConn] = {}
        #: When the marker's connection was accepted.
        self.marked: float | None = None
        self._tasks: set[asyncio.Task] = set()

    async def start(self) -> RawServer:
        self._server = await asyncio.start_server(self._accepted, "127.0.0.1", 0, limit=READ)
        self.port = self._server.sockets[0].getsockname()[1]
        return self

    def release(self) -> None:
        """Let the connections told not to read (read=0) read on: the run's stop has returned, and
        only a read tells the server a connection has gone."""
        self._released.set()

    async def close(self) -> None:
        """End the server and every connection."""
        self.release()
        if self._server is not None:
            self._server.close()
        for task in list(self._tasks):
            task.cancel()
        await asyncio.gather(*self._tasks, return_exceptions=True)
        for c in self._conns:
            if c.writer is not None:
                c.writer.transport.abort()

    def _note(self) -> None:
        """A connection said who it is, or ended: wake whoever waits for that."""
        changed, self._changed = self._changed, asyncio.Event()
        changed.set()

    def open(self) -> tuple[int, int]:
        """The connections still open, and how many of them never said who they were."""
        still = [c for c in self._conns if not c.end]
        return len(still), sum(not c.said for c in still)

    def after_mark(self) -> str | None:
        """What breaks RULE-SHELL-07 in when the run's connections were accepted (None: nothing
        does): one accepted after the marker's, which the harness opened the moment stop returned.
        The server takes connections in the order they were made, one at a time on the loop, so
        (as for the HTTP server's marker in this harness) there is no allowance to give: what the
        client connected before stop returned was taken before the marker, however late both."""
        if self.marked is None:
            return "the raw server never got the harness's marker"
        late = max((c.accepted - self.marked for c in self._conns), default=0.0)
        if late <= 0:
            return None
        return (f"RULE-SHELL-07: a stream's connection reached the raw server {late * 1000:.0f} ms after one the "
                "harness made as stop returned")

    def mark(self) -> asyncio.Future[str | None]:
        """Open the harness's marker, as stop returns: connected, and its line written, before
        this returns. The future gives what went wrong, or None if the server said ok."""
        try:
            sock = socket.create_connection(("127.0.0.1", self.port), timeout=MARK_DEADLINE)
            sock.sendall(b"opamp-raw mark=1\n")
        except OSError as e:
            sent = asyncio.get_running_loop().create_future()
            sent.set_result(repr(e))
            return sent

        async def answered() -> str | None:
            try:
                reader, writer = await asyncio.open_connection(sock=sock)
                try:
                    line = await asyncio.wait_for(reader.readline(), MARK_DEADLINE)
                finally:
                    writer.close()
            except asyncio.TimeoutError:
                return f"not answered within {MARK_DEADLINE:.0f} s"
            except OSError as e:
                return repr(e)
            return None if line == b"ok\n" else f"the raw server answered the marker {line!r}"

        return asyncio.ensure_future(answered())

    def _accepted(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        # Its run's from the moment it is accepted, whether or not it ever says who it is.
        c = RawConn(time.monotonic(), writer=writer)
        self._conns.add(c)
        self._note()
        task = asyncio.ensure_future(self._serve(c, reader, writer))
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    def _ended(self, c: RawConn, how: str) -> None:
        if not c.end:
            c.end = how
            self._note()

    async def _serve(self, c: RawConn, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        sending: asyncio.Task | None = None
        try:
            sending = await self._run(c, reader, writer)
        except asyncio.CancelledError:
            raise
        except Exception:  # noqa: BLE001 - whatever a connection does, the server goes on
            pass
        finally:
            # It read the connection's end, ended it itself, or gave up on it.
            self._ended(c, "closed")
            if not writer.is_closing():
                writer.close()
            # How the connection ended (a reset: ConnectionResetError) is its protocol's closed
            # future's, retrieved only by wait_closed or the protocol's finalizer; collected in a
            # cycle, the future can be finalized first and asyncio logs it as never retrieved.
            closed = asyncio.ensure_future(writer.wait_closed())
            closed.add_done_callback(lambda f: f.cancelled() or f.exception())
            if sending is not None:
                sending.cancel()

    async def _run(self, c: RawConn, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> asyncio.Task | None:
        """Do what the connection's first line says. The task that sends, if one was started."""
        transport = writer.transport
        # A write is over once the kernel has all of it, as a blocking one is: what the line says
        # comes after the last byte (after, end) is then counted from there.
        transport.set_write_buffer_limits(high=0)
        try:
            line = await _first_line(reader)
        except (OSError, EOFError):
            line = None
        set_: dict[str, str] = {}
        if line is not None and line.startswith("opamp-raw "):
            for kv in line[len("opamp-raw "):].split():
                key, eq, value = kv.partition("=")
                if eq:
                    set_[key] = value
        else:
            line = None

        def num(name: str, default: int) -> int:
            try:
                return int(set_[name])
            except (KeyError, ValueError):
                return default

        if line is not None and num("mark", 0) == 1:
            # The harness's own, not the run's: when what was opened as stop returned got here.
            self._conns.discard(c)
            self.marked = c.accepted
            self._note()
            writer.write(b"ok\n")
            return None
        if line is None:
            # It went, or said something else, before it said who it was.
            self._ended(c, "closed")
            return None
        c.said = True
        self._tags[max(num("tag", 0), 0)] = c
        self._note()

        tls: _Tls | None = None

        async def write(data: bytes) -> None:
            if tls is not None:
                await tls.write(data)
            else:
                writer.write(data)
                await writer.drain()

        delay = num("delay", 0)
        held = delay < 0  # never answered: only read, until it goes
        if not held:
            try:
                await asyncio.sleep(delay / 1000)
                await write(b"ok\n")
                if num("tls", 0) == 1:
                    if self._tls is None:
                        raise ConnectionError("no certificate to start TLS with")
                    tls = _Tls(self._tls, reader, writer)
                    await asyncio.wait_for(tls.handshake(), 30)
            except (OSError, ssl.SSLError, asyncio.TimeoutError):
                self._ended(c, "closed")
                return None

        async def send() -> None:
            """What the line asks the server to send, then how it ends the stream, beside the
            reading."""
            try:
                if "ask" in set_:
                    await write((await self._answer(max(num("ask", 0), 0), num("wait", 0))).encode())
                data = body(max(num("send", 0), 0))
                chunk = num("chunk", 0)
                if chunk <= 0:
                    chunk = max(len(data), 1)
                for off in range(0, len(data), chunk):
                    if off > 0:
                        await asyncio.sleep(max(num("gap", 0), 0) / 1000)
                    await write(data[off:off + chunk])
                end = set_.get("end", "hold")
                if end not in ("fin", "close", "reset"):
                    return
                await asyncio.sleep(max(num("after", 0), 0) / 1000)
                if end == "fin":
                    # Its side only: it still reads.
                    if writer.can_write_eof():
                        writer.write_eof()
                elif end == "close":
                    self._ended(c, "closed")
                    if tls is not None:
                        tls.close()
                    writer.close()
                else:
                    self._ended(c, "closed")
                    sock = transport.get_extra_info("socket")
                    if sock is not None:
                        sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
                    transport.abort()
            except Exception:  # noqa: BLE001 - the connection went under it: the reading says how
                pass

        sending = None if held else asyncio.ensure_future(send())
        if sending is not None:
            self._tasks.add(sending)
            sending.add_done_callback(self._tasks.discard)
        try:
            if num("read", 1) == 0:
                # Reads nothing until the run is over.
                transport.pause_reading()
                await self._released.wait()
                transport.resume_reading()
            echo = num("echo", 0) == 1
            while True:
                try:
                    data = await (tls.read() if tls is not None else reader.read(READ))
                except (OSError, ssl.SSLError):
                    self._ended(c, "reset")
                    break
                if not data:
                    self._ended(c, "fin")
                    break
                at = c.got % 251
                if data != _PATTERN[at:at + len(data)]:
                    c.bad = True
                c.got += len(data)
                if echo:
                    try:
                        await write(data)
                    except (OSError, ssl.SSLError):
                        self._ended(c, "reset")
                        break
            if tls is not None:
                tls.close()
        except BaseException:
            if sending is not None:
                sending.cancel()
            raise
        return sending

    async def _answer(self, tag: int, wait: int) -> str:
        """Wait until connection `tag` has ended (tag 0: until no connection that never said who
        it was is open), `wait` ms at most, and say what the server knows of it: "STATE GOT BAD\\n"."""
        loop = asyncio.get_running_loop()
        deadline = loop.time() + wait / 1000
        while True:
            state, got, bad = "none", 0, 0
            if tag == 0:
                if anonymous := sum(not c.end and not c.said for c in self._conns):
                    state, got = "open", anonymous
            elif (c := self._tags.get(tag)) is not None:
                state, got, bad = c.end or "open", c.got, int(c.bad)
            answer = f"{state} {got} {bad}\n"
            left = deadline - loop.time()
            if state != "open" or left <= 0:
                return answer
            try:
                await asyncio.wait_for(self._changed.wait(), left)
            except asyncio.TimeoutError:
                return answer


async def _first_line(reader: asyncio.StreamReader) -> str:
    """The first line, without its newline: LINE bytes at most (EOFError: none came)."""
    line = bytearray()
    while len(line) < LINE:
        b = await reader.read(1)
        if not b:
            raise EOFError("it ended before its first line")
        if b == b"\n":
            return line.decode("latin-1")
        line += b
    raise EOFError("no line")
