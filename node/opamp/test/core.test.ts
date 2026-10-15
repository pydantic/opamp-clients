// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

import assert from "node:assert/strict";
import { test } from "node:test";

import { create, fromBinary } from "@bufbuild/protobuf";

import { readFileSync } from "node:fs";

import { Core, CoreError, TEST_CORE_MODULE } from "../src/core.js";
import { ConfigSchema, type Input, InputSchema } from "../src/gen/opamp/core/v1/core_pb.js";
import { AgentToServerSchema } from "../src/gen/opamp/v1/opamp_pb.js";
import { Client } from "../src/index.js";

const input = (kind: Input["kind"]): Input => create(InputSchema, { kind } as never);
const start = () => input({ case: "start", value: {} as never });
const timer = () => input({ case: "timer", value: {} as never });

test("start sends the first report", () => {
  const uid = new Uint8Array(16).fill(7);
  const core = new Core(
    create(ConfigSchema, { instanceUid: uid, endpoint: "http://server/v1/opamp" }),
  );
  const out = core.step(0, start());
  assert.equal(out.effects.length, 1);
  const effect = out.effects[0]!.kind;
  assert.equal(effect.case, "httpRequest");
  if (effect.case !== "httpRequest") return;
  assert.equal(effect.value.url, "http://server/v1/opamp");
  const msg = fromBinary(AgentToServerSchema, effect.value.body);
  assert.deepEqual(msg.instanceUid, uid);
  assert.ok(msg.capabilities & 1n, "ReportsStatus");
  assert.ok(out.wakeAtMs !== undefined, "a wake-up for the next poll");

  // A 503 with Retry-After as a date, 30 s after the response came: nothing before then, and the
  // next request within the client's own spread after it (a quarter of the wait: 7.5 s).
  const came = Date.UTC(2026, 8, 26, 12, 0, 0);
  core.step(
    10,
    input({
      case: "httpResponse",
      value: {
        id: effect.value.id,
        status: 503,
        headers: [{ name: "Retry-After", value: new Date(came + 30_000).toUTCString() }],
        unixMs: BigInt(came),
      } as never,
    }),
  );
  const sends = (o: typeof out) => o.effects.some((e) => e.kind.case === "httpRequest");
  let last = out;
  for (const at of [30_000, 30_009]) {
    last = core.step(at, timer());
    assert.equal(sends(last), false, `at ${at} ms`);
  }
  let now = 30_009;
  for (let i = 0; i < 10 && !sends(last); i++) {
    const wake = Number(last.wakeAtMs);
    assert.ok(wake > now, `wake at ${wake} ms`);
    assert.equal(sends(core.step(wake - 1, timer())), false, `at ${wake - 1} ms`);
    last = core.step(wake, timer());
    now = wake;
  }
  assert.ok(sends(last) && now >= 30_010 && now <= 30_010 + 7_500, `sent at ${now} ms`);
});

test("a bad config is an error", () => {
  assert.throws(() => new Core(create(ConfigSchema, { instanceUid: new Uint8Array(2) })), CoreError);
});

test("cores are independent", () => {
  const a = new Core(create(ConfigSchema, { instanceUid: new Uint8Array(16).fill(1), endpoint: "http://server/v1/opamp" }));
  const b = new Core(create(ConfigSchema, { instanceUid: new Uint8Array(16).fill(2), endpoint: "http://server/v1/opamp" }));
  assert.equal(a.step(0, start()).instanceUid[0], 1);
  assert.equal(b.step(0, start()).instanceUid[0], 2);
});

test("a test can run one client on another module", async () => {
  // The embedded core's own bytes, given explicitly, work as the embedded core does.
  const bytes = readFileSync(new URL("../../core/opamp_core.wasm", import.meta.url));
  const core = new Core(create(ConfigSchema, { instanceUid: new Uint8Array(16).fill(3), endpoint: "http://server/v1/opamp" }), bytes);
  assert.equal(core.step(0, start()).instanceUid[0], 3);
  // A client given bytes that are no module fails to start: it used them.
  const config = { endpoint: "http://127.0.0.1:1", instanceUid: new Uint8Array(16).fill(4), mode: "node" as const };
  assert.throws(() => Client.start(Object.assign({ ...config }, { [TEST_CORE_MODULE]: new Uint8Array([0]) }), {}), WebAssembly.CompileError);
  // And no other client is affected.
  await Client.start(config, {}).stop();
});
