# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Tests for the checks: every check must pass at least one trace and fail at least one.

A check that cannot fail is as useless as a vacuous TLA+ property, so test_every_check_can_fail
fails when a check is added without a trace that breaks it.

    python3 -m unittest conformance/runner/test_checks.py
"""
from __future__ import annotations

import re
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from checks import CHECKS, TAP_CHECK, Trace  # noqa: E402

UID = "01928f3a7c3e7a1b8c2d3e4f5a6b7c8d"
UID2 = "01928f3a7c3e7fffbc2d3e4f5a6b7c8d"
UID3 = "01928f3a7c3e7eeebc2d3e4f5a6b7c8d"
FULL = ["agent_description", "health"]
OFFER_HASH = "aa"


INSTALLED, INSTALL_FAILED = "PackageStatusEnum_Installed", "PackageStatusEnum_InstallFailed"
# An offer as the server logs it: "a" is good (its file's SHA-256 is 5a), "b" corrupt.
PKGS = {"a": {"version": "1", "sha256": "5a", "corrupt": False},
        "b": {"version": "1", "sha256": "bb", "corrupt": True}}


def statuses(all_: str, **packages: tuple[str, str]) -> dict:
    """Package statuses as the server logs them: name=(has_version, status)."""
    return {"all": all_, "error": "",
            "packages": {n: {"status": st, "has_version": v} for n, (v, st) in packages.items()}}


class Log:
    """Builds a trace one event at a time, at increasing wall-clock times."""

    def __init__(self, transport: str = "http", exit_code: int | None = 0, **params):
        self.events: list[dict] = []
        self.transport = transport
        self.exit_code = exit_code
        self.params = params
        self.wall = 100.0
        self.msg = 0
        self.seq = -1
        self.server_cfg: dict = {}
        self.driver_cfg: dict = {}

    def at(self, wall: float) -> "Log":
        self.wall = wall
        return self

    def _add(self, source: str, event: str, **fields) -> "Log":
        self.wall += 0.1
        self.events.append({"source": source, "event": event, "wall": fields.pop("wall", self.wall), **fields})
        return self

    def server(self, event: str, **fields) -> "Log":
        return self._add("server", event, **fields)

    def driver(self, event: str, **fields) -> "Log":
        return self._add("driver", event, **fields)

    def send(self, fields=(), conn: int = 1, seq: int | None = None, uid: str = UID, caps: int = 0x3003,
             disconnect: bool = False, payload: str | None = None, **extra) -> "Log":
        self.msg += 1
        self.seq = self.seq + 1 if seq is None else seq
        fields = list(fields)
        headers = {"content_type": "application/x-protobuf", "uid_header": "01928f3a-7c3e-7a1b-8c2d-3e4f5a6b7c8d"}
        return self.server("a2s", msg=self.msg, conn=conn, seq=self.seq, uid=uid, caps=caps, fields=fields,
                           full="agent_description" in fields, disconnect=disconnect,
                           payload_hash=payload or ",".join(fields), **(headers | extra))

    def respond(self, **fields) -> "Log":
        return self.server("s2a", msg=self.msg, http_status=fields.pop("http_status", 200), **fields)

    def stop(self) -> "Log":
        return self.driver("stopping").driver("stopped")

    def runner(self, event: str, **fields) -> "Log":
        return self._add("runner", event, **fields)

    def crashed(self, run: int, ok: bool = True, sha: str | None = "s1", restarted: str | None = "s1",
                lazyfs: str | None = None) -> "Log":
        """The runner crashed run `run` (on LazyFS: `lazyfs` says how), leaving the state `sha` (ok or
        not), and the next run started from `restarted` (None: it never started)."""
        self.runner("crashed", run=run, at_s=1.2, seed=5, **({"lazyfs": lazyfs} if lazyfs else {}))
        self.runner("crash_state", run=run, ok=ok, state="the data of store 3" if ok else "7 bytes that are no whole save",
                    sha256=sha, bytes=10, files={"state.bin": 10})
        if restarted != "never":
            self.runner("restarted", run=run + 1, sha256=restarted, bytes=10)
        return self

    def auth(self, token: str, ok: bool = True, switched: bool = False, moved: bool = False) -> "Log":
        return self.server("auth", token=token, ok=ok, switched=switched, moved=moved, t=self.wall - 100)

    def settings(self, status: str, hash_: str = OFFER_HASH, error: str = "") -> "Log":
        return self.send(["connection_settings_status"],
                         connection_settings_status={"status": status, "hash": hash_, "error": error})

    def trace(self) -> Trace:
        return Trace(sorted(self.events, key=lambda e: e["wall"]),
                     {"params": self.params, "server": self.server_cfg, "driver": self.driver_cfg},
                     self.transport, self.exit_code)


def asked_every_time(gap: float, messages: int, transport: str = "http") -> Log:
    """A server that asks for the full state in its answer to every message (the description never
    arrives), heartbeats every second, and a client that sends `messages` `gap` seconds apart, then
    stops."""
    log = Log(transport).driver("started")
    log.driver_cfg = {"poll_s": 1, "max_send_bytes": 1024, "description_pad": 2048}
    for i in range(messages):
        log.at(100 + i * gap).send(["health"]).respond(report_full_state=True)
    return log.at(100 + messages * gap + 1).stop()


def clean(transport="http") -> Log:
    return Log(transport).driver("started").send(FULL).respond().send().respond()


def threw(exit_code: int | None = 1) -> Log:
    """A run under callback-throws, up to the throw: the first remote config, and the callback fails
    on it."""
    log = Log(exit_code=exit_code).driver("started").send(FULL).respond(remote_config=OFFER_HASH).driver("throwing")
    log.driver_cfg = {"poll_s": 1, "throw_on_config": True}
    return log



def offered() -> Log:
    """A run under the credentials scenario, up to the offer."""
    log = Log().driver("started").auth("boot").send(FULL).respond().auth("boot").send().respond(credentials_offer=OFFER_HASH)
    log.server_cfg = {"credentials": {"bootstrap": "boot", "offer": "new"}}
    return log

THEN_HASH = "bb"


def rotated() -> Log:
    """A run under the rotation scenario, up to the offer of the second key: the first is in use."""
    log = (offered().settings("APPLYING").auth("new", switched=True).settings("APPLIED")
           .respond(credentials_offer=THEN_HASH, credentials_offer_step=1))
    log.server_cfg = {"credentials": {"bootstrap": "boot", "offer": "new", "then": "newer"}}
    return log


def moved() -> Log:
    """A run under the move scenario, up to the offer of the new endpoint and key."""
    log = offered()
    log.server_cfg = {"credentials": {"bootstrap": "boot", "offer": "new", "move": True}}
    return log


V7 = "01928f3a7c3e7a1b8c2d3e4f5a6b7c8d"
NOT_V7 = "01928f3a7c3e4a1b8c2d3e4f5a6b7c8d"
CONFIG = "ab" * 32
APPLIED_CONFIG = {"status": "RemoteConfigStatuses_APPLIED", "hash": CONFIG, "error": ""}


M = "https://otlp.example/v1/metrics"


def metrics(log: Log) -> Log:
    """A run under the own_metrics scenario."""
    log.server_cfg = {"own_telemetry": {"signal": "metrics", "endpoint": M}}
    return log


def announcing(capabilities: int, log: Log) -> Log:
    """A run against a server that announces `capabilities`."""
    log.server_cfg = {"capabilities": capabilities}
    return log


def core(log: Log, kind: str, **fields) -> Log:
    return log.server("core", kind=kind, **fields)


# A malformed connection-settings offer, as the server logs it.
HOSTILE = {"kind": "endpoint_space", "hash": "h1", "malformed": True, "endpoint": '"http://127.0.0.1:9/v1/h0st1le/1 x"',
           "headers": [], "heartbeat": 0}
# Packages with a malformed one, as the server logs them: worked on in name order.
HPKGS = {"h1-url_scheme": {"version": "1", "sha256": "00", "corrupt": False, "gzip": False, "hostile": "url_scheme",
                           "url": '"ftp://127.0.0.1:9/packages/h0st1le/1"', "headers": []},
         "zz-good": {"version": "1", "sha256": "5a", "corrupt": False, "gzip": False}}
BAD, GOOD = "h1-url_scheme", "zz-good"


def hostile_pkgs(log: Log) -> Log:
    """A run under a hostile packages scenario, up to the offer."""
    return log.driver("started").send(FULL).respond(packages_offer="p1", packages=HPKGS)


def pkg_status(log: Log, **packages: tuple[str, str]) -> Log:
    return log.send(["package_statuses"], package_statuses=statuses("p1", **packages))


# Request heads as the server keeps them.
H_POST = [["Host", "x"], ["Content-Type", "application/x-protobuf"], ["OpAMP-Instance-UID", "u"],
          ["User-Agent", "opamp-clients"], ["Accept-Encoding", "identity"], ["Content-Length", "10"]]
H_UPGRADE = [["Host", "x"], ["Upgrade", "websocket"], ["Connection", "Upgrade"], ["Sec-WebSocket-Key", "k"],
             ["Sec-WebSocket-Version", "13"], ["OpAMP-Instance-UID", "u"], ["User-Agent", "opamp-clients"]]
H_GET = [["Host", "x"], ["Authorization", "Bearer packages"]]


def download(log: Log, headers: list) -> Log:
    return log.server("package_download", path="/packages/a/1", headers=headers, offered_headers=["Authorization"])


def with_token(log: Log) -> Log:
    log.driver_cfg = {"token": "t"}
    return log


def cookie(log: Log) -> Log:
    log.server_cfg = {"cookie": True}
    return log


GZ = {"z": {"version": "1", "sha256": "77", "corrupt": False, "gzip": True}}


def flood(log: Log) -> Log:
    base = log.wall
    for i in range(25):
        log.send(wall=base + 0.01 * (i + 1))
    return log


# Packages behind redirects, as the server describes them (redirect.go): r1 goes to another origin
# and back, r5 takes the 5 redirects allowed, f1 a sixth (to the trap), f2 from https to http.
def rpkg(sha: str, hops: list[str], refused: bool, from_: str = "main") -> dict:
    return {"version": "1", "sha256": sha, "corrupt": False, "gzip": False,
            "redirect": {"from": from_, "hops": hops, "refused": refused}}


RPKGS = {"r1": rpkg("51", ["other", "main"], False), "r5": rpkg("55", ["same"] * 5, False),
         "f1": rpkg("f1", ["same"] * 5 + ["trap"], True), "f2": rpkg("f2", ["downgrade"], True, "tls")}


def hop(log: Log, name: str, n: int, listener: str = "main", status: int = 302, auth: bool | None = None,
        home: str = "main") -> Log:
    """A download request of hop n of name's chain, as the server logs it."""
    auth = listener == home if auth is None else auth
    return log.server("package_download", path=f"/redirect/{name}/1/{n}", package=name, version="1", hop=n,
                      listener=listener, file_listener=home, authorization="Bearer packages" if auth else "",
                      status=status, found=True)


