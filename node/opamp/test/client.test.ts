// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Duplex } from "node:stream";
import { test } from "node:test";

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { WebSocketServer } from "ws";

import { Executor } from "../src/executor.js";
import { EffectSchema, type Input } from "../src/gen/opamp/core/v1/core_pb.js";
import {
  AgentDescriptionSchema,
  type AgentToServer,
  AgentToServerSchema,
  Client,
  type Notification,
  RemoteConfigStatusSchema,
  ServerToAgentSchema,
} from "../src/index.js";

/** The sequence numbers a run should have: 0 until the server has answered a message (those sent
 * before its first answer: RULE-CLIENT-20), then 1, 2, ... */
function expectedSequence(got: { sequenceNum: bigint }[]): bigint[] {
  const first = got.findIndex((m) => m.sequenceNum !== 0n);
  const zeros = first === -1 ? got.length : Math.max(1, first);
  return got.map((_, i) => (i < zeros ? 0n : BigInt(i - zeros + 1)));
}

const UID = new Uint8Array(16).fill(0x42);
const UID_TEXT = "42424242-4242-4242-4242-424242424242";
const description = create(AgentDescriptionSchema, {
  identifyingAttributes: [{ key: "service.name", value: { value: { case: "stringValue", value: "test" } } }],
});

async function body(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return new Uint8Array(Buffer.concat(chunks));
}

test("HTTP: reports, then says goodbye after the request in flight", async () => {
  const got: AgentToServer[] = [];
  const headers: IncomingMessage["headers"][] = [];
  const server = createServer(async (req, res) => {
    headers.push(req.headers);
    const msg = fromBinary(AgentToServerSchema, await body(req));
    got.push(msg);
    // Hold the first poll a little, so stop comes while it is in flight.
    if (got.length === 2) await new Promise((r) => setTimeout(r, 200));
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid })));
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const { port } = server.address() as AddressInfo;
  const notes: Notification[] = [];
  const client = Client.start(
    { endpoint: `http://127.0.0.1:${port}/v1/opamp`, instanceUid: UID, heartbeatIntervalMs: 50,
      headers: { Authorization: "Bearer t" }, mode: "node" },
    { description },
    (n) => notes.push(n),
  );
  while (got.length < 2) await new Promise((r) => setTimeout(r, 10));
  await client.stop();
  server.close();

  assert.ok(got[0]!.agentDescription, "the first message is a full report");
  assert.equal(got.at(-1)!.agentDisconnect !== undefined, true, "the last message says goodbye");
  assert.deepEqual(
    got.map((m) => m.sequenceNum),
    expectedSequence(got),
    "sequence numbers 0 until the first answer, then one apart",
  );
  for (const h of headers) {
    assert.equal(h["content-type"], "application/x-protobuf");
    assert.equal(h["opamp-instance-uid"], UID_TEXT);
    assert.equal(h.authorization, "Bearer t");
  }
  assert.equal(notes.at(-1)?.kind, "stopped");
});

test("endpoint: where the latest request went, once one has an outcome; undefined in Rust mode", async () => {
  const server = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid })));
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const endpoint = `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1/opamp`;
  try {
    for (const mode of ["node", "rust"] as const) {
      const seen: (string | undefined)[] = [];
      const client: Client = Client.start({ endpoint, instanceUid: UID, heartbeatIntervalMs: 50, mode }, { description },
        (n) => void (n.kind === "connected" && seen.push(client.endpoint)));
      assert.equal(client.endpoint, undefined, "no outcome yet");
      const deadline = Date.now() + 10_000;
      while (seen.length === 0 && Date.now() < deadline) await new Promise((r) => setTimeout(r, 10));
      await client.stop();
      assert.deepEqual(seen, [mode === "node" ? endpoint : undefined], mode);
    }
  } finally {
    server.close();
  }
});

