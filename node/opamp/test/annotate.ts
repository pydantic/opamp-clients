// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// A failing test that says, on a line of its own, how to replay it: a GitHub Actions error
// annotation (`::error title=...::...`), which the run's summary shows without its log. The long
// property tests (the shell contract's: callback, schedule, adversary) draw a seed per run, and a
// failure on CI is only worth something with the seed and the command that runs it again.
//
// `annotatedTest(file, env)` is node:test's `test`, whose body, when it throws, first prints the
// annotation: the file and the test's name as its title; then the line that replays the test alone
// (the environment `env()` gives, the variables that change what runs, and node --test with a
// pattern for its name); then the replay lines the failure's own message gives (a seed, a path);
// then the message. What the test checks is unchanged: the error is thrown on as it was.
//
// The annotation goes to stdout: node --test's spec reporter passes a test file's output through as
// it is (the TAP reporter puts "# " before it, which GitHub doesn't read).

import { test as nodeTest, type TestContext } from "node:test";

/** The variables that change what the shell-contract tests run, printed in a replay when set. */
export const KNOBS = [
  "OPAMP_MODEL_RUNS",
  "OPAMP_MODEL_PATH",
  "OPAMP_SCHED_RUNS",
  "OPAMP_SCHED_PATH",
  "OPAMP_SCHED_REPLAY",
  "OPAMP_SCHEDULE_RUNS",
  "OPAMP_ADVERSARY_BASE",
  "OPAMP_ADVERSARY_SEEDS",
  "OPAMP_ADVERSARY_SEED",
  "OPAMP_ADVERSARY_FOCUS",
  "OPAMP_ADVERSARY_KNOWN_SEEDS",
  "OPAMP_ADVERSARY_WASM",
] as const;

/** At most this much of a failure's message goes in its annotation (GitHub cuts long ones). */
const MESSAGE_MAX = 4000;
/** And at most this many of its replay lines. */
const REPLAYS_MAX = 10;

/** A workflow command's data, escaped: a percent sign and line ends. */
const escapeData = (s: string): string => s.replace(/%/g, "%25").replace(/\r/g, "%0D").replace(/\n/g, "%0A");
/** A workflow command's property (the title), escaped: those, and a colon and a comma. */
const escapeProperty = (s: string): string => escapeData(s).replace(/:/g, "%3A").replace(/,/g, "%2C");

/** The annotation: one line, whatever the title and message hold. */
export function errorAnnotation(title: string, message: string): string {
  return `::error title=${escapeProperty(title)}::${escapeData(message)}`;
}

/** A --test-name-pattern that matches exactly `name`. */
export function namePattern(name: string): string {
  return `^${name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`;
}

/** The line that runs one test of `file` (dist/test/<file>) again, with `env` and the knobs set now. */
export function replayLine(file: string, name: string, env: Record<string, string | number | undefined>): string {
  const vars = new Map<string, string>();
  for (const k of KNOBS) {
    const v = process.env[k];
    if (v !== undefined && v !== "") vars.set(k, v);
  }
  for (const [k, v] of Object.entries(env)) if (v !== undefined) vars.set(k, String(v));
  const prefix = [...vars].map(([k, v]) => `${k}=${v}`).join(" ");
  return `replay: ${prefix ? `${prefix} ` : ""}node --test --test-name-pattern="${namePattern(name)}" dist/test/${file} (in node/opamp, after npm ci and npx tsc -p .)`;
}

/** What a failure's annotation says: how to replay the test, the replay lines of its message, and
 * its message. */
export function failureMessage(replay: string, error: unknown): string {
  const text = error instanceof Error ? error.message : String(error);
  const own = [...new Set(text.split("\n").flatMap((l) => (l.includes("replay: ") ? [l.slice(l.indexOf("replay: "))] : [])))];
  own.splice(REPLAYS_MAX);
  const head = [replay, ...own].join("\n");
  const rest = text.length > MESSAGE_MAX ? `${text.slice(0, MESSAGE_MAX)}... (cut; the whole message is in the log)` : text;
  return `${head}\n${rest}`;
}

type Body = (t: TestContext) => unknown;

/** node:test's `test(name, body)`, annotated (above), for the tests of `file`. */
export function annotatedTest(file: string, env: () => Record<string, string | number | undefined> = () => ({})) {
  return (name: string, body: Body) =>
    nodeTest(name, async (t) => {
      try {
        await body(t);
      } catch (e) {
        process.stdout.write(`${errorAnnotation(`${file}: ${name}`, failureMessage(replayLine(file, name, env()), e))}\n`);
        throw e;
      }
    });
}
