// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The newer settings (docs/node-otel.md#the-settings-that-match-telemetry), each through this
// package's own sampler and gates: the shared test vectors (testdata/sdk-settings) the
// layer and every language run, then the edge cases each setting's section names, live changes
// and reverting included.

import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { type Attributes, type AttributeValue, type Context, ROOT_CONTEXT, SpanKind, trace } from "@opentelemetry/api";
import { JsonLogsSerializer, JsonTraceSerializer } from "@opentelemetry/otlp-transformer";
import { InMemoryLogRecordExporter, LoggerProvider, SimpleLogRecordProcessor } from "@opentelemetry/sdk-logs";
import {
  AlwaysOffSampler,
  BasicTracerProvider,
  InMemorySpanExporter,
  type Sampler,
  SamplingDecision,
  SimpleSpanProcessor,
  TraceIdRatioBasedSampler,
} from "@opentelemetry/sdk-trace-base";
import { type AgentRemoteConfig, type RemoteConfigStatus, RemoteConfigStatuses } from "@pydantic/opamp-client";
import { SdkSettings, type Settings } from "@pydantic/opamp-client/sdk";

import { keepApplied } from "./kept.js";
import { config as policyConfig, FILE, off, overridden, type Overrides, policy, reported } from "./policy.js";
import { Agent, Controls, glob, HEALTH_EVERY_MS, hostFromEnv, MASK, Redaction, SEVERITY_OFF } from "../src/index.js";

const LAYER = new SdkSettings();
const text = (s: string) => new TextEncoder().encode(s);
/** The policy with every control local: what the SDK starts with when nothing was kept. */
const EMPTY = text(policy());
/** 2026-10-14T12:00:00Z, the vectors' clock. */
const NOW = 1_791_979_200_000;

/**
 * A policy: given whole (text, or a parsed policy with its schema), or as the overrides of a
 * policy with every other control local.
 */
const document = (doc: string | object) =>
  typeof doc === "string" ? doc : "schema" in doc ? JSON.stringify(doc) : policy(doc as Overrides);

/** A remote config whose only file is the policy, its hash the policy's text. */
const config = (doc: string | object) => policyConfig(document(doc), document(doc));

/**
 * The settings this package puts in force for a policy, as a remote config, declared as the
 * package declares them with every exporter its own (hostFromEnv), at `now` (0: no clock).
 */
function settings(doc: string | object, now = 0): Settings {
  const d = LAYER.decide(config(doc), EMPTY, EMPTY, hostFromEnv({}), BigInt(now));
  assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, d.status?.errorMessage);
  return d.apply!;
}

/** Whether the vectors are generated ones (OPAMP_SETTINGS_VECTORS), not the shared ones. */
const RANDOM = !!process.env.OPAMP_SETTINGS_VECTORS;

/** A shared vector file. */
function vectors<T>(name: string): T[] {
  // OPAMP_SETTINGS_VECTORS: a directory of other (generated) cases in place of the shared ones, where it has the file.
  const other = process.env.OPAMP_SETTINGS_VECTORS;
  const path = other && existsSync(`${other}/${name}.json`)
    ? `${other}/${name}.json`
    : new URL(`../../../../testdata/sdk-settings/${name}.json`, import.meta.url);
  return (JSON.parse(readFileSync(path, "utf8")) as { cases: T[] }).cases;
}

type Done = (r: { code: number; error?: Error }) => void;

/** An exporter that keeps what it is handed, and answers `code`. */
function sink<T>(code = 0) {
  const got: T[][] = [];
  return {
    got,
    export: (x: T[], done: Done) => {
      got.push(x);
      done({ code, ...(code ? { error: new Error("refused") } : {}) });
    },
    shutdown: () => Promise.resolve(),
    forceFlush: () => Promise.resolve(),
  };
}

/** A log record as the gate sees one. */
const record = (scope: string, severityNumber?: number, attributes: Record<string, unknown> = {}) =>
  ({ instrumentationScope: { name: scope }, severityNumber, attributes }) as never;

// --- send_logs_level_by_scope ------------------------------------------------------------------

interface LogCase {
  name: string;
  settings: object;
  now_unix_ms?: number;
  record: { scope: string; severity: number };
  kept: boolean;
}

test("vectors: logs.json, each record through the log gate", () => {
  const cases = vectors<LogCase>("logs");
  assert.ok(cases.length > 40);
  for (const c of cases) {
    const controls = new Controls();
    controls.apply(settings(c.settings, c.now_unix_ms ?? 0));
    const out = sink();
    const results: number[] = [];
    controls.logs(out as never).export([record(c.record.scope, c.record.severity)], (r) => results.push(r.code));
    assert.equal(out.got.length === 1 && out.got[0]!.length === 1, c.kept, c.name);
    assert.deepEqual(results, [0], c.name);
  }
});

test("a scope's level, live: lower or higher than send_logs_level, then left out again", () => {
  const c = new Controls();
  const out = sink<{ instrumentationScope: { name: string }; severityNumber?: number }>();
  const logs = c.logs(out as never);
  const batch = [
    record("com.acme.db", 5), record("com.acme.dbpool", 5), record("urllib3", 13), record("noisy.lib", 21),
    record("noisy.lib"), record("app", 9), record("app", 13), record("", 5),
  ];
  const sent = () => out.got.at(-1)!.map((r) => `${r.instrumentationScope.name}:${r.severityNumber ?? 0}`);
  const done = () => {};
  c.apply(settings({ log_levels: { level: "warn", scopes: { "com.acme.db*": "debug", "urllib3*": "error", "noisy.lib": "off" } } }));
  assert.deepEqual(c.sendLogsScopeLevels.map((l) => [l.pattern, l.minSeverity]),
    [["com.acme.db*", 5], ["noisy.lib", SEVERITY_OFF], ["urllib3*", 17]]);
  logs.export(batch, done);
  assert.deepEqual(sent(), ["com.acme.db:5", "com.acme.dbpool:5", "app:13"]);
  // The empty scope name: only * matches it.
  c.apply(settings({ log_levels: { scopes: { "*": "error", "app*": "info" } } }));
  logs.export(batch, done);
  assert.deepEqual(sent(), ["noisy.lib:21", "noisy.lib:0", "app:9", "app:13"]);
  // Left out again: the level alone, then every record.
  c.apply(settings({ log_levels: { level: "warn" } }));
  assert.deepEqual(c.sendLogsScopeLevels, []);
  logs.export(batch, done);
  assert.deepEqual(sent(), ["urllib3:13", "noisy.lib:21", "noisy.lib:0", "app:13"]);
  c.apply(settings({}));
  logs.export(batch, done);
  assert.equal(sent().length, batch.length);
});

test("a scope's level: a switched-off scope sends nothing, and the export still succeeds", () => {
  const c = new Controls();
  const out = sink();
  const results: number[] = [];
  c.apply(settings({ log_levels: { scopes: { noisy: "off" } } }));
  c.logs(out as never).export([record("noisy", 21), record("noisy")], (r) => results.push(r.code));
  assert.deepEqual([out.got.length, results], [0, [0]]);
});

