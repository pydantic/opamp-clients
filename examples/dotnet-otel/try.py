# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""OpenTelemetry .NET automatic instrumentation with our plugin, against the reference server.

    python3 examples/dotnet-otel/try.py            # builds and runs in Docker
    python3 examples/dotnet-otel/try.py --local    # this host's .NET SDK, its NuGet cache, and its native core

Program.cs has no OpenTelemetry code: the HttpClient instrumentation traces its requests, the
ILogger instrumentation exports its log lines, and, with APP_LISTEN, the ASP.NET Core
instrumentation traces the requests it serves. The checks are every integration's
(examples/integrations.py), check_signals's included, then the newer settings, each against the
reference server, with the telemetry going through a recording proxy on its way to the reference
sink (check_newer_settings below). Runs with the automatic instrumentation NuGet package's
instrument.sh.

Run 7, the policies in the order the server offers them, each APPLIED, while this script sends
the application's ASP.NET Core server /ping and /health requests of its own (no traceparent: each
a new trace, which the sampler leaves to the instrumentation's Filter):
  1. sampling_rules, a rule of rate 0 for the path /health*, and head_sampling 1: the server's
     /ping requests are traced, its /health requests are not;
  2. log_levels scopes app.noisy off: the app logger's records are exported, app.noisy's are not;
  3. attribute_redaction drop server.address and mask http.request.method: spans arrive without
     server.address and with the method "[REDACTED]";
  4. trace_rate_limit 1 and log_rate_limit 1: about one trace and one log record a second, where
     the application makes some seven traces and ten log lines;
  5. head_sampling 1 alone, a version 1 policy: the caps, the rules, the scope level and redaction
     are gone (local).
