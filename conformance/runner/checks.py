# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Checks over a recorded run. Each reads the merged event log and returns a Result.

A check names the requirements it covers and its level. A failed MUST check fails the
run; a failed SHOULD check is reported as a warning.
"""
from __future__ import annotations

import re
import statistics
import uuid
from dataclasses import dataclass, field
from typing import Callable

STATUS_FIELDS = {"agent_description", "health", "effective_config", "remote_config_status"}
# Tolerance when comparing driver and server timestamps: a driver in a container has its own clock.
CLOCK_SKEW_S = 0.25


@dataclass
class Trace:
    events: list[dict]          # server and driver events, merged by wall time; each has "source"
    scenario: dict
    transport: str
    driver_exit: int | None

    def server(self, *names: str) -> list[dict]:
        return [e for e in self.events if e["source"] == "server" and (not names or e["event"] in names)]

    def driver(self, *names: str) -> list[dict]:
        return [e for e in self.events if e["source"] == "driver" and (not names or e["event"] in names)]

    def a2s(self) -> list[dict]:
        return self.server("a2s")

    def runner(self, *names: str) -> list[dict]:
        """What the runner did to the driver: crashed it (crash scenarios), and what it left."""
        return [e for e in self.events if e["source"] == "runner" and (not names or e["event"] in names)]

    @property
    def params(self) -> dict:
        return self.scenario.get("params", {})

    def stop_wall(self) -> float | None:
        stopping = self.driver("stopping")
        return stopping[0]["wall"] if stopping else None


@dataclass
class Result:
    status: str                 # pass, fail, skip
    detail: str = ""


@dataclass
class Check:
    name: str
    level: str                  # MUST or SHOULD
    covers: list[str]
    fn: Callable[[Trace], Result]
    transports: tuple[str, ...] = ("http", "ws")
    doc: str = ""
    subject: str = "client"     # what the check judges: the client, or the server (the Rust one)


CHECKS: dict[str, Check] = {}


def check(name: str, level: str, covers: list[str], transports=("http", "ws"), subject: str = "client"):
    def register(fn: Callable[[Trace], Result]) -> Callable[[Trace], Result]:
        CHECKS[name] = Check(name, level, covers, fn, tuple(transports), (fn.__doc__ or "").strip(), subject)
        return fn
    return register


# The core tap's check: not over the event log, but opamp-tap over the taps the run's drivers
# wrote (run.py runs it for every client with a core, in every scenario).
TAP_CHECK = "core_tap"


@check(TAP_CHECK, "MUST", ["RULE-CLIENT-15", "RULE-CLIENT-16"])
def core_tap(t: Trace) -> Result:
    """Every call into the client's core keeps the effects' contract (spec/shell-contract.md): results only for
    effects the client carries out, at most one each, nothing about an effect after its cancel or
    after the core stopped, stop driven to its end; and every reply is the core's own. Checked by
    opamp-tap over the run's core taps (run.py), not over the event log."""
    return Result("skip", "run.py checks the core taps")


def carries_status(e: dict) -> bool:
    return bool(STATUS_FIELDS & set(e["fields"]))


def first_per_connection(t: Trace) -> list[dict]:
    """The first message on each connection, in the order the connections were used."""
    firsts: dict[int, dict] = {}
    for e in t.a2s():
        firsts.setdefault(e["conn"], e)
    return sorted(firsts.values(), key=lambda e: e["msg"])


# ------------------------------------------------------------------------------------------
# Messages

def unaccepted_msgs(t: Trace) -> set[int]:
    """Messages the server did not process: refused by a throttle window, or answered 413/503, or
    turned away over a WebSocket (UNAVAILABLE)."""
    out = {e["msg"] for e in t.a2s() if refused(e)}
    out |= {e["msg"] for e in t.server("s2a") if e.get("http_status", 200) != 200}
    return out


def refused(e: dict) -> bool:
    """An a2s the server turned away before taking it in: over HTTP with a status
    (`refused_status`), over a WebSocket (the Rust server: UNAVAILABLE when overloaded) with
    `refused` alone, as the socket has no status to log."""
    return bool(e.get("refused_status") or e.get("refused"))


def answered(e: dict) -> bool:
    """An s2a the server processed its message for: a 200 (or a WebSocket message) without an error."""
    return e.get("http_status", 200) == 200 and not e.get("error")


def heard_before(t: Trace) -> dict[int, bool]:
    """For each message, whether the server had answered one (`answered`) before it arrived."""
    heard, out = False, {}
    for e in t.events:
        if e["source"] != "server":
            continue
        if e["event"] == "a2s":
            out[e["msg"]] = heard
        elif e["event"] == "s2a" and answered(e):
            heard = True
    return out


@check("sequence_increments_by_one", "MUST", ["OPAMP-SEQ-01"])
def sequence_increments_by_one(t: Trace) -> Result:
    """Every message's sequence_num is the previous one plus one. Resending a message the server did
    not process with the same sequence_num is allowed: the server never counted it. So is number 0
    again before the server has answered any message (RULE-CLIENT-20: 0 is how a server tells a new
    run, so a client keeps it until a message of the run gets through). Over WebSocket a message may
    cross the first answer: 0 again, until the first number other than 0 (number_zero_until_answered
    holds that one to be 1, after an answer)."""
    msgs = t.a2s()
    unaccepted = unaccepted_msgs(t)
    heard = heard_before(t)
    numbered = False  # a number other than 0 has been sent
    for prev, cur in zip(msgs, msgs[1:]):
        numbered = numbered or prev["seq"] != 0
        if cur["seq"] == prev["seq"] + 1 or (cur["seq"] == prev["seq"] and prev["msg"] in unaccepted):
            continue
        if cur["seq"] == prev["seq"] == 0 and (not heard[cur["msg"]] or (t.transport == "ws" and not numbered)):
            continue
        return Result("fail", f"message {cur['msg']} has sequence_num {cur['seq']} after {prev['seq']}")
    return Result("pass", f"{len(msgs)} messages") if msgs else Result("fail", "no messages")


@check("number_zero_until_answered", "MUST", ["RULE-CLIENT-20"])
def number_zero_until_answered(t: Trace) -> Result:
    """Every message is number 0 until the server has answered one (a ServerToAgent without an error):
    one lost, refused or answered with an error is followed by another 0, so that whichever arrives
    first, the server sees a new run. Then 1, 2, ... Over plain HTTP the request after the first
    answer is 1 (the client had the answer before it sent); over WebSocket a message may cross the
    answer, so there it is the first number other than 0 that must come after an answer, and be 1."""
    msgs = t.a2s()
    if not msgs:
        return Result("fail", "no messages")
    heard = heard_before(t)
    zeros = 0
    for i, m in enumerate(msgs):
        if m["seq"] == 0:
            zeros += 1
            if t.transport == "http" and heard[m["msg"]]:
                return Result("fail", f"message {m['msg']} is number 0 again after the server answered")
            continue
        if not heard[m["msg"]]:
            return Result("fail", f"message {m['msg']} is number {m['seq']} before the server answered any")
        if zeros and i > 0 and msgs[i - 1]["seq"] == 0 and m["seq"] != 1:
            return Result("fail", f"message {m['msg']}, the first after the answer, is number {m['seq']}, not 1")
        break
    resent = zeros - 1 if zeros else 0
    return Result("pass", f"number 0 {zeros} time(s), {resent} sent again before an answer")


@check("reports_status_capability", "MUST", ["OPAMP-CAPS-02", "OPAMP-CAPS-05"])
def reports_status_capability(t: Trace) -> Result:
    """Every message sets capabilities, including ReportsStatus."""
    bad = [e for e in t.a2s() if not e["caps"] & 1]
    if bad:
        return Result("fail", f"message {bad[0]['msg']} has capabilities {bad[0]['caps']}, without ReportsStatus "
                              f"(bit 0x1)")
    return Result("pass") if t.a2s() else Result("fail", "no messages")


@check("instance_uid_valid", "MUST", ["OPAMP-UID-03"])
def instance_uid_valid(t: Trace) -> Result:
    """Every instance_uid is 16 bytes and not all zeros."""
    for e in t.a2s():
        if len(e["uid"]) != 32 or set(e["uid"]) == {"0"}:
            return Result("fail", f"message {e['msg']} has instance_uid {e['uid']!r}")
    return Result("pass")


@check("first_message_is_full", "MUST", ["OPAMP-STATUS-01", "OPAMP-STATUS-03", "OPAMP-CONN-02"])
def first_message_is_full(t: Trace) -> Result:
    """The client's first message is a full status report."""
    msgs = t.a2s()
    if not msgs:
        return Result("fail", "no messages")
    if not msgs[0]["full"]:
        return Result("fail", f"the first message has fields {msgs[0]['fields']}")
    return Result("pass")


@check("full_report_after_reconnect", "SHOULD", ["OPAMP-STATUS-04"], transports=("ws",))
def full_report_after_reconnect(t: Trace) -> Result:
    """The first message on each new WebSocket connection is a full status report. The spec allows a
    reconnecting client to omit unchanged fields (OPAMP-STATUS-04), but anything sent on the lost
    connection may not have arrived; the core's model of the client has our clients send the full
    status."""
    firsts = first_per_connection(t)[1:]
    if not firsts:
        return Result("skip", "no reconnect")
    for e in firsts:
        if not e["full"]:
            return Result("fail", f"message {e['msg']}, first on connection {e['conn']}, has fields {e['fields']}")
    return Result("pass", f"{len(firsts)} reconnect(s)")


@check("reports_after_connecting", "MUST", ["OPAMP-STATUS-01", "OPAMP-CONN-02"], transports=("ws",))
def reports_after_connecting(t: Trace) -> Result:
    """Every WebSocket connection gets a status report soon after it opens."""
    within = t.params.get("report_within_s", 5)
    stop = t.stop_wall()
    for opened in t.server("ws_open"):
        if stop and opened["wall"] > stop:
            continue
        first = [e for e in t.a2s() if e["conn"] == opened["conn"]]
        closed = [e for e in t.server("ws_closed") if e["conn"] == opened["conn"]]
        deadline = opened["wall"] + within
        if not first or first[0]["wall"] > deadline:
            if closed and closed[0]["wall"] < deadline:
                continue
            return Result("fail", f"connection {opened['conn']}: no message within {within}s of opening")
    return Result("pass")


@check("disconnect_is_last", "MUST", ["OPAMP-CONN-01", "OPAMP-CONN-05", "OPAMP-CONN-12"])
def disconnect_is_last(t: Trace) -> Result:
    """No message follows one that carries agent_disconnect: over HTTP at all, over WebSocket on
    the same connection (a client says goodbye before it reconnects, then carries on)."""
    msgs = t.a2s()
    for i, e in enumerate(msgs):
        if not e["disconnect"]:
            continue
        after = [m for m in msgs[i + 1:] if t.transport == "http" or m["conn"] == e["conn"]]
        if after:
            return Result("fail", f"message {after[0]['msg']} follows agent_disconnect")
    return Result("pass")


@check("reconnect_says_goodbye", "MUST", ["OPAMP-CONN-12", "OPAMP-ERR-03"], transports=("ws",))
def reconnect_says_goodbye(t: Trace) -> Result:
    """A connection the client closes before it is told to stop (to reconnect) ends with
    agent_disconnect."""
    stop = t.stop_wall() or float("inf")
    closes = [e for e in t.server("close_frame_received")
              if not e.get("answers_server_close") and e["wall"] < stop]
    if not closes:
        return Result("skip", "the client closed no connection before stopping")
    for c in closes:
        on_conn = [m for m in t.a2s() if m["conn"] == c["conn"] and m["wall"] <= c["wall"]]
        if not on_conn or not on_conn[-1]["disconnect"]:
            return Result("fail", f"closed connection {c['conn']} without agent_disconnect first")
    return Result("pass", f"{len(closes)} close(s)")


@check("stop_sends_disconnect", "SHOULD", ["OPAMP-CONN-01", "OPAMP-CONN-05", "OPAMP-CONN-08"])
def stop_sends_disconnect(t: Trace) -> Result:
    """A clean stop ends with a message carrying agent_disconnect."""
    if not t.driver("stopping"):
        return Result("skip", "the driver did not stop cleanly")
    msgs = t.a2s()
    if msgs and msgs[-1]["disconnect"]:
        return Result("pass")
    return Result("fail", "the last message has no agent_disconnect")


@check("driver_survives", "MUST", ["OPAMP-S2A-01", "OPAMP-UID-06"])
def driver_survives(t: Trace) -> Result:
    """The client process does not crash: it stops when told to and exits 0. (A run the runner
    crashed on purpose, in a crash scenario, isn't asked to.)"""
    if t.driver_exit != 0:
        fatal = t.driver("fatal", "crash")
        return Result("fail", f"driver exited {t.driver_exit}" + (f": {fatal[-1].get('error', '')}" if fatal else ""))
    started, stopped = len(t.driver("started")), len(t.driver("stopped"))
    crashed = len(t.runner("crashed"))
    if not stopped or stopped < started - crashed:
        return Result("fail", "driver exited without reporting it stopped")
    return Result("pass")