test("send_logs false overrides every scope's level; a scope level on a host that cannot filter logs is refused", () => {
  const c = new Controls();
  const out = sink();
  c.apply(settings({ signal_exports: off("logs"), log_levels: { scopes: { "*": "trace" } } }));
  c.logs(out as never).export([record("app", 21)], () => {});
  assert.equal(out.got.length, 0);
  // Logs NodeSDK exports, ungated: neither send_logs_level nor the scope levels are carried out.
  const host = hostFromEnv({ OTEL_LOGS_EXPORTER: "custom" });
  assert.ok(!host.live!.includes("send_logs_level_by_scope"));
  const d = LAYER.decide(config({ log_levels: { scopes: { a: "off" } } }), EMPTY, EMPTY, host, BigInt(NOW));
  assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.equal(d.status?.errorMessage, "log_levels: scopes: this SDK cannot apply the requested override");
  assert.equal(d.apply, undefined);
});

// --- send_logs_per_second (and, below, sampling_traces_per_second) -----------------------------

interface CapCase {
  name: string;
  settings: object;
  now_unix_ms?: number;
  signal: "traces" | "logs";
  events: { at_ns: number; records?: number; parent?: string; sampled?: boolean }[];
  want: number[];
}

/** Controls under a document, on a clock the test sets. */
function clocked(doc: object, now = 0): { c: Controls; at: (ns: number) => void } {
  let t = 0;
  const c = new Controls();
  c.clock = () => t;
  c.apply(settings(doc, now));
  return { c, at: (ns) => void (t = ns) };
}

test("vectors: caps.json's log batches, through the log gate on the test's clock", () => {
  const cases = vectors<CapCase>("caps").filter((c) => c.signal === "logs");
  assert.ok(cases.length > 10);
  for (const k of cases) {
    const { c, at } = clocked(k.settings, k.now_unix_ms ?? 0);
    const out = sink();
    const logs = c.logs(out as never);
    const passed = k.events.map((e) => {
      at(e.at_ns);
      const before = out.got.length;
      logs.export(Array.from({ length: e.records! }, () => record("app", 9)), () => {});
      return out.got.length > before ? out.got.at(-1)!.length : 0;
    });
    assert.deepEqual(passed, k.want, k.name);
  }
});

test("the log cap: the first records of a batch go on, in order, and the export succeeds", () => {
  const { c } = clocked({ log_rate_limit: { records_per_second: 3 } });
  const out = sink<{ attributes: { i: number } }>();
  const results: number[] = [];
  c.logs(out as never).export(Array.from({ length: 5 }, (_, i) => record("app", 9, { i })), (r) => results.push(r.code));
  assert.deepEqual(out.got[0]!.map((r) => r.attributes.i), [0, 1, 2]);
  c.logs(out as never).export([record("app", 9)], (r) => results.push(r.code));
  assert.deepEqual([out.got.length, results], [1, [0, 0]]);
});

test("the log cap comes after the levels: a record they drop takes no token", () => {
  const { c } = clocked({ log_levels: { level: "warn" }, log_rate_limit: { records_per_second: 2 } });
  const out = sink<{ severityNumber: number }>();
  const logs = c.logs(out as never);
  logs.export([record("app", 9), record("app", 9), record("app", 13), record("app", 9), record("app", 17), record("app", 21)],
    () => {});
  assert.deepEqual(out.got[0]!.map((r) => r.severityNumber), [13, 17]);
});

test("the log cap, live: a new n starts a full bucket, the same n keeps it, left out is no cap at once", () => {
  const { c, at } = clocked({ log_rate_limit: { records_per_second: 2 } });
  const out = sink();
  const logs = c.logs(out as never);
  const send = (n: number) => {
    const before = out.got.length;
    logs.export(Array.from({ length: n }, () => record("app", 9)), () => {});
    return out.got.length > before ? out.got.at(-1)!.length : 0;
  };
  assert.equal(send(5), 2);
  // The same cap sent again (a reconnect): the same bucket, still empty.
  c.apply(settings({ log_rate_limit: { records_per_second: 2 } }));
  assert.equal(send(5), 0);
  // Another n: a full bucket of it.
  c.apply(settings({ log_rate_limit: { records_per_second: 4 } }));
  assert.equal(send(5), 4);
  at(250_000_000); // a quarter of a second: one token back
  assert.equal(send(5), 1);
  c.apply(settings({}));
  assert.equal(c.logsCap, undefined);
  assert.equal(send(5000), 5000);
  // A cap the layer refuses changes nothing: 0 is "turn the logs off".
  const d = LAYER.decide(config({ log_rate_limit: { records_per_second: 0 } }), EMPTY, EMPTY, hostFromEnv({}), 0n);
  assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.equal(d.status!.errorMessage, "log_rate_limit: records_per_second must be a number more than 0, at most 1000000");
});

test("the log cap is declared only where the package builds the log exporters", () => {
  assert.ok(hostFromEnv({}).live!.includes("send_logs_per_second"));
  assert.ok(!hostFromEnv({ OTEL_LOGS_EXPORTER: "otlp,custom" }).live!.includes("send_logs_per_second"));
});

// --- drop_attributes and mask_attributes -------------------------------------------------------

/** A vector's attribute, a key and one typed value. */
type VectorAttribute = { key: string; string?: string; int?: number; bool?: boolean; double?: number; array?: string[] };

function value(a: VectorAttribute): AttributeValue {
  return (a.string ?? a.int ?? a.bool ?? a.double ?? a.array)!;
}

interface RedactCase {
  name: string;
  settings: object;
  attributes: VectorAttribute[];
  want: VectorAttribute[];
}

/**
 * A provider whose spans go to the gate, then `gated`, and also, ungated, to `plain`: the
 * application's other exporter.
 */
function tracing(c: Controls) {
  const gated = new InMemorySpanExporter();
  const plain = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(c.spans(gated)), new SimpleSpanProcessor(plain)],
  });
  return { gated, plain, tracer: provider.getTracer("t") };
}

/** A logger whose records go to the gate, then `gated`, and also, ungated, to `plain`. */
function logging(c: Controls) {
  const gated = new InMemoryLogRecordExporter();
  const plain = new InMemoryLogRecordExporter();
  const provider = new LoggerProvider({
    processors: [new SimpleLogRecordProcessor({ exporter: c.logs(gated) }), new SimpleLogRecordProcessor({ exporter: plain })],
  });
  return { gated, plain, logger: provider.getLogger("app") };
}

