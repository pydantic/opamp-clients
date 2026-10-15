// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Helpers for the tests of the client's file operations (files.test.ts, files.properties.test.ts):
// a disk that fails or is slow (FileOps around the real one), the core tap decoded, the tap
// checker run, and an OpAMP server that makes the client save and download.

import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { fileURLToPath } from "node:url";

import { WebSocketServer } from "ws";

import { fromBinary } from "@bufbuild/protobuf";

import { type FileOps, type OpenFile, REAL } from "../src/files.js";
import { ReplySchema, StepSchema } from "../src/gen/opamp/core/v1/core_pb.js";
import {
  type AgentToServer,
  AgentToServerSchema,
  create,
  fromBinary as decode,
  PackageType,
  ServerToAgentSchema,
  toBinary,
} from "../src/index.js";

export const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

export async function until(what: string, done: () => boolean, ms = 20_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!done()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(5);
  }
}

export const sha256 = (b: Uint8Array): string => createHash("sha256").update(b).digest("hex");

// --- A disk that fails ----------------------------------------------------------------------

/** The operations FileOps carries out, by name. */
export type OpName = "create" | "write" | "sync" | "close" | "rename" | "syncDir" | "remove" | "mkdir";

/** What happens to one operation: an error before it (EIO, or `code`); for a write, half of it
 * written and then ENOSPC; a delay; or (rename, syncDir) done, then reported failed. */
export type Fault =
  | { kind: "error"; code?: string }
  | { kind: "half" }
  | { kind: "slow"; ms: number }
  | { kind: "lands" };

export const errno = (code: string, what: string): NodeJS.ErrnoException =>
  Object.assign(new Error(`${code}: injected fault, ${what}`), { code });

/** Calls of FileOps, as the faulty disk saw them: which, on what, at what time, and how long. */
export interface Call {
  op: OpName;
  path: string;
  at: number;
  ms: number;
  fault?: Fault;
}

/** FileOps over `inner` whose `decide` picks a fault (or none) for each operation. */
export function faulty(decide: (op: OpName, path: string) => Fault | undefined, inner: FileOps = REAL) {
  const calls: Call[] = [];
  /** Files made and not closed yet. */
  const open = new Set<OpenFile>();
  async function run<T>(op: OpName, path: string, f: () => Promise<T>, fault: Fault | undefined): Promise<T> {
    const call: Call = { op, path, at: performance.now(), ms: 0, ...(fault ? { fault } : {}) };
    calls.push(call);
    try {
      if (fault?.kind === "error") throw errno(fault.code ?? "EIO", `${op} ${path}`);
      if (fault?.kind === "slow") await sleep(fault.ms);
      const out = await f();
      if (fault?.kind === "lands") throw errno("EIO", `${op} ${path} (done)`);
      return out;
    } finally {
      call.ms = performance.now() - call.at;
    }
  }
  const ops: FileOps = {
    async create(path) {
      const file = await run("create", path, () => inner.create(path), decide("create", path));
      open.add(file);
      let full = false;
      const wrapped: OpenFile = {
        path,
        async write(bufs) {
          const fault = decide("write", path);
          if (fault?.kind !== "half") {
            return run("write", path, async () => {
              if (full) throw errno("ENOSPC", `write ${path}`);
              return file.write(bufs);
            }, fault);
          }
          // A write cut short: half of it goes, the next fails (the disk is full).
          full = true;
          return run("write", path, async () => {
            const total = bufs.reduce((n, b) => n + b.length, 0);
            const half = Math.floor(total / 2);
            if (half === 0) throw errno("ENOSPC", `write ${path}`);
            const first: Uint8Array[] = [];
            let left = half;
            for (const b of bufs) {
              if (left <= 0) break;
              first.push(b.subarray(0, Math.min(left, b.length)));
              left -= b.length;
            }
            return file.write(first);
          }, fault);
        },
        sync: () => run("sync", path, () => file.sync(), decide("sync", path)),
        // A close that fails has let the descriptor go all the same (as close(2) does).
        close: () => {
          const fault = decide("close", path);
          return run("close", path, () => file.close().finally(() => open.delete(file)), fault?.kind === "error" ? { kind: "lands" } : fault);
        },
      };
      return wrapped;
    },
    rename: (from, to) => run("rename", from, () => inner.rename(from, to), decide("rename", from)),
    syncDir: (dir) => run("syncDir", dir, () => inner.syncDir(dir), decide("syncDir", dir)),
    remove: (path) => run("remove", path, () => inner.remove(path), decide("remove", path)),
    mkdir: (dir) => run("mkdir", dir, () => inner.mkdir(dir), decide("mkdir", dir)),
    clear: (dir) => inner.clear(dir),
  };
  return { ops, calls, open };
}

// --- The core tap ----------------------------------------------------------------------------

export interface TapRecord {
  kind: number;
  bytes: Uint8Array;
}

/** A tap's records (a record cut short at the end is left out). */
export function readTap(path: string): TapRecord[] {
  if (!existsSync(path)) return [];
  const data = readFileSync(path);
  const out: TapRecord[] = [];
  for (let at = 0; at + 13 <= data.length; ) {
    const len = data.readUInt32LE(at + 1);
    if (at + 13 + len > data.length) break;
    out.push({ kind: data[at]!, bytes: data.subarray(at + 13, at + 13 + len) });
    at += 13 + len;
  }
  return out;
}

/** A file record, and where it is in the tap. */
export type FileOp = { at: number; op: string; path?: string; from?: string; to?: string; data?: string; len?: number };

