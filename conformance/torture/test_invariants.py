# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Tests for the torture invariants: each must pass one trace and fail another.

    python3 -m unittest conformance/torture/test_invariants.py
"""
from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "runner"))
sys.path.insert(0, str(HERE))
from invariants import INVARIANTS, Plan, Sample, desired_at, evaluate  # noqa: E402
from test_checks import FULL, UID, UID2, Log  # noqa: E402

# One quiet window from 10 s to 40 s after the server started (wall 100.0); the last 20 s must
# look healthy. Heartbeats every 2 s. A server that chooses configs wants CONFIG throughout.
CONFIG = "ab" * 32
PLAN = Plan(quiet=[(10.0, 40.0)], check_s=20, poll_s=2, storm_per_10s=40, desired=[(0.0, CONFIG)])
T0 = 100.0


def run() -> Log:
    return Log("ws").at(T0 - 0.1).server("listening").driver("started")


def heartbeats(log: Log, start: float, end: float, change: int | None = None) -> Log:
    t = start
    while t < end:
        log.at(T0 + t)
        extra = {"driver_change": change} if change is not None else {}
        log.send(FULL if change is not None else (), **extra).respond()
        t += 2
    return log


def healthy() -> Log:
    """Change 1 at 2 s, delivered, then heartbeats through the quiet window; stops at 45 s."""
    log = run().at(T0 + 1).send(FULL, driver_change=0).respond()
    log.at(T0 + 2).driver("status_change", change=1).at(T0 + 3).send(FULL, driver_change=1).respond()
    return heartbeats(log, 4, 44).at(T0 + 45).stop()


def samples(growth: float) -> list[Sample]:
    return [Sample(40 + 5 * i, 50 + growth * i, 20, 10) for i in range(12)]


FLAT = samples(0)


def rust_server(held_config: str | None, held_change: int = 1) -> Log:
    """A server that says what it holds: the agent applies CONFIG and reports change 1."""
    applied = {"status": "RemoteConfigStatuses_APPLIED", "hash": CONFIG, "error": ""}
    log = run().at(T0 + 1).send(FULL, driver_change=0).respond(offer_config=CONFIG, held_change=0, held_config=None)
    log.at(T0 + 2).driver("status_change", change=1)
    log.at(T0 + 3).send(FULL + ["remote_config_status"], driver_change=1, remote_config_status=applied)
    log.respond(held_change=held_change, held_config=held_config)
    t = 4
    while t < 44:
        log.at(T0 + t).send(()).respond(held_change=held_change, held_config=held_config)
        t += 2
    return log.at(T0 + 45).stop()


def late_change() -> Log:
    """Change 1 at 2 s, delivered; change 2 at 12 s, 2 s into the quiet window (not yet sent)."""
    log = run().at(T0 + 1).send(FULL, driver_change=0).respond()
    log.at(T0 + 2).driver("status_change", change=1).at(T0 + 3).send(FULL, driver_change=1).respond()
    return heartbeats(log, 4, 12).at(T0 + 12).driver("status_change", change=2).at(T0 + 12.5)


def asked_in_every_answer(gap: float) -> Log:
    """Change 1 delivered, then a server that asks for the full state in every answer (the field it
    needs is too large to send) through the quiet window, and a client that sends a message every
    `gap` seconds."""
    log = run().at(T0 + 1).send(FULL, driver_change=0).respond()
    log.at(T0 + 2).driver("status_change", change=1).at(T0 + 3).send(FULL, driver_change=1).respond()
    t = 4.0
    while t < 44:
        log.at(T0 + t).send(["health"]).respond(report_full_state=True)
        t += gap
    return log.at(T0 + 45).stop()


def storm() -> Log:
    """60 messages in 6 seconds."""
    log = run()
    for i in range(60):
        log.at(T0 + 1 + i * 0.1).send(()).respond()
    return log.stop()

def crashed_and_back(uid_after: str, assigned: str | None = None, adopted: bool = False,
                     change_after: int = 1, ok: bool = True) -> Log:
    """Change 1 delivered under UID (the server assigning `assigned` in its answer, and with
    `adopted` the next message using it); the driver killed at 5 s, its state file left whole if
    `ok`, and started again on it; the new run comes back as `uid_after` with `change_after`, and
    keeps going through the quiet window."""
    log = run().at(T0 + 1).send(FULL, driver_change=0).respond()
    log.at(T0 + 2).driver("status_change", change=1).at(T0 + 3).send(FULL, driver_change=1)
    log.respond(**({"assign_uid": assigned} if assigned else {}))
    if adopted:
        log.at(T0 + 4).send(uid=assigned).respond()
    log.at(T0 + 5).crashed(0, ok=ok).at(T0 + 6).driver("started")
    log.at(T0 + 7).send(FULL, seq=0, uid=uid_after, driver_change=change_after).respond()
    t = 8
    while t < 44:
        log.at(T0 + t).send(uid=uid_after).respond()
        t += 2
    return log.at(T0 + 45).stop()


# invariant -> (traces it must pass, traces it must fail); a trace is (log, samples)
CASES = {
    "status_never_goes_back": (
        # A run after a crash numbers its messages from 0 again.
        [(healthy(), FLAT), (crashed_and_back(UID), FLAT)],
        [(run().at(T0 + 1).driver("status_change", change=2).at(T0 + 2).send(FULL, driver_change=2).respond()
          .send(FULL, driver_change=1).respond().stop(), FLAT),
         (run().at(T0 + 1).send(FULL, driver_change=3).respond().stop(), FLAT),  # a change never made
         # Back from a crash with the status the application had before its last change.
         (crashed_and_back(UID, change_after=0), FLAT)],
    ),
    "uid_kept_across_crash": (
        [(crashed_and_back(UID), FLAT),
         # Assigned just before the crash: saved, or not yet (the next message would have said).
         (crashed_and_back(UID2, assigned=UID2), FLAT), (crashed_and_back(UID, assigned=UID2), FLAT),
         (crashed_and_back(UID2, assigned=UID2, adopted=True), FLAT)],
        # A new agent; or the UID it had given up for the assigned one.
        [(crashed_and_back(UID2), FLAT), (crashed_and_back(UID, assigned=UID2, adopted=True), FLAT)],
    ),
    "state_kept_across_crash": (
        [(crashed_and_back(UID), FLAT)],
        [(crashed_and_back(UID, ok=False), FLAT)],
    ),
    "converges_when_quiet": (
        [(healthy(), FLAT), (rust_server(CONFIG), FLAT),
         # Lost in a restart, then resent in full.
         (heartbeats(run().at(T0 + 1).driver("status_change", change=1).send(FULL, driver_change=1).respond()
                     .at(T0 + 5).server("restart").at(T0 + 6).send(FULL, driver_change=1).respond(), 8, 44)
          .at(T0 + 45).stop(), FLAT),
         # Rejected, so rightly never resent: the server keeps what it had.
         (heartbeats(run().at(T0 + 1).driver("status_change", change=1).send(FULL, driver_change=1)
                     .respond(http_status=413), 8, 44).at(T0 + 45).stop(), FLAT),
         # The driver counts from its own start, seconds after the server's (a JVM): its last
         # change of the chaos lands 2 s into the quiet window, and is delivered (nightly 10-02,
         # java-core http: "the server holds change 59, the application set 58").
         (heartbeats(late_change().send(FULL, driver_change=2).respond(), 14, 44).at(T0 + 45).stop(), FLAT)],
        [(heartbeats(run().at(T0 + 1).driver("status_change", change=1).send(FULL, driver_change=1).respond()
                     .at(T0 + 5).server("restart"), 8, 44).at(T0 + 45).stop(), FLAT),
         # Throttled, not rejected: it must be resent.
         (heartbeats(run().at(T0 + 1).driver("status_change", change=1).send(FULL, driver_change=1)
                     .respond(http_status=503), 8, 44).at(T0 + 45).stop(), FLAT),
         # A server that says what it holds is believed over what it received.
         (rust_server(CONFIG, held_change=0), FLAT),
         # A change late into the quiet window still has to arrive.
         (heartbeats(late_change(), 14, 44).at(T0 + 45).stop(), FLAT)],
    ),
    "server_config_converges": (
        [(rust_server(CONFIG), FLAT)],
        [(rust_server(None), FLAT)],  # applied, but the server never took it in
    ),
    "alive_when_quiet": (
        [(healthy(), FLAT)],
        [(run().at(T0 + 1).send(FULL, driver_change=0).respond().at(T0 + 45).stop(), FLAT)],
    ),
    "no_storm": (
        [(healthy(), FLAT)],
        [(storm(), FLAT)],
    ),
    "calm_when_quiet": (
        # One message per heartbeat, however often the server asks.
        [(healthy(), FLAT), (asked_in_every_answer(2), FLAT)],
        # A message per round trip while the server asks in every answer (as the kernel once did).
        [(asked_in_every_answer(0.05), FLAT)],
    ),
    "resources_bounded": (
        [(healthy(), FLAT), (healthy(), samples(0.5))],
        [(healthy(), samples(10)),
         (healthy(), [Sample(40 + 5 * i, 50, 20 + 5 * i, 10) for i in range(12)])],
    ),
}


# A proxy stall from 20 s to 30 s after the server started.
STALLED = Plan(quiet=PLAN.quiet, check_s=PLAN.check_s, poll_s=PLAN.poll_s, storm_per_10s=PLAN.storm_per_10s,
               deaf=[(20.0, 30.0)])


def closed(at: float, gave_up: float) -> Log:
    """Over WebSocket: a Close frame at `at` s, never answered; the server gives up at `gave_up` s."""
    log = heartbeats(run().at(T0 + 1).send(FULL, driver_change=0).respond(), 2, 10)
    log.at(T0 + at).server("close_frame_sent", conn=1, code=1001)
    log.at(T0 + gave_up).server("close_not_answered", conn=1, after_s=5).server("ws_closed", conn=1)
    return log.at(T0 + 35).server("ws_open", conn=2).send(FULL, conn=2, driver_change=0).respond().stop()


def assigned_then_reset(receipt: str | None, reset: bool = True) -> Log:
    """Over HTTP: the server assigns UID2 at 3 s; the proxy resets the connection before the reply is
    through (unless not `reset`); the next request still uses UID. The driver logs the UIDs it gets:
    `receipt`, or none."""
    log = Log("http").at(T0 - 0.1).server("listening").driver("started")
    log.at(T0 + 1).send(FULL).respond(assign_uid=UID2).driver("message", new_instance_uid=UID2)
    log.at(T0 + 2).send(uid=UID2).respond().at(T0 + 3).send(uid=UID2).respond(assign_uid=UID)
    if reset:
        log.at(T0 + 3.4)._add("harness", "proxy_reset", conns=1)
    if receipt:
        log.driver("message", new_instance_uid=receipt)
    return log.at(T0 + 5).send(uid=UID2).respond().stop()

# An 800 ms latency on every read through the proxy from 15 s to 30 s after the server started.
SLOW = Plan(quiet=PLAN.quiet, check_s=PLAN.check_s, poll_s=PLAN.poll_s, storm_per_10s=PLAN.storm_per_10s,
            slow=[(15.0, 30.0)])


def pb(num: int, value: int | bytes) -> bytes:
    """One protobuf field: a varint, or bytes by length."""
    def varint(n: int) -> bytes:
        out = bytearray()
        while True:
            out.append(n & 0x7F | (0x80 if n > 0x7F else 0))
            n >>= 7
            if not n:
                return bytes(out)
    if isinstance(value, int):
        return varint(num << 3) + varint(value)
    return varint(num << 3 | 2) + varint(len(value)) + value


class Tap:
    """A core tap (opamp-tap's records) of a client over one WebSocket: what it sent and read,
    in its order. `stream`: the Rust core's, which sees the bytes on the stream (the upgrade, then
    frames, the client's masked); otherwise the Java and .NET cores', which see whole messages."""

    def __init__(self, stream: bool):
        self.stream, self.data = stream, bytearray()
        if stream:
            self._write(b"GET /v1/opamp HTTP/1.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
            self._read(b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n")

    def _record(self, kind: int, body: bytes) -> None:
        self.data += bytes([kind]) + len(body).to_bytes(4, "little") + bytes(8) + body

    def _write(self, data: bytes) -> None:
        effect = pb(14, pb(1, 1) + pb(2, data)) if self.stream else pb(3, pb(1, 1) + pb(2, data))
        self._record(4, pb(1, pb(1, effect)))  # Reply.output.effects

    def _read(self, data: bytes) -> None:
        self._record(3, pb(2, pb(29, pb(1, 1) + pb(2, data)) if self.stream else pb(12, pb(1, 1) + pb(2, data))))

    def send(self, seq: int) -> Tap:
        """The client sends AgentToServer number `seq`."""
        message = b"\x00" + pb(1, bytes(16)) + pb(2, seq)
        if self.stream:
            mask = b"\x11\x22\x33\x44"
            message = (bytes([0x82, 0x80 | len(message)]) + mask
                       + bytes(c ^ mask[i % 4] for i, c in enumerate(message)))
        self._write(message)
        return self

    def read(self, rejected: bool = False, assign: str | None = None) -> Tap:
        """The client reads the answer to its oldest unanswered message: a BAD_REQUEST if `rejected`;
        one that assigns the UID `assign` (hex) if that is given."""
        message = b"\x00" + pb(1, bytes(16)) + (pb(2, pb(1, 1) + pb(2, b"rejected by scenario")) if rejected else b"")
        if assign:
            message += pb(8, pb(1, bytes.fromhex(assign)))
        self._read(bytes([0x82, len(message)]) + message if self.stream else message)
        return self

    def file(self, where: Path) -> list[Path]:
        path = where / "core-0.tap"
        path.write_bytes(bytes(self.data))
        return [path]


def queued_crossing() -> tuple[Log, Tap]:
    """Over WebSocket, as the java-core-rust ws rerun of 2026-10-10 under 800 ms latency did: the
    server rejects a heartbeat (message 2), change 40 (3) and change 41 (4), all three sent before
    any rejection came back. Each owes a full report: the client sends change 41 again (5) after
    reading the first rejection, and again (6) after reading the second, both before it read the
    third, message 4's. The server reads them 0.8 s apart and rejects message 6 too."""
    log = run().at(T0 + 1).send(FULL, driver_change=0).respond()
    log.at(T0 + 19.0).send([]).respond(error="BAD_REQUEST")
    log.at(T0 + 19.3).send(["agent_description"], payload="change 40", driver_change=40).respond(error="BAD_REQUEST")
    log.at(T0 + 19.6).send(["agent_description"], payload="change 41", driver_change=41).respond(error="BAD_REQUEST")
    log.at(T0 + 20.4).send(["agent_description"], payload="change 41", driver_change=41).respond()
    log.at(T0 + 21.2).send(["agent_description"], payload="change 41", driver_change=41).respond(error="BAD_REQUEST")
    log.at(T0 + 22.0).send(["agent_description"], payload="change 42", driver_change=42).respond()
    log = heartbeats(log, 24, 44).at(T0 + 45).stop()
    tap = Tap(stream=True).send(0).read()
    tap.send(1).send(2).send(3).read(rejected=True)        # message 2's rejection
    tap.send(4).read(rejected=True)                        # message 3's
    tap.send(5).read(rejected=True).read().read(rejected=True)  # message 4's, 5's answer, 6's
    tap.send(6).read()
    for seq in range(7, 17):
        tap.send(seq).read()
    return log, tap


def rejected_twice(read_first: bool, stream: bool = True) -> tuple[Log, Tap]:
    """Over WebSocket, as the java-core-rust ws run of 2026-10-10 did under 800 ms latency: the
    server rejects message 2 (change 32), then message 3 (change 33), both sent before either
    rejection came back. The client reads the first and owes a full report, which carries change 33
    again: message 4. With not `read_first` the client sent it before it read the second rejection
    (its tap says); with `read_first` after."""
    log = run().at(T0 + 1).send(FULL, driver_change=0).respond()
    log.at(T0 + 19.9).send(["agent_description"], payload="change 32", driver_change=32).respond(error="BAD_REQUEST")
    log.at(T0 + 20.2).driver("status_change", change=33)
    log.at(T0 + 21.0).send(["agent_description"], payload="change 33", driver_change=33).respond(error="BAD_REQUEST")
    log.at(T0 + 21.9).send(["agent_description"], payload="change 33", driver_change=33).respond()
    log = heartbeats(log, 24, 44).at(T0 + 45).stop()
    tap = Tap(stream).send(0).read().send(1).send(2).read(rejected=True)
    if read_first:
        tap.read(rejected=True).send(3).read()
    else:
        tap.send(3).read(rejected=True).read()
    for seq in range(4, 14):
        tap.send(seq).read()
    return log, tap


def assigned_while_slow(read_first: bool, stream: bool = True) -> tuple[Log, Tap]:
    """Over WebSocket, as the rust ws torture run of 2026-10-11 did under 800 ms latency: the server
    assigns UID2 in its answer to message 2; it reads messages 3 and 4, both with the old UID, 0.8 s
    later, and the driver gets UID2 then; message 5 uses UID2. With not `read_first` the client sent
    3 and 4 before it read the assignment (its tap says); with `read_first` after."""
    log = run().at(T0 + 1).send(FULL, driver_change=0).respond()
    log.at(T0 + 19.0).send([], payload="status").respond(assign_uid=UID2)
    log.at(T0 + 19.8).send([], payload="status").respond().send(["agent_description"], driver_change=1).respond()
    log.driver("message", new_instance_uid=UID2)
    for t in range(21, 45, 2):
        log.at(T0 + t).send([], uid=UID2).respond()
    log = log.at(T0 + 45).stop()
    tap = Tap(stream).send(0).read().send(1)
    if read_first:
        tap.read(assign=UID2).send(2).send(3)
    else:
        tap.send(2).send(3).read(assign=UID2)
    tap.read().read()
    for seq in range(4, 16):
        tap.send(seq).read()
    return log, tap


class AsHeardTests(unittest.TestCase):
    """The generic checks see the trace without what the network kept from the client."""

    def status(self, log: Log, plan: Plan, check: str, tap: Tap | None = None) -> str:
        with tempfile.TemporaryDirectory() as tmp:
            taps = tap.file(Path(tmp)) if tap else []
            return evaluate(log.trace(), plan, FLAT, taps)[check]["status"]

    def test_a_close_frame_stalled_for_the_whole_wait_is_excused(self):
        # Nightly 09-29: a stall from 1220.6 s to 1227.9 s, a Close frame at 1221.3 s, the server
        # giving up at 1226.3 s: every client over WebSocket "never answered" it.
        self.assertEqual(self.status(closed(21, 26), STALLED, "close_frame_answered"), "skip")
        self.assertEqual(self.status(closed(21, 26), PLAN, "close_frame_answered"), "fail")

    def test_a_close_frame_the_client_could_hear_in_time_is_judged(self):
        # The stall ends before the server gives up: the client had time to answer.
        self.assertEqual(self.status(closed(27, 32), STALLED, "close_frame_answered"), "fail")
        self.assertEqual(self.status(closed(12, 17), STALLED, "close_frame_answered"), "fail")

    def test_a_close_frame_sent_while_the_proxy_was_slow_is_excused(self):
        # Nightly 10-03, go, node and rust ws: a 2000 ms latency from 1344.9 s to 1354.5 s, a Close
        # frame at 1349.9 s. Messages the client sent before it queued behind the latency, and the
        # answer reached the server after its 5 s wait.
        slow = Plan(quiet=PLAN.quiet, check_s=PLAN.check_s, poll_s=PLAN.poll_s, storm_per_10s=PLAN.storm_per_10s,
                    slow=[(18.0, 25.0)])
        self.assertEqual(self.status(closed(21, 26), slow, "close_frame_answered"), "skip")
        # A slow proxy that ended before the Close: the client could answer in time.
        early = Plan(quiet=PLAN.quiet, check_s=PLAN.check_s, poll_s=PLAN.poll_s, storm_per_10s=PLAN.storm_per_10s,
                     slow=[(10.0, 15.0)])
        self.assertEqual(self.status(closed(21, 26), early, "close_frame_answered"), "fail")

    def test_a_close_frame_through_overlapping_stalls_is_excused(self):
        # Nightly 10-14, seed 20261014, WebSocket clients on the Rust server: a proxy stall from
        # 484.86 s to 491.10 s, a pause from 487.79 s to 495.02 s, a Close frame at 487.39 s, the
        # server giving up at 492.39 s. Neither covers the wait alone; together they do.
        import torture  # noqa: PLC0415 - imports the runner, which the other tests don't need
        sched = torture.make_schedule(20261014, 60, 120, 45, "ws", can_pause=True, server="rust")
        stalls = [(p["start"], p["end"]) for p in sched.proxy if p["kind"] == "stall"]
        self.assertTrue(any(round(lo, 2) == 484.86 and round(hi, 2) == 491.10 for lo, hi in stalls))
        self.assertTrue(any(round(lo, 2) == 487.79 and round(hi, 2) == 495.02 for lo, hi in sched.pauses))
        # The same windows, 466 s earlier, around the fixture's Close at 21.39 s.
        overlapping = torture.Schedule(seed=0, duration_s=60, quiet=PLAN.quiet,
                                       proxy=[{"kind": "stall", "start": 18.86, "end": 25.10}],
                                       pauses=[(21.79, 29.02)])
        plan = torture.plan_for(overlapping, PLAN.poll_s)
        self.assertEqual(self.status(closed(21.39, 26.39), plan, "close_frame_answered"), "skip")
        # Two windows with a gap between them: the client could hear in the gap.
        apart = torture.Schedule(seed=0, duration_s=60, quiet=PLAN.quiet,
                                 proxy=[{"kind": "stall", "start": 18.86, "end": 23.0}], pauses=[(24.0, 29.02)])
        plan = torture.plan_for(apart, PLAN.poll_s)
        self.assertEqual(self.status(closed(21.39, 26.39), plan, "close_frame_answered"), "fail")

    def test_only_faults_that_delay_by_seconds_are_slow(self):
        import torture  # noqa: PLC0415 - imports the runner, which the other tests don't need
        self.assertEqual([torture.slows(p) for p in (
            {"kind": "latency", "ms": 2000}, {"kind": "latency", "ms": 800}, {"kind": "latency", "ms": 200},
            {"kind": "slice", "bytes": 1, "delay_ms": 30}, {"kind": "slice", "bytes": 64, "delay_ms": 0},
            {"kind": "stall"})], [True, True, False, True, False, False])

    def test_a_uid_lost_with_its_reply_is_excused(self):
        # Nightly 10-02, java-core and dotnet-core http: the reply with the UID was cut by a reset,
        # and the driver, which logs the UIDs it gets, never got it.
        self.assertEqual(self.status(assigned_then_reset(None), PLAN, "adopts_assigned_uid"), "pass")

    def test_a_uid_that_arrived_is_judged(self):
        # The driver got it before the reset: not adopting it is the client's fault.
        self.assertEqual(self.status(assigned_then_reset(UID), PLAN, "adopts_assigned_uid"), "fail")
        # No fault between the reply and the next request.
        self.assertEqual(self.status(assigned_then_reset(None, reset=False), PLAN, "adopts_assigned_uid"), "fail")


    def test_messages_sent_before_the_uid_was_read_are_excused(self):
        # Torture rust ws (and node-rust ws) on the go server, nightly 10-11, seed 20261011: "message
        # 1188 still uses 01a129ea..., assigned 01a129ea...". The server read 1188 and 1189 0.8 s
        # after it wrote the assignment, under an 800 ms latency; the client's tap: it sent both
        # before it read the assignment, and adopted it from the next message on.
        for stream in (True, False):
            log, tap = assigned_while_slow(read_first=False, stream=stream)
            self.assertEqual(self.status(log, SLOW, "adopts_assigned_uid", tap), "pass", f"stream={stream}")

    def test_messages_sent_after_the_uid_was_read_are_judged(self):
        for stream in (True, False):
            log, tap = assigned_while_slow(read_first=True, stream=stream)
            self.assertEqual(self.status(log, SLOW, "adopts_assigned_uid", tap), "fail", f"stream={stream}")
        # A tap that has none of it, or no tap at all (an upstream client): the server's log alone.
        log, _ = assigned_while_slow(read_first=False)
        self.assertEqual(self.status(log, SLOW, "adopts_assigned_uid", Tap(stream=True)), "fail")
        self.assertEqual(self.status(log, SLOW, "adopts_assigned_uid"), "fail")

    def test_a_report_queued_behind_a_rejection_it_crossed_is_excused(self):
        # Rerun of nightly 10-10's java-core-rust ws under 800 ms latency: "message 71 was rejected
        # and resent unchanged 2 time(s)". The client's tap: both resends went before it read 71's
        # rejection.
        log, tap = queued_crossing()
        self.assertEqual(self.status(log, SLOW, "rejected_not_resent", tap), "pass")

    def test_a_report_sent_before_its_rejection_was_read_is_excused(self):
        # Nightly 10-10, java-core-rust ws: "message 737 was rejected and resent unchanged", inside
        # an 800 ms latency. The full report owed for the first rejection crossed the second one.
        for stream in (True, False):
            log, tap = rejected_twice(read_first=False, stream=stream)
            self.assertEqual(self.status(log, SLOW, "rejected_not_resent", tap), "pass", f"stream={stream}")

    def test_a_rejection_resent_after_it_was_read_is_judged(self):
        # The client's tap: it had read the rejection before it sent the same report again, however
        # slow the proxy was.
        for stream in (True, False):
            log, tap = rejected_twice(read_first=True, stream=stream)
            self.assertEqual(self.status(log, SLOW, "rejected_not_resent", tap), "fail", f"stream={stream}")
            self.assertEqual(self.status(log, PLAN, "rejected_not_resent", tap), "fail", f"stream={stream}")
        log, tap = queued_crossing()
        # A tap that has none of it.
        self.assertEqual(self.status(log, SLOW, "rejected_not_resent", Tap(stream=True)), "fail")
        # A client with no tap (an upstream one) is judged by the server's log alone, as before.
        log, _ = rejected_twice(read_first=False)
        self.assertEqual(self.status(log, SLOW, "rejected_not_resent"), "fail")
        log, _ = queued_crossing()
        self.assertEqual(self.status(log, SLOW, "rejected_not_resent"), "fail")

    def test_a_rejection_is_news_to_the_run_after_a_crash(self):
        # The run that sent a rejected status was killed; the next one starts from the application's
        # status, and has never heard of the rejection.
        def rejected(crash: bool) -> Log:
            log = run().at(T0 + 1).send(FULL, payload="change 1", driver_change=1).respond(error="BAD_REQUEST")
            if crash:
                log.at(T0 + 3).crashed(0).at(T0 + 4).driver("started")
            return heartbeats(log.at(T0 + 5).send(FULL, seq=0, payload="change 1", driver_change=1).respond(),
                              8, 44).at(T0 + 45).stop()
        self.assertEqual(self.status(rejected(crash=True), PLAN, "rejected_not_resent"), "pass")
        self.assertEqual(self.status(rejected(crash=False), PLAN, "rejected_not_resent"), "fail")

    def test_what_a_crash_cut_off_is_excused(self):
        # A Close frame the client was killed before answering.
        log = heartbeats(run().at(T0 + 1).send(FULL, driver_change=0).respond(), 2, 10)
        log.at(T0 + 21).server("close_frame_sent", conn=1, code=1001).at(T0 + 22).crashed(0)
        log.at(T0 + 26).server("close_not_answered", conn=1, after_s=5).server("ws_closed", conn=1)
        log.at(T0 + 27).driver("started").server("ws_open", conn=2).send(FULL, conn=2, seq=0, driver_change=0)
        self.assertEqual(self.status(log.respond().stop(), PLAN, "close_frame_answered"), "skip")
        # A UID assigned in the last answer before the crash, which the next run may not have saved.
        self.assertEqual(self.status(crashed_and_back(UID, assigned=UID2), PLAN, "adopts_assigned_uid"), "skip")
        # One the run adopted before it was killed is still judged.
        self.assertEqual(self.status(crashed_and_back(UID2, assigned=UID2, adopted=True), PLAN, "adopts_assigned_uid"),
                         "pass")


class KnownFailureTests(unittest.TestCase):
    """torture.py's known failures: what excuses a MUST failure, and what never does unasked."""

    BASELINE = """
[go."java/http"]
known = ["resources_bounded", "server_records_applied_config"]

[rust."go/ws"]
known = ["server_config_converges", "converges_when_quiet"]

[rust."rust/http"]
known = ["server_config_converges"]
reason = "a server bug, fixed on another branch"

[go."rust/ws"]
known = ["converges_when_quiet"]
"""

    def known(self, client: str, transport: str, server: str, upstream: bool) -> dict[str, str]:
        import torture  # noqa: PLC0415 - imports the runner, which the other tests don't need
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "baseline.toml"
            path.write_text(self.BASELINE)
            return torture.known_failures(client, transport, server, upstream, path)

    def test_an_upstream_client_follows_both_baselines(self):
        k = self.known("java", "http", "go", True)
        self.assertIn("resources_bounded", k)  # torture's baseline
        self.assertIn("reports_status_capability", k)  # conformance/baseline.toml
        self.assertNotIn("server_records_applied_config", k)  # a check of the server, no reason

    def test_a_check_of_the_server_needs_a_reason(self):
        self.assertEqual(set(self.known("go", "ws", "rust", True)) & {"server_config_converges"}, set())
        k = self.known("rust", "http", "rust", False)
        self.assertEqual(set(k), {"server_config_converges"})
        self.assertIn("a server bug, fixed on another branch", k["server_config_converges"])

    def test_our_client_needs_a_reason(self):
        self.assertEqual(self.known("rust", "ws", "go", False), {})


class InvariantTests(unittest.TestCase):
    def test_desired_is_read_in_time_order(self):
        # Torture schedules them in the order it draws them, not in time order.
        plan = Plan(quiet=[], check_s=0, poll_s=0, storm_per_10s=0, desired=[(0.0, "a"), (39.7, "c"), (9.5, "b")])
        self.assertEqual([desired_at(plan, t) for t in (5, 20, 45)], ["a", "b", "c"])

    def test_every_invariant_has_cases(self):
        self.assertEqual(set(CASES), set(INVARIANTS))

    def test_cases(self):
        for name, (good, bad) in CASES.items():
            fn = INVARIANTS[name]
            for log, s in good:
                with self.subTest(invariant=name, want="pass"):
                    r = fn(log.trace(), PLAN, s)
                    self.assertEqual(r.status, "pass", r.detail)
            for log, s in bad:
                with self.subTest(invariant=name, want="fail"):
                    r = fn(log.trace(), PLAN, s)
                    self.assertEqual(r.status, "fail", r.detail)


if __name__ == "__main__":
    unittest.main()
