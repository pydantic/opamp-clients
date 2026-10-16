# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The end-to-end check every SDK integration passes (examples/<integration>/try.py).

An instrumented application runs against the reference OpAMP server for these checks: built from
its sources where they are (a Cargo.toml at the root), else the binary OPAMP_EXAMPLES_SERVER names.
The SDK traces its HTTP requests. Two runs share one state directory.

Each offer is a remote config whose only file is a Logfire SDK policy, `logfire-sdk-policy.json`
(docs/sdk-settings.md): version 1 where only head_sampling and signal_exports are overridden, else
version 2. A control the step does not name is local, the SDK's own value.

Run 1, the policies, in the order the server offers them:
  1. head_sampling 0.5, signal_exports.traces enabled: APPLIED, spans arrive;
  2. head_sampling 0.5, signal_exports.traces off: APPLIED, and no span is exported after it;
  3. head_sampling 7: FAILED, naming head_sampling, and nothing changes;
  4. head_sampling 1, signal_exports.traces local: APPLIED; spans come again (a local control is
     the SDK's own value);
  5. instrumentations disabled <the HTTP instrumentation>: APPLIED. Where the switch takes
     effect at the next start (most SDKs), spans go on until then; where it takes effect at once
     (Go, Node.js, Rust), they stop although the application goes on making requests.
Run 2, the next start: the same instance UID (16 bytes), the effective config reported, and the
HTTP instrumentation off, so no spans although the application makes requests while the agent
is connected. Where the application instruments itself in code (switches=False), nothing can
switch an instrumentation off: the agent answers the fifth policy FAILED ("instrumentations:
disabled: this SDK cannot apply the requested override"), and the spans go on.
In both, the application's requests are traced and the agent's own OpAMP requests are not, and
both end with the agent saying goodbye (agent_disconnect).

check_signals runs two more, with a state directory of their own and metrics and logs exported
(the application writes a log line per request):
Run 3, the policies, in the order the server offers them:
  1. head_sampling 1: APPLIED; spans, metrics and log records arrive;
  2. signal_exports.metrics and signal_exports.logs off: APPLIED; no metrics or log records after
     it, while spans go on;
  3. head_sampling 0.25: APPLIED; metrics and log records come again (local, so the SDK's own
     value), and some of the application's requests are traced but not all; the effective config
     the agent reports is this version 1 policy, as the only file;
  4. head_sampling 0.25, log_levels warn: APPLIED; the application's INFO log lines stop (the sink
     counts records, not severities: about none per request, against about one before), and the
     effective config is now this version 2 policy;
  5. head_sampling 0 with debug until 10 s after the offer (the server fills the time in as it
     sends it): APPLIED; the application's requests are traced until then (the debug
     sampling_ratio left out, so 1), and none made after it, with no word from the server; the
     INFO log lines come again (log_levels local). Twice the gap before the next, for the window
     to end;
  6. head_sampling 1 with a sampling rule of rate 0 for client spans (every example's requests are
     its HTTP client's, a root span of kind client), and log_levels scopes switching off the
     application's loggers (app*: app and app.noisy; rust_otel_example*, Rust's target): APPLIED;
     none of the requests traced, and about no log record a request;
  7. head_sampling 1, attribute_redaction mask url.full and http.url: APPLIED; spans again (the
     rules local), each URL [REDACTED] in the sink (which names a span by its URL, else its name),
     and the log lines again (the scopes local);
  8. head_sampling 1, attribute_redaction drop URL.FULL and Http.Url (keys match ignoring case),
     trace_rate_limit 1, log_rate_limit 1: APPLIED; spans arrive, none named by a URL or
     [REDACTED]; about a span and a log record a second, of some five requests a second;
  9. polling_interval 3000 ms, instrumentations all_disabled: APPLIED.
  Throughout, export health: the agent's health carries component_health_map["exporters"], a
  child each for traces, metrics and logs, the application's own `healthy` true in every report;
  a report counts trace exports that succeeded, and log records dropped on purpose (by the level,
  the logs export off or the cap).
Run 4, the next start: the agent checks in every 3 s, not every second as its environment says,
and no spans although the application makes requests.

