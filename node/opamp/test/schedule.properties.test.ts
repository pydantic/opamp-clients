// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The shell's async edges under fast-check's scheduler: every platform call the client makes
// (fetch, the WebSocket, the TCP sockets of core mode, and the file functions) goes to a fake whose
// every completion and event is a task of fc.scheduler(), so a run picks the order in which
// requests are answered, handshakes end, messages and closes arrive, writes complete and stop
// lands. Nothing real is between the client and its fakes: no socket, no file, no timer (node:test's
// mock timers, and the client's clock from them), so a seed and a path replay a run exactly.
//
// A run (fc.scheduledModelRun over fc.commands) starts one client, real core included, over HTTP or
// WebSocket, in "platform" mode (the executor: fetch and ws) or "core" mode (streams.ts: TCP
// sockets and download files; the fake server then speaks HTTP/1.1 and WebSocket framing itself).
// Its commands are application calls, from outside and from inside the callback; the server's
// offers (remote config, custom message, heartbeat interval, package); the server closing, dropping
// or corrupting the connection; time passing; a callback that throws; stop, from outside or from
// the callback. Between commands the scheduler resolves what it likes. The server's answers come
// from lists the run draws (a request answered, refused or failed; a handshake accepted, refused or
// failed; a download whole, missing, corrupt or cut; a send or a save that fails).
//
// The model is spec/shell-contract.md. At the core's boundary (Core.step, wrapped) every input is
// checked against the effects issued so far: each effect answered at most once, by its kind, naming
// an id issued; WebSocket and stream events only between open and end, nothing about a stream
// after it was closed, nothing about a download file after its discard or cancel; a downloaded file
// is what was written; stored means saved; a timer never early; nothing about an effect after the
// core's cancel of it (RULE-SHELL-06, in both modes); nothing fed after the output that says
// stopped, and no request, connection or download started by it. The callback is held to the
// callback contract (callback.properties.test.ts's model). Once the scheduler is idle every effect
// has its answer. Stop settles within a bound of (mocked) time, and when it does no socket, request,
// download, file handle or file operation is left, and nothing is started afterwards.
//
// OPAMP_SCHED_RUNS runs (500 by default); the seed is printed, and OPAMP_SCHED_SEED replays it. A
// failure prints the line that replays its shrunk run alone: the seed, OPAMP_SCHED_PATH (fast-check's
// path) and OPAMP_SCHED_REPLAY (fc.commands' own replay path), with what happened (the core's inputs
// and effects, the fakes' events) and the scheduler's order. OPAMP_SCHED_DEBUG=1 prints each run's
// setup and commands and what it reached; =2 each run's whole trace and order.
//
// covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-06, RULE-SHELL-07, RULE-SHELL-08, RULE-SHELL-09

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { EventEmitter } from "node:events";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { mock } from "node:test";

import fc from "fast-check";

import { Core } from "../src/core.js";
import { downloadPath, Executor, type ExecutorPlatform, TEST_PLATFORM, type TestPlatform } from "../src/executor.js";
import { type FileOps, type OpenFile, TEST_FILE_OPS, tempOf } from "../src/files.js";
import { EffectSchema, type Input, type Output } from "../src/gen/opamp/core/v1/core_pb.js";
import {
  type AgentToServer,
  AgentToServerSchema,
  Client,
  ComponentHealthSchema,
  type Config,
  create,
  CustomCapabilitiesSchema,
  CustomMessageSchema,
  fromBinary,
  type Notification,
  PackageType,
  RemoteConfigStatusSchema,
  type ServerToAgent,
  ServerToAgentSchema,
  toBinary,
} from "../src/index.js";
import type { StreamsPlatform } from "../src/streams.js";
import { Transport } from "../src/transport.js";
import { annotatedTest } from "./annotate.js";

const UID = new Uint8Array(16).fill(0x92);
const CAPABILITY = "io.example.sched";
const UNDECLARED = "io.example.undeclared";
const ACCEPTS_REMOTE_CONFIG = 2n;
const SERVER_CAPABILITIES = 1n | 2n | 8n; // AcceptsStatus, OffersRemoteConfig, OffersPackages
const HEARTBEAT_MS = 1000;
const REQUEST_TIMEOUT_MS = 5000;
const MAX_RECEIVE = 64 << 10;
/** The core stops within its stop timeout (5 s), and what it cancelled unwinds. In mocked time:
 * the fakes never hang, so this is generous. */
const STOP_BOUND_MS = 5000 + 1000;
const T0 = 1_750_000_000_000;
const ENDPOINT_HOST = "opamp.test";
const DOWNLOAD_HOST = "dl.test";

// ---------------------------------------------------------------------------------------------
// What a run draws.

type Post = "ok" | "500" | "fail";
type Upgrade = "accept" | "refuse" | "fail";
type Fetched = "ok" | "404" | "corrupt" | "cut";
type Sent = "ok" | "fail";
type Saved = "ok" | "fail";
type Connected = "ok" | "refused";
type Written = "ok" | "full";

interface Setup {
  mode: "platform" | "core";
  ws: boolean;
  /** The server's answers, each list taken in turn (and round again). */
  posts: Post[];
  upgrades: Upgrade[];
  downloads: Fetched[];
  sends: Sent[];
  saves: Saved[];
  connects: Connected[];
  writes: Written[];
  /** Core mode: the most bytes a socket hands over at once. */
  chunk: number;
  /** The most bytes a file write takes at once (short writes). */
  fileWrite: number;
  /** Platform mode: the server never answers our Close frame (the core's cancel of the closing
   * handshake ends it). */
  deaf: boolean;
  /** Every file write hangs for STALL_MS of (mocked) time: a disk that stops answering. Only the
   * stop-bound test sets it. */
  stall?: boolean;
}

/** All of stop takes this, from stop() (index.ts STOP_MS: the core's stop_timeout). */
const SHELL_STOP_MS = 5000;

/** How long a stalled write takes. */
const STALL_MS = 60_000;

const lists = <T>(...xs: T[]) => fc.array(fc.constantFrom(...xs), { minLength: 1, maxLength: 6 });

const setupArb: fc.Arbitrary<Setup> = fc.record({
  mode: fc.constantFrom("platform", "core"),
  ws: fc.boolean(),
  posts: lists<Post>("ok", "ok", "ok", "500", "fail"),
  upgrades: lists<Upgrade>("accept", "accept", "accept", "refuse", "fail"),
  downloads: lists<Fetched>("ok", "ok", "404", "corrupt", "cut"),
  sends: lists<Sent>("ok", "ok", "ok", "ok", "fail"),
  saves: lists<Saved>("ok", "ok", "ok", "fail"),
  connects: lists<Connected>("ok", "ok", "ok", "refused"),
  writes: lists<Written>("ok", "ok", "ok", "full"),
  chunk: fc.constantFrom(7, 64, 1000, 1 << 20),
  fileWrite: fc.constantFrom(5, 100, 1 << 20),
  deaf: fc.constantFrom(false, false, false, true),
});

type Lists = { [K in keyof Setup]-?: Setup[K] extends unknown[] ? K : never }[keyof Setup];

/** How many answers a list gives before the network heals. */
const HEAL = 40;
const HEALTHY: { [K in Lists]: Setup[K][number] } = {
  posts: "ok",
  upgrades: "accept",
  downloads: "ok",
  sends: "ok",
  saves: "ok",
  connects: "ok",
  writes: "ok",
};

// ---------------------------------------------------------------------------------------------
// The scheduler's plumbing.

/** The scheduler's seed: each run makes its scheduler from it (scheduler()). fc.scheduler()'s own
 * values can't be used: in fast-check 4.10.2 every clone of one (fast-check runs a clone each time
 * it runs a value again, so at every step of shrinking) draws from one random stream
 * (SchedulerImplem's cloneMethod hands its own sourceTaskSelector to the clone), so each run of a
 * shrunk value orders its tasks differently from the last, and a shrunk counterexample doesn't
 * replay. From a seed it is fresh every time: the same seed, the same order. */
const schedulerArb = fc.noShrink(fc.integer());

/** A fresh scheduler from `seed`. */
const scheduler = (seed: number): fc.Scheduler => fc.sample(fc.scheduler(), { seed, numRuns: 1 })[0]!;

/** Things that happen in order (one socket's events), each when the scheduler picks it. `blocked`
 * holds back data while it says so (a paused socket). */
class Inbox {
  readonly items: { label: string; run: () => void; data: boolean; message: boolean }[] = [];
  #busy = false;

  constructor(
    readonly w: World,
    readonly blocked: () => boolean = () => false,
  ) {}

  push(label: string, run: () => void, opts: { data?: boolean; message?: boolean } = {}): void {
    this.items.push({ label, run, data: opts.data ?? false, message: opts.message ?? false });
    this.pump();
  }

  pump(): void {
    const head = this.items[0];
    if (this.#busy || head === undefined || (head.data && this.blocked())) return;
    this.#busy = true;
    void this.w.s.schedule(Promise.resolve(), head.label).then(() => {
      this.#busy = false;
      if (this.items[0] !== head || (head.data && this.blocked())) return void this.pump();
      this.items.shift();
      this.w.trace.push(`  ${head.label}`);
      this.w.guard(head.label, head.run);
      this.pump();
    });
  }

  /** Drop what hasn't happened yet (keeping what `keep` says). */
  clear(keep: (i: Inbox["items"][number]) => boolean = () => false): void {
    const kept = this.items.filter(keep);
    this.items.length = 0;
    this.items.push(...kept);
    this.pump();
  }
}

// ---------------------------------------------------------------------------------------------
// The files: in memory, every operation a scheduled task.

class Inode {
  data: Uint8Array = new Uint8Array(0);
}

class FakeHandle implements OpenFile {
  closed = false;
  constructor(
    readonly fs: MemFs,
    readonly path: string,
    readonly inode: Inode,
    /** The state file's temporary file: a save's. */
    readonly save: boolean,
  ) {}

  /** What the scheduler's report shows of a create that resolved with this handle. The scheduler
   * stringifies every value a task resolves with, as it resolves; without this it walks the
   * handle (fs, its world, the world's scheduler and so that scheduler's whole report, every
   * earlier handle's string included), so each create's string held all the ones before it: one
   * nightly run (OPAMP_SCHED_RUNS=20000) grew it past V8's string limit and the process died
   * ("Fatal JavaScript invalid size error"). */
  [fc.toStringMethod](): string {
    return `FakeHandle(${JSON.stringify(this.path)})`;
  }

