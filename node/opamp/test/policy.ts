// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Logfire SDK policies for the tests, as a server sends them (docs/sdk-settings.md).

import { create } from "@bufbuild/protobuf";

import { AgentRemoteConfigSchema, type AgentRemoteConfig, type EffectiveConfig } from "../src/index.js";

export const FILE = "logfire-sdk-policy.json";
export const V1 = "logfire-sdk-policy/v1";
const SIGNALS = ["traces", "metrics", "logs"] as const;
// The controls a policy may leave out (as local).
const MORE_CONTROLS = [
  "sampling_rules",
  "trace_rate_limit",
  "log_levels",
  "log_rate_limit",
  "attribute_redaction",
  "debug",
  "app_settings",
  "instrumentations",
] as const;

type Fields = Record<string, unknown>;

/** The overrides of a policy: each control's fields, and signal_exports each signal's. */
export interface Overrides {
  head_sampling?: Fields;
  signal_exports?: Partial<Record<(typeof SIGNALS)[number], Fields>>;
  sampling_rules?: Fields;
  trace_rate_limit?: Fields;
  log_levels?: Fields;
  log_rate_limit?: Fields;
  attribute_redaction?: Fields;
  debug?: Fields;
  app_settings?: Fields;
  instrumentations?: Fields;
}

/**
 * A policy with every control local but the ones given. The policy is the first four controls
 * alone when it overrides only head_sampling and signal_exports, else every control, unless `every`
 * says.
 */
export function policy(overrides: Overrides = {}, every?: boolean): string {
  const all = every ?? !Object.keys(overrides).every((k) => k === "head_sampling" || k === "signal_exports");
  const control = (fields: Fields | undefined) => (fields === undefined ? { mode: "local" } : { mode: "override", ...fields });
  const doc: Record<string, unknown> = {
    schema: V1,
    head_sampling: control(overrides.head_sampling),
    signal_exports: Object.fromEntries(SIGNALS.map((s) => [s, control(overrides.signal_exports?.[s])])),
  };
  if (all) for (const name of MORE_CONTROLS) doc[name] = control(overrides[name]);
  return JSON.stringify(doc);
}

/** A policy as the effective config reports it: every control, those left out local, keys sorted. */
export function full(body: string): string {
  const doc = JSON.parse(body) as Record<string, unknown>;
  for (const name of MORE_CONTROLS) doc[name] ??= { mode: "local" };
  const sort = (v: unknown): unknown =>
    Array.isArray(v) ? v.map(sort)
    : v !== null && typeof v === "object"
      ? Object.fromEntries(Object.entries(v).sort(([a], [b]) => (a < b ? -1 : 1)).map(([k, x]) => [k, sort(x)]))
    : v;
  return JSON.stringify(sort(doc));
}

/** signal_exports's overrides that stop each signal named. */
export const off = (...signals: (typeof SIGNALS)[number][]) =>
  Object.fromEntries(signals.map((s) => [s, { enabled: false }])) as Overrides["signal_exports"];

/** A remote config whose only file is the policy `body`. */
export function config(body: string | Uint8Array, hash = "h"): AgentRemoteConfig {
  const bytes = typeof body === "string" ? new TextEncoder().encode(body) : body;
  return create(AgentRemoteConfigSchema, {
    configHash: new TextEncoder().encode(hash),
    config: { configMap: { [FILE]: { body: bytes, contentType: "application/json" } } },
  });
}

/** The policy an effective config reports, exactly as the layer writes it. It is the only file. */
export function reported(e: EffectiveConfig | undefined): string {
  const files = e?.configMap?.configMap ?? {};
  if (Object.keys(files).join() !== FILE) throw new Error(`effective config files: ${Object.keys(files).join(", ")}`);
  return new TextDecoder().decode(files[FILE]!.body);
}

/** The controls an effective config reports overridden, each with its fields but the mode. */
export function overridden(e: EffectiveConfig | undefined): Record<string, unknown> {
  const doc = JSON.parse(reported(e)) as Record<string, unknown>;
  const fields = (c: Fields) => Object.fromEntries(Object.entries(c).filter(([k]) => k !== "mode"));
  const out: Record<string, unknown> = {};
  for (const [name, value] of Object.entries(doc)) {
    if (name === "schema") continue;
    if (name === "signal_exports") {
      const signals = Object.entries(value as Record<string, Fields>).filter(([, c]) => c.mode === "override");
      if (signals.length > 0) out[name] = Object.fromEntries(signals.map(([s, c]) => [s, fields(c)]));
    } else if ((value as Fields).mode === "override") {
      out[name] = fields(value as Fields);
    }
  }
  return out;
}
