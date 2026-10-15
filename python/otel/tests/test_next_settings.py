# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The settings that match telemetry, debug mode and export health, on the package's real sampler,
gates and agent: the shared vectors (testdata/sdk-settings) run through them, then each
setting's edge cases from docs/sdk-settings.md."""

from __future__ import annotations

import contextlib
import json
import os
import shutil
from pathlib import Path

import pytest
from opamp_client import anyvalue_pb2, opamp_pb2
from opamp_client.sdk import SdkSettings, settings_pb2
from opentelemetry._logs import LogRecord, SeverityNumber
from opentelemetry.sdk._logs import ReadableLogRecord
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.util.instrumentation import InstrumentationScope

import opamp_otel
from opamp_otel import Agent, Controls

APPLIED = opamp_pb2.RemoteConfigStatuses_APPLIED
FAILED = opamp_pb2.RemoteConfigStatuses_FAILED
LAYER = SdkSettings()
VECTORS = Path(__file__).resolve().parents[3] / "testdata" / "sdk-settings"


def vectors_file(name: str) -> Path:
    """The shared vectors' file, or OPAMP_SETTINGS_VECTORS's where it has one (other, generated cases)."""
    other = os.environ.get("OPAMP_SETTINGS_VECTORS")
    if other and (Path(other) / f"{name}.json").exists():
        return Path(other) / f"{name}.json"
    return VECTORS / f"{name}.json"

#: 2026-10-14T12:00:00Z, the vectors' clock.
NOW = 1_791_979_200_000
#: Every live setting, as the package declares them where its Configurator built the SDK with
#: OTLP exporters (test_the_package_declares_every_setting_it_carries_out).
LIVE = ["custom", "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
        "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs", "send_logs_level",
        "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces"]
HOST = opamp_otel.sdk_host(["otlp"])


@pytest.fixture(autouse=True)
def _own_state_home(tmp_path_factory, monkeypatch):
    home = tmp_path_factory.mktemp("state-home")
    monkeypatch.setenv("XDG_STATE_HOME", str(home))
    monkeypatch.setenv("HOME", str(home))


def vectors(name: str) -> list[dict]:
    return json.loads(vectors_file(name).read_text())["cases"]


def ids(cases: list[dict]) -> list[str]:
    return [c["name"] for c in cases]


def document(settings) -> bytes:
    return settings if isinstance(settings, bytes) else json.dumps(settings).encode()


def config(body) -> opamp_pb2.AgentRemoteConfig:
    body = document(body)
    m = opamp_pb2.AgentConfigMap()
    m.config_map["elastic"].CopyFrom(opamp_pb2.AgentConfigObject(body=body, content_type="application/json"))
    return opamp_pb2.AgentRemoteConfig(config=m, config_hash=body)


def typed(settings, now: int = NOW, host: settings_pb2.Host = HOST) -> settings_pb2.Settings:
    """The settings as the layer hands them to this package, for a server's document."""
    d = LAYER.decide(config(settings), b"{}", b"{}", host, now)
    assert d.status.status == APPLIED, d.status.error_message
    return d.apply


def controlled(settings, now: int = NOW, **kwargs) -> Controls:
    c = Controls(**kwargs)
    c.apply(typed(settings, now))
    return c


class Recording:
    """An exporter that records what reaches it and answers `result`."""

    def __init__(self, result="exported") -> None:
        self.batches: list = []
        self.result = result

    def export(self, batch, *args, **kwargs):
        self.batches.append(list(batch) if isinstance(batch, (list, tuple)) else batch)
        if isinstance(self.result, BaseException):
            raise self.result
        return self.result

    def shutdown(self, *args, **kwargs):
        pass

    def force_flush(self, *args, **kwargs):
        return True


def record(scope: str | None = "app", severity: int | None = 9, attributes=None) -> ReadableLogRecord:
    number = None if severity is None else SeverityNumber(severity)
    return ReadableLogRecord(LogRecord(body="m", severity_number=number, attributes=attributes),
                             Resource.create({}),
                             InstrumentationScope(scope) if scope is not None else None)


def exported(c: Controls, batch: list) -> list:
    """What the log gate passes on of `batch`."""
    inner = Recording()
    c.logs(inner).export(batch)
    return inner.batches[-1] if inner.batches else []


# --- send_logs_level_by_scope ---------------------------------------------------------------


@pytest.mark.parametrize("case", vectors("logs"), ids=ids(vectors("logs")))
def test_the_log_gate_keeps_what_the_shared_vectors_keep(case):
    c = controlled(case["settings"], case.get("now_unix_ms", NOW))
    r = record(case["record"]["scope"], case["record"]["severity"])
    assert (exported(c, [r]) == [r]) == case["kept"]


def test_a_scope_is_the_python_logger_a_record_is_exported_under():
    import logging
    import warnings

    from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
    from opentelemetry.sdk._logs.export import SimpleLogRecordProcessor

    c = controlled({"send_logs_level": "warn", "send_logs_level_by_scope": "app.db*=debug,urllib3=off"})
    inner = Recording()
    provider = LoggerProvider()
    provider.add_log_record_processor(SimpleLogRecordProcessor(c.logs(inner)))
    with warnings.catch_warnings():  # the SDK's own handler, deprecated for instrumentation-logging's
        warnings.simplefilter("ignore", DeprecationWarning)
        handler = LoggingHandler(logger_provider=provider)
    for name in ("app.db.pool", "app.web", "urllib3"):
        log = logging.getLogger(name)
        log.addHandler(handler)
        log.setLevel(logging.DEBUG)
        log.propagate = False
        log.debug("d")
        log.error("e")
        log.removeHandler(handler)
    got = [(r.instrumentation_scope.name, r.log_record.severity_text) for batch in inner.batches for r in batch]
    assert got == [("app.db.pool", "DEBUG"), ("app.db.pool", "ERROR"), ("app.web", "ERROR")]


def test_scope_levels_change_live_and_revert_when_withdrawn():
    c = Controls()
    db, web = record("app.db", 5), record("app.web", 5)
    assert exported(c, [db, web]) == [db, web]
    c.apply(typed({"send_logs_level": "warn", "send_logs_level_by_scope": "app.db=debug"}))
    assert exported(c, [db, web]) == [db]
    c.apply(typed({"send_logs_level_by_scope": "app.db=off"}))
    assert exported(c, [db, web]) == [web]
    c.apply(typed({}))
    assert c.logs_scope_levels == ()
    assert exported(c, [db, web]) == [db, web]


def test_a_batch_every_scope_level_drops_doesnt_reach_the_exporter():
    c = controlled({"send_logs_level_by_scope": "*=off"})
    inner = Recording()
    c.logs(inner).export([record("a"), record(None, 0)])
    assert inner.batches == []


def test_a_record_with_no_scope_is_the_empty_scope_which_only_star_matches():
    unnamed = record(None, 5)
    assert exported(controlled({"send_logs_level_by_scope": "*=error"}), [unnamed]) == []
    assert exported(controlled({"send_logs_level_by_scope": "a*=error"}), [unnamed]) == [unnamed]
    # Severity unset is 0: kept under a level, dropped under off.
    unset = record("x", None)
    assert exported(controlled({"send_logs_level_by_scope": "x=fatal"}), [unset]) == [unset]
    assert exported(controlled({"send_logs_level_by_scope": "x=off"}), [unset]) == []


