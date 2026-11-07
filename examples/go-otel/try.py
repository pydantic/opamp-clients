# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""A Go application set up with opampotel.Setup, against the reference server.

    python3 examples/go-otel/try.py

main.go traces its requests with otelhttp and logs each through otelslog. The checks are every
integration's (examples/integrations.py), check_signals's included; in Go an instrumentation is
switched off at once, not at the next start. They run twice: with the server given by
OTEL_OPAMP_*, then given in code (Setup's options, from main.go's own flags and variable) with no
OTEL_OPAMP_* set at all. check_setup runs for both: for the second, the key is given in code
(WithHeaders, from CHECKOUT_OPAMP_KEY) with the other key in OTEL_OPAMP_HEADERS, which the code
wins over, so the wrong key's warning names WithHeaders.

check_settings is Go's own, for the settings that match telemetry (docs/go-otel.md), with the
application in EXAMPLE_SETTINGS mode (a request to /health beside each to /ping, and three debug
lines a tick from a second bridge logger, "noisy"). What the reference server's sink shows of
telemetry is each span's url.full (else its name) and end, and how many log records arrive, so
each control is checked by what it does to those:
Run 7, the policies, in the order the server offers them:
  1. head_sampling 1: spans of both /ping and /health; four log records a tick;
  2. sampling_rules, a rule of rate 0 on client spans named "* /health" (main.go names its client
     spans by method and path; otelhttp's transport adds url.full only after the span starts, too
     late for a rule), and log_levels scopes noisy off: spans of /ping, none of /health; one log
     record a tick;
  3. attribute_redaction drop url.full: spans arrive, none named by its URL (the sink names a span
     by its url.full, else its name);
  4. attribute_redaction mask URL.FULL (any case): every span named [REDACTED];
  5. trace_rate_limit 1 and log_rate_limit 2: about a trace and two log records a second, of ten
     requests and twenty log lines.
Run 8, debug mode: head_sampling 0 with debug until 20 s ahead, offered once: spans while it
lasts, none after, though the server sends nothing more; the agent reports its effective config
again when it ends.
Run 9, export health: the traces exporter sent to a path the sink refuses: the agent sends its
health at start and again once a trace export has failed, the application's own still healthy.

Then check_app_settings (examples/integrations.py, its own runs 7 and 8): main.go reads the
feature flag example.feature (opampotel.AppSetting), which the server turns on and off, and is
told of each change (opampotel.OnAppSettingsChange) (docs/app-settings.md).

The truth-* checks run check_truth (examples/integrations.py) with main.go in a setup of setups.go
(EXAMPLE_SETUP): once Setup has returned, the application sets a tracer, meter or logger provider
of its own as the global one, exporting to a second sink. Nothing is set aside: the agent reports
that signal's ungoverned app_provider, its settings stay in force, and every other setting must
change the telemetry at the first sink.

