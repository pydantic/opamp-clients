// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { beforeEach, test } from "node:test";

import { context, diag, DiagLogLevel, ROOT_CONTEXT, SpanKind } from "@opentelemetry/api";
import { OTLPLogExporter as ProtoLogs } from "@opentelemetry/exporter-logs-otlp-proto";
import { resourceFromAttributes } from "@opentelemetry/resources";
import {
  AlwaysOffSampler,
  AlwaysOnSampler,
  BasicTracerProvider,
  InMemorySpanExporter,
  ParentBasedSampler,
  SimpleSpanProcessor,
  TraceIdRatioBasedSampler,
} from "@opentelemetry/sdk-trace-base";
import { type Client, RemoteConfigStatuses } from "@pydantic/opamp-client";
import { SdkSettings } from "@pydantic/opamp-client/sdk";
import { keepApplied } from "./kept.js";
import { config as policyConfig, off, overridden, policy } from "./policy.js";

import {
  Agent,
  AppSettings,
  attributes,
  batchLogOptions,
  batchSpanOptions,
  Controls,
  exporterNames,
  exportersFromEnv,
  hostFromEnv,
  metricReaderOptions,
  preparing,
  programName,
  Reachability,
  release,
  say,
  samplerFromEnv,
  shownEndpoint,
  setupFromEnv,
  type Options,
  type Setup,
} from "../src/index.js";

/** Processes of one program that each keep their own identity by default (the core's). */
const SLOTS = 16;

/** What a Setup carries to the client, without what it logs (the endpoint shown, the hints). */
function bare(s: Setup | undefined): Partial<Setup> | undefined {
  if (s === undefined) return undefined;
  const { shown: _shown, hints: _hints, ...rest } = s;
  return rest;
}

/** The core's hints and the endpoint as shown, for a server given by OTEL_OPAMP_ENDPOINT. */
function shownFor(endpoint: string): Setup {
  return setupFromEnv({ OTEL_OPAMP_ENDPOINT: endpoint, OTEL_OPAMP_STATE_DIR: "none" })!;
}

// The default state directory (and every child process's) under the test's own home.
beforeEach(() => {
  const home = mkdtempSync(join(tmpdir(), "opamp-otel-home-"));
  process.env.XDG_STATE_HOME = home;
  process.env.LOCALAPPDATA = home;
  process.env.HOME = home;
});

const LAYER = new SdkSettings();
const text = (s: string) => new TextEncoder().encode(s);
const settings = (doc: string) => LAYER.load(text(doc), hostFromEnv({})).settings;
// Every setting (docs/sdk-settings.md), and those a host built from `env` doesn't declare.
const SETTINGS = ["debug_sampling_rate", "debug_send_logs_level", "debug_until", "deactivate_all_instrumentations",
  "deactivate_instrumentations", "drop_attributes", "mask_attributes", "sampling_rate",
  "sampling_rules", "sampling_traces_per_second", "send_logs", "send_logs_level", "send_logs_level_by_scope",
  "send_logs_per_second", "send_metrics", "send_traces"];
const undeclared = (env: NodeJS.ProcessEnv) => {
  const host = hostFromEnv(env);
  return SETTINGS.filter((k) => !host.live!.includes(k));
};
const LOG_GATE = ["debug_send_logs_level", "drop_attributes", "mask_attributes", "send_logs", "send_logs_level",
  "send_logs_level_by_scope", "send_logs_per_second"];
const config = (body: string) => policyConfig(body, body);
/** The controls the agent's effective config reports overridden. */
const effective = (a: Agent) => overridden(a.effective);

/** What the package says on the diag logger while `fn` runs, as [level, message]. */
function said(fn: () => void): [string, string][] {
  const out: [string, string][] = [];
  const at = (level: string) => (message: string) => void out.push([level, message]);
  diag.setLogger({ error: at("error"), warn: at("warn"), info: at("info"), debug: at("debug"), verbose: at("verbose") },
    DiagLogLevel.ALL);
  const before = say.console;
  say.console = false; // as with OTEL_LOG_LEVEL set
  try {
    fn();
  } finally {
    say.console = before;
    diag.disable();
  }
  return out;
}

function prepared(dir: string, extra: Record<string, string> = {}): { agent: Agent; env: NodeJS.ProcessEnv } {
  const env: NodeJS.ProcessEnv = { OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: dir, ...extra };
  const agent = new Agent();
  agent.prepare(env);
  return { agent, env };
}

test("the SDK's sampler until a rate is set, and again when it is withdrawn", () => {
  const c = new Controls();
  const own = new AlwaysOffSampler();
  c.sampler.wrap(own);
  assert.equal(c.sampler.current, own);
  c.apply(settings(policy({ head_sampling: { ratio: 0.25 } })));
  assert.equal(c.sampler.current.toString(),
    new ParentBasedSampler({ root: new TraceIdRatioBasedSampler(0.25) }).toString());
  c.apply(settings(policy()));
  assert.equal(c.sampler.current, own);
  // A rate saved before the SDK builds its sampler still holds.
  const d = new Controls();
  d.apply(settings(policy({ head_sampling: { ratio: 0 } })));
  d.sampler.wrap(new AlwaysOnSampler());
  assert.match(d.sampler.current.toString(), /TraceIdRatioBased\{0\}/);
});

test("spans stop and start with send_traces, and a rate of 0 samples nothing", async () => {
  const c = new Controls();
  const out = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({
    sampler: c.sampler.wrap(new AlwaysOnSampler()),
    spanProcessors: [new SimpleSpanProcessor(c.spans(out))],
  });
  const t = provider.getTracer("t");
  const span = (name: string) => context.with(ROOT_CONTEXT, () => t.startSpan(name, { kind: SpanKind.INTERNAL }).end());
  span("one");
  c.apply(settings(policy({ signal_exports: off("traces") })));
  span("dropped");
  c.apply(settings(policy({ head_sampling: { ratio: 0 } })));
  for (let i = 0; i < 50; i++) span("unsampled");
  c.apply(settings(policy()));
  span("two");
  await provider.forceFlush();
  assert.deepEqual(out.getFinishedSpans().map((s) => s.name), ["one", "two"]);
});

test("metrics and logs stop with send_metrics and send_logs", () => {
  const c = new Controls();
  const reached: string[] = [];
  const sink = (name: string) => ({
    export: (_x: unknown, done: (r: { code: number }) => void) => { reached.push(name); done({ code: 0 }); },
    shutdown: () => Promise.resolve(),
    forceFlush: () => Promise.resolve(),
  });
  const metrics = c.metrics(sink("metrics") as never);
  const logs = c.logs(sink("logs") as never);
  const done = () => {};
  metrics.export({ scopeMetrics: [] } as never, done);
  logs.export([], done);
  c.apply(settings(policy({ signal_exports: off("metrics", "logs") })));
  metrics.export({ scopeMetrics: [] } as never, done);
  logs.export([], done);
  assert.deepEqual(reached, ["metrics", "logs"]);
});

test("send_logs_level drops the log records below it, live", () => {
  const c = new Controls();
  const sent: number[][] = [];
  const logs = c.logs({
    export: (x: { severityNumber?: number }[], done: (r: { code: number }) => void) => {
      sent.push(x.map((r) => r.severityNumber ?? 0));
      done({ code: 0 });
    },
    shutdown: () => Promise.resolve(),
    forceFlush: () => Promise.resolve(),
  } as never);
  const record = (n: number | undefined) => ({ severityNumber: n, instrumentationScope: { name: "app" } });
  const records = [undefined, 0, 1, 5, 9, 12, 13, 17, 21].map(record) as never;
  const results: number[] = [];
  const done = (r: { code: number }) => results.push(r.code);
  logs.export(records, done);
  c.apply(settings(policy({ log_levels: { level: "warn" } })));
  assert.equal(c.sendLogsMinSeverity, 13);
  logs.export(records, done);
  // Every record below the floor: nothing goes out, and the export succeeds.
  logs.export([record(9)] as never, done);
  c.apply(settings(policy({ log_levels: { level: "warn" }, signal_exports: off("logs") })));
  logs.export(records, done);
  c.apply(settings(policy()));
  assert.equal(c.sendLogsMinSeverity, undefined);
  logs.export(records, done);
  assert.deepEqual(sent, [[0, 0, 1, 5, 9, 12, 13, 17, 21], [0, 0, 13, 17, 21], [0, 0, 1, 5, 9, 12, 13, 17, 21]]);
  assert.deepEqual(results, [0, 0, 0, 0, 0]);
});