def test_scope_levels_are_declared_only_where_the_package_built_the_sdk(tmp_path):
    doc = '{"send_logs_level_by_scope":"app=off"}'
    agent = Agent()
    agent.prepare({"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp", "OTEL_OPAMP_STATE_DIR": str(tmp_path)})
    assert "send_logs_level_by_scope" in agent.host.live
    s = agent.remote_config(config(doc.encode()))
    assert s.status == APPLIED and agent.controls.logs_scope_levels == (("app", 0xFFFF),)
    assert json.loads(agent.effective.config_map.config_map["elastic"].body) == {"send_logs_level_by_scope": "app=off"}
    # Another configurator's SDK: not this package's gates, so ignored, never reported in force.
    other = opamp_otel.sdk_host(["otlp"], configured=False)
    assert "send_logs_level_by_scope" not in other.live
    d = LAYER.decide(config(doc.encode()), b"{}", b"{}", other, NOW)
    assert d.status.status == APPLIED and not d.apply.send_logs_scope_levels
    assert "send_logs_level_by_scope" in d.apply.unsupported


def test_a_bad_scope_level_is_failed_and_changes_nothing(tmp_path):
    agent = Agent()
    agent.prepare({"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp", "OTEL_OPAMP_STATE_DIR": str(tmp_path)})
    agent.remote_config(config(b'{"send_logs_level_by_scope":"app=error"}'))
    for bad in ("app=loud", "a=debug,a=info", "a b=info"):
        s = agent.remote_config(config(json.dumps({"send_logs_level_by_scope": bad}).encode()))
        assert s.status == FAILED and s.error_message.startswith("send_logs_level_by_scope:"), bad
        assert agent.controls.logs_scope_levels == (("app", 17),)


# --- the rate caps -----------------------------------------------------------------------------


class Clock:
    """A monotonic clock in nanoseconds that a test moves."""

    def __init__(self, ns: int = 0) -> None:
        self.ns = ns

    def __call__(self) -> int:
        return self.ns


@pytest.mark.parametrize("case", [c for c in vectors("caps") if c["signal"] == "logs"],
                         ids=ids([c for c in vectors("caps") if c["signal"] == "logs"]))
def test_the_log_cap_passes_what_the_shared_vectors_pass(case):
    clock = Clock()
    c = controlled(case["settings"], case.get("now_unix_ms", NOW), clock=clock)
    inner = Recording()
    gate = c.logs(inner)
    one = [record("app", 9)]  # the same record, as many times as a batch has
    passed = []
    for event in case["events"]:
        clock.ns = event["at_ns"]
        batch = one * event["records"]
        before = len(inner.batches)
        gate.export(batch)
        got = inner.batches[-1] if len(inner.batches) > before else []
        assert got == batch[:len(got)]  # the first ones, in order
        passed.append(len(got))
    assert passed == case["want"]


def test_records_the_levels_drop_take_no_tokens_and_a_change_of_cap_starts_full():
    clock = Clock()
    c = controlled({"send_logs_level": "warn", "send_logs_per_second": "2"}, clock=clock)
    debug, warn = record("app", 5), record("app", 13)
    assert exported(c, [debug, debug, debug, warn]) == [warn]  # one token taken, not four
    assert exported(c, [warn, warn]) == [warn]
    assert exported(c, [warn]) == []
    # The same cap again (a refresh, a repeated config) keeps its bucket: still empty.
    c.apply(typed({"send_logs_level": "warn", "send_logs_per_second": "2"}))
    assert exported(c, [warn]) == []
    # A new cap starts full.
    c.apply(typed({"send_logs_per_second": "3"}))
    assert exported(c, [warn] * 5) == [warn] * 3
    # Removing it: unlimited at once.
    c.apply(typed({}))
    assert c.logs_cap is None
    assert exported(c, [warn] * 50) == [warn] * 50


def test_the_log_cap_refills_on_the_monotonic_clock():
    clock = Clock(10**12)
    c = controlled({"send_logs_per_second": "4"}, clock=clock)
    r = record()
    assert len(exported(c, [r] * 10)) == 4
    clock.ns += 250_000_000
    assert len(exported(c, [r] * 10)) == 1
    clock.ns += 10 * 10**9  # a long quiet spell refills a second's worth, no more
    assert len(exported(c, [r] * 10)) == 4


def test_send_logs_false_drops_before_the_cap_takes_anything():
    clock = Clock()
    c = controlled({"send_logs": "false", "send_logs_per_second": "1"}, clock=clock)
    assert exported(c, [record()]) == []
    c.apply(typed({"send_logs_per_second": "1"}))  # the same cap: its bucket was never touched
    assert len(exported(c, [record(), record()])) == 1


def test_a_bad_cap_is_failed():
    for bad, why in (("0", "0 is refused"), ("-1", "not more than 0"), ("1000001", "not more than 0"), ("x", "not a number")):
        d = LAYER.decide(config({"send_logs_per_second": bad}), b"{}", b"{}", HOST, NOW)
        assert d.status.status == FAILED and why in d.status.error_message, bad


# --- drop_attributes and mask_attributes -------------------------------------------------------


def value(a: dict):
    """A vector's attribute value, as the Python SDK holds it."""
    (kind,) = [k for k in a if k != "key"]
    return tuple(a[kind]) if kind == "array" else a[kind]


def pairs(attributes) -> list:
    return [(k, tuple(v) if isinstance(v, (list, tuple)) else v) for k, v in dict(attributes or {}).items()]


def representable(case: dict) -> bool:
    """Attributes the SDK can hold: no empty key, no key twice (a span's attributes are a dict)."""
    keys = [a["key"] for a in case["attributes"]]
    return all(keys) and len(set(keys)) == len(keys)


REDACT = vectors("redact")
ON_THE_SDK = [c for c in REDACT if representable(c)]


@pytest.mark.parametrize("case", REDACT, ids=ids(REDACT))
def test_the_redactor_leaves_what_the_shared_vectors_leave(case):
    c = controlled(case["settings"])
    got = []
    for a in case["attributes"]:
        what = c.redactor.of(a["key"])
        if what != c.redactor.DROP:
            got.append((a["key"], "[REDACTED]" if what == c.redactor.MASK else value(a)))
    assert got == [(w["key"], value(w)) for w in case["want"]]


@pytest.mark.parametrize("case", ON_THE_SDK, ids=ids(ON_THE_SDK))
def test_spans_their_events_and_links_are_redacted_as_the_shared_vectors_say(case):
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
    from opentelemetry.trace import Link

    c = controlled(case["settings"])
    gated, plain = InMemorySpanExporter(), InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(c.spans(gated)))
    provider.add_span_processor(SimpleSpanProcessor(plain))  # an exporter the gate doesn't cover
    attributes = {a["key"]: value(a) for a in case["attributes"]}
    tracer = provider.get_tracer("t")
    linked = tracer.start_span("linked")
    span = tracer.start_span("s", attributes=attributes, links=[Link(linked.get_span_context(), attributes)])
    span.add_event("e", attributes)
    span.end()
    want = [(w["key"], value(w)) for w in case["want"]]
    [out] = gated.get_finished_spans()
    assert pairs(out.attributes) == want
    assert pairs(out.events[0].attributes) == want
    assert pairs(out.links[0].attributes) == want
    # The span itself is unchanged, for the exporter the gate doesn't cover.
    [original] = plain.get_finished_spans()
    assert pairs(span.attributes) == pairs(attributes)
    assert pairs(original.attributes) == pairs(attributes)
    assert pairs(original.events[0].attributes) == pairs(attributes)
    assert pairs(original.links[0].attributes) == pairs(attributes)


