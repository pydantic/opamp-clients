// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The adversary's harness: the client run on the adversary module, a hostile stand-in
// for the core that hands the shell effect sequences the real core never makes, and checks what
// the shell feeds back against spec/shell-contract.md. This file is the harness and the local
// server the adversary expects: one client per seed, several seeds at once, against a server
// that does what each URL's query says.
//
// Each test runs in both modes (MODES). In Rust mode the client tells the core it speaks streams,
// and each run has a raw server of its own (adversary.raw.ts; raw=PORT and tls=NAME in the
// endpoint's query), which the adversary opens streams to: it hands the shell the stream effects
// (stream_open, stream_write, stream_start_tls, stream_splice, stream_close, download_data...),
// which src/streams.ts carries out, and the platform's as well (http_request, ws_open, download),
// which Rust mode's client passes on to the executor, as a core's saves are. So Rust mode's
// streams and files run against a hostile core, and so does its wiring (Streams is offered every
// effect first; a cancel or a discard may name a save, a platform download or a stream's file),
// its stop and its shutdown. Go's harness runs Rust mode the same way (adversary_raw_test.go).
//
// OPAMP_ADVERSARY_SEEDS seeds per mode (24 by default) from OPAMP_ADVERSARY_BASE (1);
// OPAMP_ADVERSARY_SEED runs one seed alone (replay; --test-name-pattern picks the mode, and
// OPAMP_ADVERSARY_FOCUS the production in focus in the main run, if the seed ran with one: the
// replay line of a failed seed says both); OPAMP_ADVERSARY_WASM is the module (by default
// testdata/core/opamp_core_adversary.wasm in this repository).

import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import { createServer, get, type IncomingMessage, STATUS_CODES, type Server, type ServerResponse } from "node:http";
import type { AddressInfo, Socket } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { WebSocketServer } from "ws";

import { TEST_CORE_MODULE } from "../src/core.js";
import { Client, ComponentHealthSchema, type Config, create, type Notification } from "../src/index.js";
import { rawMark, RawServer } from "./adversary.raw.js";

const PREFIX = "opamp-adversary";
/** stop returns within this, or the seed fails. */
const STOP_DEADLINE_MS = 15_000;
/** After stop has returned: how long connections have to close, and what is watched for. */
const AFTER_STOP_MS = 3_000;
/** A seed whose script never asks to stop. */
const RUN_DEADLINE_MS = 90_000;
/** Seeds at once. */
const PARALLEL = 4;
/**
 * How long after the harness's own marker request a request or upgrade the client had already sent
 * may arrive. The marker is sent the moment stop returns, on a connection of its own, so it reaches
 * the server's handler as late as anything the client sent before then: on a loaded machine the
 * server, which shares this event loop with every seed's client, has taken 171 ms and 348 ms to
 * get to a request that was already on its way (seed 5 on CI), and the 50 ms this allowed from the
 * moment stop returned called that the client's.
 */
const ARRIVAL_GRACE_MS = 50;

/** The modes every test runs in: Node mode (fetch and ws), and Rust mode (the default). */
const MODES = ["node", "rust"] as const;
type Mode = (typeof MODES)[number];

/**
 * Known bugs: the productions that trigger one in this SDK, in each mode, with its S-number.
 * The main run leaves them out (`off`); "known bugs" below runs each in `focus` and expects the
 * run to fail.
 * None today: S7 (a wake_at over 2^31 - 1 ms away armed as 1 ms) is fixed in index.ts (#arm);
 * wake_far, wake_huge and wake_max hold the far wake armed until an answer comes, so a timer that
 * fires early fails the run.
 */
const KNOWN: Record<Mode, Record<string, string>> = { node: {}, rust: {} };
/** Known bugs that are races: their focus runs needn't fail (logged, not asserted). */
const RACES = new Set<string>();

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/** The test certificates (test/fixtures, make.sh): the CA the client is told to trust, and the raw
 * server's certificate under it, which is for localhost, 127.0.0.1 and ::1. */
const fixture = (name: string): string => readFileSync(new URL(`../../test/fixtures/${name}`, import.meta.url), "utf8");
/** The name the raw server's certificate is for, which the adversary starts TLS under (tls=NAME). */
const TLS_NAME = "localhost";

function repoRoot(): string {
  let dir = dirname(fileURLToPath(import.meta.url));
  for (;;) {
    if (existsSync(join(dir, "testdata", "core"))) return dir;
    const up = dirname(dir);
    if (up === dir) throw new Error("the repository root (with testdata/core) not found above this test");
    dir = up;
  }
}

