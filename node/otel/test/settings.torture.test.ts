// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The live settings flipped as fast as they come, while the application keeps emitting: one
// applier puts a seeded sequence of documents in force through the agent (rules that overlap and
// change order, caps appearing, disappearing and changing, levels per scope, redaction lists
// growing and shrinking, send_* on and off, debug windows opening and ending on the agent's timer,
// and the empty document), while workers emit spans (roots, children, remote parents, attributes,
// events, links) and log records through the real SDK pipeline (batch and simple processors) into
// exporters that answer later, as a network would.
//
// Node.js runs JavaScript on one thread: nothing here runs at the same time as anything else.
// What can interleave is everything between two awaits and every timer: a span started under one
// document and ended under another, a batch processor's timer flushing between two documents, an
// exporter answering after the next document is in force. So the workers and the applier yield
// to each other at random (a microtask, setImmediate, a timer), the processors flush on 1 ms
// timers in small batches, and the applier also flips the settings from inside the pipeline: in a
// span processor's onEnd, in a log processor's onEmit, and inside the exporter's export call,
// after the gate has decided the batch and before it answers. A seed replays the documents and
// the workload; the interleaving also rests on the timers' real time, so a replay retraces it
// closely, not exactly.
//
// What holds whatever the interleaving:
// - nothing throws, nothing is reported to the SDK's error handler or diag at warn or above, and
//   the run ends (bounded by a timeout: a promise never settled is a hang);
// - an attribute key a drop pattern of every document matches is never exported, on a span, an
//   event, a link or a log record; a key a mask pattern of every document matches, and no drop
//   pattern of any, is only ever exported as "[REDACTED]" (the "redacted" mode makes such keys);
// - an attribute key a drop pattern of every document that sends the signal matches is never
//   exported in that signal (#343): a quiet document sends neither traces nor logs and drops
//   nothing in particular, and in the "redacted" and "logs off" modes every other document drops
//   card.*, so an export held to a quiet document's redaction and another's send_* gives card.cvv
//   away (the gates read the two at once; the .NET gates once read them apart);
// - a child of a sampled local parent is never refused (by the cap or anything else): it is
//   sampled as its parent is;
// - with send_logs false in every document, no log record is exported (the "logs off" mode);
// - each export call (a batch) is as one set of settings in force decides it: every span's
//   attributes, events' and links' redacted by the same lists, every log record passing the same
//   levels and redacted by the same lists, and the signal on. So no attribute list, and no batch,
//   is ever redacted half under one document and half under another.
// - after the flipping stops and a final document is put in force, what is emitted from then on
//   is exported exactly as a fresh SDK with only that document would export it: the same spans
//   sampled, the same records kept by the levels and the caps, the same redaction.
//
// What can't be checked here: which root spans a rate or rule samples while documents flip (a
// ratio decision isn't wrong under any of the documents in the run), and how many records a cap
// lets through while it changes (a new n is a full bucket by design, so the counts depend on
// timing); the final phase checks both exactly. The "same n keeps the bucket" rule means the
// final document's caps are values no earlier document has, so that its bucket starts full as a
// fresh SDK's does; and the clock is moved 10 s on before the final phase, so a bucket the
// leftovers of the run drew on is full again.
//
// OPAMP_PROP_RUNS runs per mode (3 by default; the nightly's 1000), from the seed printed;
// OPAMP_PROP_SEED=<seed> OPAMP_PROP_RUNS=1 replays one run (the failing run's seed is in the
// failure).

import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { test } from "node:test";
import { setImmediate as immediate, setTimeout as sleep } from "node:timers/promises";
import { isDeepStrictEqual } from "node:util";