test("WebSocket: reports, answers a Close frame, reconnects, says goodbye", async () => {
  const wss = new WebSocketServer({ port: 0, host: "127.0.0.1" });
  await new Promise<void>((r) => wss.on("listening", r));
  const { port } = wss.address() as AddressInfo;
  const conns: AgentToServer[][] = [];
  const upgrades: IncomingMessage["headers"][] = [];
  wss.on("connection", (ws, req) => {
    upgrades.push(req.headers);
    const msgs: AgentToServer[] = [];
    conns.push(msgs);
    ws.on("message", (data: Buffer) => {
      assert.equal(data[0], 0, "the WebSocket header");
      const msg = fromBinary(AgentToServerSchema, new Uint8Array(data.subarray(1)));
      msgs.push(msg);
      const reply = toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid }));
      ws.send(Buffer.concat([Buffer.from([0]), reply]));
      // The server closes the first connection after its first message.
      if (conns.length === 1) ws.close(1001);
    });
  });
  const notes: Notification[] = [];
  const client = Client.start(
    { endpoint: `ws://127.0.0.1:${port}/v1/opamp`, instanceUid: UID, mode: "node" },
    { description },
    (n) => notes.push(n),
  );
  const deadline = Date.now() + 10_000;
  while ((conns.length < 2 || conns[1]!.length < 1) && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 20));
  }
  await client.stop();
  wss.close();

  assert.equal(conns.length, 2, "reconnected after the Close frame");
  for (const msgs of conns) {
    assert.ok(msgs[0]?.agentDescription, "each connection starts with a full report");
  }
  assert.ok(conns[1]!.at(-1)!.agentDisconnect !== undefined, "goodbye on stop");
  for (const h of upgrades) assert.equal(h["opamp-instance-uid"], UID_TEXT);
  assert.equal(notes.filter((n) => n.kind === "connected").length, 2);
  assert.equal(notes.at(-1)?.kind, "stopped");
});

