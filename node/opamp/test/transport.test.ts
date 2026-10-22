// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// TLS, client certificates and proxies, over plain HTTP, WebSocket and package downloads. The
// certificates are test/fixtures (make.sh); the proxies are small ones written here, an HTTP
// proxy (CONNECT) and a SOCKS5 one, both asking for a username and password.

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync } from "node:fs";
import http, { type IncomingMessage, type ServerResponse } from "node:http";
import https from "node:https";
import net, { type AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import type { TLSSocket } from "node:tls";

import fc from "fast-check";
import { WebSocketServer } from "ws";

import { EffectSchema, type Input } from "../src/gen/opamp/core/v1/core_pb.js";
import { Executor } from "../src/executor.js";
import {
  AgentCapabilities,
  AgentDescriptionSchema,
  type AgentToServer,
  AgentToServerSchema,
  Client,
  type Config,
  create,
  fromBinary,
  type Notification,
  ServerToAgentSchema,
  toBinary,
} from "../src/index.js";
import { bypassed, envProxyOn, Transport } from "../src/transport.js";

const fixture = (name: string): string => readFileSync(new URL(`../../test/fixtures/${name}`, import.meta.url), "utf8");
const CA = fixture("ca.pem");
const SERVER = { cert: fixture("server.pem"), key: fixture("server-key.pem") };
const OTHER_SERVER = { cert: fixture("other-server.pem"), key: fixture("other-server-key.pem") };
const CLIENT = { cert: fixture("client.pem"), key: fixture("client-key.pem") };

const UID = new Uint8Array(16).fill(0x42);
const description = create(AgentDescriptionSchema, {
  identifyingAttributes: [{ key: "service.name", value: { value: { case: "stringValue", value: "test" } } }],
});
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function until(what: string, done: () => boolean, ms = 10_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!done()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(10);
  }
}

const listen = async (server: net.Server): Promise<number> => {
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  return (server.address() as AddressInfo).port;
};

async function body(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return new Uint8Array(Buffer.concat(chunks));
}

interface ServerTls {
  cert: string;
  key: string;
  /** Require a client certificate signed by CA. */
  clientAuth?: boolean;
  maxVersion?: "TLSv1.2" | "TLSv1.3";
}

/** An OpAMP server over HTTP and WebSocket both, with TLS if `tls`. It records every message,
 * and for each TLS connection its version and the client certificate's CN. */
async function opampServer(tls?: ServerTls, files: Record<string, Buffer> = {}) {
  const got: AgentToServer[] = [];
  const handshakes: { version: string | null; peer: string | undefined }[] = [];
  const handle = async (req: IncomingMessage, res: ServerResponse) => {
    const file = files[req.url ?? ""];
    if (file) {
      res.end(file);
      return;
    }
    const msg = fromBinary(AgentToServerSchema, await body(req));
    got.push(msg);
    res.setHeader("Content-Type", "application/x-protobuf");
    res.end(toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid })));
  };
  const server = tls
    ? https.createServer(
        { ...tls, ca: CA, requestCert: tls.clientAuth ?? false, rejectUnauthorized: tls.clientAuth ?? false },
        handle,
      )
    : http.createServer(handle);
  server.on("secureConnection", (s: TLSSocket) => {
    const peer = s.getPeerCertificate();
    handshakes.push({ version: s.getProtocol(), peer: peer?.subject?.CN as string | undefined });
  });
  const wss = new WebSocketServer({ server });
  wss.on("connection", (ws) => {
    ws.on("message", (data: Buffer) => {
      const msg = fromBinary(AgentToServerSchema, new Uint8Array(data.subarray(1)));
      got.push(msg);
      const reply = toBinary(ServerToAgentSchema, create(ServerToAgentSchema, { instanceUid: msg.instanceUid }));
      ws.send(Buffer.concat([Buffer.from([0]), reply]));
    });
  });
  const port = await listen(server);
  return {
    port,
    got,
    handshakes,
    close: () => {
      wss.close();
      server.closeAllConnections();
      server.close();
    },
  };
}

interface Tunnel {
  target: string;
  /** SOCKS5 only: the address type the client sent (1 IPv4, 3 a name, 4 IPv6). */
  atyp?: number;
  user: string;
  pass: string;
  headers?: IncomingMessage["headers"];
  /** SOCKS5 only: what became of the proxy's own connection to the target, with times. */
  upstream?: string;
}

function pipe(a: net.Socket, b: net.Socket): void {
  a.pipe(b).on("error", () => a.destroy());
  b.pipe(a).on("error", () => b.destroy());
  a.on("error", () => b.destroy());
  b.on("error", () => a.destroy());
}

/** An HTTP proxy taking CONNECT only, with Basic credentials `user`/`pass`; an https:// one
 * (TLS to the proxy) with `tls`. */