check_upgrade (examples/integrations.py) runs main.go built on the last release (previous/go.mod,
opampotel from the module proxy; examples/previous-release.json), then on this checkout.
"""
from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from integrations import (MARGIN, DRIFT, SIGNALS, check, check_app_settings, check_setup, check_rotation, check_outage,  # noqa: E402
                          check_slots, check_upgrade, key_in_code,
                          check_signals, check_truth, ended, main, offer, pings, run, signal, spans, statuses, stop_group, target, opamp_url)
import json  # noqa: E402
import shutil  # noqa: E402
import tempfile  # noqa: E402
from datetime import datetime, timedelta, timezone  # noqa: E402

HERE = Path(__file__).resolve().parent
TARGET = target("go-otel")
APP = TARGET / "app"
#: check_upgrade's build of the same program on the last release (previous/go.mod).
PREVIOUS_APP = TARGET / "app-previous"


def build() -> None:
    subprocess.run(["go", "build", "-o", str(APP), "."], cwd=HERE, check=True)
    src = TARGET / "previous"
    src.mkdir(exist_ok=True)
    for f in ["previous/go.mod", "previous/go.sum", "main.go", "setups.go"]:
        shutil.copy(HERE / f, src)
    subprocess.run(["go", "build", "-mod=readonly", "-o", str(PREVIOUS_APP), "."], cwd=src, check=True)


def launcher(in_code: bool, target_: Path, app_: Path = APP):
    def launch(port: str, telemetry_port: str, state: Path, name: str, extra: dict[str, str]):
        endpoint = opamp_url(port)
        telemetry = f"http://127.0.0.1:{telemetry_port}"
        env = {k: v for k, v in os.environ.items() if not k.startswith("OTEL_OPAMP_")}
        env.update(OTEL_SERVICE_NAME="checkout",
                   OTEL_EXPORTER_OTLP_ENDPOINT=telemetry,
                   OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf",
                   OTEL_METRICS_EXPORTER="none",
                   OTEL_LOGS_EXPORTER="none",
                   OTEL_BSP_SCHEDULE_DELAY="500")
        if in_code:
            args = ["-opamp", endpoint, "-state-dir", str(state), "-heartbeat", "1s"]
            env.update(CHECKOUT_OPAMP_KEY="example")
        else:
            args = []
            env.update(OTEL_OPAMP_ENDPOINT=endpoint,
                       OTEL_OPAMP_HEADERS="Authorization=Bearer%20example",
                       OTEL_OPAMP_STATE_DIR=str(state),
                       OTEL_OPAMP_HEARTBEAT_INTERVAL="1s")
        app = subprocess.Popen([str(app_), *args, f"{telemetry}/ping"], env=env | extra, start_new_session=True,
                               stdout=subprocess.DEVNULL, stderr=open(target_ / f"{name}.stderr", "w"))

        return lambda: stop_group(app)
    return launch


# check_settings's environment: every signal exported, and the application's extra telemetry.
EXTRA = SIGNALS | {"OTEL_METRICS_EXPORTER": "none", "EXAMPLE_SETTINGS": "1"}


def names(events: list[dict], after: float, before: float = 1e9) -> list[str]:
    """The names the sink gives the spans that ended between `after` and `before`: url.full, else
    the span's name. (Sampling is decided when a span starts.)"""
    return [n for e in spans(events) for n, end in zip(e.get("spans") or [], e.get("ends") or []) if after < end < before]


def arrived(events: list[dict], after: float, before: float = 1e9) -> list[str]:
    """The names of the spans that arrived between `after` and `before`. (Redaction is at export.)"""
    return [n for e in spans(events, after, before) for n in e.get("spans") or []]


def logs(events: list[dict], after: float, before: float = 1e9) -> int:
    """The log records that arrived between `after` and `before`."""
    return sum(e.get("count", 0) for e in signal(events, "logs", after, before))


def judge_settings(seven: list[dict]) -> list[tuple[str, bool]]:
    out = []
    st = statuses(seven)
    out.append(("five configs answered APPLIED", [s for _, s, _ in st] == ["APPLIED"] * 5))
    if len(st) != 5:
        return out
    t1, t2, t3, t4, t5 = (t for t, _, _ in st)
    end = max(e["t"] for e in seven)
    one = names(seven, t1 + MARGIN, t2)
    out.append((f"head_sampling 1: spans of /ping and of /health ({len(one)})",
                any(n.endswith("/ping") for n in one) and any(n.endswith("/health") for n in one)))
    ticks = len(pings(seven, t1 + MARGIN, t2 - MARGIN))
    per = logs(seven, t1 + MARGIN, t2 - MARGIN) / ticks if ticks else 0
    out.append((f"four log records a tick ({per:.1f})", ticks > 5 and 3 <= per <= 5))
    two = names(seven, t2 + MARGIN, t3)
    out.append((f"sampling_rules: spans of /ping, none of /health ({len(two)})",
                any(n.endswith("/ping") for n in two) and not any(n.endswith("/health") for n in two)))
    ticks = len(pings(seven, t2 + MARGIN, t3 - MARGIN))
    per = logs(seven, t2 + MARGIN, t3 - MARGIN) / ticks if ticks else 0
    out.append((f"log_levels scopes noisy off: one log record a tick ({per:.1f})", ticks > 5 and 0.5 <= per <= 1.5))
    # Each window ends a second before the next policy's APPLIED: the agent puts a policy in force
    # before it reports it, so an export in that moment may already go out under the next one.
    three = arrived(seven, t3 + MARGIN, t4 - DRIFT)
    out.append((f"attribute_redaction drop url.full: spans, none named by a URL ({sorted(set(three))[:3]})",
                bool(three) and not any(n.startswith("http") for n in three)))
    four = arrived(seven, t4 + MARGIN, t5 - DRIFT)
    out.append((f"attribute_redaction mask URL.FULL: every span [REDACTED] ({sorted(set(four))[:3]})",
                bool(four) and all(n == "[REDACTED]" for n in four)))
    lasted = end - (t5 + MARGIN)
    five = ended(seven, t5 + MARGIN)
    out.append((f"trace_rate_limit 1: {len(five)} spans in {lasted:.0f} s, of "
                f"{2 * len(pings(seven, t5 + MARGIN))} requests",
                lasted > 8 and 0 < len(five) <= lasted + 3 < len(pings(seven, t5 + MARGIN))))
    capped = logs(seven, t5 + MARGIN)
    out.append((f"log_rate_limit 2: {capped} log records in {lasted:.0f} s, of some "
                f"{4 * len(pings(seven, t5 + MARGIN))}", 0 < capped <= 2 * lasted + 4))
    return out


