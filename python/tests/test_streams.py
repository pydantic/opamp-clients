# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Core mode's streams and files (src/pydantic_opamp_client/_streams.py): download files written off the
event loop, in order, hashed over exactly what was written, answered once (or not at all after a
discard or a cancel); backpressure both ways; and the in-flight registry: a close or a shutdown
ends what it names, a stream that connects (or finishes its TLS handshake) after its close is
aborted as it arrives, and shutdown is bounded. File writes are slowed here by wrapping
_write_all, the one call _streams.py writes with."""

from __future__ import annotations

import asyncio
import hashlib
import os
import threading
import time
from collections.abc import Callable

import pytest
import trustme
from _servers import HttpProxy, OpampServer, Socks5Proxy
from aiohttp import web

from pydantic_opamp_client import TLS, Client, Config, Notification, Proxy, Status
from pydantic_opamp_client import _streams
from pydantic_opamp_client import opamp_pb2 as pb
from pydantic_opamp_client._executor import download_path, downloads_dir
from pydantic_opamp_client._proto.opamp.core.v1 import core_pb2 as corepb
from pydantic_opamp_client._streams import Streams, proxy_choice
from pydantic_opamp_client._transport import Transport


async def until(what: str, done: Callable[[], bool], seconds: float = 20) -> None:
    deadline = time.monotonic() + seconds
    while not done():
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out waiting for {what}")
        await asyncio.sleep(0.005)


def kinds(got: list[corepb.Input], id_: int | None = None) -> list[str]:
    out = []
    for i in got:
        k = i.WhichOneof("kind")
        if id_ is None or getattr(getattr(i, k), "id", None) == id_:
            out.append(k)
    return out


@pytest.fixture
def slow(monkeypatch) -> Callable[[float], None]:
    """Make every file write `seconds` late (in its worker thread)."""
    real = _streams._write_all

    def set_(seconds: float) -> None:
        def write(files, fd, bufs, digest):
            time.sleep(seconds)
            real(files, fd, bufs, digest)

        monkeypatch.setattr(_streams, "_write_all", write)

    return set_


def new(tmp_path, deliver: Callable[[corepb.Input], None] | None = None, **kw) -> tuple[Streams, list[corepb.Input], str]:
    """A Streams (on the running loop), the inputs it reports, and a state file beside its downloads."""
    got: list[corepb.Input] = []
    state = str(tmp_path / "state")
    s = Streams(deliver or got.append, Transport(proxy=Proxy.DIRECT), state, **kw)
    return s, got, state


def data(id_: int, b: bytes) -> corepb.Effect:
    return corepb.Effect(download_data=corepb.WsMessage(id=id_, data=b))


def done(id_: int) -> corepb.Effect:
    return corepb.Effect(download_done=corepb.Id(id=id_))


# Files.

# covers: RULE-CLIENT-14
def test_a_file_is_written_in_order_synced_and_hashed_over_exactly_what_was_written(tmp_path, slow) -> None:
    slow(0.003)

    async def run() -> None:
        s, got, state = new(tmp_path)
        parts = [os.urandom(1 + (i * 7919) % 20_000) for i in range(40)]
        for p in parts:
            s.do(data(5, p))
        s.do(done(5))
        assert got == [], "nothing is said before the writes are done"
        await until("downloaded", lambda: bool(got))
        want = hashlib.sha256(b"".join(parts)).digest()
        assert kinds(got) == ["downloaded"] and got[0].downloaded.sha256 == want
        with open(download_path(state, "", 5), "rb") as f:
            assert hashlib.sha256(f.read()).digest() == want
        if os.name != "nt":
            assert os.stat(download_path(state, "", 5)).st_mode & 0o777 == 0o600
            assert os.stat(downloads_dir(state)).st_mode & 0o777 == 0o700
        await s.shutdown()
        assert kinds(got, 5) == ["downloaded"]

    asyncio.run(run())


def test_an_empty_file_is_a_file(tmp_path) -> None:
    async def run() -> None:
        s, got, state = new(tmp_path)
        s.do(done(1))
        await until("downloaded", lambda: bool(got))
        assert kinds(got) == ["downloaded"] and got[0].downloaded.sha256 == hashlib.sha256().digest()
        assert os.path.getsize(download_path(state, "", 1)) == 0
        await s.shutdown()

    asyncio.run(run())


@pytest.mark.parametrize("how", ["discard", "cancel"])
@pytest.mark.parametrize("finished", [False, True], ids=["writing", "done"])
def test_a_discard_or_cancel_while_a_write_is_pending_leaves_no_file_and_says_nothing(
        how: str, finished: bool, tmp_path, slow) -> None:
    slow(0.1)

    async def run() -> None:
        s, got, state = new(tmp_path)
        for _ in range(5):
            s.do(data(9, os.urandom(50_000)))
        if finished:
            s.do(done(9))
        await asyncio.sleep(0.02)  # the first write is in flight
        # A cancel goes on to the Executor too (it may be a save's); a discard is only this one's.
        assert s.do(corepb.Effect(**{how: corepb.Id(id=9)})) is (how == "discard")
        await asyncio.sleep(0.2)
        assert got == []
        assert not os.path.exists(download_path(state, "", 9))
        await s.shutdown()
        assert got == []

    asyncio.run(run())


def test_a_discard_of_a_file_already_answered_deletes_it(tmp_path) -> None:
    async def run() -> None:
        s, got, state = new(tmp_path)
        s.do(data(2, b"abc"))
        s.do(done(2))
        await until("downloaded", lambda: bool(got))
        assert os.path.exists(download_path(state, "", 2))
        s.do(corepb.Effect(discard=corepb.Id(id=2)))
        await until("deleted", lambda: not os.path.exists(download_path(state, "", 2)))
        await s.shutdown()
        assert kinds(got) == ["downloaded"]

    asyncio.run(run())


def test_a_failed_write_deletes_the_file_and_answers_download_failed_once(tmp_path, monkeypatch) -> None:
    calls = 0
    real = _streams._write_all

    def failing(files, fd, bufs, digest):
        nonlocal calls
        calls += 1
        if calls == 2:
            raise OSError(28, "No space left on device")
        real(files, fd, bufs, digest)

    monkeypatch.setattr(_streams, "_write_all", failing)

    async def run() -> None:
        s, got, state = new(tmp_path)
        s.do(data(3, os.urandom(1000)))
        await until("the first write", lambda: calls >= 1)
        await asyncio.sleep(0.01)
        s.do(data(3, os.urandom(1000)))
        await until("the second write", lambda: calls >= 2)
        # More after the failure: dropped, not a new file.
        s.do(data(3, os.urandom(1000)))
        await asyncio.sleep(0.05)
        assert got == []
        assert not os.path.exists(download_path(state, "", 3))
        s.do(done(3))
        await until("an answer", lambda: bool(got))
        await s.shutdown()
        assert kinds(got, 3) == ["download_failed"]
        assert "No space left" in got[0].download_failed.detail
        assert not os.path.exists(download_path(state, "", 3))

    asyncio.run(run())


def test_shutdown_waits_for_the_write_in_flight_deletes_files_not_complete_and_says_nothing(tmp_path, slow) -> None:
    slow(0.05)

    async def run() -> None:
        s, got, state = new(tmp_path)
        s.do(data(1, os.urandom(10_000)))
        s.do(data(2, os.urandom(10_000)))
        s.do(done(2))
        await asyncio.sleep(0.01)
        await s.shutdown()
        assert got == []
        assert os.listdir(downloads_dir(state)) == []
        # Effects after shutdown change nothing and report nothing.
        s.do(data(4, b"x"))
        s.do(done(4))
        s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="127.0.0.1", port=9)))
        s.do(corepb.Effect(stream_resolve=corepb.StreamName(id=2, host="localhost")))
        await asyncio.sleep(0.1)
        assert got == []

    asyncio.run(run())


# covers: RULE-CLIENT-16
def test_shutdown_is_bounded_by_a_write_that_hangs(tmp_path, monkeypatch) -> None:
    release = threading.Event()
    real = _streams._write_all

    def hung(files, fd, bufs, digest):
        release.wait(10)
        real(files, fd, bufs, digest)

    monkeypatch.setattr(_streams, "_write_all", hung)
    monkeypatch.setattr(_streams, "SHUTDOWN_S", 0.3)

    async def run() -> None:
        s, got, state = new(tmp_path)
        s.do(data(1, b"stuck"))
        await asyncio.sleep(0.05)
        started = time.monotonic()
        await s.shutdown()
        assert time.monotonic() - started < 2, "shutdown waited for the hung write"
        # Once the write returns, the file still goes.
        release.set()
        await until("the file deleted", lambda: not os.path.exists(download_path(state, "", 1)))
        assert got == []

    asyncio.run(run())


# Streams.

async def tcp_server(serve: Callable[[asyncio.StreamReader, asyncio.StreamWriter], object]) -> tuple[asyncio.Server, int]:
    writers: set[asyncio.StreamWriter] = set()

    async def handle(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
        writers.add(w)
        try:
            await serve(r, w)  # type: ignore[misc]
        except (ConnectionError, OSError):
            pass

    server = await asyncio.start_server(handle, "127.0.0.1", 0)
    server._test_writers = writers  # type: ignore[attr-defined]
    return server, server.sockets[0].getsockname()[1]


def close_server(server: asyncio.Server) -> None:
    for w in server._test_writers:  # type: ignore[attr-defined]
        if not w.transport.is_closing():
            w.transport.abort()
    server.close()


def test_a_stream_carries_bytes_both_ways_and_a_close_lets_written_bytes_go(tmp_path) -> None:
    async def run() -> None:
        received = bytearray()
        eof = asyncio.Event()

        async def echo(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
            first = await r.readexactly(5)
            w.write(first.upper())
            await w.drain()
            received.extend(await r.read())  # until our FIN
            eof.set()

        server, port = await tcp_server(echo)
        s, got, _ = new(tmp_path)
        try:
            s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="127.0.0.1", port=port)))
            await until("opened", lambda: "stream_opened" in kinds(got))
            s.do(corepb.Effect(stream_write=corepb.WsMessage(id=1, data=b"hello")))
            await until("the echo", lambda: "stream_data" in kinds(got))
            assert got[1].stream_data.data == b"HELLO"
            s.do(corepb.Effect(stream_write=corepb.WsMessage(id=1, data=b" and goodbye")))
            s.do(corepb.Effect(stream_close=corepb.StreamClose(id=1, abort=False)))
            await asyncio.wait_for(eof.wait(), 5)
            assert bytes(received) == b" and goodbye"
            await asyncio.sleep(0.05)
            assert kinds(got) == ["stream_opened", "stream_data"], "nothing more after the close"
        finally:
            await s.shutdown()
            close_server(server)

    asyncio.run(run())


def test_a_refused_connection_and_a_peer_that_closes_are_reported(tmp_path) -> None:
    async def run() -> None:
        async def closes(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
            w.write(b"bye")
            await w.drain()
            w.close()

        server, port = await tcp_server(closes)
        unused = socket_port()
        s, got, _ = new(tmp_path)
        try:
            s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="127.0.0.1", port=unused)))
            s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=2, host="127.0.0.1", port=port)))
            await until("both ended", lambda: kinds(got, 1) == ["stream_failed"]
                        and kinds(got, 2)[-1:] == ["stream_failed"])
            assert kinds(got, 2) == ["stream_opened", "stream_data", "stream_eof", "stream_failed"]
            assert b"".join(i.stream_data.data for i in got if i.WhichOneof("kind") == "stream_data") == b"bye"
        finally:
            await s.shutdown()
            close_server(server)

    asyncio.run(run())


def socket_port() -> int:
    """A port nothing listens on (just now)."""
    import socket

    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


@pytest.mark.parametrize("splice", [True, False], ids=["spliced", "download_data"])
def test_a_socket_faster_than_its_file_is_paused_nothing_reordered(splice: bool, tmp_path, slow) -> None:
    slow(0.015)
    file = os.urandom(40 << 20)

    async def run() -> None:
        async def flood(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
            await r.read(1)  # it sends once asked
            for at in range(0, len(file), 1 << 20):
                w.write(file[at:at + (1 << 20)])
                await w.drain()
            w.close()

        server, port = await tcp_server(flood)
        got: list[corepb.Input] = []

        def deliver(i: corepb.Input) -> None:
            # In place of the core: what comes unspliced is the download's, as download_data at once.
            got.append(i)
            k = i.WhichOneof("kind")
            if k == "stream_data":
                s.do(data(7, i.stream_data.data))
            elif k == "stream_eof":
                s.do(done(7))

        s, _, _ = new(tmp_path, deliver)
        peak = 0

        async def sample() -> None:
            nonlocal peak
            while True:
                peak = max(peak, s.unwritten)
                await asyncio.sleep(0.001)

        sampler = asyncio.get_running_loop().create_task(sample())
        try:
            s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="127.0.0.1", port=port)))
            await until("opened", lambda: "stream_opened" in kinds(got))
            if splice:
                s.do(corepb.Effect(stream_splice=corepb.StreamSplice(id=1, download=7, bytes=len(file))))
            s.do(corepb.Effect(stream_write=corepb.WsMessage(id=1, data=b"GET\n")))
            await until("downloaded", lambda: {"downloaded", "download_failed"} & set(kinds(got)), 120)
        finally:
            sampler.cancel()
            await s.shutdown()
            close_server(server)
        done_ = [i for i in got if i.WhichOneof("kind") == "downloaded"]
        assert done_, kinds(got)
        assert done_[0].downloaded.sha256 == hashlib.sha256(file).digest()
        assert ("stream_spliced" in kinds(got)) == splice
        # It was held near the mark (4 MiB, and a read or two past it), not the whole 40 MiB.
        assert peak > 3 << 20, f"peak {peak}: the writes were never behind"
        assert peak < 6 << 20, f"peak {peak}: the socket was not paused"

    asyncio.run(run())


def test_a_peer_that_stops_reading_is_not_read_until_it_drains_and_past_a_bound_fails(tmp_path) -> None:
    async def run() -> None:
        reading = asyncio.Event()  # the peer reads (and drops) what it is sent while set

        async def chatty(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
            async def drop() -> None:
                while True:
                    await reading.wait()
                    if not await r.read(1 << 16):
                        return

            dropping = asyncio.get_running_loop().create_task(drop())
            # It reads nothing, and keeps sending (as pings would ask for answers).
            chunk = b"\1" * (16 << 10)
            try:
                while not w.is_closing():
                    w.write(chunk)
                    await w.drain()
            finally:
                dropping.cancel()

        server, port = await tcp_server(chatty)
        s, got, _ = new(tmp_path)

        def read() -> int:
            return sum(len(i.stream_data.data) for i in got if i.WhichOneof("kind") == "stream_data")

        try:
            s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="127.0.0.1", port=port)))
            await until("opened", lambda: "stream_opened" in kinds(got))
            await until("its bytes", lambda: read() > 1 << 20)
            # Writes it doesn't take: once the transport's buffer is full, we read nothing more.
            for _ in range(8):
                s.do(corepb.Effect(stream_write=corepb.WsMessage(id=1, data=b"\2" * (1 << 20))))
            await asyncio.sleep(0.1)
            before = read()
            await asyncio.sleep(0.3)
            assert read() - before <= 512 << 10, f"read {read() - before} bytes while its writes were stuck"
            # It reads again: our writes drain, and we read again.
            reading.set()
            await until("reading again", lambda: read() > before + (1 << 20))
            # It stops again, and we write on: past the bound, the stream fails.
            reading.clear()
            for _ in range(64):
                if "stream_failed" in kinds(got):
                    break
                s.do(corepb.Effect(stream_write=corepb.WsMessage(id=1, data=b"\3" * (1 << 20))))
            failed = [i for i in got if i.WhichOneof("kind") == "stream_failed"]
            assert failed, "the stream never failed"
            assert "has not taken" in failed[0].stream_failed.detail
            # Then nothing more about it.
            n = len(got)
            s.do(corepb.Effect(stream_write=corepb.WsMessage(id=1, data=b"\4" * 10)))
            await asyncio.sleep(0.05)
            assert len(got) == n
        finally:
            await s.shutdown()
            close_server(server)

    asyncio.run(run())


def test_a_stream_the_shell_closes_on_hearing_its_splice_done_says_nothing_more(tmp_path) -> None:
    async def run() -> None:
        async def serve(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
            await r.read(1)
            w.write(b"\7" * 1000)  # the spliced bytes and more, in one piece
            await w.drain()
            await asyncio.sleep(1)

        server, port = await tcp_server(serve)
        got: list[corepb.Input] = []

        def deliver(i: corepb.Input) -> None:
            got.append(i)
            if i.WhichOneof("kind") == "stream_spliced":
                s.do(corepb.Effect(stream_close=corepb.StreamClose(id=1, abort=True)))

        s, _, state = new(tmp_path, deliver)
        try:
            s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="127.0.0.1", port=port)))
            await until("opened", lambda: "stream_opened" in kinds(got))
            s.do(corepb.Effect(stream_splice=corepb.StreamSplice(id=1, download=4, bytes=600)))
            s.do(corepb.Effect(stream_write=corepb.WsMessage(id=1, data=b"GET\n")))
            await until("spliced", lambda: "stream_spliced" in kinds(got))
            await asyncio.sleep(0.05)
            assert kinds(got) == ["stream_opened", "stream_spliced"]
            s.do(done(4))
            await until("downloaded", lambda: "downloaded" in kinds(got))
            assert os.path.getsize(download_path(state, "", 4)) == 600
        finally:
            await s.shutdown()
            close_server(server)

    asyncio.run(run())


# covers: RULE-SHELL-05
def test_a_splice_asked_for_on_hearing_the_last_one_done_takes_the_bytes_already_read(tmp_path) -> None:
    """What the shell asks in answer to an input about a stream applies to the very next bytes:
    those read in one piece with a splice's last are the next splice's, not stream_data. (The
    adversary's stream_splice_chain found them handed over: splice_leak, seed 1 in core mode.)"""
    sent = bytes(i % 251 for i in range(1000))

    async def run() -> None:
        async def serve(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
            await r.read(1)
            w.write(sent)  # three splices and more, in one piece
            await w.drain()
            await asyncio.sleep(1)

        server, port = await tcp_server(serve)
        got: list[corepb.Input] = []

        def deliver(i: corepb.Input) -> None:
            got.append(i)
            # In place of the core: on hearing a splice done, the next, twice.
            if i.WhichOneof("kind") == "stream_spliced" and kinds(got).count("stream_spliced") < 3:
                s.do(corepb.Effect(stream_splice=corepb.StreamSplice(id=1, download=4, bytes=300)))

        s, _, state = new(tmp_path, deliver)
        try:
            s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="127.0.0.1", port=port)))
            await until("opened", lambda: "stream_opened" in kinds(got))
            s.do(corepb.Effect(stream_splice=corepb.StreamSplice(id=1, download=4, bytes=300)))
            s.do(corepb.Effect(stream_write=corepb.WsMessage(id=1, data=b"GET\n")))
            await until("the rest", lambda: "stream_data" in kinds(got))
            await asyncio.sleep(0.05)
            assert kinds(got) == ["stream_opened", "stream_spliced", "stream_spliced", "stream_spliced", "stream_data"]
            assert got[-1].stream_data.data == sent[900:], "only what follows the last splice is the core's"
            s.do(done(4))
            await until("downloaded", lambda: "downloaded" in kinds(got))
            with open(download_path(state, "", 4), "rb") as f:
                assert f.read() == sent[:900], "every spliced byte, in order"
        finally:
            await s.shutdown()
            close_server(server)

    asyncio.run(run())