async function connectProxy(user: string, pass: string, tls?: { cert: string; key: string }) {
  const tunnels: Tunnel[] = [];
  const sockets = new Set<net.Socket>();
  const refuse = (_req: IncomingMessage, res: ServerResponse) => res.writeHead(405).end();
  const server = tls ? https.createServer(tls, refuse) : http.createServer(refuse);
  /** TLS handshakes the proxy completed. */
  let handshakes = 0;
  server.on("secureConnection", () => handshakes++);
  server.on("tlsClientError", () => {});
  server.on("connect", (req: IncomingMessage, client: net.Socket, head: Buffer) => {
    sockets.add(client);
    const auth = /^Basic (.*)$/.exec(req.headers["proxy-authorization"] ?? "");
    const decoded = auth ? Buffer.from(auth[1] ?? "", "base64").toString() : "";
    const at = decoded.indexOf(":");
    const tunnel = { target: req.url ?? "", user: decoded.slice(0, at), pass: decoded.slice(at + 1), headers: req.headers };
    tunnels.push(tunnel);
    if (!auth || tunnel.user !== user || tunnel.pass !== pass) {
      client.end("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic\r\n\r\n");
      return;
    }
    const [host, port] = [req.url!.slice(0, req.url!.lastIndexOf(":")), Number(req.url!.split(":").pop())];
    const up = net.connect({ host: host.replace(/^\[(.*)\]$/, "$1"), port }, () => {
      client.write("HTTP/1.1 200 Connection established\r\n\r\n");
      if (head.length) up.write(head);
      pipe(client, up);
    });
    sockets.add(up);
    up.on("error", () => client.destroy());
  });
  const port = await listen(server);
  return {
    port,
    tunnels,
    get handshakes() {
      return handshakes;
    },
    close: () => {
      for (const s of sockets) s.destroy();
      server.close();
    },
  };
}

/** Reads exactly n bytes at a time from a socket. */
function reader(s: net.Socket) {
  let buf = Buffer.alloc(0);
  let wake: (() => void) | undefined;
  s.on("data", (d: Buffer) => {
    buf = Buffer.concat([buf, d]);
    wake?.();
  });
  s.on("close", () => wake?.());
  return {
    async read(n: number): Promise<Buffer> {
      while (buf.length < n) {
        if (s.destroyed) throw new Error("closed");
        await new Promise<void>((r) => (wake = r));
      }
      const out = buf.subarray(0, n);
      buf = buf.subarray(n);
      return out;
    },
    rest(): Buffer {
      s.removeAllListeners("data");
      return buf;
    },
  };
}

/** A SOCKS5 proxy (RFC 1928) asking for username `user` and password `pass` (RFC 1929). */
async function socksProxy(user: string, pass: string) {
  const tunnels: Tunnel[] = [];
  const sockets = new Set<net.Socket>();
  const server = net.createServer(async (c) => {
    sockets.add(c);
    c.on("error", () => {});
    const r = reader(c);
    try {
      const [, n] = await r.read(2);
      const methods = [...(await r.read(n!))];
      if (!methods.includes(2)) {
        c.end(Buffer.from([5, 0xff]));
        return;
      }
      c.write(Buffer.from([5, 2]));
      const [, ulen] = await r.read(2);
      const u = (await r.read(ulen!)).toString();
      const [plen] = await r.read(1);
      const p = (await r.read(plen!)).toString();
      const tunnel: Tunnel = { target: "", user: u, pass: p };
      tunnels.push(tunnel);
      if (u !== user || p !== pass) {
        c.end(Buffer.from([1, 1]));
        return;
      }
      c.write(Buffer.from([1, 0]));
      const [, cmd, , atyp] = await r.read(4);
      let host: string;
      if (atyp === 1) host = [...(await r.read(4))].join(".");
      else if (atyp === 3) host = (await r.read((await r.read(1))[0]!)).toString();
      else host = (await r.read(16)).toString("hex").match(/.{4}/g)!.join(":");
      const port = (await r.read(2)).readUInt16BE();
      tunnel.target = `${host}:${port}`;
      tunnel.atyp = atyp;
      if (cmd !== 1) {
        c.end(Buffer.from([5, 7, 0, 1, 0, 0, 0, 0, 0, 0]));
        return;
      }
      let connected = false;
      // What became of the proxy's own connection to the server, for a failure's message.
      const began = Date.now();
      const said = (what: string) => (tunnel.upstream = `${tunnel.upstream ?? ""} +${Date.now() - began}ms ${what};`);
      const up = net.connect({ host, port }, () => {
        said("connected");
        connected = true;
        c.write(Buffer.from([5, 0, 0, 1, 0, 0, 0, 0, 0, 0]));
        const rest = r.rest();
        if (rest.length) up.write(rest);
        pipe(c, up);
      });
      sockets.add(up);
      up.on("lookup", (err, address, family) => said(`lookup ${err ? String(err) : `${address} (IPv${family})`}`));
      up.on("connectionAttempt", (ip: string, p: number) => said(`trying ${ip}:${p}`));
      up.on("connectionAttemptFailed", (ip: string, p: number, _f: number, e: Error) => said(`${ip}:${p} failed: ${e.message}`));
      up.on("connectionAttemptTimeout", (ip: string, p: number) => said(`${ip}:${p} timed out`));
      up.on("error", (e) => said(`error ${e.message}`));
      up.on("close", () => said("closed"));
      // Before the tunnel is up: "connection refused" (5). After: drop the client.
      up.on("error", () => (connected ? c.destroy() : c.end(Buffer.from([5, 5, 0, 1, 0, 0, 0, 0, 0, 0]))));
    } catch {
      c.destroy();
    }
  });
  const port = await listen(server);
  return {
    port,
    tunnels,
    close: () => {
      for (const s of sockets) s.destroy();
      server.close();
    },
  };
}

type Server = Awaited<ReturnType<typeof opampServer>>;
type Transport_ = "http" | "ws" | "http+core" | "ws+core";

/** Run a client against `server` until it has sent `messages` messages, or has failed to
 * connect when `fail`. */
