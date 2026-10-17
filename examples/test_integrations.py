# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Tests for the end-to-end checks every SDK integration passes (examples/integrations.py): each
check passes a good run and fails a bad one, and none passes for want of anything to look at.

    python3 -m unittest examples/test_integrations.py
"""
from __future__ import annotations

import os
import sys
import json
import re
import subprocess
import tempfile
import textwrap
import time
import unittest
import unittest.mock
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from integrations import (APP_KEY, POLICY_FILE, POLICY_V1, TAKES_KEYS, TRUTH, TRUTH_SWITCH, app_settings_scenario,  # noqa: E402
                          judge_truth, key_from_env, key_in_code, policy, scenario, truth_scenario,
                          judge_app_settings, judge_app_settings_restart, judge_first, judge_inventory,
                          judge_moved_start, judge_rotation, judge_own_key, judge_right_key, judge_second, judge_signals, judge_signals_restart,
                          judge_transport, judge_wrong_key, opamp_url, read_log, server_binary,
                          wait_for)
import integrations  # noqa: E402

UID = "01928f3a7c3e7a1b8c2d3e4f5a6b7c8d"
PING = "http://127.0.0.1:1/ping"


class Log:
    """A server log, one event at a time."""

    def __init__(self) -> None:
        self.events: list[dict] = []

    def a2s(self, t: float, uid: str = UID, status: str | None = None, error: str = "", config: bool = False,
            disconnect: bool = False, effective: dict | None = None, caps: int = 0,
            settings_status: bool = False) -> Log:
        """`effective`: the effective config's JSON files by name, as the server's log gives them."""
        e = {"event": "a2s", "t": t, "uid": uid, "disconnect": disconnect, "caps": caps,
             "fields": (["effective_config"] if config or effective is not None else [])
             + (["connection_settings_status"] if settings_status else [])}
        if status:
            e["remote_config_status"] = {"status": f"RemoteConfigStatuses_{status}", "hash": f"{t}", "error": error}
        if effective is not None:
            e["effective"] = effective
        self.events.append(e)
        return self

    def offer(self, t: float) -> Log:
        """The server sends a remote config."""
        self.events.append({"event": "s2a", "t": t, "offer_config": f"config-{t}"})
        return self

    def filled(self, t: float) -> Log:
        """The server fills a time into an offer (`{now+Ns}`), just before sending it."""
        self.events.append({"event": "offer_filled", "t": t, "on_message": 101})
        return self

    def health(self, t: float, healthy: bool = True, ok: int = 40, filtered: int = 50,
               signals: tuple[str, ...] = ("traces", "metrics", "logs")) -> Log:
        """A message carrying the agent's health: export health under `exporters`, each signal's
        child with `ok` exports and the logs' with `filtered` records dropped on purpose."""
        def child(name: str) -> dict:
            return {"healthy": True, "status": "ok", "last_error": "",
                    "attributes": {"exports.ok": ok, "exports.failed": 0, "items.failed": 0,
                                   "items.filtered": filtered if name == "logs" else 0}}
        exporters = {"healthy": True, "status": "ok", "last_error": "", "components": {s: child(s) for s in signals}}
        self.events.append({"event": "a2s", "t": t, "uid": UID, "disconnect": False, "fields": ["health"],
                            "health": {"healthy": healthy, "status": "", "last_error": "",
                                       "components": {"exporters": exporters}}})
        return self

    def ping(self, t: float) -> Log:
        self.events.append({"event": "stray_request", "t": t, "path": "/ping", "method": "GET"})
        return self

    def span(self, t: float, url: str = PING, ended: float | None = None) -> Log:
        """A span export arriving at `t`, of a span that ended at `ended` (just before, if not given)."""
        self.events.append({"event": "otlp", "t": t, "signal": "traces", "services": ["checkout"], "spans": [url],
                            "ends": [t - 0.1 if ended is None else ended]})
        return self

    def export(self, t: float, signal: str, count: int = 3) -> Log:
        """Metrics or log records arriving at `t`."""
        self.events.append({"event": "otlp", "t": t, "signal": signal, "services": ["checkout"], "spans": [],
                            "count": count})
        return self

    def exports(self, start: float, end: float, signals: tuple[str, ...] = ("metrics", "logs")) -> Log:
        t = start
        while t < end:
            for name in signals:
                self.export(t, name)
            t += 0.5
        return self

    def pings(self, start: float, end: float, also: str | None = None) -> Log:
        """A request to /ping every 0.2 s, and to `also` with each."""
        t = start
        while t < end:
            self.ping(t)
            if also:
                self.request(t + 0.01, also)
            t += 0.2
        return self

    def request(self, t: float, path: str) -> Log:
        self.events.append({"event": "stray_request", "t": t, "path": path, "method": "GET"})
        return self


REFUSED_SWITCH = "instrumentations: disabled: this SDK cannot apply the requested override"


def first(switch_at_once: bool, after_switch: float = 10, switches: bool = True) -> Log:
    """Run 1 as it should go: spans, then none, then spans again, then the switch; the application
    keeps making requests for `after_switch` seconds after the fifth answer. Without `switches`,
    the agent refuses the switch and the spans go on."""
    log = Log().a2s(0, config=True)
    # Each config is offered half a second before its answer arrives.
    log.offer(0.5).a2s(1, status="APPLIED").span(2).span(3)
    log.offer(9.5).a2s(10, status="APPLIED").pings(10, 20)
    log.offer(19.5).a2s(20, status="FAILED", error="head_sampling: ratio must be a number from 0 to 1").pings(20, 30)
    log.offer(29.5).a2s(30, status="APPLIED").span(32).span(35)
    if switches:
        log.offer(39.5).a2s(40, status="APPLIED").pings(40, 40 + after_switch)
    else:
        log.offer(39.5).a2s(40, status="FAILED", error=REFUSED_SWITCH).pings(40, 40 + after_switch)
    if not switch_at_once or not switches:
        log.span(45)
    log.a2s(40 + after_switch, disconnect=True)
    return log


def second(uid: str = UID, requests: int = 30, config: bool = True) -> Log:
    log = Log().a2s(0.5, uid=uid, config=config)
    log.pings(1, 1 + requests * 0.2)
    return log.a2s(10, uid=uid, disconnect=True)


