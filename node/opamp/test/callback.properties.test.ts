// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The callback contract (spec/shell-contract.md), as a model test over random runs
// (fast-check's fc.commands with asyncModelRun), against the real core and a local server: start,
// with a saved remote config or none; application calls from outside, from another task and from
// inside the callback; a callback that throws on its k-th call; stop from outside, or requested
// from inside the callback; the server offering a remote config, a custom message or a heartbeat
// interval. After each step the notifications seen so far are checked against the contract: one
// callback at a time, none before start returned, none after stopped, stopped exactly once (with
// the error when a callback threw), stop bounded and giving that error, and the order the model
// can predict.
//
// Node is single-threaded: "from another thread" is from another task (a macrotask or a
// microtask), and two callbacks at once could only be a callback re-entered from a call it made.
//
// Each test runs in both modes (MODES): Node mode (fetch and ws), and Rust mode (the core's own
// HTTP and WebSocket, over Node's sockets), against the same server.
//
// OPAMP_MODEL_RUNS runs per mode (30 by default), both modes from one seed; the seed is printed,
// and OPAMP_MODEL_SEED replays it (with OPAMP_MODEL_PATH, fast-check's path, to go straight to the
// failing run, and --test-name-pattern to pick the mode: a failure prints the whole line).

import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

import fc from "fast-check";
import { type WebSocket, WebSocketServer } from "ws";

import {
  type AgentToServer,
  AgentToServerSchema,
  Client,
  ComponentHealthSchema,
  create,
  CustomCapabilitiesSchema,
  CustomMessageSchema,
  fromBinary,
  type Notification,
  RemoteConfigStatusSchema,
  type ServerToAgent,
  ServerToAgentSchema,
  toBinary,
} from "../src/index.js";
import { annotatedTest } from "./annotate.js";

const UID = new Uint8Array(16).fill(0x87);
const CAPABILITY = "io.example.model";
const UNDECLARED = "io.example.undeclared";
const ACCEPTS_REMOTE_CONFIG = 2n;
const SERVER_CAPABILITIES = 1n | 2n; // AcceptsStatus, OffersRemoteConfig
const REQUEST_TIMEOUT_MS = 2000;
/** Stop is over within the core's stop timeout (5 s): this is it, with a margin. */
const STOP_BOUND_MS = REQUEST_TIMEOUT_MS + 3000;
const HEARTBEAT_MS = 50;

/** The modes every test runs in. */
const MODES = ["node", "rust"] as const;
type Mode = (typeof MODES)[number];

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));
const settle = () => sleep(15);

async function body(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return new Uint8Array(Buffer.concat(chunks));
}

type OfferKind = "config" | "custom" | "heartbeat";

/** The local server, over HTTP and WebSocket (properties.test.ts's): it answers every message,
 * and carries the offers the steps queue, one per answer (pushed at once on an open connection). */
class OpampServer {
  readonly http: Server;
  readonly wss: WebSocketServer;
  readonly got: AgentToServer[] = [];
  /** Open connections, and how many messages each carried. */
  readonly conns = new Map<WebSocket, number>();
  /** Offers not yet sent. */
  readonly pending: ServerToAgent[] = [];
  configs = 0;
  customs = 0;
  heartbeats = 0;
  port = 0;