/** The module, compiled once (fails, never skips, when it is missing). */
let compiled: WebAssembly.Module | undefined;
function adversary(): WebAssembly.Module {
  if (compiled) return compiled;
  const path =
    process.env.OPAMP_ADVERSARY_WASM ?? join(repoRoot(), "testdata", "core", "opamp_core_adversary.wasm");
  assert.ok(existsSync(path), `the adversary module is missing: ${path} (scripts/fetch_core.py fetch puts it there)`);
  compiled = new WebAssembly.Module(readFileSync(path));
  return compiled;
}

// ---------------------------------------------------------------------------------------------
// The local server (the README's "The local server").

/** Per run (the `run` query parameter): the open WebSocket connections (an upgrade held counts
 * from its request: the client is still connecting), and when the last request or upgrade came. */
class RunCounts {
  readonly open = new Set<Socket>();
  last = 0;
  lastWhat = "";
  /** When the harness's marker (GET /marker, sent as stop returned) reached the handler; 0: not yet. */
  marker = 0;
}

/** Sends the marker for `run` on a connection of its own; resolves once it is answered (an error's text if it wasn't). */
function mark(port: number, run: string): Promise<string | undefined> {
  return new Promise((resolve) => {
    const req = get({ host: "127.0.0.1", port, path: `/marker?run=${run}`, agent: false }, (res) => {
      res.resume();
      res.on("end", () => resolve(res.statusCode === 204 ? undefined : `status ${res.statusCode}`));
    });
    req.on("error", (e) => resolve(String(e)));
  });
}

/** Byte i is i % 251. */
function payload(size: number): Buffer {
  const b = Buffer.allocUnsafe(size);
  for (let i = 0; i < size; i++) b[i] = i % 251;
  return b;
}

class AdversaryServer {
  readonly http: Server;
  readonly wss = new WebSocketServer({ noServer: true, maxPayload: 256 << 20 });
  readonly runs = new Map<string, RunCounts>();
  readonly sockets = new Set<Socket>();
  readonly timers = new Set<NodeJS.Timeout>();
  port = 0;

  constructor() {
    this.http = createServer((req, res) => void this.#request(req, res));
    this.http.on("connection", (s: Socket) => {
      this.sockets.add(s);
      s.on("close", () => this.sockets.delete(s));
    });
    this.http.on("upgrade", (req: IncomingMessage, socket: Socket, head: Buffer) => this.#upgrade(req, socket, head));
    // An odd request (a header the parser refuses) is the client's to fail: just drop it.
    this.http.on("clientError", (_e, socket: Socket) => socket.destroy());
  }

  async listen(): Promise<void> {
    await new Promise<void>((r) => this.http.listen(0, "127.0.0.1", r));
    this.port = (this.http.address() as AddressInfo).port;
  }

  close(): void {
    for (const t of this.timers) clearTimeout(t);
    for (const c of this.wss.clients) c.terminate();
    for (const s of this.sockets) s.destroy();
    this.wss.close();
    this.http.close();
  }

  run(seed: string): RunCounts {
    let r = this.runs.get(seed);
    if (!r) {
      r = new RunCounts();
      this.runs.set(seed, r);
    }
    return r;
  }

  #later(ms: number, f: () => void): void {
    const t = setTimeout(() => {
      this.timers.delete(t);
      f();
    }, ms);
    this.timers.add(t);
  }

  async #request(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const url = new URL(req.url ?? "/", "http://x");
    const run = this.run(url.searchParams.get("run") ?? "");
    if (url.pathname === "/marker") {
      // The harness's own, not the client's: when what was sent as stop returned gets here.
      run.marker = performance.now();
      req.resume();
      res.writeHead(204).end();
      return;
    }
    run.last = performance.now();
    run.lastWhat = `${req.method} ${req.url}`;
    const q = (k: string, d: number) => Number(url.searchParams.get(k) ?? d);
    const ok = (url.pathname === "/h" && req.method === "POST") || (url.pathname === "/d" && req.method === "GET");
    req.resume();
    if (!ok) {
      res.writeHead(404).end();
      return;
    }
    const answer = () => {
      if (res.destroyed || req.socket.destroyed) return;
      if (q("drop", 0) === 1) {
        req.socket.destroy();
        return;
      }
      const size = q("size", 0);
      res.writeHead(q("status", 200), { "Content-Length": size, "Content-Type": "application/octet-stream" });
      res.end(payload(size));
    };
    const delay = q("delay", 0);
    if (delay < 0) {
      // Never answered: until the client goes, or 30 s.
      this.#later(30_000, () => req.socket.destroy());
    } else if (delay > 0) {
      this.#later(delay, answer);
    } else {
      answer();
    }
  }

