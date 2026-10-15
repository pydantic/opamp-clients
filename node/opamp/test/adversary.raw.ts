// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The adversary's raw server: what the
// streams of a Rust mode run connect to. A stream is bytes, not HTTP, so the adversary tells the
// server what to do in the first line it writes:
//
//   opamp-raw tag=7 send=1000 chunk=100 gap=5 end=fin
//
// The server answers "ok\n", then does what the line says, and keeps for each connection that said
// who it was (tag) how many bytes it read after the line, whether each was the pattern's (byte i
// is i % 251), and how the connection ended; the adversary asks for that on another stream
// (ask=TAG). One server per run, on a port of its own, so every connection is that run's, those
// that never say who they are too. Go's adversary_raw_test.go is the reference: this does what it
// does. adversary.properties.test.ts gives every Rust mode run one; adversary.raw.test.ts tests it
// by itself.

import net, { type AddressInfo, type Socket } from "node:net";
import type { Duplex } from "node:stream";
import tls from "node:tls";

/** The longest first line the server reads. */
const LINE = 256;
/** How long a TLS handshake may take. */
const HANDSHAKE_MS = 30_000;
/** The marker is answered within this of being sent (a server that never answers fails the run). */
const MARK_BOUND_MS = 30_000;

/** How a connection ended. "": open; fin: the peer closed its side; reset: it failed; closed: the
 * server ended it. */
type End = "" | "fin" | "reset" | "closed";

/** What the server knows of one connection. */
interface Conn {
  socket: Socket;
  /** When it was accepted (performance.now). */
  accepted: number;
  /** Its first line came. */
  said: boolean;
  /** Bytes read after the line. */
  got: number;
  /** One of them wasn't the pattern's. */
  bad: boolean;
  end: End;
}

/** Byte i is i % 251. */
export function pattern(size: number): Buffer {
  const b = Buffer.allocUnsafe(size);
  for (let i = 0; i < size; i++) b[i] = i % 251;
  return b;
}

/**
 * Reads a stream only when asked, as a blocking read does: what nobody asks for stays in the
 * socket (and then in the kernel, and then at the peer), which is what read=0 and a write that
 * waits for the peer rely on.
 */
class Pull {
  readonly #s: Duplex;
  #ended = false;
  #failed = false;
  #wake: (() => void) | undefined;
  readonly #on: [string, () => void][];

