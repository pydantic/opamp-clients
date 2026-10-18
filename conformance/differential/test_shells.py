# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Tests for the differential runs' allowlist (shells.py) and the runner's crash helpers (run.py).

    python3 -m unittest conformance/differential/test_shells.py
"""
from __future__ import annotations

import hashlib
import json
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent / "runner"))
import run  # noqa: E402
import shells  # noqa: E402

RULES = [
    {"clients": ["node-core-platform"], "category": "config", "first": "^stop_timeout_ms:", "reason": "Node stops slower"},
    {"clients": ["java-*"], "scenarios": ["proxy-*"], "transports": ["ws"], "reason": "no SOCKS"},
]


class Allowlist(unittest.TestCase):
    def test_a_rule_matches_client_scenario_transport_category_and_first_line(self):
        a = lambda *args: shells.allowed(RULES, *args)  # noqa: E731
        self.assertIsNotNone(a("node-core-platform", "basic", "http", "config", "stop_timeout_ms: 5000 | 1"))
        self.assertIsNone(a("node-core-platform", "basic", "http", "config", "connect_timeout_ms: 30000 | 1"))
        self.assertIsNone(a("node-core-platform", "basic", "http", "notify", "stop_timeout_ms: 5000 | 1"))
        self.assertIsNone(a("go-core-platform", "basic", "http", "config", "stop_timeout_ms: 5000 | 1"))
        self.assertIsNotNone(a("java-core-platform", "proxy-socks5", "ws", "conn", "- x | + y"))
        self.assertIsNone(a("java-core-platform", "proxy-socks5", "http", "conn", "- x | + y"))
        self.assertIsNone(a("java-core-platform", "basic", "ws", "conn", "- x | + y"))

    def test_every_rule_needs_a_reason(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "x.toml"
            p.write_text('[[allow]]\nclients = ["node-core-platform"]\n')
            with self.assertRaises(SystemExit):
                shells.load_rules(p)
            p.write_text('[[allow]]\nclients = ["node-core-platform"]\nreason = "why"\n')
            self.assertEqual(len(shells.load_rules(p)), 1)
        # The checked-in list reads.
        shells.load_rules()

    def test_the_checked_in_timing_rules_are_as_narrow_as_their_reasons(self):
        rules = shells.load_rules()
        a = lambda *args: shells.allowed(rules, *args)  # noqa: E731
        # credential-offer-broken-ws (CI): the old connection's close and the new one's refusal.
        self.assertIsNotNone(a("dotnet-core-platform", "credential-offer-broken", "ws", "conn", "- ws_failed(401) | + "))
        self.assertIsNotNone(a("java-core-platform", "credential-offer-broken", "ws", "conn", "-  | + ws_failed(401)"))
        self.assertIsNone(a("dotnet-core-platform", "credential-offer-broken", "ws", "conn", "- ws_lost | + "))
        self.assertIsNone(a("dotnet-core-platform", "close-frame", "ws", "conn", "- ws_failed(401) | + "))
        # packages-redirects-refused-http: PackageFailuresGrouped.
        self.assertIsNone(a("node-core", "packages-redirects-refused", "http", "saves", "- stored | + store x"))
        # Not allowed: the differences this change fixed rather than allowed.
        self.assertIsNone(a("node-core-platform", "close-frame", "ws", "conn", "- ws_closed(1000) | + ws_lost"))
        self.assertIsNone(a("go-core-platform", "remote-config-restart", "ws", "send",
                            "- uid#0 caps=3807 rcs(2,a2562a2b) | + uid#0 caps=3807 rcs(1,a2562a2b)"))
        # restart-mid-download-ws, own-metrics-restart-ws (go-core-platform on CI): a second message
        # before the first was answered, so the server asked for the full state. The kernel holds
        # the second until the first answer now (RULE-CLIENT-22): no longer allowed, on either
        # transport.
        full = "uid#0 caps=281f desc(id:service.name driver.change) health(true,) effective() "
        self.assertIsNone(a("go-core-platform", "restart-mid-download", "ws", "send", f"-  | + {full}pkgs(held:2)"))
        self.assertIsNone(a("go-core-platform", "own-metrics-restart", "ws", "send", f"- {full}css(2) | + "))
        self.assertIsNone(a("go-core-platform", "restart-mid-download", "ws", "recv", "-  | + flags=1"))
        self.assertIsNone(a("go-core-platform", "own-metrics-restart", "ws", "recv", "- flags=1 | + "))
        self.assertIsNone(a("go-core-platform", "own-metrics-restart", "http", "recv", "-  | + flags=1"))
        self.assertIsNone(a("go-core-platform", "basic", "ws", "recv", "-  | + flags=1"))
        self.assertIsNone(a("go-core-platform", "own-metrics-restart", "ws", "send", "- uid#0 caps=a8e7 css(2) | + "))
        # Nor the Rust client's first message after a restart saying APPLYING for a saved config
        # its callback answered (remote-config-restart-ws on CI): that one is a bug.
        self.assertIsNone(a("rust", "remote-config-restart", "ws", "send", f"- {full}rcs(2,a2562a2b) | + "))
        self.assertIsNone(a("go-core-platform", "remote-config-restart", "ws", "send", f"-  | + {full}rcs(2,a2562a2b)"))
        # A hostile server's cycle of holding and answering, in a container: hostile-packages-http.
        self.assertIsNotNone(a("java-core-platform", "hostile-packages", "http", "send", "- uid#0 caps=281f pkgs(zz-good:2) | + "))
        self.assertIsNone(a("go-core-platform", "hostile-packages", "http", "send", "- uid#0 caps=281f pkgs(zz-good:2) | + "))
        # A goodbye at the end of a run the server never let connect (accept-then-drop-ws: node,
        # tls-untrusted-http: go-core-platform, on CI): only the disconnect, only there.
        self.assertIsNotNone(a("node-core-platform", "accept-then-drop", "ws", "send", "-  | + uid#0 caps=2807 disconnect"))
        self.assertIsNotNone(a("go-core-platform", "tls-untrusted", "http", "send", "-  | + uid#0 caps=2807 disconnect"))
        self.assertIsNotNone(a("go-core-platform", "tls-untrusted", "http", "send", "- uid#0 caps=2807 disconnect | + "))
        self.assertIsNone(a("node-core-platform", "basic", "ws", "send", "-  | + uid#0 caps=2807 disconnect"))
        self.assertIsNone(a("node-core-platform", "accept-then-drop", "ws", "send", f"-  | + {full}"))
        self.assertIsNone(a("node-core-platform", "accept-then-drop", "ws", "send", "-  | + uid#0 caps=2807 health(false,) disconnect"))

    def test_compare_reports_and_allows(self):
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            for client in ("rust", "node-core-platform", "go-core-platform"):
                run_dir = out / client / "basic-http"
                run_dir.mkdir(parents=True)
                (run_dir / "core-0.tap").write_bytes(b"")
            (out / "go-core-platform" / "only-http").mkdir(parents=True)
            report = {"reference": "rust", "clients": {
                "node-core-platform": {"streams": False, "config": [["stop_timeout_ms", "5000", "1"]], "differences": []},
                "go-core-platform": {"streams": False, "config": [], "differences": [
                    {"category": "conn", "first": ["ws_closed(1000)", "ws_lost"], "diff": "- ws_closed(1000)\n+ ws_lost\n"}]},
            }}
            done = mock.Mock(returncode=0, stdout=json.dumps(report), stderr="")
            with mock.patch.object(shells.subprocess, "run", return_value=done) as called:
                lines, found = shells.compare(out, "rust", [], "", RULES)
            cmd = called.call_args[0][0]
            self.assertEqual(cmd[1:3], ["diff", "--json"])
            self.assertTrue(cmd[3].startswith("rust="))
            self.assertEqual([f["client"] for f in found], ["go-core-platform", "node-core-platform"])
            self.assertEqual([bool(f["allowed"]) for f in found], [False, True])
            text = "\n".join(lines)
            self.assertIn("allowed node-core-platform config: stop_timeout_ms: 5000 | 1", text)
            self.assertIn("DIFFERS go-core-platform conn: - ws_closed(1000) | + ws_lost", text)
            self.assertIn("+ ws_lost", text)
            # A failing diff is reported.
            failed = mock.Mock(returncode=2, stdout="", stderr="no core taps")
            with mock.patch.object(shells.subprocess, "run", return_value=failed):
                lines, found = shells.compare(out, "rust", ["go-core-platform"], "basic", RULES)
            self.assertEqual(found[0]["category"], "error")

    def test_a_run_with_no_reference_to_compare_with_fails_strict(self):
        # A change to one SDK runs its family alone: node-core's and node-core-platform's taps, no
        # rust's. That compared nothing and passed; each such run is now a difference no rule
        # allows.
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            for client in ("node-core-platform", "node-core"):
                run_dir = out / client / "basic-ws"
                run_dir.mkdir(parents=True)
                (run_dir / "core-1.tap").write_bytes(b"")
            with mock.patch.object(shells.subprocess, "run") as called:
                lines, found = shells.compare(out, "rust", [], "", RULES)
            called.assert_not_called()
            self.assertEqual([(f["client"], f["category"], f["allowed"]) for f in found],
                             [("node-core", "no-reference", None), ("node-core-platform", "no-reference", None)])
            self.assertIn("basic-ws: no run of rust to compare node-core, node-core-platform with", "\n".join(lines))
            argv = ["shells.py", "--strict", "--out", str(out)]
            with mock.patch.object(sys, "argv", argv), mock.patch.object(shells, "TAP", Path(__file__)), \
                    mock.patch("builtins.print"):
                self.assertEqual(shells.main(), 1)

    def test_the_torture_harness_runs_are_no_client(self):
        # The rust family's job keeps torture.py's run under out/torture/ with core taps: it is no
        # client, and has no rust run of its name to be compared with.
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            run_dir = out / "torture" / "rust-http-20261007"
            run_dir.mkdir(parents=True)
            (run_dir / "core-1.tap").write_bytes(b"")
            self.assertEqual(shells.runs(out, "rust", [], ""), {})


def save(*failed: str, version: str = "1.0.0") -> str:
    """A save's line in the taps' diff, with packages f4, f5 and f6 offered, `failed` marked so."""
    pkgs = ",".join(f"{n}:>{version}{' failed' if n in failed else ''}" for n in ("f4-bad-host", "f5-bad-scheme", "f6-userinfo"))
    return f"store uid#0 valid(http:///v1/opamp) pkgs({pkgs})"


F4, F5, F6 = "f4-bad-host", "f5-bad-scheme", "f6-userinfo"


def diff(*lines: tuple[str, str]) -> str:
    """A diff as opamp-tap writes it: each line marked " ", "-" or "+", "..." where lines are left out."""
    return "".join("  ...\n" if m == "..." else f"{m} {ln}\n" for m, ln in lines)


class PackageFailuresGrouped(unittest.TestCase):
    """The packages-* allowance: two clients' saves may differ only in which package failures share
    a save (failures close together reach the core in one step or two)."""

    def test_the_groupings_that_occur_are_allowed(self):
        ok = shells.package_failures_grouped
        # One client saved f4's failure alone, then f5's and f6's; the other f4's and f5's together.
        self.assertTrue(ok(diff(("...", ""), (" ", save()), (" ", "stored"), ("-", save(F4)), ("-", "stored"),
                                ("+", save(F4, F5)), ("+", "stored"), (" ", save(F4, F5, F6)), (" ", "stored"), ("...", ""))))
        # One save more on one side.
        self.assertTrue(ok(diff((" ", save(F4)), (" ", "stored"), ("+", save(F4, F5)), ("+", "stored"),
                                (" ", save(F4, F5, F6)), (" ", "stored"))))
        # Saved in another order (f5's failure before f4's), and a difference in two places.
        self.assertTrue(ok(diff((" ", save()), (" ", "stored"), ("-", save(F4)), ("-", "stored"), ("+", save(F5)),
                                ("+", "stored"), (" ", save(F4, F5)), (" ", "stored"), ("...", ""), (" ", save(F4, F5)),
                                (" ", "stored"), ("+", save(F4, F5, F6)), ("+", "stored"), (" ", save(F4, F5, F6)),
                                (" ", "stored"))))

    def test_a_real_run_is_allowed(self):
        # node-core against rust, packages-redirects-refused-http, eight runs at once: f5's failure
        # saved alone in the reference, with f6's in node-core's.
        state = "store uid#0 valid(http:///v1/opamp) pkgs(f1-six:>1.0.0 failed,f2-downgrade:>1.0.0 failed,f3-up-then-down:>1.0.0 failed,{})"
        real = diff(("...", ""), (" ", state.format("f4-bad-host:>1.0.0,f5-bad-scheme:>1.0.0,f6-userinfo:>1.0.0,zz-good:>1.0.0")),
                    (" ", "stored"), (" ", state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0,f6-userinfo:>1.0.0,zz-good:>1.0.0")),
                    (" ", "stored"),
                    ("-", state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0,zz-good:>1.0.0")),
                    ("-", "stored"),
                    (" ", state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0 failed,zz-good:>1.0.0")),
                    (" ", "stored"),
                    (" ", state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0 failed,zz-good:>1.0.0 uncertain")),
                    (" ", "stored"), ("...", ""))
        self.assertTrue(shells.package_failures_grouped(real))

    def test_a_save_replaced_whose_answer_both_have_is_allowed(self):
        # node-core against rust, packages-redirects-refused (CI, 2026-10-04 and -05): the reference
        # saved f4's failure and node-core f4's and f5's, in the same place. Each is answered
        # `stored`, which the diff shows once, after the hunk, as a line both have.
        ok = shells.package_failures_grouped
        self.assertTrue(ok(diff(("...", ""), (" ", save()), (" ", "stored"), ("-", save(F4)), ("+", save(F4, F5)),
                                (" ", "stored"), (" ", save(F4, F5, F6)), (" ", "stored"), ("...", ""))))
        # With a save more before it on one side, answered in the hunk.
        self.assertTrue(ok(diff((" ", save()), (" ", "stored"), ("-", save(F4)), ("+", save(F4)), ("+", "stored"),
                                ("+", save(F4, F5)), (" ", "stored"), (" ", save(F4, F5, F6)), (" ", "stored"))))
        # Not when what follows isn't that answer, or a failure is taken back, or one answer is missing
        # where the side's last line is its other answer.
        tail = [(" ", save(F4, F5, F6)), (" ", "stored")]
        self.assertFalse(ok(diff((" ", save()), (" ", "stored"), ("-", save(F4)), ("+", save(F4, F5)), *tail)))
        self.assertFalse(ok(diff((" ", save()), (" ", "stored"), ("-", save(F4, F5)), ("+", save(F4)),
                                 (" ", "stored"), (" ", save(F4)), (" ", "stored"))))
        self.assertFalse(ok(diff((" ", save()), (" ", "stored"), ("+", save(F4)), ("+", save(F4, F5)), ("+", "stored"),
                                 (" ", "stored"), *tail)))

    def test_a_real_run_whose_answers_line_up_is_allowed(self):
        # node-core against rust, packages-redirects-refused-http on CI: f4's failure
        # saved alone in the reference, with f5's in node-core's. Each made one save there, so the
        # diff lines their "stored" up as one unchanged line after the two saves.
        state = "store uid#0 valid(http:///v1/opamp) pkgs(f1-six:>1.0.0 failed,f2-downgrade:>1.0.0 failed,f3-up-then-down:>1.0.0 failed,{})"
        real = diff(("...", ""), (" ", state.format("f4-bad-host:>1.0.0,f5-bad-scheme:>1.0.0,f6-userinfo:>1.0.0,zz-good:>1.0.0")),
                    (" ", "stored"),
                    ("-", state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0,f6-userinfo:>1.0.0,zz-good:>1.0.0")),
                    ("+", state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0,zz-good:>1.0.0")),
                    (" ", "stored"),
                    (" ", state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0 failed,zz-good:>1.0.0")),
                    (" ", "stored"), ("...", ""))
        self.assertTrue(shells.package_failures_grouped(real))
        ok = shells.package_failures_grouped
        head, tail = [(" ", save()), (" ", "stored")], [(" ", save(F4, F5, F6)), (" ", "stored")]
        # The shared answer must be there, and be "stored": not a failed save, and not on one side only.
        self.assertFalse(ok(diff(*head, ("-", save(F4)), ("+", save(F4, F5)), (" ", "store_failed"), *tail)))
        self.assertFalse(ok(diff(*head, ("-", save(F4)), ("+", save(F4, F5)), *tail)))
        self.assertFalse(ok(diff(*head, ("-", save(F4)), ("+", save(F4, F5)), ("+", "stored"), (" ", "stored"), *tail)))
        # And a failure still isn't taken back that way.
        self.assertFalse(ok(diff(*head, ("-", save(F4, F5)), ("+", save(F4)), (" ", "stored"), (" ", save(F4)), (" ", "stored"))))

    def test_a_real_run_whose_next_save_starts_a_download_is_allowed(self):
        # dotnet-core-platform against rust, packages-redirects-refused-ws on CI: f5's failure saved
        # alone in the reference, with f6's in dotnet-core-platform's. The save both reach next also
        # marks zz-good's download as started, so it is not the differing saves' state but for
        # failures; the save both had before is.
        state = "store uid#0 valid(ws:///v1/opamp) pkgs(f1-six:>1.0.0 failed,f2-downgrade:>1.0.0 failed,f3-up-then-down:>1.0.0 failed,{})"
        before = (" ", state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0,f6-userinfo:>1.0.0,zz-good:>1.0.0"))
        f5 = state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0,zz-good:>1.0.0")
        f6 = state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0 failed,zz-good:>1.0.0")
        started = (" ", state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0 failed,zz-good:>1.0.0 uncertain"))
        ok = shells.package_failures_grouped
        self.assertTrue(ok(diff(("...", ""), before, (" ", "stored"), ("-", f5), ("+", f6), (" ", "stored"),
                                started, (" ", "stored"), ("...", ""))))
        # A differing save in a state that is neither the one before's nor the one after's is not.
        other = state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0,zz-good:>2.0.0")
        self.assertFalse(ok(diff(before, (" ", "stored"), ("-", f5), ("+", other), (" ", "stored"), started, (" ", "stored"))))
        # Nor one that goes back from the state after to the state before.
        late = state.format("f4-bad-host:>1.0.0 failed,f5-bad-scheme:>1.0.0 failed,f6-userinfo:>1.0.0,zz-good:>1.0.0 uncertain")
        self.assertFalse(ok(diff(before, (" ", "stored"), ("+", late), ("+", "stored"), ("+", f6), ("+", "stored"),
                                 started, (" ", "stored"))))

    def test_a_constructed_bad_difference_is_not(self):
        ok = shells.package_failures_grouped
        head, tail = [(" ", save()), (" ", "stored")], [(" ", save(F4, F5, F6)), (" ", "stored")]
        # A failure taken back.
        self.assertFalse(ok(diff(*head, ("-", save(F4)), ("-", "stored"), ("+", save(F5)), ("+", "stored"),
                                 ("+", save(F4)), ("+", "stored"), *tail)))
        # A save of something else (another version offered).
        self.assertFalse(ok(diff(*head, ("+", save(F4, version="2.0.0")), ("+", "stored"), *tail)))
        # A save that failed, or one answered twice.
        self.assertFalse(ok(diff(*head, ("+", save(F4)), ("+", "store_failed"), *tail)))
        self.assertFalse(ok(diff(*head, ("+", save(F4)), ("+", "stored"), ("+", "stored"), *tail)))
        # Where they ended differs: no save in common after.
        self.assertFalse(ok(diff(*head, ("-", save(F4, F5, F6)), ("-", "stored"), ("+", save(F4, F5)), ("+", "stored"))))
        # A line that isn't a save.
        self.assertFalse(ok(diff(*head, ("+", "start from uid#0"), *tail)))
        self.assertFalse(ok(""))

    def test_the_checked_in_rule_uses_it(self):
        rules = shells.load_rules()
        a = lambda *args: shells.allowed(rules, *args)  # noqa: E731
        good = diff((" ", save()), (" ", "stored"), ("-", save(F4)), ("-", "stored"), ("+", save(F4, F5)), ("+", "stored"),
                    (" ", save(F4, F5, F6)), (" ", "stored"))
        bad = diff((" ", save()), (" ", "stored"), ("-", save(F4)), ("-", "stored"), ("+", save(F5)), ("+", "stored"),
                   ("+", save()), ("+", "stored"), (" ", save(F4, F5, F6)), (" ", "stored"))
        first = f"- {save(F4)} | + {save(F4, F5)}"
        self.assertIsNotNone(a("node-core", "packages-redirects-refused", "http", "saves", first, good))
        self.assertIsNone(a("node-core", "packages-redirects-refused", "http", "saves", f"- {save(F4)} | + {save(F5)}", bad))
        self.assertIsNone(a("node-core", "remote-config", "http", "saves", first, good))
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "x.toml"
            p.write_text('[[allow]]\ngrouping = "nope"\nreason = "why"\n')
            with self.assertRaises(SystemExit):
                shells.load_rules(p)


class CrashHelpers(unittest.TestCase):
    def test_the_saved_state_a_tap_starts_from(self):
        state = b"\x0a\x03abc"
        # A Config: field 1 (varint; reserved, once the transport), endpoint (field 9, bytes),
        # saved_state (14), seed (8, varint), a fixed64 and a fixed32 field.
        config = (b"\x08\x01" + b"\x4a\x03x:y" + b"\x72" + bytes([len(state)]) + state + b"\x40\x96\x01"
                  + b"\x79" + b"\x00" * 8 + b"\x7d" + b"\x00" * 4)
        record = bytes([1]) + len(config).to_bytes(4, "little") + (0).to_bytes(8, "little") + config
        with tempfile.TemporaryDirectory() as d:
            tap = Path(d) / "core-1.tap"
            tap.write_bytes(record)
            self.assertEqual(run.saved_state(tap), state)
            # None saved; not an init; no file.
            empty = b"\x08\x01"
            tap.write_bytes(bytes([1]) + len(empty).to_bytes(4, "little") + b"\x00" * 8 + empty)
            self.assertEqual(run.saved_state(tap), b"")
            tap.write_bytes(bytes([3]) + record[1:])
            self.assertIsNone(run.saved_state(tap))
            self.assertIsNone(run.saved_state(Path(d) / "missing"))

    def test_the_crash_moment_is_the_seeds_and_within_its_window(self):
        moments = {run.crash_moment(seed, 0, 0.5, 3.0) for seed in range(50)}
        self.assertTrue(all(0.5 <= m <= 3.0 for m in moments))
        self.assertGreater(len(moments), 40)
        self.assertEqual(run.crash_moment(7, 1, 0.5, 3.0), run.crash_moment(7, 1, 0.5, 3.0))
        self.assertNotEqual(run.crash_moment(7, 0, 0.5, 3.0), run.crash_moment(7, 1, 0.5, 3.0))
        self.assertEqual(run.crash_moment(7, 0, 2.0, 1.0), 2.0)

    def test_killing_a_driver_that_has_gone_is_quiet(self):
        proc = run.subprocess.Popen(["true"], start_new_session=True)
        proc.wait()
        with mock.patch.object(run.os, "killpg", side_effect=PermissionError(1, "Operation not permitted")):
            run.kill(proc, None)
        with mock.patch.object(run.os, "killpg", side_effect=ProcessLookupError()):
            run.kill(proc, None)

    def test_counting_what_drivers_said(self):
        with tempfile.TemporaryDirectory() as d:
            log = Path(d) / "driver.jsonl"
            self.assertEqual(run.said(log, "stopped"), 0)
            log.write_text('{"event": "started"}\n{"event": "stopped"}\nnoise\n{"event": "stopped"}\n')
            self.assertEqual(run.said(log, "stopped"), 2)
            self.assertEqual(run.said(log, "started"), 1)
        self.assertEqual(hashlib.sha256(b"").hexdigest()[:4], "e3b0")


class DriverDeadlines(unittest.TestCase):
    """How long the runner gives a driver: a slow start (a JVM in a container on a loaded machine:
    25 s on CI) doesn't eat the time it has to stop and end."""

    @staticmethod
    def log(path: Path, *events: tuple[str, float]) -> None:
        path.write_text("".join(json.dumps({"event": e, "wall": w}) + "\n" for e, w in events))

    def test_the_deadline_follows_what_the_driver_said(self):
        with tempfile.TemporaryDirectory() as d:
            log = Path(d) / "driver.jsonl"
            first = {"started": 0, "stopped": 0}
            # Nothing said yet: time to start, then its run, its linger and its exit.
            self.assertEqual(run.driver_deadline(log, first, 100.0, 4.0, 3.0), 100.0 + run.START_S + 4.0 + 3.0 + run.EXIT_S)
            # Started (late): its run counts from then.
            self.log(log, ("started", 125.0))
            self.assertEqual(run.driver_deadline(log, first, 100.0, 4.0, 3.0), 125.0 + 4.0 + 3.0 + run.EXIT_S)
            # Stopped: its linger and its exit, from then.
            self.log(log, ("started", 125.0), ("stopped", 128.5))
            self.assertEqual(run.driver_deadline(log, first, 100.0, 4.0, 3.0), 128.5 + 3.0 + run.EXIT_S)
            # A later run (a restart) goes by its own events, not those of the runs before it.
            self.assertEqual(run.driver_deadline(log, {"started": 1, "stopped": 1}, 130.0, 4.0, 0.0),
                             130.0 + run.START_S + 4.0 + run.EXIT_S)
            self.assertEqual(run.event_wall(log, "stopped", 1), 128.5)
            self.assertIsNone(run.event_wall(log, "stopped", 2))

    def test_a_driver_slow_to_start_is_not_killed_after_its_stop(self):
        # CI, java-core-platform stop-mid-connect-ws: the JVM said started 25.9 s after it was
        # spawned, stopped 3.3 s later, lingered 3 s; the runner's deadline of run + linger + 30 s
        # from the spawn killed it as it ended. Here in small: 1 s to start, a run of 0.2 s, 0.3 s
        # to end; 1 s to end after stopped.
        with tempfile.TemporaryDirectory() as d, mock.patch.object(run, "EXIT_S", 1.0):
            log = Path(d) / "driver.jsonl"
            script = ("import json, sys, time\n"
                      "def say(e):\n"
                      "    print(json.dumps({'event': e, 'wall': time.time()}), flush=True)\n"
                      "time.sleep(1.0); say('started'); time.sleep(0.2); say('stopped'); time.sleep(0.3)\n")
            spawned = time.time()
            with open(log, "w") as out:
                proc = run.subprocess.Popen([sys.executable, "-c", script], stdout=out)
            code = run.wait_driver(proc, lambda: run.driver_deadline(log, {"started": 0, "stopped": 0}, spawned, 0.2, 0.0))
            self.assertEqual(code, 0, "killed as it ended")
            # Counted from the spawn, as it was, the deadline had passed before it ended.
            self.assertLess(spawned + 0.2 + run.EXIT_S, time.time())

    def test_a_driver_that_lingers_after_it_stopped_is_cut_off(self):
        with tempfile.TemporaryDirectory() as d, mock.patch.object(run, "EXIT_S", 0.5):
            log = Path(d) / "driver.jsonl"
            script = ("import json, time\n"
                      "print(json.dumps({'event': 'started', 'wall': time.time()}), flush=True)\n"
                      "print(json.dumps({'event': 'stopped', 'wall': time.time()}), flush=True)\n"
                      "time.sleep(30)\n")
            spawned = time.time()
            with open(log, "w") as out:
                proc = run.subprocess.Popen([sys.executable, "-c", script], stdout=out)
            try:
                code = run.wait_driver(proc, lambda: run.driver_deadline(log, {"started": 0, "stopped": 0}, spawned, 60.0, 0.0))
                self.assertIsNone(code)
                self.assertLess(time.time() - spawned, 10, "waited out its run, not its exit")
            finally:
                proc.kill()
                proc.wait()


if __name__ == "__main__":
    unittest.main()