  #append(bufs: readonly Uint8Array[], most: number): number {
    if (this.closed) throw Object.assign(new Error("EBADF: bad file descriptor"), { code: "EBADF" });
    let n = 0;
    const parts: Uint8Array[] = [this.inode.data];
    for (const b of bufs) {
      const take = Math.min(b.length, most - n);
      if (take <= 0) break;
      parts.push(b.subarray(0, take));
      n += take;
    }
    this.inode.data = new Uint8Array(Buffer.concat(parts));
    return n;
  }

  write(bufs: Uint8Array[]): Promise<number> {
    return this.fs.op(`write ${this.path}`, () => this.#append(bufs, this.fs.w.setup.fileWrite), this.fs.w.setup.stall, this.save);
  }

  sync(): Promise<void> {
    return this.fs.op(
      `sync ${this.path}`,
      () => {
        if (this.closed) throw new Error("EBADF: bad file descriptor");
      },
      false,
      this.save,
    );
  }

  close(): Promise<void> {
    return this.fs.op(
      `close ${this.path}`,
      () => {
        if (this.closed) throw new Error("EBADF: bad file descriptor");
        this.closed = true;
      },
      false,
      this.save,
    );
  }
}

/** The file system, as the client's FileOps (files.ts): saves and download files. */
class MemFs implements FileOps {
  readonly paths = new Map<string, Inode>();
  readonly dirs = new Set<string>();
  /** Operations called and not yet complete, but for saves'. */
  inFlight = 0;
  /** Saves' operations called and not yet complete: a save under way at stop goes on, and ends
   * whole (files.ts; a client started next on the state file waits for it). */
  saving = 0;

  constructor(readonly w: World) {}

  /** An operation: it takes effect when called, and completes when the scheduler says (a
   * stalled one, once STALL_MS have passed first). */
  op<T>(label: string, f: () => T, stalls = false, save = false): Promise<T> {
    this.w.use(label, false, save);
    if (save) this.saving++;
    else this.inFlight++;
    // A stalled one is the scheduler's only once its time has passed (a task it can't finish
    // would hold the scheduler up).
    const go = (): Promise<T> => {
      let out: Promise<T>;
      try {
        out = Promise.resolve(f());
      } catch (e) {
        out = Promise.reject(e);
      }
      return this.w.s.schedule(out, label);
    };
    const done = stalls ? new Promise<void>((r) => setTimeout(r, STALL_MS)).then(go) : go();
    return done.finally(() => {
      if (save) this.saving--;
      else this.inFlight--;
      this.w.trace.push(`  ${label.replace(this.w.stateFile, "<state>")} done`);
    });
  }

  #isSave(path: string): boolean {
    return path === tempOf(this.w.stateFile) || path === this.w.stateFile;
  }

  mkdir(dir: string): Promise<boolean> {
    return this.op(`mkdir ${dir}`, () => {
      const made = !this.dirs.has(dir);
      this.dirs.add(dir);
      return made;
    });
  }

  create(path: string): Promise<OpenFile> {
    const save = this.#isSave(path);
    return this.op(
      `create ${path}`,
      () => {
        if (save && this.w.pick("saves") === "fail") throw Object.assign(new Error("ENOSPC: no space left on device"), { code: "ENOSPC" });
        if (this.paths.has(path)) throw Object.assign(new Error(`EEXIST: file already exists, open '${path}'`), { code: "EEXIST" });
        const inode = new Inode();
        this.paths.set(path, inode);
        const h = new FakeHandle(this, path, inode, save);
        this.w.resources.push({ what: `${save ? "save" : "file"} handle ${path}`, released: () => h.closed });
        return h;
      },
      false,
      save,
    );
  }

  rename(from: string, to: string): Promise<void> {
    return this.op(
      `rename ${from} ${to}`,
      () => {
        const inode = this.paths.get(from);
        if (!inode) throw Object.assign(new Error(`ENOENT: no such file, rename '${from}'`), { code: "ENOENT" });
        this.paths.delete(from);
        this.paths.set(to, inode);
      },
      false,
      true,
    );
  }

  syncDir(dir: string): Promise<boolean> {
    // Only a save flushes a directory.
    return this.op(`syncdir ${dir}`, () => true, false, true);
  }

  remove(path: string): Promise<boolean> {
    return this.op(`remove ${path}`, () => this.paths.delete(path), false, this.#isSave(path));
  }

  clear(dir: string): void {
    this.w.use(`clear ${dir}`);
    for (const p of [...this.paths.keys()]) if (p.startsWith(`${dir}/`)) this.paths.delete(p);
  }

  read(path: string): Uint8Array | undefined {
    return this.paths.get(path)?.data;
  }
}

// ---------------------------------------------------------------------------------------------
// The server: OpAMP over whatever carries it, and the package files.

/** A connection the server can push on (a WebSocket, either mode's). */
interface Peer {
  push(payload: Uint8Array): void;
  /** A Close frame from the server. */
  serverClose(code: number): void;
  /** The connection drops (no Close). */
  drop(): void;
  /** Bytes that aren't a valid frame. */
  corrupt(): void;
}

type OfferKind = "config" | "custom" | "heartbeat" | "package";

const sha256 = (b: Uint8Array): Uint8Array => new Uint8Array(createHash("sha256").update(b).digest());
const packageFile = (n: number): Uint8Array => new Uint8Array(Buffer.from(`package ${n} `.repeat(40 + n * 13)));

class Server {
  readonly got: AgentToServer[] = [];
  readonly pending: ServerToAgent[] = [];
  readonly peers = new Set<Peer>();
  configs = 0;
  customs = 0;
  heartbeats = 0;
  packages = 0;

  /** The answer to a message (with the next offer, if any). */
  answer(bytes: Uint8Array): Uint8Array<ArrayBuffer> {
    const msg = fromBinary(AgentToServerSchema, bytes);
    this.got.push(msg);
    const out = this.pending.shift() ?? create(ServerToAgentSchema);
    out.instanceUid = msg.instanceUid;
    out.capabilities = SERVER_CAPABILITIES;
    return new Uint8Array(toBinary(ServerToAgentSchema, out));
  }

  offer(kind: OfferKind): void {
    let m: ServerToAgent;
    if (kind === "config") {
      const n = ++this.configs;
      m = create(ServerToAgentSchema, {
        remoteConfig: { config: { configMap: { "": { body: new Uint8Array([n]) } } }, configHash: new Uint8Array([n]) },
      });
    } else if (kind === "custom") {
      const n = ++this.customs;
      m = create(ServerToAgentSchema, { customMessage: { capability: CAPABILITY, type: "m", data: new Uint8Array([n]) } });
    } else if (kind === "heartbeat") {
      const n = this.heartbeats++ % 2 === 0 ? 2 : 1;
      m = create(ServerToAgentSchema, { connectionSettings: { opamp: { heartbeatIntervalSeconds: BigInt(n) } } });
    } else {
      const n = ++this.packages;
      m = create(ServerToAgentSchema, {
        packagesAvailable: {
          allPackagesHash: new Uint8Array([n]),
          packages: {
            pkg: {
              type: PackageType.PackageType_TopLevel,
              version: String(n),
              hash: new Uint8Array([n]),
              file: { downloadUrl: `http://${DOWNLOAD_HOST}/pkg/${n}`, contentHash: sha256(packageFile(n)) },
            },
          },
        },
      });
    }
    // Pushed on the connection if there is one (what waited for an answer first: in order).
    this.pending.push(m);
    const peer = [...this.peers].at(-1);
    if (peer) {
      for (const p of this.pending.splice(0)) {
        p.instanceUid = UID;
        p.capabilities = SERVER_CAPABILITIES;
        peer.push(toBinary(ServerToAgentSchema, p));
      }
    }
  }

  /** A package file, as served (`corrupt`: other bytes than the offer's hash). */
  file(n: number, corrupt: boolean): Uint8Array {
    const f = packageFile(n);
    if (corrupt) f[0] = (f[0] ?? 0) ^ 0xff;
    return f;
  }
}

// ---------------------------------------------------------------------------------------------
// Platform mode: fetch and ws.

/** The ws package's WebSocket, as far as the executor uses it, with ws's own state machine
 * (CONNECTING, OPEN, CLOSING, CLOSED) and event order: an error is followed by close; close comes
 * once, last; a failed write calls back with the error and the socket then closes (1006); messages
 * that had arrived are still emitted after terminate (ws's socketOnClose reads what was buffered);
 * a message over maxPayload is an error, then close 1009. */
class FakeWs extends EventEmitter implements Peer {
  readyState = 0;
  binaryType = "";
  /** The socket is gone or let go of (terminate, or its end). */
  destroyed = false;
  closeEmitted = false;
  closeCode = 1006;
  /** The server's Close came: ws reads nothing after it. */
  closeReceived = false;
  readonly n: number;
  readonly inbox: Inbox;
  readonly maxPayload: number;

  constructor(
    readonly w: World,
    url: string,
    opts: { maxPayload?: number },
  ) {
    super();
    w.use(`new WebSocket(${url})`, true);
    this.n = ++w.sockets;
    this.inbox = new Inbox(w);
    this.maxPayload = opts.maxPayload ?? 0;
    w.resources.push({ what: `WebSocket #${this.n}`, released: () => this.destroyed || this.closeEmitted });
    const outcome = w.pick("upgrades");
    this.inbox.push(`ws#${this.n} handshake: ${outcome}`, () => this.#handshake(outcome));
  }

  #handshake(outcome: Upgrade): void {
    if (this.readyState !== 0) return;
    if (outcome === "accept") {
      this.emit("upgrade", { socket: { unref() {} } });
      if (this.readyState !== 0) return;
      this.readyState = 1;
      this.w.server.peers.add(this);
      this.emit("open");
    } else if (outcome === "refuse") {
      this.emit("unexpected-response", {}, { statusCode: 503, headers: { "retry-after": "1" }, resume() {} });
    } else {
      this.#errorAndClose(new Error("connect ECONNREFUSED"), 1006);
    }
  }

  #errorAndClose(err: Error, code: number): void {
    this.readyState = 2;
    this.#gone();
    this.emit("error", err);
    this.#emitClose(code);
  }