/** An instrumentation as `Controls.instrument` sees one: its name, and the provider it is handed. */
function loaded(name: string) {
  return { instrumentationName: `@opentelemetry/instrumentation-${name}`, setTracerProvider: () => {} } as never;
}

test("instrumentations are switched by name, or all at once; the application's own scopes never", () => {
  const c = new Controls();
  c.instrument([loaded("http"), loaded("pg")]);
  const http = "@opentelemetry/instrumentation-http";
  const pg = "@opentelemetry/instrumentation-pg";
  assert.deepEqual([c.off(http), c.off(pg)], [false, false]);
  c.apply(settings(policy({ instrumentations: { disabled: ["http", "fs"] } })));
  assert.deepEqual([c.off(http), c.off(pg)], [true, false]);
  // fs isn't loaded (it is off by default): nothing to switch.
  assert.deepEqual(c.unloaded, ["fs"]);
  c.apply(settings(policy({ instrumentations: { all_disabled: true } })));
  assert.deepEqual([c.off(http), c.off(pg), c.off("checkout"), c.off("@opentelemetry/instrumentation-fs")],
    [true, true, false, false]);
  c.apply(settings(policy()));
  assert.deepEqual([c.off(http), c.off(pg)], [false, false]);
});

test("a switched-off instrumentation's metrics and log records aren't exported", () => {
  const c = new Controls();
  c.instrument([loaded("http"), loaded("pino")]);
  const metrics: string[][] = [];
  const logs: string[][] = [];
  const sink = <T>(into: string[][], names: (x: T) => string[]) => ({
    export: (x: T, done: (r: { code: number }) => void) => { into.push(names(x)); done({ code: 0 }); },
    shutdown: () => Promise.resolve(),
    forceFlush: () => Promise.resolve(),
  });
  type Metrics = { scopeMetrics: { scope: { name: string } }[] };
  type Logs = { instrumentationScope: { name: string } }[];
  const m = c.metrics(sink<Metrics>(metrics, (x) => x.scopeMetrics.map((s) => s.scope.name)) as never);
  const l = c.logs(sink<Logs>(logs, (x) => x.map((r) => r.instrumentationScope.name)) as never);
  const scopes = (...names: string[]) => names.map((name) => ({ scope: { name }, instrumentationScope: { name }, metrics: [] }));
  const done = () => {};
  const both = () => {
    m.export({ scopeMetrics: scopes("@opentelemetry/instrumentation-http", "checkout") } as never, done);
    l.export(scopes("@opentelemetry/instrumentation-pino", "checkout") as never, done);
  };
  both();
  c.apply(settings(policy({ instrumentations: { disabled: ["http", "pino"] } })));
  both();
  c.apply(settings(policy({ instrumentations: { all_disabled: true } })));
  m.export({ scopeMetrics: scopes("@opentelemetry/instrumentation-http") } as never, done); // nothing left
  l.export(scopes("@opentelemetry/instrumentation-pino") as never, done);
  c.apply(settings(policy()));
  both();
  assert.deepEqual(metrics, [["@opentelemetry/instrumentation-http", "checkout"], ["checkout"],
    ["@opentelemetry/instrumentation-http", "checkout"]]);
  assert.deepEqual(logs, [["@opentelemetry/instrumentation-pino", "checkout"], ["checkout"],
    ["@opentelemetry/instrumentation-pino", "checkout"]]);
});