Run 8, debug mode: head_sampling 0 with debug until some 40 s ahead, at the first message: spans
until then (debug's sampling_ratio, 1), none after it, and the effective config reported again at
the end, the server sending nothing new.
Run 9, export health: the proxy refuses the trace exports for a minute: the agent sends its
health again once the exports fail (at its next 30 s report), and again once they succeed.

Then check_app_settings (examples/integrations.py, its own runs 7 and 8): Program.cs reads the
feature flag example.feature (OpAmpAppSettings.Get), which the server turns on and off, and is
told of each change (OpAmpAppSettings.OnChange) (docs/app-settings.md).

Last, check_truth (examples/integrations.py) under the setups Setups.cs carries out, named in
EXAMPLE_SETUP: plugin-exporters, a plugin of the application's own adding a span exporter and a
log record processor; own-sdk, the application's own tracer and meter providers under
OTEL_DOTNET_AUTO_SETUP_SDK=false. Each setting shows in the telemetry or is named as set aside,
and the settings set aside are exactly those the setup takes from the plugin.
"""
from __future__ import annotations

import gzip
import http.server
import os
import platform
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts"))
import native_lib  # noqa: E402
from integrations import (  # noqa: E402
    MARGIN, ROOT, check, check_app_settings, check_setup, check_rotation, check_signals, check_truth, ended, main, offer, pings, run, sent_before,
    statuses, stop_container, stop_group, target, opamp_url,
)

IMAGE = "mcr.microsoft.com/dotnet/sdk:8.0"
# NuGet's package folder, a Docker volume: OPAMP_EXAMPLES_NUGET_VOLUME names another, for a run
# beside this one (two restores writing into one at once can leave it half written).
NUGET = ["-v", f"{os.environ.get('OPAMP_EXAMPLES_NUGET_VOLUME') or 'opamp-clients-nuget'}:/root/.nuget/packages"]
TARGET = target("dotnet-otel")
# Not "app": on a case-insensitive disk that is the published "App" of an older layout.
OUT = TARGET / "publish"
LOCAL = "--local" in sys.argv
HOST = "127.0.0.1" if LOCAL else "host.docker.internal"


def build() -> None:
    rid = "linux-arm64" if platform.machine() in ("arm64", "aarch64") else "linux-x64"
    if LOCAL:
        # The native core is this checkout's (target/release, which the projects take).
        subprocess.run(["dotnet", "publish", str(Path(__file__).resolve().parent / "App.csproj"), "-c", "Release",
                        "-r", rid, "--self-contained", "false", "--nologo", "-v", "q", "-o", str(OUT)],
                       check=True, stdout=subprocess.DEVNULL)
        return
    # The native core for Linux whatever the host (scripts/native_lib.py).
    lib = native_lib.build()
    subprocess.run(["docker", "run", "--rm", "--network", "host", "-v", f"{ROOT}:/work", *NUGET, "-w", "/work/examples/dotnet-otel", IMAGE,
                    "dotnet", "publish", "-c", "Release", "-r", rid, "--self-contained", "false", "--nologo", "-v", "q",
                    f"-p:OpAmpNativeLib=/work/{lib}", "-o", f"/work/{OUT.relative_to(ROOT)}"], check=True, stdout=subprocess.DEVNULL)


def launch(port: str, telemetry_port: str, state: Path, name: str, extra: dict[str, str]):
    telemetry = f"http://{HOST}:{telemetry_port}"
    env = {
        "OTEL_DOTNET_AUTO_PLUGINS": "Pydantic.OpAmp.AutoInstrumentation.Plugin, Pydantic.OpAmp.AutoInstrumentation",
        "OTEL_SERVICE_NAME": "checkout",
        "OTEL_OPAMP_ENDPOINT": opamp_url(port, HOST),
        "OTEL_OPAMP_HEADERS": "Authorization=Bearer%20example",
        "OTEL_OPAMP_STATE_DIR": str(state) if LOCAL else "/state",
        "OTEL_OPAMP_HEARTBEAT_INTERVAL": "1s",
        "OTEL_EXPORTER_OTLP_ENDPOINT": telemetry,
        "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
        "OTEL_METRICS_EXPORTER": "none",
        "OTEL_LOGS_EXPORTER": "none",
        "OTEL_BSP_SCHEDULE_DELAY": "500",
        # Program.cs's own activities (run 3's probe): exported only from a source named here.
        "OTEL_DOTNET_AUTO_TRACES_ADDITIONAL_SOURCES": "app",
    } | extra
    stderr = open(TARGET / f"{name}.stderr", "w")
    if LOCAL:
        listen = env.pop("APP_PORT", None)
        if listen:
            env["APP_LISTEN"] = f"http://127.0.0.1:{listen}"
        app = subprocess.Popen(["./instrument.sh", "dotnet", "App.dll", f"{telemetry}/ping"], cwd=OUT,
                               env=os.environ | env, start_new_session=True, stdout=subprocess.DEVNULL, stderr=stderr)
        return lambda: stop_group(app)
    # By this process too: two runs of one check started in the same second (one per transport,
    # side by side) would otherwise both ask for the same name.
    container = f"opamp-dotnet-otel-{name}-{os.getpid()}-{int(time.time())}"
    cmd = ["docker", "run", "--rm", "--name", container, "--add-host=host.docker.internal:host-gateway",
           "-v", f"{OUT}:/app:ro", "-v", f"{state}:/state", "-w", "/app"]
    if listen := env.pop("APP_PORT", None):
        cmd += ["-p", f"127.0.0.1:{listen}:8080"]
        env["APP_LISTEN"] = "http://0.0.0.0:8080"
    for k, v in env.items():
        cmd += ["-e", f"{k}={v}"]
    cmd += [IMAGE, "./instrument.sh", "dotnet", "App.dll", f"{telemetry}/ping"]
    app = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=stderr)

    return lambda: stop_container(app, container)


# --- A recording OTLP proxy: what the gates exported, attributes and scopes included -------------

def fields(data: bytes) -> list[tuple[int, int, object]]:
    """A protobuf message's fields, in order: (number, wire type, value). Lengths stay bytes."""
    out, i = [], 0

    def varint() -> int:
        nonlocal i
        n = shift = 0
        while True:
            b = data[i]
            i += 1
            n |= (b & 0x7F) << shift
            shift += 7
            if b < 0x80:
                return n

    while i < len(data):
        key = varint()
        number, wire = key >> 3, key & 7
        if wire == 0:
            value: object = varint()
        elif wire == 1:
            value, i = data[i:i + 8], i + 8
        elif wire == 2:
            n = varint()
            value, i = data[i:i + n], i + n
        elif wire == 5:
            value, i = data[i:i + 4], i + 4
        else:
            raise ValueError(f"wire type {wire}")
        out.append((number, wire, value))
    return out


def sub(data: bytes, number: int) -> list[bytes]:
    return [v for n, w, v in fields(data) if n == number and w == 2]


def any_value(data: bytes) -> object:
    for n, _, v in fields(data):
        if n == 1:
            return v.decode()
        if n == 2:
            return bool(v)
        if n == 3:
            return v - (1 << 64) if v >= 1 << 63 else v
    return None


def attributes(data: bytes, number: int) -> dict[str, object]:
    out = {}
    for kv in sub(data, number):
        key = sub(kv, 1)
        value = sub(kv, 2)
        out[key[0].decode() if key else ""] = any_value(value[0]) if value else None
    return out


def decode(signal_: str, body: bytes) -> list[dict]:
    """Each span (name, kind, attributes) or log record (scope, severity, attributes) of an export."""
    items = []
    for resource in sub(body, 1):
        for scoped in sub(resource, 2):
            scope = (sub(sub(scoped, 1)[0], 1) or [b""])[0].decode() if sub(scoped, 1) else ""
            for item in sub(scoped, 2):
                if signal_ == "traces":
                    kind = next((v for n, w, v in fields(item) if n == 6 and w == 0), 0)
                    items.append({"name": (sub(item, 5) or [b""])[0].decode(), "kind": kind,
                                  "attributes": attributes(item, 9)})
                else:
                    severity = next((v for n, w, v in fields(item) if n == 2 and w == 0), 0)
                    items.append({"scope": scope, "severity": severity, "attributes": attributes(item, 6)})
    return items


class Proxy:
    """Takes the agent's OTLP exports, records what is in them, and passes them on to the reference
    sink; while `refusing` names a signal, answers its exports 503 instead."""

    def __init__(self) -> None:
        self.records: list[dict] = []
        self.forward_to = ""
        self.refusing: set[str] = set()
        proxy = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self) -> None:
                body = self.rfile.read(int(self.headers.get("content-length", 0)))
                signal_ = self.path.rsplit("/", 1)[-1]
                if signal_ in proxy.refusing:
                    proxy.records.append({"wall": time.time(), "signal": signal_, "refused": True, "items": []})
                    self.send_response(503)
                    self.send_header("content-length", "0")
                    self.end_headers()
                    return
                plain = gzip.decompress(body) if self.headers.get("content-encoding") == "gzip" else body
                try:
                    items = decode(signal_, plain) if signal_ in ("traces", "logs") else []
                except (ValueError, IndexError, UnicodeDecodeError):
                    items = []
                proxy.records.append({"wall": time.time(), "signal": signal_, "items": items})
                request = urllib.request.Request(proxy.forward_to + self.path, data=body, method="POST",
                                                 headers={k: v for k, v in self.headers.items()
                                                          if k.lower() in ("content-type", "content-encoding")})
                try:
                    with urllib.request.urlopen(request, timeout=5) as r:
                        r.read()
                except OSError:  # the sink refused it or is unreachable: the SDK sees that too
                    self.send_response(502)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("content-type", "application/x-protobuf")
                self.send_header("content-length", "0")
                self.end_headers()

            def log_message(self, *args: object) -> None:
                pass

        self.server = http.server.ThreadingHTTPServer(("0.0.0.0", 0), Handler)
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def wrap(self, launch_, app_port: int):
        """The application, its telemetry sent through the proxy, serving on `app_port`; its
        requests still go to the sink."""
        def go(port: str, telemetry_port: str, state: Path, name: str, extra: dict[str, str]):
            self.forward_to = f"http://127.0.0.1:{telemetry_port}"
            self.records.clear()
            return launch_(port, telemetry_port, state, name,
                           extra | {"OTEL_EXPORTER_OTLP_ENDPOINT": f"http://{HOST}:{self.port}", "APP_PORT": str(app_port)})
        return go

    def items(self, signal_: str, after_wall: float, before_wall: float = 1e12) -> list[dict]:
        return [i for r in self.records if r["signal"] == signal_ and after_wall < r["wall"] < before_wall
                for i in r["items"]]


