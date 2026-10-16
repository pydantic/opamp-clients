// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The SDK settings layer through the module: what the Rust tests hold it to, reached from Node.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import { create } from "@bufbuild/protobuf";

import {
  AgentConfigObjectSchema,
  type AgentDescription,
  AgentRemoteConfigSchema,
  type AvailableComponents,
  type ComponentDetails,
  KeyValueSchema,
  RemoteConfigStatuses,
} from "../src/index.js";
import { type HostInit, type InventoryInit, SdkSettings, SpanKind } from "../src/sdk.js";
import { config, off, overridden, policy, reported, V1, V2 } from "./policy.js";

const text = (s: string) => new TextEncoder().encode(s);
const APPLIED = RemoteConfigStatuses.RemoteConfigStatuses_APPLIED;
const FAILED = RemoteConfigStatuses.RemoteConfigStatuses_FAILED;
const none = new Uint8Array();

// A host that declares every setting: each live one at once, each next-start one at the next start.
const declaring = {
  live: [
    "custom",
    "debug_sampling_rate",
    "debug_send_logs_level",
    "debug_until",
    "drop_attributes",
    "mask_attributes",
    "sampling_rate",
    "sampling_rules",
    "sampling_traces_per_second",
    "send_logs",
    "send_logs_level",
    "send_logs_level_by_scope",
    "send_logs_per_second",
    "send_metrics",
    "send_traces",
  ],
  nextStart: ["deactivate_all_instrumentations", "deactivate_instrumentations", "opamp_polling_interval"],
};
const now = 1_791_979_200_000n; // 2026-10-14T12:00:00Z

test("loads, decides and describes", () => {
  const layer = new SdkSettings();
  const logsOff = policy({ signal_exports: off("logs") });
  const loaded = layer.load(text(logsOff), declaring);
  assert.equal(loaded.settings?.sendLogs, false);
  assert.equal(reported(loaded.effective), logsOff);

  const kept: Uint8Array[] = [];
  const quarter = policy({ head_sampling: { ratio: 0.25 } });
  const d = layer.decide(config(quarter), loaded.current, loaded.current, declaring, 0n, (doc) => {
    kept.push(doc);
  });
  assert.equal(d.status?.status, APPLIED);
  assert.deepEqual(d.status?.lastRemoteConfigHash, text("h"));
  assert.equal(d.apply?.samplingRate, 0.25);
  assert.deepEqual(kept, [text(quarter)]);
  assert.equal(reported(d.effective), quarter);

  const bad = layer.decide(config(policy({ head_sampling: { ratio: "x" } })), d.current, loaded.current, declaring, 0n, () => {});
  assert.equal(bad.status?.status, FAILED);
  assert.match(bad.status?.errorMessage ?? "", /^head_sampling:/);
  assert.equal(bad.apply, undefined);
  assert.deepEqual(bad.current, d.current);
  assert.equal(kept.length, 1); // refused: nothing to keep

  const desc = layer.describe([
    create(KeyValueSchema, { key: "service.name", value: { value: { case: "stringValue", value: "checkout" } } }),
    create(KeyValueSchema, { key: "host.name", value: { value: { case: "stringValue", value: "h" } } }),
  ], declaring, now);
  assert.deepEqual(desc.identifyingAttributes.map((kv) => kv.key), ["service.name"]);
  assert.equal(desc.nonIdentifyingAttributes[0]?.key, "host.name");
});

/** The description's policy attributes: a string's text, an array's strings. */
function described(desc: AgentDescription): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const kv of desc.nonIdentifyingAttributes) {
    if (!kv.key.startsWith("logfire.opamp.")) continue;
    const v = kv.value?.value;
    out[kv.key] = v?.case === "arrayValue"
      ? v.value.values.map((x) => (x.value.case === "stringValue" ? x.value.value : undefined))
      : v?.value;
  }
  return out;
}

test("the description names the policy formats and the controls the host carries out", () => {
  const layer = new SdkSettings();
  const host = { live: ["sampling_rate", "send_logs", "custom"], nextStart: ["opamp_polling_interval"] };
  assert.deepEqual(described(layer.describe([], host, now)), {
    "logfire.opamp.config_format": V1,
    "logfire.opamp.config_formats": [V1, V2],
    "logfire.opamp.supported_controls": ["head_sampling", "signal_exports.logs", "app_settings", "polling_interval"],
  });
  // Every setting declared: every control. No host: none.
  const every = described(layer.describe([], declaring, now))["logfire.opamp.supported_controls"] as string[];
  for (const c of ["head_sampling", "sampling_rules", "debug", "app_settings", "instrumentations"]) assert.ok(every.includes(c), c);
  assert.deepEqual(described(layer.describe([]))["logfire.opamp.supported_controls"], []);
});

