# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The inventory (docs/available-components.md): what the package lists of the SDK it built, under
the names and the host the rest of it uses, and that the client sends it."""

from __future__ import annotations

import io
import os
import subprocess
import sys
import textwrap
from importlib.metadata import version
from pathlib import Path
from types import SimpleNamespace

import pytest
from pydantic_opamp_client import opamp_pb2
from pydantic_opamp_client.sdk import SdkSettings, settings_pb2
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor, ConsoleLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import ConsoleMetricExporter, InMemoryMetricReader, PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, ConsoleSpanExporter, SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.util._importlib_metadata import entry_points

import pydantic_opamp_otel
from _globals import sdk_globals
from _policy import config_map, policy
from pydantic_opamp_otel import Agent, Controls

LAYER = SdkSettings()
#: 2026-10-14T12:00:00Z.
NOW = 1_791_979_200_000


@pytest.fixture(autouse=True)
def _own_state_home(tmp_path_factory, monkeypatch):
    home = tmp_path_factory.mktemp("state-home")
    monkeypatch.setenv("XDG_STATE_HOME", str(home))
    monkeypatch.setenv("HOME", str(home))


def parts(c: opamp_pb2.AvailableComponents) -> dict:
    """The components as the shared vectors give them (python/tests/test_sdk_settings.py)."""

    def meta(d, key):
        return next((kv.value for kv in d.metadata if kv.key == key), None)

    p = c.components
    return {
        "sdk": {kv.key: kv.value.string_value for kv in p["sdk"].metadata},
        "instrumentations": {n: (v.string_value if (v := meta(d, "version")) else None)
                             for n, d in p["instrumentations"].sub_component_map.items()},
        "exporters": {s: {n: meta(e, "gated").bool_value for n, e in d.sub_component_map.items()}
                      for s, d in p["exporters"].sub_component_map.items()},
        "settings": {k: meta(d, "applies").string_value for k, d in p["settings"].sub_component_map.items()},
    }


def listed(exporters: list[settings_pb2.Exporter]) -> list[tuple[str, str, bool]]:
    return sorted((e.signal, e.name, e.gated) for e in exporters)


def instrumentors() -> dict[str, str]:
    """The instrumentations installed here, as opentelemetry-instrument matches them."""
    return {ep.name: ep.dist.version for ep in entry_points(group="opentelemetry_instrumentor")}


# --- the parts ------------------------------------------------------------------------------------


def test_the_sdk_entry_is_this_package_and_the_python_sdk_it_runs():
    from opentelemetry.sdk.version import __version__

    inv = pydantic_opamp_otel.inventory([])
    assert (inv.integration, inv.integration_version) == ("pydantic-opamp-otel", version("pydantic-opamp-otel"))
    assert (inv.sdk_language, inv.sdk_version) == ("python", __version__)
    assert parts(LAYER.components(inv))["sdk"] == {
        "integration.name": "pydantic-opamp-otel", "integration.version": version("pydantic-opamp-otel"),
        "telemetry.sdk.language": "python", "telemetry.sdk.version": __version__}


def test_the_instrumentations_are_the_names_opentelemetry_instrument_matches_at_their_versions():
    got = {i.name: i.version for i in pydantic_opamp_otel.instrumentations()}
    assert got == instrumentors() and "aiohttp-client" in got  # the dev group's
    assert got["aiohttp-client"] == version("opentelemetry-instrumentation-aiohttp-client")
    # Each is a name the switch takes: it lands in the variable opentelemetry-instrument reads.
    local = policy().encode()
    s = LAYER.decide(_config(policy(instrumentations={"disabled": ["aiohttp-client"]})), local, local,
                     pydantic_opamp_otel.sdk_host(), save=lambda doc: None).apply
    assert pydantic_opamp_otel.disabled_instrumentations(s, "") == "aiohttp-client"


def _ep(name: str, dist) -> SimpleNamespace:
    return SimpleNamespace(name=name, dist=dist, value=f"m:{name}")


def test_an_instrumentation_whose_library_is_missing_is_left_out_and_one_without_a_version_has_none(monkeypatch):
    import opentelemetry.instrumentation.dependencies as deps
    import opentelemetry.util._importlib_metadata as metadata

    flask, django, bare = (SimpleNamespace(version="0.56b0"), SimpleNamespace(version="0.56b0"),
                           SimpleNamespace(version=None))
    eps = [_ep("flask", flask), _ep("django", django), _ep("bare", bare), _ep("nodist", None)]
    monkeypatch.setattr(metadata, "entry_points", lambda group: eps if group == "opentelemetry_instrumentor" else [])
    # django's library isn't installed: opentelemetry-instrument would skip it, so it can't run.
    monkeypatch.setattr(deps, "get_dist_dependency_conflicts", lambda d: "missing" if d is django else None)
    got = pydantic_opamp_otel.instrumentations()
    assert [(i.name, i.version) for i in got] == [("flask", "0.56b0"), ("bare", ""), ("nodist", "")]
    assert parts(LAYER.components(settings_pb2.Inventory(instrumentations=got)))["instrumentations"] == {
        "bare": None, "flask": "0.56b0", "nodist": None}
    # A requirement it can't read: listed, as opentelemetry-instrument would still try it.
    monkeypatch.setattr(deps, "get_dist_dependency_conflicts", lambda d: 1 / 0)
    assert [i.name for i in pydantic_opamp_otel.instrumentations()] == ["flask", "django", "bare", "nodist"]


def test_no_instrumentation_installed_is_an_empty_list(monkeypatch):
    import opentelemetry.util._importlib_metadata as metadata

    monkeypatch.setattr(metadata, "entry_points", lambda group: [])
    inv = pydantic_opamp_otel.inventory([])
    assert list(inv.instrumentations) == []
    got = parts(LAYER.components(inv))
    assert got["instrumentations"] == {} and got["exporters"] == {}


class _NotAnExporter(SpanProcessor):
    """A span processor with no exporter behind it (a baggage or attribute processor, say)."""


class _Holding(SpanProcessor):
    """A span processor of the application's own that exports itself: the package can't wrap it."""

    def __init__(self, exporter) -> None:
        self.exporter = exporter


def _span(name: str = "s"):
    from opentelemetry.sdk.trace import ReadableSpan

    return ReadableSpan(name=name)


def test_every_exporter_on_the_sdks_providers_is_gated_and_what_cant_be_is_reported_ungoverned():
    from opentelemetry.exporter.otlp.proto.grpc._log_exporter import OTLPLogExporter
    from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter as HttpSpanExporter

    c = Controls()
    traces = TracerProvider()
    # As the package's Configurator builds them, given to configure(), or added by the
    # application: wherever an export processor holds it, the exporter gets the gate.
    traces.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint="http://127.0.0.1:9")))
    traces.add_span_processor(BatchSpanProcessor(c.spans(ConsoleSpanExporter())))
    traces.add_span_processor(SimpleSpanProcessor(HttpSpanExporter(endpoint="https://user:key@api.example.com/v1/traces")))
    memory = InMemorySpanExporter()
    traces.add_span_processor(SimpleSpanProcessor(memory))
    traces.add_span_processor(_NotAnExporter())
    # A processor of its own holding one: past the package's reach, so reported.
    traces.add_span_processor(_Holding(InMemorySpanExporter()))
    meters = MeterProvider(metric_readers=[
        PeriodicExportingMetricReader(ConsoleMetricExporter(), export_interval_millis=3_600_000),
        InMemoryMetricReader()])
    logs = LoggerProvider()
    logs.add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter(endpoint="http://127.0.0.1:9")))
    shown = io.StringIO()
    logs.add_log_record_processor(SimpleLogRecordProcessor(ConsoleLogRecordExporter(out=shown)))
    try:
        got = pydantic_opamp_otel.govern(c, traces, meters, logs)
        # Again: nothing is gated twice.
        again = pydantic_opamp_otel.govern(c, traces, meters, logs)
        assert listed(again.exporters) == listed(got.exporters)
        assert type(memory) is InMemorySpanExporter and traces._active_span_processor._span_processors[3].span_exporter._to is memory
        # The gate is in force on what was added past the package: send_traces stops it.
        c.apply(SdkSettings().decide(_config(policy(signal_exports={"traces": {"enabled": False}})),
                                     b"{}", b"{}", pydantic_opamp_otel.sdk_host(), save=lambda doc: None).apply)
        traces._active_span_processor._span_processors[3].on_end(_span())
        assert memory.get_finished_spans() == ()
        c.apply(SdkSettings().decide(_config(policy(signal_exports={"logs": {"enabled": False}})),
                                     b"{}", b"{}", pydantic_opamp_otel.sdk_host(), save=lambda doc: None).apply)
        logs.get_logger("t").emit(body="dropped")
        assert shown.getvalue() == ""
    finally:
        meters.shutdown()
        traces.shutdown()
        logs.shutdown()
    assert got.sdk == {"traces": True, "metrics": True, "logs": True}
    assert listed(got.exporters) == [
        ("logs", "console", True), ("logs", "otlp", True),
        ("metrics", "InMemoryMetricReader", False), ("metrics", "console", True),
        ("traces", "InMemorySpanExporter", False), ("traces", "InMemorySpanExporter", True),
        ("traces", "console", True), ("traces", "otlp", True), ("traces", "otlp", True)]
    # Each exporter by where it sends, its credentials and path left out.
    assert parts(LAYER.components(settings_pb2.Inventory(exporters=got.exporters)))["exporters"] == {
        "traces": {"otlp@127.0.0.1:9": True, "otlp@api.example.com:443": True, "console": True,
                   "InMemorySpanExporter": False},
        "metrics": {"console": True, "InMemoryMetricReader": False},
        "logs": {"otlp@127.0.0.1:9": True, "console": True}}
    # The core's words, one per ungoverned exporter, as every SDK says them.
    assert got.warnings == [
        "the traces exporter InMemorySpanExporter sends past the policy's gates, and Fleet lists it ungoverned: "
        "add it to the SDK's providers for the policy to govern it",
        "the metrics exporter InMemoryMetricReader sends past the policy's gates, and Fleet lists it ungoverned: "
        "add it to the SDK's providers for the policy to govern it"]


def test_providers_that_arent_the_sdks_have_no_exporters():
    from opentelemetry._logs import NoOpLoggerProvider
    from opentelemetry.metrics import NoOpMeterProvider
    from opentelemetry.trace import NoOpTracerProvider

    none = pydantic_opamp_otel.govern(Controls(), NoOpTracerProvider(), NoOpMeterProvider(), NoOpLoggerProvider())
    assert none.exporters == [] and none.sdk == {"traces": False, "metrics": False, "logs": False}
    empty = pydantic_opamp_otel.govern(Controls(), TracerProvider(), MeterProvider(), LoggerProvider())
    assert empty.exporters == [] and empty.sdk == {"traces": True, "metrics": True, "logs": True}


def test_an_exporter_type_no_entry_point_names_is_named_by_its_type_and_none_is_imported_to_find_out():
    class MySpanExporter(InMemorySpanExporter):
        pass

    traces = TracerProvider()
    traces.add_span_processor(SimpleSpanProcessor(MySpanExporter()))
    before = set(sys.modules)
    found = pydantic_opamp_otel.govern(Controls(), traces, None, None)
    assert listed(found.exporters) == [("traces", "MySpanExporter", True)]
    assert set(sys.modules) == before


# --- the agent ------------------------------------------------------------------------------------


def _config(body: str) -> opamp_pb2.AgentRemoteConfig:
    return opamp_pb2.AgentRemoteConfig(config=config_map(body), config_hash=body.encode())


def _asking():
    """A test server that asks for the components in full whenever it is sent only their hash, as
    a server that doesn't know them does."""
    import test_opamp_otel

    def reply(msg):
        if msg.HasField("available_components") and not msg.available_components.components:
            return opamp_pb2.ServerToAgent(instance_uid=msg.instance_uid,
                                           flags=opamp_pb2.ServerToAgentFlags_ReportAvailableComponents)
        return None

    return test_opamp_otel._Server(reply)


