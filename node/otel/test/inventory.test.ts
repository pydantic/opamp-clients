// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The inventory (docs/node-otel.md#inventory): what the package lists of what it built, as the
// layer's available components. The names are the ones deactivate_instrumentations takes; every
// exporter on the SDK's providers is gated, one the gates can't reach listed ungoverned.

import assert from "node:assert/strict";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, test } from "node:test";

import { getNodeAutoInstrumentations } from "@opentelemetry/auto-instrumentations-node";
import { SDK_INFO } from "@opentelemetry/core";
import type { Instrumentation } from "@opentelemetry/instrumentation";
import { type AvailableComponents, type ComponentDetails, RemoteConfigStatuses } from "@pydantic/logfire-fleet-client";
import { SdkSettings } from "@pydantic/logfire-fleet-client/sdk";

import { config as policyConfig, policy } from "./policy.js";
import { resourceFromAttributes } from "@opentelemetry/resources";
import type { NodeSDK } from "@opentelemetry/sdk-node";

import { Agent, hostFromEnv, inventory, managedSdk } from "../src/index.js";

const LAYER = new SdkSettings();
const text = (s: string) => new TextEncoder().encode(s);
const config = (body: string) => policyConfig(body, body);
/** 2026-10-14T12:00:00Z. */
const NOW = 1_791_979_200_000;
const PACKAGE = JSON.parse(readFileSync(new URL("../../package.json", import.meta.url), "utf8")) as { name: string; version: string };

/** An instrumentation as the package sees one: its scope name and version, and a tracer provider to take. */
const instrumentation = (name: string, version?: string) =>
  ({ instrumentationName: name, instrumentationVersion: version, setTracerProvider() {} }) as unknown as Instrumentation;

const started: NodeSDK[] = [];
after(async () => {
  for (const sdk of started) await sdk.shutdown();
});

/**
 * An agent prepared on `env`, its SDK built and started as the register builds it (NodeSDK reads
 * the variables from the process for what it builds itself), governed, with `instrumentations`.
 */
function built(env: Record<string, string> = {}, instrumentations: Instrumentation[] = []): Agent {
  const agent = new Agent();
  agent.now = () => NOW;
  const all = { OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: mkdtempSync(join(tmpdir(), "opamp-otel-")),
    OTEL_EXPORTER_PROMETHEUS_PORT: "0", ...env };
  agent.prepare(all);
  const was = { ...process.env };
  Object.assign(process.env, all);
  let sdk: NodeSDK;
  try {
    sdk = managedSdk(agent.controls, all, [], resourceFromAttributes({}));
    sdk.start();
  } finally {
    for (const k of Object.keys(all)) if (!(k in was)) delete process.env[k];
    Object.assign(process.env, was);
  }
  started.push(sdk);
  const providers = sdk as unknown as Record<"_tracerProvider" | "_meterProvider" | "_loggerProvider", unknown>;
  agent.govern({ traces: providers._tracerProvider, metrics: providers._meterProvider, logs: providers._loggerProvider });
  agent.instrument(instrumentations);
  return agent;
}

/** The components as plain values: sdk (key to text), instrumentations (name to version or null),
 * exporters (signal to name to gated) and settings (key to applies). */
function shown(c: AvailableComponents | undefined) {
  assert.ok(c);
  const meta = (d: ComponentDetails, key: string) => d.metadata.find((kv) => kv.key === key)?.value?.value.value ?? null;
  const subs = <T>(d: ComponentDetails | undefined, f: (d: ComponentDetails) => T): Record<string, T> =>
    Object.fromEntries(Object.entries(d?.subComponentMap ?? {}).map(([name, s]) => [name, f(s)]));
  return {
    hash: c.hash,
    sdk: Object.fromEntries(c.components.sdk!.metadata.map((kv) => [kv.key, kv.value?.value.value])),
    instrumentations: subs(c.components.instrumentations, (d) => meta(d, "version")),
    exporters: subs(c.components.exporters, (s) => subs(s, (e) => meta(e, "gated"))),
    settings: subs(c.components.settings, (d) => meta(d, "applies")),
  };
}

