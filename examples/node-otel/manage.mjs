// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// app.mjs's requests from an application that sets its OpenTelemetry SDK up itself (its own
// NodeSDK, its exporter and the http instrumentation), and then puts it under the OpAMP server
// with manage(), the server from OTEL_OPAMP_* (docs/node-otel.md, "Already using OpenTelemetry or
// Logfire?"):
//
//   OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp OTEL_OPAMP_HEADERS="Authorization=Bearer <key>" \
//     node manage.mjs http://localhost:8080/ping
//
// Plain `node`, no --import. The exporter sends to OTEL_EXPORTER_OTLP_ENDPOINT, as any NodeSDK's.

import { createRequire } from "node:module";

import { manage } from "@pydantic/logfire-fleet-otel";

// The SDK's packages as an application has them; here the package's own (NODE_PATH, which only
// require reads), so both share one OpenTelemetry API.
const require = createRequire(import.meta.url);
const { OTLPTraceExporter } = require("@opentelemetry/exporter-trace-otlp-proto");
const { HttpInstrumentation } = require("@opentelemetry/instrumentation-http");
const { resourceFromAttributes } = require("@opentelemetry/resources");
const { NodeSDK } = require("@opentelemetry/sdk-node");
const { BatchSpanProcessor } = require("@opentelemetry/sdk-trace-base");

const sdk = new NodeSDK({
  resource: resourceFromAttributes({ "service.name": "checkout" }),
  autoDetectResources: false,
  spanProcessors: [new BatchSpanProcessor(new OTLPTraceExporter(), { scheduledDelayMillis: 500 })],
  instrumentations: [new HttpInstrumentation()],
});
sdk.start();
const { shutdown } = manage();

// Loaded after the SDK started, so the http instrumentation patches it.
const http = require("node:http");

const timer = setInterval(() => {
  http.get(process.argv[2], (res) => res.resume()).on("error", () => {});
}, 200);

// The goodbye to the server, then the application's own SDK flushed.
process.on("SIGTERM", () => {
  clearInterval(timer);
  void shutdown().then(() => sdk.shutdown());
});
