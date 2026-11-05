# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The local report (contracts' logfire-sdk-local/v1): what the SDK does without the server, as the
host's `local` carries it to the layer, and each instrumentation's scope in the inventory."""

from __future__ import annotations

import json
import subprocess
import sys
import textwrap
from pathlib import Path
from types import SimpleNamespace

import pytest
from opentelemetry.sdk.trace.sampling import (ALWAYS_OFF, ALWAYS_ON, ParentBased, ParentBasedTraceIdRatio,
                                              TraceIdRatioBased)
from opentelemetry.util._importlib_metadata import entry_points
from pydantic_opamp_client.sdk import settings_pb2

import pydantic_opamp_otel
from pydantic_opamp_otel import LiveSampler, _decimal, _ratio, local_settings
from test_opamp_otel import _child_env, _own_state_home, _Server  # noqa: F401 - the fixture

SDK = {"traces": True, "metrics": True, "logs": True}
LOCAL = "logfire-sdk-local.json"


def _found(*signals: str) -> list[settings_pb2.FoundExporter]:
    return [settings_pb2.FoundExporter(signal=s, kind="otlp", gated=True) for s in signals]


class _OwnSampler(TraceIdRatioBased):
    """An application's sampler built on the ratio one: it may sample otherwise."""


@pytest.mark.parametrize(("sampler", "ratio"), [
    (ParentBased(ALWAYS_ON), 1.0),  # the SDK's default
    (ALWAYS_ON, 1.0),
    (ALWAYS_OFF, 0.0),
    (ParentBased(ALWAYS_OFF), 0.0),
    (TraceIdRatioBased(0.25), 0.25),
    (ParentBasedTraceIdRatio(0.1), 0.1),
    (ParentBased(_OwnSampler(0.5)), None),
    (_OwnSampler(0.5), None),
    (object(), None),
])
def test_the_ratio_is_the_sdks_own_samplers_and_none_for_any_other(sampler, ratio):
    assert _ratio(sampler) == ratio


def test_this_packages_sampler_reports_the_sdks_own_one_under_a_rate_the_server_set():
    live = LiveSampler().wrap(ParentBasedTraceIdRatio(0.3))
    live.rate(0.9)
    assert _ratio(live) == 0.3
    # The SDK gave none: its default, always on.
    assert _ratio(LiveSampler().wrap(None)) == 1.0


@pytest.mark.parametrize(("x", "text"), [(1.0, "1"), (0.0, "0"), (0.25, "0.25"), (1e-05, "0.00001"),
                                         (0.1, "0.1"), (1 / 3, "0.3333333333333333")])
def test_a_ratio_is_decimal_text(x, text):
    assert _decimal(x) == text
    assert float(text) == x


def test_each_signal_is_exported_where_the_sdk_has_an_exporter_for_it():
    got = local_settings(_found("traces", "traces", "logs"), SDK)
    assert (got.send_traces, got.send_metrics, got.send_logs) == (True, False, True)
    for setting in ("send_traces", "send_metrics", "send_logs"):
        assert got.HasField(setting)


def test_a_signal_whose_provider_isnt_the_sdks_is_left_out():
    got = local_settings(_found("traces"), {"traces": True, "metrics": False, "logs": False})
    assert got.HasField("send_traces")
    assert not got.HasField("send_metrics") and not got.HasField("send_logs")


def test_what_the_sdk_cant_tell_is_left_out():
    got = local_settings([], {}, object())
    assert got == settings_pb2.Local()
    for field in ("sampling_rate", "send_traces", "send_logs_level", "scrubbing", "instrumentations"):
        assert not got.HasField(field), field


def test_the_sampling_rate_is_the_sdks_own_samplers_ratio():
    assert local_settings([], {}, ParentBased(ALWAYS_ON)).sampling_rate == "1"
    assert local_settings([], {}, LiveSampler().wrap(ParentBasedTraceIdRatio(0.25))).sampling_rate == "0.25"


@pytest.mark.parametrize(("min_level", "level"), [(0, None), (1, "trace"), (5, "debug"), (9, "info"),
                                                  (10, None), (13, "warn"), (17, "error"), (21, "fatal")])
def test_the_logfire_sdks_own_minimum_level_where_it_is_one_of_the_policys(min_level, level):
    """0 is none set (every level is created); notice (10) is no level the policy has."""
    got = local_settings([], {}, logfire_config=SimpleNamespace(min_level=min_level, scrubbing=None))
    assert (got.send_logs_level if got.HasField("send_logs_level") else None) == level


def test_the_logfire_sdk_scrubs_unless_told_not_to():
    from logfire import ScrubbingOptions

    on = local_settings([], {}, logfire_config=SimpleNamespace(min_level=0, scrubbing=ScrubbingOptions()))
    off = local_settings([], {}, logfire_config=SimpleNamespace(min_level=0, scrubbing=False))
    assert on.HasField("scrubbing") and on.scrubbing.defaults and list(on.scrubbing.patterns) == []
    assert off.HasField("scrubbing") and not off.scrubbing.defaults


@pytest.mark.parametrize(("variable", "disabled", "all_disabled"), [
    ("", [], False),
    ("requests", ["requests"], False),
    (" requests , flask,requests,", ["requests", "flask"], False),
    ("requests,*", [], True),
])
def test_the_instrumentations_the_application_switched_off(variable, disabled, all_disabled):
    got = local_settings([], {}, disabled=variable)
    assert got.HasField("instrumentations")
    assert (list(got.instrumentations.disabled), got.instrumentations.all_disabled) == (disabled, all_disabled)


