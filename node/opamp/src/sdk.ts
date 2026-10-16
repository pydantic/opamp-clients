// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// What an OpenTelemetry SDK does with the settings an OpAMP server sends it, decided the same way
// in every language: the SDK settings layer (a WebAssembly module,
// core/opamp_sdk_settings.wasm), run in Node's own WebAssembly engine. It keeps nothing between
// calls; the caller keeps the settings documents it is handed (`current`, and the one it saves) and
// passes them back. See docs/sdk-settings.md.

import { readFileSync } from "node:fs";

import "./utf8.js";

import { create, fromBinary, type MessageInitShape, toBinary } from "@bufbuild/protobuf";

import {
  Commit_Saved,
  type Decision,
  type ExportCountsSchema,
  type HealthReport,
  type HostSchema,
  type InventorySchema,
  type Loaded,
  type Refreshed,
  type SettingsSchema,
  type Reply,
  ReplySchema,
  RequestSchema,
} from "./gen/opamp/sdk/v1/settings_pb.js";
import type { KeyValue } from "./gen/opamp/v1/anyvalue_pb.js";
import type { AgentDescription, AgentRemoteConfig, AvailableComponents } from "./gen/opamp/v1/opamp_pb.js";

export * from "./gen/opamp/sdk/v1/settings_pb.js";

/** What an SDK carries out: a setting it doesn't name is set aside (docs/sdk-settings.md#the-host). */
export type HostInit = MessageInitShape<typeof HostSchema>;

/** What the integration lists of itself and its SDK (docs/available-components.md#what-the-sdk-integrations-report). */
export type InventoryInit = MessageInitShape<typeof InventorySchema>;

let compiled: WebAssembly.Module | undefined;

function module(): WebAssembly.Module {
  compiled ??= new WebAssembly.Module(readFileSync(new URL("../../core/opamp_sdk_settings.wasm", import.meta.url)));
  return compiled;
}

interface Exports {
  memory: WebAssembly.Memory;
  alloc(len: number): number;
  free(ptr: number, len: number): void;
  call(ptr: number, len: number): bigint;
}

/** An instance of the layer. */
export class SdkSettings {
  #x: Exports = SdkSettings.#instance();