export function fileOps(recs: TapRecord[]): FileOp[] {
  return recs.flatMap((r, at) => (r.kind === 5 ? [{ at, ...JSON.parse(Buffer.from(r.bytes).toString("utf8")) }] : []));
}

/** The core's saves (store effects), in order: id, data, where. */
export function stores(recs: TapRecord[]): { at: number; id: bigint; data: Uint8Array }[] {
  const out: { at: number; id: bigint; data: Uint8Array }[] = [];
  recs.forEach((r, at) => {
    if (r.kind !== 4) return;
    for (const e of fromBinary(ReplySchema, r.bytes).output?.effects ?? []) {
      if (e.kind.case === "store") out.push({ at, id: e.kind.value.id, data: e.kind.value.data });
    }
  });
  return out;
}

/** The answers the core was fed for its saves: stored or storeFailed, by id, and where. */
export function storeAnswers(recs: TapRecord[]): { at: number; id: bigint; ok: boolean }[] {
  const out: { at: number; id: bigint; ok: boolean }[] = [];
  recs.forEach((r, at) => {
    if (r.kind !== 3) return;
    const k = fromBinary(StepSchema, r.bytes).input?.kind;
    if (k?.case === "stored") out.push({ at, id: k.value.id, ok: true });
    if (k?.case === "storeFailed") out.push({ at, id: k.value.id, ok: false });
  });
  return out;
}

/** The core's cancels, by id. */
export function cancels(recs: TapRecord[]): bigint[] {
  return recs.flatMap((r) =>
    r.kind === 4
      ? (fromBinary(ReplySchema, r.bytes).output?.effects ?? []).flatMap((e) => (e.kind.case === "cancel" ? [e.kind.value.id] : []))
      : [],
  );
}

/** The tap checker (opamp-tap, which scripts/fetch_core.py fetch puts here), if it is there. */
export const TAP_CHECKER = fileURLToPath(new URL("../../../../target/release/opamp-tap", import.meta.url));

/** Run the tap checker on `tap`: undefined if it isn't there, else its exit code and output. */
export function checkTap(tap: string): { code: number; out: string } | undefined {
  if (!existsSync(TAP_CHECKER)) return undefined;
  const r = spawnSync(TAP_CHECKER, [tap], { encoding: "utf8" });
  return { code: r.status ?? -1, out: `${r.stdout}${r.stderr}` };
}

// --- A server that makes the client save and download -----------------------------------------

export interface ServerOptions {
  /** Offer remote configs 1..configs, one more every `every` messages (1), then the last again. */
  configs?: number;
  every?: number;
  /** The first config's number (1 by default). */
  first?: number;
  /** Offer one package, this file. */
  pkg?: Buffer;
  /** Hold the first GET of the file until release() (then send junk), or the GETs that this says to. */
  hold?: (n: number) => boolean;
}

export interface TestServer {
  url: string;
  msgs: { at: number; msg: AgentToServer }[];
  gets: number;
  /** Requests for the file whose client went away before it was answered. */
  abandoned: number;
  release(): void;
  close(): void;
}

export async function opampServer(o: ServerOptions): Promise<TestServer> {
  const held: http.ServerResponse[] = [];
  const s: TestServer = {
    url: "",
    msgs: [],
    gets: 0,
    abandoned: 0,
    release() {
      for (const res of held.splice(0)) res.end(Buffer.from("OLD".repeat(100)));
    },
    close() {
      s.release();
      for (const c of wss.clients) c.terminate();
      wss.close();
      server.closeAllConnections();
      server.close();
    },
  };
  const server = http.createServer((req, res) => {
    if (req.method === "GET") {
      const n = s.gets++;
      if (o.hold?.(n)) {
        held.push(res);
        res.on("close", () => {
          if (!res.writableEnded) s.abandoned++;
        });
        return;
      }
      return void res.end(o.pkg);
    }
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const out = answer(new Uint8Array(Buffer.concat(chunks)));
      res.writeHead(200, { "Content-Type": "application/x-protobuf" });
      res.end(Buffer.from(out));
    });
  });
  // WebSocket: the same answers, each message after its header byte.
  const wss = new WebSocketServer({ server });
  wss.on("connection", (conn) => {
    conn.on("message", (data: Buffer) => conn.send(Buffer.concat([Buffer.from([0]), answer(new Uint8Array(data.subarray(1)))])));
  });
  function answer(bytes: Uint8Array): Uint8Array {
    const msg = decode(AgentToServerSchema, bytes);
    s.msgs.push({ at: performance.now(), msg });
    const k = Math.min(Math.ceil(s.msgs.length / (o.every ?? 1)), o.configs ?? 0);
    const n = k > 0 ? k + (o.first ?? 1) - 1 : 0;
    const reply = create(ServerToAgentSchema, {
      instanceUid: msg.instanceUid,
      ...(n > 0
        ? { remoteConfig: { configHash: Buffer.from(`${n}`), config: { configMap: { "": { body: Buffer.from(`config ${n}`) } } } } }
        : {}),
    });
    if (o.pkg) {
      const { port } = server.address() as AddressInfo;
      reply.packagesAvailable = create(ServerToAgentSchema, {
        packagesAvailable: {
          allPackagesHash: new Uint8Array([1]),
          packages: {
            pkg: {
              type: PackageType.PackageType_TopLevel,
              version: "1",
              hash: new Uint8Array([2]),
              file: { downloadUrl: `http://127.0.0.1:${port}/file`, contentHash: createHash("sha256").update(o.pkg).digest() },
            },
          },
        },
      }).packagesAvailable;
    }
    return toBinary(ServerToAgentSchema, reply);
  }
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  s.url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1/opamp`;
  return s;
}