test("a host says what it carries out", () => {
  const layer = new SdkSettings();
  const host = { live: ["deactivate_instrumentations"] };
  const sw = policy({ instrumentations: { disabled: ["http"] } });
  const d = layer.decide(config(sw), none, none, host);
  assert.equal(d.status?.status, APPLIED);
  assert.deepEqual(d.apply?.deactivateInstrumentations?.names, ["http"]); // live: in force now, no state needed
  assert.deepEqual(overridden(d.effective), { instrumentations: { disabled: ["http"] } });
  // An override the host does not carry out refuses the whole policy, and what was in force stays.
  const tracesOff = policy({ signal_exports: off("traces") });
  const refused = layer.decide(config(tracesOff), d.current, none, host);
  assert.equal(refused.status?.status, FAILED);
  assert.equal(refused.status?.errorMessage, "signal_exports.traces: this SDK cannot apply the requested override");
  assert.equal(refused.apply, undefined);
  assert.deepEqual(refused.current, d.current);
  assert.deepEqual(overridden(refused.effective), { instrumentations: { disabled: ["http"] } });
  // Version 2 names the field too.
  const rules = layer.decide(config(policy({ sampling_rules: { rules: [{ rate: 0 }] } })), d.current, none, host);
  assert.equal(rules.status?.errorMessage, "sampling_rules: rules: this SDK cannot apply the requested override");
  // A field nothing here knows refuses it too.
  const unknown = JSON.stringify({ ...JSON.parse(sw), x: "1" });
  assert.equal(layer.decide(config(unknown), d.current, none, host).status?.errorMessage, 'policy: unknown field "x"');
  // Saved under another host, a setting this one does not carry out is not in force.
  assert.equal(layer.load(text(tracesOff), host).settings?.sendTraces, undefined);
  // Declared nothing, nothing is carried out: every override is refused.
  const nothing = layer.decide(config(policy({ head_sampling: { ratio: 0.5 } })), none, none, undefined, 0n, () => {});
  assert.equal(nothing.status?.errorMessage, "head_sampling: this SDK cannot apply the requested override");
  // A host naming what is no setting has a bug: an error, saying which.
  assert.throws(() => layer.load(undefined, { live: ["send_metric"] }), /send_metric/);
});

test("a policy is the config's only file", () => {
  const layer = new SdkSettings();
  const beside = config(policy());
  beside.config!.configMap["other.json"] = create(AgentConfigObjectSchema, { body: text("{}"), contentType: "application/json" });
  const d = layer.decide(beside, none, none, declaring);
  assert.equal(d.status?.status, FAILED);
  assert.match(d.status?.errorMessage ?? "", /^config_map:/);
  const empty = create(AgentRemoteConfigSchema, { configHash: text("h"), config: { configMap: {} } });
  assert.match(layer.decide(empty, none, none, declaring).status?.errorMessage ?? "", /^config_map:/);
});

test("a log level arrives as the lowest severity number to export", () => {
  const layer = new SdkSettings();
  const d = layer.decide(config(policy({ log_levels: { level: "Warning" } })), none, none, declaring);
  assert.equal(d.status?.status, APPLIED);
  assert.equal(d.apply?.sendLogsMinSeverity, 13);
  assert.deepEqual(overridden(d.effective), { log_levels: { level: "warn" } });
  assert.equal(layer.load(text(policy()), declaring).settings?.sendLogsMinSeverity, undefined); // unset: every severity
});