test("vectors: redact.json, on a span's, its event's and link's, and a log record's attributes", () => {
  const cases = vectors<RedactCase>("redact");
  let run = 0;
  for (const k of cases) {
    // A JavaScript object holds a key once: a vector that repeats one doesn't apply here.
    if (new Set(k.attributes.map((a) => a.key)).size !== k.attributes.length) continue;
    run++;
    const c = new Controls();
    c.apply(settings(k.settings));
    const input: Attributes = Object.fromEntries(k.attributes.map((a) => [a.key, value(a)]));
    const want = k.want.map((a) => [a.key, value(a)]);
    assert.deepEqual(Object.entries(c.redaction.attributes(input)), want, k.name);
    // Through the SDK, which keeps no attribute with an empty key: nor does the vector then.
    const sdkInput = Object.fromEntries(Object.entries(input).filter(([key]) => key !== ""));
    const sdkWant = want.filter(([key]) => key !== "");
    const { gated, plain, tracer } = tracing(c);
    const link = { context: { traceId: "1".repeat(32), spanId: "2".repeat(16), traceFlags: 1 }, attributes: sdkInput };
    const span = tracer.startSpan("s", { attributes: sdkInput, links: [link] });
    span.addEvent("e", sdkInput);
    span.end();
    const [exported] = gated.getFinishedSpans();
    for (const got of [exported!.attributes, exported!.events[0]!.attributes!, exported!.links[0]!.attributes!]) {
      assert.deepEqual(Object.entries(got), sdkWant, k.name);
    }
    assert.deepEqual(Object.entries(plain.getFinishedSpans()[0]!.attributes), Object.entries(sdkInput), k.name);
    const { gated: logs, plain: plainLogs, logger } = logging(c);
    logger.emit({ severityNumber: 9, body: "b", attributes: sdkInput });
    assert.deepEqual(Object.entries(logs.getFinishedLogRecords()[0]!.attributes), sdkWant, k.name);
    assert.deepEqual(Object.entries(plainLogs.getFinishedLogRecords()[0]!.attributes), Object.entries(sdkInput), k.name);
  }
  // (The generated vectors repeat no key.)
  assert.equal(run, cases.length - (RANDOM ? 0 : 1));
});

test("a redacted span is the span's own otherwise, as the OTLP exporter writes it", () => {
  const c = new Controls();
  c.apply(settings({ attribute_redaction: { drop: ["http.request.header.authorization"], mask: ["*password*", "user.email"] } }));
  const { gated, plain, tracer } = tracing(c);
  const parent = tracer.startSpan("parent");
  const ctx = trace.setSpan(ROOT_CONTEXT, parent);
  const span = tracer.startSpan("GET /login", {
    kind: SpanKind.SERVER,
    attributes: { "http.request.header.authorization": "Bearer x", "user.email": "a@b.c", "DB.Password": 7, keep: "k" },
  }, ctx);
  span.addEvent("login", { "user.email": "a@b.c", ok: true });
  span.setStatus({ code: 2, message: "denied" });
  span.end();
  parent.end();
  const exported = gated.getFinishedSpans().find((s) => s.name === "GET /login")!;
  const original = plain.getFinishedSpans().find((s) => s.name === "GET /login")!;
  assert.notEqual(exported, original);
  assert.deepEqual(exported.attributes, { "user.email": MASK, "DB.Password": MASK, keep: "k" });
  assert.deepEqual(exported.events[0]!.attributes, { "user.email": MASK, ok: true });
  assert.equal(exported.events[0]!.name, "login");
  // The span's own: its context, parent, name, kind, status, times, resource and scope.
  assert.deepEqual(exported.spanContext(), original.spanContext());
  assert.deepEqual(
    [exported.parentSpanContext, exported.name, exported.kind, exported.status, exported.startTime, exported.endTime,
      exported.duration, exported.resource, exported.instrumentationScope, exported.droppedAttributesCount],
    [original.parentSpanContext, original.name, original.kind, original.status, original.startTime, original.endTime,
      original.duration, original.resource, original.instrumentationScope, original.droppedAttributesCount],
  );
  // The application's span is unchanged: another exporter sees the original.
  assert.equal(original.attributes["http.request.header.authorization"], "Bearer x");
  assert.equal(original.attributes["user.email"], "a@b.c");
  // As the OTLP exporter writes it: the masked integer is a string, the dropped key gone.
  const wire = JSON.parse(new TextDecoder().decode(JsonTraceSerializer.serializeRequest([exported])));
  const otlp = wire.resourceSpans[0].scopeSpans[0].spans[0];
  assert.deepEqual(otlp.attributes, [
    { key: "user.email", value: { stringValue: MASK } },
    { key: "DB.Password", value: { stringValue: MASK } },
    { key: "keep", value: { stringValue: "k" } },
  ]);
  assert.equal(otlp.parentSpanId, parent.spanContext().spanId);
});

test("a redacted log record keeps its body, severity and context, as the OTLP exporter writes it", () => {
  const c = new Controls();
  c.apply(settings({ attribute_redaction: { mask: ["user.*"], drop: ["secret"] } }));
  const { gated, plain, logger } = logging(c);
  const span = trace.wrapSpanContext({ traceId: "a".repeat(32), spanId: "b".repeat(16), traceFlags: 1 });
  logger.emit({ severityNumber: 13, severityText: "WARN", body: { "user.email": "in the body" },
    attributes: { "user.email": "a@b.c", "user.id": 7, secret: "s", n: 1 }, context: trace.setSpan(ROOT_CONTEXT, span) });
  const [exported] = gated.getFinishedLogRecords();
  const [original] = plain.getFinishedLogRecords();
  assert.deepEqual(exported!.attributes, { "user.email": MASK, "user.id": MASK, n: 1 });
  assert.deepEqual(original!.attributes, { "user.email": "a@b.c", "user.id": 7, secret: "s", n: 1 });
  // A log body is out of redaction's reach, map bodies too.
  assert.deepEqual(exported!.body, { "user.email": "in the body" });
  const wire = JSON.parse(new TextDecoder().decode(JsonLogsSerializer.serializeRequest([exported!])));
  const otlp = wire.resourceLogs[0].scopeLogs[0].logRecords[0];
  assert.equal(otlp.severityNumber, 13);
  assert.equal(otlp.severityText, "WARN");
  assert.equal(otlp.traceId, "a".repeat(32));
  assert.deepEqual(otlp.attributes.map((a: { key: string }) => a.key), ["user.email", "user.id", "n"]);
  assert.equal(wire.resourceLogs[0].scopeLogs[0].scope.name, "app");
});

test("redaction, live: nothing copied while it is off, on at once, off again when left out", () => {
  const c = new Controls();
  const out = sink<object>();
  const spans = c.spans(out as never);
  const span = { attributes: { token: "t" }, events: [], links: [] } as never;
  spans.export([span], () => {});
  assert.equal(out.got[0]![0], span);
  c.apply(settings({ attribute_redaction: { drop: ["token"] } }));
  spans.export([span], () => {});
  assert.deepEqual((out.got[1]![0] as { attributes: object }).attributes, {});
  c.apply(settings({}));
  assert.equal(c.redaction.on, false);
  spans.export([span], () => {});
  assert.equal(out.got[2]![0], span);
  // Nothing matched: the span itself, not a copy.
  c.apply(settings({ attribute_redaction: { mask: ["password"] } }));
  spans.export([span], () => {});
  assert.equal(out.got[3]![0], span);
});