# Stop's bound: the core's stop_timeout_ms by default, and room for the process to say so.
STOP_BOUND_S = 5.0 + 2.0


@check("stop_bounded", "MUST", ["RULE-CLIENT-16"])
def stop_bounded(t: Trace) -> Result:
    """Stop returns within its bound (the core's stop_timeout_ms, 5 s by default), whatever is in
    flight: a request no one answers, a connection still opening, a download half done."""
    stopping, stopped = t.driver("stopping"), t.driver("stopped")
    if not stopping or not stopped:
        return Result("fail", "the driver never said it stopped")
    took = stopped[0]["wall"] - stopping[0]["wall"]
    if took > STOP_BOUND_S + CLOCK_SKEW_S:
        return Result("fail", f"stop took {took:.1f} s, over its bound of {STOP_BOUND_S:.0f} s")
    return Result("pass", f"stop took {took:.1f} s")


@check("nothing_outlives_stop", "MUST", ["RULE-CLIENT-16"])
def nothing_outlives_stop(t: Trace) -> Result:
    """Every request, upgrade and download the server held, the client let go of by the time it
    said it had stopped: nothing it started outlives stop (the driver lingers after, so a socket
    left open shows)."""
    held = {e["hold"]: e for e in t.server("held")}
    if not held:
        return Result("fail", "the server held nothing: the client never got as far as the scenario needs")
    stopped = t.driver("stopped")
    if not stopped:
        return Result("fail", "the driver never said it stopped")
    by = stopped[0]["wall"] + CLOCK_SKEW_S + 0.5
    closed = {e["hold"]: e for e in t.server("held_closed") if e.get("by_client")}
    left = [f"{h['what']} {h['path']}" for n, h in held.items() if n not in closed]
    late = [f"{held[n]['what']} {held[n]['path']} ({e['wall'] - stopped[0]['wall']:.1f} s after)"
            for n, e in closed.items() if e["wall"] > by]
    if left:
        return Result("fail", "still open when the server shut down: " + ", ".join(left))
    if late:
        return Result("fail", "let go of after the client said it had stopped: " + ", ".join(late))
    return Result("pass", f"{len(held)} held, all let go of by stop")


# What a driver reports of the client's notifications (DRIVERS.md): none may come after stopped.
NOTIFICATION_EVENTS = ("connect", "connect_failed", "error_response", "message", "connection_settings",
                       "telemetry_settings", "custom_message", "custom_message_not_sent",
                       "server_custom_capabilities", "install_package", "remove_package")
# The text a driver's failing callback puts in its error (DRIVER_THROW_ON_CONFIG).
THROWN = "callback-throws"


@check("callback_failure_stops", "MUST", ["RULE-SHELL-02"])
def callback_failure_stops(t: Trace) -> Result:
    """A notification callback that throws (raises, panics) on the first remote config stops the
    client, and only the client: the application hears stopped with that error once (the driver's
    one `fatal`, then exit 1; it stays a while first, reporting anything more it hears), and
    nothing after it. The client says no goodbye, and sends nothing more than what was already on
    its way: no message more than one and a half heartbeat intervals after the throw."""
    threw = t.driver("throwing")
    if not threw:
        return Result("fail", "the callback never threw: no remote config reached it")
    if len(threw) > 1:
        return Result("fail", f"the callback threw {len(threw)} times: it was called again after it failed")
    fatal = t.driver("fatal")
    if not fatal:
        stopped = t.driver("stopped")
        why = (f"it said it stopped without the error: {stopped[0].get('error')!r}" if stopped
               else "it never heard the client stopped")
        return Result("fail", f"no fatal after the callback threw: {why}")
    if len(fatal) > 1:
        return Result("fail", f"the application heard stopped with an error {len(fatal)} times")
    error = str(fatal[0].get("error") or "")
    if THROWN not in error:
        return Result("fail", f"stopped with {error!r}, not the callback's error")
    if fatal[0]["wall"] < threw[0]["wall"]:
        return Result("fail", "fatal before the callback threw")
    if t.driver("stopping", "stopped"):
        return Result("fail", "the driver stopped the client itself: it never heard the client stopped on its own")
    heard = [e["event"] for e in t.driver(*NOTIFICATION_EVENTS) if e["wall"] > fatal[0]["wall"]]
    if heard:
        return Result("fail", f"the application heard {', '.join(heard)} after stopped")
    if t.driver_exit != 1:
        return Result("fail", f"driver exited {t.driver_exit}, not 1 through fatal")
    goodbye = [e for e in t.a2s() if e["disconnect"]]
    if goodbye:
        return Result("fail", f"message {goodbye[0]['msg']} says goodbye: a failed callback ends the client without it")
    poll = float(t.scenario.get("driver", {}).get("poll_s", 2))
    by = threw[0]["wall"] + 1.5 * poll + CLOCK_SKEW_S
    late = [e for e in t.a2s() if e["wall"] > by]
    if late:
        return Result("fail", f"message {late[0]['msg']} sent {late[0]['wall'] - threw[0]['wall']:.1f} s after the "
                              f"callback threw: the client went on")
    return Result("pass", f"stopped {fatal[0]['wall'] - threw[0]['wall']:.2f} s after the throw: {error}")


@check("crash_state_consistent", "MUST", ["RULE-SHELL-09"])
def crash_state_consistent(t: Trace) -> Result:
    """A client crashed at any moment (the runner kills it, then writes what a crash of the
    machine could have left: its unflushed data lost, in part or whole, picked from the run's core
    tap by a seed) leaves its state file whole, and no older than the last save it answered stored;
    and the next run starts from exactly that: the saved state its core is given is the file's.
    On LazyFS (run.py --lazyfs) the crash is real: what LazyFS kept of the file once it dropped
    what wasn't flushed, or tore a write. (core_tap holds every point of every run to the same,
    without crashing anything.)"""
    crashes = t.runner("crashed")
    if not crashes:
        missed = t.runner("crash_missed")
        why = f": run {missed[0]['run']} exited first" if missed else ""
        return Result("fail", "the runner crashed nothing" + why)
    states = {e["run"]: e for e in t.runner("crash_state")}
    restarts = {e["run"]: e for e in t.runner("restarted")}
    seen = []
    for c in crashes:
        n = c["run"]
        left = states.get(n)
        if not left or "error" in left:
            return Result("fail", f"run {n}: no crash state: {left and left.get('error')}")
        if not left.get("ok"):
            how = (f"on LazyFS ({c['lazyfs']}) it left the state file" if c.get("lazyfs")
                   else "it could leave the state file")
            return Result("fail", f"run {n} crashed at {c['at_s']} s (seed {c['seed']}): {how} holding {left['state']}")
        again = restarts.get(n + 1)
        if again is None:
            return Result("fail", f"run {n + 1} never started after the crash")
        if again["sha256"] != left["sha256"]:
            return Result("fail", f"run {n + 1} started from {again['bytes']} bytes of state (sha256 {again['sha256']}), "
                                  f"not what the crash left ({left['state']}, sha256 {left['sha256']})")
        seen.append(f"run {n} at {c['at_s']} s{' (' + c['lazyfs'] + ')' if c.get('lazyfs') else ''}: {left['state']}")
    return Result("pass", f"seed {crashes[0]['seed']}; " + "; ".join(seen))


@check("http_content_type", "MUST", ["OPAMP-HTTP-03"], transports=("http",))
def http_content_type(t: Trace) -> Result:
    """Every plain HTTP request has Content-Type: application/x-protobuf."""
    for e in t.a2s():
        if e.get("content_type") != "application/x-protobuf":
            return Result("fail", f"Content-Type {e.get('content_type')!r}")
    return Result("pass")


@check("http_instance_uid_header", "SHOULD", ["OPAMP-HTTP-05"], transports=("http",))
def http_instance_uid_header(t: Trace) -> Result:
    """Every plain HTTP request has OpAMP-Instance-UID set to the message's instance_uid."""
    for e in t.a2s():
        want = str(uuid.UUID(hex=e["uid"])) if len(e["uid"]) == 32 else None
        if (e.get("uid_header") or "").lower() != want:
            return Result("fail", f"header {e.get('uid_header')!r}, message instance_uid {want}")
    return Result("pass")


# ------------------------------------------------------------------------------------------
# Server requests

def responses(t: Trace, key: str) -> list[dict]:
    return [e for e in t.server("s2a") if e.get(key)]


def reached(t: Trace, reply: dict) -> bool:
    """Whether a reply got to a client still there to act on it."""
    # A reply written after the client stopped never reached it: the server had been busy so
    # long that the run ended first.
    begun = [b for b, _ in runs(t) if b <= reply["wall"]]
    if begun and any(max(begun) <= e["wall"] < reply["wall"] for e in t.driver("stopping")):
        return False
    # A reply to a goodbye: the client ended that conversation with it (over a WebSocket it
    # closes the connection right after), so the reply races its close. So does a reply its
    # goodbye crossed: the client had said goodbye on that connection before the reply could
    # reach it (read by the server just after it wrote the reply, or before).
    crossed = reply["wall"] + CLOCK_SKEW_S
    return not any(e["disconnect"] and (e["msg"] == reply["msg"] or e["msg"] > reply["msg"] and e["wall"] <= crossed
                                        and reply.get("conn") in (None, e["conn"]))
                   for e in t.a2s())


@check("adopts_assigned_uid", "MUST", ["OPAMP-UID-05", "OPAMP-UID-06"])
def adopts_assigned_uid(t: Trace) -> Result:
    """After the server assigns an instance UID, the next message uses it."""
    assigned = responses(t, "assign_uid")
    if not assigned:
        return Result("skip", "the scenario assigns no UID")
    assigned = [a for a in assigned if reached(t, a)]
    judged = 0
    for a in assigned:
        # Over a WebSocket a message can cross the response: sent before the client had it, and read
        # by the server just after it wrote it (it had been busy). Those don't count.
        # (`crossed`: torture's as_heard found in the client's tap it was sent before the client read
        # the assignment, however late the server read it.)
        crossed = a["wall"] + CLOCK_SKEW_S if t.transport == "ws" else a["wall"]
        later = [e for e in t.a2s() if e["msg"] > a["msg"] and e["wall"] > crossed
                 and a["msg"] not in e.get("crossed", ())]
        # Assigned again before the next message was read (in reply to one that crossed this
        # reply): the next message must carry the newer UID, and is judged by that assignment.
        if later and any(a["msg"] < b["msg"] < later[0]["msg"] for b in assigned):
            continue
        judged += 1
        if not later:
            return Result("fail", f"no message after the UID {a['assign_uid']} was assigned")
        if later[0]["uid"] != a["assign_uid"]:
            return Result("fail", f"message {later[0]['msg']} still uses {later[0]['uid']}, assigned {a['assign_uid']}")
    if not judged:
        return Result("skip", "no assigned UID reached the client (assigned after it stopped, or to a goodbye)")
    return Result("pass")


def next_status_is_full(t: Trace, triggers: list[dict], what: str) -> Result:
    if not triggers:
        return Result("skip", f"no {what}")
    for trig in triggers:
        later = [e for e in t.a2s() if e["msg"] > trig["msg"] and carries_status(e)]
        if later and not later[0]["full"]:
            return Result("fail", f"message {later[0]['msg']}, the first status report after message "
                                  f"{trig['msg']}'s response, is partial ({later[0]['fields']})")
    return Result("pass", f"{len(triggers)} {what}")


# The server capability a status field needs before a client sends it (opamp-kernel's `gate`).
STATUS_GATES = {"agent_description": 0x1, "health": 0x1, "available_components": 0x1,
                "remote_config_status": 0x2, "effective_config": 0x4, "package_statuses": 0x10}
# Every field of an agent's status, as the server logs them (`fields`): the rest need no capability.
FULL_STATE = {*STATUS_GATES, "connection_settings_status", "custom_capabilities"}


def taken_by_server(t: Trace) -> set[str]:
    """The status fields the scenario's server takes, by the capabilities it announces: the
    scenario's, or 0x27 (conformance/server and the Rust server alike), with the packages' two
    when it offers packages."""
    server = t.scenario.get("server", {})
    caps = server.get("capabilities")
    if caps is None:
        caps = 0x27 | (0x18 if server.get("packages") is not None else 0)
    return {f for f in FULL_STATE if caps & STATUS_GATES.get(f, caps)}