def redirected(drop=lambda e: False, change=lambda e: None, then=lambda log: log, pkgs=None) -> Log:
    """A run where every package behind redirects goes as it should; `drop` removes events,
    `change` edits them, and `then` adds more before the statuses."""
    log = Log().driver("started").send(FULL).respond(packages_offer="p1", packages=pkgs or RPKGS)
    hop(hop(hop(log, "r1", 0), "r1", 1, "other"), "r1", 2, status=200).driver(
        "install_package", name="r1", version="1", sha256="51")
    for n in range(6):
        hop(log, "r5", n, status=200 if n == 5 else 301)
    log.driver("install_package", name="r5", version="1", sha256="55")
    for n in range(6):
        hop(log, "f1", n, status=307)
    hop(log, "f2", 0, "tls", home="tls")
    then(log)
    log.send(["package_statuses"], package_statuses=statuses(
        "p1", r1=("1", INSTALLED), r5=("1", INSTALLED), f1=("", INSTALL_FAILED), f2=("", INSTALL_FAILED)))
    log.events = [e for e in log.events if not drop(e)]
    for e in log.events:
        change(e)
    return log


def is_hop(name: str, n: int):
    return lambda e: e["event"] == "package_download" and e.get("package") == name and e.get("hop") == n


def trap(log: Log, line: str) -> Log:
    return log.server("redirect_trap_connect", remote="127.0.0.1:6000", first_line=line, bytes=len(line), injected=False)


def set_status(name: str, status: str):
    def change(e):
        if e.get("package_statuses"):
            e["package_statuses"]["packages"][name]["status"] = status
    return change


def set_field(pred, **fields):
    def change(e):
        if pred(e):
            e.update(fields)
    return change


# check name -> traces it must pass, traces it must fail
# A handshake with a client certificate, as the server logs it.
CLIENT = {"client_cert": True, "client_subject": "conformance-agent", "client_verified": True}


def tls_hs(client: dict | None = None, log: Log | None = None) -> Log:
    """A completed TLS handshake, with the client certificate fields given (none by default)."""
    fields = {"client_cert": False, "client_subject": None, "client_verified": False} | (client or {})
    return (log or Log()).server("tls_handshake", tcp=1, remote="127.0.0.1:5001", version="TLS 1.3", **fields)


class ProxiedLog(Log):
    def tunnel(self, via: str, upstream: str, user: str = "u") -> "ProxiedLog":
        return self.server("proxy_tunnel", proxy="connect", id=len(self.events) + 1, client="127.0.0.1:4000",
                           via=via, target="127.0.0.1:5000", user=user, upstream=upstream)


def proxied(auth: dict | None = {"user": "u", "pass": "p"}) -> ProxiedLog:  # noqa: B006
    """A run under a proxy scenario, with the proxy's credentials."""
    log = ProxiedLog()
    log.server_cfg = {"proxy": {"kind": "connect", "auth": auth}}
    return log


def dropping(offsets: list[float], stop_at: float | None = 150.0, window=(8, 40), start: float = 100.0,
             fault: bool = True, contact: float | None = None) -> Log:
    """A run under accept-then-drop: the server started at `start` (wall) and accepted and dropped a
    connection `offsets` seconds after the first, which came 8 s in: after the server's start, or
    with `contact`, after the driver's first request that many seconds later (first_contact, the
    clock the server times its faults by)."""
    base = start + (contact or 0.0)
    log = Log("ws").driver("started", wall=base + 1).send(FULL, wall=base + 5).respond(wall=base + 5.01)
    if contact is not None:
        log.server("first_contact", wall=base, t=contact)
    log.server_cfg = {"faults": [{"window_s": list(window), "action": "accept_then_drop"}]} if fault else {}
    for i, off in enumerate(offsets):
        at = base + 8 + off
        log.server("ws_open", conn=i + 2, wall=at, t=at - start)
        log.server("accept_then_drop", conn=i + 2, wall=at + 0.0001, t=at + 0.0001 - start)
    if stop_at is not None:
        log.driver("stopping", wall=stop_at).driver("stopped", wall=stop_at + 0.1)
    return log