  #upgrade(req: IncomingMessage, socket: Socket, head: Buffer): void {
    const url = new URL(req.url ?? "/", "http://x");
    const run = this.run(url.searchParams.get("run") ?? "");
    run.last = performance.now();
    run.lastWhat = `upgrade ${req.url}`;
    run.open.add(socket);
    socket.on("close", () => run.open.delete(socket));
    socket.on("error", () => {});
    const q = (k: string) => Number(url.searchParams.get(k) ?? 0);
    if (url.pathname !== "/w") {
      socket.end("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
      return;
    }
    const answer = () => {
      if (socket.destroyed) return;
      const status = q("status");
      if (status !== 0) {
        socket.end(`HTTP/1.1 ${status} ${STATUS_CODES[status] ?? "Refused"}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n`);
        return;
      }
      this.wss.handleUpgrade(req, socket, head, (ws) => {
        ws.on("error", () => {});
        for (let i = 0; i < q("push"); i++) ws.send(payload(16), { binary: true });
        if (q("echo") === 1) ws.on("message", (data: Buffer, binary: boolean) => binary && ws.send(data, { binary: true }));
        if (q("close") > 0) this.#later(q("close"), () => ws.close(1000));
        if (q("drop") > 0) this.#later(q("drop"), () => ws.terminate());
      });
    };
    const delay = q("delay");
    if (delay < 0) this.#later(30_000, () => socket.destroy());
    else if (delay > 0) this.#later(delay, answer);
    else answer();
  }
}

// ---------------------------------------------------------------------------------------------
// The harness (the README's "The harness, in every SDK").

interface Verdict {
  seed: string;
  steps: number;
  violations: number;
  stored: string;
  used: string[];
  enabled: string[];
}

function parseVerdict(detail: string): Verdict {
  const kv = new Map<string, string>();
  for (const part of detail.slice(`${PREFIX} verdict `.length).split(" ")) {
    const i = part.indexOf("=");
    if (i > 0) kv.set(part.slice(0, i), part.slice(i + 1));
  }
  const list = (k: string) => (kv.get(k) ?? "").split(",").filter((s) => s !== "");
  return {
    seed: kv.get("seed") ?? "",
    steps: Number(kv.get("steps")),
    violations: Number(kv.get("violations")),
    stored: kv.get("stored") ?? "",
    used: list("used"),
    enabled: list("enabled"),
  };
}

interface SeedResult {
  seed: number;
  failures: string[];
  verdict: Verdict | undefined;
}

/** A listing of the downloads directory: name, size and modification time of each file. */
function listing(dir: string): string {
  try {
    return readdirSync(dir)
      .sort()
      .map((f) => {
        try {
          const s = statSync(join(dir, f));
          return `${f}:${s.size}:${s.mtimeMs}`;
        } catch {
          return `${f}:gone`;
        }
      })
      .join(" ");
  } catch {
    return "";
  }
}

/** The store id in data the adversary saved ("opamp-adversary seed S store ID ..."), if any. */
function storeId(data: Buffer): number | undefined {
  const m = /^opamp-adversary seed \d+ store (\d+) /.exec(data.subarray(0, 80).toString("latin1"));
  return m ? Number(m[1]) : undefined;
}

