// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Rust mode: the core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5 itself,
// and this carries out only what needs the platform: TCP connections, TLS on them (Node's, with
// the operator's CAs, certificate and versions), name resolution (only for a SOCKS5 proxy with
// proxy.resolveLocally: otherwise the proxy resolves), the proxy Node's convention picks for a
// URL, and the files of downloads. It decides nothing: every byte it writes, the core wrote, and
// the core keeps every time (it closes a stream it gives up on, and cancels a download's file).
//
// Download files are written off the event loop (fs/promises, through the client's FileOps:
// src/files.ts), made new (never one left behind), one write at a time per file, in the order the
// bytes came: what is queued meanwhile goes in the next write, and is hashed once it is written.
// Each download ends here once, answering the shell's download_done (synced, then downloaded;
// or download_failed if a write failed: what came after is dropped, and the file deleted), or
// not at all when the core discards or cancels it first: then the file goes once
// the write in flight is over, and nothing more is said about it.
//
// Backpressure, both ways:
// - Socket to file. Once more than HIGH_WATER bytes wait for a file's writes, the socket that
//   brought the last of them is paused (spliced bytes' socket, or the one whose data the core
//   turned into download_data just then), and resumed when they are down to LOW_WATER. Nothing is
//   dropped or reordered: a paused socket keeps what it has read, and the kernel's window stops
//   the server.
// - Core to socket. The core writes to a stream without hearing back: there is no "drained"
//   input (none is needed: what the core writes is bounded by what it is asked to send, one
//   request or message at a time, plus pongs and closing frames answering the peer). What the
//   peer could make us buffer is answers to what it sends while it reads nothing (pings, say).
//   So when a write finds the socket's buffer full (socket.write says false), reading from that
//   socket stops until it drains: a peer that doesn't read hears nothing more to answer (and an
//   HTTP server, which answers once it has read the request, loses nothing by it). And if
//   more than MAX_UNSENT bytes are still waiting when the core writes again, the stream fails
//   (stream_failed; the core reconnects as for any lost connection) rather than growing without
//   end.

import { createHash, type Hash } from "node:crypto";
import { lookup } from "node:dns";
import net from "node:net";
import { dirname } from "node:path";
import tls from "node:tls";

import { create } from "@bufbuild/protobuf";

import { type Effect, type Input, InputSchema } from "./gen/opamp/core/v1/core_pb.js";
import { downloadPath, waitAtMost } from "./executor.js";
import { type FileOps, type OpenFile, REAL, track } from "./files.js";
import type { Transport } from "./transport.js";

type Deliver = (input: Input) => void;

/** What Rust mode does its network work with: Node's sockets and name resolution. A test
 * replaces them (TEST_PLATFORM, executor.ts) to choose when each one completes. Its files go
 * through FileOps (files.ts, TEST_FILE_OPS). */
export interface StreamsPlatform {
  connect: (options: net.NetConnectOpts) => net.Socket;
  tlsConnect: (options: tls.ConnectionOptions) => tls.TLSSocket;
  lookup: typeof lookup;
}

/** Node's. */
export const NODE_STREAMS: StreamsPlatform = { connect: net.connect, tlsConnect: tls.connect, lookup };

/** An input of kind `case` (create fills in the message type from the schema). */
function input(kind: { case: NonNullable<Input["kind"]["case"]>; value: object }): Input {
  return create(InputSchema, { kind } as never);
}

const failed = (id: bigint, detail: string): Input =>
  input({ case: "streamFailed", value: { id, detail } });

/** How long a closed stream has to flush what was written before it is destroyed. */
const LINGER_MS = 2_000;

/** The most buffers one write takes. */
const MAX_WRITEV = 256;
/** Bytes waiting for a file's writes beyond which the socket they come from is paused... */
const HIGH_WATER = 4 << 20;
/** ...until they are down to this. */
const LOW_WATER = 1 << 20;
/** Bytes written to a socket and not yet taken by the peer beyond which the next write fails the
 * stream. */
const MAX_UNSENT = 16 << 20;

