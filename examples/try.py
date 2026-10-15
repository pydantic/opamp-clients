# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Run an example against the conformance suite's reference server, which offers a good remote
config, then a bad one, and tells the example to send its traces to the server's own OTLP/HTTP
receiver with a token; print what the server saw.

    python3 examples/try.py go|node|python|java|dotnet http|ws   (and rust, where examples/rust is)

Java and .NET build and run in Docker, as the conformance suite does.
"""
from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
SERVER = ROOT / "target" / "example-server"
DOCKER_RUN = ["docker", "run", "--rm", "--add-host=host.docker.internal:host-gateway", "-e", "OPAMP_ENDPOINT"]

# language -> (build commands, run command, in Docker)
EXAMPLES = {
    "go": ([["go", "build", "-o", "example", "."]], ["./example"], False),
    "node": ([["npm", "run", "build", "--prefix", str(ROOT / "node/opamp")],
              ["npm", "install", "--no-audit", "--no-fund"]], ["node", "example.mjs"], False),
    "python": ([], ["uv", "run", "--quiet", "--project", str(ROOT / "python"), "--with", "opentelemetry-sdk==1.38.0",
                    "--with", "opentelemetry-exporter-otlp-proto-http==1.38.0",
                    "python", "example.py"], False),
    "java": ([["docker", "run", "--rm", "--network", "host", "-v", f"{ROOT}/java/opamp:/src", "-v", "opamp-clients-m2:/root/.m2", "-w", "/src",
               "maven:3.9-eclipse-temurin-21", "mvn", "-q", "-B", "-DskipTests", "install"],
              ["docker", "run", "--rm", "--network", "host", "-v", f"{HERE}/java:/src", "-v", "opamp-clients-m2:/root/.m2", "-w", "/src",
               "maven:3.9-eclipse-temurin-21", "mvn", "-q", "-B", "package"]],
             DOCKER_RUN + ["-v", f"{HERE}/java:/ex", "maven:3.9-eclipse-temurin-21",
                           "java", "-cp", "/ex/target/classes:/ex/target/lib/*", "Example"], True),
    "dotnet": ([["docker", "run", "--rm", "--network", "host", "-v", f"{ROOT}:/repo", "-v", "opamp-clients-nuget:/root/.nuget",
                 "-w", "/repo/examples/dotnet", "mcr.microsoft.com/dotnet/sdk:8.0",
                 "dotnet", "publish", "-c", "Release", "-o", "/repo/examples/dotnet/out", "--nologo", "-v", "q"]],
               DOCKER_RUN + ["-v", f"{HERE}/dotnet/out:/ex", "mcr.microsoft.com/dotnet/runtime:8.0",
                             "dotnet", "/ex/Example.dll"], True),
}

# The Rust client's example is kept beside the core's sources, where examples/rust is.
if (HERE / "rust").is_dir():
    EXAMPLES["rust"] = ([["cargo", "build", "--quiet"]], ["target/debug/example"], False)


def main() -> int:
    language, transport = sys.argv[1], sys.argv[2]
    builds, run, docker = EXAMPLES[language]
    cwd = HERE / language
    subprocess.run(["go", "build", "-o", str(SERVER), "."], cwd=ROOT / "conformance" / "server", check=True)
    for cmd in builds:
        subprocess.run(cmd, cwd=cwd, check=True, stdout=subprocess.DEVNULL)

    log = ROOT / "target" / f"example-server-{language}-{transport}.jsonl"
    server = subprocess.Popen([str(SERVER), "-listen", "0.0.0.0:0" if docker else "127.0.0.1:0",
                               "-scenario", str(HERE / "scenario.json"), "-log", str(log)],
                              stdout=subprocess.PIPE, text=True)
    port = json.loads(server.stdout.readline())["addr"].rsplit(":", 1)[1]
    host = "host.docker.internal" if docker else "127.0.0.1"
    scheme = "ws" if transport == "ws" else "http"
    env = dict(os.environ, OPAMP_ENDPOINT=f"{scheme}://{host}:{port}/v1/opamp")
    example = subprocess.Popen(run, cwd=cwd, env=env, start_new_session=True)
    time.sleep(12 if docker else 8)
    try:
        os.killpg(example.pid, signal.SIGINT)  # as Ctrl-C would
        code = example.wait(timeout=20)
    except (ProcessLookupError, PermissionError):
        code = example.wait()
    except subprocess.TimeoutExpired:
        os.killpg(example.pid, signal.SIGKILL)
        code = "killed"
    time.sleep(0.3)
    server.send_signal(signal.SIGTERM)
    server.wait()

    a2s = [e for e in map(json.loads, log.read_text().splitlines()) if e["event"] == "a2s"]
    statuses = [e["remote_config_status"]["status"].rsplit("_", 1)[-1] for e in a2s if e.get("remote_config_status")]
    said_goodbye = bool(a2s) and a2s[-1]["disconnect"]
    events = list(map(json.loads, log.read_text().splitlines()))
    offers = {e["telemetry_offer"] for e in events if e.get("telemetry_offer")}
    telemetry = [e["connection_settings_status"]["status"].rsplit("_", 1)[-1] for e in a2s
                 if e.get("connection_settings_status", {}).get("hash") in offers]
    spans = [e for e in events if e["event"] == "otlp" and e["signal"] == "traces"]
    # With the offered token, and the SDK's resource (service.name).
    with_token = [e for e in spans if e["authorization"] == "Bearer example-token" and e["services"] == ["checkout"]]
    print(f"{language}/{transport}: exit {code}, {len(a2s)} messages, remote config statuses {statuses}, "
          f"goodbye {said_goodbye}, telemetry statuses {telemetry}, {len(with_token)}/{len(spans)} span exports "
          f"with the offered token and service.name")
    # Each config is reported APPLYING first (docs/remote-config.md), unless the answer overtakes it.
    settled = [s for s in statuses if s != "APPLYING"]
    # The bad config ends FAILED. The good one is APPLIED, unless the bad one arrived before the
    # application answered: then the answer is about a config since replaced, and not reported.
    ok = settled[-1:] == ["FAILED"] and set(settled) <= {"APPLIED", "FAILED"} and said_goodbye
    # The traces go where the server said, with its token, and the server hears APPLIED.
    ok = ok and "APPLIED" in telemetry and bool(with_token) and len(with_token) == len(spans)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