test("redaction: a key both lists match is dropped; drop doesn't change dropped_attributes_count", () => {
  const r = new Redaction(["token"], ["token", "user"]);
  assert.deepEqual(r.attributes({ token: "t", user: "u", other: 1 }), { user: MASK, other: 1 });
  const c = new Controls();
  c.apply(settings({ attribute_redaction: { drop: ["*"] } }));
  const { gated, tracer } = tracing(c);
  tracer.startSpan("s", { attributes: { a: 1, b: 2 } }).end();
  assert.deepEqual([gated.getFinishedSpans()[0]!.attributes, gated.getFinishedSpans()[0]!.droppedAttributesCount], [{}, 0]);
});

test("redaction is declared only where both the span and the log exporters are this package's", () => {
  for (const env of [{}, { OTEL_LOGS_EXPORTER: "none" }, { OTEL_TRACES_EXPORTER: "console" }]) {
    assert.ok(hostFromEnv(env).live!.includes("drop_attributes"), JSON.stringify(env));
    assert.ok(hostFromEnv(env).live!.includes("mask_attributes"), JSON.stringify(env));
  }
  for (const env of [{ OTEL_LOGS_EXPORTER: "custom" }, { OTEL_TRACES_EXPORTER: "zipkin" }]) {
    const host = hostFromEnv(env);
    assert.ok(!host.live!.includes("drop_attributes") && !host.live!.includes("mask_attributes"), JSON.stringify(env));
    const d = LAYER.decide(config({ attribute_redaction: { mask: ["user.email"] } }), EMPTY, EMPTY, host, 0n);
    assert.equal(d.status?.errorMessage, "attribute_redaction: mask: this SDK cannot apply the requested override");
    assert.equal(d.apply, undefined);
  }
});

test("globs: * alone, leading, trailing, **, the empty pattern, by characters", () => {
  const cases: [string, string, boolean][] = [
    ["*", "", true], ["*", "anything", true], ["", "", true], ["", "x", false],
    ["*cart", "checkout cart", true], ["*cart", "checkout carts", false], ["checkout*", "checkout", true],
    ["a**b", "a-x-b", true], ["a*b*c", "abxbyc", true], ["*ab", "aab", true], ["a?.[b]", "a?.[b]", true],
    ["a?", "ab", false], ["get", "GET", false], ["caf*é *😀", "café 😀", true], ["x😀y", "x😀y", true],
    ["x*y", "x😀y", true], ["x?y", "x😀y", false],
  ];
  for (const [p, t, want] of cases) assert.equal(glob(p, t), want, `${p} ~ ${t}`);
});

// --- sampling_rules and sampling_traces_per_second ---------------------------------------------

interface SamplingCase {
  name: string;
  settings: object;
  now_unix_ms?: number;
  span: { name: string; kind: string; attributes: VectorAttribute[]; parent: string };
  want: { how: "follow_parent" | "rate" | "app_sampler"; sampled?: boolean; rate?: number; rule?: number | null };
}

const KINDS: Record<string, SpanKind> = {
  internal: SpanKind.INTERNAL, server: SpanKind.SERVER, client: SpanKind.CLIENT, producer: SpanKind.PRODUCER,
  consumer: SpanKind.CONSUMER,
};

/** The context a span starts in, by its parent as a vector names it. */
function parented(parent: string, traceId = "0af7651916cd43dd8448eb211c80319c"): Context {
  if (parent === "none") return ROOT_CONTEXT;
  return trace.setSpanContext(ROOT_CONTEXT, {
    traceId, spanId: "b7ad6b7169203331", traceFlags: parent.endsWith("_sampled") ? 1 : 0,
    isRemote: parent.startsWith("remote"),
  });
}

/** The application's own sampler, as one a test can tell apart: it records, without sampling. */
const APP: Sampler = {
  shouldSample: () => ({ decision: SamplingDecision.RECORD, attributes: { "app.sampler": true } }),
  toString: () => "App",
};

/** Trace ids spread over the ratio sampler's range. */
function traceIds(n: number): string[] {
  return Array.from({ length: n }, (_, i) => {
    const x = (BigInt(i) * 0x9e3779b97f4a7c15n + 0x1234567n) & 0xffffffffffffffffn;
    return x.toString(16).padStart(16, "0").repeat(2);
  });
}

test("vectors: sampling.json, each span through the sampler", () => {
  const cases = vectors<SamplingCase>("sampling");
  let run = 0;
  for (const k of cases) {
    const keys = k.span.attributes.map((a) => a.key);
    // A JavaScript object holds a key once, and a number that is an integer is one (1.0 is 1): the
    // vectors with a repeated key or an integral double don't apply here.
    if (new Set(keys).size !== keys.length) continue;
    if (k.span.attributes.some((a) => a.double !== undefined && Number.isInteger(a.double))) continue;
    run++;
    const c = new Controls();
    c.sampler.wrap(APP);
    c.apply(settings(k.settings, k.now_unix_ms ?? 0));
    const attributes: Attributes = Object.fromEntries(k.span.attributes.map((a) => [a.key, value(a)]));
    const kind = KINDS[k.span.kind]!;
    const ctx = parented(k.span.parent);
    const how = c.sampler.explain(ctx, k.span.name, kind, attributes) ?? { how: "app_sampler" };
    const want = k.want.how === "rate" ? { ...k.want, rule: k.want.rule ?? undefined } : k.want;
    assert.deepEqual(how, want, k.name);
    // And what the sampler then decides, for trace ids across the range.
    for (const id of traceIds(16)) {
      const got = c.sampler.shouldSample(parented(k.span.parent, id), id, k.span.name, kind, attributes, []);
      const expected = want.how === "follow_parent"
        ? want.sampled ? SamplingDecision.RECORD_AND_SAMPLED : SamplingDecision.NOT_RECORD
        : want.how === "rate"
          ? new TraceIdRatioBasedSampler(want.rate).shouldSample(ROOT_CONTEXT, id).decision
          : SamplingDecision.RECORD;
      assert.equal(got.decision, expected, `${k.name}: ${id}`);
      if (want.how === "rate" && want.rate === 0) assert.equal(got.decision, SamplingDecision.NOT_RECORD);
      if (want.how === "rate" && want.rate === 1) assert.equal(got.decision, SamplingDecision.RECORD_AND_SAMPLED);
    }
  }
  // Every one but the repeated key and the integral double (the generated vectors draw neither).
  assert.equal(run, cases.length - (RANDOM ? 0 : 2));
});

test("a rule's rate of 0.5 samples half of 10,000 new traces, within 3%", () => {
  const c = new Controls();
  c.sampler.wrap(APP);
  c.apply(settings({ head_sampling: { ratio: 1 }, sampling_rules: { rules: [{ rate: 0.5, kind: "server", path: "/api/*" }] } }));
  let sampled = 0;
  for (let i = 0; i < 10_000; i++) {
    const id = randomBytes(16).toString("hex");
    const r = c.sampler.shouldSample(ROOT_CONTEXT, id, "GET", SpanKind.SERVER, { "url.path": "/api/orders" }, []);
    if (r.decision === SamplingDecision.RECORD_AND_SAMPLED) sampled++;
  }
  assert.ok(Math.abs(sampled / 10_000 - 0.5) < 0.03, `${sampled} of 10000`);
});

