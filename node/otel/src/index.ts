// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// OpenTelemetry auto-instrumentation for Node.js under an OpAMP server (docs/node-otel.md).
//
// `node --import @pydantic/opamp-otel/register app.js` does what upstream's
// `@opentelemetry/auto-instrumentations-node/register` does (NodeSDK, every auto-instrumentation,
// configured by the OTEL_* variables), and puts the SDK under the server named by
// OTEL_OPAMP_ENDPOINT: a sampler, exporters and instrumentation tracers the server's settings can
// change while it runs, one instance UID. NodeSDK builds its exporters from the environment
// internally, so this builds the ones it knows (OTLP over each protocol, console, none) the same
// way, and wraps them; a signal configured with anything else is left to NodeSDK, and can't be
// stopped by the server.
//
// `appSettings` is the application's side: the settings of its own the server sends (`custom`),
// read by key with a default, and a callback for when they change (docs/app-settings.md).
//
// What the settings mean, what to answer and what to report is the SDK settings layer's
// (`@pydantic/opamp-client/sdk`), the same in every language; this package carries it out on the
// Node.js SDK.

import {
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  statSync,
  unlinkSync,
  writeSync,
} from "node:fs";
import { basename, join } from "node:path";
import { randomBytes } from "node:crypto";

import {
  type AttributeValue,
  type Attributes,
  type Context,
  context,
  diag,
  isSpanContextValid,
  DiagConsoleLogger,
  type Link,
  metrics,
  ProxyTracerProvider,
  type Span,
  SpanKind,
  type SpanOptions,
  trace,
  type Tracer,
  type TracerOptions,
  TraceFlags,
  type TracerProvider,
} from "@opentelemetry/api";
import { logs } from "@opentelemetry/api-logs";
import { getNodeAutoInstrumentations, getResourceDetectors } from "@opentelemetry/auto-instrumentations-node";
import { diagLogLevelFromString, type ExportResult, SDK_INFO, suppressTracing } from "@opentelemetry/core";
import { OTLPLogExporter as GrpcLogs } from "@opentelemetry/exporter-logs-otlp-grpc";
import { OTLPLogExporter as HttpLogs } from "@opentelemetry/exporter-logs-otlp-http";
import { OTLPLogExporter as ProtoLogs } from "@opentelemetry/exporter-logs-otlp-proto";
import { OTLPMetricExporter as GrpcMetrics } from "@opentelemetry/exporter-metrics-otlp-grpc";
import { OTLPMetricExporter as HttpMetrics } from "@opentelemetry/exporter-metrics-otlp-http";
import { OTLPMetricExporter as ProtoMetrics } from "@opentelemetry/exporter-metrics-otlp-proto";
import { OTLPTraceExporter as GrpcTraces } from "@opentelemetry/exporter-trace-otlp-grpc";
import { OTLPTraceExporter as HttpTraces } from "@opentelemetry/exporter-trace-otlp-http";
import { OTLPTraceExporter as ProtoTraces } from "@opentelemetry/exporter-trace-otlp-proto";
import type { Instrumentation } from "@opentelemetry/instrumentation";
import { defaultResource, detectResources, type Resource, resourceFromAttributes } from "@opentelemetry/resources";
import {
  BatchLogRecordProcessor,
  type BatchLogRecordProcessorOptions,
  ConsoleLogRecordExporter,
  type LogRecordExporter,
  type LogRecordProcessor,
  type ReadableLogRecord,
  SimpleLogRecordProcessor,
} from "@opentelemetry/sdk-logs";
import {
  ConsoleMetricExporter,
  type IMetricReader,
  type PushMetricExporter,
  PeriodicExportingMetricReader,
  type PeriodicExportingMetricReaderOptions,
  type ResourceMetrics,
} from "@opentelemetry/sdk-metrics";
import { NodeSDK } from "@opentelemetry/sdk-node";
import {
  AlwaysOffSampler,
  AlwaysOnSampler,
  BatchSpanProcessor,
  ConsoleSpanExporter,
  ParentBasedSampler,
  type ReadableSpan,
  type Sampler,
  SamplingDecision,
  type SamplingResult,
  SimpleSpanProcessor,
  type SpanExporter,
  type SpanProcessor,
  TraceIdRatioBasedSampler,
} from "@opentelemetry/sdk-trace-base";
import {
  AgentCapabilities,
  type AvailableComponents,
  Client,
  type ComponentHealth,
  ComponentHealthSchema,
  create,
  type AnyValue,
  AnyValueSchema,
  KeyValueSchema,
  type AgentRemoteConfig,
  type EffectiveConfig,
  type KeyValue,
  type Notification,
  type RemoteConfigStatus,
  saveState,
  SETUP_LOCK_FILE,
  SETUP_UID_FILE,
  type SetupHints,
  SetupHeaderSchema,
  SetupInCodeSchema,
  SetupOs,
  SetupIdentitySchema,
  SetupStartSchema,
  SetupVariableSchema,
  coreUnavailable,
  identifySetup,
  startSetup,
} from "@pydantic/opamp-client";
import {
  type HealthReport,
  type HostInit,
  type InventoryInit,
  type Loaded,
  type SamplingRule,
  type ScopeLevel,
  SdkSettings,
  type Settings,
  settingsUnavailable,
} from "@pydantic/opamp-client/sdk";

import { AppSettings, appSettings } from "./app-settings.js";
import { asciiLower, globChars, Scrubbing } from "./scrub.js";

export {
  AppSettings,
  type AppSettingsCallback,
  type AppSettingsLog,
  type AppSettingsMap,
  appSettings,
} from "./app-settings.js";

const INSTANCE_ID = "service.instance.id";
const SUCCESS: ExportResult = { code: 0 };

/**
 * The package's own warnings and errors. diag shows nothing until a logger is set, which the
 * register does only for OTEL_LOG_LEVEL: without it they go to the console instead, so a setup
 * that doesn't work is never silent. (Info stays on diag.) `register` sets `console` from its env.
 */
export const say = {
  console: str(process.env, "OTEL_LOG_LEVEL") === undefined,
  warn(message: string): void {
    if (this.console) console.warn(message);
    else diag.warn(message);
  },
  error(message: string): void {
    if (this.console) console.error(message);
    else diag.error(message);
  },
};

/** The application settings' own reports go where the package's do. */
function appLog(): AppSettings["log"] {
  return { info: (m) => diag.info(m), error: (m) => say.error(m) };
}
appSettings.log = appLog();

// The variables as the SDK reads them (@opentelemetry/core's get*FromEnv), from `env`: empty or
// blank is unset.

function str(env: NodeJS.ProcessEnv, key: string): string | undefined {
  const raw = env[key];
  return raw === undefined || raw.trim() === "" ? undefined : raw;
}

function list(env: NodeJS.ProcessEnv, key: string): string[] | undefined {
  return str(env, key)?.split(",").map((v) => v.trim()).filter((v) => v !== "");
}

function num(env: NodeJS.ProcessEnv, key: string): number | undefined {
  const raw = str(env, key);
  if (raw === undefined) return undefined;
  const value = Number(raw);
  if (Number.isNaN(value)) {
    diag.warn(`Unknown value ${JSON.stringify(raw)} for ${key}, expected a number, using defaults`);
    return undefined;
  }
  return value;
}

/** A number above 0, or unset (NodeSDK's getNonNegativeNumberFromEnv). */
function positive(env: NodeJS.ProcessEnv, key: string): number | undefined {
  const value = num(env, key);
  if (value !== undefined && value <= 0) {
    diag.warn(`${key} (${value}) is invalid, expected number greater than 0, using default.`);
    return undefined;
  }
  return value;
}

/** A UUID's 16 bytes as text. */
function text(u: Uint8Array): string {
  const h = Buffer.from(u).toString("hex");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

function uuidBytes(u: string): Uint8Array {
  return Uint8Array.from(Buffer.from(u.replaceAll("-", ""), "hex"));
}

/** An endpoint as it is safe to log (no user info, query or fragment): the core's rule. */
export function shownEndpoint(endpoint: string): string {
  try {
    return startSetup(create(SetupStartSchema, { code: { endpoint } })).shownEndpoint;
  } catch {
    return "the endpoint";
  }
}

/** What to check when the server can't be reached, by what went wrong (the core's hints). */
function hint(hints: SetupHints | undefined, status: number | undefined): string {
  if (status === 401 || status === 403) return hints?.refused ?? "";
  if (status === 404) return hints?.notFound ?? "";
  if (status === undefined) return hints?.unreachable ?? "";
  return hints?.error ?? "";
}

/**
 * Says when the client connects, when it can't (once a run of failures, with what to check), and
 * when it's back: setup that works or doesn't is never silent.
 */
export class Reachability {
  /** The configured endpoint, as it is safe to log. */
  readonly endpoint: string;
  /** The endpoint the client uses now, as the client gives it (unset, or undefined from it: the
   * configured one): after the server moved the agent, the one it moved it to. */
  inUse: (() => string | undefined) | undefined;
  #ever = false;
  #failing = false;

  /** `endpoint`: as it is safe to log; `hints`: the core's, naming what gave the endpoint and
   * the headers. */
  constructor(endpoint: string, readonly service = "", readonly uid = "", readonly hints?: SetupHints) {
    this.endpoint = endpoint;
  }

  /** The endpoint the client uses now, as it is safe to log. */
  at(): string {
    const e = this.inUse?.();
    return e ? shownEndpoint(e) : this.endpoint;
  }

  notified(n: Notification): void {
    const endpoint = this.at();
    if (n.kind === "connected") {
      if (!this.#ever) {
        const who = this.service ? ` as ${this.service}` : "";
        diag.info(`OpAMP: connected to ${endpoint}; this SDK is managed${who}, instance ${this.uid}`);
      } else if (this.#failing) {
        diag.info(`OpAMP: connected to ${endpoint} again`);
      }
      this.#ever = true;
      this.#failing = false;
    } else if (n.kind === "connectFailed") {
      if (!this.#failing) {
        const why = n.detail || `HTTP ${n.status}`;
        say.warn(`OpAMP: can't reach ${endpoint} (${why}): ${hint(this.hints, n.status)}. Retrying in the background; `
          + "the SDK keeps its current settings meanwhile");
      } else {
        diag.debug(`OpAMP: still can't reach ${endpoint}: ${n.detail}`);
      }
      this.#failing = true;
    }
  }
}

/** The program, as a name: the script run, without its extension; `node` when there is none. */
export function programName(argv: readonly string[] = process.argv): string {
  const script = argv[1];
  return (script ? basename(script).replace(/\.[cm]?[jt]s$/, "") : "") || "node";
}

// This process, as its lock files name it: its pid and when it started. A container often gives its
// application the same pid (1) at each start, so a file naming this pid is this process's only if
// it also names this start (within the rounding of the two clocks).
const STARTED = Math.round(Date.now() - process.uptime() * 1000);
const OWN = `${process.pid} ${STARTED}`;
/** The lock files this process made, removed when it exits. */
const LOCKS = new Set<string>();
let releasedAtExit = false;

/** Whether the process a lock file names still runs: if not, the lock is a former process's. */
function holderRuns(content: string, modified: number): boolean {
  const m = /^(\d+) (\d+)$/.exec(content.trim());
  // Made, not yet written (or not ours to read): taken, unless it has been so for long.
  if (!m) return Date.now() - modified < 10_000;
  const pid = Number(m[1]);
  if (pid === process.pid) return Math.abs(Number(m[2]) - STARTED) < 2_000;
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    // EPERM: it runs, as another user.
    return (e as NodeJS.ErrnoException).code !== "ESRCH";
  }
}