test("a typical setup: the package, the SDK, each instrumentation by its short name, each exporter gated or not", () => {
  const agent = built({ OTEL_METRICS_EXPORTER: "otlp,prometheus", OTEL_LOGS_EXPORTER: "console" }, [
    instrumentation("@opentelemetry/instrumentation-http", "0.222.0"),
    instrumentation("@opentelemetry/instrumentation-express", "0.66.0"),
    instrumentation("@opentelemetry/instrumentation-fs"), // no version known
  ]);
  const c = shown(agent.components());
  assert.equal(c.hash.length, 0); // the client computes it
  assert.deepEqual(c.sdk, {
    "integration.name": "@pydantic/logfire-fleet-otel",
    "integration.version": PACKAGE.version,
    "telemetry.sdk.language": "nodejs",
    "telemetry.sdk.version": SDK_INFO["telemetry.sdk.version"],
  });
  assert.equal(PACKAGE.name, "@pydantic/logfire-fleet-otel");
  assert.deepEqual(c.instrumentations, { express: "0.66.0", fs: null, http: "0.222.0" });
  // NodeSDK builds the metrics beside prometheus: the OTLP exporter still gets its gate; the
  // Prometheus reader is pulled, so it can't, and is listed ungoverned.
  assert.deepEqual(c.exporters, {
    traces: { "otlp@localhost:4318": true },
    metrics: { "otlp@localhost:4318": true, prometheus: false },
    logs: { console: true },
  });
  // The settings are the host's, every one of them: the instrumentations are switched while the
  // application runs; debug mode is, on the clock given.
  assert.deepEqual(agent.host.live, hostFromEnv().live);
  assert.deepEqual(c.settings, shown(LAYER.components({}, agent.host, BigInt(NOW))).settings);
  assert.equal(c.settings.send_metrics, "at once");
  assert.equal(c.settings.send_traces, "at once");
  assert.equal(c.settings.deactivate_instrumentations, "at once");
  assert.equal(c.settings.debug_until, "at once");
});

