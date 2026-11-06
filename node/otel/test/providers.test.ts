// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// A global provider the application registers in place of the SDK's: what it sends through the
// API goes past the package's sampler and gates, so it is reported at once as an ungoverned
// exporter of its signal, while the policy stays APPLIED and in force on everything the SDK
// exports. Nothing is set aside and nothing refused for it. The tests share the API's globals,
// so they run in order.

import assert from "node:assert/strict";
import { test } from "node:test";

import { metrics, trace } from "@opentelemetry/api";
import { logs } from "@opentelemetry/api-logs";
import { InMemoryLogRecordExporter, LoggerProvider, SimpleLogRecordProcessor } from "@opentelemetry/sdk-logs";
import {
  AggregationTemporality,
  InMemoryMetricExporter,
  MeterProvider,
  PeriodicExportingMetricReader,
} from "@opentelemetry/sdk-metrics";
import { NodeSDK } from "@opentelemetry/sdk-node";
import { BasicTracerProvider, InMemorySpanExporter, SimpleSpanProcessor } from "@opentelemetry/sdk-trace-base";
import { type AvailableComponents, type EffectiveConfig, RemoteConfigStatuses } from "@pydantic/logfire-fleet-client";

import { Agent, APP_PROVIDER, samplerFromEnv, say, watchProviders } from "../src/index.js";
import { config as configOf, off, overridden, policy } from "./policy.js";

const config = (body: string) => configOf(body, body);

const POLICY = policy({
  head_sampling: { ratio: 1 }, signal_exports: off("traces", "metrics", "logs"), log_levels: { level: "warn" },
  attribute_redaction: { drop: ["x"] }, instrumentations: { all_disabled: true },
});
const UNSUPPORTED = "this SDK cannot apply the requested override";

/** An agent as the register prepares one, with every exporter its own; its client keeps what it is handed. */
function agent() {
  const a = new Agent();
  const got = { effective: [] as Record<string, unknown>[], components: [] as AvailableComponents[], warned: [] as string[] };
  a.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: "none" });
  a.client = {
    setEffectiveConfig: (e: EffectiveConfig) => void got.effective.push(overridden(e)),
    setAvailableComponents: (c: AvailableComponents) => got.components.push(c),
    setRemoteConfigStatus: () => {},
    setHealth: () => {},
  } as never;
  say.warn = (m: string) => void got.warned.push(m);
  return { a, got };
}

/** The SDK as the register builds it, its exporters behind the agent's gates. */
function sdk(a: Agent) {
  return new NodeSDK({
    autoDetectResources: false,
    sampler: a.controls.sampler.wrap(samplerFromEnv({})),
    spanProcessors: [new SimpleSpanProcessor(a.controls.spans(new InMemorySpanExporter()))],
    logRecordProcessors: [new SimpleLogRecordProcessor({ exporter: a.controls.logs(new InMemoryLogRecordExporter()) })],
    metricReaders: [new PeriodicExportingMetricReader({
      exporter: a.controls.metrics(new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE)), exportIntervalMillis: 60_000,
    })],
  });
}

/** The overrides the last effective config reported in force. */
const inForce = (got: { effective: Record<string, unknown>[] }) => got.effective.at(-1);
/** The exporters components list, as signal to identity to gated. */
const exportersListed = (c: AvailableComponents | undefined) =>
  Object.fromEntries(Object.entries(c?.components.exporters?.subComponentMap ?? {}).map(([signal, d]) =>
    [signal, Object.fromEntries(Object.entries(d.subComponentMap).map(([name, e]) =>
      [name, e.metadata.find((kv) => kv.key === "gated")?.value?.value.value]))]));
/** The SDK's providers, as the register hands them to the agent once started. */
const providersOf = (s: NodeSDK) => {
  const p = s as unknown as Record<"_tracerProvider" | "_meterProvider" | "_loggerProvider", unknown>;
  return { traces: p._tracerProvider, metrics: p._meterProvider, logs: p._loggerProvider };
};

