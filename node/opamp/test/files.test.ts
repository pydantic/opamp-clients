// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The client's file operations (src/files.ts), in both modes: journaled into the core tap as the
// tap checker expects (its file records), off the event loop, and never on a file left by another
// client or a crashed run (a download's file made new).

import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { TEST_CORE_TAP } from "../src/core.js";
import { Executor, downloadPath, downloadsDir } from "../src/executor.js";
import { REAL, TEST_FILE_OPS } from "../src/files.js";
import { EffectSchema, type Input } from "../src/gen/opamp/core/v1/core_pb.js";
import { AgentCapabilities, Client, type Config, create, type Notification } from "../src/index.js";
import { Streams } from "../src/streams.js";
import { Transport } from "../src/transport.js";
import {
  checkTap,
  faulty,
  fileOps,
  opampServer,
  readTap,
  sha256,
  sleep,
  storeAnswers,
  stores,
  TAP_CHECKER,
  until,
} from "./disk.js";

const MODES = ["platform", "core"] as const;
const dir = (name: string) => mkdtempSync(join(tmpdir(), `opamp-files-${name}-`));
const hex = (b: Uint8Array) => Buffer.from(b).toString("hex");

/** The tap checker passes `tap` (or, not built, says it was skipped). */
function checked(t: { diagnostic(m: string): void }, tap: string): void {
  const r = checkTap(tap);
  if (r === undefined) return void t.diagnostic(`${TAP_CHECKER} isn't built: the tap isn't checked (scripts/fetch_core.py fetch puts it there)`);
  assert.equal(r.code, 0, `opamp-tap ${tap}:\n${r.out}`);
}