test("settings read as the SDK reads its own", () => {
  const none = { OTEL_OPAMP_ENDPOINT: "http://s", OTEL_OPAMP_STATE_DIR: "none" };
  assert.deepEqual(setupFromEnv({ ...none, OTEL_OPAMP_HEADERS: "Authorization=Bearer%20a%20b, x=1=2,broken" })!.headers,
    { Authorization: "Bearer a b", x: "1=2" });
  assert.deepEqual(["1500", "500ms", "30s", "2m"].map((v) => setupFromEnv({ ...none, OTEL_OPAMP_HEARTBEAT_INTERVAL: v })!.heartbeatMs),
    [1500, 500, 30_000, 120_000]);
  assert.equal(setupFromEnv({}), undefined);
  assert.deepEqual(bare(setupFromEnv({ OTEL_OPAMP_ENDPOINT: " ws://s/v1/opamp ", OTEL_OPAMP_HEARTBEAT_INTERVAL: "1s" })),
    { endpoint: "ws://s/v1/opamp", headers: {}, heartbeatMs: 1000 });
  assert.match(samplerFromEnv({}).toString(), /ParentBased\{root=AlwaysOnSampler/);
  assert.match(samplerFromEnv({ OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "0.1" }).toString(),
    /TraceIdRatioBased\{0.1\}/);
  const e = exportersFromEnv({ OTEL_TRACES_EXPORTER: "console", OTEL_METRICS_EXPORTER: "none", OTEL_LOGS_EXPORTER: "zipkin" });
  assert.equal(e.traces?.length, 1);
  assert.deepEqual(e.metrics, []);
  assert.equal(e.logs, undefined); // left to NodeSDK
});

test("an empty variable is an unset one, as the SDK reads them", () => {
  // Not a ratio of 0: the SDK's default of 1.
  assert.match(samplerFromEnv({ OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "" }).toString(),
    /TraceIdRatioBased\{1\}/);
  assert.match(samplerFromEnv({ OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "half" }).toString(),
    /TraceIdRatioBased\{1\}/);
  assert.match(samplerFromEnv({ OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: " 0 " }).toString(),
    /TraceIdRatioBased\{0\}/);
  // An empty per-signal protocol falls through to the general one.
  const grpc = { OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: "", OTEL_EXPORTER_OTLP_PROTOCOL: "grpc" };
  assert.deepEqual(undeclared(grpc), []);
  assert.equal(exportersFromEnv(grpc).traces?.[0]?.constructor.name, "OTLPTraceExporter");
  assert.deepEqual(undeclared({ OTEL_EXPORTER_OTLP_LOGS_PROTOCOL: " ", OTEL_LOGS_EXPORTER: "" }), []);
});

test("exporter lists read as NodeSDK reads them: repeats once, none as it treats none", () => {
  assert.deepEqual(exporterNames("TRACES", { OTEL_TRACES_EXPORTER: "console, console,otlp" }), ["console", "otlp"]);
  assert.equal(exportersFromEnv({ OTEL_LOGS_EXPORTER: "console,console" }).logs?.length, 1);
  // Traces: none first is none; none among others is the default exporter alone.
  assert.deepEqual(exporterNames("TRACES", { OTEL_TRACES_EXPORTER: "none,console" }), []);
  assert.deepEqual(exporterNames("TRACES", { OTEL_TRACES_EXPORTER: "console,none" }), ["otlp"]);
  assert.deepEqual(exporterNames("TRACES", { OTEL_TRACES_EXPORTER: "null,console" }), ["console"]);
  // Metrics and logs: none anywhere is none.
  assert.deepEqual(exporterNames("METRICS", { OTEL_METRICS_EXPORTER: "console,none" }), []);
  assert.deepEqual(exporterNames("LOGS", { OTEL_LOGS_EXPORTER: "otlp,none" }), []);
  assert.deepEqual(exporterNames("LOGS", { OTEL_LOGS_EXPORTER: " , " }), ["otlp"]);
  // No logger provider at all for none, as NodeSDK: not an empty one.
  assert.equal(exportersFromEnv({ OTEL_LOGS_EXPORTER: "none" }).logs, undefined);
  assert.deepEqual(undeclared({ OTEL_LOGS_EXPORTER: "none" }), []);
});

test("the metric reader and log batching take their variables as NodeSDK does", () => {
  assert.deepEqual(metricReaderOptions({}), { exportIntervalMillis: 60_000, exportTimeoutMillis: 30_000 });
  // 0 and NaN are the defaults, not a crash and not a flood.
  assert.deepEqual(metricReaderOptions({ OTEL_METRIC_EXPORT_INTERVAL: "0", OTEL_METRIC_EXPORT_TIMEOUT: "x" }),
    { exportIntervalMillis: 60_000, exportTimeoutMillis: 30_000 });
  assert.deepEqual(metricReaderOptions({ OTEL_METRIC_EXPORT_INTERVAL: "5000" }),
    { exportIntervalMillis: 5000, exportTimeoutMillis: 5000 }); // the timeout clamped to the interval
  assert.deepEqual(metricReaderOptions({ OTEL_METRIC_EXPORT_INTERVAL: "5000", OTEL_METRIC_EXPORT_TIMEOUT: "1000" }),
    { exportIntervalMillis: 5000, exportTimeoutMillis: 1000 });
  assert.deepEqual(batchLogOptions({ OTEL_BLRP_SCHEDULE_DELAY: "200", OTEL_BLRP_MAX_QUEUE_SIZE: "-1",
    OTEL_BLRP_EXPORT_TIMEOUT: "", OTEL_BLRP_MAX_EXPORT_BATCH_SIZE: "64" }),
  { scheduledDelayMillis: 200, maxExportBatchSize: 64 });
  // Spans: NodeSDK's defaults where it refuses a value, never the variable itself.
  assert.deepEqual(batchSpanOptions({ OTEL_BSP_SCHEDULE_DELAY: "0", OTEL_BSP_MAX_QUEUE_SIZE: "-1",
    OTEL_BSP_EXPORT_TIMEOUT: "x", OTEL_BSP_MAX_EXPORT_BATCH_SIZE: "64" }),
  { maxQueueSize: 2048, scheduledDelayMillis: 5000, exportTimeoutMillis: 30_000, maxExportBatchSize: 64 });
});

test("resource attributes reach the server typed, lists as lists", () => {
  const kv = attributes(resourceFromAttributes({ s: "a", i: 3, d: 1.5, b: true, big: 2 ** 60, l: ["x", "y"], n: [1, 2] }));
  const by = Object.fromEntries(kv.map((k) => [k.key, k.value?.value]));
  assert.deepEqual(by.s, { case: "stringValue", value: "a" });
  assert.deepEqual(by.i, { case: "intValue", value: 3n });
  assert.deepEqual(by.d, { case: "doubleValue", value: 1.5 });
  assert.deepEqual(by.b, { case: "boolValue", value: true });
  assert.equal(by.big?.case, "doubleValue"); // past 2^53 an integer isn't exact
  assert.equal(by.l?.case, "arrayValue");
  assert.deepEqual((by.l?.value as { values: { value: unknown }[] }).values.map((v) => v.value),
    [{ case: "stringValue", value: "x" }, { case: "stringValue", value: "y" }]);
  assert.deepEqual((by.n?.value as { values: { value: unknown }[] }).values.map((v) => v.value),
    [{ case: "intValue", value: 1n }, { case: "intValue", value: 2n }]);
});

test("live settings are applied at once and reported, the package saving nothing; a wrong config changes nothing", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const { agent } = prepared(dir);
  const s = agent.remoteConfig(config(policy({ head_sampling: { ratio: 0.5 }, signal_exports: off("logs") })));
  assert.equal(s.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  assert.equal(agent.controls.sendLogs, false);
  assert.deepEqual(effective(agent), { head_sampling: { ratio: 0.5 }, signal_exports: off("logs") });
  // The client keeps the config applied, in its own state: nothing of the package's own.
  assert.equal(existsSync(join(dir, "sdk-settings.json")), false);
  const bad = agent.remoteConfig(config(policy({ head_sampling: { ratio: 2 } })));
  assert.equal(bad.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.match(bad.errorMessage, /^head_sampling:/);
  assert.equal(agent.controls.sendLogs, false);
});

test("instrumentations are switched at once, reported, and still switched at the next start", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const { agent } = prepared(dir);
  agent.instrument([loaded("http")]);
  const http = "@opentelemetry/instrumentation-http";
  const sw = policy({ instrumentations: { disabled: ["http"] } });
  assert.equal(agent.remoteConfig(config(sw)).status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  assert.equal(agent.controls.off(http), true);
  assert.deepEqual(effective(agent), { instrumentations: { disabled: ["http"] } });
  keepApplied(dir, sw); // as the client does, before APPLIED goes out
  const { agent: next, env } = prepared(dir, { OTEL_NODE_DISABLED_INSTRUMENTATIONS: "fs" });
  next.instrument([loaded("http")]);
  assert.equal(next.controls.off(http), true);
  assert.deepEqual(effective(next), { instrumentations: { disabled: ["http"] } });
  // Through the SDK, not the environment: a child process doesn't inherit it.
  assert.equal(env.OTEL_NODE_DISABLED_INSTRUMENTATIONS, "fs");
  // Local again: on again, at once.
  next.remoteConfig(config(policy()));
  assert.equal(next.controls.off(http), false);
  assert.deepEqual(effective(next), {});
});

test("a signal whose exporter NodeSDK builds, ungated, isn't carried out: a policy that overrides it is refused", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const { agent } = prepared(dir, { OTEL_TRACES_EXPORTER: "zipkin" });
  const s = agent.remoteConfig(config(policy({ signal_exports: off("traces", "logs") })));
  // Refused whole: nothing could stop the traces, so nothing changes.
  assert.equal(s.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.equal(s.errorMessage, "signal_exports.traces: this SDK cannot apply the requested override");
  assert.equal(agent.controls.sendTraces, true);
  assert.equal(agent.controls.sendLogs, true);
  assert.deepEqual(effective(agent), {});
  assert.equal(agent.remoteConfig(config(policy({ signal_exports: off("logs") }))).status,
    RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  assert.equal(agent.controls.sendLogs, false);
  assert.deepEqual(undeclared({}), []);
  assert.deepEqual(undeclared({ OTEL_METRICS_EXPORTER: "console,none", OTEL_EXPORTER_OTLP_PROTOCOL: "grpc" }), []);
  // An unknown protocol is http/protobuf, as NodeSDK reads it: built here, so the server can stop it.
  assert.deepEqual(undeclared({ OTEL_EXPORTER_OTLP_LOGS_PROTOCOL: "carrier-pigeon" }), []);
  assert.ok(exportersFromEnv({ OTEL_EXPORTER_OTLP_LOGS_PROTOCOL: "carrier-pigeon" }).logs?.[0] instanceof ProtoLogs);
  assert.deepEqual(undeclared({ OTEL_METRICS_EXPORTER: "otlp,prometheus" }), ["send_metrics"]);
  // What the log gate holds is carried out where send_logs is, so none of it is where send_logs isn't.
  assert.deepEqual(undeclared({ OTEL_LOGS_EXPORTER: "otlp,custom" }), LOG_GATE);
  assert.deepEqual(undeclared({ OTEL_TRACES_EXPORTER: "zipkin" }), ["drop_attributes", "mask_attributes", "send_traces"]);
  const ungated = prepared(mkdtempSync(join(tmpdir(), "opamp-otel-")), { OTEL_LOGS_EXPORTER: "custom" }).agent;
  const level = ungated.remoteConfig(config(policy({ log_levels: { level: "error" } })));
  assert.equal(level.errorMessage, "log_levels: level: this SDK cannot apply the requested override");
  assert.equal(ungated.controls.sendLogsMinSeverity, undefined);
  assert.deepEqual(effective(ungated), {});
});

test("without a state directory every setting is still carried out: none waits for the next start", () => {
  const agent = new Agent();
  agent.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp" });
  assert.equal(agent.remoteConfig(config(policy({ head_sampling: { ratio: 0.5 } }))).status,
    RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  // Instrumentations are switched while the application runs: nothing to keep for a restart.
  assert.equal(agent.remoteConfig(config(policy({ instrumentations: { all_disabled: true } }))).status,
    RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  assert.deepEqual(effective(agent), { instrumentations: { all_disabled: true } });
});

test("one UID names the instance and is kept; without an endpoint nothing changes", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const attrs = "deployment.environment.name=prod,service.instance.id=x";
  const first = prepared(dir, { OTEL_RESOURCE_ATTRIBUTES: attrs });
  assert.match(first.agent.uid!, /^[0-9a-f]{8}-[0-9a-f]{4}-7/);
  // The SDK's resource carries it; the environment, which child processes inherit, doesn't.
  assert.equal(first.env.OTEL_RESOURCE_ATTRIBUTES, attrs);
  const resource = first.agent.resource(resourceFromAttributes({ "service.instance.id": "x", "service.name": "s" }));
  assert.equal(resource.attributes["service.instance.id"], first.agent.uid);
  assert.equal(resource.attributes["service.name"], "s");
  assert.equal(statSync(join(dir, "instance-uid")).mode & 0o777, 0o600);
  assert.equal(prepared(dir).agent.uid, first.agent.uid);
  const env: NodeJS.ProcessEnv = { OTEL_NODE_DISABLED_INSTRUMENTATIONS: "fs" };
  const off = new Agent();
  off.prepare(env);
  assert.equal(off.on, false);
  assert.deepEqual(env, { OTEL_NODE_DISABLED_INSTRUMENTATIONS: "fs" });
});

test("a state directory it can't save in keeps a given UUID, not a new UID each start", () => {
  // Issue #218: no UID can be kept there, so the resource's UUID names the instance at every
  // start, as without a state directory.
  const file = join(mkdtempSync(join(tmpdir(), "opamp-otel-")), "file");
  writeFileSync(file, "");
  const unsaved = join(file, "state");
  const given = "0192f3a4-5b6c-7d8e-9f01-23456789abcd";
  for (let i = 0; i < 2; i++) {
    assert.equal(prepared(unsaved, { OTEL_RESOURCE_ATTRIBUTES: `service.instance.id=${given}` }).agent.uid, given);
  }
  // Nothing given: a new one (there is nowhere to keep it).
  assert.match(prepared(unsaved).agent.uid!, /^[0-9a-f]{8}-[0-9a-f]{4}-7/);
});

test("a UID the server assigns is the one the next start uses", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const { agent } = prepared(dir);
  const assigned = "0192f3a4-5b6c-7d8e-9f01-23456789abcd";
  agent.instanceUidChanged(Uint8Array.from(Buffer.from(assigned.replaceAll("-", ""), "hex")));
  assert.equal(readFileSync(join(dir, "instance-uid"), "utf8"), assigned);
  assert.equal(prepared(dir).agent.uid, assigned);
  // Without a state directory there is nowhere to keep it, and nothing fails.
  const bare = new Agent();
  bare.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp" });
  bare.instanceUidChanged(Uint8Array.from(Buffer.from(assigned.replaceAll("-", ""), "hex")));
});

test("a service.instance.id it can't decode never stops the application: a UID of its own", () => {
  const given = "0192f3a4-5b6c-7d8e-9f01-23456789abcd";
  for (const id of ["%E0%A4", "100%", "%zz"]) {
    const agent = new Agent();
    assert.doesNotThrow(() => agent.prepare({
      OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp",
      OTEL_OPAMP_STATE_DIR: "none",
      OTEL_RESOURCE_ATTRIBUTES: `service.instance.id=${id}`,
    }), id);
    assert.equal(agent.on, true, id);
    assert.match(agent.uid!, /^[0-9a-f]{8}-[0-9a-f]{4}-7/, id);
  }
  // One that decodes to a UUID is still the application's.
  const agent = new Agent();
  agent.prepare({
    OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp",
    OTEL_OPAMP_STATE_DIR: "none",
    OTEL_RESOURCE_ATTRIBUTES: `service.instance.id=${given.replaceAll("-", "%2D")}`,
  });
  assert.equal(agent.uid, given);
});

test("a stop before the client starts: it never starts", async () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const { agent } = prepared(dir, { OTEL_OPAMP_ENDPOINT: "http://127.0.0.1:1/v1/opamp" });
  await agent.stop();
  agent.start(resourceFromAttributes({}));
  assert.equal(agent.client, undefined);
});

test("register after its shutdown, in one process: the same slot, the same UID, and the client starts", async () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const env: NodeJS.ProcessEnv = { OTEL_OPAMP_ENDPOINT: "http://127.0.0.1:1/v1/opamp", OTEL_SERVICE_NAME: "web",
    XDG_STATE_HOME: dir, HOME: dir, LOCALAPPDATA: dir };
  // As register() does: the one process-wide agent, prepared each time.
  const agent = new Agent();
  agent.prepare({ ...env });
  const first = { dir: agent.setup?.stateDir, uid: agent.uid };
  assert.equal(basename(first.dir!), "1");
  agent.start(resourceFromAttributes({ "service.name": "web" }));
  const client = agent.client;
  assert.ok(client);
  await agent.stop(); // register's shutdown
  agent.prepare({ ...env });
  assert.deepEqual({ dir: agent.setup?.stateDir, uid: agent.uid }, first);
  agent.start(resourceFromAttributes({ "service.name": "web" }));
  assert.ok(agent.client, "the client never started again");
  assert.notEqual(agent.client, client);
  await agent.stop();
  // Its slot is free again once it has stopped.
  const next = new Agent();
  next.prepare({ ...env });
  assert.equal(next.setup?.stateDir, first.dir);
  released(next.setup);
});

test("a register right after its shutdown starts from a config applied just before: the shutdown waits for its save", async () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  keepApplied(dir, policy({ head_sampling: { ratio: 0.25 } }));
  const { agent, env } = prepared(dir);
  assert.equal(agent.controls.sampler.samplingRate, 0.25);
  // A config applied just before the shutdown: the client's save of it is still writing when its
  // stop resolves (stop doesn't wait for it), and lands a little later.
  let land: () => void = () => {};
  const landed = new Promise<void>((r) => (land = r));
  agent.client = {
    stop: async () => {
      setTimeout(() => {
        keepApplied(dir, policy({ head_sampling: { ratio: 0.5 } }));
        land();
      }, 50);
    },
    settled: () => landed,
  } as unknown as Client;
  await agent.stop(); // register's shutdown
  agent.prepare(env);
  assert.equal(agent.controls.sampler.samplingRate, 0.5, "the next register started from the config before");
  await agent.stop();
});

