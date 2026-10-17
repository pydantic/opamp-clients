// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The register in a real process: how the process ends.

import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { beforeEach, test } from "node:test";

import {
  AgentCapabilities,
  AgentRemoteConfigSchema,
  AgentToServerSchema,
  type AvailableComponents,
  ConnectionSettingsOffersSchema,
  ConnectionSettingsStatuses,
  create,
  fromBinary,
  RemoteConfigStatuses,
  ServerToAgentFlags,
  ServerToAgentSchema,
  toBinary,
} from "@pydantic/opamp-client";

import { FILE, policy } from "./policy.js";

const REGISTER = new URL("../src/register.js", import.meta.url).href;
const INDEX = new URL("../src/index.js", import.meta.url).href;

// The default state directory of every program run here under the test's own home.
beforeEach(() => {
  const home = mkdtempSync(join(tmpdir(), "opamp-otel-home-"));
  process.env.XDG_STATE_HOME = home;
  process.env.LOCALAPPDATA = home;
  process.env.HOME = home;
});

async function body(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return new Uint8Array(Buffer.concat(chunks));
}

/** An OpAMP server that answers every poll: how many it had, whether one said goodbye, and the
 * available components each sent (`askComponents`: asked for in full in the first answer). */
async function server(askComponents = false) {
  const seen = {
    polls: 0, goodbye: false, authorization: [] as (string | undefined)[], components: [] as (AvailableComponents | undefined)[],
    capabilities: [] as bigint[],
  };
  const s = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    seen.polls++;
    seen.authorization.push(req.headers.authorization);
    seen.components.push(msg.availableComponents);
    seen.capabilities.push(msg.capabilities);
    if (msg.agentDisconnect) seen.goodbye = true;
    const flags = askComponents && seen.polls === 1 ? BigInt(ServerToAgentFlags.ServerToAgentFlags_ReportAvailableComponents) : 0n;
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid, flags })));
  });
  await new Promise<void>((r) => s.listen(0, "127.0.0.1", r));
  return { seen, endpoint: `http://127.0.0.1:${(s.address() as AddressInfo).port}/v1/opamp`, close: () => s.close() };
}

/** Run `program` under the register: its exit code, signal and output. `whenPolled` runs once the
 * server has heard from it. */
async function run(program: string | string[], endpoint: string,
  whenPolled?: (kill: (s: NodeJS.Signals) => void) => void, seen?: { polls: number },
  extra: Record<string, string> = {}) {
  const args = typeof program === "string" ? ["--input-type=module", "-e", program] : program;
  const child = spawn(process.execPath, ["--import", REGISTER, ...args], {
    env: {
      ...Object.fromEntries(Object.entries(process.env).filter(([k]) => k !== "OTEL_LOG_LEVEL")),
      OTEL_OPAMP_ENDPOINT: endpoint,
      OTEL_OPAMP_HEARTBEAT_INTERVAL: "100ms",
      OTEL_TRACES_EXPORTER: "none",
      OTEL_METRICS_EXPORTER: "none",
      OTEL_LOGS_EXPORTER: "none",
      ...extra,
    },
  });
  let out = "";
  child.stdout.on("data", (d: Buffer) => (out += d.toString()));
  child.stderr.on("data", (d: Buffer) => (out += d.toString()));
  const timer = setTimeout(() => child.kill("SIGKILL"), 15_000);
  if (whenPolled && seen) {
    void (async () => {
      while (seen.polls === 0 && child.exitCode === null) await new Promise((r) => setTimeout(r, 20));
      whenPolled((s) => child.kill(s));
    })();
  }
  const [code, signal] = await new Promise<[number | null, NodeJS.Signals | null]>((r) =>
    child.once("exit", (c, s) => r([c, s])));
  clearTimeout(timer);
  return { code, signal, out };
}

test("a program whose work is done exits, and says goodbye", async () => {
  const s = await server();
  try {
    // Its work: until the server has heard from it.
    const r = await run("setTimeout(() => {}, 1500);", s.endpoint);
    assert.deepEqual([r.code, r.signal], [0, null], r.out);
    assert.ok(s.seen.polls > 0);
    assert.equal(s.seen.goodbye, true);
  } finally {
    s.close();
  }
});

