// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Carries out the core's HTTP and WebSocket effects, its saves and downloads, and reports what
// happened as inputs, each naming the effect it belongs to. It decides nothing about OpAMP and
// keeps no time of its own: the core builds every request and reads every response, gives each
// effect its deadline, and cancels what it gives up on (Effect.cancel), after which nothing more
// about that effect is reported.

import { createHash } from "node:crypto";
import { closeSync, fsyncSync, openSync, renameSync, rmSync, writeSync } from "node:fs";
import { dirname, join } from "node:path";

import { create } from "@bufbuild/protobuf";
import { type Dispatcher, fetch } from "undici";
import WebSocket from "ws";

import {
  type Download,
  type DownloadResponse,
  DownloadResponseSchema,
  type Effect,
  type Header,
  HeaderSchema,
  type HttpRequest,
  type Input,
  InputSchema,
  type Origin,
  OriginSchema,
  type WsOpen,
} from "./gen/opamp/core/v1/core_pb.js";
import { type FileOps, REAL, save, track, writeAll } from "./files.js";
import type { StreamsPlatform } from "./streams.js";
import { Transport } from "./transport.js";

type Deliver = (input: Input) => void;
type Kind = Input["kind"];

function input(kind: Kind): Input {
  return create(InputSchema, { kind });
}

function headers(list: Header[]): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const h of list) {
    (out[h.name] ??= []).push(h.value);
  }
  return out;
}

function fromHeaders(entries: Iterable<[string, string | string[] | undefined]>): Header[] {
  const out: Header[] = [];
  for (const [name, value] of entries) {
    for (const v of Array.isArray(value) ? value : value === undefined ? [] : [value]) {
      out.push(create(HeaderSchema, { name, value: v }));
    }
  }
  return out;
}

const unixMs = (): bigint => BigInt(Date.now());

/** Wait for `p`, but `ms` at most (Infinity: no bound): what is still running then is let go. */
export async function waitAtMost(p: Promise<unknown>, ms: number): Promise<void> {
  if (!Number.isFinite(ms)) return void (await p);
  let timer: NodeJS.Timeout | undefined;
  await Promise.race([
    p,
    new Promise<void>((resolve) => {
      timer = setTimeout(resolve, Math.max(0, ms));
      timer.unref();
    }),
  ]);
  clearTimeout(timer);
}

/** What went wrong: fetch says only "fetch failed", and why (a certificate it can't verify, a
 * proxy that refused) is its cause. */
function describe(e: unknown): string {
  if (!(e instanceof Error)) return String(e);
  const cause = e.cause instanceof Error ? e.cause.message : undefined;
  return cause && cause !== e.message ? `${e.message}: ${cause}` : e.message;
}

/** The request target `url` means: its path and, when it has a `?`, its query (an empty one
 * included: WHATWG's `search` leaves it out, and ws builds its target from that). */
export function requestTarget(url: string): string {
  const u = new URL(url);
  // (A `#` in a WHATWG href can only start the fragment, which is never sent.)
  const href = u.href.split("#")[0] ?? "";
  const at = href.indexOf("?");
  return u.pathname + (at < 0 ? "" : href.slice(at));
}

/** Replace what is saved at `path` with `data`, all or nothing: a new file, synced, renamed over
 * the old one, and the rename flushed. Readable by the agent's user only. Synchronous: the client
 * itself saves off its event loop (save, in files.ts), the same way. */
export function saveState(path: string, data: Uint8Array): void {
  const tmp = `${path}.tmp`;
  try {
    // A new file, never one left behind (which could be anyone's, with any mode).
    rmSync(tmp, { force: true });
    const fd = openSync(tmp, "wx", 0o600);
    try {
      writeSync(fd, data);
      fsyncSync(fd);
    } finally {
      closeSync(fd);
    }
    renameSync(tmp, path);
    // The rename itself must reach the disk, or the save isn't done (directories can't be synced
    // on Windows).
    if (process.platform !== "win32") {
      const dir = openSync(dirname(path), "r");
      try {
        fsyncSync(dir);
      } finally {
        closeSync(dir);
      }
    }
  } catch (err) {
    rmSync(tmp, { force: true });
    throw new Error(`saving ${path}: ${(err as Error).message}`);
  }
}

