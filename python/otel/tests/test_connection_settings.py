# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""A key of the agent's own (docs/connection-settings.md): with a state directory the agent says it
takes OpAMP connection settings, takes the key the server offers, says APPLIED on it, and keeps it
for the next start; without one it says nothing of the kind."""

from __future__ import annotations

import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from pydantic_opamp_client import opamp_pb2
from opentelemetry.sdk.resources import Resource

from pydantic_opamp_otel import Agent

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
    enrollment key speaks for it no more (401). Once the agent has said APPLIED of its own key,
    it can offer the next settings (`next`): another key, here or at another server's endpoint."""

    def __init__(self, keys: tuple[str, ...] = ("own",), enroll: bool = True) -> None:
        self.lock = threading.Lock()
        self.caps: list[int] = []
        self.auth: list[str | None] = []
        self.statuses: list[tuple[int, bytes]] = []  # (status, hash) of each one reported
        self.applied = False
        self.own_used = False
        #: The keys it takes besides the enrollment key, which it takes only with `enroll`.
        self.keys, self.enroll = keys, enroll
        #: Offered in answer to each message with "own" once the agent said APPLIED of it.
        self.next: opamp_pb2.ConnectionSettingsOffers | None = None
        #: Once a request comes with this key, "own" speaks for the agent no more.
        self.retires: str | None = None
        self.own_gone = False
        me = self

        class OpAMP(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                msg = opamp_pb2.AgentToServer.FromString(self.rfile.read(int(self.headers["Content-Length"])))
                auth = self.headers.get("Authorization")
                with me.lock:
                    me.auth.append(auth)
                    key = (auth or "").removeprefix("Bearer ")
                    taken = (auth == "Bearer enroll" and me.enroll and not me.own_used
                             or key in me.keys and not (key == "own" and me.own_gone))
                    if not taken:
                        self.send_response(401)
                        self.send_header("Content-Length", "0")
                        self.end_headers()
                        return
                    me.own_used |= key == "own"
                    me.own_gone |= me.retires is not None and key == me.retires
                    me.caps.append(msg.capabilities)
                    s = msg.connection_settings_status
                    if msg.HasField("connection_settings_status"):
                        me.statuses.append((s.status, s.last_connection_settings_hash))
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
                    elif key == "own" and me.next is not None and me.applied:
                        answer.connection_settings.CopyFrom(me.next)
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

    def said(self, status: int, hash_: bytes) -> bool:
        """Whether the agent reported `status` of the offer `hash_` here."""
        with self.lock:
            return (status, hash_) in self.statuses

    def requests(self) -> int:
        with self.lock:
            return len(self.auth)

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


def _offer(hash_: bytes, endpoint: str, key: str) -> opamp_pb2.ConnectionSettingsOffers:
    o = opamp_pb2.ConnectionSettingsOffers(hash=hash_)
    o.opamp.destination_endpoint = endpoint
    o.opamp.headers.headers.add(key="Authorization", value=f"Bearer {key}")
    return o


APPLIED, FAILED = opamp_pb2.ConnectionSettingsStatuses_APPLIED, opamp_pb2.ConnectionSettingsStatuses_FAILED


def test_the_agent_follows_a_move_to_another_server_and_keeps_it(server, tmp_path):
    """Once it has its own key, A offers another key at server B's endpoint: the agent goes to B
    with it, says APPLIED of the move there, and the next start goes to B with it first."""
    b = _KeyServer(keys=("moved",), enroll=False)
    try:
        with server.lock:
            server.next = _offer(b"move-to-b", b.endpoint, "moved")
        agent = _started(server, str(tmp_path))
        try:
            b.wait(lambda: b.said(APPLIED, b"move-to-b"))
            # What the agent says of reaching its server names B from now on.
            assert agent.client is not None and agent.client.endpoint == b.endpoint
        finally:
            agent.stop()
        with b.lock:
            assert all(a == "Bearer moved" for a in b.auth)
        at_a, at_b = server.requests(), b.requests()
        again = _started(server, str(tmp_path))
        try:
            b.wait(lambda: b.requests() > at_b)
            assert server.requests() == at_a, "the next start went to A first, not to B"
        finally:
            again.stop()
        assert b.auth[at_b] == "Bearer moved", "the next start comes to B with the key it was moved with"
    finally:
        b.close()


def test_the_agent_takes_a_second_key_and_keeps_it(server, tmp_path):
    """A second key at the same endpoint: the agent takes "own2", keeps it across a restart, and
    never sends "own" again."""
    with server.lock:
        server.keys, server.retires = ("own", "own2"), "own2"
        server.next = _offer(b"second-key", server.endpoint, "own2")
    agent = _started(server, str(tmp_path))
    try:
        server.wait(lambda: server.said(APPLIED, b"second-key"))
        since = server.requests()
        server.wait(lambda: server.requests() > since + 2)
    finally:
        agent.stop()
    again = _started(server, str(tmp_path))
    try:
        at = server.requests()
        server.wait(lambda: server.requests() > at + 2)
    finally:
        again.stop()
    with server.lock:
        assert all(a == "Bearer own2" for a in server.auth[since:])


def test_a_move_nobody_answers_fails_and_the_agent_stays_where_it_was(server, tmp_path):
    """A move to an endpoint nobody listens on: the agent goes back to A with the key it has, says
    FAILED of the move there, and carries on with A."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        closed = f"http://127.0.0.1:{s.getsockname()[1]}/v1/opamp"
    with server.lock:
        server.next = _offer(b"move-nowhere", closed, "moved")
    agent = _started(server, str(tmp_path))
    try:
        server.wait(lambda: server.said(FAILED, b"move-nowhere"))
        since = server.requests()
        server.wait(lambda: server.requests() > since + 2)
    finally:
        agent.stop()
    with server.lock:
        assert all(a == "Bearer own" for a in server.auth[since:])
    assert not server.said(APPLIED, b"move-nowhere")