@pytest.mark.parametrize("case", ON_THE_SDK, ids=ids(ON_THE_SDK))
def test_log_records_are_redacted_as_the_shared_vectors_say(case):
    c = controlled(case["settings"])
    attributes = {a["key"]: value(a) for a in case["attributes"]}
    r = record("app", 9, attributes)
    [out] = exported(c, [r])
    assert pairs(out.log_record.attributes) == [(w["key"], value(w)) for w in case["want"]]
    assert pairs(r.log_record.attributes) == pairs(attributes)  # the shared record: as it was


def test_redaction_keeps_the_rest_of_the_span_and_what_the_sdks_limits_dropped():
    from opentelemetry.sdk.trace import SpanLimits, TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
    from opentelemetry.trace import SpanKind, Status, StatusCode

    c = controlled({"drop_attributes": "secret", "mask_attributes": "user.email"})
    out = InMemorySpanExporter()
    provider = TracerProvider(span_limits=SpanLimits(max_span_attributes=3))
    provider.add_span_processor(SimpleSpanProcessor(c.spans(out)))
    with provider.get_tracer("t", "1.2").start_as_current_span("parent"):
        span = provider.get_tracer("t", "1.2").start_span("GET /users", kind=SpanKind.SERVER)
        for k in ("a", "b", "secret", "user.email"):  # four into three: "a" falls out, by the SDK's limit
            span.set_attribute(k, "x")
        span.set_status(Status(StatusCode.ERROR, "boom"))
        span.end()
    got = out.get_finished_spans()[0]
    assert dict(got.attributes) == {"b": "x", "user.email": "[REDACTED]"}
    assert got.dropped_attributes == 1  # the SDK's drop, not the redaction's
    for name in ("name", "kind", "parent", "resource", "start_time", "end_time", "instrumentation_scope"):
        assert getattr(got, name) == getattr(span, name), name
    assert got.get_span_context() == span.get_span_context()
    assert (got.status.status_code, got.status.description) == (StatusCode.ERROR, "boom")
    # It still encodes to OTLP as the exporter would.
    from opentelemetry.exporter.otlp.proto.common.trace_encoder import encode_spans

    encoded = encode_spans([got]).resource_spans[0].scope_spans[0].spans[0]
    assert [kv.key for kv in encoded.attributes] == ["b", "user.email"]
    assert encoded.dropped_attributes_count == 1


def test_a_span_or_record_redaction_doesnt_touch_is_passed_on_as_it_is():
    c = controlled({"drop_attributes": "secret"})
    r = record("app", 9, {"ok": 1})
    assert exported(c, [r])[0] is r


def test_redaction_changes_live_and_reverts_when_withdrawn():
    r = record("app", 9, {"user.email": "a@b.c", "token": "t"})
    c = Controls()
    assert pairs(exported(c, [r])[0].log_record.attributes) == [("user.email", "a@b.c"), ("token", "t")]
    c.apply(typed({"mask_attributes": "USER.*", "drop_attributes": "token"}))
    assert pairs(exported(c, [r])[0].log_record.attributes) == [("user.email", "[REDACTED]")]
    c.apply(typed({"drop_attributes": "*"}))
    assert pairs(exported(c, [r])[0].log_record.attributes) == []
    c.apply(typed({}))
    assert not c.redactor
    assert exported(c, [r])[0] is r


def exports_at_every_line_of_apply(c: Controls, settings: settings_pb2.Settings, export) -> int:
    """c.apply(settings), calling export() before each line of Controls.apply runs: a thread
    switch there, as the GIL allows, with a batch processor's thread exporting. How many ran."""
    import sys  # noqa: PLC0415

    ran = 0

    def line(frame, event, arg):
        nonlocal ran
        if event == "line":
            ran += 1
            export()
        return line

    def call(frame, event, arg):
        return line if frame.f_code is Controls.apply.__code__ else None

    was = sys.gettrace()
    sys.settrace(call)
    try:
        c.apply(settings)
    finally:
        sys.settrace(was)
    return ran


@pytest.mark.parametrize("signal", ["traces", "logs"])
def test_a_signal_turned_back_on_with_redaction_never_exports_the_key_while_the_document_is_applied(signal):
    """From {send_X: false} to {send_X: true, drop_attributes: *password*}, an export in the middle
    of apply is held to one document or the other, never to B's send and A's redaction: the
    password is exported under neither."""
    from opentelemetry.sdk.trace import ReadableSpan  # noqa: PLC0415

    key = "send_traces" if signal == "traces" else "send_logs"
    c = controlled({key: "false"})
    inner = Recording()
    if signal == "traces":
        gate = c.spans(inner)
        def item(): return ReadableSpan(name="s", attributes={"user.password": "hunter2", "ok": 1})
        def attributes(x): return dict(x.attributes)
    else:
        gate = c.logs(inner)
        def item(): return record(attributes={"user.password": "hunter2", "ok": 1})
        def attributes(x): return dict(x.log_record.attributes)
    ran = exports_at_every_line_of_apply(c, typed({key: "true", "drop_attributes": "*password*"}),
                                         lambda: gate.export([item()]))
    assert ran >= 5  # an export between each of apply's steps
    sent = [attributes(x) for b in inner.batches for x in b]
    assert not any("user.password" in a for a in sent), sent
    gate.export([item()])  # B in force
    assert attributes(inner.batches[-1][0]) == {"ok": 1}


def test_log_bodies_resource_and_scope_attributes_are_out_of_scope():
    c = controlled({"mask_attributes": "*"})
    r = ReadableLogRecord(LogRecord(body={"password": "p"}, severity_number=SeverityNumber.INFO,
                                    attributes={"password": "p"}),
                          Resource.create({"password": "p"}), InstrumentationScope("app", attributes={"password": "p"}))
    [out] = exported(c, [r])
    assert dict(out.log_record.attributes) == {"password": "[REDACTED]"}
    assert out.log_record.body == {"password": "p"}
    assert out.resource.attributes["password"] == "p"
    assert out.instrumentation_scope.attributes["password"] == "p"


def test_redaction_after_the_levels_and_the_cap():
    clock = Clock()
    c = controlled({"send_logs_level": "warn", "send_logs_per_second": "1", "mask_attributes": "k"}, clock=clock)
    low, high = record("app", 5, {"k": "v"}), record("app", 13, {"k": "v"})
    out = exported(c, [low, high, high])
    assert len(out) == 1 and dict(out[0].log_record.attributes) == {"k": "[REDACTED]"}


# --- sampling_rules and sampling_traces_per_second ----------------------------------------------


