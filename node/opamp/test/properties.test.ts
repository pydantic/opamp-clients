// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Properties of the client around the core, over random plans (fast-check): the application's
// calls, some made from inside the notification callback (so they reach the client while it is
// still carrying out the step that called back), others from the event loop, over either
// transport. Whatever the plan: the healths reach the server in order (the core may send only the
// latest of several), the server ends with the last health and the last description, sequence
// numbers run without a gap, and the goodbye is the last message.

import assert from "node:assert/strict";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";

import fc from "fast-check";
import { WebSocketServer } from "ws";

import {
  AgentDescriptionSchema,
  type AgentToServer,
  AgentToServerSchema,
  Client,
  create,
  ComponentHealthSchema,
  fromBinary,
  type Notification,
  RemoteConfigStatusSchema,
  ServerToAgentSchema,
  toBinary,
} from "../src/index.js";

/** The sequence numbers a run should have: 0 until the server has answered a message (those sent
 * before its first answer: RULE-CLIENT-20), then 1, 2, ... */
function expectedSequence(got: { sequenceNum: bigint }[]): bigint[] {
  const first = got.findIndex((m) => m.sequenceNum !== 0n);
  const zeros = first === -1 ? got.length : Math.max(1, first);
  return got.map((_, i) => (i < zeros ? 0n : BigInt(i - zeros + 1)));
}

const UID = new Uint8Array(16).fill(0x42);
const ACCEPTS_REMOTE_CONFIG = 2n;
const REPORTS_HEALTH = 0x800n;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

const description = (n: number) =>
  create(AgentDescriptionSchema, {
    nonIdentifyingAttributes: [{ key: "n", value: { value: { case: "intValue", value: BigInt(n) } } }],
  });

function described(m: AgentToServer): number | undefined {
  const v = m.agentDescription?.nonIdentifyingAttributes[0]?.value?.value;
  return v?.case === "intValue" ? Number(v.value) : undefined;
}

async function body(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return new Uint8Array(Buffer.concat(chunks));
}

type Step = ["describe" | "health", boolean];

async function run(plan: Step[], ws: boolean): Promise<void> {
  const got: AgentToServer[] = [];
  const reply = (msg: AgentToServer): Uint8Array => {
    got.push(msg);
    // Every answer carries a new remote config, so the application is called back often.
    const n = new TextEncoder().encode(String(got.length));
    const remoteConfig = { config: { configMap: { "": { body: n } } }, configHash: n };
    return toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid, remoteConfig }));
  };
  const server = createServer(async (req, res) => {
    const out = reply(fromBinary(AgentToServerSchema, await body(req)));
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(out);
  });
  const wss = new WebSocketServer({ server });
  wss.on("connection", (conn) => {
    conn.on("message", (data: Buffer) => {
      const out = reply(fromBinary(AgentToServerSchema, new Uint8Array(data.subarray(1))));
      conn.send(Buffer.concat([Buffer.from([0]), out]));
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const { port } = server.address() as AddressInfo;

  let done = 0; // the steps of the plan carried out
  let lastHealth = 0;
  let lastDescription = 0;
  const errors: unknown[] = [];
  let client: Client | undefined;

  const carryOut = () => {
    const [what] = plan[done]!;
    done += 1;
    if (what === "describe") {
      lastDescription = done;
      client!.setDescription(description(done));
    } else {
      lastHealth = done;
      client!.setHealth(create(ComponentHealthSchema, { healthy: true, status: String(done) }));
    }
  };
  const notify = (n: Notification) => {
    if (n.kind === "stopped" && n.error !== undefined) errors.push(n.error);
    try {
      // Answered, so the core hands over the next remote config (and calls back again).
      if (n.kind === "remoteConfig") {
        client?.setRemoteConfigStatus(create(RemoteConfigStatusSchema, { lastRemoteConfigHash: n.remoteConfig.configHash, status: 1 }));
      }
      // The steps due from the callback, called from inside it.
      while (client !== undefined && done < plan.length && plan[done]![1]) carryOut();
    } catch (e) {
      errors.push(e);
    }
  };

  client = Client.start(
    {
      endpoint: `${ws ? "ws" : "http"}://127.0.0.1:${port}/v1/opamp`,
      instanceUid: UID,
      capabilities: ACCEPTS_REMOTE_CONFIG | REPORTS_HEALTH,
      heartbeatIntervalMs: 20,
      mode: "platform",
    },
    { description: description(0), health: create(ComponentHealthSchema, { healthy: true, status: "0" }) },
    notify,
  );
  for (let i = 0; i < 2000 && done < plan.length; i++) {
    if (!plan[done]![1]) carryOut();
    await sleep(2);
  }
  assert.equal(done, plan.length, "the plan stalled");

  const healths = () => got.flatMap((m) => (m.health !== undefined ? [Number(m.health.status)] : []));
  const descriptions = () => got.map(described).filter((d) => d !== undefined);
  for (let i = 0; i < 1000; i++) {
    if (healths().at(-1) === lastHealth && descriptions().at(-1) === lastDescription) break;
    await sleep(5);
  }
  await client.stop();
  for (const c of wss.clients) c.terminate();
  wss.close();
  server.close();

  assert.deepEqual(errors, []);
  const h = healths();
  assert.ok(h.every((v, i) => i === 0 || v >= h[i - 1]!), `healths in order: ${h}`);
  assert.equal(h.at(-1), lastHealth, "the server has the last health");
  assert.equal(descriptions().at(-1), lastDescription, "the server has the last description");
  assert.deepEqual(
    got.map((m) => m.sequenceNum),
    expectedSequence(got),
    "sequence numbers 0 until the first answer, then without a gap",
  );
  assert.ok(got.at(-1)!.agentDisconnect !== undefined, "the goodbye is last");
}

test("calls from anywhere arrive once and in order", async () => {
  const step = fc.tuple(fc.constantFrom<"describe" | "health">("describe", "health"), fc.boolean());
  await fc.assert(
    fc.asyncProperty(fc.array(step, { minLength: 1, maxLength: 12 }), fc.boolean(), run),
    { numRuns: Number(process.env.FC_RUNS ?? 25) },
  );
});
