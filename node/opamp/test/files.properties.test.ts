// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// A disk that fails (fast-check): a client in either mode saves remote configs (and downloads a
// package) while one operation of one of its saves fails (EIO), writes half and then fails
// (ENOSPC), is slow (0.5 to 2 s), or lands and then reports failure (a rename, a directory sync);
// or its downloads' directory can't be made, or a discard can't delete its file. Whatever the
// fault:
// - the client survives: it never stops on its own, and keeps heartbeating while a save is slow;
// - every save gets exactly one answer, stored or store_failed (the tap says);
// - stop returns within its bound (the stop timeout plus the save in progress);
// - the state file is whole afterwards: a client started next starts from one of the versions
//   saved (or the one before), never a torn one;
// - a temporary file left behind (a failed or crashed save) never fails a later save;
// - the tap checker (opamp-tap, if there) passes both clients' taps.
//
// OPAMP_FAULT_RUNS runs (8 by default); the seed is printed, and OPAMP_FAULT_SEED (with
// OPAMP_FAULT_PATH) replays it.

import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { fromBinary } from "@bufbuild/protobuf";
import fc from "fast-check";

import { TEST_CORE_TAP } from "../src/core.js";
import { downloadsDir } from "../src/executor.js";
import { settled, TEST_FILE_OPS } from "../src/files.js";
import { ConfigSchema } from "../src/gen/opamp/core/v1/core_pb.js";
import { AgentCapabilities, Client, type Config, type Notification } from "../src/index.js";
import { checkTap, type Fault, faulty, type OpName, opampServer, readTap, sleep, storeAnswers, stores, until } from "./disk.js";

/** Where the fault goes: the `nth` time a save does `op`, or every time a download's directory is
 * made or a discard deletes a file. */
type Where = { on: "save"; op: OpName; nth: number; fault: Fault } | { on: "mkdir" } | { on: "discard" };

interface Case {
  mode: "platform" | "core";
  pkg: boolean;
  where: Where;
}

const SAVE_OPS: OpName[] = ["remove", "create", "write", "sync", "close", "rename", "syncDir"];

const faultFor = (op: OpName): fc.Arbitrary<Fault> =>
  fc.oneof(
    fc.constant<Fault>({ kind: "error" }),
    fc.constant<Fault>(op === "write" ? { kind: "half" } : { kind: "error", code: "ENOSPC" }),
    fc.integer({ min: 500, max: 2000 }).map((ms): Fault => ({ kind: "slow", ms })),
    fc.constant<Fault>(op === "rename" || op === "syncDir" ? { kind: "lands" } : { kind: "error" }),
  );

const where: fc.Arbitrary<Where> = fc.oneof(
  { weight: 6, arbitrary: fc
    .tuple(fc.constantFrom(...SAVE_OPS), fc.integer({ min: 1, max: 3 }))
    .chain(([op, nth]) => faultFor(op).map((fault): Where => ({ on: "save", op, nth, fault }))) },
  { weight: 1, arbitrary: fc.constant<Where>({ on: "mkdir" }) },
  { weight: 1, arbitrary: fc.constant<Where>({ on: "discard" }) },
);

const cases: fc.Arbitrary<Case> = fc.record({
  mode: fc.constantFrom<"platform" | "core">("platform", "core"),
  pkg: fc.boolean(),
  where,
}).map((c) => (c.where.on === "save" ? c : { ...c, pkg: true }));

const hex = (b: Uint8Array | undefined) => (b === undefined ? "none" : Buffer.from(b).toString("hex"));
/** stop's bound: the stop timeout (5 s), the slowest save (2 s), and a margin. */
const STOP_BOUND_MS = 5000 + 2000 + 2500;

