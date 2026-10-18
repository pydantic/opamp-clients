// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// The shared OpAMP client core (a WebAssembly module, core/opamp_core.wasm), run in Node's
// own WebAssembly engine: nothing native to install. Each Core is one module instance, holding
// one client's state.

import { closeSync, fchmodSync, openSync, readFileSync, writeSync } from "node:fs";

import "./utf8.js";

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";

import {
  type Config,
  ConfigSchema,
  type Input,
  type Limits,
  type Output,
  OutputSchema,
  ReplySchema,
  type SetupIdentified,
  type SetupIdentity,
  type SetupStart,
  type SetupStarted,
  SetupCallSchema,
  SetupReplySchema,
  StepSchema,
} from "./gen/opamp/core/v1/core_pb.js";

let compiled: WebAssembly.Module | undefined;

/** The module, compiled once per process. */
function module(): WebAssembly.Module {
  compiled ??= new WebAssembly.Module(readFileSync(new URL("../../core/opamp_core.wasm", import.meta.url)));
  return compiled;
}

interface Exports {
  memory: WebAssembly.Memory;
  alloc(len: number): number;
  free(ptr: number, len: number): void;
  init(ptr: number, len: number): bigint;
  step(ptr: number, len: number): bigint;
}

/**
 * Core tap: with OPAMP_CORE_TAP=<path> set, every call into the core is appended to a file for the
 * effects checker (opamp-tap). Each record is a kind byte (1 init in, 2 init out, 3 step in,
 * 4 step out), a u32 LE length, a u64 LE monotonic time in nanoseconds, then the bytes exactly as
 * they crossed (Config, Reply, Step, Reply); and 5, a file operation the client carried out for
 * the core (a JSON object: src/files.ts). A process's first core writes <path>, each later one
 * <path>.<pid>.<n>. Unset, it costs nothing.
 */
class Tap {
  static #cores = 0;
  readonly #fd: number;
  #closed = false;

  private constructor(fd: number) {
    this.#fd = fd;
  }

  /** The tap for a new core, if OPAMP_CORE_TAP is set (or `at`: that file, for tests). */
  static open(at?: string): Tap | undefined {
    const base = process.env.OPAMP_CORE_TAP;
    if (!base && !at) return undefined;
    let path = at;
    if (path === undefined) {
      const n = Tap.#cores++;
      path = n === 0 ? base! : `${base}.${process.pid}.${n}`;
    }
    try {
      // It holds the Config's headers (the enrollment key) and what is saved: the file is the
      // user's only, as the state file is, whether new or already there (open's mode is for a new
      // file only).
      const fd = openSync(path, "a", 0o600);
      try {
        if (process.platform !== "win32") fchmodSync(fd, 0o600);
      } catch (e) {
        closeSync(fd);
        throw e;
      }
      return new Tap(fd);
    } catch (e) {
      console.error(`OPAMP_CORE_TAP: can't open ${path}: ${String(e)}`);
      return undefined;
    }
  }

  /** One record, in one write: a process killed leaves whole records. */
  record(kind: number, bytes: Uint8Array): void {
    if (this.#closed) return;
    const head = Buffer.alloc(13);
    head.writeUInt8(kind, 0);
    head.writeUInt32LE(bytes.length, 1);
    head.writeBigUInt64LE(process.hrtime.bigint(), 5);
    writeSync(this.#fd, Buffer.concat([head, bytes]));
  }

  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    closeSync(this.#fd);
  }
}

/** Kind 5: a file operation the client carried out for the core (the tap checker's file records). */
const FILE = 5;

/** Tests only: a client Config carrying this key runs that module (bytes, or compiled) instead
 * of the embedded core, as the adversary's harness does. core.js is not a package export,
 * so an application can't reach it. */
export const TEST_CORE_MODULE = Symbol("opamp test core module");

/** Tests only: a client Config carrying this key taps its core to that file, as OPAMP_CORE_TAP
 * would (but that file, whatever cores came before). */
export const TEST_CORE_TAP = Symbol("opamp test core tap");

/** The core refused an input (a bug in this client) or trapped (a bug in the core). */
export class CoreError extends Error {}

/** One instance of the core: one client's state. */
export class Core {
  readonly #x: Exports;
  readonly #tap: Tap | undefined;
  /** The instance UID in use, as the last output said (16 bytes). */
  #uid: Uint8Array = new Uint8Array();
  #limits: Limits | undefined;

  /** `wasm`: another module than the embedded core (tests only); `tap`: the file to tap it to
   * (tests only; otherwise OPAMP_CORE_TAP says). */
  constructor(config: Config, wasm?: Uint8Array | WebAssembly.Module, tap?: string) {
    this.#tap = Tap.open(tap);
    const m = wasm === undefined ? module() : wasm instanceof WebAssembly.Module ? wasm : new WebAssembly.Module(wasm as Uint8Array<ArrayBuffer>);
    this.#x = new WebAssembly.Instance(m, {}).exports as unknown as Exports;
    this.#call(this.#x.init, toBinary(ConfigSchema, config), 1);
  }