test("sampling rules on a real SDK: by path, kind and name; children follow; left out, the SDK's own sampler", () => {
  const c = new Controls();
  const out = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({
    sampler: c.sampler.wrap(new AlwaysOffSampler()),
    spanProcessors: [new SimpleSpanProcessor(c.spans(out))],
  });
  const t = provider.getTracer("t");
  const request = (path: string, name = "GET", kind = SpanKind.SERVER) => {
    const root = t.startSpan(name, { kind, attributes: { "http.target": `${path}?q=1` } }, ROOT_CONTEXT);
    t.startSpan("child", {}, trace.setSpan(ROOT_CONTEXT, root)).end();
    root.end();
  };
  const exported = () => {
    const names = out.getFinishedSpans().map((s) => `${s.name}:${String(s.attributes["http.target"] ?? "")}`);
    out.reset();
    return names;
  };
  // A rule of rate 0 beside sampling_rate 1: health checks go, everything else is traced.
  c.apply(settings({ head_sampling: { ratio: 1 }, sampling_rules: { rules: [{ rate: 0, kind: "server", path: "/health*" }] } }));
  request("/healthz");
  request("/checkout");
  request("/healthz", "GET", SpanKind.CLIENT); // not a server span: the rule doesn't match
  assert.deepEqual(exported(), ["child:", "GET:/checkout?q=1", "child:", "GET:/healthz?q=1"]);
  // The first rule that matches wins over a later, more specific one.
  c.apply(settings({ sampling_rules: { rules: [{ rate: 1, name: "checkout*" }, { rate: 0, name: "checkout cart" }] } }));
  request("/x", "checkout cart", SpanKind.INTERNAL);
  request("/x", "other", SpanKind.INTERNAL); // no rule, no rate: the SDK's own (always off)
  assert.deepEqual(exported(), ["child:", "checkout cart:/x?q=1"]);
  // Left out: the SDK's own sampler for every span.
  c.apply(settings({}));
  request("/checkout");
  assert.deepEqual(exported(), []);
});

test("a caller's decision is followed whatever the rules: sampled under rate 0, unsampled under rate 1", () => {
  const c = new Controls();
  c.sampler.wrap(APP);
  c.apply(settings({ sampling_rules: { rules: [{ rate: 0 }] } }));
  const id = "0af7651916cd43dd8448eb211c80319c";
  const decide = (parent: string) => c.sampler.shouldSample(parented(parent), id, "GET", SpanKind.SERVER, {}, []).decision;
  assert.equal(decide("remote_sampled"), SamplingDecision.RECORD_AND_SAMPLED);
  assert.equal(decide("none"), SamplingDecision.NOT_RECORD);
  c.apply(settings({ sampling_rules: { rules: [{ rate: 1 }] } }));
  assert.equal(decide("remote_unsampled"), SamplingDecision.NOT_RECORD);
  assert.equal(decide("local_unsampled"), SamplingDecision.NOT_RECORD);
  assert.equal(decide("none"), SamplingDecision.RECORD_AND_SAMPLED);
});

test("rules written another way are the same rules, and report the same effective config", () => {
  const host = hostFromEnv({});
  const a = LAYER.decide(config({ sampling_rules: { rules: [{ rate: 0, path: "/health*" }] } }), EMPTY, EMPTY, host, 0n);
  const other = policy({ sampling_rules: { rules: [{ path: "/health*", rate: 0 }] } }).replace('"rate":0}', '"rate":0.0}');
  const b = LAYER.decide(config(other), EMPTY, EMPTY, host, 0n);
  assert.deepEqual(a.apply?.samplingRules.map((r) => [r.rate, r.path]), [[0, "/health*"]]);
  assert.deepEqual(b.apply?.samplingRules.map((r) => [r.rate, r.path]), [[0, "/health*"]]);
  assert.equal(reported(a.effective), reported(b.effective));
  // A bad rule: FAILED, naming it, and the rules in force stay.
  const bad = LAYER.decide(config({ sampling_rules: { rules: [{ rate: 0 }, { rate: 7 }] } }), EMPTY, EMPTY, host, 0n);
  assert.equal(bad.status?.errorMessage, "sampling_rules: rules[1].rate: 7 is not between 0 and 1");
});

test("vectors: caps.json's spans, through the sampler on the test's clock", () => {
  const cases = vectors<CapCase>("caps").filter((k) => k.signal === "traces");
  assert.ok(cases.length > 5);
  for (const k of cases) {
    const { c, at } = clocked(k.settings, k.now_unix_ms ?? 0);
    // The application's sampler says what the event says the rules or the rate decided.
    let sampled = false;
    c.sampler.wrap({ shouldSample: () => ({ decision: sampled ? SamplingDecision.RECORD_AND_SAMPLED : SamplingDecision.NOT_RECORD }) });
    const passed = k.events.map((e, i) => {
      at(e.at_ns);
      sampled = e.sampled!;
      const id = traceIds(k.events.length)[i]!;
      const r = c.sampler.shouldSample(parented(e.parent!, id), id, "s", SpanKind.SERVER, {}, []);
      return r.decision === SamplingDecision.RECORD_AND_SAMPLED ? 1 : 0;
    });
    assert.deepEqual(passed, k.want, k.name);
  }
});

test("the trace cap on a real SDK: whole traces, a local child never refused; removed, no cap at once", () => {
  const c = new Controls();
  let now = 0;
  c.clock = () => now;
  const out = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({
    sampler: c.sampler.wrap(new AlwaysOffSampler()),
    spanProcessors: [new SimpleSpanProcessor(c.spans(out))],
  });
  const t = provider.getTracer("t");
  const traceOf = (n: number) => {
    const root = t.startSpan(`root${n}`, {}, ROOT_CONTEXT);
    for (let i = 0; i < 3; i++) t.startSpan(`child${n}`, {}, trace.setSpan(ROOT_CONTEXT, root)).end();
    root.end();
  };
  c.apply(settings({ head_sampling: { ratio: 1 }, trace_rate_limit: { traces_per_second: 2 } }));
  for (let n = 0; n < 5; n++) traceOf(n);
  assert.deepEqual(out.getFinishedSpans().map((s) => s.name), [
    "child0", "child0", "child0", "root0", "child1", "child1", "child1", "root1",
  ]);
  out.reset();
  // Counted only for spans the rules or rate sampled: a rule of rate 0 takes no token.
  now = 1e9;
  c.apply(settings({ head_sampling: { ratio: 1 }, trace_rate_limit: { traces_per_second: 2 },
    sampling_rules: { rules: [{ rate: 0, name: "root0" }] } }));
  for (let n = 0; n < 4; n++) traceOf(n);
  assert.deepEqual(out.getFinishedSpans().filter((s) => s.name.startsWith("root")).map((s) => s.name), ["root1", "root2"]);
  out.reset();
  c.apply(settings({ head_sampling: { ratio: 1 } }));
  assert.equal(c.sampler.cap, undefined);
  for (let n = 0; n < 50; n++) traceOf(n);
  assert.equal(out.getFinishedSpans().length, 200);
});

// --- debug_until, debug_sampling_rate, debug_send_logs_level -----------------------------------