def judge_debug(eight: list[dict], until: float) -> list[tuple[str, bool]]:
    """`until`: debug's until, on the log's clock."""
    st = statuses(eight)
    offers = [e for e in eight if e["event"] == "s2a" and e.get("offer_config")]
    during = [n for n in names(eight, (st[0][0] if st else until) + DRIFT, until - DRIFT) if n.endswith("/ping")]
    after = [n for n in names(eight, until + DRIFT) if n.endswith("/ping")]
    requests = pings(eight, until + MARGIN)
    reported = [e for e in eight if e["event"] == "a2s" and "effective_config" in e["fields"] and e["t"] > until - DRIFT]
    return [
        ("the config answered APPLIED, and offered once", [s for _, s, _ in st] == ["APPLIED"] and len(offers) == 1),
        (f"debug mode: spans at head_sampling 0 ({len(during)})", len(during) > 5),
        (f"after debug's until, none, the server sending nothing ({len(requests)} requests)",
         len(requests) > 5 and not after),
        ("the effective config reported again at the end", bool(reported)),
    ]


def judge_health(nine: list[dict]) -> list[tuple[str, bool]]:
    health = [e for e in nine if e["event"] == "a2s" and "health" in e["fields"]]
    refused = [e for e in nine if e["event"] == "stray_request" and e.get("path") == "/nope"]
    return [
        (f"trace exports failed ({len(refused)})", bool(refused)),
        (f"health sent at start and again after the failure ({len(health)})",
         len(health) >= 2 and bool(refused) and health[-1]["t"] > refused[0]["t"]),
        ("the application's own health stays healthy", all(e.get("health", {}).get("healthy") for e in health)),
    ]


def check_settings(launch, target_: Path) -> bool:
    """Runs 7 to 9, and every check printed; True when all pass."""
    states = [Path(tempfile.mkdtemp(prefix="state-next-", dir=target_)) for _ in range(3)]  # a run each
    ok = True

    def expect(results: list[tuple[str, bool]]) -> None:
        nonlocal ok
        for what, cond in results:
            ok &= cond
            print(f"  {'ok  ' if cond else 'FAIL'} {what}")

    try:
        gap = 12
        seven = run(launch, {"server": {"faults": [
            offer(1, {"head_sampling": {"ratio": 1}}),
            offer(1 + gap, {"head_sampling": {"ratio": 1}, "log_levels": {"scopes": {"noisy": "off"}},
                            "sampling_rules": {"rules": [{"rate": 0, "kind": "client", "name": "* /health"}]}}),
            offer(1 + 2 * gap, {"head_sampling": {"ratio": 1}, "attribute_redaction": {"drop": ["url.full"]}}),
            offer(1 + 3 * gap, {"head_sampling": {"ratio": 1}, "attribute_redaction": {"mask": ["URL.FULL"]}}),
            offer(1 + 4 * gap, {"head_sampling": {"ratio": 1}, "trace_rate_limit": {"traces_per_second": 1},
                                "log_rate_limit": {"records_per_second": 2}}),
        ]}}, states[0], "run7", 150, "127.0.0.1:0", target_,
            until=lambda events: len(statuses(events)) >= 5, settle=15, env=EXTRA)
        print(f"run 7: remote config statuses {[s for _, s, _ in statuses(seven)]}, {len(spans(seven))} span exports")
        expect(judge_settings(seven))

        window = 20
        until = datetime.now(timezone.utc) + timedelta(seconds=window + 5)
        eight = run(launch, {"server": {"faults": [
            offer(1, {"head_sampling": {"ratio": 0},
                      "debug": {"until": until.isoformat(timespec="milliseconds").replace("+00:00", "Z")}}),
        ]}}, states[1], "run8", window + 25, "127.0.0.1:0", target_, env=EXTRA)
        # debug's until on the log's clock: each event has its wall time beside it.
        on_log = until.timestamp() - eight[0]["wall"] + eight[0]["t"] if eight else 0
        print(f"run 8: debug until {until:%H:%M:%S.%f}, {len(spans(eight))} span exports")
        expect(judge_debug(eight, on_log))

        def failed_twice(events: list[dict]) -> bool:
            return sum(e["event"] == "a2s" and "health" in e["fields"] for e in events) >= 2

        def nine_env(telemetry: str) -> dict[str, str]:
            return EXTRA | {"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": f"{telemetry}/nope"}

        nine = run(lambda port, tport, st, name, extra: launch(port, tport, st, name, extra | nine_env(f"http://127.0.0.1:{tport}")),
                   {"server": {}}, states[2], "run9", 90, "127.0.0.1:0", target_, until=failed_twice, settle=2, env=EXTRA)
        print(f"run 9: {sum(e['event'] == 'a2s' and 'health' in e['fields'] for e in nine)} health reports")
        expect(judge_health(nine))
    finally:
        for state in states:
            shutil.rmtree(state, ignore_errors=True)
    return ok


