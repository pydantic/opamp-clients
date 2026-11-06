// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The SDK settings layer through the module: what the Rust tests hold it to, reached from Node.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";

import {
  AgentConfigObjectSchema,
  type AgentDescription,
  type AgentRemoteConfig,
  AgentRemoteConfigSchema,
  type AvailableComponents,
  type ComponentDetails,
  type ComponentHealth,
  KeyValueSchema,
  type RemoteConfigStatus,
  RemoteConfigStatuses,
} from "../src/index.js";
import {
  type AgentEffect,
  AgentLog_Level,
  type AgentOutput,
  type AgentState,
  AgentStateSchema,
  type AgentStepInit,
  type FoundExporterInit,
  type HostInit,
  type InventoryInit,
  SdkSettings,
  type Settings,
  SpanKind,
} from "../src/sdk.js";
import { config, full, off, overridden, policy, reported, V1 } from "./policy.js";

const text = (s: string) => new TextEncoder().encode(s);
const APPLIED = RemoteConfigStatuses.RemoteConfigStatuses_APPLIED;
const FAILED = RemoteConfigStatuses.RemoteConfigStatuses_FAILED;

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
    "scrub_defaults",
    "scrub_patterns",
    "send_logs",
    "send_logs_level",
    "send_logs_level_by_scope",
    "send_logs_per_second",
    "send_metrics",
    "send_self_metrics",
    "send_traces",
  ],
  nextStart: ["deactivate_all_instrumentations", "deactivate_instrumentations"],
};
const now = 1_791_979_200_000n; // 2026-10-14T12:00:00Z

type Effect = NonNullable<AgentEffect["effect"]>;

/** The layer's agent flow, stepped by a test, on clocks that move together from `now`. */
class Flow {
  readonly layer = new SdkSettings();
  out: AgentOutput;
  readonly now: bigint;

  constructor(host?: HostInit, o: { stateDir?: boolean; coreState?: Uint8Array; now?: bigint } = {}) {
    this.now = o.now ?? now;
    this.out = this.layer.start({
      ...(host ? { host } : {}),
      stateDir: o.stateDir ?? true,
      ...(o.coreState ? { coreState: o.coreState } : {}),
      endpoint: "http://127.0.0.1:1",
      instanceUid: new Uint8Array(16),
      nowUnixMs: this.now,
      nowMonoMs: this.now,
    });
  }

  step(ms: number, input: AgentStepInit["input"]): this {
    const at = this.now + BigInt(ms);
    this.out = this.layer.step({ agent: this.out.agent, nowUnixMs: at, nowMonoMs: at, input });
    return this;
  }

  decide(c: AgentRemoteConfig, ms = 1): this {
    return this.step(ms, { case: "remoteConfig", value: c });
  }

  /** The last effect of a kind in the last output. */
  #effect(kind: Effect["case"]): unknown {
    let found: unknown;
    for (const e of this.out.effects) if (e.effect.case === kind) found = e.effect.value;
    return found;
  }

  get status(): RemoteConfigStatus | undefined {
    return this.#effect("status") as RemoteConfigStatus | undefined;
  }

  get apply(): Settings | undefined {
    return this.#effect("apply") as Settings | undefined;
  }

  get health(): ComponentHealth | undefined {
    return this.#effect("health") as ComponentHealth | undefined;
  }

  get effective() {
    return this.out.effective;
  }

  get warned(): boolean {
    return this.out.effects.some((e) => e.effect.case === "log" && e.effect.value.level >= AgentLog_Level.WARN);
  }

  get state(): AgentState {
    return fromBinary(AgentStateSchema, this.out.agent);
  }
}

const url = (name: string) => new URL(`../../../../testdata/sdk-settings/${name}`, import.meta.url);
type StartCase = { name: string; core_state?: string; want: string; warns: boolean };
const START = (JSON.parse(readFileSync(url("start.json"), "utf8")) as { cases: StartCase[] }).cases;

/**
 * The client's saved state with a config applied whose only policy file is `doc`: a state saved
 * before the client kept the config applied (start.json), with the config applied added (its field
 * 16, a message, which protobuf takes from the end of the encoding).
 */
function keptApplied(doc: string | Uint8Array): Uint8Array {
  const base = START.find((c) => c.name.startsWith("a core state saved before the core kept the config applied"))!;
  const applied = toBinary(AgentRemoteConfigSchema, config(doc));
  const size: number[] = [];
  let n = applied.length;
  do {
    size.push((n & 0x7f) | (n > 0x7f ? 0x80 : 0));
    n >>= 7;
  } while (n);
  return Uint8Array.from([...Buffer.from(base.core_state!, "hex"), (16 << 3) | 2, 1, ...size, ...applied]);
}