async function runSeed(server: AdversaryServer, seed: number, query: string, mode: Mode): Promise<SeedResult> {
  const failures: string[] = [];
  const fail = (s: string) => failures.push(s);
  const dir = mkdtempSync(join(tmpdir(), `opamp-adversary-${seed}-`));
  const stateFile = join(dir, "state.bin");
  const downloads = `${stateFile}.downloads`;
  const run = server.run(String(seed));
  let returned = false;
  let depth = 0;
  let stoppedSeen = false;
  let lastNote = 0;
  const verdicts: string[] = [];
  let client: Client | undefined;
  // The stop the application made (inside, outside, or after the client ended on its own).
  let stopAsked: ((p: { stop: Promise<void> }) => void) | undefined;
  const stopped = new Promise<{ stop: Promise<void> }>((r) => (stopAsked = r));
  const askStop = (p: Promise<void>) => {
    p.catch(() => {});
    stopAsked?.({ stop: p });
    stopAsked = undefined;
  };
  // The status says where the call was made: the adversary checks that one made inside the
  // callback reaches it before anything the client takes in next (RULE-SHELL-10).
  const call = (status: "inside" | "outside") =>
    client?.setHealth(create(ComponentHealthSchema, { healthy: true, status }));

  const notify = (n: Notification) => {
    if (!returned) fail(`a callback (${n.kind}) before start returned (RULE-SHELL-01)`);
    if (depth > 0) fail(`two callbacks at once (${n.kind}) (RULE-SHELL-01)`);
    if (stoppedSeen) fail(`a callback (${n.kind}) after stopped (RULE-SHELL-01)`);
    depth++;
    try {
      if (n.kind === "badMessage" && n.detail.startsWith(PREFIX)) {
        const d = n.detail;
        const note = /^opamp-adversary note (\d+)$/.exec(d);
        if (note) {
          const k = Number(note[1]);
          if (k !== lastNote + 1) fail(`note ${k} after note ${lastNote} (RULE-SHELL-01)`);
          lastNote = k;
        } else if (d === `${PREFIX} call inside`) {
          call("inside");
        } else if (d === `${PREFIX} call outside`) {
          setImmediate(() => call("outside"));
        } else if (d === `${PREFIX} stop inside`) {
          // Requested, not waited for: waiting here deadlocks (RULE-SHELL-03).
          askStop(client!.stop());
        } else if (d === `${PREFIX} stop outside`) {
          setImmediate(() => askStop(client!.stop()));
        } else if (d.startsWith(`${PREFIX} violation`)) {
          fail(d);
        } else if (d.startsWith(`${PREFIX} verdict`)) {
          verdicts.push(d);
        }
      } else if (n.kind === "stopped") {
        stoppedSeen = true;
        if (n.error !== undefined) fail(`the client stopped with an error: ${String(n.error)}`);
        // Ended on its own (or after a stop): stop gives the result either way.
        setImmediate(() => askStop(client!.stop()));
      }
    } finally {
      depth--;
    }
  };

  // In Rust mode the run has a raw server of its own, which the adversary opens streams to
  // (raw=PORT), and starts TLS with under the name its certificate is for (tls=NAME): the client
  // trusts the test CA.
  let raw: RawServer | undefined;
  let tls: Config["tls"];
  if (mode === "rust") {
    raw = new RawServer({ cert: fixture("server.pem"), key: fixture("server-key.pem") });
    try {
      await raw.listen();
    } catch (e) {
      fail(`the raw server: ${String(e)}`);
      return { seed, failures, verdict: undefined };
    }
    query += `&raw=${raw.port}&tls=${TLS_NAME}`;
    tls = { ca: fixture("ca.pem") };
  }
  // In either mode the adversary hands the shell platform effects, for the executor's fetch and
  // ws. No proxy: Rust mode reads HTTP_PROXY and the like, and the local server is reached directly.
  const config: Config = { endpoint: `http://127.0.0.1:${server.port}/opamp?seed=${seed}${query}`, stateFile, mode, proxy: false, tls };
  try {
    client = Client.start(
      Object.assign(config, { [TEST_CORE_MODULE]: adversary() }),
      // A status, so the step before start (setHealth) has its notes too (notify_start).
      { health: create(ComponentHealthSchema, { healthy: true, status: "start" }) },
      notify,
    );
  } catch (e) {
    fail(`start threw: ${String(e)}`);
    raw?.close();
    return { seed, failures, verdict: undefined };
  }
  returned = true;

  // Until the application stops (as the script asks), or the client ends on its own.
  let deadline: NodeJS.Timeout | undefined;
  const asked = await Promise.race([
    stopped,
    new Promise<undefined>((r) => {
      deadline = setTimeout(() => r(undefined), RUN_DEADLINE_MS);
    }),
  ]);
  clearTimeout(deadline);
  let stop: Promise<void>;
  if (asked === undefined) {
    fail(`the script never asked to stop in ${RUN_DEADLINE_MS} ms`);
    stop = client.stop();
  } else {
    stop = asked.stop;
  }
  let timer: NodeJS.Timeout | undefined;
  const result = await Promise.race([
    stop.then(
      () => "ok",
      (e: unknown) => `the client stopped with an error: ${String(e)}`,
    ),
    new Promise<string>((r) => {
      timer = setTimeout(() => r("timeout"), STOP_DEADLINE_MS);
    }),
  ]);
  clearTimeout(timer);
  const returnedAt = performance.now();
  // Sent now, on a connection of its own: what the client sent before stop returned reaches the
  // server's handler by the time this does (ARRIVAL_GRACE_MS).
  const marked = mark(server.port, String(seed));
  // And the raw server's, the same way. From here a connection the server was told not to read is
  // read: only that tells it the connection has gone.
  const rawMarked = raw && rawMark(raw.port);
  raw?.release();
  if (result === "timeout") fail(`stop did not return within ${STOP_DEADLINE_MS} ms (RULE-SHELL-07)`);
  else if (result !== "ok" && !failures.includes(result)) fail(result);
  if (result !== "timeout" && !stoppedSeen) fail("stop returned, and no stopped came (RULE-SHELL-02)");

  // After stop: connections close (within the window), nothing new arrives, downloads are done.
  const before = listing(downloads);
  const until = returnedAt + AFTER_STOP_MS;
  while (performance.now() < until) await sleep(50);
  if (run.open.size > 0) fail(`${run.open.size} connection(s) still open ${AFTER_STOP_MS} ms after stop (RULE-SHELL-07)`);
  // A connection the client gave up on at stop may still reach the server's handler after stop
  // returned (its connect was already sent), but not after the marker sent then has.
  const markerError = await marked;
  if (markerError !== undefined) fail(`the harness's marker request failed: ${markerError}`);
  else if (run.last > run.marker + ARRIVAL_GRACE_MS) {
    fail(
      `a request or upgrade (${run.lastWhat}) arrived ${Math.round(run.last - returnedAt)} ms after stop returned, ` +
        `${Math.round(run.last - run.marker)} ms after a request the harness sent then (RULE-SHELL-07)`,
    );
  }
  if (raw) {
    // The streams: every connection gone, and none made after stop returned.
    const rawError = await rawMarked;
    const late = raw.afterMark(ARRIVAL_GRACE_MS);
    if (rawError !== undefined) fail(`the harness's marker on the raw server failed: ${rawError}`);
    else if (late !== undefined) fail(late);
    const { all, anonymous } = raw.open();
    if (all > 0) {
      fail(`RULE-SHELL-07: ${all} stream connection(s) still open ${AFTER_STOP_MS} ms after stop returned (${anonymous} never said who they were)`);
    }
  }
  const after = listing(downloads);
  if (after !== before) fail(`a download changed after stop (S3, RULE-SHELL-07): [${before}] -> [${after}]`);

  // The verdict: one, without violations, and the state file holds what was last stored.
  let verdict: Verdict | undefined;
  if (verdicts.length !== 1) fail(`${verdicts.length} verdicts, not one`);
  else {
    verdict = parseVerdict(verdicts[0]!);
    if (verdict.violations > 0) fail(`the verdict counts ${verdict.violations} violation(s): ${verdicts[0]}`);
    const data = existsSync(stateFile) ? readFileSync(stateFile) : undefined;
    if (verdict.stored !== "-") {
      const want = Buffer.from(verdict.stored, "hex");
      // That data, or a later save's (RULE-SHELL-09: a save whose answer the core no longer took).
      const ok = data !== undefined && (data.equals(want) || (storeId(data) ?? 0) > (storeId(want) ?? Infinity));
      if (!ok) fail(`the state file doesn't hold what was last stored (RULE-SHELL-09): ${data?.toString("hex") ?? "no file"}`);
    }
  }
  for (const s of run.open) s.destroy();
  raw?.close();
  rmSync(dir, { recursive: true, force: true });
  return { seed, failures, verdict };
}