/** What the executor does its network work with: undici's fetch and the ws package's WebSocket.
 * These are Node's; a test replaces them (TEST_PLATFORM) to choose when each one completes
 * (test/schedule.properties.test.ts). Its files go through FileOps (files.ts, TEST_FILE_OPS). */
export interface ExecutorPlatform {
  fetch: typeof fetch;
  WebSocket: typeof WebSocket;
}

/** Node's. */
export const NODE_PLATFORM: ExecutorPlatform = { fetch, WebSocket };

/** Tests only: a client Config carrying this key does its I/O and keeps its time with a
 * TestPlatform's functions instead of Node's (as TEST_CORE_MODULE, core.ts). executor.js is not a
 * package export, so an application can't reach it. */
export const TEST_PLATFORM = Symbol("opamp test platform");

/** What a test replaces (TEST_PLATFORM): each part left out is Node's. */
export interface TestPlatform {
  /** Node mode's HTTP and WebSocket. */
  executor?: ExecutorPlatform;
  /** Rust mode's sockets (streams.ts). */
  streams?: StreamsPlatform;
  /** The client's monotonic clock, in milliseconds (performance.now). */
  now?: () => number;
}

/** Where package files are downloaded, beside the state file. */
export function downloadsDir(stateFile: string): string {
  return `${stateFile}.downloads`;
}

/** The file of download `id`. */
export function downloadPath(stateFile: string, id: bigint): string {
  return join(downloadsDir(stateFile), id.toString());
}

/** One effect under way. */
interface Entry {
  /** Give up on it at once: abort what is under way, drop its socket. */
  readonly abort: () => void;
  /** A connection, once open (and while it closes). */
  ws?: WebSocket;
  /** The core said ws_close. */
  closing?: boolean;
  /** The code of the server's Close, once reported: the connection has ended on the wire. */
  serverClose?: number;
}

/**
 * The effects under way, by id: the one place that decides whether what happened reaches the core
 * (as the other SDKs' in-flight registries). Everything runs on the event loop, so each check and
 * removal is atomic.
 *
 * - Every effect under way (a request, a connection being opened, an open or closing connection,
 *   a download, a save) has one entry.
 * - `cancel` removes the entry and aborts its work at once: a request or a download aborted (the
 *   download's file deleted), a connection dropped, a save not yet begun never begun.
 * - Only `report` and `finish` deliver, and only while the entry is there; `finish` removes it, so
 *   an effect's last word is delivered once, and nothing about it after its cancel.
 * - A resource ready after its entry has gone (a connection that opens after its cancel, or after
 *   `close`) is dropped at once by its owner, who finds no entry.
 * - After `close` nothing starts.
 */
export class InFlight {
  readonly #deliver: Deliver;
  readonly #entries = new Map<bigint, Entry>();
  #closed = false;

  constructor(deliver: Deliver) {
    this.#deliver = deliver;
  }