def third(share: float = 0.25, metrics_off: bool = True, logs_off: bool = True, at_once: bool = False,
          info_under_floor: bool = False, info_back: bool = True, debug_spans: bool = True,
          ruled: bool = True, scoped: bool = True, scope_back: bool = True, span_name: str = "[REDACTED]",
          dropped_name: str = "GET", spans_a_second: int = 1, records_a_second: int = 1,
          health: bool = True, effective: dict | None = None, effective_after: dict | None = None) -> Log:
    """Run 3 as it should go: every signal; metrics and logs off; back on with `share` of the
    requests traced; a `warn` floor, under which the INFO lines (one a request) stop; rate 0 with a
    debug window of 10 s, traced until it ends, the floor left out and the INFO lines back; a rule
    of rate 0 for client spans and the application's loggers off: no request traced, no log lines;
    the URLs masked, spans named `span_name` and log lines again; the URLs dropped (spans named
    `dropped_name`) and both caps, `spans_a_second` and `records_a_second`; then every
    instrumentation off. Export health at start and at 120 s. With the third offer the agent
    reports `effective` as its effective config (by default the policy in force), and with the
    fourth `effective_after` (by default the next policy in force)."""
    third_effective = effective or in_force({"head_sampling": {"ratio": 0.25}})
    fourth_effective = effective_after or in_force({"head_sampling": {"ratio": 0.25}, "log_levels": {"level": "warn"}})
    log = Log().a2s(0, config=True)
    if health:
        log.health(0.5, ok=0, filtered=0)
    log.offer(0.5).a2s(1, status="APPLIED").pings(1, 25).exports(1, 25).span(3).span(5)
    log.offer(24.5).a2s(25, status="APPLIED").pings(25, 50).span(30).span(40)
    if not metrics_off:
        log.exports(30, 50, ("metrics",))
    if not logs_off:
        log.exports(30, 50, ("logs",))
    log.offer(49.5).a2s(50, status="APPLIED", effective=third_effective).pings(50, 75).exports(50, 75)
    t, n = 50.0, 0
    while t < 75:  # the requests made then, every 1/share-th traced
        if share and n % round(1 / share) == 0:
            log.span(t + 0.5, ended=t + 0.05)
        t, n = t + 0.2, n + 1
    log.offer(74.5).a2s(75, status="APPLIED", effective=fourth_effective).pings(75, 100).exports(75, 100, ("metrics",))
    if info_under_floor:
        log.exports(75, 100, ("logs",))
    # Debug mode: filled in at 99.4, so over at 109.4; then rate 0 until the next offer.
    log.filled(99.4).offer(99.5).a2s(100, status="APPLIED").pings(100, 150).exports(100, 150, ("metrics",))
    if info_back:
        log.exports(100, 150, ("logs",))
    if debug_spans:
        for t in range(1, 45):  # every 0.2 s from 100.2 to 108.8, arriving half a second later
            log.span(100 + t * 0.2 + 0.5, ended=100 + t * 0.2)
    if health:
        log.health(120)
    log.offer(149.5).a2s(150, status="APPLIED").pings(150, 175).exports(150, 175, ("metrics",))
    if not ruled:
        log.span(160).span(170)
    if not scoped:
        log.exports(150, 175, ("logs",))
    log.offer(174.5).a2s(175, status="APPLIED").pings(175, 200).exports(175, 200, ("metrics",))
    if scope_back:
        log.exports(175, 200, ("logs",))
    for t in range(176, 200, 2):
        log.span(t + 0.5, url=span_name, ended=t)
    log.offer(199.5).a2s(200, status="APPLIED").pings(200, 225).exports(200, 225, ("metrics",))
    for t in range(201, 225):  # each second, as many as the caps let through
        for n in range(spans_a_second):
            log.span(t + 0.5, url=dropped_name, ended=t + n * 0.01)
        if records_a_second:
            log.export(t + 0.3, "logs", count=records_a_second)
    log.offer(224.5).a2s(225, status="APPLIED").pings(225, 235)
    if not at_once:
        log.span(230)
    return log.a2s(235, disconnect=True)


def in_force(overrides: dict[str, dict]) -> dict:
    """An effective config as the server's log gives it: the policy with `overrides`, every control
    listed, as its only file."""
    return {POLICY_FILE: json.loads(policy(overrides, every=True))}


def fourth(spans: bool = False) -> Log:
    log = Log()
    t = 0.5
    while t < 20:
        log.a2s(t)
        t += 1.0
    log.pings(1, 20)
    if spans:
        log.span(10)
    return log.a2s(20, disconnect=True)


def inventory_sent(instrumentations: tuple[str, ...] = ("urllib", "requests"), gated: bool = True,
                   deactivate: str | None = "next start", sdk: dict | None = None) -> Log:
    """Run 1's start as an SDK reports its inventory: the hash first, then, asked, in full."""
    leaf = lambda **meta: {"metadata": meta} if meta else {}  # noqa: E731
    settings = {"sampling_rate": leaf(applies="at once"), "send_traces": leaf(applies="at once")}
    if deactivate:
        settings["deactivate_instrumentations"] = leaf(applies=deactivate)
    components = {
        "sdk": {"metadata": sdk if sdk is not None else {
            "integration.name": "opamp-otel", "integration.version": "0.4.0",
            "telemetry.sdk.language": "python", "telemetry.sdk.version": "1.45.1"}},
        "instrumentations": {"components": {n: leaf(version="0.56b0") for n in instrumentations}},
        "exporters": {"components": {"traces": {"components": {"otlp": leaf(gated=gated)}}}},
        "settings": {"components": settings},
    }
    log = Log().a2s(0, config=True)
    log.events.append({"event": "a2s", "t": 0.5, "uid": UID, "disconnect": False,
                       "fields": ["available_components"], "components": components})
    return log


def failed(results: list[tuple[str, bool]]) -> list[str]:
    return [what for what, ok in results if not ok]


class Transport(unittest.TestCase):
    def setUp(self) -> None:
        self.was = os.environ.pop("OPAMP_EXAMPLES_TRANSPORT", None)

    def tearDown(self) -> None:
        os.environ.pop("OPAMP_EXAMPLES_TRANSPORT", None)
        if self.was is not None:
            os.environ["OPAMP_EXAMPLES_TRANSPORT"] = self.was

    @staticmethod
    def over(*transports: str) -> list[dict]:
        return [{"event": "a2s", "t": i, "transport": t} for i, t in enumerate(transports)]

    def test_plain_http_by_default(self):
        self.assertEqual(opamp_url("4320"), "http://127.0.0.1:4320/v1/opamp")
        self.assertEqual(judge_transport(self.over("http", "http"))[0][1], True)

    def test_a_websocket_when_asked(self):
        os.environ["OPAMP_EXAMPLES_TRANSPORT"] = "websocket"
        self.assertEqual(opamp_url("4320", "host.docker.internal"), "ws://host.docker.internal:4320/v1/opamp")
        self.assertEqual(judge_transport(self.over("ws", "ws"))[0][1], True)

    def test_a_message_over_the_other_transport_fails(self):
        os.environ["OPAMP_EXAMPLES_TRANSPORT"] = "websocket"
        self.assertEqual(judge_transport(self.over("ws", "http"))[0][1], False)
        os.environ["OPAMP_EXAMPLES_TRANSPORT"] = "http"
        self.assertEqual(judge_transport(self.over("ws"))[0][1], False)

    def test_no_message_fails(self):
        self.assertEqual(judge_transport([])[0][1], False)

    def test_an_unknown_transport_is_refused(self):
        os.environ["OPAMP_EXAMPLES_TRANSPORT"] = "grpc"
        with self.assertRaises(SystemExit):
            opamp_url("4320")


class Wait(unittest.TestCase):
    def run_clock(self, holds_at: float, seconds: float, settle: float) -> float:
        now = [0.0]

        def sleep(s: float) -> None:
            now[0] += s

        wait_for(lambda: now[0] >= holds_at, seconds, settle, sleep=sleep, clock=lambda: now[0])
        return now[0]

    def test_settle_in_full_when_the_condition_holds_late(self):
        # Holds a second before the deadline: the settle still gets all ten seconds.
        self.assertEqual(self.run_clock(holds_at=149, seconds=150, settle=10), 159)

    def test_settle_when_the_condition_holds_early(self):
        self.assertEqual(self.run_clock(holds_at=20, seconds=150, settle=10), 30)

    def test_no_settle_when_the_condition_never_holds(self):
        self.assertEqual(self.run_clock(holds_at=1e9, seconds=150, settle=10), 150)


