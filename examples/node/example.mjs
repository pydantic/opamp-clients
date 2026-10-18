// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

import { randomUUID } from "node:crypto";

import { trace } from "@opentelemetry/api";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-proto";
import { defaultResource, resourceFromAttributes } from "@opentelemetry/resources";
import { BatchSpanProcessor, ParentBasedSampler, TraceIdRatioBasedSampler } from "@opentelemetry/sdk-trace-base";
import { NodeTracerProvider } from "@opentelemetry/sdk-trace-node";
import {
  AgentCapabilities,
  AgentConfigMapSchema,
  AgentDescriptionSchema,
  Client,
  ComponentHealthSchema,
  ConnectionSettingsStatusSchema,
  ConnectionSettingsStatuses,
  EffectiveConfigSchema,
  KeyValueSchema,
  RemoteConfigStatusSchema,
  RemoteConfigStatuses,
  create,
} from "@pydantic/opamp-client";

const IDENTIFYING = new Set(["service.name", "service.namespace", "service.instance.id"]);

/** A sampler the server can change while the program runs. */
class LiveSampler {
  constructor(ratio) {
    this.current = new TraceIdRatioBasedSampler(ratio);
  }
  shouldSample(...args) {
    return this.current.shouldSample(...args);
  }
  toString() {
    return this.current.toString();
  }
}

/** An exporter the server can point elsewhere while the program runs. Until it says where, spans
 * go nowhere. */
class LiveExporter {
  current = undefined;
  /** Point it at `to`, and shut down the exporter it replaces. */
  set(to) {
    const old = this.current;
    this.current = to;
    old?.shutdown();
  }
  export(spans, done) {
    if (this.current) this.current.export(spans, done);
    else done({ code: 0 });
  }
  shutdown() {
    return this.current?.shutdown() ?? Promise.resolve();
  }
  forceFlush() {
    return this.current?.forceFlush?.() ?? Promise.resolve();
  }
}

/** An OTLP/HTTP exporter for the traces part of telemetry settings: the endpoint is the full URL
 * (.../v1/traces), with the headers offered. */
function exporter(settings) {
  const headers = Object.fromEntries((settings.headers?.headers ?? []).map((h) => [h.key, h.value]));
  return new OTLPTraceExporter({ url: settings.destinationEndpoint, headers });
}

/** The agent description, from the SDK's resource. */
function description(resource) {
  const d = create(AgentDescriptionSchema);
  for (const [key, value] of Object.entries(resource.attributes)) {
    const kv = create(KeyValueSchema, { key, value: { value: { case: "stringValue", value: String(value) } } });
    (IDENTIFYING.has(key) ? d.identifyingAttributes : d.nonIdentifyingAttributes).push(kv);
  }
  return d;
}

function effective(settings) {
  const body = new TextEncoder().encode(JSON.stringify(settings));
  return create(EffectiveConfigSchema, {
    configMap: create(AgentConfigMapSchema, { configMap: { "": { body, contentType: "application/json" } } }),
  });
}

// One UUID is both the OpAMP instance UID and service.instance.id, so the fleet view and the
// telemetry name the same instance.
const uid = randomUUID();
const resource = defaultResource().merge(
  resourceFromAttributes({ "service.name": "checkout", "service.instance.id": uid }),
);
const sampler = new LiveSampler(1);
// Spans go wherever the server says (telemetry settings), through a live exporter.
const live = new LiveExporter();
const provider = new NodeTracerProvider({
  resource,
  sampler: new ParentBasedSampler({ root: sampler }),
  spanProcessors: [new BatchSpanProcessor(live, { scheduledDelayMillis: 500 })],
});
provider.register();
const tracer = trace.getTracer("checkout");

const client = Client.start(
  {
    endpoint: process.env.OPAMP_ENDPOINT, // https://.../v1/opamp, or wss://... for WebSocket
    instanceUid: Uint8Array.from(Buffer.from(uid.replaceAll("-", ""), "hex")),
    capabilities: BigInt(
      AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig | AgentCapabilities.AgentCapabilities_ReportsOwnTraces,
    ),
    // headers: { Authorization: `Bearer ${token}` },
  },
  {
    description: description(resource),
    health: create(ComponentHealthSchema, { healthy: true }),
    effectiveConfig: effective({ sampling_ratio: 1 }),
  },
  (n) => {
    if (n.kind === "stopped" && n.error !== undefined) {
      // The client stopped on its own (a bug): don't carry on unmanaged.
      console.error("OpAMP client stopped:", n.error);
      process.exit(1);
    }
    if (n.kind === "connectionSettings") {
      // Where to send our own telemetry: all the settings, not only what changed.
      const status = create(ConnectionSettingsStatusSchema, {
        lastConnectionSettingsHash: n.connectionSettings.hash,
        status: ConnectionSettingsStatuses.ConnectionSettingsStatuses_APPLIED,
      });
      if (n.connectionSettings.ownTraces) {
        try {
          live.set(exporter(n.connectionSettings.ownTraces));
        } catch (err) {
          status.status = ConnectionSettingsStatuses.ConnectionSettingsStatuses_FAILED;
          status.errorMessage = String(err);
        }
      }
      client.setConnectionSettingsStatus(status);
      return;
    }
    if (n.kind !== "remoteConfig") return;
    const status = create(RemoteConfigStatusSchema, { lastRemoteConfigHash: n.remoteConfig.configHash });
    let ratio;
    try {
      const body = n.remoteConfig.config?.configMap[""]?.body ?? new Uint8Array();
      ratio = JSON.parse(new TextDecoder().decode(body)).sampling_ratio;
    } catch {
      ratio = undefined;
    }
    if (typeof ratio === "number" && ratio >= 0 && ratio <= 1) {
      sampler.current = new TraceIdRatioBasedSampler(ratio);
      status.status = RemoteConfigStatuses.RemoteConfigStatuses_APPLIED;
      client.setEffectiveConfig(effective({ sampling_ratio: ratio }));
    } else {
      status.status = RemoteConfigStatuses.RemoteConfigStatuses_FAILED;
      status.errorMessage = 'want {"sampling_ratio": 0..1}';
    }
    client.setRemoteConfigStatus(status);
  },
);

// The program runs until interrupted. Then the server hears the agent go (agent_disconnect), and
// the SDK flushes.
// The application's work: a span every 100 ms.
const work = setInterval(() => tracer.startSpan("checkout").end(), 100);

process.once("SIGINT", async () => {
  clearInterval(work);
  await client.stop();
  await provider.shutdown();
});
