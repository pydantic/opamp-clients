// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Core mode: the core speaks HTTP/1.1, WebSocket, CONNECT and SOCKS5 itself,
// and this carries out only what needs the platform: TCP connections, TLS on them (Node's, with
// the operator's CAs, certificate and versions), name resolution (only for a SOCKS5 proxy with
// proxy.resolveLocally: otherwise the proxy resolves) and the proxy Node's convention picks for
// a URL. It decides nothing: every byte it writes, the core wrote, and the core keeps every time
// (it closes a stream it gives up on).
//
// Backpressure, core to socket: the core writes to a stream without hearing back: there is no
// "drained" input (none is needed: what the core writes is bounded by what it is asked to send,
// one request or message at a time, plus pongs and closing frames answering the peer). What the
// peer could make us buffer is answers to what it sends while it reads nothing (pings, say). So
// when a write finds the socket's buffer full (socket.write says false), reading from that socket
// stops until it drains: a peer that doesn't read hears nothing more to answer (and an HTTP
// server, which answers once it has read the request, loses nothing by it). And if more than
// MAX_UNSENT bytes are still waiting when the core writes again, the stream fails (stream_failed;
// the core reconnects as for any lost connection) rather than growing without end.

import { lookup } from "node:dns";
import net from "node:net";
import tls from "node:tls";

import { create } from "@bufbuild/protobuf";

import { type Effect, type Input, InputSchema } from "./gen/opamp/core/v1/core_pb.js";
import type { Transport } from "./transport.js";

type Deliver = (input: Input) => void;

/** What core mode does its network work with: Node's sockets and name resolution. A test
 * replaces them (TEST_PLATFORM, executor.ts) to choose when each one completes. */
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

/** Bytes written to a socket and not yet taken by the peer beyond which the next write fails the
 * stream. */
const MAX_UNSENT = 16 << 20;

export class Streams {
  readonly #deliver: Deliver;
  readonly #transport: Transport;
  /** Each stream's socket: the TCP one, or the TLS one on it once TLS is up. */
  readonly #sockets = new Map<bigint, net.Socket>();
  /** Sockets the core closed, flushing what was written (LINGER_MS at most): shutdown destroys them. */
  readonly #lingering = new Set<net.Socket>();
  /** The names being resolved, by stream: an answer is reported only while its own entry is here. */
  readonly #resolving = new Map<bigint, object>();
  /** Paused sockets: a full write buffer holds one. */
  readonly #holds = new Map<net.Socket, number>();
  /** Sockets whose write buffer is full: held until they drain. */
  readonly #blocked = new Set<net.Socket>();
  /** The socket each TLS socket runs over (the connection, or TLS to an https:// proxy). */
  readonly #under = new WeakMap<net.Socket, net.Socket>();
  readonly #p: StreamsPlatform;

  constructor(deliver: Deliver, transport: Transport, platform = NODE_STREAMS) {
    this.#deliver = deliver;
    this.#transport = transport;
    this.#p = platform;
  }

  /** Carry out `e` if it is one of core mode's; false if it isn't. */
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
        const { id, host, serverOrigin } = k.value;
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
          // The client certificate only at the server's own origin; the CAs everywhere.
          secureContext: this.#transport.secureContextFor(serverOrigin),
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
      this.#deliver(input({ case: "streamData", value: { id, data: new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.length) } }));
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
   * client's timers, doesn't hold a program whose own work is done (see Config: platform mode lets
   * go of an open WebSocket and of an idle connection the same way). The sockets under a TLS socket
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
    this.#holds.delete(s);
    this.#blocked.delete(s);
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

  /** Drop every stream. */
  shutdown(): void {
    for (const s of this.#sockets.values()) s.destroy();
    this.#sockets.clear();
    for (const s of this.#lingering) s.destroy();
    this.#lingering.clear();
    this.#resolving.clear();
    this.#holds.clear();
    this.#blocked.clear();
  }
}