  #gone(): void {
    this.destroyed = true;
    this.w.server.peers.delete(this);
  }

  #emitClose(code: number): void {
    if (this.closeEmitted) return;
    this.#gone();
    this.readyState = 3;
    this.closeEmitted = true;
    this.emit("close", code, Buffer.alloc(0));
  }

  #abortHandshake(): void {
    this.readyState = 2;
    this.#gone();
    this.inbox.clear();
    this.inbox.push(`ws#${this.n} handshake aborted`, () =>
      this.#errorAndClose(new Error("WebSocket was closed before the connection was established"), 1006),
    );
  }

  terminate(): void {
    if (this.readyState === 3) return;
    if (this.readyState === 0) return this.#abortHandshake();
    this.readyState = 2;
    if (this.destroyed) return;
    this.#gone();
    // What had arrived is still read, then close.
    this.inbox.clear((i) => i.message);
    this.inbox.push(`ws#${this.n} closed (terminated)`, () => this.#emitClose(this.closeCode));
  }

  close(code: number): void {
    if (this.readyState === 3) return;
    if (this.readyState === 0) return this.#abortHandshake();
    if (this.readyState === 2) return;
    this.readyState = 2;
    if (this.w.setup.deaf) return;
    // The server answers our Close, then the socket ends.
    this.inbox.push(`ws#${this.n} Close answered`, () => {
      this.closeCode = code;
      this.#emitClose(code);
    });
  }

  send(data: Uint8Array, _opts: unknown, cb?: (err?: Error) => void): void {
    if (this.readyState === 0) throw new Error("WebSocket is not open: readyState 0 (CONNECTING)");
    if (this.readyState !== 1) {
      const err = new Error(`WebSocket is not open: readyState ${this.readyState}`);
      this.w.later(`ws#${this.n} send after close`, () => cb?.(err));
      return;
    }
    if (this.w.pick("sends") === "fail") {
      this.w.later(`ws#${this.n} send failed`, () => cb?.(new Error("write EPIPE")));
      this.inbox.clear((i) => i.message);
      this.inbox.push(`ws#${this.n} socket closed after a failed write`, () => {
        this.readyState = 2;
        this.#emitClose(1006);
      });
      return;
    }
    const answer = this.w.server.answer(data.subarray(1));
    this.w.later(`ws#${this.n} sent`, () => cb?.());
    this.push(answer);
  }

  // The server's side.

  push(payload: Uint8Array): void {
    const data = Buffer.concat([Buffer.from([0]), payload]);
    this.inbox.push(
      `ws#${this.n} message (${data.length} bytes)`,
      () => {
        if (this.closeEmitted || this.closeReceived) return;
        if (this.maxPayload > 0 && data.length > this.maxPayload) {
          this.#errorAndClose(new RangeError("Max payload size exceeded"), 1009);
        } else {
          this.emit("message", data, true);
        }
      },
      { message: true },
    );
  }

  serverClose(code: number): void {
    // Nothing more from the server on it.
    this.w.server.peers.delete(this);
    this.inbox.push(`ws#${this.n} server Close ${code}`, () => {
      if (this.closeEmitted || this.closeReceived) return;
      this.closeReceived = true;
      this.closeCode = code;
      // ws answers it; the server then ends the socket.
      if (this.readyState === 1) this.readyState = 2;
      this.inbox.push(`ws#${this.n} socket ended`, () => this.#emitClose(code));
    });
  }

  drop(): void {
    this.inbox.clear();
    this.inbox.push(`ws#${this.n} dropped`, () => {
      this.readyState = 2;
      this.#emitClose(this.closeCode);
    });
  }

  corrupt(): void {
    this.inbox.push(`ws#${this.n} bad frame`, () => {
      if (this.closeEmitted || this.closeReceived) return;
      this.#errorAndClose(new RangeError("Invalid WebSocket frame: RSV1 must be clear"), 1002);
    });
  }
}

/** undici's fetch: a POST to the OpAMP endpoint, or a GET of a package file (its body streamed,
 * a part at a time, failing once the request's signal has aborted). */
function fakeFetch(w: World): ExecutorPlatform["fetch"] {
  type Init = { method?: string; body?: Uint8Array; signal?: AbortSignal };
  type Held = { settled: boolean };
  const aborted = () => new DOMException("This operation was aborted", "AbortError");
  // The scheduled function takes a short name (what the scheduler's report shows); the request
  // itself is looked up by it.
  const requests = new Map<string, { url: string; init: Init; r: Held }>();
  let count = 0;
  const respond = w.s.scheduleFunction(async (what: string) => {
    const { url, init, r } = requests.get(what)!;
    requests.delete(what);
    if ((init.method ?? "GET") === "POST") {
      const outcome = w.pick("posts");
      if (outcome === "fail") throw new TypeError("fetch failed", { cause: new Error("connect ECONNREFUSED") });
      if (outcome === "500") return new Response(null, { status: 500 });
      return new Response(w.server.answer(init.body ?? new Uint8Array()), {
        status: 200,
        headers: { "content-type": "application/x-protobuf" },
      });
    }
    const n = Number(new URL(url).pathname.split("/").at(-1));
    const outcome = w.pick("downloads");
    if (outcome === "404") return new Response(null, { status: 404 });
    const file = w.server.file(n, outcome === "corrupt");
    const parts = [file.subarray(0, file.length >> 1), file.subarray(file.length >> 1)];
    let at = 0;
    const body = new ReadableStream<Uint8Array>({
      // As undici: the request's signal aborting fails its body at once.
      start: (c) => {
        init.signal?.addEventListener("abort", () => {
          if (r.settled) return;
          r.settled = true;
          c.error(aborted());
        });
      },
      pull: async (c) => {
        await w.s.schedule(Promise.resolve(), `download ${n}: body`);
        w.trace.push(`  download ${n}: body part ${at + 1}${init.signal?.aborted ? " (aborted)" : ""}`);
        if (r.settled) return;
        const part = parts[at++];
        if (init.signal?.aborted || (outcome === "cut" && at === 2) || !part) r.settled = true;
        if (init.signal?.aborted) c.error(aborted());
        else if (outcome === "cut" && at === 2) c.error(new TypeError("terminated"));
        else if (part) c.enqueue(part);
        else c.close();
      },
      cancel: () => void (r.settled = true),
    });
    return new Response(body, { status: 200 });
  });
  return ((url: string, init: Init) => {
    const what = `fetch ${init.method ?? "GET"} ${url}`;
    w.use(what, (init.method ?? "GET") === "GET");
    const r: Held = { settled: false };
    w.resources.push({ what, released: () => r.settled });
    const name = `${what} #${++count}`;
    requests.set(name, { url, init, r });
    // As undici: the request's signal aborting before the response fails the fetch at once,
    // whenever the server would have answered. (After it, the body fails: see above.)
    const signal = init.signal;
    let responded = false;
    const abort = new Promise<never>((_, reject) => {
      const gone = () => {
        if (r.settled || responded) return;
        r.settled = true;
        w.trace.push(`  ${name}: aborted`);
        reject(aborted());
      };
      if (signal?.aborted) gone();
      else signal?.addEventListener("abort", gone, { once: true });
    });
    abort.catch(() => {});
    const answered = respond(name).then(
      (res) => {
        responded = true;
        w.trace.push(`  ${name}: ${res.status}`);
        if (init.signal?.aborted) {
          void res.body?.cancel().catch(() => {});
          r.settled = true;
          throw aborted();
        }
        // A file's body is still to come: released once read, failed or cancelled.
        if (!res.body || (init.method ?? "GET") === "POST") r.settled = true;
        return res;
      },
      (e: unknown) => {
        w.trace.push(`  ${name}: ${String(e)}`);
        r.settled = true;
        throw e;
      },
    );
    answered.catch(() => {});
    return Promise.race([answered, abort]);
  }) as never;
}

// ---------------------------------------------------------------------------------------------
// Core mode: TCP sockets carrying HTTP/1.1 and WebSocket, spoken by the core.

/** A net.Socket, as streams.ts uses it: connect, data, end, error and close events in order,
 * paused data held back, write, end, destroy. */
class FakeSocket extends EventEmitter {
  /** Whether it would keep Node.js running: a net.Socket's ref and unref. */
  refd = true;
  connecting = true;
  destroyed = false;
  closed = false;
  ended = false;
  paused = false;
  writableLength = 0;
  readonly n: number;
  readonly inbox: Inbox;
  conn: RawConn | undefined;

  constructor(
    readonly w: World,
    readonly host: string,
  ) {
    super();
    this.n = ++w.sockets;
    this.inbox = new Inbox(w, () => this.paused);
    // Ended (our FIN sent: the server sees it go, and what was written is flushed) counts.
    w.resources.push({ what: `socket #${this.n} to ${host}`, released: () => this.destroyed || this.closed || this.ended });
    const outcome = w.pick("connects");
    this.inbox.push(`socket#${this.n} connect: ${outcome}`, () => {
      if (this.destroyed) return;
      if (outcome === "refused") return this.#fail(Object.assign(new Error("connect ECONNREFUSED"), { code: "ECONNREFUSED" }));
      this.connecting = false;
      this.conn = new RawConn(w, this);
      this.emit("connect");
    });
  }

  #fail(err: Error): void {
    this.destroyed = true;
    this.conn?.gone();
    this.emit("error", err);
    this.closed = true;
    this.emit("close", true);
  }

  setNoDelay(): this {
    return this;
  }

  ref(): this {
    this.refd = true;
    return this;
  }

  unref(): this {
    this.refd = false;
    return this;
  }

  write(data: Uint8Array): boolean {
    if (this.destroyed || this.ended) return false;
    this.w.guard(`socket#${this.n} server reads`, () => this.conn?.receive(Buffer.from(data)));
    if (this.w.pick("writes") === "full") {
      this.w.later(`socket#${this.n} drain`, () => {
        if (!this.destroyed) this.emit("drain");
      });
      return false;
    }
    return true;
  }

  end(): this {
    if (this.destroyed || this.ended) return this;
    this.ended = true;
    this.conn?.peerEnded();
    return this;
  }

  destroy(): this {
    if (this.destroyed) return this;
    this.destroyed = true;
    this.conn?.gone();
    this.inbox.clear();
    this.w.later(`socket#${this.n} closed (destroyed)`, () => {
      if (this.closed) return;
      this.closed = true;
      this.emit("close", false);
    });
    return this;
  }

  pause(): this {
    this.paused = true;
    return this;
  }

  resume(): this {
    this.paused = false;
    this.inbox.pump();
    return this;
  }

  // The server's side.

  deliver(bytes: Uint8Array): void {
    for (let at = 0; at < bytes.length; at += this.w.setup.chunk) {
      const part = Buffer.from(bytes.subarray(at, at + this.w.setup.chunk));
      this.inbox.push(`socket#${this.n} data (${part.length} bytes)`, () => void (this.destroyed || this.emit("data", part)), { data: true });
    }
  }

  /** The server ends its side: end, then close. */
  serverEnd(): void {
    this.inbox.push(`socket#${this.n} end`, () => void (this.destroyed || this.emit("end")), { data: true });
    this.inbox.push(`socket#${this.n} closed`, () => {
      if (this.closed) return;
      this.destroyed = true;
      this.closed = true;
      this.emit("close", false);
    });
  }

  /** A reset: what wasn't read is lost. */
  reset(): void {
    this.inbox.clear();
    this.inbox.push(`socket#${this.n} reset`, () => {
      if (this.destroyed) return;
      this.#fail(Object.assign(new Error("read ECONNRESET"), { code: "ECONNRESET" }));
    });
  }
}

