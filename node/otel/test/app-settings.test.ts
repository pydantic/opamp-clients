// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The application's own settings (docs/app-settings.md): appSettings on its own, then through the
// layer and the agent: the shared vectors (testdata/sdk-settings/custom.json), reads
// before and after a config, once per change and never for the same map, callbacks that throw or
// reject, order, unsubscribing, a FAILED config, the timer's Refresh, and restarts.

import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { type AgentRemoteConfig, AgentRemoteConfigSchema, create, type RemoteConfigStatus, RemoteConfigStatuses } from "@pydantic/opamp-client";
import { type HostInit, SdkSettings } from "@pydantic/opamp-client/sdk";

import * as standalone from "../src/app-settings.js";
import { keepApplied } from "./kept.js";
import {
  Agent, AppSettings, type AppSettingsMap, appSettings, Controls, hostFromEnv, preparing, settingsKeptOnFailure,
} from "../src/index.js";

const LAYER = new SdkSettings();
const text = (s: string) => new TextEncoder().encode(s);
const EMPTY = new Uint8Array();
const APPLIED = RemoteConfigStatuses.RemoteConfigStatuses_APPLIED;
const FAILED = RemoteConfigStatuses.RemoteConfigStatuses_FAILED;
/** 2026-10-14T12:00:00Z. */
const NOW = 1_791_979_200_000;

const config = (body: string) =>
  create(AgentRemoteConfigSchema, {
    configHash: text(body),
    config: { configMap: { elastic: { body: text(body), contentType: "application/json" } } },
  });
/** A remote config with no `elastic` entry at all. */
const noElastic = () => create(AgentRemoteConfigSchema, { configHash: text("none"), config: { configMap: {} } });

const doc = (custom: unknown, rest: object = {}) => JSON.stringify({ ...rest, custom });

interface Case {
  name: string;
  document: string;
  host?: { live?: string[] };
  refused?: string;
  custom?: Record<string, string>;
  effective?: Record<string, string>;
}

interface Typed {
  value: string;
  bool: boolean | null;
  int: number | null;
  float: number | null;
}

const VECTORS = JSON.parse(readFileSync(
  new URL("../../../../testdata/sdk-settings/custom.json", import.meta.url), "utf8",
)) as { cases: Case[]; typed: Typed[] };

/** An AppSettings whose reports are kept, and a callback that records what it is told. */
function recorded() {
  const logged: { info: string[]; error: string[] } = { info: [], error: [] };
  const app = new AppSettings({ info: (m) => logged.info.push(m), error: (m) => logged.error.push(m) });
  const told: { settings: AppSettingsMap; changed: readonly string[] }[] = [];
  const unsubscribe = app.onChange((settings, changed) => void told.push({ settings, changed }));
  return { app, logged, told, unsubscribe };
}

/**
 * An agent as the register makes one, on its own AppSettings, with the clocks and timer in the
 * test's hands and a client that keeps the effective configs it is handed.
 */
class TestAgent extends Agent {
  wall = NOW;
  mono = 0;
  armed: { fn: () => void; ms: number; cleared: boolean }[] = [];
  reported: object[] = [];

  constructor(dir: string | undefined, app: AppSettings = recorded().app, env: NodeJS.ProcessEnv = {},
    prepare = true) {
    super(app);
    this.now = () => this.wall;
    this.elapsed = () => this.mono;
    this.timers = {
      set: (fn, ms) => {
        const t = { fn: () => ((t.cleared = true), fn()), ms, cleared: false };
        this.armed.push(t);
        return t;
      },
      clear: (t) => void ((t as { cleared: boolean }).cleared = true),
    };
    if (prepare) this.prepare(TestAgent.env(dir, env));
    this.client = {
      setEffectiveConfig: (e: { configMap?: { configMap: { elastic?: { body: Uint8Array } } } }) =>
        this.reported.push(JSON.parse(new TextDecoder().decode(e.configMap!.configMap.elastic!.body))),
      setHealth: () => {},
      stop: () => Promise.resolve(),
    } as never;
  }

  static env(dir: string | undefined, env: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
    return { OTEL_OPAMP_ENDPOINT: "http://server/v1/opamp", OTEL_OPAMP_STATE_DIR: dir ?? "none", ...env };
  }

  get app(): AppSettings {
    return this.controls.app;
  }

