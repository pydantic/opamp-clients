// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The inventory (docs/node-otel.md#inventory): what the package lists of what it built, as the
// layer's available components. The names are the ones deactivate_instrumentations takes, and
// the exporters gated exactly where the package's host declares that signal's send_*.

import assert from "node:assert/strict";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { getNodeAutoInstrumentations } from "@opentelemetry/auto-instrumentations-node";
import { SDK_INFO } from "@opentelemetry/core";
import type { Instrumentation } from "@opentelemetry/instrumentation";
import { type AvailableComponents, type ComponentDetails, RemoteConfigStatuses } from "@pydantic/opamp-client";
import { SdkSettings } from "@pydantic/opamp-client/sdk";

import { config as policyConfig, policy } from "./policy.js";
import { Agent, exporterNames, hostFromEnv, inventory } from "../src/index.js";

const LAYER = new SdkSettings();
const text = (s: string) => new TextEncoder().encode(s);
const config = (body: string) => policyConfig(body, body);
/** 2026-10-14T12:00:00Z. */
const NOW = 1_791_979_200_000;
const PACKAGE = JSON.parse(readFileSync(new URL("../../package.json", import.meta.url), "utf8")) as { name: string; version: string };

/** An instrumentation as the package sees one: its scope name and version, and a tracer provider to take. */
const instrumentation = (name: string, version?: string) =>
  ({ instrumentationName: name, instrumentationVersion: version, setTracerProvider() {} }) as unknown as Instrumentation;

/** An agent prepared on `env`, its SDK built with `instrumentations`, on the test's clock. */
function built(env: Record<string, string> = {}, instrumentations: Instrumentation[] = []): Agent {
  const agent = new Agent();
  agent.now = () => NOW;
  const all = { OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: mkdtempSync(join(tmpdir(), "opamp-otel-")), ...env };
  agent.prepare(all);
  agent.instrument(instrumentations, all);
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
    "integration.name": "@pydantic/opamp-otel",
    "integration.version": PACKAGE.version,
    "telemetry.sdk.language": "nodejs",
    "telemetry.sdk.version": SDK_INFO["telemetry.sdk.version"],
  });
  assert.equal(PACKAGE.name, "@pydantic/opamp-otel");
  assert.deepEqual(c.instrumentations, { express: "0.66.0", fs: null, http: "0.222.0" });
  // Prometheus is NodeSDK's, so the server can't stop the metrics: neither of them is gated.
  assert.deepEqual(c.exporters, {
    traces: { otlp: true },
    metrics: { otlp: false, prometheus: false },
    logs: { console: true },
  });
  // The settings are the host's: send_metrics isn't declared, so it isn't listed; the
  // instrumentations are switched while the application runs; debug mode is, on the clock given.
  assert.equal(agent.host.live?.includes("send_metrics"), false);
  assert.equal(agent.host.live?.includes("send_traces"), true);
  assert.deepEqual(c.settings, shown(LAYER.components({}, agent.host, BigInt(NOW))).settings);
  assert.equal(c.settings.send_metrics, undefined);
  assert.equal(c.settings.send_traces, "at once");
  assert.equal(c.settings.deactivate_instrumentations, "at once");
  assert.equal(c.settings.debug_until, "at once");
  assert.equal(c.settings.opamp_polling_interval, "next start");
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
  assert.equal(c.sdk["integration.name"], "@pydantic/opamp-otel");
  // With none, there is nothing to stop: the send_* switches are still carried out.
  assert.equal(c.settings.send_traces, "at once");
  // Only that signal's: the others keep their default, otlp.
  assert.deepEqual(shown(built({ OTEL_METRICS_EXPORTER: "none" }).components()).exporters,
    { traces: { otlp: true }, logs: { otlp: true } });
});

test("exporters as NodeSDK reads the variables: repeats once, none as it treats none, names it doesn't know left out", () => {
  const exporters = (env: Record<string, string>) => shown(built(env).components()).exporters;
  // Repeats once.
  assert.deepEqual(exporters({ OTEL_TRACES_EXPORTER: "console,otlp,console", OTEL_LOGS_EXPORTER: "otlp,otlp" }),
    { traces: { console: true, otlp: true }, metrics: { otlp: true }, logs: { otlp: true } });
  // none among other trace exporters is the default alone; among metric ones, none.
  assert.deepEqual(exporters({ OTEL_TRACES_EXPORTER: "zipkin,none", OTEL_METRICS_EXPORTER: "otlp,none" }),
    { traces: { otlp: true }, logs: { otlp: true } });
  // A name NodeSDK doesn't build isn't listed; the rest of that signal is NodeSDK's, ungated.
  assert.deepEqual(exporters({ OTEL_TRACES_EXPORTER: "otlp,zipkin,bogus", OTEL_LOGS_EXPORTER: "custom" }),
    { traces: { otlp: false, zipkin: false }, metrics: { otlp: true } });
  // The protocol doesn't change the name: it is otlp over grpc.
  assert.deepEqual(exporters({ OTEL_EXPORTER_OTLP_PROTOCOL: "grpc" }).traces, { otlp: true });
});

test("an exporter is gated exactly where the package's host declares its signal's send_*", () => {
  const envs: Record<string, string>[] = [
    {},
    { OTEL_TRACES_EXPORTER: "zipkin" },
    { OTEL_TRACES_EXPORTER: "otlp,zipkin" },
    { OTEL_METRICS_EXPORTER: "prometheus" },
    { OTEL_METRICS_EXPORTER: "console,prometheus" },
    { OTEL_LOGS_EXPORTER: "otlp,custom" },
    { OTEL_LOGS_EXPORTER: "console", OTEL_TRACES_EXPORTER: "console", OTEL_METRICS_EXPORTER: "console" },
    { OTEL_TRACES_EXPORTER: "none,zipkin" },
  ];
  for (const env of envs) {
    const agent = built(env);
    const c = shown(agent.components());
    const live = hostFromEnv(env).live ?? [];
    assert.deepEqual(agent.host.live, live);
    for (const signal of ["traces", "metrics", "logs"]) {
      const off = !live.includes(`send_${signal}`);
      for (const [name, gated] of Object.entries(c.exporters[signal] ?? {})) {
        assert.equal(gated, !off, `${JSON.stringify(env)}: ${signal}/${name}`);
      }
      // And the setting is listed exactly where it is declared.
      assert.equal(c.settings[`send_${signal}`] === undefined, off, `${JSON.stringify(env)}: send_${signal}`);
      const named = exporterNames(signal.toUpperCase() as "TRACES", env);
      assert.ok(Object.keys(c.exporters[signal] ?? {}).every((n) => named.includes(n)), `${JSON.stringify(env)}: ${signal}`);
    }
  }
  // Where the package builds no log exporter, the log settings it can't carry out are left out too.
  const c = shown(built({ OTEL_LOGS_EXPORTER: "custom" }).components());
  for (const key of ["send_logs", "send_logs_level", "send_logs_level_by_scope", "send_logs_per_second", "drop_attributes"]) {
    assert.equal(c.settings[key], undefined, key);
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
  assert.deepEqual(inventory([], { OTEL_TRACES_EXPORTER: "none", OTEL_METRICS_EXPORTER: "none", OTEL_LOGS_EXPORTER: "none" }), {
    integration: "@pydantic/opamp-otel",
    integrationVersion: PACKAGE.version,
    sdkLanguage: "nodejs",
    sdkVersion: SDK_INFO["telemetry.sdk.version"],
    instrumentations: [],
    exporters: [],
  });
});