test("the settings that match telemetry arrive typed, where the host declares them", () => {
  const layer = new SdkSettings();
  const doc = policy({
    sampling_rules: { rules: [{ rate: 0, kind: "server", path: "/health*" }] },
    log_levels: { scopes: { "com.acme.db*": "debug" } },
    attribute_redaction: { drop: ["*token*"], mask: ["User.Email"] },
    trace_rate_limit: { traces_per_second: 50 },
    log_rate_limit: { records_per_second: 0.5 },
  });
  const d = layer.decide(config(doc), none, none, declaring, now);
  assert.equal(d.status?.status, APPLIED);
  const s = d.apply!;
  assert.equal(s.samplingRules.length, 1);
  const [rule] = s.samplingRules;
  assert.equal(rule?.rate, 0);
  assert.equal(rule?.kind, SpanKind.SERVER);
  assert.equal(rule?.path, "/health*");
  assert.deepEqual(s.sendLogsScopeLevels.map((l) => [l.pattern, l.minSeverity]), [["com.acme.db*", 5]]);
  assert.deepEqual([s.dropAttributes, s.maskAttributes], [["*token*"], ["user.email"]]);
  assert.deepEqual([s.samplingTracesPerSecond, s.sendLogsPerSecond], [50, 0.5]);
  // Undeclared: refused, never in force.
  const plain = layer.decide(config(doc), none, none, { live: ["sampling_rate"] });
  assert.equal(plain.status?.status, FAILED);
  assert.equal(plain.apply, undefined);
  assert.equal(plain.status?.errorMessage, "sampling_rules: rules: this SDK cannot apply the requested override");
});

test("debug mode arrives folded in, and a refresh at its end ends it", () => {
  const layer = new SdkSettings();
  const doc = policy({ head_sampling: { ratio: 0.1 }, debug: { until: "2026-10-14T12:30:00Z", log_level: "info" } });
  const d = layer.decide(config(doc), none, none, declaring, now, () => {});
  assert.equal(d.apply?.samplingRate, 1);
  assert.equal(d.apply?.sendLogsMinSeverity, 9);
  const end = now + 30n * 60_000n;
  assert.equal(d.refreshAtUnixMs, end);
  const before = layer.refresh(d.current, none, end - 1n, declaring);
  assert.equal(before.settings?.samplingRate, 1);
  const after = layer.refresh(d.current, none, end, declaring);
  assert.equal(after.settings?.samplingRate, 0.1);
  assert.equal(after.refreshAtUnixMs, 0n);
  assert.deepEqual(overridden(after.effective), { head_sampling: { ratio: 0.1 } });
  assert.equal(layer.load(d.current, declaring, now).refreshAtUnixMs, end);
});

test("export health: sent at the first call and on a failure, held while nothing changes", () => {
  const layer = new SdkSettings();
  const first = layer.health({}, undefined, now);
  assert.equal(first.send, true);
  assert.deepEqual(Object.keys(first.exporters!.componentHealthMap).sort(), ["logs", "metrics", "traces"]);
  assert.equal(first.exporters!.status, "ok");
  assert.equal(layer.health({}, first, now + 30_000n).send, false);
  const failed = layer.health(
    { traces: { exportsFailed: 1n, itemsFailed: 512n, lastFailed: true, lastError: "connection refused" } },
    first,
    now + 30_000n,
    { sendLogs: false },
  );
  assert.equal(failed.send, true);
  assert.equal(failed.exporters!.healthy, false);
  assert.equal(failed.exporters!.componentHealthMap.traces?.lastError, "connection refused");
  assert.equal(failed.exporters!.componentHealthMap.logs?.status, "off");
});

interface CustomCase {
  name: string;
  document: string;
  host?: { live?: string[]; next_start?: string[] };
  refused?: string;
  custom?: Record<string, string>;
  effective?: string;
}

const hostOf = (h: { live?: string[]; next_start?: string[] } | undefined) =>
  h === undefined ? declaring : { live: h.live ?? [], nextStart: h.next_start ?? [] };

test("vectors: custom.json, the application's own settings, through Decide, Load and Refresh", () => {
  const url = new URL("../../../../testdata/sdk-settings/custom.json", import.meta.url);
  const cases = (JSON.parse(readFileSync(url, "utf8")) as { cases: CustomCase[] }).cases;
  assert.ok(cases.length >= 15);
  const layer = new SdkSettings();
  for (const c of cases) {
    const host = hostOf(c.host);
    const d = layer.decide(config(c.document), none, none, host, now, () => {});
    if (c.refused !== undefined) {
      assert.equal(d.status?.status, FAILED, c.name);
      assert.equal(d.status?.errorMessage, c.refused, c.name);
      assert.equal(d.apply, undefined, c.name);
      continue;
    }
    assert.equal(d.status?.status, APPLIED, `${c.name}: ${d.status?.errorMessage}`);
    assert.deepEqual({ ...d.apply!.custom }, c.custom, c.name);
    assert.equal(reported(d.effective), c.effective, c.name);
    // The same map at the next start (Load of what Decide saved) and at the timer's Refresh.
    const loaded = layer.load(d.current, host, now);
    assert.deepEqual({ ...loaded.settings!.custom }, c.custom, `${c.name} (load)`);
    const r = layer.refresh(d.current, d.current, now, host);
    assert.deepEqual({ ...r.settings!.custom }, c.custom, `${c.name} (refresh)`);
  }
});

