// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// manage() in a real process: an application that set its SDK up itself (its own NodeSDK, as
// contrib's docs show, or the Logfire SDK's) keeps it, and puts it under the server. The policy
// reaches that SDK's exporters; the sampler stays the application's, so a policy that overrides it
// is refused.

import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { beforeEach, test } from "node:test";

import {
  AgentRemoteConfigSchema,
  AgentToServerSchema,
  create,
  fromBinary,
  RemoteConfigStatuses,
  ServerToAgentSchema,
  toBinary,
} from "@pydantic/opamp-client";

import { FILE, policy } from "./policy.js";

const INDEX = new URL("../src/index.js", import.meta.url).href;

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

/** An OpAMP server that offers `offer` in its first answer: the statuses and descriptions it hears. */
async function configServer(offer: string) {
  const seen = { polls: 0, statuses: [] as { status: number; error: string }[], serviceNames: [] as string[] };
  const s = createServer(async (req, res) => {
    const msg = fromBinary(AgentToServerSchema, await body(req));
    seen.polls++;
    if (msg.remoteConfigStatus) {
      seen.statuses.push({ status: msg.remoteConfigStatus.status, error: msg.remoteConfigStatus.errorMessage });
    }
    for (const kv of msg.agentDescription?.identifyingAttributes ?? []) {
      if (kv.key === "service.name" && kv.value?.value.case === "stringValue") seen.serviceNames.push(kv.value.value.value);
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
  const failed = () => seen.statuses.find((x) => x.status === RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  return { seen, applied, failed, endpoint: `http://127.0.0.1:${(s.address() as AddressInfo).port}/v1/opamp`, close: () => s.close() };
}

/**
 * Run an ES module that sets up its own NodeSDK (`processor`: a console span exporter's, `resource`
 * its attributes), then calls manage() with `endpoint`; once `ready` (polled from the parent) it
 * makes `spans` and exits. Its output.
 */
async function app(endpoint: string, resource: Record<string, string>, spans: string, ready: () => boolean,
  processor = "new SimpleSpanProcessor(new ConsoleSpanExporter())") {
  const program = `
    import { NodeSDK } from "@opentelemetry/sdk-node";
    import { ConsoleSpanExporter, SimpleSpanProcessor } from "@opentelemetry/sdk-trace-base";
    import { resourceFromAttributes } from "@opentelemetry/resources";
    import { trace } from "@opentelemetry/api";
    import { manage } from ${JSON.stringify(INDEX)};
    const sdk = new NodeSDK({ resource: resourceFromAttributes(${JSON.stringify(resource)}), autoDetectResources: false,
      spanProcessors: [${processor}], instrumentations: [] });
    sdk.start();
    const { shutdown } = manage({ endpoint: ${JSON.stringify(endpoint)}, heartbeatIntervalMs: 100, stateDir: "none" });
    const tracer = trace.getTracer("logfire");
    process.stdin.once("data", async () => {
      ${spans}
      await shutdown();
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

test("the application's own NodeSDK: its exporter gets the gates, and it is described by its own resource", async () => {
  const s = await configServer(policy({ attribute_redaction: { drop: ["x"] } }));
  try {
    const { code, out } = await app(s.endpoint, { "service.name": "mine" },
      `tracer.startSpan("after", { attributes: { x: "secret", y: "kept" } }).end();`, s.applied);
    assert.equal(code, 0, out);
    assert.ok(s.applied(), out);
    assert.match(out, /name: 'after'/);
    assert.match(out, /y: 'kept'/);
    assert.doesNotMatch(out, /secret/);
    assert.match(out, /manage\(\) keeps the application's own sampler/);
    assert.ok(s.seen.serviceNames.includes("mine"), String(s.seen.serviceNames));
  } finally {
    s.close();
  }
});

test("a policy that overrides the application's sampler is refused, naming it", async () => {
  const s = await configServer(policy({ head_sampling: { ratio: 0.5 } }));
  try {
    const { code, out } = await app(s.endpoint, { "service.name": "mine" }, "", () => s.failed() !== undefined);
    assert.equal(code, 0, out);
    assert.match(s.failed()?.error ?? "", /head_sampling/, out);
    assert.ok(!s.applied(), out);
  } finally {
    s.close();
  }
});

test("the Logfire SDK's logs are spans: the log level reaches them, and its spans are left be", async () => {
  const s = await configServer(policy({ log_levels: { level: "warn" } }));
  try {
    const log = (name: string, level: number) =>
      `tracer.startSpan(${JSON.stringify(name)}, { attributes: { "logfire.span_type": "log", "logfire.level_num": ${level} } }).end();`;
    const { code, out } = await app(s.endpoint, { "service.name": "shop", "telemetry.sdk.name": "logfire" },
      `tracer.startSpan("order", { attributes: { "logfire.span_type": "span" } }).end(); ${log("order paid", 9)} ${log("order slow", 13)}`,
      s.applied);
    assert.equal(code, 0, out);
    assert.ok(s.applied(), out);
    assert.match(out, /name: 'order'/);
    assert.match(out, /name: 'order slow'/);
    assert.doesNotMatch(out, /name: 'order paid'/);
  } finally {
    s.close();
  }
});

test("a processor that wraps the one holding the exporter (as the Logfire SDK's does) is looked through", async () => {
  const s = await configServer(policy({ attribute_redaction: { drop: ["x"] } }));
  // The Logfire SDK's processor holds its batch processor as `wrapped`, and calls it.
  const wrapper = `{ wrapped: new SimpleSpanProcessor(new ConsoleSpanExporter()),
    onStart(s, c) { this.wrapped.onStart(s, c); }, onEnd(s) { this.wrapped.onEnd(s); },
    shutdown() { return this.wrapped.shutdown(); }, forceFlush() { return this.wrapped.forceFlush(); } }`;
  try {
    const { code, out } = await app(s.endpoint, { "service.name": "mine" },
      `tracer.startSpan("after", { attributes: { x: "secret", y: "kept" } }).end();`, s.applied, wrapper);
    assert.equal(code, 0, out);
    assert.ok(s.applied(), out);
    assert.match(out, /y: 'kept'/);
    assert.doesNotMatch(out, /secret/);
  } finally {
    s.close();
  }
});
