// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The SDK settings layer through the module: what the Rust tests hold it to, reached from Node.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import { create } from "@bufbuild/protobuf";

import {
  AgentRemoteConfigSchema,
  type AvailableComponents,
  type ComponentDetails,
  KeyValueSchema,
  RemoteConfigStatuses,
} from "../src/index.js";
import { type HostInit, type InventoryInit, SdkSettings, SpanKind } from "../src/sdk.js";

const text = (s: string) => new TextEncoder().encode(s);
const config = (body: string) =>
  create(AgentRemoteConfigSchema, {
    configHash: text("h"),
    config: { configMap: { elastic: { body: text(body), contentType: "application/json" } } },
  });
const effective = (e: { configMap?: { configMap: Record<string, { body: Uint8Array }> } } | undefined) =>
  new TextDecoder().decode(e?.configMap?.configMap.elastic?.body);
/** What the effective config names as set aside (its elastic.set_aside entry), if anything. */
const setAside = (e: { configMap?: { configMap: Record<string, { body: Uint8Array; contentType: string }> } } | undefined) => {
  const files = e?.configMap?.configMap ?? {};
  assert.deepEqual(Object.keys(files).filter((k) => k !== "elastic" && k !== "elastic.set_aside"), []);
  const f = files["elastic.set_aside"];
  if (f === undefined) return undefined;
  assert.equal(f.contentType, "application/json");
  return JSON.parse(new TextDecoder().decode(f.body)) as unknown;
};