test("a provider of the application's own, registered once the SDK has started, is reported ungoverned and the policy stays", async () => {
  const { a, got } = agent();
  const s = sdk(a);
  watchProviders(a, () => s.start());
  a.govern(providersOf(s));
  a.instrument([]);
  const status = a.remoteConfig(config(POLICY));
  assert.equal(status.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  const all = {
    head_sampling: { ratio: 1 }, signal_exports: off("traces", "metrics", "logs"), log_levels: { level: "warn" },
    attribute_redaction: { drop: ["x"] }, instrumentations: { all_disabled: true },
  };
  assert.deepEqual(inForce(got), all);
  assert.deepEqual(exportersListed(a.components()),
    { traces: { InMemorySpanExporter: true }, metrics: { InMemoryMetricExporter: true }, logs: { InMemoryLogRecordExporter: true } });

  // The API refuses a second provider while the SDK's is registered: nothing goes past, nothing changes.
  assert.equal(trace.setGlobalTracerProvider(new BasicTracerProvider()), false);
  assert.equal(got.components.length, 0);

  // Registered once the SDK's is disabled: its spans are sampled and exported by it alone.
  trace.disable();
  const own = new InMemorySpanExporter();
  assert.equal(trace.setGlobalTracerProvider(new BasicTracerProvider({ spanProcessors: [new SimpleSpanProcessor(own)] })), true);
  trace.getTracer("app").startSpan("mine").end();
  assert.equal(own.getFinishedSpans().length, 1);
  // Reported at once as an ungoverned traces exporter, in the components and export health.
  assert.deepEqual(exportersListed(got.components.at(-1)).traces, { InMemorySpanExporter: true, [APP_PROVIDER]: false });
  const traces = a.state.health!.exporters!.componentHealthMap.traces!;
  assert.deepEqual(Object.fromEntries(Object.entries(traces.componentHealthMap).map(([k, c]) => [k, [c.status, c.healthy]])),
    { InMemorySpanExporter: ["governed", true], [APP_PROVIDER]: ["ungoverned", true] });
  // Nothing set aside: every override stays in force on what the SDK exports.
  assert.equal(got.effective.length, 1);
  assert.equal(a.controls.sampler.samplingRate, 1);
  assert.equal(a.controls.sendTraces, false);
  // The layer's words, as every SDK says them.
  assert.match(got.warned.join("\n"), /the application set a traces provider of its own: .*Fleet lists them ungoverned/);

  // The same signal again changes nothing more.
  const sent = got.components.length;
  trace.disable();
  trace.setGlobalTracerProvider(new BasicTracerProvider());
  assert.equal(got.components.length, sent);

  // A logger provider and a meter provider: reported the same way.
  logs.disable();
  logs.setGlobalLoggerProvider(new LoggerProvider());
  metrics.disable();
  metrics.setGlobalMeterProvider(new MeterProvider());
  const listed = exportersListed(got.components.at(-1));
  assert.equal(listed.logs?.[APP_PROVIDER], false);
  assert.equal(listed.metrics?.[APP_PROVIDER], false);

  // A policy decided from now on is APPLIED all the same.
  const next = a.remoteConfig(config(policy({ head_sampling: { ratio: 0.5 }, signal_exports: off("traces") })));
  assert.equal(next.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, next.errorMessage);
  assert.equal(a.controls.sampler.samplingRate, 0.5);
  await s.shutdown();
});

test("a provider of the application's own, registered before the SDK starts, is reported and the policy is APPLIED", async () => {
  trace.disable();
  logs.disable();
  metrics.disable();
  // The application's tracer provider first: the SDK's can't be registered, its instrumentations
  // would trace through the application's provider.
  trace.setGlobalTracerProvider(new BasicTracerProvider());
  const { a, got } = agent();
  const s = sdk(a);
  watchProviders(a, () => s.start());
  a.govern(providersOf(s));
  a.instrument([]);
  const status = a.remoteConfig(config(POLICY));
  assert.equal(status.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, status.errorMessage);
  assert.equal((inForce(got)?.signal_exports as Record<string, unknown>).logs !== undefined, true);
  assert.equal(exportersListed(a.components()).traces?.[APP_PROVIDER], false);
  await s.shutdown();
});

test("an exporter given to the SDK without a gate gets one once it has started", async () => {
  trace.disable();
  logs.disable();
  metrics.disable();
  const { a } = agent();
  const raw = new InMemorySpanExporter();
  const rawMetrics = new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE);
  const reader = new PeriodicExportingMetricReader({ exporter: rawMetrics, exportIntervalMillis: 60_000 });
  const s = new NodeSDK({ autoDetectResources: false, spanProcessors: [new SimpleSpanProcessor(raw)],
    logRecordProcessors: [], metricReaders: [reader] });
  watchProviders(a, () => s.start());
  a.govern(providersOf(s));
  assert.equal(a.remoteConfig(config(policy({ signal_exports: off("traces", "metrics") }))).status,
    RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  trace.getTracer("app").startSpan("dropped").end();
  assert.equal(raw.getFinishedSpans().length, 0);
  metrics.getMeter("app").createCounter("c").add(1);
  await reader.forceFlush();
  assert.equal(rawMetrics.getMetrics().length, 0);
  // Governed again: still one gate, the same listing.
  const once = a.exporters;
  a.govern(providersOf(s));
  assert.deepEqual(a.exporters, once);
  await s.shutdown();
});

test("an agent that isn't on hears nothing, and the setters still work", () => {
  trace.disable();
  const a = new Agent();
  watchProviders(a, () => {});
  const p = new BasicTracerProvider();
  assert.equal(trace.setGlobalTracerProvider(p), true);
  assert.equal(a.ownProviders.size, 0);
});

test("a failing Exporters call keeps the names last decided, and the application never sees it", () => {
  const { a, got } = agent();
  const s = sdk(a);
  watchProviders(a, () => s.start());
  a.govern(providersOf(s));
  const before = a.exporters;
  assert.ok(before.length > 0);
  // As a trapped core would.
  a.layer!.exporters = () => {
    throw new Error("the core trapped");
  };
  a.govern(providersOf(s));
  trace.disable();
  // Registering a provider of its own goes through to the API, error or not.
  assert.equal(trace.setGlobalTracerProvider(new BasicTracerProvider()), true);
  // Those listed before keep their names; the provider new since is listed as found.
  assert.deepEqual(a.exporters.filter((e) => e.name !== APP_PROVIDER), before.filter((e) => e.name !== APP_PROVIDER));
  assert.ok(a.exporters.some((e) => e.signal === "traces" && e.name === APP_PROVIDER && !e.gated));
  assert.ok(got.warned.some((w) => w.includes("can't list the exporters")), got.warned.join("\n"));
  // Regression: one listed as found carried its raw endpoint, credentials and all.
  a.found = [{ signal: "logs", kind: "Mine", gated: false, endpoint: "https://user:hunter2@col:4318/v1/logs?token=t" }];
  a.ownProviders.delete("logs"); // earlier tests may have registered one: the API is global
  a.providerOfItsOwn("logs"); // listed again
  assert.equal(a.exporters.find((e) => e.name === "Mine")?.destination, "");
  trace.disable();
});