class Inventory(unittest.TestCase):
    def test_a_full_inventory_passes(self):
        self.assertEqual(failed(judge_inventory(inventory_sent().events, "urllib", False)), [])
        self.assertEqual(failed(judge_inventory(inventory_sent(("http", "net", "dns"), deactivate="at once").events,
                                                "http,net,dns", True)), [])

    def test_no_inventory_fails_every_check(self):
        got = failed(judge_inventory(Log().a2s(0, config=True).events, "urllib", False))
        self.assertIn("the inventory is sent in full", got)
        self.assertEqual(len(got), len(judge_inventory([], "urllib", False)))

    def test_a_hash_alone_is_no_inventory(self):
        log = Log().a2s(0, config=True)
        log.events.append({"event": "a2s", "t": 1, "uid": UID, "disconnect": False, "fields": ["available_components"]})
        self.assertIn("the inventory is sent in full", failed(judge_inventory(log.events, "urllib", False)))

    def test_the_instrumentation_run_1_switches_off_must_be_listed(self):
        got = failed(judge_inventory(inventory_sent(("http", "net")).events, "http,net,dns", False))
        self.assertTrue(any(f.startswith("the instrumentations run 1 switches off are listed") for f in got), got)

    def test_an_integration_that_cant_switch_needs_no_instrumentation_listed(self):
        log = inventory_sent((), deactivate=None)
        self.assertEqual(failed(judge_inventory(log.events, "urllib", False, switches=False)), [])

    def test_an_integration_built_on_the_checkout_leaves_its_version_out(self):
        unversioned = {"integration.name": "github.com/pydantic/opamp-clients/go/otel", "telemetry.sdk.language": "go",
                       "telemetry.sdk.version": "1.46.0"}
        self.assertEqual(failed(judge_inventory(inventory_sent(sdk=unversioned).events, "urllib", False,
                                                versioned=False)), [])
        what = "it names our package and the OpenTelemetry SDK, with versions"
        self.assertTrue(any(f.startswith(what) for f in failed(
            judge_inventory(inventory_sent(sdk=unversioned).events, "urllib", False))))
        # A version made up for a build that has none is as wrong as none where one is known.
        self.assertTrue(any(f.startswith(what) for f in failed(
            judge_inventory(inventory_sent(sdk={**unversioned, "integration.version": "(devel)"}).events,
                            "urllib", False, versioned=False))))

    def test_deactivate_must_apply_when_the_integration_says(self):
        self.assertIn("deactivate_instrumentations applies at once",
                      failed(judge_inventory(inventory_sent().events, "urllib", True)))

    def test_no_gated_traces_exporter_fails(self):
        got = failed(judge_inventory(inventory_sent(gated=False).events, "urllib", False))
        self.assertTrue(any(f.startswith("a traces exporter the traces export control reaches") for f in got), got)

    def test_an_sdk_entry_without_versions_fails(self):
        log = inventory_sent(sdk={"integration.name": "opamp-otel", "telemetry.sdk.language": "python"})
        got = failed(judge_inventory(log.events, "urllib", False))
        self.assertTrue(any(f.startswith("it names our package") for f in got), got)


class First(unittest.TestCase):
    def test_the_switch_offers_each_instrumentation_by_name(self):
        # Node.js switches three instrumentations off, given as "http,net,dns": the policy takes
        # an array of names, and refuses a name with a comma in it.
        last = json.loads(scenario("http,net,dns")["server"]["faults"][-1]["body"])
        self.assertEqual(last["instrumentations"], {"mode": "override", "disabled": ["http", "net", "dns"]})

    def test_a_good_run_passes(self):
        for at_once in (False, True):
            with self.subTest(switch_at_once=at_once):
                self.assertEqual(failed(judge_first(first(at_once).events, "checkout", at_once)), [])

    def test_spans_after_an_at_once_switch_fail(self):
        self.assertIn("the instrumentation switched off at once: no spans",
                      failed(judge_first(first(False).events, "checkout", True)))

    def test_an_at_once_switch_with_no_requests_after_it_fails(self):
        # Nothing after the switch to trace: no spans proves nothing.
        log = first(True, after_switch=1)
        self.assertNotEqual(failed(judge_first(log.events, "checkout", True)), [])

    def test_no_spans_with_the_traces_export_off_and_no_requests_fails(self):
        log = first(False)
        log.events = [e for e in log.events if not (e["event"] == "stray_request" and 13 < e["t"] < 30)]
        self.assertNotEqual(failed(judge_first(log.events, "checkout", False)), [])

    def test_a_span_that_ended_before_the_traces_export_went_off_may_arrive_after(self):
        # A batch on its way when the switch came, delivered late on a busy machine.
        log = first(False).span(15, ended=9.9)
        self.assertEqual(failed(judge_first(log.events, "checkout", False)), [])

    def test_a_span_still_queued_goes_out_once_the_traces_export_is_local_again(self):
        # Ended while switched off, still in the batch when the config switching it back (offered
        # at 29.5) is put in force: exported, as it should be, before the answer (30) arrives.
        for ended in (27.0, 29.7):
            with self.subTest(ended=ended):
                log = first(False).span(29.9, ended=ended)
                self.assertEqual(failed(judge_first(log.events, "checkout", False)), [])

    def test_a_span_that_arrives_before_the_traces_export_is_local_again_fails(self):
        log = first(False).span(29.4, ended=27.0)
        self.assertTrue(any(f.startswith("no span exported once the traces export is off")
                            for f in failed(judge_first(log.events, "checkout", False))))

    def test_a_span_that_ended_after_the_traces_export_went_off_fails_until_it_is_switched_back(self):
        for arrives in (12, 25, 29.4):
            with self.subTest(arrives=arrives):
                log = first(False).span(arrives, ended=11.5)
                self.assertTrue(any(f.startswith("no span exported once the traces export is off")
                                    for f in failed(judge_first(log.events, "checkout", False))))

    def test_a_span_that_ended_after_an_at_once_switch_fails_whenever_it_arrives(self):
        log = first(True).span(46, ended=41.5)
        self.assertIn("the instrumentation switched off at once: no spans",
                      failed(judge_first(log.events, "checkout", True)))

    def test_a_log_without_end_times_counts_arrivals(self):
        log = first(False)
        log.events.append({"event": "otlp", "t": 15, "signal": "traces", "services": ["checkout"], "spans": [PING]})
        self.assertTrue(any(f.startswith("no span exported once the traces export is off")
                            for f in failed(judge_first(log.events, "checkout", False))))

    def test_where_the_switch_is_refused_the_spans_go_on(self):
        self.assertEqual(failed(judge_first(first(False, switches=False).events, "checkout", False, switches=False)), [])
        # Carried out where it cannot be: the fifth answer is APPLIED, not FAILED.
        self.assertIn("five configs answered APPLIED, APPLIED, FAILED, APPLIED, FAILED",
                      failed(judge_first(first(False).events, "checkout", False, switches=False)))

    def test_a_refusal_of_the_switch_for_another_reason_fails(self):
        log = first(False, switches=False)
        answer = [e for e in log.events if e.get("remote_config_status")][-1]
        answer["remote_config_status"]["error"] = "instrumentations: takes effect at the next start"
        self.assertTrue(any(f.startswith("the switch it cannot carry out is refused")
                            for f in failed(judge_first(log.events, "checkout", False, switches=False))))

    def test_a_failure_naming_another_control_fails(self):
        log = first(False)
        answer = next(e for e in log.events if e.get("remote_config_status", {}).get("status", "").endswith("FAILED"))
        answer["remote_config_status"]["error"] = "signal_exports.traces: enabled must be a boolean"
        self.assertTrue(any(f.startswith("the FAILED names the control")
                            for f in failed(judge_first(log.events, "checkout", False))))

    def test_a_null_span_list_is_not_a_crash(self):
        log = first(False)
        log.events.append({"event": "otlp", "t": 5, "signal": "traces", "services": None, "spans": None})
        self.assertEqual(failed(judge_first(log.events, "checkout", False)), [])


class Second(unittest.TestCase):
    one = first(False).events

    def test_a_good_run_passes(self):
        self.assertEqual(failed(judge_second(self.one, second().events)), [])

    def test_spans_fail(self):
        two = second().span(3)
        self.assertIn("the instrumentation switched off at the next start: no spans",
                      failed(judge_second(self.one, two.events)))

    def test_where_the_switch_is_refused_the_spans_go_on(self):
        # An application that instruments itself in code: nothing can switch an instrumentation off.
        self.assertEqual(failed(judge_second(self.one, second().span(3).events, switches=False)), [])
        self.assertIn("the instrumentation switch was refused (the application instruments itself): spans go on",
                      failed(judge_second(self.one, second().events, switches=False)))

    def test_another_uid_fails(self):
        self.assertNotEqual(failed(judge_second(self.one, second(uid="ff" * 16).events)), [])

    def test_an_empty_uid_both_times_fails(self):
        one = [dict(e, uid="") for e in self.one]
        self.assertNotEqual(failed(judge_second(one, second(uid="").events)), [])

    def test_a_short_uid_fails(self):
        one = [dict(e, uid="abcd") for e in self.one]
        self.assertNotEqual(failed(judge_second(one, second(uid="abcd").events)), [])

    def test_too_few_requests_fail(self):
        # No spans because the application barely ran proves nothing.
        self.assertNotEqual(failed(judge_second(self.one, second(requests=2).events)), [])

    def test_requests_before_the_agent_connected_do_not_count(self):
        two = Log().pings(0, 5).a2s(5.5, config=True).a2s(6, disconnect=True)
        self.assertNotEqual(failed(judge_second(self.one, two.events)), [])

    def test_no_effective_config_fails(self):
        self.assertNotEqual(failed(judge_second(self.one, second(config=False).events)), [])


