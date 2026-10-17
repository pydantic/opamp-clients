# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""What must hold across a whole torture run, however hostile the network and the server.

The conformance checks (runner/checks.py) look at one fault at a time. These look at hours of
them: the status the server holds never goes back, it catches up whenever things go quiet, the
client keeps talking, it doesn't hammer the server, and it doesn't grow without bound.
"""
from __future__ import annotations

import sys
from bisect import bisect_right
from dataclasses import dataclass, field
from pathlib import Path
from statistics import median
from typing import Callable

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "runner"))
import tap  # noqa: E402
from checks import CHECKS, Result, Trace  # noqa: E402
from dotnet_memory import dotnet_resources_bounded  # noqa: E402
from jvm_memory import jvm_resources_bounded  # noqa: E402

# Checks of a server that decides for itself (the Rust one), over any trace.
SERVER_CHECKS = [
    "server_offers_config_once_per_connection",
    "server_records_applied_config",
    "server_assigns_v7_uids",
]

# Conformance checks that hold for any trace, not just their own scenario.
GENERIC = [
    "instance_uid_valid",
    "reports_status_capability",
    "disconnect_is_last",
    "reconnect_says_goodbye",
    "rejected_not_resent",
    "adopts_assigned_uid",
    "http_content_type",
    "close_frame_answered",
    "driver_survives",
]

# Host and container clocks differ a little (see checks.py).
SKEW_S = 0.25


@dataclass
class Plan:
    """The parts of a torture schedule the invariants need, in seconds since the server started."""

    quiet: list[tuple[float, float]]  # windows with no faults and no status changes
    check_s: float  # the last check_s seconds of each quiet window must look healthy
    poll_s: float  # the driver's heartbeat / polling interval
    storm_per_10s: int  # most connections plus messages in any 10 s
    # The desired config's hash over time, from 0 (None: no config). Only for servers that choose
    # what to offer (the Rust one); empty otherwise.
    desired: list[tuple[float, str | None]] = field(default_factory=list)
    # Windows in which the client can neither hear the server nor answer it: the proxy stalls
    # every byte, or the driver is frozen.
    deaf: list[tuple[float, float]] = field(default_factory=list)
    # Windows in which the proxy holds each piece of data long enough that, with a heartbeat or a
    # status change every few seconds, a queue builds up behind it: a message sent then may reach
    # the other side seconds after it would have, past a wait the server keeps.
    slow: list[tuple[float, float]] = field(default_factory=list)


@dataclass
class Sample:
    t: float  # seconds since the server started
    rss_mb: float
    fds: int | None
    threads: int | None


INVARIANTS: dict[str, Callable] = {}


def invariant(fn: Callable) -> Callable:
    INVARIANTS[fn.__name__] = fn
    return fn


def server_t0(t: Trace) -> float:
    listening = t.server("listening")
    return listening[0]["wall"] if listening else min((e["wall"] for e in t.events), default=0.0)


def accepted(t: Trace) -> list[dict]:
    """Messages the server took in: answered 200 (or over WebSocket, answered), with no error."""
    answers = {e["msg"]: e for e in t.server("s2a")}
    out = []
    for m in t.a2s():
        a = answers.get(m["msg"])
        if m.get("refused_status") or a is None or a.get("error") or a.get("http_status", 200) != 200:
            continue
        out.append(m)
    return out


def reports_held(t: Trace) -> bool:
    """The server says in each answer what it holds (the Rust one), rather than holding whatever
    it last received (the Go one)."""
    return any("held_config" in e for e in t.server("s2a"))


def held_at(t: Trace, wall: float) -> int | None:
    """The status (driver.change) the server holds at `wall`. A restart forgets it."""
    events = [e for e in t.server("restart") if e["wall"] <= wall]
    last_restart = events[-1]["wall"] if events else float("-inf")
    if reports_held(t):
        answers = [e for e in t.server("s2a") if last_restart < e["wall"] <= wall and "held_config" in e]
        return answers[-1].get("held_change") if answers else None
    held = None
    for m in accepted(t):
        if last_restart < m["wall"] <= wall and "driver_change" in m:
            held = m["driver_change"]
    return held


def written_before(t: Trace, wall: float) -> int:
    """The latest status the application set before `wall` (0: the one it started with)."""
    # Some drivers write the number as a string.
    changes = [int(e["change"]) for e in t.driver("status_change") if e["wall"] <= wall]
    return max(changes, default=0)


def crash_walls(t: Trace) -> list[float]:
    """When the harness killed the driver, each time before it started it again on its state file.
    Nothing the killed process sent reaches the server after (torture.py resets its connections)."""
    return sorted(e["wall"] for e in t.runner("crashed"))


def run_of(walls: list[float], wall: float) -> int:
    """Which run of the driver (0, then one more per crash) was going at `wall`."""
    return bisect_right(walls, wall)


def rejected_changes(t: Trace) -> set[int]:
    """Status values the server rejected (413 / BAD_REQUEST)."""
    rejected = {e["msg"] for e in t.server("s2a") if e.get("http_status") == 413 or e.get("error") == "BAD_REQUEST"}
    return {m["driver_change"] for m in t.a2s() if m["msg"] in rejected and "driver_change" in m}


@invariant
def status_never_goes_back(t: Trace, plan: Plan, samples: list[Sample]) -> Result:
    """In sequence order, the status a client reports never goes back to an older one, and is
    never one the application hadn't set yet.
    covers: OPAMP-STATUS-02"""
    # (Each run after a crash numbers its messages afresh.)
    walls = crash_walls(t)
    msgs = sorted((m for m in t.a2s() if "driver_change" in m),
                  key=lambda m: (run_of(walls, m["wall"]), m["seq"], m["wall"]))
    best = -1
    for m in msgs:
        v = m["driver_change"]
        if v < best:
            return Result("fail", f"message {m['msg']} (seq {m['seq']}) reports change {v} after {best}")
        best = v
        if v > written_before(t, m["wall"] + SKEW_S):
            return Result("fail", f"message {m['msg']} reports change {v} before the application made it")
    return Result("pass", f"{len(msgs)} status reports")


@invariant
def converges_when_quiet(t: Trace, plan: Plan, samples: list[Sample]) -> Result:
    """By the end of every quiet window the server holds the application's latest status, even if
    the server restarted and lost it. A status the server rejected (413 / BAD_REQUEST) is exempt:
    the client must not send it again until it changes (OPAMP-SIZE-09, OPAMP-ERR-01), or the server
    asks for the full state.

    The driver counts its changes from its own start, a few seconds after the server's (a JVM, a
    container), so the last change of a chaotic stretch can land early in the quiet one. What must
    be held is the latest change made check_s before the window's end, or one made since.
    covers: OPAMP-STATUS-02, OPAMP-STATUS-13, OPAMP-CONN-09, OPAMP-CONN-10"""
    t0 = server_t0(t)
    checked = 0
    for _, end in plan.quiet:
        if not t.driver("stopping") or t.stop_wall() < t0 + end:
            continue  # the run ended before this window did
        want = written_before(t, t0 + end - plan.check_s)
        newest = written_before(t, t0 + end)
        got = held_at(t, t0 + end)
        if (got is None or not want <= got <= newest) and want not in rejected_changes(t):
            return Result("fail", f"at {end:.0f}s the server holds change {got}, the application set {want}")
        checked += 1
    if not checked:
        return Result("skip", "no complete quiet window")
    return Result("pass", f"{checked} quiet windows")


@invariant
def uid_kept_across_crash(t: Trace, plan: Plan, samples: list[Sample]) -> Result:
    """A client killed and started again on its state file goes on as the same agent: its first
    message carries the instance UID its last one before the crash did, or one the server assigned
    it since. Never a new one, and never one it had given up for an assigned one.
    covers: OPAMP-UID-06"""
    walls = crash_walls(t)
    if not walls:
        return Result("skip", "nothing was crashed")
    t0, msgs = server_t0(t), t.a2s()
    checked = 0
    for k, w in enumerate(walls):
        lo = walls[k - 1] if k else float("-inf")
        before = [m for m in msgs if lo < m["wall"] <= w]
        hi = walls[k + 1] if k + 1 < len(walls) else float("inf")
        after = [m for m in msgs if w < m["wall"] <= hi]
        if not before or not after:
            continue  # killed before it said anything, or the run ended
        last = before[-1]
        since = min(m["wall"] for m in before if m["uid"] == last["uid"])
        kept = {last["uid"]} | {a["assign_uid"] for a in t.server("s2a")
                                if a.get("assign_uid") and since <= a["wall"] <= w}
        if after[0]["uid"] not in kept:
            return Result("fail", f"after the crash at {w - t0:.0f}s the client came back as {after[0]['uid']}, "
                                  f"having been {last['uid']}")
        checked += 1
    if not checked:
        return Result("skip", f"{len(walls)} crash(es), none between two messages")
    return Result("pass", f"{checked} crash(es)")


@invariant
def state_kept_across_crash(t: Trace, plan: Plan, samples: list[Sample]) -> Result:
    """A client killed at any moment leaves its state file whole, holding the last save its core
    was told was stored or a later one, and starts again from exactly that file (the conformance
    suite's crash_state_consistent, on a process crash: nothing written is lost).
    covers: RULE-SHELL-09"""
    if not t.runner("crashed", "crash_missed"):
        return Result("skip", "nothing was crashed")
    return CHECKS["crash_state_consistent"].fn(t)


def desired_at(plan: Plan, at: float) -> str | None:
    wanted = None
    for when, h in sorted(plan.desired, key=lambda d: d[0]):
        if when <= at:
            wanted = h
    return wanted


@invariant
def server_config_converges(t: Trace, plan: Plan, samples: list[Sample]) -> Result:
    """By the end of every quiet window, the server knows the agent runs the config it wanted at
    the start of it, whatever restarts, lost messages and changes of mind came before.
    covers: RULE-CONFIG-01, RULE-CONFIG-03"""
    if not plan.desired or not reports_held(t):
        return Result("skip", "the server doesn't choose configs")
    if not any(m.get("remote_config_status", {}).get("status") == "RemoteConfigStatuses_APPLIED" for m in t.a2s()):
        return Result("skip", "the client never reports a config applied")
    t0 = server_t0(t)
    checked = 0
    for start, end in plan.quiet:
        if not t.driver("stopping") or t.stop_wall() < t0 + end:
            continue
        want = desired_at(plan, start)
        if want is None:
            continue
        answers = [e for e in t.server("s2a") if e["wall"] <= t0 + end and "held_config" in e]
        got = answers[-1]["held_config"] if answers else None
        if got != want:
            return Result("fail", f"at {end:.0f}s the server thinks the agent runs {str(got)[:12]}, "
                                  f"wanted {want[:12]}")
        checked += 1
    if not checked:
        return Result("skip", "no complete quiet window with a desired config")
    return Result("pass", f"{checked} quiet windows")


@invariant
def alive_when_quiet(t: Trace, plan: Plan, samples: list[Sample]) -> Result:
    """Once things are quiet, the client talks to the server at least every two intervals.
    covers: OPAMP-HB-02, OPAMP-CONN-09"""
    t0 = server_t0(t)
    limit = 2 * plan.poll_s + 2
    checked = 0
    for _, end in plan.quiet:
        lo, hi = t0 + end - plan.check_s, t0 + end
        if not t.driver("stopping") or t.stop_wall() < hi:
            continue
        walls = [lo] + [m["wall"] for m in t.a2s() if lo <= m["wall"] <= hi] + [hi]
        gap = max(b - a for a, b in zip(walls, walls[1:]))
        if gap > limit:
            return Result("fail", f"{gap:.1f}s without a message around {end:.0f}s (limit {limit:.0f}s)")
        checked += 1
    if not checked:
        return Result("skip", "no complete quiet window")
    return Result("pass", f"{checked} quiet windows")


@invariant
def calm_when_quiet(t: Trace, plan: Plan, samples: list[Sample]) -> Result:
    """Once things are quiet, the client sends about one message per heartbeat interval, however
    the server answers: in the last check_s seconds of every quiet window, at most two per interval
    and three more. A server that asks for the full state in every answer (because a field it needs
    is too large to send, and never comes) gets one message per heartbeat, not one per round trip;
    no_storm's bound is for the chaos, and far above this.
    covers: RULE-CLIENT-21"""
    t0 = server_t0(t)
    most = 2 * plan.check_s / plan.poll_s + 3
    checked, worst = 0, 0
    for _, end in plan.quiet:
        lo, hi = t0 + end - plan.check_s, t0 + end
        if not t.driver("stopping") or t.stop_wall() < hi:
            continue
        sent = sum(1 for m in t.a2s() if lo <= m["wall"] <= hi)
        if sent > most:
            asks = sum(1 for e in t.server("s2a") if lo <= e["wall"] <= hi and e.get("report_full_state"))
            return Result("fail", f"{sent} messages in the last {plan.check_s:g}s of the quiet window ending at "
                                  f"{end:.0f}s, {asks} of them asked for the full state (at most {most:.0f}, with a "
                                  f"heartbeat every {plan.poll_s:g}s)")
        worst = max(worst, sent)
        checked += 1
    if not checked:
        return Result("skip", "no complete quiet window")
    return Result("pass", f"{checked} quiet windows, at most {worst} messages in {plan.check_s:g}s")


@invariant
def no_storm(t: Trace, plan: Plan, samples: list[Sample]) -> Result:
    """However the server and network misbehave, the client doesn't hammer the server:
    connections plus messages stay under a bound in any 10 seconds.
    covers: OPAMP-CONN-03, OPAMP-ERR-04, OPAMP-ERR-05"""
    walls = sorted(e["wall"] for e in t.events if e["source"] == "server"
                   and e["event"] in ("a2s", "ws_open", "request_rejected"))
    worst, at, lo = 0, 0.0, 0
    for hi, w in enumerate(walls):
        while walls[lo] < w - 10:
            lo += 1
        if hi - lo + 1 > worst:
            worst, at = hi - lo + 1, w
    if worst > plan.storm_per_10s:
        return Result("fail", f"{worst} requests in the 10 s before {at - server_t0(t):.0f}s "
                              f"(limit {plan.storm_per_10s})")
    return Result("pass", f"at most {worst} in 10 s")


def _growth(values: list[float]) -> tuple[float, float]:
    third = max(1, len(values) // 3)
    return median(values[:third]), median(values[-third:])


@invariant
def resources_bounded(t: Trace, plan: Plan, samples: list[Sample]) -> Result:
    """Memory, open files and threads level off: the last third of the run is not far above the
    first (after warm-up). A JVM's memory is its heap after a GC and its non-heap memory
    (jvm_memory.py); a .NET client's, its GC heap live after a forced GC and its non-GC memory
    (dotnet_memory.py)."""
    warm = plan.quiet[0][1] if plan.quiet else 0.0
    for probed in (jvm_resources_bounded, dotnet_resources_bounded):
        if (r := probed(samples, warm)) is not None:
            return r
    s = [x for x in samples if x.t >= warm]
    if len(s) < 6:
        return Result("skip", f"{len(s)} samples after warm-up")
    before, after = _growth([x.rss_mb for x in s])
    if after > max(before * 1.25, before + 20):
        return Result("fail", f"memory grew from {before:.0f} MB to {after:.0f} MB")
    for name in ("fds", "threads"):
        vals = [getattr(x, name) for x in s if getattr(x, name) is not None]
        if len(vals) >= 6:
            b, a = _growth(vals)
            if a > b + 20:
                return Result("fail", f"{name} grew from {b:.0f} to {a:.0f}")
    return Result("pass", f"memory {before:.0f} -> {after:.0f} MB")


def deaf_through(plan: Plan, t0: float, a: float, b: float) -> bool:
    """The client could neither hear nor answer from `a` to `b` (walls)."""
    return any(t0 + lo <= a + SKEW_S and b - SKEW_S <= t0 + hi for lo, hi in plan.deaf)


def deaf_during(plan: Plan, t0: float, a: float, b: float) -> bool:
    """The client was deaf at some time from `a` to `b` (walls)."""
    return any(t0 + lo <= b and a <= t0 + hi for lo, hi in plan.deaf)


def slow_during(plan: Plan, t0: float, a: float, b: float) -> bool:
    """The proxy was slow at some time from `a` to `b` (walls)."""
    return any(t0 + lo <= b and a <= t0 + hi for lo, hi in plan.slow)


def as_heard(t: Trace, plan: Plan, taps: list[Path] = ()) -> Trace:
    """The trace without what the network kept from the client, for the generic checks, which
    know nothing of the faults torture adds:

    - a Close frame sent while the client was deaf (a stall, a pause) for as long as the server
      waited for the answer, or while the proxy was slow: it never reached the client in time,
      or the answer is queued behind messages sent before it;
    - a UID assignment whose reply the network lost or held (a reset, a stall, a slow proxy)
      before the client's next message, when the driver says it never got it (if it says what it
      gets at all);
    - over WebSocket, for a client of ours (one with `taps`), a message the server rejected that
      the client sent again unchanged before it had read the rejection, as its tap says: it crossed
      the rejection (a full report owed for an earlier one, queued behind a slow proxy, say), so it
      is marked `crossed` with the rejected message, which rejected_not_resent then doesn't count.
      The server's log can't tell this (the client sends without waiting for answers); a client
      with no tap is judged by the log alone;
    - likewise, a message the client sent before it had read the answer that assigned it a new
      instance UID, as its tap says: marked `crossed` with the message that answer was to, which
      adopts_assigned_uid then doesn't count (it went out with the old UID, as it had to);
    - what a crash took from the client: a Close frame it was killed before answering; a UID
      assignment with a crash before the next message (whether the UID was saved first is
      uid_kept_across_crash's to judge); and a rejection, to the runs after it, which never heard
      of it (each message of theirs is marked `crossed` with it).
    """
    t0 = server_t0(t)
    walls = crash_walls(t)

    def crash_between(a: float, b: float) -> bool:
        return any(a <= w <= b for w in walls)

    drop: set[int] = set()
    for c in t.server("close_frame_sent"):
        ends = [e["wall"] for e in t.server("close_not_answered", "ws_closed")
                if e.get("conn") == c["conn"] and e["wall"] >= c["wall"]]
        end = min(ends, default=c["wall"] + 5)
        if (deaf_through(plan, t0, c["wall"], end) or slow_during(plan, t0, c["wall"], end)
                or crash_between(c["wall"], end)):
            drop.add(id(c))
    resets = [e["wall"] for e in t.events if e["source"] == "harness" and e["event"] == "proxy_reset"]
    receipts = [e.get("new_instance_uid") for e in t.driver("message") if "new_instance_uid" in e]
    unassign: set[int] = set()
    for a in t.server("s2a"):
        if not a.get("assign_uid") or a["assign_uid"] in receipts:
            continue
        nxt = min((m["wall"] for m in t.a2s() if m["msg"] > a["msg"]), default=float("inf"))
        if (any(a["wall"] <= r <= nxt for r in resets) or deaf_during(plan, t0, a["wall"], nxt)
                or slow_during(plan, t0, a["wall"], nxt)):
            unassign.add(id(a))
    for a in t.server("s2a"):
        if a.get("assign_uid") and id(a) not in unassign:
            nxt = min((m["wall"] for m in t.a2s() if m["msg"] > a["msg"]), default=float("inf"))
            if crash_between(a["wall"], nxt):
                unassign.add(id(a))
    crossed: dict[int, list[int]] = {}
    for r in t.server("s2a"):
        if r.get("http_status") == 413 or r.get("error") == "BAD_REQUEST":
            for m in t.a2s():
                if run_of(walls, m["wall"]) > run_of(walls, r["wall"]):
                    crossed.setdefault(id(m), []).append(r["msg"])
    if t.transport == "ws" and taps:
        sent = tap.match(t.a2s(), tap.exchanges(taps))
        for found in (crossed_rejections(t, sent), crossed_assignments(t, sent)):
            for k, v in found.items():
                crossed.setdefault(k, []).extend(v)
    if not drop and not unassign and not crossed:
        return t

    def heard(e: dict) -> dict:
        if id(e) in unassign:
            e = {k: v for k, v in e.items() if k != "assign_uid"}
        if id(e) in crossed:
            e = {**e, "crossed": crossed[id(e)]}
        return e

    events = [heard(e) for e in t.events if id(e) not in drop]
    return Trace(events, t.scenario, t.transport, t.driver_exit)


def crossed_rejections(t: Trace, sent: dict[int, tap.Exchange]) -> dict[int, list[int]]:
    """The messages the client sent again unchanged after the server rejected them, before it had
    read the rejection, as its core taps say (as_heard; `sent`: the taps' exchanges by the server's
    message number): by id, the rejected messages each crossed. A message the taps don't place is
    left to be judged."""
    rejected = [e["msg"] for e in t.server("s2a") if e.get("error") == "BAD_REQUEST"]
    if not rejected:
        return {}
    a2s = t.a2s()
    by_msg = {e["msg"]: e for e in a2s}
    crossed: dict[int, list[int]] = {}
    for m in rejected:
        orig, x = by_msg.get(m), sent.get(m)
        # The answer the client read must be this rejection, or the pairing went wrong.
        if orig is None or x is None or (x.read is not None and x.error != tap.BAD_REQUEST):
            continue
        for e in a2s:
            if e["msg"] <= m or e["payload_hash"] != orig["payload_hash"]:
                continue
            y = sent.get(e["msg"])
            if y is not None and y.tap == x.tap and (x.read is None or y.sent < x.read):
                crossed.setdefault(id(e), []).append(m)
    return crossed


def crossed_assignments(t: Trace, sent: dict[int, tap.Exchange]) -> dict[int, list[int]]:
    """The messages the client sent after the server assigned it a UID but before it had read that
    answer, as its core taps say (as_heard): by id, the messages whose assigning answers each
    crossed. Only an answer the tap shows the client reading, with the same UID, is placed: one it
    never read is as_heard's to excuse (or not) by the network and the driver."""
    crossed: dict[int, list[int]] = {}
    a2s = t.a2s()
    for a in t.server("s2a"):
        x = sent.get(a["msg"])
        if not a.get("assign_uid") or x is None or x.read is None or x.assigned != a["assign_uid"]:
            continue
        for e in a2s:
            y = sent.get(e["msg"])
            if e["msg"] > a["msg"] and y is not None and y.tap == x.tap and y.sent < x.read:
                crossed.setdefault(id(e), []).append(a["msg"])
    return crossed


def evaluate(t: Trace, plan: Plan, samples: list[Sample], taps: list[Path] = ()) -> dict[str, dict]:
    """Every torture invariant, then the generic conformance checks, on one trace. `taps`: the
    client's core taps, if it is one of ours."""
    results = {}
    for name, fn in INVARIANTS.items():
        results[name] = _run(lambda: fn(t, plan, samples), "MUST")
    heard = as_heard(t, plan, taps)
    for name in GENERIC + (SERVER_CHECKS if reports_held(t) else []):
        c = CHECKS[name]
        if t.transport in c.transports:
            results[name] = _run(lambda: c.fn(heard), c.level)
    return results


def _run(fn: Callable[[], Result], level: str) -> dict:
    try:
        r = fn()
    except Exception as exc:  # a checker bug must not look like a client pass
        r = Result("error", f"{type(exc).__name__}: {exc}")
    return {"status": r.status, "detail": r.detail, "level": level}
