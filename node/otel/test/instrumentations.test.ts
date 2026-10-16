// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Upstream's instrumentations, switched while they run: http and undici (fetch) as
// getNodeAutoInstrumentations loads them, in this process, on an SDK tracer provider, switched by
// the settings layer's decisions on remote configs.

import assert from "node:assert/strict";
import { createRequire } from "node:module";
import type { AddressInfo } from "node:net";
import { after, test } from "node:test";

import { trace } from "@opentelemetry/api";
import { getNodeAutoInstrumentations } from "@opentelemetry/auto-instrumentations-node";
import { node } from "@opentelemetry/sdk-node";
import { InMemorySpanExporter, type ReadableSpan, SimpleSpanProcessor } from "@opentelemetry/sdk-trace-base";
import { RemoteConfigStatuses } from "@pydantic/opamp-client";

import { config, policy } from "./policy.js";
import { Agent } from "../src/index.js";

// As the register: the instrumentations the variables load, then the SDK, then the server's.
process.env.OTEL_NODE_ENABLED_INSTRUMENTATIONS = "http,undici";
const instrumentations = getNodeAutoInstrumentations();
const spans = new InMemorySpanExporter();
const provider = new node.NodeTracerProvider({ spanProcessors: [new SimpleSpanProcessor(spans)] });
provider.register();
const agent = new Agent();
agent.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp" });
agent.instrument(instrumentations);

function apply(settings: string): void {
  assert.equal(agent.remoteConfig(config(settings, settings)).status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
}

// Required after the instrumentations are on, as an application under the register requires it.
const http = createRequire(import.meta.url)("node:http") as typeof import("node:http");
const app = trace.getTracer("checkout");

// The application: each request handled in a span of its own.
const server = http.createServer((_req, res) => app.startActiveSpan("handle", (span) => {
  span.end();
  res.end("ok");
}));
await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/ping`;
after(() => {
  server.closeAllConnections();
  server.close();
});

function get(): Promise<void> {
  return new Promise((resolve, reject) => {
    http.get(url, (res) => res.resume().on("end", resolve)).on("error", reject);
  });
}

/** The spans `how` makes, every one ended. */
async function request(how: () => Promise<void>): Promise<ReadableSpan[]> {
  spans.reset();
  await how();
  await new Promise((r) => setTimeout(r, 50)); // the server's span ends as its response is sent
  await provider.forceFlush();
  return spans.getFinishedSpans();
}

const scopes = (s: ReadableSpan[]) => s.map((x) => x.instrumentationScope.name).sort();
const HTTP = "@opentelemetry/instrumentation-http";
const UNDICI = "@opentelemetry/instrumentation-undici";

test("http's spans stop when it is switched off and come back when it is switched on", async () => {
  // Client, server, and the application's own.
  assert.deepEqual(scopes(await request(get)), [HTTP, HTTP, "checkout"]);
  apply(policy({ instrumentations: { disabled: ["http"] } }));
  assert.deepEqual(scopes(await request(get)), ["checkout"]);
  apply(policy());
  assert.deepEqual(scopes(await request(get)), [HTTP, HTTP, "checkout"]);
  apply(policy({ instrumentations: { all_disabled: true } }));
  assert.deepEqual(scopes(await request(get)), ["checkout"]);
  apply(policy({ instrumentations: { all_disabled: false } }));
  assert.deepEqual(scopes(await request(get)), [HTTP, HTTP, "checkout"]);
});

test("the trace goes on through the instrumentations still on", async () => {
  apply(policy({ instrumentations: { disabled: ["http"] } }));
  try {
    // fetch (undici, on) calls the server (http, off): the request's span is the handler's parent.
    const made = await request(() => app.startActiveSpan("checkout", async (span) => {
      await (await fetch(url)).text();
      span.end();
    }));
    const by = (scope: string, name?: string) => {
      const found = made.filter((s) => s.instrumentationScope.name === scope && (!name || s.name === name));
      assert.equal(found.length, 1, `${scope} ${name ?? ""}: ${made.map((s) => s.name).join(", ")}`);
      return found[0]!;
    };
    const root = by("checkout", "checkout");
    const client = by(UNDICI);
    const handled = by("checkout", "handle");
    assert.deepEqual(scopes(made), [UNDICI, "checkout", "checkout"]);
    assert.equal(client.parentSpanContext?.spanId, root.spanContext().spanId);
    assert.equal(handled.parentSpanContext?.spanId, client.spanContext().spanId);
    assert.equal(new Set(made.map((s) => s.spanContext().traceId)).size, 1);
  } finally {
    apply(policy());
  }
});