UNSUPPORTED = "this SDK cannot apply the requested override"


def truth(refused: frozenset[str] = frozenset(), local: frozenset[str] = frozenset(),
          unchanged: frozenset[str] = frozenset(), switch_at_once: bool = False, error: str | None = None,
          baseline: str = "APPLIED") -> Log:
    """check_truth's run as it should go, a policy every 10 s: the baseline (every signal), then
    each override of TRUTH alone, carried out. A setting in `refused` is FAILED as an override the
    SDK can't carry out (or with `error`), one in `local` is APPLIED and reported local; the
    windows of both look like the baseline. One in `unchanged` is APPLIED and reported in force,
    and its window looks like the baseline all the same (the defect)."""
    judged = TRUTH + ([TRUTH_SWITCH] if switch_at_once else [])
    log = Log().a2s(0, config=True)
    for i, (setting, control, fields) in enumerate(judged):
        start, end = 10.0 * i + 10, 10.0 * i + 20
        overrides = {control: fields} if control and setting not in local | refused else {}
        effective = {POLICY_FILE: json.loads(policy(overrides, 2))}
        if setting in refused:
            why = error or f"{control}: {next(iter(fields))}: {UNSUPPORTED}"
            log.offer(start - 0.5).a2s(start, status="FAILED", error=why, effective=effective)
        else:
            log.offer(start - 0.5).a2s(start, status=baseline if setting is None else "APPLIED", effective=effective)
        shown = setting is None or setting in refused | local | unchanged
        log.pings(start, end)
        t = start
        while t < end:
            for signal in ("metrics", "logs"):
                if shown or setting not in (f"send_{signal}", "send_logs_level", "send_logs_level_by_scope",
                                            "send_logs_per_second"):
                    log.export(t + 0.3, signal)
            t += 0.5
        if not shown and setting == "send_logs_per_second":
            for second in range(int(start), int(end)):
                log.export(second + 0.3, "logs", count=1)
        name = {"mask_attributes": "[REDACTED]", "drop_attributes": "GET"}.get(setting, PING) if not shown else PING
        silent = ("send_traces", "sampling_rate", "sampling_rules", TRUTH_SWITCH[0])
        t = start
        while t < end:
            capped = setting == "sampling_traces_per_second" and not shown and round(t * 5) % 5
            if (shown or setting not in silent) and not capped:
                log.span(t + 0.5, url=name, ended=t + 0.05)
            t += 0.2
    return log.a2s(10.0 * len(judged) + 10, disconnect=True)


class Truth(unittest.TestCase):
    def test_every_setting_carried_out_passes(self):
        for at_once in (False, True):
            with self.subTest(switch_at_once=at_once):
                self.assertEqual(failed(judge_truth(truth(switch_at_once=at_once).events, set(), at_once)), [])

    def test_a_setting_refused_or_reported_local_passes_where_the_setup_expects_it_set_aside(self):
        refused = frozenset({"send_traces", "mask_attributes"})
        local = frozenset({"send_logs_per_second", "sampling_rate"})
        self.assertEqual(failed(judge_truth(truth(refused, local).events, set(refused | local))), [])

    def test_a_refusal_of_a_signal_or_the_head_sampling_names_the_control_alone(self):
        for setting, control in (("send_traces", "signal_exports.traces"), ("sampling_rate", "head_sampling")):
            with self.subTest(setting=setting):
                log = truth(frozenset({setting}), error=f"{control}: {UNSUPPORTED}")
                self.assertEqual(failed(judge_truth(log.events, {setting})), [])

    def test_a_setting_applied_in_force_and_not_carried_out_fails(self):
        for setting, _, _ in TRUTH[1:]:
            with self.subTest(setting=setting):
                out = failed(judge_truth(truth(unchanged=frozenset({setting})).events, set()))
                self.assertEqual(len(out), 1)
                self.assertTrue(out[0].startswith(f"{setting}: APPLIED, in force, and the telemetry didn't change"))

    def test_spans_after_the_live_switch_fail(self):
        events = truth(unchanged=frozenset({TRUTH_SWITCH[0]}), switch_at_once=True).events
        self.assertTrue(failed(judge_truth(events, set(), True))[0].startswith(f"{TRUTH_SWITCH[0]}: APPLIED, in"))

    def test_more_set_aside_than_the_setup_expects_fails(self):
        out = failed(judge_truth(truth(local=frozenset({"send_traces"})).events, set()))
        self.assertEqual(len(out), 1)
        self.assertIn("not expected: ['send_traces']", out[0])

    def test_fewer_set_aside_than_the_setup_expects_fails(self):
        out = failed(judge_truth(truth().events, {"send_logs"}))
        self.assertEqual(len(out), 1)
        self.assertIn("expected, carried out: ['send_logs']", out[0])

    def test_a_policy_refused_for_another_reason_fails(self):
        out = failed(judge_truth(truth(frozenset({"send_logs"}), error="log_levels: level: x").events, {"send_logs"}))
        self.assertEqual(len(out), 2)
        self.assertTrue(out[0].startswith("send_logs: refused as an override the SDK can't carry out"))

    def test_an_answer_neither_applied_nor_failed_fails(self):
        log = truth()
        for e in log.events:
            if e.get("remote_config_status") and e["t"] == 20:
                e["remote_config_status"]["status"] = "RemoteConfigStatuses_UNSET"
        self.assertIn("send_traces: answered APPLIED or FAILED, not UNSET", failed(judge_truth(log.events, set())))

    def test_an_override_reported_with_other_values_fails(self):
        log = truth()
        for e in log.events:
            if e.get("remote_config_status") and e["t"] == 50:  # head_sampling, ratio 0 sent
                e["effective"] = {POLICY_FILE: json.loads(policy({"head_sampling": {"ratio": 1}}, 2))}
        self.assertEqual(failed(judge_truth(log.events, set())),
                         ["sampling_rate: APPLIED, and reported in force with other values than sent"])

    def test_names_reported_sorted_are_the_names_sent(self):
        # The core reports attribute redaction's names sorted: ["http.url", "url.full"].
        log = truth()
        start = 10.0 * [s for s, _, _ in TRUTH].index("mask_attributes") + 10
        for e in log.events:
            if e.get("remote_config_status") and e["t"] == start:
                e["effective"] = {POLICY_FILE: json.loads(policy({"attribute_redaction": {"mask": ["http.url", "url.full"]}}, 2))}
        self.assertEqual(failed(judge_truth(log.events, set())), [])

    def test_rules_reported_with_their_keys_in_another_order_are_the_rules_sent(self):
        log = truth()
        i = [s for s, _, _ in TRUTH].index("sampling_rules")
        rules = [{k: v for k, v in reversed(list(r.items()))} for r in TRUTH[i][2]["rules"]]
        for e in log.events:
            if e.get("remote_config_status") and e["t"] == 10.0 * i + 10:
                e["effective"] = {POLICY_FILE: json.loads(policy({"sampling_rules": {"rules": rules}}, 2))}
        self.assertEqual(failed(judge_truth(log.events, set())), [])

    def test_a_refused_baseline_fails(self):
        out = failed(judge_truth(truth(baseline="FAILED").events, set()))
        self.assertEqual(out, ["the baseline APPLIED"])

    def test_a_window_with_too_few_requests_shows_nothing_carried_out(self):
        log = truth()
        log.events = [e for e in log.events if not (e["event"] == "stray_request" and 20 < e["t"] < 30)]
        self.assertTrue(any("too few to tell" in f for f in failed(judge_truth(log.events, set()))))

    def test_no_baseline_telemetry_fails(self):
        log = truth()
        log.events = [e for e in log.events if not (e["event"] == "otlp" and e["signal"] == "metrics" and e["t"] < 20)]
        self.assertEqual(failed(judge_truth(log.events, set())), ["baseline: metrics arrive"])

    def test_a_span_two_exporters_send_counts_once_against_the_cap(self):
        log = truth()
        log.events += [{**e, "t": e["t"] + 0.1, "ends": [e["ends"][0] + 0.001]} for e in list(log.events)
                       if e["event"] == "otlp" and e["signal"] == "traces" and 110 < e["t"] < 120]
        self.assertEqual(failed(judge_truth(log.events, set())), [])

    def test_the_scenario_offers_each_policy_once_in_order(self):
        for at_once in (False, True):
            faults = truth_scenario(at_once)["server"]["faults"]
            judged = TRUTH + ([TRUTH_SWITCH] if at_once else [])
            self.assertEqual([f["config_key"] for f in faults], [POLICY_FILE] * len(judged))
            self.assertEqual([json.loads(f["body"]) for f in faults],
                             [json.loads(policy({c: f} if c else {}, 2)) for _, c, f in judged])
            self.assertEqual(len({f["on_message"] for f in faults}), len(faults))