const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

function wsFrame(op: number, payload: Uint8Array): Buffer {
  const n = payload.length;
  const head =
    n < 126
      ? Buffer.from([0x80 | op, n])
      : n < 65536
        ? Buffer.from([0x80 | op, 126, n >> 8, n & 0xff])
        : (() => {
            const b = Buffer.alloc(10);
            b[0] = 0x80 | op;
            b[1] = 127;
            b.writeBigUInt64BE(BigInt(n), 2);
            return b;
          })();
  return Buffer.concat([head, payload]);
}

/** The server's end of a fake socket: HTTP/1.1 requests (the OpAMP endpoint, upgrades, package
 * files), then WebSocket frames after an upgrade. */
class RawConn implements Peer {
  buf = Buffer.alloc(0);
  mode: "http" | "ws" | "gone" = "http";
  /** The server sent a Close: the client's answer ends the connection. */
  closing = false;

  constructor(
    readonly w: World,
    readonly sock: FakeSocket,
  ) {}

  receive(b: Buffer): void {
    if (this.mode === "gone") return;
    this.buf = Buffer.concat([this.buf, b]);
    for (;;) {
      const mode = this.mode as RawConn["mode"];
      if (mode === "gone" || !(mode === "http" ? this.#request() : this.#frame())) return;
    }
  }

  #respond(status: number, body: Uint8Array, extra = ""): void {
    const head = `HTTP/1.1 ${status} X\r\nContent-Length: ${body.length}\r\n${extra}\r\n`;
    this.sock.deliver(Buffer.concat([Buffer.from(head, "latin1"), body]));
  }

  /** One whole request, handled; false if it hasn't all come. */
  #request(): boolean {
    const end = this.buf.indexOf("\r\n\r\n");
    if (end < 0) return false;
    const lines = this.buf.subarray(0, end).toString("latin1").split("\r\n");
    const [method, target] = (lines[0] ?? "").split(" ");
    const headers = new Map(lines.slice(1).map((l) => [l.slice(0, l.indexOf(":")).trim().toLowerCase(), l.slice(l.indexOf(":") + 1).trim()]));
    const len = Number(headers.get("content-length") ?? 0);
    if (this.buf.length < end + 4 + len) return false;
    const body = this.buf.subarray(end + 4, end + 4 + len);
    this.buf = this.buf.subarray(end + 4 + len);
    if (headers.get("upgrade")?.toLowerCase() === "websocket") {
      const outcome = this.w.pick("upgrades");
      if (outcome === "fail") {
        this.#reset();
        return false;
      }
      if (outcome === "refuse") {
        this.#respond(503, new Uint8Array(), "Retry-After: 1\r\n");
        return true;
      }
      const accept = createHash("sha1").update(`${headers.get("sec-websocket-key") ?? ""}${GUID}`).digest("base64");
      this.sock.deliver(
        Buffer.from(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`, "latin1"),
      );
      this.mode = "ws";
      this.w.server.peers.add(this);
      return true;
    }
    if (method === "POST") {
      const outcome = this.w.pick("posts");
      if (outcome === "fail") {
        this.#reset();
        return false;
      }
      if (outcome === "500") this.#respond(500, new Uint8Array());
      else this.#respond(200, this.w.server.answer(new Uint8Array(body)), "Content-Type: application/x-protobuf\r\n");
      return true;
    }
    const n = Number((target ?? "").split("/").at(-1));
    const outcome = this.w.pick("downloads");
    if (outcome === "404") {
      this.#respond(404, new Uint8Array());
      return true;
    }
    const file = this.w.server.file(n, outcome === "corrupt");
    if (outcome === "cut") {
      const head = `HTTP/1.1 200 OK\r\nContent-Length: ${file.length}\r\n\r\n`;
      this.sock.deliver(Buffer.concat([Buffer.from(head, "latin1"), file.subarray(0, file.length >> 1)]));
      this.#reset();
      return false;
    }
    this.#respond(200, file);
    return true;
  }

  /** One whole frame from the client (masked), handled; false if it hasn't all come. */
  #frame(): boolean {
    const b = this.buf;
    if (b.length < 2) return false;
    const op = (b[0] ?? 0) & 0x0f;
    const masked = ((b[1] ?? 0) & 0x80) !== 0;
    let len = (b[1] ?? 0) & 0x7f;
    let at = 2;
    if (len === 126) {
      if (b.length < 4) return false;
      len = b.readUInt16BE(2);
      at = 4;
    } else if (len === 127) {
      if (b.length < 10) return false;
      len = Number(b.readBigUInt64BE(2));
      at = 10;
    }
    const mask = masked ? b.subarray(at, at + 4) : undefined;
    if (masked) at += 4;
    if (b.length < at + len) return false;
    const payload = Buffer.from(b.subarray(at, at + len));
    if (mask) for (let i = 0; i < payload.length; i++) payload[i]! ^= mask[i % 4]!;
    this.buf = b.subarray(at + len);
    if (op === 0x2) {
      this.push(this.w.server.answer(new Uint8Array(payload.subarray(1))));
    } else if (op === 0x8) {
      // Answer a Close (unless it answers ours), then end the connection.
      if (!this.closing) this.sock.deliver(wsFrame(0x8, payload.subarray(0, 2)));
      this.#end();
    } else if (op === 0x9) {
      this.sock.deliver(wsFrame(0xa, payload));
    }
    return true;
  }

  #end(): void {
    this.gone();
    this.sock.serverEnd();
  }

  #reset(): void {
    this.gone();
    this.sock.reset();
  }

  gone(): void {
    this.mode = "gone";
    this.w.server.peers.delete(this);
  }

  /** The client ended its side: the server ends too. */
  peerEnded(): void {
    if (this.mode !== "gone") this.#end();
  }

  push(payload: Uint8Array): void {
    if (this.mode === "ws") this.sock.deliver(wsFrame(0x2, Buffer.concat([Buffer.from([0]), payload])));
  }

  serverClose(code: number): void {
    if (this.mode !== "ws" || this.closing) return;
    this.closing = true;
    this.w.server.peers.delete(this);
    this.sock.deliver(wsFrame(0x8, Buffer.from([code >> 8, code & 0xff])));
  }

  drop(): void {
    if (this.mode !== "gone") this.#reset();
  }

  corrupt(): void {
    // A reserved opcode.
    if (this.mode === "ws") this.sock.deliver(Buffer.from([0x83, 0x00]));
  }
}

function fakeStreams(w: World): StreamsPlatform {
  return {
    connect: ((opts: { host: string; port: number }) => {
      w.use(`connect ${opts.host}:${opts.port}`, true);
      return new FakeSocket(w, opts.host);
    }) as never,
    tlsConnect: (() => {
      throw new Error("TLS is not modelled here: the endpoints are plain");
    }) as never,
    lookup: ((host: string, _o: unknown, cb: (err: Error | null, a: { address: string; family: number }[]) => void) => {
      w.use(`lookup ${host}`);
      w.later(`lookup ${host}`, () => cb(null, [{ address: "127.0.0.1", family: 4 }]));
    }) as never,
  };
}

// ---------------------------------------------------------------------------------------------
// The model at the core's boundary (RULE-SHELL-04 to -09).

const idOf = (v: unknown): bigint => (v as { id: bigint }).id;

/** One effect the core issued that is answered. */
interface Pending {
  kind: string;
  /** opening, open, ended (connections and streams); answered (the rest). A connection whose
   * server's Close was reported is "closed" until the core closes it: then its end follows
   * ("draining"). One the core closes while it is open may end twice, when Closes cross: the
   * server's, then the end of the core's ("crossed"). */
  phase: "waiting" | "open" | "closed" | "draining" | "crossed" | "ended";
  /** The core said ws_close. */
  closing?: boolean;
  /** Discarded (a download's file) or cancelled: nothing more may name it. */
  gone?: boolean;
  /** Cancelled. */
  cancelled?: boolean;
  /** A stream's splices issued and not yet answered. */
  splices?: number;
  tls?: boolean;
  data?: Uint8Array;
}

class Book {
  readonly effects = new Map<string, Pending>();
  /** The last output's wake_at_ms: the timer armed now. */
  armed: bigint | undefined;
  stopped = false;
  /** Stores issued, in order, with their data. */
  readonly stores: { id: bigint; data: Uint8Array }[] = [];
  steps = 0;

  constructor(readonly w: World) {}

  #bad(what: string, rule: string): void {
    this.w.problems.push(`${what} (${rule})`);
  }

  #get(ns: string, id: bigint): Pending | undefined {
    return this.effects.get(`${ns}:${id}`);
  }

  observe(now: number, input: Input, out: Output | undefined, err: unknown): void {
    this.steps++;
    const k = input.kind;
    const what = `${k.case}(${k.case && typeof k.value === "object" && k.value !== null && "id" in k.value ? String(idOf(k.value)) : ""})`;
    const effects = (out?.effects ?? []).map((e) => {
      const v = e.kind.value as { id?: bigint; kind?: { case?: string } } | undefined;
      return e.kind.case === "notify" ? `notify(${v?.kind?.case})` : `${e.kind.case}(${v?.id ?? ""})`;
    });
    this.w.trace.push(
      `@${now} core <- ${what}${err === undefined ? "" : ` REFUSED ${String(err)}`} -> [${effects.join(", ")}]${out?.stopped ? " STOPPED" : ""}`,
    );
    if (this.stopped) this.#bad(`${what} fed after the output that said stopped`, "RULE-SHELL-07");
    if (err !== undefined) this.#bad(`the core refused ${what}: ${String(err)}`, "RULE-SHELL-04");
    this.#input(now, input, what);
    if (out) this.#output(out);
  }

  /** An answer to an effect of kind `issued` (one of): the effect it names. */
  #answer(ns: string, id: bigint, what: string, issued: string[]): Pending | undefined {
    const e = this.#get(ns, id);
    if (!e || !issued.includes(e.kind)) {
      this.#bad(`${what} answers no ${issued.join("/")} the core issued`, "RULE-SHELL-04");
      return undefined;
    }
    return e;
  }

  #input(now: number, input: Input, what: string): void {
    const k = input.kind;
    switch (k.case) {
      case "timer":
        if (this.armed === undefined) this.#bad(`a timer with none armed`, "RULE-SHELL-08");
        else if (BigInt(Math.floor(now)) < this.armed) this.#bad(`a timer at ${now} before wake_at ${this.armed}`, "RULE-SHELL-08");
        return;
      case "httpResponse":
      case "httpFailed":
      case "downloaded":
      case "downloadResponse":
      case "downloadFailed":
      case "stored":
      case "storeFailed": {
        const id = idOf(k.value);
        const coreMode = this.w.setup.mode === "core";
        const kinds =
          k.case.startsWith("http") ? ["httpRequest"] : k.case.startsWith("download") ? [coreMode ? "downloadDone" : "download"] : ["store"];
        const e = this.#answer(k.case.startsWith("download") ? "dl" : "fx", id, what, kinds);
        if (!e) return;
        if (e.phase !== "waiting") this.#bad(`${what}: answered already`, "RULE-SHELL-04");
        if (e.gone) this.#bad(`${what} after its discard or cancel`, "RULE-SHELL-06");
        e.phase = "ended";
        if (k.case === "downloaded" || k.case === "downloadResponse") {
          const file = this.w.fs.read(downloadPath(this.w.stateFile, id));
          if (!file) this.#bad(`${what}: no file`, "RULE-SHELL-04");
          else if (Buffer.compare(sha256(file), (k.value as { sha256: Uint8Array }).sha256) !== 0) {
            this.#bad(`${what}: the hash is not the file's`, "RULE-SHELL-04");
          }
        }
        if (k.case === "stored") {
          const saved = this.w.fs.read(this.w.stateFile);
          const mine = this.stores.findIndex((s) => s.id === id);
          const ok = saved !== undefined && this.stores.slice(mine).some((s) => Buffer.compare(s.data, saved) === 0);
          if (!ok) this.#bad(`${what}: the state file doesn't hold that save (or a later one)`, "RULE-SHELL-09");
        }
        return;
      }
      case "wsOpened":
      case "wsFailed": {
        const e = this.#answer("fx", idOf(k.value), what, ["wsOpen"]);
        if (!e) return;
        if (e.cancelled) this.#bad(`${what} after its cancel`, "RULE-SHELL-06");
        if (e.phase !== "waiting") this.#bad(`${what}: the open was answered already`, "RULE-SHELL-04");
        e.phase = k.case === "wsOpened" ? "open" : "ended";
        return;
      }
      case "wsMessage":
      case "wsClosed":
      case "wsLost": {
        const e = this.#answer("fx", idOf(k.value), what, ["wsOpen"]);
        if (!e) return;
        if (e.cancelled) this.#bad(`${what} after its cancel`, "RULE-SHELL-06");
        if (k.case === "wsMessage") {
          if (e.phase !== "open") this.#bad(`${what} on a connection ${e.phase === "waiting" ? "not yet open" : "that has ended"}`, "RULE-SHELL-04");
          return;
        }
        // Its end, once; twice when Closes cross (RULE-SHELL-04): the server's Close, then the end
        // of the core's close.
        if (e.phase === "open" && k.case === "wsClosed") e.phase = e.closing ? "crossed" : "closed";
        else if (e.phase === "open" || e.phase === "draining" || e.phase === "crossed") e.phase = "ended";
        else this.#bad(`${what} on a connection ${e.phase === "waiting" ? "not yet open" : e.phase === "closed" ? "the core hasn't closed" : "already ended"}`, "RULE-SHELL-04");
        return;
      }
      case "proxyAnswer":
      case "streamResolved": {
        const e = this.#answer("st", idOf(k.value), what, [k.case === "proxyAnswer" ? "proxyQuery" : "streamResolve"]);
        if (!e) return;
        if (e.phase !== "waiting") this.#bad(`${what}: answered already`, "RULE-SHELL-04");
        e.phase = "ended";
        return;
      }
      case "streamOpened":
      case "streamTlsReady":
      case "streamData":
      case "streamEof":
      case "streamSpliced":
      case "streamFailed": {
        const e = this.#answer("st", idOf(k.value), what, ["streamOpen"]);
        if (!e) return;
        if (e.phase === "ended") return this.#bad(`${what} about a stream closed or failed`, "RULE-SHELL-06");
        if (k.case === "streamFailed") {
          e.phase = "ended";
        } else if (k.case === "streamOpened") {
          if (e.phase !== "waiting") this.#bad(`${what}: opened already`, "RULE-SHELL-04");
          e.phase = "open";
        } else if (e.phase !== "open") {
          this.#bad(`${what} on a stream not yet open`, "RULE-SHELL-04");
        } else if (k.case === "streamTlsReady") {
          if (!e.tls) this.#bad(`${what} without streamStartTls`, "RULE-SHELL-04");
          e.tls = false;
        } else if (k.case === "streamSpliced") {
          if (!e.splices) this.#bad(`${what} without a splice`, "RULE-SHELL-04");
          else e.splices--;
        }
        return;
      }
      default:
        return;
    }
  }

  #output(out: Output): void {
    // What the application is to hear of this output: its notifications in order, but stopped
    // last (the core may emit one after it; nothing comes after stopped, RULE-SHELL-01).
    const told = out.effects.flatMap((e) =>
      e.kind.case === "notify" && e.kind.value.kind.case !== undefined
        ? [e.kind.value.kind.case === "heartbeatIntervalMs" ? "heartbeatInterval" : e.kind.value.kind.case]
        : [],
    );
    this.w.emitted.push(...told.filter((k) => k !== "stopped"), ...told.filter((k) => k === "stopped"));
    for (const e of out.effects) {
      const k = e.kind;
      switch (k.case) {
        case "httpRequest":
        case "wsOpen":
        case "download":
        case "store":
          this.effects.set(`${k.case === "download" ? "dl" : "fx"}:${k.value.id}`, { kind: k.case, phase: "waiting" });
          if (k.case === "store") this.stores.push({ id: k.value.id, data: k.value.data.slice() });
          break;
        case "proxyQuery":
        case "streamResolve":
          this.effects.set(`st:${k.value.id}`, { kind: k.case, phase: "waiting" });
          break;
        case "streamOpen":
          this.effects.set(`st:${k.value.id}`, { kind: k.case, phase: "waiting", splices: 0 });
          break;
        case "streamStartTls": {
          const s = this.#get("st", k.value.id);
          if (s) s.tls = true;
          break;
        }
        case "streamSplice": {
          const s = this.#get("st", k.value.id);
          if (s) s.splices = (s.splices ?? 0) + 1;
          break;
        }
        case "streamClose": {
          const s = this.#get("st", k.value.id);
          if (s) s.phase = "ended";
          break;
        }
        case "downloadDone":
          this.effects.set(`dl:${k.value.id}`, { kind: k.case, phase: "waiting" });
          break;
        case "wsClose": {
          const c = this.#get("fx", k.value.id);
          if (c) {
            c.closing = true;
            if (c.phase === "closed") c.phase = "draining";
          }
          break;
        }
        case "cancel": {
          // Nothing more about it (RULE-SHELL-06), whatever it is.
          for (const ns of ["fx", "dl"]) {
            const e = this.#get(ns, k.value.id);
            if (e) e.gone = e.cancelled = true;
          }
          this.w.forgotten.add(downloadPath(this.w.stateFile, k.value.id));
          break;
        }
        case "discard": {
          const d = this.#get("dl", k.value.id);
          if (d) d.gone = true;
          this.w.forgotten.add(downloadPath(this.w.stateFile, k.value.id));
          break;
        }
        default:
          break;
      }
    }
    this.armed = out.wakeAtMs;
    if (out.stopped) {
      this.stopped = true;
      this.w.endedAt ??= Date.now();
    }
  }

  /** Once the scheduler is idle, with the client running: every effect has its answer
   * (RULE-SHELL-05), and a download file discarded or cancelled is gone (RULE-SHELL-06). */
  idle(): void {
    for (const [key, e] of this.effects) {
      if (e.phase === "waiting" && !e.gone) this.#bad(`${e.kind} ${key} never answered`, "RULE-SHELL-05");
    }
    this.files();
  }

  files(): void {
    for (const p of this.w.forgotten) {
      if (this.w.fs.read(p) !== undefined) this.#bad(`${p} left after its discard or cancel`, "RULE-SHELL-06");
    }
  }
}

