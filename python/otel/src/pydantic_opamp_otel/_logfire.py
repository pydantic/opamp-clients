# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The Logfire SDK under an OpAMP server: ``configure_logfire`` in place of ``logfire.configure``.

The Logfire SDK builds its own providers, so the package's configurator can't build them. Instead
the live sampler goes in as Logfire's head sampler, and the exporters to Logfire are built here,
the way the Logfire SDK builds them, with the package's gates between the SDK and the wire. The
Logfire SDK then sends nothing itself (``send_to_logfire=False``). Everything else is
``logfire.configure``'s own: its scrubbing, its console, its instrumentations.

What can't be gated (console output, a processor or metric reader given here, the OTLP exporters
the Logfire SDK adds for ``OTEL_EXPORTER_OTLP_*``, or an export the Logfire SDK makes from its
credentials file) is declared, so a policy that overrides a setting it would bypass is refused
rather than answered as done (docs/python-otel.md, "The Logfire SDK").
"""

from __future__ import annotations

import dataclasses
import os
from collections.abc import Mapping
from datetime import timedelta
from typing import TYPE_CHECKING, Any

from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.metrics.export import MetricExporter, PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource

if TYPE_CHECKING:
    import logfire

#: The name the inventory gives the exporters built here.
LOGFIRE_EXPORTER = "logfire"
#: A metric exporter name that isn't gated: what sdk_host is told for a metric reader it can't stop.
_UNGATED = "ungated"


def _config(kwargs: Mapping[str, Any]) -> Any:
    """The Logfire SDK's configuration for these arguments, merged with its variables and config
    file by its own loader, so what is built here goes where it would send: nothing is set up."""
    import copy  # noqa: PLC0415
    import inspect  # noqa: PLC0415
    from pathlib import Path  # noqa: PLC0415

    from logfire._internal.config import LogfireConfig  # noqa: PLC0415

    defaults = {name: p.default for name, p in inspect.signature(LogfireConfig.__init__).parameters.items()
                if p.default is not inspect.Parameter.empty}
    names = inspect.signature(LogfireConfig._load_configuration).parameters  # noqa: SLF001
    args = {name: kwargs.get(name, defaults.get(name)) for name in names if name != "self"}
    for name in ("config_dir", "data_dir"):
        args[name] = Path(args[name]) if args[name] else None
    # The loader fills the advanced options in place: a copy, so the caller's stay as given.
    if args.get("advanced") is not None and not isinstance(args["advanced"], Mapping):
        args["advanced"] = copy.copy(args["advanced"])
    config = object.__new__(LogfireConfig)
    config._load_configuration(**args)  # noqa: SLF001
    return config


def _resolved(config) -> tuple[list[str], bool, str | None]:
    """The tokens the Logfire SDK sends with, whether it would send anyway without one (it
    creates a project when send_to_logfire is True), and its base URL. As the Logfire SDK does,
    a token or base URL it isn't given comes from its credentials file."""
    from logfire._internal.config import LogfireCredentials  # noqa: PLC0415

    send, token, base_url = config.send_to_logfire, config.token, config.advanced.base_url
    if not send:
        return [], False, base_url
    try:
        credentials = LogfireCredentials.load_creds_file(config.data_dir)
    except Exception:  # noqa: BLE001 - the Logfire SDK raises this itself when it needs the file
        credentials = None
    if credentials is not None:
        token = token or credentials.token
        base_url = base_url or credentials.logfire_api_url
    tokens = [] if not token else [token] if isinstance(token, str) else list(token)
    return tokens, not tokens and send is True, base_url


