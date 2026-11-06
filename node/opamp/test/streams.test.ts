// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Core mode's sockets (src/streams.ts): backpressure both ways, what shutdown ends, and a name
// resolved for a stream closed meanwhile.

import assert from "node:assert/strict";
import net, { type AddressInfo } from "node:net";
import { test } from "node:test";

import { type Effect, EffectSchema, type Input } from "../src/gen/opamp/core/v1/core_pb.js";
import { create } from "../src/index.js";
import { Streams } from "../src/streams.js";
import { Transport } from "../src/transport.js";

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function until(what: string, done: () => boolean, ms = 20_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!done()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(5);
  }
}

const effect = (kind: { case: NonNullable<Effect["kind"]["case"]>; value: object }): Effect =>
  create(EffectSchema, { kind } as never);

/** A Streams and the inputs it reports. */
function streams(): { s: Streams; got: Input[] } {
  const got: Input[] = [];
  const s = new Streams((i) => got.push(i), new Transport({ proxy: false }));
  return { s, got };
}

const of = (got: Input[], id: bigint) =>
  got.filter((i) => (i.kind.value as { id?: bigint } | undefined)?.id === id).map((i) => i.kind.case);

/** A TCP server whose connections `serve` handles; its port. */
async function tcpServer(serve: (s: net.Socket) => void): Promise<{ port: number; close: () => void }> {
  const sockets = new Set<net.Socket>();
  const server = net.createServer((s) => {
    sockets.add(s);
    s.on("error", () => {});
    serve(s);
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  return {
    port: (server.address() as AddressInfo).port,
    close: () => {
      for (const s of sockets) s.destroy();
      server.close();
    },
  };
}

test("a peer that stops reading: we stop reading it until it drains, and past a bound the stream fails", async () => {
  let peer: net.Socket | undefined;
  let sent = 0;
  const server = await tcpServer((c) => {
    peer = c;
    // It reads nothing, and keeps sending (as pings would ask for answers).
    c.pause();
    const chunk = Buffer.alloc(16 << 10, 1);
    const more = () => {
      while (!c.destroyed) {
        sent += chunk.length;
        if (!c.write(chunk)) return void c.once("drain", more);
      }
    };
    more();
  });
  const { s, got } = streams();
  const read = () => got.reduce((n, i) => n + (i.kind.case === "streamData" ? i.kind.value.data.length : 0), 0);
  try {
    s.do(effect({ case: "streamOpen", value: { id: 1n, host: "127.0.0.1", port: server.port } }));
    await until("opened", () => got.some((i) => i.kind.case === "streamOpened"));
    await until("its bytes", () => read() > 1 << 20);
    // Writes it doesn't take: once the socket's buffer is full, we read nothing more.
    for (let i = 0; i < 8; i++) s.do(effect({ case: "streamWrite", value: { id: 1n, data: Buffer.alloc(1 << 20, 2) } }));
    await sleep(100);
    const before = read();
    const sentBefore = sent;
    await sleep(300);
    assert.ok(read() - before <= 64 << 10, `read ${read() - before} bytes while its writes were stuck`);
    // (It could still send: the kernel's buffers took some, until they were full too.)
    assert.ok(sent >= sentBefore);
    // It reads again: our writes drain, and we read again.
    peer?.resume();
    await until("reading again", () => read() > before + (1 << 20));
    // It stops again, and we write on: past the bound, the stream fails.
    peer?.pause();
    for (let i = 0; i < 64 && !got.some((i) => i.kind.case === "streamFailed"); i++) {
      s.do(effect({ case: "streamWrite", value: { id: 1n, data: Buffer.alloc(1 << 20, 3) } }));
    }
    const f = got.find((i) => i.kind.case === "streamFailed");
    assert.ok(f, "the stream never failed");
    assert.match((f.kind.value as { detail: string }).detail, /has not taken/);
    // Then nothing more about it.
    const n = got.length;
    s.do(effect({ case: "streamWrite", value: { id: 1n, data: Buffer.alloc(10) } }));
    await sleep(50);
    assert.equal(got.length, n);
  } finally {
    s.shutdown();
    server.close();
  }
});

test("shutdown ends a stream the core closed that is still lingering with what it wrote", async () => {
  let peer: net.Socket | undefined;
  let gone = false;
  let received = 0;
  const server = await tcpServer((c) => {
    peer = c;
    // It reads nothing: what we write stays to go, and a close lingers.
    c.pause();
    c.on("data", (b: Buffer) => {
      received += b.length;
    });
    c.on("close", () => {
      gone = true;
    });
  });
  const { s, got } = streams();
  try {
    s.do(effect({ case: "streamOpen", value: { id: 1n, host: "127.0.0.1", port: server.port } }));
    await until("opened", () => got.some((i) => i.kind.case === "streamOpened"));
    await until("accepted", () => peer !== undefined);
    for (let i = 0; i < 8; i++) s.do(effect({ case: "streamWrite", value: { id: 1n, data: Buffer.alloc(1 << 20, 2) } }));
    s.do(effect({ case: "streamClose", value: { id: 1n, abort: false } }));
    await sleep(50);
    assert.ok(!gone, "it lingers until shutdown");
    s.shutdown();
    // The peer reads again: the connection is gone, what we still had to send dropped (not
    // flushed through the linger).
    peer?.resume();
    await until("the peer sees it go", () => gone, 5_000);
    assert.ok(received < 8 << 20, `all ${received} bytes went after shutdown`);
  } finally {
    server.close();
  }
});

// covers: RULE-SHELL-06
test("a name still being resolved when its stream is closed is not answered", async () => {
  const { s, got } = streams();
  try {
    // Closed in the step that asked (as the adversary's cancel_stream_resolving does); the other isn't.
    s.do(effect({ case: "streamResolve", value: { id: 1n, host: "localhost" } }));
    s.do(effect({ case: "streamClose", value: { id: 1n, abort: false } }));
    s.do(effect({ case: "streamResolve", value: { id: 2n, host: "localhost" } }));
    await until("the other's answer", () => of(got, 2n).length > 0);
    await sleep(100);
    assert.deepEqual(of(got, 2n), ["streamResolved"]);
    assert.deepEqual(of(got, 1n), []);
  } finally {
    s.shutdown();
  }
});