test("SIGTERM: goodbye, then the process ends as Node.js ends it", async () => {
  const s = await server();
  try {
    const r = await run("setInterval(() => {}, 1000);", s.endpoint, (kill) => kill("SIGTERM"), s.seen);
    assert.deepEqual([r.code, r.signal], [null, "SIGTERM"], r.out);
    assert.equal(s.seen.goodbye, true);
  } finally {
    s.close();
  }
});

test("SIGTERM with the application's own listener: the application decides", async () => {
  const s = await server();
  try {
    const program = `process.on("SIGTERM", () => { console.log("mine"); setTimeout(() => process.exit(7), 500); });
      setInterval(() => {}, 1000);`;
    const r = await run(program, s.endpoint, (kill) => kill("SIGTERM"), s.seen);
    assert.deepEqual([r.code, r.signal], [7, null], r.out);
    assert.equal(r.out.match(/mine/g)?.length, 1, r.out);
    assert.equal(s.seen.goodbye, true);
  } finally {
    s.close();
  }
});

test("the inventory reaches the server: its hash at once, in full when asked, then left out while unchanged", async () => {
  const s = await server(true);
  try {
    const r = await run("setTimeout(() => {}, 1500);", s.endpoint, undefined, undefined, {
      OTEL_NODE_ENABLED_INSTRUMENTATIONS: "http,express",
      OTEL_TRACES_EXPORTER: "zipkin", // NodeSDK's: ungated, and send_traces not carried out
    });
    assert.deepEqual([r.code, r.signal], [0, null], r.out);
    const [first, full] = s.seen.components;
    assert.ok(first && full, r.out);
    assert.ok(first.hash.length > 0, "the hash, computed by the client"); // the core's, of what was set
    assert.deepEqual(first.components, {});
    assert.deepEqual(full.hash, first.hash);
    const c = full.components;
    assert.deepEqual(Object.keys(c).sort(), ["exporters", "instrumentations", "sdk", "settings"]);
    const meta = Object.fromEntries(c.sdk!.metadata.map((kv) => [kv.key, kv.value?.value.value]));
    assert.equal(meta["integration.name"], "@pydantic/opamp-otel");
    assert.equal(meta["telemetry.sdk.language"], "nodejs");
    assert.match(String(meta["telemetry.sdk.version"]), /^\d+\.\d+\.\d+/);
    // The instrumentations the variables load, under the names deactivate_instrumentations takes.
    assert.deepEqual(Object.keys(c.instrumentations!.subComponentMap).sort(), ["express", "http"]);
    for (const i of Object.values(c.instrumentations!.subComponentMap)) {
      assert.match(String(i.metadata.find((kv) => kv.key === "version")?.value?.value.value), /^\d+\.\d+\.\d+/);
    }
    const gated = Object.fromEntries(Object.entries(c.exporters!.subComponentMap).map(([signal, d]) =>
      [signal, Object.fromEntries(Object.entries(d.subComponentMap).map(([n, e]) => [n, e.metadata[0]?.value?.value.value]))]));
    assert.deepEqual(gated, { traces: { zipkin: false } }); // metrics and logs: none
    assert.equal(c.settings!.subComponentMap.send_traces, undefined);
    assert.ok(c.settings!.subComponentMap.send_logs);
    // Sent in full once, as asked; unchanged after that, so left out. The capability on every message.
    assert.ok(s.seen.components.length > 2 && s.seen.components.slice(2).every((a) => a === undefined), r.out);
    const reports = BigInt(AgentCapabilities.AgentCapabilities_ReportsAvailableComponents);
    assert.ok(s.seen.capabilities.every((c) => (c & reports) === reports));
  } finally {
    s.close();
  }
});

