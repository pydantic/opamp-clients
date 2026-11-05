// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// logfireFleet() as the Logfire SDK's configure({ fleet }) calls it, in a real process: the program
// below builds its NodeSDK the way the Logfire SDK does, and loads the package with require(), as
// the Logfire SDK's synchronous configure() must.

import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { beforeEach, test } from "node:test";
import { fileURLToPath } from "node:url";

import {
  AgentRemoteConfigSchema,
  AgentToServerSchema,
  create,
  fromBinary,
  RemoteConfigStatuses,
  ServerToAgentSchema,
  toBinary,
} from "@pydantic/logfire-fleet-client";

import { FILE, policy } from "./policy.js";

const INDEX = fileURLToPath(new URL("../src/index.js", import.meta.url));

beforeEach(() => {
  const home = mkdtempSync(join(tmpdir(), "opamp-otel-home-"));
  process.env.XDG_STATE_HOME = home;
  process.env.HOME = home;
});

async function body(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return new Uint8Array(Buffer.concat(chunks));
}

/** An OpAMP server that offers `offer` in its first answer: the statuses and instance UIDs it hears. */
async function configServer(offer: string) {
  const seen = { polls: 0, statuses: [] as { status: number; error: string }[], uids: new Set<string>() };
  const s = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    seen.polls++;
    seen.uids.add(Buffer.from(msg.instanceUid).toString("hex"));
    if (msg.remoteConfigStatus) {
      seen.statuses.push({ status: msg.remoteConfigStatus.status, error: msg.remoteConfigStatus.errorMessage });
    }
    const reply = create(ServerToAgentSchema, { instanceUid: msg.instanceUid });
    if (seen.polls === 1) {
      const b = new TextEncoder().encode(offer);
      reply.remoteConfig = create(AgentRemoteConfigSchema, {
        configHash: b, config: { configMap: { [FILE]: { body: b, contentType: "application/json" } } },
      });
    }
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, reply));
  });
  await new Promise<void>((r) => s.listen(0, "127.0.0.1", r));
  const applied = () => seen.statuses.some((x) => x.status === RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  return { seen, applied, endpoint: `http://127.0.0.1:${(s.address() as AddressInfo).port}/v1/opamp`, close: () => s.close() };
}

/**
 * A program that sets the SDK up as the Logfire SDK does under fleet (its head sampler and resource
 * through the hook, its NodeSDK started by the hook), `times` times over, as configure() called
 * again; once `ready` it makes `spans` and exits. Its output.
 */
async function app(endpoint: string, spans: string, ready: () => boolean, times = 1) {
  const program = `
    import { createRequire } from "node:module";
    import { NodeSDK } from "@opentelemetry/sdk-node";
    import { ConsoleSpanExporter, SimpleSpanProcessor } from "@opentelemetry/sdk-trace-base";
    import { resourceFromAttributes } from "@opentelemetry/resources";
    import { trace, metrics, context, propagation } from "@opentelemetry/api";
    import { logs } from "@opentelemetry/api-logs";
    const require = createRequire(import.meta.url);
    const { logfireFleet, LOGFIRE_FLEET_CONTRACT } = require(${JSON.stringify(INDEX)});
    let sdk, fleet;
    for (let i = 0; i < ${times}; i++) {
      if (sdk) {
        // As the Logfire SDK's configure() again: the old SDK's globals are let go first.
        trace.disable(); metrics.disable(); propagation.disable(); context.disable(); logs.disable();
        await sdk.shutdown();
      }
      fleet = logfireFleet({ endpoint: ${JSON.stringify(endpoint)}, heartbeatIntervalMs: 100, stateDir: "none" });
      if (fleet.contract !== LOGFIRE_FLEET_CONTRACT) throw new Error("contract");
      const resource = resourceFromAttributes({ "service.name": "shop", "telemetry.sdk.name": "logfire",
        ...fleet.resourceAttributes });
      sdk = new NodeSDK({ resource, autoDetectResources: false, instrumentations: [], sampler: fleet.headSampler(undefined),
        spanProcessors: [new SimpleSpanProcessor(new ConsoleSpanExporter())] });
      fleet.start(() => sdk.start(), resource);
      console.log("instance", fleet.resourceAttributes["service.instance.id"]);
    }
    const tracer = trace.getTracer("logfire");
    process.stdin.once("data", async () => {
      ${spans}
      await fleet.shutdown();
      await sdk.shutdown();
      process.exit(0);
    });`;
  const child = spawn(process.execPath, ["--input-type=module", "-e", program], {
    cwd: new URL("..", import.meta.url).pathname,
    env: { ...Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("OTEL_"))) },
  });
  let out = "";
  child.stdout.on("data", (d: Buffer) => (out += d.toString()));
  child.stderr.on("data", (d: Buffer) => (out += d.toString()));
  const exited = new Promise<number | null>((r) => child.once("exit", (c) => r(c)));
  const deadline = Date.now() + 15_000;
  while (!ready() && child.exitCode === null && Date.now() < deadline) await new Promise((r) => setTimeout(r, 20));
  child.stdin.write("go\n");
  const timer = setTimeout(() => child.kill("SIGKILL"), 15_000);
  const code = await exited;
  clearTimeout(timer);
  return { code, out };
}

test("the Logfire SDK under fleet: its exporters get the gates, and its instance is the server's", async () => {
  const s = await configServer(policy({ attribute_redaction: { drop: ["x"] } }));
  try {
    const { code, out } = await app(s.endpoint, `tracer.startSpan("after", { attributes: { x: "secret", y: "kept" } }).end();`,
      s.applied);
    assert.equal(code, 0, out);
    assert.ok(s.applied(), out);
    assert.match(out, /name: 'after'/);
    assert.match(out, /y: 'kept'/);
    assert.doesNotMatch(out, /secret/);
    const uid = /instance (\S+)/.exec(out)?.[1] ?? "";
    assert.ok(uid.length > 0, out);
    assert.deepEqual([...s.seen.uids], [uid.replaceAll("-", "")], out);
    assert.match(out, new RegExp(`'service.instance.id': '${uid}'`), out);
  } finally {
    s.close();
  }
});

test("the Logfire SDK under fleet samples as the server says: its head sampler is the live one", async () => {
  const s = await configServer(policy({ head_sampling: { ratio: 0 } }));
  try {
    const { code, out } = await app(s.endpoint, `tracer.startSpan("after").end();`, s.applied);
    assert.equal(code, 0, out);
    assert.ok(s.applied(), out);
    assert.doesNotMatch(out, /name: 'after'/);
  } finally {
    s.close();
  }
});

test("configure() again under fleet: the new SDK's exporters are governed, under the same client and instance", async () => {
  const s = await configServer(policy({ attribute_redaction: { drop: ["x"] } }));
  try {
    const { code, out } = await app(s.endpoint, `tracer.startSpan("after", { attributes: { x: "secret", y: "kept" } }).end();`,
      s.applied, 2);
    assert.equal(code, 0, out);
    assert.ok(s.applied(), out);
    assert.match(out, /y: 'kept'/);
    assert.doesNotMatch(out, /secret/);
    assert.doesNotMatch(out, /of its own/, out);
    assert.equal(s.seen.uids.size, 1, out);
  } finally {
    s.close();
  }
});