async function run(
  server: Server,
  how: Transport_,
  secure: boolean,
  config: Partial<Config>,
  { messages = 2, fail = false } = {},
): Promise<Notification[]> {
  // "ws+core": WebSocket in core mode (the core's own HTTP and WebSocket over Node's sockets).
  const [transport, mode] = how.split("+") as ["http" | "ws", "core" | undefined];
  config = { ...config, mode: mode ?? "platform" };
  const scheme = (transport === "http" ? "http" : "ws") + (secure ? "s" : "");
  const notes: Notification[] = [];
  const start = server.got.length;
  const client = Client.start(
    { endpoint: `${scheme}://localhost:${server.port}/v1/opamp`, instanceUid: UID, heartbeatIntervalMs: 50, capabilities: BigInt(AgentCapabilities.AgentCapabilities_ReportsHeartbeat), ...config },
    { description },
    (n) => notes.push(n),
  );
  try {
    if (fail) await until("connectFailed", () => notes.some((n) => n.kind === "connectFailed"));
    else await until(`${messages} messages`, () => server.got.length >= start + messages);
  } finally {
    await client.stop();
  }
  return notes;
}

const failure = (notes: Notification[]): string => {
  const n = notes.find((n) => n.kind === "connectFailed");
  return n?.kind === "connectFailed" ? n.detail : "";
};

