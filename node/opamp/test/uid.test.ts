// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";

import {
  type AgentToServer,
  AgentToServerSchema,
  Client,
  type Notification,
  ServerToAgentSchema,
} from "../src/index.js";

async function body(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return new Uint8Array(Buffer.concat(chunks));
}

// No instance UID given: the core makes one from the client's entropy and wall clock (RULE-CLIENT-19),
// a UUIDv7 of now, and every client its own.
// covers: RULE-CLIENT-19
test("without an instance UID the core makes one: a UUIDv7 of now", async () => {
  const got: AgentToServer[] = [];
  const server = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    got.push(msg);
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid })));
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const { port } = server.address() as AddressInfo;
  const uids: Uint8Array[] = [];
  try {
    for (let i = 0; i < 2; i++) {
      const before = got.length;
      const client = Client.start({ endpoint: `http://127.0.0.1:${port}/v1/opamp`, mode: "platform" }, {});
      while (got.length === before) await new Promise((r) => setTimeout(r, 10));
      uids.push(got[before]!.instanceUid);
      await client.stop();
    }
  } finally {
    server.close();
  }
  const [a, b] = uids;
  assert.notDeepEqual(a, b);
  for (const u of [a!, b!]) {
    assert.equal(u.length, 16);
    assert.equal((u[6] ?? 0) >> 4, 7);
    assert.equal((u[8] ?? 0) >> 6, 2);
    const ms = [...u.slice(0, 6)].reduce((n, c) => n * 256 + c, 0);
    assert.ok(Math.abs(Date.now() - ms) < 5_000);
  }
});

// requestInstanceUid reaches the core: the client asks until the server assigns a UID, then
// uses it; without the option it never asks.
// covers: OPAMP-UID-04
for (const on of [true, false]) {
  test(`requestInstanceUid ${on ? "asks the server for a UID" : "off: never asks"}`, async () => {
    const assigned = new Uint8Array(16).fill(0x77);
    const got: AgentToServer[] = [];
    const server = createServer(async (req, res) => {
      const msg = fromBinary(AgentToServerSchema, await body(req));
      got.push(msg);
      const asks = (msg.flags & 1n) !== 0n;
      res.setHeader("Content-Type", "application/x-protobuf");
      res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, {
        instanceUid: msg.instanceUid,
        agentIdentification: asks ? { newInstanceUid: assigned } : undefined,
      })));
    });
    await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
    const { port } = server.address() as AddressInfo;
    const notes: Notification[] = [];
    const client = Client.start(
      { endpoint: `http://127.0.0.1:${port}/v1/opamp`, heartbeatIntervalMs: 50, requestInstanceUid: on, mode: "platform" },
      {},
      (n) => notes.push(n),
    );
    while (got.length < 3) await new Promise((r) => setTimeout(r, 10));
    await client.stop();
    server.close();

    assert.equal((got[0]!.flags & 1n) !== 0n, on, "the first message asks for a UID");
    for (const m of got.slice(1)) assert.equal(m.flags & 1n, 0n, "later messages don't ask");
    if (on) {
      assert.deepEqual(got.at(-1)!.instanceUid, assigned, "the assigned UID is used");
      assert.ok(notes.some((n) => n.kind === "instanceUidChanged"), "the application hears of it");
    } else {
      assert.notDeepEqual(got.at(-1)!.instanceUid, assigned);
    }
  });
}
