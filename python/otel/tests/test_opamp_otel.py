# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT

import logging
import json
import os
import subprocess
import sys
import textwrap
import threading
import time
import uuid
import warnings
from datetime import timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import MappingProxyType

import pytest
from pydantic_opamp_client import Notification, opamp_pb2
from pydantic_opamp_client.sdk import SdkSettings, settings_pb2
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.sdk.trace.sampling import ALWAYS_OFF, ALWAYS_ON, ParentBased, TraceIdRatioBased

import pydantic_opamp_otel
from _policy import FILE, V1, config_map, full, off, overrides, overrides_in, policy
from kept import keep_applied
from pydantic_opamp_otel import Agent, Controls, Setup, disabled_instrumentations

APPLIED = opamp_pb2.RemoteConfigStatuses_APPLIED
FAILED = opamp_pb2.RemoteConfigStatuses_FAILED
LAYER = SdkSettings()


@pytest.fixture(autouse=True)
def _own_state_home(tmp_path_factory, monkeypatch):
    """The default state directory (and every child process's) under the test's own home."""
    home = tmp_path_factory.mktemp("state-home")
    monkeypatch.setenv("XDG_STATE_HOME", str(home))
    monkeypatch.setenv("LOCALAPPDATA", str(home))
    monkeypatch.setenv("HOME", str(home))


def settings(doc: str):
    """The settings as the layer hands them to the package where it built the SDK."""
    return LAYER.load(doc.encode(), pydantic_opamp_otel.sdk_host(["otlp"])).settings


def config(body: str | None) -> opamp_pb2.AgentRemoteConfig:
    return opamp_pb2.AgentRemoteConfig(config=config_map(body), config_hash=(body or "").encode())


def effective(agent: Agent) -> dict:
    """The controls the agent's effective config reports overridden."""
    return overrides(agent.effective)


def prepared(tmp_path, **extra) -> tuple[Agent, dict]:
    env = {"OTEL_OPAMP_ENDPOINT": "http://server/v1/opamp", "OTEL_OPAMP_STATE_DIR": str(tmp_path), **extra}
    agent = Agent()
    agent.prepare(env)
    return agent, env


def private(path) -> bool:
    """Whether the file is its user's only: a mode of 0600, or on Windows a list allowing the user alone."""
    if os.name == "nt":
        from pydantic_opamp_client import _windows

        return _windows.dacl(str(path)) == _windows.canonical(_windows.user_only())
    return path.stat().st_mode & 0o777 == 0o600


def test_the_sdks_sampler_until_a_rate_is_set_and_again_when_it_is_withdrawn():
    c = Controls()
    c.sampler.wrap(ALWAYS_OFF)
    assert c.sampler.current is ALWAYS_OFF
    c.apply(settings(policy(head_sampling={"ratio": 0.25})))
    assert c.sampler.current.get_description() == ParentBased(TraceIdRatioBased(0.25)).get_description()
    c.apply(settings(policy()))
    assert c.sampler.current is ALWAYS_OFF


def test_a_rate_set_before_the_sdk_builds_its_sampler_still_holds():
    c = Controls()
    c.apply(settings(policy(head_sampling={"ratio": 0})))
    c.sampler.wrap(ALWAYS_ON)
    assert c.sampler.current.get_description() == ParentBased(TraceIdRatioBased(0.0)).get_description()


def test_spans_stop_and_start_with_send_traces_and_a_rate_of_0_samples_nothing():
    c = Controls()
    out = InMemorySpanExporter()
    tracer = TracerProvider(sampler=c.sampler.wrap(ALWAYS_ON))
    tracer.add_span_processor(SimpleSpanProcessor(c.spans(out)))
    t = tracer.get_tracer("t")
    t.start_span("one").end()
    c.apply(settings(policy(signal_exports=off("traces"))))
    t.start_span("dropped").end()
    c.apply(settings(policy(head_sampling={"ratio": 0})))
    for _ in range(50):
        t.start_span("unsampled").end()
    c.apply(settings(policy()))
    t.start_span("two").end()
    assert [s.name for s in out.get_finished_spans()] == ["one", "two"]


def test_next_start_settings_become_the_instrumentations_switch():
    switch = policy(instrumentations={"disabled": ["requests", "flask"]})
    assert disabled_instrumentations(settings(switch), "urllib, requests") == "urllib,requests,flask"
    assert disabled_instrumentations(settings(policy(instrumentations={"all_disabled": True})), "x") == "*"
    assert disabled_instrumentations(settings(policy(instrumentations={"all_disabled": False})), "x") is None
    assert disabled_instrumentations(settings(policy()), "") is None


def _reachability(endpoint: str, service: str = "", uid: uuid.UUID | None = None) -> pydantic_opamp_otel.Reachability:
    """Reachability as the agent makes it: the endpoint and the hints as the core's Setup gives them."""
    s = Setup.from_env({"OTEL_OPAMP_ENDPOINT": endpoint, "OTEL_OPAMP_STATE_DIR": "none"})
    return pydantic_opamp_otel.Reachability(s.shown, service, uid, s.hints)


def test_settings_read_as_the_sdk_reads_its_own():
    def env(**more):
        return {"OTEL_OPAMP_ENDPOINT": "http://s", "OTEL_OPAMP_STATE_DIR": "none", **more}

    assert Setup.from_env(env(OTEL_OPAMP_HEADERS="Authorization=Bearer%20a%20b, x=1=2,broken")).headers \
        == {"Authorization": "Bearer a b", "x": "1=2"}
    assert [Setup.from_env(env(OTEL_OPAMP_HEARTBEAT_INTERVAL=v)).heartbeat_ms for v in ["1500", "500ms", "30s", "2m"]] \
        == [1500, 500, 30_000, 120_000]
    assert Setup.from_env({}) is None
    s = Setup.from_env({"OTEL_OPAMP_ENDPOINT": " ws://s/v1/opamp ", "OTEL_OPAMP_HEARTBEAT_INTERVAL": "1s"})
    assert (s.endpoint, s.state_dir, s.heartbeat_ms) == ("ws://s/v1/opamp", None, 1000)


def test_a_bad_heartbeat_interval_warns_and_is_the_default(server, caplog):
    # As under opentelemetry-instrument, where an exception would skip all auto-instrumentation.
    for bad in ["soon", "-5s", "1h"]:
        caplog.clear()
        with caplog.at_level(logging.WARNING, logger="pydantic_opamp_otel"):
            s = Setup.from_env({"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp", "OTEL_OPAMP_HEARTBEAT_INTERVAL": bad})
        assert s.heartbeat_ms == 0
        assert caplog.messages == [f'OpAMP: OTEL_OPAMP_HEARTBEAT_INTERVAL "{bad}" is not a duration (as 1500, '
                                   "500ms, 30s or 2m): using the default"]
    # Longer than a day: a day, the longest.
    caplog.clear()
    with caplog.at_level(logging.WARNING, logger="pydantic_opamp_otel"):
        s = Setup.from_env({"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp", "OTEL_OPAMP_HEARTBEAT_INTERVAL": str(2**53)})
    assert s.heartbeat_ms == 86_400_000
    assert caplog.messages == [f'OpAMP: OTEL_OPAMP_HEARTBEAT_INTERVAL "{2**53}" is more than a day: a day it is, the longest']
    # A bad percent escape in the headers leaves that header out, with a warning naming it.
    caplog.clear()
    with caplog.at_level(logging.WARNING, logger="pydantic_opamp_otel"):
        s = Setup.from_env({"OTEL_OPAMP_ENDPOINT": "http://s", "OTEL_OPAMP_STATE_DIR": "none",
                            "OTEL_OPAMP_HEADERS": "a=x%zzy,b=%,c=%e9,d=ok%21"})
    assert s.headers == {"d": "ok!"}
    assert caplog.messages == [f'OpAMP: OTEL_OPAMP_HEADERS: the value of "{k}" has a percent escape that can\'t be '
                               "decoded, so it is left out" for k in "abc"]
    # And the application is still managed, at the default.
    agent = Agent()
    try:
        agent.prepare({"OTEL_OPAMP_ENDPOINT": server.endpoint, "OTEL_OPAMP_HEARTBEAT_INTERVAL": "soon",
                       "OTEL_OPAMP_HEADERS": "x=%zz"})
        agent.start(Resource.create({"service.name": "checkout"}))
        server.wait(lambda: len(server.got) >= 1)
        assert agent.client is not None and agent.setup.heartbeat_ms == 0
    finally:
        agent.stop()


def test_live_settings_are_applied_at_once_and_reported_and_the_package_saves_nothing(tmp_path):
    agent, _ = prepared(tmp_path)
    doc = policy(head_sampling={"ratio": 0.5}, signal_exports=off("logs"))
    s = agent.remote_config(config(doc))
    assert s.status == APPLIED and s.last_remote_config_hash == doc.encode()
    assert not agent.controls.send_logs
    assert effective(agent) == {"head_sampling": {"ratio": 0.5}, "signal_exports": off("logs")}
    # The client keeps the config applied, in its own state: nothing of the package's own.
    assert not (tmp_path / "sdk-settings.json").exists()


def described(server, configured: bool, tmp_path) -> dict:
    """The policy attributes of the description an agent's first message carries: a string's
    text, an array's strings."""
    agent, _ = prepared(tmp_path, OTEL_OPAMP_ENDPOINT=server.endpoint)
    agent.configured = configured
    agent.start(Resource.create({"service.name": "checkout"}))
    try:
        server.wait(lambda: any(m.HasField("agent_description") and m.instance_uid == agent.uid.bytes
                                for m in server.got))
    finally:
        agent.stop()
    desc = next(m.agent_description for m in server.got
                if m.HasField("agent_description") and m.instance_uid == agent.uid.bytes)
    out = {}
    for kv in desc.non_identifying_attributes:
        if kv.key.startswith("logfire.opamp."):
            v = kv.value
            out[kv.key] = ([x.string_value for x in v.array_value.values] if v.WhichOneof("value") == "array_value"
                           else v.string_value)
    return out