  constructor() {
    this.http = createServer(async (req, res) => {
      const msg = fromBinary(AgentToServerSchema, await body(req));
      res.setHeader("Content-Type", "application/x-protobuf");
      res.end(toBinary(ServerToAgentSchema, this.#answer(msg)));
    });
    this.wss = new WebSocketServer({ server: this.http });
    this.wss.on("connection", (conn) => {
      this.conns.set(conn, 0);
      conn.on("close", () => this.conns.delete(conn));
      conn.on("message", (data: Buffer) => {
        this.conns.set(conn, (this.conns.get(conn) ?? 0) + 1);
        const msg = fromBinary(AgentToServerSchema, new Uint8Array(data.subarray(1)));
        conn.send(frame(this.#answer(msg)));
      });
    });
  }

  async listen(): Promise<void> {
    await new Promise<void>((r) => this.http.listen(0, "127.0.0.1", r));
    this.port = (this.http.address() as AddressInfo).port;
  }

  close(): void {
    for (const c of this.wss.clients) c.terminate();
    this.wss.close();
    this.http.closeAllConnections();
    this.http.close();
  }

  #answer(msg: AgentToServer): ServerToAgent {
    this.got.push(msg);
    const out = this.pending.shift() ?? create(ServerToAgentSchema);
    out.instanceUid = msg.instanceUid;
    out.capabilities = SERVER_CAPABILITIES;
    return out;
  }

  /** Queue an offer: its number (the config's hash, the message's data, the seconds offered). */
  offer(kind: OfferKind): number {
    let m: ServerToAgent;
    let n: number;
    if (kind === "config") {
      n = ++this.configs;
      m = create(ServerToAgentSchema, {
        remoteConfig: { config: { configMap: { "": { body: new Uint8Array([n]) } } }, configHash: new Uint8Array([n]) },
      });
    } else if (kind === "custom") {
      n = ++this.customs;
      m = create(ServerToAgentSchema, { customMessage: { capability: CAPABILITY, type: "m", data: new Uint8Array([n]) } });
    } else {
      // 1 s and 0 in turn: each is a change (over HTTP, 0 is the configured poll interval).
      n = this.heartbeats++ % 2 === 0 ? 1 : 0;
      m = create(ServerToAgentSchema, { connectionSettings: { opamp: { heartbeatIntervalSeconds: BigInt(n) } } });
    }
    // Pushed on the client's connection: open, and carrying its messages (not one left open
    // after stop, S1).
    const conn = [...this.conns].find(([c, carried]) => carried > 0 && c.readyState === c.OPEN)?.[0];
    if (conn) {
      m.instanceUid = UID;
      m.capabilities = SERVER_CAPABILITIES;
      conn.send(frame(m));
    } else {
      this.pending.push(m);
    }
    return n;
  }
}

const frame = (m: ServerToAgent): Buffer => Buffer.concat([Buffer.from([0]), toBinary(ServerToAgentSchema, m)]);

/** What a stop gave. */
type Outcome = { ok: true } | { ok: false; error: unknown } | { timeout: true };

async function outcome(p: Promise<void>, boundMs: number): Promise<Outcome & { ms: number }> {
  const t0 = performance.now();
  let timer: NodeJS.Timeout | undefined;
  const r = await Promise.race<Outcome>([
    p.then(
      () => ({ ok: true }) as const,
      (error: unknown) => ({ ok: false, error }) as const,
    ),
    new Promise<Outcome>((res) => {
      timer = setTimeout(() => res({ timeout: true }), boundMs);
    }),
  ]);
  clearTimeout(timer);
  return { ...r, ms: performance.now() - t0 };
}

/** One client's life, as its callback saw it. */
class Life {
  client: Client | undefined;
  returned = false;
  readonly notes: Notification[] = [];
  readonly problems: string[] = [];
  depth = 0;
  calls = 0;
  /** The callback throws on this call (1 = the first). */
  throwAt: number | undefined;
  /** The first value thrown, and what it was thrown on. */
  thrown: { value: unknown; on: Notification["kind"] } | undefined;
  /** What the next callback does. */
  readonly inside: ("call" | "stop")[] = [];
  readonly stops: Promise<void>[] = [];
  /** The hash of the saved remote config handed over first, if any. */
  saved: number | undefined;
  /** The first message of this life at the server. */
  readonly since: number;
  stopped = false;

  constructor(
    readonly world: World,
    readonly what: string,
  ) {
    this.since = world.server.got.length;
  }

  notify = (n: Notification): void => {
    this.calls++;
    if (!this.returned) this.problems.push(`a callback (${n.kind}) before start returned (RULE-SHELL-01)`);
    if (this.depth > 0) this.problems.push(`two callbacks at once: ${n.kind} inside another (RULE-SHELL-01)`);
    if (this.notes.some((s) => s.kind === "stopped")) this.problems.push(`a callback (${n.kind}) after stopped (RULE-SHELL-01)`);
    this.notes.push(n);
    this.depth++;
    try {
      const client = this.client;
      if (client === undefined) throw new Error("the callback has no client handle");
      // Answer every config from inside the callback, through the handle it was given.
      if (n.kind === "remoteConfig") {
        client.setRemoteConfigStatus(
          create(RemoteConfigStatusSchema, { lastRemoteConfigHash: n.config.configHash, status: 1 }),
        );
      }
      for (const a of this.inside.splice(0)) {
        if (a === "call") {
          // Both kinds: the second calls back at once if it reaches the core inside this step.
          this.world.call(client);
          this.world.call(client);
        }
        else {
          // Requested, not waited for: waiting here would deadlock.
          const p = client.stop();
          p.catch(() => {});
          this.stops.push(p);
        }
      }
      if (this.calls === this.throwAt) {
        const value = this.world.failure();
        this.thrown ??= { value, on: n.kind };
        throw value;
      }
    } finally {
      this.depth--;
    }
  };

  /** Every check the model can make now. */
  check(final: boolean): void {
    const log = () => `${this.what}: ${this.notes.map(describe).join(", ")}`;
    assert.deepEqual(this.problems, [], log());
    const stopped = this.notes.filter((n) => n.kind === "stopped");
    assert.ok(stopped.length <= 1, `stopped more than once (RULE-SHELL-02): ${log()}`);
    if (stopped.length === 1) assert.equal(this.notes.at(-1)?.kind, "stopped", `stopped is the last word: ${log()}`);
    if (final) assert.equal(stopped.length, 1, `stopped exactly once (RULE-SHELL-02): ${log()}`);
    const s = stopped[0];
    if (this.thrown && this.thrown.on !== "stopped") {
      // The throw stops the client at once, with the error.
      assert.ok(s?.kind === "stopped", `a callback threw ${String(this.thrown.value)}: stopped follows (RULE-SHELL-02): ${log()}`);
      assert.ok(s.error === this.thrown.value, `stopped with the error thrown (RULE-SHELL-02): ${log()}`);
    } else if (s?.kind === "stopped") {
      assert.equal(s.error, undefined, `stopped without an error, none thrown: ${log()}`);
    }
    // The order the model knows.
    if (this.saved !== undefined && this.notes.length > 0) {
      const first = this.notes[0]!;
      assert.ok(
        first.kind === "remoteConfig" && first.config.configHash[0] === this.saved,
        `the saved remote config ${this.saved} is handed over first: ${log()}`,
      );
    }
    const increasing = (xs: number[], what: string) =>
      xs.forEach((x, i) => i === 0 || assert.ok(x > xs[i - 1]!, `${what} in order: ${log()}`));
    increasing(
      this.notes.flatMap((n) => (n.kind === "remoteConfig" ? [n.config.configHash[0]!] : [])),
      "remote configs",
    );
    increasing(
      this.notes.flatMap((n) => (n.kind === "customMessage" ? [n.message.data[0]!] : [])),
      "custom messages",
    );
    increasing(
      this.notes.flatMap((n) =>
        n.kind === "customMessageNotSent" ? [Number(new TextDecoder().decode(n.message.data))] : [],
      ),
      "refused custom messages",
    );
    const beats = this.notes.flatMap((n) => (n.kind === "heartbeatInterval" ? [n.ms] : []));
    beats.forEach((b, i) => {
      assert.ok([0, 1000, HEARTBEAT_MS].includes(b), `a heartbeat interval offered: ${log()}`);
      assert.ok(i === 0 || b !== beats[i - 1], `a heartbeat interval only when it changes: ${log()}`);
    });
    // Calls from one caller reach the core in order: the healths the server saw only go up.
    const healths = this.world.server.got
      .slice(this.since)
      .flatMap((m) => (m.health?.status.startsWith("h") ? [Number(m.health.status.slice(1))] : []));
    healths.forEach((h, i) => i === 0 || assert.ok(h >= healths[i - 1]!, `healths in the order set: ${healths}`));
  }
}

function describe(n: Notification): string {
  switch (n.kind) {
    case "remoteConfig":
      return `remoteConfig(${n.config.configHash[0]})`;
    case "customMessage":
      return `customMessage(${n.message.data[0]})`;
    case "customMessageNotSent":
      return `customMessageNotSent(${new TextDecoder().decode(n.message.data)})`;
    case "heartbeatInterval":
      return `heartbeatInterval(${n.ms})`;
    case "stopped":
      return n.error === undefined ? "stopped" : `stopped(${String(n.error)})`;
    default:
      return n.kind;
  }
}

class World {
  readonly server = new OpampServer();
  readonly dir = mkdtempSync(join(tmpdir(), "opamp-model-"));
  life: Life | undefined;
  lives = 0;
  healths = 0;
  failures = 0;

  constructor(
    readonly mode: Mode,
    readonly ws: boolean,
    readonly log: (msg: string) => void,
  ) {}

  get stateFile(): string {
    return join(this.dir, "state.bin");
  }

  config(): Parameters<typeof Client.start>[0] {
    return {
      endpoint: `${this.ws ? "ws" : "http"}://127.0.0.1:${this.server.port}/v1/opamp`,
      instanceUid: UID,
      capabilities: ACCEPTS_REMOTE_CONFIG,
      heartbeatIntervalMs: HEARTBEAT_MS,
      requestTimeoutMs: REQUEST_TIMEOUT_MS,
      stateFile: this.stateFile,
      mode: this.mode,
      // Rust mode reads HTTP_PROXY and the like: the local server is reached directly.
      proxy: false,
    };
  }

  status() {
    return {
      health: create(ComponentHealthSchema, { healthy: true, status: "h0" }),
      customCapabilities: create(CustomCapabilitiesSchema, { capabilities: [CAPABILITY] }),
    };
  }

  /** An application call, numbered: a health (the server sees them go up), or a custom message
   * for a capability not declared, which the core refuses at once with customMessageNotSent: a
   * notification in the step of the call, so a call made from the callback that reached the core
   * before the callback returned would call back inside it. */
  call(client: Client): void {
    const n = ++this.healths;
    if (n % 2 === 1) client.setHealth(create(ComponentHealthSchema, { healthy: true, status: `h${n}` }));
    else client.sendCustomMessage(create(CustomMessageSchema, { capability: UNDECLARED, type: "x", data: new TextEncoder().encode(String(n)) }));
  }

  /** What a callback throws: an Error, or not one (JavaScript throws anything). */
  failure(): unknown {
    const n = ++this.failures;
    return n % 3 === 0 ? `a string thrown (${n})` : n % 3 === 1 ? new Error(`the callback failed (${n})`) : { thrown: n };
  }

  /** A state file holding remote config `n`: a client gets it from the server and saves it. */
  async saveConfig(): Promise<number> {
    rmSync(this.stateFile, { force: true });
    let handed = false;
    const helper = Client.start(this.config(), this.status(), (n) => {
      if (n.kind === "remoteConfig") handed = true;
    });
    const n = this.server.offer("config");
    for (let i = 0; i < 300 && !handed; i++) await sleep(10);
    assert.ok(handed, "the helper got the remote config");
    // Saved in the step that handed it over.
    await helper.stop();
    return n;
  }

  async start(saved: boolean, throwOn: number, stopNow: boolean): Promise<void> {
    // Offers the last life didn't take (it had stopped) are not this life's.
    this.server.pending.length = 0;
    const savedHash = saved ? await this.saveConfig() : undefined;
    if (!saved) rmSync(this.stateFile, { force: true });
    const life = new Life(this, `life ${++this.lives} (saved=${saved}, throwOn=${throwOn}, stopNow=${stopNow})`);
    life.saved = savedHash;
    life.throwAt = throwOn > 0 ? throwOn : undefined;
    this.life = life;
    const client = Client.start(this.config(), this.status(), life.notify);
    life.client = client;
    life.returned = true;
    if (stopNow) {
      // Stop before the first step has run (RULE-SHELL-07).
      const p = client.stop();
      p.catch(() => {});
      life.stops.push(p);
    }
    await settle();
    if (saved) for (let i = 0; i < 100 && life.notes.length === 0; i++) await sleep(10);
    life.check(false);
  }

  /** Stop from outside, waiting: within the bound, giving the error thrown (RULE-SHELL-02, -07). */
  async stop(): Promise<void> {
    const life = this.life!;
    const client = life.client!;
    const got = await outcome(client.stop(), STOP_BOUND_MS);
    assert.ok(!("timeout" in got), `stop returned within ${STOP_BOUND_MS} ms (RULE-SHELL-07): ${life.what}`);
    const again = await outcome(client.stop(), STOP_BOUND_MS);
    const earlier = await Promise.all(life.stops.map((p) => outcome(p, STOP_BOUND_MS)));
    await settle();
    life.check(true);
    const thrown = life.thrown;
    for (const r of [got, again, ...earlier]) {
      assert.ok(!("timeout" in r), `every stop returns (RULE-SHELL-07): ${life.what}`);
      if (thrown === undefined) {
        assert.ok("ok" in r && r.ok, `stop gives no error, none thrown: ${String((r as { error?: unknown }).error)}`);
      } else if (thrown.on !== "stopped") {
        assert.ok("ok" in r && !r.ok && r.error === thrown.value, `stop gives the error thrown (RULE-SHELL-02): ${life.what}`);
      } else {
        // Thrown on stopped itself: the client had stopped; stop may say so either way.
        assert.ok("ok" in r && (r.ok || r.error === thrown.value), `stop gives nothing or the error: ${life.what}`);
      }
    }
    // Nothing after stop: a call does nothing (and doesn't throw), the server hears nothing more,
    // and every connection is closed.
    const heard = this.server.got.length;
    const told = life.notes.length;
    this.call(client);
    await sleep(30);
    assert.equal(life.notes.length, told, "no callback after stop");
    assert.equal(this.server.got.length, heard, "nothing sent after stop returned (RULE-SHELL-07)");
    for (let i = 0; i < 100 && this.server.conns.size > 0; i++) await sleep(10);
    assert.equal(this.server.conns.size, 0, `every connection closed at stop (RULE-SHELL-07, S1): ${life.what}`);
    const msgs = this.server.got.slice(life.since);
    const bye = msgs.findIndex((m) => m.agentDisconnect !== undefined);
    assert.ok(bye === -1 || bye === msgs.length - 1, "nothing after the goodbye");
    if (thrown !== undefined && thrown.on !== "stopped") assert.equal(bye, -1, "a failed callback ends the client without the goodbye");
    this.life = undefined;
  }

  close(): void {
    this.server.close();
    rmSync(this.dir, { recursive: true, force: true });
  }
}

interface Model {
  /** A client started and not yet stopped from outside. */
  running: boolean;
  /** Some client was started. */
  started: boolean;
  throwArmed: boolean;
}

type Cmd = fc.AsyncCommand<Model, World>;

class Start implements Cmd {
  constructor(
    readonly saved: boolean,
    readonly throwOn: number,
    readonly stopNow: boolean,
  ) {}
  check = (m: Readonly<Model>) => !m.running;
  async run(m: Model, w: World) {
    m.running = true;
    m.started = true;
    m.throwArmed = this.throwOn > 0;
    await w.start(this.saved, this.throwOn, this.stopNow);
  }
  toString = () => `start(saved=${this.saved}, throwOn=${this.throwOn}, stopNow=${this.stopNow})`;
}

class CallOutside implements Cmd {
  constructor(readonly how: "now" | "macrotask" | "microtask") {}
  // Also on a client that has stopped: then the call does nothing.
  check = (m: Readonly<Model>) => m.started;
  async run(_m: Model, w: World) {
    const life = w.life;
    if (life === undefined) return; // between lives: no client to call
    const call = () => w.call(life.client!);
    if (this.how === "now") call();
    else if (this.how === "macrotask") await new Promise<void>((r) => setImmediate(() => (call(), r())));
    else await new Promise<void>((r) => queueMicrotask(() => (call(), r())));
    await settle();
    life.check(false);
  }
  toString = () => `callOutside(${this.how})`;
}

class Inside implements Cmd {
  constructor(readonly what: "call" | "stop") {}
  check = (m: Readonly<Model>) => m.running;
  async run(_m: Model, w: World) {
    w.life!.inside.push(this.what);
    // A callback to do it in: the server's next custom message (if nothing comes first).
    await offer(w, "custom");
  }
  toString = () => `inside(${this.what})`;
}

class ThrowOn implements Cmd {
  constructor(readonly k: number) {}
  check = (m: Readonly<Model>) => m.running && !m.throwArmed;
  async run(m: Model, w: World) {
    m.throwArmed = true;
    const life = w.life!;
    life.throwAt = life.calls + this.k;
    await settle();
    life.check(false);
  }
  toString = () => `throwOn(+${this.k})`;
}

async function offer(w: World, kind: OfferKind): Promise<void> {
  w.server.offer(kind);
  // Over HTTP it goes with the next poll (up to a second, after a heartbeat offer of 1 s).
  for (let i = 0; i < 150 && w.server.pending.length > 0; i++) await sleep(10);
  await settle();
  w.life!.check(false);
}

class Offer implements Cmd {
  constructor(readonly kind: OfferKind) {}
  check = (m: Readonly<Model>) => m.running;
  async run(_m: Model, w: World) {
    await offer(w, this.kind);
  }
  toString = () => `offer(${this.kind})`;
}

class StopOutside implements Cmd {
  check = (m: Readonly<Model>) => m.running;
  async run(m: Model, w: World) {
    m.running = false;
    m.throwArmed = false;
    await w.stop();
  }
  toString = () => "stopOutside";
}

class Wait implements Cmd {
  constructor(readonly ms: number) {}
  check = (m: Readonly<Model>) => m.running;
  async run(_m: Model, w: World) {
    await sleep(this.ms);
    w.life!.check(false);
  }
  toString = () => `wait(${this.ms})`;
}

const commands = [
  fc
    .record({ saved: fc.boolean(), throwOn: fc.constantFrom(0, 0, 1, 2, 3), stopNow: fc.boolean() })
    // A throw on a held notification of a client stopped before start's notifications were
    // delivered is left out: the client stopped before it failed.
    .filter((s) => !(s.stopNow && s.throwOn > 0))
    .map((s) => new Start(s.saved, s.throwOn, s.stopNow)),
  fc.constantFrom("now", "macrotask", "microtask").map((h) => new CallOutside(h)),
  fc.constantFrom("call", "stop").map((w) => new Inside(w)),
  fc.integer({ min: 1, max: 3 }).map((k) => new ThrowOn(k)),
  fc.constantFrom<OfferKind>("config", "custom", "heartbeat").map((k) => new Offer(k)),
  fc.constant(new StopOutside()),
  fc.integer({ min: 5, max: 80 }).map((ms) => new Wait(ms)),
];

/** Every test here prints, when it fails, an annotation with how to replay it (annotate.ts). */
const test = annotatedTest("callback.properties.test.js", () => ({ OPAMP_MODEL_SEED: SEED }));

/** The seed both modes' runs are drawn from: the same plans in each. */
const SEED = process.env.OPAMP_MODEL_SEED ? Number(process.env.OPAMP_MODEL_SEED) : (Date.now() ^ (Math.random() * 2 ** 31)) | 0;

for (const mode of MODES) {
  // covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-07
  test(`${mode} mode: the callback contract holds over random runs (model)`, async (t) => {
    const numRuns = Number(process.env.OPAMP_MODEL_RUNS ?? 30);
    t.diagnostic(`OPAMP_MODEL_SEED=${SEED} (OPAMP_MODEL_RUNS=${numRuns})`);
    const property = fc.asyncProperty(fc.boolean(), fc.commands(commands, { maxCommands: 12 }), async (ws, cmds) => {
      const world = new World(mode, ws, (msg) => t.diagnostic(msg));
      await world.server.listen();
      try {
        await fc.asyncModelRun(() => ({ model: { running: false, started: false, throwArmed: false }, real: world }), cmds);
        if (world.life !== undefined) await world.stop();
      } finally {
        // A failed run may leave its client running: stop it without checking.
        await world.life?.client?.stop().catch(() => {});
        world.close();
      }
    });
    const path = process.env.OPAMP_MODEL_PATH;
    const out = await fc.check(property, { numRuns, seed: SEED, ...(path ? { path } : {}), endOnFailure: true });
    if (out.failed) {
      assert.fail(
        `${fc.defaultReportMessage(out) ?? "failed"}\n${String((out.errorInstance as Error)?.stack ?? out.errorInstance)}\nreplay: OPAMP_MODEL_SEED=${SEED} OPAMP_MODEL_PATH=${out.counterexamplePath} node --test --test-name-pattern="^${mode} mode" dist/test/callback.properties.test.js (in node/opamp, after tsc -p .)`,
      );
    }
  });
}

// Known bug S1, fixed: stopped before its first step has run, the client is still connecting; the
// connection that opens after stop is closed at once.
for (const mode of MODES) {
  // covers: RULE-SHELL-07
  test(`${mode} mode: S1: a WebSocket that opens after stop is closed at once`, async () => {
    const world = new World(mode, true, () => {});
    await world.server.listen();
    try {
      const client = Client.start(world.config(), world.status());
      await client.stop();
      await sleep(300);
      assert.equal(world.server.conns.size, 0, "the connection that opened after stop is closed");
    } finally {
      world.close();
    }
  });
}