// Found by the differential, in the Rust client (remote-config-restart-ws): after a restart the
// core hands the saved remote config to the application again, in the step that starts connecting,
// and reports it APPLYING until the application answers. What the callback asks before it returns
// reaches the core before anything the client takes in next: here the call is taken by the drain
// that is running the callback, before the event loop can deliver anything else (the connection's
// opening included), so the first message says APPLIED, and none says APPLYING.
// covers: RULE-SHELL-10
for (const mode of ["node", "rust"] as const) {
  test(`a config answered in the callback at a restart is never reported APPLYING (${mode} mode)`, async () => {
    const APPLIED = 1;
    const APPLYING = 2;
    const hash = new Uint8Array([0x63, 0x31]);
    const dir = mkdtempSync(join(tmpdir(), "opamp-answer-"));
    // One server for both lives: the core uses a saved config only on the endpoint it came from.
    const wss = new WebSocketServer({ port: 0, host: "127.0.0.1" });
    await new Promise<void>((r) => wss.on("listening", r));
    const { port } = wss.address() as AddressInfo;
    let got: AgentToServer[] = [];
    let offer = true;
    wss.on("connection", (ws) => {
      ws.on("message", (data: Buffer) => {
        const msg = fromBinary(AgentToServerSchema, new Uint8Array(data.subarray(1)));
        got.push(msg);
        const reply = create(ServerToAgentSchema, {
          instanceUid: msg.instanceUid,
          ...(offer ? { remoteConfig: { config: { configMap: { "": { body: new Uint8Array([1]) } } }, configHash: hash } } : {}),
        });
        offer = false;
        ws.send(Buffer.concat([Buffer.from([0]), toBinary(ServerToAgentSchema, reply)]));
      });
    });
    const statuses = (msgs: AgentToServer[]) =>
      msgs.flatMap((m) =>
        m.remoteConfigStatus && Buffer.from(m.remoteConfigStatus.lastRemoteConfigHash).equals(hash) ? [m.remoteConfigStatus.status as number] : []);
    const life = async (): Promise<AgentToServer[]> => {
      got = [];
      const client: Client = Client.start(
        { endpoint: `ws://127.0.0.1:${port}/v1/opamp`, instanceUid: UID, stateFile: join(dir, "state"), capabilities: 2n, mode },
        { description },
        (n) => {
          // Answered before the callback returns. (Never before start has returned: the client is there.)
          if (n.kind === "remoteConfig") {
            client.setRemoteConfigStatus(create(RemoteConfigStatusSchema, { lastRemoteConfigHash: n.config.configHash, status: APPLIED }));
          }
        },
      );
      const deadline = Date.now() + 10_000;
      while (!statuses(got).includes(APPLIED) && Date.now() < deadline) await new Promise((r) => setTimeout(r, 10));
      await client.stop();
      return got;
    };
    try {
      // The first life: the server offers the config, the application applies it, and it is
      // saved. (Offered by the server, a config is APPLYING in the message that goes before the
      // application is told.)
      assert.deepEqual(statuses(await life()), [APPLYING, APPLIED], "the first life");
      // The second, on what the first saved.
      const second = await life();
      assert.deepEqual(
        [Buffer.from(second[0]!.remoteConfigStatus?.lastRemoteConfigHash ?? []).toString(), second[0]!.remoteConfigStatus?.status],
        ["c1", APPLIED],
        "the first message after the restart",
      );
      assert.ok(!statuses(second).includes(APPLYING), `APPLYING reported after the restart: ${statuses(second)}`);
    } finally {
      wss.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });
}

// Found by the differential (close-frame-ws): a heartbeat sent as the server's Close
// arrived failed ("WebSocket is not open: readyState 2 (CLOSING)"), and the executor reported
// the connection lost, where every other shell reports the server's Close. A send that crosses a
// Close is no loss: the connection is ending already, and its close says how.
// covers: RULE-SHELL-04
test("a send that crosses the server's Close: the connection ends with the Close, not lost", async () => {
  // A raw server: it upgrades, sends Close(1000), and holds the TCP connection open until the
  // client's answer has come (the client is closing then), then ends it when told.
  let socket: Duplex | undefined;
  let answered!: () => void;
  const closing = new Promise<void>((r) => (answered = r));
  const server = createServer();
  server.on("upgrade", (req, sock) => {
    socket = sock;
    const accept = createHash("sha1")
      .update(`${req.headers["sec-websocket-key"]}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
      .digest("base64");
    sock.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
    sock.write(Buffer.from([0x88, 0x02, 0x03, 0xe8]));
    // The client's frames are masked: its Close is opcode 8.
    sock.on("data", (d: Buffer) => {
      if ((d[0]! & 0x0f) === 8) answered();
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const { port } = server.address() as AddressInfo;
  const got: Input[] = [];
  const x = new Executor((i) => got.push(i));
  try {
    x.do(create(EffectSchema, { kind: { case: "wsOpen", value: { id: 1n, url: `ws://127.0.0.1:${port}/v1/opamp`, maxMessageBytes: 1n << 16n, timeoutMs: 5000n } } }));
    await Promise.race([closing, new Promise((_, no) => setTimeout(() => no(new Error(`no Close answer: ${got.map((i) => i.kind.case)}`)), 5000).unref())]);
    // The core's heartbeat, sent before it heard of the Close.
    x.do(create(EffectSchema, { kind: { case: "wsSend", value: { id: 1n, data: new Uint8Array([0, 0x0a, 0]) } } }));
    await new Promise((r) => setTimeout(r, 50));
    socket!.end();
    const deadline = Date.now() + 5000;
    while (!got.some((i) => i.kind.case === "wsClosed" || i.kind.case === "wsLost") && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 10));
    }
    assert.deepEqual(
      got.map((i) => (i.kind.case === "wsClosed" ? `wsClosed(${i.kind.value.code})` : i.kind.case)),
      ["wsOpened", "wsClosed(1000)"],
    );
  } finally {
    await x.shutdown();
    server.close();
  }
});

/** Run a program that starts a client at `endpoint` in `mode` (undefined: the default) and does
 * nothing else: whether it exits by itself (in 10 s). */
async function exitsAlone(endpoint: string, mode: "node" | "rust" | undefined): Promise<boolean> {
  const index = new URL("../src/index.js", import.meta.url).href;
  const child = spawn(process.execPath, [
    "--input-type=module",
    "-e",
    `import { Client } from ${JSON.stringify(index)}; Client.start({ endpoint: ${JSON.stringify(endpoint)}, heartbeatIntervalMs: 50, mode: ${JSON.stringify(mode)}, proxy: false }, {});`,
  ], { stdio: "inherit" });
  const timer = setTimeout(() => child.kill("SIGKILL"), 10_000);
  const code = await new Promise<number | null>((r) => child.once("exit", (c) => r(c)));
  clearTimeout(timer);
  return code === 0;
}

test("the client doesn't keep Node.js running: a program whose work is done exits", async () => {
  let polls = 0;
  const server = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    polls++;
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid })));
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const http = (server.address() as AddressInfo).port;
  const wss = new WebSocketServer({ port: 0, host: "127.0.0.1" });
  await new Promise<void>((r) => wss.on("listening", r));
  let messages = 0;
  wss.on("connection", (ws) => ws.on("message", () => messages++));
  const ws = (wss.address() as AddressInfo).port;
  try {
    // In each mode, and with none named (the default, Rust mode).
    for (const mode of ["node", "rust", undefined] as const) {
      const how = mode ?? "the default mode";
      polls = messages = 0;
      assert.equal(await exitsAlone(`http://127.0.0.1:${http}/v1/opamp`, mode), true, `over HTTP (${how})`);
      assert.ok(polls > 0, `the HTTP client ran (${how})`);
      assert.equal(await exitsAlone(`ws://127.0.0.1:${ws}/v1/opamp`, mode), true, `over WebSocket (${how})`);
      assert.ok(messages > 0, `the WebSocket client ran (${how})`);
      // Nothing to reach: the client waits to retry, and that doesn't hold the program either.
      assert.equal(await exitsAlone("http://127.0.0.1:1/v1/opamp", mode), true, `retrying (${how})`);
    }
  } finally {
    server.close();
    wss.close();
  }
});

