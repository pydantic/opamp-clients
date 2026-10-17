// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

import assert from "node:assert/strict";
import { chmodSync, existsSync, mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { create, fromBinary } from "@bufbuild/protobuf";

import { Core, TEST_CORE_TAP } from "../src/core.js";
import { ConfigSchema, type Input, InputSchema, ReplySchema, StepSchema } from "../src/gen/opamp/core/v1/core_pb.js";
import { Client, type Config } from "../src/index.js";
import { opampServer, sleep, until } from "./disk.js";

const input = (kind: Input["kind"]): Input => create(InputSchema, { kind } as never);

/** The records of a tap: kind and bytes. */
function records(path: string): [number, Uint8Array][] {
  const data = readFileSync(path);
  const out: [number, Uint8Array][] = [];
  for (let at = 0; at < data.length; ) {
    const len = data.readUInt32LE(at + 1);
    out.push([data[at]!, data.subarray(at + 13, at + 13 + len)]);
    at += 13 + len;
  }
  return out;
}

test("the core tap records every crossing, a file per core", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-tap-"));
  const path = join(dir, "core.tap");
  const config = create(ConfigSchema, { instanceUid: new Uint8Array(16).fill(7), endpoint: "http://server/v1/opamp" });
  // Unset: no file.
  delete process.env.OPAMP_CORE_TAP;
  new Core(config).close();
  assert.equal(existsSync(path), false);

  // A tap file already there, readable by others: it is narrowed to the user's only.
  writeFileSync(path, "");
  chmodSync(path, 0o644);
  process.env.OPAMP_CORE_TAP = path;
  try {
    const core = new Core(config);
    core.step(5, input({ case: "start", value: {} as never }));
    core.close();
    const got = records(path);
    assert.deepEqual(got.map(([k]) => k), [1, 2, 3, 4]);
    assert.equal(fromBinary(StepSchema, got[2]![1]).nowMs, 5n);
    if (process.platform !== "win32") assert.equal(statSync(path).mode & 0o777, 0o600, "the user's only");
    // A second core in this process writes a file of its own.
    new Core(config).close();
    assert.deepEqual(records(`${path}.${process.pid}.1`).map(([k]) => k), [1, 2]);
  } finally {
    delete process.env.OPAMP_CORE_TAP;
  }
});

// The tap ends with the client's stop: the stop input, then the reply that says stopped (with the
// stopped notification); nothing is fed after it. (In between, the core may still take in the
// response to a request in flight, or what its closing handshake reads.)
for (const mode of ["node", "rust"] as const) {
  for (const ws of [false, true]) {
    test(`${mode} mode${ws ? ", WebSocket" : ""}: the tap ends with stop and stopped`, async () => {
      const server = await opampServer({ configs: 1 });
      const dir = mkdtempSync(join(tmpdir(), "opamp-tap-stop-"));
      const tap = join(dir, "core.tap");
      const client = Client.start(
        {
          endpoint: ws ? server.url.replace(/^http/, "ws") : server.url,
          stateFile: join(dir, "state.bin"),
          heartbeatIntervalMs: 50,
          mode,
          proxy: false,
          [TEST_CORE_TAP]: tap,
        } as Config,
        {},
      );
      try {
        await until("a few messages", () => server.msgs.length >= 3);
      } finally {
        await client.stop();
        server.close();
      }
      await sleep(100);
      // (File records may follow: a save the core asked for before it stopped ends whole.)
      const recs = records(tap).filter(([k]) => k !== 5);
      const steps = recs.filter(([k]) => k === 3).map(([, b]) => fromBinary(StepSchema, b).input?.kind.case);
      const replies = recs.filter(([k]) => k === 4).map(([, b]) => fromBinary(ReplySchema, b).output);
      assert.equal(recs.at(-1)?.[0], 4, "a reply last");
      assert.ok(steps.includes("stop"), `no stop: ${steps.slice(-5).join(", ")}`);
      const last = replies.at(-1);
      assert.equal(last?.stopped, true, "the last reply says stopped");
      assert.ok(last?.effects.some((e) => e.kind.case === "notify" && e.kind.value.kind.case === "stopped"), "with the stopped notification");
      assert.equal(replies.filter((o) => o?.stopped).length, 1, "nothing fed after it");
    });
  }
}
