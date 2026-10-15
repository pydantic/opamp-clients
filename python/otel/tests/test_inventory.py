# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The inventory (docs/available-components.md): what the package lists of the SDK it built, under
the names and the host the rest of it uses, and that the client sends it."""

from __future__ import annotations

import os
import subprocess
import sys
import textwrap
from importlib.metadata import version
from pathlib import Path
from types import SimpleNamespace

import pytest
from opamp_client import opamp_pb2
from opamp_client.sdk import SdkSettings, settings_pb2
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor, ConsoleLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import ConsoleMetricExporter, InMemoryMetricReader, PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, ConsoleSpanExporter, SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.util._importlib_metadata import entry_points

import opamp_otel
from opamp_otel import Agent, Controls

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

    inv = opamp_otel.inventory(None, TracerProvider(), MeterProvider(), LoggerProvider())
    assert (inv.integration, inv.integration_version) == ("pydantic-opamp-otel", version("pydantic-opamp-otel"))
    assert (inv.sdk_language, inv.sdk_version) == ("python", __version__)
    assert parts(LAYER.components(inv))["sdk"] == {
        "integration.name": "pydantic-opamp-otel", "integration.version": version("pydantic-opamp-otel"),
        "telemetry.sdk.language": "python", "telemetry.sdk.version": __version__}


def test_the_instrumentations_are_the_names_opentelemetry_instrument_matches_at_their_versions():
    got = {i.name: i.version for i in opamp_otel.instrumentations()}
    assert got == instrumentors() and "aiohttp-client" in got  # the dev group's
    assert got["aiohttp-client"] == version("opentelemetry-instrumentation-aiohttp-client")
    # Each is a name the switch takes: it lands in the variable opentelemetry-instrument reads.
    s = LAYER.decide(_config('{"deactivate_instrumentations":"aiohttp-client"}'), b"{}", b"{}",
                     opamp_otel.sdk_host(["otlp"]), save=lambda doc: None).apply
    assert opamp_otel.disabled_instrumentations(s, "") == "aiohttp-client"


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
    got = opamp_otel.instrumentations()
    assert [(i.name, i.version) for i in got] == [("flask", "0.56b0"), ("bare", ""), ("nodist", "")]
    assert parts(LAYER.components(settings_pb2.Inventory(instrumentations=got)))["instrumentations"] == {
        "bare": None, "flask": "0.56b0", "nodist": None}
    # A requirement it can't read: listed, as opentelemetry-instrument would still try it.
    monkeypatch.setattr(deps, "get_dist_dependency_conflicts", lambda d: 1 / 0)
    assert [i.name for i in opamp_otel.instrumentations()] == ["flask", "django", "bare", "nodist"]


def test_no_instrumentation_installed_is_an_empty_list(monkeypatch):
    import opentelemetry.util._importlib_metadata as metadata

    monkeypatch.setattr(metadata, "entry_points", lambda group: [])
    inv = opamp_otel.inventory(None, TracerProvider(), MeterProvider(), LoggerProvider())
    assert list(inv.instrumentations) == []
    got = parts(LAYER.components(inv))
    assert got["instrumentations"] == {} and got["exporters"] == {}


class _NotAnExporter(SpanProcessor):
    """A span processor with no exporter behind it (a baggage or attribute processor, say)."""


def test_the_exporters_are_read_off_the_providers_gated_where_the_packages_gates_wrap_them():
    from opentelemetry.exporter.otlp.proto.grpc._log_exporter import OTLPLogExporter
    from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter as HttpSpanExporter

    c = Controls()
    traces = TracerProvider()
    # As the package's Configurator builds them: behind the export processor, gated.
    traces.add_span_processor(BatchSpanProcessor(c.spans(OTLPSpanExporter(endpoint="http://127.0.0.1:9"))))
    traces.add_span_processor(BatchSpanProcessor(c.spans(ConsoleSpanExporter())))
    # Given to configure() or added by the application: nothing stops them.
    traces.add_span_processor(SimpleSpanProcessor(HttpSpanExporter(endpoint="http://127.0.0.1:9")))
    traces.add_span_processor(SimpleSpanProcessor(InMemorySpanExporter()))
    traces.add_span_processor(_NotAnExporter())
    gated_otlp = opamp_otel.GatedOtlpMetricExporter(endpoint="http://127.0.0.1:9")
    meters = MeterProvider(metric_readers=[
        PeriodicExportingMetricReader(gated_otlp, export_interval_millis=3_600_000),
        PeriodicExportingMetricReader(ConsoleMetricExporter(), export_interval_millis=3_600_000),
        InMemoryMetricReader()])
    logs = LoggerProvider()
    logs.add_log_record_processor(BatchLogRecordProcessor(c.logs(OTLPLogExporter(endpoint="http://127.0.0.1:9"))))
    logs.add_log_record_processor(SimpleLogRecordProcessor(ConsoleLogRecordExporter()))
    try:
        got = opamp_otel.exporters(traces, meters, logs)
    finally:
        meters.shutdown()
        traces.shutdown()
        logs.shutdown()
    assert listed(got) == [
        ("logs", "console", False), ("logs", "otlp", True),
        ("metrics", "InMemoryMetricReader", False), ("metrics", "console", False), ("metrics", "otlp", True),
        ("traces", "InMemorySpanExporter", False), ("traces", "console", True), ("traces", "otlp", False),
        ("traces", "otlp", True)]
    # The OTLP trace exporter twice, once ungated: the layer says what's behind it can't be stopped.
    assert parts(LAYER.components(settings_pb2.Inventory(exporters=got)))["exporters"] == {
        "traces": {"otlp": False, "console": True, "InMemorySpanExporter": False},
        "metrics": {"otlp": True, "console": False, "InMemoryMetricReader": False},
        "logs": {"otlp": True, "console": False}}


def test_a_gated_exporter_whose_send_setting_the_host_cant_carry_out_is_ungated():
    gated_otlp = opamp_otel.GatedOtlpMetricExporter(endpoint="http://127.0.0.1:9")
    meters = MeterProvider(metric_readers=[PeriodicExportingMetricReader(gated_otlp, export_interval_millis=3_600_000)])
    traces = TracerProvider()
    traces.add_span_processor(SimpleSpanProcessor(Controls().spans(ConsoleSpanExporter())))
    try:
        def gated(host):
            return listed(opamp_otel.inventory(host, traces, meters, LoggerProvider()).exporters)

        assert gated(opamp_otel.sdk_host(["otlp"])) == [("metrics", "otlp", True), ("traces", "console", True)]
        # Beside a console exporter, send_metrics is ignored; under another configurator, send_traces.
        assert gated(opamp_otel.sdk_host(["otlp", "console"])) == [
            ("metrics", "otlp", False), ("traces", "console", True)]
        assert gated(opamp_otel.sdk_host(["otlp"], configured=False)) == [
            ("metrics", "otlp", True), ("traces", "console", False)]
    finally:
        meters.shutdown()


def test_providers_that_arent_the_sdks_have_no_exporters():
    from opentelemetry._logs import NoOpLoggerProvider
    from opentelemetry.metrics import NoOpMeterProvider
    from opentelemetry.trace import NoOpTracerProvider

    assert opamp_otel.exporters(NoOpTracerProvider(), NoOpMeterProvider(), NoOpLoggerProvider()) == []
    assert opamp_otel.exporters(TracerProvider(), MeterProvider(), LoggerProvider()) == []


def test_an_exporter_type_no_entry_point_names_is_named_by_its_type_and_none_is_imported_to_find_out():
    class MySpanExporter(InMemorySpanExporter):
        pass

    traces = TracerProvider()
    traces.add_span_processor(SimpleSpanProcessor(MySpanExporter()))
    before = set(sys.modules)
    assert listed(opamp_otel.exporters(traces, None, None)) == [("traces", "MySpanExporter", False)]
    assert set(sys.modules) == before


# --- the agent ------------------------------------------------------------------------------------


def _config(body: str) -> opamp_pb2.AgentRemoteConfig:
    m = opamp_pb2.AgentConfigMap()
    m.config_map["elastic"].CopyFrom(opamp_pb2.AgentConfigObject(body=body.encode(), content_type="application/json"))
    return opamp_pb2.AgentRemoteConfig(config=m, config_hash=body.encode())


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


def test_the_settings_listed_are_those_the_agents_host_carries_out_and_the_server_gets_them(tmp_path):
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
        assert agent.components == LAYER.components(opamp_otel.inventory(agent.host), agent.host, NOW)
    settings = parts(ours.components)["settings"]
    assert settings["sampling_rules"] == settings["send_metrics"] == "at once"
    assert settings["deactivate_instrumentations"] == "next start"
    # Another configurator built the SDK: its sampler and trace and log exporters aren't ours, the
    # newer settings aren't declared, and with a console metric exporter nothing stops metrics.
    # The application's own settings are handed on whoever built the SDK.
    assert set(parts(theirs.components)["settings"]) == {
        "custom", "deactivate_all_instrumentations", "deactivate_instrumentations", "opamp_polling_interval"}
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

    monkeypatch.setattr(opamp_otel, "inventory", lambda host: 1 / 0)
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
    # The OTLP metric exporter is the gated one; none for logs: no logs entry.
    assert got["exporters"] == {"traces": {"otlp": True, "console": True}, "metrics": {"otlp": True}}
    assert got["settings"]["send_metrics"] == "at once"
    assert got["settings"]["deactivate_instrumentations"] == "next start"


def test_a_metric_exporter_beside_otlp_is_ungated_and_send_metrics_is_not_listed(tmp_path):
    server = _asking()
    try:
        # Named twice, and beside one the package can't stop.
        sent = _instrument(server, tmp_path, APP, OTEL_TRACES_EXPORTER="none",
                           OTEL_METRICS_EXPORTER="otlp,console,otlp", OTEL_LOGS_EXPORTER="console")
    finally:
        server.close()
    got = parts(sent)
    # The OTLP one is behind the gate, but send_metrics is ignored: nothing reaches either.
    assert got["exporters"] == {"metrics": {"otlp": False, "console": False}, "logs": {"console": True}}
    assert "send_metrics" not in got["settings"] and got["settings"]["send_traces"] == "at once"


def test_configure_lists_a_span_processor_it_was_given_as_ungated_and_no_instrumentation_switch(tmp_path):
    server = _asking()
    child = textwrap.dedent(f"""
        import time, opamp_otel
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
        opamp_otel.configure(endpoint={server.endpoint!r},
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
    # The span processor exports past the gate, so send_traces isn't declared: nothing stops the
    # console exporter either, and it is ungated too.
    assert got["exporters"] == {"traces": {"console": False, "InMemorySpanExporter": False}}
    assert "send_traces" not in got["settings"]
    # It instruments nothing, so nothing reads the switch; the instrumentations are still those
    # installed, under the names opentelemetry-instrument would take.
    assert "deactivate_instrumentations" not in got["settings"]
    assert got["instrumentations"] == instrumentors()