def _full(server) -> list[opamp_pb2.AvailableComponents]:
    return [m.available_components for m in server.got if m.available_components.components]


def _started(server, tmp_path, configured: bool, **env) -> Agent:
    agent = Agent()
    agent.wall_ms = lambda: NOW
    agent.prepare({"OTEL_OPAMP_ENDPOINT": server.endpoint, "OTEL_OPAMP_STATE_DIR": str(tmp_path), **env})
    agent.configured = configured
    agent.start(Resource.create({"service.name": "checkout"}))
    return agent


def test_the_settings_listed_are_those_the_agents_host_carries_out_and_the_server_gets_them(tmp_path, monkeypatch):
    sdk_globals(monkeypatch)
    server = _asking()
    try:
        ours = _started(server, tmp_path / "ours", configured=True)
        server.wait(lambda: len(_full(server)) == 1)
        ours.stop()
        theirs = _started(server, tmp_path / "theirs", configured=False, OTEL_METRICS_EXPORTER="console")
        server.wait(lambda: len(_full(server)) == 2)
        theirs.stop()
    finally:
        server.close()
    for agent in (ours, theirs):
        assert agent.components == LAYER.components(pydantic_opamp_otel.inventory(agent.exporters), agent.host, NOW)
    settings = parts(ours.components)["settings"]
    assert settings["sampling_rules"] == settings["send_metrics"] == "at once"
    assert settings["deactivate_instrumentations"] == "next start"
    # Another configurator built the SDK: its sampler isn't ours, so neither the sampling settings
    # nor debug mode are declared; the gates are, on whatever SDK the global providers are. The
    # application's own settings are handed on whoever built the SDK.
    declared = set(parts(theirs.components)["settings"])
    assert "custom" in declared and "deactivate_instrumentations" in declared
    assert not declared & {*pydantic_opamp_otel.SAMPLER, *pydantic_opamp_otel.DEBUG}
    assert declared >= {"send_traces", "send_metrics", "send_logs", "mask_attributes"}
    assert declared == {*theirs.host.live, *theirs.host.next_start}
    # The first message of each carries them in full, its hash set by the client.
    sent = [m.available_components for m in server.got if m.HasField("available_components")]
    full = _full(server)
    assert sent and all(c.hash for c in sent)
    assert len({c.hash for c in full}) == 2  # different components, different hashes
    for agent in (ours, theirs):
        want = opamp_pb2.AvailableComponents()
        want.CopyFrom(agent.components)
        assert any(c.hash and _without_hash(c) == want for c in full)
    assert all(m.capabilities & opamp_pb2.AgentCapabilities_ReportsAvailableComponents for m in server.got)