def test_names_are_resolved_for_a_stream_and_a_close_stops_a_resolution(tmp_path) -> None:
    async def run() -> None:
        gate = asyncio.get_running_loop().create_future()

        async def getaddrinfo(host, port, **kw):
            if host == "slow.test":
                await gate
            return [(2, 1, 6, "", ("127.0.0.1", 0)), (30, 1, 6, "", ("fe80::1%lo0", 0, 0, 1)),
                    (2, 1, 6, "", ("127.0.0.1", 0))]

        s, got, _ = new(tmp_path, getaddrinfo=getaddrinfo)
        s.do(corepb.Effect(stream_resolve=corepb.StreamName(id=1, host="fast.test")))
        await until("resolved", lambda: bool(got))
        assert kinds(got) == ["stream_resolved"]
        assert list(got[0].stream_resolved.addresses) == ["127.0.0.1", "fe80::1"], "once each, without a scope"
        s.do(corepb.Effect(stream_resolve=corepb.StreamName(id=2, host="slow.test")))
        await asyncio.sleep(0.01)
        s.do(corepb.Effect(stream_close=corepb.StreamClose(id=2, abort=True)))
        assert gate.cancelled(), "the resolution was stopped"
        await asyncio.sleep(0.05)
        assert kinds(got) == ["stream_resolved"], "nothing about a resolution closed"
        s.do(corepb.Effect(stream_resolve=corepb.StreamName(id=3, host="nowhere.invalid")))
        await s.shutdown()

    asyncio.run(run())


