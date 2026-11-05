# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The setups check_truth runs app.py under (examples/integrations.py): what an application does
to the SDK that logfire-fleet-otel built, named in EXAMPLE_SETUP.

- ``late-processor``: once the SDK is built, the application adds a span processor and a log
  record processor, each exporting over OTLP to the same place as the SDK's; the package puts its
  gates in front of them too. Beside them, a span processor of the application's own kind that
  exports each span itself, to a second sink (EXAMPLE_SECOND_SINK): the gates can't reach into it,
  so the package reports its exporter ungoverned."""

from __future__ import annotations


def late_processor() -> None:
    import os

    from opentelemetry import _logs, trace
    from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
    from opentelemetry.sdk.trace import SpanProcessor
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    class ExportsItself(SpanProcessor):
        """Exports each span as it ends, with an exporter of its own."""

        def __init__(self, exporter: OTLPSpanExporter) -> None:
            self.exporter = exporter

        def on_end(self, span) -> None:
            self.exporter.export([span])

    tracer_provider = trace.get_tracer_provider()
    tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    second = OTLPSpanExporter(endpoint=f"{os.environ['EXAMPLE_SECOND_SINK']}/v1/traces")
    tracer_provider.add_span_processor(ExportsItself(second))
    _logs.get_logger_provider().add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter()))


SETUPS = {"late-processor": late_processor}


def apply(names: str) -> None:
    """Carry out each setup `names` names (comma-separated)."""
    for name in filter(None, (n.strip() for n in names.split(","))):
        SETUPS[name]()
