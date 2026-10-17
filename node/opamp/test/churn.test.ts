// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Lifecycle churn: an agent that reconfigures its client stops it and starts another, again and
// again, in one process. Whatever a start leaves behind when its client stops (a core instance, a
// socket, a timer, an undici agent) adds up there. This starts and stops clients many times, at
// every point a stop can come (at once, mid-handshake, with the connection up, from the callback,
// just after a remote config, with a state file, after the callback threw), over HTTP and
// WebSocket, in both modes, against a server on loopback, and checks that the process's active
// handles, open file descriptors and memory come back to what they were after a warm-up.

import assert from "node:assert/strict";
import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { createServer, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import type { Duplex } from "node:stream";
import { join } from "node:path";
import { test } from "node:test";
import { setFlagsFromString } from "node:v8";
import { runInNewContext } from "node:vm";

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { WebSocketServer } from "ws";

import { AgentToServerSchema, Client, type Notification, RemoteConfigStatusSchema, ServerToAgentSchema } from "../src/index.js";

// A full collection between measures, without --expose-gc on the command line.
setFlagsFromString("--expose-gc");
const gc = runInNewContext("gc") as () => void;

async function body(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return new Uint8Array(Buffer.concat(chunks));
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Every answer offers this remote config. */
function reply(data: Uint8Array): { out: Uint8Array; goodbye: boolean } {
  const msg = fromBinary(AgentToServerSchema, data);
  const config = { config: { configMap: { "": { body: new Uint8Array([1]) } } }, configHash: new Uint8Array([0x63]) };
  const out = toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid, remoteConfig: config }));
  return { out, goodbye: msg.agentDisconnect !== undefined };
}

/** A server that answers every message with a remote config, over HTTP and WebSocket. On /hold it
 * holds what it gets (a WebSocket upgrade, or a POST that isn't the goodbye) until the client goes,
 * counting `held`: a stop then comes mid-handshake or mid-request. */
class ChurnServer {
  readonly http: Server;
  readonly wss = new WebSocketServer({ noServer: true });
  held = 0;
  /** Sockets upgraded or held, which closing the server leaves open. */
  readonly #upgraded = new Set<Duplex>();