@check("full_report_after_request", "MUST", ["OPAMP-STATUS-13"])
def full_report_after_request(t: Trace) -> Result:
    """The first message a client sends once it has a response with ReportFullState is its full
    state: the description, and with it every status field it has reported in this run that the
    server takes. Not the next message that happens to carry the description: a heartbeat first
    means the request was ignored, and so does a later change of the description alone. Over
    WebSocket a message may cross the response (sent before the client had it, read by the server
    just after), so one read within the clock tolerance of the response is passed over; so is a
    goodbye without the state (the client is leaving), while one that carries it is the answer.
    No message at all fails once the client has gone on for a heartbeat and a half. A request that
    never reached the client (written after it was told to stop, or in reply to its goodbye), or
    that the run ended on before then, isn't judged. (A client whose whole status is its
    description can't be told from one that ignored the request and then changed it, so the
    report-full-state scenario changes nothing after it asks.)"""
    asks = responses(t, "report_full_state")
    if not asks:
        return Result("skip", "no ReportFullState")
    taken = taken_by_server(t)
    patience = 1.5 * float(t.scenario.get("driver", {}).get("poll_s", 2)) + CLOCK_SKEW_S
    restarts = [b for b, _ in runs(t)][1:]
    judged = 0
    for ask in asks:
        if not reached(t, ask):
            continue
        # The driver's run the request fell in: a restarted client reports afresh.
        begin = max((b for b in restarts if b <= ask["wall"]), default=float("-inf"))
        end = min((b for b in restarts if b > ask["wall"]), default=float("inf"))
        before = [m for m in t.a2s() if m["wall"] >= begin and m["msg"] <= ask["msg"]]
        reported = {f for m in before for f in m["fields"]} & taken
        for m in t.a2s():
            if m["msg"] <= ask["msg"] or m["wall"] >= end:
                continue
            missing = sorted(({"agent_description"} | reported) - set(m["fields"]))
            if not missing:
                judged += 1
                break
            if m["disconnect"] or (t.transport == "ws" and m["wall"] <= ask["wall"] + CLOCK_SKEW_S):
                continue
            return Result("fail", f"message {m['msg']}, the first after message {ask['msg']}'s response asked for "
                                  f"the full state, has {m['fields']}: without {', '.join(missing)}")
        else:
            ends = [e["wall"] for e in [*t.driver("stopping", "stopped", "fatal"), *t.runner("crashed")]
                    if e["wall"] >= ask["wall"]]
            until = min([*ends, end, t.events[-1]["wall"]])
            if until - ask["wall"] > patience:
                return Result("fail", f"no full report in the {until - ask['wall']:.1f} s the client went on after "
                                      f"message {ask['msg']}'s response asked for the full state")
    if not judged:
        return Result("skip", "no ReportFullState reached a client with time left to answer it")
    return Result("pass", f"{judged} ReportFullState")


@check("asks_wait_for_the_heartbeat", "MUST", ["RULE-CLIENT-21"])
def asks_wait_for_the_heartbeat(t: Trace) -> Result:
    """A server that asks for the full state in its answer to every message gets about one message
    per heartbeat interval, not one per round trip (scenario description-too-large: the agent's
    description is over the client's max_send_bytes, so no message carries it and the server never
    learns the agent). From the server's first ask to the client's stop: at most two messages per
    heartbeat interval (the driver's poll_s), and three more; and at least one per two intervals,
    less one: the heartbeat goes on. Answering every ask at once with the same report sends
    hundreds a second."""
    asks = [e for e in t.server("s2a") if e.get("report_full_state")]
    if not asks:
        return Result("skip", "the server never asked for the full state")
    poll = float(t.scenario.get("driver", {}).get("poll_s", 2))
    start = asks[0]["wall"]
    end = t.stop_wall() or max(m["wall"] for m in t.a2s())
    sent = [m for m in t.a2s() if start < m["wall"] <= end and not m.get("disconnect")]
    span = end - start
    most, least = 2 * span / poll + 3, span / (2 * poll) - 1
    if len(sent) > most:
        return Result("fail", f"{len(sent)} messages in the {span:.1f}s after the server first asked for the full "
                              f"state, with a heartbeat every {poll:g}s (at most {most:.0f}): one per round trip")
    if len(sent) < least:
        return Result("fail", f"{len(sent)} messages in the {span:.1f}s after the server first asked for the full "
                              f"state, with a heartbeat every {poll:g}s (at least {least:.0f})")
    return Result("pass", f"{len(sent)} messages in {span:.1f}s, {len(asks)} asks")


@check("full_report_after_unaccepted", "SHOULD", ["OPAMP-STATUS-13"])
def full_report_after_unaccepted(t: Trace) -> Result:
    """After a message the server did not accept (rejected, throttled, UNAVAILABLE), the next status report is
    full. Not a spec rule: the spec relies on the server noticing the sequence gap and asking. It is the rule
    the core's model of the client shows a client needs to stay correct against servers that don't ask."""
    triggers = [e for e in t.server("s2a") if e.get("error") or e.get("http_status", 200) != 200]
    triggers += [dict(e, msg=max([m["msg"] for m in t.a2s() if m["wall"] <= e["wall"]], default=0))
                 for e in t.server("request_rejected")]
    return next_status_is_full(t, triggers, "unaccepted message(s)")


@check("rejected_not_resent", "MUST", ["OPAMP-SIZE-09", "OPAMP-ERR-01"])
def rejected_not_resent(t: Trace) -> Result:
    """A message rejected with 413 or BAD_REQUEST is never sent again unchanged."""
    rejected = [e for e in t.server("s2a") if e.get("http_status") == 413 or e.get("error") == "BAD_REQUEST"]
    if not rejected:
        return Result("skip", "nothing was rejected")
    by_msg = {e["msg"]: e for e in t.a2s()}
    asks = [e for e in t.server("s2a") if e.get("report_full_state")]

    def answers_ask(r: dict, e: dict) -> bool:
        # Sending the same content because the server asked for the full state is an answer, not a
        # retry: an ask after the rejection.
        if any(r["msg"] <= a["msg"] < e["msg"] for a in asks):
            return True
        # Over a WebSocket the answer to an ask the client had before the rejection can cross the
        # rejection: sent before the client had read it (the client one message ahead, as when a
        # status change goes out between two asks). Both within the clock tolerance of the send.
        return t.transport == "ws" and e["wall"] <= r["wall"] + CLOCK_SKEW_S and any(
            a["msg"] < r["msg"] and e["wall"] - CLOCK_SKEW_S <= a["wall"] for a in asks)

    for r in rejected:
        orig = by_msg.get(r["msg"])
        if orig is None or not carries_status(orig):
            continue
        # (`crossed`: torture's as_heard found in the client's tap it was sent before the rejection was read.)
        again = [e for e in t.a2s() if e["msg"] > r["msg"] and e["payload_hash"] == orig["payload_hash"]
                 and not answers_ask(r, e) and r["msg"] not in e.get("crossed", ())]
        if again:
            return Result("fail", f"message {r['msg']} was rejected and resent unchanged {len(again)} time(s), "
                                  f"unasked")
    return Result("pass")


@check("honours_retry_after", "SHOULD", ["OPAMP-ERR-04", "OPAMP-ERR-06"])
def honours_retry_after(t: Trace) -> Result:
    """After Retry-After (HTTP) or retry_info (WebSocket), the next attempt waits that long."""
    slack = t.params.get("retry_slack_s", 0.5)
    waits = []
    for e in t.server("request_rejected"):
        if e.get("retry_after"):
            waits.append((e["wall"], float(e["retry_after"])))
    for e in t.server("s2a"):
        if e.get("http_status") == 503 and e.get("retry_after"):
            waits.append((e["wall"], float(e["retry_after"])))
        if e.get("error") == "UNAVAILABLE" and e.get("retry_after"):
            waits.append((e["wall"], float(e["retry_after"])))
    if not waits:
        return Result("skip", "no Retry-After was sent")
    # The goodbye on a WebSocket closed after UNAVAILABLE (OPAMP-CONN-12) isn't a new attempt.
    attempts = sorted(e["wall"] for e in t.server("a2s", "request_rejected", "ws_open") if not e.get("disconnect"))
    for at, wait in waits:
        nxt = [w for w in attempts if w > at + 0.01]
        if nxt and nxt[0] < at + wait - slack:
            return Result("fail", f"next attempt {nxt[0] - at:.1f}s after a Retry-After of {wait:g}s")
    return Result("pass", f"{len(waits)} Retry-After(s)")


@check("heartbeat_follows_offer", "MUST", ["OPAMP-HB-04", "OPAMP-HB-05", "OPAMP-HB-07"])
def heartbeat_follows_offer(t: Trace) -> Result:
    """With ReportsHeartbeat, messages come at the heartbeat interval the server offered."""
    offers = [e["heartbeat_offer"] for e in t.server("s2a") if "heartbeat_offer" in e]
    if not offers or not offers[0]:
        return Result("skip", "no heartbeat interval offered")
    # Declared capabilities: the first message that sets them (some clients leave later ones 0).
    declared = next((e["caps"] for e in t.a2s() if e["caps"]), 0)
    if not declared & 0x2000:
        return Result("skip", "the client does not declare ReportsHeartbeat")
    offer = offers[0]
    stop = t.stop_wall() or float("inf")
    # Every regular message counts as a heartbeat; some clients put health in each one.
    beats = [e for e in t.a2s()[1:] if not e["disconnect"] and e["wall"] < stop]
    gaps = [b["wall"] - a["wall"] for a, b in zip(beats, beats[1:])]
    if len(gaps) < 2:
        return Result("fail", f"only {len(beats)} heartbeat(s) with {offer}s offered")
    median = statistics.median(gaps)
    tolerance = max(1.0, 0.25 * offer)
    if abs(median - offer) > tolerance:
        return Result("fail", f"median interval {median:.1f}s, offered {offer}s")
    return Result("pass", f"median interval {median:.1f}s, offered {offer}s")


@check("status_change_reported", "MUST", ["OPAMP-STATUS-02"])
def status_change_reported(t: Trace) -> Result:
    """Every status change the agent makes reaches the server."""
    changes = t.driver("status_change")
    if not changes:
        return Result("skip", "the scenario changes no status")
    within = t.params.get("report_within_s", 5)
    for c in changes:
        # Driver and server clocks can differ slightly (a driver in a container), so allow some skew.
        later = [e for e in t.a2s() if c["wall"] - CLOCK_SKEW_S <= e["wall"] <= c["wall"] + within
                 and "agent_description" in e["fields"]]
        if not later:
            return Result("fail", f"status change {c['change']} not reported within {within}s")
    return Result("pass", f"{len(changes)} change(s)")


@check("applies_remote_config", "SHOULD", ["OPAMP-CAPS-06"])
def applies_remote_config(t: Trace) -> Result:
    """An offered remote config is acknowledged with its hash. A config offered again after the
    client reported on it needs nothing new: an unchanged status is left out."""
    disconnects = {e["msg"] for e in t.a2s() if e["disconnect"]}
    # A config offered in reply to the client's last message can't be applied.
    offers = [o for o in responses(t, "offer_config") if o["msg"] not in disconnects]
    if not offers:
        return Result("skip", "no remote config offered while the client was running")
    within = t.params.get("report_within_s", 5)
    for o in offers:
        acks = [e for e in t.a2s() if e["wall"] <= o["wall"] + within
                and e.get("remote_config_status", {}).get("hash") == o["offer_config"]]
        if not acks:
            return Result("fail", f"config {o['offer_config'][:12]} not acknowledged within {within}s")
    return Result("pass")


# ------------------------------------------------------------------------------------------
# WebSocket connection handling

@check("respects_server_capabilities", "MUST", ["OPAMP-CAPS-08"])
def respects_server_capabilities(t: Trace) -> Result:
    """Once the server has said it doesn't accept the effective config, the client stops sending it."""
    caps = t.params.get("server_capabilities")
    if caps is None or caps & 0x4:
        return Result("skip", "the server accepts the effective config")
    if not any("effective_config" in m["fields"] for m in t.a2s()):
        return Result("skip", "the client never sends the effective config")
    answered = [e["wall"] for e in t.server("s2a")]
    if not answered:
        return Result("skip", "no response")
    after = [m for m in t.a2s() if m["wall"] > answered[0] and "effective_config" in m["fields"]]
    if after:
        return Result("fail", f"message {after[0]['msg']} sends the effective config to a server that doesn't take it")
    return Result("pass")


@check("close_frame_answered", "MUST", ["OPAMP-CONN-07"], transports=("ws",))
def close_frame_answered(t: Trace) -> Result:
    """The client answers a server Close frame with its own (RFC 6455 section 5.5.1)."""
    sent = t.server("close_frame_sent")
    if not sent:
        return Result("skip", "the server sent no Close frame")
    answered = {e["conn"] for e in t.server("close_frame_received") if e.get("answers_server_close")}
    for s in sent:
        if s["conn"] not in answered:
            return Result("fail", f"connection {s['conn']}: Close frame never answered")
    return Result("pass")


@check("reconnects_after_loss", "SHOULD", ["OPAMP-CONN-09", "OPAMP-CONN-10"], transports=("ws",))
def reconnects_after_loss(t: Trace) -> Result:
    """After the connection is lost, the client connects again."""
    within = t.params.get("reconnect_within_s", 15)
    stop = t.stop_wall() or float("inf")
    losses = [e for e in t.server("ws_closed") if e["wall"] < stop - 0.5]
    if not losses:
        return Result("skip", "no connection was lost")
    judged = 0
    for lost in losses:
        # A newer connection, whether the server logged it before or after the close: a client that
        # reconnects at once can open it before the server has logged the old one closed.
        again = [e for e in t.server("ws_open") if e["conn"] > lost["conn"] and e["wall"] <= lost["wall"] + within]
        if again:
            judged += 1
        elif lost["wall"] + within < stop:
            return Result("fail", f"no new connection within {within}s of losing connection {lost['conn']}")
        # Otherwise the run ended before the window did: not a reconnect, and nothing to judge.
    if not judged:
        return Result("skip", f"no loss the run outlasted by {within}s")
    return Result("pass", f"{judged} loss(es)")