class Caller:
    """Requests to the application's server, /ping and /health in turn, every 100 ms while on: with
    no traceparent, each request is a new trace there."""

    def __init__(self, port: int) -> None:
        self.base = f"http://127.0.0.1:{port}"
        self.on = threading.Event()
        threading.Thread(target=self.loop, daemon=True).start()

    def loop(self) -> None:
        n = 0
        while True:
            self.on.wait()
            path = "/health" if n % 2 else "/ping"
            n += 1
            try:
                with urllib.request.urlopen(self.base + path, timeout=2) as r:
                    r.read()
            except OSError:
                pass  # not serving yet, or gone
            time.sleep(0.1)


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def check_newer_settings(launch_, target_: Path, listen: str) -> bool:
    """Runs 7, 8 and 9 (above), and every check printed; True when all pass."""
    proxy = Proxy()
    app_port = free_port()
    caller = Caller(app_port)
    launch_ = proxy.wrap(launch_, app_port)
    ok = True

    def expect(what: str, cond: bool) -> None:
        nonlocal ok
        ok &= cond
        print(f"  {'ok  ' if cond else 'FAIL'} {what}")

    signals = {"OTEL_LOGS_EXPORTER": "otlp", "OTEL_BLRP_SCHEDULE_DELAY": "500"}
    gap = 10  # messages: some 10 s at the agent's pace (one a second)
    rules = [{"rate": 0, "path": "/health*"}]
    scenario = {"server": {"faults": [
        offer(1, {"head_sampling": {"ratio": 1}, "sampling_rules": {"rules": rules}}),
        offer(1 + gap, {"head_sampling": {"ratio": 1}, "sampling_rules": {"rules": rules},
                        "log_levels": {"scopes": {"app.noisy": "off"}}}),
        offer(1 + 2 * gap, {"head_sampling": {"ratio": 1},
                            "attribute_redaction": {"drop": ["server.address"], "mask": ["http.request.method"]}}),
        offer(1 + 3 * gap, {"head_sampling": {"ratio": 1}, "trace_rate_limit": {"traces_per_second": 1},
                            "log_rate_limit": {"records_per_second": 1}}),
        offer(1 + 4 * gap, {"head_sampling": {"ratio": 1}}),
    ]}}
    state = Path(tempfile.mkdtemp(prefix="state-newer-", dir=target_))
    state.chmod(0o777)
    try:
        caller.on.set()
        seven = run(launch_, scenario, state, "run7", 240, listen, target_,
                    until=lambda events: len(statuses(events)) >= 5, settle=10, env=signals)
        caller.on.clear()
        st = statuses(seven)
        print(f"run 7: remote config statuses {[s for _, s, _ in st]}, {len(proxy.records)} exports through the proxy")
        expect("five configs answered APPLIED", [s for _, s, _ in st] == ["APPLIED"] * 5)
        if len(st) == 5:
            offset = next(e["wall"] - e["t"] for e in seven)
            t = [offset + tt for tt, _, _ in st]
            # Each window ends when the server sent the next config, not when its APPLIED came
            # back: what the agent exports in between is already held to the next one.
            sent = sent_before(seven, st, offset)

            def served(a: float, b: float = 1e12) -> list[str]:
                """The paths of the server spans (kind 2) exported between a and b."""
                return [str(i["attributes"].get("url.path", "")) for i in proxy.items("traces", a + MARGIN, b)
                        if i["kind"] == 2]

            one = served(t[0], sent[1])
            expect(f"sampling_rules: /ping served and traced ({one.count('/ping')}), /health not "
                   f"({one.count('/health')})", "/ping" in one and "/health" not in one)
            two = proxy.items("logs", t[1] + MARGIN, sent[2])
            scopes = {i["scope"] for i in two}
            expect(f"log_levels scopes: app's records exported, app.noisy's not ({sorted(scopes)})",
                   "app" in scopes and "app.noisy" not in scopes)
            before = proxy.items("logs", t[0] + MARGIN, t[1])
            expect("app.noisy's records were exported before", any(i["scope"] == "app.noisy" for i in before))
            three = proxy.items("traces", t[2] + MARGIN, sent[3])
            expect(f"attribute_redaction drop: no span with server.address ({len(three)} spans)",
                   bool(three) and not any("server.address" in i["attributes"] for i in three))
            expect("attribute_redaction mask: every span's http.request.method is [REDACTED]",
                   bool(three) and all(i["attributes"].get("http.request.method") == "[REDACTED]" for i in three))
            expect("before them, spans had both", any("server.address" in i["attributes"]
                                                      for i in proxy.items("traces", t[0] + MARGIN, t[1])))
            expect("and /health is traced again: the rules went with the config that left them out",
                   "/health" in served(t[2], sent[3]))
            # The caps' window too: the agent lifts them before it answers (CI has seen 1.4 s
            # between), and the log cap applies at export, so records exported in that gap go
            # uncapped.
            end = sent[4]
            seconds = end - t[3] - MARGIN
            spans_ = len(proxy.items("traces", t[3] + MARGIN, end))
            logs = len(proxy.items("logs", t[3] + MARGIN, end))
            # A second's burst at most beyond the rate; the application makes some seven traces and
            # ten log records a second.
            expect(f"trace_rate_limit 1: {spans_} spans in {seconds:.0f} s",
                   seconds > 3 and 0 < spans_ <= seconds + 2)
            expect(f"log_rate_limit 1: {logs} log records in {seconds:.0f} s",
                   seconds > 3 and 0 < logs <= seconds + 2)
            after = proxy.items("traces", t[4] + MARGIN)
            expect(f"the caps removed: more again ({len(after)} spans)", len(after) > 2 * spans_ / max(seconds, 1))
            expect("and redaction gone: server.address again", any("server.address" in i["attributes"] for i in after))

        # Run 8: debug mode, ending on the agent's own timer.
        window = 40
        until = float(int(time.time()) + window)
        stamp = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(until))
        scenario = {"server": {"faults": [offer(1, {"head_sampling": {"ratio": 0}, "debug": {"until": stamp}})]}}
        eight = run(launch_, scenario, state, "run8", window + 60, listen, target_,
                    until=lambda events: time.time() > until + 15, settle=0)
        st = statuses(eight)
        print(f"run 8: remote config statuses {[s for _, s, _ in st]}, debug until {stamp}")
        expect("debug mode answered APPLIED", [s for _, s, _ in st] == ["APPLIED"])
        if st and eight:
            offset = next(e["wall"] - e["t"] for e in eight)
            during = ended(eight, st[0][0] + MARGIN, until - offset)
            expect(f"in debug mode, spans at the debug rate though head_sampling is 0 ({len(during)})", bool(during))
            made = pings(eight, until - offset + MARGIN)
            expect(f"after debug's until, none: the timer ended it ({len(made)} requests)",
                   len(made) > 5 and not ended(eight, until - offset + 1))
            late = [e for e in eight if e["event"] == "a2s" and e["t"] > until - offset - 1
                    and "effective_config" in e["fields"]]
            expect("the effective config reported again at the end", bool(late))

        # Run 9: export health, reported every 30 s.
        refuse = (15.0, 75.0)
        started = time.time()

        def refusing(events: list[dict]) -> bool:
            now = time.time() - started
            proxy.refusing = {"traces"} if refuse[0] <= now < refuse[1] else set()
            return now > refuse[1] + 40

        # A state directory of its own: run 8 left head_sampling 0 in force.
        state9 = Path(tempfile.mkdtemp(prefix="state-health-", dir=target_))
        state9.chmod(0o777)
        nine = run(launch_, {"server": {}}, state9, "run9", 200, listen, target_, until=refusing, settle=0)
        proxy.refusing = set()
        shutil.rmtree(state9, ignore_errors=True)
        health = [e["wall"] - started for e in nine if e["event"] == "a2s" and "health" in e["fields"]]
        print(f"run 9: trace exports refused from {refuse[0]:.0f} s to {refuse[1]:.0f} s; health sent at "
              f"{[round(h) for h in health]} s")
        expect("health sent at the start", bool(health) and health[0] < refuse[0])
        expect("health sent again once the exports fail", any(refuse[0] < h <= refuse[1] + 5 for h in health))
        expect("and again once they succeed", any(h > refuse[1] for h in health))
        expect("not again while nothing but the counters changes, before the failures",
               bool(health) and not any(h < refuse[0] for h in health[1:]))
    finally:
        caller.on.clear()
        proxy.server.shutdown()
        shutil.rmtree(state, ignore_errors=True)
    return ok