class AppSampler:
    """The application's own sampler, recognisable by its decision (RECORD_ONLY), or `sampled`."""

    def __init__(self, sampled: bool | None = None) -> None:
        self.calls = 0
        self.sampled = sampled

    def should_sample(self, parent_context, trace_id, name, kind=None, attributes=None, links=None, trace_state=None):
        from opentelemetry.sdk.trace.sampling import Decision, SamplingResult

        self.calls += 1
        if self.sampled is None:
            return SamplingResult(Decision.RECORD_ONLY)
        return SamplingResult(Decision.RECORD_AND_SAMPLE if self.sampled else Decision.DROP)

    def get_description(self) -> str:
        return "AppSampler"


KINDS = {"internal": 0, "server": 1, "client": 2, "producer": 3, "consumer": 4}


def parent_context(parent: str):
    """A context whose span is the vector's parent: none, local_ or remote_, sampled or unsampled."""
    from opentelemetry import trace
    from opentelemetry.trace import NonRecordingSpan, SpanContext, TraceFlags

    if parent == "none":
        return trace.set_span_in_context(trace.INVALID_SPAN)
    where, sampled = parent.split("_")
    ctx = SpanContext(0x1234, 0x99, is_remote=where == "remote",
                      trace_flags=TraceFlags(TraceFlags.SAMPLED if sampled == "sampled" else 0))
    return trace.set_span_in_context(NonRecordingSpan(ctx))


def span_attributes(span: dict) -> dict:
    out: dict = {}
    for a in span["attributes"]:
        out.setdefault(a["key"], value(a))  # the first of a key counts
    return out


def decide(c: Controls, span: dict, trace_id: int):
    from opentelemetry.trace import SpanKind

    return c.sampler.should_sample(parent_context(span["parent"]), trace_id, span["name"], SpanKind(KINDS[span["kind"]]),
                                   span_attributes(span))


TRACE_IDS = [0, 1, 2**126, 2**127 - 1, 2**127, 2**128 - 1] + [(i * 0x9E3779B97F4A7C15F39CC0605CEDC835) % 2**128
                                                                for i in range(1, 200)]
SAMPLING = vectors("sampling")


@pytest.mark.parametrize("case", SAMPLING, ids=ids(SAMPLING))
def test_the_sampler_decides_as_the_shared_vectors_say(case):
    from opentelemetry.sdk.trace.sampling import Decision, TraceIdRatioBased
    from opentelemetry.trace import SpanKind, get_current_span

    app = AppSampler()
    c = controlled(case["settings"], case.get("now_unix_ms", NOW))
    c.sampler.wrap(app)
    want, span = case["want"], case["span"]
    for trace_id in TRACE_IDS:
        got = decide(c, span, trace_id).decision
        if want["how"] == "app_sampler":
            assert got == Decision.RECORD_ONLY
        elif want["how"] == "follow_parent":
            assert got.is_sampled() == want["sampled"]
        else:
            ratio = TraceIdRatioBased(want["rate"]).should_sample(None, trace_id, "x").decision
            assert got == ratio, trace_id
    if want["how"] == "follow_parent":
        assert app.calls == 0  # the rules or rate decide, so the parent does: never the app's sampler
    if want["how"] == "rate":
        parent = get_current_span(parent_context(span["parent"])).get_span_context()
        _, rule = c.sampler._choose(c.sampler._sampling, parent, span["name"], SpanKind(KINDS[span["kind"]]),
                                    span_attributes(span))
        assert rule == want["rule"]


TRACE_CAPS = [c for c in vectors("caps") if c["signal"] == "traces"]


@pytest.mark.parametrize("case", TRACE_CAPS, ids=ids(TRACE_CAPS))
def test_the_trace_cap_passes_what_the_shared_vectors_pass(case):
    clock = Clock()
    c = controlled(case["settings"], case.get("now_unix_ms", NOW), clock=clock)
    app = AppSampler()
    c.sampler.wrap(app)
    span = {"name": "s", "kind": "server", "attributes": []}
    passed = []
    for event in case["events"]:
        clock.ns = event["at_ns"]
        app.sampled = event["sampled"]  # what the rules or the rate decided, for a root
        passed.append(int(decide(c, {**span, "parent": event["parent"]}, 7).decision.is_sampled()))
    assert passed == case["want"]


def test_a_rate_of_a_half_samples_half_of_10k_trace_ids():
    import random

    from opentelemetry.trace import SpanKind

    c = controlled({"sampling_rules": [{"rate": 0.5, "kind": "server"}], "sampling_rate": "0"})
    rng = random.Random(7)
    root = parent_context("none")
    sampled = sum(c.sampler.should_sample(root, rng.getrandbits(128), "GET", SpanKind.SERVER).decision.is_sampled()
                  for _ in range(10_000))
    assert 4_700 <= sampled <= 5_300
    # Rates 0 and 1 are exact.
    for rate, want in ((0, 0), (1, 1000)):
        c.apply(typed({"sampling_rules": [{"rate": rate}]}))
        assert sum(c.sampler.should_sample(root, rng.getrandbits(128), "x").decision.is_sampled()
                   for _ in range(1000)) == want


def test_on_a_real_sdk_a_rule_drops_health_checks_and_their_children_follow():
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
    from opentelemetry.trace import SpanKind

    c = controlled({"sampling_rate": "1", "sampling_rules": [{"rate": 0, "kind": "server", "path": "/health*"}]})
    out = InMemorySpanExporter()
    provider = TracerProvider(sampler=c.sampler.wrap(None))
    provider.add_span_processor(SimpleSpanProcessor(c.spans(out)))
    t = provider.get_tracer("t")
    # ASGI and WSGI instrumentations set http.target (old semantic conventions) at start.
    for target in ("/healthz?probe=1", "/checkout"):
        with t.start_as_current_span("GET", kind=SpanKind.SERVER, attributes={"http.target": target}):
            with t.start_as_current_span("db"):
                pass
    assert sorted(s.name for s in out.get_finished_spans()) == ["GET", "db"]
    assert [s.attributes.get("http.target") for s in out.get_finished_spans() if s.name == "GET"] == ["/checkout"]


def test_removing_the_rules_restores_the_apps_own_sampler():
    from opentelemetry.sdk.trace.sampling import Decision

    app = AppSampler()
    c = Controls()
    c.sampler.wrap(app)
    root = parent_context("none")
    assert c.sampler.should_sample(root, 1, "GET").decision == Decision.RECORD_ONLY
    c.apply(typed({"sampling_rules": [{"rate": 0}]}))
    assert c.sampler.should_sample(root, 1, "GET").decision == Decision.DROP
    c.apply(typed({}))
    assert c.sampler.should_sample(root, 1, "GET").decision == Decision.RECORD_ONLY
    assert c.sampler.current is app


def test_a_rule_of_rate_0_beside_sampling_rate_1_and_a_rule_with_no_matchers():
    c = controlled({"sampling_rate": "1", "sampling_rules": [{"rate": 0, "name": "noisy*"}, {"rate": 1}]})
    root = parent_context("none")
    assert not c.sampler.should_sample(root, 5, "noisy job").decision.is_sampled()
    assert c.sampler.should_sample(root, 5, "other").decision.is_sampled()
    c.apply(typed({"sampling_rate": "1", "sampling_rules": [{"rate": 0}]}))  # matches every span, before the rate
    assert not c.sampler.should_sample(root, 5, "other").decision.is_sampled()


