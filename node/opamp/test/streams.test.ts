// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Rust mode's sockets and files (src/streams.ts): download files written off the event loop, in
// order, hashed over exactly what was written, answered once (or not at all after a discard or a
// cancel); and backpressure both ways. File writes are slowed here by wrapping FileHandle's
// writev, the one call streams.ts writes with.

import assert from "node:assert/strict";
import { createHash, randomBytes } from "node:crypto";
import { existsSync, mkdtempSync, readdirSync, readFileSync } from "node:fs";
import { type FileHandle, open } from "node:fs/promises";
import http from "node:http";
import net, { type AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { type Effect, EffectSchema, type Input } from "../src/gen/opamp/core/v1/core_pb.js";
import { downloadPath, downloadsDir } from "../src/executor.js";
import { Client, create, type Notification, PackageType, ServerToAgentSchema, toBinary } from "../src/index.js";
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
const sha256 = (...parts: Uint8Array[]): string => {
  const h = createHash("sha256");
  for (const p of parts) h.update(p);
  return h.digest("hex");
};
const hex = (b: Uint8Array): string => Buffer.from(b).toString("hex");

type Writev = FileHandle["writev"];
let fileHandleProto: { writev: Writev } | undefined;

/** Run `body` with every FileHandle.writev going through `wrap`. */
async function withWritev<T>(wrap: (real: Writev) => Writev, body: () => Promise<T>): Promise<T> {
  if (!fileHandleProto) {
    const dir = mkdtempSync(join(tmpdir(), "opamp-fh-"));
    const h = await open(join(dir, "x"), "w");
    fileHandleProto = Object.getPrototypeOf(h) as { writev: Writev };
    await h.close();
  }
  const proto = fileHandleProto;
  const real = proto.writev;
  proto.writev = wrap(real);
  try {
    return await body();
  } finally {
    proto.writev = real;
  }
}

/** Every writev `ms` late. */
const slow = (ms: number) => (real: Writev) =>
  async function (this: FileHandle, ...args: Parameters<Writev>) {
    await sleep(ms);
    return real.apply(this, args);
  } as Writev;

/** A Streams, the inputs it reports, and a state file beside its downloads. */
function streams(): { s: Streams; got: Input[]; state: string } {
  const got: Input[] = [];
  const state = join(mkdtempSync(join(tmpdir(), "opamp-streams-")), "state");
  const s = new Streams((i) => got.push(i), new Transport({ proxy: false }), state);
  return { s, got, state };
}

const of = (got: Input[], id: bigint) =>
  got.filter((i) => (i.kind.value as { id?: bigint } | undefined)?.id === id).map((i) => i.kind.case);

test("a file is written in order, synced, and hashed over exactly what was written", async () => {
  await withWritev(slow(3), async () => {
    const { s, got, state } = streams();
    const parts = Array.from({ length: 40 }, (_, i) => randomBytes(1 + ((i * 7919) % 20_000)));
    for (const data of parts) s.do(effect({ case: "downloadData", value: { id: 5n, data } }));
    s.do(effect({ case: "downloadDone", value: { id: 5n } }));
    // Nothing is said before the writes are done.
    assert.equal(got.length, 0);
    await until("downloaded", () => got.length > 0);
    const done = got[0];
    assert.equal(done?.kind.case, "downloaded");
    const want = sha256(...parts);
    assert.equal(hex((done?.kind.value as { sha256: Uint8Array }).sha256), want);
    assert.equal(sha256(readFileSync(downloadPath(state, 5n))), want);
    await s.shutdown();
    assert.deepEqual(of(got, 5n), ["downloaded"]);
  });
});

test("an empty file is a file", async () => {
  const { s, got, state } = streams();
  s.do(effect({ case: "downloadDone", value: { id: 1n } }));
  await until("downloaded", () => got.length > 0);
  assert.equal(got[0]?.kind.case, "downloaded");
  assert.equal(readFileSync(downloadPath(state, 1n)).length, 0);
  await s.shutdown();
});

test("a discard or a cancel while a write is pending: no file, and nothing more said", async () => {
  await withWritev(slow(100), async () => {
    for (const how of ["discard", "cancel"] as const) {
      for (const done of [false, true]) {
        const { s, got, state } = streams();
        for (let i = 0; i < 5; i++) s.do(effect({ case: "downloadData", value: { id: 9n, data: randomBytes(50_000) } }));
        if (done) s.do(effect({ case: "downloadDone", value: { id: 9n } }));
        // The file is made, and its first write (100 ms) is in flight: waited for, not timed.
        await until("the file made", () => existsSync(downloadPath(state, 9n)));
        s.do(effect({ case: how, value: { id: 9n } }));
        // Deleted once that write is over: waited for, so a busy machine's late timers don't
        // count against it; then nothing more comes.
        await until(`${how} (done ${done}) deleting the file`, () => !existsSync(downloadPath(state, 9n)));
        await sleep(150);
        assert.deepEqual(got, [], `${how} (done ${done})`);
        assert.equal(existsSync(downloadPath(state, 9n)), false, `${how} (done ${done}) left a file`);
        await s.shutdown();
        assert.deepEqual(got, []);
      }
    }
  });
});

test("a failed write: the file is deleted, and download_done is answered download_failed once", async () => {
  let calls = 0;
  const failing = (real: Writev) =>
    async function (this: FileHandle, ...args: Parameters<Writev>) {
      if (++calls === 2) throw new Error("ENOSPC: no space left on device");
      return real.apply(this, args);
    } as Writev;
  await withWritev(failing, async () => {
    const { s, got, state } = streams();
    s.do(effect({ case: "downloadData", value: { id: 3n, data: randomBytes(1000) } }));
    await until("the first write", () => calls >= 1);
    await sleep(10);
    s.do(effect({ case: "downloadData", value: { id: 3n, data: randomBytes(1000) } }));
    await until("the second write", () => calls >= 2);
    // More after the failure: dropped, not a new file.
    s.do(effect({ case: "downloadData", value: { id: 3n, data: randomBytes(1000) } }));
    await sleep(50);
    assert.equal(got.length, 0);
    assert.equal(existsSync(downloadPath(state, 3n)), false);
    s.do(effect({ case: "downloadDone", value: { id: 3n } }));
    await until("an answer", () => got.length > 0);
    await s.shutdown();
    assert.deepEqual(of(got, 3n), ["downloadFailed"]);
    assert.match((got[0]?.kind.value as { detail: string }).detail, /ENOSPC/);
    assert.equal(existsSync(downloadPath(state, 3n)), false);
  });
});

test("shutdown waits for the write in flight, deletes files not complete, and says nothing", async () => {
  await withWritev(slow(50), async () => {
    const { s, got, state } = streams();
    s.do(effect({ case: "downloadData", value: { id: 1n, data: randomBytes(10_000) } }));
    s.do(effect({ case: "downloadData", value: { id: 2n, data: randomBytes(10_000) } }));
    s.do(effect({ case: "downloadDone", value: { id: 2n } }));
    await sleep(10);
    await s.shutdown();
    assert.deepEqual(got, []);
    const dir = downloadsDir(state);
    assert.deepEqual(existsSync(dir) ? readdirSync(dir) : [], []);
    // Effects after shutdown change nothing and report nothing.
    await sleep(100);
    assert.deepEqual(got, []);
  });
});

/** An OpAMP server (plain HTTP) offering one package, whose file it serves by `serve`. */
async function packageServer(file: Buffer, serve: (res: http.ServerResponse) => void) {
  const server = http.createServer((req, res) => {
    if (req.method === "GET") return serve(res);
    req.resume();
    req.on("end", () => {
      const { port } = server.address() as AddressInfo;
      const msg = create(ServerToAgentSchema, {
        packagesAvailable: {
          allPackagesHash: new Uint8Array([1]),
          packages: {
            pkg: {
              type: PackageType.PackageType_TopLevel,
              version: "1",
              hash: new Uint8Array([2]),
              file: {
                downloadUrl: `http://127.0.0.1:${port}/file`,
                contentHash: createHash("sha256").update(file).digest(),
              },
            },
          },
        },
      });
      res.writeHead(200, { "Content-Type": "application/x-protobuf" });
      res.end(Buffer.from(toBinary(ServerToAgentSchema, msg)));
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  return server;
}

/** Download the server's package in Rust mode: the installPackage notification's file. */
async function download(server: http.Server, splice: boolean): Promise<Buffer> {
  const state = join(mkdtempSync(join(tmpdir(), "opamp-rust-dl-")), "state");
  const notes: Notification[] = [];
  const client = Client.start(
    {
      endpoint: `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1/opamp`,
      stateFile: state,
      acceptPackages: true,
      heartbeatIntervalMs: 60_000,
      mode: "rust",
      rustSplice: splice,
      proxy: false,
    },
    {},
    (n) => notes.push(n),
  );
  try {
    await until("installPackage", () => notes.some((n) => n.kind === "installPackage" || (n.kind === "stopped" && n.error)), 60_000);
    const n = notes.find((n) => n.kind === "installPackage");
    assert.ok(n?.kind === "installPackage", JSON.stringify(notes.map((n) => n.kind)));
    return readFileSync(n.path);
  } finally {
    await client.stop();
  }
}

for (const framing of ["content-length", "chunked"] as const) {
  for (const splice of [true, false]) {
    test(`Rust mode: a download trickled while the file writes are slow (${framing}, splice ${splice})`, async () => {
      const file = randomBytes(3 << 20);
      const server = await packageServer(file, (res) => {
        res.writeHead(200, framing === "content-length" ? { "Content-Length": String(file.length) } : {});
        let at = 0;
        const more = () => {
          if (at >= file.length) return void res.end();
          const n = 1 + ((at * 31) % 70_000);
          const ok = res.write(file.subarray(at, at + n));
          at += n;
          if (ok) setImmediate(more);
          else res.once("drain", more);
        };
        more();
      });
      try {
        const got = await withWritev(slow(2), () => download(server, splice));
        assert.equal(got.length, file.length);
        assert.ok(got.equals(file));
      } finally {
        server.closeAllConnections();
        server.close();
      }
    });
  }
}

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

/** Write `data` to `s` as fast as it takes it, then end. */
function flood(s: net.Socket, data: Buffer): void {
  let at = 0;
  const more = () => {
    while (at < data.length) {
      const piece = data.subarray(at, at + (1 << 20));
      at += piece.length;
      if (!s.write(piece)) return void s.once("drain", more);
    }
    s.end();
  };
  more();
}

for (const splice of [true, false]) {
  test(`a socket faster than its file is paused, nothing reordered (${splice ? "spliced" : "download_data"})`, async () => {
    const file = randomBytes(40 << 20);
    // It sends once asked.
    const server = await tcpServer((s) => s.once("data", () => flood(s, file)));
    let peak = 0;
    const sampling = (real: Writev) =>
      async function (this: FileHandle, ...args: Parameters<Writev>) {
        peak = Math.max(peak, s.unwritten);
        await sleep(15);
        return real.apply(this, args);
      } as Writev;
    const got: Input[] = [];
    const state = join(mkdtempSync(join(tmpdir(), "opamp-streams-")), "state");
    // In place of the core: what comes unspliced is the download's, as download_data at once.
    const s: Streams = new Streams(
      (i) => {
        got.push(i);
        if (i.kind.case === "streamData") s.do(effect({ case: "downloadData", value: { id: 7n, data: i.kind.value.data } }));
        if (i.kind.case === "streamEof") s.do(effect({ case: "downloadDone", value: { id: 7n } }));
      },
      new Transport({ proxy: false }),
      state,
    );
    try {
      await withWritev(sampling, async () => {
        s.do(effect({ case: "streamOpen", value: { id: 1n, host: "127.0.0.1", port: server.port } }));
        await until("opened", () => got.some((i) => i.kind.case === "streamOpened"));
        if (splice) s.do(effect({ case: "streamSplice", value: { id: 1n, download: 7n, bytes: BigInt(file.length) } }));
        s.do(effect({ case: "streamWrite", value: { id: 1n, data: Buffer.from("GET\n") } }));
        await until("downloaded", () => got.some((i) => i.kind.case === "downloaded" || i.kind.case === "downloadFailed"), 60_000);
      });
      const done = got.find((i) => i.kind.case === "downloaded");
      assert.ok(done, JSON.stringify(got.map((i) => i.kind.case)));
      assert.equal(hex((done.kind.value as { sha256: Uint8Array }).sha256), sha256(file));
      assert.equal(got.some((i) => i.kind.case === "streamSpliced"), splice);
      // It was held near the mark (4 MiB, and a read or two past it), not the whole 40 MiB.
      assert.ok(peak > 3 << 20, `peak ${peak}: the writes were never behind`);
      assert.ok(peak < 6 << 20, `peak ${peak}: the socket was not paused`);
    } finally {
      await s.shutdown();
      server.close();
    }
  });
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
    await s.shutdown();
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
    await s.shutdown();
    // The peer reads again: the connection is gone, what we still had to send dropped (not
    // flushed through the linger).
    peer?.resume();
    await until("the peer sees it go", () => gone, 5_000);
    assert.ok(received < 8 << 20, `all ${received} bytes went after shutdown`);
  } finally {
    server.close();
  }
});

test("a stream the shell closes on hearing its splice is done says nothing more", async () => {
  // The spliced bytes and more, in one piece.
  const server = await tcpServer((c) => c.once("data", () => c.write(Buffer.alloc(1000, 7))));
  const got: Input[] = [];
  const state = join(mkdtempSync(join(tmpdir(), "opamp-streams-")), "state");
  const s: Streams = new Streams(
    (i) => {
      got.push(i);
      if (i.kind.case === "streamSpliced") s.do(effect({ case: "streamClose", value: { id: 1n, abort: true } }));
    },
    new Transport({ proxy: false }),
    state,
  );
  try {
    s.do(effect({ case: "streamOpen", value: { id: 1n, host: "127.0.0.1", port: server.port } }));
    await until("opened", () => got.some((i) => i.kind.case === "streamOpened"));
    s.do(effect({ case: "streamSplice", value: { id: 1n, download: 4n, bytes: 600n } }));
    s.do(effect({ case: "streamWrite", value: { id: 1n, data: Buffer.from("GET\n") } }));
    await until("spliced", () => got.some((i) => i.kind.case === "streamSpliced"));
    await sleep(50);
    assert.deepEqual(
      got.map((i) => i.kind.case),
      ["streamOpened", "streamSpliced"],
    );
    s.do(effect({ case: "downloadDone", value: { id: 4n } }));
    await until("downloaded", () => got.some((i) => i.kind.case === "downloaded"));
    assert.equal(readFileSync(downloadPath(state, 4n)).length, 600);
  } finally {
    await s.shutdown();
    server.close();
  }
});

// covers: RULE-SHELL-05
test("a splice asked for on hearing the one before is done takes the bytes that follow it", async () => {
  // Both splices' bytes and more, in one piece.
  const sent = randomBytes(1000);
  const server = await tcpServer((c) => c.once("data", () => c.write(sent)));
  const got: Input[] = [];
  const state = join(mkdtempSync(join(tmpdir(), "opamp-streams-")), "state");
  let asked = 0;
  const s: Streams = new Streams(
    (i) => {
      got.push(i);
      // The next bytes read are the file's again: the next one (as the adversary's stream_splice_chain asks).
      if (i.kind.case === "streamSpliced" && asked++ === 0) {
        s.do(effect({ case: "streamSplice", value: { id: 1n, download: 4n, bytes: 300n } }));
      }
    },
    new Transport({ proxy: false }),
    state,
  );
  try {
    s.do(effect({ case: "streamOpen", value: { id: 1n, host: "127.0.0.1", port: server.port } }));
    await until("opened", () => got.some((i) => i.kind.case === "streamOpened"));
    s.do(effect({ case: "streamSplice", value: { id: 1n, download: 4n, bytes: 600n } }));
    s.do(effect({ case: "streamWrite", value: { id: 1n, data: Buffer.from("GET\n") } }));
    await until("the bytes after both", () => got.some((i) => i.kind.case === "streamData"));
    // Only what follows the second splice is the core's.
    assert.deepEqual(
      got.map((i) => i.kind.case),
      ["streamOpened", "streamSpliced", "streamSpliced", "streamData"],
    );
    const data = got.find((i) => i.kind.case === "streamData")?.kind.value as { data: Uint8Array };
    assert.ok(Buffer.from(data.data).equals(sent.subarray(900)), `${data.data.length} bytes handed to the core`);
    s.do(effect({ case: "downloadDone", value: { id: 4n } }));
    await until("downloaded", () => got.some((i) => i.kind.case === "downloaded"));
    assert.ok(readFileSync(downloadPath(state, 4n)).equals(sent.subarray(0, 900)));
  } finally {
    await s.shutdown();
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
    await s.shutdown();
  }
});