def test_the_description_names_the_policy_formats_and_the_controls_this_sdk_carries_out(server, tmp_path):
    ours = described(server, True, tmp_path / "ours")
    assert ours["logfire.opamp.config_format"] == V1
    assert {"head_sampling", "signal_exports.traces", "sampling_rules", "debug", "app_settings",
            "instrumentations"} <= set(ours["logfire.opamp.supported_controls"])
    # Under another configurator the sampler and the gates are not this package's.
    other = described(server, False, tmp_path / "other")["logfire.opamp.supported_controls"]
    assert "head_sampling" not in other and "signal_exports.traces" not in other
    assert "app_settings" in other


def test_a_next_start_change_is_applied_where_the_client_keeps_state_and_refused_where_none_is(tmp_path):
    agent, _ = prepared(tmp_path)
    urllib = policy(instrumentations={"disabled": ["urllib"]})
    assert agent.remote_config(config(urllib)).status == APPLIED
    agent, _ = prepared("none")
    s = agent.remote_config(config(urllib))
    assert s.status == FAILED and "needs somewhere to keep state" in s.error_message


def test_the_settings_to_start_with_are_the_config_the_client_kept_applied_else_the_file_kept_before(tmp_path):
    # Kept before the client kept the config applied: the package's own file carries over.
    (tmp_path / "sdk-settings.json").write_text(policy(head_sampling={"ratio": 0.25}))
    agent, _ = prepared(tmp_path)
    assert agent.controls.sampler._rate == 0.25
    # Once the client keeps a config applied, that is where a start begins: the old file is ignored.
    keep_applied(tmp_path, policy(head_sampling={"ratio": 0.5}).encode())
    agent, _ = prepared(tmp_path)
    assert agent.controls.sampler._rate == 0.5 and effective(agent) == {"head_sampling": {"ratio": 0.5}}
    # A config applied with no policy: every setting the SDK's own, not the old file's.
    keep_applied(tmp_path, None)
    agent, _ = prepared(tmp_path)
    assert agent.controls.sampler._rate is None and effective(agent) == {}
    # A client state that doesn't read: the old file again.
    (tmp_path / "opamp-state").write_bytes(b"\xff\xff")
    agent, _ = prepared(tmp_path)
    assert agent.controls.sampler._rate == 0.25


def test_a_config_applied_through_the_server_is_where_the_next_start_begins(tmp_path):
    # The server hears APPLIED once the client has saved the config with its state: a start after
    # that comes back to it, with no file of the package's own.
    offer = config(policy(head_sampling={"ratio": 0.5}, signal_exports=off("logs")))
    server = _Server(reply=lambda msg: opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid, remote_config=offer)
                     if msg.HasField("agent_description") else None)
    applied = lambda m: m.HasField("remote_config_status") and m.remote_config_status.status == APPLIED  # noqa: E731
    agent = Agent()
    agent.configured = True
    try:
        agent.prepare({"OTEL_OPAMP_ENDPOINT": server.endpoint, "OTEL_OPAMP_STATE_DIR": str(tmp_path),
                       "OTEL_OPAMP_HEARTBEAT_INTERVAL": "1s"})
        agent.start(Resource.create({"service.name": "checkout"}))
        server.wait(lambda: any(applied(m) for m in server.got))
    finally:
        agent.stop()
        server.close()
    assert not (tmp_path / "sdk-settings.json").exists()
    again = Agent()
    again.prepare({"OTEL_OPAMP_ENDPOINT": server.endpoint, "OTEL_OPAMP_STATE_DIR": str(tmp_path)})
    assert again.controls.sampler._rate == 0.5 and not again.controls.send_logs
    assert effective(again) == {"head_sampling": {"ratio": 0.5}, "signal_exports": off("logs")}


def test_a_wrong_config_is_failed_and_changes_nothing(tmp_path):
    agent, _ = prepared(tmp_path)
    agent.remote_config(config(policy(signal_exports=off("traces"))))
    s = agent.remote_config(config(policy(head_sampling={"ratio": 2})))
    assert s.status == FAILED and s.error_message.startswith("head_sampling:")
    assert not agent.controls.send_traces
    assert effective(agent) == {"signal_exports": off("traces")}


def test_next_start_settings_wait_for_the_restart_then_switch_instrumentations_off(tmp_path):
    agent, _ = prepared(tmp_path)
    switch = policy(instrumentations={"disabled": ["urllib"]})
    assert agent.remote_config(config(switch)).status == APPLIED
    assert effective(agent) == {}
    keep_applied(tmp_path, switch.encode())  # as the client does, before APPLIED goes out
    _, env = prepared(tmp_path, OTEL_PYTHON_DISABLED_INSTRUMENTATIONS="requests")
    assert env["OTEL_PYTHON_DISABLED_INSTRUMENTATIONS"] == "requests,urllib"


def test_without_a_state_directory_a_next_start_change_is_failed():
    agent = Agent()
    agent.prepare({"OTEL_OPAMP_ENDPOINT": "http://server/v1/opamp"})
    s = agent.remote_config(config(policy(instrumentations={"all_disabled": True})))
    assert s.status == FAILED
    assert s.error_message == ("instrumentations: takes effect at the next start, which needs somewhere to keep "
                               "state")
    assert agent.remote_config(config(policy(head_sampling={"ratio": 0.5}))).status == APPLIED


def test_one_uid_names_the_instance_is_kept_and_the_metric_exporter_is_gated(tmp_path):
    first, env1 = prepared(tmp_path, OTEL_RESOURCE_ATTRIBUTES="deployment.environment.name=prod,service.instance.id=x")
    assert first.uid.version == 7
    assert env1["OTEL_METRICS_EXPORTER"] == "opamp"
    # The user's only (docs/state-file.md): a mode of 0600, or on Windows an access-control list.
    assert private(tmp_path / "instance-uid")
    second, _ = prepared(tmp_path, OTEL_METRICS_EXPORTER="console,otlp")
    assert second.uid == first.uid
    # Without state, a UUID the user gave is used.
    given = uuid.uuid4()
    a = Agent()
    a.prepare({"OTEL_OPAMP_ENDPOINT": "http://s", "OTEL_RESOURCE_ATTRIBUTES": f"service.instance.id={given}"})
    assert a.uid == given


def test_a_state_directory_it_cant_save_in_keeps_a_given_uuid(tmp_path):
    """Issue #218: with a state directory nothing can be saved in, no UID is kept, so a UUID
    service.instance.id names the instance at every start, as without a state directory; not a
    new UID each start."""
    (tmp_path / "file").write_text("")
    given = uuid.uuid4()
    for _ in range(2):
        agent, _ = prepared(tmp_path / "file" / "state", OTEL_RESOURCE_ATTRIBUTES=f"service.instance.id={given}")
        assert agent.uid == given
    # Nothing given: a new one each start (there is nowhere to keep it).
    agent, _ = prepared(tmp_path / "file" / "state")
    assert agent.uid.version == 7


def test_send_metrics_is_not_carried_out_when_no_metric_exporter_can_be_stopped(tmp_path):
    agent, _ = prepared(tmp_path, OTEL_METRICS_EXPORTER="console,prometheus")
    s = agent.remote_config(config(policy(signal_exports=off("metrics", "logs"))))
    # Refused whole: nothing could stop the metrics, so nothing changes.
    assert s.status == FAILED
    assert s.error_message == "signal_exports.metrics: this SDK cannot apply the requested override"
    assert agent.controls.send_metrics and agent.controls.send_logs
    assert effective(agent) == {}
    assert agent.remote_config(config(policy(signal_exports=off("logs")))).status == APPLIED
    assert agent.controls.send_metrics and not agent.controls.send_logs
    # Beside another, the OTLP exporter alone would stop: carried out by half is not carried out.
    for names, carried in [(["otlp"], True), (["console", "otlp"], False), (["otlp", "prometheus"], False),
                           (["none"], True), ([""], True), (["NONE"], True), (["console"], False),
                           (["otlp_proto_http"], False)]:
        assert ("send_metrics" in pydantic_opamp_otel.sdk_host(names).live) == carried, names
        # Declared from the same rule that gates them: carried out exactly where every exporter is gated.
        gated = pydantic_opamp_otel.gated_metric_exporters(names)
        assert carried == all(n in ("opamp", "") or n.lower() == "none" for n in gated), names


def test_without_an_endpoint_nothing_changes():
    env = {"OTEL_METRICS_EXPORTER": "otlp"}
    a = Agent()
    a.prepare(env)
    assert not a.on and env == {"OTEL_METRICS_EXPORTER": "otlp"}
    assert pydantic_opamp_otel.AGENT is not a


class _Recording:
    """An exporter that records what reaches it."""

    def __init__(self) -> None:
        self.batches = []

    def export(self, batch, *args, **kwargs):
        self.batches.append(batch)
        return "exported"

    def shutdown(self, *args, **kwargs):
        pass

    def force_flush(self, *args, **kwargs):
        return True