  static #instance(): Exports {
    return new WebAssembly.Instance(module(), {}).exports as unknown as Exports;
  }

  /**
   * At start, before the SDK is built: the settings to start with, from the document saved at the
   * last change (undefined: nothing saved). Put all of them in force, next-start ones too.
   * `host`: the settings the SDK carries out, at once or at the next start; one it leaves out is set
   * aside, never in force (left out: none carried out). `nowUnixMs`: the wall
   * clock, which debug mode is decided by (0: none, and debug mode is ignored). `coreState`: the
   * OpAMP core's saved state (what it asked the host to store; undefined: none). The settings then
   * come from the last config applied in it, and `saved` is read only when it holds none (a
   * document kept before the core kept the config applied).
   */
  load(saved?: Uint8Array, host?: HostInit, nowUnixMs = 0n, coreState?: Uint8Array): Loaded {
    const r = this.#call({ call: { case: "load", value: { saved, coreState, nowUnixMs, ...(host ? { host } : {}) } } });
    if (r.result.case !== "loaded") throw new Error(`load answered ${r.result.case}`);
    return r.result.value;
  }

  /**
   * How to answer a remote config. `current` is the document in force (from the last load or
   * decision), `atStart` the one loaded at start, `host` and `nowUnixMs` as for `load`.
   *
   * The layer decides what to keep for the next start; `save` keeps it (whole or not at all, where
   * `load` reads it then), and throws if it can't; the layer then answers from how that went: a
   * config whose save failed is refused, saying why. Left out: the SDK keeps no state across
   * restarts, so a config that changes a next-start setting is refused.
   */
  decide(
    config: AgentRemoteConfig,
    current: Uint8Array,
    atStart: Uint8Array,
    host?: HostInit,
    nowUnixMs = 0n,
    save?: (doc: Uint8Array) => void,
  ): Decision {
    const decide = { config, current, atStart, nowUnixMs, ...(host ? { host } : {}) };
    const p = this.#call({ call: { case: "decide", value: decide } });
    if (p.result.case !== "plan") throw new Error(`decide answered ${p.result.case}`);
    const kept = p.result.value.save;
    let saved = Commit_Saved.NOWHERE;
    let error = "";
    if (kept !== undefined && save !== undefined) {
      try {
        save(kept);
        saved = Commit_Saved.DONE;
      } catch (e) {
        saved = Commit_Saved.FAILED;
        error = e instanceof Error ? e.message : String(e);
      }
    }
    const r = this.#call({ call: { case: "commit", value: { decide, saved, error } } });
    if (r.result.case !== "decision") throw new Error(`commit answered ${r.result.case}`);
    return r.result.value;
  }

  /**
   * What is in force now, at the time a load, decision or refresh said to ask again
   * (`refreshAtUnixMs`): the end of debug mode. `current` and `atStart` as for `decide`.
   */
  refresh(current: Uint8Array, atStart: Uint8Array, nowUnixMs: bigint, host?: HostInit): Refreshed {
    const r = this.#call({ call: { case: "refresh", value: { current, atStart, nowUnixMs, ...(host ? { host } : {}) } } });
    if (r.result.case !== "refreshed") throw new Error(`refresh answered ${r.result.case}`);
    return r.result.value;
  }

  /**
   * Export health, from the gates' counts since start: whether to send it now, and the entry for
   * the agent health's `component_health_map["exporters"]`. `previous`: what the last call
   * answered; `settings`: those in force.
   */
  health(
    counts: MessageInitShape<typeof ExportCountsSchema>,
    previous: HealthReport | undefined,
    nowUnixMs: bigint,
    settings?: MessageInitShape<typeof SettingsSchema>,
  ): HealthReport {
    const r = this.#call({ call: { case: "health", value: { counts, previous, nowUnixMs, settings } } });
    if (r.result.case !== "healthReport") throw new Error(`health answered ${r.result.case}`);
    return r.result.value;
  }

  /** The agent description, from the SDK's resource attributes. */
  describe(resource: KeyValue[]): AgentDescription {
    const r = this.#call({ call: { case: "describe", value: { resource } } });
    if (r.result.case !== "description") throw new Error(`describe answered ${r.result.case}`);
    return r.result.value;
  }

  /**
   * What the instance runs, as its available components, for the client's
   * `setAvailableComponents` (the hash left empty: the client computes it). Once the SDK is built,
   * and again when what it lists changes. `host` and `nowUnixMs` as for `load`.
   */
  components(inventory: InventoryInit, host?: HostInit, nowUnixMs = 0n): AvailableComponents {
    const r = this.#call({ call: { case: "components", value: { inventory, nowUnixMs, ...(host ? { host } : {}) } } });
    if (r.result.case !== "availableComponents") throw new Error(`components answered ${r.result.case}`);
    return r.result.value;
  }

  #call(request: MessageInitShape<typeof RequestSchema>): Reply {
    const data = toBinary(RequestSchema, create(RequestSchema, request));
    const x = this.#x;
    const ptr = x.alloc(data.length);
    new Uint8Array(x.memory.buffer, ptr, data.length).set(data);
    let packed: bigint;
    try {
      packed = x.call(ptr, data.length);
    } catch (e) {
      // The call owned the request (freeing it here would free it twice); a trap leaves the
      // instance's memory and stack as they were mid-call, so the next call gets a fresh one.
      this.#x = SdkSettings.#instance();
      throw new Error(`the SDK settings layer trapped: ${String(e)}`);
    }
    const outPtr = Number(packed >> 32n);
    const outLen = Number(packed & 0xffffffffn);
    const raw = new Uint8Array(x.memory.buffer, outPtr, outLen).slice();
    x.free(outPtr, outLen);
    const reply = fromBinary(ReplySchema, raw);
    if (reply.result.case === "error") throw new Error(reply.result.value);
    return reply;
  }
}
