# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The Logfire SDK under an OpAMP server: ``configure_logfire`` in place of ``logfire.configure``.

The Logfire SDK builds its own providers, so the package's configurator can't build them. Instead
the live sampler goes in as Logfire's head sampler, and the exporters to Logfire are built here,
the way the Logfire SDK builds them, with the package's gates between the SDK and the wire. The
Logfire SDK then sends nothing itself (``send_to_logfire=False``). Everything else is
``logfire.configure``'s own: its scrubbing, its console, its instrumentations.

Everything else the Logfire SDK exports through (its console, a processor or metric reader given
here, the OTLP exporters it adds for ``OTEL_EXPORTER_OTLP_*``, an export it makes from its
credentials file) gets the gates once it is built (``govern``, at start), so the policy reaches
every destination. What can't be gated is reported to the server as ungoverned, never refused
(docs/python-otel.md, "The Logfire SDK").
"""

from __future__ import annotations

import dataclasses
import os
from collections.abc import Callable, Mapping
from datetime import timedelta
from typing import TYPE_CHECKING, Any

from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource

if TYPE_CHECKING:
    import logfire

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


def _resolved(config) -> tuple[list[str], str | None]:
    """The tokens the Logfire SDK sends with, and its base URL. As the Logfire SDK does, a token or
    base URL it isn't given comes from its credentials file."""
    from logfire._internal.config import LogfireCredentials  # noqa: PLC0415

    send, token, base_url = config.send_to_logfire, config.token, config.advanced.base_url
    if not send:
        return [], base_url
    try:
        credentials = LogfireCredentials.load_creds_file(config.data_dir)
    except Exception:  # noqa: BLE001 - the Logfire SDK raises this itself when it needs the file
        credentials = None
    if credentials is not None:
        token = token or credentials.token
        base_url = base_url or credentials.logfire_api_url
    tokens = [] if not token else [token] if isinstance(token, str) else list(token)
    return tokens, base_url


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
    spans = _gated_spans(controls, RetryFewerSpansSpanExporter, QuietSpanExporter(BodySizeCheckingOTLPSpanExporter(
        endpoint=urljoin(url, "/v1/traces"), session=session, compression=gzip, headers=headers)))
    # Before logfire 5.2 pending spans were always sent, and there was no setting for them.
    if getattr(advanced, "pending_spans", True):
        spans = RemovePendingSpansExporter(spans)
    reader = None
    if metrics:
        from . import GatedMetricExporter  # noqa: PLC0415

        reader = PeriodicExportingMetricReader(GatedMetricExporter(QuietMetricExporter(
            OTLPMetricExporter(endpoint=urljoin(url, "/v1/metrics"), headers=headers, session=session,
                               compression=gzip, preferred_temporality=METRICS_PREFERRED_TEMPORALITY),
            preferred_temporality=METRICS_PREFERRED_TEMPORALITY)))
    logs = QuietLogExporter(OTLPLogExporter(endpoint=urljoin(url, "/v1/logs"), session=session, headers=headers,
                                            compression=gzip))
    return (DynamicBatchSpanProcessor(spans), reader, BatchLogRecordProcessor(controls.logs(logs)))


def _gated_spans(controls, retry: Callable[[Any], Any], sender: Any) -> Any:
    """The span gate around Logfire's retrying exporter (`retry`, over `sender`): the gate decides
    and counts each span once, above the retries, so a retried log span isn't charged to the cap
    again or counted twice; redaction sits under them, so every span sent is redacted, the
    body-too-large span the retry makes included."""
    return controls.spans(retry(controls.redacting(sender)), log_spans=True, redact=False)


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

    tokens, base_url = _resolved(config)
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

    # The sampler is the package's; every exporter the Logfire SDK builds gets its gate at start.
    AGENT.configured = True
    AGENT.logfire = True
    instance = logfire.configure(**kwargs)
    AGENT.logfire_config = getattr(instance, "config", None)
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