def gaps_text(gaps: list[float], most: int = 8) -> str:
    shown = ", ".join(f"{g:.2f}" for g in gaps[:most])
    return f"[{shown}{', ...' if len(gaps) > most else ''}]s"


@check("reconnect_backs_off", "MUST", ["OPAMP-CONN-03", "OPAMP-CONN-09", "OPAMP-CONN-10"], transports=("ws",))
def reconnect_backs_off(t: Trace) -> Result:
    """A server (or a proxy in front of it) that accepts every WebSocket upgrade and drops the
    connection before answering anything (fault accept_then_drop) gets reconnects that back off,
    not a storm: a connection lost before the server answered on it is a failed attempt. Over the
    first backoff_observe_s seconds from the first such connection: at most backoff_max_attempts
    connections and, given four gaps or more between them, the last at least backoff_min_growth_s
    longer than the first.

    Exponential backoff from 1 s with equal jitter (each delay between half and all of twice the
    one before) gives four to six connections in 30 s, and a fourth gap at least 3 s longer than
    the first. Each gap also holds the client's time to notice the drop and connect again, which
    varies: the Java client in Docker under load took 2.2 s for its first reconnect, then 0.7 s to
    1.4 s. Hence a difference in seconds, not a ratio, and nothing judged on fewer than four gaps.
    Reconnecting at once gives hundreds of connections, or with a slow connect, gaps that do not
    grow. Fewer than three connections is no storm: a client that waits longer, or gives up (which
    reconnects_after_loss judges), passes once the run has outlasted the observation."""
    faults = [f for f in t.scenario.get("server", {}).get("faults", []) if f.get("action") == "accept_then_drop"]
    if not faults:
        return Result("skip", "the server accepted and dropped no connection")
    observe = t.params.get("backoff_observe_s", 30)
    most = t.params.get("backoff_max_attempts", 8)
    growth = t.params.get("backoff_min_growth_s", 1.0)
    dropped = sorted(e["wall"] for e in t.server("accept_then_drop"))
    if not dropped:
        return Result("skip", "the client never connected while the server accepted and dropped connections")
    first = dropped[0]
    seen = [w for w in dropped if w <= first + observe]
    gaps = [b - a for a, b in zip(seen, seen[1:])]
    if len(seen) > most:
        return Result("fail", f"{len(seen)} connections in {observe:g}s, each dropped before an answer "
                              f"(at most {most}): gaps {gaps_text(gaps)}")
    # Did the run (or the fault's window) outlast the observation? The window counts from the
    # driver's first request (the server logs first_contact), which places it on the wall clock;
    # a log without it counts from the server's start, which its events' t (seconds since) gives.
    horizon = t.stop_wall() or float("inf")
    contact = t.server("first_contact")
    timed = [e for e in t.server("accept_then_drop") if "t" in e]
    if contact or timed:
        started = contact[0]["wall"] if contact else timed[0]["wall"] - timed[0]["t"]
        horizon = min(horizon, started + max(f["window_s"][1] for f in faults))
    if len(seen) < 3:
        if horizon < first + observe:
            return Result("skip", f"{len(seen)} connection(s): the run or the fault ended "
                                  f"{max(0.0, horizon - first):.1f}s after the first, before {observe:g}s")
        # Waiting longer than that, or giving up (reconnects_after_loss's to judge), is no storm.
        return Result("pass", f"{len(seen)} connection(s) in {observe:g}s (gaps {gaps_text(gaps)})")
    if len(gaps) >= 4 and gaps[-1] < gaps[0] + growth:
        return Result("fail", f"{len(seen)} connections in {observe:g}s, gaps {gaps_text(gaps)}: the last is not "
                              f"{growth:g}s longer than the first, no backoff")
    return Result("pass", f"{len(seen)} connections in {observe:g}s, gaps {gaps_text(gaps)}")


# ------------------------------------------------------------------------------------------
# Connection-settings offers (scenario "credentials"; docs/connection-settings.md)

def credentials_offer(t: Trace) -> tuple[dict | None, str]:
    """The first response carrying the credentials offer, and the offer's hash."""
    offers = [e for e in t.server("s2a") if e.get("credentials_offer")]
    return (offers[0], offers[0]["credentials_offer"]) if offers else (None, "")


def settings_status(t: Trace, hash_: str, status: str) -> list[dict]:
    return [e for e in t.a2s() if e.get("connection_settings_status", {}).get("hash") == hash_
            and e["connection_settings_status"]["status"].endswith(status)]


@check("connection_settings_applied", "MUST", ["OPAMP-CONNSET-03", "OPAMP-CONNSET-06"])
def connection_settings_applied(t: Trace) -> Result:
    """The agent connects with the offered credentials, and only then reports APPLIED for them."""
    offer, h = credentials_offer(t)
    if offer is None:
        return Result("fail", "the server never got to offer credentials")
    token = t.scenario["server"]["credentials"]["offer"]
    used = [e for e in t.server("auth") if e["ok"] and e["token"] == token and e["wall"] >= offer["wall"]]
    if not used:
        return Result("fail", "the agent never connected with the offered credentials")
    applied = settings_status(t, h, "APPLIED")
    if not applied:
        return Result("fail", "no APPLIED status for the offer")
    if applied[0]["wall"] < used[0]["wall"]:
        return Result("fail", f"message {applied[0]['msg']} reports APPLIED before the credentials were used")
    return Result("pass", f"APPLIED {applied[0]['wall'] - offer['wall']:.1f}s after the offer")


@check("connection_settings_applying", "SHOULD", ["OPAMP-CONNSET-02"])
def connection_settings_applying(t: Trace) -> Result:
    """The agent reports APPLYING for an offer before APPLIED."""
    offer, h = credentials_offer(t)
    if offer is None:
        return Result("skip", "no offer")
    applying, applied = settings_status(t, h, "APPLYING"), settings_status(t, h, "APPLIED")
    if not applying:
        return Result("fail", "no APPLYING status for the offer")
    if applied and applied[0]["msg"] < applying[0]["msg"]:
        return Result("fail", "APPLIED came before APPLYING")
    return Result("pass")


@check("connection_settings_failed", "MUST", ["OPAMP-CONNSET-04"])
def connection_settings_failed(t: Trace) -> Result:
    """Offered credentials that don't work are reported FAILED, and the agent goes back to the ones
    it had."""
    offer, h = credentials_offer(t)
    if offer is None:
        return Result("fail", "the server never got to offer credentials")
    failed = settings_status(t, h, "FAILED")
    if not failed:
        return Result("fail", "no FAILED status for the offer")
    bootstrap = t.scenario["server"]["credentials"]["bootstrap"]
    if not [e for e in t.server("auth") if e["ok"] and e["token"] == bootstrap and e["wall"] > offer["wall"]]:
        return Result("fail", "the agent never went back to the credentials it had")
    return Result("pass", f"FAILED: {failed[0]['connection_settings_status']['error']}")


@check("never_locked_out", "MUST", ["OPAMP-CONNSET-06"])
def never_locked_out(t: Trace) -> Result:
    """The agent keeps access: once it has connected with the offered credentials it is never
    refused again, a restart included (it kept them), and every run of the driver gets in."""
    auth = t.server("auth")
    switched = next((e for e in auth if e.get("switched")), None)
    if switched:
        refused = [e for e in auth if not e["ok"] and e["wall"] > switched["wall"]]
        if refused:
            return Result("fail", f"refused with token {refused[0]['token']!r} {refused[0]['t']:.1f}s in, "
                                  f"after the switch to the offered credentials")
    starts = [e["wall"] for e in t.driver("started")] + [float("inf")]
    for i, (begin, end) in enumerate(zip(starts, starts[1:])):
        if not [e for e in auth if e["ok"] and begin - CLOCK_SKEW_S <= e["wall"] < end]:
            return Result("fail", f"run {i + 1} of the driver never got in")
    if auth and not auth[-1]["ok"]:
        return Result("fail", f"the last attempt was refused (token {auth[-1]['token']!r})")
    return Result("pass", f"{len(starts) - 1} run(s)")


def offer_at(t: Trace, step: int) -> tuple[dict | None, str]:
    """The first response carrying the offer at `step` (0 the first, 1 the key after it), and its
    hash."""
    offers = [e for e in t.server("s2a") if e.get("credentials_offer") and e.get("credentials_offer_step", 0) == step]
    return (offers[0], offers[0]["credentials_offer"]) if offers else (None, "")


@check("connection_settings_rotated", "MUST", ["OPAMP-CONNSET-03", "OPAMP-CONNSET-06"])
def connection_settings_rotated(t: Trace) -> Result:
    """A second key, offered once the agent has the first: the agent connects with it, only then
    reports APPLIED for it, and never sends an older key again, a restart included."""
    offer, h = offer_at(t, 1)
    if offer is None:
        return Result("fail", "the server never got to offer the second key")
    creds = t.scenario["server"]["credentials"]
    auth = t.server("auth")
    used = [e for e in auth if e["ok"] and e["token"] == creds["then"] and e["wall"] >= offer["wall"]]
    if not used:
        return Result("fail", "the agent never connected with the second key")
    applied = settings_status(t, h, "APPLIED")
    if not applied:
        return Result("fail", "no APPLIED status for the second key")
    if applied[0]["wall"] < used[0]["wall"]:
        return Result("fail", f"message {applied[0]['msg']} reports APPLIED before the second key was used")
    old = [e for e in auth if e["token"] != creds["then"] and e["wall"] > used[0]["wall"]]
    if old:
        return Result("fail", f"sent the older key {old[0]['token']!r} {old[0]['t']:.1f}s in, after the second one worked")
    return Result("pass", f"APPLIED {applied[0]['wall'] - offer['wall']:.1f}s after the offer")


@check("connection_settings_moved", "MUST", ["OPAMP-CONNSET-03", "OPAMP-CONNSET-06"])
def connection_settings_moved(t: Trace) -> Result:
    """An offer of another endpoint with a new key (a move): the agent goes there with the new key,
    never sends the new key to the old endpoint nor the old key to the new one, and once there
    stays there, a restart included."""
    offer, h = offer_at(t, 0)
    if offer is None:
        return Result("fail", "the server never got to offer the move")
    creds = t.scenario["server"]["credentials"]
    auth = t.server("auth")
    for e in auth:
        if e["token"] == creds["offer"] and not e.get("moved"):
            return Result("fail", f"sent the new key to the old endpoint {e['t']:.1f}s in")
        if e.get("moved") and e["token"] != creds["offer"]:
            return Result("fail", f"sent the key {e['token']!r} to the new endpoint {e['t']:.1f}s in")
    there = [e for e in auth if e["ok"] and e.get("moved")]
    if not there:
        return Result("fail", "the agent never reached the new endpoint")
    back = [e for e in auth if not e.get("moved") and e["wall"] > there[0]["wall"]]
    if back:
        return Result("fail", f"went back to the old endpoint {back[0]['t']:.1f}s in, after reaching the new one")
    applied = settings_status(t, h, "APPLIED")
    if not applied:
        return Result("fail", "no APPLIED status for the move")
    if applied[0]["wall"] < there[0]["wall"]:
        return Result("fail", f"message {applied[0]['msg']} reports APPLIED before the new endpoint answered")
    return Result("pass", f"moved {there[0]['wall'] - offer['wall']:.1f}s after the offer")


# ------------------------------------------------------------------------------------------
# The remote config lifecycle (scenario "remote_config"; docs/remote-config.md)

def runs(t: Trace) -> list[tuple[float, float]]:
    """Each run of the driver: from its start to the next (a scenario can restart it)."""
    starts = [e["wall"] for e in t.driver("started")]
    return list(zip(starts, starts[1:] + [float("inf")]))


def handed(t: Trace, begin: float, end: float) -> list[str]:
    """The remote configs the application was given in a run, in order."""
    return [e["remote_config_hash"] for e in t.driver("message")
            if e.get("remote_config_hash") and begin <= e["wall"] < end]


@check("remote_config_handed_once", "MUST", ["RULE-CLIENT-01", "OPAMP-S2A-02"])
def remote_config_handed_once(t: Trace) -> Result:
    """A config the server sends again and again reaches the application once per run."""
    offers = [e for e in t.server("s2a") if e.get("offer_config")]
    if len(offers) < 2:
        return Result("fail", f"the server sent its config {len(offers)} time(s): nothing to check")
    for i, (begin, end) in enumerate(runs(t)):
        got = handed(t, begin, end)
        if len(got) != len(set(got)):
            return Result("fail", f"run {i + 1}: the application got {got}")
    return Result("pass", f"{len(offers)} offers")


