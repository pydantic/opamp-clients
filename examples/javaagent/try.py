# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The upstream OpenTelemetry Java agent with our extension, against the reference server.

    python3 examples/javaagent/try.py            # builds with Maven and runs the JVM in Docker
    python3 examples/javaagent/try.py --local    # this host's Maven and JDK (MAVEN_ARGS is passed on)

App.java has no OpenTelemetry code: the agent traces its HTTP requests and exports its
java.util.logging lines. The checks are every integration's (examples/integrations.py),
check_signals's included, then the settings that match telemetry, each against the reference
server, with the telemetry going through a recording proxy on its way to the reference sink
(check_matching_settings below):

Run 7, the policies in the order the server offers them, each APPLIED:
  1. sampling_rules, a rule of rate 0 for client spans to */health*, and head_sampling 1: the
     application's /ping requests are traced, its /health checks are not;
  2. log_levels scopes app.noisy off: the app logger's records are exported, app.noisy's are not;
  3. attribute_redaction drop server.address and mask http.request.method: spans arrive without
     server.address and with the method "[REDACTED]";
  4. trace_rate_limit 1 and log_rate_limit 1: about one trace and one log record a second, where
     the application makes five requests and ten log lines;
  5. head_sampling 1 alone, a version 1 policy: the caps are gone (local).
