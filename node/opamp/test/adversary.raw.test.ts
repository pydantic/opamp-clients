// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The adversary's raw server (adversary.raw.ts) by itself: it does what its first line says, and
// tells what it saw of a connection. The
// same cases as Go's TestAdversaryRawServer.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import net, { type Socket } from "node:net";
import { test } from "node:test";
import tls from "node:tls";

import { pattern, rawMark, RawServer } from "./adversary.raw.js";

const fixture = (name: string): string => readFileSync(new URL(`../../test/fixtures/${name}`, import.meta.url), "utf8");
const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/** One side of a connection: what came, a line or all of it at a time. */
class Peer {
  #got = Buffer.alloc(0);
  /** "fin": the other side closed cleanly; "reset": it didn't. */
  #end: "fin" | "reset" | undefined;
  #wake: (() => void) | undefined;

  constructor(readonly s: net.Socket | tls.TLSSocket) {
    const wake = () => this.#wake?.();
    s.on("data", (chunk: Buffer) => ((this.#got = Buffer.concat([this.#got, chunk])), wake()));
    s.on("end", () => ((this.#end ??= "fin"), wake()));
    s.on("error", () => ((this.#end ??= "reset"), wake()));
    s.on("close", () => ((this.#end ??= "reset"), wake()));
  }

  async #until(done: () => boolean, ms: number): Promise<boolean> {
    // Over when the bound fires, not when the clock says: a timer can fire a little before
    // performance.now() reaches its time, and a wait that then went on had nothing left to end it.
    let over = false;
    const bound = setTimeout(() => ((over = true), this.#wake?.()), ms);
    try {
      while (!done()) {
        if (over) return false;
        await new Promise<void>((r) => (this.#wake = r));
      }
      return true;
    } finally {
      clearTimeout(bound);
    }
  }

  /** The next line, with its newline; what there is, if none comes in `ms`. */
  async line(ms = 20_000): Promise<string> {
    await this.#until(() => this.#got.includes(0x0a) || this.#end !== undefined, ms);
    const nl = this.#got.indexOf(0x0a);
    const n = nl < 0 ? this.#got.length : nl + 1;
    const line = this.#got.subarray(0, n).toString("latin1");
    this.#got = this.#got.subarray(n);
    return line;
  }

  /** The next `n` bytes (fewer if the connection ends first). */
  async bytes(n: number): Promise<Buffer> {
    await this.#until(() => this.#got.length >= n || this.#end !== undefined, 20_000);
    const out = this.#got.subarray(0, n);
    this.#got = this.#got.subarray(out.length);
    return out;
  }

  /** Everything up to the end, and how it ended. */
  async all(): Promise<{ body: Buffer; end: "fin" | "reset" | undefined }> {
    await this.#until(() => this.#end !== undefined, 20_000);
    return { body: this.#got, end: this.#end };
  }
}

// covers: RULE-SHELL-05, RULE-SHELL-06, RULE-SHELL-07
test("the raw server does what its first line says, and tells what it saw of a connection", async (t) => {
  const s = new RawServer({ cert: fixture("server.pem"), key: fixture("server-key.pem") });
  await s.listen();
  const sockets: Socket[] = [];
  t.after(() => {
    for (const c of sockets) c.destroy();
    s.close();
  });
  const connect = async (): Promise<Socket> => {
    const c = net.connect({ host: "127.0.0.1", port: s.port, allowHalfOpen: true });
    sockets.push(c);
    c.on("error", () => {});
    await new Promise<void>((r) => c.once("connect", r));
    return c;
  };
  /** A connection that said `line` and was answered ok. */
  const dial = async (line: string): Promise<Peer> => {
    const c = await connect();
    const peer = new Peer(c);
    c.write(`${line}\n`);
    assert.equal(await peer.line(), "ok\n", line);
    return peer;
  };
  const ask = async (tag: number): Promise<string> => (await dial(`opamp-raw tag=${1000 + tag} ask=${tag} wait=5000`)).line();

  // send, in pieces, then its side closed: the pattern, then the end.
  let peer = await dial("opamp-raw tag=1 send=1000 chunk=300 gap=1 end=fin");
  let all = await peer.all();
  assert.deepEqual([all.body.equals(pattern(1000)), all.end], [true, "fin"], "send=1000");

  // echo, and what it read: all of the pattern, then the peer's end.
  peer = await dial("opamp-raw tag=2 echo=1");
  peer.s.write(pattern(5000));
  assert.ok((await peer.bytes(5000)).equals(pattern(5000)), "echo");
  peer.s.end();
  assert.equal(await ask(2), "fin 5000 0\n", "after the pattern and a close");

  // Bytes that aren't the pattern's, and a reset.
  peer = await dial("opamp-raw tag=3");
  peer.s.write(Buffer.from([0, 1, 9]));
  await sleep(100);
  (peer.s as Socket).resetAndDestroy();
  assert.equal(await ask(3), "reset 3 1\n", "after three bytes, one wrong, and a reset");

  // One still open is said to be, once the wait is over; one never seen is none.
  await dial("opamp-raw tag=4");
  assert.equal(await (await dial("opamp-raw tag=1004 ask=4 wait=200")).line(), "open 0 0\n", "a connection still open");
  assert.equal(await ask(99), "none 0 0\n", "a connection never made");

  // A connection that never says who it is counts until it goes.
  const anon = await connect();
  assert.equal(await (await dial("opamp-raw tag=1005 ask=0 wait=300")).line(), "open 1 0\n", "with a connection that never said who it was");
  anon.destroy();
  assert.equal(await ask(0), "none 0 0\n", "once it has gone");
  // One that says something else is closed, and so is one whose line is too long.
  for (const said of ["GET / HTTP/1.1\r\n\r\n", `opamp-raw ${"x".repeat(300)}\n`]) {
    const other = new Peer(await connect());
    other.s.write(said);
    assert.equal((await other.all()).body.length, 0, `answered ${JSON.stringify(said.slice(0, 20))}`);
  }
  assert.equal(await ask(0), "none 0 0\n", "once those have been closed");

  // TLS after ok, under the certificate's name; then the pattern inside it, and a close.
  peer = await dial("opamp-raw tag=6 tls=1 send=100 end=close");
  peer.s.removeAllListeners("data");
  const secure = tls.connect({ socket: peer.s, servername: "localhost", ca: fixture("ca.pem") });
  all = await new Peer(secure).all();
  assert.deepEqual([all.body.equals(pattern(100)), all.end], [true, "fin"], "tls=1 send=100");
  // And what is read inside it is counted as it is outside.
  peer = await dial("opamp-raw tag=11 tls=1 echo=1");
  peer.s.removeAllListeners("data");
  const inside = new Peer(tls.connect({ socket: peer.s, servername: "localhost", ca: fixture("ca.pem") }));
  inside.s.write(pattern(3000));
  assert.ok((await inside.bytes(3000)).equals(pattern(3000)), "echo inside TLS");
  inside.s.end();
  assert.equal(await ask(11), "fin 3000 0\n", "after the pattern and a close, inside TLS");

  // A peer that goes instead of starting TLS is given up on at once.
  peer = await dial("opamp-raw tag=12 tls=1");
  peer.s.destroy();
  assert.equal(await ask(12), "closed 0 0\n", "gone before its TLS handshake");

  // read=0 reads nothing until the run is released; delay=-1 never answers; reset is one.
  peer = await dial("opamp-raw tag=7 read=0");
  peer.s.write(pattern(10));
  peer.s.end();
  assert.equal(await (await dial("opamp-raw tag=1007 ask=7 wait=200")).line(), "open 0 0\n", "read=0 before the release");
  s.release();
  assert.equal(await ask(7), "fin 10 0\n", "read=0 after the release");
  all = await (await dial("opamp-raw tag=8 end=reset after=50")).all();
  assert.equal(all.end, "reset", "end=reset ended the stream cleanly");
  const silent = new Peer(await connect());
  silent.s.write("opamp-raw tag=9 delay=-1\n");
  assert.equal(await silent.line(200), "", "delay=-1 answered");

  // The marker: a connection accepted after it is late, one before it isn't.
  assert.match(s.afterMark(50) ?? "", /never got/, "before the marker");
  assert.equal(await rawMark(s.port), undefined);
  assert.equal(s.afterMark(50), undefined, "nothing came after the marker");
  await sleep(100);
  await dial("opamp-raw tag=10");
  assert.match(s.afterMark(50) ?? "", /RULE-SHELL-07/, "a connection made after the marker passed");
  // What is still open is counted (the marker's connection is not the run's), until it has gone.
  const another = await connect();
  await sleep(50);
  assert.equal(s.open().anonymous, 1, "one that never said who it was");
  assert.ok(s.open().all > 1, "and those left open above");
  another.destroy();
  for (const c of sockets) c.destroy();
  for (let i = 0; i < 100 && s.open().all > 0; i++) await sleep(50);
  assert.deepEqual(s.open(), { all: 0, anonymous: 0 }, "once every connection has gone");
});

test("a line that never comes is given up on when its bound is over, whatever the clock says", async (t) => {
  const server = net.createServer((c) => c.on("error", () => {}));
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const c = net.connect({ host: "127.0.0.1", port: (server.address() as net.AddressInfo).port });
  c.on("error", () => {});
  t.after(() => (c.destroy(), server.close()));
  await new Promise<void>((r) => c.once("connect", r));
  // The clock stands still: a wait that went by it never ended (CI hung on it for 20 minutes).
  const now = performance.now();
  t.mock.method(performance, "now", () => now);
  let timer: NodeJS.Timeout | undefined;
  const hung = new Promise<string>((r) => (timer = setTimeout(() => r("hung"), 2000)));
  assert.equal(await Promise.race([new Peer(c).line(50), hung]), "");
  clearTimeout(timer);
});