/** A download's file. */
interface File {
  id: bigint;
  path: string;
  /** Open, once the first write has made it. */
  handle: OpenFile | undefined;
  /** Made by this client: only then may it delete it. */
  made: boolean;
  /** Bytes waiting for the next write, in order. */
  queue: Uint8Array[];
  /** How many bytes are queued or being written. */
  unwritten: number;
  /** Sockets paused until the bytes waiting are down to LOW_WATER. */
  paused: Set<net.Socket>;
  /** Of the bytes written. */
  hash: Hash;
  /** A write is running (the pump): only it touches the handle. */
  busy: boolean;
  /** download_done came: sync, close and answer once everything queued is written. */
  done: boolean;
  /** Discarded or cancelled (or shut down): delete it, say nothing. */
  gone: boolean;
  /** A write failed: the file is deleted, and download_done is answered with this. */
  error: string | undefined;
}

export class Streams {
  readonly #deliver: Deliver;
  readonly #transport: Transport;
  readonly #stateFile: string | undefined;
  /** Each stream's socket: the TCP one, or the TLS one on it once TLS is up. */
  readonly #sockets = new Map<bigint, net.Socket>();
  /** Sockets the core closed, flushing what was written (LINGER_MS at most): shutdown destroys them. */
  readonly #lingering = new Set<net.Socket>();
  /** The names being resolved, by stream: an answer is reported only while its own entry is here. */
  readonly #resolving = new Map<bigint, object>();
  readonly #files = new Map<bigint, File>();
  /** Files being written or deleted: shutdown waits for them. */
  readonly #pumps = new Set<Promise<void>>();
  #closed = false;
  /** Streams whose next bytes go to a download's file: which download, and how many are left. */
  readonly #splices = new Map<bigint, { download: bigint; left: number }>();
  /** Why each paused socket is paused: how many files it waits for, and a full write buffer. */
  readonly #holds = new Map<net.Socket, number>();
  /** Sockets whose write buffer is full: held until they drain. */
  readonly #blocked = new Set<net.Socket>();
  /** The socket each TLS socket runs over (the connection, or TLS to an https:// proxy). */
  readonly #under = new WeakMap<net.Socket, net.Socket>();
  /** The socket whose bytes the core is being given (their download_data came from it). */
  #reading: net.Socket | undefined;

  readonly #p: StreamsPlatform;
  /** The file system downloads use (the journal's, with the tap on). */
  readonly #ops: FileOps;

  constructor(
    deliver: Deliver,
    transport: Transport,
    stateFile: string | undefined,
    ops: FileOps = REAL,
    platform = NODE_STREAMS,
  ) {
    this.#deliver = deliver;
    this.#transport = transport;
    this.#stateFile = stateFile;
    this.#p = platform;
    this.#ops = ops;
  }