for (const transport of ["http", "ws", "http+core", "ws+core"] as const) {
  // covers: RULE-TLS-01
  test(`${transport}: TLS with the CA it is given`, async () => {
    const server = await opampServer(SERVER);
    try {
      await run(server, transport, true, { tls: { ca: CA } });
      assert.ok(server.got[0]!.agentDescription, "the first message is a full report");
      for (const h of server.handshakes) assert.match(h.version ?? "", /^TLSv1\.[23]$/);
      // Node's CAs as well: the server's CA still counts.
      const before = server.got.length;
      await run(server, transport, true, { tls: { ca: CA, includeSystemCas: true } });
      assert.ok(server.got.length > before);
    } finally {
      server.close();
    }
  });

  // covers: RULE-TLS-01
  test(`${transport}: a server whose certificate it can't verify gets nothing`, async () => {
    const server = await opampServer(OTHER_SERVER);
    try {
      // Another CA's certificate; then Node's own CAs, which know neither.
      const notes = await run(server, transport, true, { tls: { ca: CA } }, { fail: true });
      assert.match(failure(notes), /certificate/i);
      await run(server, transport, true, {}, { fail: true });
      assert.equal(server.got.length, 0, "nothing sent");
    } finally {
      server.close();
    }
  });

  // covers: RULE-TLS-02
  test(`${transport}: presents its client certificate in every handshake`, async () => {
    const server = await opampServer({ ...SERVER, clientAuth: true });
    try {
      await run(server, transport, true, { tls: { ca: CA, ...CLIENT } });
      assert.ok(server.handshakes.length > 0);
      for (const h of server.handshakes) assert.equal(h.peer, "test-agent");
      // Without one, the server refuses the handshake.
      const before = server.got.length;
      await run(server, transport, true, { tls: { ca: CA } }, { fail: true });
      assert.equal(server.got.length, before);
    } finally {
      server.close();
    }
  });

  test(`${transport}: TLS versions`, async () => {
    const server = await opampServer({ ...SERVER, maxVersion: "TLSv1.2" });
    try {
      await run(server, transport, true, { tls: { ca: CA, maxVersion: "TLSv1.2" } });
      for (const h of server.handshakes) assert.equal(h.version, "TLSv1.2");
      const before = server.got.length;
      await run(server, transport, true, { tls: { ca: CA, minVersion: "TLSv1.3" } }, { fail: true });
      assert.equal(server.got.length, before);
    } finally {
      server.close();
    }
  });

  for (const secure of [false, true]) {
    const scheme = (transport.startsWith("http") ? "http" : "ws") + (secure ? "s" : "") + transport.replace(/^\w+/, "");

    // covers: RULE-PROXY-01
    test(`${scheme}: through an HTTP proxy with credentials`, async () => {
      const server = await opampServer(secure ? { ...SERVER, clientAuth: true } : undefined);
      const proxy = await connectProxy("agent", "p@ss:w/rd");
      const url = `http://agent:${encodeURIComponent("p@ss:w/rd")}@127.0.0.1:${proxy.port}`;
      try {
        await run(server, transport, secure, {
          tls: { ca: CA, ...CLIENT },
          proxy: { url, headers: { "X-Proxy-Test": "yes" } },
        });
        assert.ok(proxy.tunnels.length > 0);
        for (const t of proxy.tunnels) {
          assert.equal(t.target, `localhost:${server.port}`, "the proxy resolves the name");
          assert.equal(t.headers?.["x-proxy-test"], "yes");
        }
        if (secure) for (const h of server.handshakes) assert.equal(h.peer, "test-agent", "TLS inside the tunnel");
        // Wrong credentials: the proxy refuses, and the server hears nothing.
        const before = server.got.length;
        const wrong = `http://agent:nope@127.0.0.1:${proxy.port}`;
        const notes = await run(server, transport, secure, { tls: { ca: CA }, proxy: { url: wrong } }, { fail: true });
        assert.match(failure(notes), /407/);
        assert.equal(server.got.length, before);
      } finally {
        proxy.close();
        server.close();
      }
    });

    // covers: RULE-PROXY-01
    test(`${scheme}: through an HTTPS proxy: TLS to the proxy, then the server's inside`, async () => {
      const server = await opampServer(secure ? { ...SERVER, clientAuth: true } : undefined);
      const proxy = await connectProxy("agent", "pw", SERVER);
      try {
        await run(server, transport, secure, {
          tls: { ca: CA, ...CLIENT },
          proxy: { url: `https://agent:pw@localhost:${proxy.port}` },
        });
        assert.ok(proxy.handshakes > 0, "TLS to the proxy");
        assert.ok(proxy.tunnels.length > 0);
        for (const t of proxy.tunnels) assert.equal(t.target, `localhost:${server.port}`);
        if (secure) {
          assert.ok(server.handshakes.length > 0);
          for (const h of server.handshakes) assert.equal(h.peer, "test-agent", "TLS inside the tunnel");
        }
      } finally {
        proxy.close();
        server.close();
      }
      // A proxy whose certificate the CA doesn't vouch for gets no CONNECT, and the server nothing.
      const other = await opampServer(secure ? SERVER : undefined);
      const untrusted = await connectProxy("agent", "pw", OTHER_SERVER);
      try {
        const url = `https://agent:pw@localhost:${untrusted.port}`;
        const notes = await run(other, transport, secure, { tls: { ca: CA }, proxy: { url } }, { fail: true });
        assert.match(failure(notes), /certificate/i);
        assert.equal(untrusted.tunnels.length, 0);
        assert.equal(other.got.length, 0);
      } finally {
        untrusted.close();
        other.close();
      }
    });

    // covers: RULE-PROXY-01
    test(`${scheme}: through a SOCKS5 proxy with credentials`, async () => {
      const server = await opampServer(secure ? { ...SERVER, clientAuth: true } : undefined);
      const proxy = await socksProxy("agent", "proxy-secret");
      try {
        await run(server, transport, secure, {
          tls: { ca: CA, ...CLIENT },
          proxy: { url: `socks5://agent:proxy-secret@127.0.0.1:${proxy.port}` },
        });
        assert.ok(proxy.tunnels.length > 0);
        for (const t of proxy.tunnels) {
          assert.equal(t.target, `localhost:${server.port}`, "the proxy resolves the name");
          assert.equal(t.atyp, 3);
        }
        if (secure) for (const h of server.handshakes) assert.equal(h.peer, "test-agent", "TLS inside the tunnel");
        const before = server.got.length;
        const wrong = `socks5://agent:nope@127.0.0.1:${proxy.port}`;
        await run(server, transport, secure, { proxy: { url: wrong } }, { fail: true });
        assert.equal(server.got.length, before);
      } finally {
        proxy.close();
        server.close();
      }
    });
  }

  // covers: RULE-PROXY-01
  test(`${transport}: a proxy URL without a scheme is an HTTP proxy`, async () => {
    const server = await opampServer();
    const proxy = await connectProxy("agent", "pw");
    try {
      await run(server, transport, false, { proxy: { url: `agent:pw@127.0.0.1:${proxy.port}` } });
      assert.ok(proxy.tunnels.length > 0);
      for (const t of proxy.tunnels) assert.equal(t.target, `localhost:${server.port}`);
    } finally {
      proxy.close();
      server.close();
    }
  });
  for (const socks of ["socks5", "socks5h"]) {
    // covers: RULE-PROXY-01
    test(`${transport}: ${socks}:// gives the proxy the name to resolve`, async () => {
      const server = await opampServer();
      const proxy = await socksProxy("agent", "pw");
      try {
        await run(server, transport, false, { proxy: { url: `${socks}://agent:pw@127.0.0.1:${proxy.port}` } });
        assert.ok(proxy.tunnels.length > 0);
        for (const t of proxy.tunnels) {
          assert.equal(t.atyp, 3, "a name (ATYP DOMAIN)");
          assert.equal(t.target, `localhost:${server.port}`);
        }
      } finally {
        proxy.close();
        server.close();
      }
    });

    // covers: RULE-PROXY-01
    test(`${transport}: ${socks}:// with resolveLocally gives the proxy an address`, async () => {
      const server = await opampServer();
      const proxy = await socksProxy("agent", "pw");
      try {
        const url = `${socks}://agent:pw@127.0.0.1:${proxy.port}`;
        await run(server, transport, false, { proxy: { url, resolveLocally: true } });
        assert.ok(proxy.tunnels.length > 0);
        for (const t of proxy.tunnels) {
          assert.ok(t.atyp === 1 || t.atyp === 4, `an address (ATYP ${t.atyp})`);
          assert.ok(!t.target.startsWith("localhost"), t.target);
        }
        assert.ok(proxy.tunnels.some((t) => t.target === `127.0.0.1:${server.port}`), "the address the server listens on");
      } finally {
        proxy.close();
        server.close();
      }
    });
  }
}