// ---------------------------------------------------------------------------------------------
// The callback contract (callback.properties.test.ts's model, RULE-SHELL-01 to -03).

class Life {
  client: Client | undefined;
  returned = false;
  readonly notes: Notification[] = [];
  depth = 0;
  calls = 0;
  throwAt: number | undefined;
  thrown: { value: unknown; on: Notification["kind"] } | undefined;
  readonly inside: ("call" | "stop")[] = [];
  readonly stops: Promise<void>[] = [];

  constructor(readonly w: World) {}

  notify = (n: Notification): void => {
    this.calls++;
    const p = this.w.problems;
    if (!this.returned) p.push(`a callback (${n.kind}) before start returned (RULE-SHELL-01)`);
    if (this.depth > 0) p.push(`two callbacks at once: ${n.kind} inside another (RULE-SHELL-01)`);
    if (this.notes.some((s) => s.kind === "stopped")) p.push(`a callback (${n.kind}) after stopped (RULE-SHELL-01)`);
    if (this.w.stopSettled) p.push(`a callback (${n.kind}) after stop returned (RULE-SHELL-07)`);
    this.notes.push(n);
    if (n.kind === "stopped") this.w.endedAt ??= Date.now();
    this.depth++;
    try {
      const client = this.client;
      if (client === undefined) throw new Error("the callback has no client handle");
      if (n.kind === "remoteConfig") {
        client.setRemoteConfigStatus(create(RemoteConfigStatusSchema, { lastRemoteConfigHash: n.remoteConfig.configHash, status: 1 }));
      } else if (n.kind === "installPackage") {
        client.packageInstalled(n.packageName, n.package.hash);
      } else if (n.kind === "removePackage") {
        client.packageRemoved(n.packageName);
      }
      for (const a of this.inside.splice(0)) {
        if (a === "call") this.w.call();
        else this.w.stop("inside");
      }
      if (this.calls === this.throwAt) {
        const value = this.w.failure();
        this.thrown ??= { value, on: n.kind };
        throw value;
      }
    } finally {
      this.depth--;
    }
  };