def test_logs_stop_and_start_with_send_logs():
    from opentelemetry.sdk._logs.export import LogRecordExportResult

    c = Controls()
    inner = _Recording()
    gated = c.logs(inner)
    assert gated.export(["a"]) == "exported"
    c.apply(settings(policy(signal_exports=off("logs"))))
    assert gated.export(["b"]) == LogRecordExportResult.SUCCESS
    assert inner.batches == [["a"]]


def test_send_logs_level_drops_records_below_it_live():
    from opentelemetry._logs import LogRecord, SeverityNumber
    from opentelemetry.sdk._logs import ReadableLogRecord
    from opentelemetry.sdk._logs.export import LogRecordExportResult

    def record(severity):
        return ReadableLogRecord(LogRecord(body="m", severity_number=severity), Resource.create({}))

    debug, info, warn, error = (record(SeverityNumber[n]) for n in ("DEBUG", "INFO", "WARN", "ERROR"))
    unspecified, unset = record(SeverityNumber.UNSPECIFIED), record(None)
    batch = [debug, info, warn, error, unspecified, unset]
    c = Controls()
    inner = _Recording()
    gated = c.logs(inner)
    c.apply(settings(policy(log_levels={"level": "warn"})))
    assert c.logs_min_severity == 13
    gated.export(batch)
    assert inner.batches[-1] == [warn, error, unspecified, unset]
    # A batch with nothing at or above the level doesn't reach the exporter.
    assert gated.export([debug, info]) == LogRecordExportResult.SUCCESS
    assert len(inner.batches) == 1
    c.apply(settings(policy(log_levels={"level": "warn"}, signal_exports=off("logs"))))
    assert gated.export(batch) == LogRecordExportResult.SUCCESS
    assert len(inner.batches) == 1
    c.apply(settings(policy()))
    assert c.logs_min_severity is None
    gated.export(batch)
    assert inner.batches[-1] == batch


def test_send_logs_level_is_not_declared_with_send_logs_when_another_configurator_built_the_sdk():
    live = list(pydantic_opamp_otel.sdk_host(["otlp"], configured=False).live)
    assert "send_logs" not in live and "send_logs_level" not in live
    assert "send_logs_level" in pydantic_opamp_otel.sdk_host(["otlp"]).live


def test_the_metric_exporter_is_the_otlp_one_the_protocol_picks_and_stops_with_send_metrics(monkeypatch):
    from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
    from opentelemetry.sdk.metrics.export import MetricExportResult

    monkeypatch.setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
    gated = pydantic_opamp_otel.GatedOtlpMetricExporter()
    assert isinstance(gated._to, OTLPMetricExporter)
    inner = _Recording()
    gated._to = inner
    pydantic_opamp_otel.AGENT.controls.apply(settings(policy(signal_exports=off("metrics"))))
    try:
        assert gated.export("m") == MetricExportResult.SUCCESS
        assert inner.batches == []
    finally:
        pydantic_opamp_otel.AGENT.controls.apply(settings(policy()))
    assert gated.export("m") == "exported"


def test_the_clients_own_requests_are_not_traced(tmp_path):
    # aiohttp (the client's HTTP library) instrumented before the client starts: every request it
    # made would be a span, were it not the agent's own.
    import threading
    import time
    from http.server import BaseHTTPRequestHandler, HTTPServer

    from opentelemetry import trace
    from opentelemetry.instrumentation.aiohttp_client import AioHttpClientInstrumentor
    from opentelemetry.sdk.resources import Resource

    seen = []

    class OpAMP(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            self.rfile.read(int(self.headers["Content-Length"]))
            seen.append(self.path)
            self.send_response(200)
            self.send_header("Content-Type", "application/x-protobuf")
            self.send_header("Content-Length", "0")
            self.end_headers()

        def log_message(self, *args):
            pass

    server = HTTPServer(("127.0.0.1", 0), OpAMP)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    out = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(out))
    trace.set_tracer_provider(provider)
    AioHttpClientInstrumentor().instrument()
    try:
        agent = Agent()
        agent.prepare({"OTEL_OPAMP_ENDPOINT": f"http://127.0.0.1:{server.server_port}/v1/opamp",
                       "OTEL_OPAMP_HEARTBEAT_INTERVAL": "1s"})
        agent.start(Resource.create({"service.name": "checkout"}))
        deadline = time.time() + 10
        while len(seen) < 3 and time.time() < deadline:
            time.sleep(0.1)
        agent.stop()
    finally:
        AioHttpClientInstrumentor().uninstrument()
        server.shutdown()
    assert len(seen) >= 3
    assert [s.name for s in out.get_finished_spans()] == []


class _Server:
    """An OpAMP server over plain HTTP: keeps every message, answers with `reply` (or nothing)."""

    def __init__(self, reply=None) -> None:
        self.got: list[opamp_pb2.AgentToServer] = []
        #: Each request's headers, names lowercased.
        self.headers: list[dict[str, str]] = []
        got, seen = self.got, self.headers

        class OpAMP(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                msg = opamp_pb2.AgentToServer.FromString(self.rfile.read(int(self.headers["Content-Length"])))
                got.append(msg)
                seen.append({k.lower(): v for k, v in self.headers.items()})
                answer = (reply and reply(msg)) or opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid)
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
        self.port = self._http.server_port
        self.endpoint = f"http://127.0.0.1:{self.port}/v1/opamp"

    def wait(self, cond, seconds: float = 10) -> None:
        deadline = time.time() + seconds
        while not cond() and time.time() < deadline:
            time.sleep(0.05)
        assert cond()

    def close(self) -> None:
        self._http.shutdown()
        self._http.server_close()


@pytest.fixture
def server():
    s = _Server()
    yield s
    s.close()


def test_a_remote_config_offered_at_once_is_answered_however_late_the_starting_thread_wakes(monkeypatch):
    # The first reply offers a config. The client hands the notification over on its own loop as
    # soon as it has started; the thread that started it may not have run again by then (a loaded
    # machine). The offer must still be carried out and answered, not dropped.
    offer = config(policy(signal_exports=off("traces")))
    server = _Server(reply=lambda msg: opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid, remote_config=offer)
                     if msg.HasField("agent_description") else None)

    def status(msg, st):
        return msg.HasField("remote_config_status") and msg.remote_config_status.status == st

    real = pydantic_opamp_otel.asyncio.run_coroutine_threadsafe

    class Late:
        """The starting thread's wait, woken only once the agent has told the server APPLYING:
        the notification is out by then."""

        def __init__(self, fut) -> None:
            self._fut = fut

        def result(self, timeout=None):
            out = self._fut.result(timeout)
            server.wait(lambda: any(status(m, opamp_pb2.RemoteConfigStatuses_APPLYING) for m in server.got))
            return out

    monkeypatch.setattr(pydantic_opamp_otel.asyncio, "run_coroutine_threadsafe", lambda coro, loop: Late(real(coro, loop)))
    agent = Agent()
    agent.configured = True  # as when the package's Configurator built the SDK
    try:
        agent.prepare({"OTEL_OPAMP_ENDPOINT": server.endpoint, "OTEL_OPAMP_HEARTBEAT_INTERVAL": "1s"})
        agent.start(Resource.create({"service.name": "checkout"}))
        server.wait(lambda: any(status(m, APPLIED) for m in server.got))
        assert not agent.controls.send_traces
    finally:
        monkeypatch.undo()
        agent.stop()
        server.close()


def test_the_goodbye_reaches_a_server_named_by_host_at_exit(server):
    # By the time atexit runs, the thread pool that resolves host names takes no more work: the
    # goodbye goes before it shuts down, and exit raises nothing either way.
    child = textwrap.dedent(f"""
        import time, pydantic_opamp_otel
        from opentelemetry.sdk.resources import Resource
        agent = pydantic_opamp_otel.AGENT
        agent.prepare({{"OTEL_OPAMP_ENDPOINT": "http://localhost:{server.port}/v1/opamp"}})
        agent.start(Resource.create({{"service.name": "checkout"}}))
        while not agent.client._x._session:
            time.sleep(0.05)
        time.sleep(0.5)
        agent.client._x._session.connector.clear_dns_cache()  # as once aiohttp's 10 s cache expires
    """)
    run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60)
    assert run.returncode == 0 and "Traceback" not in run.stderr, run.stderr
    assert server.got and server.got[-1].HasField("agent_disconnect")


def test_the_uid_reaches_the_sdk_not_the_environment_so_a_child_process_is_not_the_same_instance(monkeypatch):
    env = {"OTEL_OPAMP_ENDPOINT": "http://server/v1/opamp",
           "OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=prod"}
    agent = Agent()
    agent.prepare(env)
    assert env["OTEL_RESOURCE_ATTRIBUTES"] == "deployment.environment.name=prod"
    child = Agent()
    child.prepare(dict(env))  # a child process under opentelemetry-instrument, with the parent's environment
    assert child.uid != agent.uid
    # The configurator gives it to the SDK, over what the environment says.
    built = {}
    monkeypatch.setattr(pydantic_opamp_otel._OTelSDKConfigurator, "_configure", lambda self, **kw: built.update(kw))
    monkeypatch.setattr(pydantic_opamp_otel, "AGENT", agent)
    pydantic_opamp_otel.Configurator()._configure(resource_attributes={"a": "b", "service.instance.id": "mine"})
    assert built["resource_attributes"] == {"a": "b", "service.instance.id": str(agent.uid)}
    assert agent.configured