def test_a_sampled_remote_parent_with_rule_0_is_sampled_and_an_unsampled_one_with_rule_1_is_not():
    c = controlled({"sampling_rules": [{"rate": 0}]})
    assert c.sampler.should_sample(parent_context("remote_sampled"), 5, "GET").decision.is_sampled()
    c.apply(typed({"sampling_rules": [{"rate": 1}]}))
    assert not c.sampler.should_sample(parent_context("remote_unsampled"), 5, "GET").decision.is_sampled()


def test_the_cap_holds_on_a_real_sdk_children_of_a_sampled_root_take_no_token():
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    clock = Clock()
    c = controlled({"sampling_rate": "1", "sampling_traces_per_second": "2"}, clock=clock)
    out = InMemorySpanExporter()
    provider = TracerProvider(sampler=c.sampler.wrap(None))
    provider.add_span_processor(SimpleSpanProcessor(out))
    t = provider.get_tracer("t")
    for i in range(5):
        with t.start_as_current_span(f"root{i}"):
            for j in range(3):
                t.start_span(f"child{i}.{j}").end()
    names = sorted(s.name for s in out.get_finished_spans())
    assert names == sorted(["root0", "root1", "child0.0", "child0.1", "child0.2", "child1.0", "child1.1", "child1.2"])
    clock.ns += 500_000_000  # half a second: one more
    for i in range(5, 8):
        t.start_span(f"root{i}").end()
    assert sum(s.name.startswith("root") for s in out.get_finished_spans()) == 3
    # A new cap starts full; removing it is unlimited at once.
    c.apply(typed({"sampling_rate": "1", "sampling_traces_per_second": "3"}))
    assert sum(c.sampler.should_sample(parent_context("none"), 1, "r").decision.is_sampled() for _ in range(5)) == 3
    c.apply(typed({"sampling_rate": "1"}))
    assert sum(c.sampler.should_sample(parent_context("none"), 1, "r").decision.is_sampled() for _ in range(50)) == 50


def test_the_cap_counts_only_the_traces_the_rules_or_rate_sampled():
    clock = Clock()
    c = controlled({"sampling_rules": [{"rate": 0, "name": "drop*"}], "sampling_rate": "1",
                    "sampling_traces_per_second": "1"}, clock=clock)
    root = parent_context("none")
    for _ in range(10):
        assert not c.sampler.should_sample(root, 1, "drop me").decision.is_sampled()
    assert c.sampler.should_sample(root, 1, "keep").decision.is_sampled()  # the token is still there
    assert not c.sampler.should_sample(root, 1, "keep").decision.is_sampled()


def test_bad_rules_are_failed_naming_the_rule():
    for rules, why in (([{"rate": 7}], "sampling_rules[0].rate: 7 is not between 0 and 1"),
                       ([{"rate": 1}, {"rate": 1, "route": "/x"}], "sampling_rules[1]"),
                       ([{"rate": 1, "kind": "sideways"}], "sampling_rules[0].kind"),
                       ([{"name": "x"}], "sampling_rules[0]")):
        d = LAYER.decide(config({"sampling_rules": rules}), b"{}", b"{}", HOST, NOW)
        assert d.status.status == FAILED and why in d.status.error_message, (rules, d.status.error_message)


def test_rules_and_the_cap_are_reported_in_force_and_in_the_samplers_description(tmp_path):
    agent = Agent()
    agent.prepare({"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp", "OTEL_OPAMP_STATE_DIR": str(tmp_path)})
    s = agent.remote_config(config(b'{"sampling_rules":"[{\\"rate\\": 0, \\"kind\\": \\"server\\", \\"path\\": \\"/health*\\"}]",'
                                   b'"sampling_traces_per_second":"50"}'))
    assert s.status == APPLIED
    eff = json.loads(agent.effective.config_map.config_map["elastic"].body)
    assert eff == {"sampling_rules": '[{"kind":"server","path":"/health*","rate":0}]', "sampling_traces_per_second": "50"}
    assert agent.controls.sampler.get_description() == "OpampLiveSampler{ParentBased{root:AlwaysOnSampler,remoteParentSampled:AlwaysOnSampler,remoteParentNotSampled:AlwaysOffSampler,localParentSampled:AlwaysOnSampler,localParentNotSampled:AlwaysOffSampler},rules=1,traces_per_second=50.0}"


# --- debug mode: the refresh timer ---------------------------------------------------------------


class Clocks:
    """An agent's wall clock (ms since the epoch) and monotonic clock (s), moved together by a test."""

    def __init__(self, wall_ms: int = NOW) -> None:
        self.wall = wall_ms
        self.mono = 1000.0

    def advance(self, ms: int) -> None:
        self.wall += ms
        self.mono += ms / 1000


def agent_at(clocks: Clocks, state, **env) -> Agent:
    """An agent prepared as the Distro prepares it, its clocks the test's."""
    agent = Agent()
    agent.wall_ms = lambda: clocks.wall
    agent.monotonic = lambda: clocks.mono
    agent.prepare({"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp", "OTEL_OPAMP_STATE_DIR": str(state), **env})
    return agent


def eff(agent: Agent) -> dict:
    return json.loads(agent.effective.config_map.config_map["elastic"].body or b"{}")


def set_aside(agent: Agent) -> dict | None:
    """What the agent's effective config names as set aside (its elastic.set_aside entry)."""
    files = agent.effective.config_map.config_map
    return json.loads(files["elastic.set_aside"].body) if "elastic.set_aside" in files else None


DEBUG = vectors("debug") + vectors("saving")


@contextlib.contextmanager
def failing_save(state: Path, fail: bool):
    """While it lasts, if `fail`, the settings file can't be saved: a directory, not empty, stands
    where it goes. What was saved before is put back after."""
    if not fail:
        yield
        return
    path, kept = state / "sdk-settings.json", state / "sdk-settings.kept"
    if path.exists():
        path.rename(kept)
    (path / "in-the-way").mkdir(parents=True)
    try:
        yield
    finally:
        shutil.rmtree(path)
        if kept.exists():
            kept.rename(path)


@pytest.mark.parametrize("case", DEBUG, ids=ids(DEBUG))
def test_debug_mode_and_saving_over_time_are_what_the_shared_vectors_say_through_the_agent(case, tmp_path):
    clocks = Clocks()
    nowhere = any(step.get("save") == "nowhere" for step in case["steps"])
    state = "none" if nowhere else tmp_path
    agent = agent_at(clocks, state)
    for step in case["steps"]:
        clocks.wall = step["now_unix_ms"]
        if step["call"] == "decide":
            with failing_save(tmp_path, step.get("save") == "failed"):
                status = agent.remote_config(config(step["document"].encode()))
            want_status = {"applied": APPLIED, "failed": FAILED}[step["want"].get("status", "applied")]
            assert status.status == want_status, status.error_message
            assert status.error_message.startswith(step["want"].get("error", "")), status.error_message
        elif step["call"] == "load":
            agent = agent_at(clocks, state)  # a restart
        else:
            agent._refresh(step["now_unix_ms"])
        want = step["want"]
        if "sampling_rate" in want:
            assert agent.controls.sampler._rate == want["sampling_rate"]
        if "refresh_at_unix_ms" in want:
            assert agent._refresh_at == want["refresh_at_unix_ms"]
        if "effective" in want:
            assert eff(agent) == want["effective"]
        if "set_aside" in want:
            assert set_aside(agent) == want["set_aside"]