check_setup runs two more (and run 6's next start) against a server that requires a bearer token
(an enrollment key):
Run 5, OTEL_OPAMP_HEADERS with the wrong token: the server refuses every request, the application
runs and is traced all the same (with the SDK's own settings), and the agent's warning names
OTEL_OPAMP_HEADERS.
Run 6, the right token: the server accepts the agent, which reports its effective config. The
agent says it takes OpAMP connection settings and reports their status (it has a state
directory), so the server hands it a key of its own (docs/connection-settings.md), and it takes
it: the server confirms the key (`key_promoted`) once the agent says APPLIED while using it.
Then, against the same server, the next start with the same state directory and the enrollment
key still configured: the agent comes back on the key it was given, which it kept (the server no
longer takes the enrollment key for it: a request with it would be refused), and checks in every
second, as it is set up to (the key's offer carried the server's interval, which lasts until the
next start).
For an application that gives the token in code (key_in_code()), the same runs with the token
given in code and OTEL_OPAMP_HEADERS set to the other one, which the code wins over: run 5's
warning names the option given in code, not OTEL_OPAMP_HEADERS, and run 6 is accepted.

check_rotation runs one more (and its next start) against the same kind of server, whose operator
then rotates and moves the agent (docs/connection-settings.md, "Moves and rotations"): once the
agent's own key is confirmed, the operator gives it a second key at the same endpoint; once that
one is confirmed, a third at the server's second listener, the same server on another port. The
agent takes each (three keys confirmed) without a refused request, and once it has reached the
second listener it opens no connection to the first. The next start, with the same state
directory and the first endpoint still configured, goes straight to the second listener on the
key it was given there: no connection to the first, nothing refused, no new key.

check_truth runs one more, under a setup (EXAMPLE_SETUP) where a setting may not reach the
telemetry: the server offers TRUTH's documents, one setting each beside sampling_rate 1, and each
is either carried out (the sink shows it) or named as set aside in the effective config, never
APPLIED and neither. The settings set aside must be the ones the setup expects.

check_app_settings runs two more, for an integration that lets the application read its own
settings (docs/app-settings.md), the policy's app_settings control. Its example application reads
the key `example.feature` with the default `off` each round, and while it is `on` also requests
/feature; it asks to be told of changes, and requests /changed once for each change it is told of.
The sink sees both.
Run 7, the policies, in the order the server offers them:
  1. app_settings {example.feature: on}: APPLIED; /feature requests start (none before: the
     default);
  2. app_settings {example.feature: [on]}: FAILED, naming app_settings, and /feature goes on;
  3. head_sampling 1, as a version 1 policy (app_settings local): APPLIED; /feature stops (back to
     the default) while the application goes on making requests;
  4. app_settings {example.feature: on} again: APPLIED; /feature again;
  5. the same values again beside head_sampling 1: APPLIED, and no change to tell;
  so /changed three times in all: on, off, on.
Run 8, the next start with the same state directory and no offer: /feature from the start (the
saved settings), and no /changed.
"""
from __future__ import annotations

import concurrent.futures
import contextlib
import functools
import itertools
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from collections.abc import Callable
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SETTINGS = "application/json"
# After a setting is answered, an export already on its way may still arrive: on a loaded runner,
# seconds later.
MARGIN = 3.0
# A span's end (the application's clock) against the server's log (the same host's clock).
DRIFT = 1.0
# Requests the application makes (every 200 ms) for an absence of spans to mean something.
REQUESTS = 5

# Launch the application: (OpAMP port, telemetry port, state directory, log name, environment to add)
# -> a stop function.
Launch = Callable[[str, str, Path, str, dict[str, str]], Callable[[], None]]

# What check_signals adds to the application's environment: every signal exported, and often.
SIGNALS = {"OTEL_METRICS_EXPORTER": "otlp", "OTEL_LOGS_EXPORTER": "otlp", "OTEL_METRIC_EXPORT_INTERVAL": "500",
           "OTEL_BLRP_SCHEDULE_DELAY": "500"}
# The polling interval run 3 offers, against the 1 s the environment asks for.
POLLING = 3.0
# How far ahead of its offer run 3's debug until is, in seconds.
DEBUG = 10
# AcceptsOpAMPConnectionSettings and ReportsConnectionSettingsStatus: what an agent declares for the
# server to hand it a key of its own.
TAKES_KEYS = 0x100 | 0x8000


def websocket() -> bool:
    """Whether the applications talk to the server over a WebSocket (OPAMP_EXAMPLES_TRANSPORT
    `websocket`) or plain HTTP (`http`, the default)."""
    transport = os.environ.get("OPAMP_EXAMPLES_TRANSPORT", "http")
    if transport not in ("http", "websocket"):
        sys.exit(f"OPAMP_EXAMPLES_TRANSPORT={transport}: http or websocket")
    return transport == "websocket"


def opamp_url(port: str, host: str = "127.0.0.1") -> str:
    """The OpAMP endpoint an application is given for the server on `port`: plain HTTP, or a
    WebSocket (websocket()). The checks are the same over either, and judge_transport() holds
    every run, and every next start, to the one asked for."""
    return f"{'ws' if websocket() else 'http'}://{host}:{port}/v1/opamp"


def judge_transport(events: list[dict]) -> list[tuple[str, bool]]:
    """Every message the server took came over the transport asked for (opamp_url())."""
    want = "ws" if websocket() else "http"
    a2s = [e for e in events if e["event"] == "a2s"]
    came = sorted({e.get("transport") for e in a2s}, key=str)
    return [(f"every message came over {'a WebSocket' if want == 'ws' else 'plain HTTP'} ({len(a2s)}, {came})",
             bool(a2s) and came == [want])]


def target(name: str) -> Path:
    """A runner's own directory, for its builds, the server's logs and the state: two runners, at
    once or one after the other, share nothing. Under the checkout, not /tmp, as a Docker daemon
    in another container (docker-in-docker) can only mount what it shares with the runner."""
    path = ROOT / "target" / "integrations" / name
    # A file of that name is a build from before runners had directories of their own.
    if path.is_file():
        path.unlink()
    path.mkdir(parents=True, exist_ok=True)
    return path


def stop_group(app: subprocess.Popen, sig: int = signal.SIGTERM) -> None:
    """Stop an application started in a session of its own (start_new_session): `sig` to its
    process group, SIGKILL if it hasn't exited 20 s later. One already gone is fine."""
    with contextlib.suppress(ProcessLookupError):
        os.killpg(app.pid, sig)
    try:
        app.wait(timeout=20)
    except subprocess.TimeoutExpired:
        with contextlib.suppress(ProcessLookupError):
            os.killpg(app.pid, signal.SIGKILL)
        app.wait()


def stop_container(app: subprocess.Popen, container: str) -> None:
    """Stop an application run by `docker run --rm --name <container>` (`app`): 20 s to exit, then
    the container is removed whatever it is doing."""
    quiet = {"check": False, "stdout": subprocess.DEVNULL, "stderr": subprocess.DEVNULL}
    subprocess.run(["docker", "stop", "-t", "20", container], **quiet)
    try:
        app.wait(timeout=30)
    except subprocess.TimeoutExpired:
        app.kill()
        app.wait()
    subprocess.run(["docker", "rm", "-f", container], **quiet)


#: The remote config's only file: the Logfire SDK policy (docs/sdk-settings.md).
POLICY_FILE = "logfire-sdk-policy.json"
POLICY_V1 = "logfire-sdk-policy/v1"
POLICY_V2 = "logfire-sdk-policy/v2-draft"
_SIGNALS = ("traces", "metrics", "logs")
_V2_CONTROLS = ("sampling_rules", "trace_rate_limit", "log_levels", "log_rate_limit", "attribute_redaction", "debug",
                "app_settings", "instrumentations", "polling_interval")


def policy(overrides: dict[str, dict], version: int | None = None) -> str:
    """A Logfire SDK policy, as its JSON text. `overrides` maps each control the policy overrides (a
    signal's export as `signal_exports.logs`) to the fields of its override. Every other control is
    local. The policy is version 1 when it overrides only version 1 controls, unless `version` says
    2."""
    v1 = all(c == "head_sampling" or c.startswith("signal_exports.") for c in overrides)
    version = version or (1 if v1 else 2)
    unknown = set(overrides) - {"head_sampling", *(f"signal_exports.{s}" for s in _SIGNALS), *_V2_CONTROLS}
    if unknown or (version == 1 and not v1):
        raise ValueError(f"no control of policy version {version}: {sorted(unknown) or sorted(overrides)}")

    def control(name: str) -> dict:
        fields = overrides.get(name)
        return {"mode": "local"} if fields is None else {"mode": "override", **fields}

    doc: dict = {"schema": POLICY_V1 if version == 1 else POLICY_V2, "head_sampling": control("head_sampling"),
                 "signal_exports": {s: control(f"signal_exports.{s}") for s in _SIGNALS}}
    if version == 2:
        doc.update({c: control(c) for c in _V2_CONTROLS})
    return json.dumps(doc, separators=(",", ":"))


def offer(n: int, overrides: dict[str, dict], version: int | None = None) -> dict:
    """A scenario's offer, on message `n`, of a remote config whose only file is the policy with
    `overrides` (policy())."""
    return {"on_message": n, "action": "offer_config", "config_key": POLICY_FILE,
            "body": policy(overrides, version), "content_type": SETTINGS}


def policy_in_force(effective: dict) -> dict:
    """The policy an effective config reports, as the server's log gives it (effective_configs())."""
    return effective.get(POLICY_FILE) or {}


def overridden(effective: dict, control: str) -> dict | None:
    """The fields of the override of `control` (a signal's export as `signal_exports.logs`) in the
    policy an effective config reports, without `mode`; None when the control is local."""
    doc = policy_in_force(effective)
    found = doc.get("signal_exports", {}).get(control.split(".")[1]) if control.startswith("signal_exports.") \
        else doc.get(control)
    if not isinstance(found, dict) or found.get("mode") != "override":
        return None
    return {k: v for k, v in found.items() if k != "mode"}


def scenario(instrumentation: str) -> dict:
    return {"server": {"faults": [
        offer(1, {"head_sampling": {"ratio": 0.5}, "signal_exports.traces": {"enabled": True}}),
        offer(10, {"head_sampling": {"ratio": 0.5}, "signal_exports.traces": {"enabled": False}}),
        offer(16, {"head_sampling": {"ratio": 7}}),
        offer(22, {"head_sampling": {"ratio": 1}}),
        # Well after the fourth: a fast-polling client (the Java agent, two messages a second)
        # still leaves a window to see spans come back in.
        offer(40, {"instrumentations": {"disabled": instrumentation.split(",")}}),
    ]}}


def signals_scenario(gap: int = 25) -> dict:
    """Run 3's offers, `gap` messages apart: at one a second, time enough for a share of requests."""
    return {"server": {"faults": [
        offer(1, {"head_sampling": {"ratio": 1}}),
        offer(1 + gap, {"signal_exports.metrics": {"enabled": False}, "signal_exports.logs": {"enabled": False}}),
        offer(1 + 2 * gap, {"head_sampling": {"ratio": 0.25}}),
        offer(1 + 3 * gap, {"head_sampling": {"ratio": 0.25}, "log_levels": {"level": "warn"}}),
        # The server fills the time in as it sends the offer.
        offer(1 + 4 * gap, {"head_sampling": {"ratio": 0}, "debug": {"until": f"{{now+{DEBUG}s}}"}}),
        offer(1 + 6 * gap, {"head_sampling": {"ratio": 1}, "sampling_rules": {"rules": [{"rate": 0, "kind": "client"}]},
                            "log_levels": {"scopes": {"app*": "off", "rust_otel_example*": "off"}}}),
        offer(1 + 7 * gap, {"head_sampling": {"ratio": 1}, "attribute_redaction": {"mask": ["url.full", "http.url"]}}),
        offer(1 + 8 * gap, {"head_sampling": {"ratio": 1}, "attribute_redaction": {"drop": ["URL.FULL", "Http.Url"]},
                            "trace_rate_limit": {"traces_per_second": 1}, "log_rate_limit": {"records_per_second": 1}}),
        offer(1 + 9 * gap, {"polling_interval": {"milliseconds": int(POLLING * 1000)},
                            "instrumentations": {"all_disabled": True}}),
    ]}}


#: The key of the application's own settings that the example applications read
#: (check_app_settings), and the paths they request.
APP_KEY = "example.feature"


def app_settings_scenario(gap: int = 10) -> dict:
    """Run 7's offers, `gap` messages apart."""
    # The first a few messages in: the application runs a while on its defaults first.
    return {"server": {"faults": [
        offer(4, {"app_settings": {"values": {APP_KEY: "on"}}}),
        offer(4 + gap, {"app_settings": {"values": {APP_KEY: ["on"]}}}),
        offer(4 + 2 * gap, {"head_sampling": {"ratio": 1}}),
        offer(4 + 3 * gap, {"app_settings": {"values": {APP_KEY: "on"}}}),
        offer(4 + 4 * gap, {"head_sampling": {"ratio": 1}, "app_settings": {"values": {APP_KEY: "on"}}}),
    ]}}


@functools.cache
def built_server() -> Path:
    """The reference server: OPAMP_EXAMPLES_SERVER if set, else built from its sources, once a
    process, where they are (a Cargo.toml at the root)."""
    given = os.environ.get("OPAMP_EXAMPLES_SERVER")
    if given:
        if not Path(given).is_file():
            sys.exit(f"OPAMP_EXAMPLES_SERVER={given}: no such file")
        return Path(given).resolve()
    if not (ROOT / "Cargo.toml").is_file():
        sys.exit("the integration checks need their reference OpAMP server, whose sources aren't here: "
                 "set OPAMP_EXAMPLES_SERVER to the path of its binary")
    subprocess.run(["cargo", "build", "--release", "--locked", "-p", "opamp-agents", "--example", "sdk-integrations"],
                   cwd=ROOT, check=True)
    cargo_target = Path(os.environ.get("CARGO_TARGET_DIR", ROOT / "target"))
    if not cargo_target.is_absolute():
        cargo_target = ROOT / cargo_target
    return cargo_target / "release" / "examples" / "sdk-integrations"


def server_binary(target_: Path) -> Path:
    """The server, built if this process hasn't yet, in `target_` (run() starts it from there)."""
    target_.mkdir(parents=True, exist_ok=True)
    binary = target_ / "server"
    # Copied aside, then renamed over it: a check running at once (main()'s --jobs) may be running
    # the server there, and Linux refuses to write a file being run (ETXTBSY). The rename leaves
    # that one running the file it started.
    aside = target_ / f".server-{os.getpid()}"
    shutil.copy2(built_server(), aside)
    os.replace(aside, binary)
    return binary


def read_log(log: Path) -> list[dict]:
    """The server's log so far; a line still being written is left for the next read."""
    events = []
    for line in log.read_text().splitlines() if log.exists() else []:
        try:
            event = json.loads(line)
            if "ends_wall" in event:
                event["ends"] = [end - event["wall"] + event["t"] for end in event["ends_wall"]]
            events.append(event)
        except json.JSONDecodeError:
            break
    return events


def wait_for(holds: Callable[[], bool], seconds: float, settle: float,
             sleep: Callable[[float], None] = time.sleep, clock: Callable[[], float] = time.monotonic) -> bool:
    """Wait until `holds`, at most `seconds`; once it holds, `settle` seconds more, in full however
    late it came. True when it held."""
    deadline = clock() + seconds
    while not holds():
        if clock() >= deadline:
            return False
        sleep(1)
    sleep(settle)
    return True


@contextlib.contextmanager
def serving(scenario_: dict, name: str, listen: str, target_: Path):
    """The server, with `scenario_`, for the block: (OpAMP port, telemetry port, its log). It is
    stopped whatever happens."""
    path = target_ / f"{name}-scenario.json"
    path.write_text(json.dumps(scenario_))
    log = target_ / f"{name}.jsonl"
    server = subprocess.Popen([str(server_binary(target_)), "-listen", listen, "-scenario", str(path), "-log", str(log)],
                              stdout=subprocess.PIPE, text=True)
    try:
        listening = json.loads(server.stdout.readline())
        assert listening["implementation"] == "rust"
        yield listening["addr"].rsplit(":", 1)[1], listening["telemetry_addr"].rsplit(":", 1)[1], log
    finally:
        server.terminate()
        server.wait()


def start(launch: Launch, served: tuple[str, str, Path], state: Path, name: str, seconds: float,
          until: Callable[[list[dict]], bool] | None, settle: float, env: dict[str, str], skip: int = 0) -> None:
    """Start the application against the server (`served`, from serving()) and run it for
    `seconds`, or, with `until`, until it holds of the server's log past its first `skip` events
    (at most `seconds`), then `settle` seconds more to see what follows: a slow runner gets the
    time it needs, a fast one isn't kept waiting. The application is stopped whatever happens."""
    port, telemetry_port, log = served
    stop = launch(port, telemetry_port, state, name, env)
    try:
        if until is None:
            time.sleep(seconds)
        else:
            wait_for(lambda: until(read_log(log)[skip:]), seconds, settle)
    finally:
        stop()
    time.sleep(0.5)  # the last export and the goodbye, on their way


def run(launch: Launch, scenario_: dict, state: Path, name: str, seconds: float, listen: str, target_: Path,
        until: Callable[[list[dict]], bool] | None = None, settle: float = 0,
        env: dict[str, str] | None = None) -> list[dict]:
    """Run the application against the server for `seconds`, or, with `until`, until it holds
    (at most `seconds`), then `settle` seconds more (start()). The application and the server are
    stopped whatever happens. `env` is added to the application's environment."""
    with serving(scenario_, name, listen, target_) as served:
        start(launch, served, state, name, seconds, until, settle, env or {})
    return read_log(served[2])


def run_and_restart(launch: Launch, scenario_: dict, state: Path, name: str, seconds: float, listen: str,
                    target_: Path, until: Callable[[list[dict]], bool], again: Callable[[list[dict]], bool],
                    settle: float = 0, env: dict[str, str] | None = None) -> tuple[list[dict], list[dict]]:
    """run(), then the application's next start against the same server (which keeps what it
    knows of the agent, its keys included), with the same state directory, until `again` holds of
    what follows: the server's log of each start."""
    env = env or {}
    with serving(scenario_, name, listen, target_) as served:
        start(launch, served, state, name, seconds, until, settle, env)
        first = len(read_log(served[2]))
        start(launch, served, state, f"{name}-again", seconds, again, settle, env, skip=first)
    events = read_log(served[2])
    return events[:first], events[first:]


def statuses(events: list[dict]) -> list[tuple[float, str, str]]:
    out: list[tuple[float, str, str | None, str]] = []
    for e in events:
        rcs = e.get("remote_config_status")
        if e["event"] == "a2s" and rcs:
            status = rcs["status"].rsplit("_", 1)[-1]
            if status != "APPLYING" and (not out or out[-1][1:3] != (status, rcs.get("hash"))):
                out.append((e["t"], status, rcs.get("hash"), rcs.get("error", "")))
    return [(t, s, err) for t, s, _, err in out]


def spans(events: list[dict], after: float = -1, before: float = 1e9) -> list[dict]:
    return [e for e in events if e["event"] == "otlp" and e["signal"] == "traces" and after < e["t"] < before]


def ended(events: list[dict], after: float, arrived_before: float = 1e9) -> list[float]:
    """When the spans that ended after `after` and arrived before `arrived_before` ended. A switch
    that works stops every span that ends after it, until the setting that switches it back is sent:
    a span that ended before the switch may still be on its way (a batch, a retry, a busy machine),
    and one still queued goes out once it's switched back. A log without end times counts arrivals."""
    out = []
    for e in spans(events, before=arrived_before):
        names = e.get("spans") or []
        ends = e.get("ends") or [e["t"]] * len(names)
        out += [t for t in ends if after < t]
    return out


def signal(events: list[dict], name: str, after: float = -1, before: float = 1e9) -> list[dict]:
    """Exports of metrics or logs that arrived between `after` and `before`, with something in them."""
    return [e for e in events if e["event"] == "otlp" and e["signal"] == name and after < e["t"] < before
            and e.get("count", 1)]


def per_request(events: list[dict], after: float, before: float) -> tuple[float, int]:
    """Log records that arrived between `after` and `before`, per application request made then,
    and how many requests that is. The sink counts records, not their severities: the application
    writes one INFO line per request, so about 1 (or more) with every severity exported, about 0
    with a `warn` floor."""
    made = len(pings(events, after, before))
    records = sum(e.get("count", 1) for e in signal(events, "logs", after, before))
    return (records / made if made else 0.0), made


def ending(events: list[dict], after: float, before: float, arrived_before: float = 1e9) -> list[str]:
    """The names the sink gave the spans that ended between `after` and `before` and arrived before
    `arrived_before`: a span's URL (url.full or http.url) when it has one, else its name."""
    return [name for e in spans(events, before=arrived_before)
            for name, end in zip(e.get("spans") or [], e.get("ends") or []) if after < end < before]


def records(events: list[dict], after: float, before: float) -> int:
    """Log records that arrived between `after` and `before`."""
    return sum(e.get("count", 1) for e in signal(events, "logs", after, before))


def debug_end(events: list[dict], applied: float) -> float:
    """When run 3's debug window ends, in the log's time: DEBUG seconds after the server filled the
    offer in (it rounds the time up to a whole second, so up to a second later). Without the
    server's word, after the offer."""
    filled = [e["t"] for e in events if e["event"] == "offer_filled" and e["t"] < applied]
    return (filled[-1] if filled else offered_before(events, applied)) + DEBUG


def exporters(events: list[dict]) -> list[tuple[bool, dict]]:
    """The export health each health report carried: (the application's own healthy, the
    `exporters` entry's children by signal, each {status, healthy, attributes})."""
    out = []
    for e in events:
        health = e.get("health") if e["event"] == "a2s" else None
        if health:
            entry = (health.get("components") or {}).get("exporters") or {}
            out.append((bool(health.get("healthy")), entry.get("components") or {}))
    return out


def counted(child: dict, name: str) -> int:
    """An export health counter (`exports.ok`, `items.filtered`), 0 when absent."""
    try:
        return int(float((child.get("attributes") or {}).get(name) or 0))
    except (TypeError, ValueError):
        return 0


def traced(events: list[dict], after: float, before: float) -> int:
    """How many of the application's requests made between `after` and `before` have a span: the
    spans that ended then, whenever they arrived."""
    return sum(after < end < before and "/ping" in name
               for e in spans(events) for name, end in zip(e.get("spans") or [], e.get("ends") or []))


def effective_configs(events: list[dict], after: float = -1, before: float = 1e9) -> list[dict]:
    """The effective configs the agent reported between `after` and `before`: each its JSON files
    by name, as the server's log gives them. The only file is the policy in force (POLICY_FILE)."""
    return [e["effective"] for e in events if e["event"] == "a2s" and "effective" in e and after < e["t"] < before]


def judge_effective(three: list[dict], t3: float, t4: float, end: float) -> list[tuple[str, bool]]:
    """Steps 3 and 4: the effective config reports the policy in force, as the only file. Step 3's
    is version 1, with head_sampling 0.25 and the signals local (APPLIED at t3, until the next,
    APPLIED at t4). Step 4's is version 2, with the log level beside it. `end`: when the offer
    after that was sent."""
    during = effective_configs(three, offered_before(three, t3), offered_before(three, t4))
    after = effective_configs(three, t4 - DRIFT, end)

    def three_in_force(e: dict) -> bool:
        return (list(e) == [POLICY_FILE] and policy_in_force(e).get("schema") == POLICY_V1
                and overridden(e, "head_sampling") == {"ratio": 0.25}
                and all(overridden(e, f"signal_exports.{s}") is None for s in _SIGNALS))

    def four_in_force(e: dict) -> bool:
        return (list(e) == [POLICY_FILE] and policy_in_force(e).get("schema") == POLICY_V2
                and overridden(e, "head_sampling") == {"ratio": 0.25} and overridden(e, "log_levels") == {"level": "warn"})

    return [
        (f"the effective config reports the version 1 policy in force, head_sampling 0.25 and the signals local "
         f"({during[-1] if during else 'none reported'})", any(three_in_force(e) for e in during)),
        (f"then the version 2 policy, with the log level warn beside it ({after[-1] if after else 'none reported'})",
         bool(after) and four_in_force(after[-1])),
    ]


def offered_before(events: list[dict], t: float) -> float:
    """When the server last sent a remote config before `t`."""
    return max((e["t"] for e in events if e["event"] == "s2a" and e.get("offer_config") and e["t"] < t), default=t)


def pings(events: list[dict], after: float = -1, before: float = 1e9) -> list[dict]:
    """The application's requests (to /ping, which the server doesn't serve)."""
    return [e for e in events if e["event"] == "stray_request" and e.get("path") == "/ping" and after < e["t"] < before]


def requests(events: list[dict], path: str, after: float = -1, before: float = 1e9) -> list[dict]:
    """The application's requests to `path` (which the server doesn't serve)."""
    return [e for e in events if e["event"] == "stray_request" and e.get("path") == path and after < e["t"] < before]


def connected(events: list[dict]) -> bool:
    """Run 2 has seen enough: the agent is connected and the application has made its requests."""
    a2s = [e for e in events if e["event"] == "a2s"]
    return bool(a2s) and len(pings(events, a2s[0]["t"])) > REQUESTS


def judge_first(one: list[dict], service: str, switch_at_once: bool, switches: bool = True) -> list[tuple[str, bool]]:
    """Run 1's checks: (what, whether it holds). switches: the integration carries the
    instrumentation switch out; without, it refuses the fifth policy, and the spans go on."""
    out = []
    st = statuses(one)
    last = "APPLIED" if switches else "FAILED"
    out.append((f"five configs answered APPLIED, APPLIED, FAILED, APPLIED, {last}",
                [s for _, s, _ in st] == ["APPLIED", "APPLIED", "FAILED", "APPLIED", last]))
    if len(st) == 5:
        (t1, _, _), (t2, _, _), (_, _, err), (t4, _, _), (t5, _, err5) = st
        out.append((f"the FAILED names the control ({err!r})", err.startswith("head_sampling:")))
        if not switches:
            refused = "instrumentations: disabled: this SDK cannot apply the requested override"
            out.append((f"the switch it cannot carry out is refused ({err5!r})", err5 == refused))
        out.append(("spans arrive, with service.name", any(s["services"] == [service] for s in spans(one, t1, t2))))
        off = pings(one, t2 + MARGIN, t4)
        out.append((f"no span exported once the traces export is off ({len(off)} requests)",
                    len(off) > REQUESTS and not ended(one, t2 + DRIFT, offered_before(one, t4))))
        out.append(("spans again once the traces export is local", bool(spans(one, t4 + MARGIN, t5))))
        if not switches:
            out.append(("spans go on after the refused switch", bool(spans(one, t5 + MARGIN))))
        elif switch_at_once:
            # No spans means something only if there were requests to trace.
            after = pings(one, t5 + MARGIN)
            out.append((f"the application kept making requests after the switch ({len(after)})",
                        len(after) > REQUESTS))
            out.append(("the instrumentation switched off at once: no spans",
                        len(after) > REQUESTS and not ended(one, t5 + DRIFT)))
        else:
            out.append(("the instrumentation stays on until the next start", bool(spans(one, t5 + MARGIN))))
    traced = [u for e in spans(one) for u in e.get("spans") or []]
    out.append(("the application's requests are traced", any("/ping" in u for u in traced)))
    out.append(("the agent's own OpAMP requests are not", not any("/v1/opamp" in u for u in traced)))
    a2s = [e for e in one if e["event"] == "a2s"]
    out.append(("the effective config is reported", any("effective_config" in e["fields"] for e in a2s)))
    out.append(("goodbye (agent_disconnect) at the end", bool(a2s) and a2s[-1]["disconnect"]))
    return out


def inventory(events: list[dict]) -> dict:
    """The last inventory an agent sent in full: its available components as the log gives them
    (docs/available-components.md); {} if it sent none."""
    full = [e["components"] for e in events if e["event"] == "a2s" and e.get("components")]
    return full[-1] if full else {}


def judge_inventory(one: list[dict], instrumentation: str, switch_at_once: bool,
                    switches: bool = True, versioned: bool = True) -> list[tuple[str, bool]]:
    """The inventory's checks, on run 1: (what, whether it holds). `instrumentation`: what run 1
    switches off, every one of its names listed (unless the integration can't switch any:
    switches=False); switch_at_once and versioned as check()'s."""
    inv = inventory(one)
    sub = lambda part: inv.get(part, {}).get("components", {})  # noqa: E731
    sdk = inv.get("sdk", {}).get("metadata", {})
    keys = ("integration.name", "telemetry.sdk.language", "telemetry.sdk.version")
    given = lambda k: isinstance(sdk.get(k), str) and bool(sdk[k])  # noqa: E731
    out = [("the inventory is sent in full", bool(inv)),
           (f"it names our package and the OpenTelemetry SDK, with versions ({sdk})",
            all(given(k) for k in keys) and (given("integration.version") if versioned
                                             else "integration.version" not in sdk))]
    traces = sub("exporters").get("traces", {}).get("components", {})
    out.append((f"a traces exporter the traces export control reaches ({sorted(traces)})",
                any(e.get("metadata", {}).get("gated") is True for e in traces.values())))
    settings = {k: v.get("metadata", {}).get("applies") for k, v in sub("settings").items()}
    # The inventory names the settings the controls set: head_sampling sets sampling_rate,
    # signal_exports.traces send_traces and instrumentations deactivate_instrumentations.
    out.append(("sampling_rate and send_traces apply at once",
                settings.get("sampling_rate") == settings.get("send_traces") == "at once"))
    if switches:
        names = instrumentation.split(",")
        listed = sub("instrumentations")
        out.append((f"the instrumentations run 1 switches off are listed ({names} in {len(listed)})",
                    all(n in listed for n in names)))
        when = "at once" if switch_at_once else "next start"
        out.append((f"deactivate_instrumentations applies {when}", settings.get("deactivate_instrumentations") == when))
    return out


def judge_second(one: list[dict], two: list[dict], switches: bool = True) -> list[tuple[str, bool]]:
    """Run 2's checks, against run 1's log: (what, whether it holds). switches: the integration
    carries the instrumentation switch out; without, it refused the switch and the spans go on."""
    a2s = [e for e in one if e["event"] == "a2s"]
    a2s2 = [e for e in two if e["event"] == "a2s"]
    uid = a2s2[0]["uid"] if a2s2 else ""
    # The requests made while the agent was connected: no spans for these is the check.
    requests = pings(two, a2s2[0]["t"]) if a2s2 else []
    return [
        (f"the same instance UID after the restart, 16 bytes ({uid!r})",
         bool(re.fullmatch(r"[0-9a-f]{32}", uid)) and bool(a2s) and uid == a2s[0]["uid"]),
        ("the effective config is reported", any("effective_config" in e["fields"] for e in a2s2)),
        (f"the application made requests while the agent was connected ({len(requests)})",
         len(requests) > REQUESTS),
        ("the instrumentation switched off at the next start: no spans",
         len(requests) > REQUESTS and not spans(two)) if switches else
        ("the instrumentation switch was refused (the application instruments itself): spans go on",
         bool(spans(two, a2s2[0]["t"] if a2s2 else -1))),
        ("goodbye (agent_disconnect) at the end", bool(a2s2) and a2s2[-1]["disconnect"]),
    ]


def judge_signals(three: list[dict], service: str, switch_at_once: bool) -> list[tuple[str, bool]]:
    """Run 3's checks: (what, whether it holds)."""
    out = []
    st = statuses(three)
    out.append(("nine configs answered APPLIED", [s for _, s, _ in st] == ["APPLIED"] * 9))
    if len(st) != 9:
        return out
    t1, t2, t3, t4, t5, t6, t7, t8, t9 = (t for t, _, _ in st)
    for name in ("metrics", "logs"):
        out.append((f"{name} arrive", any(e.get("services") == [service] for e in signal(three, name, t1, t2))))
        # An export already on its way may arrive; one still queued goes out once switched back.
        late = signal(three, name, t2 + MARGIN, offered_before(three, t3))
        out.append((f"no {name} exported once the {name} export is off ({len(late)} exports)", not late))
        out.append((f"{name} again once the {name} export is local", bool(signal(three, name, t3 + MARGIN, t4))))
    out.append(("spans go on while metrics and logs are off", bool(spans(three, t2 + MARGIN, t3))))
    made = len(pings(three, t3 + DRIFT, t4 - DRIFT))
    share = traced(three, t3 + DRIFT, t4 - DRIFT) / made if made else 0
    # 0.25 of some 100 requests: 0.05 and 0.6 are each more than four standard deviations away.
    out.append((f"head_sampling 0.25 traces some requests but not all ({share:.2f} of {made})",
                made > 4 * REQUESTS and 0.05 < share < 0.6))
    # log_levels warn: the application's INFO lines (one a request) stop, and come again once the
    # control is local. Records queued before the floor may still arrive within MARGIN.
    before, made_before = per_request(three, t3 + MARGIN, t4)
    under, made_under = per_request(three, t4 + MARGIN, offered_before(three, t5))
    again, made_again = per_request(three, t5 + MARGIN, t6)
    out.append((f"INFO log lines arrive before the log level is set ({before:.2f} records a request, "
                f"{made_before} requests)", made_before > REQUESTS and before >= 0.5))
    out.append((f"no INFO log lines under the log level warn ({under:.2f} records a request, "
                f"{made_under} requests)", made_under > REQUESTS and under <= 0.1))
    out.append((f"INFO log lines again once log_levels is local ({again:.2f} records a request, "
                f"{made_again} requests)", made_again > REQUESTS and again >= 0.5))
    out += judge_effective(three, t3, t4, offered_before(three, t5))
    out += judge_debug(three, t5, offered_before(three, t6))
    out += judge_telemetry_settings(three, t6, t7, t8, offered_before(three, t9))
    if switch_at_once:
        after = pings(three, t9 + MARGIN)
        out.append((f"every instrumentation switched off at once: no spans ({len(after)} requests)",
                    len(after) > REQUESTS and not ended(three, t9 + DRIFT)))
    out += judge_health(three)
    a2s = [e for e in three if e["event"] == "a2s"]
    out.append(("goodbye (agent_disconnect) at the end", bool(a2s) and a2s[-1]["disconnect"]))
    return out


def judge_debug(three: list[dict], applied: float, next_offer: float) -> list[tuple[str, bool]]:
    """Step 5: head_sampling 0 with debug until DEBUG seconds ahead, APPLIED at `applied`; the next
    offer sent at `next_offer`. Spans end within the window, none after it (a span may end up to a
    second past the time given, which is rounded up), and the server sends nothing in between."""
    until = debug_end(three, applied)
    during = ending(three, applied + DRIFT, until - DRIFT)
    after = pings(three, until + 1 + MARGIN, next_offer)
    late = ending(three, until + 1 + DRIFT, next_offer)
    quiet = not [e for e in three if e["event"] == "s2a" and e.get("offer_config") and applied < e["t"] < next_offer]
    return [
        (f"debug: traced in the window at head_sampling 0 ({len(during)} spans)", bool(during)),
        (f"debug: no span once the window is over ({len(late)} spans, {len(after)} requests after it), "
         f"with no word from the server", len(after) > REQUESTS and not late and quiet),
    ]


def judge_telemetry_settings(three: list[dict], t6: float, t7: float, t8: float,
                             end: float) -> list[tuple[str, bool]]:
    """Steps 6 to 8: the sampling rule and the loggers off (APPLIED at t6), the mask (t7), and the
    drop with both caps
    (t8), until the next offer, sent at `end`. Redaction is at export: a span counts for a step
    when it ended after the step was APPLIED and arrived before the next was offered."""
    out = []
    # The rule: rate 0 for every client span, at head_sampling 1. Spans that ended before t6 were
    # started at rate 0 (step 5's, the window over), so nothing arrives late either.
    # The requests' spans are the ones the sink names by a URL.
    made = len(pings(three, t6 + MARGIN, offered_before(three, t7)))
    ruled = [n for n in ending(three, t6 + DRIFT, offered_before(three, t7) - DRIFT) if "://" in n]
    out.append((f"sampling_rules: a rule of rate 0 for client spans, at head_sampling 1: no request traced "
                f"({len(ruled)} spans, {made} requests)", made > REQUESTS and not ruled))
    # The levels per scope: each example logs a line or two a request on its own loggers only.
    off, made_off = per_request(three, t6 + MARGIN, offered_before(three, t7))
    back, made_back = per_request(three, t7 + MARGIN, offered_before(three, t8))
    out.append((f"log_levels scopes: no log lines with the application's loggers off ({off:.2f} records "
                f"a request, {made_off} requests)", made_off > REQUESTS and off <= 0.1))
    out.append((f"log_levels scopes: log lines again once local ({back:.2f} records a request, "
                f"{made_back} requests)", made_back > REQUESTS and back >= 0.5))
    masked = ending(three, t7 + DRIFT, 1e9, arrived_before=offered_before(three, t8))
    shown = [n for n in masked if "://" in n]
    out.append((f"attribute_redaction mask: spans again, each URL [REDACTED] ({masked.count('[REDACTED]')} of "
                f"{len(masked)} spans, {shown[:2]})", "[REDACTED]" in masked and not shown))
    dropped = ending(three, t8 + DRIFT, 1e9, arrived_before=end)
    named = [n for n in dropped if "://" in n or n == "[REDACTED]"]
    out.append((f"attribute_redaction drop: spans arrive, none with a URL ({len(dropped)} spans, {named[:3]})",
                bool(dropped) and not named))
    # The caps: a token bucket of one a second, full to start. Five requests a second (and a log
    # line or two each) without them.
    window = end - DRIFT - (t8 + MARGIN)
    made = len(pings(three, t8 + MARGIN, end - DRIFT))
    capped = len(ending(three, t8 + MARGIN, end - DRIFT))
    logged = records(three, t8 + MARGIN, end)
    out.append((f"trace_rate_limit 1: {capped} spans in {window:.1f} s of {made} requests",
                window > 3 and made > 2 * window and 0 < capped <= window + 3))
    out.append((f"log_rate_limit 1: {logged} log records in {window:.1f} s of {made} requests",
                window > 3 and made > 2 * window and 0 < logged <= window + 3))
    return out


def judge_health(three: list[dict]) -> list[tuple[str, bool]]:
    """Export health over run 3: every report keeps the application's own healthy, one carries the
    exporters entry with a child per signal, and the counters move (sent at most a minute apart
    while only they do, so a run of minutes has a report after the first exports)."""
    reports = exporters(three)
    signals = ("traces", "metrics", "logs")
    shaped = [c for _, c in reports if all(s in c for s in signals)]
    ok = max((counted(c["traces"], "exports.ok") for c in shaped), default=0)
    filtered = max((counted(c["logs"], "items.filtered") for c in shaped), default=0)
    return [
        (f"export health: an exporters entry with traces, metrics and logs ({len(shaped)} of {len(reports)} "
         f"reports), the application's own healthy in each", bool(shaped) and all(h for h, _ in reports)),
        (f"export health counts the trace exports that succeeded (exports.ok up to {ok}) and the log "
         f"records dropped on purpose (items.filtered up to {filtered})", ok > 0 and filtered > 0),
    ]


def judge_signals_restart(four: list[dict]) -> list[tuple[str, bool]]:
    """Run 4's checks: (what, whether it holds)."""
    a2s = [e for e in four if e["event"] == "a2s" and not e["disconnect"]]
    gaps = sorted(b["t"] - a["t"] for a, b in itertools.pairwise(a2s))
    median = gaps[len(gaps) // 2] if gaps else 0
    requests = pings(four, a2s[0]["t"]) if a2s else []
    return [
        (f"polling_interval in force at the next start: every {POLLING:g} s ({len(gaps)} gaps, median "
         f"{median:.1f} s)", len(gaps) >= 3 and POLLING - 0.5 <= median <= POLLING + 1.5),
        (f"the application made requests while the agent was connected ({len(requests)})",
         len(requests) > REQUESTS),
        ("every instrumentation switched off at the next start: no spans",
         len(requests) > REQUESTS and not spans(four)),
    ]


def judge_app_settings(seven: list[dict]) -> list[tuple[str, bool]]:
    """Run 7's checks: (what, whether it holds)."""
    out = []
    st = statuses(seven)
    out.append(("five configs answered APPLIED, FAILED, APPLIED, APPLIED, APPLIED",
                [s for _, s, _ in st] == ["APPLIED", "FAILED", "APPLIED", "APPLIED", "APPLIED"]))
    if len(st) != 5:
        return out
    (t1, _, _), (t2, _, err), (t3, _, _), (t4, _, _), _ = st
    a2s = [e for e in seven if e["event"] == "a2s"]
    before = pings(seven, -1, offered_before(seven, t1))
    early = requests(seven, "/feature", -1, offered_before(seven, t1))
    out.append((f"the default before any config: no /feature ({len(before)} requests, {len(early)} /feature)",
                len(before) > 1 and not early))
    on = requests(seven, "/feature", t1 + DRIFT, offered_before(seven, t2))
    out.append((f"app_settings {APP_KEY} on: the application reads it and requests /feature ({len(on)})", len(on) > 1))
    out.append((f"the FAILED names the control ({err!r})", err.startswith("app_settings: values")))
    kept = requests(seven, "/feature", t2 + DRIFT, offered_before(seven, t3))
    out.append((f"a refused config changes nothing: /feature goes on ({len(kept)})", len(kept) > 1))
    made = pings(seven, t3 + DRIFT, offered_before(seven, t4))
    off = requests(seven, "/feature", t3 + DRIFT, offered_before(seven, t4))
    out.append((f"app_settings local: back to the default, no /feature ({len(off)}) while the application goes on "
                f"({len(made)} requests)", len(made) > REQUESTS and not off))
    again = requests(seven, "/feature", t4 + DRIFT)
    out.append((f"app_settings {APP_KEY} on again: /feature again ({len(again)})", len(again) > 1))
    changed = requests(seven, "/changed")
    out.append((f"told of each change once: on, off, on, and not of a refused config or the same map again "
                f"({len(changed)} /changed)", len(changed) == 3))
    out.append(("goodbye (agent_disconnect) at the end", bool(a2s) and a2s[-1]["disconnect"]))
    return out


def judge_app_settings_restart(eight: list[dict]) -> list[tuple[str, bool]]:
    """Run 8's checks: (what, whether it holds)."""
    a2s = [e for e in eight if e["event"] == "a2s"]
    made = pings(eight, a2s[0]["t"]) if a2s else []
    feature = requests(eight, "/feature")
    return [
        (f"the saved settings at the next start: /feature with no offer ({len(feature)} of {len(made)} rounds)",
         len(made) > REQUESTS and len(feature) > REQUESTS),
        (f"nothing changed to tell ({len(requests(eight, '/changed'))} /changed)", not requests(eight, "/changed")),
    ]


def rejected(events: list[dict]) -> list[dict]:
    """The requests the server refused for their credentials."""
    return [e for e in events if e["event"] == "request_rejected" and e.get("status") == 401]


HEADERS = "OTEL_OPAMP_HEADERS"


def judge_wrong_key(five: list[dict], stderr: str, named: str = HEADERS) -> list[tuple[str, bool]]:
    """Run 5's checks: (what, whether it holds). `named`: what gave the key, which the agent's
    warning names; given in code, the warning names that and not OTEL_OPAMP_HEADERS."""
    refused = rejected(five)
    traced = [u for e in spans(five) for u in e.get("spans") or []]
    out = [
        (f"the server refused the agent's credentials ({len(refused)} requests) and accepted none",
         bool(refused) and not any(e["event"] == "a2s" for e in five)),
        ("the application's requests are traced all the same", any("/ping" in u for u in traced)),
        (f"the agent's warning names {named}", named in stderr),
    ]
    if named != HEADERS:
        out.append((f"and not {HEADERS}, which the key given in code wins over", HEADERS not in stderr))
    return out


def promoted(events: list[dict]) -> bool:
    """The server confirmed a key of the agent's own: the agent said APPLIED while using it."""
    return any(e["event"] == "key_promoted" for e in events)


def judge_right_key(six: list[dict]) -> list[tuple[str, bool]]:
    """Run 6's checks: (what, whether it holds)."""
    a2s = [e for e in six if e["event"] == "a2s" and not e["disconnect"]]
    declared = [e.get("caps", 0) & TAKES_KEYS == TAKES_KEYS for e in a2s]
    reported = [e for e in a2s if "connection_settings_status" in e["fields"]]
    return [
        (f"the server accepted the agent ({len(a2s)} messages), refusing none",
         len(a2s) > 1 and not rejected(six)),
        ("the effective config is reported", any("effective_config" in e["fields"] for e in a2s)),
        ("the agent says it takes OpAMP connection settings and reports their status, in every message",
         bool(declared) and all(declared)),
        (f"it reports how the key it was offered went ({len(reported)} statuses)", bool(reported)),
        ("it took a key of its own: the server confirmed it (key_promoted)", promoted(six)),
        ("the application's requests are traced", any("/ping" in u for e in spans(six) for u in e.get("spans") or [])),
    ]


def judge_own_key(again: list[dict]) -> list[tuple[str, bool]]:
    """The checks of run 6's next start: (what, whether it holds)."""
    a2s = [e for e in again if e["event"] == "a2s" and not e["disconnect"]]
    gaps = sorted(b["t"] - a["t"] for a, b in itertools.pairwise(a2s))
    median = gaps[len(gaps) // 2] if gaps else 0
    return [
        (f"the next start comes back on the key it was given, kept in its state directory: the server "
         f"accepted it ({len(a2s)} messages), refusing none", len(a2s) > 1 and not rejected(again)),
        ("with no new key to take", not promoted(again)),
        (f"it checks in every second, as it is set up to (median {median:.1f} s)",
         len(gaps) >= 3 and median <= 1.5),
        ("the application's requests are traced",
         any("/ping" in u for e in spans(again) for u in e.get("spans") or [])),
    ]


def check(launch: Launch, instrumentation: str, target_: Path, service: str = "checkout",
          listen: str = "127.0.0.1:0", seconds: tuple[float, float] = (150, 30), switch_at_once: bool = False,
          switches: bool = True, versioned: bool = True) -> bool:
    """Both runs, and every check printed; True when all pass. `target_`: the runner's own
    directory (target()). `seconds`: the most each run waits for what it needs to see.
    switch_at_once: the integration switches instrumentations off while the application runs,
    not at the next start. switches=False: it can't switch them off at all (the application
    instruments itself in code), so it refuses the policy with the switch. versioned=False: the
    application is built on this checkout, so our package has no version to give (Go's, through
    a replace directive): the inventory must leave integration.version out rather than make one
    up."""
    target_.mkdir(parents=True, exist_ok=True)
    state = Path(tempfile.mkdtemp(prefix="state-", dir=target_))
    state.chmod(0o777)  # the application may run as another user, in a container
    ok = True

    def expect(results: list[tuple[str, bool]]) -> None:
        nonlocal ok
        for what, cond in results:
            ok &= cond
            print(f"  {'ok  ' if cond else 'FAIL'} {what}")

    try:
        # Until the fifth offer is answered, then long enough to see what the last one did.
        one = run(launch, scenario(instrumentation), state, "run1", seconds[0], listen, target_,
                  until=lambda events: len(statuses(events)) >= 5, settle=10)
        print(f"run 1: remote config statuses {[s for _, s, _ in statuses(one)]}")
        expect(judge_first(one, service, switch_at_once, switches))
        expect(judge_transport(one))
        expect(judge_inventory(one, instrumentation, switch_at_once, switches, versioned))

        # Until the application has made its requests with the agent connected, then long enough
        # for any span of theirs to have been exported.
        two = run(launch, {"server": {}}, state, "run2", seconds[1], listen, target_,
                  until=connected, settle=2 * MARGIN)
        print(f"run 2: {sum(e['event'] == 'a2s' for e in two)} messages, {len(pings(two))} application requests, "
              f"{len(spans(two))} span exports")
        expect(judge_second(one, two, switches))
        expect(judge_transport(two))
    finally:
        # Files a container wrote as root may stay; the next check makes a directory of its own.
        shutil.rmtree(state, ignore_errors=True)
    return ok


def check_signals(launch: Launch, target_: Path, service: str = "checkout", listen: str = "127.0.0.1:0",
                  seconds: tuple[float, float] = (420, 60), switch_at_once: bool = False) -> bool:
    """Runs 3 and 4, and every check printed; True when all pass. switch_at_once: as check()'s, for
    instrumentations all_disabled."""
    state = Path(tempfile.mkdtemp(prefix="state-signals-", dir=target_))
    state.chmod(0o777)
    ok = True

    def expect(results: list[tuple[str, bool]]) -> None:
        nonlocal ok
        for what, cond in results:
            ok &= cond
            print(f"  {'ok  ' if cond else 'FAIL'} {what}")

    try:
        three = run(launch, signals_scenario(), state, "run3", seconds[0], listen, target_,
                    until=lambda events: len(statuses(events)) >= 9, settle=10, env=SIGNALS)
        print(f"run 3: remote config statuses {[s for _, s, _ in statuses(three)]}, "
              f"{len(signal(three, 'metrics'))} metrics and {len(signal(three, 'logs'))} log exports")
        expect(judge_signals(three, service, switch_at_once))
        expect(judge_transport(three))

        def enough(events: list[dict]) -> bool:
            a2s = [e for e in events if e["event"] == "a2s"]
            return len(a2s) > 5 and connected(events)

        four = run(launch, {"server": {}}, state, "run4", seconds[1], listen, target_,
                   until=enough, settle=2 * MARGIN, env=SIGNALS)
        print(f"run 4: {sum(e['event'] == 'a2s' for e in four)} messages, {len(pings(four))} application requests")
        expect(judge_signals_restart(four))
        expect(judge_transport(four))
    finally:
        shutil.rmtree(state, ignore_errors=True)
    return ok


#: check_truth's policies, `TRUTH_GAP` messages apart: each overrides one control (the first, the
#: baseline, none), and the next leaves it local again, so each window shows that override alone.
#: (The setting judged, the control that sends it, the override's fields.)
TRUTH: list[tuple[str | None, str | None, dict]] = [
    (None, None, {}),
    ("send_traces", "signal_exports.traces", {"enabled": False}),
    ("send_metrics", "signal_exports.metrics", {"enabled": False}),
    ("send_logs", "signal_exports.logs", {"enabled": False}),
    ("sampling_rate", "head_sampling", {"ratio": 0}),
    ("send_logs_level", "log_levels", {"level": "warn"}),
    ("send_logs_level_by_scope", "log_levels", {"scopes": {"app*": "off", "rust_otel_example*": "off"}}),
    ("sampling_rules", "sampling_rules", {"rules": [{"rate": 0, "kind": "client"}]}),
    ("mask_attributes", "attribute_redaction", {"mask": ["url.full", "http.url"]}),
    ("drop_attributes", "attribute_redaction", {"drop": ["url.full", "http.url"]}),
    ("sampling_traces_per_second", "trace_rate_limit", {"traces_per_second": 1}),
    ("send_logs_per_second", "log_rate_limit", {"records_per_second": 1}),
]
#: Where the instrumentation switches are live (switch_at_once), last: nothing traced after it.
TRUTH_SWITCH = ("deactivate_all_instrumentations", "instrumentations", {"all_disabled": True})
#: Messages between check_truth's policies: at one a second, a window of some eight seconds past
#: MARGIN, long enough for a rate cap of one a second to show.
TRUTH_GAP = 12


def truth_scenario(switch_at_once: bool = False, gap: int = TRUTH_GAP) -> dict:
    """check_truth's offers (TRUTH), `gap` messages apart, each a version 2 policy."""
    judged = TRUTH + ([TRUTH_SWITCH] if switch_at_once else [])
    return {"server": {"faults": [
        offer(1 + i * gap, {control: fields} if control else {}, version=2)
        for i, (_, control, fields) in enumerate(judged)]}}


def refused_as_unsupported(error: str, control: str, fields: dict) -> bool:
    """Whether a FAILED answer's `error` is the SDK refusing the override of `control` (one of
    `fields`) because it can't carry it out: the error names the control, and the field where the
    control has several, before the words every SDK refuses such an override with."""
    unsupported = "this SDK cannot apply the requested override"
    return any(error in (f"{control}: {unsupported}", f"{control}: {field}: {unsupported}") for field in fields)


def same(reported: object, sent: object) -> bool:
    """Whether a field reported in force has the value sent. A list is a set of names (attribute
    redaction's), which the core reports sorted and without repeats."""
    if isinstance(reported, list) and isinstance(sent, list):
        key = lambda v: json.dumps(v, sort_keys=True)  # noqa: E731
        return sorted(set(map(key, reported))) == sorted(set(map(key, sent)))
    return reported == sent


def in_force(events: list[dict], control: str, fields: dict, after: float, before: float) -> bool | None:
    """How the effective configs the agent reported between `after` and `before` have the override
    of `control`: True when one has every one of `fields` with its value, False when none has any
    of them (set aside: a policy in force reports what it set aside local), None otherwise (in force
    with other values, which no SDK may report)."""
    reported = [overridden(e, control) or {} for e in effective_configs(events, after, before)]
    if any(all(same(r.get(k), v) for k, v in fields.items()) for r in reported):
        return True
    return False if all(not set(fields) & set(r) for r in reported) else None


def carried_out(events: list[dict], setting: str, applied: float, end: float) -> tuple[bool, str]:
    """Whether the telemetry the sink received shows `setting` carried out, in the window from
    when its document was APPLIED (`applied`) until the next was offered (`end`): (whether it
    does, what was seen). A window with too few of the application's requests to tell shows
    nothing carried out."""
    made = len(pings(events, applied + MARGIN, end - DRIFT))
    window = end - DRIFT - (applied + MARGIN)
    if made <= REQUESTS:
        return False, f"{made} requests, too few to tell"
    if setting in ("send_traces", TRUTH_SWITCH[0]):
        late = ended(events, applied + DRIFT, arrived_before=end)
        return not late, f"{len(late)} spans, {made} requests"
    if setting in ("send_metrics", "send_logs"):
        late = signal(events, setting.removeprefix("send_"), applied + MARGIN, end)
        return not late, f"{len(late)} exports, {made} requests"
    if setting in ("sampling_rate", "sampling_rules"):
        # A rate of 0 for new traces, or a rule of rate 0 for client spans: every example's
        # requests are its HTTP client's root spans.
        n = traced(events, applied + DRIFT, end - DRIFT)
        return not n, f"{n} of {made} requests traced"
    if setting in ("send_logs_level", "send_logs_level_by_scope"):
        per, made = per_request(events, applied + MARGIN, end)
        return per <= 0.1, f"{per:.2f} log records a request, {made} requests"
    if setting in ("mask_attributes", "drop_attributes"):
        names = ending(events, applied + DRIFT, 1e9, arrived_before=end)
        shown = [n for n in names if "://" in n or (setting == "drop_attributes" and n == "[REDACTED]")]
        return bool(names) and not shown, f"{len(names)} spans, {shown[:2]}"
    if setting == "sampling_traces_per_second":
        # Spans, not exports: a span two exporters send is still one span the cap let through. (The
        # sink gives each export's end times on the log's clock, which can differ by a millisecond.)
        n = len({(name, round(at, 2)) for e in spans(events)
                 for name, at in zip(e.get("spans") or [], e.get("ends") or []) if applied + MARGIN < at < end - DRIFT})
        return window > 3 and 0 < n <= window + 3, f"{n} spans in {window:.1f} s of {made} requests"
    if setting == "send_logs_per_second":
        n = records(events, applied + MARGIN, end)
        return window > 3 and 0 < n <= window + 3, f"{n} log records in {window:.1f} s of {made} requests"
    raise ValueError(f"no measure of {setting}")


def judge_truth(events: list[dict], expected_aside: set[str], switch_at_once: bool = False,
                service: str = "checkout") -> list[tuple[str, bool]]:
    """check_truth's checks (what, whether it holds): the baseline APPLIED, and its spans, metrics
    and log records arrive; and each setting either shows in the telemetry or is set aside, never
    neither (an APPLIED answer for an override the SDK didn't carry out). Set aside is the policy
    refused (FAILED, naming the control the SDK can't carry out), or APPLIED and the override
    reported local in the effective config (the SDK stopped carrying it out while it ran). The
    settings set aside must be `expected_aside`: no more (a setting this setup could carry out,
    given up), and no fewer (the integration's table out of date)."""
    judged = TRUTH + ([TRUTH_SWITCH] if switch_at_once else [])
    st = statuses(events)
    out = [(f"{len(judged)} configs answered ({[s for _, s, _ in st]})", len(st) == len(judged)),
           ("the baseline APPLIED", bool(st) and st[0][1] == "APPLIED")]
    if len(st) != len(judged) or st[0][1] != "APPLIED":
        return out
    times = [t for t, _, _ in st]
    nexts = [offered_before(events, t) for t in times[1:]] + [max(e["t"] for e in events)]
    for name in ("traces", "metrics", "logs"):
        got = spans(events, times[0], nexts[0]) if name == "traces" else signal(events, name, times[0], nexts[0])
        out.append((f"baseline: {name} arrive", any(e.get("services") in (None, [service]) for e in got)))
    aside_seen: set[str] = set()
    for (setting, control, fields), (answered, status, error), end in zip(judged[1:], st[1:], nexts[1:]):
        offered = offered_before(events, answered)
        if status == "FAILED":
            refused = refused_as_unsupported(error, control, fields)
            out.append((f"{setting}: refused as an override the SDK can't carry out ({error!r})", refused))
            aside_seen |= {setting} if refused else set()
            continue
        if status != "APPLIED":
            out.append((f"{setting}: answered APPLIED or FAILED, not {status}", False))
            continue
        reported = in_force(events, control, fields, offered, end)
        if reported is None:
            out.append((f"{setting}: APPLIED, and reported in force with other values than sent", False))
            continue
        if not reported:
            aside_seen.add(setting)
            out.append((f"{setting}: APPLIED, set aside and reported local", True))
            continue
        done, seen = carried_out(events, setting, answered, end)
        out.append((f"{setting}: APPLIED and carried out ({seen})" if done else
                    f"{setting}: APPLIED, in force, and the telemetry didn't change ({seen})", done))
    extra = sorted(aside_seen - expected_aside)
    missing = sorted(expected_aside & {s for s, _, _ in judged} - aside_seen)
    out.append((f"set aside as this setup expects ({sorted(aside_seen)}; not expected: {extra}; "
                f"expected, carried out: {missing})", not extra and not missing))
    return out


def check_truth(launch: Launch, target_: Path, setup: str, expected_aside: set[str], env: dict[str, str] | None = None,
                switch_at_once: bool = False, service: str = "checkout", listen: str = "127.0.0.1:0",
                seconds: float = 360) -> bool:
    """One run of TRUTH's documents under `setup`, the application's setup the matrix in
    docs/sdk-settings.md names as one where a setting may not reach the telemetry (EXAMPLE_SETUP in
    its environment, beside `env`); every check printed; True when all pass."""
    state = Path(tempfile.mkdtemp(prefix=f"state-truth-{setup}-", dir=target_))
    state.chmod(0o777)
    ok = True
    try:
        judged = len(TRUTH) + switch_at_once
        events = run(launch, truth_scenario(switch_at_once), state, f"truth-{setup}", seconds, listen, target_,
                     # The last window as long as the others: some eight seconds of requests.
                     until=lambda ev: len(statuses(ev)) >= judged and len(pings(ev, statuses(ev)[-1][0])) > 8 * REQUESTS,
                     settle=2, env={**SIGNALS, **(env or {}), "EXAMPLE_SETUP": setup})
        print(f"truth ({setup}): remote config statuses {[s for _, s, _ in statuses(events)]}")
        for what, cond in judge_truth(events, expected_aside, switch_at_once, service) + judge_transport(events):
            ok &= cond
            print(f"  {'ok  ' if cond else 'FAIL'} {what}")
    finally:
        shutil.rmtree(state, ignore_errors=True)
    return ok


def key_from_env(token: str) -> dict[str, str]:
    """The environment that gives the agent `token`: OTEL_OPAMP_HEADERS."""
    return {HEADERS: f"Authorization=Bearer%20{token}"}


def key_in_code(variable: str) -> Callable[[str], dict[str, str]]:
    """For an application that gives the agent its token in code, from its own `variable`: the
    environment that gives it `token`, with OTEL_OPAMP_HEADERS set to the other token (the wrong
    one for the right, and the right one for the wrong), which the code wins over."""
    def key(token: str) -> dict[str, str]:
        return {variable: token} | key_from_env("example" if token == "wrong" else "wrong")
    return key


def check_setup(launch: Launch, target_: Path, listen: str = "127.0.0.1:0", seconds: float = 60,
                env: dict[str, str] | None = None, key: Callable[[str], dict[str, str]] = key_from_env,
                named: str = HEADERS) -> bool:
    """Runs 5 and 6 (and 6's next start), and every check printed; True when all pass. `env` is added to the
    application's environment in both: the agent's warning is read from the application's stderr,
    so an integration whose logs go elsewhere when exported says so here. key(token): the
    environment that gives the agent `token`, wrong in run 5 and right in run 6 (key_from_env, or
    key_in_code(variable)); `named`: what the agent's warning names, the option given in code when
    the key is given there."""
    target_.mkdir(parents=True, exist_ok=True)
    state = Path(tempfile.mkdtemp(prefix="state-setup-", dir=target_))
    state.chmod(0o777)
    scenario_ = {"server": {"enrollment_key": "example"}}
    env = env or {}
    ok = True

    def expect(results: list[tuple[str, bool]]) -> None:
        nonlocal ok
        for what, cond in results:
            ok &= cond
            print(f"  {'ok  ' if cond else 'FAIL'} {what}")

    try:
        def refused(events: list[dict]) -> bool:
            return len(rejected(events)) > 1 and len(pings(events)) > 2 * REQUESTS and bool(spans(events))

        five = run(launch, scenario_, state, "run5", seconds, listen, target_, until=refused, settle=MARGIN,
                   env=env | key("wrong"))
        stderr = target_ / "run5.stderr"
        print(f"run 5: {len(rejected(five))} requests refused, {len(pings(five))} application requests")
        expect(judge_wrong_key(five, stderr.read_text(errors="replace") if stderr.exists() else "", named))

        def keyed(events: list[dict]) -> bool:
            return promoted(events) and bool(spans(events))

        def accepted(events: list[dict]) -> bool:
            return sum(e["event"] == "a2s" for e in events) > 5 and bool(spans(events))

        six, again = run_and_restart(launch, scenario_, state, "run6", seconds, listen, target_, until=keyed,
                                     again=accepted, settle=MARGIN, env=env | key("example"))
        print(f"run 6: {sum(e['event'] == 'a2s' for e in six)} messages, {len(rejected(six))} refused, "
              f"key {'confirmed' if promoted(six) else 'not confirmed'}; the next start: "
              f"{sum(e['event'] == 'a2s' for e in again)} messages, {len(rejected(again))} refused")
        expect(judge_right_key(six))
        expect(judge_transport(six))
        expect(judge_own_key(again))
        expect(judge_transport(again))
    finally:
        shutil.rmtree(state, ignore_errors=True)
    return ok


def connections(events: list[dict], listener: str) -> list[dict]:
    """The connections the fixture accepted on `listener` ("first" or "second"; with a move)."""
    return [e for e in events if e["event"] == "connection" and e.get("listener") == listener]


def judge_rotation(nine: list[dict]) -> list[tuple[str, bool]]:
    """The rotation run's checks: (what, whether it holds)."""
    keys = [e for e in nine if e["event"] == "key_promoted"]
    done = [(e.get("action"), e.get("agents", 0)) for e in nine if e["event"] == "operator"]
    second = connections(nine, "second")
    reached = second[0]["t"] if second else None
    back = [e for e in connections(nine, "first") if reached is not None and e["t"] > reached]
    after = [e for e in nine if e["event"] == "a2s" and reached is not None and e["t"] > reached]
    return [
        (f"the operator rotated the agent's key and then moved it ({done})",
         [a for a, _ in done] == ["rotate", "move"] and all(n >= 1 for _, n in done)),
        (f"the agent took its own key, the second and the moved one: three confirmed ({len(keys)})", len(keys) == 3),
        (f"no request refused ({len(rejected(nine))})", not rejected(nine)),
        ("it reached the second listener", reached is not None),
        (f"and opened no connection to the first after that ({len(back)})", reached is not None and not back),
        (f"the server heard from it there ({len(after)} messages)", len(after) > 1),
        ("the application's requests are traced after the move",
         reached is not None and any("/ping" in u for e in spans(nine, after=reached) for u in e.get("spans") or [])),
    ]


def judge_moved_start(again: list[dict]) -> list[tuple[str, bool]]:
    """The checks of the rotation run's next start: (what, whether it holds)."""
    a2s = [e for e in again if e["event"] == "a2s" and not e["disconnect"]]
    return [
        (f"the next start goes straight to the second listener ({len(connections(again, 'second'))} connections), "
         f"none to the first ({len(connections(again, 'first'))})",
         bool(connections(again, "second")) and not connections(again, "first")),
        (f"on the key it was given there: the server accepted it ({len(a2s)} messages), refusing none",
         len(a2s) > 1 and not rejected(again)),
        ("with no new key to take", not promoted(again)),
        ("the application's requests are traced",
         any("/ping" in u for e in spans(again) for u in e.get("spans") or [])),
    ]


def check_rotation(launch: Launch, target_: Path, listen: str = "127.0.0.1:0", seconds: float = 90,
                   env: dict[str, str] | None = None, key: Callable[[str], dict[str, str]] = key_from_env,
                   host: str = "127.0.0.1") -> bool:
    """The rotation run and its next start, and every check printed; True when all pass. `env` and
    `key` as check_setup's; `host`: what the application reaches the server by (opamp_url()), which
    the move names too."""
    target_.mkdir(parents=True, exist_ok=True)
    state = Path(tempfile.mkdtemp(prefix="state-rotation-", dir=target_))
    state.chmod(0o777)
    scenario_ = {"server": {"enrollment_key": "example", "after_promotion": ["rotate", "move"], "move_host": host}}
    env = env or {}
    ok = True

    def expect(results: list[tuple[str, bool]]) -> None:
        nonlocal ok
        for what, cond in results:
            ok &= cond
            print(f"  {'ok  ' if cond else 'FAIL'} {what}")

    try:
        def moved(events: list[dict]) -> bool:
            second = connections(events, "second")
            return (sum(e["event"] == "key_promoted" for e in events) >= 3 and bool(second)
                    and bool(spans(events, after=second[0]["t"])))

        def accepted(events: list[dict]) -> bool:
            return sum(e["event"] == "a2s" for e in events) > 5 and bool(spans(events))

        nine, again = run_and_restart(launch, scenario_, state, "rotation", seconds, listen, target_, until=moved,
                                      again=accepted, settle=MARGIN, env=env | key("example"))
        print(f"the rotation run: {sum(e['event'] == 'key_promoted' for e in nine)} keys confirmed, "
              f"{len(connections(nine, 'second'))} connections to the second listener; the next start: "
              f"{sum(e['event'] == 'a2s' for e in again)} messages, {len(rejected(again))} refused")
        expect(judge_rotation(nine))
        expect(judge_transport(nine))
        expect(judge_moved_start(again))
        expect(judge_transport(again))
    finally:
        shutil.rmtree(state, ignore_errors=True)
    return ok


def check_app_settings(launch: Launch, target_: Path, listen: str = "127.0.0.1:0",
                       seconds: tuple[float, float] = (150, 60), env: dict[str, str] | None = None) -> bool:
    """Runs 7 and 8, and every check printed; True when all pass. For an integration whose example
    application reads the app_settings key example.feature (docs/app-settings.md): each round, a
    request to /feature while it is `on`, and
    one to /changed for each change it is told of. `env` is added to the application's environment."""
    state = Path(tempfile.mkdtemp(prefix="state-app-", dir=target_))
    state.chmod(0o777)
    env = env or {}
    ok = True

    def expect(results: list[tuple[str, bool]]) -> None:
        nonlocal ok
        for what, cond in results:
            ok &= cond
            print(f"  {'ok  ' if cond else 'FAIL'} {what}")

    try:
        def done(events: list[dict]) -> bool:
            st = statuses(events)
            return len(st) >= 5 and len(pings(events, st[-1][0])) > REQUESTS

        # (Its files are app7 and app8: an integration's own runs 7 to 9 write run7 to run9 in the same
        # directory, and the two checks may run at once, main()'s --jobs.)
        seven = run(launch, app_settings_scenario(), state, "app7", seconds[0], listen, target_, until=done,
                    settle=MARGIN, env=env)
        print(f"run 7: remote config statuses {[s for _, s, _ in statuses(seven)]}, "
              f"{len(requests(seven, '/feature'))} /feature and {len(requests(seven, '/changed'))} /changed requests")
        expect(judge_app_settings(seven))
        expect(judge_transport(seven))

        def enough(events: list[dict]) -> bool:
            a2s = [e for e in events if e["event"] == "a2s"]
            return bool(a2s) and len(pings(events, a2s[0]["t"])) > 2 * REQUESTS

        eight = run(launch, {"server": {}}, state, "app8", seconds[1], listen, target_, until=enough,
                    settle=MARGIN, env=env)
        print(f"run 8: {len(pings(eight))} application requests, {len(requests(eight, '/feature'))} /feature")
        expect(judge_app_settings_restart(eight))
        expect(judge_transport(eight))
    finally:
        shutil.rmtree(state, ignore_errors=True)
    return ok


def main(checks: dict[str, Callable[[], bool]], prepare: Callable[[], object] = lambda: None,
         argv: list[str] | None = None, options: tuple[str, ...] = (), script: str | None = None) -> int:
    """An integration's try.py: run the checks it names (`checks`, each independent of the others:
    its own server, state directory and logs), in order, every one by default; the exit status.

        try.py                    # every check, after prepare() (the integration's build)
        try.py signals setup      # those alone, as one of CI's jobs runs its share
        try.py --jobs 4 signals   # up to four at once, each in a process of its own, after one prepare()
        try.py --list             # their names

    A check waits on its scenario's clock (one message a second), not on the machine, so several
    run side by side in the time of the longest; each one's output is printed whole when it ends.
    `options`: the flags of the try.py's own (`--local`), left for it to read, and given to each
    check's process. `script`: the try.py, for those processes (the program run, by default)."""
    argv = list(sys.argv[1:] if argv is None else argv)
    given = [a for a in argv if a in options]
    argv = [a for a in argv if a not in options]
    if argv == ["--list"]:
        print("\n".join(checks))
        return 0
    jobs, prepared = 1, False
    while argv[:1] in (["--jobs"], ["--prepared"]):
        if argv[0] == "--prepared":
            # (A check's process under --jobs: the build is done.)
            prepared, argv = True, argv[1:]
        elif len(argv) < 2 or not argv[1].isdigit() or int(argv[1]) < 1:
            print("--jobs: how many checks at once, 1 or more", file=sys.stderr)
            return 2
        else:
            jobs, argv = int(argv[1]), argv[2:]
    unknown = [a for a in argv if a not in checks]
    if unknown:
        print(f"no such check: {' '.join(unknown)}; the checks: {' '.join(checks)}", file=sys.stderr)
        return 2
    if not prepared:
        prepare()
    names = argv or list(checks)
    if jobs > 1 and len(names) > 1:
        program = script or sys.argv[0]

        def one(name: str) -> tuple[str, int]:
            r = subprocess.run([sys.executable, program, *given, "--prepared", name], stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, text=True, check=False)
            return r.stdout, r.returncode

        ok = True
        # Each check's output as soon as it ends: a job cut short keeps what had finished.
        with concurrent.futures.ThreadPoolExecutor(jobs) as pool:
            for done in concurrent.futures.as_completed([pool.submit(one, name) for name in names]):
                output, code = done.result()
                print(output, end="", flush=True)
                ok &= code == 0
        return 0 if ok else 1
    ok = True
    for name in names:
        print(f"=== {name}", flush=True)
        start = time.monotonic()
        passed = checks[name]()
        print(f"=== {name}: {'passed' if passed else 'FAILED'} in {time.monotonic() - start:.0f} s", flush=True)
        ok &= passed
    return 0 if ok else 1