test("a CommonJS program that requires an ES module starts", async () => {
  // require() of an ES module goes through the loader hook too (Node.js 22.12 and later): the
  // module import-in-the-middle wraps it in imports import-in-the-middle's own register, and when
  // that wasn't loaded yet, Node.js failed an internal assertion and the program died at start
  // (the demo's payment service, through uuid).
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-cjs-"));
  mkdirSync(join(dir, "node_modules", "esm-only"), { recursive: true });
  writeFileSync(join(dir, "node_modules", "esm-only", "package.json"),
    JSON.stringify({ name: "esm-only", type: "module", exports: "./index.js" }));
  writeFileSync(join(dir, "node_modules", "esm-only", "index.js"), "export { answer } from './answer.js';\n");
  writeFileSync(join(dir, "node_modules", "esm-only", "answer.js"), "export const answer = 42;\n");
  writeFileSync(join(dir, "app.cjs"), "const { answer } = require('esm-only'); console.log('answer', answer);\n");
  const s = await server();
  try {
    const { code, out } = await run([join(dir, "app.cjs")], s.endpoint);
    assert.equal(code, 0, out);
    assert.match(out, /answer 42/);
  } finally {
    s.close();
  }
});

test("a server it can't reach is said on stderr, with no OTEL_LOG_LEVEL", async () => {
  const s = await server();
  s.close(); // its port, with nothing listening
  const r = await run("setTimeout(() => {}, 1000);", s.endpoint);
  assert.deepEqual([r.code, r.signal], [0, null], r.out);
  assert.match(r.out, /OpAMP: can't reach http:\/\/127\.0\.0\.1:\d+\/v1\/opamp \(.*\): check OTEL_OPAMP_ENDPOINT and that the server is up/);
});

test("a bad heartbeat interval or header escape is warned about, and the program starts managed", async () => {
  const s = await server();
  const home = mkdtempSync(join(tmpdir(), "opamp-otel-home-"));
  try {
    const r = await run("setTimeout(() => {}, 1500);", s.endpoint, undefined, undefined, {
      OTEL_OPAMP_HEARTBEAT_INTERVAL: "soon",
      OTEL_OPAMP_HEADERS: "Authorization=Bearer%zz",
      XDG_STATE_HOME: home, HOME: home, LOCALAPPDATA: home,
    });
    assert.deepEqual([r.code, r.signal], [0, null], r.out);
    assert.ok(r.out.includes("OpAMP: OTEL_OPAMP_HEARTBEAT_INTERVAL 'soon' is not a duration (as 1500, 500ms, 30s or 2m): "
      + "using the default, 30s"), r.out);
    assert.ok(r.out.includes("OpAMP: OTEL_OPAMP_HEADERS: the value of 'Authorization' has a percent escape that can't "
      + "be decoded: it is sent as written"), r.out);
    assert.ok(s.seen.polls > 0 && s.seen.goodbye, r.out);
  } finally {
    s.close();
  }
});

/** Run `program`, an ES module that imports register from the package (INDEX) and binds `shutdown`,
 * with no OTEL_OPAMP_* variable: its exit code and output. It runs until `until` holds (or 15 s),
 * then gets a SIGTERM, on which it shuts down, says so and exits: no fixed time a slow start (a cloud
 * resource detector timing out) can outrun. */
async function inCode(program: string, until: (out: string) => boolean, extra: Record<string, string> = {}) {
  const child = spawn(process.execPath, ["--input-type=module", "-e", `${program}
    process.on("SIGTERM", () => shutdown().then(() => { console.log("still running"); process.exit(0); }));
    setInterval(() => {}, 1000);`], {
    env: {
      ...Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("OTEL_"))),
      OTEL_TRACES_EXPORTER: "none",
      OTEL_METRICS_EXPORTER: "none",
      OTEL_LOGS_EXPORTER: "none",
      ...extra,
    },
  });
  let out = "";
  child.stdout.on("data", (d: Buffer) => (out += d.toString()));
  child.stderr.on("data", (d: Buffer) => (out += d.toString()));
  const exited = new Promise<number | null>((r) => child.once("exit", (c) => r(c)));
  const deadline = Date.now() + 15_000;
  while (!until(out) && child.exitCode === null && Date.now() < deadline) await new Promise((r) => setTimeout(r, 20));
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), 10_000);
  const code = await exited;
  clearTimeout(timer);
  return { code, out };
}