def test_a_forked_child_drops_the_parents_client_and_the_parent_carries_on(server, tmp_path):
    if not hasattr(os, "fork"):
        pytest.skip("no fork")
    agent, _ = prepared(tmp_path, OTEL_OPAMP_ENDPOINT=server.endpoint, OTEL_OPAMP_HEARTBEAT_INTERVAL="100ms")
    agent.start(Resource.create({"service.name": "checkout"}))
    server.wait(lambda: len(server.got) >= 1)
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", DeprecationWarning)  # forking with threads
        pid = os.fork()
    if pid == 0:
        ok = False
        try:
            began = time.monotonic()
            agent.stop()
            ok = agent.client is None and time.monotonic() - began < 1
        finally:
            os._exit(0 if ok else 1)
    _, status = os.waitpid(pid, 0)
    assert os.waitstatus_to_exitcode(status) == 0
    before = len(server.got)
    server.wait(lambda: len(server.got) > before)  # the parent still checks in
    agent.stop()
    assert [m.HasField("agent_disconnect") for m in server.got].count(True) == 1


def test_a_forked_worker_follows_the_live_settings_its_parent_is_given(server, tmp_path):
    """gunicorn's model: the parent is the instance, its forked workers serve. A setting the
    server sends the parent after the fork reaches every worker."""
    if not hasattr(os, "fork"):
        pytest.skip("no fork")
    agent, _ = prepared(tmp_path, OTEL_OPAMP_ENDPOINT=server.endpoint, OTEL_OPAMP_HEARTBEAT_INTERVAL="100ms")
    agent.configured = True  # the package's Configurator built the SDK
    agent.start(Resource.create({"service.name": "checkout"}))
    server.wait(lambda: len(server.got) >= 1)
    assert agent._relay is None  # nothing is made for a process that never forks
    agent.remote_config(config(policy(head_sampling={"ratio": 0.5})))
    ready_r, ready_w = os.pipe()
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", DeprecationWarning)  # forking with threads
        pid = os.fork()
    if pid == 0:
        ok = False
        try:
            c = agent.controls
            forked_with = (c.sampler._rate, c.send_traces)
            os.write(ready_w, b"x")
            deadline = time.monotonic() + 10
            while (c.sampler._rate, c.send_traces) != (0.25, False) and time.monotonic() < deadline:
                time.sleep(0.05)
            ok = forked_with == (0.5, True) and (c.sampler._rate, c.send_traces) == (0.25, False)
            ok = ok and agent.client is None  # a worker never talks to the server
        finally:
            os._exit(0 if ok else 1)
    os.read(ready_r, 1)
    relay = agent._relay
    assert relay is not None and relay.exists()
    agent.remote_config(config(policy(head_sampling={"ratio": 0.25}, signal_exports=off("traces"))))
    _, status = os.waitpid(pid, 0)
    assert os.waitstatus_to_exitcode(status) == 0
    agent.stop()
    assert not relay.parent.exists()  # the parent cleans up; its workers keep what's in force
    # One instance: the workers never said hello or goodbye.
    assert {m.instance_uid for m in server.got} == {agent.uid.bytes}


def test_a_worker_keeps_its_settings_when_the_relay_is_gone(tmp_path):
    agent = Agent()
    relay = tmp_path / "live-settings.pb"
    relay.write_bytes(settings_pb2.Settings(sampling_rate=0.1, send_logs=False).SerializeToString())
    follower = threading.Thread(target=agent._follow, args=(relay, 0.01), daemon=True)
    follower.start()
    deadline = time.monotonic() + 5
    while agent.controls.sampler._rate != 0.1 and time.monotonic() < deadline:
        time.sleep(0.01)
    assert (agent.controls.sampler._rate, agent.controls.send_logs) == (0.1, False)
    # A foreign file: read again, not applied. Put in place whole, as the parent puts its own
    # (save_state renames a new file over the old one). Written in place it is empty for a moment,
    # and an empty file is a message, every setting at its default: caught then (the follower
    # looks every 10 ms here), the worker took the defaults, and the rate was None.
    garbage = tmp_path / "garbage"
    garbage.write_bytes(b"\xff\xff not a message")
    os.replace(garbage, relay)
    time.sleep(0.1)
    assert agent.controls.sampler._rate == 0.1
    relay.unlink()
    follower.join(5)
    assert not follower.is_alive()
    assert agent.controls.sampler._rate == 0.1


def test_settings_another_configurator_would_not_carry_out_are_refused(server, tmp_path):
    agent, _ = prepared(tmp_path, OTEL_OPAMP_ENDPOINT=server.endpoint)
    agent.start(Resource.create({"service.name": "checkout"}))  # our Configurator never ran
    try:
        s = agent.remote_config(config(policy(head_sampling={"ratio": 0.5},
                                              signal_exports=off("traces", "metrics", "logs"))))
        metrics = agent.remote_config(config(policy(signal_exports=off("metrics"))))
    finally:
        agent.stop()
    assert s.status == FAILED
    assert s.error_message == "head_sampling: this SDK cannot apply the requested override"
    # The gated metric exporter is still the SDK's.
    assert metrics.status == APPLIED and effective(agent) == {"signal_exports": off("metrics")}
    ours, _ = prepared(tmp_path / "ours", OTEL_OPAMP_ENDPOINT=server.endpoint)
    ours.configured = True
    ours.start(Resource.create({"service.name": "checkout"}))
    try:
        ours.remote_config(config(policy(head_sampling={"ratio": 0.5}, signal_exports=off("traces"))))
    finally:
        ours.stop()
    assert effective(ours) == {"head_sampling": {"ratio": 0.5}, "signal_exports": off("traces")}


def test_with_a_configuration_file_the_instance_is_managed_but_no_exporter_is_ours(server, tmp_path, monkeypatch):
    monkeypatch.setenv("OTEL_CONFIG_FILE", str(tmp_path / "otel.yaml"))
    agent, _ = prepared(tmp_path, OTEL_OPAMP_ENDPOINT=server.endpoint)
    agent.start(Resource.create({"service.name": "checkout"}))  # what the Configurator does after the file's SDK
    agent.start(Resource.create({"service.name": "checkout"}))  # once only, should upstream's hook run too
    try:
        s = agent.remote_config(config(policy(head_sampling={"ratio": 0.5},
                                              signal_exports=off("traces", "metrics", "logs"),
                                              instrumentations={"disabled": ["requests"]})))
        switch = agent.remote_config(config(policy(instrumentations={"disabled": ["requests"]})))
    finally:
        agent.stop()
    assert s.status == FAILED
    assert s.error_message == "head_sampling: this SDK cannot apply the requested override"
    assert switch.status == APPLIED
    assert list(agent.host.live) == ["custom"]  # the application's own settings only
    assert list(agent.host.next_start) == ["deactivate_instrumentations", "deactivate_all_instrumentations"]
    assert effective(agent) == {}  # the switch is pending the next start, which reads it


def test_a_uid_the_server_assigns_is_kept_for_the_next_start(tmp_path):
    assigned = uuid.uuid4()

    def reply(msg):
        if msg.instance_uid != assigned.bytes:
            return opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid,
                                           agent_identification=opamp_pb2.AgentIdentification(
                                               new_instance_uid=assigned.bytes))
        return None

    server = _Server(reply)
    try:
        first, _ = prepared(tmp_path, OTEL_OPAMP_ENDPOINT=server.endpoint, OTEL_OPAMP_HEARTBEAT_INTERVAL="100ms")
        first.start(Resource.create({"service.name": "checkout"}))
        server.wait(lambda: (tmp_path / "instance-uid").read_text(encoding="utf-8") == str(assigned))
        first.stop()
        assert private(tmp_path / "instance-uid")
        second, _ = prepared(tmp_path, OTEL_OPAMP_ENDPOINT=server.endpoint)
        assert second.uid == assigned
        before = len(server.got)
        second.start(Resource.create({"service.name": "checkout"}))
        server.wait(lambda: len(server.got) > before)
        second.stop()
        assert {m.instance_uid for m in server.got[before:]} == {assigned.bytes}
    finally:
        server.close()


def test_the_instance_uid_changed_notification_is_saved(tmp_path):
    agent, _ = prepared(tmp_path)
    new = uuid.uuid4()
    agent._notified(Notification("instance_uid_changed", instance_uid=new.bytes))
    assert agent.uid == new and (tmp_path / "instance-uid").read_text(encoding="utf-8") == str(new)


def test_under_gevent_the_client_leaves_the_applications_own_event_loop_free(server):
    """Locust's model: gevent patches threading, so a thread is a greenlet on the one OS thread,
    and asyncio runs one loop there at a time. The client's loop must not be that one: an asyncio
    loop the application runs in a greenlet (locust-plugins' Playwright users) still runs, and the
    client still checks in."""
    pytest.importorskip("gevent")
    child = textwrap.dedent(f"""
        from gevent import monkey
        monkey.patch_all()
        import asyncio, gevent, pydantic_opamp_otel
        from opentelemetry.sdk.resources import Resource
        agent = pydantic_opamp_otel.AGENT
        agent.prepare({{"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:{server.port}/v1/opamp",
                        "OTEL_OPAMP_HEARTBEAT_INTERVAL": "100ms"}})
        agent.start(Resource.create({{"service.name": "load-generator"}}))
        gevent.sleep(0.5)
        loop = asyncio.new_event_loop()
        ran = []
        loop.call_soon(lambda: (ran.append(True), loop.stop()))
        g = gevent.spawn(loop.run_forever)
        g.join(timeout=5)
        assert g.successful() and ran, g.exception
        loop.close()  # (left to the collector, Windows' proactor loop fails closing under gevent)
        gevent.sleep(0.5)
        agent.stop()
    """)
    run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60)
    assert run.returncode == 0 and "Traceback" not in run.stderr, run.stderr
    assert len(server.got) >= 3
    assert server.got[-1].HasField("agent_disconnect")