  /**
   * The remote config answered, and, APPLIED with a state directory, kept as the client keeps it
   * (in its state, before APPLIED goes out): this agent has no client, so a restart finds it there.
   */
  override remoteConfig(config: AgentRemoteConfig): RemoteConfigStatus {
    const s = super.remoteConfig(config);
    const dir = this.setup?.stateDir;
    if (s.status === RemoteConfigStatuses.RemoteConfigStatuses_APPLIED && dir !== undefined) {
      keepApplied(dir, config.config?.configMap.elastic?.body ?? new Uint8Array());
    }
    return s;
  }

  get timer() {
    const t = this.armed.at(-1);
    return t && !t.cleared ? t : undefined;
  }

  get settingsInForce(): object {
    return JSON.parse(new TextDecoder().decode(this.effective?.configMap?.configMap.elastic?.body));
  }
}

const stateDir = () => mkdtempSync(join(tmpdir(), "opamp-app-"));

// --- the shared vectors ---------------------------------------------------------------------------

test("vectors: custom.json's cases through the layer, with each case's host", () => {
  assert.ok(VECTORS.cases.length >= 30);
  for (const c of VECTORS.cases) {
    const host: HostInit = c.host ?? { live: ["custom"] };
    const d = LAYER.decide(config(c.document), EMPTY, EMPTY, host, 0n);
    if (c.refused !== undefined) {
      assert.equal(d.status?.status, FAILED, c.name);
      assert.equal(d.status?.errorMessage, c.refused, c.name);
      assert.equal(d.apply, undefined, c.name);
    } else {
      assert.equal(d.status?.status, APPLIED, `${c.name}: ${d.status?.errorMessage}`);
      assert.deepEqual({ ...d.apply!.custom }, c.custom, c.name);
    }
  }
});

test("vectors: custom.json's cases through the agent: what the application reads, and what is reported", () => {
  for (const c of VECTORS.cases.filter((k) => k.host === undefined)) {
    const { app, told } = recorded();
    const agent = new TestAgent(undefined, app);
    // Something in force first: a refused config must leave it, an applied one replace it.
    assert.equal(agent.remoteConfig(config(doc({ before: "1" }))).status, APPLIED);
    const before = app.snapshot();
    told.length = 0;
    const status = agent.remoteConfig(config(c.document));
    if (c.refused !== undefined) {
      assert.equal(status.status, FAILED, c.name);
      assert.equal(status.errorMessage, c.refused, c.name);
      assert.equal(app.snapshot(), before, c.name);
      assert.equal(app.get("before", "default"), "1", c.name);
      assert.deepEqual(told, [], c.name);
      assert.deepEqual(agent.settingsInForce, { custom: '{"before":"1"}' }, c.name);
    } else {
      assert.equal(status.status, APPLIED, `${c.name}: ${status.errorMessage}`);
      assert.deepEqual({ ...app.snapshot() }, c.custom, c.name);
      for (const [k, v] of Object.entries(c.custom!)) assert.equal(app.get(k, "default"), v, c.name);
      assert.equal(app.get("before", "default"), c.custom!.before ?? "default", c.name);
      assert.deepEqual(agent.settingsInForce, c.effective, c.name);
      // Once, with every key added or removed.
      assert.equal(told.length, 1, c.name);
      const keys = [...new Set(["before", ...Object.keys(c.custom!)])].sort();
      assert.deepEqual(told[0]!.changed, keys, c.name);
    }
  }
});

test("vectors: custom.json's typed reads (getInt: only safe integers, else the default)", () => {
  assert.ok(VECTORS.typed.length >= 30);
  for (const t of VECTORS.typed) {
    const app = new AppSettings({ info: () => {}, error: () => {} });
    app.put({ k: t.value });
    const what = JSON.stringify(t.value);
    // Two defaults each: a value read is the same whatever the default; one not read is the default.
    for (const def of [true, false]) assert.equal(app.getBool("k", def), t.bool ?? def, `getBool ${what}`);
    const int = t.int !== null && Number.isSafeInteger(t.int) ? t.int : null;
    for (const def of [-99, 99]) assert.equal(app.getInt("k", def), int ?? def, `getInt ${what}`);
    for (const def of [-0.125, 0.125]) assert.equal(app.getFloat("k", def), t.float ?? def, `getFloat ${what}`);
    // The value itself, as sent.
    assert.equal(app.get("k", "default"), t.value);
  }
});