class FakeTransport(asyncio.Transport):
    def __init__(self) -> None:
        super().__init__()
        self.aborted = False
        self.closing = False
        self.written = bytearray()

    def write(self, data) -> None:
        self.written += data

    def abort(self) -> None:
        self.aborted = self.closing = True

    def close(self) -> None:
        self.closing = True

    def is_closing(self) -> bool:
        return self.closing

    def get_write_buffer_size(self) -> int:
        return 0

    def pause_reading(self) -> None:
        pass

    def resume_reading(self) -> None:
        pass

    def is_reading(self) -> bool:
        return True


# covers: RULE-CLIENT-15
@pytest.mark.parametrize("swallows", [False, True], ids=["cancellable", "swallows-the-cancel"])
def test_a_stream_that_connects_after_its_close_is_aborted_at_once(swallows: bool, tmp_path) -> None:
    """The connect is under way when the core closes the stream; it completes anyway (a connect
    that ignores its cancel, or one that had just finished): the transport is aborted as it comes,
    and the core hears nothing of it."""
    async def run() -> None:
        gate = asyncio.get_running_loop().create_future()
        made: list[FakeTransport] = []

        async def connect(factory, host, port):
            proto = factory()
            try:
                await asyncio.shield(gate)
            except asyncio.CancelledError:
                if not swallows:
                    raise
                await gate  # it connects all the same
            t = FakeTransport()
            made.append(t)
            proto.connection_made(t)
            proto.data_received(b"too late")
            return t, proto

        s, got, _ = new(tmp_path, connect=connect)
        s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="h", port=1)))
        await asyncio.sleep(0)
        s.do(corepb.Effect(stream_close=corepb.StreamClose(id=1, abort=False)))
        gate.set_result(None)
        await asyncio.sleep(0.01)
        assert got == []
        if swallows:
            assert made and made[0].aborted, "aborted at once"
        else:
            assert not made
        await s.shutdown()
        assert got == []

    asyncio.run(run())