/**
 * Take a lock file without waiting: held until the process ends, never by a process it starts.
 * Node.js has no flock (or LockFileEx) in core, and this package takes no native dependency for
 * one, so the lock is the file itself: made exclusively (O_EXCL, atomic on every local file
 * system), naming the process that holds it, removed when it exits. One left by a process that
 * ended without exiting (killed, crashed) names a process no longer running, and is taken over.
 * Unlike the operating system's lock, two processes starting in the same instant can both take
 * over the same stale file; the check that it is still that file narrows that to a moment.
 */
function lock(path: string): boolean {
  for (let attempt = 0; attempt < 2; attempt++) {
    let fd: number;
    try {
      fd = openSync(path, "wx", 0o600);
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "EEXIST") throw e;
      let held: ReturnType<typeof statSync>;
      let content: string;
      try {
        held = statSync(path);
        content = readFileSync(path, "utf8");
      } catch (e2) {
        if ((e2 as NodeJS.ErrnoException).code === "ENOENT") continue; // released meanwhile
        throw e2;
      }
      if (holderRuns(content, held.mtimeMs)) return false;
      try {
        const now = statSync(path);
        if (now.ino === held.ino && now.mtimeMs === held.mtimeMs) unlinkSync(path);
      } catch (e2) {
        if ((e2 as NodeJS.ErrnoException).code !== "ENOENT") throw e2;
      }
      continue;
    }
    try {
      writeSync(fd, OWN);
    } finally {
      closeSync(fd);
    }
    if (!releasedAtExit) process.once("exit", () => LOCKS.forEach(release));
    releasedAtExit = true;
    LOCKS.add(path);
    return true;
  }
  return false;
}

/** Give up a lock `lock` took: the slot is free for the next process. */
export function release(path: string): void {
  LOCKS.delete(path);
  try {
    if (readFileSync(path, "utf8") === OWN) unlinkSync(path);
  } catch {
    // Gone already.
  }
}

/**
 * The first of the default state directory's slots (docs/default-state-dir.md, as the core names
 * them) this process can make and lock: the directory and its lock file, or undefined where
 * there's none to be had (not writable, every slot taken: `allTaken` is the line to say then):
 * then each start is a new instance. Never throws.
 */
function slot(slots: readonly string[], allTaken: string): { dir: string; lock: string } | undefined {
  if (slots.length === 0) return undefined;
  try {
    for (const dir of slots) {
      mkdirSync(dir, { recursive: true, mode: 0o700 });
      const path = join(dir, SETUP_LOCK_FILE);
      if (lock(path)) return { dir, lock: path };
    }
  } catch (e) {
    diag.debug(`OpAMP: no state directory of its own (${String(e)}): each start is a new instance`);
    return undefined;
  }
  diag.info(`OpAMP: ${allTaken}`);
  return undefined;
}

/** Where the server is, from OTEL_OPAMP_*; undefined when the package is off. */
export interface Setup {
  endpoint: string;
  headers: Record<string, string>;
  stateDir?: string;
  heartbeatMs: number;
  /** The lock file on the default state directory, held while the process runs. */
  stateLock?: string;
  /** The endpoint as it is safe to log, and what to check when the server can't be reached
   * (naming what gave the endpoint and the headers): the core's. */
  shown: string;
  hints?: SetupHints;
}

/**
 * The server, given in code: each one given wins over its variable (headers replace
 * OTEL_OPAMP_HEADERS whole); an endpoint turns the package on without OTEL_OPAMP_ENDPOINT. A state
 * directory of "none" turns the default one off, as the variable's. Blank is not given. A value of
 * the wrong type warns and is not given; a heartbeat interval that isn't a whole number of
 * milliseconds warns and is the default. Nothing here throws.
 */
export interface Options {
  endpoint?: string;
  headers?: Record<string, string>;
  stateDir?: string;
  heartbeatIntervalMs?: number;
}

/** A value given in code as JavaScript may give it: what it is, for a warning. */
function shown(value: unknown): string {
  return typeof value === "string" ? JSON.stringify(value) : String(value);
}

/**
 * Options as given in code, each one kept only if it is of its type: one that isn't (as plain
 * JavaScript, or a value read from elsewhere, may give it) warns and is not given, so its variable
 * applies, else the default. Never throws.
 */
function given(options: unknown): Options {
  if (options === undefined || options === null) return {};
  if (typeof options !== "object") {
    say.warn(`OpAMP: register's options ${shown(options)} are not an object: using the variables`);
    return {};
  }
  const o = options as Record<string, unknown>;
  const out: Options = {};
  for (const key of ["endpoint", "stateDir"] as const) {
    const v = o[key];
    if (typeof v === "string") out[key] = v;
    else if (v !== undefined && v !== null) say.warn(`OpAMP: ${key} ${shown(v)} is not a string: it is not used`);
  }
  const h = o.headers;
  if (h !== undefined && h !== null) {
    if (typeof h !== "object" || Array.isArray(h)) {
      say.warn("OpAMP: headers is not an object of names to values: it is not used");
    } else {
      out.headers = {};
      for (const [name, value] of Object.entries(h)) {
        // The name only: the value may be a secret.
        if (typeof value === "string") out.headers[name] = value;
        else say.warn(`OpAMP: headers: the value of '${name}' is not a string: that header is not sent`);
      }
    }
  }
  if (o.heartbeatIntervalMs !== undefined && o.heartbeatIntervalMs !== null) {
    out.heartbeatIntervalMs = o.heartbeatIntervalMs as number; // checked where it is read
  }
  return out;
}

const ENDPOINT_IN_CODE = "the endpoint given to register()";
const HEADERS_IN_CODE = "the headers given to register()";

/** The variables the core's Setup reads. */
const SETUP_VARIABLES = ["OTEL_OPAMP_DISABLED", "OTEL_SDK_DISABLED", "OTEL_OPAMP_ENDPOINT", "OTEL_OPAMP_HEADERS",
  "OTEL_OPAMP_STATE_DIR", "OTEL_OPAMP_HEARTBEAT_INTERVAL", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "HOME",
  "XDG_STATE_HOME", "LOCALAPPDATA"];

let coreWarned = false;

/**
 * Whether the core can be loaded, by `unavailable` (why it can't, or undefined); when it can't,
 * said once, and the SDK runs unmanaged.
 */
function coreLoads(unavailable: string | undefined): boolean {
  if (unavailable !== undefined && !coreWarned) {
    coreWarned = true;
    say.warn(`OpAMP: the core can't be loaded on ${process.platform}-${process.arch}; the SDK runs unmanaged: ${unavailable}`);
  }
  return unavailable === undefined;
}

/**
 * Where the server is, from OTEL_OPAMP_* and what the code gives; undefined when the package is
 * off. The rules are the core's (its Setup call), the same in every language: what is here is the
 * I/O, reading the variables, taking a slot of the default state directory, and saying the
 * warnings. Nothing here throws.
 */
export function setupFromEnv(env: NodeJS.ProcessEnv = process.env, code: Options = {}): Setup | undefined {
  const options = given(code);
  // OTEL_OPAMP_DISABLED and OTEL_SDK_DISABLED are the core's to read (Setup). A core that can't be
  // loaded can't read them: then the SDK runs unmanaged whatever they say, and says why.
  if (!coreLoads(coreUnavailable())) return undefined;
  let heartbeatMs: bigint | undefined;
  const ms = options.heartbeatIntervalMs;
  if (ms !== undefined) {
    if (typeof ms === "number" && Number.isFinite(ms)) {
      heartbeatMs = BigInt(Math.trunc(ms));
    } else {
      say.warn(`OpAMP: heartbeatIntervalMs ${shown(ms)} is not a number of milliseconds: it is not used`);
    }
  }
  let cwd = "";
  try {
    cwd = process.cwd();
  } catch {
    // Removed from under the process: no working directory to tell it by.
  }
  const start = create(SetupStartSchema, {
    env: SETUP_VARIABLES.flatMap((name) => {
      const value = env[name];
      return value ? [create(SetupVariableSchema, { name, value })] : [];
    }),
    os: process.platform === "win32" ? SetupOs.WINDOWS : process.platform === "darwin" ? SetupOs.MACOS : SetupOs.UNIX,
    program: programName(),
    cwd,
    code: create(SetupInCodeSchema, {
      ...(options.endpoint !== undefined ? { endpoint: options.endpoint } : {}),
      ...(options.stateDir !== undefined ? { stateDir: options.stateDir } : {}),
      ...(heartbeatMs !== undefined ? { heartbeatMs } : {}),
      headersGiven: options.headers !== undefined,
      headers: Object.entries(options.headers ?? {}).map(([name, value]) => create(SetupHeaderSchema, { name, value })),
      endpointName: ENDPOINT_IN_CODE,
      headersName: HEADERS_IN_CODE,
      heartbeatName: "heartbeatIntervalMs",
    }),
  });
  let s;
  try {
    s = startSetup(start);
  } catch (e) {
    say.warn(`OpAMP: the core could not read the settings (${String(e)}): the SDK runs unmanaged`);
    return undefined;
  }
  for (const w of s.warnings) say.warn(`OpAMP: ${w}`);
  if (s.off) {
    // Turned off by a variable (said once, here); off for want of an endpoint says nothing.
    if (s.offReason) say.warn(`OpAMP: ${s.offReason}`);
    return undefined;
  }
  // The settings layer only once there is a server: an application without one never loads it.
  if (!coreLoads(settingsUnavailable())) return undefined;
  const setup: Setup = {
    endpoint: s.endpoint,
    headers: Object.fromEntries(s.headers.map((h) => [h.name, h.value])),
    heartbeatMs: Number(s.heartbeatIntervalMs),
    shown: s.shownEndpoint,
    ...(s.hints ? { hints: s.hints } : {}),
  };
  if (s.stateDir) {
    setup.stateDir = s.stateDir;
  } else {
    const claimed = slot(s.defaultSlots, s.allSlotsTaken);
    if (claimed) {
      setup.stateDir = claimed.dir;
      setup.stateLock = claimed.lock;
    }
  }
  return setup;
}