interface SettingsCase {
  name: string;
  document: string;
  host?: { live?: string[]; next_start?: string[] };
  now_unix_ms?: number;
  refused?: string;
  effective?: string;
  refresh_at_unix_ms?: number;
}

test("vectors: settings.json, how the layer reads a policy, through Decide", () => {
  const url = new URL("../../../../testdata/sdk-settings/settings.json", import.meta.url);
  const cases = (JSON.parse(readFileSync(url, "utf8")) as { cases: SettingsCase[] }).cases;
  assert.ok(cases.length > 100);
  const layer = new SdkSettings();
  for (const c of cases) {
    // The policy is also the one in force at start, so a next-start setting is in force at once.
    const doc = text(c.document);
    const d = layer.decide(config(doc), none, doc, hostOf(c.host), BigInt(c.now_unix_ms ?? 0), () => {});
    if (c.refused !== undefined) {
      assert.equal(d.status?.errorMessage, c.refused, c.name);
      assert.equal(d.status?.status, FAILED, c.name);
      continue;
    }
    assert.equal(d.status?.status, APPLIED, `${c.name}: ${d.status?.errorMessage}`);
    assert.equal(reported(d.effective), c.effective, c.name);
    if (c.refresh_at_unix_ms !== undefined) assert.equal(d.refreshAtUnixMs, BigInt(c.refresh_at_unix_ms), c.name);
  }
});

test("the save the layer asks for decides the answer (#215: decide, then commit)", () => {
  const layer = new SdkSettings();
  const current = text(policy({ head_sampling: { ratio: 0.5 } }));
  const quarter = config(policy({ head_sampling: { ratio: 0.25 } }));
  const failed = layer.decide(quarter, current, current, declaring, 0n, () => {
    throw new Error("ENOSPC: no space left on device");
  });
  assert.equal(failed.status?.status, FAILED);
  assert.equal(failed.status?.errorMessage, "can't save the settings: ENOSPC: no space left on device");
  assert.equal(failed.apply, undefined);
  assert.deepEqual(failed.current, current);
  assert.deepEqual(overridden(failed.effective), { head_sampling: { ratio: 0.5 } });
  // Whatever is thrown refuses the config.
  const odd = layer.decide(quarter, current, current, declaring, 0n, () => {
    throw "a string";
  });
  assert.equal(odd.status?.errorMessage, "can't save the settings: a string");
  // Nowhere to keep state: a live change is applied, a next-start one refused.
  const live = layer.decide(quarter, current, current, declaring);
  assert.equal(live.status?.status, APPLIED);
  const minute = policy({ polling_interval: { milliseconds: 60_000 } });
  const later = layer.decide(config(minute), current, current, declaring);
  assert.equal(
    later.status?.errorMessage,
    "polling_interval: takes effect at the next start, which needs somewhere to keep state",
  );
  const kept: Uint8Array[] = [];
  const saved = layer.decide(config(minute), current, current, declaring, 0n, (doc) => {
    kept.push(doc);
  });
  assert.equal(saved.status?.status, APPLIED);
  assert.deepEqual(saved.pendingRestart, ["opamp_polling_interval"]);
  assert.deepEqual(kept, [text(minute)]);
});

test("a string that starts with U+FEFF comes back from the layer as it was sent", () => {
  // @bufbuild/protobuf's default decoder drops a leading U+FEFF (src/utf8.ts): the patterns the
  // matchers run on would differ from every other language's.
  const layer = new SdkSettings();
  const d = layer.decide(
    config(policy({
      sampling_rules: { rules: [{ rate: 1, name: "﻿GET", attributes: { "﻿k": "﻿*" } }] },
      log_levels: { scopes: { "﻿app*": "error" } },
      attribute_redaction: { drop: ["﻿secret"], mask: ["﻿﻿pass*"] },
    })),
    new Uint8Array(),
    new Uint8Array(),
    { live: ["sampling_rules", "send_logs_level", "send_logs_level_by_scope", "drop_attributes", "mask_attributes"] },
  );
  assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, d.status?.errorMessage);
  const rule = d.apply!.samplingRules[0]!;
  assert.equal(rule.name, "﻿GET");
  assert.deepEqual(rule.attributes, { "﻿k": "﻿*" });
  assert.equal(d.apply!.sendLogsScopeLevels[0]!.pattern, "﻿app*");
  assert.deepEqual(d.apply!.dropAttributes, ["﻿secret"]);
  assert.deepEqual(d.apply!.maskAttributes, ["﻿﻿pass*"]);
});