# covers: RULE-CLIENT-15
def test_a_tls_handshake_that_completes_after_its_close_is_aborted(tmp_path) -> None:
    async def run() -> None:
        gate = asyncio.get_running_loop().create_future()
        tls_made: list[FakeTransport] = []

        async def connect(factory, host, port):
            proto = factory()
            t = FakeTransport()
            proto.connection_made(t)
            return t, proto

        async def start_tls(raw, proto, ctx, server_hostname=None):
            try:
                await asyncio.shield(gate)
            except asyncio.CancelledError:
                await gate  # it finishes all the same
            t = FakeTransport()
            tls_made.append(t)
            return t

        s, got, _ = new(tmp_path, connect=connect, start_tls=start_tls)
        s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="h", port=1)))
        await until("opened", lambda: bool(got))
        s.do(corepb.Effect(stream_start_tls=corepb.StreamName(id=1, host="h")))
        await asyncio.sleep(0)
        s.do(corepb.Effect(stream_close=corepb.StreamClose(id=1, abort=False)))
        gate.set_result(None)
        await asyncio.sleep(0.01)
        assert kinds(got) == ["stream_opened"]
        assert tls_made and tls_made[0].aborted
        await s.shutdown()

    asyncio.run(run())


def test_shutdown_ends_connects_handshakes_and_lingering_closes_at_once(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(_streams, "LINGER_S", 60)

    async def run() -> None:
        hung = asyncio.get_running_loop().create_future()
        made: list[FakeTransport] = []

        async def connect(factory, host, port):
            proto = factory()
            if host == "hangs":
                await hung
            t = FakeTransport()
            made.append(t)
            proto.connection_made(t)
            return t, proto

        s, got, _ = new(tmp_path, connect=connect)
        s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="hangs", port=1)))
        s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=2, host="ok", port=1)))
        s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=3, host="ok", port=1)))
        await until("opened", lambda: len(got) == 2)
        s.do(corepb.Effect(stream_close=corepb.StreamClose(id=3, abort=False)))  # lingers
        assert made[1].closing and not made[1].aborted
        started = time.monotonic()
        await s.shutdown()
        assert time.monotonic() - started < 1
        assert all(t.aborted for t in made), "every transport aborted"
        assert not s._in.tasks and not s._in.timers and not s._in.streams and not s._in.lingering
        assert kinds(got) == ["stream_opened", "stream_opened"]

    asyncio.run(run())