/** The sampler OTEL_TRACES_SAMPLER names, as the specification defines the values. */
export function samplerFromEnv(env: NodeJS.ProcessEnv = process.env): Sampler {
  // Unset, not a number or out of range: 1, as NodeSDK.
  const ratio = () => {
    const arg = num(env, "OTEL_TRACES_SAMPLER_ARG");
    return new TraceIdRatioBasedSampler(arg !== undefined && arg >= 0 && arg <= 1 ? arg : 1);
  };
  switch ((str(env, "OTEL_TRACES_SAMPLER") ?? "parentbased_always_on").trim()) {
    case "always_on":
      return new AlwaysOnSampler();
    case "always_off":
      return new AlwaysOffSampler();
    case "traceidratio":
      return ratio();
    case "parentbased_always_off":
      return new ParentBasedSampler({ root: new AlwaysOffSampler() });
    case "parentbased_traceidratio":
      return new ParentBasedSampler({ root: ratio() });
    default:
      return new ParentBasedSampler({ root: new AlwaysOnSampler() });
  }
}

/**
 * A rate cap's token bucket, the algorithm every language runs (the shared vectors'
 * caps.json pins it): full at its first take; refilled at `perSecond` tokens a second on
 * a monotonic clock in nanoseconds, up to max(perSecond, 1) (a burst of a second, at least one); a
 * take of n gets the whole tokens there are, up to n. A clock that goes back refills nothing.
 */
export class Bucket {
  #tokens: number;
  #last: number | undefined;

  constructor(readonly perSecond: number) {
    this.#tokens = Math.max(perSecond, 1);
  }