import {
  type Attributes,
  type Context,
  diag,
  DiagLogLevel,
  type Link,
  ROOT_CONTEXT,
  type Span,
  SpanKind,
  trace,
  TraceFlags,
} from "@opentelemetry/api";
import { setGlobalErrorHandler } from "@opentelemetry/core";
import {
  BatchLogRecordProcessor,
  type LogRecordProcessor,
  LoggerProvider,
  type ReadableLogRecord,
  type SdkLogRecord,
} from "@opentelemetry/sdk-logs";
import {
  BasicTracerProvider,
  BatchSpanProcessor,
  type IdGenerator,
  type ReadableSpan,
  SimpleSpanProcessor,
  type SpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import { RemoteConfigStatuses } from "@pydantic/opamp-client";
import { SdkSettings, type Settings } from "@pydantic/opamp-client/sdk";

import { config as policyConfig, type Overrides, policy } from "./policy.js";

import { Agent, Controls, glob, hostFromEnv, MASK } from "../src/index.js";

const LAYER = new SdkSettings();
const text = (s: string) => new TextEncoder().encode(s);
/** The policy with every control local: what the SDK starts with when nothing was kept. */
const EMPTY = text(policy());
/** 2026-10-14T12:00:00Z, the vectors' clock. */
const NOW = 1_791_979_200_000;

/**
 * The policy a server sends for a generated document. The generator names each setting as
 * Settings does, with its value as text. The policy carries each one in its control.
 */
function policyOf(d: Record<string, unknown>): string {
  const list = (v: unknown) => String(v).split(",").map((s) => s.trim());
  const o: Overrides = {};
  if (d.sampling_rate !== undefined) o.head_sampling = { ratio: Number(d.sampling_rate) };
  const signals = Object.fromEntries((["traces", "metrics", "logs"] as const)
    .filter((s) => d[`send_${s}`] !== undefined).map((s) => [s, { enabled: d[`send_${s}`] === "true" }]));
  if (Object.keys(signals).length > 0) o.signal_exports = signals;
  if (d.sampling_rules !== undefined && (d.sampling_rules as object[]).length > 0) o.sampling_rules = { rules: d.sampling_rules };
  if (d.sampling_traces_per_second !== undefined) o.trace_rate_limit = { traces_per_second: Number(d.sampling_traces_per_second) };
  if (d.send_logs_per_second !== undefined) o.log_rate_limit = { records_per_second: Number(d.send_logs_per_second) };
  const levels: Record<string, unknown> = {};
  if (d.send_logs_level !== undefined) levels.level = d.send_logs_level;
  if (d.send_logs_level_by_scope) levels.scopes = Object.fromEntries(list(d.send_logs_level_by_scope).map((e) => e.split("=")));
  if (Object.keys(levels).length > 0) o.log_levels = levels;
  const redaction: Record<string, unknown> = {};
  if (d.drop_attributes !== undefined) redaction.drop = [...new Set(list(d.drop_attributes))];
  if (d.mask_attributes !== undefined) redaction.mask = [...new Set(list(d.mask_attributes))];
  if (Object.keys(redaction).length > 0) o.attribute_redaction = redaction;
  if (d.debug_until !== undefined) {
    o.debug = { until: d.debug_until };
    if (d.debug_sampling_rate !== undefined) o.debug.sampling_ratio = Number(d.debug_sampling_rate);
    if (d.debug_send_logs_level !== undefined) o.debug.log_level = d.debug_send_logs_level;
  }
  return policy(o);
}

const config = (doc: Record<string, unknown>) => policyConfig(policyOf(doc), policyOf(doc));

// --- the seed, and a generator from it ---------------------------------------------------------

const SEED = process.env.OPAMP_PROP_SEED ? Number(process.env.OPAMP_PROP_SEED) : (Date.now() ^ (Math.random() * 2 ** 31)) >>> 0;
const RUNS = Number(process.env.OPAMP_PROP_RUNS ?? 3);

/** mulberry32: small, seeded, good enough to pick documents and workloads by. */
class Rng {
  #s: number;

  constructor(seed: number) {
    this.#s = seed >>> 0;
  }

  next(): number {
    this.#s = (this.#s + 0x6d2b79f5) >>> 0;
    let t = this.#s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 2 ** 32;
  }

  int(lo: number, hi: number): number {
    return lo + Math.floor(this.next() * (hi - lo + 1));
  }

  chance(p: number): boolean {
    return this.next() < p;
  }

  pick<T>(xs: readonly T[]): T {
    return xs[Math.floor(this.next() * xs.length)]!;
  }

  /** Some of `xs`, in an order of their own. */
  some<T>(xs: readonly T[]): T[] {
    const out = xs.filter(() => this.chance(0.4));
    for (let i = out.length - 1; i > 0; i--) {
      const j = this.int(0, i);
      [out[i], out[j]] = [out[j]!, out[i]!];
    }
    return out;
  }
}

// --- the documents -----------------------------------------------------------------------------

/** Rules that overlap: a span named "checkout cart" on /api/ as a server matches four of them. */
const RULES: object[] = [
  { rate: 0, name: "health*" },
  { rate: 1, name: "checkout*" },
  { rate: 0.5, kind: "server" },
  { rate: 0, attributes: { tenant: "t2" } },
  { rate: 1, path: "/api/*" },
  { rate: 0, name: "checkout cart" },
];
const SCOPE_LEVELS = ["app.db*=debug", "app.http=error", "noisy=off", "app*=warn", "*=info", "app.dbpool=trace"];
const LEVELS = ["trace", "debug", "info", "warn", "error"];
/** Redaction patterns that come and go: none of them matches `id`, nor a *password* key. */
const DROP_POOL = ["grp.a*", "grp.b.x", "user.email", "grp.*", "tenant", "GRP.C*"];
const MASK_POOL = ["grp.*", "grp.c*", "user.*", "plain", "tenant", "*.y"];

type Mode = "mixed" | "redacted" | "logs off";

/** What every document of a mode carries: the empty document is this alone. */
const BASE: Record<Mode, { drop?: string[]; mask?: string[]; send_logs?: string }> = {
  mixed: {},
  redacted: { drop: ["secret.*"], mask: ["*password*"] },
  "logs off": { send_logs: "false" },
};

/**
 * A document of the run, at wall time `wall` (debug windows end within 800 ms of it, often before
 * the next document). One in ten of the run's (not the final one) is quiet: it sends neither traces
 * nor logs, and doesn't drop card.*, which every other document of the "redacted" and "logs off"
 * modes drops.
 */
function documentOf(r: Rng, mode: Mode, wall: number, final = false): Record<string, unknown> {
  const base = BASE[mode];
  const d: Record<string, unknown> = {};
  const drop = [...(base.drop ?? [])];
  const mask = [...(base.mask ?? [])];
  const quiet = !final && r.chance(0.1);
  if (!quiet && mode !== "mixed") drop.push("card.*");
  if (!r.chance(0.08)) {
    if (r.chance(0.5)) d.sampling_rate = r.pick(["0", "0.25", "0.5", "1"]);
    if (r.chance(0.5)) d.sampling_rules = r.some(RULES);
    if (r.chance(0.5)) d.sampling_traces_per_second = r.pick(["1", "3", "20"]);
    for (const k of ["send_traces", "send_metrics", "send_logs"]) {
      if (r.chance(0.3)) d[k] = r.pick(["true", "true", "false"]);
    }
    if (r.chance(0.5)) d.send_logs_level = r.pick(LEVELS);
    if (r.chance(0.5)) d.send_logs_level_by_scope = r.some(SCOPE_LEVELS).join(", ");
    if (r.chance(0.4)) d.send_logs_per_second = r.pick(["2", "5", "50"]);
    drop.push(...r.some(DROP_POOL));
    mask.push(...r.some(MASK_POOL));
    if (r.chance(0.25)) {
      d.debug_until = new Date(wall + r.int(1, 800)).toISOString();
      if (r.chance(0.5)) d.debug_sampling_rate = r.pick(["0", "0.5", "1"]);
      if (r.chance(0.5)) d.debug_send_logs_level = r.pick(LEVELS);
    }
  }
  if (base.send_logs) d.send_logs = base.send_logs;
  if (quiet) {
    d.send_traces = "false";
    d.send_logs = "false";
  }
  if (drop.length) d.drop_attributes = drop.join(", ");
  if (mask.length) d.mask_attributes = mask.join(", ");
  return d;
}

// --- the agent, on clocks and a timer the applier moves ----------------------------------------

/** An agent as the register makes one, on the test's clocks and timer, with a client that keeps nothing. */
class FlippedAgent extends Agent {
  wall = NOW;
  mono = 0;
  armed: { fn: () => void; due: number; cleared: boolean }[] = [];
  /** Every Settings put in force since the run's first document. */
  applied: (Settings | undefined)[] = [];
  /** Debug windows the timer ended. */
  ended = 0;

  constructor() {
    super();
    this.now = () => this.wall;
    this.elapsed = () => this.mono;
    this.timers = {
      set: (fn, ms) => {
        const t = { fn: () => ((t.cleared = true), fn()), due: this.mono + ms, cleared: false };
        this.armed.push(t);
        return t;
      },
      clear: (t) => void ((t as { cleared: boolean }).cleared = true),
    };
    this.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: "none" });
    this.client = { setEffectiveConfig: () => {}, setRemoteConfigStatus: () => {}, setHealth: () => {} } as never;
    // The caps refill by the same monotonic clock the agent's timer waits by.
    this.controls.clock = () => this.mono * 1e6;
    const apply = this.controls.apply.bind(this.controls);
    this.controls.apply = (s) => {
      this.applied.push(s);
      apply(s);
    };
  }

  /** A document from the server, which must be applied. */
  send(doc: object): void {
    const status = this.remoteConfig(config(doc as Record<string, unknown>));
    assert.equal(status.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, `${status.errorMessage}: ${JSON.stringify(doc)}`);
  }

  /** Both clocks on by `ms`, and the timer fired if it is due (the end of a debug window). */
  pass(ms: number): void {
    this.wall += ms;
    this.mono += ms;
    const t = this.armed.at(-1);
    if (t && !t.cleared && t.due <= this.mono) {
      const before = this.refreshAt;
      t.fn();
      if (before && !this.refreshAt) this.ended++;
    }
  }
}