for (const mode of MODES) {
  // covers: RULE-SHELL-09
  test(`${mode} mode: saves and a download's file are journaled in the tap, stored after the directory sync`, async (t) => {
    const file = Buffer.from("the package ".repeat(5000));
    const server = await opampServer({ configs: 2, pkg: file });
    const d = dir("journal");
    const state = join(d, "state.bin");
    const tap = join(d, "core.tap");
    const notes: Notification[] = [];
    let client: Client | undefined;
    client = Client.start(
      {
        endpoint: server.url,
        stateFile: state,
        acceptPackages: true,
        capabilities: BigInt(AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig),
        heartbeatIntervalMs: 50,
        mode,
        proxy: false,
        [TEST_CORE_TAP]: tap,
      } as Config,
      {},
      (n) => {
        notes.push(n);
        if (n.kind === "installPackage") client?.packageInstalled(n.packageName, n.package.hash);
      },
    );
    try {
      await until("the package installed and its file discarded, and the configs saved", () => {
        const recs = readTap(tap);
        const discarded = fileOps(recs).some((o) => o.op === "remove" && o.path?.startsWith("state.bin.downloads/"));
        return discarded && stores(recs).length >= 3 && storeAnswers(recs).length === stores(recs).length;
      });
    } finally {
      await client.stop();
      server.close();
    }
    const recs = readTap(tap);
    const ops = fileOps(recs);
    assert.deepEqual(ops[0], { at: ops[0]!.at, op: "state", path: "state.bin" }, "the state file is named first");
    assert.deepEqual(recs.slice(0, ops[0]!.at).map((r) => r.kind), [1, 2], "right after init");
    assert.deepEqual(ops[1] && { op: ops[1].op, path: ops[1].path }, { op: "clear", path: "state.bin.downloads" });

    // Each save: [remove], create, write (its data), sync, rename, syncdir; and stored after it.
    const saves = ops.filter((o) => o.path === "state.bin.tmp" || o.op === "rename" || o.op === "syncdir");
    const answers = storeAnswers(recs);
    let i = 0;
    for (const s of stores(recs)) {
      if (saves[i]?.op === "remove") i++;
      const [create, ...rest] = saves.slice(i, i + 5);
      assert.ok(create && create.at > s.at, "the save begins after the core asks for it");
      assert.deepEqual(
        [create, ...rest].map((o) => o?.op),
        ["create", "write", "sync", "rename", "syncdir"],
        `save ${s.id}: ${JSON.stringify(saves.slice(i, i + 5))}`,
      );
      assert.equal(rest[0]!.data, hex(s.data), `save ${s.id} writes its data`);
      assert.deepEqual([rest[2]!.from, rest[2]!.to, rest[3]!.path], ["state.bin.tmp", "state.bin", "."]);
      const answer = answers.find((a) => a.id === s.id);
      assert.ok(answer?.ok, `save ${s.id} stored`);
      assert.ok(answer.at > rest[3]!.at, `save ${s.id}: stored after the directory sync`);
      i += 5;
    }

    // The download's file: its directory made, the file made new, written, synced, then deleted.
    const dl = ops.filter((o) => o.path?.startsWith("state.bin.downloads"));
    const name = dl.find((o) => o.op === "create")?.path;
    assert.ok(name, JSON.stringify(dl));
    const of = dl.filter((o) => o.path === name || o.op === "mkdir");
    assert.deepEqual(of[0], { at: of[0]!.at, op: "mkdir", path: "state.bin.downloads" });
    assert.equal(of[1]!.op, "create");
    const writes = of.filter((o) => o.op === "write");
    assert.equal(writes.reduce((n, o) => n + (o.len ?? 0), 0), file.length, "every byte written");
    assert.deepEqual(of.slice(-2).map((o) => o.op), ["sync", "remove"]);
    checked(t, tap);
  });

  // A download given up on at stop never touches the file of the download of the same id a client
  // started next on the state file makes.
  // covers: RULE-SHELL-07
  test(`${mode} mode: a download held at stop doesn't touch the next client's file of the same id`, async () => {
    const file = Buffer.from("the package, version 1");
    const server = await opampServer({ pkg: file, hold: (n) => n === 0 });
    const state = join(dir("s3"), "state.bin");
    const config = { endpoint: server.url, stateFile: state, acceptPackages: true, heartbeatIntervalMs: 60_000, mode, proxy: false } as Config;
    try {
      const a = Client.start(config, {});
      await until("the first download", () => server.gets >= 1);
      await a.stop();
      const paths: string[] = [];
      const b = Client.start(config, {}, (n) => {
        if (n.kind === "installPackage") paths.push(n.path);
      });
      try {
        await until("installPackage", () => paths.length > 0);
        server.release(); // the first client's download, if it is still there, goes on now
        await sleep(300);
        assert.equal(sha256(readFileSync(paths[0]!)), sha256(file), "the file handed over is the package");
      } finally {
        await b.stop();
      }
    } finally {
      server.close();
    }
  });

  // A file left at a download's path (by a crashed run) is cleared at start: the download is
  // written to a file made new, and the tap says so.
  test(`${mode} mode: a file left in the downloads is never written over or handed over`, async (t) => {
    const file = Buffer.from("the package, fresh");
    const server = await opampServer({ pkg: file });
    const d = dir("left");
    const state = join(d, "state.bin");
    const tap = join(d, "core.tap");
    mkdirSync(downloadsDir(state), { recursive: true });
    for (let id = 0n; id < 4n; id++) writeFileSync(downloadPath(state, id), "PARTIAL");
    const paths: string[] = [];
    const client = Client.start(
      { endpoint: server.url, stateFile: state, acceptPackages: true, heartbeatIntervalMs: 60_000, mode, proxy: false, [TEST_CORE_TAP]: tap } as Config,
      {},
      (n) => {
        if (n.kind === "installPackage") paths.push(n.path);
      },
    );
    try {
      await until("installPackage", () => paths.length > 0);
      assert.equal(sha256(readFileSync(paths[0]!)), sha256(file));
    } finally {
      await client.stop();
      server.close();
    }
    assert.ok(!fileOps(readTap(tap)).some((o) => o.op === "open"), "no file written in place");
    checked(t, tap);
  });
}

// A download's file is made new: one already at its path (another client's, or a crashed run's)
// fails the download, untouched.
test("a download never writes over a file already at its path (platform mode)", async () => {
  const file = Buffer.from("the package");
  const server = await opampServer({ pkg: file });
  const state = join(dir("exec"), "state.bin");
  mkdirSync(downloadsDir(state), { recursive: true });
  writeFileSync(downloadPath(state, 1n), "SOMEONE ELSE'S");
  const got: Input[] = [];
  const x = new Executor((i) => got.push(i), state, new Transport({ proxy: false }));
  try {
    const url = server.url.replace("/v1/opamp", "/file");
    x.do(create(EffectSchema, { kind: { case: "download", value: { id: 1n, url, maxBytes: 1n << 20n, timeoutMs: 5000n } } }));
    await until("an answer", () => got.length > 0);
    assert.equal(got[0]!.kind.case, "downloadFailed", JSON.stringify(got[0]!.kind.value, (_, v) => (typeof v === "bigint" ? `${v}` : v)));
    assert.equal(readFileSync(downloadPath(state, 1n), "utf8"), "SOMEONE ELSE'S");
  } finally {
    x.shutdown();
    server.close();
  }
});