class Signals(unittest.TestCase):
    def test_a_good_run_passes(self):
        for at_once in (False, True):
            with self.subTest(switch_at_once=at_once):
                self.assertEqual(failed(judge_signals(third(at_once=at_once).events, "checkout", at_once)), [])

    def test_no_effective_config_fails(self):
        log = third()
        for e in log.events:
            e.pop("effective", None)
        got = failed(judge_signals(log.events, "checkout", False))
        self.assertTrue(any(f.startswith("the effective config reports the policy in force") for f in got))
        self.assertTrue(any(f.startswith("then the next policy") for f in got))

    def test_an_effective_config_of_another_policy_fails(self):
        for wrong in (in_force({"head_sampling": {"ratio": 0.5}}),
                      in_force({"head_sampling": {"ratio": 0.25}, "debug": {"until": "2026-10-14T12:10:00Z"}}),
                      in_force({"head_sampling": {"ratio": 0.25}, "signal_exports.logs": {"enabled": False}}),
                      {"settings.json": {"sampling_rate": "0.25"}},
                      in_force({"head_sampling": {"ratio": 0.25}}) | {"other.json": {}}):
            with self.subTest(wrong=wrong):
                self.assertTrue(any(f.startswith("the effective config reports the policy in force")
                                    for f in failed(judge_signals(third(effective=wrong).events, "checkout", False))))

    def test_the_policy_before_still_reported_once_the_level_is_set_fails(self):
        kept = in_force({"head_sampling": {"ratio": 0.25}})
        self.assertTrue(any(f.startswith("then the next policy")
                            for f in failed(judge_signals(third(effective_after=kept).events, "checkout", False))))

    def test_the_effective_policy_names_its_schema(self):
        self.assertEqual(in_force({})[POLICY_FILE]["schema"], POLICY_V1)

    def test_metrics_after_the_metrics_export_is_off_fail(self):
        self.assertTrue(any(f.startswith("no metrics exported")
                            for f in failed(judge_signals(third(metrics_off=False).events, "checkout", False))))

    def test_logs_after_the_logs_export_is_off_fail(self):
        self.assertTrue(any(f.startswith("no logs exported")
                            for f in failed(judge_signals(third(logs_off=False).events, "checkout", False))))

    def test_an_export_on_its_way_when_switched_off_may_arrive(self):
        log = third().export(26, "metrics").export(27.5, "logs")
        self.assertEqual(failed(judge_signals(log.events, "checkout", False)), [])

    def test_an_empty_export_is_not_one(self):
        log = third().export(40, "metrics", count=0)
        self.assertEqual(failed(judge_signals(log.events, "checkout", False)), [])

    def test_no_metrics_at_all_fails(self):
        log = third()
        log.events = [e for e in log.events if e.get("signal") != "metrics"]
        self.assertIn("metrics arrive", failed(judge_signals(log.events, "checkout", False)))

    def test_info_lines_under_a_warn_floor_fail(self):
        self.assertTrue(any(f.startswith("no INFO log lines under the log level warn")
                            for f in failed(judge_signals(third(info_under_floor=True).events, "checkout", False))))

    def test_info_lines_not_back_once_the_floor_is_left_out_fail(self):
        self.assertTrue(any(f.startswith("INFO log lines again once log_levels is local")
                            for f in failed(judge_signals(third(info_back=False).events, "checkout", False))))

    def test_no_info_lines_before_the_floor_fails(self):
        log = third()
        log.events = [e for e in log.events if not (e.get("signal") == "logs" and 50 < e["t"] < 75)]
        self.assertTrue(any(f.startswith("INFO log lines arrive before the log level is set")
                            for f in failed(judge_signals(log.events, "checkout", False))))

    def test_records_queued_before_the_floor_may_arrive(self):
        log = third().export(76, "logs").export(77.5, "logs")
        self.assertEqual(failed(judge_signals(log.events, "checkout", False)), [])

    def test_a_few_warnings_under_the_floor_pass(self):
        log = third().export(85, "logs", count=1).export(95, "logs", count=1)
        self.assertEqual(failed(judge_signals(log.events, "checkout", False)), [])

    def test_no_requests_under_the_floor_fails(self):
        log = third()
        log.events = [e for e in log.events if not (e["event"] == "stray_request" and 75 < e["t"] < 100)]
        self.assertTrue(any(f.startswith("no INFO log lines under the log level warn")
                            for f in failed(judge_signals(log.events, "checkout", False))))

    def test_every_request_traced_at_a_quarter_fails(self):
        self.assertTrue(any(f.startswith("head_sampling 0.25")
                            for f in failed(judge_signals(third(share=1).events, "checkout", False))))

    def test_no_request_traced_at_a_quarter_fails(self):
        self.assertTrue(any(f.startswith("head_sampling 0.25")
                            for f in failed(judge_signals(third(share=0).events, "checkout", False))))

    def test_spans_after_an_at_once_switch_fail(self):
        self.assertTrue(any(f.startswith("every instrumentation switched off at once")
                            for f in failed(judge_signals(third().events, "checkout", True))))

    def test_a_failed_config_fails(self):
        log = third()
        answer = next(e for e in log.events if e.get("remote_config_status"))
        answer["remote_config_status"]["status"] = "RemoteConfigStatuses_FAILED"
        self.assertIn("nine configs answered APPLIED", failed(judge_signals(log.events, "checkout", False)))


def fails(log: Log, start: str, at_once: bool = False) -> bool:
    """Whether a check whose description starts with `start` fails on run 3's `log`."""
    return any(f.startswith(start) for f in failed(judge_signals(log.events, "checkout", at_once)))


class SignalsDebug(unittest.TestCase):
    def test_no_span_in_the_window_fails(self):
        self.assertTrue(fails(third(debug_spans=False), "debug: traced in the window"))

    def test_a_span_once_the_window_is_over_fails(self):
        self.assertTrue(fails(third().span(125, ended=124.9), "debug: no span once the window is over"))

    def test_a_span_ending_just_past_the_rounded_time_passes(self):
        # The time is rounded up to a whole second: a span may end up to a second past 109.4.
        self.assertFalse(fails(third().span(111, ended=110.3), "debug:"))

    def test_a_span_of_the_window_arriving_after_it_passes(self):
        self.assertEqual(failed(judge_signals(third().span(115, ended=108).events, "checkout", False)), [])

    def test_the_server_offering_again_in_the_window_fails(self):
        # Debug mode must end on the agent's own timer, not on a word from the server.
        self.assertTrue(fails(third().offer(112), "debug: no span once the window is over"))

    def test_no_requests_after_the_window_fails(self):
        log = third()
        log.events = [e for e in log.events if not (e["event"] == "stray_request" and 110 < e["t"] < 150)]
        self.assertTrue(fails(log, "debug: no span once the window is over"))

    def test_without_the_servers_word_the_offer_times_it(self):
        log = third()
        log.events = [e for e in log.events if e["event"] != "offer_filled"]
        self.assertEqual(failed(judge_signals(log.events, "checkout", False)), [])