// --- the pipeline ------------------------------------------------------------------------------

/** A span as an exporter was handed it, copied at once: what it exported. */
interface SpanOut {
  id: string;
  name: string;
  traceId: string;
  spanId: string;
  parentSpanId: string | undefined;
  sampled: boolean;
  attributes: Attributes;
  events: { name: string; attributes: Attributes | undefined }[];
  links: (Attributes | undefined)[];
}

/** A log record as an exporter was handed it. */
interface LogOut {
  id: string;
  scope: string;
  severity: number | undefined;
  body: unknown;
  attributes: Attributes;
}

const spanOut = (s: ReadableSpan): SpanOut => ({
  id: String(s.attributes.id),
  name: s.name,
  traceId: s.spanContext().traceId,
  spanId: s.spanContext().spanId,
  parentSpanId: s.parentSpanContext?.spanId,
  sampled: (s.spanContext().traceFlags & TraceFlags.SAMPLED) !== 0,
  attributes: { ...s.attributes },
  events: s.events.map((e) => ({ name: e.name, attributes: e.attributes && { ...e.attributes } })),
  links: s.links.map((l) => l.attributes && { ...l.attributes }),
});

const logOut = (r: ReadableLogRecord): LogOut => ({
  id: String(r.attributes.id),
  scope: r.instrumentationScope.name,
  severity: r.severityNumber,
  body: r.body,
  attributes: { ...(r.attributes as Attributes) },
});