test("getInt: the safe integers' edges, and beyond them the default", () => {
  const app = new AppSettings({ info: () => {}, error: () => {} });
  const cases: [string, number | null][] = [
    ["9007199254740991", 9007199254740991], ["-9007199254740991", -9007199254740991],
    ["+0009007199254740991", 9007199254740991], ["9007199254740992", null], ["-9007199254740992", null],
    ["9007199254740993", null], ["9223372036854775807", null], ["-9223372036854775808", null],
    ["99999999999999999999999", null], ["-0", 0], ["1.0", null], ["1e3", null], [" -12\r\n", -12],
  ];
  for (const [value, want] of cases) {
    app.put({ k: value });
    assert.equal(app.getInt("k", 7), want ?? 7, value);
    if (want === 0) assert.ok(Object.is(app.getInt("k", 7), 0), "-0 reads as 0");
  }
});

test("typed reads: only space, tab, CR and LF are trimmed; no other Unicode space or case", () => {
  const app = new AppSettings({ info: () => {}, error: () => {} });
  const reads: [string, boolean | null, number | null, number | null][] = [
    ["\t\r\n true \n", true, null, null], [" true", null, null, null], ["true ", null, null, null],
    ["\u000btrue", null, null, null], ["﻿true", null, null, null], ["TrUe", true, null, null],
    ["fALSE", false, null, null], ["truе", null, null, null], ["İrue", null, null, null],
    ["1 0", null, null, null], ["\n42\n", null, 42, 42], ["-.5", null, null, null], ["0.5e-2", null, null, 0.005],
    ["-0", null, 0, -0], ["+1.5E+2", null, null, 150], ["1e308", null, null, 1e308], ["-1e309", null, null, null],
    ["0x1p3", null, null, null], ["1n", null, null, null], ["00.5", null, null, null],
  ];
  for (const [value, bool, int, float] of reads) {
    app.put({ k: value });
    assert.equal(app.getBool("k", false) === true ? true : app.getBool("k", true) === false ? false : null, bool, `bool ${JSON.stringify(value)}`);
    assert.equal(app.getInt("k", 7) === 7 ? null : app.getInt("k", 7), int, `int ${JSON.stringify(value)}`);
    assert.equal(app.getFloat("k", 7.5) === 7.5 ? null : app.getFloat("k", 7.5), float, `float ${JSON.stringify(value)}`);
  }
});

// --- reads ---------------------------------------------------------------------------------------

test("before any config: every read is its default, and the snapshot is empty and frozen", () => {
  const app = new AppSettings();
  assert.equal(app.get("checkout.new_flow", "off"), "off");
  assert.equal(app.get("checkout.new_flow"), undefined);
  assert.equal(app.getBool("b", true), true);
  assert.equal(app.getInt("i", 3), 3);
  assert.equal(app.getFloat("f", 0.5), 0.5);
  assert.deepEqual({ ...app.snapshot() }, {});
  assert.ok(Object.isFrozen(app.snapshot()));
  // An agent on its own AppSettings, prepared with no saved settings, puts nothing in force.
  const agent = new TestAgent(undefined);
  assert.deepEqual({ ...agent.app.snapshot() }, {});
  // No endpoint, nothing managed: the defaults, always.
  const off = new Agent(new AppSettings());
  off.prepare({});
  assert.equal(off.on, false);
  assert.equal(off.controls.app.get("x", "def"), "def");
});

test("get never throws and never answers from Object's prototype, whatever the key", () => {
  const app = new AppSettings({ info: () => {}, error: () => {} });
  app.put({ a: "1" });
  for (const key of ["constructor", "toString", "__proto__", "hasOwnProperty", "valueOf", "", "A"]) {
    assert.equal(app.get(key, "def"), "def", key);
    assert.equal(app.get(key), undefined, key);
    assert.equal(app.snapshot()[key], undefined, key);
  }
  for (const key of [undefined, null, 1, {}, Symbol("s"), ["a"]] as unknown as string[]) {
    assert.equal(app.get(key, "def"), "def");
    assert.equal(app.getBool(key, true), true);
    assert.equal(app.getInt(key, 1), 1);
    assert.equal(app.getFloat(key, 1.5), 1.5);
  }
  // Keys the server may send that are Object's own names are values like any other.
  app.put({ constructor: "c", toString: "t" });
  assert.equal(app.get("constructor", "def"), "c");
  assert.equal(app.snapshot().toString, "t");
  assert.equal(Object.getPrototypeOf(app.snapshot()), null);
});