CASES: dict[str, tuple[list[Log], list[Log]]] = {
    "server_offers_config_once_per_connection": (
        [core(Log().send(FULL), "connected").respond(offer_config=CONFIG).send().respond()
            .server("core", kind="connected").send(FULL).respond(offer_config=CONFIG),
         # The client lost the reply and resynchronised.
         core(Log().send(FULL), "connected").respond(offer_config=CONFIG).send(FULL).respond(offer_config=CONFIG)],
        [core(Log().send(FULL), "connected").respond(offer_config=CONFIG).send().respond(offer_config=CONFIG),
         # A resynchronisation, but the client had already named the config.
         core(Log().send(FULL), "connected").respond(offer_config=CONFIG)
            .send(FULL, remote_config_status={"status": "RemoteConfigStatuses_APPLYING", "hash": CONFIG, "error": ""})
            .respond(offer_config=CONFIG)],
    ),
    "server_records_applied_config": (
        [Log().send(FULL).respond(offer_config=CONFIG)
            .send(["remote_config_status"], remote_config_status={"status": "RemoteConfigStatuses_APPLIED",
                                                                  "hash": CONFIG, "error": ""})
            .server("core", kind="config_applied", hash=CONFIG),
         # Turned away before the core saw it, over a WebSocket (UNAVAILABLE, no refused_status: the
         # Rust server, nightly 2026-10-08) and over HTTP (503): the server never had it to record.
         Log("ws").send(FULL).respond(offer_config=CONFIG)
            .send(["remote_config_status"], refused="unavailable", remote_config_status=APPLIED_CONFIG)
            .respond(error="UNAVAILABLE"),
         Log().send(FULL).respond(offer_config=CONFIG)
            .send(["remote_config_status"], refused="unavailable", refused_status=503,
                  remote_config_status=APPLIED_CONFIG)
            .respond(http_status=503)],
        [Log().send(FULL).respond(offer_config=CONFIG)
            .send(["remote_config_status"], remote_config_status={"status": "RemoteConfigStatuses_APPLIED",
                                                                  "hash": CONFIG, "error": ""}),
         # Turned away once, then taken in, and still never recorded.
         Log("ws").send(FULL).respond(offer_config=CONFIG)
            .send(["remote_config_status"], refused="unavailable", remote_config_status=APPLIED_CONFIG)
            .respond(error="UNAVAILABLE")
            .send(FULL, seq=1, remote_config_status=APPLIED_CONFIG).respond()],
    ),
    "server_assigns_v7_uids": (
        [Log().send(FULL, uid="0" * 32).respond(assign_uid=V7)],
        [Log().send(FULL, uid="0" * 32).respond(assign_uid=NOT_V7)],
    ),
    "server_sees_goodbye": (
        [core(Log("ws").driver("started").send(FULL), "connected").send(disconnect=True)
            .server("core", kind="disconnected").stop()],
        [core(Log("ws").driver("started").send(FULL), "connected").send(disconnect=True).stop()],
    ),
    "sequence_increments_by_one": (
        [clean(),
         Log().send(FULL).respond().send(refused_status=429).send(seq=1).respond(),
         # Turned away over a WebSocket (the Rust server's UNAVAILABLE): never counted, so sent again.
         Log("ws").send(FULL).respond().send(refused="unavailable").respond(error="UNAVAILABLE")
              .send(seq=1).respond(),
         Log().send(FULL).respond(http_status=503).send(FULL, seq=0).respond(),
         # Number 0 again: the first reply was lost, then one was an error (RULE-CLIENT-20).
         Log().send(FULL).send(FULL, seq=0).respond(error="BAD_REQUEST").send(FULL, seq=0).respond()
              .send(seq=1).respond(),
         # Over WebSocket a message may cross the first answer (java-core status-change-ws, under load).
         Log("ws").send(FULL).respond().send(seq=0).respond().send(seq=1).respond()],
        [clean().send(seq=1).respond(), Log().send(FULL, seq=0).send(seq=5),
         Log().send(FULL).respond().send(seq=0).respond(),
         Log("ws").send(FULL).respond().send(seq=1).respond().send(seq=0).send(seq=0),
         Log().send(FULL, seq=3).send(seq=3)],
    ),
    "number_zero_until_answered": (
        [clean(),
         Log().send(FULL).send(FULL, seq=0).respond(error="BAD_REQUEST").send(FULL, seq=0).respond(http_status=413)
              .send(FULL, seq=0).respond().send(seq=1).respond(),
         # Over WebSocket a message may cross the first answer.
         Log("ws").send(FULL).respond().send(seq=0).send(seq=1)],
        [Log().send(FULL).send(FULL, seq=1).respond(),
         Log().send(FULL).respond().send(seq=0).respond(),
         Log().send(FULL).respond().send(seq=2).respond(),
         Log("ws").send(FULL).send(seq=1),
         Log().send(FULL, seq=4)],
    ),
    "reports_status_capability": (
        [clean()],
        [Log().send(FULL, caps=0x2)],
    ),
    "instance_uid_valid": (
        [clean()],
        [Log().send(FULL, uid="0" * 32), Log().send(FULL, uid="abcd")],
    ),
    "first_message_is_full": (
        [clean()],
        [Log().send(["health"]).send(FULL)],
    ),
    "full_report_after_reconnect": (
        [clean("ws").send(FULL, conn=2), clean("ws").send(FULL, conn=2).send(conn=2)],
        # The bug this caught in the checker itself: the reconnect's bare first message was skipped.
        [clean("ws").send(conn=2).send(FULL, conn=2)],
    ),
    "reports_after_connecting": (
        [Log("ws").server("ws_open", conn=1).send(FULL).respond()],
        [Log("ws").server("ws_open", conn=1).send(FULL).server("ws_open", conn=2).at(200).send(conn=3)],
    ),
    "respects_server_capabilities": (
        [Log(server_capabilities=0x3).send(FULL + ["effective_config"]).respond().send(["health"]).respond()],
        [Log(server_capabilities=0x3).send(FULL + ["effective_config"]).respond()
            .send(["effective_config"]).respond()],
    ),
    "disconnect_is_last": (
        [clean().send(disconnect=True),
         Log("ws").send(FULL, conn=1).send(disconnect=True, conn=1).send(FULL, conn=2)],
        [clean().send(disconnect=True).send(),
         Log("ws").send(FULL, conn=1).send(disconnect=True, conn=1).send(conn=1)],
    ),
    "reconnect_says_goodbye": (
        [Log("ws").send(FULL, conn=1).send(disconnect=True, conn=1)
            .server("close_frame_received", conn=1, answers_server_close=False).send(FULL, conn=2)],
        [Log("ws").send(FULL, conn=1)
            .server("close_frame_received", conn=1, answers_server_close=False).send(FULL, conn=2)],
    ),
    "stop_sends_disconnect": (
        [clean().driver("stopping").send(disconnect=True).driver("stopped")],
        [clean().stop()],
    ),
    "driver_survives": (
        # A run the runner crashed on purpose says nothing: the others stop.
        [clean().stop(), clean().crashed(0).driver("started").stop()],
        [Log(exit_code=1).send(FULL).driver("fatal", error="boom"), Log().send(FULL),
         clean().crashed(0).driver("started")],
    ),
    "callback_failure_stops": (
        # Stopped at once; or a message already on its way after the throw.
        [threw().driver("fatal", error="the notification callback panicked: callback-throws"),
         threw().send().driver("fatal", error="RuntimeException: callback-throws")],
        # No throw; no fatal (the driver said stopped, or heard nothing); the wrong error; two
        # fatals; the driver stopped it; a notification after stopped; the process died (exit 2,
        # or killed); a goodbye; still polling; thrown twice.
        [Log(exit_code=1).send(FULL).driver("fatal", error="callback-throws"),
         threw(0).stop(),
         threw(None),
         threw().driver("fatal", error="boom"),
         threw().driver("fatal", error="callback-throws").driver("fatal", error="callback-throws"),
         threw(0).driver("fatal", error="callback-throws").stop(),
         threw().driver("fatal", error="callback-throws").driver("connect"),
         threw(2).driver("fatal", error="callback-throws"),
         threw(None).driver("fatal", error="callback-throws"),
         threw().send(disconnect=True).driver("fatal", error="callback-throws"),
         threw().driver("fatal", error="callback-throws").at(105).send(),
         threw().driver("throwing").driver("fatal", error="callback-throws")],
    ),
    "crash_state_consistent": (
        [clean().crashed(0).driver("started").stop(),
         clean().crashed(0, sha=None, restarted=None).driver("started").crashed(1).driver("started").stop(),
         clean().crashed(0, lazyfs="torn").driver("started").crashed(1, lazyfs="clear-cache").driver("started").stop()],
        # No crash (the run ended first); a torn state (by the tap's model, or left by LazyFS); the
        # next run never started; it started from something other than what the crash left; the
        # crash state couldn't be made.
        [clean().stop().runner("crash_missed", run=0, at_s=2.0, exit=0),
         clean().crashed(0, ok=False).driver("started").stop(),
         clean().crashed(0, ok=False, lazyfs="clear-cache").driver("started").stop(),
         clean().crashed(0, restarted="never"),
         clean().crashed(0, restarted="s2").driver("started").stop(),
         clean().runner("crashed", run=0, at_s=1.0, seed=5).runner("crash_state", run=0, error="no tap")],
    ),
    "stop_bounded": (
        [Log().driver("stopping").driver("stopped", wall=106.0)],
        [Log().driver("stopping").driver("stopped", wall=109.0), Log().driver("stopping")],
    ),
    "nothing_outlives_stop": (
        # Let go of before stop ended, or in the same instant.
        [Log().server("held", hold=1, what="messages", path="/v1/opamp").server("held_closed", hold=1, by_client=True)
         .stop(),
         Log().server("held", hold=1, what="downloads", path="/p").stop().server("held_closed", hold=1, by_client=True)],
        # Never let go of (the server closed it at shutdown), let go of seconds after stop, nothing
        # held at all, or no stop.
        [Log().server("held", hold=1, what="messages", path="/v1/opamp").stop()
         .server("held_closed", hold=1, by_client=False),
         Log().server("held", hold=1, what="downloads", path="/p").stop()
         .server("held_closed", hold=1, by_client=True, wall=110.0),
         Log().stop(),
         Log().server("held", hold=1, what="messages", path="/v1/opamp")],
    ),
    "http_content_type": (
        [clean()],
        [Log().send(FULL, content_type="application/json")],
    ),
    "http_instance_uid_header": (
        [clean()],
        [Log().send(FULL, uid_header=""), Log().send(FULL, uid_header="not-it")],
    ),
    "adopts_assigned_uid": (
        [Log().send(FULL).respond(assign_uid=UID2).send(uid=UID2),
         # Over a WebSocket, a message that crossed the response (read just after it) still has the old UID.
         Log("ws").send(FULL).respond(assign_uid=UID2).send(uid=UID).at(102).send(uid=UID2),
         # A UID assigned after the client stopped (the server was that slow) never reached it.
         Log("ws").driver("started").send(FULL).respond(assign_uid=UID2).send(uid=UID2).stop()
                  .at(103).respond(assign_uid=UID).at(104).send(uid=UID2),
         # Assigned again before the next message (in reply to one that crossed the first): the
         # next message uses the newer UID (torture, node ws).
         Log("ws").send(FULL).respond(assign_uid=UID2).send(uid=UID).respond(assign_uid=UID3).at(102).send(uid=UID3),
         # Assigned in reply to a goodbye: the client closed that conversation as it said goodbye
         # (torture, go-core ws).
         Log("ws").send(FULL).respond(assign_uid=UID2).at(102).send(uid=UID2).respond()
                  .send(uid=UID2, disconnect=True).respond(assign_uid=UID3)
                  .server("close_frame_received", conn=1, answers_server_close=False)
                  .at(104).server("ws_open", conn=2).send(FULL, conn=2, uid=UID2),
         # The goodbye crossed the reply that assigned a UID: the client said goodbye before it
         # could read it, and its next run keeps the UID it had (torture, python-core ws).
         Log("ws").send(FULL).respond(assign_uid=UID2).send(uid=UID2).respond()
                  .send(uid=UID2).send(uid=UID2, disconnect=True).server("s2a", msg=3, http_status=200, assign_uid=UID3)
                  .at(104).server("ws_open", conn=2).send(FULL, conn=2, uid=UID2)],
        [Log().send(FULL).respond(assign_uid=UID2).send(uid=UID),
         # A goodbye read well after the reply that assigned the UID crossed nothing.
         Log("ws").send(FULL).respond(assign_uid=UID2).send(uid=UID2).respond()
                  .send(uid=UID2).server("s2a", msg=3, http_status=200, assign_uid=UID3)
                  .at(102).send(uid=UID2, disconnect=True).at(104).server("ws_open", conn=2).send(FULL, conn=2, uid=UID2),
         Log("ws").send(FULL).respond(assign_uid=UID2).send(uid=UID).at(102).send(uid=UID),
         # Assigned twice, the next message keeps neither.
         Log("ws").send(FULL).respond(assign_uid=UID2).send(uid=UID).respond(assign_uid=UID3).at(102).send(uid=UID),
         # Assigned twice, the next message takes the older (python http): judged by the newer.
         Log().send(FULL).respond(assign_uid=UID2).send(uid=UID).respond(assign_uid=UID3).send(uid=UID2)],
    ),
    "full_report_after_request": (
        [Log().send(FULL).respond(report_full_state=True).send(FULL),
         # Every status field reported so far comes again, not the description alone.
         Log().send(FULL + ["effective_config"]).respond().send(["remote_config_status"])
              .respond(report_full_state=True).send(FULL + ["effective_config", "remote_config_status"]),
         # Over a WebSocket, a heartbeat that crossed the response (read just after it) comes first.
         Log("ws").send(FULL).respond(report_full_state=True).send().send(FULL),
         # What the server said it doesn't take (35: no effective config) is left out of the full report.
         announcing(35, Log().send(FULL + ["effective_config"]).respond(report_full_state=True).send(FULL)),
         # A client that sends nothing more until its goodbye, and puts its state in that (opamp-java, ws).
         Log("ws").send().respond(report_full_state=True).at(109).driver("stopping")
                  .send(FULL, disconnect=True).driver("stopped")],
        [Log().send(FULL).respond(report_full_state=True).send(["health"]),
         # The request ignored: the heartbeats go on. A full report later isn't its answer.
         Log().send(FULL).respond(report_full_state=True).send().respond().send().respond(),
         Log().send(FULL).respond(report_full_state=True).send().respond().send(FULL),
         # Ignored, and then the description changes: a report with the description, and still partial.
         Log().send(FULL).respond(report_full_state=True).at(103).send(["agent_description"]),
         Log("ws").send(FULL).respond(report_full_state=True).at(103).send(["agent_description"]),
         # Over a WebSocket, a heartbeat a second after the response didn't cross it.
         Log("ws").send(FULL).respond(report_full_state=True).at(101).send().send(FULL),
         # A field reported before, and missing from the full report.
         Log().send(FULL + ["effective_config"]).respond(report_full_state=True).send(FULL),
         # Nothing at all, though the client went on for 10 s; or only its goodbye, without its state.
         Log().send(FULL).respond(report_full_state=True).at(110).stop(),
         Log("ws").send(FULL).respond(report_full_state=True).at(110).driver("stopping").send(disconnect=True)
                  .driver("stopped")],
    ),
    "asks_wait_for_the_heartbeat": (
        [asked_every_time(1.0, 10), asked_every_time(1.0, 10, transport="ws")],
        # One per round trip; and a client that stops talking.
        [asked_every_time(0.01, 1000), asked_every_time(5.0, 10)],
    ),
    "full_report_after_unaccepted": (
        [Log().send(FULL).respond(http_status=413).send(FULL)],
        [Log().send(FULL).respond(http_status=413).send(["health"]),
         Log().send(FULL).server("request_rejected", status=429).send(["health"])],
    ),
    "rejected_not_resent": (
        [Log().send(FULL, payload="big").respond(http_status=413).send(["health"], payload="small"),
         Log().send(FULL, payload="big").respond(http_status=413).send().respond(report_full_state=True)
              .send(FULL, payload="big"),
         # Torture seed 20260929 (node-rust, ws): a status change put the client one message ahead
         # of the server's asks, so the answer to the second ask crossed the rejection of the answer
         # to the first.
         Log("ws").send(FULL).respond().send().respond(report_full_state=True)
              .at(110).send(["remote_config_status"], payload="applied").at(110.0001).respond(report_full_state=True)
              .at(110.0002).send(["remote_config_status"], payload="applied").at(110.0003).respond(error="BAD_REQUEST")
              .at(110.0004).send(["remote_config_status"], payload="applied").respond(report_full_state=True)],
        [Log().send(FULL, payload="big").respond(http_status=413).send(FULL, payload="big"),
         Log("ws").send(FULL, payload="big").respond(error="BAD_REQUEST").send(FULL, payload="big"),
         # The same, but the ask was long before: nothing the client was answering.
         Log("ws").send(FULL).respond().send().respond(report_full_state=True)
              .at(110).send(["remote_config_status"], payload="applied").at(110.0003).respond(error="BAD_REQUEST")
              .at(110.0004).send(["remote_config_status"], payload="applied"),
         # Or the resend long after the rejection.
         Log("ws").send(FULL).respond().send().respond(report_full_state=True)
              .at(110).send(["remote_config_status"], payload="applied").at(110.0001).respond(report_full_state=True)
              .at(110.0002).send(["remote_config_status"], payload="applied").at(110.0003).respond(error="BAD_REQUEST")
              .at(112).send(["remote_config_status"], payload="applied")],
    ),
    "honours_retry_after": (
        [Log().send(FULL).at(110).server("request_rejected", retry_after="4").at(114.2).send(),
         # The goodbye after UNAVAILABLE comes at once, however long the client takes to send it.
         Log("ws").send(FULL).at(110).respond(error="UNAVAILABLE", retry_after=4).at(110.05)
              .send(disconnect=True).at(114.1).server("ws_open", conn=2)],
        [Log().send(FULL).at(110).server("request_rejected", retry_after="4").at(111).send(),
         Log("ws").send(FULL).at(110).respond(error="UNAVAILABLE", retry_after=4).at(111).server("ws_open", conn=2)],
    ),
    "heartbeat_follows_offer": (
        [Log().send(FULL).respond(heartbeat_offer=3).at(103).send().at(106).send().at(109).send(),
         # Capabilities only on the first message, health in every heartbeat (the .NET client).
         Log().send(FULL).respond(heartbeat_offer=3).at(103).send(["health"], caps=0).at(106)
              .send(["health"], caps=0).at(109).send(["health"], caps=0)],
        [Log().send(FULL).respond(heartbeat_offer=3).at(108).send().at(116).send().at(124).send(),
         Log().send(FULL).respond(heartbeat_offer=3)],
    ),
    "status_change_reported": (
        [clean().driver("status_change", change=1).send(["agent_description"]),
         # The server logged the report a few milliseconds before the driver's clock said it changed.
         clean().at(120).driver("status_change", change=1, wall=120.05).send(["agent_description"], wall=120.01)],
        [clean().driver("status_change", change=1).at(200).send(["agent_description"])],
    ),
    "applies_remote_config": (
        [clean().respond(offer_config="abc").send(["remote_config_status"],
                                                  remote_config_status={"status": "APPLIED", "hash": "abc"})
                .respond(offer_config="abc").send(),
         clean().respond(offer_config="abc").send(["remote_config_status"],
                                                  remote_config_status={"status": "APPLIED", "hash": "abc"})],
        [clean().respond(offer_config="abc").send(),
         clean().respond(offer_config="abc").send(disconnect=True).respond(offer_config="def")],
    ),
    "close_frame_answered": (
        [Log("ws").send(FULL).server("close_frame_sent", conn=1)
              .server("close_frame_received", conn=1, answers_server_close=True)],
        [Log("ws").send(FULL).server("close_frame_sent", conn=1).server("close_not_answered", conn=1)],
    ),
    "reconnects_after_loss": (
        [Log("ws").send(FULL).server("ws_closed", conn=1).at(103).server("ws_open", conn=2).at(130).stop(),
         # Reconnected at once: the new connection is logged before the old one's close.
         Log("ws").send(FULL).server("ws_open", conn=2).server("ws_closed", conn=1).at(130).stop()],
        [Log("ws").send(FULL).server("ws_closed", conn=1).at(130).stop(),
         # The lost connection's own opening is no reconnect.
         Log("ws").server("ws_open", conn=1).send(FULL).server("ws_closed", conn=1).at(130).stop()],
    ),
    "reconnect_backs_off": (
        # The answered connection goes at 8 s; the reconnect is at once, then 1-2 s, 2-4 s, 4-8 s...
        [dropping([0, 1.6, 4.9, 11.2]),
         # Longest delays from attempt 1: the last gap only twice the first.
         dropping([0, 2.0, 4.0, 8.0, 16.0]),
         # Shortest delays from attempt 0 (the first connection the window saw was never answered).
         dropping([0, 0.5, 1.5, 3.5, 7.5, 15.5]),
         # A slow connect in every gap, slowest on the first (the Java client's 2.2 s, then 0.7 s):
         # the longest first delay, the shortest after it.
         dropping([0, 3.2, 5.0, 8.5, 13.2]),
         # Three gaps are not judged on growth: with jitter and a slow connect they may not grow.
         dropping([0, 3.0, 5.0, 7.0]),
         # Waited longer than the observation (the upstream Java client: 30 s), or gave up.
         dropping([0, 30.2]),
         dropping([0]),
         # Accepts after the observation are not counted.
         dropping([0, 0.6, 1.8, 4.5, 9.9, 18.0, 31.0, 31.5, 32.0])],
        # A storm: reconnecting at once after every drop.
        [dropping([i * 0.005 for i in range(400)]),
         # A short backoff, then a storm.
         dropping([0, 1.0] + [1.0 + i * 0.01 for i in range(1, 50)]),
         # A fixed delay of 2 s: too many.
         dropping([i * 2.0 for i in range(10)]),
         # A fixed delay of 3 s: few enough, but no growth.
         dropping([i * 3.0 for i in range(7)]),
         # Shrinking gaps.
         dropping([0, 4.0, 7.0, 9.0, 10.0]),
         # A storm slowed by a slow connect (the Java client in Docker): gaps that don't grow.
         dropping([0, 2.22, 3.62, 4.64, 5.45]),
         # Four gaps that do not grow.
         dropping([0, 2.0, 4.0, 6.0, 8.5])],
    ),
    "remote_config_handed_once": (
        [clean().respond(offer_config="aa").driver("message", remote_config_hash="aa").respond(offer_config="aa")],
        [clean().respond(offer_config="aa"),
         clean().respond(offer_config="aa").driver("message", remote_config_hash="aa").respond(offer_config="aa")
                .driver("message", remote_config_hash="aa")],
    ),
    "remote_config_kept_across_restart": (
        [clean().respond(offer_config="aa").driver("message", remote_config_hash="aa").stop()
                .driver("started").driver("message", remote_config_hash="aa").send().respond().stop()],
        [clean().respond(offer_config="aa").driver("message", remote_config_hash="aa").stop(),
         clean().stop().driver("started").stop(),
         clean().respond(offer_config="aa").driver("message", remote_config_hash="aa").stop()
                .driver("started").send().respond(offer_config="aa").driver("message", remote_config_hash="aa").stop(),
         clean().respond(offer_config="aa").driver("message", remote_config_hash="aa").stop()
                .driver("started").driver("message", remote_config_hash="aa").respond(offer_config="aa").stop()],
    ),
    "telemetry_settings_applied": (
        [metrics(clean().respond(telemetry_offer="cc").driver("telemetry_settings", hash="cc", metrics_endpoint=M)
                 .settings("APPLIED", "cc"))],
        [metrics(clean()),
         metrics(clean().respond(telemetry_offer="cc").settings("APPLIED", "cc")),
         metrics(clean().respond(telemetry_offer="cc").driver("telemetry_settings", hash="cc", metrics_endpoint="x")
                 .settings("APPLIED", "cc")),
         metrics(clean().respond(telemetry_offer="cc").driver("telemetry_settings", hash="cc", metrics_endpoint=M)),
         metrics(clean().respond(telemetry_offer="cc").settings("APPLIED", "cc").at(110)
                 .driver("telemetry_settings", hash="cc", metrics_endpoint=M))],
    ),
    "telemetry_settings_kept_across_restart": (
        [clean().respond(telemetry_offer="cc").driver("telemetry_settings", hash="cc").stop()
                .driver("started").driver("telemetry_settings", hash="cc").send().respond().stop()],
        [clean().respond(telemetry_offer="cc").driver("telemetry_settings", hash="cc").stop(),
         clean().stop().driver("started").stop(),
         clean().respond(telemetry_offer="cc").driver("telemetry_settings", hash="cc").stop()
                .driver("started").send().stop(),
         clean().respond(telemetry_offer="cc").driver("telemetry_settings", hash="cc").stop()
                .driver("started").driver("telemetry_settings", hash="cc").respond(telemetry_offer="cc").stop()],
    ),
    "components_hash_first": (
        [Log().send(FULL, caps=0x4001, available_components={"hash": "ab", "components": 0})],
        [Log().send(FULL, caps=0x4001),
         Log().send(FULL, caps=0x4001, available_components={"hash": "", "components": 0}),
         Log().send(FULL, caps=0x4001, available_components={"hash": "ab", "components": 2}),
         Log().send(FULL, caps=0x0001, available_components={"hash": "ab", "components": 0})],
    ),
    "components_in_full_when_asked": (
        [Log().send(FULL, available_components={"hash": "ab", "components": 0}).respond(ask_components=True)
              .send(available_components={"hash": "ab", "components": 2}).respond().send()],
        [Log().send(FULL, available_components={"hash": "ab", "components": 0}).respond(),
         Log().send(FULL, available_components={"hash": "ab", "components": 0}).respond(ask_components=True)
              .send(available_components={"hash": "ab", "components": 0}),
         Log().send(FULL, available_components={"hash": "ab", "components": 0}).respond(ask_components=True)
              .send(available_components={"hash": "", "components": 2}),
         Log().send(FULL, available_components={"hash": "ab", "components": 0}).respond(ask_components=True)
              .send(available_components={"hash": "ab", "components": 2}).respond()
              .send(available_components={"hash": "ab", "components": 2})],
    ),
    "custom_capabilities_declared": (
        [Log().send(FULL, custom_capabilities=["io.opentelemetry.echo"])],
        [Log().send(FULL), Log().send(FULL, custom_capabilities=["io.example.other"])],
    ),
    "custom_message_answered": (
        [Log().send(FULL).respond(custom_ping="68")
              .driver("custom_message", capability="io.opentelemetry.echo", type="ping", data="68")
              .send(custom_message={"capability": "io.opentelemetry.echo", "type": "pong", "data": "68"})],
        [Log().send(FULL).respond(),
         Log().send(FULL).respond(custom_ping="68").send(),
         Log().send(FULL).respond(custom_ping="68")
              .driver("custom_message", capability="io.opentelemetry.echo", type="ping", data="68").send(),
         Log().send(FULL).respond(custom_ping="68")
              .driver("custom_message", capability="io.opentelemetry.echo", type="ping", data="68")
              .send(custom_message={"capability": "io.opentelemetry.echo", "type": "pong", "data": "00"})],
    ),
    "undeclared_custom_message_ignored": (
        [Log().send(FULL).respond(custom_other=True)],
        [Log().send(FULL).respond(),
         Log().send(FULL).respond(custom_other=True)
              .driver("custom_message", capability="io.opentelemetry.other", type="ping", data="")],
    ),
    "server_custom_capabilities_told": (
        [Log().driver("server_custom_capabilities", capabilities=["io.opentelemetry.echo"])],
        [Log(),
         Log().driver("server_custom_capabilities", capabilities=["io.example.other"]),
         Log().driver("server_custom_capabilities", capabilities=["io.opentelemetry.echo"])
              .driver("server_custom_capabilities", capabilities=["io.opentelemetry.echo"])],
    ),
    "package_statuses_reported": (
        [Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .send(["package_statuses"], package_statuses=statuses("a1", a=("1", INSTALLED), b=("", INSTALL_FAILED)))],
        [Log().send(FULL),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS).send(),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .send(["package_statuses"], package_statuses=statuses("a1"))
              .respond(packages_offer="a2", packages=PKGS)
              .send(["package_statuses"], package_statuses=statuses("a2")).send(["package_statuses"], package_statuses=statuses("a1")),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .send(["package_statuses"], package_statuses=statuses("a1"))],
    ),
    "packages_installed_verified": (
        [Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .driver("install_package", name="a", version="1", sha256="5a")
              .send(["package_statuses"], package_statuses=statuses("a1", a=("1", INSTALLED)))],
        [Log().send(FULL),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .driver("install_package", name="a", version="1", sha256="00"),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .driver("install_package", name="a", version="1", sha256="5a")
              .send(["package_statuses"], package_statuses=statuses("a1", a=("1", "PackageStatusEnum_Installing")))],
    ),
    "corrupt_package_refused": (
        [Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .send(["package_statuses"], package_statuses=statuses("a1", b=("", INSTALL_FAILED)))],
        [Log().send(FULL).respond(packages_offer="a1", packages={"a": PKGS["a"]}),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .driver("install_package", name="b", version="1", sha256="bb")
              .send(["package_statuses"], package_statuses=statuses("a1", b=("", INSTALL_FAILED))),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .send(["package_statuses"], package_statuses=statuses("a1", b=("", "PackageStatusEnum_Downloading")))],
    ),
    "unoffered_package_removed": (
        [Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .driver("install_package", name="a", version="1", sha256="5a")
              .respond(packages_offer="a2", packages={}).driver("remove_package", name="a")
              .send(["package_statuses"], package_statuses=statuses("a2"))],
        [Log().send(FULL),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .driver("install_package", name="a", version="1", sha256="5a"),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .driver("install_package", name="a", version="1", sha256="5a")
              .respond(packages_offer="a2", packages={}),
         Log().send(FULL).respond(packages_offer="a1", packages=PKGS)
              .driver("install_package", name="a", version="1", sha256="5a")
              .respond(packages_offer="a2", packages={}).driver("remove_package", name="a")
              .send(["package_statuses"], package_statuses=statuses("a2", a=("1", INSTALLED)))],
    ),
    "package_download_credentials": (
        [Log().server("package_download", path="/packages/a/1", authorization="Bearer packages", found=True)],
        [Log(),
         Log().server("package_download", path="/packages/a/1", authorization="", found=True)],
    ),
    "redirects_not_followed": (
        [Log().server("request_rejected", status=307, upgrade=False).send(FULL).respond()],
        [clean(),
         Log().server("request_rejected", status=307, upgrade=False).server("stray_request", path="/v1/moved")
              .send(FULL).respond(),
         Log().send(FULL).respond().server("request_rejected", status=307, upgrade=False)],
    ),
    "sends_core_headers": (
        [Log().send(FULL, user_agent="opamp-clients", accept_encoding="identity").respond(),
         Log("ws").server("ws_open", conn=1, user_agent="opamp-clients", extensions="").send(FULL)],
        [Log().send(FULL, user_agent="Go-http-client/1.1", accept_encoding="identity").respond(),
         Log().send(FULL, user_agent="opamp-clients", accept_encoding="gzip").respond(),
         Log("ws").server("ws_open", conn=1, user_agent="opamp-clients", extensions="permessage-deflate"),
         Log("ws").server("ws_open", conn=1, user_agent="node", extensions="")],
    ),
    "malformed_settings_failed": (
        [Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE).settings("APPLYING", "h1")
              .settings("FAILED", "h1").stop(),
         # Offered just before the stop: too soon to judge, but another one was.
         Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE).settings("FAILED", "h1")
              .respond(hostile_offer=HOSTILE | {"hash": "h2"}).stop()],
        [Log().send(FULL).respond(),
         Log().send(FULL).respond(hostile_offer=HOSTILE).settings("APPLYING", "h1").settings("APPLIED", "h1"),
         Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE).settings("APPLYING", "h1").at(130).stop(),
         Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE).at(130).settings("FAILED", "h1").stop(),
         Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE).stop(),
         # The client died at the offer: not excused by the stop that followed.
         Log(exit_code=1).driver("started").send(FULL).respond(hostile_offer=HOSTILE)
              .driver("fatal", error="IllegalArgumentException").stop()],
    ),
    "malformed_packages_failed": (
        [pkg_status(pkg_status(hostile_pkgs(Log()), **{BAD: ("", INSTALL_FAILED)}),
                    **{BAD: ("", INSTALL_FAILED), GOOD: ("1", INSTALLED)}).stop()],
        [Log().send(FULL).respond(packages_offer="p1", packages=PKGS),
         pkg_status(hostile_pkgs(Log()), **{BAD: ("", "PackageStatusEnum_Downloading")}).at(200).stop(),
         pkg_status(hostile_pkgs(Log()), **{BAD: ("1", INSTALLED), GOOD: ("1", INSTALLED)}),
         # Wedged: the good package is never reached.
         pkg_status(hostile_pkgs(Log()), **{BAD: ("", INSTALL_FAILED)}).at(200).stop(),
         pkg_status(hostile_pkgs(Log()).at(150), **{BAD: ("", INSTALL_FAILED), GOOD: ("1", INSTALLED)})],
    ),
    "no_request_to_malformed": (
        [Log().send(FULL).respond(hostile_offer=HOSTILE).settings("FAILED", "h1"),
         hostile_pkgs(Log()),
         # Connected, then gave up before sending anything: no request was made.
         hostile_pkgs(Log()).server("hostile_connect", remote="127.0.0.1:5", first_line='""', bytes=0)],
        [Log().send(FULL).respond(),
         Log().send(FULL).respond(hostile_offer=HOSTILE)
              .server("hostile_connect", remote="127.0.0.1:5", first_line='"GET /v1/h0st1le/1%20x HTTP/1.1"', bytes=40),
         hostile_pkgs(Log()).server("hostile_request", method="GET", path='"/packages/h0st1le/1"', headers=[])],
    ),
    "keeps_reporting": (
        [Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE).settings("FAILED", "h1")
              .driver("status_change", change=1).send(["agent_description"]).stop(),
         # Offered in reply to the last message before the stop.
         Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE).stop()],
        [Log().send(FULL).respond(),
         Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE).at(130).stop(),
         Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE).send()
              .driver("status_change", change=1).at(120).send(["agent_description"]).stop(),
         flood(Log().driver("started").send(FULL).respond(hostile_offer=HOSTILE)),
         Log(exit_code=1).driver("started").send(FULL).respond(hostile_offer=HOSTILE)
              .driver("fatal", error="IllegalArgumentException").stop()],
    ),
    "headers_as_given": (
        [Log().send(FULL, headers=H_POST).respond(),
         with_token(Log().send(FULL, headers=H_POST + [["Authorization", "Bearer t"]])),
         Log("ws").server("ws_open", conn=1, headers=H_UPGRADE).send(FULL),
         download(Log(), H_GET),
         # Chunked instead of a length; the stack's casing.
         Log().send(FULL, headers=[["host", "x"], ["content-type", "a"], ["opamp-instance-uid", "u"],
                                   ["user-agent", "o"], ["accept-encoding", "identity"], ["transfer-encoding", "chunked"]])],
        [Log().send(FULL).respond(),
         Log().send(FULL, headers=H_POST + [["Accept", "*/*"]]),
         Log().send(FULL, headers=H_POST + [["Authorization", "Bearer t"]]),
         Log().send(FULL, headers=H_POST + [["User-Agent", "Go-http-client/1.1"]]),
         Log("ws").server("ws_open", conn=1, headers=H_UPGRADE + [["Sec-WebSocket-Extensions", "permessage-deflate"]]),
         Log("ws").server("ws_open", conn=1, headers=H_UPGRADE + [["Origin", "http://x"]]),
         download(Log(), H_GET + [["Accept-Encoding", "gzip"]]),
         download(Log(), H_GET + [["Content-Length", "0"]])],
    ),
    "header_case_kept": (
        [Log().send(FULL, headers=H_POST), Log("ws").server("ws_open", conn=1, headers=H_UPGRADE), download(Log(), H_GET)],
        [Log().send(FULL),
         Log().send(FULL, headers=[["Host", "x"], ["Opamp-Instance-Uid", "u"]]),
         Log("ws").server("ws_open", conn=1, headers=[["host", "x"], ["user-agent", "opamp-clients"]]),
         download(Log(), [["Host", "x"], ["authorization", "Bearer packages"]])],
    ),
    "no_cookies": (
        [cookie(Log().send(FULL, headers=H_POST).respond().send(headers=H_POST)),
         cookie(Log("ws").server("ws_open", conn=1, headers=H_UPGRADE).send(FULL)
                .server("ws_open", conn=2, headers=H_UPGRADE))],
        [Log().send(FULL, headers=H_POST).respond().send(headers=H_POST),
         cookie(Log().send(FULL, headers=H_POST).respond()
                .send(headers=H_POST + [["Cookie", "opamp_conformance=set-by-server"]])),
         cookie(Log("ws").server("ws_open", conn=1, headers=H_UPGRADE).send(FULL)
                .server("ws_open", conn=2, headers=H_UPGRADE + [["Cookie", "opamp_conformance=set-by-server"]]))],
    ),
    "redirects_followed": (
        [redirected()],
        [Log().send(FULL).respond(packages_offer="p1", packages=PKGS),
         redirected(drop=is_hop("r1", 2)),
         redirected(change=set_field(lambda e: e["event"] == "install_package" and e["name"] == "r5", sha256="56")),
         redirected(change=set_status("r1", INSTALL_FAILED))],
    ),
    "refused_redirects_failed": (
        [redirected()],
        [redirected(pkgs={"r1": RPKGS["r1"]}),
         redirected(then=lambda log: trap(log, '"GET /redirect/f2/1/1 HTTP/1.1"')),
         redirected(then=lambda log: trap(log, '"\\u0016\\u0003\\u0001"')),
         redirected(then=lambda log: log.driver("install_package", name="f1", version="1", sha256="f1")),
         redirected(drop=is_hop("f2", 0)),
         redirected(change=set_status("f1", "PackageStatusEnum_Downloading")),
         redirected(then=lambda log: hop(log, "f2", 0, "tls", home="tls"))],
    ),
    "redirect_headers_same_origin": (
        [redirected()],
        [redirected(change=set_field(is_hop("r1", 1), authorization="Bearer packages")),
         redirected(change=set_field(is_hop("r1", 2), authorization="")),
         redirected(drop=is_hop("r1", 1))],
    ),
    "redirect_hops_bounded": (
        [redirected()],
        [redirected(pkgs={"r1": RPKGS["r1"], "f2": RPKGS["f2"]}),
         redirected(then=lambda log: hop(log, "f1", 6, status=200)),
         redirected(then=lambda log: trap(log, '"GET /redirect/f1/1/6 HTTP/1.1"')),
         redirected(drop=lambda e: is_hop("r5", 4)(e) or is_hop("r5", 5)(e))],
    ),
    "no_decompression": (
        [Log().send(FULL).respond(packages_offer="g1", packages=GZ)
              .driver("install_package", name="z", version="1", sha256="77")],
        [Log().send(FULL).respond(packages_offer="p1", packages=PKGS),
         Log().send(FULL).respond(packages_offer="g1", packages=GZ)
              .driver("install_package", name="z", version="1", sha256="78"),
         Log().send(FULL).respond(packages_offer="g1", packages=GZ)
              .send(["package_statuses"], package_statuses=statuses("g1", z=("", INSTALL_FAILED)))],
    ),
    "instance_uid_kept_across_restart": (
        [clean().stop().at(200).driver("started").send(FULL).respond().stop(),
         # Assigned in reply to the run's last message, and stopped before it sent another.
         Log().driver("started").send(FULL).respond(assign_uid=UID2).stop()
             .at(200).driver("started").send(FULL, uid=UID2).respond().stop()],
        [clean().stop(),
         clean().stop().at(200).driver("started").send(FULL, uid=UID2).respond().stop(),
         clean().stop().at(200).driver("started").stop(),
         Log().driver("started").send(FULL).respond(assign_uid=UID2).stop()
             .at(200).driver("started").send(FULL).respond().stop()],
    ),
    "requests_uid_until_assigned": (
        [Log().send(FULL, flags=1).respond(assign_uid=UID2).send(uid=UID2, flags=0).send(uid=UID2, disconnect=True),
         Log().send(FULL, flags=1).respond().send(flags=1).respond().send(disconnect=True),
         Log().send(FULL, flags=1).respond().send(flags=1).respond(assign_uid=UID2).send(uid=UID2)],
        [Log().send(FULL).respond(),
         Log().send(FULL, flags=1).respond().send(flags=0),
         Log().send(FULL, flags=1).respond(assign_uid=UID2).send(uid=UID2, flags=1),
         Log("ws").send(FULL, flags=1).send(flags=1).respond(assign_uid=UID2)
         .server("s2a", msg=2, assign_uid=UID).send(uid=UID2, flags=0)],
    ),
    "uid_assigned_once": (
        [Log().send(FULL, flags=1).respond(assign_uid=UID2).send(uid=UID2)],
        [Log().send(FULL, flags=1).respond(),
         Log("ws").send(FULL, flags=1).send(flags=1).server("s2a", msg=1, assign_uid=UID2)
         .server("s2a", msg=2, assign_uid=V7)],
    ),
    "uid_not_requested_after_restart": (
        [Log().driver("started").send(FULL, flags=1).respond(assign_uid=UID2).send(uid=UID2).stop()
         .at(200).driver("started").send(FULL, uid=UID2).respond().stop()],
        [Log().driver("started").send(FULL, flags=1).respond(assign_uid=UID2).stop(),
         Log().driver("started").send(FULL, flags=1).respond().stop().at(200).driver("started").send(FULL).stop(),
         Log().driver("started").send(FULL, flags=1).respond(assign_uid=UID2).stop()
         .at(200).driver("started").send(FULL, uid=UID2, flags=1).respond().stop(),
         Log().driver("started").send(FULL, flags=1).respond(assign_uid=UID2).stop()
         .at(200).driver("started").send(FULL, uid=UID).respond().stop(),
         Log().driver("started").send(FULL, flags=1).respond(assign_uid=UID2).stop().at(200).driver("started").stop()],
    ),
    "connection_settings_applied": (
        [offered().settings("APPLYING").auth("new", switched=True).settings("APPLIED")],
        [clean(),
         offered().settings("APPLYING").settings("APPLIED"),
         offered().settings("APPLYING").auth("new", switched=True).send(),
         offered().settings("APPLIED").auth("new", switched=True)],
    ),
    "connection_settings_applying": (
        [offered().settings("APPLYING").auth("new").settings("APPLIED")],
        [offered().auth("new").settings("APPLIED"),
         offered().settings("APPLIED").settings("APPLYING")],
    ),
    "connection_settings_failed": (
        [offered().settings("APPLYING").auth("new", ok=False).auth("boot").settings("FAILED", error="refused")],
        [clean(),
         offered().settings("APPLYING").auth("new", ok=False).auth("boot").send(),
         offered().settings("APPLYING").auth("new", ok=False).settings("FAILED")],
    ),
    "never_locked_out": (
        [offered().auth("new", switched=True).stop().driver("started").auth("new").stop(),
         offered().auth("new", ok=False).auth("boot").stop()],
        [offered().auth("new", switched=True).stop().driver("started").auth("boot", ok=False).auth("new").stop(),
         offered().auth("new", switched=True).stop().driver("started").stop(),
         offered().auth("new", ok=False).stop()],
    ),
    "connection_settings_rotated": (
        [rotated().settings("APPLYING", hash_=THEN_HASH).auth("newer", switched=True)
         .settings("APPLIED", hash_=THEN_HASH).stop().driver("started").auth("newer").stop()],
        [offered().settings("APPLYING").auth("new", switched=True).settings("APPLIED"),
         rotated().settings("APPLYING", hash_=THEN_HASH).settings("APPLIED", hash_=THEN_HASH),
         rotated().auth("newer", switched=True).send(),
         rotated().settings("APPLIED", hash_=THEN_HASH).auth("newer", switched=True),
         rotated().auth("newer", switched=True).settings("APPLIED", hash_=THEN_HASH)
         .stop().driver("started").auth("new", ok=False).auth("newer").stop()],
    ),
    "connection_settings_moved": (
        [moved().settings("APPLYING").auth("new", switched=True, moved=True).settings("APPLIED")
         .stop().driver("started").auth("new", moved=True).stop()],
        [clean(),
         moved().auth("new", switched=True).settings("APPLIED"),
         moved().auth("boot", ok=True, moved=True).settings("APPLIED"),
         moved().settings("APPLIED").auth("new", switched=True, moved=True),
         moved().auth("new", switched=True, moved=True).send(),
         moved().auth("new", switched=True, moved=True).settings("APPLIED")
         .stop().driver("started").auth("boot", ok=False).auth("new", moved=True).stop()],
    ),
    "connected_over_tls": (
        [tls_hs().send(FULL, tls="TLS 1.3").respond().stop(),
         tls_hs(log=Log("ws").server("tls_handshake_failed", tcp=1, error="EOF", alert=None))
             .send(FULL, tls="TLS 1.2")],
        [Log().send(FULL, tls=None).respond(),
         tls_hs().send(FULL, tls=None).respond(),
         tls_hs(),
         tls_hs().server("tls_handshake_failed", tcp=2, error="remote error: tls: bad certificate",
                         alert="bad certificate").send(FULL, tls="TLS 1.3"),
         tls_hs().send(FULL, tls="TLS 1.0")],
    ),
    "rejects_untrusted_server": (
        [Log().server("tls_handshake_failed", tcp=1, error="remote error: tls: bad certificate",
                      alert="bad certificate").driver("connect_failed", error="x509").stop(),
         Log("ws").server("tls_handshake_failed", tcp=1, error="EOF", alert=None).stop(),
         # Finished the handshake, checked the certificate after, hung up: nothing sent.
         tls_hs().server("tls_closed", tcp=1, app_bytes=0, closed_by="client").stop(),
         tls_hs().server("tls_closed", tcp=1, app_bytes=0, closed_by="shutdown")],
        [Log().stop(),
         tls_hs().send(FULL, tls="TLS 1.3").respond().stop(),
         Log().server("tls_handshake_failed", tcp=1, error="EOF", alert=None).send(FULL, tls=None),
         tls_hs().server("tls_closed", tcp=1, app_bytes=180, closed_by="client").stop(),
         tls_hs().stop(),  # never closed: what it carried is unknown
         tls_hs().server("tls_closed", tcp=2, app_bytes=0, closed_by="client").stop()],
    ),
    "refuses_in_handshake": (
        [Log().server("tls_handshake_failed", tcp=1, error="remote error: tls: bad certificate",
                      alert="bad certificate").stop(),
         Log("ws").server("tls_handshake_failed", tcp=1, error="EOF", alert=None).stop()],
        [Log().stop(),
         tls_hs().server("tls_closed", tcp=1, app_bytes=0, closed_by="client").stop(),
         Log().server("tls_handshake_failed", tcp=1, error="EOF", alert=None)
              .server("tls_handshake", tcp=2, remote="127.0.0.1:5002", version="TLS 1.3", client_cert=False,
                      client_subject=None, client_verified=False)],
    ),
    "presented_client_cert": (
        [tls_hs(CLIENT).send(FULL, tls="TLS 1.3", client_subject="conformance-agent").respond()],
        [tls_hs().send(FULL, tls="TLS 1.3", client_subject=None).respond(),
         Log().server("tls_handshake_failed", tcp=1, error="tls: client didn't provide a certificate", alert=None),
         tls_hs(CLIENT | {"client_subject": "someone-else"})
             .send(FULL, tls="TLS 1.3", client_subject="someone-else"),
         tls_hs(CLIENT | {"client_verified": False})
             .send(FULL, tls="TLS 1.3", client_subject="conformance-agent"),
         tls_hs(CLIENT),
         tls_hs(CLIENT).send(FULL, tls="TLS 1.3", client_subject=None)],
    ),
    "went_through_proxy": (
        [proxied().tunnel("forward", "127.0.0.1:5001").send(FULL, remote="127.0.0.1:5001").respond()
                  .tunnel("forward", "127.0.0.1:5002").send(remote="127.0.0.1:5002").respond(),
         proxied(auth=None).tunnel("socks5", "127.0.0.1:5001", user="")
                  .send(FULL, remote="127.0.0.1:5001")],
        [Log().send(FULL, remote="127.0.0.1:5001").respond(),
         proxied().tunnel("connect", "127.0.0.1:5001").send(FULL, remote="127.0.0.1:6000").respond(),
         proxied().tunnel("connect", "127.0.0.1:5001", user="mallory").send(FULL, remote="127.0.0.1:5001"),
         proxied().server("proxy_refused", proxy="connect", id=1, reason="auth", detail="no credentials"),
         proxied().tunnel("connect", "127.0.0.1:5001")],
    ),
}