class SignalsTelemetry(unittest.TestCase):
    def test_a_request_traced_under_the_rule_fails(self):
        self.assertTrue(fails(third(ruled=False), "sampling_rules"))

    def test_a_span_without_a_url_under_the_rule_passes(self):
        # Not a request's span: the rule is for client spans, and the sink names those by a URL.
        self.assertFalse(fails(third().span(165, url="tick"), "sampling_rules"))

    def test_no_requests_under_the_rule_fails(self):
        log = third()
        log.events = [e for e in log.events if not (e["event"] == "stray_request" and 150 < e["t"] < 175)]
        self.assertTrue(fails(log, "sampling_rules"))

    def test_log_lines_with_the_loggers_off_fail(self):
        self.assertTrue(fails(third(scoped=False), "log_levels scopes: no log lines"))

    def test_log_lines_not_back_once_the_levels_are_left_out_fail(self):
        self.assertTrue(fails(third(scope_back=False), "log_levels scopes: log lines again"))

    def test_a_url_shown_under_the_mask_fails(self):
        self.assertTrue(fails(third(span_name=PING), "attribute_redaction mask"))

    def test_no_span_under_the_mask_fails(self):
        log = third()
        log.events = [e for e in log.events if not (e.get("spans") == ["[REDACTED]"])]
        self.assertTrue(fails(log, "attribute_redaction mask"))

    def test_a_masked_span_beside_one_without_a_url_passes(self):
        self.assertFalse(fails(third().span(185, url="tick"), "attribute_redaction mask"))

    def test_a_dropped_url_still_shown_fails(self):
        self.assertTrue(fails(third(dropped_name=PING), "attribute_redaction drop"))

    def test_a_masked_url_under_the_drop_fails(self):
        self.assertTrue(fails(third(dropped_name="[REDACTED]"), "attribute_redaction drop"))

    def test_a_masked_span_queued_before_the_drop_may_arrive(self):
        # Ended under the mask, exported under the drop: it counts for neither step's check.
        self.assertEqual(failed(judge_signals(third().span(202, url="GET", ended=199).events, "checkout", False)), [])

    def test_no_span_under_the_drop_fails(self):
        self.assertTrue(fails(third(spans_a_second=0), "attribute_redaction drop"))

    def test_too_many_spans_under_the_trace_cap_fail(self):
        self.assertTrue(fails(third(spans_a_second=3), "trace_rate_limit"))

    def test_no_span_under_the_trace_cap_fails(self):
        self.assertTrue(fails(third(spans_a_second=0), "trace_rate_limit"))

    def test_too_many_records_under_the_log_cap_fail(self):
        self.assertTrue(fails(third(records_a_second=3), "log_rate_limit"))

    def test_no_record_under_the_log_cap_fails(self):
        self.assertTrue(fails(third(records_a_second=0), "log_rate_limit"))

    def test_too_few_requests_for_the_caps_to_mean_anything_fails(self):
        log = third()
        log.events = [e for e in log.events if not (e["event"] == "stray_request" and 200 < e["t"] < 225
                                                     and round(e["t"] * 5) % 4)]
        self.assertTrue(fails(log, "trace_rate_limit"))
        self.assertTrue(fails(log, "log_rate_limit"))


class SignalsHealth(unittest.TestCase):
    def test_no_health_fails(self):
        self.assertTrue(fails(third(health=False), "export health: an exporters entry"))

    def test_the_application_unhealthy_fails(self):
        self.assertTrue(fails(third().health(130, healthy=False), "export health: an exporters entry"))

    def test_a_signal_missing_fails(self):
        log = third(health=False).health(0.5, ok=0, filtered=0, signals=("traces", "metrics"))
        log.health(120, signals=("traces", "metrics"))
        self.assertTrue(fails(log, "export health: an exporters entry"))

    def test_no_successful_export_counted_fails(self):
        log = third(health=False).health(0.5, ok=0, filtered=0).health(120, ok=0)
        self.assertTrue(fails(log, "export health counts"))

    def test_no_record_filtered_counted_fails(self):
        log = third(health=False).health(0.5, ok=0, filtered=0).health(120, filtered=0)
        self.assertTrue(fails(log, "export health counts"))

    def test_a_report_at_start_alone_fails(self):
        self.assertTrue(fails(third(health=False).health(0.5, ok=0, filtered=0), "export health counts"))

    def test_counters_as_strings_are_read(self):
        log = third(health=False).health(0.5, ok=0, filtered=0)
        log.health(120)
        attributes = log.events[-1]["health"]["components"]["exporters"]["components"]
        attributes["traces"]["attributes"]["exports.ok"] = "40"
        attributes["logs"]["attributes"]["items.filtered"] = "50"
        self.assertFalse(fails(log, "export health"))


class SignalsRestart(unittest.TestCase):
    def test_a_good_run_passes(self):
        self.assertEqual(failed(judge_signals_restart(fourth().events)), [])

    def test_no_requests_fail(self):
        log = Log()
        log.a2s(0.5).a2s(20, disconnect=True)
        self.assertIn("every instrumentation switched off at the next start: no spans",
                      failed(judge_signals_restart(log.events)))

    def test_spans_fail(self):
        self.assertIn("every instrumentation switched off at the next start: no spans",
                      failed(judge_signals_restart(fourth(spans=True).events)))


def refused(n: int = 4) -> Log:
    """Run 5 as it should go: requests refused, the application traced all the same."""
    log = Log().pings(0, 10).span(2).span(5)
    for i in range(n):
        log.events.append({"event": "request_rejected", "t": 1 + 2 * i, "status": 401})
    return log


def accepted(every: float = 1.0, caps: int = 0x1 | TAKES_KEYS, promoted: bool = True,
             statuses: bool = True) -> Log:
    """Run 6 as it should go: the agent, declaring it takes keys, is offered one on its first
    message (and the server's interval, 30 s, with it); it says APPLYING, tries the key, and says
    APPLIED with it, which the server confirms."""
    log = Log().a2s(0.5, config=True, caps=caps)
    log.a2s(0.7, caps=caps, settings_status=statuses).a2s(0.9, caps=caps, settings_status=statuses)
    if promoted:
        log.events.append({"event": "key_promoted", "t": 0.9, "uid": UID})
    t = 1.5
    while t < 10:
        log.a2s(t, caps=caps)
        t += every
    return log.pings(0, 10).span(3).a2s(10, disconnect=True, caps=caps)


def again(every: float = 1.0) -> Log:
    """Run 6's next start as it should go: on the key it kept, every second, as set up to."""
    log = Log().a2s(0.5, config=True, caps=0x1 | TAKES_KEYS, settings_status=True)
    t = 1.5
    while t < 10:
        log.a2s(t, caps=0x1 | TAKES_KEYS)
        t += every
    return log.pings(0, 10).span(3).a2s(10, disconnect=True)


WARNING = "OpAMP: can't reach ... (HTTP 401): check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>)"


class WrongKey(unittest.TestCase):
    def test_a_good_run_passes(self):
        self.assertEqual(failed(judge_wrong_key(refused().events, WARNING)), [])

    def test_nothing_refused_fails(self):
        self.assertNotEqual(failed(judge_wrong_key(refused(0).events, WARNING)), [])

    def test_a_message_accepted_fails(self):
        self.assertNotEqual(failed(judge_wrong_key(refused().a2s(3).events, WARNING)), [])

    def test_an_application_left_untraced_fails(self):
        log = refused()
        log.events = [e for e in log.events if e["event"] != "otlp"]
        self.assertIn("the application's requests are traced all the same",
                      failed(judge_wrong_key(log.events, WARNING)))

    def test_a_silent_agent_fails(self):
        self.assertIn("the agent's warning names OTEL_OPAMP_HEADERS",
                      failed(judge_wrong_key(refused().events, "OpAMP: can't reach the server")))

    def test_a_key_in_code_is_named_and_not_the_variable(self):
        warning = "OpAMP: can't reach ... (HTTP 401): check WithHeaders (Authorization=Bearer <token>)"
        self.assertEqual(failed(judge_wrong_key(refused().events, warning, "WithHeaders")), [])
        # The variable named when the key came from code: the warning sends the user the wrong way.
        self.assertIn("and not OTEL_OPAMP_HEADERS, which the key given in code wins over",
                      failed(judge_wrong_key(refused().events, WARNING + " WithHeaders", "WithHeaders")))
        self.assertIn("the agent's warning names WithHeaders",
                      failed(judge_wrong_key(refused().events, WARNING, "WithHeaders")))

    def test_the_key_in_code_runs_beside_the_other_key_in_the_variable(self):
        key = key_in_code("CHECKOUT_OPAMP_KEY")
        self.assertEqual(key("wrong"), {"CHECKOUT_OPAMP_KEY": "wrong",
                                        "OTEL_OPAMP_HEADERS": "Authorization=Bearer%20example"})
        self.assertEqual(key("example"), {"CHECKOUT_OPAMP_KEY": "example",
                                          "OTEL_OPAMP_HEADERS": "Authorization=Bearer%20wrong"})
        self.assertEqual(key_from_env("wrong"), {"OTEL_OPAMP_HEADERS": "Authorization=Bearer%20wrong"})