def _child_env(**extra) -> dict:
    """The environment of a child process, with no OTEL_* of the test run's own."""
    return {**{k: v for k, v in os.environ.items() if not k.startswith("OTEL_")}, **extra}


def test_configure_sets_an_sdk_up_in_code_under_the_server(tmp_path):
    """An application that sets its SDK up in code, not under opentelemetry-instrument (Locust's
    gevent rules that out): one call builds the SDK the OTEL_* variables describe and puts it
    under the server, the live sampler and gated exporters included."""
    ready, answered = tmp_path / "ready", tmp_path / "answered"
    offer = config(policy(head_sampling={"ratio": 0.5}, signal_exports=off("traces", "metrics")))

    def reply(msg):
        if msg.remote_config_status.status == APPLIED:
            answered.touch()  # the child waits for this, so it can't stop before the answer goes out
        return opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid, remote_config=offer) if ready.exists() else None

    server = _Server(reply=reply)
    child = textwrap.dedent(f"""
        import pathlib, time, pydantic_opamp_otel
        from opentelemetry import metrics, trace
        # The server given in code, as an application with its settings in its own config would.
        pydantic_opamp_otel.configure(endpoint={server.endpoint!r}, heartbeat_interval=0.1)
        agent = pydantic_opamp_otel.AGENT
        tracer = trace.get_tracer("t")
        tracer.start_span("before").end()
        trace.get_tracer_provider().force_flush()
        pathlib.Path({str(ready)!r}).touch()
        deadline = time.monotonic() + 10
        while agent.controls.send_traces and time.monotonic() < deadline:
            time.sleep(0.05)
        while not pathlib.Path({str(answered)!r}).exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        tracer.start_span("after").end()
        trace.get_tracer_provider().force_flush()
        provider = trace.get_tracer_provider()
        assert provider.resource.attributes["service.instance.id"] == str(agent.uid)
        assert provider.sampler is agent.controls.sampler and agent.controls.sampler._rate == 0.5
        assert "deactivate_instrumentations" not in agent.host.next_start  # it instruments nothing
        (reader,) = metrics.get_meter_provider()._metric_readers
        assert isinstance(reader._exporter, pydantic_opamp_otel.GatedOtlpMetricExporter)
        print("uid", agent.uid)
    """)
    try:
        run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60,
                             env=_child_env(OTEL_SERVICE_NAME="load-generator", OTEL_TRACES_EXPORTER="console",
                                            OTEL_LOGS_EXPORTER="none",
                                            OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:9"))
    finally:
        server.close()
    assert run.returncode == 0, run.stderr
    assert '"name": "before"' in run.stdout and '"name": "after"' not in run.stdout
    uid = uuid.UUID(run.stdout.split("uid ")[-1].strip())
    assert {m.instance_uid for m in server.got} == {uid.bytes}
    assert any(m.remote_config_status.status == APPLIED for m in server.got)
    assert server.got[-1].HasField("agent_disconnect")


@pytest.mark.parametrize("given, variables, said", [
    # In code, all four, over variables that would each be noticed: another server, another
    # header, another directory, an interval too long to see.
    ({"headers": {"Authorization": "Bearer code"}, "state_dir": "{tmp}/code", "heartbeat_interval": "100ms"},
     {"OTEL_OPAMP_ENDPOINT": "http://127.0.0.1:9/v1/opamp", "OTEL_OPAMP_HEADERS": "Authorization=Bearer%20env,X-Env=1",
      "OTEL_OPAMP_STATE_DIR": "{tmp}/env", "OTEL_OPAMP_HEARTBEAT_INTERVAL": "2m"}, []),
    # None of them usable but the endpoint: warned about, and the application runs, managed.
    ({"headers": "Authorization=Bearer code", "state_dir": 5, "heartbeat_interval": -1},
     {"OTEL_OPAMP_HEADERS": "X-Env=1"},
     ["headers should map names", "state_dir should be a path", "heartbeat_interval (-1000 ms) is no interval"]),
])
def test_configure_takes_the_server_in_code_end_to_end(tmp_path, given, variables, said):
    server = _Server()
    given = {k: v.format(tmp=tmp_path) if isinstance(v, str) else v for k, v in given.items()}
    child = textwrap.dedent(f"""
        import time, pydantic_opamp_otel
        pydantic_opamp_otel.configure(endpoint={server.endpoint!r}, **{given!r})
        time.sleep(1.5)
        print("state_dir", pydantic_opamp_otel.AGENT.setup.state_dir)
    """)
    try:
        run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60,
                             env=_child_env(**{k: v.format(tmp=tmp_path) for k, v in variables.items()},
                                            OTEL_TRACES_EXPORTER="none", OTEL_METRICS_EXPORTER="none",
                                            OTEL_LOGS_EXPORTER="none"))
    finally:
        server.close()
    assert run.returncode == 0 and "Traceback" not in run.stderr, run.stderr
    for words in said:
        assert words in run.stderr
    assert server.got and server.got[-1].HasField("agent_disconnect")
    # Only the headers given in code, and nothing of the variable's.
    assert not any("x-env" in h for h in server.headers)
    assert {h.get("authorization") for h in server.headers} == {"Bearer code" if said == [] else None}
    if said == []:
        assert (tmp_path / "code" / "instance-uid").exists() and not (tmp_path / "env").exists()
        assert len(server.got) >= 4  # every 100 ms, not every 2 minutes
    else:
        assert "state_dir None" not in run.stdout  # the default one, as without OTEL_OPAMP_STATE_DIR


def test_configure_without_an_endpoint_is_the_sdks_own_setup():
    child = textwrap.dedent("""
        import os, pydantic_opamp_otel
        from opentelemetry import metrics, trace
        from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
        from opentelemetry.sdk.trace.sampling import ALWAYS_OFF
        pydantic_opamp_otel.configure(sampler=ALWAYS_OFF, resource_attributes={"a": "b"})
        provider = trace.get_tracer_provider()
        assert provider.sampler is ALWAYS_OFF
        assert provider.resource.attributes["a"] == "b"
        (spans,) = provider._active_span_processor._span_processors
        assert type(spans._batch_processor._exporter) is OTLPSpanExporter
        (reader,) = metrics.get_meter_provider()._metric_readers
        assert type(reader._exporter) is OTLPMetricExporter
        assert os.environ["OTEL_METRICS_EXPORTER"] == "otlp" and pydantic_opamp_otel.AGENT.client is None
    """)
    run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60,
                         env=_child_env(OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:9",
                                        OTEL_METRIC_EXPORT_TIMEOUT="1"))
    assert run.returncode == 0, run.stderr


def test_in_code_the_instrumentation_switches_are_refused(tmp_path):
    # The application instruments itself: nothing reads OTEL_PYTHON_DISABLED_INSTRUMENTATIONS.
    keep_applied(tmp_path, policy(instrumentations={"disabled": ["urllib"]}).encode())
    env = {"OTEL_OPAMP_ENDPOINT": "http://server/v1/opamp", "OTEL_OPAMP_STATE_DIR": str(tmp_path)}
    agent = Agent()
    agent.prepare(env, auto_instrumented=False)
    assert "OTEL_PYTHON_DISABLED_INSTRUMENTATIONS" not in env
    s = agent.remote_config(config(policy(instrumentations={"all_disabled": True}, signal_exports=off("logs"))))
    assert s.status == FAILED
    assert s.error_message == "instrumentations: all_disabled: this SDK cannot apply the requested override"
    assert effective(agent) == {}
    host = pydantic_opamp_otel.sdk_host(["otlp"], configured=False, auto_instrumented=False)
    assert (list(host.live), list(host.next_start)) == (["custom", "send_metrics"], [])


