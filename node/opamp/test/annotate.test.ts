// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// annotate.ts: a failing test's GitHub annotation is one line that says how to replay it, and the
// test fails as it would have.

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { errorAnnotation, failureMessage, KNOBS, namePattern, replayLine } from "./annotate.js";

test("an annotation is one line, its title and message escaped", () => {
  assert.equal(errorAnnotation("a: b, c", "50%\nnext\r"), "::error title=a%3A b%2C c::50%25%0Anext%0D");
});

test("a name pattern matches the name alone", () => {
  const name = "node mode: the shell contract holds (fast-check's scheduler) [x] $1.5 ^ok";
  const re = new RegExp(namePattern(name));
  assert.ok(re.test(name));
  assert.ok(!re.test(`${name} too`));
  assert.ok(!re.test(`a ${name}`));
});

test("a replay line carries the seed given and the knobs set", () => {
  // Only this knob set, whatever the run's environment sets (CI sets OPAMP_MODEL_RUNS).
  const before = new Map(KNOBS.map((k) => [k, process.env[k]]));
  for (const k of KNOBS) delete process.env[k];
  process.env.OPAMP_SCHED_RUNS = "20000";
  try {
    const line = replayLine("schedule.properties.test.js", "a (b)", { OPAMP_SCHED_SEED: 42 });
    assert.equal(
      line,
      'replay: OPAMP_SCHED_RUNS=20000 OPAMP_SCHED_SEED=42 node --test --test-name-pattern="^a \\(b\\)$" dist/test/schedule.properties.test.js (in node/opamp, after npm ci and npx tsc -p .)',
    );
  } finally {
    for (const [k, v] of before) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  }
});

test("a failure's message puts its own replay lines first, after the test's", () => {
  const error = new Error("2 of 500 seeds failed:\nseed 7:\n  bad\n  replay: OPAMP_ADVERSARY_SEED=7 node --test x\nseed 9: v core_side=1 replay: OPAMP_ADVERSARY_SEED=9 node --test x");
  const text = failureMessage("replay: whole", error);
  assert.deepEqual(text.split("\n").slice(0, 3), [
    "replay: whole",
    "replay: OPAMP_ADVERSARY_SEED=7 node --test x",
    "replay: OPAMP_ADVERSARY_SEED=9 node --test x",
  ]);
  assert.ok(text.endsWith(error.message));
  assert.ok(failureMessage("r", "x".repeat(10_000)).length < 5_000);
});

test("a failing test prints its annotation on a line of its own, and still fails", () => {
  const dir = mkdtempSync(join(tmpdir(), "opamp-annotate-"));
  try {
    const annotate = fileURLToPath(new URL("./annotate.js", import.meta.url));
    const file = join(dir, "fails.test.mjs");
    writeFileSync(
      file,
      `import { annotatedTest } from ${JSON.stringify(annotate)};\n` +
        `const test = annotatedTest("fails.test.js", () => ({ OPAMP_MODEL_SEED: 5 }));\n` +
        `test("it fails", () => { throw new Error("broken\\nreplay: OPAMP_MODEL_SEED=5 OPAMP_MODEL_PATH=1:2"); });\n` +
        `test("it passes", () => {});\n`,
    );
    // (Run as a test run of its own, not as a file of this one: no NODE_TEST_CONTEXT.)
    const env = { ...process.env };
    delete env.NODE_TEST_CONTEXT;
    const r = spawnSync(process.execPath, ["--test", "--test-reporter=spec", file], { encoding: "utf8", env });
    assert.notEqual(r.status, 0, "the test still fails");
    const lines = r.stdout.split("\n").filter((l) => l.startsWith("::error "));
    assert.equal(lines.length, 1, r.stdout);
    assert.match(lines[0]!, /^::error title=fails\.test\.js%3A it fails::replay: .*OPAMP_MODEL_SEED=5 node --test --test-name-pattern="\^it fails\$" dist\/test\/fails\.test\.js/);
    assert.match(lines[0]!, /%0Areplay: OPAMP_MODEL_SEED=5 OPAMP_MODEL_PATH=1:2%0Abroken/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