// covers: RULE-TLS-01, RULE-TLS-02, RULE-PROXY-01
test("downloads go the same way: the CA, the client certificate, the proxy", async () => {
  const file = Buffer.from("package".repeat(1000));
  const server = await opampServer({ ...SERVER, clientAuth: true }, { "/file": file });
  const other = await opampServer(OTHER_SERVER, { "/file": file });
  const httpProxy = await connectProxy("agent", "pw");
  const socks = await socksProxy("agent", "pw");
  const state = join(mkdtempSync(join(tmpdir(), "opamp-tls-")), "state");
  try {
    let id = 0n;
    for (const proxy of [
      undefined,
      { url: `http://agent:pw@127.0.0.1:${httpProxy.port}` },
      { url: `socks5://agent:pw@127.0.0.1:${socks.port}` },
    ]) {
      let deliver: (i: Input) => void = () => {};
      const x = new Executor((i) => deliver(i), state, new Transport({ tls: { ca: CA, ...CLIENT }, proxy }));
      const get = (url: string) =>
        new Promise<Input>((resolve) => {
          deliver = resolve;
          id++;
          x.do(create(EffectSchema, { kind: { case: "download", value: { id, url, maxBytes: 1n << 20n, timeoutMs: 5000n } } }));
        });
      const ok = await get(`https://localhost:${server.port}/file`);
      assert.equal(ok.kind.case, "downloadResponse", (ok.kind.value as { detail?: string }).detail);
      assert.equal((ok.kind.value as { status: number }).status, 200);
      const sha = createHash("sha256").update(file).digest();
      assert.deepEqual([...(ok.kind.value as { sha256: Uint8Array }).sha256], [...sha]);
      const bad = await get(`https://localhost:${other.port}/file`);
      assert.equal(bad.kind.case, "downloadFailed");
      assert.match((bad.kind.value as { detail: string }).detail, /certificate/i);
      x.shutdown();
    }
    assert.ok(httpProxy.tunnels.length >= 2);
    assert.ok(socks.tunnels.length >= 2);
    for (const h of server.handshakes) assert.equal(h.peer, "test-agent");
  } finally {
    for (const s of [server, other, httpProxy, socks]) s.close();
  }
});