test("every instrumentation getNodeAutoInstrumentations loads is listed, with its version, and switched off by that name", () => {
  const all = getNodeAutoInstrumentations();
  for (const i of all) i.disable();
  const agent = built({}, all);
  const listed = shown(agent.components()).instrumentations;
  assert.equal(Object.keys(listed).length, all.length);
  assert.ok(all.length > 30);
  for (const i of all) {
    const [name, version] = Object.entries(listed).find(([n]) => `@opentelemetry/instrumentation-${n}` === i.instrumentationName)!;
    assert.equal(version, i.instrumentationVersion, name);
    // The name listed is the one the server switches it by.
    const s = agent.remoteConfig(config(policy({ instrumentations: { disabled: [name] } })));
    assert.equal(s.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
    assert.equal(agent.controls.off(i.instrumentationName), true, name);
    assert.equal(all.filter((j) => agent.controls.off(j.instrumentationName)).length, 1, name);
  }
});

test("no instrumentations, and every exporter set to none: the sdk and settings only", () => {
  const agent = built({ OTEL_TRACES_EXPORTER: "none", OTEL_METRICS_EXPORTER: "none", OTEL_LOGS_EXPORTER: "none" });
  const c = shown(agent.components());
  assert.deepEqual([c.instrumentations, c.exporters], [{}, {}]);
  assert.equal(c.sdk["integration.name"], "@pydantic/logfire-fleet-otel");
  // With none, there is nothing to stop: the send_* switches are still carried out.
  assert.equal(c.settings.send_traces, "at once");
  // Only that signal's: the others keep their default, otlp.
  assert.deepEqual(shown(built({ OTEL_METRICS_EXPORTER: "none" }).components()).exporters,
    { traces: { "otlp@localhost:4318": true }, logs: { "otlp@localhost:4318": true } });
});

test("exporters as NodeSDK reads the variables: repeats once, none as it treats none, each gated where it sends", () => {
  const exporters = (env: Record<string, string>) => shown(built(env).components()).exporters;
  const otlp = "otlp@localhost:4318";
  // Repeats once.
  assert.deepEqual(exporters({ OTEL_TRACES_EXPORTER: "console,otlp,console", OTEL_LOGS_EXPORTER: "otlp,otlp" }),
    { traces: { console: true, [otlp]: true }, metrics: { [otlp]: true }, logs: { [otlp]: true } });
  // none among other trace exporters is the default alone; among metric ones, none.
  assert.deepEqual(exporters({ OTEL_TRACES_EXPORTER: "zipkin,none", OTEL_METRICS_EXPORTER: "otlp,none" }),
    { traces: { [otlp]: true }, logs: { [otlp]: true } });
  // What NodeSDK builds itself (zipkin) gets the gate as well; a name it doesn't know builds nothing.
  assert.deepEqual(exporters({ OTEL_TRACES_EXPORTER: "otlp,zipkin,bogus", OTEL_LOGS_EXPORTER: "custom" }),
    { traces: { [otlp]: true, "zipkin@localhost:9411": true }, metrics: { [otlp]: true } });
  // The protocol doesn't change the name: it is otlp over grpc, to its own port.
  assert.deepEqual(exporters({ OTEL_EXPORTER_OTLP_PROTOCOL: "grpc" }).traces, { "otlp@localhost:4317": true });
  // Where it sends is the variables', credentials and path left out.
  assert.deepEqual(exporters({ OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: "https://u:k@api.honeycomb.io/v1/traces" }).traces,
    { "otlp@api.honeycomb.io:443": true });
});

test("every setting is declared, whatever the exporters: the policy is in force on every one it reaches", () => {
  const envs: Record<string, string>[] = [
    {},
    { OTEL_TRACES_EXPORTER: "zipkin" },
    { OTEL_METRICS_EXPORTER: "prometheus" },
    { OTEL_LOGS_EXPORTER: "otlp,custom" },
    { OTEL_LOGS_EXPORTER: "console", OTEL_TRACES_EXPORTER: "console", OTEL_METRICS_EXPORTER: "console" },
  ];
  for (const env of envs) {
    const agent = built(env);
    assert.deepEqual(agent.host.live, hostFromEnv().live, JSON.stringify(env));
    const s = agent.remoteConfig(config(policy({ signal_exports: { traces: { enabled: false }, metrics: { enabled: false },
      logs: { enabled: false } } })));
    assert.equal(s.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, JSON.stringify(env));
  }
});

test("an instrumentation given twice is listed once; one outside upstream's names has no name to list", () => {
  const c = shown(built({}, [
    instrumentation("@opentelemetry/instrumentation-http", "0.222.0"),
    instrumentation("@opentelemetry/instrumentation-http", "0.222.0"),
    instrumentation("@opentelemetry/instrumentation-pg", "0.60.0"),
    instrumentation("@opentelemetry/instrumentation-pg", "0.61.0"),
    instrumentation("my-own-instrumentation", "1.0.0"),
  ]).components());
  assert.deepEqual(c.instrumentations, { http: "0.222.0", pg: "0.60.0,0.61.0" });
  // As the instrumentations control has it: no name switches one outside upstream's names.
  const agent = built({}, [instrumentation("my-own-instrumentation")]);
  agent.remoteConfig(config(policy({ instrumentations: { disabled: ["my-own-instrumentation"] } })));
  assert.equal(agent.controls.off("my-own-instrumentation"), false);
  agent.remoteConfig(config(policy({ instrumentations: { all_disabled: true } })));
  assert.equal(agent.controls.off("my-own-instrumentation"), true);
});

test("the same setup in another order is the same components", () => {
  const a = [instrumentation("@opentelemetry/instrumentation-http", "1"), instrumentation("@opentelemetry/instrumentation-dns", "2")];
  const one = built({ OTEL_TRACES_EXPORTER: "console,otlp" }, a).components()!;
  const two = built({ OTEL_TRACES_EXPORTER: "otlp,console" }, [...a].reverse()).components()!;
  assert.deepEqual(shown(one), shown(two));
});

test("before the SDK is built, or with the package off, there is nothing to report", () => {
  const agent = new Agent();
  agent.prepare({ OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp" });
  assert.equal(agent.components(), undefined);
  const off = new Agent();
  off.prepare({});
  off.instrument([]);
  assert.equal(off.components(), undefined);
  // The inventory itself needs no agent.
  assert.deepEqual(inventory([]), {
    integration: "@pydantic/logfire-fleet-otel",
    integrationVersion: PACKAGE.version,
    sdkLanguage: "nodejs",
    sdkVersion: SDK_INFO["telemetry.sdk.version"],
    instrumentations: [],
    exporters: [],
  });
});