class BaselineTest(unittest.TestCase):
    def test_a_flaky_check_is_neither_a_regression_nor_an_improvement(self):
        import tempfile
        from run import compare_baseline

        def run(status):
            return [{"client": "c", "transport": "http",
                     "results": {"seq": {"status": status, "level": "MUST", "subject": "client"}}}]

        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "baseline.toml"
            path.write_text('["c/http"]\nfail = []\nwarn = []\n')
            self.assertEqual(len(compare_baseline(run("fail"), path)), 1)
            path.write_text('["c/http"]\nfail = ["seq"]\nwarn = []\n')
            self.assertEqual(len(compare_baseline(run("pass"), path)), 1)
            path.write_text('["c/http"]\nfail = []\nwarn = []\nflaky = ["seq"]\n')
            self.assertEqual(compare_baseline(run("fail"), path), [])
            self.assertEqual(compare_baseline(run("pass"), path), [])

    def test_ours_to_pass(self):
        from run import must_failures

        clients = {"up": {"upstream": True}, "own": {}}

        def rec(client, name, subject):
            return {"client": client, "transport": "http", "scenario": "s",
                    "results": {name: {"status": "fail", "level": "MUST", "subject": subject}}}

        flaky = {"up/http": {"flaky": ["goodbye"]}, "own/http": {"flaky": ["goodbye"]}}
        # A server check fails whichever client ran, unless that upstream client's race is flaky.
        self.assertEqual(len(must_failures([rec("up", "goodbye", "server")], clients, {})), 1)
        self.assertEqual(must_failures([rec("up", "goodbye", "server")], clients, flaky), [])
        # An upstream client's own failures follow the baseline, not this.
        self.assertEqual(must_failures([rec("up", "seq", "client")], clients, {}), [])
        # Ours fail whatever the baseline says.
        self.assertEqual(len(must_failures([rec("own", "goodbye", "server")], clients, flaky)), 1)
        self.assertEqual(len(must_failures([rec("own", "seq", "client")], clients, flaky)), 1)


