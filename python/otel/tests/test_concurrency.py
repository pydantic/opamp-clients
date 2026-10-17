# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Controls under fire: one thread puts a seeded run of settings documents in force as fast as it
can while worker threads emit spans (roots and children, with attributes, events and links) and
log records through the real SDK pipeline (batch processors, the package's gates, in-memory
exporters). Then a final document, and what is emitted after it follows that document exactly.

Whatever the interleaving:

- nothing raises (in a worker, the applier or a gate on the processors' threads), and the run ends
  within its bound (no deadlock);
- a key a drop pattern of every document in the run matches is never exported, on a span, its
  events, its links or a log record;
- a key a mask pattern of every document matches (and no drop pattern of any) is only ever
  exported as "[REDACTED]";
- a child of a sampled local parent is sampled: the trace cap never refuses it;
- with send_logs false in every document, no log record is exported;
- a key every document that sends a signal drops is never exported on that signal, nor one they
  all drop or mask in the clear, though a document that doesn't send it keeps it: an export is
  held to one document's send_* and redaction, never to the next one's send_* and the last one's
  redaction (the "switched" profile, where every document either sends nothing or drops
  ALWAYS_DROP, is built to catch that);
- a span (its attributes, its events' and its links') and a log record are each redacted under one
  document of the run, never part under one and part under another.

What can't be checked mid-run, and why: which document a sampling decision, a level, a cap or a
send_* switch was made under. The gates read the settings at export, on the processors' threads,
and the sampler at span start, so a span or record emitted under one document is judged under
whichever is in force when it gets there; any document of the run is a right answer, and the
rates, rules and levels of the run's documents between them keep or drop nearly everything. The
caps' counts depend on how the batches fell. Those are checked once the flipping stops, against
the final document, by the layer's reference matchers (Evaluate).

The seed is printed (and named in every failure); OPAMP_PROP_SEED=<seed> replays it.
OPAMP_PROP_RUNS=<n> runs n seeds per profile (default 1), for a longer nightly run."""

from __future__ import annotations

import json
import os
import random
import sys
import threading
import time
import traceback

import pytest
from opamp_client import anyvalue_pb2, opamp_pb2
from opamp_client.sdk import SdkSettings, settings_pb2
from opentelemetry import trace
from opentelemetry._logs import LogRecord, SeverityNumber
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.sdk.trace.sampling import TraceIdRatioBased
from opentelemetry.trace import Link, NonRecordingSpan, SpanContext, SpanKind, TraceFlags

import opamp_otel
from opamp_otel import Controls

APPLIED = opamp_pb2.RemoteConfigStatuses_APPLIED
LAYER = SdkSettings()
#: 2026-10-14T12:00:00Z: the clock the documents are decided at (debug_until is against it).
NOW = 1_791_979_200_000
#: What the SDK declares, as a real agent does: a setting the host leaves out is set aside, never applied.
HOST = opamp_otel.sdk_host(["otlp"])
#: How long the applier flips, in seconds, per run.
FLIPPING = 0.6
#: The bound on a run: past it, something is stuck.
BOUND = 30.0
WORKERS = 4
#: The GIL's switch interval while the threads run, in seconds (the interpreter's default is 0.005).
SWITCH = 0.00001
#: What the simulated monotonic clock the caps refill on moves by, per document put in force.
TICK_NS = 20_000_000


@pytest.fixture(autouse=True)
def _own_state_home(tmp_path_factory, monkeypatch):
    home = tmp_path_factory.mktemp("state-home")
    monkeypatch.setenv("XDG_STATE_HOME", str(home))
    monkeypatch.setenv("HOME", str(home))


#: Seeds that once failed, run every time. 311230235 and 69531056 (quiet): send_logs wasn't declared by the old
#: hand-written HOST, so it was set aside and logs flowed in a run where every document turned them off.
FOUND = {"free": [], "redacted": [], "quiet": [311230235, 69531056], "switched": []}


def seeds(profile: str) -> list[int]:
    if seed := os.environ.get("OPAMP_PROP_SEED"):
        return [int(seed)]
    runs = int(os.environ.get("OPAMP_PROP_RUNS", "1"))
    return FOUND[profile] + [random.SystemRandom().getrandbits(32) for _ in range(runs)]


# --- the documents -------------------------------------------------------------------------------

#: The attribute keys the workers emit, with a value each (lists among them: masked whole).
VALUES = {
    "secret.token": "s3cr3t", "SECRET.Key": "k3y", "user.email": "a@b.c", "user.ids": (1, 2, 3),
    "plain": "p", "k": "v1", "n": 7, "db.statement": "select 1", "auth": True, "tags": ("x", "y"),
    "http.target": "/health?probe=1", "url.path": "/checkout",
}
KEYS = list(VALUES)
#: Drop patterns every document of a redacted run has, and a key each matches whatever the case.
ALWAYS_DROP, DROPPED = "secret.*", ("secret.token", "SECRET.Key")
#: Mask patterns every document of a redacted run has; no drop pattern of the run matches these keys.
ALWAYS_MASK, MASKED = "user.*", ("user.email", "user.ids")
DROP_POOL = {"free": ["secret.*", "db.*", "k", "auth*", "*email*", "plain", "*.ids", "*"],
             "redacted": ["db.*", "k", "auth*", "tags"]}
MASK_POOL = {"free": ["user.*", "plain", "n", "*token*", "db.statement", "tags"],
             "redacted": ["plain", "n", "*email*", "db.statement", "*token*"]}
RULES = [{"rate": 0, "kind": "server", "path": "/health*"}, {"rate": 0.5, "name": "job*"},
         {"rate": 1, "attributes": {"k": "v*"}}, {"rate": 0}, {"rate": 1, "name": "GET *"},
         {"rate": 0.25, "kind": "internal"}, {"rate": 1, "path": "/checkout"}]
LEVELS = ["trace", "debug", "info", "warn", "error", "fatal"]
SCOPES = ["app.db.pool", "app.web", "noisy", "urllib3", "app"]
SCOPE_PATTERNS = ["app.db*", "noisy", "urllib3", "app.web", "app*", "*"]
#: The caps a run flips through; a final document's are none of these, so they start full.
FLIP_CAPS, FINAL_CAPS = ["1", "2", "5", "50"], ["3", "4"]


def generate(rng: random.Random, profile: str, n: int) -> list[dict]:
    """`n` documents, each a few changes from the one before: rules added, removed and reordered;
    caps appearing, changing and going; levels per scope; redaction lists growing and shrinking;
    send_* switched; debug windows opening and closing; now and then nothing at all.
    `profile`: "free" (anything), "redacted" (every document drops ALWAYS_DROP and masks
    ALWAYS_MASK), "quiet" (redacted, and send_logs false in every one) or "switched" (half the
    documents send neither spans nor logs and may keep ALWAYS_DROP's keys; every other one drops
    ALWAYS_DROP)."""
    pool = "free" if profile == "free" else "redacted"
    doc: dict = {}
    out = []
    for _ in range(n):
        doc = dict(doc)
        for _ in range(rng.randrange(1, 4)):
            change(rng, doc, pool)
        if rng.random() < 0.08:
            doc = {}  # the empty document (what a redacted or quiet run holds in every one, added back)
        if profile == "switched" and rng.random() < 0.5:
            out.append(fixed({**doc, "send_traces": "false", "send_logs": "false"}, profile))
            continue
        out.append(fixed(doc, profile))
    return out


def change(rng: random.Random, doc: dict, pool: str) -> None:
    what = rng.choice(["rules", "rules", "rate", "trace_cap", "log_cap", "level", "scopes", "drop", "mask",
                       "send", "debug"])
    if what == "rules":
        rules = list(doc.get("sampling_rules", []))
        op = rng.choice(["add", "remove", "shuffle"])
        if op == "add" or not rules:
            rules.insert(rng.randrange(len(rules) + 1), rng.choice(RULES))
        elif op == "remove":
            rules.pop(rng.randrange(len(rules)))
        else:
            rng.shuffle(rules)
        set_or_drop(doc, "sampling_rules", rules)
    elif what == "rate":
        set_or_drop(doc, "sampling_rate", maybe(rng, ["0", "0.5", "1"]))
    elif what == "trace_cap":
        set_or_drop(doc, "sampling_traces_per_second", maybe(rng, FLIP_CAPS))
    elif what == "log_cap":
        set_or_drop(doc, "send_logs_per_second", maybe(rng, FLIP_CAPS))
    elif what == "level":
        set_or_drop(doc, "send_logs_level", maybe(rng, LEVELS))
    elif what == "scopes":
        patterns = rng.sample(SCOPE_PATTERNS, rng.randrange(len(SCOPE_PATTERNS) // 2))
        set_or_drop(doc, "send_logs_level_by_scope",
                    ",".join(f"{p}={rng.choice([*LEVELS, 'off'])}" for p in patterns))
    elif what in ("drop", "mask"):
        key = f"{what}_attributes"
        have = [p for p in doc.get(key, "").split(",") if p]
        if have and rng.random() < 0.4:
            have.pop(rng.randrange(len(have)))
        else:
            have.append(rng.choice((DROP_POOL if what == "drop" else MASK_POOL)[pool]))
        set_or_drop(doc, key, ",".join(dict.fromkeys(have)))
    elif what == "send":
        key = rng.choice(["send_traces", "send_logs", "send_metrics"])
        set_or_drop(doc, key, maybe(rng, ["true", "false"]))
    elif rng.random() < 0.5:
        doc.pop("debug_until", None)
        doc.pop("debug_sampling_rate", None)
        doc.pop("debug_send_logs_level", None)
    else:
        # Open (ten minutes ahead) or already over (an hour ago).
        doc["debug_until"] = rng.choice(["2026-10-14T12:10:00Z", "2026-10-14T11:00:00Z"])
        set_or_drop(doc, "debug_sampling_rate", rng.choice([None, "0.5", "1"]))
        set_or_drop(doc, "debug_send_logs_level", rng.choice([None, "debug", "info"]))


def maybe(rng: random.Random, values: list):
    """One of `values`, or (as often as not) None: the setting withdrawn."""
    return None if rng.random() < 0.5 else rng.choice(values)


def set_or_drop(doc: dict, key: str, value) -> None:
    if value is None or value == [] or value == "":
        doc.pop(key, None)
    else:
        doc[key] = value


def fixed(doc: dict, profile: str) -> dict:
    """The document with what the profile holds in every document."""
    doc = dict(doc)
    if profile in ("redacted", "quiet"):
        for key, always in (("drop_attributes", ALWAYS_DROP), ("mask_attributes", ALWAYS_MASK)):
            doc[key] = ",".join(dict.fromkeys([always, *[p for p in doc.get(key, "").split(",") if p]]))
    if profile == "quiet":
        doc["send_logs"] = "false"
    if profile == "switched" and not doc.get("send_traces") == doc.get("send_logs") == "false":
        have = [p for p in doc.get("drop_attributes", "").split(",") if p]
        doc["drop_attributes"] = ",".join(dict.fromkeys([ALWAYS_DROP, *have]))
    return doc


def final_document(rng: random.Random, profile: str) -> dict:
    """The document the run ends on: a generated one, with caps the run never had (they start full)."""
    doc = generate(rng, profile, rng.randrange(1, 6))[-1]
    for key in ("sampling_traces_per_second", "send_logs_per_second"):
        set_or_drop(doc, key, rng.choice([None, *FINAL_CAPS]))
    return doc


def typed(doc: dict) -> settings_pb2.Settings:
    """The settings as the layer hands them to the package, for a server's document."""
    body = json.dumps(doc).encode()
    m = opamp_pb2.AgentConfigMap()
    m.config_map["elastic"].CopyFrom(opamp_pb2.AgentConfigObject(body=body, content_type="application/json"))
    d = LAYER.decide(opamp_pb2.AgentRemoteConfig(config=m, config_hash=body), b"{}", b"{}", HOST, NOW)
    assert d.status.status == APPLIED, (doc, d.status.error_message)
    return d.apply


def evaluate(doc: dict, **input_) -> settings_pb2.Evaluated:
    """The layer's reference matchers on one input under `doc`."""
    e = settings_pb2.Evaluate(settings=json.dumps(doc).encode(), host=HOST, now_unix_ms=NOW, **input_)
    return LAYER._call(settings_pb2.Request(evaluate=e)).evaluated


def anyvalue(v) -> anyvalue_pb2.AnyValue:
    if isinstance(v, bool):
        return anyvalue_pb2.AnyValue(bool_value=v)
    if isinstance(v, int):
        return anyvalue_pb2.AnyValue(int_value=v)
    if isinstance(v, tuple):
        return anyvalue_pb2.AnyValue(array_value=anyvalue_pb2.ArrayValue(values=[anyvalue(x) for x in v]))
    return anyvalue_pb2.AnyValue(string_value=v)


KEEP, DROP, MASK = "keep", "drop", "mask"


def fates(doc: dict) -> dict[str, str]:
    """What `doc` does to each key, by the layer's reference redactor."""
    ref = evaluate(doc, attributes=settings_pb2.Attributes(attributes=[
        anyvalue_pb2.KeyValue(key=k, value=anyvalue(VALUES[k])) for k in KEYS]))
    out = dict.fromkeys(KEYS, DROP)
    for kv in ref.redacted:
        masked = kv.value.HasField("string_value") and kv.value.string_value == "[REDACTED]"
        out[kv.key] = MASK if masked else KEEP
    return out


# --- what the workers emit -----------------------------------------------------------------------

ROOTS = [("GET /health", SpanKind.SERVER), ("GET /checkout", SpanKind.SERVER), ("job.noisy", SpanKind.INTERNAL),
         ("db.query", SpanKind.CLIENT), ("POST /login", SpanKind.SERVER), ("job.batch", SpanKind.CONSUMER)]


def some(rng: random.Random) -> dict:
    """A few of the keys, with their values."""
    return {k: VALUES[k] for k in rng.sample(KEYS, rng.randrange(len(KEYS) + 1))}


class Recorder:
    """An in-memory exporter that keeps every batch it is handed, from any thread."""

    def __init__(self) -> None:
        self.batches: list[list] = []
        self._lock = threading.Lock()

    def export(self, batch, *args, **kwargs):
        from opentelemetry.sdk._logs.export import LogRecordExportResult
        from opentelemetry.sdk.trace.export import SpanExportResult

        with self._lock:
            self.batches.append(list(batch))
        return SpanExportResult.SUCCESS if hasattr(batch[0], "get_span_context") else LogRecordExportResult.SUCCESS

    def items(self) -> list:
        with self._lock:
            return [x for b in self.batches for x in b]

    def clear(self) -> None:
        with self._lock:
            self.batches.clear()

    def shutdown(self, *args, **kwargs):
        pass

    def force_flush(self, *args, **kwargs):
        return True


class Guarded:
    """A gate as the batch processors call it, keeping what it raised: the processors log an
    exporter's exception and carry on, so the test would never see it otherwise."""

    def __init__(self, gate, errors: list) -> None:
        self._gate, self._errors = gate, errors

    def export(self, batch, *args, **kwargs):
        try:
            return self._gate.export(batch, *args, **kwargs)
        except BaseException:
            self._errors.append(traceback.format_exc())
            raise

    def shutdown(self, *args, **kwargs):
        return self._gate.shutdown()

    def force_flush(self, *args, **kwargs):
        return self._gate.force_flush()


class Clock:
    """The caps' monotonic clock, in nanoseconds: the applier moves it, a worker only reads it."""

    def __init__(self) -> None:
        self.ns = 0

    def __call__(self) -> int:
        return self.ns


class Pipeline:
    """The real SDK, as the package's Configurator builds it: the live sampler, batch processors,
    the gates in front of in-memory exporters."""

    def __init__(self, controls: Controls) -> None:
        self.errors: list[str] = []
        self.spans, self.logs = Recorder(), Recorder()
        self.tracer_provider = TracerProvider(sampler=controls.sampler.wrap(None))
        self.tracer_provider.add_span_processor(BatchSpanProcessor(
            Guarded(controls.spans(self.spans), self.errors), schedule_delay_millis=2, max_export_batch_size=64))
        self.logger_provider = LoggerProvider()
        self.logger_provider.add_log_record_processor(BatchLogRecordProcessor(
            Guarded(controls.logs(self.logs), self.errors), schedule_delay_millis=2, max_export_batch_size=64))
        self.tracer = self.tracer_provider.get_tracer("torture")

    def flush(self) -> None:
        assert self.tracer_provider.force_flush(int(BOUND * 1000)), "spans didn't flush"
        assert self.logger_provider.force_flush(int(BOUND * 1000)), "logs didn't flush"

    def shutdown(self) -> None:
        self.tracer_provider.shutdown()
        self.logger_provider.shutdown()


class Emitted:
    """What a worker emitted: each span's own attributes, its events' and its links' (by span ID),
    each log record's (by body); and every broken promise it saw at once."""

    def __init__(self) -> None:
        self.spans: dict[int, tuple[dict, list[dict], list[dict]]] = {}
        self.logs: dict[str, dict] = {}
        self.violations: list[str] = []


def emit(p: Pipeline, rng: random.Random, e: Emitted, tag: str, n: int) -> None:
    """One root span with its children, links and events, and a few log records."""
    name, kind = rng.choice(ROOTS)
    parent = None
    if rng.random() < 0.1:  # now and then joining a caller's trace
        ctx = SpanContext(rng.getrandbits(128) | 1, rng.getrandbits(64) | 1, is_remote=True,
                          trace_flags=TraceFlags(TraceFlags.SAMPLED if rng.random() < 0.5 else 0))
        parent = trace.set_span_in_context(NonRecordingSpan(ctx))
    attrs = some(rng)
    link_ctx = SpanContext(rng.getrandbits(128) | 1, rng.getrandbits(64) | 1, is_remote=True)
    links = [some(rng) for _ in range(rng.randrange(3))]
    with p.tracer.start_as_current_span(name, context=parent, kind=kind, attributes=attrs,
                                        links=[Link(link_ctx, a) for a in links]) as root:
        events = [some(rng) for _ in range(rng.randrange(3))]
        for a in events:
            root.add_event("e", a)
        e.spans[root.get_span_context().span_id] = (attrs, events, links)
        root_sampled = root.get_span_context().trace_flags.sampled
        for j in range(rng.randrange(1, 4)):
            c_attrs, c_events = some(rng), [some(rng)]
            child = p.tracer.start_span(f"child.{j}", attributes=c_attrs, links=[Link(link_ctx, c_attrs)])
            child.add_event("ce", c_events[0])
            e.spans[child.get_span_context().span_id] = (c_attrs, c_events, [c_attrs])
            if root_sampled and not child.get_span_context().trace_flags.sampled:
                e.violations.append(f"child.{j} of the sampled local parent {name} wasn't sampled")
            child.end()
    for i in range(rng.randrange(1, 4)):
        body = f"{tag}-{n}-{i}"
        a = some(rng)
        e.logs[body] = a
        p.logger_provider.get_logger(rng.choice(SCOPES)).emit(LogRecord(
            body=body, severity_number=SeverityNumber(rng.choice([1, 5, 9, 13, 17, 21])) if rng.random() < 0.9 else None,
            attributes=a))


# --- the checks ----------------------------------------------------------------------------------


def seen(original: dict, exported) -> dict[str, str] | str:
    """What was done to each of `original`'s keys, as exported; or what went wrong."""
    exported = dict(exported or {})
    out = {}
    for k, v in original.items():
        if k not in exported:
            out[k] = DROP
        elif exported[k] == "[REDACTED]":
            out[k] = MASK
        elif exported[k] == v:
            out[k] = KEEP
        else:
            return f"{k} exported as {exported[k]!r}, emitted as {v!r}"
    if extra := set(exported) - set(original):
        return f"keys exported that weren't emitted: {sorted(extra)}"
    return out


def one_document(lists: list[dict], exported: list, tables: list[dict[str, str]]) -> str | None:
    """None when one document's redaction gives all of `exported` from `lists`; else why not."""
    got = {}
    for original, out in zip(lists, exported, strict=True):
        s = seen(original, out)
        if isinstance(s, str):
            return s
        for k, fate in s.items():
            if got.setdefault(k, fate) != fate:
                return f"{k} both {got[k]} and {fate} in one span"
    if not any(all(t[k] == fate for k, fate in got.items()) for t in tables):
        return f"no document of the run does {got}"
    return None


def unsent(docs: list[dict], tables: list[dict[str, str]], send: str) -> tuple[set[str], set[str]]:
    """The keys every document that sends a signal (`send`: its send_* setting) drops, and those
    they all drop or mask: what's never exported on it, and never in the clear. Neither, when no
    document sends it."""
    sending = [t for d, t in zip(docs, tables) if d.get(send) != "false"]
    if not sending:
        return set(), set()
    return ({k for k in KEYS if all(t[k] == DROP for t in sending)},
            {k for k in KEYS if all(t[k] != KEEP for t in sending)})


def held_when_sent(exported: list, what: str, never: tuple[set[str], set[str]]) -> list[str]:
    """The drop and mask promises of the documents that send a signal, on one span's or record's
    attribute lists."""
    dropped, hidden = never
    out = []
    for attrs in exported:
        attrs = dict(attrs or {})
        out += [f"{what}: {k} exported, dropped by every document that sends it" for k in dropped if k in attrs]
        out += [f"{what}: {k} exported as {attrs[k]!r}, dropped or masked by every document that sends it"
                for k in hidden - dropped if k in attrs and attrs[k] != "[REDACTED]"]
    return out


def check_run(p: Pipeline, emitted: Emitted, docs: list[dict], tables: list[dict], profile: str) -> list[str]:
    """The promises that hold whatever the interleaving, on everything exported."""
    problems = list(emitted.violations)
    span_never, log_never = unsent(docs, tables, "send_traces"), unsent(docs, tables, "send_logs")
    for span in p.spans.items():
        original = emitted.spans.get(span.get_span_context().span_id)
        if original is None:
            problems.append(f"a span exported that no worker emitted: {span.name}")
            continue
        attrs, events, links = original
        lists = [attrs, *events, *links]
        exported = [span.attributes, *(ev.attributes for ev in span.events), *(lk.attributes for lk in span.links)]
        if len(exported) != len(lists):
            problems.append(f"{span.name}: {len(exported)} attribute lists exported, {len(lists)} emitted")
            continue
        if why := one_document(lists, exported, tables):
            problems.append(f"{span.name}: {why}")
        problems += promised(exported, f"span {span.name}", profile)
        problems += held_when_sent(exported, f"span {span.name}", span_never)
    records = p.logs.items()
    if profile == "quiet" and records:
        problems.append(f"{len(records)} log records exported with send_logs false in every document")
    for r in records:
        original = emitted.logs[r.log_record.body]
        if why := one_document([original], [r.log_record.attributes], tables):
            problems.append(f"log {r.log_record.body}: {why}")
        problems += promised([r.log_record.attributes], f"log {r.log_record.body}", profile)
        problems += held_when_sent([r.log_record.attributes], f"log {r.log_record.body}", log_never)
    return problems


def promised(exported: list, what: str, profile: str) -> list[str]:
    """The drop and mask promises of a redacted run, on one span's or record's attribute lists."""
    if profile not in ("redacted", "quiet"):
        return []
    out = []
    for attrs in exported:
        attrs = dict(attrs or {})
        out += [f"{what}: {k} exported, dropped in every document" for k in DROPPED if k in attrs]
        out += [f"{what}: {k} exported as {attrs[k]!r}, masked in every document"
                for k in MASKED if k in attrs and attrs[k] != "[REDACTED]"]
    return out


def check_final(p: Pipeline, doc: dict, seed: int) -> list[str]:
    """Emit, alone, after `doc` is in force: everything follows `doc`, by the layer's reference."""
    rng = random.Random(seed ^ 0xF1A1)
    table = fates(doc)
    problems = []
    roots, logs = [], []
    for i in range(40):
        name, kind = rng.choice(ROOTS)
        attrs = some(rng)
        with p.tracer.start_as_current_span(f"{name}#{i}", kind=kind, attributes=attrs) as root:
            child = p.tracer.start_span(f"child#{i}", attributes=attrs)
            child.end()
        roots.append((f"{name}#{i}", kind, attrs, root, child))
    for i in range(60):
        scope, severity, attrs = rng.choice(SCOPES), rng.choice([0, 1, 5, 9, 13, 17, 21]), some(rng)
        body = f"final-{i}"
        p.logger_provider.get_logger(scope).emit(LogRecord(
            body=body, severity_number=SeverityNumber(severity) if severity else None, attributes=attrs))
        logs.append((body, scope, severity, attrs))
    p.flush()

    # Spans: the rules or rate by the reference, then the cap from a full bucket on a still clock.
    sampled = []
    for name, kind, attrs, root, _ in roots:
        ref = evaluate(doc, span=settings_pb2.SpanStart(
            name=name, kind=kind.value + 1,
            attributes=[anyvalue_pb2.KeyValue(key=k, value=anyvalue(v)) for k, v in attrs.items()])).sampling
        if ref.how == settings_pb2.SpanSampling.RATE:
            trace_id = root.get_span_context().trace_id
            sampled.append(TraceIdRatioBased(ref.rate).should_sample(None, trace_id, "x").decision.is_sampled())
        else:  # the application's own sampler: the SDK's default, which samples every root
            sampled.append(True)
    if "sampling_traces_per_second" in doc:
        run = settings_pb2.CapRun(signal=settings_pb2.CapRun.TRACES, events=[
            settings_pb2.CapEvent(at_ns=0, parent=settings_pb2.PARENT_NONE, sampled=s) for s in sampled])
        sampled = [bool(x) for x in evaluate(doc, cap=run).passed]
    want_spans = {}
    for (name, _, attrs, root, child), s in zip(roots, sampled, strict=True):
        if root.get_span_context().trace_flags.sampled != s:
            problems.append(f"final: {name} sampled {not s}, the document says {s}")
        if child.get_span_context().trace_flags.sampled != s:
            problems.append(f"final: the child of {name} doesn't follow it")
        if s:
            want_spans[name] = want_spans[f"child#{name.rsplit('#', 1)[1]}"] = redacted(attrs, table)
    if doc.get("send_traces") == "false":
        want_spans = {}
    got_spans = {span.name: dict(span.attributes) for span in p.spans.items()}
    if got_spans != want_spans:
        problems.append(f"final: spans exported {got_spans}, the document says {want_spans}")

    # Logs: send_logs and the levels by the reference, then the cap's first records.
    kept = [log for log in logs if evaluate(doc, log=settings_pb2.LogRecord(scope=log[1], severity=log[2])).kept]
    if "send_logs_per_second" in doc and kept:
        run = settings_pb2.CapRun(signal=settings_pb2.CapRun.LOGS,
                                  events=[settings_pb2.CapEvent(at_ns=0, records=len(kept))])
        kept = kept[:evaluate(doc, cap=run).passed[0]]
    want_logs = [(body, redacted(attrs, table)) for body, _, _, attrs in kept]
    got_logs = [(r.log_record.body, dict(r.log_record.attributes or {})) for r in p.logs.items()]
    if got_logs != want_logs:
        problems.append(f"final: logs exported {got_logs}, the document says {want_logs}")
    return problems


def redacted(attrs: dict, table: dict[str, str]) -> dict:
    return {k: "[REDACTED]" if table[k] == MASK else v for k, v in attrs.items() if table[k] != DROP}


# --- the run -------------------------------------------------------------------------------------


def torture(seed: int, profile: str) -> list[str]:
    rng = random.Random(seed)
    docs = generate(rng, profile, 60)
    final = final_document(rng, profile)
    settings = [typed(d) for d in docs]
    final_settings = typed(final)
    tables = [fates(d) for d in [*docs, final]]

    clock = Clock()
    c = Controls(clock=clock)
    c.apply(settings[0])  # a run's promises are its documents': the defaults before them aren't one
    p = Pipeline(c)
    stop = threading.Event()
    errors: list[str] = p.errors
    emitted = [Emitted() for _ in range(WORKERS)]

    def applier() -> None:
        try:
            i = 0
            while not stop.is_set():
                clock.ns += TICK_NS
                c.apply(settings[i % len(settings)])
                i += 1
                if i % 7 == 0:
                    time.sleep(0)  # let the workers in now and then; otherwise flip back to back
        except BaseException:
            errors.append(traceback.format_exc())

    def worker(w: int) -> None:
        wrng = random.Random(seed * 31 + w)
        n = 0
        try:
            while not stop.is_set():
                emit(p, wrng, emitted[w], f"w{w}", n)
                n += 1
        except BaseException:
            errors.append(traceback.format_exc())

    threads = [threading.Thread(target=applier, name="applier", daemon=True)]
    threads += [threading.Thread(target=worker, args=(w,), name=f"worker-{w}", daemon=True) for w in range(WORKERS)]
    # The interpreter hands the GIL over every few microseconds, not every 5 ms: a thread is
    # stopped mid-apply, mid-export, mid-sampling far more often.
    interval = sys.getswitchinterval()
    sys.setswitchinterval(SWITCH)
    try:
        for t in threads:
            t.start()
        time.sleep(FLIPPING)
        stop.set()
        deadline = time.monotonic() + BOUND
        for t in threads:
            t.join(max(deadline - time.monotonic(), 0))
    finally:
        sys.setswitchinterval(interval)
    stuck = [t.name for t in threads if t.is_alive()]
    if stuck:
        return [f"stuck after {BOUND}s: {stuck}"]
    p.flush()

    merged = Emitted()
    for e in emitted:
        merged.spans.update(e.spans)
        merged.logs.update(e.logs)
        merged.violations += e.violations
    problems = [f"raised: {e}" for e in errors]
    problems += check_run(p, merged, docs, tables, profile)
    if not p.spans.items() and not p.logs.items() and profile != "quiet":
        problems.append("nothing exported: the run checked nothing")

    # The flipping is over: a final document, and everything after it follows it alone.
    before = len(errors)
    c.apply(final_settings)
    p.spans.clear()
    p.logs.clear()
    problems += check_final(p, final, seed)
    problems += [f"raised: {e}" for e in errors[before:]]
    p.shutdown()
    return problems


@pytest.mark.parametrize("profile", ["free", "redacted", "quiet", "switched"])
def test_controls_hold_their_promises_while_settings_flip_under_live_telemetry(profile):
    for seed in seeds(profile):
        print(f"torture {profile}: seed {seed} (replay: OPAMP_PROP_SEED={seed})")
        started = time.monotonic()
        problems = torture(seed, profile)
        listed = "\n".join(p[:500] for p in problems[:20])  # the first few, cut short
        assert not problems, f"seed {seed} ({profile}), replay with OPAMP_PROP_SEED={seed}:\n{listed}"
        assert time.monotonic() - started < BOUND, f"seed {seed}: the run took too long"