test("a register that failed, then another at once: the second keeps the slot the first gave back", async () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const env: NodeJS.ProcessEnv = { OTEL_OPAMP_ENDPOINT: "http://127.0.0.1:1/v1/opamp", OTEL_SERVICE_NAME: "web",
    XDG_STATE_HOME: dir, HOME: dir, LOCALAPPDATA: dir };
  const agent = new Agent();
  assert.throws(() => preparing(agent, new AppSettings({ info: () => {}, error: () => {} }), { ...env }, () => {
    throw new Error("the SDK can't be built");
  }));
  agent.prepare({ ...env });
  const lock = agent.setup!.stateLock!;
  assert.equal(basename(dirname(lock)), "1");
  await new Promise((r) => setImmediate(r)); // the failed one's stop has ended
  assert.ok(existsSync(lock), "the failed register's stop let the new one's slot go");
  await agent.stop();
  assert.equal(existsSync(lock), false);
});

test("OTEL_SDK_DISABLED: the package stays out of the way too", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const agent = new Agent();
  agent.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: dir, OTEL_SDK_DISABLED: " TRUE " });
  assert.equal(agent.on, false);
  assert.throws(() => readFileSync(join(dir, "instance-uid")));
});

test("the settings to start with are the config the client kept applied, else the file kept before", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const rate = () => prepared(dir).agent.controls.sampler.samplingRate;
  // Kept before the client kept the config applied (an earlier release): it carries over.
  writeFileSync(join(dir, "sdk-settings.json"), policy({ head_sampling: { ratio: 0.25 } }));
  assert.equal(rate(), 0.25);
  // Once the client keeps a config applied, that is where a start begins: the old file is ignored.
  keepApplied(dir, policy({ head_sampling: { ratio: 0.5 } }));
  assert.equal(rate(), 0.5);
  // A config applied with no policy: every setting the SDK's own, not the old file's.
  keepApplied(dir, undefined);
  assert.equal(rate(), undefined);
  // A client state that doesn't read: the old file again.
  writeFileSync(join(dir, "opamp-state"), new Uint8Array([0xff, 0xff]));
  assert.equal(rate(), 0.25);
});