def _without_hash(c: opamp_pb2.AvailableComponents) -> opamp_pb2.AvailableComponents:
    out = opamp_pb2.AvailableComponents()
    out.CopyFrom(c)
    out.hash = b""
    return out


def test_an_inventory_that_cant_be_listed_leaves_the_instance_managed(tmp_path, monkeypatch, caplog):
    import test_opamp_otel

    monkeypatch.setattr(pydantic_opamp_otel, "inventory", lambda exporters: 1 / 0)
    server = test_opamp_otel._Server()
    try:
        agent = _started(server, tmp_path, configured=True, OTEL_OPAMP_HEARTBEAT_INTERVAL="100ms")
        server.wait(lambda: len(server.got) >= 2)
        agent.stop()
    finally:
        server.close()
    assert agent.components is None and "can't list what this SDK runs" in caplog.text
    assert not any(m.HasField("available_components") for m in server.got)


# --- end to end -----------------------------------------------------------------------------------


def _child_env(**extra) -> dict:
    return {**{k: v for k, v in os.environ.items() if not k.startswith("OTEL_")}, **extra}


def _instrument(server, tmp_path, app: str, **env) -> opamp_pb2.AvailableComponents:
    """Run `app` under opentelemetry-instrument against `server`; the components it sent."""
    script = tmp_path / "app.py"
    script.write_text(textwrap.dedent(app))
    run = subprocess.run([str(Path(sys.executable).parent / "opentelemetry-instrument"), sys.executable, str(script)],
                         capture_output=True, text=True, timeout=60,
                         env=_child_env(OTEL_OPAMP_ENDPOINT=server.endpoint, OTEL_OPAMP_STATE_DIR=str(tmp_path / "s"),
                                        OTEL_SERVICE_NAME="checkout", OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:9",
                                        OTEL_METRIC_EXPORT_TIMEOUT="1", OTEL_BSP_EXPORT_TIMEOUT="1", **env))
    assert run.returncode == 0 and "Traceback" not in run.stderr, run.stderr
    full = _full(server)
    assert full and all(c.hash for c in full), run.stderr
    return full[0]