  /** Carry out `e` if it is one of Rust mode's; false if it isn't. */
  do(e: Effect): boolean {
    const k = e.kind;
    switch (k.case) {
      case "proxyQuery": {
        const p = this.#transport.proxyFor(k.value.url);
        const headers = Object.entries(p?.headers ?? {}).map(([name, value]) => ({ name, value }));
        const resolveLocally = p?.resolveLocally ?? false;
        this.#deliver(input({ case: "proxyAnswer", value: { id: k.value.id, proxy: p?.url ?? "", headers, resolveLocally } }));
        return true;
      }
      case "streamResolve": {
        const { id, host } = k.value;
        const asked = {};
        this.#resolving.set(id, asked);
        this.#p.lookup(host, { all: true }, (err, addresses) => {
          // The core closed the stream meanwhile (or opened it, or shut down): the answer is
          // nobody's, and nothing more is said about a closed stream.
          if (this.#resolving.get(id) !== asked) return;
          this.#resolving.delete(id);
          if (err) this.#deliver(failed(id, err.message));
          else this.#deliver(input({ case: "streamResolved", value: { id, addresses: addresses.map((a) => a.address) } }));
        });
        return true;
      }
      case "streamOpen": {
        const { id, host, port } = k.value;
        // A name still being resolved for it is no longer waited for.
        this.#resolving.delete(id);
        const s = this.#p.connect({ host, port });
        s.setNoDelay(true);
        this.#watch(id, s, "connect", () => this.#deliver(input({ case: "streamOpened", value: { id } })));
        return true;
      }
      case "streamStartTls": {
        const { id, host } = k.value;
        const raw = this.#sockets.get(id);
        if (!raw) return true;
        // From now on the bytes are TLS's, not the core's.
        raw.removeAllListeners("data").removeAllListeners("end").removeAllListeners("error");
        this.#holds.delete(raw);
        this.#blocked.delete(raw);
        raw.on("error", () => {});
        const s = this.#p.tlsConnect({
          socket: raw,
          host,
          servername: net.isIP(host) ? undefined : host,
          secureContext: this.#transport.secureContext,
        });
        // The handshake is the stream's again (a proxy's answer to CONNECT let the socket go).
        this.#under.set(s, raw);
        this.#keepsRunning(s, true);
        this.#watch(id, s, "secureConnect", () => this.#deliver(input({ case: "streamTlsReady", value: { id } })));
        return true;
      }
      case "streamWrite": {
        const { id, data } = k.value;
        const s = this.#sockets.get(id);
        if (!s) return true;
        if (s.writableLength > MAX_UNSENT) {
          // The peer has stopped reading: give up on it rather than buffer without end.
          const detail = `the peer has not taken the last ${s.writableLength} bytes written to it`;
          this.#forget(id, s);
          s.destroy();
          this.#deliver(failed(id, detail));
          return true;
        }
        if (!s.write(data) && !this.#blocked.has(s)) {
          // Read nothing more from it until it has drained.
          this.#blocked.add(s);
          this.#hold(s);
          s.once("drain", () => {
            if (this.#blocked.delete(s)) this.#release(s);
          });
        }
        return true;
      }
      case "streamClose": {
        const { id, abort } = k.value;
        // Closed while its name was being resolved: that answer is dropped when it comes.
        this.#resolving.delete(id);
        const s = this.#sockets.get(id);
        if (!s) return true;
        this.#forget(id, s);
        if (abort || s.connecting) {
          // Given up on (or never connected: nothing written has anywhere to go).
          s.destroy();
        } else {
          s.end();
          // Cleared once the socket has closed: the timer holds the socket, and through its
          // listeners the client and its core, which a client started next would otherwise
          // keep company for LINGER_MS each (found by test/churn.test.ts).
          const linger = setTimeout(() => s.destroy(), LINGER_MS).unref();
          this.#lingering.add(s);
          s.once("close", () => {
            clearTimeout(linger);
            this.#lingering.delete(s);
          });
        }
        return true;
      }
      case "streamSplice": {
        const { id, download, bytes } = k.value;
        this.#splices.set(id, { download, left: Number(bytes) });
        return true;
      }
      case "downloadData":
        this.#append(k.value.id, k.value.data, this.#reading);
        return true;
      case "downloadDone":
        this.#finish(k.value.id);
        return true;
      // A download's file the core is done with, or gave up on (cancel: its stream is closed
      // already; a save's cancel is the Executor's too).
      case "discard":
      case "cancel": {
        const f = this.#files.get(k.value.id);
        if (f) {
          // Deleted once the write in flight is over.
          f.gone = true;
          this.#pump(f);
        } else if (this.#stateFile !== undefined) {
          // Complete (or never begun: then there is nothing to delete).
          this.#track(this.#ops.remove(downloadPath(this.#stateFile, k.value.id)).then(() => {}, () => {}));
        }
        return true;
      }
      default:
        return false;
    }
  }

  /** Report what happens on `s` as stream `id`'s, once `ready` fires. */
  #watch(id: bigint, s: net.Socket, ready: string, onReady: () => void): void {
    this.#sockets.set(id, s);
    const mine = () => this.#sockets.get(id) === s;
    s.once(ready, () => {
      if (mine()) onReady();
    });
    s.on("data", (chunk: Buffer) => {
      if (!mine()) return;
      // The peer has answered: from here it is a connection kept open.
      this.#keepsRunning(s, false);
      // Body bytes: straight to the file, past the core. Hearing a splice done, the core may ask
      // for another at once: the bytes that follow in this chunk are the next ones read, so they
      // are that splice's, not the core's.
      for (let splice = this.#splices.get(id); splice; splice = this.#splices.get(id)) {
        const n = Math.min(splice.left, chunk.length);
        this.#append(splice.download, chunk.subarray(0, n), s);
        splice.left -= n;
        chunk = chunk.subarray(n);
        if (splice.left === 0) {
          this.#splices.delete(id);
          this.#from(s, input({ case: "streamSpliced", value: { id } }));
        }
        // (Hearing the splice done, the shell may have closed the stream: nothing more of it.)
        if (chunk.length === 0 || !mine()) return;
      }
      this.#from(s, input({ case: "streamData", value: { id, data: new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.length) } }));
    });
    s.on("end", () => {
      if (mine()) this.#deliver(input({ case: "streamEof", value: { id } }));
    });
    s.on("error", (err: Error) => {
      if (mine()) {
        this.#forget(id, s);
        s.destroy();
        this.#deliver(failed(id, err.message));
      }
    });
    s.on("close", () => {
      // Gone without an error or an end (reset under TLS, say): the core must hear it.
      if (mine()) {
        this.#forget(id, s);
        this.#deliver(failed(id, "the connection closed"));
      }
    });
  }

  /** Whether `s` keeps Node.js running. A stream does while it is being opened and until the
   * peer's first bytes arrive on it; after that it is a connection kept open, which, like the
   * client's timers, doesn't hold a program whose own work is done (see Config: Node mode lets go
   * of an open WebSocket and of an idle connection the same way). The sockets under a TLS socket
   * follow it. */
  #keepsRunning(s: net.Socket, on: boolean): void {
    for (let x: net.Socket | undefined = s; x; x = this.#under.get(x)) {
      if (on) x.ref();
      else x.unref();
    }
  }

  /** Stream `id` (socket `s`) is no longer ours to report on. */
  #forget(id: bigint, s: net.Socket): void {
    this.#sockets.delete(id);
    this.#splices.delete(id);
    this.#holds.delete(s);
    this.#blocked.delete(s);
  }

  /** Deliver `i`, what `s` read: the download_data the core makes of it came from `s`. */
  #from(s: net.Socket, i: Input): void {
    this.#reading = s;
    try {
      this.#deliver(i);
    } finally {
      this.#reading = undefined;
    }
  }

  /** Stop reading from `s` (for one more reason). */
  #hold(s: net.Socket): void {
    const n = this.#holds.get(s) ?? 0;
    this.#holds.set(s, n + 1);
    if (n === 0) s.pause();
  }

  /** One reason fewer to keep `s` paused: read again if it was the last. */
  #release(s: net.Socket): void {
    const n = this.#holds.get(s);
    if (n === undefined) return;
    if (n > 1) return void this.#holds.set(s, n - 1);
    this.#holds.delete(s);
    s.resume();
  }

  /** Queue `data` for the end of download `id`'s file (made at the first); `from`: the socket it
   * came from, paused while too much waits. */
  #append(id: bigint, data: Uint8Array, from?: net.Socket): void {
    const f = this.#file(id);
    // After a failure (or a discard), the bytes go nowhere.
    if (f.error !== undefined || f.gone || data.length === 0) return;
    f.queue.push(data);
    f.unwritten += data.length;
    if (from && f.unwritten > HIGH_WATER && !f.paused.has(from)) {
      f.paused.add(from);
      this.#hold(from);
    }
    this.#pump(f);
  }

  /** Bytes waiting for files' writes, all of them (for tests). */
  get unwritten(): number {
    let n = 0;
    for (const f of this.#files.values()) n += f.unwritten;
    return n;
  }

  /** Resume the sockets `f` paused. */
  #unpause(f: File): void {
    for (const s of f.paused) this.#release(s);
    f.paused.clear();
  }

  #file(id: bigint): File {
    let f = this.#files.get(id);
    if (!f) {
      const path = downloadPath(this.#stateFile ?? "", id);
      f = {
        id,
        path,
        handle: undefined,
        made: false,
        queue: [],
        unwritten: 0,
        paused: new Set(),
        hash: createHash("sha256"),
        busy: false,
        done: false,
        gone: false,
        error: undefined,
      };
      this.#files.set(id, f);
    }
    return f;
  }

  /** The file is complete: once what is queued is written, sync it and tell the core its digest. */
  #finish(id: bigint): void {
    const f = this.#file(id);
    f.done = true;
    this.#pump(f);
  }

  #track(p: Promise<void>): void {
    this.#pumps.add(p);
    void p.finally(() => this.#pumps.delete(p));
    // A client started next on the state file waits for it.
    if (this.#stateFile !== undefined) track(this.#stateFile, p);
  }

  /** Carry `f` forward (unless that is under way): write what is queued; then, if it is done,
   * sync and answer; if it is gone or failed, delete it. */
  #pump(f: File): void {
    if (f.busy) return;
    f.busy = true;
    this.#track(
      this.#run(f).finally(() => {
        f.busy = false;
        // What came while it finished (a download_done or a discard the answer led to).
        if (this.#files.get(f.id) === f && (f.gone || f.done || (f.error === undefined && f.queue.length > 0))) {
          this.#pump(f);
        }
      }),
    );
  }

  async #run(f: File): Promise<void> {
    try {
      while (!f.gone && f.error === undefined && (f.queue.length > 0 || (f.done && !f.handle))) {
        if (!f.handle) {
          await this.#ops.mkdir(dirname(f.path));
          f.handle = await this.#ops.create(f.path);
          f.made = true;
          continue;
        }
        await this.#write(f, f.queue.splice(0, MAX_WRITEV));
      }
      if (f.gone || f.error !== undefined || !f.done) {
        if (f.gone || f.error !== undefined) await this.#remove(f);
        return;
      }
      // Complete: durable before the core hears of it.
      const handle = f.handle as OpenFile;
      await handle.sync();
      f.handle = undefined;
      await handle.close();
      if (f.gone) return void (await this.#remove(f));
      this.#files.delete(f.id);
      const sha256 = new Uint8Array(f.hash.digest());
      this.#tell(input({ case: "downloaded", value: { id: f.id, sha256 } }));
    } catch (err) {
      f.error = `writing ${f.path}: ${(err as Error).message}`;
      await this.#remove(f);
    }
  }

  /** Write `bufs` in full, hashing each byte once it is written. */
  async #write(f: File, bufs: Uint8Array[]): Promise<void> {
    const handle = f.handle as OpenFile;
    while (bufs.length > 0) {
      let n = await handle.write(bufs);
      // A short write: hash what went, and write the rest.
      while (bufs.length > 0) {
        const b = bufs[0] as Uint8Array;
        if (n >= b.length) {
          f.hash.update(b);
          f.unwritten -= b.length;
          n -= b.length;
          bufs.shift();
        } else {
          if (n > 0) {
            f.hash.update(b.subarray(0, n));
            f.unwritten -= n;
            bufs[0] = b.subarray(n);
          }
          break;
        }
      }
      if (f.unwritten <= LOW_WATER) this.#unpause(f);
    }
  }

  /** Close and delete a file that failed or is gone, then answer download_done if it came. */
  async #remove(f: File): Promise<void> {
    // Nothing more is written: the sockets it paused read on (into nothing, until they end).
    f.queue = [];
    f.unwritten = 0;
    this.#unpause(f);
    const handle = f.handle;
    f.handle = undefined;
    await handle?.close().catch(() => {});
    // Only a file it made is its to delete (one left there is someone else's).
    if (f.made) await this.#ops.remove(f.path).catch(() => {});
    if (f.gone) {
      if (this.#files.get(f.id) === f) this.#files.delete(f.id);
    } else if (f.done) {
      this.#files.delete(f.id);
      this.#tell(input({ case: "downloadFailed", value: { id: f.id, detail: f.error ?? "" } }));
    }
  }

  /** Report `i`, unless this has shut down. */
  #tell(i: Input): void {
    if (!this.#closed) this.#deliver(i);
  }

  /** Drop every stream, and every file not complete (once its write in flight is over, or
   * `boundMs` has passed: a write that hangs is let go, and its file deleted when it ends). */
  async shutdown(boundMs = Infinity): Promise<void> {
    this.#closed = true;
    for (const s of this.#sockets.values()) s.destroy();
    this.#sockets.clear();
    for (const s of this.#lingering) s.destroy();
    this.#lingering.clear();
    this.#resolving.clear();
    this.#splices.clear();
    this.#holds.clear();
    this.#blocked.clear();
    for (const f of this.#files.values()) {
      f.gone = true;
      this.#pump(f);
    }
    const files = async () => {
      while (this.#pumps.size > 0) await Promise.allSettled([...this.#pumps]);
    };
    await waitAtMost(files(), boundMs);
  }
}