test("the snapshot is frozen, and no later change alters it", () => {
  const app = new AppSettings({ info: () => {}, error: () => {} });
  app.put({ a: "1", b: "2" });
  const first = app.snapshot();
  assert.ok(Object.isFrozen(first));
  assert.throws(() => {
    (first as Record<string, string>).a = "x";
  }, TypeError);
  app.put({ a: "3" });
  assert.deepEqual({ ...first }, { a: "1", b: "2" });
  assert.deepEqual({ ...app.snapshot() }, { a: "3" });
  // The same map again: the very same snapshot.
  const second = app.snapshot();
  app.put({ a: "3" });
  assert.equal(app.snapshot(), second);
  // A map put in force is copied: changing the caller's object afterwards changes nothing.
  const mine: Record<string, string> = { z: "1" };
  app.put(mine);
  mine.z = "2";
  assert.equal(app.get("z", "def"), "1");
});

test("appSettings is one per process, whichever path loads it, and the package's agent uses it", async () => {
  assert.equal(standalone.appSettings, appSettings);
  assert.equal(standalone.AppSettings, AppSettings);
  assert.equal((globalThis as Record<symbol, unknown>)[Symbol.for("@pydantic/opamp-otel.appSettings")], appSettings);
  const { AGENT } = await import("../src/index.js");
  assert.equal(AGENT.controls.app, appSettings);
  // A CommonJS application's require() reaches the same one (require of an ES module, Node.js 22.12+).
  const required = createRequire(import.meta.url)("../src/app-settings.js") as typeof standalone;
  assert.equal(required.appSettings, appSettings);
  // Nothing in force in this process: no server.
  assert.equal(appSettings.get("example.feature", "off"), "off");
});

// --- changes -------------------------------------------------------------------------------------

test("once per change, with the keys added, changed and removed; get already new inside the callback", () => {
  const { app, told, logged } = recorded();
  const inside: (string | undefined)[] = [];
  app.onChange(() => void inside.push(app.get("a")));
  app.put({ a: "1", b: "2" });
  app.put({ a: "1", b: "3", c: "4" });
  app.put({ c: "4" });
  app.put({});
  assert.deepEqual(told.map((t) => t.changed), [["a", "b"], ["b", "c"], ["a", "b"], ["c"]]);
  assert.deepEqual(told.map((t) => ({ ...t.settings })), [{ a: "1", b: "2" }, { a: "1", b: "3", c: "4" }, { c: "4" }, {}]);
  assert.deepEqual(inside, ["1", "1", undefined, undefined]);
  for (const t of told) {
    assert.ok(Object.isFrozen(t.settings) && Object.isFrozen(t.changed));
  }
  assert.deepEqual(logged.info, [
    "OpAMP: application settings changed: a, b", "OpAMP: application settings changed: b, c",
    "OpAMP: application settings changed: a, b", "OpAMP: application settings changed: c",
  ]);
});

test("not told when nothing changed: the same map, another order, nothing to nothing", () => {
  const { app, told, logged } = recorded();
  app.put(undefined);
  app.put({});
  app.put({ a: "1", b: "2" });
  app.put({ b: "2", a: "1" });
  assert.equal(told.length, 1);
  assert.equal(logged.info.length, 1);
  // Case and spaces are a change: values are kept exactly as sent.
  app.put({ a: "1 ", b: "2" });
  app.put({ A: "1 ", b: "2" });
  assert.deepEqual(told.map((t) => t.changed), [["a", "b"], ["a"], ["A", "a"]]);
});

test("through the agent: the same map again, sent another way, or another setting changing, tells nothing", () => {
  const { app, told } = recorded();
  const agent = new TestAgent(undefined, app);
  const sends = [
    doc({ "example.feature": "on", limit: 25 }),
    doc({ limit: "25", "example.feature": "on" }), // 25 sent is "25" taken; another order
    doc(JSON.stringify({ "example.feature": "on", limit: "25" })), // in a string
    doc({ "example.feature": "on", limit: "25" }, { sampling_rate: "0.5" }), // another setting
    doc({ "example.feature": "on", limit: "25", gone: null }), // a null entry is a key left out
  ];
  for (const body of sends) assert.equal(agent.remoteConfig(config(body)).status, APPLIED, body);
  assert.equal(told.length, 1);
  assert.deepEqual(told[0]!.changed, ["example.feature", "limit"]);
  assert.equal(app.getInt("limit", 0), 25);
  assert.equal(agent.controls.sampler.samplingRate, undefined);
});

