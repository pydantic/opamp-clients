# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Core mode's streams (src/logfire_fleet_client/_streams.py): backpressure, and the in-flight
registry: a close or a shutdown ends what it names, a stream that connects (or finishes its TLS
handshake) after its close is aborted as it arrives, and shutdown is bounded."""

from __future__ import annotations

import asyncio
import os
import time
from collections.abc import Callable

import pytest
import trustme
from _servers import OpampServer

from logfire_fleet_client import TLS, Client, Config, Notification, Proxy, Status
from logfire_fleet_client import _streams
from logfire_fleet_client import opamp_pb2 as pb
from logfire_fleet_client._proto.opamp.core.v1 import core_pb2 as corepb
from logfire_fleet_client._streams import Streams, proxy_choice
from logfire_fleet_client._transport import Transport


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


def new(tmp_path, deliver: Callable[[corepb.Input], None] | None = None, **kw) -> tuple[Streams, list[corepb.Input], str]:
    """A Streams (on the running loop), the inputs it reports, and a path for the test's own use."""
    got: list[corepb.Input] = []
    s = Streams(deliver or got.append, Transport(proxy=Proxy.DIRECT), **kw)
    return s, got, str(tmp_path / "state")


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
            # Two bursts: Windows takes a whole send into the socket's buffer while that holds less
            # than SO_SNDBUF, so the first can leave asyncio's buffer empty; the second can't.
            for _ in range(2):
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


# The client certificate goes only to a TLS handshake the core says is with the server's own
# origin (stream_start_tls's server_origin): any other gets the context without it, whose CAs are
# the same.
# covers: RULE-TLS-02
def test_the_client_certificate_only_on_a_stream_to_the_servers_origin(tmp_path) -> None:
    ca = trustme.CA()
    agent = ca.issue_cert("agent")
    tls = TLS(ca_pem=ca.cert_pem.bytes(), cert_pem=agent.cert_chain_pems[0].bytes(),
              key_pem=agent.private_key_pem.bytes())

    async def run() -> None:
        contexts: list[object] = []

        async def connect(factory, host, port):
            proto = factory()
            t = FakeTransport()
            proto.connection_made(t)
            return t, proto

        async def start_tls(raw, proto, ctx, server_hostname=None):
            contexts.append(ctx)
            return FakeTransport()

        transport = Transport(tls, proxy=Proxy.DIRECT)
        assert transport.ssl is not transport.elsewhere_ssl
        got: list[corepb.Input] = []
        s = Streams(got.append, transport, connect=connect, start_tls=start_tls)
        for id_, origin in ((1, True), (2, False)):
            s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=id_, host="h", port=1)))
            await until("opened", lambda: len(got) == 2 * id_ - 1)
            s.do(corepb.Effect(stream_start_tls=corepb.StreamName(id=id_, host="h", server_origin=origin)))
            await until("TLS", lambda: len(got) == 2 * id_)
            assert got[-1].WhichOneof("kind") == "stream_tls_ready"
        assert contexts == [transport.ssl, transport.elsewhere_ssl]
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
        s = Streams(got.append, Transport(proxy=Proxy("socks5h://u:p%40ss@proxy.test:1080", resolve_locally=True)))
        s.do(corepb.Effect(proxy_query=corepb.Url(id=4, url="wss://opamp.test/v1/opamp")))
        assert got[-1].proxy_answer == corepb.ProxyAnswer(id=4, proxy="socks5h://u:p%40ss@proxy.test:1080",
                                                          resolve_locally=True)
        s = Streams(got.append, Transport(proxy=Proxy("http://proxy.test:3128", headers={"X-T": "1"})))
        s.do(corepb.Effect(proxy_query=corepb.Url(id=5, url="http://opamp.test/v1/opamp")))
        assert got[-1].proxy_answer == corepb.ProxyAnswer(id=5, proxy="http://proxy.test:3128",
                                                          headers=[corepb.Header(name="X-T", value="1")])
        # The environment's is the core's to pick (it was told the variables): asked, none.
        monkeypatch.setenv("HTTPS_PROXY", "proxy.test:3128")
        s = Streams(got.append, Transport(core_mode=True))
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
        monkeypatch.setenv("HTTPS_PROXY", "http://us\udce9r:pw@proxy.test:3128")
        choice = proxy_choice(Transport(core_mode=True))
        assert [(h.name, h.value) for h in choice.environment if h.name == "HTTPS_PROXY"] == [
            ("HTTPS_PROXY", "http://us�r:pw@proxy.test:3128")], choice
        choice.SerializeToString()

    asyncio.run(run())


# The client in core mode.

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

    asyncio.run(run())


def test_the_core_hears_of_an_opamp_server_through_core_mode(tmp_path) -> None:
    """The core-mode client against the tests' own server, every byte the core's."""
    async def run() -> None:
        server = await OpampServer().start()
        notes: list[Notification] = []
        client = Client.start(Config(endpoint=server.endpoint("ws"), mode="core", heartbeat_interval_ms=50,
                                     capabilities=pb.AgentCapabilities_ReportsHeartbeat),
                              Status(), notes.append)
        await until("messages", lambda: len(server.arrivals) >= 3)
        await client.stop()
        await server.close()
        assert [n.kind for n in notes][:1] == ["connected"] and notes[-1].kind == "stopped"

    asyncio.run(run())