def test_the_timer_ends_debug_mode_at_its_deadline_and_reports_it(tmp_path):
    clocks = Clocks()
    agent = agent_at(clocks, tmp_path)
    reported = []
    agent.client = type("Client", (), {"set_effective_config": lambda self, e: reported.append(e)})()
    agent.remote_config(config({"sampling_rate": "0.1", "send_logs_level": "error",
                                "send_logs_level_by_scope": "noisy=off", "sampling_rules": [{"rate": 0}],
                                "debug_until": "2026-10-14T12:10:00Z", "debug_send_logs_level": "info"}))
    c = agent.controls
    assert (c.sampler._rate, c.logs_min_severity, c.logs_scope_levels, c.sampler._rules) == (1, 9, (), ())
    assert agent._tick() == pytest.approx(600)
    clocks.advance(599_999)
    assert agent._tick() == pytest.approx(0.001) and c.sampler._rate == 1  # still in debug: half-open
    clocks.advance(1)
    assert agent._tick() is None  # due: refreshed, nothing more to wait for
    assert (c.sampler._rate, c.logs_min_severity, c.logs_scope_levels) == (0.1, 17, (("noisy", 0xFFFF),))
    assert len(c.sampler._rules) == 1
    assert eff(agent) == {"sampling_rate": "0.1", "send_logs_level": "error", "send_logs_level_by_scope": "noisy=off",
                          "sampling_rules": '[{"rate":0}]'}
    assert reported and reported[-1] is agent.effective


def test_a_wall_clock_set_back_doesnt_hold_debug_mode_open(tmp_path):
    clocks = Clocks()
    agent = agent_at(clocks, tmp_path)
    agent.remote_config(config({"sampling_rate": "0.1", "debug_until": "2026-10-14T12:10:00Z"}))
    clocks.mono += 600  # ten minutes pass...
    clocks.wall -= 3_600_000  # ...while the wall clock is set back an hour
    agent._tick()
    assert agent.controls.sampler._rate == 0.1 and agent._refresh_at == 0


def test_a_failed_config_after_the_wall_clock_passed_debug_until_ends_debug_mode(tmp_path):
    clocks = Clocks()
    agent = agent_at(clocks, tmp_path)
    agent.remote_config(config({"sampling_rate": "0.1", "debug_until": "2026-10-14T12:10:00Z"}))
    assert agent.controls.sampler._rate == 1
    # The machine sleeps: the wall clock passes debug_until, the monotonic one doesn't move, so
    # the timer hasn't gone off. Then a bad document comes.
    clocks.wall += 15 * 60_000
    s = agent.remote_config(config({"sampling_rate": "7"}))
    assert s.status == FAILED
    # What is in force is what is reported: debug mode is over.
    assert eff(agent) == {"sampling_rate": "0.1"}
    assert agent.controls.sampler._rate == 0.1 and agent._refresh_at == 0


def test_a_newer_decision_replaces_the_timer_so_the_old_one_does_nothing(tmp_path):
    clocks = Clocks()
    agent = agent_at(clocks, tmp_path)
    agent.remote_config(config({"sampling_rate": "0.1", "debug_until": "2026-10-14T12:10:00Z"}))
    clocks.advance(60_000)
    agent.remote_config(config({"sampling_rate": "0.2", "debug_until": "2026-10-14T12:30:00Z"}))
    clocks.advance(540_000)  # the first window's end: nothing happens
    agent._tick()
    assert agent.controls.sampler._rate == 1 and agent._refresh_at == NOW + 30 * 60_000
    agent.remote_config(config({"sampling_rate": "0.2"}))  # no debug_until: debug ends at once
    assert agent._refresh_deadline is None and agent._tick() is None
    assert agent.controls.sampler._rate == 0.2


def test_caps_redaction_and_send_switches_stay_in_debug_mode(tmp_path):
    clocks = Clocks()
    agent = agent_at(clocks, tmp_path)
    agent.remote_config(config({"send_logs": "false", "mask_attributes": "k", "send_logs_per_second": "5",
                                "sampling_traces_per_second": "7", "debug_until": "2026-10-14T12:10:00Z"}))
    c = agent.controls
    assert (c.send_logs, c.redactor.mask, c.logs_cap.per_second, c.sampler._cap.per_second) == (False, ("k",), 5, 7)


def test_debug_mode_is_ignored_where_the_package_didnt_build_the_sdk():
    other = opamp_otel.sdk_host(["otlp"], configured=False)
    d = LAYER.decide(config({"debug_until": "2026-10-14T12:10:00Z"}), b"{}", b"{}", other, NOW)
    assert d.status.status == APPLIED and "debug_until" in d.apply.unsupported and not d.refresh_at_unix_ms


def test_a_debug_window_too_far_ahead_is_failed(tmp_path):
    agent = agent_at(Clocks(), tmp_path)
    s = agent.remote_config(config({"debug_until": "2026-10-16T12:00:01Z"}))
    assert s.status == FAILED and "more than 24h ahead" in s.error_message


def test_the_timer_thread_ends_debug_mode_on_its_own():
    """The real timer thread, on the real clocks: a window a fifth of a second long."""
    import datetime
    import threading
    import time

    agent = Agent()
    agent.prepare({"OTEL_OPAMP_ENDPOINT": "http://s/v1/opamp", "OTEL_OPAMP_STATE_DIR": "none"})
    end = datetime.datetime.fromtimestamp(time.time() + 0.2, datetime.UTC).isoformat(timespec="milliseconds")
    agent.remote_config(config({"sampling_rate": "0.1", "debug_until": end.replace("+00:00", "Z")}))
    assert agent.controls.sampler._rate == 1
    agent._ticking = True
    timer = threading.Thread(target=agent._timer, daemon=True)
    timer.start()
    try:
        deadline = time.monotonic() + 5
        while agent.controls.sampler._rate != 0.1 and time.monotonic() < deadline:
            time.sleep(0.01)
        assert agent.controls.sampler._rate == 0.1 and agent._refresh_at == 0
    finally:
        agent.stop()
        timer.join(5)
    assert not timer.is_alive()


def test_a_saved_debug_window_ends_when_the_client_cant_start(tmp_path, caplog):
    """The document saved last run, in force from the start, ends its debug window on its own even
    when the client can't start: the timer runs whether or not there is a client."""
    import datetime
    import time

    end = datetime.datetime.fromtimestamp(time.time() + 0.3, datetime.UTC).isoformat(timespec="milliseconds")
    (tmp_path / "sdk-settings.json").write_text(json.dumps({"sampling_rate": "0.1",
                                                            "debug_until": end.replace("+00:00", "Z")}))
    agent = Agent()
    # The core refuses the scheme: the client doesn't start.
    agent.prepare({"OTEL_OPAMP_ENDPOINT": "ftp://s/v1/opamp", "OTEL_OPAMP_STATE_DIR": str(tmp_path)})
    assert agent.controls.sampler._rate == 1  # debug mode, from the saved document
    agent.configured = True
    try:
        agent.start(Resource.create({}))
        assert agent.client is None and "the client did not start" in caplog.text
        deadline = time.monotonic() + 5
        while agent.controls.sampler._rate != 0.1 and time.monotonic() < deadline:
            time.sleep(0.01)
        assert agent.controls.sampler._rate == 0.1 and agent._refresh_at == 0
    finally:
        agent.stop()