/** The seeds of this run: OPAMP_ADVERSARY_SEED alone, or the batch. */
function seeds(): number[] {
  if (process.env.OPAMP_ADVERSARY_SEED) return [Number(process.env.OPAMP_ADVERSARY_SEED)];
  const count = Number(process.env.OPAMP_ADVERSARY_SEEDS ?? 24);
  const base = Number(process.env.OPAMP_ADVERSARY_BASE ?? 1);
  return Array.from({ length: count }, (_, i) => base + i);
}

/** Run `list`, PARALLEL at once, each client in `mode`. */
async function runAll(list: number[], query: (seed: number) => string, mode: Mode): Promise<SeedResult[]> {
  const server = new AdversaryServer();
  await server.listen();
  try {
    const out: SeedResult[] = [];
    let next = 0;
    await Promise.all(
      Array.from({ length: Math.min(PARALLEL, list.length) }, async () => {
        while (next < list.length) {
          const seed = list[next++]!;
          out.push(await runSeed(server, seed, query(seed), mode));
        }
      }),
    );
    return out.sort((a, b) => a.seed - b.seed);
  } finally {
    server.close();
  }
}

/** How to run `seed` alone, in `mode` (every test here is named for its mode), with `focus` in
 * focus in the main run if it ran so. */
const replay = (seed: number, mode: Mode, focus?: string) =>
  `replay: OPAMP_ADVERSARY_SEED=${seed}${focus ? ` OPAMP_ADVERSARY_FOCUS=${focus}` : ""} node --test --test-name-pattern="^${mode} mode" dist/test/adversary.properties.test.js (in node/opamp, after tsc -p .)`;

