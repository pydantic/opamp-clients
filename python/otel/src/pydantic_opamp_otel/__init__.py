# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""OpenTelemetry auto-instrumentation under an OpAMP server.

Installed next to ``opentelemetry-instrument``, this package puts the SDK it sets up under the
server named by ``OTEL_OPAMP_ENDPOINT`` (docs/python-otel.md). It plugs in where upstream lets a
distribution plug in:

- ``Distro`` (``opentelemetry_distro``): before anything is built, loads the saved settings and
  turns the next-start ones into the instrumentation's own switches; gives the instance its UID.
- ``Configurator`` (``opentelemetry_configurator``): builds the SDK with a sampler and exporters
  the server's settings can change while it runs, and the instance UID as service.instance.id.
- ``GatedOtlpMetricExporter`` (``opentelemetry_metrics_exporter``): the OTLP metric exporter,
  stoppable (metrics have no export-processor hook to wrap).
- ``start`` (``_opentelemetry_opamp``, upstream's experimental OpAMP hook): starts the OpAMP
  client once the SDK is built.
- ``configure``: the same in one call, for an application that sets its SDK up in code rather
  than under ``opentelemetry-instrument``.
- ``app_settings``: the application's own settings the server sends (``custom``), for the
  application to read (docs/app-settings.md).

What the settings mean, what to answer and what to report is the SDK settings layer's
(``pydantic_opamp_client.sdk``), the same in every language; this package carries it out on the Python SDK.
"""

from __future__ import annotations

import asyncio
import atexit
import copy
import dataclasses
import concurrent.futures.thread  # noqa: F401 - its exit hook is registered before ours (see _at_exit)
import logging
import math
import os
import platform
import secrets
import selectors
import shutil
import sys
import tempfile
import threading
import time
import uuid
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from datetime import timedelta
from pathlib import Path

from pydantic_opamp_client import Client, Config, CoreError, Notification, Status, anyvalue_pb2, opamp_pb2, save_state, setup

from pydantic_opamp_client.sdk import SdkSettings, settings_pb2
from opentelemetry.instrumentation.distro import BaseDistro
from opentelemetry.sdk._configuration import _OTelSDKConfigurator
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor, LogRecordExporter
from opentelemetry.sdk.metrics.export import MetricExporter, MetricExportResult
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import Event, ReadableSpan
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SpanExporter, SpanExportResult
from opentelemetry.trace import Link, SpanKind, get_current_span
from opentelemetry.sdk.trace.sampling import (ALWAYS_ON, Decision, ParentBased, Sampler, SamplingResult,
                                              TraceIdRatioBased)

from . import app_settings
from ._match import Bucket, Redactor, Scrubber, attribute_text, glob, level_keeps, rebucket, threshold
from .app_settings import AppSettings

__all__ = ["Configurator", "Distro", "GatedOtlpMetricExporter", "app_settings", "configure", "configure_logfire", "start"]

_log = logging.getLogger(__name__)


class _LastResort(logging.Handler):
    """Python's last resort for this package's warnings, where nothing else would show them. The
    OpenTelemetry log handler on the root logger (logging auto-instrumentation) counts as a handler,
    so Python's own last resort is skipped: a warning would leave only as an exported log record, or
    not at all with no logs exporter. What a person has to fix still reaches stderr; where the
    application has a handler of its own, that one shows it and this stays quiet."""

    def emit(self, record: logging.LogRecord) -> None:
        logger: logging.Logger | None = _log
        while logger:
            for h in logger.handlers:
                if h is not self and record.levelno >= h.level and not type(h).__module__.startswith("opentelemetry."):
                    return
            logger = logger.parent if logger.propagate else None
        (logging.lastResort or logging.StreamHandler(sys.stderr)).handle(record)


_log.addHandler(_LastResort(logging.WARNING))
_INSTANCE_ID = "service.instance.id"


#: The variables the core's Setup reads (setup.SetupStart).
_SETUP_VARIABLES = ("OTEL_OPAMP_DISABLED", "OTEL_SDK_DISABLED", "OTEL_OPAMP_ENDPOINT", "OTEL_OPAMP_HEADERS",
                    "OTEL_OPAMP_STATE_DIR", "OTEL_OPAMP_HEARTBEAT_INTERVAL", "OTEL_SERVICE_NAME",
                    "OTEL_RESOURCE_ATTRIBUTES", "HOME", "XDG_STATE_HOME", "LOCALAPPDATA")


_core_warned = False


def _core_loads() -> bool:
    """Whether the core can be loaded; when it can't, said once, and the SDK runs unmanaged."""
    global _core_warned
    reason = setup.unavailable()
    if reason is not None and not _core_warned:
        _core_warned = True
        _log.warning("OpAMP: the core can't be loaded on %s-%s; the SDK runs unmanaged: %s", sys.platform,
                     platform.machine(), reason)
    return reason is None


def _setup_os() -> int:
    """This operating system, as Setup names it."""
    if sys.platform == "win32":
        return setup.OS_WINDOWS
    if sys.platform == "darwin":
        return setup.OS_MACOS
    return setup.OS_UNIX


def program_name(argv=sys.argv) -> str:
    """The program, as a name: the script run, or the module run with -m."""
    first = argv[0] if argv and argv[0] not in ("", "-c", "-m") else "python"
    name = Path(first).name
    return name[:-3] if name.endswith(".py") else name


def _lock(fd: int) -> bool:
    """Take the operating system's lock on an open file without waiting: held until the process
    ends, never by a process it starts."""
    try:
        if sys.platform == "win32":
            import msvcrt

            msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
        else:
            import fcntl

            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return True
    except OSError:
        return False


def slot(slots, all_taken: str) -> tuple[Path, int] | None:
    """The first of the default state directory's slots (docs/default-state-dir.md, as the core's
    Setup names them) this process can make and lock, and its lock's descriptor, held until the
    process ends; None where there's none to be had (not writable, every slot taken: all_taken is
    the line to say then): each start is then a new instance. Never an error."""
    for name in slots:
        d = Path(name)
        try:
            d.mkdir(mode=0o700, parents=True, exist_ok=True)
            fd = os.open(d / setup.LOCK_FILE, os.O_RDWR | os.O_CREAT, 0o600)
        except OSError as e:
            _log.debug("OpAMP: no state directory of its own (%s): each start is a new instance", e)
            return None
        if _lock(fd):
            return d, fd
        os.close(fd)
    if slots:
        _log.info("OpAMP: %s", all_taken)
    return None


def shown_endpoint(endpoint: str, otherwise: str) -> str:
    """An endpoint as it is safe to log (no user info, query or fragment), as the core shows it;
    ``otherwise`` if the core can't say."""
    try:
        return setup.start(setup.SetupStart(code=setup.SetupInCode(endpoint=endpoint))).shown_endpoint or otherwise
    except CoreError:
        return otherwise


def hint(hints: setup.SetupHints, status: int | None) -> str:
    """What to check when the server can't be reached, by what went wrong, as the core words it:
    it names what gave the endpoint and the headers, the argument given in code or the variable."""
    if status in (401, 403):
        return hints.refused
    if status == 404:
        return hints.not_found
    if status is None:
        return hints.unreachable
    return hints.error


class Reachability:
    """Says when the client connects, when it can't (once a run of failures, with what to check),
    and when it's back: setup that works or doesn't is never silent."""

    def __init__(self, shown: str, service: str = "", uid: uuid.UUID | None = None,
                 hints: setup.SetupHints | None = None) -> None:
        self.endpoint = shown  # the configured one, as it is safe to log
        #: The endpoint the client uses now, as the client gives it (None, or None from it: the
        #: configured one): after the server moved the agent, the one it moved it to.
        self.in_use: Callable[[], str | None] | None = None
        self.hints = hints if hints is not None else setup.SetupHints()
        self.service = service
        self.uid = uid
        self.ever = False
        self.failing = False

    def at(self) -> str:
        """The endpoint the client uses now, as it is safe to log."""
        e = self.in_use() if self.in_use is not None else None
        return shown_endpoint(e, self.endpoint) if e else self.endpoint

    def notified(self, n: Notification) -> None:
        endpoint = self.at()
        if n.kind == "connected":
            if not self.ever:
                who = f" as {self.service}" if self.service else ""
                _log.info("OpAMP: connected to %s; this SDK is managed%s, instance %s", endpoint, who, self.uid)
            elif self.failing:
                _log.info("OpAMP: connected to %s again", endpoint)
            self.ever, self.failing = True, False
        elif n.kind == "connect_failed":
            if not self.failing:
                why = n.detail or f"HTTP {n.status}"
                _log.warning("OpAMP: can't reach %s (%s): %s. Retrying in the background; the SDK keeps its "
                             "current settings meanwhile", endpoint, why, hint(self.hints, n.status))
            else:
                _log.debug("OpAMP: still can't reach %s: %s", endpoint, n.detail)
            self.failing = True


@dataclass
class Setup:
    """Where the server is, from OTEL_OPAMP_*, as the core's Setup decides it; None when the
    package is off."""

    endpoint: str
    headers: dict[str, str] = field(default_factory=dict)
    state_dir: Path | None = None
    heartbeat_ms: int = 0
    #: The lock on the default state directory, held while the process runs.
    state_lock: int | None = None
    #: The endpoint as it is safe to log.
    shown: str = ""
    #: What to check when the server can't be reached, naming what gave the endpoint and the
    #: headers, the argument or the variable.
    hints: setup.SetupHints = field(default_factory=setup.SetupHints)

    @staticmethod
    def from_env(env=os.environ, *, endpoint: str | None = None, headers: Mapping[str, str] | None = None,
                 state_dir: str | os.PathLike | None = None,
                 heartbeat_interval: timedelta | float | str | None = None) -> Setup | None:
        """The keyword arguments are what the application gave in code: each one given wins over
        its variable, and a blank one is not given. One it can't use is warned about and is the
        default, as a variable it can't read is: it never stops the application. The rules are the
        core's (its Setup call), the same in every language; what is here is the I/O: reading the
        variables, taking a slot of the default state directory, and saying the warnings."""
        if not _core_loads():
            return None
        env = {name: env[name] for name in _SETUP_VARIABLES if env.get(name)}
        code = setup.SetupInCode(endpoint_name="the endpoint given to configure()",
                                 headers_name="the headers given to configure()", heartbeat_name="heartbeat_interval")
        if endpoint is not None and not isinstance(endpoint, str):
            _log.warning("OpAMP: endpoint should be a string, not %s: this SDK is not managed",
                         type(endpoint).__name__)
            return None
        if endpoint is not None:
            code.endpoint = endpoint
        if headers is not None and not (isinstance(headers, Mapping) and all(
                isinstance(k, str) and isinstance(v, str) for k, v in headers.items())):
            # Not the values: they may be secrets.
            _log.warning("OpAMP: headers should map names to values, all strings: none are sent")
            headers = {}
        if headers is not None:
            code.headers_given = True
            code.headers.extend(setup.Header(name=k, value=v) for k, v in headers.items())
        heartbeat_in_code = False
        if isinstance(heartbeat_interval, str):
            # "30s", read as the variable is; blank: not given.
            if heartbeat_interval.strip():
                env["OTEL_OPAMP_HEARTBEAT_INTERVAL"] = heartbeat_interval
                heartbeat_in_code = True
        elif heartbeat_interval is not None:
            seconds = (heartbeat_interval.total_seconds() if isinstance(heartbeat_interval, timedelta)
                       else heartbeat_interval)
            if isinstance(seconds, (int, float)) and not isinstance(seconds, bool) and math.isfinite(seconds):
                ms = seconds * 1000
                # Less than a millisecond is one; more than the core takes is more than a day.
                code.heartbeat_ms = max(1, round(min(ms, 2**53))) if ms > 0 else round(max(ms, -2**53))
            else:
                _log.warning("OpAMP: heartbeat_interval %r is not a positive duration: using the default",
                             heartbeat_interval)
                env.pop("OTEL_OPAMP_HEARTBEAT_INTERVAL", None)  # displaced, as a bad variable is the default
        given_dir = os.fspath(state_dir) if isinstance(state_dir, os.PathLike) else state_dir
        if given_dir is not None and not isinstance(given_dir, str):
            _log.warning("OpAMP: state_dir should be a path, not %s: using the default", type(state_dir).__name__)
            env.pop("OTEL_OPAMP_STATE_DIR", None)
        elif given_dir is not None:
            code.state_dir = given_dir
        try:
            cwd = os.getcwd()
        except OSError:
            cwd = ""
        start = setup.SetupStart(env=[setup.Variable(name=k, value=v) for k, v in env.items()], os=_setup_os(),
                                 program=program_name(), cwd=cwd, code=code)
        try:
            started = setup.start(start)
        except CoreError as e:
            _log.error("OpAMP: the core could not read the settings; the SDK runs unmanaged: %s", e)
            return None
        for warning in started.warnings:
            if heartbeat_in_code and warning.startswith("OTEL_OPAMP_HEARTBEAT_INTERVAL "):
                # The string given in code, read as the variable: named as given.
                warning = "heartbeat_interval" + warning[len("OTEL_OPAMP_HEARTBEAT_INTERVAL"):]
            _log.warning("OpAMP: %s", warning)
        if started.off:
            # Turned off by a variable (said once, here); off for want of an endpoint says nothing.
            if started.off_reason:
                _log.warning("OpAMP: %s", started.off_reason)
            return None
        scheme = started.endpoint.partition("://")[0].lower()
        if scheme not in ("http", "https", "ws", "wss"):
            named = "OTEL_OPAMP_ENDPOINT" if not (endpoint or "").strip() else code.endpoint_name
            _log.warning('OpAMP: %s "%s" is not http(s):// or ws(s)://, so the client can\'t reach it',
                         named, started.shown_endpoint)
        out = Setup(started.endpoint, {h.name: h.value for h in started.headers},
                    heartbeat_ms=started.heartbeat_interval_ms, shown=started.shown_endpoint, hints=started.hints)
        if started.state_dir:
            out.state_dir = Path(started.state_dir)
        else:
            claimed = slot(started.default_slots, started.all_slots_taken)
            if claimed is not None:
                out.state_dir, out.state_lock = claimed
        return out


@dataclass(frozen=True)
class _Rule:
    """A sampling rule, ready to match: the layer checked it."""

    sampler: Sampler  # its rate, by the SDK's own trace-ID ratio sampler
    name: str | None = None
    kind: int | None = None  # the Python SDK's SpanKind value
    path: str | None = None
    attributes: tuple[tuple[str, str], ...] = ()

    @staticmethod
    def of(rule: settings_pb2.SamplingRule) -> _Rule:
        return _Rule(TraceIdRatioBased(rule.rate),
                     rule.name if rule.HasField("name") else None,
                     # OTLP numbers kinds from INTERNAL 1; the Python SDK from INTERNAL 0.
                     rule.kind - 1 if rule.HasField("kind") else None,
                     rule.path if rule.HasField("path") else None,
                     tuple(sorted(rule.attributes.items())))

    def matches(self, name: str, kind: int, attributes) -> bool:
        if self.name is not None and not glob(self.name, name):
            return False
        if self.kind is not None and self.kind != kind:
            return False
        if self.path is not None:
            path = _path(attributes)
            if path is None or not glob(self.path, path):
                return False
        for key, pattern in self.attributes:
            text = attribute_text(attributes.get(key)) if attributes else None
            if text is None or not glob(pattern, text):
                return False
        return True


def _path(attributes) -> str | None:
    """What a rule's `path` matches: url.path, or else http.target up to its query."""
    if not attributes:
        return None
    path = attributes.get("url.path")
    if isinstance(path, str):
        return path
    target = attributes.get("http.target")
    return target.split("?", 1)[0] if isinstance(target, str) else None


@dataclass(frozen=True)
class _Sampling:
    """What the sampler does, swapped whole when the settings change (a span reads it once)."""

    current: Sampler  # with no rule matching: the rate, else the SDK's own sampler
    rules: tuple[_Rule, ...] = ()
    cap: Bucket | None = None
    #: The rules or the rate decide new traces; a span with a parent follows it.
    decides: bool = False


_FOLLOW = ParentBased(ALWAYS_ON)


class LiveSampler(Sampler):
    """The SDK's own sampler until the server sets a sampling rate or rules, then those for new
    traces (a child follows its parent's decision, as the default does), and a cap on the traces
    started or joined a second. `clock`: the cap's monotonic clock, in nanoseconds."""

    def __init__(self, clock=time.monotonic_ns) -> None:
        self._clock = clock
        self._configured: Sampler = ParentBased(ALWAYS_ON)
        self._rate: float | None = None
        self._rules: tuple[_Rule, ...] = ()
        self._rules_from: tuple = ()
        self._cap: Bucket | None = None
        self._sampling = _Sampling(self._configured)

    @property
    def current(self) -> Sampler:
        """The sampler for a span no rule matches: the rate's, or the SDK's own."""
        return self._sampling.current

    def wrap(self, configured: Sampler | None) -> LiveSampler:
        """The sampler the SDK's configuration built. A rate set before (at start) still holds."""
        self._configured = configured or ParentBased(ALWAYS_ON)
        self._swap()
        return self

    def rate(self, rate: float | None) -> None:
        self._rate = rate
        self._swap()

    def configure(self, rate: float | None, rules=(), per_second: float | None = None) -> None:
        """sampling_rate, sampling_rules (as the layer hands them over) and
        sampling_traces_per_second, at once. The same cap keeps its bucket; a new one is full."""
        self._rate = rate
        key = tuple(r.SerializeToString(deterministic=True) for r in rules)
        if key != self._rules_from:
            self._rules, self._rules_from = tuple(_Rule.of(r) for r in rules), key
        self._cap = rebucket(self._cap, per_second, self._clock)
        self._swap()

    def _swap(self) -> None:
        rate = self._rate
        current = self._configured if rate is None else ParentBased(TraceIdRatioBased(rate))
        self._sampling = _Sampling(current, self._rules, self._cap, bool(self._rules) or rate is not None)

    def should_sample(self, parent_context, trace_id, name, kind=None, attributes=None, links=None,
                      trace_state=None):
        s = self._sampling
        parent = get_current_span(parent_context).get_span_context()
        sampler, _ = self._choose(s, parent, name, kind, attributes)
        result = sampler.should_sample(parent_context, trace_id, name, kind, attributes, links, trace_state)
        cap = s.cap
        # A sampled root, or a span joining a caller's sampled trace, takes a token; a child of a
        # span sampled here never does (refusing it would break its trace), nor one the
        # application's own sampler samples under a caller that didn't sample.
        takes = not parent.is_valid or (parent.is_remote and parent.trace_flags.sampled)
        if cap is not None and result.decision.is_sampled() and takes and cap.take(1) == 0:
            return SamplingResult(Decision.DROP, None, result.trace_state)
        return result

    @staticmethod
    def _choose(s: _Sampling, parent, name, kind, attributes) -> tuple[Sampler, int | None]:
        """The sampler that decides a span, and the index of the rule that chose it, if one did."""
        if not s.decides:
            return s.current, None
        if parent.is_valid:
            return _FOLLOW, None  # rules and rates decide only new traces
        kind_ = (kind if kind is not None else SpanKind.INTERNAL).value
        for i, rule in enumerate(s.rules):
            if rule.matches(name, kind_, attributes):
                return rule.sampler, i
        return s.current, None

    def get_description(self) -> str:
        s = self._sampling
        rules = f",rules={len(s.rules)}" if s.rules else ""
        cap = f",traces_per_second={s.cap.per_second}" if s.cap is not None else ""
        return f"OpampLiveSampler{{{s.current.get_description()}{rules}{cap}}}"


class Counts:
    """What a signal's gate saw since start, for export health: after the exporter's own retries."""

    #: The error text when an exporter answers FAILURE: the Python exporters log why, and don't say.
    FAILURE = "the exporter answered FAILURE (its own log says why)"

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._c = settings_pb2.SignalCounts()

    def exported(self, items: int, result) -> None:
        """An export of `items` came back with `result`: failed when it is FAILURE."""
        if getattr(result, "name", None) == "FAILURE":
            self.failed(items, self.FAILURE)
            return
        with self._lock:
            self._c.exports_ok += 1
            self._c.last_failed = False

    def failed(self, items: int, error: str) -> None:
        with self._lock:
            self._c.exports_failed += 1
            self._c.items_failed += items
            self._c.last_failed = True
            self._c.last_error = error

    def filtered(self, items: int) -> None:
        """Dropped on purpose: send_* false, a level, a cap."""
        if items:
            with self._lock:
                self._c.items_filtered += items

    def proto(self) -> settings_pb2.SignalCounts:
        with self._lock:
            out = settings_pb2.SignalCounts()
            out.CopyFrom(self._c)
            return out


def _export(counts: Counts, items: int, export):
    """Call the exporter, counting what came of it; an exception is counted, then raised as before."""
    try:
        result = export()
    except Exception as e:
        counts.failed(items, str(e) or type(e).__name__)
        raise
    counts.exported(items, result)
    return result


@dataclass(frozen=True)
class Gates:
    """Whether each signal is sent, and what the log and span gates hold exports to: one
    document's worth, never changed (Controls.apply puts a new one in force)."""

    send_traces: bool = True
    send_metrics: bool = True
    send_logs: bool = True
    #: The lowest SeverityNumber of a log record exported (send_logs_level); None: every one.
    logs_min_severity: int | None = None
    #: send_logs_level_by_scope: (pattern, lowest SeverityNumber), as the layer sorts them.
    logs_scope_levels: tuple[tuple[str, int], ...] = ()
    #: send_logs_per_second's bucket; None: no cap.
    logs_cap: Bucket | None = None
    #: drop_attributes and mask_attributes, then scrubbing, for spans and log records at export.
    redactor: Redactor = field(default_factory=lambda: Redactor((), ()))


class Controls:
    """What the server changes while the SDK runs. `clock`: the monotonic clock the rate caps
    refill on, in nanoseconds. `app`: where the application reads its own settings (custom)."""

    def __init__(self, clock=time.monotonic_ns, app: AppSettings | None = None) -> None:
        self._clock = clock
        self.app = app if app is not None else AppSettings()
        self.sampler = LiveSampler(clock)
        #: What each signal's gate counted since start, for export health.
        self.traces, self.metrics, self.logs_counts = Counts(), Counts(), Counts()
        #: What the gates export by, swapped whole by apply: a gate reads it once per export, so
        #: an export is held to one document's settings, never to half of one and half of the next.
        self.gates = Gates()

    @property
    def send_traces(self) -> bool:
        return self.gates.send_traces

    @property
    def send_metrics(self) -> bool:
        return self.gates.send_metrics

    @property
    def send_logs(self) -> bool:
        return self.gates.send_logs

    @property
    def logs_min_severity(self) -> int | None:
        return self.gates.logs_min_severity

    @property
    def logs_scope_levels(self) -> tuple[tuple[str, int], ...]:
        return self.gates.logs_scope_levels

    @property
    def logs_cap(self) -> Bucket | None:
        return self.gates.logs_cap

    @property
    def redactor(self) -> Redactor:
        return self.gates.redactor

    def apply(self, s: settings_pb2.Settings) -> None:
        """Carry out the live settings; a setting left out is the SDK's own."""
        self.sampler.configure(s.sampling_rate if s.HasField("sampling_rate") else None, s.sampling_rules,
                               s.sampling_traces_per_second if s.HasField("sampling_traces_per_second") else None)
        was = self.gates
        # The same cap keeps its bucket (a refresh puts the same settings in force); a new one is full.
        cap = rebucket(was.logs_cap, s.send_logs_per_second if s.HasField("send_logs_per_second") else None,
                       self._clock)
        drop, mask = tuple(s.drop_attributes), tuple(s.mask_attributes)
        scrub = (s.scrub_defaults, tuple(s.scrub_patterns))
        r = was.redactor
        redactor = r if (drop, mask, scrub) == (r.drop, r.mask, (r.scrubber.defaults, r.scrubber.patterns)) \
            else Redactor(drop, mask, Scrubber(*scrub))
        # One store: an export running meanwhile is held to the last document or to this one, whole.
        self.gates = Gates(
            send_traces=not s.HasField("send_traces") or s.send_traces,
            send_metrics=not s.HasField("send_metrics") or s.send_metrics,
            send_logs=not s.HasField("send_logs") or s.send_logs,
            logs_min_severity=s.send_logs_min_severity if s.HasField("send_logs_min_severity") else None,
            logs_scope_levels=tuple((lv.pattern, lv.min_severity) for lv in s.send_logs_scope_levels),
            logs_cap=cap, redactor=redactor)
        # Last: a callback the application gave sees the SDK's settings already in force.
        self.app.put(s.custom)

    def spans(self, to: SpanExporter) -> SpanExporter:
        return _GatedSpanExporter(to, self)

    def logs(self, to: LogRecordExporter) -> LogRecordExporter:
        return _GatedLogExporter(to, self)

    def counts(self) -> settings_pb2.ExportCounts:
        return settings_pb2.ExportCounts(traces=self.traces.proto(), metrics=self.metrics.proto(),
                                         logs=self.logs_counts.proto())


def disabled_instrumentations(s: settings_pb2.Settings, local: str) -> str | None:
    """OTEL_PYTHON_DISABLED_INSTRUMENTATIONS with the next-start settings added to the SDK's own
    list; None when they add nothing."""
    if s.HasField("deactivate_all_instrumentations") and s.deactivate_all_instrumentations:
        return "*"
    names = list(s.deactivate_instrumentations.names)
    if not names:
        return None
    own = [n.strip() for n in local.split(",") if n.strip()]
    return ",".join(dict.fromkeys(own + names))


class _GatedSpanExporter(SpanExporter):
    def __init__(self, to: SpanExporter, controls: Controls) -> None:
        self._to, self._c = to, controls

    def export(self, spans):
        """send_traces, then redaction and scrubbing."""
        c, g = self._c, self._c.gates  # one document's gates for the whole export
        if not g.send_traces:
            c.traces.filtered(len(spans))
            return SpanExportResult.SUCCESS
        redactor = g.redactor
        if redactor:
            spans = [_redacted_span(span, redactor) for span in spans]
        return _export(c.traces, len(spans), lambda: self._to.export(spans))

    def shutdown(self) -> None:
        self._to.shutdown()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return self._to.force_flush(timeout_millis)


class _GatedLogExporter(LogRecordExporter):
    def __init__(self, to: LogRecordExporter, controls: Controls) -> None:
        self._to, self._c = to, controls

    def export(self, batch):
        """send_logs, then the levels, then the rate cap, then redaction and scrubbing (of the
        attributes and the body); a batch left empty isn't
        exported. What they drop is counted as filtered."""
        c, g = self._c, self._c.gates  # one document's gates for the whole export
        counts, given = c.logs_counts, len(batch)
        if g.send_logs:
            floor, levels, cap, redactor = g.logs_min_severity, g.logs_scope_levels, g.logs_cap, g.redactor
            if floor is None and not levels and cap is None and not redactor:
                return _export(counts, given, lambda: self._to.export(batch))
            if floor is not None or levels:
                batch = [r for r in batch if _kept(r, floor, levels)]
            if cap is not None and batch:
                # The first records the tokens allow; the rest are dropped from the end.
                batch = list(batch)[:cap.take(len(batch))]
            if redactor and batch:
                batch = [_redacted_record(r, redactor) for r in batch]
            counts.filtered(given - len(batch))
            if batch:
                return _export(counts, len(batch), lambda: self._to.export(batch))
        else:
            counts.filtered(given)
        from opentelemetry.sdk._logs.export import LogRecordExportResult  # noqa: PLC0415

        return LogRecordExportResult.SUCCESS

    def shutdown(self) -> None:
        self._to.shutdown()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return self._to.force_flush(timeout_millis)


def _kept(record, floor: int | None, levels: tuple[tuple[str, int], ...] = ()) -> bool:
    """A log record at its scope's level (send_logs_level_by_scope) or send_logs_level, or above;
    or one with no severity (0 or unset: it can't be judged), unless its scope is off. The scope is
    the one it is exported under: the Python logger's name, for the logging handler's records."""
    severity = getattr(getattr(record, "log_record", record), "severity_number", None)
    number = int(getattr(severity, "value", severity) or 0)
    if levels:
        scope = getattr(getattr(record, "instrumentation_scope", None), "name", None) or ""
        floor = threshold(levels, floor, scope)
    return level_keeps(floor, number)


def _redacted_attributes(attributes, redacted: dict):
    """Attributes as exported (`redacted`, Redactor.apply's), in a container of their own: the
    original is shared with every other processor and exporter, and stays as it was. What the
    SDK's limits dropped still counts (a redaction's drop doesn't)."""
    from opentelemetry.attributes import BoundedAttributes  # noqa: PLC0415

    out = BoundedAttributes(maxlen=None, attributes=redacted, immutable=True)
    out.dropped = getattr(attributes, "dropped", 0)
    return out


class _RedactedSpan(ReadableSpan):
    """A span as exported after redaction: what the SDK dropped by its limits is the original's."""

    def __init__(self, original: ReadableSpan, **kwargs) -> None:
        super().__init__(**kwargs)
        self._original = original

    @property
    def dropped_attributes(self) -> int:
        return self._original.dropped_attributes

    @property
    def dropped_events(self) -> int:
        return self._original.dropped_events

    @property
    def dropped_links(self) -> int:
        return self._original.dropped_links


def _redacted_span(span: ReadableSpan, redactor: Redactor) -> ReadableSpan:
    """The span with its attributes, its events' and its links' redacted and scrubbed; the span
    itself when nothing in it changes. A new span, never the original changed."""
    events, links = span.events or (), span.links or ()
    own = redactor.apply(span.attributes)
    of_events = [redactor.apply(e.attributes) for e in events]
    of_links = [redactor.apply(link.attributes) for link in links]
    if own is None and all(r is None for r in of_events) and all(r is None for r in of_links):
        return span

    def redact(attributes, redacted):
        return attributes if redacted is None else _redacted_attributes(attributes, redacted)

    return _RedactedSpan(
        span, name=span.name, context=span.get_span_context(), parent=span.parent, resource=span.resource,
        attributes=redact(span.attributes, own),
        events=tuple(Event(e.name, redact(e.attributes, r), e.timestamp) for e, r in zip(events, of_events)),
        links=tuple(Link(link.context, redact(link.attributes, r)) for link, r in zip(links, of_links)),
        kind=span.kind, status=span.status, start_time=span.start_time, end_time=span.end_time,
        instrumentation_scope=span.instrumentation_scope)


def _redacted_record(record, redactor: Redactor):
    """The log record with its attributes redacted and scrubbed and its body scrubbed; the record
    itself when nothing changes. Its LogRecord is shared with every other processor, and
    ReadableLogRecord is frozen: a copy."""
    inner = getattr(record, "log_record", None)
    if inner is None:
        return record
    redacted = redactor.apply(inner.attributes)
    body = redactor.body(inner.body)
    if redacted is None and body is Scrubber._UNCHANGED:
        return record
    inner = copy.copy(inner)
    if redacted is not None:
        inner.attributes = _redacted_attributes(inner.attributes, redacted)
    if body is not Scrubber._UNCHANGED:
        inner.body = body
    return dataclasses.replace(record, log_record=inner)


#: The application's own settings (docs/app-settings.md): declared always, as the application reads
#: them through app_settings whoever built the SDK.
APP_SETTINGS = "custom"
#: The settings the package's sampler carries out, where its Configurator built the SDK.
SAMPLER = ("sampling_rate", "sampling_rules", "sampling_traces_per_second")
#: Those of the gate on every trace exporter (the SDK's export span processor wraps them all).
TRACE_GATE = ("send_traces",)
#: Those of the gate on every log exporter (the export log record processor, likewise).
LOG_GATE = ("send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second")
#: Redaction and scrubbing, at both: every span and log record exported has to pass one.
REDACTION = ("drop_attributes", "mask_attributes", "scrub_defaults", "scrub_patterns")
#: Debug mode: the layer folds it into the rate and the level, which the timer puts in force.
DEBUG = ("debug_until", "debug_sampling_rate", "debug_send_logs_level")
#: The instrumentation switches, which only opentelemetry-instrument reads, at start.
INSTRUMENTATIONS = ("deactivate_instrumentations", "deactivate_all_instrumentations")


def gated_metric_exporters(names: list[str]) -> list[str]:
    """The metric exporters as the SDK is to build them: the OTLP one in the package's gate (the
    ``opamp`` exporter, which builds OTLP inside it); metrics have no export-processor hook."""
    return ["opamp" if n == "otlp" else n for n in names]


def _every_metric_exporter_gated(names: list[str]) -> bool:
    """Whether every metric exporter the SDK builds from `names` goes through the gate (``none``,
    in any case, is none). One that doesn't (prometheus, console, otlp_proto_http) keeps exporting
    whatever send_metrics says, so then it isn't carried out at all, rather than by half."""
    return all(n == "opamp" or not n or n.lower() == "none" for n in gated_metric_exporters(names))


def read_if_there(path: Path) -> bytes | None:
    """The file's bytes, or None if there is none."""
    try:
        return path.read_bytes()
    except FileNotFoundError:
        return None


def sdk_host(metrics_exporters: list[str], configured: bool = True, auto_instrumented: bool = True,
             config_file: bool = False, span_processors: bool = False,
             log_processors: bool = False) -> settings_pb2.Host:
    """What this package carries out, declared from what it builds (docs/sdk-settings.md, "The
    host"); the layer sets aside every setting not named. Where its Configurator built the SDK
    (`configured`), the sampler and the trace and log gates are its own, but not a span or log
    record processor the application passed in (`span_processors`, `log_processors`): that one
    exports past the gate. send_metrics where every metric exporter (`metrics_exporters`, as
    named) is gated, unless a declarative configuration file (`config_file`) built them. The
    instrumentation switches, at the next start, only under opentelemetry-instrument
    (`auto_instrumented`). The application's own settings (custom)
    always: the application reads them, whoever built the SDK."""
    live: list[str] = [APP_SETTINGS]
    if configured:
        live += SAMPLER + DEBUG
        if not span_processors:
            live += TRACE_GATE
        if not log_processors:
            live += LOG_GATE
        if not span_processors and not log_processors:
            live += REDACTION
    if not config_file and _every_metric_exporter_gated(metrics_exporters):
        live.append("send_metrics")
    return settings_pb2.Host(live=live, next_start=INSTRUMENTATIONS if auto_instrumented else ())


def _undeclared(host: settings_pb2.Host) -> list[str]:
    """The settings `host` doesn't carry out, for a warning."""
    every = SAMPLER + TRACE_GATE + LOG_GATE + REDACTION + DEBUG + ("send_metrics",) + INSTRUMENTATIONS
    return [k for k in every if k not in host.live and k not in host.next_start]


#: The package as published, which the inventory names.
PACKAGE = "pydantic-opamp-otel"
#: Exporter entry points by the name OTEL_*_EXPORTER gives them in every language: the OTLP ones
#: by their protocol, and this package's gated OTLP metric exporter, are `otlp`.
_EXPORTER_NAMES = {"otlp_proto_grpc": "otlp", "otlp_proto_http": "otlp", "opamp": "otlp", "zipkin_json": "zipkin",
                   "zipkin_proto": "zipkin"}


def instrumentations() -> list[settings_pb2.Instrumentation]:
    """The instrumentations opentelemetry-instrument can run, switched off or not: its entry points
    (the names OTEL_PYTHON_DISABLED_INSTRUMENTATIONS, and so deactivate_instrumentations, takes),
    but for those whose library isn't installed or conflicts, which it skips. Each at its
    distribution's version."""
    from opentelemetry.instrumentation.dependencies import get_dist_dependency_conflicts  # noqa: PLC0415
    from opentelemetry.util._importlib_metadata import entry_points  # noqa: PLC0415

    out = []
    for ep in entry_points(group="opentelemetry_instrumentor"):
        dist = getattr(ep, "dist", None)
        try:
            if dist is not None and get_dist_dependency_conflicts(dist) is not None:
                continue
        except Exception:  # noqa: BLE001 - a requirement it can't read: listed, as upstream would try it
            _log.debug("OpAMP: the requirements of %s", ep.name, exc_info=True)
        out.append(settings_pb2.Instrumentation(name=ep.name, version=getattr(dist, "version", None) or ""))
    return out


def _exporter_name(signal: str, kind: type) -> str:
    """An exporter's (or a metric reader's) name: its entry point's, as OTEL_*_EXPORTER names it,
    or else its type's. Only entry points whose module is loaded are looked at: one that isn't
    can't be this one, and nothing is imported to find out."""
    from opentelemetry.util._importlib_metadata import entry_points  # noqa: PLC0415

    names = []
    for ep in entry_points(group=f"opentelemetry_{signal}_exporter"):
        module, _, attr = ep.value.partition(":")
        found = sys.modules.get(module.strip())
        if found is None or not attr.strip():
            continue
        for part in attr.strip().split("."):
            found = getattr(found, part, None)
        if found is kind:
            names.append(_EXPORTER_NAMES.get(ep.name, ep.name))
    return min(names) if names else kind.__name__


def _processors_exporter(processor):
    """The exporter a span or log record processor exports to (the SDK's batch and simple ones),
    or None: a processor that isn't an export processor, or one whose exporter it can't see."""
    batch = getattr(processor, "_batch_processor", None)
    for holder, attr in ((batch, "_exporter"), (processor, "span_exporter"), (processor, "_exporter")):
        exporter = getattr(holder, attr, None) if holder is not None else None
        if exporter is not None:
            return exporter
    return None


def exporters(tracer_provider, meter_provider, logger_provider) -> list[settings_pb2.Exporter]:
    """The exporters the SDK was built with, read off its providers once it is built, each with
    whether the package's send_* gate wraps it: the trace and log exporters its configurator put
    behind its export processors, and its OTLP metric exporter. Everything else (a span processor
    given to configure(), a declarative configuration file's exporters, a Prometheus reader) is
    ungated, as the host says (sdk_host). A provider that isn't the SDK's has none."""
    out = []
    multi = getattr(tracer_provider, "_active_span_processor", None)
    for p in getattr(multi, "_span_processors", ()):
        e = _processors_exporter(p)
        if e is not None:
            gated = isinstance(e, _GatedSpanExporter)
            inner = e._to if gated else e  # noqa: SLF001
            out.append(settings_pb2.Exporter(signal="traces", name=_exporter_name("traces", type(inner)), gated=gated))
    for reader in getattr(meter_provider, "_metric_readers", ()):
        # A push exporter is under a periodic reader; a pull one (Prometheus) is the reader itself.
        inner = getattr(reader, "_exporter", None)
        inner = reader if inner is None else inner
        out.append(settings_pb2.Exporter(signal="metrics", name=_exporter_name("metrics", type(inner)),
                                         gated=isinstance(inner, GatedOtlpMetricExporter)))
    multi = getattr(logger_provider, "_multi_log_record_processor", None)
    for p in getattr(multi, "_log_record_processors", ()):
        e = _processors_exporter(p)
        if e is not None:
            gated = isinstance(e, _GatedLogExporter)
            inner = e._to if gated else e  # noqa: SLF001
            out.append(settings_pb2.Exporter(signal="logs", name=_exporter_name("logs", type(inner)), gated=gated))
    return out


def inventory(host: settings_pb2.Host | None = None, tracer_provider=None, meter_provider=None,
              logger_provider=None, listed: list[tuple[str, str]] | None = None) -> settings_pb2.Inventory:
    """What the package lists of itself and the SDK it runs (docs/available-components.md): the
    global providers unless others are given. Under `host`, a signal whose send_* setting it
    can't carry out has no gated exporter: the OTLP metric exporter beside one the package can't
    stop is gated, but send_metrics is not declared, so nothing reaches it. `listed`: the gated
    exporters (signal, name) built outside the providers' reach (configure_logfire's), in place of
    reading them off the providers."""
    from importlib.metadata import PackageNotFoundError, version  # noqa: PLC0415

    from opentelemetry import _logs, metrics, trace  # noqa: PLC0415
    from opentelemetry.sdk.version import __version__ as sdk_version  # noqa: PLC0415

    try:
        own = version(PACKAGE)
    except PackageNotFoundError:
        own = ""
    if listed is not None:
        built = [settings_pb2.Exporter(signal=signal, name=name, gated=True) for signal, name in listed]
    else:
        built = exporters(tracer_provider or trace.get_tracer_provider(), meter_provider or metrics.get_meter_provider(),
                          logger_provider or _logs.get_logger_provider())
    if host is not None:
        carried = {*host.live, *host.next_start}
        for e in built:
            e.gated = e.gated and f"send_{e.signal}" in carried
    return settings_pb2.Inventory(
        integration=PACKAGE, integration_version=own, sdk_language="python", sdk_version=sdk_version,
        instrumentations=instrumentations(), exporters=built)


class Agent:
    """The package's state: one per process. `app`: where the application reads its own settings."""

    def __init__(self, app: AppSettings | None = None) -> None:
        self.controls = Controls(app=app)
        self.layer: SdkSettings | None = None
        self.setup: Setup | None = None
        self.loaded = settings_pb2.Loaded()
        self.host = settings_pb2.Host()
        self.current = b"{}"
        self.effective = opamp_pb2.EffectiveConfig()
        self.uid: uuid.UUID | None = None
        #: The package's Configurator built the SDK, so the sampler and exporters are its own.
        self.configured = False
        #: What the Configurator was given besides: span and log record processors of the
        #: application's own (which export past the gates), and metric exporters by name.
        self.span_processors = False
        self.log_processors = False
        self.extra_metric_exporters: list[str] = []
        #: The gated exporters configure_logfire built, for the inventory (None: read off the providers).
        self.listed_exporters: list[tuple[str, str]] | None = None
        #: configure_logfire built the SDK (the Logfire SDK's own), for what the warnings say.
        self.logfire = False
        self.client: Client | None = None
        self._loop: asyncio.AbstractEventLoop | None = None
        # Reentrant: a decision under it re-arms the timer, which takes it too.
        self._lock = threading.RLock()
        #: Wakes the timer (refresh_at_unix_ms, export health) when a decision re-arms it.
        self._wake = threading.Condition(self._lock)
        #: The clocks: the wall clock in milliseconds since the Unix epoch, which the layer decides
        #: debug mode by, and the monotonic one in seconds, which the timer waits on.
        self.wall_ms = lambda: time.time_ns() // 1_000_000
        self.monotonic = time.monotonic
        #: When the layer said to call Refresh (refresh_at_unix_ms; 0: never), and that as a
        #: monotonic deadline (None: never), so a wall-clock jump while it waits doesn't move it.
        self._refresh_at = 0
        self._refresh_deadline: float | None = None
        self._ticking = False
        #: Export health: the layer's last report, and when to ask it again (monotonic).
        self._health: settings_pb2.HealthReport | None = None
        self._health_next = 0.0
        self._started_ns = time.time_ns()
        self._saved: bytes | None = None
        self._core_state: bytes | None = None
        self._metric_exporters: list[str] = []
        self._auto_instrumented = True
        self._hooked = False
        self._inherited: tuple = ()
        #: The live settings in force, as the layer handed them over.
        self._applied = settings_pb2.Settings()
        #: Where the process that talks to the server hands its live settings to the processes
        #: forked from it; made at the first fork (see _before_fork).
        self._relay: Path | None = None
        self._relay_owner = 0
        self.reachability: Reachability | None = None
        #: The inventory as reported at start (available components); None before or without.
        self.components: opamp_pb2.AvailableComponents | None = None

    @property
    def on(self) -> bool:
        return self.setup is not None

    def prepare(self, env=os.environ, auto_instrumented: bool = True, **given) -> None:
        """Before the SDK is built: load the saved settings, put them in force (next-start ones
        through the instrumentation's own switch, which only opentelemetry-instrument reads: not
        `auto_instrumented`, they aren't carried out), and name the instance. `given` is the server
        as the application gave it in code (Setup.from_env)."""
        self.setup = Setup.from_env(env, **given)
        if self.setup is None:
            return
        self.layer = SdkSettings()
        names = [n.strip() for n in env.get("OTEL_METRICS_EXPORTER", "otlp").split(",")]
        self._metric_exporters = names
        self._auto_instrumented = auto_instrumented
        self.host = sdk_host(names, auto_instrumented=auto_instrumented)
        saved = core_state = None
        if self.setup.state_dir is not None:
            try:
                # The client's state in it can hold credentials the server issued.
                self.setup.state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
                # The settings to start with are the last config applied, which the client keeps in
                # its state (opamp-state); sdk-settings.json is what this package kept itself
                # before, read only while the client's state holds no config applied.
                core_state = read_if_there(self.setup.state_dir / "opamp-state")
                saved = read_if_there(self.setup.state_dir / "sdk-settings.json")
            except OSError as e:
                _log.warning("OpAMP: saved SDK settings unreadable, using the SDK's own: %s", e)
        self._saved, self._core_state = saved, core_state
        self.loaded = self.layer.load(saved, self.host, self.wall_ms(), core_state=core_state)
        if self.loaded.warning:
            _log.warning("OpAMP: %s", self.loaded.warning)
        self.current = self.loaded.current
        self.effective = self.loaded.effective
        self._put_in_force(self.loaded.settings)
        self._arm(self.loaded.refresh_at_unix_ms)
        disabled = disabled_instrumentations(self.loaded.settings, env.get("OTEL_PYTHON_DISABLED_INSTRUMENTATIONS", ""))
        if disabled is not None:
            env["OTEL_PYTHON_DISABLED_INSTRUMENTATIONS"] = disabled
        env["OTEL_METRICS_EXPORTER"] = ",".join(gated_metric_exporters(names))
        # The UID reaches the SDK through the Configurator, not the environment: a child process
        # inherits the environment, and would join the fleet as this instance.
        self.uid = self._identity(env)

    def _identity(self, env) -> uuid.UUID:
        """One UID names the instance to the server and in the telemetry: the one saved in the
        state directory; else a new one, saved there; else, where none can be kept (no state
        directory, or one it can't be saved in), a UUID service.instance.id given in
        OTEL_RESOURCE_ATTRIBUTES, which is the same at every start; else a new one."""
        directory = self.setup.state_dir if self.setup else None
        path = directory / setup.UID_FILE if directory is not None else None
        saved = ""
        if path is not None:
            try:
                saved = path.read_text(encoding="utf-8")
            except (OSError, ValueError):  # UnicodeDecodeError is one
                pass
        identity = setup.SetupIdentity(state_dir=path is not None, saved=saved,
                                       resource_attributes=env.get("OTEL_RESOURCE_ATTRIBUTES", ""),
                                       entropy=secrets.token_bytes(16), unix_ms=int(time.time() * 1000))
        try:
            got = setup.identify(identity)
        except CoreError as e:
            _log.error("OpAMP: the core could not make an instance UID: %s", e)
            return uuid.uuid4()
        if got.save and path is not None:
            try:
                save_state(str(path), got.text.encode())
            except OSError as e:
                _log.warning("OpAMP: can't save the instance UID: %s", e)
                return uuid.UUID(bytes=got.unsaved_uid)
        return uuid.UUID(bytes=got.instance_uid)

    def _endpoint_in_use(self) -> str | None:
        """The endpoint the client uses now, if a client runs. (The client is read once: a stop
        that clears it meanwhile leaves this call with the one it read.)"""
        client = self.client
        return client.endpoint if client is not None else None

    def start(self, resource: Resource) -> None:
        """Once the SDK is built: join the fleet, on a thread of the client's own."""
        if self.setup is None or self.layer is None or self.uid is None or self._loop is not None:
            return
        # What was built is known now: the settings it cannot carry out are declared no more, so
        # a policy that overrides one is refused. The settings loaded at start are loaded again,
        # under the host as built.
        config_file = bool(os.environ.get("OTEL_CONFIG_FILE"))
        built = self._built_host()
        if built != self.host:
            self.host = built
            lost = ", ".join(_undeclared(built))
            if config_file:
                _log.warning("OpAMP: the SDK was built from OTEL_CONFIG_FILE, so its sampler and "
                             "exporters aren't this package's: %s can't be carried out", lost)
            elif not self.configured:
                _log.warning("OpAMP: the SDK wasn't built by this package's configurator "
                             "(OTEL_PYTHON_CONFIGURATOR=opamp picks it): %s can't be carried out", lost)
            elif self.logfire:
                _log.warning("OpAMP: the Logfire SDK's console, or a processor, metric reader or OTLP exporter "
                             "it was given, exports past this package's gates: %s can't be carried out "
                             "(console=False keeps the console out)", lost)
            else:
                _log.warning("OpAMP: the processors or metric exporters passed to the configurator export "
                             "past this package's gates: %s can't be carried out", lost)
            self.loaded = self.layer.load(self._saved, self.host, self.wall_ms(), core_state=self._core_state)
            self.current = self.loaded.current
            self.effective = self.loaded.effective
            self._put_in_force(self.loaded.settings)
            self._arm(self.loaded.refresh_at_unix_ms)
        loop = _loop_thread("opamp")
        self._loop = loop
        s = self.setup
        config = Config(endpoint=s.endpoint, instance_uid=self.uid.bytes,
                        # What the integration does and reports, declared: the client takes
                        # no status field whose bit isn't.
                        capabilities=(opamp_pb2.AgentCapabilities_AcceptsRemoteConfig
                                      | opamp_pb2.AgentCapabilities_ReportsRemoteConfig
                                      | opamp_pb2.AgentCapabilities_ReportsHeartbeat
                                      | opamp_pb2.AgentCapabilities_ReportsHealth
                                      | opamp_pb2.AgentCapabilities_ReportsEffectiveConfig
                                      | opamp_pb2.AgentCapabilities_ReportsAvailableComponents),
                        heartbeat_interval_ms=s.heartbeat_ms, headers=s.headers,
                        state_file=str(s.state_dir / "opamp-state") if s.state_dir else None,
                        # A key of its own, offered by the server: taken with a state directory
                        # (state_file) and kept there; without one the client neither declares
                        # nor takes it.
                        accept_connection_settings=True,
                        # Platform mode, asked for by name: the client's requests go through aiohttp,
                        # which is what the suppressed context below keeps out of the traces.
                        mode="platform")
        self.reachability = Reachability(s.shown, str(resource.attributes.get("service.name", "")), self.uid,
                                         s.hints)
        self.reachability.in_use = self._endpoint_in_use
        with self._lock:
            first = self._export_health()  # the first report is always sent: with the agent's first message
        status = Status(description=self.layer.describe(attributes(resource), self.host, self.wall_ms()),
                        health=self._health_message(first), effective_config=self.effective,
                        available_components=self._components())

        async def begin() -> None:
            # The client's own requests aren't the application's: everything it starts inherits
            # this context, so no instrumentation traces them.
            _suppress()
            # Set here, on the loop: the client hands over notifications on the loop as soon as
            # start returns. Set on this thread after .result(), a loaded machine delivered the
            # first remote config first, _notified dropped it unanswered, and no remote config
            # came after it.
            self.client = Client.start(config, status, self._notified)

        try:
            _result(asyncio.run_coroutine_threadsafe(begin(), loop), 10)
        except Exception:  # noqa: BLE001 - the application runs on, unmanaged
            _log.exception("OpAMP: the client did not start; the SDK runs unmanaged")
        # With a client or without: the settings loaded at start are in force, and a debug window
        # in them ends on its own, whether the server can reach the instance or not.
        self._ticking = True
        threading.Thread(target=self._timer, name="opamp-timer", daemon=True).start()
        if not self._hooked:
            self._hooked = True
            _at_exit(self.stop)
            if hasattr(os, "register_at_fork"):
                os.register_at_fork(before=self._before_fork, after_in_child=self._forked)

    def _built_host(self) -> settings_pb2.Host:
        """What the package carries out of the SDK as built (sdk_host), as it knows it now."""
        return sdk_host(self._metric_exporters + self.extra_metric_exporters, configured=self.configured,
                        auto_instrumented=self._auto_instrumented,
                        config_file=bool(os.environ.get("OTEL_CONFIG_FILE")),
                        span_processors=self.span_processors, log_processors=self.log_processors)

    def processor_added(self, signal: str) -> None:
        """The application added a span (`signal` "traces") or log record ("logs") processor to
        the SDK once it was built: it exports past the gate, as one passed to the configurator
        does. The gate's settings are declared no more, and what is in force is decided again and
        reported, so the settings it can't carry out are named as set aside from now on rather
        than left answered as done."""
        with self._lock:
            if signal == "traces":
                if self.span_processors:
                    return
                self.span_processors = True
            else:
                if self.log_processors:
                    return
                self.log_processors = True
            built = self._built_host()
            if built == self.host or self.layer is None:
                return
            lost = [k for k in _undeclared(built) if k not in _undeclared(self.host)]
            self.host = built
            _log.warning("OpAMP: a %s processor added to the SDK once it was built exports past this package's "
                         "gate: %s can't be carried out", "span" if signal == "traces" else "log record",
                         ", ".join(lost))
            self._refresh()
            if self.client is not None and self.components is not None:
                components = self._components()
                if components is not None:
                    self.client.set_available_components(components)

    def _components(self) -> opamp_pb2.AvailableComponents | None:
        """The inventory, once the SDK is built, as the available components: under the same host
        and clock as the decisions. Nothing the package lists changes while it runs (the host is
        settled by now), so it is sent once; None, unreported, should it fail."""
        assert self.layer is not None
        try:
            self.components = self.layer.components(inventory(self.host, listed=self.listed_exporters), self.host,
                                                     self.wall_ms())
        except Exception:  # noqa: BLE001 - reporting: the instance is managed without it
            _log.warning("OpAMP: can't list what this SDK runs", exc_info=True)
            self.components = None
        return self.components

    def stop(self) -> None:
        with self._wake:
            self._ticking = False
            self._wake.notify_all()
        client, loop = self.client, self._loop
        self.client = None
        if client is not None and loop is not None:
            try:
                async def end() -> None:
                    _suppress()  # the goodbye is the agent's own request too
                    await client.stop()
                    # The loop's resolver threads end here, on the loop's thread: under gevent they
                    # are greenlets of its hub, which no other thread can join at exit.
                    await loop.shutdown_default_executor()

                # Bounded by the core (stop_timeout_ms, 5 s by default): no race of our own.
                _result(asyncio.run_coroutine_threadsafe(end(), loop))
            except Exception:  # noqa: BLE001
                _log.debug("OpAMP: stopping the client", exc_info=True)
        if self._relay is not None and self._relay_owner == os.getpid():
            shutil.rmtree(self._relay.parent, ignore_errors=True)
            self._relay = None

    def _arm(self, refresh_at_unix_ms: int) -> None:
        """One timer for the layer's refresh_at_unix_ms (0: none), in place of any earlier one: a
        decision that came after a timer was armed replaces it, so the old one never fires (the
        deadline is read under the same lock)."""
        self._refresh_at = refresh_at_unix_ms
        if refresh_at_unix_ms:
            self._refresh_deadline = self.monotonic() + max(refresh_at_unix_ms - self.wall_ms(), 0) / 1000
        else:
            self._refresh_deadline = None
        with self._wake:
            self._wake.notify_all()

    def _timer(self) -> None:
        """The timer's thread: wait for the next deadline, or a decision that moves it."""
        with self._wake:
            while self._ticking:
                wait = self._tick()
                self._wake.wait(wait)

    def _tick(self) -> float | None:
        """Under the lock: whatever is due now (the refresh, export health); how long until the
        next deadline (None: none)."""
        now = self.monotonic()
        if self._refresh_deadline is not None and now >= self._refresh_deadline:
            self._refresh()
        if self._ticking and now >= self._health_next:
            report = self._export_health()
            if report.send and self.client is not None:
                self.client.set_health(self._health_message(report))
        deadlines = [d for d in (self._refresh_deadline, self._health_next if self._ticking else None) if d is not None]
        return max(min(deadlines) - self.monotonic(), 0) if deadlines else None

    def _export_health(self) -> settings_pb2.HealthReport:
        """Under the lock: the layer's export health report from the gates' counts, every
        HEALTH_INTERVAL seconds. It says whether to send it: at once on a change of health or
        status, at most a minute apart while only the counters move."""
        assert self.layer is not None
        report = self.layer.health(self.controls.counts(), self._health, self.wall_ms(), self._applied)
        self._health = report
        self._health_next = self.monotonic() + HEALTH_INTERVAL
        return report

    def _health_message(self, report: settings_pb2.HealthReport) -> opamp_pb2.ComponentHealth:
        """The agent's health: healthy, as the application is running; export health under
        component_health_map["exporters"], which a failing exporter makes unhealthy."""
        health = opamp_pb2.ComponentHealth(healthy=True, start_time_unix_nano=self._started_ns)
        health.component_health_map["exporters"].CopyFrom(report.exporters)
        return health

    def _refresh(self, now_unix_ms: int | None = None) -> None:
        """Under the lock, at refresh_at_unix_ms: what is in force now (the end of debug mode), put
        in force and reported, with or without the server. The monotonic deadline decides that it
        is time: a wall clock set back meanwhile doesn't hold debug mode open."""
        assert self.layer is not None
        now = max(self.wall_ms(), self._refresh_at) if now_unix_ms is None else now_unix_ms
        r = self.layer.refresh(self.current, self.loaded.current, now, self.host)
        if r.warning:
            _log.warning("OpAMP: %s", r.warning)
        self._put_in_force(r.settings)
        self.effective = r.effective
        self._arm(r.refresh_at_unix_ms)
        if self.client is not None:
            self.client.set_effective_config(self.effective)

    def _put_in_force(self, s: settings_pb2.Settings) -> None:
        """Carry out the live settings, here and in every process forked from here."""
        self.controls.apply(s)
        self._applied = s
        if self._relay is not None and self._relay_owner == os.getpid():
            try:
                save_state(str(self._relay), s.SerializeToString())
            except OSError as e:
                _log.warning("OpAMP: forked processes keep their settings: can't hand them on: %s", e)

    def _before_fork(self) -> None:
        """About to fork, and talking to the server: make the relay the child will follow, once.
        A forked child is a worker of this process (gunicorn's, Celery's, uWSGI's): it isn't an
        instance of its own, and the server's live settings reach it through this file."""
        if self.client is None or self._relay is not None:
            return
        # Under the lock, so a remote config decided meanwhile is either in the file or after it.
        with self._lock:
            try:
                relay = Path(tempfile.mkdtemp(prefix="opamp-otel-")) / "live-settings.pb"
                save_state(str(relay), self._applied.SerializeToString())
            except OSError as e:
                _log.warning("OpAMP: forked processes will keep the settings they fork with: %s", e)
                return
            self._relay, self._relay_owner = relay, os.getpid()

    def _forked(self) -> None:
        """In a child forked from this process (gunicorn's workers, multiprocessing's fork): the
        client, its thread (gone in the child) and its connection are the parent's, as is the
        instance UID. The child drops its copy without a word to the server; the parent stays the
        instance, and the child follows the live settings the parent puts in force (the relay)."""
        # Kept, not dropped: nothing of the parent's is finalized (and closed) in the child.
        self._inherited = (self.client, self._loop)
        self.client = None
        self._loop = None
        self._lock = threading.RLock()
        self._wake = threading.Condition(self._lock)
        self._ticking = False  # the parent's timer: its relay hands on what it puts in force
        if self._relay is not None:
            threading.Thread(target=self._follow, args=(self._relay,), name="opamp-relay", daemon=True).start()

    def _follow(self, relay: Path, every: float = 1.0) -> None:
        """In a forked child: put in force the live settings the parent hands on, until it's gone."""
        seen = None
        while True:
            try:
                st = os.stat(relay)
                if (st.st_ino, st.st_mtime_ns, st.st_size) != seen:
                    seen = (st.st_ino, st.st_mtime_ns, st.st_size)
                    self.controls.apply(settings_pb2.Settings.FromString(relay.read_bytes()))
            except FileNotFoundError:
                return  # the parent stopped: the settings in force now stay
            except Exception:  # noqa: BLE001 - a torn read is read again next time
                _log.debug("OpAMP: reading the parent's settings", exc_info=True)
                seen = None
            time.sleep(every)

    def _notified(self, n: Notification) -> None:
        """On the client's loop: keep it quick."""
        if self.reachability is not None:
            self.reachability.notified(n)
        if n.kind == "remote_config" and self.client is not None:
            answer = self.remote_config(n.remote_config)
            self.client.set_effective_config(self.effective)
            self.client.set_remote_config_status(answer)
        elif n.kind == "instance_uid_changed":
            self.renamed(uuid.UUID(bytes=n.instance_uid))
        elif n.kind == "state_from_newer_version":
            _log.warning("OpAMP: the saved state was written by a newer version of this SDK (state format %d), which "
                         "this one can't read: it is kept as it is, and this run saves nothing", n.state_version)
        elif n.kind == "stopped" and n.error is not None:
            _log.error("OpAMP: the client stopped; the SDK runs unmanaged: %s", n.error)

    def renamed(self, uid: uuid.UUID) -> None:
        """The server named the instance anew: saved, so the next start uses it for both the
        OpAMP instance UID and service.instance.id. The running SDK's resource can't change."""
        self.uid = uid
        if self.setup is not None and self.setup.state_dir is not None:
            try:
                save_state(str(self.setup.state_dir / setup.UID_FILE), str(uid).encode())
            except OSError as e:
                _log.warning("OpAMP: can't save the instance UID the server assigned: %s", e)
        _log.info("OpAMP: the server named this instance %s; service.instance.id changes at the next start", uid)

    def remote_config(self, config: opamp_pb2.AgentRemoteConfig) -> opamp_pb2.RemoteConfigStatus:
        """Carry out the layer's decision on a remote config; the answer for the server."""
        assert self.layer is not None and self.setup is not None
        with self._lock:
            now = self.wall_ms()
            # With a state directory the client keeps the config this applies, in its own state,
            # and tells the server APPLIED once that is saved: nothing for this package to save.
            # Without one, nothing is kept, and a next-start change is refused.
            save = None if self.setup.state_dir is None else lambda doc: None
            d = self.layer.decide(config, self.current, self.loaded.current, self.host, now, save)
            if d.HasField("apply"):
                self._put_in_force(d.apply)
            if d.pending_restart:
                _log.info("OpAMP: %s changed; in force when the application restarts", ", ".join(d.pending_restart))
            self.current = d.current
            self.effective = d.effective
            if not d.HasField("apply") and self._refresh_at and now >= self._refresh_at:
                # FAILED, so nothing changes, but what is reported is now's: once the wall clock
                # has passed the end of debug mode (a machine that slept, before the monotonic
                # timer went off), it ends here, as the report says.
                self._refresh(now)
            else:
                self._arm(d.refresh_at_unix_ms)
            return d.status


def attributes(resource: Resource) -> list[anyvalue_pb2.KeyValue]:
    """The resource's attributes, typed as OpAMP's KeyValue."""
    out = []
    for key, value in resource.attributes.items():
        if isinstance(value, bool):
            v = anyvalue_pb2.AnyValue(bool_value=value)
        elif isinstance(value, int):
            v = anyvalue_pb2.AnyValue(int_value=value)
        elif isinstance(value, float):
            v = anyvalue_pb2.AnyValue(double_value=value)
        else:
            v = anyvalue_pb2.AnyValue(string_value=str(value))
        out.append(anyvalue_pb2.KeyValue(key=key, value=v))
    return out


def _loop_thread(name: str) -> asyncio.AbstractEventLoop:
    """An event loop running on a thread of its own. Under gevent's monkey patching (Locust,
    gunicorn's gevent workers) a threading.Thread is a greenlet on the caller's thread, where asyncio
    runs one loop at a time: the client's would be the only one, and an asyncio loop the
    application runs in a greenlet (locust-plugins' Playwright users) would fail to start. There
    the loop gets a thread of the operating system's, and is made on it, so that its selector is
    that thread's gevent hub's (_GeventLoop)."""
    monkey = sys.modules.get("gevent.monkey")
    if not _patched(monkey):
        loop = asyncio.new_event_loop()
        threading.Thread(target=loop.run_forever, name=name, daemon=True).start()
        return loop
    made: list[asyncio.AbstractEventLoop] = []
    ready = monkey.get_original("_thread", "allocate_lock")()
    ready.acquire()

    def run() -> None:
        loop = _GeventLoop()
        made.append(loop)
        loop.call_soon(ready.release)
        loop.run_forever()

    monkey.get_original("_thread", "start_new_thread")(run, ())
    ready.acquire()
    return made[0]


class _GeventLoop(asyncio.SelectorEventLoop):
    """The client's loop under gevent, on a thread of the operating system's. Its selector waits in
    the thread's gevent hub, which runs that thread's greenlets meanwhile: the threads the client
    starts (the loop's executor, state saves) are greenlets there. One that finishes a future
    calls the loop on the loop's own thread, so asyncio doesn't wake the selector, and the loop
    slept through it, for the rest of its wait (stop: until the core gave up on the goodbye).
    Here anything scheduled during a wait wakes it."""

    def __init__(self) -> None:
        selector = selectors.DefaultSelector()  # gevent's, monkey patched
        wait = selector.select

        def select(timeout=None):
            self._selecting = True
            try:
                return wait(timeout)
            finally:
                self._selecting = False

        selector.select = select
        self._selecting = False
        super().__init__(selector)

    def call_soon(self, callback, *args, context=None):
        handle = super().call_soon(callback, *args, context=context)
        if self._selecting:
            self._write_to_self()
        return handle

    def call_at(self, when, callback, *args, context=None):
        handle = super().call_at(when, callback, *args, context=context)
        if self._selecting:
            self._write_to_self()
        return handle


def _patched(monkey) -> bool:
    return monkey is not None and monkey.is_module_patched("threading")


def _result(future: concurrent.futures.Future, timeout: float | None = None):
    """What the client's loop made of `future`, waited for from another thread. Under gevent the
    loop is on a thread of the operating system's (_loop_thread), which a gevent wait on this one
    can't hear from, and blocking this thread outright would stop its greenlets (the
    application's): this yields to them while it waits."""
    monkey = sys.modules.get("gevent.monkey")
    if not _patched(monkey):
        return future.result(timeout)
    import gevent

    deadline = None if timeout is None else time.monotonic() + timeout
    while not future.done() and (deadline is None or time.monotonic() < deadline):
        gevent.sleep(0.005)
    return future.result(0)


def _suppress() -> None:
    """Suppress instrumentation in the current context, as OpenTelemetry's own exporters do."""
    from opentelemetry.context import _SUPPRESS_INSTRUMENTATION_KEY, attach, set_value  # noqa: PLC0415

    attach(set_value(_SUPPRESS_INSTRUMENTATION_KEY, True))


def _at_exit(stop) -> None:
    """Run `stop` as the interpreter exits, before the SDK's own flush and while host names still
    resolve: with threading's exit hooks, which run before atexit's, last registered first. By
    atexit the thread pool that resolves host names has shut down, and a goodbye to a server named
    by host would be lost. That pool's hook is registered at import (above), so ours runs first."""
    register = getattr(threading, "_register_atexit", None)
    try:
        if register is not None:
            register(stop)
            return
    except RuntimeError:  # the interpreter is already exiting
        pass
    atexit.register(stop)


#: How often the timer asks the layer about export health, in seconds.
HEALTH_INTERVAL = 30.0

AGENT = Agent(app=app_settings.SETTINGS)


class Distro(BaseDistro):
    """The upstream distro's defaults (OTLP for every signal), and the saved settings put in force
    before the instrumentations load."""

    def _configure(self, **kwargs) -> None:
        _otlp_defaults()
        AGENT.prepare()


def _otlp_defaults() -> None:
    os.environ.setdefault("OTEL_TRACES_EXPORTER", "otlp")
    os.environ.setdefault("OTEL_METRICS_EXPORTER", "otlp")
    os.environ.setdefault("OTEL_LOGS_EXPORTER", "otlp")
    os.environ.setdefault("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")


class Configurator(_OTelSDKConfigurator):
    """The SDK, with a sampler and exporters the server's settings change while it runs."""

    def _configure(self, **kwargs) -> None:
        if AGENT.on:
            from opentelemetry.sdk._configuration import _get_sampler, _import_sampler  # noqa: PLC0415

            sampler = kwargs.get("sampler")
            kwargs["sampler"] = AGENT.controls.sampler.wrap(sampler if sampler is not None
                                                            else _import_sampler(_get_sampler()))
            spans = kwargs.get("export_span_processor") or BatchSpanProcessor
            kwargs["export_span_processor"] = lambda exporter: spans(AGENT.controls.spans(exporter))
            logs = kwargs.get("export_log_record_processor") or BatchLogRecordProcessor
            kwargs["export_log_record_processor"] = lambda exporter: logs(AGENT.controls.logs(exporter))
            kwargs["resource_attributes"] = {**(kwargs.get("resource_attributes") or {}), _INSTANCE_ID: str(AGENT.uid)}
            # Metric exporters named in code go through the gate as the variable's do; processors
            # passed in export past the gates, which the host then declares (sdk_host).
            if kwargs.get("metric_exporter_names"):
                kwargs["metric_exporter_names"] = gated_metric_exporters(list(kwargs["metric_exporter_names"]))
            AGENT.extra_metric_exporters = list(kwargs.get("metric_exporter_names") or [])
            AGENT.span_processors = bool(kwargs.get("span_processors"))
            AGENT.log_processors = bool(kwargs.get("log_record_processors"))
            # With a declarative configuration file (newer SDKs) the SDK ignores all of the above.
            AGENT.configured = not os.environ.get("OTEL_CONFIG_FILE")
        super()._configure(**kwargs)
        if AGENT.on and AGENT.configured:
            _watch_processors()
        if AGENT.on and not AGENT.configured:
            # The file's SDK skips upstream's OpAMP hook (start): join the fleet from here, with
            # what the file built, so the instance is still managed (its next-start settings, its
            # identity) and says which settings it can't carry out.
            from opentelemetry import trace  # noqa: PLC0415

            AGENT.start(getattr(trace.get_tracer_provider(), "resource", None) or Resource.create({}))


def _watch_processors() -> None:
    """Once the configurator has built the SDK: a span or log record processor the application
    adds to its providers from then on is told to the agent (Agent.processor_added), then added."""
    from opentelemetry import _logs, trace  # noqa: PLC0415
    from opentelemetry.sdk._logs import LoggerProvider  # noqa: PLC0415
    from opentelemetry.sdk.trace import TracerProvider  # noqa: PLC0415

    def watched(provider, method: str, signal: str) -> None:
        add = getattr(provider, method)

        def added(*args, **kwargs):
            AGENT.processor_added(signal)
            return add(*args, **kwargs)

        setattr(provider, method, added)

    tracer_provider = trace.get_tracer_provider()
    if isinstance(tracer_provider, TracerProvider):
        watched(tracer_provider, "add_span_processor", "traces")
    logger_provider = _logs.get_logger_provider()
    if isinstance(logger_provider, LoggerProvider):
        watched(logger_provider, "add_log_record_processor", "logs")


class GatedOtlpMetricExporter(MetricExporter):
    """The OTLP metric exporter the protocol variables choose (as ``otlp`` would), or nothing
    while the server says not to send metrics."""

    def __init__(self, **kwargs) -> None:
        from opentelemetry.sdk._configuration import _get_exporter_entry_point  # noqa: PLC0415
        from opentelemetry.util._importlib_metadata import entry_points  # noqa: PLC0415

        name = _get_exporter_entry_point("otlp", "metrics")
        (ep,) = entry_points(group="opentelemetry_metrics_exporter", name=name)
        self._to: MetricExporter = ep.load()(**kwargs)
        super().__init__(preferred_temporality=self._to._preferred_temporality,  # noqa: SLF001
                         preferred_aggregation=self._to._preferred_aggregation)  # noqa: SLF001

    def export(self, metrics_data, timeout_millis: float = 10_000, **kwargs) -> MetricExportResult:
        counts, points = AGENT.controls.metrics, _points(metrics_data)
        if not AGENT.controls.gates.send_metrics:
            counts.filtered(points)
            return MetricExportResult.SUCCESS
        return _export(counts, points, lambda: self._to.export(metrics_data, timeout_millis=timeout_millis, **kwargs))

    def force_flush(self, timeout_millis: float = 10_000) -> bool:
        return self._to.force_flush(timeout_millis=timeout_millis)

    def shutdown(self, timeout_millis: float = 30_000, **kwargs) -> None:
        self._to.shutdown(timeout_millis=timeout_millis, **kwargs)


def _points(metrics_data) -> int:
    """The data points in an export, which health counts metrics by."""
    try:
        return sum(len(m.data.data_points) for rm in metrics_data.resource_metrics for sm in rm.scope_metrics
                   for m in sm.metrics)
    except (AttributeError, TypeError):
        return 0


def start(resource: Resource) -> None:
    """Upstream's post_sdk_init_function OpAMP hook: the SDK is built; join the fleet."""
    AGENT.start(resource)


def configure(*, endpoint: str | None = None, headers: Mapping[str, str] | None = None,
              state_dir: str | os.PathLike | None = None, heartbeat_interval: timedelta | float | str | None = None,
              **kwargs) -> None:
    """Set the SDK up in code as ``opentelemetry-instrument`` would, under the OpAMP server: the
    tracer, meter and logger providers the OTEL_* variables describe (OTLP by default), set as the
    global ones, with the live sampler and gated exporters, and the client started. Without an
    endpoint it is upstream's distro and configurator, unmanaged.

    The server is ``endpoint``, ``headers`` (in place of OTEL_OPAMP_HEADERS, not merged with it),
    ``state_dir`` ("none": none, not even the default one) and ``heartbeat_interval`` (seconds, a
    timedelta, or "30s" as the variable) where given and not blank, else the OTEL_OPAMP_*
    variables. A value it can't use is warned about and is the default; it never raises. Call it
    once, where the application would build its providers, and before its instrumentors. The other
    keyword arguments are upstream's configurator's (``sampler``, ``resource_attributes``,
    ``span_processors``, ...). It instruments nothing, so the instrumentation switches the server
    sends aren't carried out.

    For example (examples/python-otel/in_code.py)::

        pydantic_opamp_otel.configure(
            endpoint="https://opamp.example.com/v1/opamp",
            headers={"Authorization": f"Bearer {settings.opamp_key}"},
            state_dir="/var/lib/checkout/opamp",
            heartbeat_interval=timedelta(seconds=30),
        )
        URLLibInstrumentor().instrument()
    """
    _otlp_defaults()
    AGENT.prepare(auto_instrumented=False, endpoint=endpoint, headers=headers, state_dir=state_dir,
                  heartbeat_interval=heartbeat_interval)
    # The SDK's configurator starts the client through the package's OpAMP hook (start).
    Configurator().configure(**kwargs)


from ._logfire import configure_logfire  # noqa: E402 - it reads the package's AGENT
