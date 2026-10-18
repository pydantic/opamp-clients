# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Core mode's byte-stream glue (src/pydantic_opamp_client/_streams.py) over random plans (Hypothesis),
on fake transports so every cut is the plan's: the peer's bytes arrive in pieces cut anywhere
(some before the core has heard the stream is open, or its TLS up), the core's writes are split
anywhere, part of the body may be spliced to a file, and the core closes the stream at any point:
before the connect completes, racing it (closed in the same step that completes it, with a
connect that honours its cancel or one that swallows it), mid-handshake, or between any two
pieces. Whatever the plan:

- the core hears stream_opened (and stream_tls_ready) first, then exactly the bytes that arrived
  while it wanted them, in order, less those spliced, and stream_spliced once all of those came;
- the peer gets exactly the bytes the core wrote while the stream was ready, in order;
- after the close, the core hears nothing more of the stream; a transport that arrives after it
  is aborted at once; and once shut down, every transport is aborted and nothing is in flight.
"""

from __future__ import annotations

import asyncio
import hashlib
import os

from hypothesis import given, settings
from hypothesis import strategies as st

from pydantic_opamp_client import Proxy
from pydantic_opamp_client._proto.opamp.core.v1 import core_pb2 as corepb
from pydantic_opamp_client._streams import Streams
from pydantic_opamp_client._transport import Transport


class FakeTransport(asyncio.Transport):
    """A transport that keeps what is written and how it ended."""

    def __init__(self, inner: FakeTransport | None = None) -> None:
        super().__init__()
        self.inner = inner  # the transport TLS runs on, which goes with it
        self.written = bytearray()
        self.aborted = False
        self.closing = False
        self.paused = False

    def write(self, data) -> None:
        assert not self.closing, "written after its close"
        self.written += data

    def abort(self) -> None:
        self.aborted = self.closing = True
        if self.inner is not None:
            self.inner.abort()

    def close(self) -> None:
        self.closing = True
        if self.inner is not None:
            self.inner.close()

    def is_closing(self) -> bool:
        return self.closing

    def get_write_buffer_size(self) -> int:
        return 0

    def pause_reading(self) -> None:
        self.paused = True

    def resume_reading(self) -> None:
        self.paused = False

    def is_reading(self) -> bool:
        return not self.paused


# Where the core closes the stream: before the connect completes, in the step that completes it,
# while the connect is finishing (after connection_made), during the TLS handshake, or after
# action i of the plan; or never (None).
BEFORE, RACING, MID_CONNECT, MID_TLS = "before", "racing", "mid-connect", "mid-tls"


@st.composite
def plans(draw):
    payload = draw(st.binary(max_size=600))
    cuts = []
    rest = len(payload)
    while rest > 0:
        n = draw(st.integers(1, max(1, min(rest, 80))))
        cuts.append(n)
        rest -= n
    writes = draw(st.lists(st.binary(min_size=1, max_size=40), max_size=6))
    tls = draw(st.booleans())
    # Pieces that arrive before the core has heard the stream is ready (open, or TLS up).
    early = draw(st.integers(0, len(cuts)))
    # The rest, interleaved with the writes.
    later = ["chunk"] * (len(cuts) - early) + ["write"] * len(writes)
    order = draw(st.permutations(later))
    points = [BEFORE, RACING, MID_CONNECT] + ([MID_TLS] if tls else []) + list(range(len(order) + 1))
    close = draw(st.one_of(st.none(), st.sampled_from(points)))
    return {
        "payload": payload, "cuts": cuts, "writes": writes, "tls": tls, "early": early, "order": order,
        "close": close, "abort": draw(st.booleans()), "swallows": draw(st.booleans()),
        "splice": draw(st.one_of(st.none(), st.integers(1, max(1, len(payload))))),
    }


# covers: RULE-CLIENT-14, RULE-CLIENT-15
@settings(max_examples=int(os.environ.get("HYPOTHESIS_EXAMPLES", "300")), deadline=None)
@given(plan=plans())
def test_bytes_reach_the_core_and_the_peer_as_sent_whatever_the_cuts_and_the_close(plan, tmp_path_factory) -> None:
    tmp = tmp_path_factory.mktemp("streams")
    asyncio.run(_run(plan, str(tmp / "state")))


async def _run(plan: dict, state: str) -> None:
    payload, cuts, close = plan["payload"], plan["cuts"], plan["close"]
    chunks = []
    at = 0
    for n in cuts:
        chunks.append(payload[at:at + n])
        at += n
    early, later = chunks[:plan["early"]], iter(chunks[plan["early"]:])
    writes = iter(plan["writes"])

    loop = asyncio.get_running_loop()
    connected, handshaken, mid = loop.create_future(), loop.create_future(), loop.create_future()
    made: list[FakeTransport] = []
    proto: list[asyncio.Protocol] = []

    async def connect(factory, host, port):
        p = factory()
        proto.append(p)
        try:
            await asyncio.shield(connected)
        except asyncio.CancelledError:
            if not plan["swallows"]:
                raise
            await connected  # it connects all the same
        t = FakeTransport()
        made.append(t)
        p.connection_made(t)
        if not plan["tls"]:
            for c in early:
                p.data_received(c)
        # A turn of the loop before the connect returns: a close may come in it.
        mid.set_result(None)
        try:
            await asyncio.sleep(0)
        except asyncio.CancelledError:
            if not plan["swallows"]:
                raise
        return t, p

    async def start_tls(raw, p, ctx, server_hostname=None):
        try:
            await asyncio.shield(handshaken)
        except asyncio.CancelledError:
            if not plan["swallows"]:
                raise
            await handshaken
        t = FakeTransport(raw)
        made.append(t)
        for c in early:
            p.data_received(c)
        return t

    got: list[corepb.Input] = []
    s = Streams(got.append, Transport(proxy=Proxy.DIRECT), state, connect=connect, start_tls=start_tls)
    closed_at: int | None = None  # how many inputs the core had when it closed the stream
    wanted = bytearray()  # the bytes that arrived while the core wanted them
    written = bytearray()  # the bytes the core wrote while the stream was ready

    def close_now() -> None:
        nonlocal closed_at
        closed_at = len(got)
        s.do(corepb.Effect(stream_close=corepb.StreamClose(id=1, abort=plan["abort"])))

    def ready() -> bool:
        k = [i.WhichOneof("kind") for i in got]
        return ("stream_tls_ready" if plan["tls"] else "stream_opened") in k

    async def settle(until=lambda: False) -> None:
        for _ in range(50):
            if until():
                return
            await asyncio.sleep(0)

    s.do(corepb.Effect(stream_open=corepb.StreamOpen(id=1, host="server", port=1)))
    # The body's first bytes go to the file (the core asks before they can come).
    if plan["splice"] is not None:
        s.do(corepb.Effect(stream_splice=corepb.StreamSplice(id=1, download=7, bytes=plan["splice"])))
    await asyncio.sleep(0)
    if close == BEFORE:
        close_now()
    connected.set_result(None)
    if close == RACING:
        close_now()
    if close == MID_CONNECT and closed_at is None:
        await settle(mid.done)
        close_now()
    await settle(lambda: closed_at is not None or bool(got))
    if closed_at is None:
        assert [i.WhichOneof("kind") for i in got][:1] == ["stream_opened"]
        if not plan["tls"]:
            wanted += b"".join(early)
        else:
            s.do(corepb.Effect(stream_start_tls=corepb.StreamName(id=1, host="server")))
            await asyncio.sleep(0)
            if close == MID_TLS:
                close_now()
            handshaken.set_result(None)
            await settle(lambda: closed_at is not None or ready())
            if closed_at is None:
                assert ready()
                wanted += b"".join(early)
    await settle()
    if close == 0 and closed_at is None:
        close_now()
    for i, what in enumerate(plan["order"], start=1):
        if what == "chunk":
            c = next(later)
            if proto:
                proto[0].data_received(c)
            if closed_at is None and ready():
                wanted += c
        else:
            w = next(writes)
            s.do(corepb.Effect(stream_write=corepb.WsMessage(id=1, data=w)))
            if closed_at is None and ready():
                written += w
        if close == i and closed_at is None:
            close_now()
    for _ in range(3):
        await asyncio.sleep(0)

    # The core heard the stream's events in order, and nothing after its close.
    mine = [i for i in got]
    if closed_at is not None:
        assert len(got) == closed_at, f"heard after the close: {got[closed_at:]}"
    k = [i.WhichOneof("kind") for i in mine]
    expect_head = []
    if "stream_opened" in k:
        expect_head.append("stream_opened")
        if plan["tls"] and "stream_tls_ready" in k:
            expect_head.append("stream_tls_ready")
    assert k[:len(expect_head)] == expect_head, k
    assert set(k[len(expect_head):]) <= {"stream_data", "stream_spliced"}, k
    data = b"".join(i.stream_data.data for i in mine if i.WhichOneof("kind") == "stream_data")
    spliced_n = 0 if plan["splice"] is None else min(plan["splice"], len(wanted))
    assert data == bytes(wanted[spliced_n:]), "the bytes, in order, less those spliced"
    assert k.count("stream_spliced") == (1 if plan["splice"] is not None and len(wanted) >= plan["splice"] else 0)
    if "stream_spliced" in k and "stream_data" in k:
        assert k.index("stream_spliced") < k.index("stream_data"), "spliced bytes come first"

    # The peer got the bytes written while the stream was ready.
    last = made[-1] if made else None
    if last is not None and closed_at is None:
        assert bytes(last.written) == bytes(written)
    if closed_at is not None:
        for t in made:
            assert t.closing, "closed"
            if plan["abort"] or not ready():
                assert t.aborted, "aborted, at once"

    # A complete splice is the file's.
    if plan["splice"] is not None and "stream_spliced" in k and closed_at is None:
        s.do(corepb.Effect(download_done=corepb.Id(id=7)))
        for _ in range(500):
            if any(i.WhichOneof("kind") == "downloaded" for i in got):
                break
            await asyncio.sleep(0.002)
        d = [i for i in got if i.WhichOneof("kind") == "downloaded"]
        assert d and d[0].downloaded.sha256 == hashlib.sha256(bytes(wanted[:plan["splice"]])).digest()

    await s.shutdown()
    assert all(t.aborted for t in made), "shutdown aborts every transport"
    assert not s._in.tasks and not s._in.streams and not s._in.timers and not s._in.lingering