def test_proxy_queries_answer_with_the_proxy_as_given(tmp_path, monkeypatch) -> None:
    async def run() -> None:
        got: list[corepb.Input] = []
        s = Streams(got.append, Transport(proxy=Proxy("socks5h://u:p%40ss@proxy.test:1080", resolve_locally=True)), None)
        s.do(corepb.Effect(proxy_query=corepb.Url(id=4, url="wss://opamp.test/v1/opamp")))
        assert got[-1].proxy_answer == corepb.ProxyAnswer(id=4, proxy="socks5h://u:p%40ss@proxy.test:1080",
                                                          resolve_locally=True)
        s = Streams(got.append, Transport(proxy=Proxy("http://proxy.test:3128", headers={"X-T": "1"})), None)
        s.do(corepb.Effect(proxy_query=corepb.Url(id=5, url="http://opamp.test/v1/opamp")))
        assert got[-1].proxy_answer == corepb.ProxyAnswer(id=5, proxy="http://proxy.test:3128",
                                                          headers=[corepb.Header(name="X-T", value="1")])
        # The environment's is the core's to pick (it was told the variables): asked, none.
        monkeypatch.setenv("HTTPS_PROXY", "proxy.test:3128")
        s = Streams(got.append, Transport(core_mode=True), None)
        s.do(corepb.Effect(proxy_query=corepb.Url(id=6, url="https://opamp.test/v1/opamp")))
        assert got[-1].proxy_answer == corepb.ProxyAnswer(id=6)
        assert not s.do(corepb.Effect(store=corepb.Store(id=1))), "not core mode's"

    asyncio.run(run())


