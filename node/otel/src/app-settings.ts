// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The application's own settings, as the OpAMP server sends them (docs/app-settings.md).
//
// The server sends them beside the SDK's settings, in the `custom` map of the remote config's
// `elastic` entry: string keys to string values, which the SDK doesn't read but hands on. The
// application reads them here:
//
//     import { appSettings } from "@pydantic/opamp-otel";
//
//     if (appSettings.get("checkout.new_flow", "off") === "on") { ... }
//     const verbose = appSettings.getBool("example.verbose", false);
//     const unsubscribe = appSettings.onChange((settings, changed) => {
//       if (changed.includes("example.log_level")) logger.level = settings["example.log_level"] ?? "info";
//     });
//
// The package keeps one map per process, on globalThis: the register (`--import`) and the
// application's own import see the same values even if they load the package by two paths. This
// module is small on purpose, with nothing of the SDK in it: `@pydantic/opamp-otel/app-settings`
// loads only it.

import { diag } from "@opentelemetry/api";

/** The settings in force: every key the server sent, its value as text. Never changed in place. */
export type AppSettingsMap = Readonly<Record<string, string>>;

/** Told after a change is put in force: the whole map now in force, and the keys added, changed or removed (sorted). */
export type AppSettingsCallback = (settings: AppSettingsMap, changed: readonly string[]) => unknown;

/** Where the module reports: a change put in force (info) and a callback that failed (error). */
export interface AppSettingsLog {
  info(message: string): void;
  error(message: string): void;
}

/** The spaces a typed read takes round a value: every language trims these, and only these. */
const SPACES = /^[ \t\r\n]+|[ \t\r\n]+$/g;
const INT = /^[+-]?[0-9]+$/;
/** A JSON number (a leading + too): what every language reads the same way. */
const FLOAT = /^[+-]?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$/;

/** A map nothing changes, with no prototype: `settings["constructor"]` is undefined unless sent. */
function frozen(entries: Iterable<[string, string]>): AppSettingsMap {
  const out = Object.create(null) as Record<string, string>;
  for (const [k, v] of entries) out[k] = v;
  return Object.freeze(out);
}

const EMPTY = frozen([]);

/** A callback as added: each addition its own, so removing one leaves the same callback added again. */
interface Entry {
  readonly callback: AppSettingsCallback;
}

/**
 * The application's settings in force, and who to tell when they change. The package keeps one
 * (`appSettings`); a test may make its own.
 */
export class AppSettings {
  #values: AppSettingsMap = EMPTY;
  #callbacks: Entry[] = [];
  log: AppSettingsLog;

  constructor(log?: AppSettingsLog) {
    this.log = log ?? {
      info: (m) => diag.info(m),
      error: (m) => console.error(m),
    };
  }

  /**
   * The value of `key` in force, or `def` while there is none: before the first config (and no
   * saved one), when the config in force leaves the key out, and without an OpAMP server. Never
   * throws, whatever the key.
   */
  get(key: string, def: string): string;
  get(key: string, def?: string): string | undefined;
  get(key: string, def?: string): string | undefined {
    const values = this.#values;
    return typeof key === "string" && Object.hasOwn(values, key) ? values[key] : def;
  }

  /** `true` or `false` in any ASCII case, spaces round it allowed; `def` for anything else (`on`, `1` and `yes` too). */
  getBool(key: string, def: boolean): boolean {
    const text = this.#text(key);
    if (text === undefined || !/^[A-Za-z]+$/.test(text)) return def;
    const lower = text.replace(/[A-Z]/g, (c) => c.toLowerCase());
    return lower === "true" ? true : lower === "false" ? false : def;
  }

  /**
   * A decimal integer (a sign allowed, leading zeros too, spaces round it); `def` for anything
   * else. Only a safe integer (±2^53 - 1) is one: a number beyond can't be held exactly, so it is
   * `def` too, where the other languages take the whole 64-bit range.
   */
  getInt(key: string, def: number): number {
    const text = this.#text(key);
    if (text === undefined || !INT.test(text)) return def;
    const n = Number(text);
    // Beyond ±2^53 - 1, Number rounds: never to a safe integer, so a rounded value is never taken.
    return Number.isSafeInteger(n) ? n + 0 : def; // + 0: -0 is 0
  }

  /** A number as JSON writes one (a sign allowed, spaces round it), finite; `def` for anything else. */
  getFloat(key: string, def: number): number {
    const text = this.#text(key);
    if (text === undefined || !FLOAT.test(text)) return def;
    const n = Number(text);
    return Number.isFinite(n) ? n : def;
  }

  /** Every setting in force, frozen: one consistent map, whatever changes after. */
  snapshot(): AppSettingsMap {
    return this.#values;
  }

  /**
   * Call `callback(settings, changed)` after each change put in force (not for the values in
   * force now: read those with snapshot()). Returns a function that removes it; calling that again
   * does nothing. The same callback added twice is called twice.
   */
  onChange(callback: AppSettingsCallback): () => void {
    if (typeof callback !== "function") throw new TypeError("appSettings.onChange: the callback must be a function");
    const entry: Entry = { callback };
    // A new array each time: a change being told goes on with the callbacks it started with.
    this.#callbacks = [...this.#callbacks, entry];
    return () => {
      this.#callbacks = this.#callbacks.filter((e) => e !== entry);
    };
  }

  /**
   * Put `values` in force (the layer's Settings.custom), and tell the callbacks if anything
   * changed. The package calls it; an application doesn't.
   */
  put(values: Readonly<Record<string, string>> | undefined): void {
    const next = frozen(Object.entries(values ?? {})
      .filter(([, v]) => typeof v === "string")
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
    const old = this.#values;
    const changed = [...new Set([...Object.keys(old), ...Object.keys(next)])]
      .filter((k) => old[k] !== next[k])
      .sort();
    if (changed.length === 0) return;
    this.#values = Object.keys(next).length ? next : EMPTY;
    try {
      this.log.info(`OpAMP: application settings changed: ${changed.join(", ")}`);
    } catch {
      // The values are in force: the callbacks are still told.
    }
    const told = this.#values;
    const keys = Object.freeze(changed);
    for (const { callback } of this.#callbacks) {
      try {
        const result = callback(told, keys);
        if (result !== null && typeof (result as PromiseLike<unknown> | undefined)?.then === "function") {
          // Promise.resolve: a thenable's then is called as a promise's is, later, and anything it throws is a rejection.
          Promise.resolve(result).catch((e: unknown) => this.#failed(callback, e));
        }
      } catch (e) {
        this.#failed(callback, e);
      }
    }
  }

  #failed(callback: AppSettingsCallback, e: unknown): void {
    const name = callback.name || "anonymous";
    const why = e instanceof Error ? (e.stack ?? String(e)) : String(e);
    try {
      this.log.error(`OpAMP: an application settings callback (${name}) failed: ${why}`);
    } catch {
      // A log that throws: nothing more to do, and the client goes on.
    }
  }

  #text(key: string): string | undefined {
    return this.get(key)?.replace(SPACES, "");
  }
}

const GLOBAL = Symbol.for("@pydantic/opamp-otel.appSettings");

/**
 * The package's: what the server's config puts in force, and what the application reads. One per
 * process (per realm: a worker thread has its own, which no server config reaches).
 */
export const appSettings: AppSettings = ((globalThis as Record<symbol, unknown>)[GLOBAL] as AppSettings | undefined)
  ?? ((globalThis as Record<symbol, unknown>)[GLOBAL] = new AppSettings());
