// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, statSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { create } from "@bufbuild/protobuf";

import { type DownloadResponse, EffectSchema, type Input } from "../src/gen/opamp/core/v1/core_pb.js";
import { Executor, downloadPath, downloadsDir } from "../src/executor.js";

// No redirect is followed: each response is reported as it came, whatever its status (its body in
// the file), with a redirect's Location resolved by the WHATWG URL parser and its origin; the core
// decides (RULE-CLIENT-17).
// covers: RULE-CLIENT-17
test("downloads are hashed, report every status and a redirect's Location, and leave nothing when they fail", async () => {
  const body = Buffer.from("package".repeat(100));
  const server = createServer((req, res) => {
    if (req.url === "/moved") {
      res.writeHead(302, { location: "/file?x=1#frag" }).end("moved");
    } else if (req.url === "/away") {
      res.writeHead(301, { location: "HTTPS://Example.COM:443/a/../b" }).end();
    } else if (req.url === "/odd") {
      res.writeHead(307, { location: "http://[::1" }).end();
    } else if (req.url === "/file" && req.headers.authorization === "t") {
      res.end(body);
    } else {
      res.writeHead(404).end("missing");
    }
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  const base = `http://127.0.0.1:${port}`;
  const state = join(mkdtempSync(join(tmpdir(), "opamp-dl-")), "state");
  const got: Input[] = [];
  let arrived: () => void = () => {};
  const x = new Executor((i) => {
    got.push(i);
    arrived();
  }, state);
  const get = async (id: bigint, path: string, maxBytes: bigint): Promise<Input> => {
    const next = new Promise<void>((resolve) => {
      arrived = resolve;
    });
    const headers = [{ name: "Authorization", value: "t" }];
    x.do(create(EffectSchema, { kind: { case: "download", value: { id, url: base + path, headers, maxBytes, timeoutMs: 5000n } } }));
    await next;
    return got[got.length - 1] as Input;
  };
  const response = (i: Input): DownloadResponse => {
    assert.equal(i.kind.case, "downloadResponse", JSON.stringify(i.kind.value, (_, v) => (typeof v === "bigint" ? `${v}` : v)));
    return i.kind.value as DownloadResponse;
  };
  const sha = (b: Buffer | string) => [...createHash("sha256").update(b).digest()];
  try {
    const done = response(await get(1n, "/file", 1n << 20n));
    assert.equal(done.status, 200);
    assert.equal(done.location, "");
    assert.deepEqual([...done.sha256], sha(body));
    assert.deepEqual(readFileSync(downloadPath(state, 1n)), body);
    if (process.platform !== "win32") {
      assert.equal(statSync(downloadsDir(state)).mode & 0o777, 0o700);
      assert.equal(statSync(downloadPath(state, 1n)).mode & 0o777, 0o600);
    }
    x.do(create(EffectSchema, { kind: { case: "discard", value: { id: 1n } } }));
    // Deleted off the event loop.
    await x.settled();
    assert.ok(!existsSync(downloadPath(state, 1n)));

    // A redirect: not followed; where it points, resolved against the URL, and its origin.
    const moved = response(await get(2n, "/moved", 1n << 20n));
    assert.equal(moved.status, 302);
    assert.equal(moved.location, `${base}/file?x=1#frag`);
    assert.deepEqual([moved.locationOrigin?.scheme, moved.locationOrigin?.host, moved.locationOrigin?.port], ["http", "127.0.0.1", port]);
    assert.deepEqual([...moved.sha256], sha("moved"));
    // The scheme's default port is 0, and the URL as WHATWG writes it.
    const away = response(await get(3n, "/away", 1n << 20n));
    assert.equal(away.location, "https://example.com/b");
    assert.deepEqual([away.locationOrigin?.scheme, away.locationOrigin?.host, away.locationOrigin?.port], ["https", "example.com", 0]);
    // A Location that doesn't parse goes as given, with no origin: the core refuses it.
    const odd = response(await get(4n, "/odd", 1n << 20n));
    assert.equal(odd.location, "http://[::1");
    assert.equal(odd.locationOrigin, undefined);
    // Any status: the body is in the file, for the core to judge.
    const missing = response(await get(5n, "/missing", 1n << 20n));
    assert.equal(missing.status, 404);
    assert.deepEqual(readFileSync(downloadPath(state, 5n)), Buffer.from("missing"));

    // Over the limit: failed, and nothing left.
    const failed = await get(6n, "/file", 10n);
    assert.equal(failed.kind.case, "downloadFailed");
    assert.ok(!existsSync(downloadPath(state, 6n)));
  } finally {
    await x.shutdown();
    server.close();
  }
});

// The core's cancel of a download under way: nothing more is said of it, and its file (made new,
// this client's) is deleted; a file already at the path is never touched.
// covers: RULE-SHELL-06
test("a cancelled download says nothing more and leaves no file", async () => {
  let release: () => void = () => {};
  const held = new Promise<void>((r) => (release = r));
  const server = createServer((req, res) => {
    res.writeHead(200);
    res.write("part");
    void held.then(() => res.end("rest"));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const state = join(mkdtempSync(join(tmpdir(), "opamp-dl-")), "state");
  const got: Input[] = [];
  const x = new Executor((i) => got.push(i), state);
  try {
    x.do(create(EffectSchema, { kind: { case: "download", value: { id: 1n, url: `${base}/f`, maxBytes: 1n << 20n, timeoutMs: 0n } } }));
    for (let i = 0; i < 200 && !existsSync(downloadPath(state, 1n)); i++) await new Promise((r) => setTimeout(r, 10));
    assert.ok(existsSync(downloadPath(state, 1n)), "the file is being written");
    x.do(create(EffectSchema, { kind: { case: "cancel", value: { id: 1n } } }));
    release();
    await x.settled();
    await new Promise((r) => setTimeout(r, 100));
    assert.deepEqual(got, [], "nothing said of it");
    assert.ok(!existsSync(downloadPath(state, 1n)), "its file deleted");
  } finally {
    release();
    await x.shutdown();
    server.close();
  }
});
