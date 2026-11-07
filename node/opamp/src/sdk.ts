// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// What an OpenTelemetry SDK does with the settings an OpAMP server sends it, decided the same way
// in every language: the SDK settings layer (a WebAssembly module,
// core/opamp_sdk_settings.wasm), run in Node's own WebAssembly engine. It keeps nothing between
// calls; the caller keeps the agent flow's state it is handed and passes it back with the next
// step. See docs/sdk-settings.md.

import { readFileSync } from "node:fs";

import "./utf8.js";

import { create, fromBinary, type MessageInitShape, toBinary } from "@bufbuild/protobuf";

import {
  type AgentOutput,
  type AgentStartSchema,
  type AgentStepSchema,
  type ExportersDecided,
  type FoundExporterSchema,
  type HostSchema,
  type InventorySchema,
  type Reply,
  ReplySchema,
  RequestSchema,
} from "./gen/opamp/sdk/v1/settings_pb.js";
import type { KeyValue } from "./gen/opamp/v1/anyvalue_pb.js";
import type { AgentDescription, AvailableComponents } from "./gen/opamp/v1/opamp_pb.js";

export * from "./gen/opamp/sdk/v1/settings_pb.js";

/**
 * What an SDK carries out. A policy that overrides a setting it does not name is refused
 * (docs/sdk-settings.md#the-host).
 */
export type HostInit = MessageInitShape<typeof HostSchema>;

/** What the integration lists of itself and its SDK (docs/available-components.md#what-the-sdk-integrations-report). */
export type InventoryInit = MessageInitShape<typeof InventorySchema>;
/** The agent flow's start (`start`). */
export type AgentStartInit = MessageInitShape<typeof AgentStartSchema>;
/** One input to the agent flow (`step`). */
export type AgentStepInit = MessageInitShape<typeof AgentStepSchema>;
/** An exporter as an integration found it on its SDK's providers, for `exporters`. */
export type FoundExporterInit = MessageInitShape<typeof FoundExporterSchema>;

let compiled: WebAssembly.Module | undefined;

function module(): WebAssembly.Module {
  compiled ??= new WebAssembly.Module(readFileSync(new URL("../../core/opamp_sdk_settings.wasm", import.meta.url)));
  return compiled;
}

let unavailable: { reason: string | undefined } | undefined;

/**
 * Why the SDK settings layer can't be loaded here (its module missing, a file that isn't one, or
 * one that won't instantiate), or undefined when it can: found out once per process. An
 * integration asks before it makes one, and runs unmanaged when the layer can't be loaded.
 */
export function settingsUnavailable(): string | undefined {
  if (unavailable === undefined) {
    try {
      new WebAssembly.Instance(module(), {});
      unavailable = { reason: undefined };
    } catch (e) {
      unavailable = { reason: String(e) };
    }
  }
  return unavailable.reason;
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
   * The agent flow's start, before the SDK is built: its state, the effects to carry out in order
   * (the settings to put in force first), and when to step it next (docs/sdk-settings.md, "The
   * agent flow"). A host naming what is no setting throws.
   */
  start(start: AgentStartInit): AgentOutput {
    const r = this.#call({ call: { case: "agentStart", value: start } });
    if (r.result.case !== "agentOutput") throw new Error(`agentStart answered ${r.result.case}`);
    return r.result.value;
  }

  /**
   * One input to the agent flow (`step.agent`: the state the last output gave): the new state, the
   * effects to carry out in order, and when to step it next.
   */
  step(step: AgentStepInit): AgentOutput {
    const r = this.#call({ call: { case: "agentStep", value: step } });
    if (r.result.case !== "agentOutput") throw new Error(`agentStep answered ${r.result.case}`);
    return r.result.value;
  }

  /**
   * The agent description, from the SDK's resource attributes. It also names the policy formats the
   * SDK reads and the controls `host` carries out (left out: none). `nowUnixMs`: the wall clock
   * (0: none, and the SDK cannot carry out debug mode).
   */
  describe(resource: KeyValue[], host?: HostInit, nowUnixMs = 0n): AgentDescription {
    const r = this.#call({ call: { case: "describe", value: { resource, nowUnixMs, ...(host ? { host } : {}) } } });
    if (r.result.case !== "description") throw new Error(`describe answered ${r.result.case}`);
    return r.result.value;
  }

  /**
   * What the instance runs, as its available components, for the client's
   * `setAvailableComponents` (the hash left empty: the client computes it). Once the SDK is built,
   * and again when what it lists changes. `host` and `nowUnixMs` as for `describe`.
   */
  components(inventory: InventoryInit, host?: HostInit, nowUnixMs = 0n): AvailableComponents {
    const r = this.#call({ call: { case: "components", value: { inventory, nowUnixMs, ...(host ? { host } : {}) } } });
    if (r.result.case !== "availableComponents") throw new Error(`components answered ${r.result.case}`);
    return r.result.value;
  }

  /**
   * What Fleet is told of the exporters found on the SDK's providers, decided the same in every
   * language: the name each is listed under, where it sends (its own endpoint, else the
   * `OTEL_EXPORTER_*` variables in `env`), the governed and ungoverned signals, and a warning for
   * each ungoverned exporter. At start and when the providers change; never per span.
   */
  exporters(found: FoundExporterInit[], env: Record<string, string | undefined> = {}, defaultProtocol = ""): ExportersDecided {
    const vars = Object.entries(env).flatMap(([name, value]) =>
      name.startsWith("OTEL_EXPORTER_") && value !== undefined ? [{ name, value }] : []);
    const r = this.#call({ call: { case: "exporters", value: { found, env: vars, defaultProtocol } } });
    if (r.result.case !== "exportersDecided") throw new Error(`exporters answered ${r.result.case}`);
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