/** An agent as the register makes one, on clocks and a timer the test sets and fires. */
class TestAgent extends Agent {
  wall = NOW;
  mono = 0;
  armed: { fn: () => void; ms: number; cleared: boolean }[] = [];
  reported: object[] = [];

  constructor(dir: string | undefined, env: NodeJS.ProcessEnv = {}, wall = NOW) {
    super();
    this.wall = wall;
    this.now = () => this.wall;
    this.elapsed = () => this.mono;
    this.timers = {
      set: (fn, ms) => {
        // Fired, it is spent: the agent arms another if it waits for more.
        const t = { fn: () => ((t.cleared = true), fn()), ms, cleared: false };
        this.armed.push(t);
        return t;
      },
      clear: (t) => void ((t as { cleared: boolean }).cleared = true),
    };
    this.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: dir ?? "none", ...env });
    // A client that keeps what it is handed: the effective config, and (below) health.
    this.client = {
      setEffectiveConfig: (e: never) => this.reported.push(overridden(e)),
      setHealth: () => {},
    } as never;
  }

  /**
   * The remote config answered, and, APPLIED with a state directory, kept as the client keeps it
   * (in its state, before APPLIED goes out): this agent has no client, so a restart finds it there.
   */
  override remoteConfig(config: AgentRemoteConfig): RemoteConfigStatus {
    const s = super.remoteConfig(config);
    const dir = this.setup?.stateDir;
    if (s.status === RemoteConfigStatuses.RemoteConfigStatuses_APPLIED && dir !== undefined) {
      keepApplied(dir, config.config?.configMap[FILE]?.body);
    }
    return s;
  }

  /** The timer armed now: the latest, not cleared. */
  get timer() {
    const t = this.armed.at(-1);
    return t && !t.cleared ? t : undefined;
  }

  /** Both clocks forward by `ms`, and the timer fired if it is due by then. */
  pass(ms: number): void {
    this.wall += ms;
    this.mono += ms;
  }

  /** The controls the effective config reports overridden. */
  get settingsInForce(): Record<string, unknown> {
    return overridden(this.effective);
  }
}

interface DebugCase {
  name: string;
  steps: {
    call: "decide" | "load" | "refresh";
    document?: string;
    save?: "done" | "nowhere" | "failed";
    now_unix_ms: number;
    want: {
      status?: string;
      error?: string;
      sampling_rate?: number | null;
      refresh_at_unix_ms?: number;
      effective?: string;
    };
  }[];
}

// saving.json's cases whose save fails are the layer's: here the client keeps the config applied,
// and its save failing holds APPLIED back rather than refusing the config (the client's tests).
test("vectors: debug.json and saving.json, step by step through the agent: decide, a restart, the timer's refresh", () => {
  const all = [...vectors<DebugCase>("debug"), ...vectors<DebugCase>("saving")]
    .filter((k) => !k.steps.some((s) => s.save === "failed"));
  assert.ok(all.length >= 13);
  // A next-start change is another SDK's case: this one carries every setting out at once, the
  // instrumentation switches too, so nothing it applies waits for a restart.
  const cases = all.filter((k) => !k.steps.some((s) => s.document?.includes('"instrumentations":{"mode":"override"')));
  for (const k of cases) {
    const nowhere = k.steps.some((s) => s.save === "nowhere");
    const dir = nowhere ? undefined : mkdtempSync(join(tmpdir(), "opamp-debug-"));
    let agent = new TestAgent(dir, {}, k.steps[0]!.now_unix_ms);
    for (const [i, step] of k.steps.entries()) {
      const what = `${k.name}, step ${i + 1} (${step.call})`;
      let status: number | undefined;
      let error = "";
      if (step.call === "load") {
        agent = new TestAgent(dir, {}, step.now_unix_ms);
      } else {
        agent.wall = step.now_unix_ms;
        if (step.call === "decide") {
          const s = agent.remoteConfig(config(step.document!));
          status = s.status;
          error = s.errorMessage;
        } else agent.refresh();
      }
      const w = step.want;
      if (w.status !== undefined) {
        assert.equal(status, w.status === "applied" ? RemoteConfigStatuses.RemoteConfigStatuses_APPLIED
          : RemoteConfigStatuses.RemoteConfigStatuses_FAILED, what);
      }
      if (w.error !== undefined) assert.ok(error.startsWith(w.error), `${what}: ${error}`);
      if (w.sampling_rate !== undefined) assert.equal(agent.controls.sampler.samplingRate, w.sampling_rate ?? undefined, what);
      if (w.refresh_at_unix_ms !== undefined) {
        assert.equal(agent.refreshAt, w.refresh_at_unix_ms, what);
        // The one timer, armed for the time left, or none.
        const timer = agent.timer;
        if (w.refresh_at_unix_ms === 0) assert.equal(timer, undefined, what);
        else assert.equal(timer?.ms, w.refresh_at_unix_ms - step.now_unix_ms, what);
      }
      if (w.effective !== undefined) assert.equal(reported(agent.effective), w.effective, what);
    }
  }
});

test("debug mode: the timer ends it with no server, puts the earlier settings back and reports them", () => {
  const agent = new TestAgent(mkdtempSync(join(tmpdir(), "opamp-debug-")));
  agent.client = undefined; // the server out of reach: the end comes all the same
  const out = sink<{ instrumentationScope: { name: string }; severityNumber: number }>();
  const logs = agent.controls.logs(out as never);
  const batch = [record("noisy", 5), record("app", 5), record("app", 9), record("app", 13)];
  const until = new Date(NOW + 5000).toISOString();
  agent.remoteConfig(config({
    head_sampling: { ratio: 0.1 }, sampling_rules: { rules: [{ rate: 0, name: "GET" }] },
    log_levels: { level: "warn", scopes: { noisy: "off" } }, debug: { until, log_level: "info" },
  }));
  // In debug mode: the debug rate in place of the rate and rules, the debug level in place of the levels.
  assert.equal(agent.controls.sampler.samplingRate, 1);
  assert.equal(agent.controls.sampler.explain(ROOT_CONTEXT, "GET", SpanKind.SERVER, {})?.how, "rate");
  logs.export(batch, () => {});
  assert.deepEqual(out.got.at(-1)!.map((r) => `${r.instrumentationScope.name}:${r.severityNumber}`), ["app:9", "app:13"]);
  assert.equal(agent.timer?.ms, 5000);
  agent.pass(4999);
  agent.timer!.fn(); // early: nothing due yet, armed again for what is left
  assert.equal(agent.controls.sampler.samplingRate, 1);
  assert.equal(agent.timer?.ms, 1);
  agent.pass(1);
  agent.timer!.fn();
  // Exactly at its end it is over: the rate, the rules and the levels sent before it.
  assert.equal(agent.controls.sampler.samplingRate, 0.1);
  assert.deepEqual(agent.controls.sampler.explain(ROOT_CONTEXT, "GET", SpanKind.SERVER, {}), { how: "rate", rate: 0, rule: 0 });
  logs.export(batch, () => {});
  assert.deepEqual(out.got.at(-1)!.map((r) => `${r.instrumentationScope.name}:${r.severityNumber}`), ["app:13"]);
  assert.equal(agent.timer, undefined);
  assert.equal(agent.refreshAt, 0);
  assert.ok(!("debug" in agent.settingsInForce));
});