# --- export health ---------------------------------------------------------------------------------


HEALTH = vectors("health")


def counted(c: Controls, counts: dict) -> None:
    """Put the step's counts in the gates' counters, as if the gates had counted them."""
    for signal, gate in (("traces", c.traces), ("metrics", c.metrics), ("logs", c.logs_counts)):
        gate._c = settings_pb2.SignalCounts(**counts.get(signal, {}))


def attributes_of(health) -> dict:
    return {kv.key: kv.value.int_value for kv in health.attributes}


@pytest.mark.parametrize("case", HEALTH, ids=ids(HEALTH))
def test_export_health_is_what_the_shared_vectors_say_through_the_agent(case, tmp_path):
    clocks = Clocks()
    agent = agent_at(clocks, tmp_path)
    for step in case["steps"]:
        clocks.wall = step["now_unix_ms"]
        agent._put_in_force(typed(step.get("settings", {}), step["now_unix_ms"]))
        counted(agent.controls, step.get("counts", {}))
        with agent._lock:
            report = agent._export_health()
        want = step["want"]
        if "send" in want:
            assert report.send == want["send"]
        if "sent_unix_ms" in want:
            assert report.sent_unix_ms == want["sent_unix_ms"]
        e = report.exporters
        for field in ("healthy", "status", "last_error", "start_time_unix_nano", "status_time_unix_nano"):
            if field in want:
                assert getattr(e, field) == want[field], field
        for signal, w in want.get("signals", {}).items():
            child = e.component_health_map[signal]
            for field in ("healthy", "status", "last_error", "status_time_unix_nano"):
                if field in w:
                    assert getattr(child, field) == w[field], (signal, field)
            for k, n in w.get("counters", {}).items():
                assert attributes_of(child)[k] == n, (signal, k)


class FakeClient:
    def __init__(self) -> None:
        self.health: list = []
        self.effective: list = []

    def set_health(self, h) -> None:
        self.health.append(h)

    def set_effective_config(self, e) -> None:
        self.effective.append(e)


def test_the_gates_count_exports_failures_and_what_they_drop_on_purpose():
    from opentelemetry.sdk._logs.export import LogRecordExportResult
    from opentelemetry.sdk.trace.export import SpanExportResult

    c = controlled({"send_logs_level": "warn"})
    logs = c.logs(Recording(LogRecordExportResult.SUCCESS))
    logs.export([record("a", 5), record("a", 13), record("a", 17)])
    failing = c.logs(Recording(LogRecordExportResult.FAILURE))
    failing.export([record("a", 13)])
    got = c.logs_counts.proto()
    assert (got.exports_ok, got.exports_failed, got.items_failed, got.items_filtered, got.last_failed) == (1, 1, 1, 1, True)
    assert got.last_error == opamp_otel.Counts.FAILURE
    logs.export([record("a", 13)])
    assert not c.logs_counts.proto().last_failed  # the next success clears it
    # An exporter that raises: counted with its error, then raised as before.
    spans = c.spans(Recording(ConnectionError("connection refused")))
    with pytest.raises(ConnectionError):
        spans.export(["s1", "s2"])
    got = c.traces.proto()
    assert (got.exports_failed, got.items_failed, got.last_error) == (1, 2, "connection refused")
    c.spans(Recording(SpanExportResult.SUCCESS)).export(["s"])
    assert (c.traces.proto().exports_ok, c.traces.proto().last_failed) == (1, False)
    # send_* false: filtered, not failed.
    c.apply(typed({"send_traces": "false", "send_logs": "false"}))
    spans.export(["a", "b", "c"])
    logs.export([record()])
    assert (c.traces.proto().items_filtered, c.logs_counts.proto().items_filtered) == (3, 2)


def test_metrics_are_counted_by_data_point_at_the_gated_metric_exporter(monkeypatch):
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import InMemoryMetricReader, MetricExportResult

    monkeypatch.setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
    reader = InMemoryMetricReader()
    meter = MeterProvider(metric_readers=[reader]).get_meter("m")
    counter = meter.create_counter("c")
    counter.add(1, {"k": "a"})
    counter.add(1, {"k": "b"})
    data = reader.get_metrics_data()
    gated = opamp_otel.GatedOtlpMetricExporter()
    gated._to = Recording(MetricExportResult.FAILURE)
    before = opamp_otel.AGENT.controls.metrics.proto()
    gated.export(data)
    after = opamp_otel.AGENT.controls.metrics.proto()
    assert (after.exports_failed - before.exports_failed, after.items_failed - before.items_failed) == (1, 2)
    opamp_otel.AGENT.controls.apply(typed({"send_metrics": "false"}))
    try:
        gated.export(data)
        assert opamp_otel.AGENT.controls.metrics.proto().items_filtered - before.items_filtered == 2
    finally:
        opamp_otel.AGENT.controls.apply(typed({}))


def test_the_timer_sends_export_health_on_a_failure_and_holds_it_while_nothing_changes(tmp_path):
    from opentelemetry.sdk.trace.export import SpanExportResult

    clocks = Clocks()
    agent = agent_at(clocks, tmp_path)
    agent.client = client = FakeClient()
    agent._ticking = True  # as start() does, before its thread runs _tick
    with agent._lock:
        first = agent._export_health()  # what start() sends with the first message
    message = agent._health_message(first)
    assert message.healthy and message.component_health_map["exporters"].status == "ok"
    clocks.advance(30_000)
    assert agent._tick() == pytest.approx(30)
    assert client.health == []  # nothing changed
    agent.controls.spans(Recording(SpanExportResult.FAILURE)).export(["s"])
    clocks.advance(30_000)
    agent._tick()
    [sent] = client.health
    assert sent.healthy  # the application's: a failing exporter doesn't make the service look down
    exporters = sent.component_health_map["exporters"]
    assert (exporters.healthy, exporters.status) == (False, "failing")
    assert exporters.component_health_map["traces"].last_error == opamp_otel.Counts.FAILURE
    agent.controls.spans(Recording(SpanExportResult.SUCCESS)).export(["s"])
    clocks.advance(30_000)
    agent._tick()
    assert client.health[-1].component_health_map["exporters"].healthy  # cleared at the next tick


def test_the_first_message_carries_export_health(tmp_path):
    from opentelemetry.sdk.resources import Resource

    import test_opamp_otel

    server = test_opamp_otel._Server()
    agent = Agent()
    try:
        agent.prepare({"OTEL_OPAMP_ENDPOINT": server.endpoint, "OTEL_OPAMP_STATE_DIR": str(tmp_path)})
        agent.configured = True
        agent.start(Resource.create({"service.name": "checkout"}))
        server.wait(lambda: any(m.HasField("health") for m in server.got))
        health = next(m.health for m in server.got if m.HasField("health"))
        assert health.healthy
        assert sorted(health.component_health_map["exporters"].component_health_map) == ["logs", "metrics", "traces"]
    finally:
        agent.stop()
        server.close()