#: The signal each setup (setups.go) takes past Setup's gates: a global provider of the
#: application's own gets all of it, reported as an ungoverned app_provider.
GLOBAL_TRACER = frozenset({"traces"})
GLOBAL_METER = frozenset({"metrics"})
GLOBAL_LOGGER = frozenset({"logs"})

IN_CODE_TARGET = target("go-otel/in-code")
FROM_ENV, IN_CODE = launcher(False, TARGET), launcher(True, IN_CODE_TARGET)
# The server given by OTEL_OPAMP_*, then given in code (Setup options), which win over
# OTEL_OPAMP_* (setup-in-code sets OTEL_OPAMP_HEADERS to the other key).
CHECKS = {
    "first": lambda: check(FROM_ENV, "otelhttp", TARGET, switch_at_once=True, versioned=False),
    "signals": lambda: check_signals(FROM_ENV, TARGET, switch_at_once=True),
    "setup": lambda: check_setup(FROM_ENV, TARGET),
    "rotation": lambda: check_rotation(FROM_ENV, TARGET),
    "outage": lambda: check_outage(FROM_ENV, TARGET),
    "upgrade": lambda: check_upgrade(launcher(False, TARGET, PREVIOUS_APP), FROM_ENV, TARGET),
    "slots": lambda: check_slots(FROM_ENV, TARGET),
    "settings": lambda: check_settings(FROM_ENV, TARGET),
    "app": lambda: check_app_settings(FROM_ENV, TARGET),
    "truth-global-tracer": lambda: check_truth(FROM_ENV, TARGET, "global-tracer", set(), switch_at_once=True,
                                     ungoverned=GLOBAL_TRACER, past=GLOBAL_TRACER),
    "truth-global-meter": lambda: check_truth(FROM_ENV, TARGET, "global-meter", set(), switch_at_once=True,
                                     ungoverned=GLOBAL_METER, past=GLOBAL_METER),
    "truth-global-logger": lambda: check_truth(FROM_ENV, TARGET, "global-logger", set(), switch_at_once=True,
                                     ungoverned=GLOBAL_LOGGER, past=GLOBAL_LOGGER),
    "first-in-code": lambda: check(IN_CODE, "otelhttp", IN_CODE_TARGET, switch_at_once=True, versioned=False),
    "signals-in-code": lambda: check_signals(IN_CODE, IN_CODE_TARGET, switch_at_once=True),
    "settings-in-code": lambda: check_settings(IN_CODE, IN_CODE_TARGET),
    "setup-in-code": lambda: check_setup(IN_CODE, IN_CODE_TARGET, key=key_in_code("CHECKOUT_OPAMP_KEY"),
                                         named="WithHeaders"),
}

if __name__ == "__main__":
    sys.exit(main(CHECKS, build))