test("debug mode: the end reported to the server; the same document again doesn't extend it", () => {
  const agent = new TestAgent(undefined);
  const doc = policy({ head_sampling: { ratio: 0.25 }, debug: { until: new Date(NOW + 60_000).toISOString() } });
  agent.remoteConfig(config(doc));
  assert.equal(agent.timer?.ms, 60_000);
  agent.pass(30_000);
  agent.remoteConfig(config(doc)); // a reconnect: the same window, 30 s left
  assert.equal(agent.timer?.ms, 30_000);
  agent.pass(30_000);
  agent.timer!.fn();
  assert.deepEqual(agent.reported.at(-1), { head_sampling: { ratio: 0.25 } });
  assert.equal(agent.controls.sampler.samplingRate, 0.25);
});

test("debug mode: a timer armed before a newer decision does nothing; a new end replaces the old", () => {
  const agent = new TestAgent(undefined);
  agent.remoteConfig(config({ debug: { until: new Date(NOW + 10_000).toISOString() } }));
  const stale = agent.timer!;
  agent.remoteConfig(config({ debug: { until: new Date(NOW + 20_000).toISOString() } }));
  assert.ok(stale.cleared);
  agent.pass(15_000);
  stale.fn(); // fired anyway (it was on its way): a no-op
  assert.equal(agent.controls.sampler.samplingRate, 1);
  assert.equal(agent.timer?.ms, 20_000);
  // A config with debug local ends debug mode at once, and the timer with it.
  agent.remoteConfig(config({ signal_exports: { logs: { enabled: true } } }));
  assert.equal(agent.controls.sampler.samplingRate, undefined);
  assert.equal(agent.timer, undefined);
});

test("debug mode ends on the monotonic clock: the wall clock moving back doesn't extend it", () => {
  const agent = new TestAgent(undefined);
  agent.remoteConfig(config({ head_sampling: { ratio: 0 }, debug: { until: new Date(NOW + 10_000).toISOString() } }));
  agent.mono += 10_000;
  agent.wall -= 3_600_000; // the wall clock set back an hour meanwhile
  agent.timer!.fn();
  assert.equal(agent.controls.sampler.samplingRate, 0);
  assert.equal(agent.timer, undefined);
});