  /** Take up to `n` tokens at `nowNs`; how many were taken. */
  take(nowNs: number, n: number): number {
    const last = this.#last ?? nowNs;
    const elapsed = Math.max(0, nowNs - last) / 1e9;
    this.#tokens = Math.min(this.#tokens + elapsed * this.perSecond, Math.max(this.perSecond, 1));
    this.#last = Math.max(last, nowNs);
    const taken = Math.min(n, Math.floor(this.#tokens));
    this.#tokens -= taken;
    return taken;
  }
}

/** The same cap as before, or a full bucket for a new one (a change of n starts a full bucket). */
function bucket(was: Bucket | undefined, perSecond: number | undefined): Bucket | undefined {
  if (perSecond === undefined) return undefined;
  return was?.perSecond === perSecond ? was : new Bucket(perSecond);
}

/** A monotonic clock, in nanoseconds: what the rate caps refill by. */
export const monotonicNs = (): number => performance.now() * 1e6;

/** How the sampler decides a span: as the layer's Evaluate answers it (SamplingDecision). */
export type Sampling =
  | { how: "follow_parent"; sampled: boolean }
  | { how: "rate"; rate: number; rule: number | undefined }
  | { how: "app_sampler" };

/**
 * The text a rule's attribute glob matches: a string's own, an integer's decimal, `true` or
 * `false`. A double, an array or a missing attribute has none. An integer is a number that is a
 * safe integer, as OTLP exports it (an int); JavaScript can't tell 1.0 from 1.
 */
function attributeText(value: AttributeValue | undefined): string | undefined {
  if (typeof value === "string") return value;
  if (typeof value === "boolean") return String(value);
  if (typeof value === "number" && Number.isSafeInteger(value)) return String(value);
  return undefined;
}

/** The path a rule's `path` matches: `url.path`, or else `http.target` up to its `?`. */
function spanPath(attributes: Attributes): string | undefined {
  const path = attributes["url.path"];
  if (typeof path === "string") return path;
  const target = attributes["http.target"];
  return typeof target === "string" ? target.split("?", 1)[0] : undefined;
}

/** A sampling rule, its globs decoded once, and the SDK's ratio sampler at its rate. */
class Rule {
  readonly ratio: Sampler;
  readonly #name: string[] | undefined;
  readonly #path: string[] | undefined;
  readonly #attributes: [string, string[]][];

  constructor(readonly rule: SamplingRule) {
    this.ratio = new TraceIdRatioBasedSampler(rule.rate);
    this.#name = rule.name === undefined ? undefined : Array.from(rule.name);
    this.#path = rule.path === undefined ? undefined : Array.from(rule.path);
    this.#attributes = Object.entries(rule.attributes).map(([k, p]) => [k, Array.from(p)]);
  }

  /** Whether a span starting so matches every matcher the rule has. */
  matches(name: string, kind: SpanKind, attributes: Attributes): boolean {
    // The layer's SpanKind is OTLP's, one more than the API's (SpanKind.INTERNAL is 0).
    if (this.rule.kind !== undefined && this.rule.kind !== kind + 1) return false;
    if (this.#name && !globChars(this.#name, Array.from(name))) return false;
    if (this.#path) {
      const path = spanPath(attributes);
      if (path === undefined || !globChars(this.#path, Array.from(path))) return false;
    }
    return this.#attributes.every(([key, p]) => {
      const text = attributeText(attributes[key]);
      return text !== undefined && globChars(p, Array.from(text));
    });
  }
}

/**
 * The SDK's own sampler until the server sets a sampling rate or rules; then, for a new trace,
 * the first rule its root span matches, else the rate, else the SDK's own sampler, each rate by the
 * SDK's trace-ID ratio sampler; a child follows its parent's decision, as the default does. Then
 * the trace cap: a sampled root span, or a span whose remote parent was sampled, takes a token,
 * and without one isn't sampled.
 */
export class LiveSampler implements Sampler {
  #configured: Sampler = new ParentBasedSampler({ root: new AlwaysOnSampler() });
  #rate: number | undefined;
  #ratio: Sampler | undefined;
  #rules: readonly Rule[] = [];
  /** A child, local or remote, as its parent was: the default's parent rules. */
  readonly #follow: Sampler = new ParentBasedSampler({ root: new AlwaysOffSampler() });
  current: Sampler = this.#configured;
  /** sampling_traces_per_second: the traces' bucket; undefined, no cap. */
  cap: Bucket | undefined;
  /** The clock the cap refills by (nanoseconds, monotonic); a test's own in tests. */
  clock: () => number = monotonicNs;

  /** The sampler the configuration built. A rate set before (at start) still holds. */
  wrap(configured: Sampler): this {
    this.#configured = configured;
    this.rate(this.#rate);
    return this;
  }

  /** The sampling_rate in force (in debug mode, the debug rate); undefined: none. */
  get samplingRate(): number | undefined {
    return this.#rate;
  }

  rate(rate: number | undefined): void {
    this.#rate = rate;
    this.#ratio = rate === undefined ? undefined : new TraceIdRatioBasedSampler(rate);
    this.current =
      rate === undefined
        ? this.#configured
        : new ParentBasedSampler({ root: this.#ratio! });
  }

  /** sampling_rules, as the layer hands them (none: the rate, or the SDK's own sampler, alone). */
  rules(rules: readonly SamplingRule[]): void {
    this.#rules = rules.map((r) => new Rule(r));
  }

  /** sampling_traces_per_second: a new n starts a full bucket, the same n keeps it. */
  perSecond(n: number | undefined): void {
    this.cap = bucket(this.cap, n);
  }

  /**
   * How a span starting so is decided, before the cap: undefined while neither rules nor a rate
   * are set (the SDK's own sampler then decides every span, as without the server).
   */
  explain(ctx: Context, name: string, kind: SpanKind, attributes: Attributes): Sampling | undefined {
    if (this.#rules.length === 0 && this.#rate === undefined) return undefined;
    const parent = trace.getSpanContext(ctx);
    if (parent && isSpanContextValid(parent)) {
      return { how: "follow_parent", sampled: (parent.traceFlags & TraceFlags.SAMPLED) === TraceFlags.SAMPLED };
    }
    const i = this.#rules.findIndex((r) => r.matches(name, kind, attributes));
    if (i >= 0) return { how: "rate", rate: this.#rules[i]!.rule.rate, rule: i };
    return this.#rate === undefined ? { how: "app_sampler" } : { how: "rate", rate: this.#rate, rule: undefined };
  }

  shouldSample(context: Context, traceId: string, name: string, kind: SpanKind, attributes: Attributes,
    links: Link[]): SamplingResult {
    const how = this.explain(context, name, kind, attributes);
    const sampler = how?.how === "follow_parent"
      ? this.#follow
      : how?.how === "rate"
        ? (how.rule === undefined ? this.#ratio! : this.#rules[how.rule]!.ratio)
        : this.#configured;
    const result = sampler.shouldSample(context, traceId, name, kind, attributes, links);
    if (!this.cap || result.decision !== SamplingDecision.RECORD_AND_SAMPLED) return result;
    // The cap: a new trace, or one a traced caller sent, takes a token. A child of a span sampled
    // here takes none: refusing it would break its trace. Nor does a span the application's own
    // sampler samples under a caller that didn't sample.
    const parent = trace.getSpanContext(context);
    if (parent && isSpanContextValid(parent)
      && (!parent.isRemote || !(parent.traceFlags & TraceFlags.SAMPLED))) return result;
    return this.cap.take(this.clock(), 1) === 1 ? result : { decision: SamplingDecision.NOT_RECORD };
  }

  toString(): string {
    return `OpampLiveSampler{${this.current.toString()}}`;
  }
}

/** The prefix of an auto-instrumentation's scope name; what follows is its name in the settings. */
const INSTRUMENTATION = "@opentelemetry/instrumentation-";

/**
 * The name deactivate_instrumentations switches an instrumentation by, from its scope name:
 * upstream's short one (`http` for `@opentelemetry/instrumentation-http`); undefined for a scope
 * outside upstream's names.
 */
function switchName(scope: string): string | undefined {
  return scope.startsWith(INSTRUMENTATION) ? scope.slice(INSTRUMENTATION.length) : undefined;
}

/** A tracer that starts no span: the parent's span context instead, as the API's no-op tracer. */
const OFF: Tracer = new ProxyTracerProvider().getTracer("off");

/** A send_logs_level_by_scope level that drops every record of its scopes (`off`). */
export const SEVERITY_OFF = 0xffff;

/**
 * Whether a send_logs_level_by_scope pattern matches a scope name: an exact name, or a prefix
 * ending in `*` (by characters: `com.acme*` matches `com.acmesoft`; `*` alone, every name).
 */
export function scopeMatches(pattern: string, scope: string): boolean {
  return pattern.endsWith("*") ? scope.startsWith(pattern.slice(0, -1)) : pattern === scope;
}


/**
 * Whether `text` matches the glob `pattern`: `*` matches any run of characters, none included, and
 * every other character is itself. By Unicode code points (as the layer's chars), so a character
 * past the BMP is one character, as in UTF-8 and UTF-16 alike.
 */
export function glob(pattern: string, text: string): boolean {
  return globChars(Array.from(pattern), Array.from(text));
}

/** What a masked attribute's value becomes, whatever its type. */
export const MASK = "[REDACTED]";

/**
 * drop_attributes and mask_attributes, then scrubbing (scrub_defaults, scrub_patterns). Redaction
 * patterns are globs on attribute keys, matched ignoring ASCII case (the layer hands them
 * lowercase). An attribute a drop pattern matches is removed; one only a mask pattern matches has
 * its value replaced by MASK; every other one is scrubbed (scrub.ts), and so is a log record's
 * body. A copy is made only of what changes: the application's own span or record, which other
 * processors share, is never changed.
 */
export class Redaction {
  readonly #drop: string[][];
  readonly #mask: string[][];
  readonly #scrub: Scrubbing;

  constructor(drop: readonly string[] = [], mask: readonly string[] = [], scrub: Scrubbing = new Scrubbing()) {
    this.#drop = drop.map((p) => Array.from(asciiLower(p)));
    this.#mask = mask.map((p) => Array.from(asciiLower(p)));
    this.#scrub = scrub;
  }

  /** Whether there is anything to redact or scrub. */
  get on(): boolean {
    return this.#drop.length > 0 || this.#mask.length > 0 || this.#scrub.on;
  }

  /** The attributes as exported: the same object when nothing changed. */
  attributes<T extends Attributes | undefined>(attributes: T): T {
    if (!attributes || !this.on) return attributes;
    let out: Attributes | undefined;
    for (const key of Object.keys(attributes)) {
      const k = Array.from(asciiLower(key));
      if (this.#drop.some((p) => globChars(p, k))) {
        out ??= { ...attributes };
        delete out[key];
      } else if (this.#mask.some((p) => globChars(p, k))) {
        out ??= { ...attributes };
        out[key] = MASK;
      } else if (this.#scrub.on) {
        const v = attributes[key];
        const s = this.#scrub.value(key, v);
        if (s === v) continue;
        out ??= { ...attributes };
        out[key] = s as AttributeValue;
      }
    }
    return (out ?? attributes) as T;
  }

  /**
   * A span as exported: its attributes, its events' and its links' redacted. The original when
   * nothing matched; otherwise an object over it (its prototype), so spanContext() and the rest
   * are the span's own.
   */
  span(span: ReadableSpan): ReadableSpan {
    const attributes = this.attributes(span.attributes);
    let events: ReadableSpan["events"] = span.events;
    for (const [i, e] of span.events.entries()) {
      const a = this.attributes(e.attributes);
      if (a === e.attributes) continue;
      if (events === span.events) events = [...span.events];
      events[i] = { ...e, attributes: a };
    }
    let links: ReadableSpan["links"] = span.links;
    for (const [i, l] of span.links.entries()) {
      const a = this.attributes(l.attributes);
      if (a === l.attributes) continue;
      if (links === span.links) links = [...span.links];
      links[i] = { ...l, attributes: a };
    }
    if (attributes === span.attributes && events === span.events && links === span.links) return span;
    return Object.create(span, {
      attributes: { value: attributes, enumerable: true },
      events: { value: events, enumerable: true },
      links: { value: links, enumerable: true },
    }) as ReadableSpan;
  }

  /** A log record as exported, as `span`: its attributes redacted, its body scrubbed. */
  log(record: ReadableLogRecord): ReadableLogRecord {
    const attributes = this.attributes(record.attributes as Attributes);
    const body = record.body === undefined ? undefined : this.#scrub.value(undefined, record.body);
    if (attributes === record.attributes && body === record.body) return record;
    return Object.create(record, {
      attributes: { value: attributes, enumerable: true },
      body: { value: body, enumerable: true },
    }) as ReadableLogRecord;
  }
}

/** What a signal's gate counted since start (the layer's SignalCounts). */
export class SignalCount {
  exportsOk = 0;
  exportsFailed = 0;
  itemsFailed = 0;
  itemsFiltered = 0;
  lastFailed = false;
  lastError = "";

  /** An export's result, after the exporter's retries, of `items` spans, points or records. */
  result(r: ExportResult, items: number): void {
    if (r.code === 0) {
      this.exportsOk++;
      this.lastFailed = false;
    } else {
      this.exportsFailed++;
      this.itemsFailed += items;
      this.lastFailed = true;
      this.lastError = r.error ? (r.error.message || String(r.error)) : "";
    }
  }

  /** As the layer's Health call takes it. */
  get message() {
    return {
      exportsOk: BigInt(this.exportsOk), exportsFailed: BigInt(this.exportsFailed), itemsFailed: BigInt(this.itemsFailed),
      itemsFiltered: BigInt(this.itemsFiltered), lastFailed: this.lastFailed, lastError: this.lastError,
    };
  }
}

/** The data points of some scopes' metrics: what export health counts metrics in. */
function points(scopes: ResourceMetrics["scopeMetrics"]): number {
  return scopes.reduce((n, s) => n + s.metrics.reduce((m, metric) => m + metric.dataPoints.length, 0), 0);
}

/** What the server changes while the SDK runs. `app`: where the application reads its own settings (custom). */
export class Controls {
  readonly sampler = new LiveSampler();
  readonly app: AppSettings;

  constructor(app: AppSettings = new AppSettings(appLog())) {
    this.app = app;
  }

  sendTraces = true;
  sendMetrics = true;
  sendLogs = true;
  /** The lowest severity number of a log record exported; undefined: every one. */
  sendLogsMinSeverity: number | undefined;
  /** The instrumentations switched off: every one, or these by name (`http`). */
  #off: { all: boolean; names: ReadonlySet<string> } = { all: false, names: new Set() };
  /** The scope names of the instrumentations the server can switch: those loaded at start. */
  readonly #loaded = new Set<string>();
  /**
   * send_logs_level_by_scope: a level per scope-name pattern, as the layer sorts them (the first
   * that matches a scope is its entry).
   */
  sendLogsScopeLevels: readonly ScopeLevel[] = [];
  /** send_logs_per_second: the log records' bucket; undefined, no cap. */
  logsCap: Bucket | undefined;
  /** drop_attributes, mask_attributes and scrubbing, at the span and log exporters' wrappers. */
  redaction = new Redaction();
  #clock: () => number = monotonicNs;

  /** The clock the caps refill by (nanoseconds, monotonic), the sampler's too; a test's own in tests. */
  get clock(): () => number {
    return this.#clock;
  }

  set clock(clock: () => number) {
    this.#clock = clock;
    this.sampler.clock = clock;
  }

  /**
   * What the gates counted since start, per signal, for export health: each export's result after
   * the exporter's own retries, and the items dropped on purpose (send_* false, a switched-off
   * instrumentation, a level, a cap).
   */
  readonly counts: Record<"traces" | "metrics" | "logs", SignalCount> = {
    traces: new SignalCount(), metrics: new SignalCount(), logs: new SignalCount(),
  };

  /** The settings in force, as last applied: export health reports a signal turned off as off. */
  settings: Settings | undefined;

  /** `done`, after counting the export's result for `signal`. */
  #counted(signal: "traces" | "metrics" | "logs", items: number, done: (r: ExportResult) => void) {
    return (r: ExportResult) => {
      this.counts[signal].result(r, items);
      done(r);
    };
  }

  /**
   * Carry out the live settings; a setting left out is the SDK's own. The SDK's settings and the
   * application's (custom) go in force in this one synchronous call: nothing runs between them, so
   * both always come from the same document.
   */
  apply(s: Settings | undefined): void {
    this.settings = s;
    this.sampler.rate(s?.samplingRate);
    this.sampler.rules(s?.samplingRules ?? []);
    this.sampler.perSecond(s?.samplingTracesPerSecond);
    this.sendTraces = s?.sendTraces ?? true;
    this.sendMetrics = s?.sendMetrics ?? true;
    this.sendLogs = s?.sendLogs ?? true;
    this.sendLogsMinSeverity = s?.sendLogsMinSeverity;
    this.sendLogsScopeLevels = s?.sendLogsScopeLevels ?? [];
    this.logsCap = bucket(this.logsCap, s?.sendLogsPerSecond);
    this.redaction = new Redaction(s?.dropAttributes, s?.maskAttributes,
      new Scrubbing(s?.scrubDefaults ?? false, s?.scrubPatterns ?? []));
    this.#off = {
      all: s?.deactivateAllInstrumentations === true,
      names: new Set((s?.deactivateInstrumentations?.names ?? []).map((n) => n.trim()).filter((n) => n !== "")),
    };
    // Last: a callback the application gave sees the SDK's settings already in force.
    this.app.put(s?.custom);
  }

  /**
   * Put the instrumentations under the server: each gets a tracer provider (over `provider`, the
   * global one by default) whose tracer starts no span while the instrumentation is switched off.
   * Call it once the SDK has started: NodeSDK hands them its own providers as it starts.
   */
  instrument(instrumentations: Instrumentation[], provider: TracerProvider = trace.getTracerProvider()): void {
    const switching = this.tracerProvider(provider);
    for (const i of instrumentations) {
      this.#loaded.add(i.instrumentationName);
      i.setTracerProvider(switching);
    }
  }

  /**
   * Whether the instrumentation with this scope name (`@opentelemetry/instrumentation-http`) is
   * switched off. Only an instrumentation `instrument` was given can be: the application's own
   * scopes never are.
   */
  off(scope: string): boolean {
    if (!this.#loaded.has(scope)) return false;
    if (this.#off.all) return true;
    const name = switchName(scope);
    return name !== undefined && this.#off.names.has(name);
  }

  /** The names switched off that match no instrumentation loaded: there was nothing to switch. */
  get unloaded(): string[] {
    return [...this.#off.names].filter((n) => !this.#loaded.has(INSTRUMENTATION + n));
  }

  /**
   * Tracers that start no span while their instrumentation is switched off. The span they return
   * instead carries the parent's context, so the trace goes on through the instrumentations still
   * on, and propagation is unchanged.
   */
  tracerProvider(provider: TracerProvider): TracerProvider {
    return {
      getTracer: (name: string, version?: string, options?: TracerOptions): Tracer =>
        new SwitchedTracer(provider.getTracer(name, version, options), name, this),
    };
  }

  /** While traces aren't sent, what would be exported is dropped; what is exported is redacted. */
  spans(to: SpanExporter): SpanExporter {
    return {
      export: (spans: ReadableSpan[], done: (r: ExportResult) => void) => {
        if (!this.sendTraces) {
          this.counts.traces.itemsFiltered += spans.length;
          return done(SUCCESS);
        }
        const redaction = this.redaction;
        to.export(redaction.on ? spans.map((s) => redaction.span(s)) : spans, this.#counted("traces", spans.length, done));
      },
      shutdown: () => to.shutdown(),
      forceFlush: () => to.forceFlush?.() ?? Promise.resolve(),
    };
  }

  /**
   * While metrics aren't sent, what would be exported is dropped, and a switched-off
   * instrumentation's metrics are always dropped: its instruments were made at start and go on
   * recording; only exporting can stop.
   */
  metrics(to: PushMetricExporter): PushMetricExporter {
    const gated: PushMetricExporter = {
      export: (metrics: ResourceMetrics, done: (r: ExportResult) => void) => {
        const all = points(metrics.scopeMetrics);
        if (!this.sendMetrics) {
          this.counts.metrics.itemsFiltered += all;
          return done(SUCCESS);
        }
        const kept = metrics.scopeMetrics.filter((m) => !this.off(m.scope.name));
        const n = points(kept);
        this.counts.metrics.itemsFiltered += all - n;
        if (kept.length === metrics.scopeMetrics.length) return to.export(metrics, this.#counted("metrics", n, done));
        if (kept.length === 0) return done(SUCCESS);
        to.export({ ...metrics, scopeMetrics: kept }, this.#counted("metrics", n, done));
      },
      shutdown: () => to.shutdown(),
      forceFlush: () => to.forceFlush(),
    };
    if (to.selectAggregationTemporality) gated.selectAggregationTemporality = (t) => to.selectAggregationTemporality!(t);
    if (to.selectAggregation) gated.selectAggregation = (t) => to.selectAggregation!(t);
    return gated;
  }

  /**
   * The lowest severity number exported for the log records of a scope (its exported name): the
   * first entry of send_logs_level_by_scope that matches it, else send_logs_level; undefined:
   * every record. SEVERITY_OFF: none.
   */
  threshold(scope: string): number | undefined {
    for (const l of this.sendLogsScopeLevels) {
      if (scopeMatches(l.pattern, scope)) return l.minSeverity;
    }
    return this.sendLogsMinSeverity;
  }

  /**
   * Whether a log record passes the levels (before the rate cap and redaction): at or above its
   * scope's threshold. One without a severity number (0) can't be judged and is kept, except
   * where its scope is off.
   */
  passes(scope: string, severity: number | undefined): boolean {
    const t = this.threshold(scope);
    if (t === undefined) return true;
    if (t === SEVERITY_OFF) return false;
    return !severity || severity >= t;
  }

  /**
   * While logs aren't sent, what would be exported is dropped, and a switched-off instrumentation's
   * records (a log bridge's: pino, bunyan) are always dropped. Then the levels: send_logs_level,
   * or the scope's own in send_logs_level_by_scope; then send_logs_per_second, which keeps a
   * batch's first records while its tokens last. What is exported is redacted.
   */
  logs(to: LogRecordExporter): LogRecordExporter {
    return {
      export: (logs: ReadableLogRecord[], done: (r: ExportResult) => void) => {
        if (!this.sendLogs) {
          this.counts.logs.itemsFiltered += logs.length;
          return done(SUCCESS);
        }
        let kept = logs.filter((r) => !this.off(r.instrumentationScope.name)
          && this.passes(r.instrumentationScope.name, r.severityNumber));
        if (this.logsCap && kept.length > 0) {
          const allowed = this.logsCap.take(this.clock(), kept.length);
          if (allowed < kept.length) kept = kept.slice(0, allowed);
        }
        const redaction = this.redaction;
        if (redaction.on) kept = kept.map((r) => redaction.log(r));
        this.counts.logs.itemsFiltered += logs.length - kept.length;
        if (kept.length === 0 && logs.length > 0) return done(SUCCESS);
        to.export(kept, this.#counted("logs", kept.length, done));
      },
      shutdown: () => to.shutdown(),
      forceFlush: () => to.forceFlush?.() ?? Promise.resolve(),
    };
  }
}

/** An instrumentation's tracer, or no span while the instrumentation is switched off. */
class SwitchedTracer implements Tracer {
  constructor(readonly tracer: Tracer, readonly scope: string, readonly controls: Controls) {}

  #now(): Tracer {
    return this.controls.off(this.scope) ? OFF : this.tracer;
  }

  startSpan(name: string, options?: SpanOptions, ctx?: Context): Span {
    return this.#now().startSpan(name, options, ctx);
  }

  startActiveSpan<F extends (span: Span) => unknown>(_name: string, _options: F | SpanOptions, _context?: F | Context,
    _fn?: F): ReturnType<F> {
    const t = this.#now();
    // As given: the API's tracers tell the forms apart by the number of arguments.
    return Reflect.apply(t.startActiveSpan, t, arguments) as ReturnType<F>;
  }
}

type Signal = "TRACES" | "METRICS" | "LOGS";

/**
 * The exporters a signal's variable names, as NodeSDK reads it: each once, the default (otlp)
 * when there are none, and nothing (`[]`) for `none`. Traces are read as NodeSDK reads them:
 * `none` first is none, `none` among others is the default alone (`null` is dropped). For
 * metrics and logs, `none` anywhere is none.
 */
export function exporterNames(signal: Signal, env: NodeJS.ProcessEnv = process.env, warn = false): string[] {
  const key = `OTEL_${signal}_EXPORTER`;
  let names = [...new Set(list(env, key) ?? [])];
  if (signal === "TRACES") {
    names = names.filter((n) => n !== "null");
    if (names[0] === "none") return [];
    if (names.length > 1 && names.includes("none")) {
      if (warn) diag.warn(`${key} contains "none" along with other exporters. Using default otlp exporter.`);
      return ["otlp"];
    }
  }
  if (names.length === 0) return ["otlp"];
  return names.includes("none") ? [] : names;
}

const PROTOCOLS = new Set(["grpc", "http/json", "http/protobuf"]);

/** A signal's OTLP protocol, as NodeSDK reads it: one it doesn't know is http/protobuf. */
function protocol(signal: Signal, env: NodeJS.ProcessEnv, warn = false): string {
  const p = (str(env, `OTEL_EXPORTER_OTLP_${signal}_PROTOCOL`) ?? str(env, "OTEL_EXPORTER_OTLP_PROTOCOL"))?.trim()
    || "http/protobuf";
  if (PROTOCOLS.has(p)) return p;
  if (warn) diag.warn(`Unsupported OTLP ${signal.toLowerCase()} protocol: "${p}". Using http/protobuf.`);
  return "http/protobuf";
}

/**
 * Whether this package builds a signal's exporters, and so can stop them: it builds OTLP, console
 * and none; anything else is left to NodeSDK, ungated.
 */
export function builds(signal: Signal, env: NodeJS.ProcessEnv = process.env): boolean {
  return exporterNames(signal, env).every((n) => n === "none" || n === "console" || n === "otlp");
}

/**
 * What this package carries out, declared to the settings layer (docs/sdk-settings.md, "The
 * host"). The layer refuses an override of any setting not named, and sets it aside in a policy in
 * force. The host always names the sampler's settings and the instrumentation switches, at once
 * (instrumentations are switched while the application runs). It names a signal's sending, and
 * what its gate holds, only where this package builds every one of its exporters (`builds`).
 * Nothing waits for the next start. `own`: the signals the application registered a
 * global provider of its own for (watchProviders). Their telemetry goes past the gate, and for
 * traces past the sampler, so those settings aren't declared.
 */
export function hostFromEnv(env: NodeJS.ProcessEnv = process.env, own: ReadonlySet<ApiSignal> = new Set()): HostInit {
  // The sampler is this package's whatever the exporters, but it samples only the spans of the
  // SDK's tracer provider. Debug mode is the layer's: the package only runs the timer for its end
  // (Agent.refresh).
  const sampled = !own.has("traces");
  const live = [...(sampled ? ["sampling_rate", "sampling_rules", "sampling_traces_per_second"] : []), "debug_until",
    ...(sampled ? ["debug_sampling_rate"] : []), "deactivate_instrumentations", "deactivate_all_instrumentations"];
  // custom, the application's own settings (docs/app-settings.md): it reads them through
  // appSettings, whatever the exporters.
  live.push("custom");
  const spansGated = builds("TRACES", env) && !own.has("traces");
  const logsGated = builds("LOGS", env) && !own.has("logs");
  if (spansGated) live.push("send_traces");
  if (builds("METRICS", env) && !own.has("metrics")) live.push("send_metrics");
  // The levels, the cap and debug mode's level are held at the same gate as send_logs.
  if (logsGated) {
    live.push("send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second", "debug_send_logs_level");
  }
  // Redaction is of spans and log records alike: only where both pass through this package.
  if (spansGated && logsGated) live.push("drop_attributes", "mask_attributes", "scrub_defaults", "scrub_patterns");
  return { live };
}

/** A signal the OpenTelemetry API holds one global provider for. */
export type ApiSignal = "traces" | "metrics" | "logs";

const PROVIDER: Record<ApiSignal, string> = { traces: "tracer", metrics: "meter", logs: "logger" };

/**
 * The global providers the API holds, as `watchProviders` sees them registered: the SDK's, and
 * whom to tell of another.
 */
const registry: { agent: Agent | undefined; starting: boolean; sdk: Partial<Record<ApiSignal, unknown>>; watching: boolean } = {
  agent: undefined, starting: false, sdk: {}, watching: false,
};

/**
 * Run `start` (which starts the SDK) and from then on tell `agent` of each global provider the
 * application registers through the API in place of the SDK's (Agent.providerOfItsOwn). What the
 * application sends through the API then goes to that provider: past this package's sampler and
 * gates. The API's setters are wrapped, once, so the agent hears of it as it happens. While
 * `start` runs a registration is the SDK's own; one the API refuses then means the application
 * registered its own first. A provider the application builds and uses without registering it
 * isn't seen. Nor is one registered through another copy of the API: the API keeps one global
 * per signal, but each copy has its own setters.
 */
export function watchProviders(agent: Agent, start: () => void): void {
  registry.agent = agent;
  registry.sdk = {};
  if (!registry.watching) {
    registry.watching = true;
    const watch = <P, R>(signal: ApiSignal, set: (provider: P) => R, took: (result: R, provider: P) => boolean) =>
      (provider: P): R => {
        const result = set(provider);
        if (registry.starting && took(result, provider)) registry.sdk[signal] = provider;
        else if (registry.starting || (took(result, provider) && provider !== registry.sdk[signal])) {
          registry.agent?.providerOfItsOwn(signal);
        }
        return result;
      };
    trace.setGlobalTracerProvider = watch("traces", trace.setGlobalTracerProvider.bind(trace), (r) => r === true);
    metrics.setGlobalMeterProvider = watch("metrics", metrics.setGlobalMeterProvider.bind(metrics), (r) => r === true);
    // The logs API answers with the provider it holds: the one given, or the one it kept.
    logs.setGlobalLoggerProvider = watch("logs", logs.setGlobalLoggerProvider.bind(logs), (r, p) => r === p);
  }
  registry.starting = true;
  try {
    start();
  } finally {
    registry.starting = false;
  }
}

/**
 * The exporters each signal's variables name, as NodeSDK would build them, or undefined when a
 * name is one this package doesn't build (NodeSDK builds that signal, ungated). Logs set to
 * `none` are undefined too: NodeSDK, reading the same variable, sets up no logger provider, where
 * no processors would still set up an empty one.
 */
export function exportersFromEnv(env: NodeJS.ProcessEnv = process.env) {
  const build = <T>(signal: Signal, otlp: Record<string, () => T>, console: () => T): T[] | undefined => {
    if (!builds(signal, env)) {
      say.warn(`OpAMP: the ${signal.toLowerCase()} exporter is left to NodeSDK: the server can't stop it`);
      return undefined;
    }
    const names = exporterNames(signal, env, true);
    if (names.length === 0 && signal === "LOGS") return undefined;
    return names.map((name) => (name === "console" ? console() : otlp[protocol(signal, env, true)]!()));
  };
  return {
    traces: build<SpanExporter>("TRACES", {
      grpc: () => new GrpcTraces(), "http/json": () => new HttpTraces(), "http/protobuf": () => new ProtoTraces(),
    }, () => new ConsoleSpanExporter()),
    metrics: build<PushMetricExporter>("METRICS", {
      grpc: () => new GrpcMetrics(), "http/json": () => new HttpMetrics(), "http/protobuf": () => new ProtoMetrics(),
    }, () => new ConsoleMetricExporter()),
    logs: build<LogRecordExporter>("LOGS", {
      grpc: () => new GrpcLogs(), "http/json": () => new HttpLogs(), "http/protobuf": () => new ProtoLogs(),
    }, () => new ConsoleLogRecordExporter()),
  };
}

/**
 * The exporters NodeSDK builds from a signal's variable, by name. It warns of any other name and
 * builds nothing for it.
 */
const NODE_SDK_BUILDS: Record<Signal, ReadonlySet<string>> = {
  TRACES: new Set(["otlp", "zipkin", "console"]),
  METRICS: new Set(["otlp", "prometheus", "console"]),
  LOGS: new Set(["otlp", "console"]),
};

/** This package as published, from its package.json: its name and version (empty if unreadable). */
function published(): { name: string; version: string } {
  try {
    const p = JSON.parse(readFileSync(new URL("../../package.json", import.meta.url), "utf8")) as Record<string, unknown>;
    return { name: typeof p.name === "string" ? p.name : "", version: typeof p.version === "string" ? p.version : "" };
  } catch {
    return { name: "", version: "" };
  }
}

/**
 * What this instance runs, for the layer's `components` (docs/node-otel.md#inventory): this
 * package and the SDK; each instrumentation it loaded (`instrumentations`, switched off or not)
 * under the name deactivate_instrumentations takes, with its version; and the exporters each
 * signal's variable builds, named as the variable names them. A signal this package builds is
 * gated; one it leaves to NodeSDK (send_* not declared, hostFromEnv) is listed as NodeSDK builds
 * it, ungated.
 */
export function inventory(instrumentations: readonly Instrumentation[], env: NodeJS.ProcessEnv = process.env): InventoryInit {
  const own = published();
  const signals: Signal[] = ["TRACES", "METRICS", "LOGS"];
  return {
    integration: own.name,
    integrationVersion: own.version,
    sdkLanguage: String(SDK_INFO["telemetry.sdk.language"]),
    sdkVersion: String(SDK_INFO["telemetry.sdk.version"]),
    // One outside upstream's names can't be switched off by name (only by
    // deactivate_all_instrumentations): there is no name to list it under.
    instrumentations: instrumentations.flatMap((i) => {
      const name = switchName(i.instrumentationName);
      return name === undefined ? [] : [{ name, version: i.instrumentationVersion ?? "" }];
    }),
    exporters: signals.flatMap((signal) => {
      const gated = builds(signal, env);
      const names = exporterNames(signal, env).filter((n) => gated || NODE_SDK_BUILDS[signal].has(n));
      return names.map((name) => ({ signal: signal.toLowerCase(), name, gated }));
    }),
  };
}

/**
 * An OTLP metric reader's interval and timeout, as NodeSDK reads OTEL_METRIC_EXPORT_INTERVAL and
 * OTEL_METRIC_EXPORT_TIMEOUT: a value not above 0, or not a number, is the default; the timeout
 * is at most the interval.
 */
export function metricReaderOptions(env: NodeJS.ProcessEnv = process.env) {
  const exportIntervalMillis = positive(env, "OTEL_METRIC_EXPORT_INTERVAL") ?? 60_000;
  const timeout = positive(env, "OTEL_METRIC_EXPORT_TIMEOUT") ?? 30_000;
  return { exportIntervalMillis, exportTimeoutMillis: Math.min(timeout, exportIntervalMillis) };
}

type LogBatching = Pick<BatchLogRecordProcessorOptions,
  "maxQueueSize" | "scheduledDelayMillis" | "exportTimeoutMillis" | "maxExportBatchSize">;

/** A batch log processor's OTEL_BLRP_* settings, as NodeSDK reads them; unset ones left out. */
export function batchLogOptions(env: NodeJS.ProcessEnv = process.env): LogBatching {
  const out: LogBatching = {};
  const set = (k: keyof LogBatching, key: string) => {
    const v = positive(env, key);
    if (v !== undefined) out[k] = v;
  };
  set("maxQueueSize", "OTEL_BLRP_MAX_QUEUE_SIZE");
  set("scheduledDelayMillis", "OTEL_BLRP_SCHEDULE_DELAY");
  set("exportTimeoutMillis", "OTEL_BLRP_EXPORT_TIMEOUT");
  set("maxExportBatchSize", "OTEL_BLRP_MAX_EXPORT_BATCH_SIZE");
  return out;
}

/**
 * A batch span processor's OTEL_BSP_* settings, as NodeSDK reads them. Every one is given: the
 * processor this package builds would otherwise read a variable NodeSDK refused (0 or below).
 */
export function batchSpanOptions(env: NodeJS.ProcessEnv = process.env) {
  return {
    maxQueueSize: positive(env, "OTEL_BSP_MAX_QUEUE_SIZE") ?? 2048,
    scheduledDelayMillis: positive(env, "OTEL_BSP_SCHEDULE_DELAY") ?? 5000,
    exportTimeoutMillis: positive(env, "OTEL_BSP_EXPORT_TIMEOUT") ?? 30_000,
    maxExportBatchSize: positive(env, "OTEL_BSP_MAX_EXPORT_BATCH_SIZE") ?? 512,
  };
}

function anyValue(value: AttributeValue | null | undefined): AnyValue {
  const v =
    typeof value === "boolean"
      ? { case: "boolValue" as const, value }
      : typeof value === "number"
        ? Number.isSafeInteger(value)
          ? { case: "intValue" as const, value: BigInt(value) }
          : { case: "doubleValue" as const, value }
        : typeof value === "string"
          ? { case: "stringValue" as const, value }
          : Array.isArray(value)
            ? { case: "arrayValue" as const, value: { values: (value as unknown[]).map((e) => anyValue(e as AttributeValue)) } }
            : undefined; // null or undefined: an empty value, as OTLP has it
  return create(AnyValueSchema, v ? { value: v } : {});
}

/** The resource's attributes, typed as OpAMP's KeyValue (a list as a list). */
export function attributes(resource: Resource): KeyValue[] {
  return Object.entries(resource.attributes)
    .filter(([, value]) => value !== undefined)
    .map(([key, value]) => create(KeyValueSchema, { key, value: anyValue(value) }));
}

/** Write a state file, all or nothing, readable by the agent's user only. */
function write(path: string, content: string | Uint8Array): void {
  saveState(path, typeof content === "string" ? new TextEncoder().encode(content) : content);
}

/** How often export health is looked at (the layer sends it at once on a change, else at most every 60 s). */
export const HEALTH_EVERY_MS = 30_000;

/** The package's state: one per process. `app`: where the application reads its own settings. */
export class Agent {
  readonly controls: Controls;

  constructor(app?: AppSettings) {
    this.controls = new Controls(app);
  }

  setup: Setup | undefined;
  layer: SdkSettings | undefined;
  loaded: Loaded | undefined;
  host: HostInit = {};
  current: Uint8Array = new TextEncoder().encode("{}");
  effective: EffectiveConfig | undefined;
  uid: string | undefined;
  client: Client | undefined;
  reachability: Reachability | undefined;
  #stopping: Promise<void> | undefined;
  /** Each prepare: a stop that ends after a later prepare leaves that one's slot alone. */
  #run = 0;
  /** The wall clock, in milliseconds since the Unix epoch: what debug mode is decided by. */
  now: () => number = Date.now;
  /** A monotonic clock, in milliseconds: what the agent's timer waits by. */
  elapsed: () => number = () => performance.now();
  /** How the agent's one timer is set and cleared; a test's own in tests. */
  timers: { set(fn: () => void, ms: number): unknown; clear(handle: unknown): void } = {
    set: (fn, ms) => setTimeout(fn, ms).unref(),
    clear: (handle) => clearTimeout(handle as NodeJS.Timeout),
  };
  /** When the layer said to call Refresh (wall clock, ms; 0: never), and that, monotonic. */
  #refreshAt = 0;
  #refreshDue = 0;
  #timer: unknown;
  /** Each arming of the timer: a timer armed before the latest arming does nothing. */
  #generation = 0;
  /** When export health is next due (monotonic, ms; 0: not before the client starts). */
  #healthDue = 0;
  /** The layer's last answer on export health. */
  health: HealthReport | undefined;
  /** What the instance runs, once the SDK is built (`instrument`): reported as its available components. */
  inventory: InventoryInit | undefined;
  /** The signals the application registered a global provider of its own for (watchProviders). */
  readonly ownProviders = new Set<ApiSignal>();
  /** The environment the host is read from. */
  #env: NodeJS.ProcessEnv = {};

  get on(): boolean {
    return this.setup !== undefined;
  }

  /** Before the SDK is built: load the saved settings, put them in force, name the instance. */
  prepare(env: NodeJS.ProcessEnv = process.env, options: Options = {}): void {
    // A register after an earlier one's shutdown: that run's client stopped, this one's starts.
    this.#stopping = undefined;
    this.client = undefined;
    this.#run++;
    this.setup = setupFromEnv(env, options);
    if (!this.setup) return;
    this.layer = new SdkSettings();
    this.#env = env;
    this.ownProviders.clear();
    this.host = hostFromEnv(env);
    let saved: Uint8Array | undefined;
    let coreState: Uint8Array | undefined;
    const dir = this.setup.stateDir;
    if (dir) {
      try {
        mkdirSync(dir, { recursive: true });
        // The settings to start with are the last config applied, which the client keeps in its
        // state (opamp-state); sdk-settings.json is what this package kept itself before, read
        // only while the client's state holds no config applied.
        const state = join(dir, "opamp-state");
        if (existsSync(state)) coreState = readFileSync(state);
        const path = join(dir, "sdk-settings.json");
        if (existsSync(path)) saved = readFileSync(path);
      } catch (e) {
        say.warn(`OpAMP: saved SDK settings unreadable, using the SDK's own: ${String(e)}`);
      }
    }
    this.loaded = this.layer.load(saved, this.host, BigInt(this.now()), coreState);
    if (this.loaded.warning) say.warn(`OpAMP: ${this.loaded.warning}`);
    this.current = this.loaded.current;
    this.effective = this.loaded.effective;
    this.controls.apply(this.loaded.settings);
    this.uid = this.#identity(env);
    this.#refreshAfter(this.loaded.refreshAtUnixMs);
  }

  /** When the layer said to call Refresh next (wall clock, ms since the epoch; 0: never). */
  get refreshAt(): number {
    return this.#refreshAt;
  }

  /**
   * The layer's refresh_at_unix_ms: the one timer armed for it, in place of any earlier one. The
   * time left is taken from the wall clock once, then waited for on a monotonic clock, so the wall
   * clock moving meanwhile doesn't move it.
   */
  #refreshAfter(at: bigint): void {
    this.#refreshAt = Number(at);
    this.#refreshDue = this.#refreshAt ? this.elapsed() + Math.max(0, this.#refreshAt - this.now()) : 0;
    this.arm();
  }

  /** (Re)arm the agent's one timer, for the next thing it waits for; none: no timer. */
  arm(): void {
    if (this.#timer !== undefined) this.timers.clear(this.#timer);
    this.#timer = undefined;
    const generation = ++this.#generation;
    const due = this.#due();
    if (due === undefined || this.#stopping) return;
    // At most setTimeout's longest wait: a timer that fires early finds nothing due, and re-arms.
    const wait = Math.min(Math.max(0, due - this.elapsed()), 0x7fffffff);
    this.#timer = this.timers.set(() => this.#tick(generation), wait);
  }

  /** The monotonic time the timer is next due at. */
  #due(): number | undefined {
    const due = [this.#refreshAt ? this.#refreshDue : 0, this.#healthDue].filter((t) => t > 0);
    return due.length ? Math.min(...due) : undefined;
  }

  #tick(generation: number): void {
    // Armed before a later decision, which armed its own: nothing to do.
    if (generation !== this.#generation) return;
    this.#timer = undefined;
    context.with(suppressTracing(context.active()), () => {
      if (this.#refreshAt && this.elapsed() >= this.#refreshDue) {
        // Due by the monotonic clock: at least the time it was due at, whatever the wall clock says.
        this.refresh(Math.max(this.now(), this.#refreshAt));
      }
      if (this.#healthDue && this.elapsed() >= this.#healthDue) {
        this.#healthDue = this.elapsed() + HEALTH_EVERY_MS;
        this.reportHealth();
      }
      this.arm();
    });
  }

  /** From now on, export health every 30 s, on the same timer as the end of debug mode. */
  watchHealth(): void {
    this.#healthDue = this.elapsed() + HEALTH_EVERY_MS;
    this.arm();
  }

  /**
   * Export health, from what the gates counted: the layer says whether to send it, and builds the
   * `exporters` entry of the agent's health. The top-level `healthy` stays the application's, which
   * this package doesn't judge: true. Returns the agent's health, as sent or held.
   */
  reportHealth(): ComponentHealth {
    const c = this.controls.counts;
    const previous = this.health;
    this.health = this.layer!.health(
      { traces: c.traces.message, metrics: c.metrics.message, logs: c.logs.message },
      previous,
      BigInt(this.now()),
      this.controls.settings,
    );
    const health = create(ComponentHealthSchema, {
      healthy: true,
      ...(this.health.exporters ? { componentHealthMap: { exporters: this.health.exporters } } : {}),
    });
    if (this.health.send) {
      const e = this.health.exporters;
      if (e && e.status !== previous?.exporters?.status) {
        diag.info(`OpAMP: export health: ${e.status}${e.lastError ? ` (${e.lastError})` : ""}`);
      }
      this.client?.setHealth(health);
    }
    return health;
  }

  /**
   * The time the layer said to ask again has come (the end of debug mode): what is in force now,
   * put in force and reported, with or without the server.
   */
  refresh(now = this.now()): void {
    const layer = this.layer;
    if (!layer) return;
    const r = layer.refresh(this.current, this.loaded?.current ?? new Uint8Array(), BigInt(now), this.host);
    if (r.warning) say.warn(`OpAMP: ${r.warning}`);
    this.controls.apply(r.settings);
    const before = this.#refreshAt;
    this.effective = r.effective;
    if (this.effective) this.client?.setEffectiveConfig(this.effective);
    this.#refreshAfter(r.refreshAtUnixMs);
    if (before && !this.#refreshAt) diag.info("OpAMP: debug mode is over: the settings sent before it are in force");
  }

  /**
   * The application registered a global provider of its own for `signal`, in place of the SDK's:
   * what it sends through the API goes past this package's sampler (traces) and gates. The
   * settings held there are declared no more. What is in force is decided again through the
   * layer, put in force and reported at once, so those settings are named as set aside from now
   * on rather than left answered APPLIED.
   */
  providerOfItsOwn(signal: ApiSignal): void {
    if (!this.layer || this.ownProviders.has(signal)) return;
    this.ownProviders.add(signal);
    const was = new Set(this.host.live ?? []);
    this.host = hostFromEnv(this.#env, this.ownProviders);
    const lost = [...was].filter((k) => !this.host.live?.includes(k));
    if (lost.length === 0) return;
    say.warn(`OpAMP: the application registered a ${PROVIDER[signal]} provider of its own, which exports past `
      + `this package: ${lost.join(", ")} can't be carried out`);
    // Called from the application's code: what the client sends isn't the application's to trace.
    context.with(suppressTracing(context.active()), () => {
      this.refresh();
      const components = this.components();
      if (components) this.client?.setAvailableComponents(components);
    });
  }

  /**
   * Once the SDK has started: the instrumentations it loaded, under the server, switched as the
   * settings in force say; and the inventory, of them and the exporters `env` builds.
   */
  instrument(instrumentations: Instrumentation[], env: NodeJS.ProcessEnv = process.env): void {
    this.controls.instrument(instrumentations);
    this.inventory = inventory(instrumentations, env);
    this.#unloaded();
  }

  /**
   * What the instance runs, as its available components (the hash left to the client), on the
   * host and clock the settings are decided on. Nothing it lists changes while it runs: the
   * instrumentations and exporters are the SDK's from its start, the host is read once.
   */
  components(): AvailableComponents | undefined {
    if (!this.layer || !this.inventory) return undefined;
    return this.layer.components(this.inventory, this.host, BigInt(this.now()));
  }

  #unloaded(): void {
    const names = this.controls.unloaded;
    if (names.length > 0) diag.info(`OpAMP: no instrumentation loaded by these names, nothing to switch off: ${names.join(", ")}`);
  }

  /**
   * The SDK's resource, named by the instance UID. It goes to the SDK, not into
   * OTEL_RESOURCE_ATTRIBUTES: a child process inherits the environment, and would take the UID.
   */
  resource(detected: Resource): Resource {
    return this.uid ? detected.merge(resourceFromAttributes({ [INSTANCE_ID]: this.uid })) : detected;
  }

  /**
   * The instance UID, as the core's Setup decides it: the one saved in the state directory; else a
   * new one, saved there; else, where none can be kept (no state directory, or one it can't be
   * saved in), a UUID service.instance.id given in OTEL_RESOURCE_ATTRIBUTES, the same at every
   * start; else a new one. What is here is the I/O: the file, the entropy and the clock.
   */
  #identity(env: NodeJS.ProcessEnv): string {
    const dir = this.setup?.stateDir;
    const path = dir ? join(dir, SETUP_UID_FILE) : undefined;
    let saved = "";
    if (path) {
      try {
        saved = readFileSync(path, "utf8");
      } catch {
        // None saved yet.
      }
    }
    let id;
    try {
      id = identifySetup(create(SetupIdentitySchema, {
        stateDir: path !== undefined, saved, resourceAttributes: env.OTEL_RESOURCE_ATTRIBUTES ?? "",
        entropy: randomBytes(16), unixMs: BigInt(Date.now()),
      }));
    } catch (e) {
      say.warn(`OpAMP: the core could not choose the instance UID (${String(e)}): this start is a new instance`);
      return text(randomBytes(16));
    }
    if (path && id.save) {
      try {
        write(path, id.text);
      } catch (e) {
        say.warn(`OpAMP: can't save the instance UID: ${String(e)}`);
        return text(id.unsavedUid);
      }
    }
    return text(id.instanceUid);
  }

  /** Once the SDK is built: join the fleet. */
  start(resource: Resource): void {
    const s = this.setup;
    const layer = this.layer;
    const uid = this.uid;
    // Stopped first (a SIGTERM while the resource was being detected): don't start at all.
    if (!s || !layer || !uid || this.#stopping) return;
    const heartbeat = s.heartbeatMs;
    this.reachability = new Reachability(s.shown, String(resource.attributes["service.name"] ?? ""), uid, s.hints);
    this.reachability.inUse = () => this.client?.endpoint;
    try {
      const availableComponents = this.components();
      // The client's own requests aren't the application's: everything it starts (its timers,
      // its requests) runs in this context, so no instrumentation traces them.
      this.client = context.with(suppressTracing(context.active()), () => Client.start(
        {
          endpoint: s.endpoint,
          instanceUid: uuidBytes(uid),
          capabilities: BigInt(AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig),
          headers: s.headers,
          // A key of its own, offered by the server: taken with a state directory (stateFile,
          // below) and kept there; without one the client neither declares nor takes it.
          acceptConnectionSettings: true,
          ...(heartbeat ? { heartbeatIntervalMs: heartbeat } : {}),
          ...(s.stateDir ? { stateFile: join(s.stateDir, "opamp-state") } : {}),
          // Platform mode, asked for by name: the client's requests go through fetch and ws, which
          // is what the suppressed context above keeps out of the application's traces.
          mode: "platform",
        },
        {
          description: layer.describe(attributes(resource), this.host, BigInt(this.now())),
          health: this.reportHealth(),
          ...(this.effective ? { effectiveConfig: this.effective } : {}),
          ...(availableComponents ? { availableComponents } : {}),
        },
        (n) => this.#notified(n),
      ));
      this.watchHealth();
    } catch (e) {
      say.error(`OpAMP: the client did not start; the SDK runs unmanaged: ${String(e)}`);
    }
  }

  /**
   * The setup failed after prepare: the agent does nothing more. Its timer is cleared and never
   * armed again, the client never starts, and the default state directory's slot is given up.
   */
  abandon(): void {
    void this.stop();
    if (this.setup?.stateLock) release(this.setup.stateLock);
    this.setup = undefined;
    this.layer = undefined;
  }

  /**
   * The server hears the agent go (agent_disconnect); then the SDK may flush. Once the client has
   * stopped, the default state directory's slot is given up: a register after this, in the same
   * process, takes it back, and with it the same identity.
   */
  stop(): Promise<void> {
    if (this.#timer !== undefined) this.timers.clear(this.#timer);
    this.#timer = undefined;
    const lock = this.setup?.stateLock;
    const run = this.#run;
    const client = this.client;
    // Called from the application's context (a signal handler): the goodbye is the agent's too.
    this.#stopping ??= context
      .with(suppressTracing(context.active()), () => client?.stop() ?? Promise.resolve())
      .catch(() => {})
      // A save the client left writing (a config applied just before) lands first: a register
      // after this one, in the same process, reads the settings to start with from that state,
      // before its client starts. (The write keeps Node.js running until it is done anyway.)
      .then(() => client?.settled())
      .then(() => {
        // Unless a later prepare (after abandon gave it up at once) holds the same slot again.
        if (lock && (this.#run === run || this.setup?.stateLock !== lock)) release(lock);
      });
    return this.#stopping;
  }

  #notified(n: Notification): void {
    this.reachability?.notified(n);
    if (n.kind === "remoteConfig" && this.client) {
      const answer = this.remoteConfig(n.remoteConfig);
      if (this.effective) this.client.setEffectiveConfig(this.effective);
      this.client.setRemoteConfigStatus(answer);
    } else if (n.kind === "instanceUidChanged") {
      this.instanceUidChanged(n.instanceUid);
    } else if (n.kind === "stopped" && n.error !== undefined) {
      say.error(`OpAMP: the client stopped; the SDK runs unmanaged: ${String(n.error)}`);
    }
  }

  /**
   * The server assigned the instance another UID: the next start uses it, as the OpAMP instance
   * UID and as service.instance.id (this run's resource is already the SDK's).
   */
  instanceUidChanged(bytes: Uint8Array): void {
    const uid = text(bytes);
    if (uid === this.uid) return;
    this.uid = uid;
    const dir = this.setup?.stateDir;
    if (!dir) return;
    try {
      write(join(dir, SETUP_UID_FILE), uid);
      diag.info(`OpAMP: the server named this instance ${uid}; service.instance.id follows at the next start`);
    } catch (e) {
      say.warn(`OpAMP: can't save the instance UID the server assigned: ${String(e)}`);
    }
  }

  /** Carry out the layer's decision on a remote config; the answer for the server. */
  remoteConfig(config: AgentRemoteConfig): RemoteConfigStatus {
    const s = this.setup!;
    const now = this.now();
    const d = this.layer!.decide(
      config,
      this.current,
      this.loaded?.current ?? new Uint8Array(),
      this.host,
      BigInt(now),
      // With a state directory the client keeps the config this applies, in its own state, and
      // tells the server APPLIED once that is saved: nothing for this package to save. Without
      // one, nothing is kept, and a next-start change is refused.
      s.stateDir === undefined ? undefined : () => {},
    );
    if (d.apply) {
      this.controls.apply(d.apply);
      this.#unloaded();
    }
    if (d.pendingRestart.length > 0) {
      diag.info(`OpAMP: ${d.pendingRestart.join(", ")} changed; in force when the application restarts`);
    }
    this.current = d.current;
    this.effective = d.effective;
    if (!d.apply && this.#refreshAt && now >= this.#refreshAt) {
      // FAILED, so nothing changes, but what is reported is now's: once the wall clock has passed
      // the end of debug mode (a machine that slept, before the timer went off), it ends here, as
      // the report says.
      this.refresh(now);
    } else {
      // Applied or not, when to call Refresh next: a refused config leaves the window as it was.
      this.#refreshAfter(d.refreshAtUnixMs);
    }
    return d.status!;
  }
}

export const AGENT = new Agent(appSettings);

/**
 * What `register` does: NodeSDK with every auto-instrumentation, under the server (`options`, then
 * OTEL_OPAMP_*). Returns the SDK, started, and a function that stops the client and then the SDK.
 */
export function register(
  options: Options = {},
  env: NodeJS.ProcessEnv = process.env,
): { sdk: NodeSDK; shutdown: () => Promise<void> } {
  // As upstream's register: OTEL_LOG_LEVEL's logger from the start, so what is said before NodeSDK
  // (which sets its own) is heard.
  const logLevel = str(env, "OTEL_LOG_LEVEL");
  say.console = logLevel === undefined;
  if (logLevel !== undefined) diag.setLogger(new DiagConsoleLogger(), { logLevel: diagLogLevelFromString(logLevel) });
  // A register that fails (the SDK can't be built) leaves what appSettings reads as it was, the
  // saved values Load put in force taken back: an application that catches it runs unmanaged.
  return preparing(AGENT, appSettings, env, () => registered(env, logLevel), options);
}

/**
 * `agent` prepared from `options`, then `env`, and then `build`'s result. If either throws, nothing
 * prepared outlives it: the agent is abandoned (its timer cleared, so no Refresh puts the saved
 * settings back in force later) and `app` reads what it read before.
 */
export function preparing<T>(
  agent: Agent,
  app: AppSettings,
  env: NodeJS.ProcessEnv,
  build: () => T,
  options: Options = {},
): T {
  return settingsKeptOnFailure(app, () => {
    try {
      agent.prepare(env, options);
      return build();
    } catch (e) {
      agent.abandon();
      throw e;
    }
  });
}

/**
 * `fn`'s result; if it throws, `app` is put back to the values in force before, and the error
 * goes on.
 */
export function settingsKeptOnFailure<T>(app: AppSettings, fn: () => T): T {
  const before = app.snapshot();
  try {
    return fn();
  } catch (e) {
    app.put(before);
    throw e;
  }
}

function registered(env: NodeJS.ProcessEnv, logLevel: string | undefined): { sdk: NodeSDK; shutdown: () => Promise<void> } {
  // Every instrumentation the variables load: the server switches them while they run.
  const instrumentations = getNodeAutoInstrumentations();
  const resource = AGENT.resource(defaultResource().merge(detectResources({ detectors: getResourceDetectors() })));
  if (!AGENT.on) {
    if (logLevel !== undefined) diag.disable(); // NodeSDK sets it again, without a warning
    const sdk = new NodeSDK({ instrumentations, resource, autoDetectResources: false });
    sdk.start();
    return { sdk, shutdown: () => sdk.shutdown() };
  }
  const c = AGENT.controls;
  const built = exportersFromEnv(env);
  const spanProcessors: SpanProcessor[] | undefined = built.traces?.map((e) =>
    e instanceof ConsoleSpanExporter ? new SimpleSpanProcessor(c.spans(e)) : new BatchSpanProcessor(c.spans(e), batchSpanOptions(env)),
  );
  // As NodeSDK: the variables for OTLP; console at the reader's defaults.
  const metricReaders: IMetricReader[] | undefined = built.metrics?.map((e) => {
    const options: PeriodicExportingMetricReaderOptions = { exporter: c.metrics(e) };
    return new PeriodicExportingMetricReader(
      e instanceof ConsoleMetricExporter ? options : { ...options, ...metricReaderOptions(env) },
    );
  });
  const logRecordProcessors: LogRecordProcessor[] | undefined = built.logs?.map((e) =>
    e instanceof ConsoleLogRecordExporter
      ? new SimpleLogRecordProcessor({ exporter: c.logs(e) })
      : new BatchLogRecordProcessor({ exporter: c.logs(e), ...batchLogOptions(env) }),
  );
  if (logLevel !== undefined) diag.disable(); // NodeSDK sets it again, without a warning
  const sdk = new NodeSDK({
    instrumentations,
    resource,
    autoDetectResources: false,
    sampler: c.sampler.wrap(samplerFromEnv(env)),
    ...(spanProcessors ? { spanProcessors } : {}),
    ...(metricReaders ? { metricReaders } : {}),
    ...(logRecordProcessors ? { logRecordProcessors } : {}),
  });
  watchProviders(AGENT, () => sdk.start());
  AGENT.instrument(instrumentations, env);
  const ready = resource.waitForAsyncAttributes?.() ?? Promise.resolve();
  void ready
    .catch((e: unknown) => say.warn(`OpAMP: resource detection failed; describing the agent without it: ${String(e)}`))
    .then(() => AGENT.start(resource))
    .catch((e: unknown) => say.error(`OpAMP: the client did not start; the SDK runs unmanaged: ${String(e)}`));
  return {
    sdk,
    shutdown: async () => {
      await AGENT.stop();
      await sdk.shutdown();
    },
  };
}