type Done = (r: { code: number }) => void;

/**
 * An exporter that copies what it is handed at once, then answers later (a microtask, the next
 * turn or a timer), as a network would; `during` runs inside the export call, before the answer.
 */
class Sink<T, Out> {
  batches: Out[][] = [];
  pending = 0;

  constructor(readonly copy: (x: T) => Out, readonly r: Rng | undefined, readonly during: () => void = () => {}) {}

  export(items: T[], done: Done): void {
    this.batches.push(items.map(this.copy));
    this.during();
    if (!this.r) return done({ code: 0 });
    this.pending++;
    const answer = () => {
      this.pending--;
      done({ code: 0 });
    };
    const how = this.r.int(0, 2);
    if (how === 0) queueMicrotask(answer);
    else if (how === 1) setImmediate(answer);
    else setTimeout(answer, this.r.int(0, 2));
  }

  shutdown(): Promise<void> {
    return Promise.resolve();
  }

  forceFlush(): Promise<void> {
    return Promise.resolve();
  }

  get all(): Out[] {
    return this.batches.flat();
  }
}

/** Ids at random while the documents flip, then from a sequence, the same in both pipelines. */
class Ids implements IdGenerator {
  seq: number | undefined;

  #next(bytes: number): string {
    if (this.seq === undefined) return randomBytes(bytes).toString("hex");
    const x = (BigInt(++this.seq) * 0x9e3779b97f4a7c15n + 0x1234567n) & 0xffffffffffffffffn;
    return x.toString(16).padStart(16, "0").repeat(bytes / 8);
  }

  generateTraceId(): string {
    return this.#next(16);
  }

  generateSpanId(): string {
    return this.#next(8);
  }
}

/** A span processor or a log processor that runs `fn` as each span ends or record is emitted. */
const onEnd = (fn: () => void): SpanProcessor => ({
  onStart: () => {},
  onEnd: () => fn(),
  forceFlush: () => Promise.resolve(),
  shutdown: () => Promise.resolve(),
});
const onEmit = (fn: () => void): LogRecordProcessor => ({
  onEmit: (_r: SdkLogRecord) => fn(),
  forceFlush: () => Promise.resolve(),
  shutdown: () => Promise.resolve(),
});

// --- the workload ------------------------------------------------------------------------------

const NAMES = ["GET", "health check", "checkout cart", "checkout", "work"];
const KINDS = [SpanKind.SERVER, SpanKind.INTERNAL, SpanKind.CLIENT];
const TARGETS = ["/api/orders?q=1", "/health", "/other"];
const SCOPES = ["app", "app.db", "app.dbpool", "app.http", "noisy"];
const SEVERITIES = [0, 1, 5, 9, 13, 17, 21];

/** The attributes everything carries: keys every pattern above matches, or none does. */
function attributesOf(r: Rng, id: string): Attributes {
  return {
    id,
    "secret.token": "s3cr3t",
    "Secret.Key": "k",
    "user.password": "hunter2",
    "DB.Password": 7,
    "grp.a.x": 1,
    "grp.a.y": "y",
    "grp.b.x": true,
    "grp.c.z": [1, 2],
    tenant: r.pick(["t1", "t2"]),
    "user.email": "a@b.c",
    "card.cvv": "123",
    plain: "v",
  };
}

/** What a worker handed the SDK, by its `id`: what each exported span or record started as. */
interface Emitted {
  spans: Map<string, { attributes: Attributes; events: Attributes[]; links: Attributes[] }>;
  logs: Map<string, Attributes>;
}

/** Between two steps of a worker: nothing, a microtask, the next turn, or a timer. */
async function yieldTo(r: Rng): Promise<void> {
  const how = r.int(0, 4);
  if (how === 1) await Promise.resolve();
  else if (how === 2) await immediate();
  else if (how === 3) await sleep(r.int(0, 1));
}

