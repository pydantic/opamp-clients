// The client's saved state (opamp-state) holding a config applied, as the client leaves it once
// the server has heard APPLIED: what the next start's settings come from. A test without a client
// writes it here.
//
// The bytes are a state the client saved before it kept the config applied (the shared vectors'
// testdata/sdk-settings/start.json), with the config applied added: its field 16, a message, which
// protobuf takes from the end of the encoding.

import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import { AgentRemoteConfigSchema, create, toBinary } from "@pydantic/opamp-client";

import { FILE } from "./policy.js";

const BEFORE = "a core state saved before the core kept the config applied: the policy kept before carries over";

function varint(n: number): number[] {
  const out: number[] = [];
  for (;;) {
    const byte = n & 0x7f;
    n = Math.floor(n / 128);
    out.push(byte | (n ? 0x80 : 0));
    if (!n) return out;
  }
}

/** A client state whose config applied has `document` as its policy (undefined: a config with no policy). */
export function coreState(document: Uint8Array | string | undefined): Uint8Array {
  const start = JSON.parse(
    readFileSync(new URL("../../../../testdata/sdk-settings/start.json", import.meta.url), "utf8"),
  ) as { cases: { name: string; core_state?: string }[] };
  const base = Buffer.from(start.cases.find((c) => c.name === BEFORE)!.core_state!, "hex");
  const body = typeof document === "string" ? new TextEncoder().encode(document) : document;
  const applied = toBinary(AgentRemoteConfigSchema, create(AgentRemoteConfigSchema, {
    configHash: new TextEncoder().encode("applied"),
    config: { configMap: body === undefined ? {} : { [FILE]: { body, contentType: "application/json" } } },
  }));
  return new Uint8Array([...base, 0x82, 0x01, ...varint(applied.length), ...applied]);
}

/** Leave in `dir` what the client would after a config with `document` was applied. */
export function keepApplied(dir: string, document: Uint8Array | string | undefined): void {
  writeFileSync(join(dir, "opamp-state"), coreState(document));
}