def test_processors_and_exporters_passed_in_code_export_past_the_gates_so_their_settings_arent_carried_out(tmp_path):
    """configure()'s own span processor and a metric exporter named in code export past the
    package's gates: a policy that overrides send_traces, send_metrics or redaction (which needs
    every span) is refused, not answered as done while the spans and metrics keep going."""
    answered = tmp_path / "answered"
    offers = [config(policy(signal_exports=off("traces", "metrics", "logs"), attribute_redaction={"drop": ["x"]})),
              config(policy(signal_exports=off("logs")))]
    statuses: list[int] = []

    def reply(msg):
        if msg.HasField("remote_config_status") and msg.remote_config_status.last_remote_config_hash:
            statuses.append(msg.remote_config_status.status)
            if msg.remote_config_status.status == APPLIED:
                answered.touch()
            return None
        # The refused policy first, then one the SDK carries out.
        offer = offers[1] if FAILED in statuses else offers[0]
        return opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid, remote_config=offer)

    server = _Server(reply=reply)
    child = textwrap.dedent(f"""
        import json, pathlib, time, pydantic_opamp_otel
        from opentelemetry import trace
        from opentelemetry.sdk.trace.export import ConsoleSpanExporter, SimpleSpanProcessor
        pydantic_opamp_otel.configure(endpoint={server.endpoint!r}, heartbeat_interval=0.1,
                             span_processors=[SimpleSpanProcessor(ConsoleSpanExporter())],
                             metric_exporter_names=["console"])
        agent = pydantic_opamp_otel.AGENT
        deadline = time.monotonic() + 10
        while not pathlib.Path({str(answered)!r}).exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        trace.get_tracer("t").start_span("after").end()
        assert agent.controls.send_traces and agent.controls.send_metrics and not agent.controls.send_logs
        print("effective", agent.effective.config_map.config_map["logfire-sdk-policy.json"].body.decode())
    """)
    try:
        run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60,
                             env=_child_env(OTEL_TRACES_EXPORTER="none", OTEL_METRICS_EXPORTER="none",
                                            OTEL_LOGS_EXPORTER="otlp", OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:9"))
    finally:
        server.close()
    assert run.returncode == 0, run.stderr
    assert '"name": "after"' in run.stdout  # the application's own processor still exports
    assert json.loads(run.stdout.split("effective ")[-1]) == full(policy(signal_exports=off("logs")))
    assert statuses.index(FAILED) < statuses.index(APPLIED)
    assert "send_traces" in run.stderr  # warned at start


def test_a_processor_added_once_the_sdk_is_built_sets_its_gates_settings_aside_and_says_so(tmp_path):
    """A span or log record processor the application adds to the SDK after the configurator built
    it exports past the gates, as one passed in code does. The overrides in force through those gates
    are then set aside: the effective config reports them local, and is reported again, rather than
    leaving them in force while the new processor goes on exporting (#431)."""
    answered = tmp_path / "answered"
    offer = config(policy(head_sampling={"ratio": 1}, signal_exports=off("traces", "logs"),
                          attribute_redaction={"drop": ["x"]}))

    def reply(msg):
        if msg.HasField("remote_config_status") and msg.remote_config_status.status == APPLIED:
            answered.touch()
        if msg.HasField("remote_config_status") and msg.remote_config_status.last_remote_config_hash:
            return None
        return opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid, remote_config=offer)

    server = _Server(reply=reply)
    child = textwrap.dedent(f"""
        import json, pathlib, time, pydantic_opamp_otel
        from opentelemetry import _logs, trace
        from opentelemetry.sdk._logs.export import BatchLogRecordProcessor, ConsoleLogExporter
        from opentelemetry.sdk.trace.export import ConsoleSpanExporter, SimpleSpanProcessor
        pydantic_opamp_otel.configure(endpoint={server.endpoint!r}, heartbeat_interval=0.1)
        agent = pydantic_opamp_otel.AGENT
        deadline = time.monotonic() + 10
        while not pathlib.Path({str(answered)!r}).exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        def report(when):
            print(when, agent.effective.config_map.config_map[{FILE!r}].body.decode())
        report("before")
        trace.get_tracer_provider().add_span_processor(span_processor=SimpleSpanProcessor(ConsoleSpanExporter()))
        report("spans")
        _logs.get_logger_provider().add_log_record_processor(log_record_processor=BatchLogRecordProcessor(ConsoleLogExporter()))
        report("logs")
        trace.get_tracer("t").start_span("after").end()
        assert agent.controls.send_traces and agent.controls.send_logs
        assert agent.controls.sampler._rate == 1
        time.sleep(0.5)  # the next heartbeat carries the effective config
    """)
    try:
        run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60,
                             env=_child_env(OTEL_TRACES_EXPORTER="none", OTEL_METRICS_EXPORTER="none",
                                            OTEL_LOGS_EXPORTER="none"))
    finally:
        server.close()
    assert run.returncode == 0, run.stderr
    said = {line.split(" ", 1)[0]: overrides_in(json.loads(line.split(" ", 1)[1])) for line in run.stdout.splitlines()
            if line.split(" ", 1)[0] in ("before", "spans", "logs")}
    assert said["before"] == {"head_sampling": {"ratio": 1}, "signal_exports": off("traces", "logs"),
                              "attribute_redaction": {"drop": ["x"]}}
    # The span processor: the traces' export, and redaction, which needs every span, set aside.
    assert said["spans"] == {"head_sampling": {"ratio": 1}, "signal_exports": off("logs")}
    assert said["logs"] == {"head_sampling": {"ratio": 1}}
    assert '"name": "after"' in run.stdout  # the application's own processor exports
    assert "past this package's gate: send_traces" in run.stderr
    # The server is told: the last effective config it got reports them local.
    last = [m.effective_config for m in server.got if m.HasField("effective_config")][-1]
    assert overrides(last) == {"head_sampling": {"ratio": 1}}


def test_a_metric_exporter_beside_otlp_keeps_send_metrics_from_being_answered_done(tmp_path):
    # OTLP beside prometheus. Only the OTLP one would stop, so neither does.
    agent, env = prepared(tmp_path, OTEL_METRICS_EXPORTER="otlp,prometheus")
    assert env["OTEL_METRICS_EXPORTER"] == "opamp,prometheus"
    assert "send_metrics" not in agent.host.live
    s = agent.remote_config(config(policy(signal_exports=off("metrics", "logs"))))
    assert s.status == FAILED and agent.controls.send_metrics and agent.controls.send_logs
    assert s.error_message == "signal_exports.metrics: this SDK cannot apply the requested override"
    assert effective(agent) == {}


def test_a_state_directory_that_cant_be_written_is_the_clients_to_keep_trying(tmp_path):
    # The package saves nothing: the client keeps the config applied with its state, and until
    # that save lands the server hears APPLYING, not APPLIED (the client's own tests). The same
    # in every language.
    (tmp_path / "file").write_text("")
    agent, _ = prepared(tmp_path / "file" / "state")
    s = agent.remote_config(config(policy(head_sampling={"ratio": 0.5})))
    assert s.status == APPLIED and agent.controls.sampler._rate == 0.5


def _released(setup: Setup | None) -> None:
    """The process that held the default state directory ends."""
    if setup is not None and setup.state_lock is not None:
        os.close(setup.state_lock)


# The server given in code (configure()'s arguments, through Setup.from_env), against the
# OTEL_OPAMP_* variables. DEFAULT_DIR: the user's own default state directory.
DEFAULT_DIR = object()
OPTIONS = {
    # option: (its variable, a value in code, a value of the variable, read from code, read from the variable,
    #          the default)
    "endpoint": ("OTEL_OPAMP_ENDPOINT", " https://code/v1/opamp ", "http://env/v1/opamp",
                 "https://code/v1/opamp", "http://env/v1/opamp", None),
    # In code the values are sent as given, not percent-decoded as the variable's are.
    "headers": ("OTEL_OPAMP_HEADERS", {"Authorization": "Bearer code%20k"}, "Authorization=Bearer%20env,X-Env=1",
                {"Authorization": "Bearer code%20k"}, {"Authorization": "Bearer env", "X-Env": "1"}, {}),
    "state_dir": ("OTEL_OPAMP_STATE_DIR", "/srv/code/opamp", "/srv/env/opamp",
                  Path("/srv/code/opamp"), Path("/srv/env/opamp"), DEFAULT_DIR),
    "heartbeat_interval": ("OTEL_OPAMP_HEARTBEAT_INTERVAL", timedelta(seconds=2), "5s", 2000, 5000, 0),
}


def _from_code(tmp_path, option: str, given: dict, variables: dict):
    """Setup.from_env with `option` as given in code (if at all) and `variables`; the other options in
    code are fixed (an endpoint, no state directory), so only this one varies. (setup, what it read)."""
    env = {"XDG_STATE_HOME": str(tmp_path), "HOME": str(tmp_path), "LOCALAPPDATA": str(tmp_path), **variables}
    kwargs = {"endpoint": "http://base/v1/opamp", "state_dir": "none"}
    kwargs.pop(option, None)
    setup = Setup.from_env(env, **kwargs, **given)
    read = None if setup is None else {"endpoint": setup.endpoint, "headers": setup.headers,
                                       "state_dir": setup.state_dir, "heartbeat_interval": setup.heartbeat_ms}[option]
    return setup, read


def _expect(tmp_path, read, expected) -> None:
    if expected is DEFAULT_DIR:
        # Under the base directory each OS has (docs/default-state-dir.md), all set to tmp_path.
        base = tmp_path / "Library" / "Application Support" if sys.platform == "darwin" else tmp_path
        assert read is not None and read.is_relative_to(base / "opamp")
    else:
        assert read == expected


@pytest.mark.parametrize("given", ["code", "variable", "both", "neither"])
@pytest.mark.parametrize("option", OPTIONS)
def test_each_option_in_code_wins_over_its_variable_and_either_over_the_default(tmp_path, caplog, option, given):
    variable, in_code, as_variable, from_code, from_variable, default = OPTIONS[option]
    with caplog.at_level(logging.WARNING, logger="pydantic_opamp_otel"):
        setup, read = _from_code(tmp_path, option, {option: in_code} if given in ("code", "both") else {},
                                 {variable: as_variable} if given in ("variable", "both") else {})
    try:
        _expect(tmp_path, read, {"code": from_code, "both": from_code, "variable": from_variable,
                                 "neither": default}[given])
    finally:
        _released(setup)
    assert caplog.messages == []


@pytest.mark.parametrize("option, in_code, expected", [
    ("endpoint", "wss://code/v1/opamp", "wss://code/v1/opamp"),
    ("endpoint", "HTTPS://code/v1/opamp", "HTTPS://code/v1/opamp"),
    # In place of the variable's, not merged with it: none at all, too.
    ("headers", {}, {}),
    ("headers", MappingProxyType({"a": "1", "b": "x=y,z"}), {"a": "1", "b": "x=y,z"}),
    ("state_dir", Path("/srv/code/opamp"), Path("/srv/code/opamp")),
    ("state_dir", " /srv/code/opamp ", Path("/srv/code/opamp")),
    # "none" wins over the variable too: no state directory, not even the default one.
    ("state_dir", "none", None),
    ("state_dir", " NONE ", None),
    ("heartbeat_interval", 2, 2000),
    ("heartbeat_interval", 0.5, 500),
    ("heartbeat_interval", 0.0001, 1),  # rounded, but never to 0 (the default)
    ("heartbeat_interval", timedelta(minutes=1, milliseconds=500), 60_500),
    ("heartbeat_interval", "2m", 120_000),
    ("heartbeat_interval", " 30s ", 30_000),
    ("heartbeat_interval", "250ms", 250),
    ("heartbeat_interval", "1500", 1500),  # milliseconds, as the variable
])
def test_the_forms_each_option_takes_in_code(tmp_path, caplog, option, in_code, expected):
    variable, _, as_variable, *_ = OPTIONS[option]
    with caplog.at_level(logging.WARNING, logger="pydantic_opamp_otel"):
        setup, read = _from_code(tmp_path, option, {option: in_code}, {variable: as_variable})
    _released(setup)
    assert read == expected
    assert caplog.messages == []