class FullReportAfterRequestTest(unittest.TestCase):
    def check(self, log: Log):
        return CHECKS["full_report_after_request"].fn(log.trace())

    def test_a_request_the_client_could_not_answer_is_not_judged(self):
        self.assertEqual(self.check(clean()).status, "skip")
        # In reply to its goodbye, or written after it was told to stop.
        goodbye = Log().send(FULL).respond().send(disconnect=True).respond(report_full_state=True)
        self.assertEqual(self.check(goodbye).status, "skip")
        late = Log().driver("started").send(FULL).stop().respond(report_full_state=True)
        self.assertEqual(self.check(late).status, "skip")
        # The run ended, or the client was restarted, within a heartbeat and a half of it.
        self.assertEqual(self.check(Log().send(FULL).respond(report_full_state=True).at(102).stop()).status, "skip")
        restarted = Log().driver("started").send(FULL).respond(report_full_state=True).driver("started").send(FULL)
        self.assertEqual(self.check(restarted).status, "skip")

    def test_silence_fails_once_a_heartbeat_and_a_half_has_passed(self):
        # The same two seconds, with a heartbeat every second (the scenarios') and every two.
        for poll_s, status in ((1, "fail"), (2, "skip")):
            log = Log().send(FULL).respond(report_full_state=True).at(102).stop()
            log.driver_cfg = {"poll_s": poll_s}
            r = self.check(log)
            self.assertEqual(r.status, status, r.detail)
        log = Log().send(FULL).respond(report_full_state=True).at(102).stop()
        log.driver_cfg = {"poll_s": 1}
        self.assertIn("no full report in the 1.9 s", self.check(log).detail)

    def test_the_failure_names_what_is_missing(self):
        r = self.check(Log().send(FULL + ["effective_config"]).respond(report_full_state=True)
                       .send(["agent_description"]))
        self.assertEqual(r.status, "fail")
        self.assertIn("message 2, the first after message 1's response", r.detail)
        self.assertIn("without effective_config, health", r.detail)

    def test_each_request_is_judged(self):
        # Answered once, ignored the second time.
        twice = Log().send(FULL).respond(report_full_state=True).send(FULL).respond(report_full_state=True).send()
        self.assertEqual(self.check(twice).status, "fail")
        # One answered, one in reply to the goodbye: the first is the verdict.
        r = self.check(Log().send(FULL).respond(report_full_state=True).send(FULL).respond().driver("stopping")
                       .send(disconnect=True).respond(report_full_state=True).driver("stopped"))
        self.assertEqual((r.status, r.detail), ("pass", "1 ReportFullState"))

    def test_a_restarted_client_owes_what_it_has_reported_since(self):
        # The first run's remote config status isn't asked of the second, which never reported one.
        log = (Log().driver("started").send(FULL + ["remote_config_status"]).respond().stop()
               .driver("started").send(FULL).respond().send().respond(report_full_state=True).send(FULL))
        self.assertEqual(self.check(log).status, "pass")
        # Within a run it is.
        log = (Log().driver("started").send(FULL + ["remote_config_status"]).respond()
               .send().respond(report_full_state=True).send(FULL))
        self.assertEqual(self.check(log).status, "fail")

    def test_the_fields_a_server_takes_follow_its_capabilities(self):
        from checks import taken_by_server
        every = {"agent_description", "health", "effective_config", "remote_config_status", "package_statuses",
                 "available_components", "connection_settings_status", "custom_capabilities"}
        self.assertEqual(taken_by_server(Log().trace()), every - {"package_statuses"})
        offering = Log()
        offering.server_cfg = {"packages": {"offer": []}}
        self.assertEqual(taken_by_server(offering.trace()), every)
        self.assertEqual(taken_by_server(announcing(35, Log()).trace()),
                         every - {"package_statuses", "effective_config"})
        # A package status the default server doesn't take isn't owed.
        log = Log().send(FULL + ["package_statuses"]).respond(report_full_state=True).send(FULL)
        self.assertEqual(self.check(log).status, "pass")


