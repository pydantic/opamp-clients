// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// app.mjs's requests from an application on the Logfire JS SDK: logfire.configure() as it is (its
// own NodeSDK, auto-instrumentation and exporters, sending where LOGFIRE_TOKEN and
// LOGFIRE_BASE_URL say), and then manage(), the server from OTEL_OPAMP_* (docs/node-otel.md,
// "Already using OpenTelemetry or Logfire?"):
//
//   OTEL_OPAMP_ENDPOINT=https://opamp.example.com/v1/opamp OTEL_OPAMP_HEADERS="Authorization=Bearer <key>" \
//     LOGFIRE_TOKEN=<write token> node logfire-js.mjs http://localhost:8080/ping
//
// Its dependency is in package.json here (npm ci), the Logfire JS SDK at the version checked.

import { createRequire } from "node:module";

import * as logfire from "@pydantic/logfire-node";
import { manage } from "@pydantic/logfire-fleet-otel";

logfire.configure({ serviceName: "checkout", console: false, metrics: false });
const { shutdown } = manage();

// Loaded after the SDK started, so the http instrumentation patches it.
const http = createRequire(import.meta.url)("node:http");

const timer = setInterval(() => {
  http.get(process.argv[2], (res) => res.resume()).on("error", () => {});
}, 200);

// The goodbye to the server, then the Logfire SDK's own shutdown (its SIGTERM handler).
process.on("SIGTERM", () => {
  clearInterval(timer);
  void shutdown();
});
