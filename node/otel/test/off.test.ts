// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The off switch (tests/shell-tables/off.json at the repository root): every case through
// register(), in a real process given the case's environment. Off: no client starts and the
// reason is said once; on: the client starts and polls.

import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

import { create, ServerToAgentSchema, toBinary } from "@pydantic/opamp-client";

const INDEX = new URL("../src/index.js", import.meta.url).href;
const TABLES = fileURLToPath(new URL("../../../../tests/shell-tables/", import.meta.url));

interface Case {
  id: string;
  env: Record<string, string>;
  code_endpoint: string | null;
  off: boolean;
  reason: string;
}
const CASES = (JSON.parse(readFileSync(TABLES + "off.json", "utf8")) as { cases: Case[] }).cases;

/** A server that counts polls; every endpoint of a case is pointed at it, so an "on" case is seen. */
async function server() {
  const seen = { polls: 0 };
  const s = createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      seen.polls++;
      res.setHeader("Content-Type", "application/x-protobuf");
      res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, {})));
    });
  });
  await new Promise<void>((r) => s.listen(0, "127.0.0.1", r));
  return { seen, endpoint: `http://127.0.0.1:${(s.address() as AddressInfo).port}/v1/opamp`, close: () => s.close() };
}

async function run(c: Case) {
  const s = await server();
  const local = (v: string) => (/^https?:\/\//.test(v) ? s.endpoint : v);
  const env: Record<string, string> = {
    ...Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("OTEL_")) as [string, string][]),
    OTEL_OPAMP_STATE_DIR: "none",
    OTEL_TRACES_EXPORTER: "none",
    OTEL_METRICS_EXPORTER: "none",
    OTEL_LOGS_EXPORTER: "none",
    ...Object.fromEntries(Object.entries(c.env).map(([k, v]) => [k, local(v)])),
  };
  const options = c.code_endpoint === null ? {} : { endpoint: local(c.code_endpoint) };
  const program = `
    import { AGENT, register } from ${JSON.stringify(INDEX)};
    const { shutdown } = register(${JSON.stringify(options)});
    console.log("ON=" + AGENT.on);
    if (!AGENT.on) await shutdown(); else setInterval(() => {}, 1000);`;
  const child = spawn(process.execPath, ["--input-type=module", "-e", program], { env });
  // Without OTEL_LOG_LEVEL the package's warnings are console.warn's: stderr.
  let out = "";
  let warned = "";
  child.stdout.on("data", (d: Buffer) => (out += d.toString()));
  child.stderr.on("data", (d: Buffer) => (warned += d.toString()));
  const exited = new Promise<void>((r) => child.once("exit", () => r()));
  const deadline = Date.now() + 15_000;
  // On: until the server has heard from it, then SIGTERM. Off: it ends by itself.
  while (child.exitCode === null && !(out.includes("ON=true") && s.seen.polls > 0) && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 20));
  }
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), 10_000);
  await exited;
  clearTimeout(timer);
  s.close();
  return { out, warned, polls: s.seen.polls };
}

test("off.json: every case through register()", async () => {
  assert.equal(CASES.length, 16);
  const results = await Promise.all(CASES.map(run));
  CASES.forEach((c, i) => {
    const { out, warned, polls } = results[i]!;
    const said = warned.split("\n").filter((l) => l.includes("OpAMP: off"));
    if (c.off) {
      assert.match(out, /ON=false/, `${c.id}: ${out}`);
      assert.equal(polls, 0, `${c.id}: no client`);
    } else {
      assert.match(out, /ON=true/, `${c.id}: ${out}`);
      assert.ok(polls > 0, `${c.id}: the client polls: ${out}${warned}`);
    }
    if (c.reason) {
      assert.deepEqual(said, [`OpAMP: ${c.reason}`], `${c.id}: said once, at WARN: ${warned}`);
    } else {
      assert.deepEqual(said, [], `${c.id}: nothing to say: ${warned}`);
    }
  });
});