@pytest.mark.skipif(os.name == "nt", reason="the environment's bytes are POSIX's")
def test_a_proxy_from_the_environment_that_isnt_utf8_still_reaches_the_core(monkeypatch) -> None:
    # Python keeps the environment's bytes that aren't UTF-8 as lone surrogates, which a protobuf
    # string refuses: the proxy answer raised in the step that asked for it, and the client
    # stopped. As every string from outside the core: what isn't UTF-8 becomes U+FFFD.
    async def run() -> None:
        got: list[corepb.Input] = []
        monkeypatch.setenv("HTTPS_PROXY", "http://us\udce9r:pw@proxy.test:3128")
        choice = proxy_choice(Transport(core_mode=True))
        assert [(h.name, h.value) for h in choice.environment if h.name == "HTTPS_PROXY"] == [
            ("HTTPS_PROXY", "http://us�r:pw@proxy.test:3128")], choice
        choice.SerializeToString()

    asyncio.run(run())


# The client in core mode.

def package_app(file: bytes, framing: str, chunk: Callable[[int], int], redirect: bool = False) -> web.Application:
    """An OpAMP server (plain HTTP) offering one package, whose file it serves (`framing`:
    content-length or chunked), in pieces `chunk(at)` long; behind a redirect if `redirect`."""
    async def opamp(req: web.Request) -> web.Response:
        msg = pb.AgentToServer.FromString(await req.read())
        base = f"{req.scheme}://{req.host}"
        offer = pb.PackagesAvailable(all_packages_hash=b"\1", packages={"pkg": pb.PackageAvailable(
            type=pb.PackageType_TopLevel, version="1", hash=b"\2",
            file=pb.DownloadableFile(download_url=base + ("/moved" if redirect else "/file"),
                                     content_hash=hashlib.sha256(file).digest()))})
        reply = pb.ServerToAgent(instance_uid=msg.instance_uid)
        if not msg.HasField("agent_disconnect"):
            reply.packages_available.CopyFrom(offer)
        return web.Response(body=reply.SerializeToString(), content_type="application/x-protobuf")

    async def moved(_req: web.Request) -> web.Response:
        raise web.HTTPFound("/file")

    async def serve(req: web.Request) -> web.StreamResponse:
        resp = web.StreamResponse()
        if framing == "content-length":
            resp.content_length = len(file)
        await resp.prepare(req)
        at = 0
        while at < len(file):
            n = chunk(at)
            await resp.write(file[at:at + n])
            at += n
        await resp.write_eof()
        return resp

    app = web.Application()
    app.router.add_post("/v1/opamp", opamp)
    app.router.add_get("/moved", moved)
    app.router.add_get("/file", serve)
    return app