test("a download never writes over a file already at its path (core mode)", async () => {
  const state = join(dir("streams"), "state.bin");
  mkdirSync(downloadsDir(state), { recursive: true });
  writeFileSync(downloadPath(state, 2n), "SOMEONE ELSE'S");
  const got: Input[] = [];
  const s = new Streams((i) => got.push(i), new Transport({ proxy: false }), state);
  s.do(create(EffectSchema, { kind: { case: "downloadData", value: { id: 2n, data: Buffer.from("new") } } }));
  s.do(create(EffectSchema, { kind: { case: "downloadDone", value: { id: 2n } } }));
  await until("an answer", () => got.length > 0);
  await s.shutdown();
  assert.equal(got[0]!.kind.case, "downloadFailed");
  assert.equal(readFileSync(downloadPath(state, 2n), "utf8"), "SOMEONE ELSE'S");
});

// RULE-SHELL-07: a download whose file is still being flushed when the core says stopped is the
// shell's to delete (the core gave up on it), once the flush is over; and by the time stop returns,
// however much of the stop timeout the core took (STOP_GRACE_MS at least, after it).
// covers: RULE-SHELL-07
for (const mode of MODES) {
  test(`${mode} mode: a download's file being flushed as the core stops is gone when stop returns`, async () => {
    const pkg = Buffer.from("the package ".repeat(10_000));
    const server = await opampServer({ pkg });
    const state = join(dir("flushed"), "state.bin");
    const downloads = downloadsDir(state);
    const flushMs = 600; // longer than the stop timeout, well within the grace after it
    let flushing = false;
    let flushed = false;
    const ops = {
      ...REAL,
      async create(path: string) {
        const f = await REAL.create(path);
        if (!path.startsWith(downloads)) return f;
        return {
          path: f.path,
          write: (bufs: Uint8Array[]) => f.write(bufs),
          async sync() {
            flushing = true;
            await sleep(flushMs);
            await f.sync();
            flushed = true;
          },
          close: () => f.close(),
        };
      },
    };
    const listing = () => (existsSync(downloads) ? readdirSync(downloads).sort() : []);
    const installed: string[] = [];
    const config = { endpoint: server.url, stateFile: state, acceptPackages: true, heartbeatIntervalMs: 60_000,
      stopTimeoutMs: 100, mode, proxy: false, [TEST_FILE_OPS]: ops } as Config;
    try {
      const c = Client.start(config, {}, (n) => {
        if (n.kind === "installPackage") installed.push(n.path);
      });
      await until("the download's file being flushed", () => flushing);
      const t0 = performance.now();
      await c.stop();
      const took = performance.now() - t0;
      const atReturn = listing();
      await sleep(flushMs + 500);
      assert.ok(flushed, "the flush ended");
      assert.deepEqual(atReturn, [], `stop returned with ${atReturn} still there (deleted later: ${listing().length === 0})`);
      assert.deepEqual(listing(), []);
      assert.ok(took < 100 + 1000 + 500, `stop took ${took} ms`);
      assert.deepEqual(installed, []);
    } finally {
      server.close();
    }
  });
}

// Saves are off the event loop. A slow save holds up neither heartbeats nor stop, and a
// client started next on the state file waits for it, and starts from what it saved.
// covers: RULE-SHELL-07, RULE-SHELL-09
for (const mode of MODES) {
  test(`${mode} mode: a slow save holds up neither heartbeats nor stop; the next client waits for it`, async () => {
    const server = await opampServer({});
    const state = join(dir("s4"), "state.bin");
    let gate: () => void = () => {};
    const released = new Promise<void>((r) => (gate = r));
    const disk = faulty(() => undefined, {
      ...REAL,
      async syncDir(d) {
        await released;
        return REAL.syncDir(d);
      },
    });
    // No instance UID: the core makes one up and saves it at once.
    const config = { endpoint: server.url, stateFile: state, heartbeatIntervalMs: 50, mode, proxy: false } as Config;
    const a = Client.start({ ...config, [TEST_FILE_OPS]: disk.ops } as Config, {});
    try {
      await until("a save", () => disk.calls.some((c) => c.op === "syncDir"));
      const began = performance.now();
      await until("heartbeats while the save is writing", () => server.msgs.filter((m) => m.at > began).length >= 3, 3000);
      const t0 = performance.now();
      await a.stop();
      // The core waits for the save, up to its stop timeout (5 s), then gives up on it: stop
      // returns, and the save goes on, unreported.
      assert.ok(performance.now() - t0 < 6500, `stop took ${performance.now() - t0} ms`);

      const b = Client.start(config, {});
      const before = server.msgs.length;
      await sleep(300);
      assert.equal(server.msgs.length, before, "the next client started while a save was still writing");
      gate();
      await until("the next client's first message", () => server.msgs.length > before);
      await b.stop();
      const uid = hex(server.msgs[0]!.msg.instanceUid);
      assert.equal(hex(server.msgs[before]!.msg.instanceUid), uid, "the next client starts from what was saved");
    } finally {
      gate();
      server.close();
    }
  });
}