test("a value that starts with U+FEFF reaches the application as it was sent, and at the next start", () => {
  // @bufbuild/protobuf's default decoder drops a leading U+FEFF (@pydantic/opamp-client's src/utf8.ts).
  // Keys are ASCII (docs/app-settings.md), so only a value can carry one.
  const BOM = "\uFEFF";
  const dir = stateDir();
  const { app, told } = recorded();
  const agent = new TestAgent(dir, app);
  const sent = { flag: `${BOM}on`, twice: `${BOM}${BOM}x`, only: BOM };
  const status = agent.remoteConfig(config(doc(sent)));
  assert.equal(status.status, APPLIED, status.errorMessage);
  assert.deepEqual({ ...app.snapshot() }, sent);
  assert.equal(app.get("flag"), `${BOM}on`);
  assert.deepEqual(told.map((t) => t.changed), [["flag", "only", "twice"]]);
  // The same value without it is a change.
  agent.remoteConfig(config(doc({ ...sent, flag: "on" })));
  assert.deepEqual(told.map((t) => t.changed), [["flag", "only", "twice"], ["flag"]]);
  agent.remoteConfig(config(doc(sent)));
  const next = recorded();
  new TestAgent(dir, next.app);
  assert.deepEqual({ ...next.app.snapshot() }, sent);
  rmSync(dir, { recursive: true });
});

test("left out, null, {} or blank, or no elastic entry: every key back to its default, told once", () => {
  for (const out of ["{}", doc(null), doc({}), doc("  "), doc(""), "none"]) {
    const { app, told } = recorded();
    const agent = new TestAgent(undefined, app);
    agent.remoteConfig(config(doc({ "example.feature": "on" })));
    const s = agent.remoteConfig(out === "none" ? noElastic() : config(out));
    assert.equal(s.status, APPLIED, out);
    assert.equal(app.get("example.feature", "off"), "off", out);
    assert.deepEqual(told.map((t) => t.changed), [["example.feature"], ["example.feature"]], out);
    assert.deepEqual({ ...told[1]!.settings }, {}, out);
  }
});

test("a FAILED config leaves the values, tells nothing, and the effective config as it was", () => {
  const { app, told } = recorded();
  const agent = new TestAgent(undefined, app);
  agent.remoteConfig(config(doc({ "example.feature": "on" }, { sampling_rate: "0.5" })));
  const reported = agent.settingsInForce;
  for (const bad of [
    doc({ "example.feature": ["on"] }), doc({ "a b": "x" }), doc([1]), doc("[1]"), doc({ a: "x".repeat(4097) }),
    '{"custom":{"a":"1","a":"2"}}', doc({ "example.feature": "off" }, { sampling_rate: "2" }),
  ]) {
    const s = agent.remoteConfig(config(bad));
    assert.equal(s.status, FAILED, bad);
    assert.equal(app.get("example.feature", "off"), "on", bad);
    assert.deepEqual(agent.settingsInForce, reported, bad);
  }
  assert.equal(told.length, 1);
  assert.equal(agent.controls.sampler.samplingRate, 0.5);
});

test("the package saves nothing: the client keeps the config applied", () => {
  const dir = stateDir();
  const { app, told } = recorded();
  const agent = new TestAgent(dir, app);
  assert.equal(agent.remoteConfig(config(doc({ "example.feature": "on" }))).status, APPLIED);
  assert.equal(agent.remoteConfig(config(doc({ "example.feature": "off" }))).status, APPLIED);
  assert.equal(existsSync(join(dir, "sdk-settings.json")), false);
  assert.equal(app.get("example.feature", "def"), "off");
  assert.equal(told.length, 2);
});

