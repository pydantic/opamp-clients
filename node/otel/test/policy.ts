// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Logfire SDK policies for the tests, as a server sends them (docs/sdk-settings.md).

import { type AgentRemoteConfig, AgentRemoteConfigSchema, create, type EffectiveConfig } from "@pydantic/opamp-client";

export const FILE = "logfire-sdk-policy.json";
export const V1 = "logfire-sdk-policy/v1";
export const V2 = "logfire-sdk-policy/v2-draft";
const SIGNALS = ["traces", "metrics", "logs"] as const;
const V2_CONTROLS = [
  "sampling_rules",
  "trace_rate_limit",
  "log_levels",
  "log_rate_limit",
  "attribute_redaction",
  "debug",
  "app_settings",
  "instrumentations",
  "polling_interval",
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
  polling_interval?: Fields;
}

/**
 * A policy with every control local but the ones given. The policy is version 1 when it overrides
 * only head_sampling and signal_exports, else version 2, unless `version` says.
 */
export function policy(overrides: Overrides = {}, version?: 1 | 2): string {
  const v = version ?? (Object.keys(overrides).every((k) => k === "head_sampling" || k === "signal_exports") ? 1 : 2);
  const control = (fields: Fields | undefined) => (fields === undefined ? { mode: "local" } : { mode: "override", ...fields });
  const doc: Record<string, unknown> = {
    schema: v === 1 ? V1 : V2,
    head_sampling: control(overrides.head_sampling),
    signal_exports: Object.fromEntries(SIGNALS.map((s) => [s, control(overrides.signal_exports?.[s])])),
  };
  if (v === 2) for (const name of V2_CONTROLS) doc[name] = control(overrides[name]);
  return JSON.stringify(doc);
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