class ReconnectBacksOffTest(unittest.TestCase):
    def check(self, log: Log):
        return CHECKS["reconnect_backs_off"].fn(log.trace())

    def test_skips_without_the_fault(self):
        self.assertEqual(self.check(dropping([0, 0.01, 0.02], fault=False)).status, "skip")
        # Or without a connection in its window: the client waited it out, or never came back.
        self.assertEqual(self.check(dropping([])).status, "skip")

    def test_too_few_is_judged_only_when_the_run_outlasts_the_observation(self):
        # The driver stopped 3 s after the first drop: not long enough to see a third connection.
        self.assertEqual(self.check(dropping([0, 1.2], stop_at=111.5)).status, "skip")
        # The fault's window closed 2 s after the first drop (server time: started at 100).
        self.assertEqual(self.check(dropping([0, 1.2], window=(8, 10))).status, "skip")
        # The window counts from the driver's first request (12 s after the server started): it
        # closes 37 s after the first drop, past the observation, so two connections are judged.
        self.assertEqual(self.check(dropping([0, 1.2], stop_at=200.0, window=(8, 45), contact=12.0)).status, "pass")
        # A window that closes 25 s after the first drop, counted from that first request.
        self.assertEqual(self.check(dropping([0, 1.2], stop_at=200.0, window=(8, 33), contact=12.0)).status, "skip")
        # A storm fails however short the run.
        self.assertEqual(self.check(dropping([i * 0.005 for i in range(50)], stop_at=109)).status, "fail")

    def test_growth_needs_four_gaps(self):
        # Two or three gaps say little with jitter and a slow connect; a client slower than 1 s,
        # 2 s, 4 s still backs off.
        for stop_at in (115, 150):
            r = self.check(dropping([0, 4.0, 8.0], stop_at=stop_at))
            self.assertEqual(r.status, "pass", r.detail)
        r = self.check(dropping([0, 2.0, 4.0, 5.9]))
        self.assertEqual(r.status, "pass", r.detail)
        r = self.check(dropping([0, 2.0, 4.0, 5.9, 8.8]))
        self.assertEqual(r.status, "fail", r.detail)
        r = self.check(dropping([0, 2.0, 4.0, 5.9, 9.0]))
        self.assertEqual(r.status, "pass", r.detail)

    def test_the_storm_detail_counts_it(self):
        r = self.check(dropping([i * 0.005 for i in range(400)]))
        self.assertIn("400 connections in 30s", r.detail)
        self.assertIn("at most 8", r.detail)

    def test_params(self):
        log = dropping([i * 2.0 for i in range(10)])
        log.params = {"backoff_max_attempts": 12, "backoff_min_growth_s": 0}
        self.assertEqual(self.check(log).status, "pass")

    def test_simulated_clients(self):
        """Seeded runs of a client that backs off as the kernel does (1 s to 60 s, equal jitter),
        from attempt 0 (the first connection the window saw was never answered) or 1 (it was), with
        the Java client's connect times (2.2 s for the first reconnect, then 0.7 s to 1.4 s) or
        none; and of clients that reconnect at once, as slowly as that."""
        import random

        def connect(rng, first, slow):
            return (rng.uniform(1.0, 2.2) if first else rng.uniform(0.7, 1.4)) if slow else rng.uniform(0, 0.05)

        for seed in range(500):
            rng = random.Random(seed)
            slow, attempt = rng.random() < 0.5, rng.choice([0, 1])
            at, offsets = 0.0, [0.0]
            while at < 40:
                base = min(60.0, 2.0 ** attempt)
                at += rng.uniform(base / 2, base) + connect(rng, len(offsets) == 1, slow)
                attempt += 1
                offsets.append(at)
            with self.subTest(seed=seed, client="backs off", slow=slow):
                r = self.check(dropping(offsets, stop_at=None))
                self.assertEqual(r.status, "pass", r.detail)
            at, offsets = 0.0, [0.0]
            while at < 40:
                at += connect(rng, len(offsets) == 1, slow=True)
                offsets.append(at)
            with self.subTest(seed=seed, client="storm"):
                self.assertEqual(self.check(dropping(offsets, stop_at=None)).status, "fail")