@pytest.mark.parametrize("blank", ["", " ", "\t\n"])
@pytest.mark.parametrize("option", ["endpoint", "state_dir", "heartbeat_interval"])
def test_blank_in_code_is_not_given(tmp_path, caplog, option, blank):
    variable, _, as_variable, _, from_variable, default = OPTIONS[option]
    with caplog.at_level(logging.WARNING, logger="pydantic_opamp_otel"):
        setup, read = _from_code(tmp_path, option, {option: blank}, {variable: as_variable})
        _released(setup)
        assert read == from_variable  # the variable's
        setup, read = _from_code(tmp_path, option, {option: blank}, {})
        try:
            _expect(tmp_path, read, default)  # or the default
        finally:
            _released(setup)
    assert caplog.messages == []


@pytest.mark.parametrize("option, in_code, expected, warning", [
    # Not a string: not managed, rather than managed somewhere the application didn't mean.
    ("endpoint", 4320, None, "OpAMP: endpoint should be a string, not int: this SDK is not managed"),
    ("endpoint", b"http://code/v1/opamp", None,
     "OpAMP: endpoint should be a string, not bytes: this SDK is not managed"),
    # A string it can't use is still the endpoint (as the variable): the client says it can't reach it.
    ("endpoint", "ftp://code/v1/opamp", "ftp://code/v1/opamp",
     'OpAMP: the endpoint given to configure() "ftp://code/v1/opamp" is not http(s):// or ws(s)://, so the '
     "client can't reach it"),
    ("endpoint", "code:4320/v1/opamp", "http://code:4320/v1/opamp",
     ('OpAMP: the endpoint given to configure() "code:4320/v1/opamp" has no scheme, so it is taken as http://; name it in full, as '
      "https://host/v1/opamp or wss://host/v1/opamp")),
    *[("headers", bad, {}, "OpAMP: headers should map names to values, all strings: none are sent")
      for bad in ["Authorization=Bearer k", [("Authorization", "Bearer k")], {"Authorization": b"Bearer k"},
                  {"Authorization": None}, {"Retries": 3}, {1: "Bearer k"}]],
    ("state_dir", 4320, DEFAULT_DIR, "OpAMP: state_dir should be a path, not int: using the default"),
    ("state_dir", b"/srv/code", DEFAULT_DIR, "OpAMP: state_dir should be a path, not bytes: using the default"),
    *[("heartbeat_interval", bad, 0, (f"OpAMP: heartbeat_interval {bad!r} is not a positive duration: using the "
                                      "default"))
      for bad in [float("nan"), float("inf"), True, [5], b"5s", {"seconds": 5}]],
    *[("heartbeat_interval", bad, 0, f"OpAMP: heartbeat_interval ({ms} ms) is no interval: using the default")
      for bad, ms in [(0, 0), (-1, -1000), (0.0, 0), (timedelta(0), 0), (timedelta(days=-1), -86_400_000)]],
    *[("heartbeat_interval", bad, 0, (f'OpAMP: heartbeat_interval "{bad.strip()}" is not a duration (as 1500, '
                                      "500ms, 30s or 2m): using the default"))
      for bad in ["soon", "-5s", "1.5s", "1h", " 30 seconds "]],
])
def test_a_value_in_code_it_cannot_use_warns_and_is_the_default_never_raising(tmp_path, caplog, option, in_code,
                                                                                expected, warning):
    # With the variable set too: the value in code displaced it, as a bad variable is the default.
    variable, _, as_variable, *_ = OPTIONS[option]
    with caplog.at_level(logging.WARNING, logger="pydantic_opamp_otel"):
        setup, read = _from_code(tmp_path, option, {option: in_code}, {variable: as_variable})
    try:
        _expect(tmp_path, read, expected)
    finally:
        _released(setup)
    assert caplog.messages == [warning]
    assert "Bearer" not in caplog.text  # a header's value may be a secret


def test_by_default_an_instance_keeps_its_uid_across_restarts(tmp_path):
    env = {"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp", "OTEL_SERVICE_NAME": "checkout", "XDG_STATE_HOME": str(tmp_path),
           "HOME": str(tmp_path), "LOCALAPPDATA": str(tmp_path)}
    first = Agent()
    first.prepare(dict(env))
    assert first.setup.state_dir is not None and first.setup.state_dir.parent.name.startswith("checkout-")
    # Another process of it, running at once: a slot of its own, so an identity of its own.
    other = Agent()
    other.prepare(dict(env))
    assert other.setup.state_dir != first.setup.state_dir and other.uid != first.uid
    _released(first.setup)
    _released(other.setup)
    again = Agent()
    again.prepare(dict(env))
    assert again.setup.state_dir == first.setup.state_dir and again.uid == first.uid
    _released(again.setup)
    # Another service is another instance.
    elsewhere = Agent()
    elsewhere.prepare({**env, "OTEL_SERVICE_NAME": "billing"})
    assert elsewhere.uid != first.uid
    _released(elsewhere.setup)


def test_a_service_instance_id_that_isnt_a_uuid_keeps_the_default_state_directory(tmp_path):
    # It can't be the instance UID, so it is replaced by one: kept in the default directory, the
    # same at every start, rather than a new one each time.
    env = {"OTEL_OPAMP_ENDPOINT": "http://s", "XDG_STATE_HOME": str(tmp_path), "HOME": str(tmp_path),
           "LOCALAPPDATA": str(tmp_path), "OTEL_SERVICE_NAME": "web", "OTEL_RESOURCE_ATTRIBUTES": "service.instance.id=web-1"}
    uids = []
    for _ in range(2):
        a = Agent()
        a.prepare(dict(env))
        assert a.setup.state_dir is not None and a.setup.state_dir.is_relative_to(tmp_path)
        uids.append(a.uid)
        _released(a.setup)  # as the process exiting would
    assert uids[0] == uids[1]


def test_the_default_state_directory_gives_way(tmp_path):
    base = {"OTEL_OPAMP_ENDPOINT": "http://s", "XDG_STATE_HOME": str(tmp_path), "HOME": str(tmp_path),
            "LOCALAPPDATA": str(tmp_path)}
    # Turned off, or a service.instance.id the application chose (a UUID, so the instance UID): none.
    assert Setup.from_env({**base, "OTEL_OPAMP_STATE_DIR": "none"}).state_dir is None
    chosen = f"service.instance.id={uuid.uuid4()}"
    assert Setup.from_env({**base, "OTEL_RESOURCE_ATTRIBUTES": chosen}).state_dir is None
    # Named: that one, unlocked as before.
    named = Setup.from_env({**base, "OTEL_OPAMP_STATE_DIR": str(tmp_path / "mine")})
    assert named.state_dir == tmp_path / "mine" and named.state_lock is None
    # Nowhere to keep it: none, and no error.
    assert Setup.from_env({"OTEL_OPAMP_ENDPOINT": "http://s"}).state_dir is None
    (tmp_path / "file").write_text("")
    assert Setup.from_env({"OTEL_OPAMP_ENDPOINT": "http://s", "XDG_STATE_HOME": str(tmp_path / "file"),
                           "HOME": str(tmp_path / "file"), "LOCALAPPDATA": str(tmp_path / "file")}).state_dir is None
    # Every slot taken: none.
    held = [Setup.from_env(base) for _ in range(16)]
    assert all(h.state_dir is not None for h in held)
    assert Setup.from_env(base).state_dir is None
    for h in held:
        _released(h)


def test_connecting_failing_and_recovering_are_each_said_once(caplog):
    caplog.set_level(logging.DEBUG, logger="pydantic_opamp_otel")
    uid = uuid.UUID(int=7)
    r = _reachability("https://user:secret@opamp.example.com/v1/opamp?token=x", "checkout", uid)
    r.notified(Notification("connect_failed", status=401, detail="HTTP 401"))
    r.notified(Notification("connect_failed", status=401, detail="HTTP 401"))
    r.notified(Notification("connected"))
    r.notified(Notification("connected"))
    r.notified(Notification("connect_failed", detail="connection refused"))
    r.notified(Notification("connected"))
    said = [(rec.levelno, rec.getMessage()) for rec in caplog.records if rec.levelno >= logging.INFO]
    assert said == [
        (logging.WARNING, "OpAMP: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused "
                          "this agent's credentials: check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). "
                          "Retrying in the background; the SDK keeps its current settings meanwhile"),
        (logging.INFO, f"OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, "
                       f"instance {uid}"),
        (logging.WARNING, "OpAMP: can't reach https://opamp.example.com/v1/opamp (connection refused): check "
                          "OTEL_OPAMP_ENDPOINT and that the server is up. Retrying in the background; the SDK keeps "
                          "its current settings meanwhile"),
        (logging.INFO, "OpAMP: connected to https://opamp.example.com/v1/opamp again"),
    ]
    assert "secret" not in caplog.text and "token=x" not in caplog.text