APP = """
    import time
    time.sleep(1)
"""


def test_under_opentelemetry_instrument_the_inventory_is_what_it_built(tmp_path):
    from opentelemetry.sdk.version import __version__

    server = _asking()
    try:
        sent = _instrument(server, tmp_path, APP, OTEL_TRACES_EXPORTER="otlp,console", OTEL_METRICS_EXPORTER="otlp",
                           OTEL_LOGS_EXPORTER="none", OTEL_PYTHON_DISABLED_INSTRUMENTATIONS="aiohttp-client")
    finally:
        server.close()
    got = parts(sent)
    assert got["sdk"] == {"integration.name": "pydantic-opamp-otel",
                          "integration.version": version("pydantic-opamp-otel"),
                          "telemetry.sdk.language": "python", "telemetry.sdk.version": __version__}
    # Switched off, still listed: it can run.
    assert got["instrumentations"] == instrumentors()
    # Every exporter it built is gated, each OTLP one by where it sends; none for logs: no logs entry.
    assert got["exporters"] == {"traces": {"otlp@127.0.0.1:9": True, "console": True},
                                "metrics": {"otlp@127.0.0.1:9": True}}
    assert got["settings"]["send_metrics"] == "at once"
    assert got["settings"]["deactivate_instrumentations"] == "next start"


