// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The core's Setup call, through the module: off without an endpoint; the endpoint, the headers
// and the heartbeat as it reads them; the instance UID saved, or made and to be saved.

import assert from "node:assert/strict";
import { test } from "node:test";

import {
  CoreError,
  create,
  identifySetup,
  SetupIdentitySchema,
  SetupStartSchema,
  SetupUidFrom,
  SetupVariableSchema,
  startSetup,
} from "../src/index.js";

test("Setup is the core's call", () => {
  assert.equal(startSetup(create(SetupStartSchema)).off, true);
  const env = { OTEL_OPAMP_ENDPOINT: "https://user:pw@opamp.example.com/v1/opamp?k=v",
    OTEL_OPAMP_HEADERS: "Authorization=Bearer%20k", OTEL_OPAMP_HEARTBEAT_INTERVAL: "30s", OTEL_OPAMP_STATE_DIR: "none" };
  const s = startSetup(create(SetupStartSchema, {
    env: Object.entries(env).map(([name, value]) => create(SetupVariableSchema, { name, value })),
  }));
  assert.equal(s.off, false);
  assert.equal(s.shownEndpoint, "https://opamp.example.com/v1/opamp");
  assert.deepEqual(s.headers.map((h) => [h.name, h.value]), [["Authorization", "Bearer k"]]);
  assert.equal(s.heartbeatIntervalMs, 30_000n);
  assert.deepEqual([s.stateDir, s.defaultSlots], ["", []]);
  const saved = "0199e3a0-0000-7000-8000-000000000007";
  const id = identifySetup(create(SetupIdentitySchema, { stateDir: true, saved: `${saved}\n`, entropy: new Uint8Array(16) }));
  assert.deepEqual([id.from, id.save, id.text], [SetupUidFrom.SAVED, false, saved]);
  const made = identifySetup(create(SetupIdentitySchema, { stateDir: true, entropy: new Uint8Array(16), unixMs: 1n }));
  assert.deepEqual([made.from, made.save, made.instanceUid.length], [SetupUidFrom.MADE, true, 16]);
  assert.throws(() => identifySetup(create(SetupIdentitySchema)), CoreError);
});