Run 8, debug mode: head_sampling 0 with debug until some 40 s ahead, at the first message: spans
until then (debug's sampling_ratio, 1), none after it, and the effective config reported again at
the end, the server sending nothing new.
Run 9, export health: the proxy refuses the trace exports for a while: the agent sends its
health again once the exports fail (after the exporter's retries), and again once they succeed.

Then check_truth (examples/integrations.py), once for each setup Setups.java makes, a second
agent extension built here and given to the agent only when EXAMPLE_SETUP names a setup:
extension-processor (a span processor, a log record processor and a metric reader of its own,
exporting to the same sink) and extension-sampler (a sampler of its own). Each setting the server
offers must show in the telemetry or be named as set aside, and the set-aside settings must be
exactly what the setup takes from the extension's control.

Then check_app_settings (examples/integrations.py, its own runs 7 and 8): App.java reads the
feature flag example.feature (AppSettings.get), which the server turns on and off, and is told of
each change (AppSettings.onChange) (docs/app-settings.md). App.java is compiled against, and run
with, the extension's app-settings jar; the extension fills it through the bootstrap class loader.
"""
from __future__ import annotations

import gzip
import http.server
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
import zipfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from integrations import (  # noqa: E402
    MARGIN, ROOT, check, check_app_settings, check_setup, check_rotation, check_signals, check_truth, ended, main, offer, pings, run, sent_before, statuses,
    stop_container, stop_group, target, opamp_url,
)

HERE = Path(__file__).resolve().parent
TARGET = target("javaagent")
AGENT_VERSION = "2.31.1"
# Maven's cache, a Docker volume: OPAMP_EXAMPLES_M2_VOLUME names another, for a run beside this one
# (two builds installing into one at once can leave it half written).
M2 = ["-v", f"{os.environ.get('OPAMP_EXAMPLES_M2_VOLUME') or 'opamp-clients-m2'}:/root/.m2"]
LOCAL = "--local" in sys.argv


def build() -> None:
    if LOCAL:
        mvn = ["mvn", "-q", "-B", *os.environ.get("MAVEN_ARGS", "").split()]
        for module, goal in [("java/opamp", "install"), ("java/opamp-javaagent", "package")]:
            subprocess.run([*mvn, "-DskipTests", goal, "-f", str(ROOT / module / "pom.xml")], check=True,
                           stdout=subprocess.DEVNULL)
        artifact = f"io.opentelemetry.javaagent:opentelemetry-javaagent:{AGENT_VERSION}"
        subprocess.run([*mvn, "org.apache.maven.plugins:maven-dependency-plugin:3.9.0:copy", f"-Dartifact={artifact}",
                        f"-DoutputDirectory={TARGET}", "-Dmdep.stripVersion=true",
                        "-f", str(ROOT / "java/opamp-javaagent/pom.xml")], check=True, stdout=subprocess.DEVNULL)
        copy_jars()
        for name in SETUPS_CLASSPATH:
            subprocess.run([*mvn, "org.apache.maven.plugins:maven-dependency-plugin:3.9.0:copy",
                            f"-Dartifact=io.opentelemetry:{name}:{SDK_VERSION}", f"-DoutputDirectory={TARGET / 'setups-lib'}",
                            "-Dmdep.stripVersion=true", "-f", str(ROOT / "java/opamp-javaagent/pom.xml")], check=True,
                           stdout=subprocess.DEVNULL)
        classpath = ":".join(str(TARGET / "setups-lib" / f"{n}.jar") for n in SETUPS_CLASSPATH)
        subprocess.run(["javac", "-cp", classpath, "-d", str(TARGET / "setups"), str(HERE / "Setups.java")], check=True)
        subprocess.run(["javac", "-cp", f"{TARGET / APP_SETTINGS}:{app_api(TARGET)}",
                        "-d", str(TARGET / "app"), str(HERE / "App.java")], check=True)
    else:
        for module, goal in [("java/opamp", "install"), ("java/opamp-javaagent", "package")]:
            subprocess.run(["docker", "run", "--rm", "--network", "host", "-v", f"{ROOT / module}:/src", *M2, "-w",
                            "/src", "maven:3.9-eclipse-temurin-21", "mvn", "-q", "-B", "-DskipTests", goal],
                           check=True, stdout=subprocess.DEVNULL)
        subprocess.run(["docker", "run", "--rm", "--network", "host", *M2, "-v", f"{TARGET}:/out",
                        "maven:3.9-eclipse-temurin-21", "mvn", "-q", "-B", "dependency:copy",
                        f"-Dartifact=io.opentelemetry.javaagent:opentelemetry-javaagent:{AGENT_VERSION}",
                        "-DoutputDirectory=/out", "-Dmdep.stripVersion=true"], check=True, stdout=subprocess.DEVNULL)
        copy_jars()
        for name in SETUPS_CLASSPATH:
            subprocess.run(["docker", "run", "--rm", "--network", "host", *M2, "-v", f"{TARGET}:/out",
                            "maven:3.9-eclipse-temurin-21", "mvn", "-q", "-B", "dependency:copy",
                            f"-Dartifact=io.opentelemetry:{name}:{SDK_VERSION}", "-DoutputDirectory=/out/setups-lib",
                            "-Dmdep.stripVersion=true"], check=True, stdout=subprocess.DEVNULL)
        classpath = ":".join(f"/out/setups-lib/{n}.jar" for n in SETUPS_CLASSPATH)
        # Compiled, not run as a source file: the agent stays off under the JDK's source launcher.
        # Against the OpenTelemetry API too, for run 3's probe (App.java), which the agent bridges.
        subprocess.run(["docker", "run", "--rm", "-v", f"{HERE}:/app:ro", "-v", f"{TARGET}:/out",
                        "eclipse-temurin:21-jdk", "javac", "-cp", f"/out/{APP_SETTINGS}:{app_api('/out')}",
                        "-d", "/out/app", "/app/App.java"], check=True)
        subprocess.run(["docker", "run", "--rm", "-v", f"{HERE}:/app:ro", "-v", f"{TARGET}:/out",
                        "eclipse-temurin:21-jdk", "javac", "-cp", classpath, "-d", "/out/setups", "/app/Setups.java"],
                       check=True)
    package_setups()


#: The SDK the agent bundles, and what Setups.java (check_truth's second extension) compiles against.
SDK_VERSION = "1.66.0"
SETUPS_CLASSPATH = ["opentelemetry-api", "opentelemetry-context", "opentelemetry-common", "opentelemetry-sdk-common",
                    "opentelemetry-sdk-trace", "opentelemetry-sdk-logs", "opentelemetry-sdk-metrics",
                    "opentelemetry-sdk-extension-autoconfigure-spi", "opentelemetry-exporter-otlp"]
SETUPS = "example-setups.jar"


def package_setups() -> None:
    """Setups.java's classes as an agent extension of their own: a jar with its service file."""
    with zipfile.ZipFile(TARGET / SETUPS, "w") as jar:
        for f in sorted((TARGET / "setups").glob("*.class")):
            jar.write(f, f.name)
        jar.writestr("META-INF/services/io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider",
                     "Setups\n")


APP_SETTINGS = "opamp-app-settings.jar"


def app_api(root: object) -> str:
    """The OpenTelemetry API the application compiles against and runs with, for its probe, as a
    class path under `root` (where setups-lib is): the agent bridges it to its own SDK."""
    return ":".join(f"{root}/setups-lib/{n}.jar" for n in ("opentelemetry-api", "opentelemetry-context",
                                                           "opentelemetry-common"))


def copy_jars() -> None:
    """The extension, and the application's side of its own settings (AppSettings), which the
    program compiles against and runs with (docs/app-settings.md)."""
    built = ROOT / "java/opamp-javaagent/target"
    shutil.copy(built / "opamp-javaagent-extension-0.1.0-SNAPSHOT.jar", TARGET / "opamp-javaagent-extension.jar")
    shutil.copy(built / "opamp-javaagent-extension-0.1.0-SNAPSHOT-app-settings.jar", TARGET / APP_SETTINGS)


HOST = "127.0.0.1" if LOCAL else "host.docker.internal"


def launch(port: str, telemetry_port: str, state: Path, name: str, extra: dict[str, str]):
    telemetry = f"http://{HOST}:{telemetry_port}"
    env = {
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
    } | extra
    agent = TARGET if LOCAL else Path("/agent")
    extensions = f"{agent}/opamp-javaagent-extension.jar"
    if env.get("EXAMPLE_SETUP"):  # check_truth's setups: a second extension (Setups.java)
        extensions += f",{agent}/{SETUPS}"
    java = ["java", f"-javaagent:{agent}/opentelemetry-javaagent.jar", f"-Dotel.javaagent.extensions={extensions}",
            "-cp", f"{agent}/app:{agent}/{APP_SETTINGS}:{app_api(agent)}", "App", f"{telemetry}/ping"]
    stderr = open(TARGET / f"{name}.stderr", "w")
    if LOCAL:
        app = subprocess.Popen(java, env=os.environ | env, start_new_session=True, stdout=subprocess.DEVNULL,
                               stderr=stderr)
        return lambda: stop_group(app)
    # By this process too: two runs of one check started in the same second (one per transport,
    # side by side) would otherwise both ask for the same name.
    container = f"opamp-javaagent-{name}-{os.getpid()}-{int(time.time())}"
    cmd = ["docker", "run", "--rm", "--name", container, "--add-host=host.docker.internal:host-gateway",
           "-v", f"{TARGET}:/agent:ro", "-v", f"{state}:/state"]
    for k, v in env.items():
        cmd += ["-e", f"{k}={v}"]
    app = subprocess.Popen([*cmd, "eclipse-temurin:21-jdk", *java], stdout=subprocess.DEVNULL, stderr=stderr)
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
    """Each span (name, attributes) or log record (scope, severity, attributes) of an export."""
    items = []
    for resource in sub(body, 1):
        for scoped in sub(resource, 2):
            scope = (sub(sub(scoped, 1)[0], 1) or [b""])[0].decode() if sub(scoped, 1) else ""
            for item in sub(scoped, 2):
                if signal_ == "traces":
                    items.append({"name": (sub(item, 5) or [b""])[0].decode(), "attributes": attributes(item, 9)})
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
                except OSError:  # the sink refused it or is unreachable: the agent sees that too
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

    def wrap(self, launch_):
        """The application, its telemetry sent through the proxy; its requests still go to the sink."""
        def go(port: str, telemetry_port: str, state: Path, name: str, extra: dict[str, str]):
            self.forward_to = f"http://127.0.0.1:{telemetry_port}"
            self.records.clear()
            return launch_(port, telemetry_port, state, name,
                           extra | {"OTEL_EXPORTER_OTLP_ENDPOINT": f"http://{HOST}:{self.port}"})
        return go

    def items(self, signal_: str, after_wall: float, before_wall: float = 1e12) -> list[dict]:
        return [i for r in self.records if r["signal"] == signal_ and after_wall < r["wall"] < before_wall
                for i in r["items"]]


def check_matching_settings(launch_, target_: Path, listen: str) -> bool:
    """Runs 7, 8 and 9 (above), and every check printed; True when all pass."""
    proxy = Proxy()
    launch_ = proxy.wrap(launch_)
    ok = True

    def expect(what: str, cond: bool) -> None:
        nonlocal ok
        ok &= cond
        print(f"  {'ok  ' if cond else 'FAIL'} {what}")

    signals = {"OTEL_LOGS_EXPORTER": "otlp", "OTEL_BLRP_SCHEDULE_DELAY": "500"}
    gap = 20  # messages: some 10 s at the agent's pace
    rules = [{"rate": 0, "kind": "client", "attributes": {"url.full": "*/health*"}}]
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
    state = Path(tempfile.mkdtemp(prefix="state-matching-", dir=target_))
    state.chmod(0o777)
    try:
        seven = run(launch_, scenario, state, "run7", 240, listen, target_,
                    until=lambda events: len(statuses(events)) >= 5, settle=10, env=signals)
        st = statuses(seven)
        print(f"run 7: remote config statuses {[s for _, s, _ in st]}, {len(proxy.records)} exports through the proxy")
        expect("five configs answered APPLIED", [s for _, s, _ in st] == ["APPLIED"] * 5)
        if len(st) == 5:
            offset = next(e["wall"] - e["t"] for e in seven)
            t = [offset + tt for tt, _, _ in st]
            # Each window ends when the server sent the next config, not when its APPLIED came
            # back: what the agent exports in between is already held to the next one.
            sent = sent_before(seven, st, offset)

            def urls(a: float, b: float) -> list[str]:
                return [str(i["attributes"].get("url.full", "")) for i in proxy.items("traces", a + MARGIN, b)]

            one = urls(t[0], sent[1])
            expect(f"sampling_rules: /ping traced ({sum('/ping' in u for u in one)}), /health not "
                   f"({sum('/health' in u for u in one)})",
                   any("/ping" in u for u in one) and not any("/health" in u for u in one))
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
            seconds = sent[4] - t[3] - MARGIN
            spans = len(proxy.items("traces", t[3] + MARGIN, sent[4]))
            logs = len(proxy.items("logs", t[3] + MARGIN, sent[4]))
            # A second's burst at most beyond the rate; the application makes 5 traces and 10 records a second.
            expect(f"trace_rate_limit 1: {spans} spans in {seconds:.0f} s",
                   seconds > 3 and 0 < spans <= seconds + 2)
            expect(f"log_rate_limit 1: {logs} log records in {seconds:.0f} s",
                   seconds > 3 and 0 < logs <= seconds + 2)
            after = len(proxy.items("traces", t[4] + MARGIN))
            expect(f"the caps removed: more again ({after} spans)", after > 2 * spans / max(seconds, 1))

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

        # Run 9: export health.
        refuse = (15.0, 75.0)
        started = time.time()

        def refusing(events: list[dict]) -> bool:
            now = time.time() - started
            if refuse[0] <= now < refuse[1]:
                proxy.refusing = {"traces"}
            else:
                proxy.refusing = set()
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
        proxy.server.shutdown()
        shutil.rmtree(state, ignore_errors=True)
    return ok


#: What another extension's span processor, log record processor and metric reader (Setups.java)
#: take from the extension's control: their signals' gates, set aside. The sampler stays its own.
EXTENSION_PROCESSOR = {"send_traces", "send_metrics", "send_logs", "send_logs_level", "send_logs_level_by_scope",
                       "send_logs_per_second", "mask_attributes", "drop_attributes"}
#: What another extension's sampler (Setups.java) takes: the settings the extension's sampler holds.
EXTENSION_SAMPLER = {"sampling_rate", "sampling_rules", "sampling_traces_per_second"}

LISTEN = "127.0.0.1:0" if LOCAL else "0.0.0.0:0"
CHECKS = {
    "first": lambda: check(launch, "java-http-client", TARGET, listen=LISTEN),
    "signals": lambda: check_signals(launch, TARGET, listen=LISTEN),
    "setup": lambda: check_setup(launch, TARGET, listen=LISTEN),
    "rotation": lambda: check_rotation(launch, TARGET, listen=LISTEN, host=HOST),
    "matching": lambda: check_matching_settings(launch, TARGET, listen=LISTEN),
    "app": lambda: check_app_settings(launch, TARGET, listen=LISTEN),
    "truth-extension-processor": lambda: check_truth(launch, TARGET, "extension-processor", EXTENSION_PROCESSOR,
                                                     listen=LISTEN),
    "truth-extension-sampler": lambda: check_truth(launch, TARGET, "extension-sampler", EXTENSION_SAMPLER,
                                                   listen=LISTEN),
}

if __name__ == "__main__":
    sys.exit(main(CHECKS, build, options=("--local",)))
