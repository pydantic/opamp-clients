// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// app.cjs as an ES module: `import` loads http without require, so its instrumentation needs the
// loader hook the register installs. (The logs API and the package come through require:
// NODE_PATH, where this example finds them, doesn't reach `import`. An application with the
// package installed writes `import { appSettings } from "@pydantic/opamp-otel"`.)
//
// As app.cjs, it reads the feature flag `example.feature` the server sends (docs/app-settings.md):
// /feature each round while it is `on`, /changed once per change it is told of.

import http from "node:http";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { logs } = require("@opentelemetry/api-logs");
// The whole package this time: the same module the register loaded, so the same appSettings.
const { appSettings } = require("@pydantic/opamp-otel");
const log = logs.getLogger("app");

const get = (url) => http.get(url, (res) => res.resume()).on("error", () => {});
const feature = new URL("/feature", process.argv[2]).href;
const changedUrl = new URL("/changed", process.argv[2]).href;

// Told when the server's change is put in force: the work is done in the loop below.
const changes = [];
const unsubscribe = appSettings.onChange((settings, changed) => changes.push(changed));

const timer = setInterval(() => {
  get(process.argv[2]);
  log.emit({ severityText: "INFO", severityNumber: 9, body: `requested ${process.argv[2]}` });
  if (appSettings.get("example.feature", "off") === "on") get(feature);
  while (changes.length > 0) {
    log.emit({ severityText: "INFO", severityNumber: 9, body: `settings changed: ${changes.shift().join(", ")}` });
    get(changedUrl);
  }
}, 200);

process.on("SIGTERM", () => {
  clearInterval(timer);
  unsubscribe();
});
