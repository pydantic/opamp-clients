// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// scrub_defaults and scrub_patterns: the Logfire SDK's scrubbing, at the span and log exporters'
// wrappers (docs/sdk-settings.md, "Scrubbing"). The matchers are the layer's, character for
// character: the shared vectors (scrub.json) and its reference (Evaluate) hold them to it.

/**
 * The keys whose values scrubbing never touches, at any depth: the Logfire SDK's SAFE_KEYS
 * (logfire 5.2.0). Each is matched exactly, case and all.
 */
export const SAFE_KEYS: ReadonlySet<string> = new Set([
  "agent_session_id", "binary_content", "code.filepath", "code.function", "code.lineno", "db.plan",
  "db.query.text", "db.statement", "do_not_scrub", "error.type", "event.name", "exception.message",
  "exception.stacktrace", "exception.type", "fastapi.route.name", "fastapi.route.operation_id",
  "gen_ai.conversation.id", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.provider.name",
  "gen_ai.request.model", "gen_ai.response.model", "gen_ai.system", "gen_ai.system_instructions",
  "gen_ai.tool.call.id", "gen_ai.tool.definitions", "gen_ai.tool.name",
  "gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.cache_read.input_tokens",
  "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.raw", "http.method",
  "http.route", "http.scheme", "http.status_code", "http.target", "http.url",
  "langsmith.metadata.session_id", "langsmith.trace.session_name", "logfire.config",
  "logfire.json_schema", "logfire.level_name", "logfire.level_num", "logfire.logger_name", "logfire.msg",
  "logfire.msg_template", "logfire.package_versions", "logfire.pending_parent_id", "logfire.sample_rate",
  "logfire.scrubbed", "logfire.span_type", "logfire.tags", "logfire.version", "model_request_parameters",
  "pydantic_ai.all_messages", "rpc.method", "url.full", "url.path", "url.query",
]);

/**
 * The kinds of the Logfire SDK's default patterns. word: the word, as it is. auth: `auth`, unless
 * `or` or `ors` and a word boundary follow (author, authors). pair: two words with at most one of
 * `.`, `_`, ` `, `-` between (api key, api_key). token: `pylf_v`, digits, `_` (a Logfire token's
 * prefix). acronym: the word on its own, a word boundary or a `_` (part of the match) on each side.
 */
type Pattern =
  | { kind: "word"; word: string }
  | { kind: "auth" }
  | { kind: "pair"; a: string; b: string }
  | { kind: "token" }
  | { kind: "acronym"; word: string };

const w = (word: string): Pattern => ({ kind: "word", word });
const pair = (a: string, b: string): Pattern => ({ kind: "pair", a, b });
const acronym = (word: string): Pattern => ({ kind: "acronym", word });

/**
 * The Logfire SDK's default patterns (logfire 5.2.0's DEFAULT_PATTERNS), in its order: at a
 * position, the first that matches there is the match.
 */
const DEFAULTS: readonly Pattern[] = [
  w("password"), w("passwd"), w("mysql_pwd"), w("secret"), { kind: "auth" }, w("credential"),
  pair("private", "key"), pair("api", "key"), w("session"), w("cookie"), pair("social", "security"),
  pair("credit", "card"), pair("logfire", "token"), { kind: "token" }, acronym("csrf"), acronym("xsrf"),
  acronym("jwt"), acronym("ssn"),
];

/**
 * Whether the text's character c is the pattern's character p, as Python's re.IGNORECASE has it: a
 * lowercase ASCII letter also matches its uppercase, and `i` also `İ` and `ı`, `s` also `ſ`, `k`
 * also the Kelvin sign; any other character only itself.
 */
function sameLetter(p: string, c: string | undefined): boolean {
  if (c === undefined) return false;
  if (p < "a" || p > "z") return p === c;
  if (c === p || c === p.toUpperCase()) return true;
  switch (p) {
    case "i":
      return c === "İ" || c === "ı";
    case "s":
      return c === "ſ";
    case "k":
      return c === "K";
  }
  return false;
}

