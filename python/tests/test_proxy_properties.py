# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Python mode's own CONNECT and SOCKS5 client (``_transport._ProxyConnector``: Rust mode uses
the core's, proved) against a fake proxy whose answers Hypothesis writes:
split anywhere, too long, cut short and then closed, reset or left hanging, malformed (a status
line that isn't one, a status in other digits, a SOCKS version, method, reply or address type
that isn't one), or followed by bytes before the server has said anything. On the seeded loop's
in-memory network, so every cut and every order is the plan's and the seed's.

An independent reading of the two protocols (RFC 1928 and 1929, RFC 9110's CONNECT) says what
each plan must come to, and the connector must agree:

- a tunnel, only for an answer that grants one;
- otherwise aiohttp's ClientProxyConnectionError (what the executor reports as the request or
  connection failing), with the connection to the proxy closed: never another exception;
- a proxy that stops answering and holds the connection: the connect timeout, nothing else;
- either of the first two where only timing decides (bytes after the answer, a close racing
  the last of it, an answer longer than the 16 KiB the connector reads, in one piece or not).

Runs: ``HYPOTHESIS_EXAMPLES`` (default 300)."""

from __future__ import annotations

import asyncio
import os
import socket
from collections import deque
from types import SimpleNamespace

import aiohttp
import pytest
import yarl
from aiohttp.client_reqrep import ConnectionKey
from hypothesis import given, settings, strategies as st
from hypothesis import note as _note
from hypothesis.errors import InvalidArgument

from _sched_loop import MemTransport, Network, SeededLoop
from opamp_client._transport import Proxy, Transport, _ProxyConnector

HEAD_LIMIT = 16 * 1024
CONNECT_TIMEOUT = 30.0
USER, PASSWORD = b"user", b"secret"


def note(text: str) -> None:
    try:
        _note(text)
    except InvalidArgument:  # outside a Hypothesis test (a replay)
        pass


class FakeProxy(asyncio.Protocol):
    """Answers the client's i-th message with stage i, in pieces (one per turn); after the last
    stage (or a message past it) it ends as the plan says: stays open, closes or resets. A stage
    begun while the one before is still going out follows the rest of it, as on one connection."""

    def __init__(self, loop: SeededLoop, stages: list[bytes], cuts: list[list[int]], end: str) -> None:
        self.loop = loop
        self.stages = deque(zip(stages, cuts))
        self.end = end
        self.heard: list[bytes] = []
        self.transport: MemTransport | None = None
        self.ended = False
        # What is still to go out, in order: a piece, or None for the end once the rest is out.
        self.outbox: deque[bytes | None] = deque()

    def connection_made(self, transport) -> None:
        self.transport = transport

    def data_received(self, data: bytes) -> None:
        self.heard.append(bytes(data))
        if self.stages:
            reply, cuts = self.stages.popleft()
            self._queue([*_pieces(reply, cuts), *([None] if not self.stages else [])])
        else:
            self._queue([None])

    def _queue(self, items: list[bytes | None]) -> None:
        idle = not self.outbox
        self.outbox.extend(items)
        if idle:
            self._send()

    def _send(self) -> None:
        t = self.transport
        if t is None or t.closing or not self.outbox:
            return
        piece = self.outbox.popleft()
        if piece is None:
            self.outbox.clear()
            self._finish()
            return
        t.write(piece)
        if self.outbox:
            self.loop.call_soon(self._send)

    def _finish(self) -> None:
        if self.ended or self.transport is None:
            return
        self.ended = True
        if self.end == "close":
            self.transport.close()
        elif self.end == "abort":
            self.transport.abort()


def _pieces(data: bytes, cuts: list[int]) -> list[bytes]:
    if not data:
        return []
    at = sorted({c % len(data) for c in cuts} - {0})
    return [data[a:b] for a, b in zip([0, *at], [*at, len(data)])]


class App(asyncio.Protocol):
    """The protocol the tunnel is handed to (aiohttp's, normally)."""

    def __init__(self) -> None:
        self.made = 0
        self.data = bytearray()

    def connection_made(self, transport) -> None:
        self.made += 1

    def data_received(self, data: bytes) -> None:
        self.data += data

    def connection_lost(self, exc) -> None:
        pass


# -- what the plan must come to


class _Short(Exception):
    pass


class Model:
    """The client's side of the exchange, read from the RFCs: what it must come to."""

    def __init__(self, stages: list[bytes], end: str) -> None:
        self.stages, self.end = stages, end
        self.buf = bytearray()
        self.sent = 0

    def send(self) -> None:
        """The client sends its next message: the proxy answers with its next stage."""
        if self.sent < len(self.stages):
            self.buf += self.stages[self.sent]
            self.sent += 1

    def read(self, n: int) -> bytes:
        if len(self.buf) < n:
            raise _Short
        out = bytes(self.buf[:n])
        del self.buf[:n]
        return out

    def short(self) -> str:
        # Nothing more comes before the client's next message, which waits for this answer.
        return "error" if self.sent == len(self.stages) and self.end != "open" else "timeout"

    def done(self) -> str:
        if self.buf:
            return "either"  # bytes after the answer: caught if they came with it
        return "ok" if self.end == "open" else "either"  # a close racing the end of the answer

    def connect(self) -> str:
        self.send()
        i = self.buf.find(b"\r\n\r\n")
        if i < 0:
            return "error" if len(self.buf) > HEAD_LIMIT else self.short()
        head = bytes(self.buf[:i])
        del self.buf[:i + 4]
        status = head.split(b"\r\n", 1)[0].decode("latin-1")
        parts = status.split(" ", 2)
        good = (len(parts) >= 2 and parts[0].startswith("HTTP/1.") and parts[1].isascii()
                and parts[1].isdigit() and 200 <= int(parts[1]) < 300)
        verdict = self.done() if good else "error"
        if i + 4 > HEAD_LIMIT + 1:
            return "either" if verdict != "error" else "error"
        return verdict

    def socks5(self, auth: bool) -> str:
        try:
            self.send()
            version, method = self.read(2)
            if version != 5:
                return "error"
            if method == 2 and auth:
                self.send()
                _, status = self.read(2)
                if status != 0:
                    return "error"
            elif method != 0:
                return "error"
            self.send()
            version, reply, _, kind = self.read(4)
            if version != 5 or reply != 0:
                return "error"
            if kind == 1:
                self.read(6)
            elif kind == 4:
                self.read(18)
            elif kind == 3:
                (n,) = self.read(1)
                self.read(n + 2)
            else:
                return "error"
            return self.done()
        except _Short:
            return self.short()


# -- plans

_STATUS = st.one_of(
    st.sampled_from(["200", "200", "204", "299", "300", "407", "500", "2000", "20", "", "²00", "2³0", "-200", "+200"]),
    st.text(alphabet="0123456789²³¹x", min_size=1, max_size=4))


@st.composite
def connect_plans(draw):
    version = draw(st.sampled_from(["HTTP/1.1"] * 4 + ["HTTP/1.0", "HTTP/2", "HTTP/1.x", "http/1.1", ""]))
    line = f"{version} {draw(_STATUS)} {draw(st.sampled_from(['Connection established', 'OK', '']))}"
    if draw(st.integers(0, 3)) == 0:
        line = draw(st.sampled_from([line.replace(" ", "  ", 1), line.strip(), "garbage", "HTTP/1.1"]))
    headers = draw(st.lists(st.sampled_from(["Proxy-Agent: x", "Via: 1.1 p", "Via: 1.1 p", "X: " + "y" * 9000]),
                            max_size=2))
    reply = ("\r\n".join([line, *headers]) + "\r\n\r\n").encode("latin-1")
    if draw(st.integers(0, 3)) == 0:  # cut short
        reply = reply[:draw(st.integers(0, len(reply)))]
    if draw(st.integers(0, 3)) == 0:  # the server's bytes, too early
        reply += draw(st.binary(min_size=1, max_size=20))
    return [reply]


@st.composite
def socks_plans(draw, auth: bool):
    # A good exchange, then (mostly) a fault or two in it.
    kind = draw(st.sampled_from([1, 1, 3, 4]))
    address = {1: bytes(4), 4: bytes(16), 3: bytes([n := draw(st.integers(0, 40))]) + b"h" * n}[kind]
    greet = [5, 2 if auth else 0]
    answer = [5, 0, 0, kind]
    faults = draw(st.lists(st.sampled_from(["version", "method", "auth", "reply", "kind", "short", "merge", "extra"]),
                           max_size=2))
    if "version" in faults:
        greet[0] = draw(st.sampled_from([4, 0, 6]))
    if "method" in faults:
        greet[1] = draw(st.sampled_from([0, 2, 0xFF, 1, 3]))
    if "reply" in faults:
        answer[draw(st.integers(0, 1))] = draw(st.sampled_from([1, 4, 5, 8, 9]))
    if "kind" in faults:
        answer[3] = draw(st.sampled_from([0, 2, 7, 1, 3, 4]))
    stages = [bytes(greet)]
    if auth and greet[1] == 2:
        stages.append(bytes([1, draw(st.sampled_from([1, 0xFF])) if "auth" in faults else 0]))
    stages.append(bytes(answer) + address + b"\x04\x38")
    if "short" in faults:  # one stage cut short, and nothing after it
        i = draw(st.integers(0, len(stages) - 1))
        stages = stages[:i] + [stages[i][:draw(st.integers(0, len(stages[i]) - 1))]]
    if "merge" in faults and len(stages) > 1:  # two answers at once
        i = draw(st.integers(0, len(stages) - 2))
        stages[i:i + 2] = [stages[i] + stages[i + 1]]
    if "extra" in faults:  # the server's bytes, too early
        stages[-1] += draw(st.binary(min_size=1, max_size=20))
    return stages


@st.composite
def plans(draw):
    kind = draw(st.sampled_from(["connect", "socks5", "socks5-auth"]))
    stages = draw(connect_plans() if kind == "connect" else socks_plans(kind == "socks5-auth"))
    return {
        "kind": kind, "stages": stages,
        "cuts": [draw(st.lists(st.integers(0, 20_000), max_size=4)) for _ in stages],
        "end": draw(st.sampled_from(["open", "open", "close", "abort"])),
        "seed": draw(st.integers(0, 2**32 - 1)),
        "target": draw(st.sampled_from(["server.test", "10.1.2.3", "::1"])),
    }


def _proxy(kind: str) -> Proxy:
    if kind == "connect":
        return Proxy("http://proxy.test:3128")
    if kind == "socks5":
        return Proxy("socks5://proxy.test:1080")
    return Proxy(f"socks5://{USER.decode()}:{PASSWORD.decode()}@proxy.test:1080")


async def _tunnel(loop: SeededLoop, plan: dict, proxies: list[FakeProxy]):
    connector = _ProxyConnector(Transport(proxy=_proxy(plan["kind"])), Transport().ssl)
    host = plan["target"]
    key = ConnectionKey(host, 443, False, None, None, None, None, None)
    req = SimpleNamespace(url=yarl.URL(f"http://{host if ':' not in host else f'[{host}]'}:443/"), connection_key=key)
    app = App()
    try:
        transport, proto = await connector._wrap_create_connection(
            lambda: app, addr_infos=[(socket.AF_INET, socket.SOCK_STREAM, 6, "", (host, 443))], req=req,
            timeout=aiohttp.ClientTimeout(sock_connect=CONNECT_TIMEOUT), client_error=aiohttp.ClientConnectorError,
            ssl=None, server_hostname=None)
        return "ok", (transport, proto, app)
    except (TimeoutError, asyncio.TimeoutError) as e:
        return "timeout", e
    except aiohttp.ClientConnectorError as e:
        return "error", e
    finally:
        await connector.close()


# covers: RULE-PROXY-01
@settings(max_examples=int(os.environ.get("HYPOTHESIS_EXAMPLES", "300")), deadline=None)
@given(plan=plans())
def test_the_proxy_connector_agrees_with_the_protocols(plan: dict) -> None:
    check(plan)


def check(plan: dict) -> None:
    note(f"loop seed {plan['seed']}")
    loop = SeededLoop(plan["seed"], auto=True)
    proxies: list[FakeProxy] = []

    def serve(_host: str, _port: int) -> FakeProxy:
        p = FakeProxy(loop, plan["stages"], plan["cuts"], plan["end"])
        proxies.append(p)
        return p

    loop.network = net = Network(loop, serve, accept_all=True)
    model = Model(plan["stages"], plan["end"])
    want = model.connect() if plan["kind"] == "connect" else model.socks5(plan["kind"] == "socks5-auth")
    try:
        got, what = loop.run_until_complete(_tunnel(loop, plan, proxies))
        note(f"wanted {want}, got {got}: {what!r}")
        assert got == want or (want == "either" and got in ("ok", "error")), f"wanted {want}, got {got}: {what!r}"
        if got == "error":
            assert isinstance(what, aiohttp.ClientProxyConnectionError), f"{type(what).__name__}: {what}"
        if got != "ok":
            loop.run_until_complete(asyncio.sleep(0))
            assert not net.open(), "the connection to the proxy is left open"
        else:
            transport, proto, app = what
            assert proto is app and app.made == 1, "the tunnel is handed over once"
            assert transport is net.transports[0]
            transport.abort()
        if plan["kind"] != "connect" and proxies and proxies[0].heard:
            # What the client said: a greeting offering what it has.
            greet = proxies[0].heard[0]
            assert greet == (b"\x05\x02\x00\x02" if plan["kind"] == "socks5-auth" else b"\x05\x01\x00"), greet
    finally:
        loop.finish()


@pytest.mark.parametrize("status", ["²00", "2³4", "¹99"])
def test_a_status_in_other_digits_is_refused_as_the_proxy_s_error(status: str) -> None:
    # Found by the test above: str.isdigit takes "²00" for a number, and int() then raised a
    # ValueError out of the connector instead of its own error.
    check({
        "kind": "connect", "stages": [f"HTTP/1.1 {status} OK\r\n\r\n".encode("latin-1")], "cuts": [[]],
        "end": "open", "seed": 1, "target": "server.test"})


def test_answers_that_overlap_reach_the_client_in_order() -> None:
    # Found by the test above (nightly 2026-10-11): the greeting's answer and the authentication's
    # sent at once and in pieces, and the client's next message arriving while they were still
    # going out. The fake proxy began its next answer then, ahead of the rest of the one before, so
    # the client read bytes in an order no proxy sends them, and was "refused".
    check({
        "kind": "socks5-auth", "stages": [b"\x05\x02\x01\x00", b"\x05\x00\x00\x01\x00\x00\x00\x00\x048"],
        "cuts": [[1, 2, 3], []], "end": "open", "seed": 1, "target": "server.test"})