/**
 * One worker: traces of a root, its children and a record or two, yielding between every step.
 * `y` is how it yields: at random while the documents flip, never in the final phase.
 */
async function worker(
  w: string, n: number, r: Rng, tracer: ReturnType<BasicTracerProvider["getTracer"]>, provider: LoggerProvider,
  emitted: Emitted, y: () => Promise<void>,
): Promise<void> {
  let previous: Span | undefined;
  for (let i = 0; i < n; i++) {
    const id = `${w}-${i}`;
    const attributes: Attributes = { ...attributesOf(r, id), "http.target": r.pick(TARGETS) };
    const links: Link[] = [];
    if (previous && r.chance(0.5)) links.push({ context: previous.spanContext(), attributes: attributesOf(r, `${id}-link`) });
    // A caller's context sometimes: sampled or not, remote.
    let parent: Context = ROOT_CONTEXT;
    if (r.chance(0.25)) {
      parent = trace.setSpanContext(ROOT_CONTEXT, {
        traceId: randomOrSeq(r), spanId: "b7ad6b7169203331", traceFlags: r.chance(0.5) ? 1 : 0, isRemote: true,
      });
    }
    const root = tracer.startSpan(r.pick(NAMES), { kind: r.pick(KINDS), attributes, links }, parent);
    const record = { attributes, events: [] as Attributes[], links: links.map((l) => l.attributes!) };
    emitted.spans.set(id, record);
    previous = root;
    await y();
    const event = attributesOf(r, `${id}-event`);
    root.addEvent("started", event);
    record.events.push(event);
    await y();
    const ctx = trace.setSpan(ROOT_CONTEXT, root);
    for (let c = 0, children = r.int(0, 3); c < children; c++) {
      const cid = `${id}-c${c}`;
      const ca = { ...attributesOf(r, cid), "http.target": r.pick(TARGETS) };
      const child = tracer.startSpan(r.pick(NAMES), { kind: r.pick(KINDS), attributes: ca }, ctx);
      // The invariant the cap must keep: a child of a span sampled here is sampled, whatever
      // document is in force by now.
      if (root.spanContext().traceFlags & TraceFlags.SAMPLED) {
        assert.ok(child.spanContext().traceFlags & TraceFlags.SAMPLED, `${cid}: the child of a sampled local parent was refused`);
      }
      const crecord = { attributes: ca, events: [] as Attributes[], links: [] as Attributes[] };
      emitted.spans.set(cid, crecord);
      await y();
      if (r.chance(0.5)) {
        const ce = attributesOf(r, `${cid}-event`);
        child.addEvent("step", ce);
        crecord.events.push(ce);
      }
      child.end();
      await y();
    }
    for (let l = 0, records = r.int(0, 2); l < records; l++) {
      const lid = `${id}-l${l}`;
      const la = attributesOf(r, lid);
      emitted.logs.set(lid, la);
      provider.getLogger(r.pick(SCOPES)).emit({ severityNumber: r.pick(SEVERITIES), body: lid, attributes: la, context: ctx });
      await y();
    }
    root.end();
    await y();
  }
}

/** A remote caller's trace id: drawn from the run's generator, so the final phase's are the same twice. */
function randomOrSeq(r: Rng): string {
  return Array.from({ length: 4 }, () => r.int(0, 0xffffffff).toString(16).padStart(8, "0")).join("");
}

// --- the checks --------------------------------------------------------------------------------

/** One set of settings as the gates carry it out: a Controls with only it applied. */
function gateOf(s: Settings | undefined): Controls {
  const c = new Controls();
  c.apply(s);
  return c;
}

/** The distinct settings in force during the run, as far as the gates are concerned. */
function distinct(applied: (Settings | undefined)[]): Controls[] {
  const seen = new Map<string, Controls>();
  for (const s of applied) {
    const key = JSON.stringify([s?.sendTraces, s?.sendLogs, s?.sendLogsMinSeverity, s?.sendLogsScopeLevels,
      s?.dropAttributes, s?.maskAttributes]);
    if (!seen.has(key)) seen.set(key, gateOf(s));
  }
  return [...seen.values()];
}

const same = (a: unknown, b: unknown) => isDeepStrictEqual(a ?? {}, b ?? {});

/** Whether a batch of spans is as `g` decides each span: the signal on, every list redacted by it. */
function spansAs(g: Controls, batch: SpanOut[], emitted: Emitted): boolean {
  if (!g.sendTraces) return false;
  return batch.every((s) => {
    const e = emitted.spans.get(s.id)!;
    return same(g.redaction.attributes(e.attributes), s.attributes)
      && s.events.length === e.events.length && s.events.every((x, i) => same(g.redaction.attributes(e.events[i]), x.attributes))
      && s.links.length === e.links.length && s.links.every((x, i) => same(g.redaction.attributes(e.links[i]), x));
  });
}