  constructor() {
    this.http = createServer(async (req, res) => {
      const { out, goodbye } = reply(await body(req));
      if (req.url === "/hold" && !goodbye) {
        this.held++;
        return; // answered never: the client's going closes it
      }
      res.setHeader("Content-Type", "application/x-protobuf");
      res.end(out);
    });
    this.http.on("upgrade", (req, socket, head) => {
      socket.on("error", () => {});
      this.#upgraded.add(socket);
      socket.on("close", () => this.#upgraded.delete(socket));
      if (req.url === "/hold") {
        this.held++;
        // The handshake never ends: the client's going closes it. (The server's sockets are
        // half-open: its end is closed here, or it stays open.)
        socket.on("end", () => socket.destroy());
        return;
      }
      this.wss.handleUpgrade(req, socket, head, (ws) => {
        ws.on("message", (data: Buffer) => ws.send(Buffer.concat([Buffer.from([0]), reply(new Uint8Array(data.subarray(1))).out])));
        ws.on("error", () => {});
      });
    });
  }

  async listen(): Promise<number> {
    await new Promise<void>((r) => this.http.listen(0, "127.0.0.1", r));
    return (this.http.address() as AddressInfo).port;
  }

  async close(): Promise<void> {
    for (const socket of this.#upgraded) socket.destroy();
    this.http.closeAllConnections();
    await new Promise((r) => this.http.close(r));
  }
}

/** Where a run's client is stopped. */
const STOPS = ["at once", "held", "connected", "from callback", "after config", "state file", "threw"] as const;
type Stop = (typeof STOPS)[number];

/** Start one client, stop it at `where`, and check it stopped as it should. */
async function churnRun(srv: ChurnServer, port: number, dir: string, mode: "rust" | "node", ws: boolean, where: Stop): Promise<void> {
  const endpoint = `${ws ? "ws" : "http"}://127.0.0.1:${port}${where === "held" ? "/hold" : "/v1/opamp"}`;
  let configs = 0;
  let stopped: Notification | undefined;
  let fromCallback: Promise<void> | undefined;
  const held = srv.held;
  const client: Client = Client.start(
    {
      endpoint,
      mode,
      proxy: false,
      heartbeatIntervalMs: 3_600_000,
      // Over HTTP the goodbye waits for the request held: the stop gives up on both then.
      stopTimeoutMs: where === "held" ? 50 : 2000,
      capabilities: 2n, // AcceptsRemoteConfig
      // The same file each time, as an agent that restarts its client keeps it.
      ...(where === "state file" ? { stateFile: join(dir, "state") } : {}),
    },
    {},
    (n) => {
      if (n.kind === "stopped") stopped = n;
      if (n.kind !== "remoteConfig") return;
      configs++;
      client.setRemoteConfigStatus(create(RemoteConfigStatusSchema, { lastRemoteConfigHash: n.config.configHash, status: 1 }));
      if (where === "from callback") fromCallback ??= client.stop();
      if (where === "threw") throw new Error("churn");
    },
  );
  const until = async (what: string, ok: () => boolean) => {
    for (const deadline = Date.now() + 10_000; !ok(); await sleep(1)) {
      if (Date.now() > deadline) assert.fail(`${where}: ${what} never happened`);
    }
  };
  switch (where) {
    case "held":
      await until("the held request", () => srv.held > held);
      break;
    case "connected":
    case "after config":
    case "state file":
      await until("a remote config", () => configs > 0);
      break;
    case "from callback":
      await until("the stop from the callback", () => fromCallback !== undefined);
      await fromCallback;
      break;
    case "threw":
      await until("the stop the throw made", () => stopped !== undefined);
      break;
  }
  if (where === "threw") {
    await assert.rejects(client.stop(), /churn/, `${where}: stop() didn't say why`);
  } else {
    await client.stop();
  }
  assert.equal(stopped?.kind, "stopped", `${where}: stop() returned before stopped was told`);
}

interface Usage {
  handles: number;
  fds: number; // -1 where /proc/self/fd isn't there
  memory: number; // heap and external (WebAssembly memories, buffers), bytes
}

function openFds(): number {
  try {
    return readdirSync("/proc/self/fd").length;
  } catch {
    return -1;
  }
}

/** What the process holds, after a full collection. (V8 frees the WebAssembly memories it
 * collected off the main thread, after the collection: the second one counts them gone.) */
async function measure(): Promise<Usage> {
  gc();
  await sleep(50);
  gc();
  const m = process.memoryUsage();
  return { handles: process.getActiveResourcesInfo().length, fds: openFds(), memory: m.heapUsed + m.external };
}

const show = (u: Usage) => `${u.handles} handles, ${u.fds} fds, ${u.memory >> 10} KiB`;

/** Measure until handles and fds are within `slack` of `base` (what a client let go of closes on
 * the event loop: a socket destroyed, a save finishing), or 5 s have gone. */
async function settle(base: Usage, slack: number): Promise<Usage> {
  for (const deadline = Date.now() + 5000; ; await sleep(20)) {
    const u = await measure();
    if ((u.handles <= base.handles + slack && u.fds <= base.fds + slack) || Date.now() > deadline) return u;
  }
}

// Starting and stopping clients again and again leaves nothing behind: after a warm-up, rounds of
// a client stopped at every point, over HTTP and WebSocket, leave the process with no more active
// handles, open file descriptors or memory than before them. A client's core instance is hundreds
// of KiB of WebAssembly memory, so a client kept is plain in the memory: Rust mode's streams kept
// each client stopped for 2 s after its connection had closed (the linger timer, never cleared,
// held the socket and so the client), some 80 MiB over these rounds. Its connections are a
// descriptor each (the client's timers and sockets are unref'd, which active handles leave out).
for (const mode of ["rust", "node"] as const) {
  test(`churn leaves nothing behind (${mode} mode)`, async (t) => {
    const warmup = 2;
    const rounds = 15;
    const srv = new ChurnServer();
    const port = await srv.listen();
    const dir = mkdtempSync(join(tmpdir(), "opamp-churn-"));
    const round = async () => {
      for (const where of STOPS) {
        for (const ws of [false, true]) await churnRun(srv, port, dir, mode, ws, where);
      }
    };
    try {
      for (let i = 0; i < warmup; i++) await round();
      const base = await settle(await measure(), 0);
      const began = Date.now();
      for (let i = 0; i < rounds; i++) await round();
      const took = Date.now() - began;
      const after = await settle(base, 2);
      const runs = rounds * STOPS.length * 2;
      t.diagnostic(`${runs} clients in ${took} ms: before ${show(base)}; after ${show(after)}`);
      assert.ok(after.handles <= base.handles + 2,
        `active handles grew from ${base.handles} to ${after.handles} over ${runs} clients: ${process.getActiveResourcesInfo().join(", ")}`);
      if (base.fds >= 0) assert.ok(after.fds <= base.fds + 2, `open fds grew from ${base.fds} to ${after.fds} over ${runs} clients`);
      assert.ok(after.memory <= base.memory + 16 * 2 ** 20,
        `memory grew from ${base.memory >> 10} KiB to ${after.memory >> 10} KiB over ${runs} clients`);
    } finally {
      await srv.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });
}