def _otlp_variables(signal: str) -> bool:
    """The Logfire SDK adds an OTLP exporter of its own for the signal (ungated) when one of
    these is set and the signal's OTEL_*_EXPORTER is otlp or unset."""
    if not (os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT") or os.environ.get(f"OTEL_EXPORTER_OTLP_{signal.upper()}_ENDPOINT")):
        return False
    return os.environ.get(f"OTEL_{signal.upper()}_EXPORTER", "").lower() in ("otlp", "")


def _logfire_exporters(token: str, base_url: str | None, controls, advanced, metrics: bool = True):
    """For one token: the span processor, the metric reader (its exporter gated; None without
    metrics, as an unregistered reader would still run) and the log record processor, built as the Logfire SDK builds its own, each with the package's gate."""
    from urllib.parse import urljoin  # noqa: PLC0415

    from logfire._internal.config import METRICS_PREFERRED_TEMPORALITY  # noqa: PLC0415
    from logfire._internal.exporters.dynamic_batch import DynamicBatchSpanProcessor  # noqa: PLC0415
    from logfire._internal.exporters.otlp import (  # noqa: PLC0415
        BodySizeCheckingOTLPSpanExporter, OTLPExporterHttpSession, QuietLogExporter, QuietSpanExporter,
        RetryFewerSpansSpanExporter)
    from logfire._internal.exporters.quiet_metrics import QuietMetricExporter  # noqa: PLC0415
    from logfire._internal.exporters.remove_pending import RemovePendingSpansExporter  # noqa: PLC0415
    from logfire._internal.config import get_base_url_from_token, install_logfire_response_hook  # noqa: PLC0415
    from logfire.version import VERSION  # noqa: PLC0415
    from opentelemetry.exporter.otlp.proto.http import Compression  # noqa: PLC0415
    from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter  # noqa: PLC0415
    from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter  # noqa: PLC0415

    url = base_url or get_base_url_from_token(token)
    headers = {"User-Agent": f"logfire/{VERSION}", "Authorization": token}
    # zstd as the Logfire SDK sends it: the session compresses, where it can (else gzip, as it warns).
    from logfire._internal.exporters import otlp  # noqa: PLC0415

    # logfire before 5.2 has no zstd.
    zstd = getattr(advanced, "compression", None) == "zstd" and getattr(otlp, "zstd_compress", None) is not None
    session = OTLPExporterHttpSession(_use_zstd=True) if zstd else OTLPExporterHttpSession()
    session.headers.update(headers)
    # Ingest's responses, as the Logfire SDK reads them: by default its X-Logfire-Warning header.
    install_logfire_response_hook(session, getattr(advanced, "server_response_hook", None))
    gzip = Compression.NoCompression if zstd else Compression.Gzip
    # The gate sits under the retry and pending-span handling, so what it drops is never retried
    # and what it lets through is redacted before it is sent, a pending span included.
    spans = BodySizeCheckingOTLPSpanExporter(endpoint=urljoin(url, "/v1/traces"), session=session,
                                             compression=gzip, headers=headers)
    spans = RetryFewerSpansSpanExporter(controls.spans(QuietSpanExporter(spans)))
    # Before logfire 5.2 pending spans were always sent, and there was no setting for them.
    if getattr(advanced, "pending_spans", True):
        spans = RemovePendingSpansExporter(spans)
    reader = None
    if metrics:
        reader = PeriodicExportingMetricReader(GatedMetricExporter(QuietMetricExporter(
            OTLPMetricExporter(endpoint=urljoin(url, "/v1/metrics"), headers=headers, session=session,
                               compression=gzip, preferred_temporality=METRICS_PREFERRED_TEMPORALITY),
            preferred_temporality=METRICS_PREFERRED_TEMPORALITY)))
    logs = QuietLogExporter(OTLPLogExporter(endpoint=urljoin(url, "/v1/logs"), session=session, headers=headers,
                                            compression=gzip))
    return (DynamicBatchSpanProcessor(spans), reader, BatchLogRecordProcessor(controls.logs(logs)))


class GatedMetricExporter(MetricExporter):
    """A metric exporter, or nothing while the server says not to send metrics."""

    def __init__(self, to: MetricExporter) -> None:
        self._to = to
        super().__init__(preferred_temporality=to._preferred_temporality,  # noqa: SLF001
                         preferred_aggregation=to._preferred_aggregation)  # noqa: SLF001

    def export(self, metrics_data, timeout_millis: float = 10_000, **kwargs):
        from . import AGENT, MetricExportResult, _export, _points  # noqa: PLC0415

        counts, points = AGENT.controls.metrics, _points(metrics_data)
        if not AGENT.controls.gates.send_metrics:
            counts.filtered(points)
            return MetricExportResult.SUCCESS
        return _export(counts, points, lambda: self._to.export(metrics_data, timeout_millis=timeout_millis, **kwargs))

    def force_flush(self, timeout_millis: float = 10_000) -> bool:
        return self._to.force_flush(timeout_millis=timeout_millis)

    def shutdown(self, timeout_millis: float = 30_000, **kwargs) -> None:
        self._to.shutdown(timeout_millis=timeout_millis, **kwargs)


def _head(sampling) -> Any:
    """The sampler the Logfire SDK would build from its head option: None for every trace."""
    from opentelemetry.sdk.trace.sampling import ParentBasedTraceIdRatio  # noqa: PLC0415

    head = sampling.head
    if isinstance(head, (int, float)):
        return ParentBasedTraceIdRatio(head) if head < 1 else None
    return head


def configure_logfire(*, endpoint: str | None = None, headers: Mapping[str, str] | None = None,
                      state_dir: str | os.PathLike | None = None,
                      heartbeat_interval: timedelta | float | str | None = None, **kwargs) -> logfire.Logfire:
    """``logfire.configure`` under the OpAMP server: call it in its place, with the same keyword
    arguments. The server is ``endpoint``, ``headers``, ``state_dir`` and ``heartbeat_interval``,
    or the OTEL_OPAMP_* variables, as for ``configure()``. Without an endpoint from either it is
    ``logfire.configure(**kwargs)``, unmanaged. It never raises for the server's settings: one it
    can't use is warned about and is the default."""
    import logfire  # noqa: PLC0415
    from logfire import AdvancedOptions, MetricsOptions  # noqa: PLC0415

    from . import _INSTANCE_ID, AGENT  # noqa: PLC0415

    # A copy: the package's variables for opentelemetry-instrument aren't the Logfire SDK's.
    env = {**os.environ, "OTEL_METRICS_EXPORTER": "otlp"}
    AGENT.prepare(env, auto_instrumented=False, endpoint=endpoint, headers=headers, state_dir=state_dir,
                  heartbeat_interval=heartbeat_interval)
    if not AGENT.on:
        return logfire.configure(**kwargs)
    kwargs = dict(kwargs)
    controls = AGENT.controls
    # As the Logfire SDK resolves them (its variables and config file included), as what is given
    # here replaces what it would have read.
    config = _config(kwargs)
    sampling = config.sampling
    kwargs["sampling"] = dataclasses.replace(sampling, head=controls.sampler.wrap(_head(sampling)))
    kwargs["resource_attributes"] = {**config.resource_attributes, _INSTANCE_ID: str(AGENT.uid)}

    tokens, sends_ungated, base_url = _resolved(config)
    console = config.console is not False
    given_spans = list(kwargs.get("additional_span_processors") or ())
    advanced = kwargs.get("advanced")
    if isinstance(advanced, Mapping):
        advanced = AdvancedOptions(**advanced)
    given_logs = list(getattr(advanced, "log_record_processors", ()) or ())
    metrics = kwargs.get("metrics")
    given_readers = list(getattr(metrics, "additional_readers", ()) or ())
    built = [_logfire_exporters(t, base_url, controls, config.advanced, metrics is not False) for t in tokens]
    if tokens:
        kwargs["send_to_logfire"] = False
        kwargs["additional_span_processors"] = [*given_spans, *(b[0] for b in built)]
        if advanced is None:
            advanced = AdvancedOptions()
        kwargs["advanced"] = dataclasses.replace(advanced, log_record_processors=[*given_logs, *(b[2] for b in built)])
        if metrics is not False:
            metrics = metrics or MetricsOptions()
            kwargs["metrics"] = dataclasses.replace(metrics, additional_readers=[*given_readers, *(b[1] for b in built)])

    # What exports past the gates, so the settings it would bypass aren't declared (sdk_host).
    AGENT.configured = True
    AGENT.logfire = True
    AGENT.span_processors = bool(given_spans) or console or sends_ungated or _otlp_variables("traces")
    AGENT.log_processors = bool(given_logs) or console or sends_ungated or _otlp_variables("logs")
    ungated_metrics = metrics is not False and (bool(given_readers) or sends_ungated or _otlp_variables("metrics"))
    AGENT.extra_metric_exporters = [_UNGATED] if ungated_metrics else []
    AGENT.listed_exporters = [(signal, LOGFIRE_EXPORTER) for _ in built for signal in
                              ("traces", *(("metrics",) if metrics is not False else ()), "logs")]
    instance = logfire.configure(**kwargs)
    AGENT.start(_resource(instance))
    return instance


def _resource(instance) -> Resource:
    """The resource the Logfire SDK built for `instance`: its own tracer provider's, which is the
    global one only when it isn't ``local``."""
    provider = getattr(getattr(instance, "config", None), "_tracer_provider", None)
    if provider is None:
        from opentelemetry import trace  # noqa: PLC0415

        provider = trace.get_tracer_provider()
    provider = getattr(provider, "provider", provider)
    return getattr(provider, "resource", None) or Resource.create({})