def test_a_metric_exporter_beside_otlp_is_gated_too_and_send_metrics_is_listed(tmp_path):
    server = _asking()
    try:
        # Named twice, and beside one the package can't stop.
        sent = _instrument(server, tmp_path, APP, OTEL_TRACES_EXPORTER="none",
                           OTEL_METRICS_EXPORTER="otlp,console,otlp", OTEL_LOGS_EXPORTER="console")
    finally:
        server.close()
    got = parts(sent)
    # Each behind the gate, the console's too: send_metrics stops both.
    assert got["exporters"] == {"metrics": {"otlp@127.0.0.1:9": True, "console": True}, "logs": {"console": True}}
    assert got["settings"]["send_metrics"] == got["settings"]["send_traces"] == "at once"


def test_configure_gates_a_span_processor_it_was_given_and_lists_no_instrumentation_switch(tmp_path):
    server = _asking()
    child = textwrap.dedent(f"""
        import time, pydantic_opamp_otel
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
        pydantic_opamp_otel.configure(endpoint={server.endpoint!r},
                             span_processors=[SimpleSpanProcessor(InMemorySpanExporter())])
        time.sleep(0.5)
    """)
    try:
        run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60,
                             env=_child_env(OTEL_TRACES_EXPORTER="console", OTEL_METRICS_EXPORTER="none",
                                            OTEL_LOGS_EXPORTER="none", OTEL_OPAMP_STATE_DIR=str(tmp_path)))
    finally:
        server.close()
    assert run.returncode == 0, run.stderr
    [sent] = {c.SerializeToString(): c for c in _full(server)}.values()
    got = parts(sent)
    # The span processor's exporter gets the gate as the console's does: send_traces stops both.
    assert got["exporters"] == {"traces": {"console": True, "InMemorySpanExporter": True}}
    assert got["settings"]["send_traces"] == "at once"
    # It instruments nothing, so nothing reads the switch; the instrumentations are still those
    # installed, under the names opentelemetry-instrument would take.
    assert "deactivate_instrumentations" not in got["settings"]
    assert got["instrumentations"] == instrumentors()


class _FailingLayer:
    """A layer whose Exporters call fails, as a trapped core would."""

    def exporters(self, *args, **kwargs):
        raise RuntimeError("the core trapped")


def test_a_failing_exporters_call_leaves_the_gates_in_place_and_raises_nothing(caplog):
    # Reporting only: the gates are in front of every exporter all the same, each exporter is
    # listed as it was found, and the application never sees the error.
    c = Controls()
    traces = TracerProvider()
    memory = InMemorySpanExporter()
    traces.add_span_processor(SimpleSpanProcessor(memory))
    try:
        got = pydantic_opamp_otel.govern(c, traces, MeterProvider(), LoggerProvider(), layer=_FailingLayer())
    finally:
        traces.shutdown()
    assert got.exporters == [settings_pb2.Exporter(signal="traces", name="InMemorySpanExporter", gated=True)]
    assert traces._active_span_processor._span_processors[0].span_exporter._to is memory
    assert "can't list the exporters" in caplog.text


def test_a_failing_exporters_call_keeps_the_names_last_decided():
    # Identities in Fleet stay put: an exporter listed before keeps its name, one new since is
    # listed as it was found.
    otlp = settings_pb2.FoundExporter(signal="traces", kind="otlp_proto_grpc", gated=True, endpoint="col:4317")
    listed_as: dict = {}
    first = pydantic_opamp_otel.Governed()
    first.found = [otlp]
    assert [e.name for e in first.decide(LAYER, {}, listed_as).exporters] == ["otlp"]
    later = pydantic_opamp_otel.Governed()
    later.found = [otlp, settings_pb2.FoundExporter(signal="traces", kind="Mine", gated=False)]
    got = later.decide(_FailingLayer(), {}, listed_as).exporters
    assert [(e.name, e.gated, e.destination) for e in got] == [("otlp", True, "col:4317"), ("Mine", False, "")]
