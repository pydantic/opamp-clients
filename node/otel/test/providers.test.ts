// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// A global provider the application registers in place of the SDK's: what it sends through the
// API goes past the package's sampler and gates, so the overrides held there are set aside,
// reported local at once, and a policy that overrides them from then on is refused. The tests
// share the API's globals, so they run in order.

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
import { type AvailableComponents, type EffectiveConfig, RemoteConfigStatuses } from "@pydantic/opamp-client";

import { Agent, samplerFromEnv, say, watchProviders } from "../src/index.js";
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

/** The overrides the last effective config reported in force: what was set aside is local. */
const inForce = (got: { effective: Record<string, unknown>[] }) => got.effective.at(-1);
const settingsListed = (c: AvailableComponents | undefined) =>
  Object.keys(c?.components.settings?.subComponentMap ?? {}).sort();

test("a provider of the application's own, registered once the SDK has started, sets its settings aside and says so", async () => {
  const { a, got } = agent();
  const s = sdk(a);
  watchProviders(a, () => s.start());
  a.instrument([], {});
  const status = a.remoteConfig(config(POLICY));
  assert.equal(status.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  a.client!.setEffectiveConfig(a.effective!);
  assert.deepEqual(inForce(got), {
    head_sampling: { ratio: 1 }, signal_exports: off("traces", "metrics", "logs"), log_levels: { level: "warn" },
    attribute_redaction: { drop: ["x"] }, instrumentations: { all_disabled: true },
  });
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
  // Reported at once: head_sampling, the traces' export and redaction set aside (local), the rest in force.
  assert.equal(got.effective.length, 2);
  assert.deepEqual(inForce(got), {
    signal_exports: off("metrics", "logs"), log_levels: { level: "warn" }, instrumentations: { all_disabled: true },
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
  assert.deepEqual(inForce(got), { signal_exports: off("metrics"), instrumentations: { all_disabled: true } });
  assert.equal(a.controls.sendLogs, true);
  // A meter provider: the metrics' export.
  metrics.disable();
  metrics.setGlobalMeterProvider(new MeterProvider());
  assert.deepEqual(inForce(got), { instrumentations: { all_disabled: true } });

  // A policy decided from now on that overrides them is refused, and changes nothing.
  const refused = a.remoteConfig(config(policy({ head_sampling: { ratio: 0.5 }, signal_exports: off("traces") })));
  assert.equal(refused.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.equal(refused.errorMessage, `head_sampling: ${UNSUPPORTED}`);
  a.client!.setEffectiveConfig(a.effective!);
  assert.deepEqual(inForce(got), { instrumentations: { all_disabled: true } });
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
  // Traces past the SDK from the start: a policy that overrides what they need is refused.
  const refused = a.remoteConfig(config(POLICY));
  assert.equal(refused.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.equal(refused.errorMessage, `head_sampling: ${UNSUPPORTED}`);
  // The SDK's logger and meter providers registered, so logs and metrics are still carried out.
  const status = a.remoteConfig(config(policy({ signal_exports: off("metrics", "logs") })));
  assert.equal(status.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  a.client!.setEffectiveConfig(a.effective!);
  assert.deepEqual(inForce(got), { signal_exports: off("metrics", "logs") });
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