class RightKey(unittest.TestCase):
    def test_a_good_run_passes(self):
        self.assertEqual(failed(judge_right_key(accepted().events)), [])

    def test_a_refused_request_fails(self):
        log = accepted()
        log.events.append({"event": "request_rejected", "t": 4, "status": 401})
        self.assertNotEqual(failed(judge_right_key(log.events)), [])

    def test_the_key_offers_interval_passes(self):
        # The key's offer carries the server's interval: the first start checks in at it.
        self.assertEqual(failed(judge_right_key(accepted(every=30).events)), [])

    def test_no_effective_config_fails(self):
        log = accepted()
        log.events[0]["fields"] = []
        self.assertIn("the effective config is reported", failed(judge_right_key(log.events)))

    def test_an_agent_that_doesnt_say_it_takes_keys_fails(self):
        # As on main: AcceptsRemoteConfig alone, so no key is offered, none reported, none taken.
        self.assertEqual(failed(judge_right_key(accepted(caps=0x1, promoted=False, statuses=False).events)), [
            "the agent says it takes OpAMP connection settings and reports their status, in every message",
            "it reports how the key it was offered went (0 statuses)",
            "it took a key of its own: the server confirmed it (key_promoted)"])

    def test_half_the_capabilities_fail(self):
        self.assertNotEqual(failed(judge_right_key(accepted(caps=0x1 | 0x100).events)), [])

    def test_a_key_never_confirmed_fails(self):
        self.assertIn("it took a key of its own: the server confirmed it (key_promoted)",
                      failed(judge_right_key(accepted(promoted=False).events)))


class OwnKey(unittest.TestCase):
    def test_a_good_run_passes(self):
        self.assertEqual(failed(judge_own_key(again().events)), [])

    def test_a_refused_request_fails(self):
        # The key not kept: the next start comes with the enrollment key, which no longer speaks for it.
        log = again()
        log.events.append({"event": "request_rejected", "t": 0.4, "status": 401})
        self.assertNotEqual(failed(judge_own_key(log.events)), [])

    def test_nothing_accepted_fails(self):
        log = Log().pings(0, 10).span(3)
        log.events.append({"event": "request_rejected", "t": 0.4, "status": 401})
        self.assertNotEqual(failed(judge_own_key(log.events)), [])

    def test_a_new_key_fails(self):
        log = again()
        log.events.append({"event": "key_promoted", "t": 1, "uid": UID})
        self.assertEqual(failed(judge_own_key(log.events)), ["with no new key to take"])

    def test_the_default_interval_fails(self):
        self.assertNotEqual(failed(judge_own_key(again(every=30).events)), [])


def rotated(back: bool = False, moved: bool = True, refused: bool = False) -> Log:
    """The rotation run as it should go: the agent's own key confirmed, the operator rotates it,
    the second confirmed, the operator moves it, the third confirmed at the second listener.
    moved=False: the agent never takes the move."""
    log = Log().pings(0, 20).a2s(0.5).a2s(1)
    log.events += [{"event": "connection", "t": 0.4, "listener": "first"},
                   {"event": "key_promoted", "t": 2, "uid": UID},
                   {"event": "operator", "t": 2.1, "action": "rotate", "agents": 1},
                   {"event": "key_promoted", "t": 4, "uid": UID},
                   {"event": "operator", "t": 4.1, "action": "move", "agents": 1}]
    if moved:
        log.events += [{"event": "connection", "t": 5, "listener": "second"},
                       {"event": "key_promoted", "t": 6, "uid": UID}]
    if back:
        log.events.append({"event": "connection", "t": 8, "listener": "first"})
    if refused:
        log.events.append({"event": "request_rejected", "t": 7, "status": 401})
    return log.a2s(5.5).a2s(6.5).a2s(7.5).span(9)


def moved_start(first: bool = False) -> Log:
    """The rotation run's next start: straight to the second listener."""
    log = Log().pings(0, 10).a2s(0.5).a2s(1).a2s(2).span(3)
    log.events.append({"event": "connection", "t": 0.4, "listener": "second"})
    if first:
        log.events.append({"event": "connection", "t": 0.3, "listener": "first"})
    return log


class Rotation(unittest.TestCase):
    def test_a_good_run_passes(self):
        self.assertEqual(failed(judge_rotation(rotated().events)), [])

    def test_a_connection_back_to_the_first_listener_fails(self):
        self.assertEqual(failed(judge_rotation(rotated(back=True).events)),
                         ["and opened no connection to the first after that (1)"])

    def test_a_move_never_taken_fails(self):
        self.assertIn("it reached the second listener", failed(judge_rotation(rotated(moved=False).events)))

    def test_a_refused_request_fails(self):
        self.assertEqual(failed(judge_rotation(rotated(refused=True).events)), ["no request refused (1)"])

    def test_a_move_before_the_rotation_fails(self):
        log = rotated()
        ops = [e for e in log.events if e["event"] == "operator"]
        ops[0]["action"], ops[1]["action"] = "move", "rotate"
        self.assertIn("the operator rotated the agent's key and then moved it "
                      "([('move', 1), ('rotate', 1)])", failed(judge_rotation(log.events)))

    def test_an_operator_that_changed_no_agent_fails(self):
        log = rotated()
        next(e for e in log.events if e["event"] == "operator")["agents"] = 0
        self.assertNotEqual(failed(judge_rotation(log.events)), [])


class MovedStart(unittest.TestCase):
    def test_a_good_start_passes(self):
        self.assertEqual(failed(judge_moved_start(moved_start().events)), [])

    def test_a_start_at_the_configured_endpoint_fails(self):
        self.assertNotEqual(failed(judge_moved_start(moved_start(first=True).events)), [])

    def test_a_new_key_fails(self):
        log = moved_start()
        log.events.append({"event": "key_promoted", "t": 1, "uid": UID})
        self.assertEqual(failed(judge_moved_start(log.events)), ["with no new key to take"])


def seventh(early: bool = False, on: bool = True, failed_off: bool = False, stays_on: bool = False,
            again: bool = True, changes: int = 3, error: str = "app_settings: values must be an object of keys to strings, "
            "numbers or booleans") -> Log:
    """Run 7 as it should go: the defaults (no /feature) until the first offer; on (/feature each
    round, one /changed); a FAILED config that changes nothing; app_settings local (no /feature,
    one /changed); on again (/feature, one /changed); the same values again (nothing to tell)."""
    log = Log().a2s(0, config=True).pings(0, 3, "/feature" if early else None)
    log.offer(3.5).a2s(4, status="APPLIED").request(4.1, "/changed").pings(4, 14, "/feature" if on else None)
    log.offer(13.5).a2s(14, status="FAILED", error=error).pings(14, 24, None if failed_off else "/feature")
    log.offer(23.5).a2s(24, status="APPLIED").request(24.1, "/changed").pings(24, 34, "/feature" if stays_on else None)
    log.offer(33.5).a2s(34, status="APPLIED").request(34.1, "/changed").pings(34, 44, "/feature" if again else None)
    log.offer(43.5).a2s(44, status="APPLIED").pings(44, 50, "/feature" if again else None)
    for n in range(changes - 3):
        log.request(45 + n, "/changed")
    if changes < 3:
        log.events = [e for e in log.events if e.get("path") != "/changed" or e["t"] > 30]
    return log.a2s(50, disconnect=True)