  /** Effect `id` begins, undone by `abort`; false (and nothing begins) once closed. */
  start(id: bigint, abort: () => void): boolean {
    if (this.#closed) return false;
    this.#entries.set(id, { abort });
    return true;
  }

  /** Effect `id`'s entry, while it is under way. */
  get(id: bigint): Entry | undefined {
    return this.#entries.get(id);
  }

  /** Deliver something effect `id` has to say, if it is still under way. */
  report(id: bigint, i: Input): boolean {
    if (!this.#entries.has(id)) return false;
    this.#deliver(i);
    return true;
  }

  /** Deliver effect `id`'s last word, if it is still under way, and forget it. False: it had gone. */
  finish(id: bigint, i: Input): boolean {
    if (!this.#entries.delete(id)) return false;
    this.#deliver(i);
    return true;
  }

  /** Give up on effect `id` at once: nothing more about it is delivered. */
  cancel(id: bigint): void {
    const e = this.#entries.get(id);
    if (e === undefined) return;
    this.#entries.delete(id);
    e.abort();
  }

  /** Start nothing more, and give up on everything under way. */
  close(): void {
    this.#closed = true;
    for (const id of [...this.#entries.keys()]) this.cancel(id);
  }
}

export class Executor {
  readonly #f: InFlight;
  readonly #stateFile: string | undefined;
  /** TLS and proxy for every request, connection and download. */
  readonly #transport: Transport;
  /** Downloads under way, and download files being deleted: shutdown waits for them. */
  readonly #files = new Set<Promise<void>>();
  readonly #p: ExecutorPlatform;
  /** The file system saves and downloads use (the journal's, with the tap on). */
  readonly #ops: FileOps;
  /** The last save asked for: the next waits for it (saves land one at a time, in order). */
  #lastSave: Promise<void> = Promise.resolve();
  /** Saves, downloads and deletions under way. */
  readonly #work = new Set<Promise<void>>();
  /** The URL of the OpAMP request or connection whose outcome came last (undefined: none yet).
   * Set before the outcome reaches the core: a notification of it finds it in place. */
  used: string | undefined;

  constructor(
    deliver: Deliver,
    stateFile?: string,
    transport: Transport = new Transport(),
    ops: FileOps = REAL,
    platform = NODE_PLATFORM,
  ) {
    this.#f = new InFlight(deliver);
    this.#stateFile = stateFile;
    this.#transport = transport;
    this.#ops = ops;
    this.#p = platform;
  }

  do(effect: Effect): void {
    const k = effect.kind;
    switch (k.case) {
      case "httpRequest": {
        const abort = new AbortController();
        if (!this.#f.start(k.value.id, () => abort.abort())) break;
        const id = k.value.id;
        const url = k.value.url;
        void this.#post(k.value, abort.signal).then((i) => {
          this.used = url;
          this.#f.finish(id, i);
        });
        break;
      }
      case "wsOpen":
        this.#open(k.value);
        break;
      case "wsSend": {
        const { id, data } = k.value;
        const e = this.#f.get(id);
        const ws = e?.ws;
        // A connection closing, or ended on the wire, says how it ends: nothing more goes out.
        if (!ws || e.closing || e.serverClose !== undefined) break;
        ws.send(data, { binary: true }, (err) => {
          // A send that crossed the connection's end is no loss: its end says how it went. Only a
          // write that fails on an open connection ends it.
          if (err && ws.readyState === WebSocket.OPEN && this.#f.get(id) === e) {
            ws.terminate();
            this.#f.finish(id, input({ case: "wsLost", value: { $typeName: "opamp.core.v1.Failed", id, detail: err.message } }));
          }
        });
        break;
      }
      case "wsClose": {
        const id = k.value.id;
        const e = this.#f.get(id);
        if (!e?.ws || e.closing) break;
        e.closing = true;
        if (e.serverClose !== undefined) {
          // Our answer to the server's Close: ws answered it already, and the handshake is done.
          this.#f.finish(id, input({ case: "wsClosed", value: { $typeName: "opamp.core.v1.WsClosed", id, code: e.serverClose } }));
        } else {
          // Its end (the server's answer, or the connection gone) is reported by its close.
          e.ws.close(1000);
        }
        break;
      }
      case "store":
        this.#store(k.value.id, k.value.data);
        break;
      case "cancel":
        // A request, a connection being opened, open or closing, a download (its file deleted), a
        // save (one writing lands whole, unreported; one not begun never begins).
        this.#f.cancel(k.value.id);
        break;
      case "download":
        this.#download(k.value);
        break;
      case "discard":
        if (this.#stateFile !== undefined) {
          // A file that can't be deleted stays until the next start clears the downloads.
          this.#remove(downloadPath(this.#stateFile, k.value.id));
        }
        break;
      default:
        break; // notifications are the client's
    }
  }

  /** Delete a download's file, off the event loop; shutdown waits for it. */
  #remove(path: string): void {
    const removal = this.#ops.remove(path).then(() => {}, () => {});
    this.#files.add(removal);
    void removal.finally(() => this.#files.delete(removal));
    this.#track(removal);
  }

  /** Save `data` off the event loop, after the saves asked for before it, and answer stored once
   * it is on the disk (or store_failed); nothing if the core cancels it first. */
  #store(id: bigint, data: Uint8Array): void {
    const stateFile = this.#stateFile;
    let cancelled = false;
    if (!this.#f.start(id, () => (cancelled = true))) return;
    if (stateFile === undefined) {
      // Answered once the step that asked is over, like any result (its cancel may come first).
      const detail = "no state file";
      queueMicrotask(() => this.#f.finish(id, input({ case: "storeFailed", value: { $typeName: "opamp.core.v1.Failed", id, detail } })));
      return;
    }
    const before = this.#lastSave;
    const done = (async () => {
      await before;
      // Given up on before it began: nothing is written. (Once begun, it ends whole.)
      if (cancelled) return;
      let answer: Input;
      try {
        await save(this.#ops, stateFile, data);
        answer = input({ case: "stored", value: { $typeName: "opamp.core.v1.Id", id } });
      } catch (err) {
        const detail = (err as Error).message;
        answer = input({ case: "storeFailed", value: { $typeName: "opamp.core.v1.Failed", id, detail } });
      }
      this.#f.finish(id, answer);
    })();
    this.#lastSave = done.catch(() => {});
    this.#track(done);
  }

  /** Count `work` as this executor's, and its state file's (a client started next on the file
   * waits for it). */
  #track(work: Promise<void>): void {
    const w = work.catch(() => {});
    this.#work.add(w);
    void w.finally(() => this.#work.delete(w));
    if (this.#stateFile !== undefined) track(this.#stateFile, w);
  }

  /** Once every save, download and deletion under way has ended. */
  async settled(): Promise<void> {
    while (this.#work.size > 0) await Promise.allSettled([...this.#work]);
  }

  /** GET a package file into its path, whatever the status, hashing it as it goes, and report the
   * response; a redirect is not followed (the core decides). On failure, or once cancelled,
   * nothing of it is left. */
  #download(d: Download): void {
    const abort = new AbortController();
    if (!this.#f.start(d.id, () => abort.abort())) return;
    const path = downloadPath(this.#stateFile ?? "", d.id);
    const done = (async () => {
      let answer: Input;
      let made = false;
      try {
        const r = await fetchTo(this.#p, d, path, this.#transport.dispatcher, abort.signal, this.#ops, () => (made = true));
        answer = input({ case: "downloadResponse", value: r });
      } catch (e) {
        // fetchTo deleted the file it made.
        const detail = describe(e);
        answer = input({ case: "downloadFailed", value: { $typeName: "opamp.core.v1.Failed", id: d.id, detail } });
        made = false;
      }
      // Cancelled as it finished: its file goes all the same.
      if (!this.#f.finish(d.id, answer) && made) await this.#ops.remove(path).catch(() => {});
    })();
    this.#files.add(done);
    void done.finally(() => this.#files.delete(done));
    this.#track(done);
  }

  /** Give up on everything under way (S1, S3; the core has cancelled it all already when it
   * stopped): once it is over, nothing of it is left (a download given up on has closed and
   * deleted its file, and a discarded one is deleted: a client started next on the same state
   * file may use the same ids), or `boundMs` has passed (a disk that has stopped answering holds
   * no stop up: that write is let go). A save already writing lands whole, unreported. */
  async shutdown(boundMs = Infinity): Promise<void> {
    this.#f.close();
    this.#transport.close();
    const files = async () => {
      while (this.#files.size > 0) await Promise.allSettled([...this.#files]);
    };
    await waitAtMost(files(), boundMs);
  }

  async #post(r: HttpRequest, signal: AbortSignal): Promise<Input> {
    const failed = (detail: string): Input =>
      input({ case: "httpFailed", value: { $typeName: "opamp.core.v1.Failed", id: r.id, detail } });
    const limit = Number(r.maxResponseBytes);
    try {
      const res = await this.#p.fetch(r.url, {
        method: "POST",
        headers: pairs(r.headers),
        body: r.body.slice(),
        signal,
        // A 3xx is the core's to judge (an error): never followed. (fetch decompresses whatever it
        // is sent; the core asks for identity, so a server sends nothing to decompress.)
        redirect: "manual",
        dispatcher: this.#transport.dispatcher,
      });
      const chunks: Uint8Array[] = [];
      let size = 0;
      if (res.body) {
        for await (const chunk of res.body) {
          size += chunk.length;
          if (size > limit) {
            await res.body.cancel().catch(() => {});
            return failed(`response over ${limit} bytes`);
          }
          chunks.push(chunk);
        }
      }
      const body = new Uint8Array(size);
      let at = 0;
      for (const c of chunks) {
        body.set(c, at);
        at += c.length;
      }
      return input({
        case: "httpResponse",
        value: {
          $typeName: "opamp.core.v1.HttpResponse",
          id: r.id,
          status: res.status,
          headers: fromHeaders(res.headers.entries()),
          body,
          unixMs: unixMs(),
        },
      });
    } catch (e) {
      return failed(describe(e));
    }
  }

  #open(o: WsOpen): void {
    const id = o.id;
    let ws: WebSocket | undefined;
    // Cancelled while it opens: the handshake is aborted (and says nothing).
    if (!this.#f.start(id, () => ws?.terminate())) return;
    const failed = (detail: string, status?: number, hs: Header[] = []): Input => {
      this.used = o.url;
      return input({ case: "wsFailed", value: { $typeName: "opamp.core.v1.WsFailed", id, status, headers: hs, detail, unixMs: unixMs() } });
    };
    const lost = (detail: string): Input => input({ case: "wsLost", value: { $typeName: "opamp.core.v1.Failed", id, detail } });
    // ws takes a maxPayload of 0 as no limit: the core's limit is kept below, on every message.
    const limit = o.maxMessageBytes;
    // No permessage-deflate: the core owns compression (and asks for none). No handshake timeout:
    // the core cancels an open that takes too long.
    const socket = new this.#p.WebSocket(o.url, {
      ...this.#transport.ws(o.url),
      headers: headers(o.headers),
      maxPayload: Number(limit > 0n ? limit : 1n),
      perMessageDeflate: false,
      // The target the URL means, exactly: ws would send "/p" for "ws://h/p?".
      finishRequest: (req) => {
        req.path = requestTarget(o.url);
        req.end();
      },
    });
    ws = socket;
    socket.binaryType = "nodebuffer";
    // An open connection doesn't keep Node.js running (see Config).
    socket.on("upgrade", (res) => res.socket.unref());
    socket.on("unexpected-response", (_req, res) => {
      this.#f.finish(id, failed(`upgrade refused: ${res.statusCode}`, res.statusCode, fromHeaders(Object.entries(res.headers))));
      res.resume();
      socket.terminate();
    });
    socket.on("open", () => {
      const e = this.#f.get(id);
      // Opened after its cancel (or shutdown): closed at once, and nothing said of it (S1).
      if (e === undefined) return void socket.terminate();
      e.ws = socket;
      this.used = o.url;
      this.#f.report(id, input({ case: "wsOpened", value: { $typeName: "opamp.core.v1.Id", id } }));
    });
    socket.on("message", (data: Buffer, isBinary: boolean) => {
      const e = this.#f.get(id);
      // Text messages mean nothing to OpAMP; once the core has closed, nothing but the end is.
      if (!isBinary || e === undefined || e.closing || e.serverClose !== undefined) return;
      if (BigInt(data.length) > limit) {
        // Over the core's limit (one of 0 included): the connection goes, as ws does over its own.
        socket.terminate();
        this.#f.finish(id, lost(`message over ${limit} bytes`));
      } else {
        this.#f.report(id, input({ case: "wsMessage", value: { $typeName: "opamp.core.v1.WsMessage", id, data: new Uint8Array(data) } }));
      }
    });
    socket.on("error", (err: Error) => {
      const e = this.#f.get(id);
      if (e === undefined) return;
      this.#f.finish(id, e.ws ? lost(err.message) : failed(err.message));
    });
    socket.on("close", (code: number) => {
      const e = this.#f.get(id);
      if (e === undefined) return;
      if (!e.ws) {
        this.#f.finish(id, failed("closed before it opened"));
      } else if (code === 1006) {
        // No Close frame at all: the connection dropped.
        this.#f.finish(id, lost(e.closing ? "connection dropped while closing" : "connection dropped"));
      } else if (e.closing) {
        // The end of our close (1005: a Close frame without a code).
        this.#f.finish(id, input({ case: "wsClosed", value: { $typeName: "opamp.core.v1.WsClosed", id, code } }));
      } else {
        // The server's Close (ws has answered it): the core answers with ws_close, which ends it.
        e.serverClose = code;
        this.#f.report(id, input({ case: "wsClosed", value: { $typeName: "opamp.core.v1.WsClosed", id, code } }));
      }
    });
  }
}

/** Where a redirect points: its Location `given` resolved against the URL `requested` by the
 * WHATWG URL parser, and that URL's origin (port 0 for the scheme's default). A Location that
 * doesn't resolve goes as given, with no origin: the core refuses it. */
export function location(requested: string, given: string): { location: string; locationOrigin?: Origin } {
  let u: URL;
  try {
    u = new URL(given, requested);
  } catch {
    return { location: given };
  }
  const scheme = u.protocol.replace(/:$/, "");
  // WHATWG leaves a special scheme's default port out; another's is what the URL says.
  const port = u.port === "" ? 0 : Number(u.port) === DEFAULT_PORTS[scheme] ? 0 : Number(u.port);
  return {
    location: u.href,
    locationOrigin: create(OriginSchema, { scheme, host: u.hostname, port }),
  };
}

/** The ports the url crate takes as a scheme's default (the core reports those as 0). */
const DEFAULT_PORTS: Record<string, number> = { http: 80, https: 443, ws: 80, wss: 443, ftp: 21 };

const pairs = (list: Header[]): [string, string][] =>
  Object.entries(headers(list)).flatMap(([n, vs]) => vs.map((v): [string, string] => [n, v]));

/** Stream `d`'s response, whatever its status, to `path` (no redirect followed), up to its size
 * limit: the response, with its body's SHA-256 and a redirect's Location. `cancel` gives up on it.
 * The file is made new (never one left behind, which could be anyone's: then the download fails;
 * `made` says when it is), and deleted if the download fails or is cancelled after. */
async function fetchTo(
  p: ExecutorPlatform,
  d: Download,
  path: string,
  dispatcher: Dispatcher,
  cancel: AbortSignal,
  ops: FileOps,
  made: () => void,
): Promise<DownloadResponse> {
  const res = await p.fetch(d.url, { headers: pairs(d.headers), signal: cancel, redirect: "manual", dispatcher });
  let file;
  try {
    await ops.mkdir(dirname(path));
    // Given up on meanwhile: no file.
    cancel.throwIfAborted();
    file = await ops.create(path);
    made();
  } catch (e) {
    await res.body?.cancel().catch(() => {});
    throw e;
  }
  const hash = createHash("sha256");
  let size = 0;
  try {
    if (res.body) {
      for await (const chunk of res.body) {
        size += chunk.length;
        if (size > Number(d.maxBytes)) {
          await res.body.cancel().catch(() => {});
          throw new Error(`file over ${d.maxBytes} bytes`);
        }
        hash.update(chunk);
        await writeAll(file, chunk);
      }
    }
    // Given up on while it was written (a body without one too): the file goes.
    cancel.throwIfAborted();
    await file.sync();
  } catch (e) {
    await file.close().catch(() => {});
    await ops.remove(path).catch(() => {});
    throw e;
  }
  await file.close();
  const r = create(DownloadResponseSchema, { id: d.id, status: res.status, sha256: new Uint8Array(hash.digest()) });
  const given = res.status >= 300 && res.status < 400 ? res.headers.get("location") : null;
  if (given !== null) Object.assign(r, location(d.url, given));
  return r;
}
