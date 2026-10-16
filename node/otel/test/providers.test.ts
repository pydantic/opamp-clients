// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// A global provider the application registers in place of the SDK's: what it sends through the
// API goes past the package's sampler and gates, so the settings held there are set aside, named
// as such and reported at once. The tests share the API's globals, so they run in order.

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
import { type AvailableComponents, AgentRemoteConfigSchema, create, RemoteConfigStatuses } from "@pydantic/opamp-client";

import { Agent, samplerFromEnv, say, watchProviders } from "../src/index.js";

const text = (s: string) => new TextEncoder().encode(s);
const json = (b: Uint8Array | undefined) => JSON.parse(new TextDecoder().decode(b));
const config = (body: string) =>
  create(AgentRemoteConfigSchema, {
    configHash: text(body),
    config: { configMap: { elastic: { body: text(body), contentType: "application/json" } } },
  });

const DOCUMENT = JSON.stringify({
  sampling_rate: "1", send_traces: "false", send_metrics: "false", send_logs: "false", send_logs_level: "warn",
  drop_attributes: "x", deactivate_all_instrumentations: "true",
});

/** An agent as the register prepares one, with every exporter its own; its client keeps what it is handed. */
function agent() {
  const a = new Agent();
  const got = { effective: [] as object[], components: [] as AvailableComponents[], warned: [] as string[] };
  a.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: "none" });
  a.client = {
    setEffectiveConfig: (e: { configMap?: { configMap: Record<string, { body: Uint8Array }> } }) => {
      const m = e.configMap!.configMap;
      got.effective.push([json(m.elastic?.body), m["elastic.set_aside"] ? json(m["elastic.set_aside"].body).unsupported : []]);
    },
    setAvailableComponents: (c: AvailableComponents) => got.components.push(c),
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

/** The settings each effective config reported names as set aside. */
const aside = (got: { effective: object[] }) => (got.effective.at(-1) as [object, string[]])[1];
const inForce = (got: { effective: object[] }) => (got.effective.at(-1) as [Record<string, string>, string[]])[0];
const settingsListed = (c: AvailableComponents | undefined) =>
  Object.keys(c?.components.settings?.subComponentMap ?? {}).sort();

test("a provider of the application's own, registered once the SDK has started, sets its settings aside and says so", async () => {
  const { a, got } = agent();
  const s = sdk(a);
  watchProviders(a, () => s.start());
  a.instrument([], {});
  const status = a.remoteConfig(config(DOCUMENT));
  assert.equal(status.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  a.client!.setEffectiveConfig(a.effective!);
  assert.deepEqual(aside(got), []);
  assert.equal(a.controls.sampler.samplingRate, 1);
  const before = settingsListed(a.components());
  assert.ok(before.includes("send_traces") && before.includes("sampling_rate"), before.join(", "));

  // The API refuses a second provider while the SDK's is registered: nothing goes past, nothing changes.
  assert.equal(trace.setGlobalTracerProvider(new BasicTracerProvider()), false);
  assert.equal(got.effective.length, 1);

  // Registered once the SDK's is disabled: its spans are sampled and exported by it alone.
  trace.disable();
  const own = new InMemorySpanExporter();
  assert.equal(trace.setGlobalTracerProvider(new BasicTracerProvider({ spanProcessors: [new SimpleSpanProcessor(own)] })), true);
  trace.getTracer("app").startSpan("mine").end();
  assert.equal(own.getFinishedSpans().length, 1);
  // Reported at once: the sampler's settings, send_traces and redaction set aside, the rest in force.
  assert.equal(got.effective.length, 2);
  assert.deepEqual(aside(got), ["drop_attributes", "sampling_rate", "send_traces"]);
  assert.deepEqual(inForce(got), {
    send_metrics: "false", send_logs: "false", send_logs_level: "warn", deactivate_all_instrumentations: "true",
  });
  assert.equal(a.controls.sampler.samplingRate, undefined);
  assert.equal(a.controls.sendTraces, true);
  assert.equal(a.controls.sendLogs, false);
  // The inventory's settings follow, sent again.
  const listed = settingsListed(got.components.at(-1));
  assert.ok(!listed.includes("send_traces") && !listed.includes("sampling_rate") && listed.includes("send_logs"), listed.join(", "));
  assert.match(got.warned.join("\n"), /a tracer provider of its own, which exports past this package: sampling_rate, .*send_traces/);

  // The same signal again changes nothing more.
  trace.disable();
  trace.setGlobalTracerProvider(new BasicTracerProvider());
  assert.equal(got.effective.length, 2);

  // A logger provider: the log gate's settings.
  logs.disable();
  logs.setGlobalLoggerProvider(new LoggerProvider());
  assert.deepEqual(aside(got), ["drop_attributes", "sampling_rate", "send_logs", "send_logs_level", "send_traces"]);
  assert.equal(a.controls.sendLogs, true);
  // A meter provider: send_metrics.
  metrics.disable();
  metrics.setGlobalMeterProvider(new MeterProvider());
  assert.deepEqual(aside(got),
    ["drop_attributes", "sampling_rate", "send_logs", "send_logs_level", "send_metrics", "send_traces"]);
  assert.deepEqual(inForce(got), { deactivate_all_instrumentations: "true" });

  // A config decided from now on sets them aside too.
  a.remoteConfig(config(JSON.stringify({ sampling_rate: "0.5", send_traces: "false" })));
  a.client!.setEffectiveConfig(a.effective!);
  assert.deepEqual(aside(got), ["sampling_rate", "send_traces"]);
  await s.shutdown();
});

test("a provider of the application's own, registered before the SDK starts, is seen as the SDK's is refused", async () => {
  trace.disable();
  logs.disable();
  metrics.disable();
  // The application's tracer provider first: the SDK's can't be registered, its instrumentations
  // would trace through the application's provider.
  trace.setGlobalTracerProvider(new BasicTracerProvider());
  const { a, got } = agent();
  const s = sdk(a);
  watchProviders(a, () => s.start());
  a.instrument([], {});
  a.remoteConfig(config(DOCUMENT));
  a.client!.setEffectiveConfig(a.effective!);
  // Traces set aside; the SDK's logger and meter providers registered, so logs and metrics stay.
  assert.deepEqual(aside(got), ["drop_attributes", "sampling_rate", "send_traces"]);
  assert.equal(inForce(got).send_logs, "false");
  assert.equal(inForce(got).send_metrics, "false");
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
