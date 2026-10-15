// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// The shells' shared tables (tests/shell-tables/ at the repository root), as this client
// reads them, in both modes:
// - urls.json: every URL the core accepts is taken by the URL type this shell parses with (WHATWG
//   URL: undici's fetch and the ws package parse with it in Node mode, and Rust mode's proxy choice
//   does), read alike, and sent as exactly its target: in Node mode through the Executor (undici's
//   fetch, the ws package), in Rust mode through the core over the shell's sockets.
// - proxy.json: the proxy chosen from the environment: in Node mode by Node's convention (Transport's
//   connector and ws options), with the differences listed below; in Rust mode (the core, told
//   the environment's variables) by the shared rule, exactly (tests/shell-tables/README.md).
// - tls.json: the TLS settings taken or refused (a Transport is built from them; Rust mode's TLS
//   on a stream is the same Transport's).
// Where this client differs from a table, by Node's convention, the case is listed below with the
// reason: the test fails if it stops differing.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

import { create } from "@bufbuild/protobuf";

import { Executor } from "../src/executor.js";
import { ConfigSchema, EffectSchema, type Input, InputSchema } from "../src/gen/opamp/core/v1/core_pb.js";
import { Client } from "../src/index.js";
import { Core } from "../src/core.js";
import { type RustProxyChoice, type TlsOptions, type TlsVersion, Transport } from "../src/transport.js";

const TABLES = fileURLToPath(new URL("../../../../tests/shell-tables/", import.meta.url));
const table = <T>(name: string): T => JSON.parse(readFileSync(TABLES + name, "utf8")) as T;

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
async function until(what: string, done: () => boolean, ms = 10_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!done()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(2);
  }
}

// --- URLs ------------------------------------------------------------------------------------

interface UrlEntry {
  url: string;
  scheme: string;
  host: string;
  port: number;
  path: string;
  query: string | null;
  target: string;
}

const DEFAULT_PORT: Record<string, number> = { "http:": 80, "ws:": 80, "https:": 443, "wss:": 443 };

/** A host as WHATWG URL writes it: names in lower case, addresses in their canonical form. */
const canonicalHost = (host: string): string => new URL(`http://${host}/`).hostname;

/** How WHATWG URL (the URL type undici's fetch, ws and Rust mode's shell parse with) reads `url`:
 * host, port (the scheme's default when none), path, and query (null when there is no `?`). */
function read(url: string): { host: string; port: number; path: string; query: string | null } {
  const u = new URL(url);
  const at = u.href.indexOf("?");
  return {
    host: u.hostname,
    port: Number(u.port) || (DEFAULT_PORT[u.protocol] ?? 0),
    path: u.pathname,
    query: at < 0 ? null : u.href.slice(at + 1),
  };
}

/** A server that records each request's raw request-target (Node's req.url is the target as
 * sent, neither decoded nor normalized), for plain requests and WebSocket upgrades alike. */