def test_each_instrumentation_is_listed_with_its_scope_the_module_of_its_entry_point():
    got = {i.name: i.scope for i in pydantic_opamp_otel.instrumentations()}
    assert got["aiohttp-client"] == "opentelemetry.instrumentation.aiohttp_client"
    want = {ep.name: ep.value.partition(":")[0].strip() for ep in entry_points(group="opentelemetry_instrumentor")}
    assert got == want
    # The scope is the tracer's: the instrumentation gets it by its module's name.
    from opentelemetry.instrumentation import aiohttp_client

    assert aiohttp_client.__name__ == got["aiohttp-client"]


def _local_of(run: subprocess.CompletedProcess) -> settings_pb2.Local:
    assert run.returncode == 0 and "Traceback" not in run.stderr, run.stderr
    line = next(line for line in run.stdout.splitlines() if line.startswith("local "))
    return settings_pb2.Local.FromString(bytes.fromhex(line.removeprefix("local ")))


_PRINT = """
    import time
    import pydantic_opamp_otel
    print("local", pydantic_opamp_otel.AGENT.host.local.SerializeToString().hex())
    time.sleep(1)  # for the server to ask for the components in full
"""


def test_under_opentelemetry_instrument_the_host_carries_what_the_sdk_it_built_does(tmp_path):
    from test_inventory import _asking

    server = _asking()  # asks for the components in full
    try:
        script = tmp_path / "app.py"
        script.write_text(textwrap.dedent(_PRINT))
        run = subprocess.run(
            [str(Path(sys.executable).parent / "opentelemetry-instrument"), sys.executable, str(script)],
            capture_output=True, text=True, timeout=60,
            env=_child_env(OTEL_OPAMP_ENDPOINT=server.endpoint, OTEL_OPAMP_STATE_DIR=str(tmp_path / "s"),
                           OTEL_SERVICE_NAME="checkout", OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:9",
                           OTEL_METRIC_EXPORT_TIMEOUT="1", OTEL_BSP_EXPORT_TIMEOUT="1",
                           OTEL_TRACES_SAMPLER="parentbased_traceidratio", OTEL_TRACES_SAMPLER_ARG="0.25",
                           OTEL_TRACES_EXPORTER="otlp", OTEL_METRICS_EXPORTER="none", OTEL_LOGS_EXPORTER="console",
                           OTEL_PYTHON_DISABLED_INSTRUMENTATIONS="aiohttp-client"))
    finally:
        server.close()
    got = _local_of(run)
    assert got.sampling_rate == "0.25"
    assert (got.send_traces, got.send_metrics, got.send_logs) == (True, False, True)
    assert list(got.instrumentations.disabled) == ["aiohttp-client"] and not got.instrumentations.all_disabled
    # The OpenTelemetry SDK has no level or scrubbing of its own to tell.
    assert not got.HasField("send_logs_level") and not got.HasField("scrubbing")
    # The server is told, beside the policy in force: the layer's document, the same in every language.
    files = [m.effective_config.config_map.config_map for m in server.got if m.HasField("effective_config")]
    assert files and all(set(f) == {"logfire-sdk-policy.json", LOCAL} for f in files)
    doc = json.loads(files[0][LOCAL].body)
    assert doc["head_sampling"] == {"ratio": 0.25}
    assert doc["signal_exports"] == {"traces": {"enabled": True}, "metrics": {"enabled": False},
                                     "logs": {"enabled": True}}
    assert doc["instrumentations"] == {"disabled": ["aiohttp-client"], "all_disabled": False}
    assert doc["sampling_rules"] == {"rules": []} and "log_levels" not in doc and "scrubbing" not in doc
    # The policy holds no local value: every control there is local.
    policy = json.loads(files[0]["logfire-sdk-policy.json"].body)
    assert policy["head_sampling"] == {"mode": "local"}
    # And each instrumentation's scope, in the components.
    components = [m.available_components for m in server.got if m.available_components.components]
    scopes = {n: {kv.key: kv.value.string_value for kv in d.metadata}.get("scope")
              for n, d in components[0].components["instrumentations"].sub_component_map.items()}
    assert scopes["aiohttp-client"] == "opentelemetry.instrumentation.aiohttp_client"


def test_manage_reports_the_applications_sampler_and_no_instrumentations(tmp_path):
    server = _Server()
    child = textwrap.dedent(f"""
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor, ConsoleSpanExporter
        from opentelemetry.sdk.trace.sampling import TraceIdRatioBased
        import pydantic_opamp_otel
        provider = TracerProvider(sampler=TraceIdRatioBased(0.5))
        provider.add_span_processor(BatchSpanProcessor(ConsoleSpanExporter()))
        trace.set_tracer_provider(provider)
        pydantic_opamp_otel.manage(endpoint={server.endpoint!r}, state_dir={str(tmp_path / "s")!r})
        print("local", pydantic_opamp_otel.AGENT.host.local.SerializeToString().hex())
        pydantic_opamp_otel.AGENT.stop()
    """)
    try:
        run = subprocess.run([sys.executable, "-c", child], capture_output=True, text=True, timeout=60,
                             env=_child_env())
    finally:
        server.close()
    got = _local_of(run)
    assert got.sampling_rate == "0.5"
    assert got.HasField("send_traces") and got.send_traces
    # This package loads no instrumentation under manage(): nothing of its own to tell.
    assert not got.HasField("instrumentations")