const off = (names: string[]) => (names.length ? `&off=${names.join(",")}` : "");

for (const mode of MODES) {
  // covers: RULE-SHELL-01, RULE-SHELL-02, RULE-SHELL-03, RULE-SHELL-04, RULE-SHELL-05, RULE-SHELL-07, RULE-SHELL-08, RULE-SHELL-09, RULE-SHELL-10
  test(`${mode} mode: the shell survives the adversary: every seed, no violation`, async (t) => {
    const list = seeds();
    const known = Object.keys(KNOWN[mode]);
    // A replay of a seed that ran with a production in focus (below) names it.
    const focus = process.env.OPAMP_ADVERSARY_FOCUS;
    t.diagnostic(`seeds ${list[0]}..${list.at(-1)}; known bugs left out: ${known.join(", ")}`);
    const results = await runAll(list, () => `${focus ? `&focus=${focus}` : ""}${off(known)}`, mode);
    const report = (rs: SeedResult[], inFocus: (seed: number) => string | undefined) =>
      rs.map((r) => `seed ${r.seed}:\n  ${r.failures.join("\n  ")}\n  ${replay(r.seed, mode, inFocus(r.seed))}`).join("\n");
    const failed = results.filter((r) => r.failures.length > 0);
    assert.equal(failed.length, 0, `${failed.length} of ${results.length} seeds failed:\n${report(failed, () => focus)}`);
    // Across the batch, every production enabled ran somewhere (when the batch is not one seed).
    if (list.length > 1) {
      const used = new Set(results.flatMap((r) => r.verdict?.used ?? []));
      const enabled = new Set(results.flatMap((r) => r.verdict?.enabled ?? []));
      let never = [...enabled].filter((p) => !used.has(p));
      if (never.length > 0) {
        // A production is drawn at random, and runs only if the run's timing leaves it room (a far
        // wake only while nothing with a deadline is under way), so one can miss a batch by
        // chance: wake_huge ran in two or three of these 24 seeds, and in none with every test
        // file running at once on a loaded machine (in Rust mode). Each missed one runs in focus,
        // on seeds past the batch's, before it is called one that can't run (as Java's harness
        // does).
        const next = list.at(-1)! + 1;
        const again = new Map(never.flatMap((p, i) => [0, 1].map((k): [number, string] => [next + 2 * i + k, p])));
        t.diagnostic(`not used in the batch, run in focus on seeds ${next}..${next + again.size - 1}: ${never.join(", ")}`);
        const more = await runAll([...again.keys()], (seed) => `&focus=${again.get(seed)}${off(known)}`, mode);
        const bad = more.filter((r) => r.failures.length > 0);
        assert.equal(bad.length, 0, `${bad.length} of ${more.length} seeds in focus failed:\n${report(bad, (seed) => again.get(seed))}`);
        for (const r of more) for (const p of r.verdict?.used ?? []) used.add(p);
        never = never.filter((p) => !used.has(p));
      }
      assert.deepEqual(never, [], `productions enabled but never used in the batch, nor in focus: ${never.join(", ")}`);
    }
  });
}