// stop() doesn't wait for a save already writing; settled() does, and then the state file holds
// what was saved: what an application reads there next is what a client started next starts from.
test("settled: once the save stop left writing has landed, and the state file holds it", async () => {
  const server = await opampServer({});
  const state = join(dir("settled"), "state.bin");
  let gate: () => void = () => {};
  const released = new Promise<void>((r) => (gate = r));
  const disk = faulty(() => undefined, {
    ...REAL,
    async rename(from, to) {
      await released;
      return REAL.rename(from, to);
    },
  });
  // No instance UID: the core makes one up and saves it at once.
  const config = { endpoint: server.url, stateFile: state, heartbeatIntervalMs: 50, stopTimeoutMs: 200, mode: "platform",
    proxy: false } as Config;
  const a = Client.start({ ...config, [TEST_FILE_OPS]: disk.ops } as Config, {});
  try {
    await until("a save", () => disk.calls.some((c) => c.op === "rename"));
    await a.stop();
    assert.equal(existsSync(state), false, "the save landed before it was let go");
    let done = false;
    const settled = a.settled().then(() => void (done = true));
    await sleep(300);
    assert.equal(done, false, "settled while the save was still writing");
    gate();
    await settled;
    assert.ok(readFileSync(state).length > 0, "settled before the save landed");
    // The next client starts from it: the same instance UID.
    const before = server.msgs.length;
    const b = Client.start(config, {});
    await until("the next client's first message", () => server.msgs.length > before);
    await b.stop();
    await b.settled();
    assert.equal(hex(server.msgs[before]!.msg.instanceUid), hex(server.msgs[0]!.msg.instanceUid));
  } finally {
    gate();
    server.close();
  }
  // Without a state file there is nothing to wait for.
  const c = Client.start({ endpoint: "http://127.0.0.1:1/", mode: "platform", proxy: false } as Config, {});
  await c.settled();
  await c.stop();
});

test("no state file: nothing journaled, a store fails", async () => {
  const got: Input[] = [];
  new Executor((i) => got.push(i)).do(create(EffectSchema, { kind: { case: "store", value: { id: 3n, data: new Uint8Array([1]) } } }));
  await Promise.resolve();
  assert.deepEqual(got.map((i) => i.kind.case), ["storeFailed"]);
});

// Saves land one at a time, in the order the core asked for them, each answered once.
// covers: RULE-SHELL-09
test("saves land one at a time, in order; a cancelled save not begun is never written", async () => {
  const state = join(dir("order"), "state.bin");
  const disk = faulty((op) => (op === "sync" ? { kind: "slow", ms: 30 } : undefined));
  const got: Input[] = [];
  const x = new Executor((i) => got.push(i), state, new Transport({ proxy: false }), disk.ops);
  const store = (id: bigint) => create(EffectSchema, { kind: { case: "store", value: { id, data: Buffer.from(`save ${id}`) } } });
  for (const id of [1n, 2n, 3n, 4n]) x.do(store(id));
  // The core gives up on 3 before it begins (core mode, at its stop timeout), and on 1 while it
  // is writing: 1 ends whole, unreported; 3 is never written.
  x.do(create(EffectSchema, { kind: { case: "cancel", value: { id: 3n } } }));
  await until("save 1 writing", () => disk.calls.some((c) => c.op === "create"));
  x.do(create(EffectSchema, { kind: { case: "cancel", value: { id: 1n } } }));
  await x.settled();
  assert.deepEqual(got.map((i) => [i.kind.case, (i.kind.value as { id: bigint }).id]), [["stored", 2n], ["stored", 4n]]);
  assert.equal(readFileSync(state, "utf8"), "save 4");
  const creates = disk.calls.filter((c) => c.op === "create");
  const dirs = disk.calls.filter((c) => c.op === "syncDir");
  assert.equal(creates.length, 3, "three saves written");
  for (let i = 1; i < creates.length; i++) {
    assert.ok(creates[i]!.at >= dirs[i - 1]!.at + dirs[i - 1]!.ms, "a save begins once the one before is over");
  }
});