  check(final: boolean): void {
    const log = () => this.notes.map((n) => n.kind).join(", ");
    // Every notification the core emitted is heard, in order (stopped last in its output), until
    // stopped; a callback that throws ends it with the shell's own stopped (RULE-SHELL-01, -02).
    const heard = this.notes.map((n) => n.kind as string);
    const end = this.w.emitted.indexOf("stopped");
    const expected = end < 0 ? this.w.emitted : this.w.emitted.slice(0, end + 1);
    const failed = this.thrown !== undefined && this.thrown.on !== "stopped";
    const own = failed && heard.at(-1) === "stopped" ? heard.slice(0, -1) : heard;
    const why = () => `heard ${heard.join(", ")}; the core emitted ${this.w.emitted.join(", ")}`;
    assert.deepEqual(own, expected.slice(0, own.length), `notifications as the core emitted them (RULE-SHELL-01): ${why()}`);
    if (final && !failed) assert.deepEqual(heard, expected, `every notification heard, stopped last (RULE-SHELL-01): ${why()}`);
    const stopped = this.notes.filter((n) => n.kind === "stopped");
    assert.ok(stopped.length <= 1, `stopped more than once (RULE-SHELL-02): ${log()}`);
    if (stopped.length === 1) assert.equal(this.notes.at(-1)?.kind, "stopped", `stopped is the last word: ${log()}`);
    if (final) assert.equal(stopped.length, 1, `stopped exactly once (RULE-SHELL-02): ${log()}`);
    const s = stopped[0];
    if (this.thrown && this.thrown.on !== "stopped") {
      assert.ok(s?.kind === "stopped", `a callback threw: stopped follows (RULE-SHELL-02): ${log()}`);
      assert.ok(s.error === this.thrown.value, `stopped with the error thrown (RULE-SHELL-02): ${String(s.error)}`);
    } else if (s?.kind === "stopped") {
      assert.equal(s.error, undefined, `stopped without an error, none thrown: ${String(s.error)}: ${log()}`);
    }
    const increasing = (xs: number[], what: string) =>
      xs.forEach((x, i) => i === 0 || assert.ok(x > xs[i - 1]!, `${what} in order: ${log()}`));
    increasing(this.notes.flatMap((n) => (n.kind === "remoteConfig" ? [n.remoteConfig.configHash[0]!] : [])), "remote configs");
    increasing(this.notes.flatMap((n) => (n.kind === "customMessage" ? [n.customMessage.data[0]!] : [])), "custom messages");
    increasing(
      this.notes.flatMap((n) => (n.kind === "customMessageNotSent" ? [Number(new TextDecoder().decode(n.customMessage.data))] : [])),
      "refused custom messages",
    );
  }
}

// ---------------------------------------------------------------------------------------------
// One run's world.

/** The world of the run under way: a core stepped first while it is reports to it, for good (a
 * failed run's client may live on, and a real timer, a request's deadline, step it later). */
let current: World | undefined;
const worlds = new WeakMap<Core, World | undefined>();

const realStep = Core.prototype.step;
Core.prototype.step = function (this: Core, nowMs: number, input: Input): Output {
  if (!worlds.has(this)) worlds.set(this, current);
  const w = worlds.get(this);
  let out: Output | undefined;
  try {
    out = realStep.call(this, nowMs, input);
    return out;
  } catch (e) {
    w?.book.observe(nowMs, input, undefined, e);
    throw e;
  } finally {
    if (out) w?.book.observe(nowMs, input, out, undefined);
  }
};

const DIR = mkdtempSync(join(tmpdir(), "opamp-sched-"));

/** Worlds made so far (each its own state file). */
let worldsMade = 0;

class World {
  readonly fs = new MemFs(this);
  readonly server = new Server();
  readonly book = new Book(this);
  readonly problems: string[] = [];
  /** What happened, for a failure's report: the core's inputs and effects, the fakes' events. */
  readonly trace: string[] = [];
  /** Everything the client was handed that must be let go of by the time stop returns. */
  readonly resources: { what: string; released: () => boolean }[] = [];
  /** Download files discarded or cancelled. */
  readonly forgotten = new Set<string>();
  /** The notifications the core emitted, in the order the application is to hear them. */
  readonly emitted: string[] = [];
  readonly life = new Life(this);
  readonly #turns = new Map<string, number>();
  sockets = 0;
  healths = 0;
  failures = 0;
  stopCalled = false;
  stopSettled = false;
  /** What was still held when stop returned. */
  heldAtStop: string[] = [];
  /** When stop() was first called, and when the client ended (the core's stopped, or a callback's
   * failure): all of stop takes one stop timeout from the first of them. */
  stopCalledAt: number | undefined;
  endedAt: number | undefined;
  /** Stop returned at its bound, letting go of file work still in flight (a disk that is slow or
   * has stopped answering): that work may end after (closing and deleting what it wrote). */
  letGo = false;
  /** Its own: a save let go at a run's stop may still be under way when the next run starts, and
   * a client waits for the work on its state file left by one before it (files.ts). */
  readonly stateFile = join(DIR, `state-${++worldsMade}.bin`);

  constructor(
    readonly s: fc.Scheduler,
    readonly setup: Setup,
  ) {}

  /** The next answer from one of the run's lists; after HEAL of them, the healthy one (the core
   * reconnects at once after losing a connection that had opened: a server whose every send
   * fails would never let the scheduler go idle). */
  pick<K extends Lists>(list: K): Setup[K][number] {
    const xs = this.setup[list] as Setup[K][number][];
    const n = this.#turns.get(list) ?? 0;
    this.#turns.set(list, n + 1);
    return n >= HEAL ? (HEALTHY[list] as Setup[K][number]) : xs[n % xs.length]!;
  }

  /** `f`, when the scheduler picks `label`. */
  later(label: string, f: () => void): void {
    void this.s.schedule(Promise.resolve(), label).then(() => {
      this.trace.push(`  ${label}`);
      this.guard(label, f);
    });
  }

  guard(label: string, f: () => void): void {
    try {
      f();
    } catch (e) {
      this.problems.push(`${label} threw: ${(e as Error)?.stack ?? String(e)}`);
    }
  }

  /** A platform call: none once stop has returned, and nothing opened (a connection, a
   * download) once the core has said stopped (RULE-SHELL-07). */
  use(what: string, opens = false, save = false): void {
    // What was let go of at the bound may end after: its file closed, and deleted. A save under
    // way at stop ends whole, unreported (stop doesn't wait for it; a client started next on the
    // state file does: files.ts).
    const unwinding = save || (this.letGo && /^(write|sync|close|remove) /.test(what));
    if (this.stopSettled && !unwinding) this.problems.push(`${what} after stop returned (RULE-SHELL-07)`);
    else if (opens && this.book.stopped) this.problems.push(`${what} once the core said stopped (RULE-SHELL-07)`);
  }

  config(): Config {
    return {
      endpoint: `${this.setup.ws ? "ws" : "http"}://${ENDPOINT_HOST}/v1/opamp`,
      instanceUid: UID,
      capabilities: ACCEPTS_REMOTE_CONFIG,
      heartbeatIntervalMs: HEARTBEAT_MS,
      requestTimeoutMs: REQUEST_TIMEOUT_MS,
      maxReceiveBytes: MAX_RECEIVE,
      stateFile: this.stateFile,
      acceptPackages: true,
      proxy: false,
      mode: this.setup.mode,
      [TEST_PLATFORM]: this.platform(),
      [TEST_FILE_OPS]: this.fs,
    } as Config;
  }

  /** The fakes, and the mocked clock. */
  platform(): TestPlatform & { executor: ExecutorPlatform } {
    const world = this;
    return {
      now: () => Date.now() - T0,
      executor: {
        fetch: fakeFetch(this),
        WebSocket: class extends FakeWs {
          constructor(url: string, opts: { maxPayload?: number }) {
            super(world, url, opts);
          }
        } as never,
      },
      streams: fakeStreams(this),
    };
  }

  start(): void {
    rmSync(this.stateFile, { force: true });
    const client = Client.start(
      this.config(),
      {
        health: create(ComponentHealthSchema, { healthy: true, status: "h0" }),
        customCapabilities: create(CustomCapabilitiesSchema, { capabilities: [CAPABILITY] }),
      },
      this.life.notify,
    );
    this.life.client = client;
    this.life.returned = true;
  }

  /** An application call (as the callback test's): a health, or a custom message the core refuses
   * at once (a callback in the call's own step). */
  call(): void {
    const client = this.life.client!;
    const n = ++this.healths;
    if (n % 2 === 1) client.setHealth(create(ComponentHealthSchema, { healthy: true, status: `h${n}` }));
    else client.sendCustomMessage(create(CustomMessageSchema, { capability: UNDECLARED, type: "x", data: new TextEncoder().encode(String(n)) }));
  }

  failure(): unknown {
    const n = ++this.failures;
    return n % 2 === 0 ? `a string thrown (${n})` : new Error(`the callback failed (${n})`);
  }

