# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The setups check_truth runs app.py under (examples/integrations.py): what an application does
to the SDK that pydantic-opamp-otel built, named in EXAMPLE_SETUP.

- ``late-processor``: once the SDK is built, the application adds a span processor and a log
  record processor of its own, each exporting over OTLP to the same place as the SDK's. Their
  spans and log records go past the package's gates."""

from __future__ import annotations


def late_processor() -> None:
    from opentelemetry import _logs, trace
    from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    trace.get_tracer_provider().add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    _logs.get_logger_provider().add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter()))


SETUPS = {"late-processor": late_processor}


def apply(names: str) -> None:
    """Carry out each setup `names` names (comma-separated)."""
    for name in filter(None, (n.strip() for n in names.split(","))):
        SETUPS[name]()
