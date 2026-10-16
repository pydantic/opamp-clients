# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""node --import @pydantic/opamp-otel/register, against the reference server.

    python3 examples/node-otel/try.py

app.cjs has no OpenTelemetry setup: the http instrumentation traces its requests, and it logs
each through the logs API. The checks are every integration's (examples/integrations.py),
check_signals's included, run for app.cjs and again for app.mjs, the same program as an ES
module (instrumented through the loader hook the register installs). check() runs a third time
for in-code.mjs, which gives the OpAMP server to register() in code: run with plain `node`, no
OTEL_OPAMP_* variable set, and check_setup a third time for it, with the key given to register()
(from APP_OPAMP_TOKEN) and the other key in OTEL_OPAMP_HEADERS, which the code wins over. (
check_signals needs the application's log lines, which in-code.mjs doesn't write: it runs for the
others.)

Then check_settings, Node.js's own for now: the settings that match telemetry
(docs/node-otel.md#the-settings-that-match-telemetry), against the reference server, with app.cjs
exporting OTLP/JSON to a sink here that reads every attribute (EXAMPLE_SETTINGS=1: a span of its
own around each request, a health check beside it, three loggers).
Run 7, two documents:
  1. sampling_rules (rate 0 for client requests whose url.full has /health), send_logs_level warn with
     send_logs_level_by_scope app=info, noisy=off, drop_attributes http.request.header.*,
     mask_attributes user.email: APPLIED; no /health span, /ping and checkout spans traced; the
     authorization header dropped and the e-mail masked on spans and log records; app's INFO records
     exported, none of noisy's, and other's INFO dropped by the global level;
  2. sampling_rate 1, sampling_traces_per_second 1, send_logs_per_second 2: APPLIED; about a trace
     a second, about two log records a second, where the application makes ten and fifteen.
Run 8, debug mode and export health:
  1. sampling_rate 0, send_logs_level error, debug_until some 20 s ahead, debug_send_logs_level
     info: APPLIED; spans and INFO records exported in the window. The server is then stopped;
     once the window is over, neither is exported: the end comes without the server;
  2. the sink refuses traces (HTTP 400) and the server comes back: the agent sends its health
     again, and says the traces exporter is failing.

Then check_app_settings, for app.cjs and app.mjs: each reads the feature flag example.feature
through appSettings (app.cjs from @pydantic/opamp-otel/app-settings, app.mjs from the package
itself), which the server turns on and off (docs/app-settings.md). The package reaches them as
an installed one would, by its name: examples/node-otel/node_modules holds a link to node/otel.
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from integrations import (ROOT, check, check_app_settings, check_setup, check_signals, check_truth,  # noqa: E402
                          key_in_code, main, read_log, server_binary, statuses, stop_group, target)

HERE = Path(__file__).resolve().parent
TARGET = target("node-otel")
OTEL = ROOT / "node" / "otel"


def build() -> None:
    for d in [ROOT / "node" / "opamp", OTEL]:
        subprocess.run(["npm", "ci", "--no-audit", "--no-fund"], cwd=d, check=True, stdout=subprocess.DEVNULL)
        subprocess.run(["npm", "run", "build"], cwd=d, check=True, stdout=subprocess.DEVNULL)
    # in-code.mjs imports the package by name, and app.cjs and app.mjs require its appSettings by
    # name, as `npm install <this repository>/node/otel` in the application's directory would let them.
    link = HERE / "node_modules" / "@pydantic" / "opamp-otel"
    link.parent.mkdir(parents=True, exist_ok=True)
    if not link.is_symlink():
        link.symlink_to(OTEL, target_is_directory=True)


APP = "app.cjs"


def launch(port: str, telemetry_port: str, state: Path, name: str, extra: dict[str, str]):
    base = f"http://127.0.0.1:{port}"
    telemetry = f"http://127.0.0.1:{telemetry_port}"
    env = dict(os.environ,
               OTEL_SERVICE_NAME="checkout",
               OTEL_OPAMP_ENDPOINT=f"{base}/v1/opamp",
               OTEL_OPAMP_HEADERS="Authorization=Bearer%20example",
               OTEL_OPAMP_STATE_DIR=str(state),
               OTEL_OPAMP_HEARTBEAT_INTERVAL="1s",
               OTEL_EXPORTER_OTLP_ENDPOINT=telemetry,
               OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf",
               OTEL_METRICS_EXPORTER="none",
               OTEL_LOGS_EXPORTER="none",
               OTEL_BSP_SCHEDULE_DELAY="500",
               NODE_PATH=str(OTEL / "node_modules"))
    if APP == "in-code.mjs":
        # The server in the application's own settings, given to register() in code; none in
        # OTEL_OPAMP_*, here or inherited (a run may set one, to show the code wins).
        env = {k: v for k, v in env.items() if not k.startswith("OTEL_OPAMP_")}
        env.update(APP_OPAMP_URL=f"{base}/v1/opamp", APP_OPAMP_TOKEN="example", APP_STATE_DIR=str(state))
        command = ["node", str(HERE / APP)]
    else:
        command = ["node", "--import", str(OTEL / "dist/src/register.js"), str(HERE / APP)]
    app = subprocess.Popen([*command, f"{telemetry}/ping"], env=env | extra, start_new_session=True, stdout=subprocess.DEVNULL,
                           stderr=open(TARGET / APP.replace(".", "-") / f"{name}.stderr", "w"))

    return lambda: stop_group(app)


# --- check_settings -------------------------------------------------------------------------------

# An export on its way when a setting is answered may still arrive this much later.
SETTLE = 3.0


def value(v: dict):
    """An OTLP/JSON AnyValue as Python."""
    for key in ("stringValue", "boolValue", "doubleValue"):
        if key in v:
            return v[key]
    if "intValue" in v:
        return int(v["intValue"])
    return v


def attributes(item: dict) -> dict:
    """A span's or log record's attributes, by key."""
    return {a["key"]: value(a["value"]) for a in item.get("attributes", [])}


class Sink(ThreadingHTTPServer):
    """An OTLP/HTTP JSON receiver that keeps every span and log record with its attributes, each
    with when it arrived (time.monotonic()); `refuse` names signals it answers 400."""

    def __init__(self):
        self.spans: list[dict] = []
        self.logs: list[dict] = []
        self.requests: list[tuple[float, str]] = []
        self.refuse: set[str] = set()
        self.lock = threading.Lock()
        super().__init__(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.serve_forever, daemon=True).start()

    @property
    def port(self) -> str:
        return str(self.server_address[1])


class Handler(BaseHTTPRequestHandler):
    server: Sink

    def log_message(self, *args) -> None:
        pass

    def do_GET(self) -> None:  # the application's own requests: /ping, /health
        with self.server.lock:
            self.server.requests.append((time.monotonic(), self.path))
        self.send_response(404)
        self.end_headers()

    def body(self) -> bytes:
        """The request's body, chunked (as Node.js's OTLP exporter sends it) or not."""
        if self.headers.get("transfer-encoding", "").lower() != "chunked":
            return self.rfile.read(int(self.headers.get("content-length", 0)))
        out = b""
        while size := int(self.rfile.readline().split(b";")[0].strip() or b"0", 16):
            out += self.rfile.read(size)
            self.rfile.readline()
        self.rfile.readline()
        return out

    def do_POST(self) -> None:
        body = json.loads(self.body() or b"{}")
        now = time.monotonic()
        signal = self.path.rsplit("/", 1)[-1]
        with self.server.lock:
            for rs in body.get("resourceSpans", []):
                for ss in rs.get("scopeSpans", []):
                    for sp in ss.get("spans", []):
                        self.server.spans.append({"t": now, "name": sp["name"], "trace": sp["traceId"],
                                                  "parent": sp.get("parentSpanId", ""),
                                                  "attributes": attributes(sp)})
            for rl in body.get("resourceLogs", []):
                for sl in rl.get("scopeLogs", []):
                    for r in sl.get("logRecords", []):
                        self.server.logs.append({"t": now, "scope": sl.get("scope", {}).get("name", ""),
                                                 "severity": r.get("severityNumber", 0),
                                                 "attributes": attributes(r)})
        refused = signal in self.server.refuse
        self.send_response(400 if refused else 200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")


class Server:
    """The reference server, on a port of its own choosing or the one given, with a scenario of
    offers; its log read as it goes."""

    def __init__(self, name: str, offers: list[dict], listen: str = "127.0.0.1:0"):
        self.log = TARGET / f"{name}.jsonl"
        scenario = TARGET / f"{name}-scenario.json"
        scenario.write_text(json.dumps({"server": {"faults": [
            {"on_message": n, "action": "offer_config", "config_key": "elastic", "body": json.dumps(body),
             "content_type": "application/json"} for n, body in offers]}}))
        self.process = subprocess.Popen([str(TARGET / "server"), "-listen", listen, "-scenario", str(scenario),
                                         "-log", str(self.log)], stdout=subprocess.PIPE, text=True)
        listening = json.loads(self.process.stdout.readline())
        self.port = listening["addr"].rsplit(":", 1)[1]

    def events(self) -> list[dict]:
        return read_log(self.log)

    def stop(self) -> None:
        self.process.terminate()
        self.process.wait()


def wait(holds, seconds: float) -> bool:
    deadline = time.monotonic() + seconds
    while not holds():
        if time.monotonic() > deadline:
            return False
        time.sleep(0.25)
    return True


def answered(server: Server, n: int, seconds: float = 60) -> list[float]:
    """When (time.monotonic()) each of the first `n` remote configs was answered: seen as it goes."""
    seen: list[float] = []

    def more() -> bool:
        while len(seen) < len(statuses(server.events())):
            seen.append(time.monotonic())
        return len(seen) >= n

    wait(more, seconds)
    return seen


def settings_app(sink: Sink, opamp_port: str, state: Path, name: str):
    env = dict(os.environ,
               OTEL_SERVICE_NAME="checkout",
               OTEL_OPAMP_ENDPOINT=f"http://127.0.0.1:{opamp_port}/v1/opamp",
               OTEL_OPAMP_STATE_DIR=str(state),
               OTEL_OPAMP_HEARTBEAT_INTERVAL="1s",
               OTEL_EXPORTER_OTLP_ENDPOINT=f"http://127.0.0.1:{sink.port}",
               OTEL_EXPORTER_OTLP_PROTOCOL="http/json",
               OTEL_METRICS_EXPORTER="none",
               OTEL_LOGS_EXPORTER="otlp",
               OTEL_BSP_SCHEDULE_DELAY="300",
               OTEL_BLRP_SCHEDULE_DELAY="300",
               OTEL_LOG_LEVEL="info",
               EXAMPLE_SETTINGS="1",
               NODE_PATH=str(OTEL / "node_modules"))
    # What the package says: diag's console logger writes INFO to stdout, warnings to stderr.
    stderr = TARGET / "settings" / f"{name}.stderr"
    stderr.parent.mkdir(parents=True, exist_ok=True)
    out = open(stderr, "w")
    app = subprocess.Popen(["node", "--import", str(OTEL / "dist/src/register.js"), str(HERE / "app.cjs"),
                            f"http://127.0.0.1:{sink.port}/ping"], env=env, start_new_session=True,
                           stdout=out, stderr=subprocess.STDOUT)
    return app, stderr


def between(items: list[dict], after: float, before: float = float("inf")) -> list[dict]:
    return [i for i in items if after < i["t"] < before]


def check_settings() -> bool:
    ok = True

    def expect(what: str, cond: bool) -> None:
        nonlocal ok
        ok &= cond
        print(f"  {'ok  ' if cond else 'FAIL'} {what}")

    # The reference server, built (or found built) and copied here, as each shared check does.
    server_binary(TARGET)
    state = Path(tempfile.mkdtemp(prefix="state-settings-", dir=TARGET))
    # Run 7: the rules, levels per scope and redaction; then the caps.
    sink = Sink()
    first = {"sampling_rate": "1",
             "sampling_rules": [{"rate": 0, "kind": "client", "attributes": {"url.full": "*/health*"}}],
             "send_logs_level": "warn", "send_logs_level_by_scope": "app=info, noisy=off",
             "drop_attributes": "http.request.header.*", "mask_attributes": "user.email"}
    caps = {"sampling_rate": "1", "sampling_traces_per_second": "1", "send_logs_per_second": "2"}
    server = Server("run7", [(1, first), (16, caps)])
    app, _ = settings_app(sink, server.port, state, "run7")
    try:
        t1, t2 = (answered(server, 2, 90) + [0.0, 0.0])[:2]
        if t2:
            time.sleep(12)
        end = time.monotonic()
    finally:
        stop_group(app)
        server.stop()
        sink.shutdown()
    st = [s for _, s, _ in statuses(server.events())]
    print(f"run 7: remote config statuses {st}, {len(sink.spans)} spans and {len(sink.logs)} log records")
    expect("both configs answered APPLIED", st == ["APPLIED", "APPLIED"])
    if st == ["APPLIED", "APPLIED"]:
        # Config 1's window ends a second before the server logs config 2 APPLIED: the agent applies
        # it first, and the sink's clock and the server's log differ by a little.
        one_end = t2 - 1.0
        one = between(sink.spans, t1 + SETTLE, one_end)
        url = lambda s: str(s["attributes"].get("url.full", ""))  # noqa: E731
        made = [p for t, p in sink.requests if t1 + SETTLE < t < one_end and p == "/health"]
        expect(f"sampling_rules: no span of a request to /health ({len(made)} made)",
               len(made) > 5 and not any("/health" in url(s) for s in one))
        expect("sampling_rules: the requests to /ping still traced", any("/ping" in url(s) for s in one))
        checkout = [s for s in one if s["name"] == "checkout cart"]
        expect(f"drop_attributes: no span keeps the authorization header ({len(checkout)} checkout spans)",
               bool(checkout) and not any("http.request.header.authorization" in s["attributes"] for s in one))
        expect("mask_attributes: the e-mail masked on spans, other attributes kept",
               all(s["attributes"].get("user.email") == "[REDACTED]" and s["attributes"].get("tenant.id") == "acme"
                   for s in checkout))
        logs = between(sink.logs, t1 + SETTLE, one_end)
        scopes = {r["scope"] for r in logs}
        expect(f"send_logs_level_by_scope: app's INFO records exported, none of noisy's, and other's INFO "
               f"dropped by the global level (scopes {sorted(scopes)})", scopes == {"app"})
        expect("drop/mask on log records too",
               all(r["attributes"].get("user.email") == "[REDACTED]"
                   and "http.request.header.authorization" not in r["attributes"] for r in logs))
        span_window = between(sink.spans, t2 + SETTLE, end)
        roots = {s["trace"] for s in span_window if not s["parent"]}
        seconds = end - (t2 + SETTLE)
        before = {s["trace"] for s in one if not s["parent"]}
        rate_before = len(before) / max(t2 - t1 - SETTLE, 1)
        expect(f"sampling_traces_per_second 1: {len(roots)} traces in {seconds:.0f} s "
               f"(before: {rate_before:.1f} a second)",
               rate_before > 3 and 0 < len(roots) <= seconds + 3)
        records = between(sink.logs, t2 + SETTLE, end)
        expect(f"send_logs_per_second 2: {len(records)} records in {seconds:.0f} s",
               0 < len(records) <= 2 * seconds + 4)

    # Run 8: debug mode, ending with the server stopped; then export health.
    sink = Sink()
    until = datetime.now(UTC) + timedelta(seconds=20)
    debug = {"sampling_rate": "0", "send_logs_level": "error", "debug_until": until.strftime("%Y-%m-%dT%H:%M:%SZ"),
             "debug_send_logs_level": "info"}
    server = Server("run8a", [(1, debug)])
    port = server.port
    app, stderr = settings_app(sink, port, state, "run8")
    ends = time.monotonic() + (until - datetime.now(UTC)).total_seconds()
    try:
        (t1,) = (answered(server, 1, 15) + [0.0])[:1]
        st = [s for _, s, _ in statuses(server.events())]
        time.sleep(4)
        server.stop()  # the end of the window comes without the server
        left = ends - time.monotonic()
        print(f"run 8: debug mode answered {st}; the server stopped, {left:.0f} s of the window left")
        expect("debug mode answered APPLIED", st == ["APPLIED"])
        expect("debug mode: spans and INFO records in the window, though sampling_rate is 0 and the level error",
               bool(between(sink.spans, t1 + 1)) and any(r["severity"] == 9 for r in between(sink.logs, t1 + 1)))
        time.sleep(max(0.0, ends - time.monotonic()) + 6)
        after = ends + SETTLE
        expect(f"debug mode over without the server: no span or INFO record after it "
               f"({len(sink.requests)} requests made)",
               not between(sink.spans, after) and not any(r["severity"] < 17 for r in between(sink.logs, after)))
        # Export health: traces refused, with sampling back on for something to refuse.
        sink.refuse.add("traces")
        server = Server("run8b", [(1, {"sampling_rate": "1"})], listen=f"127.0.0.1:{port}")
        failing = wait(lambda: "export health: failing" in stderr.read_text(errors="replace"), 75)
        time.sleep(3)
        health = [e for e in server.events() if e["event"] == "a2s" and "health" in e["fields"]]
        expect("export health: the agent says the traces exporter is failing", failing)
        # A message with only health in it: sent for the change, not with a reconnect's full state.
        expect(f"export health: sent to the server on its own ({len(health)} messages with health), "
               "the top level still healthy",
               any(e["fields"] == ["health"] for e in health) and all(e["health"]["healthy"] for e in health))
    finally:
        stop_group(app)
        server.stop()
        sink.shutdown()
        shutil.rmtree(state, ignore_errors=True)
    return ok


# --- check_truth's setups (setups.cjs) ----------------------------------------------------------

#: What a tracer provider the application registers in place of the register's takes from the
#: package's control: its own spans go past the sampler and the trace gate, so the settings either
#: holds are set aside. Redaction too, which needs every span and log record.
OWN_TRACER_PROVIDER = {"send_traces", "sampling_rate", "sampling_rules", "sampling_traces_per_second",
                       "mask_attributes", "drop_attributes"}
#: A logger provider of its own: the log gate's settings, and redaction.
OWN_LOGGER_PROVIDER = {"send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second",
                       "mask_attributes", "drop_attributes"}
#: A meter provider of its own: send_metrics.
OWN_METER_PROVIDER = {"send_metrics"}


def of(app: str, run_check: Callable[[Path], bool]) -> Callable[[], bool]:
    """A shared check of `app`, in a directory of its own: run_check(directory)."""
    def run_() -> bool:
        global APP
        APP = app
        directory = TARGET / app.replace(".", "-")
        directory.mkdir(exist_ok=True)
        return run_check(directory)
    return run_


# Each shared check of app.cjs and app.mjs, the first and the setup of in-code.mjs (it writes no log
# lines), then the settings that match telemetry (app.cjs), then the
# application's own settings (app-cjs, app-mjs). net and dns trace the same requests at the socket
# level, and are on by default.
CHECKS = {
    f"{name}-{app.removeprefix('app.')}": of(app, run_check)
    for app in ["app.cjs", "app.mjs"]
    for name, run_check in [
        ("first", lambda d: check(launch, "http,net,dns", d, switch_at_once=True)),
        ("signals", lambda d: check_signals(launch, d, switch_at_once=True)),
        ("setup", lambda d: check_setup(launch, d)),
    ]
} | {
    "first-in-code": of("in-code.mjs", lambda d: check(launch, "http,net,dns", d, switch_at_once=True)),
    "setup-in-code": of("in-code.mjs", lambda d: check_setup(launch, d, key=key_in_code("APP_OPAMP_TOKEN"),
                                                             named="the headers given to register()")),
    "settings": check_settings,
} | {
    f"app-{app.removeprefix('app.')}": of(app, lambda d: check_app_settings(launch, d))
    for app in ["app.cjs", "app.mjs"]
} | {
    # The application's own spans aren't an instrumentation's: deactivate_all_instrumentations
    # doesn't stop them, and the check can't tell them apart, so it isn't judged there.
    "truth-own-tracer-provider": of("app.cjs", lambda d: check_truth(launch, d, "own-tracer-provider",
                                                                     OWN_TRACER_PROVIDER)),
    "truth-own-logger-provider": of("app.cjs", lambda d: check_truth(launch, d, "own-logger-provider",
                                                                     OWN_LOGGER_PROVIDER, switch_at_once=True)),
    "truth-own-meter-provider": of("app.cjs", lambda d: check_truth(launch, d, "own-meter-provider",
                                                                    OWN_METER_PROVIDER, switch_at_once=True)),
}

if __name__ == "__main__":
    sys.exit(main(CHECKS, build))