/** The process that held the default state directory ends. */
function released(setup: Setup | undefined): void {
  if (setup?.stateLock) release(setup.stateLock);
}

test("by default an instance keeps its UID across restarts", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const env: NodeJS.ProcessEnv = { OTEL_OPAMP_ENDPOINT: "http://s/v1/opamp", OTEL_SERVICE_NAME: "checkout",
    XDG_STATE_HOME: dir, HOME: dir, LOCALAPPDATA: dir };
  const first = new Agent();
  first.prepare({ ...env });
  const state = first.setup!.stateDir!;
  // <service>-<hash>/<slot>, the same as every language's.
  const key = ["checkout", "", programName(), process.cwd()].join("\0");
  assert.equal(basename(dirname(state)), `checkout-${createHash("sha256").update(key).digest("hex").slice(0, 8)}`);
  assert.equal(basename(state), "1");
  if (process.platform !== "win32") assert.equal(statSync(state).mode & 0o777, 0o700);
  // Another process of it, running at once: a slot of its own, so an identity of its own.
  const other = new Agent();
  other.prepare({ ...env });
  assert.notEqual(other.setup!.stateDir, state);
  assert.notEqual(other.uid, first.uid);
  released(first.setup);
  released(other.setup);
  const again = new Agent();
  again.prepare({ ...env });
  assert.equal(again.setup!.stateDir, state);
  assert.equal(again.uid, first.uid);
  released(again.setup);
  // Another service is another instance.
  const elsewhere = new Agent();
  elsewhere.prepare({ ...env, OTEL_SERVICE_NAME: "billing" });
  assert.notEqual(elsewhere.uid, first.uid);
  released(elsewhere.setup);
});

test("a service.instance.id that isn't a UUID keeps the default state directory, so the UID is the same each start", () => {
  // It can't be the instance UID, so it is replaced by one: kept in the default directory, rather
  // than a new one each start.
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const env = { OTEL_OPAMP_ENDPOINT: "http://s", XDG_STATE_HOME: dir, HOME: dir, LOCALAPPDATA: dir,
    OTEL_SERVICE_NAME: "web", OTEL_RESOURCE_ATTRIBUTES: "service.instance.id=web-1" };
  const uids = [0, 1].map(() => {
    const agent = new Agent();
    agent.prepare({ ...env });
    assert.ok(agent.setup?.stateDir?.startsWith(dir), agent.setup?.stateDir);
    released(agent.setup);
    return agent.uid;
  });
  assert.equal(uids[0], uids[1]);
});

test("the default state directory gives way", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const base: NodeJS.ProcessEnv = { OTEL_OPAMP_ENDPOINT: "http://s", XDG_STATE_HOME: dir, HOME: dir, LOCALAPPDATA: dir };
  // Turned off, or a service.instance.id the application chose (a UUID, so the instance UID): none.
  assert.equal(setupFromEnv({ ...base, OTEL_OPAMP_STATE_DIR: "none" })!.stateDir, undefined);
  assert.equal(setupFromEnv({ ...base, OTEL_RESOURCE_ATTRIBUTES: `service.instance.id=${randomUUID()}` })!.stateDir,
    undefined);
  // Named: that one, unlocked as before.
  const named = setupFromEnv({ ...base, OTEL_OPAMP_STATE_DIR: join(dir, "mine") })!;
  assert.equal(named.stateDir, join(dir, "mine"));
  assert.equal(named.stateLock, undefined);
  // Nowhere to keep it: none, and no error.
  assert.equal(setupFromEnv({ OTEL_OPAMP_ENDPOINT: "http://s" })!.stateDir, undefined);
  const file = join(dir, "file");
  writeFileSync(file, "");
  assert.equal(setupFromEnv({ OTEL_OPAMP_ENDPOINT: "http://s", XDG_STATE_HOME: file, HOME: file, LOCALAPPDATA: file })!
    .stateDir, undefined);
  // Every slot taken: none.
  const held = Array.from({ length: SLOTS }, () => setupFromEnv(base));
  assert.ok(held.every((h) => h?.stateDir !== undefined));
  assert.equal(setupFromEnv(base)!.stateDir, undefined);
  held.forEach(released);
});

// The server given in code (register's options) against OTEL_OPAMP_*: for each option, given in
// code only, in its variable only, in both (code wins) and in neither (the default). The other
// options are left out, with an endpoint on the other side where the row isn't the endpoint's.
// service.instance.id is the application's (a UUID), so no default state directory is claimed:
// "neither" is no state directory (the default one has tests of its own above).
const NO_DEFAULT_DIR: NodeJS.ProcessEnv = { OTEL_RESOURCE_ATTRIBUTES: "service.instance.id=0199e3a0-0000-7000-8000-000000000007" };

interface Row {
  option: keyof Options;
  variable: string;
  code: unknown;
  env: string;
  fromCode: unknown;
  fromEnv: unknown;
  neither: unknown;
  read: (s: Setup | undefined) => unknown;
}

const ROWS: Row[] = [
  { option: "endpoint", variable: "OTEL_OPAMP_ENDPOINT", code: " https://code/v1/opamp ", env: "https://env/v1/opamp",
    fromCode: "https://code/v1/opamp", fromEnv: "https://env/v1/opamp", neither: undefined, read: (s) => s?.endpoint },
  { option: "headers", variable: "OTEL_OPAMP_HEADERS", code: { Authorization: "Bearer code" }, env: "Authorization=Bearer%20env,x=1",
    fromCode: { Authorization: "Bearer code" }, fromEnv: { Authorization: "Bearer env", x: "1" }, neither: {}, read: (s) => s?.headers },
  { option: "stateDir", variable: "OTEL_OPAMP_STATE_DIR", code: "/state/code", env: "/state/env",
    fromCode: "/state/code", fromEnv: "/state/env", neither: undefined, read: (s) => s?.stateDir },
  { option: "heartbeatIntervalMs", variable: "OTEL_OPAMP_HEARTBEAT_INTERVAL", code: 1500, env: "5s",
    fromCode: 1500, fromEnv: 5000, neither: 0, read: (s) => s?.heartbeatMs },
];

/** Setup with `option` given in code and/or its variable set: on through the other side when
 * `option` isn't the endpoint. */