  /** Feed the core one input at `nowMs` (milliseconds on a monotonic clock). */
  step(nowMs: number, input: Input): Output {
    const step = create(StepSchema, { nowMs: BigInt(Math.floor(nowMs)), unixMs: BigInt(Date.now()), input });
    return this.#call(this.#x.step, toBinary(StepSchema, step), 3);
  }

  /** Whether the core is tapped: then the client records its file operations with `file`. */
  get tapped(): boolean {
    return this.#tap !== undefined;
  }

  /** Record a file operation the client carried out for the core, once it succeeded and before
   * anything that depends on it reaches the core. */
  file(record: object): void {
    this.#tap?.record(FILE, Buffer.from(JSON.stringify(record)));
  }

  /** Release the tap's file, if there is one. */
  close(): void {
    this.#tap?.close();
  }

  #call(fn: (ptr: number, len: number) => bigint, data: Uint8Array, kind: number): Output {
    const x = this.#x;
    this.#tap?.record(kind, data);
    const ptr = x.alloc(data.length);
    new Uint8Array(x.memory.buffer, ptr, data.length).set(data);
    let packed: bigint;
    try {
      packed = fn(ptr, data.length);
    } catch (e) {
      // The call owned the input (freeing it here would free it twice). A trap ends the client,
      // and this instance with it.
      throw new CoreError(`trap: ${String(e)}`);
    }
    const outPtr = Number(packed >> 32n);
    const outLen = Number(packed & 0xffffffffn);
    // Copy out before freeing: the memory may be reused (or grow, detaching the view).
    const raw = new Uint8Array(x.memory.buffer, outPtr, outLen).slice();
    x.free(outPtr, outLen);
    this.#tap?.record(kind + 1, raw);
    const reply = fromBinary(ReplySchema, raw);
    if (reply.error) {
      throw new CoreError(reply.error);
    }
    const out = reply.output ?? create(OutputSchema);
    if (out.instanceUid.length === 16) this.#uid = out.instanceUid;
    if (out.limits !== undefined) this.#limits = out.limits;
    return out;
  }

  /** The limits the core goes by, as given or by default: its start reports them. */
  get limits(): Limits | undefined {
    return this.#limits;
  }

  /** The instance UID in use (the core's, if the application gave none). */
  get instanceUid(): Uint8Array {
    return this.#uid;
  }
}

let unavailable: { reason: string | undefined } | undefined;

/**
 * Why the client core can't be loaded here (its module missing, a file that isn't one, or one
 * that won't instantiate), or undefined when it can: found out once per process. An integration
 * asks before its Setup call, and runs unmanaged when the core can't be loaded; the calls into it
 * throw then. (The SDK settings layer's module: settingsUnavailable, in ./sdk.)
 */
export function coreUnavailable(): string | undefined {
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

interface SetupExports {
  memory: WebAssembly.Memory;
  alloc(len: number): number;
  free(ptr: number, len: number): void;
  setup(ptr: number, len: number): bigint;
}

/** The core's Setup call, on an instance of its own: stateless, it needs no client. */
function setupCall(call: Parameters<typeof create<typeof SetupCallSchema>>[1]) {
  const x = new WebAssembly.Instance(module(), {}).exports as unknown as SetupExports;
  const data = toBinary(SetupCallSchema, create(SetupCallSchema, call));
  const ptr = x.alloc(data.length);
  new Uint8Array(x.memory.buffer, ptr, data.length).set(data);
  let packed: bigint;
  try {
    packed = x.setup(ptr, data.length);
  } catch (e) {
    throw new CoreError(`trap: ${String(e)}`);
  }
  const outPtr = Number(packed >> 32n);
  const outLen = Number(packed & 0xffffffffn);
  const reply = fromBinary(SetupReplySchema, new Uint8Array(x.memory.buffer, outPtr, outLen).slice());
  if (reply.result.case === "error") throw new CoreError(reply.result.value);
  return reply.result;
}

/**
 * Where the server is and how to reach it, from the environment and what the code gives: the
 * core's Setup call (an OpenTelemetry integration's start-up decisions, the same in every
 * language). `off` without an endpoint. Throws CoreError on a failure of the core.
 */
export function startSetup(start: SetupStart): SetupStarted {
  const r = setupCall({ call: { case: "start", value: start } });
  if (r.case !== "started") throw new CoreError("setup: no answer to start");
  return r.value;
}

/** The instance UID, once the state directory is known (and its slot locked): the core's Setup
 * call. Throws CoreError on a failure of the core, or a call it can't use. */
export function identifySetup(identity: SetupIdentity): SetupIdentified {
  const r = setupCall({ call: { case: "identity", value: identity } });
  if (r.case !== "identified") throw new CoreError("setup: no answer to identity");
  return r.value;
}