async def download(endpoint: str, state: str, splice: bool, **config) -> bytes:
    """Download the server's package in core mode: the install_package notification's file."""
    notes: list[Notification] = []
    client = Client.start(Config(endpoint=endpoint, state_file=state, accept_packages=True, heartbeat_interval_ms=60_000,
                                 mode="core", splice_downloads=splice, **config), Status(), notes.append)
    try:
        await until("install_package", lambda: any(n.kind == "install_package" or n.kind == "stopped" for n in notes), 60)
        n = next((n for n in notes if n.kind == "install_package"), None)
        assert n is not None, [(n.kind, n.detail) for n in notes]
        with open(n.path, "rb") as f:
            return f.read()
    finally:
        await asyncio.wait_for(client.stop(), 10)


async def serve(app: web.Application, ssl_context=None) -> tuple[web.AppRunner, int]:
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0, ssl_context=ssl_context)
    await site.start()
    return runner, site._server.sockets[0].getsockname()[1]  # type: ignore[union-attr]


@pytest.mark.parametrize("framing", ["content-length", "chunked"])
@pytest.mark.parametrize("splice", [True, False], ids=["splice", "no-splice"])
def test_core_mode_downloads_a_package_trickled_while_file_writes_are_slow(framing: str, splice: bool, tmp_path,
                                                                            slow) -> None:
    slow(0.002)
    file = os.urandom(3 << 20)

    async def run() -> None:
        runner, port = await serve(package_app(file, framing, lambda at: 1 + (at * 31) % 70_000))
        try:
            got = await download(f"http://127.0.0.1:{port}/v1/opamp", str(tmp_path / "state"), splice,
                                 proxy=Proxy.DIRECT)
        finally:
            await runner.cleanup()
        assert got == file

    asyncio.run(run())