/** Whether t[i] is a word character (an ASCII letter, digit or `_`); outside t, none. */
function isWord(t: readonly string[], i: number): boolean {
  const c = t[i];
  return c !== undefined && /^[A-Za-z0-9_]$/.test(c);
}

/** Whether there is a word boundary before t[at]. */
const boundary = (t: readonly string[], at: number): boolean => isWord(t, at - 1) !== isWord(t, at);

/** Where `word` ends if it is at t[at:], else -1. */
function wordAt(t: readonly string[], at: number, word: string): number {
  let i = at;
  for (const p of word) {
    if (!sameLetter(p, t[i])) return -1;
    i++;
  }
  return i;
}

const isDigit = (c: string | undefined): boolean => c !== undefined && c >= "0" && c <= "9";

/**
 * Where `d` ends if it matches at t[i:], else -1. Where a pattern could match two ways, it takes
 * the way Python's regex tries first (a separator, then none).
 */
function defaultAt(t: readonly string[], i: number, d: Pattern): number {
  switch (d.kind) {
    case "word":
      return wordAt(t, i, d.word);
    case "auth": {
      const end = wordAt(t, i, "auth");
      if (end < 0) return -1;
      const or = wordAt(t, end, "or");
      if (or >= 0 && ((sameLetter("s", t[or]) && boundary(t, or + 1)) || boundary(t, or))) return -1;
      return end;
    }
    case "pair": {
      const mid = wordAt(t, i, d.a);
      if (mid < 0) return -1;
      const c = t[mid];
      if (c === "." || c === "_" || c === " " || c === "-") {
        const end = wordAt(t, mid + 1, d.b);
        if (end >= 0) return end;
      }
      return wordAt(t, mid, d.b);
    }
    case "token": {
      let j = wordAt(t, i, "pylf_v");
      if (j < 0) return -1;
      const digits = j;
      while (isDigit(t[j])) j++;
      return j > digits && t[j] === "_" ? j + 1 : -1;
    }
    case "acronym": {
      const after = (end: number): number => {
        if (end < 0) return -1;
        if (boundary(t, end)) return end;
        return t[end] === "_" ? end + 1 : -1;
      };
      if (boundary(t, i)) {
        const end = after(wordAt(t, i, d.word));
        if (end >= 0) return end;
      }
      return t[i] === "_" ? after(wordAt(t, i + 1, d.word)) : -1;
    }
  }
}

/**
 * The first match of a default pattern in t, as Python's re.search finds it: the leftmost start,
 * and there the first pattern in their order. [start, end]; undefined: none.
 */
function findDefault(t: readonly string[]): [number, number] | undefined {
  for (let i = 0; i < t.length; i++) {
    for (const d of DEFAULTS) {
      const end = defaultAt(t, i, d);
      if (end >= 0) return [i, end];
    }
  }
  return undefined;
}

/** What a scrubbed value becomes, naming why: the text a default pattern matched, or the pattern. */
export const scrubbed = (why: string): string => `[Scrubbed due to '${why}']`;

/** ASCII letters lowercase, every other character as it is (`É` stays `É`). */
export const asciiLower = (s: string): string => s.replace(/[A-Z]+/g, (m) => m.toLowerCase());

/**
 * Whether the text `t` matches the glob `p`, both as code points: `*` any run of characters, none
 * included, every other character itself. A character at a time: on a mismatch the last `*` takes
 * one more character and the pattern after it starts again (at most pattern x text steps, the
 * layer's algorithm).
 */
export function globChars(p: readonly string[], t: readonly string[]): boolean {
  let pi = 0;
  let ti = 0;
  let star: [number, number] | undefined;
  while (ti < t.length) {
    if (p[pi] === "*") {
      pi += 1;
      star = [pi, ti];
    } else if (pi < p.length && p[pi] === t[ti]) {
      pi += 1;
      ti += 1;
    } else if (star) {
      star = [star[0], star[1] + 1];
      [pi, ti] = star;
    } else {
      return false;
    }
  }
  return p.slice(pi).every((c) => c === "*");
}

/** What scrubbing does with an attribute by its key: leave it, replace its value (with this string), or scrub its values. */
export type KeyScrub = { keep: true } | { replace: string } | { values: true };