test("loads, decides and describes", () => {
  const layer = new SdkSettings();
  const loaded = layer.load(text('{"send_logs":"false"}'), declaring);
  assert.equal(loaded.settings?.sendLogs, false);
  assert.equal(effective(loaded.effective), '{"send_logs":"false"}');

  const kept: Uint8Array[] = [];
  const d = layer.decide(config('{"sampling_rate":"0.25"}'), loaded.current, loaded.current, declaring, 0n, (doc) => {
    kept.push(doc);
  });
  assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  assert.deepEqual(d.status?.lastRemoteConfigHash, text("h"));
  assert.equal(d.apply?.samplingRate, 0.25);
  assert.deepEqual(kept, [text('{"sampling_rate":"0.25"}')]);
  assert.equal(effective(d.effective), '{"sampling_rate":"0.25"}');
  assert.equal(setAside(d.effective), undefined); // nothing set aside, no entry

  const bad = layer.decide(config('{"sampling_rate":"x"}'), d.current, loaded.current, declaring, 0n, () => {});
  assert.equal(bad.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.match(bad.status?.errorMessage ?? "", /^sampling_rate:/);
  assert.equal(bad.apply, undefined);
  assert.deepEqual(bad.current, d.current);
  assert.equal(kept.length, 1); // refused: nothing to keep

  const desc = layer.describe([
    create(KeyValueSchema, { key: "service.name", value: { value: { case: "stringValue", value: "checkout" } } }),
    create(KeyValueSchema, { key: "host.name", value: { value: { case: "stringValue", value: "h" } } }),
  ]);
  assert.deepEqual(desc.identifyingAttributes.map((kv) => kv.key), ["service.name"]);
  assert.deepEqual(desc.nonIdentifyingAttributes.map((kv) => kv.key), ["host.name"]);
});

test("a host says what it carries out", () => {
  const layer = new SdkSettings();
  const host = { live: ["deactivate_instrumentations"] };
  const d = layer.decide(config('{"send_traces":"false","deactivate_instrumentations":"http","x":"1"}'), new Uint8Array(), new Uint8Array(), host);
  assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  // Not declared, so not carried out; a setting nothing knows is told apart from it.
  assert.deepEqual([d.apply?.unsupported, d.apply?.ignored], [["send_traces"], ["x"]]);
  assert.deepEqual(d.apply?.deactivateInstrumentations?.names, ["http"]); // live: in force now, no state needed
  assert.equal(effective(d.effective), '{"deactivate_instrumentations":"http"}');
  // Named for the server beside what is in force (#215), by Decide, Load and Refresh alike, and
  // still after a FAILED, which leaves the document in force.
  const names = { ignored: ["x"], unsupported: ["send_traces"] };
  assert.deepEqual(setAside(d.effective), names);
  assert.equal(d.status?.errorMessage, "");
  assert.deepEqual(setAside(layer.load(d.current, host).effective), names);
  assert.deepEqual(setAside(layer.refresh(d.current, new Uint8Array(), 1n, host).effective), names);
  const failed = layer.decide(config('{"deactivate_instrumentations":"HTTP"}'), d.current, new Uint8Array(), host);
  assert.equal(failed.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.deepEqual(setAside(failed.effective), names);
  assert.equal(layer.load(text('{"send_traces":"false"}'), host).settings?.sendTraces, undefined);
  // A host naming what is no setting has a bug: an error, saying which.
  assert.throws(() => layer.load(undefined, { live: ["send_metric"] }), /send_metric/);
});

test("a log level arrives as the lowest severity number to export", () => {
  const layer = new SdkSettings();
  const d = layer.decide(config('{"send_logs_level":"Warning"}'), new Uint8Array(), new Uint8Array(), declaring);
  assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  assert.equal(d.apply?.sendLogsMinSeverity, 13);
  assert.equal(effective(d.effective), '{"send_logs_level":"warn"}');
  assert.equal(layer.load(text("{}"), declaring).settings?.sendLogsMinSeverity, undefined); // unset: every severity
});

// A host that declares every setting: each live one at once, each next-start one at the next start.
const declaring = {
  live: [
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

test("the settings that match telemetry arrive typed, where the host declares them", () => {
  const layer = new SdkSettings();
  const doc =
    '{"sampling_rules":[{"rate":0,"kind":"server","path":"/health*"}],"send_logs_level_by_scope":"com.acme.db*=debug",' +
    '"drop_attributes":"*token*","mask_attributes":"User.Email","sampling_traces_per_second":"50","send_logs_per_second":"0.5"}';
  const d = layer.decide(config(doc), new Uint8Array(), new Uint8Array(), declaring, now);
  assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  const s = d.apply!;
  assert.equal(s.samplingRules.length, 1);
  const [rule] = s.samplingRules;
  assert.equal(rule?.rate, 0);
  assert.equal(rule?.kind, SpanKind.SERVER);
  assert.equal(rule?.path, "/health*");
  assert.deepEqual(s.sendLogsScopeLevels.map((l) => [l.pattern, l.minSeverity]), [["com.acme.db*", 5]]);
  assert.deepEqual([s.dropAttributes, s.maskAttributes], [["*token*"], ["user.email"]]);
  assert.deepEqual([s.samplingTracesPerSecond, s.sendLogsPerSecond], [50, 0.5]);
  // Undeclared: ignored, never in force.
  const plain = layer.decide(config(doc), new Uint8Array(), new Uint8Array(), { live: ["sampling_rate"] });
  assert.equal(plain.apply?.samplingRules.length, 0);
  assert.equal(plain.apply?.unsupported.length, 6);
  assert.equal(effective(plain.effective), "{}");
});

test("debug mode arrives folded in, and a refresh at its end ends it", () => {
  const layer = new SdkSettings();
  const doc = '{"sampling_rate":"0.1","debug_until":"2026-10-14T12:30:00Z","debug_send_logs_level":"info"}';
  const d = layer.decide(config(doc), new Uint8Array(), new Uint8Array(), declaring, now, () => {});
  assert.equal(d.apply?.samplingRate, 1);
  assert.equal(d.apply?.sendLogsMinSeverity, 9);
  const end = now + 30n * 60_000n;
  assert.equal(d.refreshAtUnixMs, end);
  const before = layer.refresh(d.current, new Uint8Array(), end - 1n, declaring);
  assert.equal(before.settings?.samplingRate, 1);
  const after = layer.refresh(d.current, new Uint8Array(), end, declaring);
  assert.equal(after.settings?.samplingRate, 0.1);
  assert.equal(after.refreshAtUnixMs, 0n);
  assert.equal(effective(after.effective), '{"sampling_rate":"0.1"}');
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
  host?: { live?: string[] };
  refused?: string;
  custom?: Record<string, string>;
  effective?: Record<string, string>;
  set_aside?: object;
}

test("vectors: custom.json, the application's own settings, through Decide, Load and Refresh", () => {
  const url = new URL("../../../../testdata/sdk-settings/custom.json", import.meta.url);
  const cases = (JSON.parse(readFileSync(url, "utf8")) as { cases: CustomCase[] }).cases;
  assert.ok(cases.length >= 30);
  const layer = new SdkSettings();
  const none = new Uint8Array();
  // Without a host of its own, a case's host declares every setting, as the Rust vectors' does.
  const every = { ...declaring, live: [...declaring.live, "custom"] };
  for (const c of cases) {
    const host = c.host ?? every;
    const d = layer.decide(config(c.document), none, none, host, now, () => {});
    if (c.refused !== undefined) {
      assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED, c.name);
      assert.equal(d.status?.errorMessage, c.refused, c.name);
      assert.equal(d.apply, undefined, c.name);
      continue;
    }
    assert.equal(d.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED, `${c.name}: ${d.status?.errorMessage}`);
    assert.deepEqual({ ...d.apply!.custom }, c.custom, c.name);
    assert.deepEqual(JSON.parse(effective(d.effective)), c.effective, c.name);
    assert.deepEqual(setAside(d.effective), c.set_aside, c.name);
    // The same map at the next start (Load of what Decide saved) and at the timer's Refresh.
    const loaded = layer.load(d.current, host, now);
    assert.deepEqual({ ...loaded.settings!.custom }, c.custom, `${c.name} (load)`);
    const r = layer.refresh(d.current, d.current, now, host);
    assert.deepEqual({ ...r.settings!.custom }, c.custom, `${c.name} (refresh)`);
  }
});

test("the save the layer asks for decides the answer (#215: decide, then commit)", () => {
  const layer = new SdkSettings();
  const current = text('{"sampling_rate":"0.5"}');
  const failed = layer.decide(config('{"sampling_rate":"0.25"}'), current, current, declaring, 0n, () => {
    throw new Error("ENOSPC: no space left on device");
  });
  assert.equal(failed.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_FAILED);
  assert.equal(failed.status?.errorMessage, "can't save the settings: ENOSPC: no space left on device");
  assert.equal(failed.apply, undefined);
  assert.deepEqual(failed.current, current);
  assert.equal(effective(failed.effective), '{"sampling_rate":"0.5"}');
  // Whatever is thrown refuses the config.
  const odd = layer.decide(config('{"sampling_rate":"0.25"}'), current, current, declaring, 0n, () => {
    throw "a string";
  });
  assert.equal(odd.status?.errorMessage, "can't save the settings: a string");
  // Nowhere to keep state: a live change is applied, a next-start one refused.
  const live = layer.decide(config('{"sampling_rate":"0.25"}'), current, current, declaring);
  assert.equal(live.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  const later = layer.decide(config('{"opamp_polling_interval":"1m"}'), current, current, declaring);
  assert.equal(
    later.status?.errorMessage,
    "opamp_polling_interval take effect at the next start, which needs somewhere to keep state",
  );
  const kept: Uint8Array[] = [];
  const saved = layer.decide(config('{"opamp_polling_interval":"1m"}'), current, current, declaring, 0n, (doc) => {
    kept.push(doc);
  });
  assert.equal(saved.status?.status, RemoteConfigStatuses.RemoteConfigStatuses_APPLIED);
  assert.deepEqual(saved.pendingRestart, ["opamp_polling_interval"]);
  assert.deepEqual(kept, [text('{"opamp_polling_interval":"1m"}')]);
});

test("a string that starts with U+FEFF comes back from the layer as it was sent", () => {
  // @bufbuild/protobuf's default decoder drops a leading U+FEFF (src/utf8.ts): the patterns the
  // matchers run on would differ from every other language's.
  const layer = new SdkSettings();
  const d = layer.decide(
    config(JSON.stringify({
      sampling_rules: [{ rate: 1, name: "﻿GET", attributes: { "﻿k": "﻿*" } }],
      send_logs_level_by_scope: "﻿app*=error",
      drop_attributes: "﻿secret",
      mask_attributes: "﻿﻿pass*",
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
    // Without a host, one that declares every setting, the application's own too (as custom.json's).
    const host: HostInit = c.host
      ? { live: c.host.live ?? [], nextStart: c.host.next_start ?? [] }
      : { live: [...declaring.live, "custom"], nextStart: declaring.nextStart };
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

type StartCase = { name: string; core_state?: string; saved?: string; want: string | null };

test("vectors: start.json, the settings to start with from the core's saved state", () => {
  const url = new URL("../../../../testdata/sdk-settings/start.json", import.meta.url);
  const cases = (JSON.parse(readFileSync(url, "utf8")) as { cases: StartCase[] }).cases;
  const layer = new SdkSettings();
  for (const c of cases) {
    const coreState = c.core_state === undefined ? undefined : new Uint8Array(Buffer.from(c.core_state, "hex"));
    const saved = c.saved === undefined ? undefined : new TextEncoder().encode(c.saved);
    const loaded = layer.load(saved, undefined, 0n, coreState);
    assert.equal(new TextDecoder().decode(loaded.current), c.want ?? "{}", c.name);
  }
});