for (const mode of MODES) {
  // covers: RULE-SHELL-05, RULE-SHELL-07, RULE-SHELL-08
  test(`${mode} mode: known bugs still fail in focus: when one passes, it looks fixed`, async (t) => {
    const per = Number(process.env.OPAMP_ADVERSARY_KNOWN_SEEDS ?? 4);
    const names = Object.keys(KNOWN[mode]);
    // Seeds of their own (the server counts per seed): 1000 + 100 * the entry + k.
    const runs = names.flatMap((name, i) => Array.from({ length: per }, (_, k) => ({ seed: 1000 + 100 * i + k, name })));
    const query = new Map(runs.map((r) => [r.seed, `&focus=${r.name}${off(names.filter((n) => n !== r.name))}`]));
    const results = await runAll(
      runs.map((r) => r.seed),
      (seed) => query.get(seed)!,
      mode,
    );
    const problems: string[] = [];
    for (const name of names) {
      const mine = results.filter((r) => runs.find((x) => x.seed === r.seed)?.name === name);
      const failing = mine.filter((r) => r.failures.length > 0);
      const used = mine.filter((r) => r.verdict?.used.includes(name));
      if (failing.length > 0) {
        t.diagnostic(`${name} (${KNOWN[mode][name]}): seed ${failing[0]!.seed}: ${failing[0]!.failures[0]}`);
      } else if (RACES.has(name)) {
        t.diagnostic(`${name} (${KNOWN[mode][name]}): no seed failed this time (a race; tolerated)`);
      } else if (used.length === 0) {
        problems.push(`${name}: not drawn in ${per} seeds in focus (more seeds: OPAMP_ADVERSARY_KNOWN_SEEDS)`);
      } else {
        problems.push(`${name} no longer fails in focus (seeds ${used.map((r) => r.seed).join(", ")}): the bug looks fixed, take it off KNOWN (${KNOWN[mode][name]})`);
      }
    }
    assert.deepEqual(problems, []);
  });
}

/** The cancel productions (RULE-SHELL-06), in every run's grammar. */
const CANCELS = [
  "cancel_download",
  "cancel_http",
  "cancel_http_done",
  "cancel_store",
  "cancel_twice",
  "cancel_ws_closing",
  "cancel_ws_opening",
  "stop_cancel",
];
/** And those of the streams and their files, in a Rust mode run's. */
const STREAM_CANCELS = ["cancel_file", "cancel_splice", "cancel_stream_open", "cancel_stream_opening", "cancel_stream_resolving"];

// RULE-SHELL-06: nothing after a cancel, a late resource released. Every cancel production is
// enabled; each runs in focus, and then the whole batch.
for (const mode of MODES) {
  // covers: RULE-SHELL-06
  test(`${mode} mode: cancel: RULE-SHELL-06`, async (t) => {
    const [r] = await runAll([1], () => `&steps=5${off(Object.keys(KNOWN[mode]))}`, mode);
    assert.ok(r?.verdict, `a verdict: ${r?.failures.join("; ")}`);
    const cancels = r.verdict.enabled.filter((p) => p.startsWith("cancel_") || p === "stop_cancel");
    assert.deepEqual(
      [...CANCELS, ...(mode === "rust" ? STREAM_CANCELS : [])].filter((p) => !cancels.includes(p)),
      [],
      `every cancel production enabled: ${r.verdict.enabled.join(",")}`,
    );
    // Two seeds of each in focus, PARALLEL seeds at once as in the batch (each production with
    // its own server): one production after another, half the slots sat idle on the clocks.
    const focused: string[][] = cancels.map(() => []);
    let nextProd = 0;
    await Promise.all(
      Array.from({ length: Math.max(1, Math.floor(PARALLEL / 2)) }, async () => {
        while (nextProd < cancels.length) {
          const i = nextProd++;
          const prod = cancels[i]!;
          const query = `&focus=${prod}${off(Object.keys(KNOWN[mode]))}`;
          for (const x of await runAll(seeds().slice(0, 2), () => query, mode)) {
            if (x.failures.length > 0) focused[i]!.push(`seed ${x.seed}, ${prod} in focus: ${x.failures.join("; ")} ${replay(x.seed, mode, prod)}`);
          }
        }
      }),
    );
    const problems: string[] = focused.flat();
    t.diagnostic(`in focus: ${cancels.join(", ")}`);
    const results = await runAll(seeds(), () => off(Object.keys(KNOWN[mode])), mode);
    for (const x of results.filter((x) => x.failures.length > 0)) problems.push(`seed ${x.seed}: ${x.failures.join("; ")} ${replay(x.seed, mode)}`);
    assert.deepEqual(problems, []);
  });
}
