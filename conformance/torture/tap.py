# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""What a client of ours sent and read over WebSocket, in its own order, from its core tap.

The server's log says when it read a message and wrote an answer; under a slow proxy that says
little of when the client sent the one or read the other (messages queue behind the latency). The
core tap (opamp-tap's records, run.py's check_taps) has every call into the core in the order it
happened: the messages the core sent (ws_send, or in Rust mode the WebSocket frames it wrote on a
stream) and the ones it read (ws_message, or the frames on a stream). Paired in order on each
connection, as the server answers them, they say for each message the client sent whether it had
read the answer to an earlier one by then.

Only what that needs is decoded, by hand (as run.py's saved_state does), so the harness needs no
protobuf package: Step, Reply, Output, Effect and WsMessage of proto/opamp/core/v1/core.proto,
AgentToServer.sequence_num, ServerToAgent.error_response.type.
"""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

STEP_IN, STEP_OUT = 3, 4
# core.proto field numbers.
STEP_INPUT, REPLY_OUTPUT, OUTPUT_EFFECTS = 2, 1, 1
INPUT_WS_MESSAGE, INPUT_STREAM_DATA = 12, 29
EFFECT_WS_SEND, EFFECT_STREAM_WRITE = 3, 14
WS_MESSAGE_ID, WS_MESSAGE_DATA = 1, 2
# opamp.proto field numbers.
A2S_SEQUENCE_NUM, S2A_ERROR_RESPONSE, ERROR_TYPE = 2, 2, 1
S2A_AGENT_IDENTIFICATION, NEW_INSTANCE_UID = 8, 1
BAD_REQUEST = 1  # ServerErrorResponseType


@dataclass
class Exchange:
    """One message the client sent over WebSocket, and the answer it read to it, if it did."""
    tap: int
    conn: tuple
    seq: int
    sent: int                 # its place in the tap's order of calls
    read: int | None = None   # where in that order the client read the answer
    error: int | None = None  # the answer's ServerErrorResponseType, if it was an error
    assigned: str | None = None  # the instance UID the answer assigned (hex), if it did


def _varint(b: bytes, i: int) -> tuple[int, int]:
    shift = value = 0
    while True:
        if i >= len(b):
            raise ValueError("varint cut short")
        c = b[i]
        i += 1
        value |= (c & 0x7F) << shift
        shift += 7
        if not c & 0x80:
            return value, i


def fields(b: bytes):
    """(number, value) of each field of a protobuf message: an int, or bytes for a length."""
    i = 0
    while i < len(b):
        key, i = _varint(b, i)
        num, wire = key >> 3, key & 7
        if wire == 0:
            value, i = _varint(b, i)
        elif wire == 2:
            n, i = _varint(b, i)
            value, i = b[i:i + n], i + n
        elif wire == 1:
            value, i = b[i:i + 8], i + 8
        elif wire == 5:
            value, i = b[i:i + 4], i + 4
        else:
            raise ValueError(f"wire type {wire}")
        yield num, value


def field(b: bytes, num: int, default=None):
    for n, v in fields(b):
        if n == num:
            return v
    return default


def records(data: bytes):
    """(kind, bytes) of each whole record of a tap."""
    i = 0
    while i + 13 <= len(data):
        kind, n = data[i], int.from_bytes(data[i + 1:i + 5], "little")
        if i + 13 + n > len(data):
            return
        yield kind, data[i + 13:i + 13 + n]
        i += 13 + n


class _Stream:
    """One direction of a Rust-mode stream: HTTP heads (a CONNECT's, the upgrade's), then
    WebSocket frames once the upgrade went through."""

    def __init__(self, outgoing: bool):
        self.outgoing, self.buf, self.ws, self.partial = outgoing, bytearray(), False, b""

    def feed(self, data: bytes) -> list[bytes]:
        self.buf += data
        out: list[bytes] = []
        while True:
            if not self.ws:
                end = self.buf.find(b"\r\n\r\n")
                if end < 0:
                    return out
                head = bytes(self.buf[:end]).lower()
                del self.buf[:end + 4]
                self.ws = (b"upgrade: websocket" in head) if self.outgoing else head.startswith(b"http/1.1 101")
                continue
            msg = self._frame()
            if msg is None:
                return out
            out.extend(msg)

    def _frame(self) -> list[bytes] | None:
        b = self.buf
        if len(b) < 2:
            return None
        fin, op, masked, n, p = b[0] & 0x80, b[0] & 0x0F, b[1] & 0x80, b[1] & 0x7F, 2
        if n == 126:
            if len(b) < 4:
                return None
            n, p = int.from_bytes(b[2:4], "big"), 4
        elif n == 127:
            if len(b) < 10:
                return None
            n, p = int.from_bytes(b[2:10], "big"), 10
        mask = None
        if masked:
            if len(b) < p + 4:
                return None
            mask, p = bytes(b[p:p + 4]), p + 4
        if len(b) < p + n:
            return None
        payload = bytes(b[p:p + n])
        del b[:p + n]
        if mask:
            payload = bytes(c ^ mask[i % 4] for i, c in enumerate(payload))
        if op >= 8:  # control frames: Close, Ping, Pong
            return []
        self.partial = (self.partial + payload) if op == 0 else payload
        if not fin:
            return []
        whole, self.partial = self.partial, b""
        return [whole] if op in (0, 2) else []


def _protobuf(message: bytes) -> bytes:
    """An OpAMP WebSocket message's protobuf: after its header (a varint, 0)."""
    _, i = _varint(message, 0)
    return message[i:]


class _Reader:
    """One tap's sends and reads, in order (exchanges)."""

    def __init__(self, n: int):
        self.n, self.order, self.out = n, 0, []
        self.streams: dict[tuple, _Stream] = {}
        self.unanswered: dict[tuple, list[Exchange]] = {}
        self.unpaired: set[tuple] = set()

    def sent(self, conn: tuple, message: bytes) -> None:
        ex = Exchange(self.n, conn, field(_protobuf(message), A2S_SEQUENCE_NUM, 0), self.order)
        self.out.append(ex)
        self.unanswered.setdefault(conn, []).append(ex)

    def read(self, conn: tuple, message: bytes) -> None:
        waiting = self.unanswered.get(conn)
        if conn in self.unpaired or not waiting:
            self.unpaired.add(conn)
            return
        ex = waiting.pop(0)
        ex.read = self.order
        s2a = _protobuf(message)
        err = field(s2a, S2A_ERROR_RESPONSE)
        ex.error = None if err is None else field(err, ERROR_TYPE, 0)
        ident = field(s2a, S2A_AGENT_IDENTIFICATION)
        uid = None if ident is None else field(ident, NEW_INSTANCE_UID)
        ex.assigned = uid.hex() if uid else None

    def stream(self, conn: tuple, data: bytes, outgoing: bool) -> list[bytes]:
        return self.streams.setdefault((conn, outgoing), _Stream(outgoing)).feed(data)

    def record(self, kind: int, body: bytes) -> None:
        self.order += 1
        if kind == STEP_IN:
            for num, v in fields(field(body, STEP_INPUT, b"")):
                conn = ("ws" if num == INPUT_WS_MESSAGE else "stream", field(v, WS_MESSAGE_ID, 0))
                if num == INPUT_WS_MESSAGE:
                    self.read(conn, field(v, WS_MESSAGE_DATA, b""))
                elif num == INPUT_STREAM_DATA:
                    for m in self.stream(conn, field(v, WS_MESSAGE_DATA, b""), False):
                        self.read(conn, m)
        elif kind == STEP_OUT:
            for num, effect in fields(field(body, REPLY_OUTPUT, b"")):
                if num != OUTPUT_EFFECTS:
                    continue
                for en, v in fields(effect):
                    conn = ("ws" if en == EFFECT_WS_SEND else "stream", field(v, WS_MESSAGE_ID, 0))
                    if en == EFFECT_WS_SEND:
                        self.sent(conn, field(v, WS_MESSAGE_DATA, b""))
                    elif en == EFFECT_STREAM_WRITE:
                        for m in self.stream(conn, field(v, WS_MESSAGE_DATA, b""), True):
                            self.sent(conn, m)


def exchanges(taps: list[Path]) -> list[Exchange]:
    """Every message the client sent over WebSocket, with the answer it read to it, from its core
    taps. An answer belongs to the oldest unanswered message on its connection (the server answers
    in order). A connection that brings a message before any is unanswered (a push) has its
    answers left unpaired from there on: none of them is known."""
    out: list[Exchange] = []
    for n, path in enumerate(taps):
        try:
            data = path.read_bytes()
        except OSError:
            continue
        reader = _Reader(n)
        try:
            for kind, body in records(data):
                reader.record(kind, body)
        except ValueError:
            pass  # a record the reader can't take apart ends what is known of this tap
        out += reader.out
    return out


def match(a2s: list[dict], sent: list[Exchange]) -> dict[int, Exchange]:
    """The server's messages (its a2s events, in the order it read them) paired with the client's
    sends: each with the next send of the same sequence number (a message the server never read,
    lost with its connection, is passed over). By the server's message number."""
    paired: dict[int, Exchange] = {}
    i = 0
    for m in sorted(a2s, key=lambda e: e["msg"]):
        j = i
        while j < len(sent) and sent[j].seq != m.get("seq"):
            j += 1
        if j < len(sent):
            paired[m["msg"]] = sent[j]
            i = j + 1
    return paired