test("the server given in code, with no OTEL_OPAMP_* variable: polled with its headers, and a goodbye", async () => {
  const s = await server();
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-state-"));
  try {
    const { code, out } = await inCode(`
      import { register } from ${JSON.stringify(INDEX)};
      const { shutdown } = register({
        endpoint: ${JSON.stringify(s.endpoint)},
        headers: { Authorization: "Bearer from-code" },
        stateDir: ${JSON.stringify(dir)},
        heartbeatIntervalMs: 100,
      });`, () => s.seen.polls > 3);
    assert.equal(code, 0, out);
    // At 100 ms, not the default 30 s: several polls, within the 15 s it may wait.
    assert.ok(s.seen.polls > 2, `${s.seen.polls} polls: ${out}`);
    assert.ok(s.seen.authorization.every((a) => a === "Bearer from-code"), String(s.seen.authorization));
    assert.equal(s.seen.goodbye, true);
    // The state directory given in code is the one used.
    assert.match(readFileSync(join(dir, "instance-uid"), "utf8"), /^[0-9a-f-]{36}$/);
  } finally {
    s.close();
  }
});

test("given in code, the variables are not read: code wins in a real process", async () => {
  const s = await server();
  try {
    const { code, out } = await inCode(`
      import { register } from ${JSON.stringify(INDEX)};
      const { shutdown } = register({ endpoint: ${JSON.stringify(s.endpoint)}, headers: { Authorization: "Bearer code" },
        heartbeatIntervalMs: 100, stateDir: "none" });`, () => s.seen.polls > 3, {
      OTEL_OPAMP_ENDPOINT: "http://127.0.0.1:1/v1/opamp",
      OTEL_OPAMP_HEADERS: "Authorization=Bearer%20env",
      OTEL_OPAMP_HEARTBEAT_INTERVAL: "1m",
    });
    assert.equal(code, 0, out);
    assert.ok(s.seen.polls > 2, `${s.seen.polls} polls: ${out}`);
    assert.ok(s.seen.authorization.every((a) => a === "Bearer code"), String(s.seen.authorization));
  } finally {
    s.close();
  }
});

test("bad values given in code are warned about and the program runs: a scheme the client can't speak, wrong types", async () => {
  const { code, out } = await inCode(`
    import { register } from ${JSON.stringify(INDEX)};
    const { shutdown } = register({ endpoint: "ftp://127.0.0.1:1/v1/opamp", headers: "Authorization=x", stateDir: 7,
      heartbeatIntervalMs: -5 });`, (out) => /did not start/.test(out));
  assert.equal(code, 0, out);
  assert.match(out, /still running/);
  assert.match(out, /OpAMP: headers is not an object of names to values: it is not used/);
  assert.match(out, /OpAMP: stateDir 7 is not a string: it is not used/);
  assert.match(out, /OpAMP: heartbeatIntervalMs -5 is not a whole number of milliseconds: using the default, 30s/);
  // The client refuses the scheme at start (the shared core checks the endpoint): unmanaged, still running.
  assert.match(out, /OpAMP: the client did not start; the SDK runs unmanaged: .*a scheme the client doesn't take/);
});

/** An OpAMP server that takes an enrollment key and, as the reference server does, hands an agent
 * that says it takes connection settings a key of its own in its answer to the agent's first
 * message with the enrollment key; once the agent uses its own key, the enrollment key speaks for
 * it no more (401). */
/** An OpAMP server that offers `offer` (a Logfire SDK policy) in its first answer, if
 * given: the statuses it is sent, and the settings each first message reports in force. */
async function configServer(offer?: string) {
  const seen = { polls: 0, applied: false, firstEffective: [] as string[] };
  const s = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    seen.polls++;
    const status = msg.remoteConfigStatus;
    if (status?.status === RemoteConfigStatuses.RemoteConfigStatuses_APPLIED) seen.applied = true;
    const reported = msg.effectiveConfig?.configMap?.configMap[FILE];
    if (msg.sequenceNum === 0n && reported) seen.firstEffective.push(new TextDecoder().decode(reported.body));
    const reply = create(ServerToAgentSchema, { instanceUid: msg.instanceUid });
    if (offer !== undefined && seen.polls === 1) {
      const body = new TextEncoder().encode(offer);
      reply.remoteConfig = create(AgentRemoteConfigSchema, {
        configHash: body, config: { configMap: { [FILE]: { body, contentType: "application/json" } } },
      });
    }
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, reply));
  });
  await new Promise<void>((r) => s.listen(0, "127.0.0.1", r));
  return { seen, endpoint: `http://127.0.0.1:${(s.address() as AddressInfo).port}/v1/opamp`, close: () => s.close() };
}