// In both modes: the options are read the same way, before anything is sent.
for (const mode of ["platform", "core"] as const) {
  test(`${mode} mode: settings it can't use throw at start`, () => {
    const start = (config: Partial<Config>) =>
      void Client.start({ endpoint: "https://localhost:1/v1/opamp", instanceUid: UID, mode, ...config }, {}).stop();
    assert.throws(() => start({ proxy: { url: "ftp://proxy:21" } }), /ftp/);
    for (const url of ["socks4://proxy:1080", "socks4a://proxy:1080", "SOCKS4://proxy:1080"]) {
      assert.throws(() => start({ proxy: { url } }), /socks4a?:\/\/ is not supported/, url);
    }
    for (const url of ["socks://proxy:1080", "socks5x://proxy:1080", "unix:///tmp/proxy"]) {
      assert.throws(() => start({ proxy: { url } }), /is not a proxy this client can use/, url);
    }
    for (const url of ["http://proxy:3128", "https://proxy:3128"]) {
      assert.throws(() => start({ proxy: { url, resolveLocally: true } }), /resolveLocally is for SOCKS5/, url);
      assert.doesNotThrow(() => start({ proxy: { url, resolveLocally: false } }), url);
    }
    assert.throws(() => start({ proxy: { url: "socks5://proxy:1080", resolveLocally: "yes" as never } }), /resolveLocally/);
    assert.throws(() => start({ proxy: { url: "not a url" } }), /not a URL/);
    // Never a direct connection instead: SOCKS4 and any scheme it doesn't know throw.
    assert.throws(() => start({ proxy: { url: "socks4://proxy:1080" } }), /socks4:\/\/ is not supported/);
    assert.throws(() => start({ proxy: { url: "SOCKS4A://u:secret@proxy" } }), (e: Error) => /socks4a/.test(e.message) && !/secret/.test(e.message));
    assert.throws(() => start({ proxy: { url: "socks://proxy" } }), /socks:\/\//);
    // resolveLocally is for SOCKS5 proxies only.
    for (const url of ["http://proxy:3128", "https://proxy", "proxy:3128"]) {
      assert.throws(() => start({ proxy: { url, resolveLocally: true } }), /resolveLocally/, url);
    }
    // No scheme: an HTTP proxy, IPv6 included (`proxy.example:` is not a scheme).
    for (const url of ["proxy.example:3128", "localhost:3128", "[::1]:3128", "u:p@proxy", "socks5://p", "socks5h://p"]) {
      assert.doesNotThrow(() => start({ proxy: { url } }), url);
    }
    for (const url of ["socks5://p", "socks5h://p:9050"]) assert.doesNotThrow(() => start({ proxy: { url, resolveLocally: true } }));
    assert.throws(() => start({ proxy: { url: "socks5://p", resolveLocally: "yes" as never } }), /resolveLocally is true or false/);
    assert.throws(() => start({ proxy: { url: "[::1" } }), /not a URL/);
    assert.throws(() => start({ tls: { minVersion: "TLSv1.1" as never } }), /versions/);
    assert.throws(() => start({ tls: { cert: CLIENT.cert } }), /cert and key/);
    assert.throws(() => start({ tls: { cert: CLIENT.cert, key: SERVER.key } }), /tls:/);
    assert.throws(() => start({ tls: { ca: "not a certificate" } }), /tls:/);
  });
}

// The mode a client runs in: core mode, unless it asks for platform mode by name. With HTTP_PROXY
// set and Node not told to read it, only core mode's rule takes the proxy: the proxy says which
// mode ran. What the mode doesn't honour throws.
test("with no mode named the client runs in core mode; \"platform\" is platform mode", async () => {
  const server = await opampServer();
  const proxy = await socksProxy("agent", "pw");
  try {
    await withEnv({ HTTP_PROXY: `socks5://agent:pw@127.0.0.1:${proxy.port}` }, async () => {
      for (const [mode, through] of [[undefined, true], ["core", true], ["platform", false]] as const) {
        const tunnels = proxy.tunnels.length;
        const messages = server.got.length;
        const notes: Notification[] = [];
        const client = Client.start(
          // By address: the proxy then needs no lookup of the server's name. The two tests before
          // this one start clients on proxies with made-up names (`proxy`, `p`, `proxy.example`)
          // and don't wait for them; on a CI runner those lookups hang and fill Node's four
          // lookup threads, and this proxy's lookup of `localhost` had not begun after 10 s.
          { endpoint: `http://127.0.0.1:${server.port}/v1/opamp`, instanceUid: UID, heartbeatIntervalMs: 50, capabilities: BigInt(AgentCapabilities.AgentCapabilities_ReportsHeartbeat), mode },
          { description },
          (n) => notes.push(n),
        );
        try {
          await until(`a message (mode ${mode})`, () => server.got.length > messages);
        } catch (e) {
          // What the client and the proxy saw, for a failure that only a CI runner has shown.
          const through = proxy.tunnels.slice(tunnels).map((t) => `${t.target}:${t.upstream ?? " nothing"}`);
          throw new Error(
            `${String(e)}; the client said ${JSON.stringify(failure(notes))}; tunnels asked for: ${JSON.stringify(through)}; ` +
              `Node ${process.version}; the server got ${server.got.length - messages} message(s)`,
          );
        } finally {
          await client.stop();
        }
        assert.equal(proxy.tunnels.length > tunnels, through, `mode ${mode}: through the environment's proxy`);
      }
    });
  } finally {
    proxy.close();
    server.close();
  }
  const start = (config: Partial<Config>) =>
    void Client.start({ endpoint: "http://localhost:1/v1/opamp", instanceUid: UID, proxy: false, ...config }, {}).stop();
  assert.throws(() => start({ mode: "python" as never }), /mode is "core" or "platform", not "python"/);
  assert.throws(() => start({ mode: "platform", spliceDownloads: false }), /spliceDownloads in platform mode/);
  assert.doesNotThrow(() => start({ mode: "platform", spliceDownloads: true }));
  assert.doesNotThrow(() => start({ spliceDownloads: false }));
});

// covers: RULE-PROXY-01
test("any credentials in the proxy URL reach the proxy as they were", async () => {
  const server = net.createServer((c) => c.end());
  const port = await listen(server);
  // A username has no ':' (RFC 7617); SOCKS5 takes 1 to 255 bytes of each (RFC 1929).
  const cred = (colon: boolean) =>
    fc.string({ unit: "grapheme", minLength: 1, maxLength: 40 }).filter((s) => (colon || !s.includes(":")) && Buffer.byteLength(s) <= 255);
  try {
    await fc.assert(
      fc.asyncProperty(cred(false), cred(true), fc.boolean(), async (user, pass, socks) => {
        const proxy = socks ? await socksProxy(user, pass) : await connectProxy(user, pass);
        const scheme = socks ? "socks5" : "http";
        const url = `${scheme}://${encodeURIComponent(user)}:${encodeURIComponent(pass)}@127.0.0.1:${proxy.port}`;
        const t = new Transport({ proxy: { url } });
        try {
          const s = await t.connect("127.0.0.1", port, false);
          s.destroy();
          assert.deepEqual(
            proxy.tunnels.map((x) => [x.user, x.pass]),
            [[user, pass]],
          );
        } finally {
          t.close();
          proxy.close();
        }
      }),
      { numRuns: 60 },
    );
  } finally {
    server.close();
  }
});

/** The variables Node's proxies from the environment read. */
const PROXY_VARS = ["NODE_USE_ENV_PROXY", "http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "no_proxy", "NO_PROXY"];

/** Run `f` with these variables (and the rest of PROXY_VARS unset), then put them back. */
async function withEnv(vars: Record<string, string>, f: () => Promise<void>): Promise<void> {
  const saved = Object.fromEntries(PROXY_VARS.map((k) => [k, process.env[k]]));
  for (const k of PROXY_VARS) delete process.env[k];
  Object.assign(process.env, vars);
  try {
    await f();
  } finally {
    for (const k of PROXY_VARS) {
      if (saved[k] === undefined) delete process.env[k];
      else process.env[k] = saved[k];
    }
  }
}

const major = Number(process.versions.node.split(".")[0]);
const hasFlag = process.allowedNodeEnvironmentFlags.has("--use-env-proxy");
/** This Node takes proxies from the environment when told to (22.21 or later, 24 or later). */
const envProxySupported = hasFlag || major >= 24;

test("Node's switch for proxies from the environment", () => {
  const on = (env: NodeJS.ProcessEnv, execArgv: string[] = []) => envProxyOn({ env, execArgv });
  assert.equal(on({}), false);
  assert.equal(on({ HTTP_PROXY: "http://p:1" }), false, "the variables alone do nothing");
  if (hasFlag) {
    assert.equal(on({ NODE_USE_ENV_PROXY: "1" }), true);
    for (const v of ["0", "true", "yes", ""]) assert.equal(on({ NODE_USE_ENV_PROXY: v }), false, v);
    assert.equal(on({}, ["--use-env-proxy"]), true);
    assert.equal(on({ NODE_OPTIONS: "--max-old-space-size=100 --use-env-proxy" }), true);
    // A flag beats the variable; the command line beats NODE_OPTIONS.
    assert.equal(on({ NODE_USE_ENV_PROXY: "1" }, ["--no-use-env-proxy"]), false);
    assert.equal(on({ NODE_USE_ENV_PROXY: "0" }, ["--use-env-proxy"]), true);
    assert.equal(on({ NODE_OPTIONS: "--use-env-proxy" }, ["--no-use-env-proxy"]), false);
  } else if (major >= 24) {
    // 24.0 to 24.4: the variable only, any value.
    assert.equal(on({ NODE_USE_ENV_PROXY: "1" }), true);
    assert.equal(on({ NODE_USE_ENV_PROXY: "true" }), true);
  } else {
    assert.equal(on({ NODE_USE_ENV_PROXY: "1" }), false, "this Node has no proxies from the environment");
  }
});

test("NO_PROXY", () => {
  const list = (s: string) => s.split(",").map((e) => e.trim().toLowerCase());
  const cases: [string, string, number, boolean][] = [
    ["*", "anything", 80, true],
    ["example.com", "EXAMPLE.com", 443, true],
    ["example.com", "a.example.com", 443, false],
    ["localhost:8080", "localhost", 8080, true],
    ["localhost:8080", "localhost", 80, false],
    [".example.com", "a.example.com", 80, true],
    [".example.com", "example.com", 80, true],
    [".example.com", "badexample.com", 80, false],
    ["*.example.com", "a.b.example.com", 80, true],
    ["*.example.com", "example.com", 80, false],
    ["10.0.0.1 - 10.0.0.9", "10.0.0.5", 80, true],
    ["10.0.0.1-10.0.0.9", "10.0.0.10", 80, false],
    [" Other.org , LOCALHOST ", "localhost", 80, true],
    ["other.org", "localhost", 80, false],
  ];
  for (const [noProxy, host, port, want] of cases) {
    assert.equal(bypassed(list(noProxy), host, port), want, `${noProxy} / ${host}:${port}`);
  }
});

// Node's own reading (its internals, where this Node has them) is the oracle.
test("NO_PROXY is decided as Node decides it", { skip: !hasFlag }, () => {
  const host = fc.constantFrom("example.com", "a.example.com", "badexample.com", "LocalHost", "10.0.0.5", "192.168.1.20", "::1");
  const entry = fc.constantFrom("*", ".example.com", "*.example.com", "example.com", "localhost:8080", "localhost",
    "10.0.0.1-10.0.0.9", " 192.168.1.0 - 192.168.1.255 ", "10.0.0.9-10.0.0.1", "", "EXAMPLE.COM", "::1", "a-b.example.com");
  const cases = fc.sample(fc.tuple(fc.array(entry, { maxLength: 4 }).map((e) => e.join(",")), host, fc.constantFrom(80, 443, 8080)), 300);
  const script = `
    const { parseProxyConfigFromEnv } = require("internal/http");
    const cases = JSON.parse(require("fs").readFileSync(0, "utf8"));
    console.log(JSON.stringify(cases.map(([noProxy, host, port]) =>
      !parseProxyConfigFromEnv({ HTTP_PROXY: "http://proxy:1", NO_PROXY: noProxy }, "http:", false).shouldUseProxy(host, port))));`;
  const node: boolean[] = JSON.parse(
    execFileSync(process.execPath, ["--expose-internals", "-e", script], { input: JSON.stringify(cases) }).toString(),
  );
  // Older Node releases matched `.example.com` against badexample.com too; we follow the newer
  // (curl's) rule, so on such a Node the cases that differ only by that are left out.
  const oldNode = JSON.parse(
    execFileSync(process.execPath, ["--expose-internals", "-e", script], {
      input: JSON.stringify([[".example.com", "badexample.com", 80]]),
    }).toString(),
  )[0] as boolean;
  const onlyTheOldQuirk = (noProxy: string, host: string) =>
    oldNode &&
    noProxy.split(",").some((e) => {
      const entry = e.trim().toLowerCase();
      return entry.startsWith(".") && host.toLowerCase().endsWith(entry.slice(1)) && !host.toLowerCase().endsWith(entry);
    });
  cases.forEach(([noProxy, host, port], i) => {
    if (onlyTheOldQuirk(noProxy, host)) return;
    const ours = bypassed(noProxy ? noProxy.split(",").map((e) => e.trim().toLowerCase()) : [], host, port);
    assert.equal(ours, node[i], `NO_PROXY=${JSON.stringify(noProxy)} ${host}:${port}`);
  });
});

// covers: RULE-PROXY-01
test("without a proxy: the environment's when Node is told to use it, else direct", { skip: !envProxySupported }, async () => {
  const plain = await opampServer();
  const secure = await opampServer(SERVER, { "/file": Buffer.from("package") });
  const proxy = await connectProxy("agent", "pw");
  const url = `http://agent:pw@127.0.0.1:${proxy.port}`;
  const state = join(mkdtempSync(join(tmpdir(), "opamp-env-")), "state");
  let id = 0n;
  const download = (config: { proxy?: false }) =>
    new Promise<Input>((resolve) => {
      const x = new Executor((i) => {
        x.shutdown();
        resolve(i);
      }, state, new Transport({ tls: { ca: CA }, ...config }));
      id++;
      const value = { id, url: `https://localhost:${secure.port}/file`, maxBytes: 1n << 20n, timeoutMs: 5000n };
      x.do(create(EffectSchema, { kind: { case: "download", value } }));
    });
  /** How many tunnels each scheme and a download open, and that each works. */
  const tunnels = async (config: { proxy?: false } = {}): Promise<number[]> => {
    const counts: number[] = [];
    for (const [transport, tls] of [["http", false], ["ws", false], ["http", true], ["ws", true]] as const) {
      const before = proxy.tunnels.length;
      await run(tls ? secure : plain, transport, tls, { tls: { ca: CA }, ...config });
      counts.push(proxy.tunnels.length - before);
    }
    const before = proxy.tunnels.length;
    assert.equal((await download(config)).kind.case, "downloadResponse");
    counts.push(proxy.tunnels.length - before);
    return counts;
  };
  try {
    const on = { NODE_USE_ENV_PROXY: "1", HTTP_PROXY: url, HTTPS_PROXY: url };
    await withEnv(on, async () => {
      assert.ok((await tunnels()).every((n) => n > 0), "every scheme and the download go through the proxy");
      for (const t of proxy.tunnels) assert.equal(t.target.split(":")[0], "localhost");
      assert.deepEqual(await tunnels({ proxy: false }), [0, 0, 0, 0, 0], "proxy: false goes direct");
    });
    await withEnv({ ...on, NO_PROXY: "example.com, localhost" }, async () => {
      assert.deepEqual(await tunnels(), [0, 0, 0, 0, 0], "NO_PROXY goes direct");
    });
    // Lower case wins, as in Node: a proxy that isn't there fails.
    await withEnv({ ...on, http_proxy: "http://127.0.0.1:1" }, async () => {
      await run(plain, "http", false, {}, { fail: true });
    });
    await withEnv({ HTTP_PROXY: url, HTTPS_PROXY: url }, async () => {
      assert.deepEqual(await tunnels(), [0, 0, 0, 0, 0], "Node not told to: direct");
    });
  } finally {
    for (const s of [plain, secure, proxy]) s.close();
  }
});

// covers: RULE-PROXY-01
test("the environment's proxies follow the same rules, resolveLocally too", { skip: !envProxySupported }, () => {
  const platform = (vars: Record<string, string>) => ({
    env: { NODE_USE_ENV_PROXY: "1", ...vars },
    execArgv: hasFlag ? ["--use-env-proxy"] : [],
  });
  const proxyFor = (vars: Record<string, string>, proxy?: { resolveLocally: boolean }) => {
    const t = new Transport({ proxy }, platform(vars));
    try {
      return t.proxyFor("https://opamp.example.com/v1/opamp");
    } finally {
      t.close();
    }
  };
  assert.deepEqual(proxyFor({ HTTPS_PROXY: "socks5://p:1080" }), { url: "socks5://p:1080", headers: {}, resolveLocally: false });
  assert.equal(proxyFor({ HTTPS_PROXY: "socks5h://p" })?.resolveLocally, false);
  assert.equal(proxyFor({ HTTPS_PROXY: "socks5://p" }, { resolveLocally: true })?.resolveLocally, true);
  assert.equal(proxyFor({ HTTPS_PROXY: "proxy.example:3128" })?.url, "proxy.example:3128");
  // Not ignored, and not direct: an error.
  for (const url of ["socks4://p", "socks4a://p", "ftp://p"]) {
    assert.throws(() => proxyFor({ HTTPS_PROXY: url }), /proxy:/, url);
  }
  assert.throws(() => proxyFor({ HTTP_PROXY: "http://p:3128" }, { resolveLocally: true }), /resolveLocally/);
  // Not told to use the environment: nothing read, nothing to refuse.
  const off = new Transport({}, { env: { HTTPS_PROXY: "socks4://p" }, execArgv: [] });
  assert.equal(off.proxyFor("https://opamp.example.com/"), undefined);
  off.close();
});

// covers: RULE-PROXY-01
test("SOCKS5 from the environment: the proxy resolves; SOCKS4 and unknown schemes are errors", { skip: !envProxySupported }, async () => {
  const server = await opampServer();
  const proxy = await socksProxy("agent", "pw");
  const start = () => void Client.start({ endpoint: "http://localhost:1/v1/opamp", instanceUid: UID, mode: "platform" }, {}).stop();
  try {
    for (const socks of ["socks5", "socks5h"]) {
      const url = `${socks}://agent:pw@127.0.0.1:${proxy.port}`;
      await withEnv({ NODE_USE_ENV_PROXY: "1", HTTP_PROXY: url }, async () => {
        const before = proxy.tunnels.length;
        for (const transport of ["http", "ws"] as const) await run(server, transport, false, {});
        const mine = proxy.tunnels.slice(before);
        assert.ok(mine.length >= 2, socks);
        for (const t of mine) {
          assert.equal(t.atyp, 3, `${socks}: a name (ATYP DOMAIN)`);
          assert.equal(t.target, `localhost:${server.port}`);
        }
      });
    }
    for (const [v, want] of [
      ["socks4://127.0.0.1:1080", /socks4:\/\/ is not supported/],
      ["socks4a://127.0.0.1:1080", /socks4a:\/\/ is not supported/],
      ["ftp://127.0.0.1:21", /ftp:\/\/ is not a proxy/],
    ] as const) {
      for (const name of ["HTTP_PROXY", "https_proxy"]) {
        await withEnv({ NODE_USE_ENV_PROXY: "1", [name]: v }, async () => assert.throws(start, want, `${name}=${v}`));
      }
      // Not told to use the environment: not read, so no error.
      await withEnv({ HTTP_PROXY: v }, async () => assert.doesNotThrow(start));
    }
  } finally {
    proxy.close();
    server.close();
  }
});