def test_the_package_declares_every_setting_it_carries_out():
    every = opamp_otel.sdk_host(["otlp"])
    assert sorted(every.live) == LIVE
    assert sorted(every.next_start) == ["deactivate_all_instrumentations", "deactivate_instrumentations",
                                        "opamp_polling_interval"]
    # Metrics play no part in the rest.
    assert sorted(opamp_otel.sdk_host(["console"]).live) == [k for k in LIVE if k != "send_metrics"]
    # Another configurator's SDK: only the gated metric exporter, which the variable still names,
    # and the application's own settings (custom), which the application reads whoever built the
    # SDK (test_app_settings.py).
    assert list(opamp_otel.sdk_host(["otlp"], configured=False).live) == ["custom", "send_metrics"]
    assert list(opamp_otel.sdk_host(["otlp"], configured=False, config_file=True).live) == ["custom"]
    # Every declared name is a setting: the layer refuses a host naming one that isn't.
    LAYER.load(b"{}", every)


# --- the native matchers against the layer's reference ones (Evaluate), on generated inputs --------


def evaluate(settings: dict, **input_) -> settings_pb2.Evaluated:
    e = settings_pb2.Evaluate(settings=json.dumps(settings).encode(), host=HOST, now_unix_ms=NOW, **input_)
    return LAYER._call(settings_pb2.Request(evaluate=e)).evaluated


ALPHABET = ["a", "b", "A", ".", "*", "é", "É", "?"]


def word(rng, n: int = 4, star: bool = True) -> str:
    return "".join(rng.choice(ALPHABET if star else ALPHABET[:4] + ALPHABET[5:]) for _ in range(rng.randrange(n + 1)))


def test_the_sampler_agrees_with_the_layers_reference_on_generated_spans():
    import random

    from opentelemetry.trace import SpanKind, get_current_span

    rng = random.Random(215)
    matched = 0
    kinds = list(KINDS)
    for _ in range(300):
        rules = []
        for _ in range(rng.randrange(1, 4)):
            rule: dict = {"rate": rng.choice([0, 0.5, 1])}
            if rng.random() < 0.5:
                rule["name"] = word(rng)
            if rng.random() < 0.3:
                rule["kind"] = rng.choice(kinds)
            if rng.random() < 0.3:
                rule["path"] = word(rng)
            if rng.random() < 0.3:
                rule["attributes"] = {rng.choice(["k", "n"]): word(rng)}
            rules.append(rule)
        attrs = []
        if rng.random() < 0.5:
            attrs.append({"key": rng.choice(["url.path", "http.target"]), "string": word(rng, star=False) + rng.choice(["", "?q"])})
        if rng.random() < 0.5:
            attrs.append(rng.choice([{"key": "k", "string": word(rng, star=False)}, {"key": "n", "int": rng.randrange(-20, 300)},
                                     {"key": "k", "bool": rng.random() < 0.5}, {"key": "n", "double": 0.5}]))
        span = {"name": word(rng, star=False), "kind": rng.choice(kinds), "attributes": attrs,
                "parent": rng.choice(["none", "none", "remote_sampled", "local_unsampled"])}
        settings = {"sampling_rules": rules}
        ref = evaluate(settings, span=settings_pb2.SpanStart(
            name=span["name"], kind=KINDS[span["kind"]] + 1,
            attributes=[anyvalue_pb2.KeyValue(key=a["key"], value=anyvalue(a)) for a in attrs],
            parent=["none", "local_sampled", "local_unsampled", "remote_sampled", "remote_unsampled"].index(span["parent"])))
        c = controlled(settings)
        parent = get_current_span(parent_context(span["parent"])).get_span_context()
        _, rule = c.sampler._choose(c.sampler._sampling, parent, span["name"], SpanKind(KINDS[span["kind"]]),
                                    span_attributes(span))
        how = ref.sampling.how
        matched += ref.sampling.HasField("rule")
        assert (rule if how == settings_pb2.SpanSampling.RATE else None) == (
            ref.sampling.rule if ref.sampling.HasField("rule") else None), (settings, span)
    assert 30 < matched < 270  # the generated rules match some spans, not all


def anyvalue(a: dict):
    from opamp_client import anyvalue_pb2

    if "string" in a:
        return anyvalue_pb2.AnyValue(string_value=a["string"])
    if "bool" in a:
        return anyvalue_pb2.AnyValue(bool_value=a["bool"])
    if "int" in a:
        return anyvalue_pb2.AnyValue(int_value=a["int"])
    return anyvalue_pb2.AnyValue(double_value=a["double"])


def test_the_log_levels_agree_with_the_layers_reference_on_generated_scopes():
    import random

    rng = random.Random(189)
    levels = ["trace", "debug", "info", "warn", "error", "fatal", "off"]
    for _ in range(300):
        entries = {}
        for _ in range(rng.randrange(1, 4)):
            name = "".join(rng.choice("ab.é") for _ in range(rng.randrange(1, 4)))
            entries[name + rng.choice(["", "*"])] = rng.choice(levels)
        if rng.random() < 0.2:
            entries["*"] = rng.choice(levels)
        settings = {"send_logs_level_by_scope": ",".join(f"{p}={lv}" for p, lv in entries.items())}
        if rng.random() < 0.5:
            settings["send_logs_level"] = rng.choice(levels[:-1])
        scope = "".join(rng.choice("ab.é") for _ in range(rng.randrange(0, 5)))
        severity = rng.choice([0, 1, 5, 9, 12, 13, 17, 21, 24])
        ref = evaluate(settings, log=settings_pb2.LogRecord(scope=scope, severity=severity))
        r = record(scope, severity)
        assert (exported(controlled(settings), [r]) == [r]) == ref.kept, (settings, scope, severity)


def test_redaction_agrees_with_the_layers_reference_on_generated_keys():
    import random

    rng = random.Random(231)
    changed = 0
    for _ in range(300):
        settings = {k: ",".join(word(rng) or "x" for _ in range(rng.randrange(1, 3)))
                    for k in ("drop_attributes", "mask_attributes") if rng.random() < 0.7}
        keys = list(dict.fromkeys(word(rng, 5, star=False) or "k" for _ in range(4)))
        ref = evaluate(settings, attributes=settings_pb2.Attributes(attributes=[
            anyvalue_pb2.KeyValue(key=k, value=anyvalue({"int": i})) for i, k in enumerate(keys)]))
        changed += len(ref.redacted) != len(keys) or any(kv.value.HasField("string_value") for kv in ref.redacted)
        want = [(kv.key, kv.value.string_value if kv.value.HasField("string_value") else kv.value.int_value)
                for kv in ref.redacted]
        r = record("app", 9, {k: i for i, k in enumerate(keys)})
        [out] = exported(controlled(settings), [r])
        assert pairs(out.log_record.attributes) == want, (settings, keys)
    assert 30 < changed < 270  # the generated patterns redact some attributes, not all