@check("remote_config_kept_across_restart", "MUST", ["RULE-CLIENT-02"])
def remote_config_kept_across_restart(t: Trace) -> Result:
    """After a restart the application gets the config it had at once, and the server has no need
    to send it again."""
    spans = runs(t)
    if len(spans) < 2:
        return Result("fail", "the driver ran once: no restart")
    first = handed(t, *spans[0])
    if not first:
        return Result("fail", "no config in the first run")
    begin, end = spans[1]
    again = handed(t, begin, end)
    if not again or again[0] != first[-1]:
        return Result("fail", f"after the restart the application got {again}, not {first[-1]}")
    resent = [e for e in t.server("s2a") if e.get("offer_config") and e["wall"] >= begin - CLOCK_SKEW_S]
    if resent:
        return Result("fail", f"the server had to send the config again (message {resent[0]['msg']})")
    return Result("pass")


# ------------------------------------------------------------------------------------------
# Telemetry settings (scenario "own_telemetry"; docs/telemetry-settings.md)

def telemetry_handed(t: Trace, begin: float, end: float) -> list[dict]:
    """The telemetry settings the application was given in a run, in order."""
    return [e for e in t.driver("telemetry_settings") if begin <= e["wall"] < end]


@check("telemetry_settings_applied", "MUST", ["RULE-CLIENT-07"])
def telemetry_settings_applied(t: Trace) -> Result:
    """The application gets the offered metrics endpoint, and the server hears APPLIED for the offer
    only after that."""
    offers = [e for e in t.server("s2a") if e.get("telemetry_offer")]
    if not offers:
        return Result("fail", "the server offered no telemetry settings (was ReportsOwnMetrics declared?)")
    h = offers[0]["telemetry_offer"]
    endpoint = t.scenario["server"]["own_telemetry"]["endpoint"]
    given = [e for e in t.driver("telemetry_settings") if e.get("hash") == h]
    if not given:
        return Result("fail", "the application was never given the offered settings")
    if given[0].get("metrics_endpoint") != endpoint:
        return Result("fail", f"the application got metrics endpoint {given[0].get('metrics_endpoint')!r}")
    applied = settings_status(t, h, "APPLIED")
    if not applied:
        return Result("fail", "no APPLIED status for the offer")
    if applied[0]["wall"] < given[0]["wall"] - CLOCK_SKEW_S:
        return Result("fail", f"message {applied[0]['msg']} reports APPLIED before the application had the settings")
    return Result("pass", f"APPLIED {applied[0]['wall'] - offers[0]['wall']:.1f}s after the offer")


@check("telemetry_settings_kept_across_restart", "MUST", ["RULE-CLIENT-07"])
def telemetry_settings_kept_across_restart(t: Trace) -> Result:
    """After a restart the application gets the settings it had at once, and the server has no
    need to send them again."""
    spans = runs(t)
    if len(spans) < 2:
        return Result("fail", "the driver ran once: no restart")
    first = telemetry_handed(t, *spans[0])
    if not first:
        return Result("fail", "no telemetry settings in the first run")
    begin, end = spans[1]
    again = telemetry_handed(t, begin, end)
    if not again or again[0].get("hash") != first[-1].get("hash"):
        return Result("fail", f"after the restart the application got {[e.get('hash') for e in again]}")
    resent = [e for e in t.server("s2a") if e.get("telemetry_offer") and e["wall"] >= begin - CLOCK_SKEW_S]
    if resent:
        return Result("fail", f"the server had to send the settings again (message {resent[0]['msg']})")
    return Result("pass")


# ------------------------------------------------------------------------------------------
# Available components (scenario "available-components")

REPORTS_AVAILABLE_COMPONENTS = 0x4000


@check("components_hash_first", "SHOULD", ["OPAMP-COMP-01", "OPAMP-COMP-04"])
def components_hash_first(t: Trace) -> Result:
    """The first message carries the components' hash alone, and the capability that says so."""
    msgs = t.a2s()
    if not msgs:
        return Result("fail", "no messages")
    first = msgs[0]
    ac = first.get("available_components")
    if not ac or not ac["hash"]:
        return Result("fail", "the first message has no available_components hash")
    if ac["components"]:
        return Result("fail", f"the first message has {ac['components']} components in full")
    if not first["caps"] & REPORTS_AVAILABLE_COMPONENTS:
        return Result("fail", "ReportsAvailableComponents not declared")
    return Result("pass")


@check("components_in_full_when_asked", "MUST", ["RULE-CLIENT-09", "OPAMP-COMP-02"])
def components_in_full_when_asked(t: Trace) -> Result:
    """Asked (ReportAvailableComponents), the client sends its components in full in the next
    message about them, once; every other time, the hash alone."""
    asks = [e for e in t.server("s2a") if e.get("ask_components")]
    if not asks:
        return Result("fail", "the server never asked for the components")
    about = [e for e in t.a2s() if e.get("available_components")]
    if any(not e["available_components"]["hash"] for e in about):
        return Result("fail", "available_components without the hash")
    # Messages the client sent after the one the request answered.
    after = [e for e in about if e["msg"] > asks[0]["msg"]]
    if not after or not after[0]["available_components"]["components"]:
        return Result("fail", "the message after the request doesn't have the components in full")
    full = [e for e in about if e["available_components"]["components"]]
    if len(full) > len(asks):
        return Result("fail", f"components in full {len(full)} times for {len(asks)} request(s)")
    return Result("pass", f"{after[0]['available_components']['components']} components")


# ------------------------------------------------------------------------------------------
# Custom capabilities and messages (scenario "custom-messages")

ECHO = "io.opentelemetry.echo"


@check("custom_capabilities_declared", "MUST", ["RULE-CLIENT-10"])
def custom_capabilities_declared(t: Trace) -> Result:
    """The first message declares the agent's custom capabilities."""
    msgs = t.a2s()
    if not msgs:
        return Result("fail", "no messages")
    caps = msgs[0].get("custom_capabilities")
    if not caps or ECHO not in caps:
        return Result("fail", f"the first message declares {caps!r}")
    return Result("pass")


@check("custom_message_answered", "MUST", ["RULE-CLIENT-10"])
def custom_message_answered(t: Trace) -> Result:
    """A custom message for a declared capability reaches the application, and the one it sends
    back reaches the server."""
    pings = [e for e in t.server("s2a") if e.get("custom_ping") is not None]
    if not pings:
        return Result("fail", "the server sent no custom message")
    data = pings[0]["custom_ping"]
    got = [e for e in t.driver("custom_message") if e.get("capability") == ECHO and e.get("type") == "ping"]
    if not got or got[0].get("data") != data:
        return Result("fail", f"the application got {got[:1]}")
    pongs = [e for e in t.a2s() if (e.get("custom_message") or {}).get("type") == "pong"]
    if not pongs or pongs[0]["custom_message"] != {"capability": ECHO, "type": "pong", "data": data}:
        return Result("fail", f"the server got {[e['custom_message'] for e in pongs[:1]]}")
    return Result("pass")


@check("undeclared_custom_message_ignored", "MUST", ["RULE-CLIENT-10"])
def undeclared_custom_message_ignored(t: Trace) -> Result:
    """A custom message for a capability the agent didn't declare never reaches the application."""
    if not any(e.get("custom_other") for e in t.server("s2a")):
        return Result("fail", "the server sent no custom message for another capability")
    leaked = [e for e in t.driver("custom_message") if e.get("capability") != ECHO]
    if leaked:
        return Result("fail", f"the application got {leaked[0]}")
    return Result("pass")


@check("server_custom_capabilities_told", "SHOULD", ["RULE-CLIENT-10"])
def server_custom_capabilities_told(t: Trace) -> Result:
    """The application hears the server's custom capabilities, once while they don't change."""
    told = t.driver("server_custom_capabilities")
    if not told:
        return Result("fail", "the application never heard the server's custom capabilities")
    if len(told) > 1 and all(e.get("capabilities") == told[0].get("capabilities") for e in told):
        return Result("fail", f"told the same capabilities {len(told)} times")
    caps = told[0].get("capabilities")
    return Result("pass" if caps and ECHO in caps else "fail", f"{caps!r}")


# ------------------------------------------------------------------------------------------
# The instance UID across restarts (docs/instance-uid.md)

def package_offers(t: Trace) -> list[dict]:
    """The packages offers the server sent, in order: each with its aggregate hash and packages."""
    return [e for e in t.server("s2a") if e.get("packages_offer")]


def package_statuses(t: Trace) -> list[dict]:
    """The package statuses the agent reported, in order."""
    return [e["package_statuses"] for e in t.a2s() if e.get("package_statuses")]


INSTALLED, INSTALL_FAILED = "PackageStatusEnum_Installed", "PackageStatusEnum_InstallFailed"


@check("package_statuses_reported", "MUST", ["OPAMP-PKG-28", "OPAMP-PKG-29", "OPAMP-PKG-34"])
def package_statuses_reported(t: Trace) -> Result:
    """Every offer's aggregate hash comes back in the statuses, and the last statuses are about the
    last offer, with every package it offers."""
    offers, statuses = package_offers(t), package_statuses(t)
    if not offers:
        return Result("fail", "the server offered no packages")
    reported = {s.get("all") for s in statuses}
    for o in offers:
        if o["packages_offer"] not in reported:
            return Result("fail", f"offer {o['packages_offer'][:8]} never reported")
    last = statuses[-1]
    if last.get("all") != offers[-1]["packages_offer"]:
        return Result("fail", "the last statuses are not about the last offer")
    missing = sorted(set(offers[-1]["packages"]) - set(last.get("packages", {})))
    if missing:
        return Result("fail", f"not reported: {missing}")
    return Result("pass")


@check("packages_installed_verified", "MUST", ["RULE-CLIENT-11", "OPAMP-PKG-16", "OPAMP-PKG-18", "OPAMP-PKG-19",
                                               "OPAMP-PKG-23"])
def packages_installed_verified(t: Trace) -> Result:
    """Every good package offered reaches the application with its file (the SHA-256 the server
    served), and those of the last offer end up reported INSTALLED at the version offered."""
    offers, statuses = package_offers(t), package_statuses(t)
    if not offers:
        return Result("fail", "the server offered no packages")
    installs = t.driver("install_package")
    for o in offers:
        for name, p in o["packages"].items():
            if p["corrupt"]:
                continue
            if not any(e.get("name") == name and e.get("version") == p["version"] and e.get("sha256") == p["sha256"]
                       for e in installs):
                return Result("fail", f"{name} {p['version']} never reached the application with its file")
    last = statuses[-1].get("packages", {}) if statuses else {}
    for name, p in offers[-1]["packages"].items():
        got = last.get(name, {})
        if not p["corrupt"] and (got.get("status") != INSTALLED or got.get("has_version") != p["version"]):
            return Result("fail", f"{name} last reported as {got}")
    return Result("pass")


@check("corrupt_package_refused", "MUST", ["RULE-CLIENT-11"])
def corrupt_package_refused(t: Trace) -> Result:
    """A package whose file doesn't match its content hash never reaches the application, and is
    reported INSTALL_FAILED."""
    corrupt = {(n, p["version"]) for o in package_offers(t) for n, p in o["packages"].items() if p["corrupt"]}
    if not corrupt:
        return Result("fail", "the server offered no corrupt package")
    leaked = [e for e in t.driver("install_package") if (e.get("name"), e.get("version")) in corrupt]
    if leaked:
        return Result("fail", f"the application got {leaked[0]}")
    names = {n for n, _ in corrupt}
    if not any(s.get("packages", {}).get(n, {}).get("status") == INSTALL_FAILED
               for s in package_statuses(t) for n in names):
        return Result("fail", "never reported INSTALL_FAILED")
    return Result("pass")


@check("unoffered_package_removed", "SHOULD", ["OPAMP-PKG-20"])
def unoffered_package_removed(t: Trace) -> Result:
    """A package the agent installed that the server no longer offers is handed over for removal,
    and no longer reported."""
    offers, statuses = package_offers(t), package_statuses(t)
    if not offers:
        return Result("fail", "the server offered no packages")
    gone = {e.get("name") for e in t.driver("install_package")} - set(offers[-1]["packages"])
    if not gone:
        return Result("fail", "no package the agent installed was dropped")
    kept = gone - {e.get("name") for e in t.driver("remove_package")}
    if kept:
        return Result("fail", f"not removed: {sorted(kept)}")
    still = gone & set(statuses[-1].get("packages", {}) if statuses else {})
    if still:
        return Result("fail", f"still reported: {sorted(still)}")
    return Result("pass")


@check("package_download_credentials", "SHOULD", ["OPAMP-PKG-24", "OPAMP-PKG-25"])
def package_download_credentials(t: Trace) -> Result:
    """Every package file is fetched with an HTTP GET carrying the headers the offer gave."""
    downloads = t.server("package_download")
    if not downloads:
        return Result("fail", "no package was downloaded")
    bad = [d for d in downloads if d.get("authorization") != "Bearer packages"]
    if bad:
        return Result("fail", f"{bad[0]['path']} without the offered credentials")
    return Result("pass", f"{len(downloads)} downloads")