function setupWith(row: Row, code: unknown, env: string | undefined): Setup | undefined {
  const e: NodeJS.ProcessEnv = { ...NO_DEFAULT_DIR, ...(env === undefined ? {} : { [row.variable]: env }) };
  const o = (code === undefined ? {} : { [row.option]: code }) as Options;
  if (row.option !== "endpoint") e.OTEL_OPAMP_ENDPOINT = "http://s/v1/opamp";
  return setupFromEnv(e, o);
}

test("each option: in code, in its variable, both (code wins), neither (the default)", () => {
  for (const row of ROWS) {
    const lines = said(() => {
      assert.deepEqual(row.read(setupWith(row, row.code, undefined)), row.fromCode, `${row.option}: code only`);
      assert.deepEqual(row.read(setupWith(row, undefined, row.env)), row.fromEnv, `${row.option}: variable only`);
      assert.deepEqual(row.read(setupWith(row, row.code, row.env)), row.fromCode, `${row.option}: both`);
      assert.deepEqual(row.read(setupWith(row, undefined, undefined)), row.neither, `${row.option}: neither`);
    });
    assert.deepEqual(lines.filter(([level]) => level !== "debug" && level !== "verbose"), [], row.option);
  }
  // All four at once, from either side.
  const env: NodeJS.ProcessEnv = { ...NO_DEFAULT_DIR, ...Object.fromEntries(ROWS.map((r) => [r.variable, r.env])) };
  const code = Object.fromEntries(ROWS.map((r) => [r.option, r.code])) as Options;
  const all = (s: Setup | undefined) => ROWS.map((r) => r.read(s));
  assert.deepEqual(all(setupFromEnv(env, code)), ROWS.map((r) => r.fromCode));
  assert.deepEqual(all(setupFromEnv(env)), ROWS.map((r) => r.fromEnv));
  assert.deepEqual(all(setupFromEnv(env, { endpoint: "http://s/v1/opamp" })).slice(1), ROWS.slice(1).map((r) => r.fromEnv));
});

test("blank in code is not given: the variable, else the default", () => {
  for (const row of ROWS.filter((r) => r.option === "endpoint" || r.option === "stateDir")) {
    for (const blank of ["", "  ", "\t\n"]) {
      assert.deepEqual(row.read(setupWith(row, blank, row.env)), row.fromEnv, `${row.option}: ${JSON.stringify(blank)}`);
      assert.deepEqual(row.read(setupWith(row, blank, undefined)), row.neither, `${row.option}: ${JSON.stringify(blank)}`);
      assert.deepEqual(row.read(setupWith(row, blank, " ")), row.neither, `${row.option}: blank in both`);
    }
  }
  // undefined and null, as plain JavaScript may give them, are left out too.
  for (const row of ROWS) {
    for (const left of [undefined, null]) {
      assert.deepEqual(row.read(setupWith(row, left, row.env)), row.fromEnv, `${row.option}: ${String(left)}`);
    }
  }
});

test("headers in code replace OTEL_OPAMP_HEADERS whole, as given", () => {
  const env: NodeJS.ProcessEnv = { ...NO_DEFAULT_DIR, OTEL_OPAMP_ENDPOINT: "http://s", OTEL_OPAMP_HEADERS: "a=1,b=2" };
  // None of the variable's is merged in; none given is none sent.
  assert.deepEqual(setupFromEnv(env, { headers: { b: "3" } })!.headers, { b: "3" });
  assert.deepEqual(setupFromEnv(env, { headers: {} })!.headers, {});
  // Not percent-decoded: given in code, a value is what is sent; a header HTTP can't carry (a name
  // with spaces) is left out, with a warning.
  const lines = said(() => {
    assert.deepEqual(setupFromEnv(env, { headers: { a: "Bearer%20x", " b ": "y" } })!.headers, { a: "Bearer%20x" });
  });
  assert.deepEqual(lines.filter(([level]) => level === "warn"),
    [["warn", `OpAMP: the headers given to register(): " b " isn't a header HTTP can carry, so it is left out`]]);
  // A copy: the application changing its object later changes nothing.
  const mine = { a: "1" };
  const setup = setupFromEnv(env, { headers: mine })!;
  mine.a = "2";
  assert.deepEqual(setup.headers, { a: "1" });
});

test("stateDir \"none\" in code turns the state directory off, the variable's and the default", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const home: NodeJS.ProcessEnv = { XDG_STATE_HOME: dir, HOME: dir, LOCALAPPDATA: dir };
  for (const none of ["none", "NONE", " None "]) {
    // Over the variable's directory, and over the default one.
    assert.equal(setupFromEnv({ ...home, OTEL_OPAMP_STATE_DIR: join(dir, "env") }, { endpoint: "http://s", stateDir: none })!
      .stateDir, undefined);
    const off = setupFromEnv(home, { endpoint: "http://s", stateDir: none })!;
    assert.deepEqual([off.stateDir, off.stateLock], [undefined, undefined]);
  }
  // The variable's "none" holds when code gives no directory, and gives way when it gives one.
  assert.equal(setupFromEnv({ ...home, OTEL_OPAMP_STATE_DIR: "none" }, { endpoint: "http://s" })!.stateDir, undefined);
  assert.equal(setupFromEnv({ ...home, OTEL_OPAMP_STATE_DIR: "none" }, { endpoint: "http://s", stateDir: join(dir, "code") })!
    .stateDir, join(dir, "code"));
  // Without either, the default state directory: an endpoint in code alone is enough for it.
  const claimed = setupFromEnv(home, { endpoint: "http://s" })!;
  assert.ok(claimed.stateDir?.startsWith(join(dir, "opamp")), claimed.stateDir);
  release(claimed.stateLock!);
});