async function run(c: Case, log: (m: string) => void): Promise<void> {
  const d = mkdtempSync(join(tmpdir(), "opamp-faults-"));
  const state = join(d, "state.bin");
  const tmp = `${state}.tmp`;
  const tapA = join(d, "a.tap");
  const tapB = join(d, "b.tap");
  const seen = new Map<OpName, number>();
  const disk = faulty((op, path) => {
    const w = c.where;
    if (w.on === "mkdir") return op === "mkdir" && path === downloadsDir(state) ? { kind: "error", code: "EACCES" } : undefined;
    if (w.on === "discard") return op === "remove" && path.startsWith(downloadsDir(state)) ? { kind: "error" } : undefined;
    const ofSave = op === "syncDir" || path === tmp;
    if (!ofSave || op !== w.op) return undefined;
    const n = (seen.get(op) ?? 0) + 1;
    seen.set(op, n);
    return n === w.nth ? w.fault : undefined;
  });
  const server = await opampServer({ configs: 3, every: 5, ...(c.pkg ? { pkg: Buffer.from("the package ".repeat(1000)) } : {}) });
  const errors: unknown[] = [];
  const base = {
    stateFile: state,
    capabilities: BigInt(AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig),
    heartbeatIntervalMs: 50,
    requestTimeoutMs: 3000,
    acceptPackages: c.pkg,
    mode: c.mode,
    proxy: false,
  };
  let a: Client | undefined;
  const notify = (n: Notification) => {
    if (n.kind === "stopped" && n.error !== undefined) errors.push(n.error);
    if (n.kind === "installPackage") a?.packageInstalled(n.packageName, n.package.hash);
  };
  a = Client.start({ ...base, endpoint: server.url, [TEST_FILE_OPS]: disk.ops, [TEST_CORE_TAP]: tapA } as Config, {}, notify);
  try {
    // The configs are in (the last at the 15th message), every save asked for is answered, and the
    // package is done with. (A save fault may not strike: the core asks for fewer saves when one is
    // slow, taking in what comes meanwhile at once.)
    const progress = () => {
      const recs = readTap(tapA);
      const done = !c.pkg || c.where.on === "mkdir" || disk.calls.some((k) => k.op === "remove" && k.path.startsWith(downloadsDir(state)));
      return { msgs: server.msgs.length, asked: stores(recs).length, answered: storeAnswers(recs).length, done };
    };
    const ready = () => {
      const p = progress();
      return p.msgs >= 20 && p.asked >= 2 && p.answered >= p.asked && p.done;
    };
    await until("the saves answered", ready, 15_000).catch((e: Error) => {
      throw new Error(`${e.message}: ${JSON.stringify(progress())}`);
    });
    // Slow: the client went on meanwhile.
    for (const k of disk.calls.filter((k) => k.fault?.kind === "slow")) {
      const during = server.msgs.filter((m) => m.at > k.at && m.at < k.at + k.ms).length;
      assert.ok(during >= 2, `${during} messages during a ${Math.round(k.ms)} ms ${k.op}: the client waited for it`);
    }
  } finally {
    const t0 = performance.now();
    await a.stop();
    const took = performance.now() - t0;
    server.close();
    assert.ok(took < STOP_BOUND_MS, `stop took ${Math.round(took)} ms`);
  }
  assert.deepEqual(errors, [], "the client stopped on its own");
  await settled(state);
  assert.deepEqual([...disk.open].map((f) => f.path), [], "files left open");
  const recs = readTap(tapA);
  const asked = stores(recs);
  const answers = storeAnswers(recs);
  for (const s of asked) {
    const n = answers.filter((x) => x.id === s.id).length;
    assert.ok(n <= 1, `save ${s.id} answered ${n} times`);
  }
  for (const x of answers) assert.ok(asked.some((s) => s.id === x.id), `an answer to save ${x.id}, never asked`);
  const struck = disk.calls.filter((k) => k.fault).map((k) => `${k.fault!.kind} ${k.op}`);
  log(`${asked.length} saves, ${answers.filter((x) => !x.ok).length} failed; ${struck.join(", ") || "the fault didn't strike"}`);
  const checkA = checkTap(tapA);
  if (checkA) assert.equal(checkA.code, 0, `opamp-tap ${tapA}:\n${checkA.out}`);

  // Whole: one of the saves' data, or nothing (the version before the first).
  await settled(state);
  const saved = existsSync(state) ? new Uint8Array(readFileSync(state)) : undefined;
  assert.ok(saved === undefined || asked.some((s) => hex(s.data) === hex(saved)), `the state file holds no whole save: ${hex(saved)}`);

  // A crashed save's temporary file, left behind: the next save makes its own.
  writeFileSync(tmp, "LEFT BY A CRASH", { mode: 0o644 });
  const next = await opampServer({ configs: 1, first: 50 });
  const b = Client.start({ ...base, acceptPackages: false, endpoint: next.url, [TEST_CORE_TAP]: tapB } as Config, {});
  try {
    await until("the next client's save", () => storeAnswers(readTap(tapB)).length > 0, 15_000);
  } finally {
    await b.stop();
    next.close();
  }
  const recsB = readTap(tapB);
  const init = fromBinary(ConfigSchema, recsB[0]!.bytes);
  assert.equal(hex(init.savedState.length > 0 ? init.savedState : undefined), hex(saved), "the next client starts from what was saved");
  const answersB = storeAnswers(recsB);
  assert.ok(answersB.every((x) => x.ok), "a save after a temporary file left behind failed");
  const checkB = checkTap(tapB);
  if (checkB) assert.equal(checkB.code, 0, `opamp-tap ${tapB}:\n${checkB.out}`);
}

// covers: RULE-SHELL-05, RULE-SHELL-07, RULE-SHELL-09
test("faults of the disk: the client survives, answers every save once, and the state file stays whole", async (t) => {
  const seed = process.env.OPAMP_FAULT_SEED ? Number(process.env.OPAMP_FAULT_SEED) : (Date.now() ^ (Math.random() * 2 ** 31)) | 0;
  const numRuns = Number(process.env.OPAMP_FAULT_RUNS ?? 8);
  t.diagnostic(`OPAMP_FAULT_SEED=${seed} (OPAMP_FAULT_RUNS=${numRuns})`);
  const property = fc.asyncProperty(cases, async (c) => {
    t.diagnostic(JSON.stringify(c));
    await run(c, (m) => t.diagnostic(`  ${m}`));
    await sleep(0);
  });
  const path = process.env.OPAMP_FAULT_PATH;
  const out = await fc.check(property, { numRuns, seed, ...(path ? { path } : {}), endOnFailure: true });
  if (out.failed) {
    assert.fail(
      `${fc.defaultReportMessage(out) ?? "failed"}\n${String((out.errorInstance as Error)?.stack ?? out.errorInstance)}\nreplay: OPAMP_FAULT_SEED=${seed} OPAMP_FAULT_PATH=${out.counterexamplePath} npm test`,
    );
  }
});