test("debug mode: a refused config after the wall clock passed its end ends it, as reported", () => {
  const agent = new TestAgent(undefined);
  agent.remoteConfig(config({ head_sampling: { ratio: 0.1 }, debug: { until: new Date(NOW + 10 * 60_000).toISOString() } }));
  assert.equal(agent.controls.sampler.samplingRate, 1);
  // The machine sleeps: the wall clock passes the end, the monotonic one doesn't, so the timer
  // hasn't gone off. Then a bad document comes.
  agent.wall += 15 * 60_000;
  const s = agent.remoteConfig(config({ head_sampling: { ratio: 7 } }));
  assert.equal(s.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.deepEqual(agent.settingsInForce, { head_sampling: { ratio: 0.1 } });
  assert.equal(agent.controls.sampler.samplingRate, 0.1);
  assert.equal(agent.refreshAt, 0);
  assert.equal(agent.timer, undefined);
  assert.deepEqual(agent.reported.at(-1), { head_sampling: { ratio: 0.1 } });
});

test("debug mode: refused more than 24 h ahead, and an end already past is applied but over", () => {
  const agent = new TestAgent(undefined);
  const far = agent.remoteConfig(config({ debug: { until: new Date(NOW + 25 * 3_600_000).toISOString() } }));
  assert.equal(far.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.match(far.errorMessage, /more than 24h ahead/);
  const past = agent.remoteConfig(config({ head_sampling: { ratio: 0.5 }, debug: { until: new Date(NOW - 1000).toISOString() } }));
  assert.equal(past.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  assert.equal(agent.controls.sampler.samplingRate, 0.5);
  assert.equal(agent.timer, undefined);
  assert.deepEqual(agent.settingsInForce, { head_sampling: { ratio: 0.5 } });
});

test("debug mode: the caps stay, and the debug level is refused where the logs are not this package's", () => {
  const agent = new TestAgent(undefined);
  const until = new Date(NOW + 1000).toISOString();
  agent.remoteConfig(config({ log_rate_limit: { records_per_second: 2 }, debug: { until } }));
  assert.equal(agent.controls.logsCap?.perSecond, 2);
  const ungated = new TestAgent(undefined, { OTEL_LOGS_EXPORTER: "custom" });
  const s = ungated.remoteConfig(config({ debug: { until, log_level: "info" } }));
  assert.equal(s.errorMessage, "debug: log_level: this SDK cannot apply the requested override");
  assert.equal(ungated.controls.sampler.samplingRate, undefined);
  // The debug rate alone is this package's to carry out.
  ungated.remoteConfig(config({ debug: { until } }));
  assert.equal(ungated.controls.sendLogsMinSeverity, undefined);
  assert.equal(ungated.controls.sampler.samplingRate, 1);
  assert.deepEqual(ungated.settingsInForce, { debug: { until } });
});

test("a stopped agent arms no timer", async () => {
  const agent = new TestAgent(undefined);
  agent.client = undefined;
  agent.remoteConfig(config({ debug: { until: new Date(NOW + 1000).toISOString() } }));
  const t = agent.timer!;
  await agent.stop();
  assert.ok(t.cleared);
  agent.arm();
  assert.equal(agent.timer, undefined);
});

// --- export health -------------------------------------------------------------------------------

type Signal = "traces" | "metrics" | "logs";

interface HealthWant {
  healthy?: boolean;
  status?: string;
  last_error?: string;
  start_time_unix_nano?: number;
  status_time_unix_nano?: number;
}

interface HealthCase {
  name: string;
  steps: {
    now_unix_ms: number;
    settings?: object;
    counts?: Partial<Record<Signal, Partial<{ exports_ok: number; exports_failed: number; items_failed: number;
      items_filtered: number; last_failed: boolean; last_error: string }>>>;
    want: HealthWant & { send?: boolean; sent_unix_ms?: number;
      signals?: Partial<Record<Signal, HealthWant & { counters?: Record<string, number> }>> };
  }[];
}

/** A ComponentHealth as the vectors give one. */
function checkHealth(got: { healthy: boolean; status: string; lastError: string; startTimeUnixNano: bigint;
  statusTimeUnixNano: bigint } | undefined, want: HealthWant, what: string): void {
  assert.ok(got, what);
  if (want.healthy !== undefined) assert.equal(got.healthy, want.healthy, what);
  if (want.status !== undefined) assert.equal(got.status, want.status, what);
  if (want.last_error !== undefined) assert.equal(got.lastError, want.last_error, what);
  if (want.start_time_unix_nano !== undefined) assert.equal(got.startTimeUnixNano, BigInt(want.start_time_unix_nano), what);
  if (want.status_time_unix_nano !== undefined) assert.equal(got.statusTimeUnixNano, BigInt(want.status_time_unix_nano), what);
}

test("vectors: health.json, call after call through the agent: its gates' counts and settings in force", () => {
  const cases = vectors<HealthCase>("health");
  assert.ok(cases.length >= 14);
  for (const k of cases) {
    const agent = new TestAgent(undefined, {}, k.steps[0]!.now_unix_ms);
    const sent: { componentHealthMap: Record<string, unknown> }[] = [];
    agent.client = { setHealth: (h: never) => sent.push(h), setEffectiveConfig: () => {} } as never;
    for (const [i, step] of k.steps.entries()) {
      const what = `${k.name}, step ${i + 1}`;
      agent.wall = step.now_unix_ms;
      agent.remoteConfig(config(step.settings ?? {}));
      for (const signal of ["traces", "metrics", "logs"] as const) {
        const n = step.counts?.[signal] ?? {};
        Object.assign(agent.controls.counts[signal], {
          exportsOk: n.exports_ok ?? 0, exportsFailed: n.exports_failed ?? 0, itemsFailed: n.items_failed ?? 0,
          itemsFiltered: n.items_filtered ?? 0, lastFailed: n.last_failed ?? false, lastError: n.last_error ?? "",
        });
      }
      const before = sent.length;
      const health = agent.reportHealth();
      const w = step.want;
      // The top level stays the application's; export health is under exporters.
      assert.equal(health.healthy, true, what);
      if (w.send !== undefined) assert.equal(sent.length > before, w.send, what);
      if (w.sent_unix_ms !== undefined) assert.equal(agent.health!.sentUnixMs, BigInt(w.sent_unix_ms), what);
      const exporters = health.componentHealthMap.exporters;
      checkHealth(exporters, w, what);
      for (const [signal, sw] of Object.entries(w.signals ?? {})) {
        const child = exporters!.componentHealthMap[signal];
        checkHealth(child, sw, `${what}: ${signal}`);
        for (const [key, n] of Object.entries(sw.counters ?? {})) {
          const kv = child!.attributes.find((a) => a.key === key);
          assert.deepEqual(kv?.value?.value, { case: "intValue", value: BigInt(n) }, `${what}: ${signal} ${key}`);
        }
      }
    }
  }
});

/** A span exporter that answers each export with the next of `codes`. */
function answering(...codes: number[]) {
  return {
    export: (_x: unknown, done: Done) => {
      const code = codes.shift() ?? 0;
      done(code ? { code, error: new Error("connect ECONNREFUSED 127.0.0.1:4318") } : { code });
    },
    shutdown: () => Promise.resolve(),
    forceFlush: () => Promise.resolve(),
  };
}

test("the gates count each export's result, and what they drop on purpose", () => {
  const c = new Controls();
  const spans = c.spans(answering(0, 1) as never);
  const done = () => {};
  const span = { attributes: {}, events: [], links: [] } as never;
  spans.export([span, span], done);
  spans.export([span, span, span], done);
  assert.deepEqual(c.counts.traces.message, {
    exportsOk: 1n, exportsFailed: 1n, itemsFailed: 3n, itemsFiltered: 0n, lastFailed: true,
    lastError: "connect ECONNREFUSED 127.0.0.1:4318",
  });
  spans.export([span], done); // a success clears the failure
  assert.equal(c.counts.traces.lastFailed, false);
  c.apply(settings({ signal_exports: off("traces") }));
  spans.export([span, span], done);
  assert.deepEqual([c.counts.traces.exportsOk, c.counts.traces.itemsFiltered], [2, 2]);
  // Logs: a level, the cap and send_logs false drop on purpose; redaction drops no record.
  const { c: l } = clocked({ log_levels: { level: "warn" }, log_rate_limit: { records_per_second: 1 },
    attribute_redaction: { mask: ["x"] } });
  const logs = l.logs(answering() as never);
  logs.export([record("app", 9), record("app", 13), record("app", 17)], done);
  assert.deepEqual([l.counts.logs.exportsOk, l.counts.logs.itemsFiltered], [1, 2]);
  logs.export([record("app", 9)], done); // all filtered: not an export
  assert.deepEqual([l.counts.logs.exportsOk, l.counts.logs.itemsFiltered], [1, 3]);
  // Metrics count data points.
  const m = new Controls();
  const metrics = m.metrics({ ...answering(1), selectAggregationTemporality: undefined } as never);
  metrics.export({ resource: {}, scopeMetrics: [{ scope: { name: "a" }, metrics: [{ dataPoints: [1, 2] }, { dataPoints: [3] }] }] } as never, done);
  assert.deepEqual([m.counts.metrics.exportsFailed, m.counts.metrics.itemsFailed], [1, 3]);
  m.apply(settings({ signal_exports: off("metrics") }));
  metrics.export({ resource: {}, scopeMetrics: [{ scope: { name: "a" }, metrics: [{ dataPoints: [1] }] }] } as never, done);
  assert.equal(m.counts.metrics.itemsFiltered, 1);
});

test("export health on the agent's timer: at start, a failure at once, a recovery, nothing while nothing changes", () => {
  const agent = new TestAgent(undefined);
  const health = agent.reportHealth(); // what the client starts with
  assert.equal(health.componentHealthMap.exporters?.status, "ok");
  const sent: { componentHealthMap: { exporters?: { status: string; lastError: string } } }[] = [];
  agent.client = { setHealth: (h: never) => sent.push(h), setEffectiveConfig: () => {} } as never;
  const exporters = () => sent.at(-1)!.componentHealthMap.exporters!;
  agent.watchHealth(); // as start() does once the client runs
  const tick = () => {
    assert.equal(agent.timer?.ms, HEALTH_EVERY_MS);
    agent.pass(HEALTH_EVERY_MS);
    agent.timer!.fn();
  };
  const spans = agent.controls.spans(answering(1, 0) as never);
  const span = { attributes: {}, events: [], links: [] } as never;
  tick();
  assert.equal(sent.length, 0); // nothing changed since start
  spans.export([span], () => {});
  tick();
  assert.equal(exporters().status, "failing");
  assert.match(exporters().lastError, /^traces: connect ECONNREFUSED/);
  tick();
  assert.equal(sent.length, 1); // still failing, the counters still: nothing sent
  spans.export([span], () => {});
  tick();
  assert.equal(exporters().status, "ok");
  // send_traces false: off, not failing, sent at once at the next look.
  agent.remoteConfig(config({ signal_exports: off("traces") }));
  tick();
  const traces = (exporters() as unknown as { componentHealthMap: Record<string, { status: string }> }).componentHealthMap.traces;
  assert.equal(traces!.status, "off");
  // Debug mode's end and health share the one timer: the earlier is what it waits for.
  agent.remoteConfig(config({ debug: { until: new Date(agent.wall + 5000).toISOString() } }));
  assert.equal(agent.timer?.ms, 5000);
  agent.pass(5000);
  agent.timer!.fn();
  assert.equal(agent.timer?.ms, HEALTH_EVERY_MS - 5000);
});