/** Whether a batch of records is as `g` decides each: the signal on, the levels, redaction. */
function logsAs(g: Controls, batch: LogOut[], emitted: Emitted): boolean {
  if (!g.sendLogs) return false;
  return batch.every((l) => g.passes(l.scope, l.severity) && same(g.redaction.attributes(emitted.logs.get(l.id)), l.attributes));
}

/** Every attribute list exported: a span's, its events', its links', a record's. */
function lists(spans: SpanOut[], logs: LogOut[]): Attributes[] {
  return [
    ...spans.flatMap((s) => [s.attributes, ...s.events.map((e) => e.attributes ?? {}), ...s.links.map((l) => l ?? {})]),
    ...logs.map((l) => l.attributes),
  ];
}

const KEYS = Object.keys(attributesOf(new Rng(0), ""));
const lower = (s: string) => s.replace(/[A-Z]+/g, (m) => m.toLowerCase());
const matched = (patterns: readonly string[], key: string) => patterns.some((p) => glob(lower(p), lower(key)));

// --- a run -------------------------------------------------------------------------------------

const WORKERS = 4;
const TRACES = 30;
const FLIPS = 120;
const TIMEOUT_MS = 30_000;

/** One run of a mode from a seed: the flipping, the invariants, then the final document. */
/** What a run went through: so that a change making it go through less is seen. */
interface Reached {
  flips: number;
  /** Flips from inside the pipeline: a processor's onEnd or onEmit, an exporter's export call. */
  inside: number;
  /** Debug windows the agent's timer ended while spans and records were in flight. */
  ended: number;
  spans: number;
  logs: number;
}