#: The plugin first, then the application's own (Setups.cs), which automatic instrumentation calls after it.
PLUGINS = ("Pydantic.OpAmp.AutoInstrumentation.Plugin, Pydantic.OpAmp.AutoInstrumentation:"
           "Example.ExtraExporters, App")
#: What a span exporter and a log record processor of the application's own plugin take from the
#: plugin's control: their gates' settings, set aside. The sampler stays the plugin's.
PLUGIN_EXPORTERS = {"send_traces", "send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second",
                    "mask_attributes", "drop_attributes"}
#: What the application's own tracer and meter providers take (OTEL_DOTNET_AUTO_SETUP_SDK=false):
#: the sampler and the traces and metrics gates. The logs stay automatic instrumentation's.
OWN_SDK = {"send_traces", "send_metrics", "sampling_rate", "sampling_rules", "sampling_traces_per_second",
           "mask_attributes", "drop_attributes"}

LISTEN = "127.0.0.1:0" if LOCAL else "0.0.0.0:0"
CHECKS = {
    "first": lambda: check(launch, "httpclient", TARGET, listen=LISTEN),
    "signals": lambda: check_signals(launch, TARGET, listen=LISTEN),
    "setup": lambda: check_setup(launch, TARGET, listen=LISTEN),
    "rotation": lambda: check_rotation(launch, TARGET, listen=LISTEN, host=HOST),
    "newer": lambda: check_newer_settings(launch, TARGET, listen=LISTEN),
    "app": lambda: check_app_settings(launch, TARGET, listen=LISTEN),
    "truth-plugin-exporters": lambda: check_truth(launch, TARGET, "plugin-exporters", PLUGIN_EXPORTERS, listen=LISTEN,
                                                  env={"OTEL_DOTNET_AUTO_PLUGINS": PLUGINS}),
    "truth-own-sdk": lambda: check_truth(launch, TARGET, "own-sdk", OWN_SDK, listen=LISTEN,
                                         env={"OTEL_DOTNET_AUTO_SETUP_SDK": "false"}),
}

if __name__ == "__main__":
    sys.exit(main(CHECKS, build, options=("--local",)))