@check("instance_uid_kept_across_restart", "MUST", ["RULE-CLIENT-06", "OPAMP-UID-05"])
def instance_uid_kept_across_restart(t: Trace) -> Result:
    """After a restart the client goes on with the UID it had (made up, or assigned by the server)."""
    spans = runs(t)
    if len(spans) < 2:
        return Result("fail", "the driver ran once: no restart")
    per_run = [[e for e in t.a2s() if begin - CLOCK_SKEW_S <= e["wall"] < end - CLOCK_SKEW_S] for begin, end in spans]
    for i, (before, after) in enumerate(zip(per_run, per_run[1:])):
        if not before or not after:
            return Result("fail", f"run {i + 1 if not before else i + 2} sent nothing")
        # The UID the run ended with: the one it last sent, or one the server assigned in reply to
        # it (a run stopped before its next message still goes on with that).
        had = before[-1]["uid"]
        _, end = spans[i]
        assigned = [e["assign_uid"] for e in t.server("s2a")
                    if e.get("assign_uid") and before[-1]["wall"] <= e["wall"] < end - CLOCK_SKEW_S]
        if assigned:
            had = assigned[-1]
        if after[0]["uid"] != had:
            return Result("fail", f"run {i + 2} started as {after[0]['uid']}, not {had}")
    return Result("pass", f"kept {per_run[-1][0]['uid']}")

# ------------------------------------------------------------------------------------------
# Asking the server for an instance UID (docs/instance-uid.md)

REQUEST_INSTANCE_UID = 0x1


def asks(e: dict) -> bool:
    return bool(e.get("flags", 0) & REQUEST_INSTANCE_UID)


@check("requests_uid_until_assigned", "MUST", ["OPAMP-UID-04"])
def requests_uid_until_assigned(t: Trace) -> Result:
    """Every message asks for a UID (RequestInstanceUid) until the server has assigned one, and
    none after, restarts included. The goodbye may leave the flag off. A message asks only once
    every message before it on its connection has been answered: a server that honours the flag
    would assign a UID to each."""
    msgs = sorted(t.a2s(), key=lambda e: e["msg"])
    if not msgs or not asks(msgs[0]):
        return Result("fail", "the first message doesn't ask for a UID")
    assigned = sorted(e["msg"] for e in responses(t, "assign_uid"))
    replies = t.server("s2a")
    for e in msgs:
        answered = any(n < e["msg"] for n in assigned)
        waiting = [m["msg"] for m in msgs if m["msg"] < e["msg"] and m["conn"] == e["conn"] and asks(m)
                   and not any(r["msg"] == m["msg"] and r["wall"] <= e["wall"] for r in replies)]
        if asks(e) and waiting:
            return Result("fail", f"message {e['msg']} asks for a UID while message {waiting[0]} is unanswered")
        if asks(e) and answered:
            return Result("fail", f"message {e['msg']} asks for a UID after one was assigned")
        if not asks(e) and not answered and not e["disconnect"]:
            return Result("fail", f"message {e['msg']} doesn't ask for a UID, and none was assigned")
    if not assigned:
        return Result("pass", f"never assigned: all {len(msgs)} messages asked")
    return Result("pass", f"{sum(asks(e) for e in msgs)} message(s) asked")


@check("uid_assigned_once", "MUST", ["OPAMP-UID-04"])
def uid_assigned_once(t: Trace) -> Result:
    """A server that assigns a new UID to every message that asks assigns the client one: the
    client never has a second message asking in flight (which would leave an agent on the server
    that nobody is), and never asks again once it has one."""
    assigned = responses(t, "assign_uid")
    if not assigned:
        return Result("fail", "the server assigned no UID: nothing to check")
    if len(assigned) > 1:
        return Result("fail", f"the server assigned {len(assigned)} UIDs, for messages {[e['msg'] for e in assigned]}")
    return Result("pass", assigned[0]["assign_uid"])


@check("uid_not_requested_after_restart", "MUST", ["OPAMP-UID-04", "RULE-CLIENT-06"])
def uid_not_requested_after_restart(t: Trace) -> Result:
    """Once the server has assigned a UID, a restart goes on with it, without asking again."""
    spans = runs(t)
    if len(spans) < 2:
        return Result("fail", "the driver ran once: no restart")
    assigned = responses(t, "assign_uid")
    if not assigned:
        return Result("fail", "the server assigned no UID before the restart")
    uid = assigned[0]["assign_uid"]
    begin = spans[1][0]
    after = [e for e in t.a2s() if e["wall"] >= begin - CLOCK_SKEW_S]
    if not after:
        return Result("fail", "nothing sent after the restart")
    if after[0]["uid"] != uid:
        return Result("fail", f"after the restart the client is {after[0]['uid']}, not {uid}")
    again = [e for e in after if asks(e)]
    if again:
        return Result("fail", f"message {again[0]['msg']} asks for a UID again after the restart")
    return Result("pass", f"{len(after)} message(s) as {uid}")

# ------------------------------------------------------------------------------------------
# HTTP as the core says (docs/how-it-works.md)

CORE_USER_AGENT = "opamp-clients"


@check("redirects_not_followed", "MUST", ["RULE-CLIENT-04"])
def redirects_not_followed(t: Trace) -> Result:
    """A 3xx is an error to retry: the client never goes where it points, and comes back after."""
    redirects = [e for e in t.server("request_rejected") if 300 <= e["status"] < 400]
    if not redirects:
        return Result("fail", "the server redirected nothing: nothing to check")
    stray = t.server("stray_request")
    if stray:
        return Result("fail", f"the client followed a redirect to {stray[0]['path']}")
    last = redirects[-1]["wall"]
    back = [e for e in t.a2s() + t.server("ws_open") if e["wall"] > last]
    if not back:
        return Result("fail", "the client never came back after the redirects")
    return Result("pass", f"{len(redirects)} redirect(s) not followed")


@check("sends_core_headers", "MUST", ["RULE-CLIENT-05"])
def sends_core_headers(t: Trace) -> Result:
    """Every request and upgrade carries the core's User-Agent, requests ask for no compression,
    and no upgrade offers a compression extension."""
    requests = (t.a2s() if t.transport == "http" else []) + t.server("request_rejected")
    upgrades = t.server("ws_open")
    if not requests and not upgrades:
        return Result("fail", "no requests")
    for e in requests + upgrades:
        if e.get("user_agent") != CORE_USER_AGENT:
            return Result("fail", f"User-Agent {e.get('user_agent')!r}, not {CORE_USER_AGENT!r}")
    for e in requests:
        if "accept_encoding" in e and e["accept_encoding"] != "identity":
            return Result("fail", f"Accept-Encoding {e['accept_encoding']!r}, not 'identity'")
    for e in upgrades:
        if e.get("extensions"):
            return Result("fail", f"the upgrade offers {e['extensions']!r}")
    return Result("pass", f"{len(requests)} request(s), {len(upgrades)} upgrade(s)")

# ------------------------------------------------------------------------------------------
# A hostile server (conformance/server/hostile.go): malformed values the client must refuse

def hostile_responses(t: Trace) -> list[dict]:
    """Responses that carried something hostile: a connection-settings offer, or packages."""
    return [e for e in t.server("s2a") if e.get("hostile_offer")
            or any(p.get("hostile") for p in (e.get("packages") or {}).values())]


def clean_stop(t: Trace) -> float:
    """When the driver was told to stop, if the client was still running then; otherwise (it died,
    or stopped on its own) never: nothing it was sent is excused for the run ending."""
    stop = t.stop_wall()
    if stop is None or t.driver_exit != 0 or any(e["wall"] <= stop for e in t.driver("fatal")):
        return float("inf")
    return stop


def first_settings_offers(t: Trace, malformed: bool) -> list[dict]:
    """The first response carrying each hostile connection-settings offer (malformed or not)."""
    seen: dict[str, dict] = {}
    for e in t.server("s2a"):
        o = e.get("hostile_offer")
        if o and bool(o["malformed"]) == malformed:
            seen.setdefault(o["hash"], e)
    return list(seen.values())


@check("malformed_settings_failed", "MUST", ["RULE-CLIENT-12", "OPAMP-CONNSET-04"])
def malformed_settings_failed(t: Trace) -> Result:
    """Every malformed connection-settings offer (an endpoint with whitespace, no host, a scheme
    no client speaks or past 8000 octets; a header whose name isn't a token or whose value holds CR,
    LF or NUL) is reported FAILED soon after it came, and never APPLIED. An offer the run ended
    too soon after to judge is left out."""
    offers = first_settings_offers(t, malformed=True)
    if not offers:
        return Result("fail", "the server offered no malformed connection settings")
    within = (t.scenario.get("server", {}).get("hostile") or {}).get("answer_within_s", 15)
    stop = clean_stop(t)
    judged = 0
    for e in offers:
        o = e["hostile_offer"]
        about = [m for m in t.a2s() if m.get("connection_settings_status", {}).get("hash") == o["hash"]]
        said = [m["connection_settings_status"]["status"].rsplit("_", 1)[-1] for m in about]
        if "APPLIED" in said:
            return Result("fail", f"{o['kind']} offer {o['endpoint']} reported APPLIED")
        failed = [m for m in about if m["connection_settings_status"]["status"].endswith("FAILED")]
        if failed and failed[0]["wall"] <= e["wall"] + within:
            judged += 1
            continue
        if not failed and e["wall"] + within > stop:
            continue
        return Result("fail", f"{o['kind']} offer (endpoint {o['endpoint']}, headers {o['headers']}): "
                              + (f"FAILED only {failed[0]['wall'] - e['wall']:.1f}s after it came" if failed else
                                 f"no FAILED within {within}s ({'last ' + said[-1] if said else 'never answered'})"))
    if not judged:
        return Result("fail", "the run ended before any malformed offer could be judged")
    return Result("pass", f"{judged} malformed offer(s) FAILED")


@check("malformed_packages_failed", "MUST", ["RULE-CLIENT-12", "RULE-CLIENT-11", "OPAMP-PKG-28"])
def malformed_packages_failed(t: Trace) -> Result:
    """Every package whose download URL (another scheme, relative, whitespace, no host) or headers
    (a name that isn't a token, a value with CR, LF or NUL) are malformed is reported
    INSTALL_FAILED, and the well-formed one offered with them INSTALLED: taken in name order, each
    within 35 s of the one before (three attempts, 10 s apart, and some slack). A flow a bad package
    wedges never gets to the next."""
    offers = [o for o in package_offers(t) if any(p.get("hostile") for p in o["packages"].values())]
    if not offers:
        return Result("fail", "the server offered no malformed package")
    offer = offers[0]
    bound = t.params.get("package_settle_s", 35)
    stop = clean_stop(t)
    statuses = [(e["wall"], e["package_statuses"]) for e in t.a2s() if e.get("package_statuses")]
    prev = offer["wall"]
    for name in sorted(offer["packages"]):
        p = offer["packages"][name]
        want = INSTALL_FAILED if p.get("hostile") else INSTALLED
        what = f"{name} ({p['url']}, headers {p['headers']})" if p.get("hostile") else name
        settled = [(w, st["packages"][name]) for w, st in statuses
                   if st.get("packages", {}).get(name, {}).get("status") in (INSTALLED, INSTALL_FAILED)]
        if not settled:
            last = [st["packages"][name]["status"] for _, st in statuses if name in st.get("packages", {})]
            why = "the run ended first" if prev + bound > stop else f"not within {bound}s of the one before"
            return Result("fail", f"{what} never reported installed or failed ({why}; "
                                  f"{'last ' + last[-1] if last else 'never reported'})")
        wall, got = settled[0]
        if got["status"] != want:
            return Result("fail", f"{what} reported {got['status']}: {got.get('error', '')}")
        if wall > prev + bound:
            return Result("fail", f"{what} settled {wall - prev:.1f}s after the one before, past {bound}s")
        prev = wall
    return Result("pass", f"{len(offer['packages'])} packages settled in {prev - offer['wall']:.1f}s")


@check("no_request_to_malformed", "MUST", ["RULE-CLIENT-12"])
def no_request_to_malformed(t: Trace) -> Result:
    """No request is ever made with a malformed value: nothing reaches the endpoints or URLs the
    hostile server offers (a port of their own, and /packages/h0st1le/ for a relative URL), and no
    request carries a header a malformed value smuggled in."""
    if not hostile_responses(t):
        return Result("fail", "the server sent nothing malformed: nothing to check")
    # A connection on which nothing was sent (the client gave up writing the request) is no request.
    hits = [h for h in t.server("hostile_connect", "hostile_request")
            if h["event"] == "hostile_request" or h.get("bytes", 1) and h.get("first_line") != '""']
    if hits:
        h = hits[0]
        return Result("fail", f"{len(hits)} request(s) made with malformed values, first "
                              + (f"to the hostile port: {h.get('first_line')}"
                                 + (", with a header injected" if h.get("injected") else "")
                                 if h["event"] == "hostile_connect"
                                 else f"{h.get('method')} {h.get('path')}"))
    return Result("pass")