class ChecksTest(unittest.TestCase):
    def test_every_check_has_cases(self):
        # The core tap's check isn't over the event log: TapCheckTest has its cases.
        self.assertEqual(sorted(set(CHECKS) - {TAP_CHECK}), sorted(CASES))

    def test_every_check_can_pass_and_fail(self):
        for name, (passing, failing) in CASES.items():
            check = CHECKS[name]
            self.assertTrue(passing and failing, f"{name}: needs at least one passing and one failing trace")
            for i, log in enumerate(passing):
                with self.subTest(check=name, case=f"pass {i}"):
                    r = check.fn(log.trace())
                    self.assertEqual(r.status, "pass", r.detail)
            for i, log in enumerate(failing):
                with self.subTest(check=name, case=f"fail {i}"):
                    r = check.fn(log.trace())
                    self.assertEqual(r.status, "fail", r.detail)

    def test_every_check_covers_a_real_requirement(self):
        import tomllib
        # The requirements' list, where it is kept (beside the core's sources); elsewhere each id is
        # held to the shape of one.
        path = Path(__file__).resolve().parents[2] / "spec" / "requirements.toml"
        reqs = tomllib.loads(path.read_text()) if path.exists() else None
        ids = {r["id"] for r in reqs["req"]} | {r["id"] for r in reqs.get("rule", [])} if reqs else set()
        for check in CHECKS.values():
            self.assertTrue(check.covers, f"{check.name} covers nothing")
            for rid in check.covers:
                if reqs:
                    self.assertIn(rid, ids, f"{check.name}: unknown requirement {rid}")
                else:
                    self.assertRegex(rid, re.compile(r"^(OPAMP|RULE)-[A-Z0-9]+-\d+$"), check.name)