// An async callback that fails stops the client as a callback that throws does (RULE-SHELL-02):
// stopped says why, once, and stop() rejects with it. The client never looked at what the
// callback returned: the rejected promise was unhandled, and Node.js ended the process with it
// (exit 1), the client and the application with it. Run in a process of its own, so that a
// regression ends that one.
// covers: RULE-SHELL-02
test("an async callback that rejects stops the client, not the process", async () => {
  const index = new URL("../src/index.js", import.meta.url).href;
  for (const mode of ["node", "rust"] as const) {
    const child = spawn(process.execPath, [
      "--input-type=module",
      "-e",
      `import { Client } from ${JSON.stringify(index)};
       const stopped = [];
       const client = Client.start({ endpoint: "http://127.0.0.1:1/v1/opamp", heartbeatIntervalMs: 50, mode: ${JSON.stringify(mode)}, proxy: false }, {}, async (n) => {
         if (n.kind === "stopped") return void stopped.push(n.error?.message ?? "none");
         await null;
         throw new Error("async boom");
       });
       setTimeout(async () => {
         let err;
         try { await client.stop(); } catch (e) { err = e.message; }
         console.log(JSON.stringify({ err, stopped }));
       }, 1000);`,
    ], { stdio: ["ignore", "pipe", "pipe"] });
    let out = "";
    let errOut = "";
    child.stdout.on("data", (d: Buffer) => (out += d.toString()));
    child.stderr.on("data", (d: Buffer) => (errOut += d.toString()));
    const timer = setTimeout(() => child.kill("SIGKILL"), 15_000);
    const code = await new Promise<number | null>((r) => child.once("exit", (c) => r(c)));
    clearTimeout(timer);
    assert.equal(code, 0, `${mode} mode: the process ended (${code}): ${errOut}`);
    assert.deepEqual(JSON.parse(out), { err: "async boom", stopped: ["async boom"] }, `${mode} mode`);
  }
});

