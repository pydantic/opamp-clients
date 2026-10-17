// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The setups check_truth runs app.cjs and app.mjs under (examples/integrations.py): what an
// application does beside the SDK the register built, named in EXAMPLE_SETUP (comma-separated).
// Each builds a provider of its own, exporting over OTLP to the same place as the SDK's, and
// registers it as the global one in place of the register's (the API refuses a second one until
// the first is disabled). What the application then sends through the API goes past the
// package's sampler and gates.
//
// - `own-tracer-provider`: a tracer provider; the application traces each request with it too,
//   in a span that carries the request's URL.
// - `own-logger-provider`: a logger provider; the application's log records go through it.
// - `own-meter-provider`: a meter provider; the application counts its requests with it.
//
// apply() runs before the application gets its tracers, loggers and meters, as such a setup
// would. It returns what the application calls for each request it makes.

const api = require("@opentelemetry/api");
const { logs } = require("@opentelemetry/api-logs");
const { detectResources, envDetector } = require("@opentelemetry/resources");

// The application's own resource: OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES, as the SDK's.
const resource = detectResources({ detectors: [envDetector] });

function ownTracerProvider() {
  const { NodeTracerProvider } = require("@opentelemetry/sdk-trace-node");
  const { BatchSpanProcessor } = require("@opentelemetry/sdk-trace-base");
  const { OTLPTraceExporter } = require("@opentelemetry/exporter-trace-otlp-proto");
  api.trace.disable();
  new NodeTracerProvider({ resource, spanProcessors: [new BatchSpanProcessor(new OTLPTraceExporter())] }).register();
  const tracer = api.trace.getTracer("checkout");
  // Beside the request, not around it: the http instrumentation's span isn't its child.
  return (url) => tracer.startSpan("GET", { kind: api.SpanKind.CLIENT, attributes: { "url.full": url } }).end();
}

function ownLoggerProvider() {
  const { BatchLogRecordProcessor, LoggerProvider } = require("@opentelemetry/sdk-logs");
  const { OTLPLogExporter } = require("@opentelemetry/exporter-logs-otlp-proto");
  logs.disable();
  logs.setGlobalLoggerProvider(new LoggerProvider({
    resource,
    processors: [new BatchLogRecordProcessor({ exporter: new OTLPLogExporter() })],
  }));
}

function ownMeterProvider() {
  const { MeterProvider, PeriodicExportingMetricReader } = require("@opentelemetry/sdk-metrics");
  const { OTLPMetricExporter } = require("@opentelemetry/exporter-metrics-otlp-proto");
  api.metrics.disable();
  api.metrics.setGlobalMeterProvider(new MeterProvider({
    resource,
    readers: [new PeriodicExportingMetricReader({
      exporter: new OTLPMetricExporter(), exportIntervalMillis: Number(process.env.OTEL_METRIC_EXPORT_INTERVAL || 60000),
    })],
  }));
  const requests = api.metrics.getMeter("checkout").createCounter("checkout.requests");
  return () => requests.add(1);
}

const SETUPS = {
  "own-tracer-provider": ownTracerProvider,
  "own-logger-provider": ownLoggerProvider,
  "own-meter-provider": ownMeterProvider,
};

/** Carry out each setup `names` names; what to call for each request the application makes. */
function apply(names) {
  const each = names.split(",").map((n) => n.trim()).filter(Boolean).map((n) => {
    if (!SETUPS[n]) throw new Error(`no setup named ${n}`);
    return SETUPS[n]();
  }).filter(Boolean);
  return (url) => each.forEach((f) => f(url));
}

module.exports = { apply };
