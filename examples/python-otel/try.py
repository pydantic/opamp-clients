# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""opentelemetry-instrument with pydantic-opamp-otel, against the reference server.

    python3 examples/python-otel/try.py

app.py has no OpenTelemetry code: the urllib instrumentation traces its requests. The checks are
every integration's (examples/integrations.py), run for app.py and again for in_code.py, the same
program with its SDK set up in code (opamp_otel.configure()) and the server given there, with no
OTEL_OPAMP_* variable set. It instruments itself, so the instrumentation switch is ignored. For
app.py, then check_next: the newer settings, one after another in one run against the same server
and sink (docs/python-otel.md#the-newer-settings):

  1. sampling_rules: a rule of rate 0 on the /health requests: only /ping is traced;
  2. mask_attributes url.full and http.url: the sink sees each span's URL as [REDACTED];
  3. drop_attributes url.full and http.url: the sink sees no URL, only the span name, GET;
  4. send_logs_level_by_scope app.noisy=off: one log record a round, not two;
  5. sampling_traces_per_second 1 and send_logs_per_second 1: about one of each a second;
  6. sampling_rate 0 with debug_until a few seconds ahead: traced until then, and then not,
     with no word from the server;
and export health throughout: the metrics go to a port nothing listens on, so a health report
follows the first one, when the timer finds the metric exports failing. Then check_app_settings:
app.py reads the feature flag example.feature (opamp_otel.app_settings), which the server turns on
and off (docs/app-settings.md).
"""
from __future__ import annotations

import datetime
import json
import os
import signal
import subprocess
import sys
import time
from collections.abc import Callable
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from integrations import (DRIFT, MARGIN, ROOT, SETTINGS, check, check_app_settings, check_setup,  # noqa: E402
                          key_in_code,
                          check_signals, main, pings, run, signal as exports, spans, statuses, stop_group, target, opamp_url)

HERE = Path(__file__).resolve().parent
TARGET = target("python-otel")
UV = ["uv", "run", "--quiet", "--no-project", "--python", "3.13",
      "--with", str(ROOT / "python"), "--with", str(ROOT / "python/otel"),
      # aiohttp-client too: the OpAMP client is aiohttp, and its own requests must not be traced.
      "--with", "opentelemetry-instrumentation-urllib", "--with", "opentelemetry-instrumentation-aiohttp-client"]
APPS = {"app.py": ["opentelemetry-instrument", "python", str(HERE / "app.py")],
        "in_code.py": ["python", str(HERE / "in_code.py")]}
APP = "app.py"


def launch(port: str, telemetry_port: str, state: Path, name: str, extra: dict[str, str]):
    telemetry = f"http://127.0.0.1:{telemetry_port}"
    env = {k: v for k, v in os.environ.items() if not k.startswith("OTEL_OPAMP_")}
    env.update(OTEL_EXPORTER_OTLP_ENDPOINT=telemetry,
               OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf",
               OTEL_METRICS_EXPORTER="none",
               OTEL_LOGS_EXPORTER="none",
               OTEL_BSP_SCHEDULE_DELAY="500",
               OTEL_PYTHON_LOGGING_AUTO_INSTRUMENTATION_ENABLED="true")
    if APP == "app.py":
        env.update(OTEL_SERVICE_NAME="checkout",
                   OTEL_OPAMP_ENDPOINT=opamp_url(port),
                   OTEL_OPAMP_HEADERS="Authorization=Bearer%20example",
                   OTEL_OPAMP_STATE_DIR=str(state),
                   OTEL_OPAMP_HEARTBEAT_INTERVAL="1s")
    else:  # the application's own settings, which it gives configure()
        env.update(CHECKOUT_OPAMP_URL=opamp_url(port),
                   CHECKOUT_OPAMP_KEY="example",
                   CHECKOUT_STATE_DIR=str(state),
                   CHECKOUT_OPAMP_HEARTBEAT="1s")
    stderr = open(TARGET / APP.replace(".", "-") / f"{name}.stderr", "w")
    app = subprocess.Popen([*UV, *APPS[APP], f"{telemetry}/ping"], env=env | extra, start_new_session=True,
                           stdout=subprocess.DEVNULL, stderr=stderr)

    return lambda: stop_group(app, signal.SIGINT)  # as Ctrl-C would: atexit runs


# check_next's offers: (message, settings). The agent checks in every second.
NEXT = [
    (1, {"sampling_rate": "1",
         "sampling_rules": [{"rate": 0, "kind": "client", "attributes": {"http.url": "*/health"}},
                            {"rate": 0, "kind": "client", "attributes": {"url.full": "*/health"}}]}),
    (16, {"sampling_rate": "1", "mask_attributes": "url.full, http.url"}),
    (26, {"sampling_rate": "1", "drop_attributes": "URL.FULL, http.url"}),  # keys match ignoring case
    (36, {"sampling_rate": "1", "send_logs_level_by_scope": "app.noisy=off"}),
    (50, {"sampling_rate": "1", "sampling_traces_per_second": "1", "send_logs_per_second": "1"}),
    (66, {"sampling_rate": "0", "debug_until": None}),  # filled in: DEBUG seconds after it is sent
]
DEBUG = 12.0


def ending(events: list[dict], after: float, before: float) -> list[str]:
    """The names the sink gave the spans that ended between `after` and `before`: the URL (url.full
    or http.url) when the span has one, else its name."""
    return [name for e in spans(events) for name, end in zip(e.get("spans") or [], e.get("ends") or [])
            if after < end < before]


def records(events: list[dict], after: float, before: float) -> int:
    return sum(e.get("count", 0) for e in exports(events, "logs", after, before))


def judge_next(events: list[dict], debug_until: float) -> list[tuple[str, bool]]:
    st = statuses(events)
    out = [("six configs answered APPLIED", [s for _, s, _ in st] == ["APPLIED"] * 6)]
    if len(st) != 6:
        return out
    t = [at for at, _, _ in st] + [max(e["t"] for e in events)]
    a, b = t[0] + DRIFT, t[1] - DRIFT
    names = ending(events, a, b)
    out.append((f"sampling_rules: /ping traced, /health not ({len(names)} spans)",
                any(n.endswith("/ping") for n in names) and not any(n.endswith("/health") for n in names)))
    names = ending(events, t[1] + DRIFT, t[2] - DRIFT)
    out.append((f"mask_attributes: every URL [REDACTED] ({len(names)} spans)",
                bool(names) and set(names) == {"[REDACTED]"}))
    names = ending(events, t[2] + DRIFT, t[3] - DRIFT)
    out.append((f"drop_attributes: no URL, only the span's name ({sorted(set(names))})",
                bool(names) and set(names) == {"GET"}))
    before = records(events, t[1] + MARGIN, t[3]) / max(len(pings(events, t[1] + MARGIN, t[3])), 1)
    after = records(events, t[3] + MARGIN, t[4]) / max(len(pings(events, t[3] + MARGIN, t[4])), 1)
    out.append((f"send_logs_level_by_scope: log records a round {before:.2f}, then {after:.2f} with app.noisy off",
                before > 1.6 and 0.6 < after < 1.3))
    window = t[5] - (t[4] + MARGIN)
    traced = len(ending(events, t[4] + MARGIN, t[5]))
    logged = records(events, t[4] + MARGIN, t[5])
    made = len(pings(events, t[4] + MARGIN, t[5]))
    out.append((f"rate caps: {traced} spans and {logged} log records in {window:.0f} s of {made} rounds",
                window > 5 and made > 3 * window and 0 < traced <= window + 3 and 0 < logged <= window + 3))
    during = ending(events, t[5] + DRIFT, debug_until - DRIFT)
    late = ending(events, debug_until + MARGIN, t[6])
    out.append((f"debug_until: traced in the window ({len(during)} spans), not after it ({len(late)} spans, "
                f"{t[6] - debug_until - MARGIN:.0f} s), with no word from the server",
                bool(during) and not late and t[6] - debug_until - MARGIN > 3))
    health = [e for e in events if e["event"] == "a2s" and "health" in e["fields"]]
    out.append((f"export health: reported at start and again once the metric exports fail ({len(health)} reports)",
                len(health) >= 2 and all(e["health"]["healthy"] for e in health)))
    return out


def check_next(launch, target_: Path, seconds: float = 200) -> bool:
    """The newer settings in one run (above); every check printed; True when all pass."""
    import shutil
    import tempfile

    state = Path(tempfile.mkdtemp(prefix="state-next-", dir=target_))
    # debug_until is a time: as late as the offer could be sent, plus DEBUG. The agent's first
    # message comes once its environment is resolved (done before), so a second a message.
    debug_until = time.time() + NEXT[-1][0] + 30 + DEBUG
    offers = []
    for n, body in NEXT:
        if "debug_until" in body:
            when = datetime.datetime.fromtimestamp(debug_until, datetime.UTC)
            body = {**body, "debug_until": when.isoformat(timespec="seconds").replace("+00:00", "Z")}
            debug_until = when.timestamp()
        offers.append({"on_message": n, "action": "offer_config", "config_key": "elastic",
                       "body": json.dumps(body), "content_type": SETTINGS})
    env = {"APP_HEALTH_CHECKS": "1", "OTEL_LOGS_EXPORTER": "otlp", "OTEL_BLRP_SCHEDULE_DELAY": "500",
           # Nothing listens on port 9: every metric export fails, and export health says so.
           "OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://127.0.0.1:9/v1/metrics",
           "OTEL_METRIC_EXPORT_INTERVAL": "2000", "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT": "1"}
    ok = True
    try:
        # The server's log times are the same host's clock in seconds since it started: debug_until
        # in the log's time is found from an event's wall clock.
        def done(events: list[dict]) -> bool:
            return len(statuses(events)) >= 6 and time.time() > debug_until + MARGIN + 8

        events = run(launch, {"server": {"faults": offers}}, state, "run-next", seconds, "127.0.0.1:0", target_,
                     until=done, settle=2, env=env)
        wall = next((e for e in events if "wall" in e), None)
        until = debug_until - wall["wall"] + wall["t"] if wall else debug_until
        print(f"run next: remote config statuses {[s for _, s, _ in statuses(events)]}")
        for what, cond in judge_next(events, until):
            ok &= cond
            print(f"  {'ok  ' if cond else 'FAIL'} {what}")
    finally:
        shutil.rmtree(state, ignore_errors=True)
    return ok


def of(app: str, run_check: Callable[[Path], bool]) -> Callable[[], bool]:
    """A check of `app` (APPS), in a directory of its own: run_check(directory)."""
    def run_() -> bool:
        global APP
        APP = app
        directory = TARGET / app.replace(".", "-")
        directory.mkdir(exist_ok=True)
        return run_check(directory)
    return run_


# The signals runs need deactivate_all, which configure() can't carry out; the next-settings run
# needs app.py's health checks and second logger: app.py's alone. The setup runs are both's: the key
# in OTEL_OPAMP_HEADERS, then given to configure() (from CHECKOUT_OPAMP_KEY) with the other key in
# OTEL_OPAMP_HEADERS, which the code wins over.
CHECKS = {
    "first": of("app.py", lambda d: check(launch, "urllib", d)),
    "signals": of("app.py", lambda d: check_signals(launch, d)),
    # With the logging handler installed, the agent's warnings are log records, not stderr.
    "setup": of("app.py", lambda d: check_setup(launch, d, env={"OTEL_PYTHON_LOGGING_AUTO_INSTRUMENTATION_ENABLED": "false"})),
    "next": of("app.py", lambda d: check_next(launch, d)),
    "app": of("app.py", lambda d: check_app_settings(launch, d)),
    "first-in-code": of("in_code.py", lambda d: check(launch, "urllib", d, switches=False)),
    "setup-in-code": of("in_code.py", lambda d: check_setup(
        launch, d, env={"OTEL_PYTHON_LOGGING_AUTO_INSTRUMENTATION_ENABLED": "false"},
        key=key_in_code("CHECKOUT_OPAMP_KEY"), named="the headers given to configure()")),
}

if __name__ == "__main__":
    # Resolve the environment once, so the runs don't spend their time installing.
    sys.exit(main(CHECKS, lambda: subprocess.run([*UV, "python", "-c", "import opamp_otel"], check=True)))