/** Run a program that starts a client at `endpoint` in `mode`, and half a second later ends by
 * awaiting stop(), with nothing else of its own going on: whether stop() returned before the
 * program exited. */
async function stopsBeforeItExits(endpoint: string, mode: "node" | "rust" | undefined): Promise<boolean> {
  const index = new URL("../src/index.js", import.meta.url).href;
  const child = spawn(process.execPath, [
    "--input-type=module",
    "-e",
    `import { Client } from ${JSON.stringify(index)};
     const client = Client.start({ endpoint: ${JSON.stringify(endpoint)}, heartbeatIntervalMs: 50, mode: ${JSON.stringify(mode)}, proxy: false }, {});
     setTimeout(async () => { await client.stop(); console.log("stopped"); }, 500);`,
  ], { stdio: ["ignore", "pipe", "inherit"] });
  let out = "";
  child.stdout.on("data", (d: Buffer) => (out += d.toString()));
  const timer = setTimeout(() => child.kill("SIGKILL"), 15_000);
  await new Promise<void>((r) => child.once("exit", () => r()));
  clearTimeout(timer);
  return out.includes("stopped");
}

// The other half of not keeping Node.js running: stop() is the application's work, and holds the
// program until the client has stopped, so the server hears the goodbye. (The client's sockets
// don't hold it: in Rust mode a program that ended by awaiting stop() would exit mid-stop.)
test("stop() keeps Node.js running until the client has stopped: the goodbye is sent", async () => {
  let goodbyes = 0;
  const server = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    if (msg.agentDisconnect) goodbyes++;
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid })));
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const http = (server.address() as AddressInfo).port;
  const wss = new WebSocketServer({ port: 0, host: "127.0.0.1" });
  await new Promise<void>((r) => wss.on("listening", r));
  wss.on("connection", (ws) =>
    ws.on("message", (data: Buffer) => {
      // The message after its header byte.
      if (fromBinary(AgentToServerSchema, data.subarray(1)).agentDisconnect) goodbyes++;
    }),
  );
  const ws = (wss.address() as AddressInfo).port;
  try {
    for (const mode of ["node", "rust", undefined] as const) {
      const how = mode ?? "the default mode";
      for (const endpoint of [`http://127.0.0.1:${http}/v1/opamp`, `ws://127.0.0.1:${ws}/v1/opamp`]) {
        goodbyes = 0;
        assert.equal(await stopsBeforeItExits(endpoint, mode), true, `${endpoint} (${how}): stop() returned`);
        assert.equal(goodbyes, 1, `${endpoint} (${how}): the goodbye`);
      }
    }
  } finally {
    server.close();
    wss.close();
  }
});

// covers: OPAMP-SIZE-02, OPAMP-SIZE-08
test("maxSendBytes reaches the core: a description too large for it is left out", async () => {
  const got: AgentToServer[] = [];
  const server = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    got.push(msg);
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid })));
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const { port } = server.address() as AddressInfo;
  const big = create(AgentDescriptionSchema, {
    identifyingAttributes: [{ key: "big", value: { value: { case: "stringValue", value: "x".repeat(4096) } } }],
  });
  const notes: Notification[] = [];
  const client = Client.start(
    { endpoint: `http://127.0.0.1:${port}/v1/opamp`, instanceUid: UID, maxSendBytes: 1024, mode: "node" },
    { description: big },
    (n) => notes.push(n),
  );
  while (got.length < 1) await new Promise((r) => setTimeout(r, 10));
  await client.stop();
  server.close();

  assert.equal(got[0]!.agentDescription, undefined, "the description is left out");
  assert.ok(notes.some((n) => n.kind === "statusTooLarge" && n.field === "description"), JSON.stringify(notes));
});

