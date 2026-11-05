// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// A shop that takes an order a second, for trying Logfire Fleet (see ../README.md).
//
// Each order is a span with an INFO log record inside it; every fifth order is slow and logs a
// WARN as well. Set the policy's minimum log level to Warning in Fleet and, in Live view, the
// INFO records stop while the warnings and spans keep coming.
//
// Run it under the integration, which builds the SDK and connects it to Fleet:
//   node --import @pydantic/logfire-fleet-otel/register app.mjs

import { logs, SeverityNumber } from "@opentelemetry/api-logs";
import { trace } from "@opentelemetry/api";

const tracer = trace.getTracer("fleet-quickstart");
const log = logs.getLogger("fleet-quickstart");

let order = 0;
setInterval(() => {
  order += 1;
  tracer.startActiveSpan(`checkout order ${order}`, (span) => {
    log.emit({ severityNumber: SeverityNumber.INFO, severityText: "INFO", body: `order ${order} paid` });
    if (order % 5 === 0) {
      log.emit({ severityNumber: SeverityNumber.WARN, severityText: "WARN", body: `order ${order} took longer than 2 s` });
    }
    span.end();
  });
  console.log(`order ${order} sent`);
}, 1000);
