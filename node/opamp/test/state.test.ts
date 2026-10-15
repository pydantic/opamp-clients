// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { create } from "@bufbuild/protobuf";

import { EffectSchema, type Input } from "../src/gen/opamp/core/v1/core_pb.js";
import { Executor, saveState } from "../src/executor.js";
import { Client } from "../src/index.js";

const dir = () => mkdtempSync(join(tmpdir(), "opamp-state-"));

test("saves replace what was saved, privately", () => {
  const path = join(dir(), "state.bin");
  saveState(path, new Uint8Array([1]));
  saveState(path, new Uint8Array([2, 3]));
  assert.deepEqual([...readFileSync(path)], [2, 3]);
  assert.ok(!existsSync(`${path}.tmp`));
  if (process.platform !== "win32") assert.equal(statSync(path).mode & 0o777, 0o600);
});

test("a leftover temporary file is replaced, not reused", () => {
  const path = join(dir(), "state.bin");
  writeFileSync(`${path}.tmp`, "stale, readable by all", { mode: 0o644 });
  saveState(path, new Uint8Array([1]));
  assert.deepEqual([...readFileSync(path)], [1]);
  if (process.platform !== "win32") assert.equal(statSync(path).mode & 0o777, 0o600);
});

test("a store is answered", async () => {
  const got: Input[] = [];
  const store = (id: bigint) => create(EffectSchema, { kind: { case: "store", value: { id, data: new Uint8Array([9]) } } });
  // Off the event loop: answered once the save is over.
  for (const [id, path] of [[7n, join(dir(), "state.bin")], [8n, join(dir(), "missing", "state.bin")], [9n, undefined]] as const) {
    const x = new Executor((i) => got.push(i), path);
    x.do(store(id));
    await x.settled();
  }
  assert.deepEqual(
    got.map((i) => [i.kind.case, (i.kind.value as { id: bigint }).id]),
    [
      ["stored", 7n],
      ["storeFailed", 8n],
      ["storeFailed", 9n],
    ],
  );
});

test("an unreadable state file stops the client starting", () => {
  const instanceUid = new Uint8Array(16);
  assert.throws(() => Client.start({ endpoint: "http://127.0.0.1:1", instanceUid, stateFile: dir(), mode: "node" }, {}));
});