// The core's cancel (RULE-SHELL-06): a request in flight is aborted, a connection being opened is
// given up on and, should its handshake complete after the cancel, closed at once, an open one is
// dropped; nothing more is said of any of them.
// covers: RULE-SHELL-06
test("a cancel aborts a request, a dial and a connection, and nothing more is said of them", async () => {
  const sockets = new Set<Duplex>();
  let posted: () => void = () => {};
  const post = new Promise<void>((r) => (posted = r));
  let upgrade: () => void = () => {};
  const upgrading = new Promise<void>((r) => (upgrade = r));
  let completeUpgrade: () => void = () => {};
  const late = new Promise<void>((r) => (completeUpgrade = r));
  const closedSockets: string[] = [];
  const server = createServer((req, res) => {
    posted();
    req.on("error", () => {});
    res.on("close", () => closedSockets.push("request"));
    // Never answered.
  });
  let upgrades = 0;
  server.on("upgrade", (req, sock: Duplex) => {
    sockets.add(sock);
    sock.on("error", () => {});
    // Read what comes (the end of the stream included).
    sock.resume();
    const n = ++upgrades;
    // The client's end of it (half-open sockets wait for ours to close).
    sock.on("end", () => closedSockets.push(`ws${n}`));
    sock.on("close", () => closedSockets.push(`ws${n}`));
    const accept = createHash("sha1")
      .update(`${req.headers["sec-websocket-key"]}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
      .digest("base64");
    const answer = () => sock.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
    if (n === 1) {
      upgrade();
      void late.then(answer);
    } else {
      answer();
    }
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const { port } = server.address() as AddressInfo;
  const got: Input[] = [];
  const x = new Executor((i) => got.push(i));
  const until = async (what: () => string, ok: () => boolean) => {
    for (let i = 0; i < 500 && !ok(); i++) await new Promise((r) => setTimeout(r, 10));
    assert.ok(ok(), what());
  };
  try {
    const url = `ws://127.0.0.1:${port}/v1/opamp`;
    x.do(create(EffectSchema, { kind: { case: "httpRequest", value: { id: 1n, url: `http://127.0.0.1:${port}/v1/opamp`, body: new Uint8Array([1]), maxResponseBytes: 1n << 16n, timeoutMs: 5000n } } }));
    x.do(create(EffectSchema, { kind: { case: "wsOpen", value: { id: 2n, url, maxMessageBytes: 1n << 16n, timeoutMs: 5000n } } }));
    await post;
    await upgrading;
    x.do(create(EffectSchema, { kind: { case: "cancel", value: { id: 1n } } }));
    x.do(create(EffectSchema, { kind: { case: "cancel", value: { id: 2n } } }));
    completeUpgrade();
    x.do(create(EffectSchema, { kind: { case: "wsOpen", value: { id: 3n, url, maxMessageBytes: 1n << 16n, timeoutMs: 5000n } } }));
    await until(() => "the third connection opens", () => got.some((i) => i.kind.case === "wsOpened"));
    x.do(create(EffectSchema, { kind: { case: "cancel", value: { id: 3n } } }));
    await until(() => `every socket closed: ${closedSockets} ${got.map((i) => i.kind.case)}`, () => ["request", "ws1", "ws2"].every((s) => closedSockets.includes(s)));
    await new Promise((r) => setTimeout(r, 100));
    assert.deepEqual(got.map((i) => `${i.kind.case}(${(i.kind.value as { id: bigint }).id})`), ["wsOpened(3)"]);
  } finally {
    await x.shutdown();
    for (const s of sockets) s.destroy();
    server.close();
  }
});
