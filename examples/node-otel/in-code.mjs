// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// app.mjs with the OpAMP server given in code, from the application's own settings (here its own
// variables; a config file is as good), not OTEL_OPAMP_*:
//
//   APP_OPAMP_URL=https://opamp.example.com/v1/opamp APP_OPAMP_TOKEN=<enrollment key> \
//     APP_STATE_DIR=/var/lib/checkout node in-code.mjs http://localhost:8080/ping
//
// Plain `node`, no --import: register() sets everything up here, so the shutdown is ours.

import { createRequire } from "node:module";

import { register } from "@pydantic/opamp-otel";

const { shutdown } = register({
  endpoint: process.env.APP_OPAMP_URL,
  headers: { Authorization: `Bearer ${process.env.APP_OPAMP_TOKEN}` },
  stateDir: process.env.APP_STATE_DIR,
  heartbeatIntervalMs: 1000,
});

// Loaded after register(), so the http instrumentation patches it. A static `import` would load it
// first (imports run before the module's code); for those, run the register through
// `node --import` (docs/node-otel.md).
const http = createRequire(import.meta.url)("node:http");

const timer = setInterval(() => {
  http.get(process.argv[2], (res) => res.resume()).on("error", () => {});
}, 200);

// The goodbye to the server, then the last telemetry flushed.
process.on("SIGTERM", () => {
  clearInterval(timer);
  void shutdown();
});