// A value given in code it can't use: [what, options, the warning, what each reads]. Each warns
// once and never throws; a value of the wrong type is not given (its variable applies, else the
// default); a heartbeat interval that isn't a whole number of milliseconds is the default, as the
// variable's that can't be read.
const ENV: NodeJS.ProcessEnv = {
  ...NO_DEFAULT_DIR,
  OTEL_OPAMP_ENDPOINT: "http://env/v1/opamp",
  OTEL_OPAMP_HEADERS: "a=env",
  OTEL_OPAMP_STATE_DIR: "/state/env",
  OTEL_OPAMP_HEARTBEAT_INTERVAL: "5s",
};
const FROM_ENV = { endpoint: "http://env/v1/opamp", headers: { a: "env" }, stateDir: "/state/env", heartbeatMs: 5000 };
const notMs = (v: string) => `OpAMP: heartbeatIntervalMs ${v} is not a number of milliseconds: it is not used`;
const INVALID: [string, unknown, string | undefined, Partial<Setup>][] = [
  ["an endpoint without a scheme", { endpoint: "opamp.example.com:4320/v1/opamp" },
    'OpAMP: the endpoint given to register() "opamp.example.com:4320/v1/opamp" has no scheme, so it is taken as http://; name it in full, '
      + "as https://host/v1/opamp or wss://host/v1/opamp", { endpoint: "http://opamp.example.com:4320/v1/opamp" }],
  // Its secrets are kept out of the warning.
  ["an endpoint without a scheme, with user info and a query", { endpoint: "user:secret@opamp.example.com/v1/opamp?token=x" },
    'OpAMP: the endpoint given to register() "opamp.example.com/v1/opamp" has no scheme, so it is taken as http://; name it in full, '
      + "as https://host/v1/opamp or wss://host/v1/opamp", { endpoint: "http://user:secret@opamp.example.com/v1/opamp?token=x" }],
  // Kept: the client refuses it when it starts, and the SDK runs unmanaged; nothing stops.
  ["an endpoint with a scheme the client doesn't speak", { endpoint: "ftp://opamp.example.com/v1/opamp" },
    undefined, { endpoint: "ftp://opamp.example.com/v1/opamp" }],
  ["an endpoint that is a number", { endpoint: 4320 }, "OpAMP: endpoint 4320 is not a string: it is not used", {}],
  ["an endpoint that is a URL object", { endpoint: new URL("http://code/v1/opamp") },
    "OpAMP: endpoint http://code/v1/opamp is not a string: it is not used", {}],
  ["headers as a string", { headers: "a=code" }, "OpAMP: headers is not an object of names to values: it is not used", {}],
  ["headers as an array", { headers: [["a", "code"]] }, "OpAMP: headers is not an object of names to values: it is not used", {}],
  ["a header whose value isn't a string", { headers: { a: 1, b: "code" } },
    "OpAMP: headers: the value of 'a' is not a string: that header is not sent", { headers: { b: "code" } }],
  ["a stateDir that is a number", { stateDir: 7 }, "OpAMP: stateDir 7 is not a string: it is not used", {}],
  ["a stateDir that is true", { stateDir: true }, "OpAMP: stateDir true is not a string: it is not used", {}],
  // The core's rule: 0 or less is the default; longer than a day is a day; each with a warning.
  ["a negative heartbeat interval", { heartbeatIntervalMs: -1 },
    "OpAMP: heartbeatIntervalMs (-1 ms) is no interval: using the default", { heartbeatMs: 0 }],
  ["a heartbeat interval past 2^53", { heartbeatIntervalMs: 2 ** 53 },
    "OpAMP: heartbeatIntervalMs is more than a day: a day it is, the longest", { heartbeatMs: 86_400_000 }],
  // A fraction of a millisecond is cut off, as every language's duration in milliseconds.
  ["a fractional heartbeat interval", { heartbeatIntervalMs: 1.5 }, undefined, { heartbeatMs: 1 }],
  // Not a number of milliseconds: not given, as any value of the wrong type.
  ["a NaN heartbeat interval", { heartbeatIntervalMs: NaN }, notMs("NaN"), {}],
  ["an infinite heartbeat interval", { heartbeatIntervalMs: Infinity }, notMs("Infinity"), {}],
  ["a heartbeat interval as a duration string", { heartbeatIntervalMs: "5s" }, notMs('"5s"'), {}],
  ["options that are a string", "http://code/v1/opamp",
    'OpAMP: register\'s options "http://code/v1/opamp" are not an object: using the variables', {}],
  ["options that are a number", 5, "OpAMP: register's options 5 are not an object: using the variables", {}],
  ["options that are null", null, undefined, {}],
];

test("a value in code it can't use warns and is not used; nothing throws", () => {
  for (const [what, options, warning, expected] of INVALID) {
    let setup: Setup | undefined;
    const lines = said(() => {
      setup = setupFromEnv(ENV, options as Options);
    });
    assert.deepEqual(bare(setup), { ...FROM_ENV, ...expected }, what);
    assert.deepEqual(lines.filter(([level]) => level === "warn"), warning === undefined ? [] : [["warn", warning]], what);
    // Without the variables: the default, and on only if an endpoint is left.
    said(() => {
      setup = setupFromEnv(NO_DEFAULT_DIR, { endpoint: "http://code/v1/opamp", ...(options as object) } as Options);
    });
    if (typeof options === "object" && options !== null && "endpoint" in options && !expected.endpoint) {
      assert.equal(setup, undefined, what); // no endpoint left: off
    } else if (typeof options === "object" && options !== null) {
      assert.deepEqual(setup?.headers, expected.headers ?? {}, what);
      assert.equal(setup?.stateDir, expected.stateDir, what);
      assert.equal(setup?.heartbeatMs, expected.heartbeatMs ?? 0, what);
    }
  }
  // Every one together: each warned about, and the agent still starts managed.
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const agent = new Agent();
  const lines = said(() => {
    agent.prepare({ OTEL_OPAMP_ENDPOINT: "http://env/v1/opamp", OTEL_OPAMP_STATE_DIR: dir },
      { endpoint: 1, headers: "x", stateDir: 2, heartbeatIntervalMs: "3" } as unknown as Options);
  });
  assert.equal(agent.on, true);
  assert.equal(agent.setup!.endpoint, "http://env/v1/opamp");
  assert.equal(lines.filter(([level]) => level === "warn").length, 4);
});

test("an endpoint in code turns the package on; OTEL_SDK_DISABLED still turns it off", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const agent = new Agent();
  agent.prepare({}, { endpoint: "http://server/v1/opamp", stateDir: dir });
  assert.equal(agent.on, true);
  assert.equal(agent.setup!.stateDir, dir);
  assert.match(readFileSync(join(dir, "instance-uid"), "utf8"), /^[0-9a-f-]{36}$/);
  // The application's state directory, given in code, keeps the UID across restarts as the variable's.
  const again = new Agent();
  again.prepare({}, { endpoint: "http://server/v1/opamp", stateDir: dir });
  assert.equal(again.uid, agent.uid);
  for (const disabled of ["true", " TRUE "]) {
    const off = new Agent();
    off.prepare({ OTEL_SDK_DISABLED: disabled }, { endpoint: "http://server/v1/opamp" });
    assert.equal(off.on, false);
  }
});

test("a lock left by a process that is gone is taken over; a running one's isn't", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  const env: NodeJS.ProcessEnv = { OTEL_OPAMP_ENDPOINT: "http://s", XDG_STATE_HOME: dir, HOME: dir, LOCALAPPDATA: dir };
  const first = setupFromEnv(env)!;
  released(first);
  const lock = first.stateLock!;
  // Killed, so it never removed its lock.
  const gone = spawnSync(process.execPath, ["-e", ""]).pid!;
  writeFileSync(lock, `${gone} 0`);
  const after = setupFromEnv(env)!;
  assert.equal(after.stateDir, first.stateDir);
  released(after);
  // This pid, from an earlier start (a container's application is often pid 1 each time).
  writeFileSync(lock, `${process.pid} 0`);
  const restarted = setupFromEnv(env)!;
  assert.equal(restarted.stateDir, first.stateDir);
  released(restarted);
  // Another process, running: its slot stays its own.
  writeFileSync(lock, `${process.ppid} 0`);
  const next = setupFromEnv(env)!;
  assert.notEqual(next.stateDir, first.stateDir);
  released(next);
});

test("connecting, failing and recovering are each said once", () => {
  const uid = "00000000-0000-0000-0000-000000000007";
  const lines = said(() => {
    const s = shownFor("https://user:secret@opamp.example.com/v1/opamp?token=x");
    const r = new Reachability(s.shown, "checkout", uid, s.hints);
    r.notified({ kind: "connectFailed", status: 401, detail: "HTTP 401" });
    r.notified({ kind: "connectFailed", status: 401, detail: "HTTP 401" });
    r.notified({ kind: "connected" });
    r.notified({ kind: "connected" });
    r.notified({ kind: "connectFailed", detail: "connection refused" });
    r.notified({ kind: "connected" });
  });
  assert.deepEqual(lines.filter(([level]) => level !== "debug"), [
    ["warn", "OpAMP: can't reach https://opamp.example.com/v1/opamp (HTTP 401): the server refused this agent's "
      + "credentials: check OTEL_OPAMP_HEADERS (Authorization=Bearer <token>). Retrying in the background; the SDK "
      + "keeps its current settings meanwhile"],
    ["info", `OpAMP: connected to https://opamp.example.com/v1/opamp; this SDK is managed as checkout, instance ${uid}`],
    ["warn", "OpAMP: can't reach https://opamp.example.com/v1/opamp (connection refused): check OTEL_OPAMP_ENDPOINT "
      + "and that the server is up. Retrying in the background; the SDK keeps its current settings meanwhile"],
    ["info", "OpAMP: connected to https://opamp.example.com/v1/opamp again"],
  ]);
  const all = lines.map(([, m]) => m).join("\n");
  assert.ok(!all.includes("secret") && !all.includes("token=x"));
});