class TapCheckTest(unittest.TestCase):
    """run.py's core_tap: opamp-tap's verdict on a run's taps (its own rules are tested with the
    checker), here with a stand-in for it."""

    def test_the_checkers_verdict_is_the_checks(self):
        import os
        import tempfile
        from run import check_taps
        with tempfile.TemporaryDirectory() as d:
            tap = Path(d) / "core-0.tap"
            tap.write_bytes(b"")
            ok, bad = Path(d) / "ok", Path(d) / "bad"
            ok.write_text("#!/bin/sh\necho '1 sessions in 1 taps keep the effects contract'\n")
            bad.write_text("#!/bin/sh\necho 'FAIL x session 0'\necho '  after-cancel (call 3): ws_lost for 4'\n"
                           "exit 1\n")
            for f in (ok, bad):
                os.chmod(f, 0o755)
            self.assertEqual(check_taps([tap], ok).status, "pass")
            r = check_taps([tap], bad)
            self.assertEqual(r.status, "fail")
            self.assertIn("after-cancel", r.detail)
            # No tap at all: the client never tapped its core.
            self.assertEqual(check_taps([], ok).status, "fail")
            # The taps of a run the runner killed are marked so (their last call may have no
            # reply); the others aren't.
            later = Path(d) / "core-1.tap.42.1"
            later.write_bytes(b"")
            args = Path(d) / "args"
            echo = Path(d) / "echo"
            echo.write_text(f"#!/bin/sh\necho \"$@\" > {args}\n")
            os.chmod(echo, 0o755)
            self.assertEqual(check_taps([tap, later], echo, killed=[later]).status, "pass")
            self.assertEqual(args.read_text().split(), ["--quiet", "--killed", str(later), str(tap), str(later)])
        self.assertEqual(CHECKS[TAP_CHECK].level, "MUST")


if __name__ == "__main__":
    unittest.main()