test("starts, decides and describes", () => {
  const f = new Flow(declaring);
  assert.ok(f.apply);
  assert.equal(reported(f.effective), full(policy()));
  const quarter = policy({ head_sampling: { ratio: 0.25 } });
  f.decide(config(quarter));
  assert.equal(f.status?.status, APPLIED);
  assert.deepEqual(f.status?.lastRemoteConfigHash, text("h"));
  assert.equal(f.apply?.samplingRate, 0.25);
  assert.equal(reported(f.effective), full(quarter));
  f.decide(config(policy({ head_sampling: { ratio: "x" } })), 2);
  assert.equal(f.status?.status, FAILED);
  assert.match(f.status?.errorMessage ?? "", /^head_sampling:/);
  assert.equal(f.apply, undefined);
  assert.equal(reported(f.effective), full(quarter));
  // A step without an input, or on a state that isn't one, is an error.
  assert.throws(() => f.layer.step({ agent: f.out.agent }));
  assert.throws(() => f.layer.step({ agent: Uint8Array.of(0xff), input: { case: "timer", value: {} } }));

  const desc = f.layer.describe([
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

test("the description names the policy format and the controls the host carries out", () => {
  const layer = new SdkSettings();
  const host = { live: ["sampling_rate", "send_logs", "custom"], nextStart: ["deactivate_instrumentations"] };
  assert.deepEqual(described(layer.describe([], host, now)), {
    "logfire.opamp.config_format": V1,
    "logfire.opamp.supported_controls": ["head_sampling", "signal_exports.logs", "app_settings", "instrumentations"],
  });
  // Every setting declared: every control. No host: none.
  const every = described(layer.describe([], declaring, now))["logfire.opamp.supported_controls"] as string[];
  for (const c of ["head_sampling", "sampling_rules", "debug", "app_settings", "instrumentations"]) assert.ok(every.includes(c), c);
  assert.deepEqual(described(layer.describe([]))["logfire.opamp.supported_controls"], []);
});

test("a host says what it carries out", () => {
  const host = { live: ["deactivate_instrumentations"] };
  const sw = policy({ instrumentations: { disabled: ["http"] } });
  const f = new Flow(host, { stateDir: false }).decide(config(sw));
  assert.equal(f.status?.status, APPLIED);
  assert.deepEqual(f.apply?.deactivateInstrumentations?.names, ["http"]); // live: in force now, no state needed
  assert.deepEqual(overridden(f.effective), { instrumentations: { disabled: ["http"] } });
  // An override the host does not carry out refuses the whole policy, and what was in force stays.
  f.decide(config(policy({ signal_exports: off("traces") })), 2);
  assert.equal(f.status?.status, FAILED);
  assert.equal(f.status?.errorMessage, "signal_exports.traces: this SDK cannot apply the requested override");
  assert.equal(f.apply, undefined);
  assert.deepEqual(overridden(f.effective), { instrumentations: { disabled: ["http"] } });
  // A control past the first two names the field too.
  f.decide(config(policy({ sampling_rules: { rules: [{ rate: 0 }] } })), 3);
  assert.equal(f.status?.errorMessage, "sampling_rules: rules: this SDK cannot apply the requested override");
  // A field nothing here knows refuses it too.
  const unknown = JSON.stringify({ ...JSON.parse(sw), x: "1" });
  assert.equal(f.decide(config(unknown), 4).status?.errorMessage, 'policy: unknown field "x"');
  // Declared nothing, nothing is carried out: every override is refused.
  const nothing = new Flow().decide(config(policy({ head_sampling: { ratio: 0.5 } })));
  assert.equal(nothing.status?.errorMessage, "head_sampling: this SDK cannot apply the requested override");
  // A host naming what is no setting has a bug: an error, saying which.
  assert.throws(() => new Flow({ live: ["send_metric"] }), /send_metric/);
});

test("a policy is the config's only file", () => {
  const beside = config(policy());
  beside.config!.configMap["other.json"] = create(AgentConfigObjectSchema, { body: text("{}"), contentType: "application/json" });
  const f = new Flow(declaring).decide(beside);
  assert.equal(f.status?.status, FAILED);
  assert.match(f.status?.errorMessage ?? "", /^config_map:/);
  const empty = create(AgentRemoteConfigSchema, { configHash: text("h"), config: { configMap: {} } });
  assert.match(f.decide(empty, 2).status?.errorMessage ?? "", /^config_map:/);
});

test("a next-start change needs somewhere to keep state", () => {
  const quarter = policy({ head_sampling: { ratio: 0.25 } });
  assert.equal(new Flow(declaring, { stateDir: false }).decide(config(quarter)).status?.status, APPLIED); // live
  const redis = policy({ instrumentations: { disabled: ["redis"] } });
  assert.equal(
    new Flow(declaring, { stateDir: false }).decide(config(redis)).status?.errorMessage,
    "instrumentations: takes effect at the next start, which needs somewhere to keep state",
  );
  assert.equal(new Flow(declaring).decide(config(redis)).status?.status, APPLIED);
});

test("a log level arrives as the lowest severity number to export", () => {
  const f = new Flow(declaring);
  assert.equal(f.apply?.sendLogsMinSeverity, undefined); // unset: every severity
  f.decide(config(policy({ log_levels: { level: "Warning" } })));
  assert.equal(f.status?.status, APPLIED);
  assert.equal(f.apply?.sendLogsMinSeverity, 13);
  assert.deepEqual(overridden(f.effective), { log_levels: { level: "warn" } });
});

test("the settings that match telemetry arrive typed, where the host declares them", () => {
  const doc = policy({
    sampling_rules: { rules: [{ rate: 0, kind: "server", path: "/health*" }] },
    log_levels: { scopes: { "com.acme.db*": "debug" } },
    attribute_redaction: { drop: ["*token*"], mask: ["User.Email"] },
    trace_rate_limit: { traces_per_second: 50 },
    log_rate_limit: { records_per_second: 0.5 },
  });
  const d = new Flow(declaring).decide(config(doc));
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
  const plain = new Flow({ live: ["sampling_rate"] }).decide(config(doc));
  assert.equal(plain.status?.status, FAILED);
  assert.equal(plain.apply, undefined);
  assert.equal(plain.status?.errorMessage, "sampling_rules: rules: this SDK cannot apply the requested override");
});

test("debug mode arrives folded in, and the timer at its end ends it", () => {
  const doc = policy({ head_sampling: { ratio: 0.1 }, debug: { until: "2026-10-14T12:30:00Z", log_level: "info" } });
  const f = new Flow(declaring).decide(config(doc), 0);
  assert.equal(f.apply?.samplingRate, 1);
  assert.equal(f.apply?.sendLogsMinSeverity, 9);
  const end = 30 * 60_000;
  assert.equal(f.state.refreshAtUnixMs, now + BigInt(end));
  assert.ok(f.out.wakeAtMonoMs > 0n && f.out.wakeAtMonoMs <= now + BigInt(end));
  f.step(end, { case: "timer", value: {} });
  assert.equal(f.apply?.samplingRate, 0.1);
  assert.equal(f.state.refreshAtUnixMs ?? 0n, 0n);
  assert.deepEqual(overridden(f.effective), { head_sampling: { ratio: 0.1 } });
});

test("export health: sent when built and on a failure, held while nothing changes", () => {
  const f = new Flow(declaring);
  const exporters = (counts = {}) => ({ case: "exporters" as const, value: { exporters: [], counts } });
  f.step(0, { case: "built", value: { service: "checkout" } });
  f.step(0, exporters());
  const first = f.health!.componentHealthMap.exporters!;
  assert.deepEqual(Object.keys(first.componentHealthMap).sort(), ["logs", "metrics", "traces"]);
  assert.equal(first.status, "ok");
  assert.equal(f.step(30_000, exporters()).health, undefined);
  f.step(30_000, exporters({ traces: { exportsFailed: 1n, itemsFailed: 512n, lastFailed: true, lastError: "connection refused" } }));
  const sent = f.health!.componentHealthMap.exporters!;
  assert.equal(sent.healthy, false);
  assert.equal(sent.componentHealthMap.traces?.lastError, "connection refused");
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

test("vectors: custom.json, the application's own settings, through the flow", () => {
  const cases = (JSON.parse(readFileSync(url("custom.json"), "utf8")) as { cases: CustomCase[] }).cases;
  assert.ok(cases.length >= 15);
  for (const c of cases) {
    const d = new Flow(hostOf(c.host)).decide(config(c.document));
    if (c.refused !== undefined) {
      assert.equal(d.status?.status, FAILED, c.name);
      assert.equal(d.status?.errorMessage, c.refused, c.name);
      assert.equal(d.apply, undefined, c.name);
      continue;
    }
    assert.equal(d.status?.status, APPLIED, `${c.name}: ${d.status?.errorMessage}`);
    assert.deepEqual({ ...d.apply!.custom }, c.custom, c.name);
    assert.equal(reported(d.effective), c.effective, c.name);
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

test("vectors: settings.json, how the layer reads a policy, through the flow", () => {
  const cases = (JSON.parse(readFileSync(url("settings.json"), "utf8")) as { cases: SettingsCase[] }).cases;
  assert.ok(cases.length > 100);
  for (const c of cases) {
    // A host that gives no time is another SDK's case: this one always has a clock.
    if (c.now_unix_ms === undefined && c.document.includes('"debug":{"mode":"override"')) continue;
    // The policy kept applied too, so a next-start setting is in force at once.
    const d = new Flow(hostOf(c.host), { coreState: keptApplied(c.document), now: BigInt(c.now_unix_ms ?? now) })
      .decide(config(c.document), 0);
    if (c.refused !== undefined) {
      assert.equal(d.status?.errorMessage, c.refused, c.name);
      assert.equal(d.status?.status, FAILED, c.name);
      continue;
    }
    assert.equal(d.status?.status, APPLIED, `${c.name}: ${d.status?.errorMessage}`);
    assert.equal(reported(d.effective), c.effective, c.name);
    if (c.refresh_at_unix_ms !== undefined) assert.equal(d.state.refreshAtUnixMs ?? 0n, BigInt(c.refresh_at_unix_ms), c.name);
  }
});

test("a string that starts with U+FEFF comes back from the layer as it was sent", () => {
  // @bufbuild/protobuf's default decoder drops a leading U+FEFF (src/utf8.ts): the patterns the
  // matchers run on would differ from every other language's.
  const d = new Flow(
    { live: ["sampling_rules", "send_logs_level", "send_logs_level_by_scope", "drop_attributes", "mask_attributes"] },
    { stateDir: false },
  ).decide(config(policy({
    sampling_rules: { rules: [{ rate: 1, name: "\uFEFFGET", attributes: { "\uFEFFk": "\uFEFF*" } }] },
    log_levels: { scopes: { "\uFEFFapp*": "error" } },
    attribute_redaction: { drop: ["\uFEFFsecret"], mask: ["\uFEFF\uFEFFpass*"] },
  })));
  assert.equal(d.status?.status, APPLIED, d.status?.errorMessage);
  const rule = d.apply!.samplingRules[0]!;
  assert.equal(rule.name, "\uFEFFGET");
  assert.deepEqual(rule.attributes, { "\uFEFFk": "\uFEFF*" });
  assert.equal(d.apply!.sendLogsScopeLevels[0]!.pattern, "\uFEFFapp*");
  assert.deepEqual(d.apply!.dropAttributes, ["\uFEFFsecret"]);
  assert.deepEqual(d.apply!.maskAttributes, ["\uFEFF\uFEFFpass*"]);
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
    // Where each exporter sends, for those that say: signal to identity to destination.
    destinations: Object.fromEntries(Object.entries(subs(c.components.exporters, (signal) =>
      Object.fromEntries(Object.entries(subs(signal, (e) => text(meta(e, "destination")))).filter(([, d]) => d))))
      .filter(([, d]) => Object.keys(d).length > 0)),
    settings: subs(c.components.settings, (d) => text(meta(d, "applies"))),
  };
}

test("vectors: exporters.json, each case through Exporters", () => {
    type Found = { signal: string; kind?: string; gated?: boolean; endpoint?: string; from_env?: boolean };
  type Case = { name: string; found: Found[]; env?: Record<string, string>; default_protocol?: string; want: unknown };
  const cases = (JSON.parse(readFileSync(url("exporters.json"), "utf8")) as { cases: Case[] }).cases;
  assert.ok(cases.length > 10);
  const layer = new SdkSettings();
  for (const c of cases) {
    const found: FoundExporterInit[] = c.found.map(({ from_env, ...f }) => ({ ...f, fromEnv: from_env }));
    const got = layer.exporters(found, c.env ?? {}, c.default_protocol ?? "");
    assert.deepEqual({
      exporters: got.exporters.map((e) => ({ signal: e.signal, name: e.name, gated: e.gated, destination: e.destination })),
      governed: got.governed, ungoverned: got.ungoverned, warnings: got.warnings,
    }, c.want, c.name);
  }
});

test("vectors: components.json, each inventory through Components", () => {
    const cases = (JSON.parse(readFileSync(url("components.json"), "utf8")) as { cases: ComponentsCase[] }).cases;
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

test("vectors: start.json, the settings to start with from the core's saved state", () => {
  for (const c of START) {
    const coreState = c.core_state === undefined ? undefined : new Uint8Array(Buffer.from(c.core_state, "hex"));
    const f = new Flow(declaring, coreState ? { coreState } : {});
    assert.equal(new TextDecoder().decode(f.state.current), c.want, c.name);
    assert.equal(f.warned, c.warns, c.name);
  }
});