  /** stop(), not waited for: when it settles, nothing must be left (RULE-SHELL-07). */
  stop(from: "outside" | "inside"): void {
    const p = this.life.client!.stop();
    p.catch(() => {});
    this.life.stops.push(p);
    if (this.stopCalled) return;
    this.stopCalled = true;
    this.stopCalledAt = Date.now();
    const settled = () => {
      if (this.stopSettled) return;
      this.stopSettled = true;
      const begun = Math.min(this.stopCalledAt ?? Infinity, this.endedAt ?? Infinity);
      this.letGo = Date.now() - begun >= SHELL_STOP_MS;
      this.heldAtStop = this.resources
        .filter((r) => !r.released() && !r.what.startsWith("save handle") && !(this.letGo && r.what.startsWith("file handle")))
        .map((r) => r.what);
      if (this.fs.inFlight > 0 && !this.letGo) this.heldAtStop.push(`${this.fs.inFlight} file operation(s) in flight`);
    };
    void p.then(settled, settled);
    void from;
  }

  check(): void {
    assert.deepEqual(this.problems, [], "the shell contract");
    this.life.check(false);
  }

  /** The end of a run: settle, check every effect answered, stop, check what stop left. */
  async finish(): Promise<void> {
    await this.s.waitIdle();
    this.check();
    const running = !this.life.notes.some((n) => n.kind === "stopped") && !this.stopCalled;
    if (running) this.book.idle();
    this.check();
    this.trace.push("finish: stop");
    if (!this.stopCalled) this.stop("outside");
    // Once nothing else can happen (the scheduler idle), only time is left to pass: stop is over
    // within the bound of it. (Time the commands passed while the scheduler held I/O back is the
    // world's, a slow server or disk, not the shell's.)
    let waited = 0;
    for (;;) {
      await this.s.waitIdle();
      if (this.stopSettled || waited > STOP_BOUND_MS) break;
      mock.timers.tick(250);
      waited += 250;
    }
    assert.ok(this.stopSettled, `stop returned within ${STOP_BOUND_MS} ms of (mocked) time once idle (RULE-SHELL-07)`);
    assert.deepEqual(this.heldAtStop, [], "nothing held once stop returned (RULE-SHELL-07)");
    // Nothing after stop: a call does nothing, nothing is started, no callback comes.
    const told = this.life.notes.length;
    this.call();
    mock.timers.tick(5000);
    await this.s.waitIdle();
    assert.equal(this.life.notes.length, told, "no callback after stop");
    this.book.files();
    // A save let go at stop has ended by now, whole: its file closed (a stalled disk's writes
    // take longer than this run lasts).
    if (!this.setup.stall) {
      const open = this.resources.filter((r) => r.what.startsWith("save handle") && !r.released()).map((r) => r.what);
      assert.deepEqual(open, [], "a save under way at stop ends, its file closed (RULE-SHELL-09)");
    }
    this.check();
    this.life.check(true);
    // The harness's own: what the scheduler kept of the values its tasks resolved with stays
    // small (FakeHandle's toStringMethod), or a long night's run outgrows V8's limits.
    const biggest = Math.max(0, ...this.s.report().map((r) => r.outputValue?.length ?? 0));
    assert.ok(biggest <= 1000, `the scheduler's report holds a value of ${biggest} characters`);
    const thrown = this.life.thrown;
    for (const p of this.life.stops) {
      const r = await p.then(
        () => ({ ok: true as const }),
        (error: unknown) => ({ ok: false as const, error }),
      );
      if (thrown === undefined) assert.ok(r.ok, `stop gives no error, none thrown: ${String((r as { error?: unknown }).error)}`);
      else if (thrown.on !== "stopped") assert.ok(!r.ok && r.error === thrown.value, "stop gives the error thrown (RULE-SHELL-02)");
      else assert.ok(r.ok || r.error === thrown.value, "stop gives nothing or the error");
    }
  }
}

// ---------------------------------------------------------------------------------------------
// The commands.

interface Model {
  stopped: boolean;
  throwArmed: boolean;
}

type Cmd = fc.AsyncCommand<Model, World>;

/** Every command is synchronous: it starts things, and the scheduler decides when they finish. */
abstract class Step implements Cmd {
  check(_m: Readonly<Model>): boolean {
    return true;
  }
  abstract act(m: Model, w: World): void;
  async run(m: Model, w: World): Promise<void> {
    w.trace.push(`command ${this.toString()}`);
    this.act(m, w);
    w.check();
  }
  abstract toString(): string;
}

class Call extends Step {
  constructor(readonly how: "now" | "later") {
    super();
  }
  act(_m: Model, w: World) {
    if (this.how === "now") w.call();
    else w.later("an application call", () => w.call());
  }
  override toString() {
    return `call(${this.how})`;
  }
}

class Inside extends Step {
  constructor(readonly what: "call" | "stop") {
    super();
  }
  act(_m: Model, w: World) {
    w.life.inside.push(this.what);
  }
  override toString() {
    return `inside(${this.what})`;
  }
}

class ThrowOn extends Step {
  constructor(readonly k: number) {
    super();
  }
  override check(m: Readonly<Model>) {
    return !m.throwArmed;
  }
  act(m: Model, w: World) {
    m.throwArmed = true;
    w.life.throwAt = w.life.calls + this.k;
  }
  override toString() {
    return `throwOn(+${this.k})`;
  }
}

class Offer extends Step {
  constructor(readonly kind: OfferKind) {
    super();
  }
  act(_m: Model, w: World) {
    w.server.offer(this.kind);
  }
  override toString() {
    return `offer(${this.kind})`;
  }
}

class ServerEnds extends Step {
  constructor(readonly how: "close" | "going away" | "drop" | "corrupt") {
    super();
  }
  act(_m: Model, w: World) {
    for (const p of w.server.peers) {
      if (this.how === "close") p.serverClose(1000);
      else if (this.how === "going away") p.serverClose(1001);
      else if (this.how === "drop") p.drop();
      else p.corrupt();
    }
  }
  override toString() {
    return `server(${this.how})`;
  }
}

class Tick extends Step {
  constructor(readonly ms: number) {
    super();
  }
  act() {
    mock.timers.tick(this.ms);
  }
  override toString() {
    return `tick(${this.ms})`;
  }
}

class Stop extends Step {
  override check(m: Readonly<Model>) {
    return !m.stopped;
  }
  act(m: Model, w: World) {
    m.stopped = true;
    w.stop("outside");
  }
  override toString() {
    return "stop";
  }
}

// Weighted so that about half the runs end with stop or a throw of their own (the other half
// are stopped at the end), and packages, which need an offer, time (over HTTP, a poll) and many
// steps, are often downloaded.
const commands = [
  fc.oneof(
    { weight: 4, arbitrary: fc.constantFrom("now", "later").map((h) => new Call(h as "now" | "later")) },
    { weight: 2, arbitrary: fc.constantFrom("call", "stop", "call").map((x) => new Inside(x as "call" | "stop")) },
    { weight: 1, arbitrary: fc.integer({ min: 1, max: 4 }).map((k) => new ThrowOn(k)) },
    {
      weight: 8,
      arbitrary: fc.constantFrom<OfferKind>("config", "custom", "heartbeat", "package", "package").map((k) => new Offer(k)),
    },
    {
      weight: 3,
      arbitrary: fc.constantFrom("close", "going away", "drop", "corrupt").map((h) => new ServerEnds(h as ServerEnds["how"])),
    },
    { weight: 8, arbitrary: fc.constantFrom(1, 100, 999, 1000, 2000, 5000).map((ms) => new Tick(ms)) },
    { weight: 1, arbitrary: fc.constant(new Stop()) },
  ),
];

// ---------------------------------------------------------------------------------------------

/** The main test's seed: OPAMP_SCHED_SEED, or one of its own (printed). */
const SCHED_SEED = process.env.OPAMP_SCHED_SEED ? Number(process.env.OPAMP_SCHED_SEED) : (Date.now() ^ (Math.random() * 2 ** 31)) | 0;
/** Every test here prints, when it fails, an annotation with how to replay it (annotate.ts). */
const test = annotatedTest("schedule.properties.test.js", () => ({ OPAMP_SCHED_SEED: SCHED_SEED }));

// covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-06, RULE-SHELL-07, RULE-SHELL-08, RULE-SHELL-09
test("the shell contract holds whatever the order of its I/O (fast-check's scheduler)", async (t) => {
  const seed = SCHED_SEED;
  const numRuns = Number(process.env.OPAMP_SCHED_RUNS ?? 500);
  const path = process.env.OPAMP_SCHED_PATH;
  const replayPath = process.env.OPAMP_SCHED_REPLAY;
  t.diagnostic(`OPAMP_SCHED_SEED=${seed} (OPAMP_SCHED_RUNS=${numRuns})`);
  const started = performance.now();
  const out = await fc.check(property(replayPath), { numRuns, seed, ...(path ? { path } : {}) });
  t.diagnostic(`${numRuns} runs in ${Math.round(performance.now() - started)} ms`);
  if (out.failed) {
    // fc.commands shrinks with a replay path of its own, printed in the counterexample.
    const replay = /replayPath="([^"]*)"/.exec(fc.stringify(out.counterexample))?.[1];
    assert.fail(
      `${fc.defaultReportMessage(out) ?? "failed"}\n${String((out.errorInstance as Error)?.stack ?? out.errorInstance)}\n` +
        `replay: OPAMP_SCHED_SEED=${seed} OPAMP_SCHED_PATH=${out.counterexamplePath}${replay ? ` OPAMP_SCHED_REPLAY=${replay}` : ""} ` +
        `node --test dist/test/schedule.properties.test.js`,
    );
  }
});

/** The property: one run, from the scheduler, the setup and the commands drawn. `replayPath`:
 * fc.commands' own, to replay a shrunk failure exactly. */