async function targets() {
  const seen: string[] = [];
  const server = http.createServer((req, res) => {
    seen.push(req.url ?? "");
    req.resume();
    res.writeHead(200, { "Content-Type": "application/x-protobuf" }).end();
  });
  server.on("upgrade", (req, socket) => {
    socket.on("error", () => {});
    seen.push(req.url ?? "");
    socket.end("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const port = (server.address() as AddressInfo).port;
  return { seen, port, close: () => (server.closeAllConnections(), server.close()) };
}

const urls = table<{ urls: UrlEntry[] }>("urls.json").urls;

test("urls.json: WHATWG URL takes every URL the core accepts, and reads it alike", () => {
  assert.ok(urls.length > 200);
  const wrong: string[] = [];
  for (const e of urls) {
    let got;
    try {
      got = read(e.url);
    } catch (err) {
      wrong.push(`${e.url}: refused: ${(err as Error).message}`);
      continue;
    }
    const want = { host: canonicalHost(e.host), port: e.port, path: e.path, query: e.query };
    if (JSON.stringify(got) !== JSON.stringify(want)) {
      wrong.push(`${e.url}: read as ${JSON.stringify(got)}, the core as ${JSON.stringify(want)}`);
    }
    // Rust mode: the shell's proxy choice parses it too.
    try {
      new Transport({ proxy: false }).proxyFor(e.url);
    } catch (err) {
      wrong.push(`${e.url}: Rust mode's proxyFor refused it: ${(err as Error).message}`);
    }
  }
  assert.deepEqual(wrong, []);
});

test("urls.json, Node mode: fetch and ws send each URL's exact target", async () => {
  const server = await targets();
  const transport = new Transport({ proxy: false });
  const wrong: string[] = [];
  try {
    let id = 0n;
    for (const e of urls) {
      const ws = e.scheme === "ws" || e.scheme === "wss";
      const url = `${ws ? "ws" : "http"}://127.0.0.1:${server.port}${e.target}`;
      const got: Input[] = [];
      const x = new Executor((i) => got.push(i), undefined, transport);
      const before = server.seen.length;
      id++;
      if (ws) {
        x.do(create(EffectSchema, { kind: { case: "wsOpen", value: { id, url, maxMessageBytes: 1n << 20n, timeoutMs: 5000n } } }));
      } else {
        x.do(create(EffectSchema, { kind: { case: "httpRequest", value: { id, url, body: new Uint8Array(), maxResponseBytes: 1n << 20n, timeoutMs: 5000n } } }));
      }
      await until(`an answer for ${url}`, () => got.length > 0);
      const sent = server.seen[before];
      if (sent !== e.target) {
        const answer = got[0]!.kind;
        wrong.push(`${e.url}: sent ${JSON.stringify(sent)}, not ${JSON.stringify(e.target)} (${answer.case}: ${(answer.value as { detail?: string }).detail ?? ""})`);
      }
    }
  } finally {
    transport.close();
    server.close();
  }
  assert.deepEqual(wrong, []);
});

test("urls.json, Rust mode: the core sends each URL's exact target over the shell's sockets", async () => {
  const server = await targets();
  const wrong: string[] = [];
  try {
    for (const e of urls) {
      const ws = e.scheme === "ws" || e.scheme === "wss";
      const endpoint = `${ws ? "ws" : "http"}://127.0.0.1:${server.port}${e.target}`;
      const before = server.seen.length;
      const client = Client.start({ endpoint, mode: "rust", proxy: false, heartbeatIntervalMs: 60_000, instanceUid: new Uint8Array(16).fill(1) }, {});
      try {
        await until(`a request for ${endpoint}`, () => server.seen.length > before);
      } finally {
        await client.stop();
      }
      const sent = server.seen[before];
      if (sent !== e.target) wrong.push(`${e.url}: sent ${JSON.stringify(sent)}, not ${JSON.stringify(e.target)}`);
    }
  } finally {
    server.close();
  }
  assert.deepEqual(wrong, []);
});

// --- Proxies ---------------------------------------------------------------------------------

interface ProxyCase {
  id: string;
  env: Record<string, string>;
  url: string;
  proxy: string | null;
  why: string;
}

/** Where this client differs from proxy.json: what it picks, and why. It reads the environment as
 * Node's own built-in proxy support does (NODE_USE_ENV_PROXY / --use-env-proxy: lib/internal/http.js
 * and undici's EnvHttpProxyAgent), so that an application's fetch and this client agree. */
const PROXY_DIFFERENCES: Record<string, { proxy: string | null; why: string }> = {
  "http-proxy-upper-first": { proxy: "http://lower-proxy:1", why: "Node reads the lower-case name first" },
  "all-proxy-for-http": { proxy: null, why: "Node has no ALL_PROXY" },
  "all-proxy-for-https": { proxy: null, why: "Node has no ALL_PROXY" },
  "empty-falls-to-all": { proxy: null, why: "Node has no ALL_PROXY" },
  "no-proxy-subdomain": { proxy: "http://web-proxy:3128", why: "in Node a NO_PROXY name without a leading dot is that host only" },
  "no-proxy-cidr": { proxy: "http://web-proxy:3128", why: "Node's NO_PROXY has no CIDR networks (it has a-b address ranges)" },
  "no-proxy-port": { proxy: null, why: "Node's NO_PROXY matches host:port entries" },
  "upper-empty-hides-lower": { proxy: "http://lower-proxy:1", why: "Node reads the lower-case name first" },
  "value-trimmed": {
    proxy: "refused (proxy: HTTP_PROXY is not a URL)",
    why: "Node takes the value as it is, spaces and all: no URL, which the client refuses at start",
  },
  "socks5-from-the-environment": { proxy: null, why: "Node has no ALL_PROXY" },
  "no-proxy-star-label": { proxy: null, why: "in Node's NO_PROXY *.example.com is the subdomains" },
  "no-proxy-star-address": { proxy: null, why: "in Node's NO_PROXY * is every host, addresses too" },
  "no-proxy-ipv6-cidr": { proxy: "http://web-proxy:3128", why: "Node's NO_PROXY has no CIDR networks" },
  "no-proxy-network-host-bits": { proxy: "http://web-proxy:3128", why: "Node's NO_PROXY has no CIDR networks" },
};

/** A proxy URL as a route: scheme://host:port, credentials left out (no scheme: http). */
function route(proxy: string): string {
  const u = new URL(/^[a-z][a-z0-9+.-]*:\/\//i.test(proxy) ? proxy : `http://${proxy}`);
  const scheme = u.protocol.slice(0, -1);
  const port = Number(u.port) || (scheme === "https" ? 443 : scheme.startsWith("socks") ? 1080 : 80);
  return `${scheme}://${u.hostname}:${port}`;
}

test("proxy.json, Node mode: the proxy chosen from the environment, by Node's convention", () => {
  const cases = table<{ cases: ProxyCase[] }>("proxy.json").cases;
  assert.ok(cases.length > 30);
  const wrong: string[] = [];
  for (const c of cases) {
    // Exactly the case's variables, and Node told to take proxies from the environment (as an
    // application does), injected rather than set on this process.
    const env = { ...c.env, NODE_USE_ENV_PROXY: "1" };
    let got: string | null;
    try {
      const chosen = new Transport({}, { env, execArgv: [] }).proxyFor(c.url);
      got = chosen === undefined ? null : route(chosen.url);
    } catch (err) {
      got = `refused (${(err as Error).message})`;
    }
    const differs = PROXY_DIFFERENCES[c.id];
    const want = differs ? differs.proxy : c.proxy;
    if (got !== want) {
      wrong.push(`${c.id}: ${got}, ${differs ? `the known difference says ${want} (${differs.why})` : `the table says ${want} (${c.why})`}`);
    }
  }
  for (const id of Object.keys(PROXY_DIFFERENCES)) {
    if (!cases.some((c) => c.id === id)) wrong.push(`${id}: a known difference for a case the table no longer has`);
  }
  assert.deepEqual(wrong, []);
});

/** Where Rust mode's core connects first for `url` when told `choice`: the proxy as the table
 * writes a route (scheme://host:port, without credentials), or null for directly. The core picks
 * it itself: it asks no proxy_query. */
function coreRoute(choice: RustProxyChoice, url: string): string | null {
  const core = new Core(
    create(ConfigSchema, { instanceUid: new Uint8Array(16), endpoint: url, streams: true, entropy: new Uint8Array(32), proxy: choice }),
  );
  try {
    const out = core.step(1, create(InputSchema, { kind: { case: "start", value: {} } }));
    let open: { id: bigint; host: string; port: number } | undefined;
    for (const e of out.effects) {
      assert.notEqual(e.kind.case, "proxyQuery", `${url}: the core asked which proxy`);
      if (e.kind.case === "streamOpen") open = e.kind.value;
    }
    assert.ok(open, `${url}: no stream opened`);
    const u = new URL(url);
    const secure = u.protocol === "https:" || u.protocol === "wss:";
    const host = u.hostname.replace(/^\[(.*)\]$/, "$1").toLowerCase();
    if (open.host.toLowerCase() === host && open.port === (Number(u.port) || (secure ? 443 : 80))) return null;
    // Which kind of proxy: SOCKS5 greets with version 5, an https:// one is TLS first.
    const next = core.step(2, create(InputSchema, { kind: { case: "streamOpened", value: { id: open.id } } }));
    let scheme = "http";
    for (const e of next.effects) {
      if (e.kind.case === "streamWrite" && e.kind.value.data[0] === 5) scheme = "socks5";
      if (e.kind.case === "streamStartTls") scheme = "https";
    }
    return `${scheme}://${open.host.includes(":") ? `[${open.host}]` : open.host}:${open.port}`;
  } finally {
    core.close();
  }
}

// Rust mode: the client hands the core the environment's variables (Config.proxy) and the core
// picks by the shared rule: each case through this client's reading of the environment and the
// core it ships, exactly as the table says.
// covers: RULE-PROXY-02
test("proxy.json, Rust mode: the proxy chosen from the environment, exactly as the table says", () => {
  const cases = table<{ cases: ProxyCase[] }>("proxy.json").cases;
  const wrong: string[] = [];
  for (const c of cases) {
    // Rust mode's rule reads the environment whatever Node is told: no NODE_USE_ENV_PROXY.
    const choice = new Transport({}, { env: { ...c.env }, execArgv: [] }, true).rustProxyChoice();
    const got = coreRoute(choice, c.url);
    if (got !== c.proxy) wrong.push(`${c.id}: ${got}, the table says ${c.proxy} (${c.why})`);
  }
  assert.deepEqual(wrong, []);
});

// What Rust mode tells the core: the configured proxy and no variables; none with `false`;
// otherwise each variable the rule reads that is set, even empty, by its own name. A proxy_query
// (only a core that wasn't told asks) hears the configured proxy or none.
// covers: RULE-PROXY-02
test("Rust mode tells the core its proxy", () => {
  const env = { env: { HTTP_PROXY: "http://env:1", no_proxy: "", REQUEST_METHOD: "GET", OTHER: "x" }, execArgv: [] };
  const configured = new Transport({ proxy: { url: "http://u:p%40ss@configured:8080", headers: { "X-A": "1" } } }, env, true);
  assert.deepEqual(configured.rustProxyChoice(), {
    direct: false,
    url: "http://u:p%40ss@configured:8080",
    headers: [{ name: "X-A", value: "1" }],
    resolveLocally: false,
    environment: [],
  });
  assert.equal(configured.proxyFor("http://x/")?.url, "http://u:p%40ss@configured:8080");
  const direct = new Transport({ proxy: false }, env, true);
  assert.deepEqual(direct.rustProxyChoice(), { direct: true, url: "", headers: [], resolveLocally: false, environment: [] });
  assert.equal(direct.proxyFor("http://x/"), undefined);
  const fromEnv = new Transport({ proxy: { resolveLocally: true } }, env, true);
  assert.deepEqual(fromEnv.rustProxyChoice(), {
    direct: false,
    url: "",
    headers: [],
    resolveLocally: true,
    environment: [
      { name: "REQUEST_METHOD", value: "GET" },
      { name: "HTTP_PROXY", value: "http://env:1" },
      { name: "no_proxy", value: "" },
    ],
  });
  assert.equal(fromEnv.proxyFor("http://x/"), undefined);
  // The core reads them by the rule: CGI, so none.
  assert.equal(coreRoute(fromEnv.rustProxyChoice(), "http://example.com/"), null);
});
// httpoxy: under CGI the environment's proxies are ignored, but a proxy the application configured
// still applies (in both modes: Node mode's connector and Rust mode's proxy_query answers).
test("under CGI: no proxy from the environment, a configured one still", () => {
  const env = { REQUEST_METHOD: "GET", HTTP_PROXY: "http://web-proxy:3128", HTTPS_PROXY: "http://secure-proxy:3129", NODE_USE_ENV_PROXY: "1" };
  const platform = { env, execArgv: [] };
  for (const url of ["http://example.com/", "https://example.com/", "ws://example.com/", "wss://example.com/"]) {
    assert.equal(new Transport({}, platform).proxyFor(url), undefined, url);
    assert.equal(new Transport({}, platform, true).proxyFor(url), undefined, url);
    assert.equal(new Transport({ proxy: { resolveLocally: false } }, platform).proxyFor(url), undefined, url);
    assert.equal(new Transport({ proxy: { url: "http://configured:8080" } }, platform).proxyFor(url)?.url, "http://configured:8080", url);
    assert.equal(new Transport({ proxy: { url: "http://configured:8080" } }, platform, true).proxyFor(url)?.url, "http://configured:8080", url);
    // Node mode: the WebSocket goes through it too (ws options with a tunnel, not a direct TLS).
    assert.ok(new Transport({ proxy: { url: "http://configured:8080" } }, platform).ws(url).createConnection, url);
    assert.equal(new Transport({}, platform).ws(url).createConnection, undefined, url);
  }
});

test("Node mode takes proxies from the environment only when Node is told to; Rust mode always", () => {
  const env = { HTTP_PROXY: "http://web-proxy:3128", HTTPS_PROXY: "http://secure-proxy:3129" };
  for (const url of ["http://example.com/", "https://example.com/"]) {
    assert.equal(new Transport({}, { env, execArgv: [] }).proxyFor(url), undefined, url);
    // Rust mode hands the core the variables, and the core picks.
    assert.notEqual(coreRoute(new Transport({}, { env, execArgv: [] }, true).rustProxyChoice(), url), null, url);
  }
});

// --- TLS -------------------------------------------------------------------------------------

interface TlsCase {
  id: string;
  ca?: string;
  cert?: string;
  key?: string;
  min?: string;
  max?: string;
  expect: "ok" | "refused";
  why: string;
}

/** A version as TlsOptions names it (TLSv1.2, TLSv1.3); older ones by Node's names, which the
 * client must refuse. */
const version = (v: string | undefined): TlsVersion | undefined =>
  v === undefined ? undefined : ((v === "1.0" ? "TLSv1" : `TLSv${v}`) as TlsVersion);

test("tls.json: the TLS settings taken and refused (both modes)", () => {
  const cases = table<{ cases: TlsCase[] }>("tls.json").cases;
  assert.ok(cases.length > 20);
  const file = (p: string | undefined) => (p === undefined ? undefined : readFileSync(TABLES + p));
  const wrong: string[] = [];
  for (const c of cases) {
    const tls: TlsOptions = {
      ...(c.ca ? { ca: file(c.ca) } : {}),
      ...(c.cert ? { cert: file(c.cert) } : {}),
      ...(c.key ? { key: file(c.key) } : {}),
      ...(c.min ? { minVersion: version(c.min) } : {}),
      ...(c.max ? { maxVersion: version(c.max) } : {}),
    };
    let got: string;
    try {
      new Transport({ tls, proxy: false }).close();
      got = "ok";
    } catch (err) {
      got = `refused (${(err as Error).message})`;
    }
    if (!got.startsWith(c.expect)) wrong.push(`${c.id}: ${got}, the table says ${c.expect} (${c.why})`);
  }
  assert.deepEqual(wrong, []);
});