@check("keeps_reporting", "MUST", ["RULE-CLIENT-12", "OPAMP-S2A-01", "OPAMP-STATUS-02"])
def keeps_reporting(t: Trace) -> Result:
    """After the first hostile value the client goes on: another message follows every hostile
    response (one the run outlasted by 5 s), every status change the driver makes is reported
    within 5 s, and no second holds more than 20 messages."""
    hostile = hostile_responses(t)
    if not hostile:
        return Result("fail", "the server sent nothing hostile: nothing to check")
    within = 5.0
    stop = clean_stop(t)
    msgs = [m for m in t.a2s() if not m["disconnect"]]
    for h in hostile:
        if h["wall"] + within < stop and not any(m["wall"] > h["wall"] for m in msgs):
            what = h.get("hostile_offer", {}).get("kind", "packages")
            return Result("fail", f"nothing sent after the response to message {h['msg']} ({what})")
    first = hostile[0]["wall"]
    changes = [c for c in t.driver("status_change") if c["wall"] > first]
    for c in changes:
        if not any(c["wall"] - CLOCK_SKEW_S <= m["wall"] <= c["wall"] + within and "agent_description" in m["fields"]
                   for m in msgs):
            return Result("fail", f"status change {c['change']}, {c['wall'] - first:.1f}s after the first hostile "
                                  f"value, not reported within {within:g}s")
    walls = [m["wall"] for m in msgs if m["wall"] > first]
    for i, w in enumerate(walls):
        if i + 20 < len(walls) and walls[i + 20] - w < 1.0:
            return Result("fail", f"21 messages within a second, {w - first:.1f}s after the first hostile value")
    return Result("pass", f"{len(walls)} message(s), {len(changes)} status change(s) after the first hostile value")


# ------------------------------------------------------------------------------------------
# Requests exactly as the core gives them (docs/how-it-works.md: no extra headers, no cookies, no
# decompression)

CORE_POST = ["Content-Type", "OpAMP-Instance-UID", "User-Agent", "Accept-Encoding"]
CORE_UPGRADE = ["OpAMP-Instance-UID", "User-Agent"]
# What HTTP needs to carry a request (RFC 9112: Host, and the body's framing), and a WebSocket
# upgrade (RFC 6455 section 4.1: the client MUST send these; Origin, Sec-WebSocket-Protocol and
# Sec-WebSocket-Extensions are optional, and the core asks for none of them).
STACK_HTTP = {"host", "content-length", "transfer-encoding"}
STACK_UPGRADE = {"host", "upgrade", "connection", "sec-websocket-key", "sec-websocket-version"}
STACK_DOWNLOAD = {"host"}


def raw_requests(t: Trace) -> list[tuple[str, dict, list[str], set[str]]]:
    """Every request whose head the server kept: (what it was, the event, the headers the core
    names for it, what the HTTP stack may add)."""
    app = ["Authorization"] if t.scenario.get("driver", {}).get("token") else []
    out = []
    for e in t.server("a2s", "ws_open", "request_rejected", "package_download"):
        if e.get("headers") is None:
            continue
        if e["event"] == "ws_open" or (e["event"] == "request_rejected" and e.get("upgrade")):
            out.append(("upgrade", e, CORE_UPGRADE + app, STACK_UPGRADE))
        elif e["event"] == "package_download":
            out.append(("download", e, list(e.get("offered_headers", [])), STACK_DOWNLOAD))
        else:
            out.append(("POST", e, CORE_POST + app, STACK_HTTP))
    return out


def count(reqs: list) -> str:
    kinds = [k for k, *_ in reqs]
    return ", ".join(f"{kinds.count(k)} {k}" for k in sorted(set(kinds)))


@check("headers_as_given", "MUST", ["RULE-CLIENT-13"])
def headers_as_given(t: Trace) -> Result:
    """Every request, upgrade and download carries the headers the core names for it (for a
    download, those the offer gives), each once, and otherwise only what HTTP needs to carry it:
    Host, Content-Length or Transfer-Encoding for a POST, and for an upgrade Upgrade, Connection,
    Sec-WebSocket-Key and Sec-WebSocket-Version. (A Cookie is left to no_cookies.)"""
    reqs = raw_requests(t)
    if not reqs:
        return Result("fail", "no request heads were kept")
    for kind, e, core_names, stack in reqs:
        # A Cookie is no_cookies' to judge.
        allowed = {n.lower() for n in core_names} | stack | {"cookie"}
        names = [n for n, _ in e["headers"]]
        extra = [n for n in names if n.lower() not in allowed]
        if extra:
            return Result("fail", f"{kind} carries {extra} (the core asked for {core_names})")
        lower = [n.lower() for n in names]
        twice = sorted({n for n in names if lower.count(n.lower()) > 1})
        if twice:
            return Result("fail", f"{kind} carries {twice} more than once")
    return Result("pass", count(reqs))


@check("header_case_kept", "SHOULD", ["RULE-CLIENT-13"])
def header_case_kept(t: Trace) -> Result:
    """Every header the core names goes out spelled as the core spells it (OpAMP-Instance-UID).
    SHOULD: HTTP header names are case-insensitive (RFC 9110 section 5.1), so no conforming server
    can tell, but how-it-works.md asks for the request exactly as given."""
    reqs = raw_requests(t)
    if not reqs:
        return Result("fail", "no request heads were kept")
    for kind, e, core_names, _ in reqs:
        spelled = {n.lower(): n for n in core_names}
        for n, _ in e["headers"]:
            want = spelled.get(n.lower())
            if want and n != want:
                return Result("fail", f"{kind} sends {n!r}, the core names {want!r}")
    return Result("pass", count(reqs))


@check("no_cookies", "MUST", ["RULE-CLIENT-13"])
def no_cookies(t: Trace) -> Result:
    """The client keeps no cookies: after the server has set one (Set-Cookie on every response),
    no request, upgrade or download sends it back."""
    if not t.scenario.get("server", {}).get("cookie"):
        return Result("fail", "the server set no cookie: nothing to check")
    answered = [e["wall"] for e in t.server("s2a", "ws_open", "package_download")]
    if not answered:
        return Result("fail", "no response set the cookie")
    later = [(k, e) for k, e, *_ in raw_requests(t) if e["wall"] > answered[0]]
    if not later:
        return Result("skip", "no request after the cookie was set (the client never reconnected)")
    for kind, e in later:
        sent = [v for n, v in e["headers"] if n.lower() == "cookie"]
        if sent:
            return Result("fail", f"{kind} sends Cookie: {sent[0]}")
    return Result("pass", f"{len(later)} request(s) after the cookie, none with it")


@check("no_decompression", "MUST", ["RULE-CLIENT-14"])
def no_decompression(t: Trace) -> Result:
    """A package file served with Content-Encoding: gzip reaches the application as sent: its
    SHA-256 is that of the bytes on the wire, which is what the offer's content hash names."""
    gz = {(n, p["version"]): p for o in package_offers(t) for n, p in o["packages"].items() if p.get("gzip")}
    if not gz:
        return Result("fail", "the server served no gzipped file: nothing to check")
    installs = t.driver("install_package")
    for (name, version), p in sorted(gz.items()):
        got = [e for e in installs if e.get("name") == name and e.get("version") == version]
        if any(e.get("sha256") == p["sha256"] for e in got):
            continue
        if got:
            return Result("fail", f"{name}: the application got a file with SHA-256 {got[0].get('sha256', '')[:12]}, "
                                  f"not {p['sha256'][:12]} as sent")
        last = [s["packages"][name] for s in package_statuses(t) if name in s.get("packages", {})]
        return Result("fail", f"{name} never reached the application"
                              + (f" ({last[-1]['status']}: {last[-1].get('error', '')})" if last else ""))
    return Result("pass", f"{len(gz)} gzipped file(s) kept as sent")

# ------------------------------------------------------------------------------------------
# Package downloads behind redirects (conformance/server/redirect.go; docs/packages.md,
# "Redirects"). Each hop of a chain is logged as a package_download with its package, its hop (0
# for the offer's URL), the listener it came to and the one of the file's own origin. Every target
# a client must refuse is a port that answers nothing (redirect_trap_connect).

MAX_REDIRECTS = 5
TRAP_PATH = re.compile(r"/redirect/([^/\s\"]+)/([^/\s\"]+)/(\d+)")


def redirect_packages(t: Trace) -> dict[str, dict]:
    """The packages offered behind redirects, by name, as the server described them."""
    return {n: p for o in package_offers(t) for n, p in o["packages"].items() if p.get("redirect")}


def hops_requested(t: Trace, name: str) -> list[dict]:
    return [d for d in t.server("package_download") if d.get("package") == name]


def trapped(t: Trace) -> list[tuple[str | None, dict]]:
    """Every connection to the trap, with the package its request names (if it got that far)."""
    out = []
    for e in t.server("redirect_trap_connect"):
        m = TRAP_PATH.search(e.get("first_line", ""))
        out.append((m.group(1) if m else None, e))
    return out


def last_status(t: Trace, name: str) -> dict:
    got = [s["packages"][name] for s in package_statuses(t) if name in s.get("packages", {})]
    return got[-1] if got else {}


@check("redirects_followed", "MUST", ["RULE-CLIENT-17", "RULE-CLIENT-11", "OPAMP-PKG-23"])
def redirects_followed(t: Trace) -> Result:
    """Every package behind redirects the client may follow (at most 5; 301, 302, 303, 307 or 308;
    never from https to http; to a well-formed URL, relative or absolute, at any origin) is
    followed hop by hop to the end of its chain, reaches the application with the file served
    there (its SHA-256), and is last reported INSTALLED at the version offered."""
    allowed = {n: p for n, p in redirect_packages(t).items() if not p["redirect"]["refused"]}
    if not allowed:
        return Result("fail", "no package was offered behind redirects to follow")
    installs = t.driver("install_package")
    for name, p in sorted(allowed.items()):
        hops = len(p["redirect"]["hops"])
        asked = {d.get("hop") for d in hops_requested(t, name)}
        missing = [h for h in range(hops + 1) if h not in asked]
        last = last_status(t, name)
        why = f" (last {last.get('status')}: {last.get('error', '')})" if last else " (never reported)"
        if missing:
            return Result("fail", f"{name}: hop {missing[0]} of its {hops} redirect(s) never requested{why}")
        if not any(e.get("name") == name and e.get("version") == p["version"] and e.get("sha256") == p["sha256"]
                   for e in installs):
            return Result("fail", f"{name} {p['version']} never reached the application with its file{why}")
        if last.get("status") != INSTALLED or last.get("has_version") != p["version"]:
            return Result("fail", f"{name} last reported as {last}")
    return Result("pass", f"{len(allowed)} package(s) behind redirects installed")


@check("refused_redirects_failed", "MUST", ["RULE-CLIENT-17", "RULE-CLIENT-12", "OPAMP-PKG-28"])
def refused_redirects_failed(t: Trace) -> Result:
    """Every package whose chain ends in a redirect the client must refuse (a sixth, from https to
    http, to a URL the client wouldn't take from the server) fails there: the client gets that
    redirect, never requests its target (nothing reaches the trap), never hands the package to the
    application, and reports it INSTALL_FAILED without trying it again (the refusal is final)."""
    refused = {n: p for n, p in redirect_packages(t).items() if p["redirect"]["refused"]}
    if not refused:
        return Result("fail", "no package was offered behind a redirect to refuse")
    for name, e in trapped(t):
        if name is None or name in refused:
            return Result("fail", f"{name or 'a download'}: the client followed a redirect it must refuse "
                                  f"({e.get('first_line')})")
    leaked = [e for e in t.driver("install_package") if e.get("name") in refused]
    if leaked:
        return Result("fail", f"the application got {leaked[0].get('name')}")
    for name, p in sorted(refused.items()):
        hops = p["redirect"]["hops"]
        reqs = hops_requested(t, name)
        got = [d for d in reqs if d.get("hop") == len(hops) - 1 and 300 <= d.get("status", 0) < 400]
        if not got:
            return Result("fail", f"{name}: the client never got the redirect to refuse (hop {len(hops)} of "
                                  f"{hops}; requested hops {sorted({d.get('hop') for d in reqs})})")
        if not any(s.get("packages", {}).get(name, {}).get("status") == INSTALL_FAILED for s in package_statuses(t)):
            last = last_status(t, name)
            return Result("fail", f"{name} ({hops[-1]}) never reported INSTALL_FAILED"
                                  + (f" (last {last.get('status')})" if last else ""))
        again = [d for d in reqs if d.get("hop") == 0]
        if len(again) > 1:
            return Result("fail", f"{name} ({hops[-1]}): downloaded {len(again)} times; the refusal is final")
    return Result("pass", f"{len(refused)} refused redirect(s) failed their package")