test("the timer's Refresh (debug mode's end) puts the same map in force again: nothing to tell", () => {
  const { app, told } = recorded();
  const agent = new TestAgent(undefined, app);
  const until = new Date(NOW + 5000).toISOString();
  agent.remoteConfig(config(doc({ "example.feature": "on" }, { sampling_rate: "0.1", debug_until: until })));
  assert.equal(told.length, 1);
  assert.equal(agent.controls.sampler.samplingRate, 1); // in debug mode
  assert.equal(agent.timer?.ms, 5000);
  agent.wall += 5000;
  agent.mono += 5000;
  agent.timer!.fn();
  // Debug mode is over and the rate is back; the application's settings are untouched by it.
  assert.equal(agent.controls.sampler.samplingRate, 0.1);
  assert.equal(app.get("example.feature", "off"), "on");
  assert.equal(told.length, 1);
  assert.deepEqual(agent.reported.at(-1), { custom: '{"example.feature":"on"}', sampling_rate: "0.1" });
  // A Refresh called again (no debug mode at all): still nothing.
  agent.refresh();
  assert.equal(told.length, 1);
});

test("debug mode leaves the application's settings alone: they apply in it, and stay after it", () => {
  const { app, told } = recorded();
  const agent = new TestAgent(undefined, app);
  const until = new Date(NOW + 1000).toISOString();
  agent.remoteConfig(config(doc({ a: "1" }, { debug_until: until })));
  agent.remoteConfig(config(doc({ a: "2" }, { debug_until: until })));
  assert.equal(app.get("a"), "2");
  agent.wall += 1000;
  agent.mono += 1000;
  agent.timer!.fn();
  assert.equal(app.get("a"), "2");
  assert.deepEqual(told.map((t) => t.settings.a), ["1", "2"]);
});

// --- callbacks -----------------------------------------------------------------------------------

test("callbacks run in the order they were added, each change in the order it happened", () => {
  const app = new AppSettings({ info: () => {}, error: () => {} });
  const calls: string[] = [];
  app.onChange((s) => void calls.push(`first ${s.v}`));
  app.onChange((s) => void calls.push(`second ${s.v}`));
  app.onChange((s) => void calls.push(`third ${s.v}`));
  app.put({ v: "1" });
  app.put({ v: "2" });
  assert.deepEqual(calls, ["first 1", "second 1", "third 1", "first 2", "second 2", "third 2"]);
});

