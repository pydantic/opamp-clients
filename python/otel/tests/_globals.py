# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The global providers as the tests need them, without setting them for the whole process (the
SDK lets them be set once)."""

from __future__ import annotations

from opentelemetry import _logs, metrics, trace
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.trace import TracerProvider


def sdk_globals(monkeypatch) -> tuple[TracerProvider, MeterProvider, LoggerProvider]:
    """The global providers as the OpenTelemetry SDK's, whoever built them: what `govern` finds."""
    providers = TracerProvider(), MeterProvider(), LoggerProvider()
    monkeypatch.setattr(trace, "get_tracer_provider", lambda: providers[0])
    monkeypatch.setattr(metrics, "get_meter_provider", lambda: providers[1])
    monkeypatch.setattr(_logs, "get_logger_provider", lambda: providers[2])
    return providers