@check("redirect_headers_same_origin", "MUST", ["RULE-CLIENT-17", "OPAMP-PKG-25"])
def redirect_headers_same_origin(t: Trace) -> Result:
    """The offer's headers (its Authorization) go only to the file's own origin: no request a
    redirect sent to another origin (another port) carries them, and every request to the file's
    origin does, back there after another origin too."""
    reqs = [d for d in t.server("package_download") if d.get("listener")]
    away = [d for d in reqs if d["listener"] != d.get("file_listener")]
    back = [d for d in reqs if d["listener"] == d.get("file_listener") and d.get("hop", 0) > 0]
    if not away or not back:
        return Result("fail", "no redirect to another origin and one to the file's own: nothing to check")
    for d in away:
        if d.get("authorization"):
            return Result("fail", f"{d.get('package')} hop {d.get('hop')}, at the {d['listener']} origin (the file's "
                                  f"is {d.get('file_listener')}), carries the offer's Authorization")
    for d in reqs:
        if d["listener"] == d.get("file_listener") and d.get("authorization") != "Bearer packages":
            return Result("fail", f"{d.get('package')} hop {d.get('hop')}, at the file's own origin "
                                  f"({d['listener']}), without the offer's Authorization")
    return Result("pass", f"{len(away)} request(s) elsewhere without the headers, {len(reqs) - len(away)} at home with them")


@check("redirect_hops_bounded", "MUST", ["RULE-CLIENT-17"])
def redirect_hops_bounded(t: Trace) -> Result:
    """A download follows at most 5 redirects, and no fewer when they are allowed: nothing past the
    5th is requested (nor reaches the trap), and a chain of 5 or more is followed as far as the 5th
    redirect's target."""
    long = {n: p for n, p in redirect_packages(t).items() if len(p["redirect"]["hops"]) >= MAX_REDIRECTS}
    if not long:
        return Result("fail", f"no chain of {MAX_REDIRECTS} redirects or more was offered: nothing to check")
    past = [d for d in t.server("package_download") if d.get("hop", 0) > MAX_REDIRECTS]
    if past:
        return Result("fail", f"{past[0].get('package')}: hop {past[0]['hop']} requested")
    for name, e in trapped(t):
        if name in long:
            return Result("fail", f"{name}: past {MAX_REDIRECTS} redirects ({e.get('first_line')})")
    for name, p in sorted(long.items()):
        asked = {d.get("hop") for d in hops_requested(t, name)}
        if MAX_REDIRECTS not in asked:
            return Result("fail", f"{name}: stopped before its {MAX_REDIRECTS}th redirect (requested hops "
                                  f"{sorted(h for h in asked if h is not None)})")
    return Result("pass", f"{len(long)} chain(s) of {MAX_REDIRECTS} or more, none followed further")


# ------------------------------------------------------------------------------------------
# The Rust server (run with --server rust, where it is built). Its `core` events say what it decided.

def core(t: Trace, *kinds: str) -> list[dict]:
    return [e for e in t.server("core") if not kinds or e["kind"] in kinds]


def epochs(t: Trace) -> list[tuple[dict, int]]:
    """Each response with the number of the connection it belongs to, as the server sees
    connections: a new one starts at each hello (core `connected`) and ends at each goodbye."""
    out, epoch = [], 0
    for e in t.server("s2a", "core"):
        if e["event"] == "core" and e["kind"] in ("connected", "disconnected"):
            epoch += 1
        elif e["event"] == "s2a":
            out.append((e, epoch))
    return out


@check("server_offers_config_once_per_connection", "MUST", ["RULE-CONFIG-01"], subject="server")
def server_offers_config_once_per_connection(t: Trace) -> Result:
    """The server offers a config at most once per connection: every offer makes a supervisor
    validate it and restart. Again only if the client, without ever naming the config in a
    status, sent its description in between: it resynchronised, and may have missed the offer."""
    last: dict[tuple[int, str], int] = {}
    offers = 0
    for e, epoch in epochs(t):
        if not e.get("offer_config"):
            continue
        offers += 1
        key = (epoch, e["offer_config"])
        if key in last:
            between = [m for m in t.a2s() if last[key] < m["msg"] <= e["msg"]]
            named = any(m.get("remote_config_status", {}).get("hash") == e["offer_config"] for m in between)
            resync = any("agent_description" in m["fields"] for m in between)
            if named or not resync:
                return Result("fail", f"config {e['offer_config'][:12]} offered again on the same connection "
                                      f"(message {e['msg']})")
        last[key] = e["msg"]
    return Result("pass", f"{offers} offer(s)") if offers else Result("skip", "no config offered")


@check("server_records_applied_config", "MUST", ["RULE-CONFIG-03"], subject="server")
def server_records_applied_config(t: Trace) -> Result:
    """When the client reports the offered config APPLIED, the server records it as the config the
    agent runs. (Not one the server turned away before taking it in: it never had it.)"""
    applied = [e for e in t.a2s() if e.get("remote_config_status", {}).get("status") == "RemoteConfigStatuses_APPLIED"
               and e["remote_config_status"]["hash"]]
    if not applied:
        return Result("skip", "the client reported no config applied")
    recorded = {e["hash"] for e in core(t, "config_applied")}
    missing = [e for e in applied if e["remote_config_status"]["hash"] not in recorded and not refused(e)]
    if missing:
        return Result("fail", f"message {missing[0]['msg']} reports {missing[0]['remote_config_status']['hash'][:12]} "
                              f"applied; the server never recorded it")
    turned_away = sum(refused(e) for e in applied)
    return Result("pass", f"{turned_away} turned away before the server took it in" if turned_away else "")


@check("server_assigns_v7_uids", "MUST", ["OPAMP-SRV-05"], subject="server")
def server_assigns_v7_uids(t: Trace) -> Result:
    """Every instance UID the server assigns is a UUID v7 (RFC 9562): some clients crash on others."""
    assigned = [e["assign_uid"] for e in t.server("s2a") if e.get("assign_uid")]
    if not assigned:
        return Result("skip", "no UID assigned")
    for uid in assigned:
        u = uuid.UUID(hex=uid)
        if u.version != 7 or u.variant != uuid.RFC_4122:
            return Result("fail", f"assigned {uid}: version {u.version}, variant {u.variant}")
    return Result("pass", f"{len(assigned)} assigned")


@check("server_sees_goodbye", "MUST", ["RULE-CONN-02", "OPAMP-SRV-01"], subject="server")
def server_sees_goodbye(t: Trace) -> Result:
    """The server records the agent as disconnected when it says goodbye or its connection closes."""
    if not core(t):
        return Result("skip", "not a server that reports its decisions")
    if not t.driver("stopped"):
        return Result("skip", "the driver did not stop cleanly")
    events = core(t, "connected", "disconnected")
    if not events:
        return Result("fail", "the server recorded no connection")
    if events[-1]["kind"] != "disconnected":
        if t.transport == "http" and not t.a2s()[-1]["disconnect"]:
            return Result("skip", "no goodbye over plain HTTP, and no connection to lose")
        return Result("fail", "the agent is still connected after it stopped")
    return Result("pass")


# ------------------------------------------------------------------------------------------
# TLS and proxies (the Go server: conformance/server/tls.go, proxy.go)

# The common name of the client certificate the runner hands to drivers (tls.go: ClientSubject).
CLIENT_SUBJECT = "conformance-agent"


@check("connected_over_tls", "MUST", ["OPAMP-CONNSET-15", "RULE-TLS-01"])
def connected_over_tls(t: Trace) -> Result:
    """Given an https:// or wss:// endpoint and the CA that signed the server's certificate, the
    client completes a TLS handshake and every message arrives over TLS 1.2 or later. A handshake
    the client aborts with an alert means it refused a certificate it was told to trust."""
    handshakes = t.server("tls_handshake")
    if not handshakes:
        return Result("fail", "no TLS handshake completed")
    refused = [e for e in t.server("tls_handshake_failed") if e.get("alert")]
    if refused:
        return Result("fail", f"the client refused a handshake: {refused[0]['alert']}")
    msgs = t.a2s()
    if not msgs:
        return Result("fail", "no messages")
    for e in msgs:
        if e.get("tls") not in ("TLS 1.2", "TLS 1.3"):
            return Result("fail", f"message {e['msg']} arrived over {e.get('tls') or 'no TLS'}")
    return Result("pass", f"{len(handshakes)} handshake(s), {sorted({e['tls'] for e in msgs})}")


@check("rejects_untrusted_server", "MUST", ["OPAMP-CONNSET-15", "RULE-TLS-01"])
def rejects_untrusted_server(t: Trace) -> Result:
    """A client given a CA that did not sign the server's certificate sends that server nothing:
    no message, and not a byte of application data on any connection whose handshake completed
    (a client may finish the handshake before checking the certificate, then hang up). A completed
    handshake whose close the server never logged could have carried anything, and fails."""
    handshakes = t.server("tls_handshake")
    failed = t.server("tls_handshake_failed")
    if not handshakes and not failed:
        return Result("fail", "the client attempted no TLS handshake")
    if t.a2s():
        return Result("fail", f"the client sent {len(t.a2s())} message(s) to a server it cannot verify")
    closed = {e.get("tcp"): e for e in t.server("tls_closed")}
    for h in handshakes:
        c = closed.get(h.get("tcp"))
        if c is None:
            return Result("fail", f"handshake {h.get('tcp')} completed and its close was never logged: "
                                  "what the client sent on it is unknown")
        if c.get("app_bytes", 0) > 0:
            return Result("fail", f"the client sent {c['app_bytes']} byte(s) on connection {h.get('tcp')} "
                                  "to a server it cannot verify")
    alerts = sorted({e["alert"] for e in failed if e.get("alert")})
    return Result("pass", f"{len(failed)} handshake(s) refused, {len(handshakes)} completed and closed with "
                          "nothing sent" + (f", alerts {alerts}" if alerts else ""))


@check("refuses_in_handshake", "SHOULD", ["OPAMP-CONNSET-15", "RULE-TLS-01"])
def refuses_in_handshake(t: Trace) -> Result:
    """A client given a CA that did not sign the server's certificate refuses it during the TLS
    handshake (ideally with an alert), rather than finishing the handshake and checking the
    certificate after."""
    handshakes = t.server("tls_handshake")
    if handshakes:
        return Result("fail", f"the client finished {len(handshakes)} handshake(s) before checking the "
                              "server's certificate, then hung up")
    failed = t.server("tls_handshake_failed")
    if not failed:
        return Result("fail", "the client attempted no TLS handshake")
    alerts = sorted({e["alert"] for e in failed if e.get("alert")})
    return Result("pass", f"{len(failed)} handshake(s) refused" + (f", alerts {alerts}" if alerts else ", no alert"))


@check("presented_client_cert", "MUST", ["RULE-TLS-02"])
def presented_client_cert(t: Trace) -> Result:
    """Every TLS handshake presents the client certificate the driver was configured with (the
    server verifies it against its CA), and every message arrives on such a connection."""
    handshakes = t.server("tls_handshake")
    if not handshakes:
        failed = t.server("tls_handshake_failed")
        return Result("fail", "no TLS handshake completed" + (f": {failed[0]['error']}" if failed else ""))
    for e in handshakes:
        if not e.get("client_cert") or e.get("client_subject") != CLIENT_SUBJECT or not e.get("client_verified"):
            verified = "" if e.get("client_verified") or not e.get("client_cert") else " (not verified by the CA)"
            return Result("fail", f"handshake {e.get('tcp')} presented "
                                  f"{e.get('client_subject') or 'no client certificate'}{verified}")
    msgs = t.a2s()
    if not msgs:
        return Result("fail", "no messages")
    for e in msgs:
        if e.get("client_subject") != CLIENT_SUBJECT:
            return Result("fail", f"message {e['msg']} arrived with client certificate {e.get('client_subject')!r}")
    return Result("pass", f"{len(handshakes)} handshake(s)")


@check("went_through_proxy", "MUST", ["RULE-PROXY-01"])
def went_through_proxy(t: Trace) -> Result:
    """Every message reaches the server through the configured proxy: it arrives on a connection
    the proxy opened for a tunnel (or forwarded request) it logged, with the proxy's credentials."""
    tunnels = t.server("proxy_tunnel")
    if not tunnels:
        refused = t.server("proxy_refused")
        return Result("fail", "nothing went through the proxy"
                              + (f"; it refused {refused[0]['reason']}: {refused[0]['detail']}" if refused else ""))
    auth = (t.scenario.get("server", {}).get("proxy") or {}).get("auth")
    if auth:
        wrong = [e for e in tunnels if e.get("user") != auth["user"]]
        if wrong:
            return Result("fail", f"tunnel {wrong[0]['id']} opened as user {wrong[0].get('user')!r}")
    upstream = {e["upstream"] for e in tunnels}
    msgs = t.a2s()
    if not msgs:
        return Result("fail", "no messages")
    for e in msgs:
        if e.get("remote") not in upstream:
            return Result("fail", f"message {e['msg']} came from {e.get('remote')}, not through the proxy")
    return Result("pass", f"{len(tunnels)} tunnel(s): {sorted({e['via'] for e in tunnels})}")
