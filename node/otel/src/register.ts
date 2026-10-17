// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// `node --import @pydantic/opamp-otel/register app.js`: upstream's auto-instrumentation register,
// under the OpAMP server named by OTEL_OPAMP_ENDPOINT (docs/node-otel.md).

import { createRequire, register as registerHooks } from "node:module";
import { pathToFileURL } from "node:url";

import { register, say } from "./index.js";

// The module every wrapper the hook writes imports, loaded whole before the hook is on. A
// CommonJS program's require() of an ES module goes through the hook too (Node.js 22.12 and
// later), and requires its wrappers synchronously: if this module was still loading then, Node.js
// failed an internal assertion and the program died at start.
const fromInstrumentation = createRequire(createRequire(import.meta.url).resolve("@opentelemetry/instrumentation"));
await import(pathToFileURL(fromInstrumentation.resolve("import-in-the-middle/lib/register.js")).href);

// ES modules: `import` doesn't go through require, so the instrumentations can patch what it
// loads only through import-in-the-middle's loader hook. Upstream's register leaves that to a
// separate --experimental-loader flag; one --import here installs both the require hooks and
// import-in-the-middle's loader hook, so the application needs no --experimental-loader flag.
registerHooks("@opentelemetry/instrumentation/hook.mjs", import.meta.url);

const { shutdown } = register();
let stopped: Promise<void> | undefined;

function stop(): Promise<void> {
  // The server hears the agent go (agent_disconnect), then the SDK flushes.
  stopped ??= shutdown().catch((e: unknown) => say.error(`OpAMP: shutting down: ${String(e)}`));
  return stopped;
}

function terminated(): void {
  // A SIGTERM listener replaces Node.js's default, which is to end the process. The application's
  // own listener decides for it; without one, the process ends as Node.js would have ended it
  // (by the signal), once the goodbye is said and the telemetry flushed.
  const alone = process.listenerCount("SIGTERM") === 1;
  void stop().then(() => {
    if (!alone) return;
    process.removeListener("SIGTERM", terminated);
    process.kill(process.pid, "SIGTERM");
  });
}

// As upstream's register: a SIGTERM, or Node.js exiting normally.
process.on("SIGTERM", terminated);
process.once("beforeExit", () => void stop());