interface ComponentsCase {
  name: string;
  inventory: {
    integration?: string;
    integration_version?: string;
    sdk_language?: string;
    sdk_version?: string;
    instrumentations?: { name?: string; version?: string }[];
    exporters?: { signal?: string; name?: string; gated?: boolean }[];
  };
  host?: { live?: string[]; next_start?: string[] };
  now_unix_ms?: number;
  want: Record<string, unknown>;
}

/** The components as the vectors give them: sdk (key to text), instrumentations (name to version
 * or null), exporters (signal to name to gated) and settings (key to applies). */
function shown(c: AvailableComponents): Record<string, unknown> {
  const meta = (d: ComponentDetails, key: string) => d.metadata.find((kv) => kv.key === key)?.value?.value;
  const text = (v: ReturnType<typeof meta>) => {
    if (v === undefined) return null;
    assert.equal(v.case, "stringValue");
    return v.value;
  };
  const subs = <T>(d: ComponentDetails | undefined, f: (d: ComponentDetails, name: string) => T) =>
    Object.fromEntries(Object.entries(d?.subComponentMap ?? {}).map(([name, s]) => [name, f(s, name)]));
  const sdk = c.components.sdk!;
  return {
    sdk: Object.fromEntries(sdk.metadata.map((kv) => [kv.key, text(kv.value?.value)])),
    instrumentations: subs(c.components.instrumentations, (d) => text(meta(d, "version"))),
    exporters: subs(c.components.exporters, (signal, s) =>
      subs(signal, (e, name) => {
        const g = meta(e, "gated");
        assert.equal(g?.case, "boolValue", `${s}/${name}: gated`);
        return g.value;
      })),
    settings: subs(c.components.settings, (d) => text(meta(d, "applies"))),
  };
}

test("vectors: components.json, each inventory through Components", () => {
  const url = new URL("../../../../testdata/sdk-settings/components.json", import.meta.url);
  const cases = (JSON.parse(readFileSync(url, "utf8")) as { cases: ComponentsCase[] }).cases;
  assert.ok(cases.length > 10);
  const layer = new SdkSettings();
  for (const c of cases) {
    // Without a host, one that declares every setting, the application's own too.
    const host: HostInit = c.host
      ? { live: c.host.live ?? [], nextStart: c.host.next_start ?? [] }
      : declaring;
    const i = c.inventory;
    const inventory: InventoryInit = {
      integration: i.integration,
      integrationVersion: i.integration_version,
      sdkLanguage: i.sdk_language,
      sdkVersion: i.sdk_version,
      instrumentations: i.instrumentations,
      exporters: i.exporters,
    };
    const got = layer.components(inventory, host, BigInt(c.now_unix_ms ?? 0));
    assert.equal(got.hash.length, 0, `${c.name}: the client computes the hash`);
    assert.deepEqual(Object.keys(got.components).sort(), ["exporters", "instrumentations", "sdk", "settings"], c.name);
    const parts = shown(got);
    for (const [part, want] of Object.entries(c.want)) assert.deepEqual(parts[part], want, `${c.name}: ${part}`);
  }
});

type StartCase = { name: string; core_state?: string; saved?: string; want: string; warns: boolean };

test("vectors: start.json, the settings to start with from the core's saved state", () => {
  const url = new URL("../../../../testdata/sdk-settings/start.json", import.meta.url);
  const cases = (JSON.parse(readFileSync(url, "utf8")) as { cases: StartCase[] }).cases;
  const layer = new SdkSettings();
  for (const c of cases) {
    const coreState = c.core_state === undefined ? undefined : new Uint8Array(Buffer.from(c.core_state, "hex"));
    const saved = c.saved === undefined ? undefined : new TextEncoder().encode(c.saved);
    const loaded = layer.load(saved, declaring, 0n, coreState);
    assert.equal(new TextDecoder().decode(loaded.current), c.want, c.name);
    assert.equal(loaded.warning !== "", c.warns, `${c.name}: ${loaded.warning}`);
  }
});