test("a config applied through the server is where the next start begins, with no file of the package's own", async () => {
  // The server hears APPLIED once the client has saved the config with its state.
  const first = await configServer(policy({ head_sampling: { ratio: 0.5 } }));
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-state-"));
  const env = { OTEL_OPAMP_STATE_DIR: dir };
  try {
    const r = await run("setInterval(() => {}, 1000);", first.endpoint, (kill) => void (async () => {
      const deadline = Date.now() + 12_000;
      while (!first.seen.applied && Date.now() < deadline) await new Promise((w) => setTimeout(w, 20));
      kill("SIGTERM");
    })(), first.seen, env);
    assert.ok(first.seen.applied, `never APPLIED: ${r.out}`);
  } finally {
    first.close();
  }
  assert.equal(existsSync(join(dir, "sdk-settings.json")), false);
  // The next start, against a server that sends nothing: the settings applied, in force from the first message.
  const second = await configServer();
  try {
    const r = await run("setTimeout(() => {}, 1000);", second.endpoint, undefined, undefined, env);
    const sampling = second.seen.firstEffective.map((e) => JSON.parse(e).head_sampling);
    assert.deepEqual(sampling, [{ mode: "override", ratio: 0.5 }], r.out);
  } finally {
    second.close();
  }
});

/** An OpAMP server that takes an enrollment key and, as the reference server does, hands an agent
 * that says it takes connection settings a key of its own; once the agent uses it, the enrollment
 * key speaks for it no more (401). Once the agent has said APPLIED of its own key, it offers
 * `next` (if set) in answer to each message with it: another key, here or at another server's
 * endpoint. `keys` are the keys it takes besides the enrollment key (taken only with `enroll`);
 * once a request comes with `retires`, "own" is taken no more. */
async function keyServer(keys = ["own"], enroll = true) {
  const takesKeys = BigInt(AgentCapabilities.AgentCapabilities_AcceptsOpAMPConnectionSettings)
    | BigInt(AgentCapabilities.AgentCapabilities_ReportsConnectionSettingsStatus);
  const seen = {
    polls: 0, authorization: [] as (string | undefined)[], capabilities: [] as bigint[], applied: false, ownUsed: false,
    statuses: [] as [ConnectionSettingsStatuses, string][], ownGone: false,
  };
  const plan = { keys, next: undefined as ReturnType<typeof offer> | undefined, retires: undefined as string | undefined };
  let endpoint = "";
  const s = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    const auth = req.headers.authorization;
    seen.authorization.push(auth);
    const key = auth?.startsWith("Bearer ") ? auth.slice("Bearer ".length) : "";
    const taken = (auth === "Bearer enroll" && enroll && !seen.ownUsed)
      || (plan.keys.includes(key) && !(key === "own" && seen.ownGone));
    if (!taken) {
      res.statusCode = 401;
      res.end();
      return;
    }
    seen.polls++;
    seen.ownUsed ||= key === "own";
    seen.ownGone ||= plan.retires !== undefined && key === plan.retires;
    seen.capabilities.push(msg.capabilities);
    const status = msg.connectionSettingsStatus;
    if (status) seen.statuses.push([status.status, Buffer.from(status.lastConnectionSettingsHash).toString()]);
    if (status?.status === ConnectionSettingsStatuses.ConnectionSettingsStatuses_APPLIED
      && Buffer.from(status.lastConnectionSettingsHash).toString() === "own-key-offer" && auth === "Bearer own") {
      seen.applied = true;
    }
    const reply = create(ServerToAgentSchema, { instanceUid: msg.instanceUid });
    if (auth === "Bearer enroll" && (msg.capabilities & takesKeys) === takesKeys) {
      reply.connectionSettings = create(ConnectionSettingsOffersSchema, {
        hash: new TextEncoder().encode("own-key-offer"),
        opamp: {
          destinationEndpoint: endpoint,
          headers: { headers: [{ key: "Authorization", value: "Bearer own" }] },
          heartbeatIntervalSeconds: 1n,
        },
      });
    } else if (key === "own" && plan.next && seen.applied) {
      reply.connectionSettings = plan.next;
    }
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, reply));
  });
  await new Promise<void>((r) => s.listen(0, "127.0.0.1", r));
  endpoint = `http://127.0.0.1:${(s.address() as AddressInfo).port}/v1/opamp`;
  /** Whether the agent reported `status` of the offer `hash` here. */
  const said = (status: ConnectionSettingsStatuses, hash: string) =>
    seen.statuses.some(([st, h]) => st === status && h === hash);
  return { seen, plan, said, endpoint, takesKeys, close: () => s.close() };
}