function property(replayPath?: string) {
  return fc.asyncProperty(schedulerArb, setupArb, fc.commands(commands, { maxCommands: 40, size: "medium", ...(replayPath ? { replayPath } : {}) }), async (seed, setup, cmds) => {
    const s = scheduler(seed);
    mock.timers.enable({ apis: ["setTimeout", "Date"], now: T0 });
    const w = new World(s, setup);
    current = w;
    if (process.env.OPAMP_SCHED_DEBUG) console.error(`run: ${JSON.stringify(setup)} ${[...cmds].join(" ")}`);
    try {
      w.start();
      await fc.scheduledModelRun(s, () => ({ model: { stopped: false, throwArmed: false }, real: w }), cmds);
      await w.finish();
      if (process.env.OPAMP_SCHED_DEBUG) console.error(`reached: ${setup.mode} ${setup.ws ? "ws" : "http"} ${[...new Set(w.trace.flatMap((l) => [/core <- (\w+)/.exec(l)?.[1], ...[...l.matchAll(/(\w+)\(\d*\)/g)].map((m) => `fx:${m[1]}`), /^command (\w+)/.exec(l)?.[1]]).filter(Boolean))].sort().join(" ")}`);
    } catch (e) {
      if (e instanceof Error) {
        const tail =
          `\n--- what happened (last 100) ---\n${w.trace.slice(-100).join("\n")}` +
          `\n--- the scheduler's order (last 60) ---\n${s.report().slice(-60).map((r) => `${r.status} ${r.label}`).join("\n")}`;
        e.message += tail;
        e.stack += tail;
      }
      throw e;
    } finally {
      if (process.env.OPAMP_SCHED_DEBUG === "2") {
        console.error(`trace:\n${w.trace.join("\n")}\nend trace`);
        console.error(`sched:\n${s.report().map((r) => `${r.taskId} ${r.status} ${r.label}`).join("\n")}\nend sched`);
      }
      current = undefined;
      mock.timers.reset();
    }
  });
}

// Found by the test above (on this branch at its first run; the pinned replay is of the test as
// it is now, seed 1): a send whose write fails
// reported ws_lost from its callback, and ws's close that follows reported the connection's end
// again (executor.ts, wsSend: the failure didn't go through the connection's own end). The
// replay pins that run; the property under it tries every order of the failed write's callback,
// the socket's close and what was in flight.
// covers: RULE-SHELL-04
test("a WebSocket whose write fails ends once (seed 1)", async () => {
  await fc.assert(property("Cf:B"), { seed: 1, path: "2:1:1:1:1:2:2:2" });
  const setup: Setup = {
    mode: "platform",
    ws: true,
    posts: ["ok"],
    upgrades: ["accept"],
    downloads: ["ok"],
    sends: ["ok", "fail"],
    saves: ["ok"],
    connects: ["ok"],
    writes: ["ok"],
    chunk: 64,
    fileWrite: 100,
    deaf: false,
  };
  await fc.assert(
    fc.asyncProperty(schedulerArb, fc.integer({ min: 1, max: 4 }), fc.boolean(), async (seed, sends, push) => {
      const s = scheduler(seed);
      mock.timers.enable({ apis: ["setTimeout", "Date"], now: T0 });
      const w = new World(s, setup);
      try {
        const got: Input[] = [];
        const x = new Executor((i) => got.push(i), w.stateFile, new Transport({ proxy: false }), w.fs, w.platform().executor);
        const id = 1n;
        x.do(create(EffectSchema, { kind: { case: "wsOpen", value: { id, url: "ws://opamp.test/v1/opamp", maxMessageBytes: 1n << 16n, timeoutMs: 5000n } } }));
        await s.waitIdle();
        for (let i = 0; i < sends; i++) {
          x.do(create(EffectSchema, { kind: { case: "wsSend", value: { id, data: new Uint8Array([0, 0x0a, 0]) } } }));
          if (push) w.server.offer("custom");
        }
        await s.waitIdle();
        const ends = got.filter((i) => (i.kind.case === "wsClosed" || i.kind.case === "wsLost") && i.kind.value.id === id);
        const after = ends[0] ? got.slice(got.indexOf(ends[0]) + 1).filter((i) => idOf(i.kind.value) === id) : [];
        assert.equal(ends.length, sends >= 2 ? 1 : 0, `one end: ${got.map((i) => i.kind.case).join(", ")}`);
        assert.deepEqual(after, [], `nothing after the end: ${got.map((i) => i.kind.case).join(", ")}`);
        x.shutdown();
      } finally {
        mock.timers.reset();
      }
    }),
    { numRuns: 100, seed: 92 },
  );
});

// Found by the test above (seed 1, once the double end was fixed): a download that shutdown
// aborted went on after stop had returned: its write in flight, its file's close and the removal
// of what it had written all came later (so a client restarted on the same state file
// could have its own download of the same id deleted). The replay pins that run; the property
// under it shuts the executor down at every point of a download.
// covers: RULE-SHELL-07
test("nothing of a download is left once shutdown is over (seed 1)", async () => {
  await fc.assert(property("MMFAJ:V"), { seed: 1, path: "81:0:0:0:0:0:0:0:0:0:0:0:2:2:10" });
  const setup: Setup = {
    mode: "platform",
    ws: false,
    posts: ["ok"],
    upgrades: ["accept"],
    downloads: ["ok", "cut", "corrupt"],
    sends: ["ok"],
    saves: ["ok"],
    connects: ["ok"],
    writes: ["ok"],
    chunk: 64,
    fileWrite: 100,
    deaf: false,
  };
  await fc.assert(
    fc.asyncProperty(schedulerArb, fc.nat(12), async (seed, steps) => {
      const s = scheduler(seed);
      mock.timers.enable({ apis: ["setTimeout", "Date"], now: T0 });
      const w = new World(s, setup);
      try {
        const got: Input[] = [];
        const x = new Executor((i) => got.push(i), w.stateFile, new Transport({ proxy: false }), w.fs, w.platform().executor);
        w.server.offer("package");
        const effect = { id: 2n, url: `http://${DOWNLOAD_HOST}/pkg/1`, maxBytes: 1n << 20n, timeoutMs: 600_000n };
        x.do(create(EffectSchema, { kind: { case: "download", value: effect } }));
        for (let i = 0; i < steps && s.count() > 0; i++) await s.waitNext(1);
        await s.waitFor(Promise.resolve(x.shutdown()));
        w.stopSettled = true;
        const held = w.resources.filter((r) => !r.released()).map((r) => r.what);
        assert.deepEqual(held, [], "nothing held once shutdown is over");
        assert.equal(w.fs.inFlight, 0, "no file operation in flight once shutdown is over");
        await s.waitIdle();
        assert.deepEqual(w.problems, [], "nothing done after shutdown");
        assert.ok(got.every((i) => i.kind.case === "downloadResponse"), `nothing said of a download given up on: ${got.map((i) => i.kind.case)}`);
      } finally {
        mock.timers.reset();
      }
    }),
    { numRuns: 200, seed: 92 },
  );
});

// Found by the test above (seed 1 then; seed 4 pinned here, once the two above were fixed): stop
// asked for over HTTP while
// a poll was in flight; its answer offered a package, and the core's last output was the goodbye,
// stopped, and a download: the client started the download (a GET to the server after it said
// stopped), only to abort it at shutdown.
// covers: RULE-SHELL-07
test("an output that says stopped starts no download (seed 4)", async () => {
  await fc.assert(property("IHL:F"), { seed: 4, path: "78:4:2:2:2:2:2:2:2:2:2:2:2:8:14" });
});

// Found by the test above (seed 2), once it compared what the core emitted with what the
// application heard: an output carried stopped and then a notification (here the remote config
// that came back with the goodbye's answer; Go and Python saw install_package in the step that
// answers a save after stop). A core bug, fixed elsewhere; but the shell dropped the
// notification, since nothing is told after stopped. Within each output, stopped is now told
// last.
// covers: RULE-SHELL-01
test("stopped is told last within an output (seed 2)", async () => {
  await fc.assert(property("d:B"), { seed: 2, path: "62:1:1:1:1:1:1:1:2:2:2:5:1:2:2:2:2" });
});

// Asked of every shell (Go and Python fixed it): all of stop takes one stop_timeout, measured from
// stop(). Node's stop doesn't wait for a save (one under way ends whole, after); but once the core had
// stopped, the client waited for every download file's write in flight without any bound (core
// mode's Streams.shutdown, and platform mode's aborted downloads): a disk that stops answering held
// stop for as long. A run here stalls every file write, and stops while a package downloads.
// covers: RULE-SHELL-07
test("stop is bounded though a file write hangs", async () => {
  await fc.assert(
    fc.asyncProperty(schedulerArb, fc.constantFrom<"platform" | "core">("platform", "core"), fc.boolean(), fc.nat(40), async (seed, mode, ws, steps) => {
      const s = scheduler(seed);
      const setup: Setup = {
        mode,
        ws,
        posts: ["ok"],
        upgrades: ["accept"],
        downloads: ["ok"],
        sends: ["ok"],
        saves: ["ok"],
        connects: ["ok"],
        writes: ["ok"],
        chunk: 64,
        fileWrite: 1 << 20,
        deaf: false,
        stall: true,
      };
      mock.timers.enable({ apis: ["setTimeout", "Date"], now: T0 });
      const w = new World(s, setup);
      current = w;
      try {
        const dbg = (m: string) => process.env.OPAMP_SCHED_DEBUG && console.error(`stall ${mode} ${ws} ${steps}: ${m} (${s.count()} tasks)`);
        w.start();
        dbg("started");
        await s.waitIdle();
        dbg("idle");
        w.server.offer("package");
        mock.timers.tick(HEARTBEAT_MS);
        dbg("ticked");
        for (let i = 0; i < steps && s.count() > 0; i++) {
          await s.waitNext(1);
        }
        const writing = w.fs.inFlight > 0;
        if (process.env.OPAMP_SCHED_DEBUG) console.error(`stall: stopping (${mode} ${ws} steps=${steps})`);
        w.stop("outside");
        let waited = 0;
        for (;;) {
          await s.waitIdle();
          if (w.stopSettled || waited > STALL_MS) break;
          mock.timers.tick(250);
          waited += 250;
        }
        // The core's stop_timeout (5 s) and the shell's grace once the core has stopped (1 s,
        // STOP_GRACE_MS in src/index.ts), whatever the disk does.
        if (process.env.OPAMP_SCHED_DEBUG) console.error(`stall run: ${mode} ${ws} steps=${steps} waited=${waited} settled=${w.stopSettled}`);
        assert.ok(w.stopSettled && waited <= 6_000, `stop returned within 6 s of stop() (waited ${waited} ms, a write in flight: ${writing})`);
      } finally {
        current = undefined;
        mock.timers.reset();
      }
    }),
    { numRuns: 60, seed: 92 },
  );
});