test("a callback that throws is logged; the others still run, and the config is still APPLIED", () => {
  const { app, logged } = recorded();
  const calls: string[] = [];
  app.onChange(function boom() {
    throw new Error("the application's bug");
  });
  app.onChange(() => {
    // A throw of something that isn't an Error.
    throw "a string"; // eslint-disable-line no-throw-literal
  });
  app.onChange((s) => void calls.push(s.v!));
  const agent = new TestAgent(undefined, app);
  assert.equal(agent.remoteConfig(config(doc({ v: "1" }))).status, APPLIED);
  assert.equal(agent.remoteConfig(config(doc({ v: "2" }))).status, APPLIED);
  assert.deepEqual(calls, ["1", "2"]);
  assert.equal(logged.error.length, 4);
  assert.match(logged.error[0]!, /^OpAMP: an application settings callback \(boom\) failed: Error: the application's bug/);
  assert.match(logged.error[1]!, /^OpAMP: an application settings callback \(anonymous\) failed: a string$/);
  assert.equal(app.get("v"), "2");
});

test("a callback's rejected promise is logged, and doesn't hold up the others", async () => {
  const { app, logged } = recorded();
  const calls: string[] = [];
  app.onChange(async function later() {
    await Promise.resolve();
    throw new Error("rejected");
  });
  app.onChange(() => Promise.reject(new Error("at once")));
  app.onChange((s) => void calls.push(s.v!));
  // A promise that resolves is fine; so is a thenable.
  app.onChange(async () => {});
  app.onChange(() => ({ then: (ok: () => void) => ok() }));
  // A thenable whose then throws: a rejection like any other.
  app.onChange(function odd() {
    return { then: () => { throw new Error("bad thenable"); } };
  });
  app.put({ v: "1" });
  assert.deepEqual(calls, ["1"]);
  assert.deepEqual(logged.error, [], "nothing logged before the promises settle");
  await new Promise((r) => setImmediate(r));
  assert.equal(logged.error.length, 3);
  assert.ok(logged.error.some((m) => /callback \(odd\) failed: Error: bad thenable/.test(m)));
  assert.ok(logged.error.some((m) => /callback \(later\) failed: Error: rejected/.test(m)));
  assert.ok(logged.error.some((m) => /callback \(anonymous\) failed: Error: at once/.test(m)));
});

test("a log that throws doesn't stop the callbacks either", () => {
  const app = new AppSettings({ info: () => {}, error: () => { throw new Error("log down"); } });
  const calls: string[] = [];
  app.onChange(() => { throw new Error("bug"); });
  app.onChange((s) => void calls.push(s.v!));
  app.put({ v: "1" });
  assert.deepEqual(calls, ["1"]);
});

test("unsubscribing: from the next change, twice is nothing, and the same callback added twice is two", () => {
  const app = new AppSettings({ info: () => {}, error: () => {} });
  const calls: string[] = [];
  const cb = (s: AppSettingsMap) => void calls.push(s.v!);
  const one = app.onChange(cb);
  const two = app.onChange(cb);
  app.put({ v: "1" });
  assert.deepEqual(calls, ["1", "1"]);
  one();
  one();
  app.put({ v: "2" });
  assert.deepEqual(calls, ["1", "1", "2"]);
  two();
  app.put({ v: "3" });
  assert.deepEqual(calls, ["1", "1", "2"]);
  assert.throws(() => app.onChange(undefined as never), TypeError);
});

test("a callback may unsubscribe itself or others, or subscribe more, while it runs: from the next change", () => {
  const app = new AppSettings({ info: () => {}, error: () => {} });
  const calls: string[] = [];
  let removeSecond = () => {};
  const removeFirst = app.onChange((s) => {
    calls.push(`first ${s.v}`);
    removeFirst(); // itself
    removeSecond(); // a later one: still told of this change
    app.onChange((t) => void calls.push(`added ${t.v}`));
  });
  removeSecond = app.onChange((s) => void calls.push(`second ${s.v}`));
  app.put({ v: "1" });
  app.put({ v: "2" });
  assert.deepEqual(calls, ["first 1", "second 1", "added 2"]);
});

// --- restarts --------------------------------------------------------------------------------------

test("a restart with a state directory: the values in force at exit, from the start, with nothing to tell", () => {
  const dir = stateDir();
  const first = new TestAgent(dir);
  first.remoteConfig(config(doc({ "example.feature": "on", limit: "25" })));
  assert.equal(existsSync(join(dir, "sdk-settings.json")), false); // the client keeps it
  // The next start: in force before any config, and the server not reached at all.
  const { app, told } = recorded();
  const second = new TestAgent(dir, app);
  assert.equal(app.get("example.feature", "off"), "on");
  assert.equal(app.getInt("limit", 0), 25);
  assert.deepEqual(second.settingsInForce, { custom: '{"example.feature":"on","limit":"25"}' });
  // Its callback was added before Load here (a test can; the register's application can't): told once, at Load.
  assert.deepEqual(told.map((t) => t.changed), [["example.feature", "limit"]]);
  // The server sends the same again: nothing to tell.
  second.remoteConfig(config(doc({ limit: "25", "example.feature": "on" })));
  assert.equal(told.length, 1);
  // Then leaves custom out, and the start after has the defaults.
  second.remoteConfig(config("{}"));
  assert.equal(app.get("example.feature", "off"), "off");
  const third = new TestAgent(dir);
  assert.equal(third.app.get("example.feature", "off"), "off");
});

test("a restart with a state directory, a callback added after start (as an application's): not told of the saved values", () => {
  const dir = stateDir();
  new TestAgent(dir).remoteConfig(config(doc({ a: "1" })));
  const app = new AppSettings({ info: () => {}, error: () => {} });
  const agent = new TestAgent(dir, app);
  const told: string[][] = [];
  app.onChange((_, changed) => void told.push([...changed]));
  assert.equal(app.get("a"), "1");
  agent.remoteConfig(config(doc({ a: "1" })));
  assert.deepEqual(told, []);
  agent.remoteConfig(config(doc({ a: "2" })));
  assert.deepEqual(told, [["a"]]);
});

test("a restart without a state directory starts from the defaults", () => {
  const first = new TestAgent(undefined);
  first.remoteConfig(config(doc({ a: "1" })));
  assert.equal(first.app.get("a"), "1");
  const second = new TestAgent(undefined);
  assert.equal(second.app.get("a", "def"), "def");
});

test("a saved document with a bad custom map: the SDK's own settings, the defaults, and a warning", () => {
  const dir = stateDir();
  new TestAgent(dir).remoteConfig(config(doc({ a: "1" })));
  // Written by hand, not by the client: a map the layer refuses.
  keepApplied(dir, doc({ "a b": "1" }));
  const agent = new TestAgent(dir);
  assert.equal(agent.app.get("a b", "def"), "def");
  assert.equal(agent.app.get("a", "def"), "def");
  assert.ok(agent.loaded?.warning, "the load warns");
});

// --- what the package declares --------------------------------------------------------------------

test("custom is declared live always, whatever builds the exporters", () => {
  for (const env of [
    {}, { OTEL_TRACES_EXPORTER: "zipkin" }, { OTEL_LOGS_EXPORTER: "none", OTEL_METRICS_EXPORTER: "none" },
    { OTEL_TRACES_EXPORTER: "zipkin", OTEL_LOGS_EXPORTER: "something", OTEL_METRICS_EXPORTER: "prometheus" },
  ]) {
    const host = hostFromEnv(env);
    assert.ok(host.live?.includes("custom"), JSON.stringify(env));
    assert.ok(!host.nextStart?.includes("custom"), JSON.stringify(env));
  }
});

test("Controls without an AppSettings of their own keep a private one: tests and the package's agent don't mix", () => {
  const a = new Controls();
  const b = new Controls();
  assert.notEqual(a.app, b.app);
  assert.notEqual(a.app, appSettings);
  a.apply(LAYER.decide(config(doc({ k: "v" })), EMPTY, EMPTY, { live: ["custom"] }, 0n).apply);
  assert.equal(a.app.get("k"), "v");
  assert.equal(b.app.get("k"), undefined);
  assert.equal(appSettings.get("k"), undefined);
  // Settings left out entirely (no reply): the defaults.
  a.apply(undefined);
  assert.equal(a.app.get("k"), undefined);
});

test("a setup that fails after Load takes the saved values back; one that succeeds keeps them", () => {
  const dir = stateDir();
  new TestAgent(dir).remoteConfig(config(doc({ "example.feature": "on" })));
  const { app, told } = recorded();
  app.put({ mine: "before" });
  told.length = 0;
  assert.throws(() => settingsKeptOnFailure(app, () => {
    new TestAgent(dir, app); // Load: the saved values in force
    assert.equal(app.get("example.feature", "off"), "on");
    throw new Error("the SDK can't be built");
  }), /can't be built/);
  assert.deepEqual({ ...app.snapshot() }, { mine: "before" });
  assert.deepEqual(told.map((t) => t.changed), [["example.feature", "mine"], ["example.feature", "mine"]]);
  const agent = settingsKeptOnFailure(app, () => new TestAgent(dir, app));
  assert.equal(agent.app.get("example.feature", "off"), "on");
});

test("a register that fails after Load: no timer left to put the saved settings back by a later Refresh", () => {
  const dir = stateDir();
  // Saved: the flag on, in a debug window, so Load arms the timer for its end.
  const until = new Date(NOW + 5000).toISOString();
  new TestAgent(dir).remoteConfig(config(doc({ "example.feature": "on" }, { debug_until: until })));
  const { app, told } = recorded();
  const agent = new TestAgent(dir, app, {}, false);
  assert.throws(() => preparing(agent, app, TestAgent.env(dir), () => {
    assert.equal(app.get("example.feature", "off"), "on");
    assert.equal(agent.timer?.ms, 5000);
    throw new Error("the SDK can't be built");
  }), /can't be built/);
  assert.equal(app.get("example.feature", "off"), "off");
  assert.equal(agent.timer, undefined, "the timer is cleared");
  assert.equal(agent.on, false);
  // Even a timer that fires anyway (already on its way) puts nothing back.
  for (const t of agent.armed) t.fn();
  agent.refresh();
  assert.equal(app.get("example.feature", "off"), "off");
  assert.equal(agent.armed.filter((t) => !t.cleared).length, 0, "nothing armed again");
  assert.equal(told.length, 2);
  // The same, succeeding: prepared, and the flag on.
  const ok = new TestAgent(dir, app, {}, false);
  assert.equal(preparing(ok, app, TestAgent.env(dir), () => "built"), "built");
  assert.equal(app.get("example.feature", "off"), "on");
  assert.equal(ok.timer?.ms, 5000);
});