/** An offer, by hash, of `key` at `endpoint`. */
function offer(hash: string, endpoint: string, key: string) {
  return create(ConnectionSettingsOffersSchema, {
    hash: new TextEncoder().encode(hash),
    opamp: { destinationEndpoint: endpoint, headers: { headers: [{ key: "Authorization", value: `Bearer ${key}` }] } },
  });
}

/** Run a program that keeps running until `done` holds (or 12 s, before run's own 15 s). */
function runUntil(endpoint: string, seen: { polls: number }, env: Record<string, string>, done: () => boolean) {
  return run("setInterval(() => {}, 1000);", endpoint, (kill) => void (async () => {
    const deadline = Date.now() + 12_000;
    while (!done() && Date.now() < deadline) await new Promise((w) => setTimeout(w, 20));
    kill("SIGTERM");
  })(), seen, env);
}

const APPLIED = ConnectionSettingsStatuses.ConnectionSettingsStatuses_APPLIED;
const FAILED = ConnectionSettingsStatuses.ConnectionSettingsStatuses_FAILED;

test("with a state directory the agent takes a key of its own, says APPLIED on it, and the next start keeps it", async () => {
  const s = await keyServer();
  const dir = mkdtempSync(join(tmpdir(), "opamp-otel-state-"));
  const env = { OTEL_OPAMP_HEADERS: "Authorization=Bearer%20enroll", OTEL_OPAMP_STATE_DIR: dir };
  try {
    const r = await run("setInterval(() => {}, 1000);", s.endpoint, (kill) => void (async () => {
      const deadline = Date.now() + 12_000; // before run's own (15 s)
      while (!s.seen.applied && Date.now() < deadline) await new Promise((w) => setTimeout(w, 20));
      kill("SIGTERM");
    })(), s.seen, env);
    assert.ok(s.seen.applied, `the key offered was never APPLIED on it: ${r.out}`);
    assert.equal(s.seen.capabilities[0]! & s.takesKeys, s.takesKeys, "it says it takes connection settings");
    const before = s.seen.authorization.length;
    const again = await run("setTimeout(() => {}, 1000);", s.endpoint, undefined, undefined, env);
    assert.ok(s.seen.authorization.length > before, again.out);
    assert.equal(s.seen.authorization[before], "Bearer own", "the next start comes with the key it was given");
  } finally {
    s.close();
  }
});

test("without a state directory the agent doesn't say it takes connection settings", async () => {
  const s = await keyServer();
  try {
    const r = await run("setTimeout(() => {}, 1000);", s.endpoint, undefined, undefined,
      { OTEL_OPAMP_HEADERS: "Authorization=Bearer%20enroll", OTEL_OPAMP_STATE_DIR: "none" });
    assert.ok(s.seen.polls > 0, r.out);
    assert.ok(s.seen.capabilities.every((c) => (c & s.takesKeys) === 0n));
    assert.ok(s.seen.authorization.every((a) => a === "Bearer enroll"));
  } finally {
    s.close();
  }
});