# covers: RULE-PROXY-01
@pytest.mark.parametrize("kind", ["http", "https", "socks5"])
def test_core_mode_downloads_go_through_the_proxy_with_tls_and_redirects(kind: str, tmp_path) -> None:
    file = os.urandom(200_000)

    async def run() -> None:
        ca = trustme.CA()
        import ssl

        ctx = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
        ca.issue_cert("127.0.0.1").configure_cert(ctx)
        runner, port = await serve(package_app(file, "content-length", lambda at: 65536, redirect=True), ctx)
        proxy_tls = ctx if kind == "https" else None
        proxy = await (Socks5Proxy if kind == "socks5" else HttpProxy)("agent", "secret", proxy_tls).start()
        try:
            got = await download(f"https://127.0.0.1:{port}/v1/opamp", str(tmp_path / "state"), True,
                                 tls=TLS(ca_pem=ca.cert_pem.bytes()),
                                 proxy=f"{kind}://agent:secret@127.0.0.1:{proxy.port}")
        finally:
            await proxy.close()
            await runner.cleanup()
        assert got == file
        assert len(proxy.log) >= 2, "the OpAMP connection and the download's"
        assert all(e.get("user") == "agent" for e in proxy.log)

    asyncio.run(run())


# covers: RULE-CLIENT-16
def test_core_mode_stop_while_the_upgrade_is_held_returns_and_closes_the_connection() -> None:
    """A server that holds the upgrade; stop 300 ms after start returns within 5 s, and
    the server sees the connection closed."""
    async def run() -> None:
        closed = asyncio.Event()

        async def hold(r: asyncio.StreamReader, w: asyncio.StreamWriter) -> None:
            await r.readuntil(b"\r\n\r\n")
            try:
                await asyncio.wait_for(r.read(), 10)  # it never answers; EOF when the client goes
            finally:
                closed.set()

        server, port = await tcp_server(hold)
        try:
            client = Client.start(Config(endpoint=f"ws://127.0.0.1:{port}/v1/opamp", mode="core", proxy=Proxy.DIRECT),
                                  Status())
            await asyncio.sleep(0.3)
            started = time.monotonic()
            await asyncio.wait_for(client.stop(), 5)
            assert time.monotonic() - started < 5
            await asyncio.wait_for(closed.wait(), 5)
        finally:
            close_server(server)

    asyncio.run(run())


def test_mode_is_core_unless_platform_is_asked_for() -> None:
    """With nothing said the client runs in core mode; "platform" is platform mode; what the mode
    doesn't honour is an error, never ignored."""
    async def run() -> None:
        assert Config(endpoint="http://127.0.0.1:1/v1/opamp").mode == "core"
        for config, core_mode in ((Config(endpoint="http://127.0.0.1:1/v1/opamp", proxy=Proxy.DIRECT), True),
                             (Config(endpoint="http://127.0.0.1:1/v1/opamp", proxy=Proxy.DIRECT, mode="core"), True),
                             (Config(endpoint="http://127.0.0.1:1/v1/opamp", proxy=Proxy.DIRECT, mode="platform"), False)):
            client = Client.start(config, Status())
            assert (client._streams is not None) == core_mode, config.mode
            await client.stop()
        with pytest.raises(ValueError, match='mode is "core" or "platform"'):
            Client.start(Config(endpoint="http://127.0.0.1:1/v1/opamp", mode="node"), Status())
        with pytest.raises(ValueError, match="splice_downloads in platform mode"):
            Client.start(Config(endpoint="http://127.0.0.1:1/v1/opamp", mode="platform", splice_downloads=False), Status())
        await Client.start(Config(endpoint="http://127.0.0.1:1/v1/opamp", proxy=Proxy.DIRECT, splice_downloads=False),
                           Status()).stop()

    asyncio.run(run())


def test_the_core_hears_of_an_opamp_server_through_core_mode(tmp_path) -> None:
    """The core-mode client against the tests' own server, every byte the core's."""
    async def run() -> None:
        server = await OpampServer().start()
        notes: list[Notification] = []
        client = Client.start(Config(endpoint=server.endpoint("ws"), mode="core", heartbeat_interval_ms=50),
                              Status(), notes.append)
        await until("messages", lambda: len(server.arrivals) >= 3)
        await client.stop()
        await server.close()
        assert [n.kind for n in notes][:1] == ["connected"] and notes[-1].kind == "stopped"

    asyncio.run(run())
