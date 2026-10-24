# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""configure_logfire: the Logfire SDK under the server (docs/python-otel.md, "The Logfire SDK")."""

import gzip
import subprocess
import sys
import textwrap
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
from pydantic_opamp_client import opamp_pb2

from _policy import policy
from test_opamp_otel import APPLIED, FAILED, _child_env, _own_state_home, _Server, config  # noqa: F401 - the fixture


class _Ingest:
    """Logfire's ingest, as far as the exporters see it: keeps every span it is sent, with the
    request's Authorization header."""

    def __init__(self) -> None:
        self.spans: list[tuple[str, dict]] = []
        self.auth: set[str] = set()
        spans, auth = self.spans, self.auth

        class Ingest(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                body = self.rfile.read(int(self.headers["Content-Length"]))
                if self.headers.get("Content-Encoding") == "gzip":
                    body = gzip.decompress(body)
                auth.add(self.headers.get("Authorization", ""))
                if self.path == "/v1/traces":
                    for rs in ExportTraceServiceRequest.FromString(body).resource_spans:
                        resource = {a.key: a.value.string_value for a in rs.resource.attributes}
                        for ss in rs.scope_spans:
                            for s in ss.spans:
                                attrs = {a.key: a.value.string_value for a in s.attributes}
                                spans.append((s.name, {**attrs, "resource": resource}))
                self.send_response(200)
                self.send_header("Content-Length", "0")
                self.end_headers()

            def log_message(self, *args):
                pass

        self._http = ThreadingHTTPServer(("127.0.0.1", 0), Ingest)
        threading.Thread(target=self._http.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self._http.server_port}"

    def names(self) -> list[str]:
        return [name for name, _ in self.spans]

    def close(self) -> None:
        self._http.shutdown()
        self._http.server_close()


def _run(server: _Server, ingest: _Ingest, tmp_path, offer: str, console: bool = False, extra: str = ""):
    """A child that sets the Logfire SDK up with configure_logfire, sends a span "before", waits for
    the server's answer to `offer`, then sends "after" (with a `user.email` attribute)."""
    ready, answered = tmp_path / "ready", tmp_path / "answered"
    remote = config(offer)

    def reply(msg):
        if msg.remote_config_status.status in (APPLIED, FAILED):
            answered.touch()
        return opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid, remote_config=remote) if ready.exists() else None

    server._reply = reply
    child = textwrap.dedent(f"""
        import pathlib, time, logfire, pydantic_opamp_otel
        pydantic_opamp_otel.configure_logfire(
            endpoint={server.endpoint!r}, heartbeat_interval=0.1, token="pylf_v1_us_test",
            service_name="rocket", console={None if console else False!r}, metrics=False, send_to_logfire=True,
            inspect_arguments=False,
            advanced=logfire.AdvancedOptions(base_url={ingest.url!r}){extra})
        agent = pydantic_opamp_otel.AGENT
        with logfire.span("before"):
            pass
        logfire.force_flush()
        pathlib.Path({str(ready)!r}).touch()
        deadline = time.monotonic() + 10
        while not pathlib.Path({str(answered)!r}).exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        time.sleep(0.2)
        with logfire.span("after", **{{"user.email": "a@example.com"}}):
            pass
        logfire.force_flush()
        print("uid", agent.uid)
        print("live", sorted(agent.host.live))
    """)
    return subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60,
                          env=_child_env(LOGFIRE_SEND_TO_LOGFIRE="", LOGFIRE_TOKEN=""))


class _ReplyServer(_Server):
    def __init__(self) -> None:
        self._reply = None
        super().__init__(reply=lambda msg: self._reply(msg) if self._reply else None)


@pytest.fixture
def servers():
    server, ingest = _ReplyServer(), _Ingest()
    yield server, ingest
    server.close()
    ingest.close()


def test_the_logfire_sdk_takes_the_servers_settings(tmp_path, servers):
    """The Logfire SDK set up with configure_logfire joins the fleet and sends to Logfire through
    the package's gates: a policy that masks an attribute is carried out on what reaches Logfire,
    with the instance UID as service.instance.id and the token as the Logfire SDK sends it."""
    server, ingest = servers
    run = _run(server, ingest, tmp_path, policy(attribute_redaction={"mask": ["user.email"]}))
    assert run.returncode == 0 and "Traceback" not in run.stderr, run.stderr
    uid = uuid.UUID(run.stdout.split("uid ")[-1].split()[0])
    assert {m.instance_uid for m in server.got} == {uid.bytes}
    assert any(m.remote_config_status.status == APPLIED for m in server.got)
    assert server.got[-1].HasField("agent_disconnect")
    assert ingest.names() == ["before", "after"], run.stderr
    (after,) = [a for n, a in ingest.spans if n == "after"]
    assert after["user.email"] != "a@example.com"
    assert after["resource"]["service.instance.id"] == str(uid)
    assert after["resource"]["service.name"] == "rocket"
    assert ingest.auth == {"pylf_v1_us_test"}
    # Every gate, the sampler and redaction are carried out: nothing exports past them.
    for setting in ("send_traces", "send_logs", "sampling_rate", "mask_attributes", "scrub_patterns"):
        assert setting in run.stdout.split("live ")[-1]


def test_a_logfire_sdk_stops_sending_traces_when_the_server_says(tmp_path, servers):
    server, ingest = servers
    run = _run(server, ingest, tmp_path, policy(signal_exports={"traces": {"enabled": False}}))
    assert run.returncode == 0 and "Traceback" not in run.stderr, run.stderr
    assert any(m.remote_config_status.status == APPLIED for m in server.got)
    assert ingest.names() == ["before"]


def test_console_output_is_past_the_gates_so_a_policy_that_needs_them_is_refused(tmp_path, servers):
    """The Logfire SDK's console shows every span, ungated: a policy that masks an attribute
    can't be carried out on it, so it is refused, and what reaches Logfire is as before."""
    server, ingest = servers
    run = _run(server, ingest, tmp_path, policy(attribute_redaction={"mask": ["user.email"]}), console=True)
    assert run.returncode == 0 and "Traceback" not in run.stderr, run.stderr
    assert any(m.remote_config_status.status == FAILED for m in server.got)
    assert not any(m.remote_config_status.status == APPLIED for m in server.got)
    assert "the Logfire SDK's console" in run.stderr and "mask_attributes" in run.stderr
    # The sampler is still the server's to set.
    assert "sampling_rate" in run.stdout.split("live ")[-1]


def test_without_an_endpoint_it_is_logfire_configure():
    child = textwrap.dedent("""
        import logfire, pydantic_opamp_otel
        pydantic_opamp_otel.configure_logfire(send_to_logfire=False, console=False)
        assert not pydantic_opamp_otel.AGENT.on
        assert logfire.DEFAULT_LOGFIRE_INSTANCE.config.send_to_logfire is False
    """)
    run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60, env=_child_env())
    assert run.returncode == 0, run.stderr


def test_a_logfire_sdk_samples_as_the_server_says(tmp_path, servers):
    """The live sampler is the Logfire SDK's head sampler: a ratio of 0 starts no more traces."""
    server, ingest = servers
    run = _run(server, ingest, tmp_path, policy(head_sampling={"ratio": 0.0}))
    assert run.returncode == 0 and "Traceback" not in run.stderr, run.stderr
    assert any(m.remote_config_status.status == APPLIED for m in server.got)
    assert ingest.names() == ["before"]