const KEEP: KeyScrub = { keep: true };
const VALUES: KeyScrub = { values: true };

/** A log body's or an attribute value's map: its members, by key. */
type Members = { [key: string]: unknown };

/** A plain object, its prototype Object's or none (Object.create(null)): the OTLP exporter writes either as a map. */
const isMembers = (v: unknown): v is Members => {
  if (typeof v !== "object" || v === null || Array.isArray(v) || ArrayBuffer.isView(v)) return false;
  const proto: unknown = Object.getPrototypeOf(v);
  return proto === Object.prototype || proto === null;
};

/**
 * scrub_defaults and scrub_patterns: the Logfire SDK's default patterns, and globs (lowercase, as
 * the layer hands them, sorted) matched ignoring ASCII case against a whole key or string value.
 */
export class Scrubbing {
  readonly defaults: boolean;
  readonly #patterns: { text: string; chars: string[] }[];

  constructor(defaults = false, patterns: readonly string[] = []) {
    this.defaults = defaults;
    this.#patterns = patterns.map((p) => ({ text: p, chars: Array.from(p) }));
  }

  /** Whether scrubbing changes anything at all. */
  get on(): boolean {
    return this.defaults || this.#patterns.length > 0;
  }

  /**
   * What becomes of the attribute (or a map's member) `key`: kept (scrubbing off, or one of
   * SAFE_KEYS), its value replaced (a default pattern matches anywhere in the key, or a pattern
   * matches it), or its values scrubbed.
   */
  key(key: string): KeyScrub {
    if (!this.on || SAFE_KEYS.has(key)) return KEEP;
    if (this.defaults) {
      const t = Array.from(key);
      const m = findDefault(t);
      if (m) return { replace: scrubbed(t.slice(m[0], m[1]).join("")) };
    }
    const p = this.#pattern(key);
    return p === undefined ? VALUES : { replace: scrubbed(p) };
  }

  /**
   * What a string value or a log body becomes; undefined: kept. A default pattern scrubs it when
   * it matches part of it (a value that is all the match, `password` alone, says nothing secret and
   * is kept, as the Logfire SDK keeps it); a pattern when it matches it whole.
   */
  text(text: string): string | undefined {
    if (!this.on) return undefined;
    if (this.defaults) {
      const t = Array.from(text);
      const m = findDefault(t);
      if (m && (m[0] !== 0 || m[1] !== t.length)) return scrubbed(t.slice(m[0], m[1]).join(""));
    }
    const p = this.#pattern(text);
    return p === undefined ? undefined : scrubbed(p);
  }

  /** The first pattern that matches `text` whole, ignoring ASCII case. */
  #pattern(text: string): string | undefined {
    if (this.#patterns.length === 0) return undefined;
    const t = Array.from(asciiLower(text));
    return this.#patterns.find((p) => globChars(p.chars, t))?.text;
  }

  /**
   * A value as scrubbing leaves it: an attribute's or a map member's (`key` its key), or a log
   * body or a list's item (`key` undefined). The same value when nothing changes; the value given
   * is never changed. A string is scrubbed, a list's strings each, a map's members each as an
   * attribute; any other value is kept.
   */
  value<T>(key: string | undefined, value: T): T | string {
    if (key !== undefined) {
      const what = this.key(key);
      if ("keep" in what) return value;
      if ("replace" in what) return what.replace;
    }
    if (typeof value === "string") return this.text(value) ?? value;
    if (Array.isArray(value)) {
      let out: unknown[] | undefined;
      for (const [i, item] of value.entries()) {
        const s = this.value(undefined, item);
        if (s === item) continue;
        out ??= [...value];
        out[i] = s;
      }
      return (out ?? value) as T;
    }
    if (isMembers(value)) {
      let out: Members | undefined;
      for (const [k, member] of Object.entries(value)) {
        const s = this.value(k, member);
        if (s === member) continue;
        out ??= { ...value };
        out[k] = s;
      }
      return (out ?? value) as T;
    }
    return value;
  }
}