def test_what_is_said_names_the_endpoint_in_use(caplog):
    """After the server moved the agent to another endpoint, what it says names the one the client
    uses now (logged without credentials), not the one configured."""
    caplog.set_level(logging.INFO, logger="pydantic_opamp_otel")
    in_use: list[str | None] = [None]  # none yet: the configured one
    r = _reachability("https://a.example.com/v1/opamp")
    r.in_use = lambda: in_use[0]
    r.notified(Notification("connected"))
    in_use[0] = "https://user:secret@b.example.com/v1/opamp?token=x"
    r.notified(Notification("connect_failed", detail="connection refused"))
    r.notified(Notification("connected"))
    said = [rec.getMessage() for rec in caplog.records if rec.levelno >= logging.INFO]
    assert said[0].startswith("OpAMP: connected to https://a.example.com/v1/opamp;")
    assert said[1].startswith("OpAMP: can't reach https://b.example.com/v1/opamp (connection refused)")
    assert said[2:] == ["OpAMP: connected to https://b.example.com/v1/opamp again"]
    assert "secret" not in caplog.text and "token=x" not in caplog.text


@pytest.mark.parametrize("given, endpoint_from, headers_from", [
    ({}, "OTEL_OPAMP_ENDPOINT", "OTEL_OPAMP_HEADERS"),
    # Given in code, each wins over its variable, and the warning names the argument.
    ({"endpoint": "http://s/v1/opamp", "headers": {"Authorization": "Bearer k"}},
     "the endpoint given to configure()", "the headers given to configure()"),
    ({"endpoint": "http://s/v1/opamp"}, "the endpoint given to configure()", "OTEL_OPAMP_HEADERS"),
    ({"headers": {}}, "OTEL_OPAMP_ENDPOINT", "the headers given to configure()"),
])
def test_the_warning_names_what_gave_the_server(caplog, given, endpoint_from, headers_from):
    setup = Setup.from_env({"OTEL_OPAMP_ENDPOINT": "http://other/v1/opamp", "OTEL_OPAMP_HEADERS": "a=b",
                            "OTEL_OPAMP_STATE_DIR": "none"}, **given)
    assert setup is not None
    r = pydantic_opamp_otel.Reachability(setup.shown, "", None, setup.hints)
    caplog.clear()
    for status in (401, 404, None):
        r.notified(Notification("connect_failed", status=status))
        r.notified(Notification("connected"))
    said = [rec.getMessage() for rec in caplog.records if rec.levelno == logging.WARNING]
    assert len(said) == 3
    assert f"check {headers_from} (Authorization=Bearer <token>)" in said[0]
    assert f"check {endpoint_from} (it usually ends in /v1/opamp)" in said[1]
    assert f"check {endpoint_from} and that the server is up" in said[2]


@pytest.mark.parametrize("endpoint, logged", [
    ("https://opamp.example.com:notaport/v1/opamp", "https://opamp.example.com:notaport/v1/opamp"),
    ("https://[::1/v1/opamp?k=v", "https://[::1/v1/opamp"),
    ("wss://k:s@[::1]:4320/v1/opamp#x", "wss://[::1]:4320/v1/opamp"),
])
def test_a_malformed_endpoint_is_logged_not_raised(endpoint, logged):
    assert pydantic_opamp_otel.shown_endpoint(endpoint, "?") == logged


def test_an_endpoint_without_a_scheme_or_with_one_the_client_cannot_speak_is_warned_about(caplog):
    setup = pydantic_opamp_otel.Setup.from_env({"OTEL_OPAMP_ENDPOINT": "opamp.example.com:4320/v1/opamp"})
    assert setup is not None
    assert "has no scheme" in caplog.text
    # Taken as http://, as the warning says: the client refuses an endpoint without a scheme.
    assert setup.endpoint == "http://opamp.example.com:4320/v1/opamp"
    # A scheme the client doesn't speak: warned about at start, not only at each failed connect.
    caplog.clear()
    assert pydantic_opamp_otel.Setup.from_env({"OTEL_OPAMP_ENDPOINT": "ftp://u:p@opamp.example.com/v1/opamp?k=v"}) is not None
    assert caplog.messages == ['OpAMP: OTEL_OPAMP_ENDPOINT "ftp://opamp.example.com/v1/opamp" is not http(s):// or '
                               "ws(s)://, so the client can't reach it"]


_ROOT_HANDLERS = {
    # logging auto-instrumentation: the OpenTelemetry handler alone on the root logger
    "otel": "from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler\n"
            "logging.getLogger().addHandler(LoggingHandler(logger_provider=LoggerProvider()))",
    "none": "",
    "stderr": "logging.basicConfig(format='app: %(message)s')",
    "otel+stderr": "from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler\n"
                   "logging.basicConfig(format='app: %(message)s')\n"
                   "logging.getLogger().addHandler(LoggingHandler(logger_provider=LoggerProvider()))",
    "errors only": "logging.basicConfig(level=logging.ERROR, format='app: %(message)s')\n"
                   "logging.getLogger().handlers[0].setLevel(logging.ERROR)",
}


@pytest.mark.parametrize("root", list(_ROOT_HANDLERS))
def test_a_warning_reaches_stderr_once_whatever_the_root_logger_has(root):
    """The package's warnings say what to fix. The OpenTelemetry log handler on the root logger
    (OTEL_PYTHON_LOGGING_AUTO_INSTRUMENTATION_ENABLED) used to swallow them: Python's last resort is
    skipped once any handler exists. Each shows on stderr exactly once, by the application's handler
    where it has one that takes warnings; an application that turned warnings off sees none."""
    program = "import logging\n" + _ROOT_HANDLERS[root] + "\nimport pydantic_opamp_otel\npydantic_opamp_otel._log.warning('OpAMP: fix me')\n"
    err = subprocess.run([sys.executable, "-c", program], capture_output=True, text=True, timeout=60).stderr
    assert err.count("OpAMP: fix me") == (0 if root == "errors only" else 1), err
    assert ("app: OpAMP: fix me" in err) == (root in ("stderr", "otel+stderr")), err


@pytest.mark.parametrize(("library", "reason"), [(None, "isn't installed"), (b"not a library", "can't be loaded")])
def test_a_core_that_cant_be_loaded_is_said_once_and_the_sdk_runs_unmanaged(tmp_path, library, reason):
    # An pydantic_opamp_core package without its library, or with a file that isn't one: the import and
    # configure() go through, the SDK works on its own settings, and one line says why.
    core = tmp_path / "pydantic_opamp_core"
    core.mkdir()
    (core / "__init__.py").write_text("")
    if library is not None:
        from pydantic_opamp_client import _native
        (core / _native._lib_name()).write_bytes(library)
    child = textwrap.dedent("""
        import pydantic_opamp_otel
        from opentelemetry import trace
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
        pydantic_opamp_otel.configure(endpoint="http://127.0.0.1:9/v1/opamp")
        pydantic_opamp_otel.AGENT.prepare()  # set up again: not said again
        exporter = InMemorySpanExporter()
        trace.get_tracer_provider().add_span_processor(SimpleSpanProcessor(exporter))
        with trace.get_tracer("app").start_as_current_span("work"):
            pass
        assert [s.name for s in exporter.get_finished_spans()] == ["work"]
        assert pydantic_opamp_otel.AGENT.setup is None and pydantic_opamp_otel.AGENT.client is None
    """)
    env = {**os.environ, "PYTHONPATH": os.pathsep.join([str(tmp_path), *filter(None, [os.environ.get("PYTHONPATH")])]),
           "OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none", "OTEL_LOGS_EXPORTER": "none"}
    run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60, env=env)
    assert run.returncode == 0, run.stderr
    said = [line for line in run.stderr.splitlines() if "OpAMP" in line]
    assert len(said) == 1, run.stderr
    assert said[0].startswith("OpAMP: the core can't be loaded on ") and "; the SDK runs unmanaged: " in said[0]
    assert reason in said[0], said


_OFF = json.loads((Path(__file__).parents[3] / "tests" / "shell-tables" / "off.json").read_text(encoding="utf-8"))["cases"]


@pytest.mark.parametrize("case", _OFF, ids=[c["id"] for c in _OFF])
def test_the_off_switch_table_through_configure(case):
    """tests/shell-tables/off.json, each case through configure() in a process given its
    environment: off, no client starts and the reason is said once, at WARNING; on, the client
    starts and the server hears from it. Every endpoint of a case is the test server."""
    server = _Server()
    local = lambda v: server.endpoint if v.startswith(("http://", "https://")) else v  # noqa: E731
    endpoint = None if case["code_endpoint"] is None else local(case["code_endpoint"])
    child = textwrap.dedent(f"""
        import logging, sys
        logging.basicConfig(stream=sys.stdout, format="%(levelname)s %(message)s")
        import pydantic_opamp_otel
        pydantic_opamp_otel.configure(endpoint={endpoint!r})
        print("client", pydantic_opamp_otel.AGENT.client is not None)
    """)
    try:
        run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60,
                             env=_child_env(OTEL_OPAMP_STATE_DIR="none", OTEL_TRACES_EXPORTER="none",
                                            OTEL_METRICS_EXPORTER="none", OTEL_LOGS_EXPORTER="none",
                                            **{k: local(v) for k, v in case["env"].items()}))
    finally:
        server.close()
    assert run.returncode == 0, run.stderr
    assert f"client {not case['off']}" in run.stdout, run.stdout + run.stderr
    assert bool(server.got) is not case["off"], server.got
    said = [line for line in run.stdout.splitlines() if "OpAMP: off" in line]
    assert said == ([f"WARNING OpAMP: {case['reason']}"] if case["reason"] else []), run.stdout
