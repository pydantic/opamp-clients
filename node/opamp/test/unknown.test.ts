// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { test } from "node:test";

import { create } from "@bufbuild/protobuf";

import { Core } from "../src/core.js";
import { EffectSchema, OutputSchema } from "../src/gen/opamp/core/v1/core_pb.js";
import { Client, type Notification } from "../src/index.js";

test("an effect this client doesn't know stops it, and says why at once", async (t) => {
  // A newer core: its effect has a kind this client has no case for.
  t.mock.method(Core.prototype, "step", () => create(OutputSchema, { effects: [create(EffectSchema)] }));
  const notes: Notification[] = [];
  const client = Client.start({ endpoint: "http://127.0.0.1:1", instanceUid: new Uint8Array(16).fill(1), mode: "platform" }, {}, (n) =>
    notes.push(n),
  );
  // It failed while starting: the application hears once start has returned, without stop().
  assert.deepEqual(notes, []);
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(notes.length, 1);
  const n = notes[0] as Notification | undefined;
  assert.ok(n?.kind === "stopped" && /older than its core/.test(String(n.error)));
  await assert.rejects(client.stop(), /older than its core/);
});

test("a callback that throws stops the client, which says so once", async () => {
  const notes: Notification[] = [];
  const client = Client.start({ endpoint: "http://127.0.0.1:1", instanceUid: new Uint8Array(16).fill(1), mode: "platform" }, {}, (n) => {
    notes.push(n);
    throw new Error("the application's bug");
  });
  // The refused connection is told, the callback throws, and Stopped follows (thrown at too).
  const stopped = await new Promise<Notification>((resolve) => {
    const poll = setInterval(() => {
      const s = notes.find((n) => n.kind === "stopped");
      if (s) {
        clearInterval(poll);
        resolve(s);
      }
    }, 10);
  });
  assert.ok(stopped.kind === "stopped" && /the application's bug/.test(String(stopped.error)));
  assert.equal(notes.filter((n) => n.kind === "stopped").length, 1);
  await assert.rejects(client.stop(), /the application's bug/);
});

test("a callback that throws undefined stops the client, and says so (RULE-SHELL-02)", async () => {
  const notes: Notification[] = [];
  const client = Client.start({ endpoint: "http://127.0.0.1:1", instanceUid: new Uint8Array(16).fill(1), mode: "platform" }, {}, (n) => {
    notes.push(n);
    // JavaScript lets anyone throw anything, undefined included.
    throw undefined;
  });
  const stopped = await new Promise<Notification>((resolve) => {
    const poll = setInterval(() => {
      const s = notes.find((n) => n.kind === "stopped");
      if (s) {
        clearInterval(poll);
        resolve(s);
      }
    }, 10);
  });
  assert.ok(stopped.kind === "stopped" && /threw undefined/.test(String(stopped.error)));
  await assert.rejects(client.stop(), /threw undefined/);
});