  constructor(s: Duplex) {
    this.#s = s;
    const wake = () => this.#wake?.();
    this.#on = [
      ["readable", wake],
      ["end", () => ((this.#ended = true), wake())],
      ["error", () => ((this.#failed = true), wake())],
      ["close", () => ((this.#failed = true), wake())],
    ];
    for (const [name, f] of this.#on) s.on(name, f);
  }

  /** The next bytes; "fin" at the peer's end; "reset" if reading failed (or the stream was closed here). */
  async read(): Promise<Buffer | "fin" | "reset"> {
    for (;;) {
      const chunk = this.#s.read() as Buffer | null;
      if (chunk !== null) return chunk;
      if (this.#ended) return "fin";
      if (this.#failed || this.#s.destroyed) return "reset";
      await new Promise<void>((r) => (this.#wake = r));
      this.#wake = undefined;
    }
  }

  /** Give back bytes read too far: the next read returns them first. */
  unread(rest: Buffer): void {
    if (rest.length > 0) this.#s.unshift(rest);
  }

  /** Stop listening: the stream is someone else's to read (TLS's). */
  detach(): void {
    for (const [name, f] of this.#on) this.#s.off(name, f);
  }
}

/** Write `data` on `s`: true once it has gone (as far as the kernel), false if it can't. */
function write(s: Duplex, data: Uint8Array | string): Promise<boolean> {
  return new Promise((resolve) => {
    if (s.destroyed || s.writableEnded) return resolve(false);
    s.write(data, (err) => resolve(err == null));
  });
}

/** One run's raw server. */
export class RawServer {
  readonly #server: net.Server;
  /** What tls=1 starts TLS with (undefined: it can't). */
  readonly #tls: tls.SecureContext | undefined;
  /** Every connection, but the harness's marker. */
  readonly #conns = new Set<Conn>();
  /** Those that said who they were. */
  readonly #tags = new Map<number, Conn>();
  /** Who waits for a connection to say who it is, or to end. */
  readonly #waiting = new Set<() => void>();
  /** Sleeps under way: teardown ends them. */
  readonly #sleeps = new Map<NodeJS.Timeout, (ok: boolean) => void>();
  /** When the marker's connection was accepted; undefined: not yet. */
  #marked: number | undefined;
  #release!: () => void;
  /** Resolved once the run's stop has returned: read=0 connections read on. */
  readonly #released = new Promise<void>((r) => (this.#release = r));
  #quit = false;
  port = 0;

  /** `certificate`: the server's, for tls=1 (PEM). */
  constructor(certificate?: { cert: string | Buffer; key: string | Buffer }) {
    this.#tls = certificate && tls.createSecureContext(certificate);
    // Nothing is done for the server: it ends a connection when the line says, or at the peer's end.
    this.#server = net.createServer({ allowHalfOpen: true }, (socket) => {
      const c: Conn = { socket, accepted: performance.now(), said: false, got: 0, bad: false, end: "" };
      socket.on("error", () => {});
      void this.#serve(c).catch(() => this.#ended(c, "closed"));
    });
  }

  /** Listen on a port of its own. */
  async listen(): Promise<void> {
    await new Promise<void>((r) => this.#server.listen(0, "127.0.0.1", r));
    this.port = (this.#server.address() as AddressInfo).port;
  }

  /** Let the connections told not to read (read=0) read on: the run's stop has returned, and only
   * a read tells the server a connection has gone. */
  release(): void {
    this.#release();
  }

  /** End the server and every connection. */
  close(): void {
    this.release();
    this.#quit = true;
    this.#server.close();
    for (const [t, done] of this.#sleeps) {
      clearTimeout(t);
      done(false);
    }
    this.#sleeps.clear();
    for (const c of this.#conns) c.socket.destroy();
    this.#wake();
  }

  /** The connections still open, and how many of them never said who they were. */
  open(): { all: number; anonymous: number } {
    let all = 0;
    let anonymous = 0;
    for (const c of this.#conns) {
      if (c.end !== "") continue;
      all++;
      if (!c.said) anonymous++;
    }
    return { all, anonymous };
  }

  /**
   * What breaks RULE-SHELL-07 in when the run's connections were accepted (undefined: nothing
   * does): one accepted more than `graceMs` after the marker's, which the harness opened the
   * moment stop returned. (The server accepts in the order the connections were made.)
   */
  afterMark(graceMs: number): string | undefined {
    if (this.#marked === undefined) return "the raw server never got the harness's marker";
    for (const c of this.#conns) {
      const late = c.accepted - this.#marked;
      if (late > graceMs) {
        return `RULE-SHELL-07: a stream's connection reached the raw server ${Math.round(late)} ms after one the harness made as stop returned`;
      }
    }
    return undefined;
  }

  /** A connection said who it was, or ended: whoever waits for that looks again. */
  #wake(): void {
    const waiting = [...this.#waiting];
    this.#waiting.clear();
    for (const w of waiting) w();
  }

  #ended(c: Conn, how: End): void {
    if (c.end === "") c.end = how;
    this.#wake();
  }

  /** Wait `ms`; false if the server is torn down first. */
  #sleep(ms: number): Promise<boolean> {
    if (ms <= 0) return Promise.resolve(true);
    if (this.#quit) return Promise.resolve(false);
    return new Promise((resolve) => {
      const t = setTimeout(() => {
        this.#sleeps.delete(t);
        resolve(true);
      }, ms);
      this.#sleeps.set(t, resolve);
    });
  }

  async #serve(c: Conn): Promise<void> {
    const tcp = c.socket;
    // Its run's from the moment it is accepted, whether or not it ever says who it is.
    this.#conns.add(c);
    this.#wake();
    let pull = new Pull(tcp);
    const line = await firstLine(pull);
    const set = new Map<string, string>();
    if (line !== undefined) {
      for (const kv of line.slice("opamp-raw ".length).split(" ")) {
        const i = kv.indexOf("=");
        if (i >= 0) set.set(kv.slice(0, i), kv.slice(i + 1));
      }
    }
    const num = (name: string, def: number): number => {
      const v = set.get(name) ?? "";
      return /^[+-]?\d+$/.test(v) ? Number(v) : def;
    };
    if (line !== undefined && num("mark", 0) === 1) {
      // The harness's own, not the run's: when what was opened as stop returned got here.
      this.#conns.delete(c);
      this.#marked = c.accepted;
      this.#wake();
      tcp.end("ok\n", () => tcp.destroy());
      return;
    }
    if (line === undefined) {
      // It went, or said something else, before it said who it was.
      this.#ended(c, "closed");
      tcp.destroy();
      return;
    }
    const tag = num("tag", 0);
    c.said = true;
    this.#tags.set(tag, c);
    this.#wake();

    // What is written and read: the socket, or TLS on it.
    let rw: Duplex = tcp;
    const giveUp = () => {
      this.#ended(c, "closed");
      rw.destroy();
      tcp.destroy();
    };
    const delay = num("delay", 0);
    const held = delay < 0; // never answered: only read, until it goes
    if (!held) {
      if (!(await this.#sleep(delay)) || !(await write(tcp, "ok\n"))) return giveUp();
      if (num("tls", 0) === 1) {
        if (!this.#tls) return giveUp();
        // From here the socket's bytes are TLS's. (What came with the line is not: a peer starts
        // TLS in answer to the ok.)
        pull.detach();
        const secure = new tls.TLSSocket(tcp, { isServer: true, secureContext: this.#tls });
        secure.on("error", () => {});
        rw = secure;
        pull = new Pull(secure);
        const up = await new Promise<boolean>((resolve) => {
          const bound = setTimeout(() => done(false), HANDSHAKE_MS);
          // Until the handshake is over the only thing there is to read is the peer's end: looking
          // for it (a read of nothing) is what tells a peer that went from one still at it.
          const look = () => void secure.read(0);
          const done = (ok: boolean) => {
            clearTimeout(bound);
            secure.off("readable", look);
            resolve(ok);
          };
          secure.on("readable", look);
          secure.once("secure", () => done(true));
          secure.once("end", () => done(false));
          secure.once("error", () => done(false));
          secure.once("close", () => done(false));
        });
        if (!up) return giveUp();
      }
    }

    // What the line asks the server to send, then how it ends the stream, beside the reading.
    const sent = (async () => {
      if (held) return;
      if (set.has("ask")) {
        const about = /^\d+$/.test(set.get("ask") ?? "") ? Number(set.get("ask")) : 0;
        if (!(await write(rw, await this.#answer(about, num("wait", 0))))) return;
      }
      const body = pattern(Math.max(num("send", 0), 0));
      let chunk = num("chunk", 0);
      if (chunk <= 0) chunk = Math.max(body.length, 1);
      const gap = num("gap", 0);
      for (let off = 0; off < body.length; off += chunk) {
        if (off > 0 && !(await this.#sleep(gap))) return;
        if (!(await write(rw, body.subarray(off, Math.min(off + chunk, body.length))))) return;
      }
      const end = set.get("end") ?? "";
      if (end === "" || end === "hold") return;
      if (!(await this.#sleep(num("after", 0)))) return;
      switch (end) {
        case "fin":
          // Its side only: it still reads.
          rw.end();
          break;
        case "close":
          this.#ended(c, "closed");
          // What was sent goes first (and TLS's own close), then the connection.
          rw.end(() => {
            rw.destroy();
            tcp.destroy();
          });
          break;
        case "reset":
          this.#ended(c, "closed");
          tcp.resetAndDestroy();
          rw.destroy();
          break;
      }
    })();

    if (num("read", 1) === 0) {
      // Reads nothing until the run is over (teardown releases it too).
      await this.#released;
    }
    const echo = num("echo", 0) === 1;
    for (;;) {
      const r = await pull.read();
      if (typeof r === "string") {
        this.#ended(c, r);
        break;
      }
      for (let i = 0; i < r.length; i++) {
        if (r[i] !== (c.got + i) % 251) c.bad = true;
      }
      c.got += r.length;
      if (echo && !(await write(rw, r))) {
        this.#ended(c, "reset");
        break;
      }
    }
    rw.destroy();
    tcp.destroy();
    await sent;
  }

  /** Wait until connection `tag` has ended (tag 0: until no connection that never said who it was
   * is open), `wait` ms at most, and say what the server knows of it: "STATE GOT BAD\n". */
  async #answer(tag: number, wait: number): Promise<string> {
    let over = wait <= 0;
    let wake: (() => void) | undefined;
    const deadline = over
      ? undefined
      : setTimeout(() => {
          over = true;
          wake?.();
        }, wait);
    try {
      for (;;) {
        let state = "none";
        let got = 0;
        let bad = 0;
        if (tag === 0) {
          for (const c of this.#conns) {
            if (c.end === "" && !c.said) {
              state = "open";
              got++;
            }
          }
        } else {
          const c = this.#tags.get(tag);
          if (c) {
            state = c.end === "" ? "open" : c.end;
            got = c.got;
            bad = c.bad ? 1 : 0;
          }
        }
        const answer = `${state} ${got} ${bad}\n`;
        if (state !== "open" || over || this.#quit) return answer;
        await new Promise<void>((r) => {
          wake = r;
          this.#waiting.add(r);
        });
        if (wake) this.#waiting.delete(wake);
      }
    } finally {
      clearTimeout(deadline);
    }
  }
}

/** The first line, without its newline and only if it is the adversary's ("opamp-raw ..."): the
 * newline within LINE bytes. What came after it is given back to `pull`. */
async function firstLine(pull: Pull): Promise<string | undefined> {
  let seen = Buffer.alloc(0);
  for (;;) {
    const r = await pull.read();
    if (typeof r === "string") return undefined;
    seen = Buffer.concat([seen, r]);
    const nl = seen.subarray(0, LINE).indexOf(0x0a);
    if (nl >= 0) {
      pull.unread(seen.subarray(nl + 1));
      const line = seen.subarray(0, nl).toString("latin1");
      return line.startsWith("opamp-raw ") ? line : undefined;
    }
    if (seen.length >= LINE) return undefined;
  }
}

/**
 * Opens the harness's marker on the raw server at `port`, as stop returns: resolves once the
 * server has said ok (undefined), or with what went wrong.
 */
export function rawMark(port: number): Promise<string | undefined> {
  return new Promise((resolve) => {
    const s = net.connect({ host: "127.0.0.1", port });
    let said = "";
    const done = (problem: string | undefined) => {
      clearTimeout(bound);
      s.destroy();
      resolve(problem);
    };
    const bound = setTimeout(() => done(`no answer in ${MARK_BOUND_MS} ms`), MARK_BOUND_MS);
    s.on("error", (e) => done(String(e)));
    s.on("data", (chunk: Buffer) => {
      said += chunk.toString("latin1");
      if (said.includes("\n")) done(said === "ok\n" ? undefined : `the raw server answered the marker ${JSON.stringify(said)}`);
    });
    s.on("close", () => done(`the raw server closed the marker's connection after ${JSON.stringify(said)}`));
    s.write("opamp-raw mark=1\n");
  });
}
