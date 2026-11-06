// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The local report (contracts' logfire-sdk-local/v1): what the SDK register builds does without
// the server, as the host's `local` carries it to the layer; and each instrumentation's scope in
// the inventory.

import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { createRequire } from "node:module";

import { getNodeAutoInstrumentations } from "@opentelemetry/auto-instrumentations-node";
import type { Instrumentation } from "@opentelemetry/instrumentation";

import {
  Agent, decimal, inventory, localFromEnv, managedHost, samplerFromEnv, samplingRateFromEnv, whenApplied,
} from "../src/index.js";

test("the sampling rate is the ratio of the sampler OTEL_TRACES_SAMPLER names", () => {
  const cases: [Record<string, string>, number, RegExp][] = [
    [{}, 1, /ParentBased\{root=AlwaysOnSampler/],
    [{ OTEL_TRACES_SAMPLER: "always_on" }, 1, /^AlwaysOnSampler/],
    [{ OTEL_TRACES_SAMPLER: "always_off" }, 0, /^AlwaysOffSampler/],
    [{ OTEL_TRACES_SAMPLER: "parentbased_always_off" }, 0, /ParentBased\{root=AlwaysOffSampler/],
    [{ OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "0.25" }, 0.25, /^TraceIdRatioBased\{0.25\}/],
    [{ OTEL_TRACES_SAMPLER: "parentbased_traceidratio", OTEL_TRACES_SAMPLER_ARG: "0.1" }, 0.1,
      /ParentBased\{root=TraceIdRatioBased\{0.1\}/],
    // Out of range or not a number: 1, as the sampler built.
    [{ OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "2" }, 1, /^TraceIdRatioBased\{1\}/],
    [{ OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "half" }, 1, /^TraceIdRatioBased\{1\}/],
  ];
  for (const [env, rate, sampler] of cases) {
    assert.equal(samplingRateFromEnv(env), rate, JSON.stringify(env));
    assert.match(samplerFromEnv(env).toString(), sampler, JSON.stringify(env));
    assert.equal(localFromEnv(env).samplingRate, String(rate));
  }
});

test("a ratio is decimal text, never with an exponent", () => {
  for (const [x, text] of [[1, "1"], [0, "0"], [0.25, "0.25"], [0.00001, "0.00001"], [1e-7, "0.0000001"],
    [1.5e-7, "0.00000015"], [1 / 3, "0.3333333333333333"]] as const) {
    assert.equal(decimal(x), text);
    assert.equal(Number(text), x);
  }
});

test("each signal is exported where the variables name an exporter for it", () => {
  assert.deepEqual(localFromEnv({}), { samplingRate: "1", sendTraces: true, sendMetrics: true, sendLogs: true });
  const off = localFromEnv({ OTEL_TRACES_EXPORTER: "none", OTEL_METRICS_EXPORTER: "console", OTEL_LOGS_EXPORTER: "none" });
  assert.deepEqual([off.sendTraces, off.sendMetrics, off.sendLogs], [false, true, false]);
  // One NodeSDK builds itself (zipkin, prometheus) can't be told here: left out.
  const left = localFromEnv({ OTEL_TRACES_EXPORTER: "zipkin", OTEL_METRICS_EXPORTER: "prometheus" });
  assert.equal("sendTraces" in left, false);
  assert.equal("sendMetrics" in left, false);
  assert.equal(left.sendLogs, true);
  // The SDK has no level or scrubbing of its own.
  assert.equal("sendLogsLevel" in left, false);
  assert.equal("scrubbing" in left, false);
});

test("register's host carries the local values; manage()'s doesn't: the SDK is the application's", () => {
  const agent = new Agent();
  agent.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: mkdtempSync(join(tmpdir(), "opamp-otel-")),
    OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "0.5", OTEL_LOGS_EXPORTER: "none" });
  assert.deepEqual(agent.host.local, { samplingRate: "0.5", sendTraces: true, sendMetrics: true, sendLogs: false });
  // Reported beside the policy in force, as the layer writes it.
  const files = () => agent.effective!.configMap!.configMap;
  const local = JSON.parse(new TextDecoder().decode(files()["logfire-sdk-local.json"]!.body)) as Record<string, unknown>;
  assert.deepEqual(local.head_sampling, { ratio: 0.5 });
  assert.deepEqual(local.signal_exports, { traces: { enabled: true }, metrics: { enabled: true }, logs: { enabled: false } });
  assert.equal("log_levels" in local, false);
  agent.narrow(managedHost());
  assert.equal(agent.host.local, undefined);
  assert.deepEqual(Object.keys(files()), ["logfire-sdk-policy.json"]);
  agent.abandon();
});

test("each instrumentation is listed with its scope: the name its tracer is under", () => {
  const i = (name: string, version?: string) =>
    ({ instrumentationName: name, instrumentationVersion: version, setTracerProvider() {} }) as unknown as Instrumentation;
  const got = inventory([i("@opentelemetry/instrumentation-http", "0.222.0"), i("@opentelemetry/instrumentation-fs")])
    .instrumentations;
  assert.deepEqual(got, [
    { name: "http", version: "0.222.0", scope: "@opentelemetry/instrumentation-http" },
    { name: "fs", version: "", scope: "@opentelemetry/instrumentation-fs" },
  ]);
});

/** An instrumentation with module definitions, as @opentelemetry/instrumentation holds them. */
function patching(name: string, defs: Record<string, unknown>[]) {
  return {
    instrumentationName: name,
    instrumentationVersion: "1.0.0",
    setTracerProvider() {},
    _modules: defs,
  } as unknown as Instrumentation;
}

test("an instrumentation applies once it patches a module the application loaded, or one of its files", () => {
  const seen: string[] = [];
  const main = { name: "pg", patch: (e: unknown) => e, files: [{ name: "pg/lib/client.js", patch: (e: unknown) => e }] };
  whenApplied(patching("@opentelemetry/instrumentation-pg", [main]), () => seen.push("pg"));
  assert.deepEqual(seen, []); // its library isn't loaded
  // The require hook calls the definition's patch, as it reads it then: once only.
  const exports = {};
  assert.equal(main.patch(exports), exports);
  main.patch(exports);
  main.files[0]!.patch(exports);
  assert.deepEqual(seen, ["pg"]);
  // Patched already (before it was watched), or patching no module at all: at once.
  const before: string[] = [];
  whenApplied(patching("a", [{ name: "a", patch: (e: unknown) => e, moduleExports: {} }]), () => before.push("a"));
  whenApplied(patching("@opentelemetry/instrumentation-undici", []), () => before.push("undici"));
  assert.deepEqual(before, ["a", "undici"]);
});

test("a real instrumentation applies once the application requires its module", () => {
  const all = getNodeAutoInstrumentations({ "@opentelemetry/instrumentation-fs": { enabled: false } });
  const applied: string[] = [];
  for (const i of all) whenApplied(i, () => applied.push(i.instrumentationName));
  assert.equal(applied.includes("@opentelemetry/instrumentation-mongodb"), false); // not installed here
  const http = createRequire(import.meta.url)("node:http");
  assert.ok(http);
  assert.ok(applied.includes("@opentelemetry/instrumentation-http"), applied.join(", "));
  for (const i of all) i.disable();
});

test("the inventory lists the instrumentations that apply, and one that applies later once it does", async () => {
  const agent = new Agent();
  agent.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: mkdtempSync(join(tmpdir(), "opamp-otel-")) });
  const pg = { name: "pg", patch: (e: unknown) => e };
  agent.instrument([
    patching("@opentelemetry/instrumentation-http", [{ name: "http", patch: (e: unknown) => e, moduleExports: {} }]),
    patching("@opentelemetry/instrumentation-pg", [pg]),
    patching("@opentelemetry/instrumentation-undici", []),
  ]);
  const names = () => (agent.inventory?.instrumentations ?? []).map((i) => i.name);
  assert.deepEqual(names(), ["http", "undici"]);
  pg.patch({}); // the application loads pg
  assert.deepEqual(names(), ["http", "undici"]); // listed once the require has returned
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(names(), ["http", "pg", "undici"]);
  agent.abandon();
});