async function run(mode: Mode, seed: number): Promise<Reached> {
  const r = new Rng(seed);
  const agent = new FlippedAgent();
  const c = agent.controls;
  // What the SDK reports: an error handed to its handler, or diag at warn and above.
  const reported: string[] = [];
  setGlobalErrorHandler((e) => reported.push(`error handler: ${String(e)}`));
  diag.setLogger({
    error: (m) => reported.push(`diag error: ${m}`), warn: (m) => reported.push(`diag warn: ${m}`),
    info: () => {}, debug: () => {}, verbose: () => {},
  }, { logLevel: DiagLogLevel.WARN, suppressOverrideMessage: true });

  // The documents, and the one applier that puts them in force, from wherever it is called.
  const documents: object[] = [];
  let flipping = true;
  const flip = () => {
    if (!flipping) return;
    const doc = documentOf(r, mode, agent.wall);
    documents.push(doc);
    agent.send(doc);
  };
  let inside = 0;
  const intrude = (p: number) => () => {
    if (!flipping || !r.chance(p)) return;
    inside++;
    flip();
  };

  const ids = new Ids();
  const batchSpans = new Sink<ReadableSpan, SpanOut>(spanOut, r, intrude(0.2));
  const simpleSpans = new Sink<ReadableSpan, SpanOut>(spanOut, r);
  const batchLogs = new Sink<ReadableLogRecord, LogOut>(logOut, r, intrude(0.2));
  const tracing = new BasicTracerProvider({
    sampler: c.sampler,
    idGenerator: ids,
    spanProcessors: [
      onEnd(intrude(0.03)),
      new BatchSpanProcessor(c.spans(batchSpans as never), { maxExportBatchSize: 7, scheduledDelayMillis: 1, maxQueueSize: 100_000 }),
      new SimpleSpanProcessor(c.spans(simpleSpans as never)),
    ],
  });
  const logging = new LoggerProvider({
    processors: [
      onEmit(intrude(0.03)),
      new BatchLogRecordProcessor({ exporter: c.logs(batchLogs as never), maxExportBatchSize: 5, scheduledDelayMillis: 1, maxQueueSize: 100_000 }),
    ],
  });
  const tracer = tracing.getTracer("app");
  const emitted: Emitted = { spans: new Map(), logs: new Map() };

  // The first document before anything is emitted: the run's documents are all that decide it.
  flip();
  agent.applied = [agent.applied.at(-1)];
  const workerRngs = Array.from({ length: WORKERS }, (_, i) => new Rng(seed * 31 + i + 1));
  const workers = workerRngs.map((wr, i) => worker(`w${i}`, TRACES, wr, tracer, logging, emitted, () => yieldTo(wr)));
  const applier = (async () => {
    for (let i = 1; i < FLIPS; i++) {
      flip();
      agent.pass(r.int(0, 300));
      await yieldTo(r);
    }
  })();
  let timer: NodeJS.Timeout | undefined;
  const hang = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`no end after ${TIMEOUT_MS} ms: a hang`)), TIMEOUT_MS);
  });
  try {
    await Promise.race([Promise.all([applier, ...workers]), hang]);
    flipping = false;

    // Everything emitted so far out of the pipeline: the processors flushed, the exporters answered.
    const drain = async () => {
      for (;;) {
        const before = batchSpans.batches.length + batchLogs.batches.length;
        await Promise.race([Promise.all([tracing.forceFlush(), logging.forceFlush()]), hang]);
        while (batchSpans.pending + simpleSpans.pending + batchLogs.pending > 0) await Promise.race([sleep(1), hang]);
        await sleep(3);
        if (batchSpans.batches.length + batchLogs.batches.length === before) return;
      }
    };
    await drain();

    // --- the invariants of the run ---
    const spanBatches = [...batchSpans.batches, ...simpleSpans.batches];
    const allSpans = spanBatches.flat();
    const allLogs = batchLogs.all;
    assert.ok(allSpans.length > 0, "no span exported in the run");
    const gates = distinct(agent.applied);
    for (const batch of spanBatches) {
      assert.ok(gates.some((g) => spansAs(g, batch, emitted)),
        `a batch of spans not as any one document of the run decides it: ${JSON.stringify(batch.map((s) => s.attributes))}`);
    }
    for (const batch of batchLogs.batches) {
      assert.ok(gates.some((g) => logsAs(g, batch, emitted)),
        `a batch of log records not as any one document of the run decides it: ${JSON.stringify(batch)}`);
    }
    const applied = agent.applied;
    const everyDrop = applied.reduce<string[]>((acc, s, i) => (i === 0 ? [...(s?.dropAttributes ?? [])] : acc.filter((p) => s?.dropAttributes.includes(p))), []);
    const everyMask = applied.reduce<string[]>((acc, s, i) => (i === 0 ? [...(s?.maskAttributes ?? [])] : acc.filter((p) => s?.maskAttributes.includes(p))), []);
    const anyDrop = [...new Set(applied.flatMap((s) => s?.dropAttributes ?? []))];
    const dropped = KEYS.filter((k) => matched(everyDrop, k));
    const masked = KEYS.filter((k) => matched(everyMask, k) && !matched(anyDrop, k));
    if (mode === "redacted") {
      // (card.cvv too, in a run without a quiet document.)
      assert.deepEqual([dropped.filter((k) => k !== "card.cvv").sort(), masked.sort()],
        [["Secret.Key", "secret.token"], ["DB.Password", "user.password"]]);
    }
    for (const list of lists(allSpans, allLogs)) {
      for (const k of dropped) assert.ok(!(k in list), `${k}, dropped by every document, exported: ${JSON.stringify(list)}`);
      for (const k of masked) assert.equal(list[k], MASK, `${k}, masked by every document: ${JSON.stringify(list)}`);
    }
    // A key every document that sends the signal drops: never in it, whatever a document that
    // doesn't send it keeps.
    const droppedBySenders = (sends: (s: Settings | undefined) => boolean) => {
      const senders = applied.filter(sends);
      return KEYS.filter((k) => senders.every((s) => matched(s?.dropAttributes ?? [], k)));
    };
    const bySpanSenders = droppedBySenders((s) => s?.sendTraces ?? true);
    const byLogSenders = droppedBySenders((s) => s?.sendLogs ?? true);
    if (mode !== "mixed" && applied.some((s) => s?.sendTraces === false && s.sendLogs === false)) {
      assert.ok(bySpanSenders.includes("card.cvv") && !dropped.includes("card.cvv"), JSON.stringify(bySpanSenders));
    }
    for (const list of lists(allSpans, [])) {
      for (const k of bySpanSenders) assert.ok(!(k in list), `${k}, dropped by every document that sends traces, exported in a span: ${JSON.stringify(list)}`);
    }
    for (const list of lists([], allLogs)) {
      for (const k of byLogSenders) assert.ok(!(k in list), `${k}, dropped by every document that sends logs, exported in a log record: ${JSON.stringify(list)}`);
    }
    if (mode === "logs off") assert.equal(allLogs.length, 0, "a log record exported with send_logs false in every document");

    // --- the final document ---
    // Its caps are values no document of the run had: a new n, so a full bucket, as a fresh SDK's.
    const final = documentOf(r, mode, agent.wall, true);
    if ("sampling_traces_per_second" in final || r.chance(0.5)) final.sampling_traces_per_second = "7";
    if ("send_logs_per_second" in final || r.chance(0.5)) final.send_logs_per_second = "11";
    // A debug window that outlasts the final phase: the clock moves on 10 s, and the timer that
    // would end it isn't fired, so a fresh SDK decided at the same time is in it too.
    if ("debug_until" in final) final.debug_until = new Date(agent.wall + 3_600_000).toISOString();
    documents.push(final);
    agent.send(final);
    // What was emitted before it is out: from here on, only what follows it.
    await drain();
    // On 10 s: a bucket the run's leftovers drew on is full again. Then the clock stands still.
    agent.wall += 10_000;
    agent.mono += 10_000;
    const marks = [batchSpans.batches.length, simpleSpans.batches.length, batchLogs.batches.length];

    // A fresh SDK, with only the final document: what everything emitted from now on must match.
    const fresh = new Controls();
    fresh.clock = c.clock;
    const freshIds = new Ids();
    const freshSpans = new Sink<ReadableSpan, SpanOut>(spanOut, undefined);
    const freshLogs = new Sink<ReadableLogRecord, LogOut>(logOut, undefined);
    const freshTracing = new BasicTracerProvider({
      sampler: fresh.sampler, idGenerator: freshIds, spanProcessors: [new SimpleSpanProcessor(fresh.spans(freshSpans as never))],
    });
    const freshLogging = new LoggerProvider({
      processors: [new BatchLogRecordProcessor({ exporter: fresh.logs(freshLogs as never), maxExportBatchSize: 5, scheduledDelayMillis: 1 })],
    });
    const d = LAYER.decide(config(final), EMPTY, EMPTY, hostFromEnv({}), BigInt(agent.wall));
    assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, d.status?.errorMessage);
    fresh.apply(d.apply);

    // The same workload through both, one step after another (no yields: the order of the
    // records, which the log cap keeps the first of, is then the same in both).
    const finalSeed = seed ^ 0x5eed;
    const finalEmitted: Emitted = { spans: new Map(), logs: new Map() };
    ids.seq = 0;
    freshIds.seq = 0;
    await worker("final", 40, new Rng(finalSeed), tracer, logging, finalEmitted, () => Promise.resolve());
    await worker("final", 40, new Rng(finalSeed), freshTracing.getTracer("app"), freshLogging, { spans: new Map(), logs: new Map() },
      () => Promise.resolve());
    await drain();
    await freshLogging.forceFlush();
    const byId = <T extends { id: string; spanId?: string }>(xs: T[]) =>
      [...xs].sort((a, b) => (a.id + (a.spanId ?? "")).localeCompare(b.id + (b.spanId ?? "")));
    const after = {
      batch: batchSpans.batches.slice(marks[0]).flat(),
      simple: simpleSpans.batches.slice(marks[1]).flat(),
      logs: batchLogs.batches.slice(marks[2]).flat(),
    };
    for (const x of [...after.batch, ...after.simple, ...after.logs]) {
      assert.ok(x.id.startsWith("final-"), `${x.id}, emitted before the final document, exported after it`);
    }
    const final_ = `final document ${JSON.stringify(final)}`;
    assert.deepEqual(byId(after.batch), byId(freshSpans.all), `spans (batch processor) not as a fresh SDK under the ${final_}`);
    assert.deepEqual(byId(after.simple), byId(freshSpans.all), `spans (simple processor) not as a fresh SDK under the ${final_}`);
    assert.deepEqual(byId(after.logs), byId(freshLogs.all), `log records not as a fresh SDK under the ${final_}`);
    if (mode === "logs off") assert.equal(after.logs.length, 0);

    assert.deepEqual(reported, [], "the SDK reported errors or warnings");
    return { flips: documents.length, inside, ended: agent.ended, spans: allSpans.length, logs: allLogs.length };
  } finally {
    clearTimeout(timer);
    flipping = false;
    diag.disable();
  }
}