def eighth(feature: bool = True, changed: bool = False) -> Log:
    log = Log().a2s(0.5).pings(0, 10, "/feature" if feature else None)
    if changed:
        log.request(1, "/changed")
    return log.a2s(10, disconnect=True)


class AppSettings(unittest.TestCase):
    def test_the_scenario_offers_app_settings_as_policies(self):
        faults = app_settings_scenario()["server"]["faults"]
        self.assertEqual({f["config_key"] for f in faults}, {POLICY_FILE})
        bodies = [json.loads(f["body"]) for f in faults]
        on = {"mode": "override", "values": {APP_KEY: "on"}}
        self.assertEqual(bodies[0]["app_settings"], on)
        self.assertIsInstance(bodies[1]["app_settings"]["values"][APP_KEY], list)  # refused
        self.assertEqual(bodies[2]["schema"], POLICY_V1)  # no app_settings control: local
        self.assertNotIn("app_settings", bodies[2])
        self.assertEqual(bodies[3]["app_settings"], on)
        self.assertEqual(bodies[4]["app_settings"], on)  # the same values again
        self.assertEqual(bodies[4]["head_sampling"], {"mode": "override", "ratio": 1})

    def test_a_good_run_passes(self):
        self.assertEqual(failed(judge_app_settings(seventh().events)), [])

    def test_the_flag_on_before_any_config_fails(self):
        self.assertNotEqual(failed(judge_app_settings(seventh(early=True).events)), [])

    def test_the_flag_never_read_fails(self):
        self.assertNotEqual(failed(judge_app_settings(seventh(on=False).events)), [])

    def test_a_refused_config_that_changes_the_flag_fails(self):
        self.assertNotEqual(failed(judge_app_settings(seventh(failed_off=True).events)), [])

    def test_a_flag_left_on_once_app_settings_is_local_fails(self):
        self.assertNotEqual(failed(judge_app_settings(seventh(stays_on=True).events)), [])

    def test_the_flag_not_back_once_on_again_fails(self):
        self.assertNotEqual(failed(judge_app_settings(seventh(again=False).events)), [])

    def test_told_of_the_same_values_again_fails(self):
        self.assertNotEqual(failed(judge_app_settings(seventh(changes=4).events)), [])

    def test_not_told_of_a_change_fails(self):
        self.assertNotEqual(failed(judge_app_settings(seventh(changes=2).events)), [])

    def test_a_failure_naming_another_setting_fails(self):
        self.assertNotEqual(failed(judge_app_settings(seventh(error="head_sampling: ratio must be a number from 0 to 1").events)), [])

    def test_a_restart_with_the_saved_flag_passes(self):
        self.assertEqual(failed(judge_app_settings_restart(eighth().events)), [])

    def test_a_restart_that_forgets_the_flag_fails(self):
        self.assertNotEqual(failed(judge_app_settings_restart(eighth(feature=False).events)), [])

    def test_a_restart_that_tells_a_change_fails(self):
        self.assertNotEqual(failed(judge_app_settings_restart(eighth(changed=True).events)), [])


class RustLog(unittest.TestCase):
    def test_export_end_times_use_the_server_clock(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "server.jsonl"
            path.write_text(json.dumps({"event": "otlp", "t": 20, "wall": 100, "ends_wall": [95, 99]}) + "\n")
            self.assertEqual(read_log(path)[0]["ends"], [15, 19])


class ServerBinary(unittest.TestCase):
    def test_the_server_is_put_in_place_while_another_check_runs_it(self):
        # Checks side by side (main()'s --jobs) each put the server in the integration's directory
        # and run it from there; writing over a file being run fails with ETXTBSY.
        root = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(root, ignore_errors=True))
        # (A program, not a script: the kernel refuses writes only to a file it is executing.)
        built = root / "built"
        __import__("shutil").copy2(__import__("shutil").which("sleep"), built)
        with unittest.mock.patch.dict("os.environ", {"OPAMP_EXAMPLES_SERVER": str(built)}):
            integrations.built_server.cache_clear()
            self.addCleanup(integrations.built_server.cache_clear)
            running = subprocess.Popen([str(server_binary(root / "t")), "5"])
            try:
                self.assertEqual(server_binary(root / "t"), root / "t" / "server")
                self.assertEqual(sorted(p.name for p in (root / "t").iterdir()), ["server"])
            finally:
                running.kill()
                running.wait()


class Main(unittest.TestCase):
    """main(): a try.py's checks, one after another or side by side (--jobs)."""

    TRY = textwrap.dedent("""\
        import sys, time
        from pathlib import Path
        sys.path.insert(0, {examples!r})
        from integrations import main
        out = Path({out!r})

        def prepare():
            with (out / "prepared").open("a") as f:
                f.write("x")

        def check(name, seconds, passes=True):
            def run():
                (out / name).write_text(" ".join(sys.argv[1:]))
                time.sleep(seconds)
                print("  a line of " + name)
                return passes
            return run

        CHECKS = {{"a": check("a", 1.5), "b": check("b", 1.5), "c": check("c", 0.1), "bad": check("bad", 0.1, False)}}
        sys.exit(main(CHECKS, prepare, options=("--local",)))
        """)

    def setUp(self) -> None:
        self.out = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(self.out, ignore_errors=True))
        self.script = self.out / "try.py"
        self.script.write_text(self.TRY.format(examples=str(Path(__file__).resolve().parent), out=str(self.out)))

    def run_try(self, *args: str) -> tuple[subprocess.CompletedProcess, float]:
        start = time.monotonic()
        r = subprocess.run([sys.executable, str(self.script), *args], capture_output=True, text=True, check=False)
        return r, time.monotonic() - start

    def test_checks_run_one_after_another_by_default(self):
        r, took = self.run_try("a", "c")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertGreaterEqual(took, 1.5)
        self.assertRegex(r.stdout, r"(?s)=== a\n  a line of a\n=== a: passed in \d+ s\n=== c\n  a line of c\n=== c: passed")
        self.assertEqual((self.out / "prepared").read_text(), "x")

    def test_with_jobs_checks_run_side_by_side_after_one_build(self):
        r, took = self.run_try("--local", "--jobs", "3", "a", "b", "c")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        # Each check's output whole, as it ends (c, the short one, before a and b); the build once,
        # in the first process.
        blocks = re.findall(r"=== (\w+)\n  a line of \1\n=== \1: passed in \d+ s\n", r.stdout)
        self.assertEqual("".join(re.findall(r"=== \w+\n  a line of \w+\n=== \w+: passed in \d+ s\n", r.stdout)), r.stdout)
        self.assertEqual(sorted(blocks), ["a", "b", "c"])
        self.assertEqual(blocks[0], "c")
        self.assertEqual((self.out / "prepared").read_text(), "x")
        # a and b at once: in less than the 3 s they take one after the other (the interpreter
        # starts aside).
        self.assertLess(took, 3.0 + 1.5)
        # Each check's process is given the try.py's own flags.
        self.assertEqual((self.out / "a").read_text(), "--local --prepared a")

    def test_a_check_that_fails_fails_the_run_side_by_side_too(self):
        for args in (["c", "bad"], ["--jobs", "2", "c", "bad"]):
            r, _ = self.run_try(*args)
            self.assertEqual(r.returncode, 1, args)
            self.assertIn("=== c: passed", r.stdout)
            self.assertIn("=== bad: FAILED", r.stdout)

    def test_jobs_must_be_a_count(self):
        for args in (["--jobs"], ["--jobs", "0", "a"], ["--jobs", "x", "a"]):
            r, _ = self.run_try(*args)
            self.assertEqual(r.returncode, 2, args)
            self.assertIn("--jobs", r.stderr)
        self.assertFalse((self.out / "prepared").exists())
        r, _ = self.run_try("--jobs", "2", "nope")
        self.assertEqual(r.returncode, 2)
        self.assertIn("no such check: nope", r.stderr)


if __name__ == "__main__":
    unittest.main()