test("the agent follows a move to another server, says APPLIED there, and the next start goes there first", async () => {
  const a = await keyServer();
  const b = await keyServer(["moved"], false);
  a.plan.next = offer("move-to-b", b.endpoint, "moved");
  const env = { OTEL_OPAMP_HEADERS: "Authorization=Bearer%20enroll", OTEL_OPAMP_STATE_DIR: mkdtempSync(join(tmpdir(), "opamp-otel-state-")) };
  try {
    const r = await runUntil(a.endpoint, a.seen, env, () => b.said(APPLIED, "move-to-b"));
    assert.ok(b.said(APPLIED, "move-to-b"), `the move was never APPLIED at B: ${r.out}`);
    assert.ok(b.seen.authorization.every((x) => x === "Bearer moved"), "B hears only the key the agent was moved with");
    const [atA, atB] = [a.seen.authorization.length, b.seen.authorization.length];
    const again = await run("setTimeout(() => {}, 1000);", a.endpoint, undefined, undefined, env);
    assert.ok(b.seen.authorization.length > atB, again.out);
    assert.equal(a.seen.authorization.length, atA, "the next start went to A, not to B");
    assert.equal(b.seen.authorization[atB], "Bearer moved", "the next start comes to B with the key it was moved with");
  } finally {
    a.close();
    b.close();
  }
});

test("the agent takes a second key at the same endpoint, keeps it, and never sends the old one again", async () => {
  const s = await keyServer(["own", "own2"]);
  s.plan.retires = "own2";
  s.plan.next = offer("second-key", s.endpoint, "own2");
  const env = { OTEL_OPAMP_HEADERS: "Authorization=Bearer%20enroll", OTEL_OPAMP_STATE_DIR: mkdtempSync(join(tmpdir(), "opamp-otel-state-")) };
  try {
    let since = -1;
    const r = await runUntil(s.endpoint, s.seen, env, () => {
      if (since < 0 && s.said(APPLIED, "second-key")) since = s.seen.authorization.length;
      return since >= 0 && s.seen.authorization.length > since + 2;
    });
    assert.ok(since >= 0, `the second key was never APPLIED: ${r.out}`);
    const before = s.seen.authorization.length;
    const again = await run("setTimeout(() => {}, 1000);", s.endpoint, undefined, undefined, env);
    assert.ok(s.seen.authorization.length > before, again.out);
    assert.deepEqual(s.seen.authorization.slice(since).filter((x) => x !== "Bearer own2"), [],
      "after the second key was APPLIED only it is sent");
  } finally {
    s.close();
  }
});

test("a move nobody answers is FAILED, and the agent carries on where it was with the key it had", async () => {
  const closed = createServer();
  await new Promise<void>((r) => closed.listen(0, "127.0.0.1", r));
  const nowhere = `http://127.0.0.1:${(closed.address() as AddressInfo).port}/v1/opamp`;
  await new Promise((r) => closed.close(r));
  const s = await keyServer();
  s.plan.next = offer("move-nowhere", nowhere, "moved");
  const env = { OTEL_OPAMP_HEADERS: "Authorization=Bearer%20enroll", OTEL_OPAMP_STATE_DIR: mkdtempSync(join(tmpdir(), "opamp-otel-state-")) };
  try {
    let since = -1;
    const r = await runUntil(s.endpoint, s.seen, env, () => {
      if (since < 0 && s.said(FAILED, "move-nowhere")) since = s.seen.authorization.length;
      return since >= 0 && s.seen.authorization.length > since + 2;
    });
    assert.ok(since >= 0, `the move was never FAILED: ${r.out}`);
    assert.ok(s.seen.authorization.length > since + 2, `the agent did not carry on with A: ${r.out}`);
    assert.deepEqual(s.seen.authorization.slice(since).filter((x) => x !== "Bearer own"), []);
    assert.ok(!s.said(APPLIED, "move-nowhere"));
  } finally {
    s.close();
  }
});
