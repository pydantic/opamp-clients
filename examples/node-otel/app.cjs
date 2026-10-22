// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// An application with no OpenTelemetry setup at all: an HTTP request every 200 ms, and a log
// record for each. Run with `node --import @pydantic/opamp-otel/register`, its http
// instrumentation traces (and measures) the requests; the package puts the SDK under the OpAMP
// server. The log records go through the OpenTelemetry logs API, the provider the register
// installs; a logging library's instrumentation (pino, winston, bunyan) would send them the same way.
//
// Its one piece of OpAMP code reads a setting of its own the server sends (docs/app-settings.md):
// a feature flag, `example.feature`. While the server says `on`, each round also requests
// /feature; and each time its settings change it requests /changed, once. Without a server, or
// until the server says, the flag is its default, `off`.

const http = require("node:http");
const { logs } = require("@opentelemetry/api-logs");
// The application's settings alone, without the rest of the package: what the register filled in.
const { appSettings } = require("@pydantic/opamp-otel/app-settings");

// check_truth's setups (setups.cjs), before the application gets its loggers and tracers: none
// otherwise, and nothing to call for each request.
const setup = process.env.EXAMPLE_SETUP ? require("./setups.cjs").apply(process.env.EXAMPLE_SETUP) : () => {};

const log = logs.getLogger("app");

// With EXAMPLE_SETTINGS=1 (try.py's settings runs) it does more, for the settings that match
// telemetry to have something to match: a span of its own around each request, with attributes a
// server may want redacted, a health check beside it, and log records from three loggers.
const demo = process.env.EXAMPLE_SETTINGS === "1";
const { trace } = require("@opentelemetry/api");
const tracer = trace.getTracer("checkout");
const noisy = logs.getLogger("noisy");
const other = logs.getLogger("other");
const health = new URL("/health", process.argv[2]).href;
const feature = new URL("/feature", process.argv[2]).href;
const changedUrl = new URL("/changed", process.argv[2]).href;

// With EXAMPLE_PROBE=1 (integrations.py's run 3) each round also probes redaction: a span of its
// own, `example.probe`, with the URL it requests (url.full) on an event and on a link to the round
// before's probe, and a log record `example.probe` with that URL; each with a note (example.note)
// that says password, for scrubbing to replace. It probes before or after the
// request, at random: under a rate limit of one a second, the first record after the token comes
// takes it, and in a fixed order the probe might never be that one.
const probing = process.env.EXAMPLE_PROBE === "1";
let lastProbe;
// What the probe carries beside its URL (example.note): it says password, so scrubbing's default
// patterns replace it.
const note = "the password is p4ss";
const probe = (url) => {
  const attributes = { "url.full": url, "example.note": note };
  const span = trace.getTracer("app").startSpan("example.probe", {
    links: lastProbe ? [{ context: lastProbe, attributes }] : [],
  });
  span.addEvent("example.probe", attributes);
  span.end();
  lastProbe = span.spanContext();
  log.emit({ severityText: "INFO", severityNumber: 9, body: "example.probe", attributes });
};

const get = (url) => http.get(url, (res) => res.resume()).on("error", () => {});

// Told when the server's change is put in force, on the OpAMP client's turn: keep it quick, and
// do the work in the application's own loop.
const changes = [];
appSettings.onChange((settings, changed) => changes.push(changed));

const timer = setInterval(() => {
  if (appSettings.get("example.feature", "off") === "on") get(feature);
  while (changes.length > 0) {
    log.emit({ severityText: "INFO", severityNumber: 9, body: `settings changed: ${changes.shift().join(", ")}` });
    get(changedUrl);
  }
  if (!demo) {
    const early = Math.random() < 0.5;
    if (probing && early) probe(process.argv[2]);
    get(process.argv[2]);
    setup(process.argv[2]);
    log.emit({ severityText: "INFO", severityNumber: 9, body: `requested ${process.argv[2]}` });
    if (probing && !early) probe(process.argv[2]);
    return;
  }
  const attributes = {
    "user.email": "ann@example.com", "http.request.header.authorization": "Bearer s3cret", "tenant.id": "acme",
  };
  tracer.startActiveSpan("checkout cart", { attributes }, (span) => {
    get(process.argv[2]);
    span.end();
  });
  get(health);
  log.emit({ severityText: "INFO", severityNumber: 9, body: "checked out", attributes });
  noisy.emit({ severityText: "WARN", severityNumber: 13, body: "noise" });
  other.emit({ severityText: "INFO", severityNumber: 9, body: "other" });
}, 200);

// Stop working on SIGTERM; the register's own handler says goodbye and flushes, and the process
// ends once nothing is left running.
process.on("SIGTERM", () => clearInterval(timer));
