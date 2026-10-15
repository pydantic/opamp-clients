# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""A key of the agent's own (docs/connection-settings.md): with a state directory the agent says it
takes OpAMP connection settings, takes the key the server offers, says APPLIED on it, and keeps it
for the next start; without one it says nothing of the kind."""

from __future__ import annotations

import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from opamp_client import opamp_pb2
from opentelemetry.sdk.resources import Resource

from opamp_otel import Agent

TAKES_KEYS = (opamp_pb2.AgentCapabilities_AcceptsOpAMPConnectionSettings
              | opamp_pb2.AgentCapabilities_ReportsConnectionSettingsStatus)
OFFER = b"own-key-offer"


@pytest.fixture(autouse=True)
def _own_state_home(tmp_path_factory, monkeypatch):
    home = tmp_path_factory.mktemp("state-home")
    monkeypatch.setenv("XDG_STATE_HOME", str(home))
    monkeypatch.setenv("HOME", str(home))


class _KeyServer:
    """An OpAMP server over plain HTTP that takes an enrollment key and, as the reference server
    does, hands an agent that says it takes connection settings a key of its own in its answer to
    the agent's first message with the enrollment key; once the agent uses its own key, the
    enrollment key speaks for it no more (401)."""

    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.caps: list[int] = []
        self.auth: list[str | None] = []
        self.applied = False
        self.own_used = False
        me = self

        class OpAMP(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                msg = opamp_pb2.AgentToServer.FromString(self.rfile.read(int(self.headers["Content-Length"])))
                auth = self.headers.get("Authorization")
                with me.lock:
                    me.auth.append(auth)
                    if auth not in ("Bearer enroll", "Bearer own") or (auth == "Bearer enroll" and me.own_used):
                        self.send_response(401)
                        self.send_header("Content-Length", "0")
                        self.end_headers()
                        return
                    me.own_used |= auth == "Bearer own"
                    me.caps.append(msg.capabilities)
                    s = msg.connection_settings_status
                    if (msg.HasField("connection_settings_status") and auth == "Bearer own"
                            and s.status == opamp_pb2.ConnectionSettingsStatuses_APPLIED
                            and s.last_connection_settings_hash == OFFER):
                        me.applied = True
                    answer = opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid)
                    if auth == "Bearer enroll" and msg.capabilities & TAKES_KEYS == TAKES_KEYS:
                        answer.connection_settings.hash = OFFER
                        o = answer.connection_settings.opamp
                        o.destination_endpoint = me.endpoint
                        o.headers.headers.add(key="Authorization", value="Bearer own")
                        o.heartbeat_interval_seconds = 1
                body = answer.SerializeToString()
                self.send_response(200)
                self.send_header("Content-Type", "application/x-protobuf")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        self._http = ThreadingHTTPServer(("127.0.0.1", 0), OpAMP)
        threading.Thread(target=self._http.serve_forever, daemon=True).start()
        self.endpoint = f"http://127.0.0.1:{self._http.server_port}/v1/opamp"

    def wait(self, cond, seconds: float = 15) -> None:
        deadline = time.time() + seconds
        while not cond() and time.time() < deadline:
            time.sleep(0.05)
        assert cond()

    def close(self) -> None:
        self._http.shutdown()
        self._http.server_close()


@pytest.fixture
def server():
    s = _KeyServer()
    yield s
    s.close()


def _started(server: _KeyServer, state_dir: str) -> Agent:
    agent = Agent()
    agent.prepare({"OTEL_OPAMP_ENDPOINT": server.endpoint, "OTEL_OPAMP_STATE_DIR": state_dir,
                   "OTEL_OPAMP_HEADERS": "Authorization=Bearer%20enroll", "OTEL_OPAMP_HEARTBEAT_INTERVAL": "100ms"})
    agent.configured = True
    agent.start(Resource.create({"service.name": "checkout"}))
    return agent


def test_with_a_state_directory_the_agent_takes_a_key_of_its_own_and_keeps_it(server, tmp_path):
    agent = _started(server, str(tmp_path))
    try:
        server.wait(lambda: server.applied)
    finally:
        agent.stop()
    with server.lock:
        assert server.caps[0] & TAKES_KEYS == TAKES_KEYS
        before = len(server.auth)
    again = _started(server, str(tmp_path))
    try:
        server.wait(lambda: len(server.auth) > before)
    finally:
        again.stop()
    assert server.auth[before] == "Bearer own", "the next start comes with the key it was given"


def test_without_a_state_directory_the_agent_doesnt_say_it_takes_connection_settings(server):
    agent = _started(server, "none")
    try:
        server.wait(lambda: len(server.caps) >= 2)
    finally:
        agent.stop()
    assert all(c & TAKES_KEYS == 0 for c in server.caps)
    assert all(a == "Bearer enroll" for a in server.auth)