for (const mode of ["mixed", "redacted", "logs off"] as const) {
  test(`settings flipped while spans and records are emitted: ${mode} mode`, { timeout: 60_000 + RUNS * 30_000 }, async (t) => {
    t.diagnostic(`OPAMP_PROP_SEED=${SEED} (OPAMP_PROP_RUNS=${RUNS})`);
    const reached: Reached = { flips: 0, inside: 0, ended: 0, spans: 0, logs: 0 };
    for (let i = 0; i < RUNS; i++) {
      const seed = (SEED + i) >>> 0;
      try {
        const got = await run(mode, seed);
        for (const k of Object.keys(reached) as (keyof Reached)[]) reached[k] += got[k];
      } catch (e) {
        const err = e as Error;
        err.message += `\nreplay: OPAMP_PROP_SEED=${seed} OPAMP_PROP_RUNS=1 node --test --test-name-pattern="${mode} mode" `
          + "dist/test/settings.torture.test.js (in node/otel, after tsc -p .)";
        throw err;
      }
    }
    t.diagnostic(`reached: ${JSON.stringify(reached)}`);
    // The run went where it is meant to: flips from inside the pipeline, debug windows the timer
    // ended, spans and (where they're on) records exported.
    assert.ok(reached.inside > 0 && reached.ended > 0 && reached.spans > 0, JSON.stringify(reached));
    if (mode !== "logs off") assert.ok(reached.logs > 0, JSON.stringify(reached));
  });
}