test("after the server moved the agent, what is said names the endpoint the client uses now", () => {
  let inUse: string | undefined; // none yet: the configured one
  const lines = said(() => {
    const r = new Reachability(shownFor("https://a.example.com/v1/opamp").shown);
    r.inUse = () => inUse;
    r.notified({ kind: "connected" });
    inUse = "https://user:secret@b.example.com/v1/opamp?token=x";
    r.notified({ kind: "connectFailed", detail: "connection refused" });
    r.notified({ kind: "connected" });
  }).filter(([level]) => level !== "debug");
  assert.equal(lines.length, 3, JSON.stringify(lines));
  assert.ok(lines[0]![1].startsWith("OpAMP: connected to https://a.example.com/v1/opamp;"), lines[0]![1]);
  assert.ok(lines[1]![1].startsWith("OpAMP: can't reach https://b.example.com/v1/opamp (connection refused)"), lines[1]![1]);
  assert.equal(lines[2]![1], "OpAMP: connected to https://b.example.com/v1/opamp again");
  const all = lines.map(([, m]) => m).join("\n");
  assert.ok(!all.includes("secret") && !all.includes("token=x"));
});

test("the warning names what gave the server: the option given in code, else the variable", () => {
  const env = { ...NO_DEFAULT_DIR, OTEL_OPAMP_ENDPOINT: "http://env/v1/opamp", OTEL_OPAMP_HEADERS: "a=env" };
  const cases: [Options, string, string][] = [
    [{}, "OTEL_OPAMP_ENDPOINT", "OTEL_OPAMP_HEADERS"],
    // Given in code, each wins over its variable, and the warning names the option.
    [{ endpoint: "http://code/v1/opamp", headers: { Authorization: "Bearer k" } }, "the endpoint given to register()",
      "the headers given to register()"],
    [{ endpoint: "http://code/v1/opamp" }, "the endpoint given to register()", "OTEL_OPAMP_HEADERS"],
    [{ headers: {} }, "OTEL_OPAMP_ENDPOINT", "the headers given to register()"],
  ];
  for (const [options, endpointFrom, headersFrom] of cases) {
    const s = setupFromEnv(env, options)!;
    const lines = said(() => {
      const r = new Reachability(s.shown, "", "u", s.hints);
      for (const status of [401, 404, undefined]) {
        r.notified({ kind: "connectFailed", status, detail: "" });
        r.notified({ kind: "connected" });
      }
    }).filter(([level]) => level === "warn").map(([, m]) => m);
    assert.equal(lines.length, 3, endpointFrom);
    assert.ok(lines[0]!.includes(`check ${headersFrom} (Authorization=Bearer <token>)`), lines[0]);
    assert.ok(lines[1]!.includes(`check ${endpointFrom} (it usually ends in /v1/opamp)`), lines[1]);
    assert.ok(lines[2]!.includes(`check ${endpointFrom} and that the server is up`), lines[2]);
  }
});

test("a bad heartbeat interval warns and is the default; a header with a bad escape is left out", () => {
  for (const bad of ["soon", "-5s", "1h"]) {
    let setup;
    const lines = said(() => {
      setup = setupFromEnv({ OTEL_OPAMP_ENDPOINT: "http://s/v1/opamp", OTEL_OPAMP_HEARTBEAT_INTERVAL: bad });
    });
    assert.deepEqual(bare(setup), { endpoint: "http://s/v1/opamp", headers: {}, heartbeatMs: 0 });
    assert.deepEqual(lines.filter(([level]) => level === "warn"), [["warn", `OpAMP: OTEL_OPAMP_HEARTBEAT_INTERVAL "${bad}" is not a duration (as 1500, `
      + "500ms, 30s or 2m): using the default"]]);
  }
  // Too long to count: a day, the longest, as for one the server offers.
  const long = said(() => {
    assert.equal(setupFromEnv({ OTEL_OPAMP_ENDPOINT: "http://s/v1/opamp", OTEL_OPAMP_HEARTBEAT_INTERVAL: "9".repeat(400) })!.heartbeatMs,
      86_400_000);
  });
  assert.equal(long.filter(([level, m]) => level === "warn" && m.includes("is more than a day")).length, 1);
  let h;
  const lines = said(() => {
    h = setupFromEnv({ OTEL_OPAMP_ENDPOINT: "http://s/v1/opamp", OTEL_OPAMP_HEADERS: "a=x%zzy,b=%,c=%e9,d=ok%21" })!.headers;
  });
  assert.deepEqual(h, { d: "ok!" });
  assert.deepEqual(lines.filter(([level]) => level === "warn"), ["a", "b", "c"].map((k) => ["warn", `OpAMP: OTEL_OPAMP_HEADERS: the value of "${k}" has a `
    + "percent escape that can't be decoded, so it is left out"]));
  // The agent is still on, at the default.
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-"));
  said(() => {
    const { agent } = prepared(dir, { OTEL_OPAMP_HEARTBEAT_INTERVAL: "soon", OTEL_OPAMP_HEADERS: "x=%zz" });
    assert.equal(agent.on, true);
  });
});

test("an endpoint without a scheme is warned about, and taken as http://", () => {
  let setup: Setup | undefined;
  const lines = said(() => {
    setup = setupFromEnv({ OTEL_OPAMP_ENDPOINT: "opamp.example.com:4320/v1/opamp" });
  });
  assert.ok(setup);
  assert.ok(lines.some(([level, m]) => level === "warn" && m.includes("has no scheme")));
  // Taken as http://, as the warning says: the client refuses an endpoint without a scheme.
  assert.equal(setup!.endpoint, "http://opamp.example.com:4320/v1/opamp");
});

test("the endpoint is logged without its secrets, whatever it is", () => {
  assert.equal(shownEndpoint("https://user:secret@opamp.example.com:4320/v1/opamp?token=x#f"),
    "https://opamp.example.com:4320/v1/opamp");
  assert.equal(shownEndpoint("https://user:secret@opamp.example.com:notaport/v1/opamp?token=x"),
    "https://opamp.example.com:notaport/v1/opamp");
  assert.equal(shownEndpoint("https://user:secret@[::1/v1/opamp?k=v"), "https://[::1/v1/opamp");
});

test("without OTEL_LOG_LEVEL, warnings and errors go to the console; info stays on diag", (t) => {
  const warned = t.mock.method(console, "warn", () => {});
  const errored = t.mock.method(console, "error", () => {});
  const before = say.console;
  say.console = true;
  const lines: string[] = [];
  diag.setLogger({ error: () => {}, warn: () => {}, info: (m) => void lines.push(m), debug: () => {}, verbose: () => {} },
    DiagLogLevel.ALL);
  try {
    const s = shownFor("http://opamp.example.com/v1/opamp");
    const r = new Reachability(s.shown, "", "u", s.hints);
    r.notified({ kind: "connectFailed", status: 404, detail: "" });
    r.notified({ kind: "connected" });
    say.error("OpAMP: x");
  } finally {
    say.console = before;
    diag.disable();
  }
  assert.deepEqual(warned.mock.calls.map((c) => c.arguments), [[
    "OpAMP: can't reach http://opamp.example.com/v1/opamp (HTTP 404): nothing serves OpAMP at that path: check "
      + "OTEL_OPAMP_ENDPOINT (it usually ends in /v1/opamp). Retrying in the background; the SDK keeps its current "
      + "settings meanwhile"]]);
  assert.deepEqual(errored.mock.calls.map((c) => c.arguments), [["OpAMP: x"]]);
  assert.deepEqual(lines, ["OpAMP: connected to http://opamp.example.com/v1/opamp; this SDK is managed, instance u"]);
});
