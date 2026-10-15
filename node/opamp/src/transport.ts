// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// How the client reaches the server: the TLS it trusts and presents, and the proxy it goes
// through. One Transport is built from one set of options, and gives an undici dispatcher (for
// the plain HTTP requests and package downloads) and options for the ws package (for WebSocket),
// so all three go the same way.
//
// Without a proxy in the options, the environment's apply. In Rust mode (the client's default)
// by the rule every SDK's Rust mode shares (rustProxy below). In Node mode by Node's own default:
// the proxies in the environment (HTTP_PROXY, HTTPS_PROXY, NO_PROXY) when Node is told to use
// them (NODE_USE_ENV_PROXY=1 or --use-env-proxy), read the way Node reads them; otherwise none.
// `proxy: false` goes direct.
// Under CGI (REQUEST_METHOD set) the environment's proxies are ignored, as Go, Python, Rust and
// .NET's clients do: there HTTP_PROXY may be a request's Proxy header (httpoxy). A proxy in the
// options still applies.
//
// A SOCKS5 proxy, socks5:// or socks5h:// alike, is given the server's name to resolve (unlike
// curl, where socks5:// resolves on the client), unless `resolveLocally` says to resolve it here.

import { X509Certificate } from "node:crypto";
import { lookup } from "node:dns/promises";
import http, { type ClientRequestArgs } from "node:http";
import https from "node:https";
import net from "node:net";
import tls from "node:tls";

import { SocksClient } from "socks";
import { Agent, buildConnector, type Dispatcher } from "undici";

/** A TLS version the client may use. Nothing older than 1.2. */
export type TlsVersion = "TLSv1.2" | "TLSv1.3";

/** TLS for https:// and wss:// endpoints, and for package downloads. */
export interface TlsOptions {
  /** The CA certificates (PEM) to verify the server's certificate against. They replace the CAs
   * Node trusts by default unless `includeDefaultCa` is set. Omitted: Node's defaults. */
  ca?: string | Buffer | (string | Buffer)[];
  /** Trust Node's default CAs as well as `ca` (its bundled roots, NODE_EXTRA_CA_CERTS, and the
   * system's with --use-system-ca). Default false. */
  includeDefaultCa?: boolean;
  /** A client certificate (PEM, chain included) to present in every handshake, with `key`. */
  cert?: string | Buffer;
  /** The client certificate's private key (PEM, not encrypted). */
  key?: string | Buffer;
  /** The oldest TLS version to accept; default TLS 1.2. */
  minVersion?: TlsVersion;
  /** The newest TLS version to offer; default TLS 1.3. */
  maxVersion?: TlsVersion;
}

/** A proxy to reach the server through. */
export interface ProxyOptions {
  /** `http://`, `https://`, `socks5://` or `socks5h://`, with `user:password@` when the proxy
   * needs them (percent-encoded); `host:port` with no scheme is an `http://` proxy. Every
   * connection is a tunnel: CONNECT through an HTTP proxy, a SOCKS5 CONNECT otherwise. The proxy
   * resolves the server's name, for `socks5://` as for `socks5h://` (unlike curl, which resolves
   * it itself for `socks5://`). `socks4://`, `socks4a://` and any other scheme throw. Omitted:
   * the environment's proxies, as without `proxy`, with `resolveLocally`. */
  url?: string;
  /** Headers sent to an HTTP proxy with each CONNECT. */
  headers?: Record<string, string>;
  /** Resolve the server's name here and give a SOCKS5 proxy its address, for a proxy that can't
   * resolve names the client can. Default false. SOCKS5 only: with an `http://` or `https://`
   * proxy it throws. Only the application sets it, never the server. */
  resolveLocally?: boolean;
}

/** Everything about how to reach the server. */
export interface ConnectionOptions {
  tls?: TlsOptions;
  /** A proxy; `false` for none, whatever the environment says; omitted, Node's default. */
  proxy?: ProxyOptions | false;
}

/** Where a Transport reads Node's proxy settings: the process, unless a test says otherwise. */
export interface Platform {
  env: NodeJS.ProcessEnv;
  execArgv: string[];
}

/** What the ws package takes from a Transport. */
export interface WsOptions {
  secureContext?: tls.SecureContext;
  createConnection?: ClientRequestArgs["createConnection"];
}

/** How long a proxy has to open a tunnel. */
const PROXY_TIMEOUT_MS = 30_000;

interface Proxy {
  /** As given: Rust mode hands it to the core. */
  url: string;
  /** Resolve the server's name here and give the SOCKS5 proxy the address. */
  resolveLocally: boolean;
  kind: "http" | "socks5";
  secure: boolean;
  host: string;
  port: number;
  user: string;
  password: string;
  headers: Record<string, string>;
}

const SCHEMES = ["http", "https", "socks5", "socks5h"];

/** The proxy `url` names (from `from`, for errors), resolving names locally if `resolveLocally`.
 * Errors leave the URL out: it may hold a password. */
function parseProxy(p: ProxyOptions & { url: string }, from = "the url"): Proxy {
  // No scheme is http://. A scheme is only one followed by `//`: `proxy.example:3128` would
  // otherwise be the scheme `proxy.example`.
  const text = /^[a-z][a-z0-9+.-]*:\/\//i.test(p.url) ? p.url : `http://${p.url}`;
  let url: URL;
  try {
    url = new URL(text);
  } catch {
    throw new TypeError(`proxy: ${from} is not a URL`);
  }
  const scheme = url.protocol.slice(0, -1).toLowerCase();
  if (scheme === "socks4" || scheme === "socks4a") {
    throw new TypeError(`proxy: ${scheme}:// is not supported (SOCKS4 and SOCKS4a are refused); use socks5://`);
  }
  if (!SCHEMES.includes(scheme)) {
    throw new TypeError(`proxy: ${scheme}:// is not a proxy this client can use (http, https, socks5 or socks5h)`);
  }
  const socks = scheme.startsWith("socks5");
  if (p.resolveLocally !== undefined && typeof p.resolveLocally !== "boolean") {
    throw new TypeError("proxy: resolveLocally is true or false");
  }
  if (p.resolveLocally && !socks) {
    throw new TypeError(`proxy: resolveLocally is for SOCKS5 proxies, not ${scheme}:// (from ${from})`);
  }
  const host = url.hostname.startsWith("[") ? url.hostname.slice(1, -1) : url.hostname;
  if (host === "") throw new TypeError(`proxy: ${from} has no host`);
  return {
    url: p.url,
    resolveLocally: p.resolveLocally ?? false,
    kind: socks ? "socks5" : "http",
    secure: scheme === "https",
    host,
    port: Number(url.port) || (socks ? 1080 : scheme === "https" ? 443 : 80),
    user: decodeURIComponent(url.username),
    password: decodeURIComponent(url.password),
    headers: p.headers ?? {},
  };
}

/** The certificates in `v`, each checked: Node would skip what it can't read, and trust less
 * than it was told to without saying so. */
function certificates(v: string | Buffer | (string | Buffer)[]): string[] {
  const out: string[] = [];
  for (const pem of Array.isArray(v) ? v : [v]) {
    const found = pem.toString().match(/-----BEGIN CERTIFICATE-----[^-]+-----END CERTIFICATE-----/g) ?? [];
    if (found.length === 0) throw new TypeError("tls: ca holds no PEM certificate");
    for (const cert of found) {
      try {
        new X509Certificate(cert);
      } catch (err) {
        throw new TypeError(`tls: a certificate in ca: ${(err as Error).message}`);
      }
    }
    out.push(...found);
  }
  return out;
}

/** Whether Node is told to take proxies from the environment. Node 22.21 and later, and 24.5 and
 * later: --use-env-proxy, in NODE_OPTIONS or on the command line (the last said wins, and
 * --no-use-env-proxy turns it off), or else NODE_USE_ENV_PROXY=1. Node 24.0 to 24.4 have only
 * NODE_USE_ENV_PROXY, any value (for fetch); older Node has neither. */
export function envProxyOn({ env, execArgv }: Platform): boolean {
  if (!process.allowedNodeEnvironmentFlags.has("--use-env-proxy")) {
    return Number(process.versions.node.split(".")[0]) >= 24 && Boolean(env.NODE_USE_ENV_PROXY);
  }
  let on = env.NODE_USE_ENV_PROXY === "1";
  for (const arg of [...(env.NODE_OPTIONS ?? "").split(/\s+/), ...execArgv]) {
    if (arg === "--use-env-proxy") on = true;
    else if (arg === "--no-use-env-proxy") on = false;
  }
  return on;
}

/** The proxies in the environment, read as Node reads them (lib/internal/http.js): lower case
 * before upper. Where Node ignores anything but http:// and https://, this client also takes
 * socks5:// and socks5h:// (the proxy resolves names, as in the options) and refuses anything
 * else: a proxy it can't use is an error, not a direct connection. */
interface EnvProxies {
  http: Proxy | undefined;
  https: Proxy | undefined;
  noProxy: string[];
}

function envProxies(env: NodeJS.ProcessEnv, resolveLocally: boolean): EnvProxies {
  const read = (lower: string, upper: string): Proxy | undefined => {
    const name = env[lower] ? lower : upper;
    const url = env[name];
    if (!url) return undefined;
    return parseProxy({ url, resolveLocally }, name);
  };
  const noProxy = env.no_proxy || env.NO_PROXY;
  return {
    http: read("http_proxy", "HTTP_PROXY"),
    https: read("https_proxy", "HTTPS_PROXY"),
    noProxy: noProxy ? noProxy.split(",").map((e) => e.trim().toLowerCase()) : [],
  };
}

/** Rust mode's proxy rule (tests/shell-tables/README.md, "Rust mode's proxy rule"; the
 * reference's, the Rust client's): the proxy the environment `env` names for `url`,
 * trimmed, as given; undefined: directly. The same in every SDK's Rust mode. */
export function rustProxy(env: NodeJS.ProcessEnv, url: string): string | undefined {
  // Under CGI, HTTP_PROXY may be a request's Proxy header (httpoxy): no variable is taken.
  if (env.REQUEST_METHOD !== undefined) return undefined;
  // The upper-case name if it is set at all (even empty), else the lower-case one.
  const read = (upper: string): string | undefined => env[upper] ?? env[upper.toLowerCase()];
  // Empty (or spaces) is unset.
  const proxy = (upper: string): string | undefined => read(upper)?.trim() || undefined;
  const u = new URL(url);
  const secure = u.protocol === "https:" || u.protocol === "wss:";
  const named = proxy(secure ? "HTTPS_PROXY" : "HTTP_PROXY") ?? proxy("ALL_PROXY");
  if (named === undefined) return undefined;
  const host = u.hostname.replace(/^\[(.*)\]$/, "$1");
  return rustNoProxy(read("NO_PROXY") ?? "", host) ? undefined : named;
}

/** An IP address as its family and value, or undefined for anything else (a name: nothing is
 * resolved). IPv4: four decimal parts, 0 to 255, without leading zeros; IPv6: its text forms
 * (an IPv4 tail too), without a zone. */
function ipValue(text: string): { v6: boolean; value: bigint } | undefined {
  const v4 = (t: string): bigint | undefined => {
    const parts = t.split(".");
    if (parts.length !== 4) return undefined;
    let n = 0n;
    for (const part of parts) {
      if (!/^(0|[1-9][0-9]{0,2})$/.test(part) || Number(part) > 255) return undefined;
      n = (n << 8n) | BigInt(part);
    }
    return n;
  };
  const four = v4(text);
  if (four !== undefined) return { v6: false, value: four };
  if (!text.includes(":") || !/^[0-9a-fA-F:.]+$/.test(text)) return undefined;
  // An IPv4 tail is two groups.
  let groups = text;
  const lastColon = text.lastIndexOf(":");
  if (text.slice(lastColon + 1).includes(".")) {
    const tail = v4(text.slice(lastColon + 1));
    if (tail === undefined) return undefined;
    groups = `${text.slice(0, lastColon + 1)}${(tail >> 16n).toString(16)}:${(tail & 0xffffn).toString(16)}`;
  }
  const halves = groups.split("::");
  if (halves.length > 2) return undefined;
  const side = (h: string): string[] | undefined => {
    if (h === "") return [];
    const g = h.split(":");
    return g.every((x) => /^[0-9a-fA-F]{1,4}$/.test(x)) ? g : undefined;
  };
  const head = side(halves[0] ?? "");
  const tail = halves.length === 2 ? side(halves[1] ?? "") : [];
  if (head === undefined || tail === undefined) return undefined;
  const missing = 8 - head.length - tail.length;
  if (halves.length === 2 ? missing < 1 : missing !== 0) return undefined;
  let n = 0n;
  for (const g of [...head, ...Array<string>(missing).fill("0"), ...tail]) n = (n << 16n) | BigInt(`0x${g}`);
  return { v6: true, value: n };
}

/** Whether NO_PROXY `list` sends `host` (a name, or an address without brackets) direct, by Rust
 * mode's rule: an address by the address and network entries, a name by the name entries. */
export function rustNoProxy(list: string, host: string): boolean {
  const ip = ipValue(host);
  const name = host.toLowerCase();
  for (const raw of list.split(",")) {
    const entry = raw.trim();
    if (entry === "") continue;
    // An address entry: a network (address/prefix) or an address.
    const slash = entry.indexOf("/");
    const net = ipValue(slash < 0 ? entry : entry.slice(0, slash));
    const bits = net === undefined ? 0 : net.v6 ? 128 : 32;
    const prefix = slash < 0 ? bits : /^(0|[1-9][0-9]{0,2})$/.test(entry.slice(slash + 1)) ? Number(entry.slice(slash + 1)) : -1;
    if (net !== undefined && prefix >= 0 && prefix <= bits) {
      if (ip !== undefined && ip.v6 === net.v6) {
        const shift = BigInt(bits - prefix);
        if (ip.value >> shift === net.value >> shift) return true;
      }
      continue;
    }
    // A name entry: never an address's.
    if (ip !== undefined) continue;
    const d = entry.toLowerCase();
    if (d === name || (d.startsWith(".") && d.slice(1) === name)) return true;
    if (name.endsWith(d)) {
      if (d.startsWith(".") || name[name.length - d.length - 1] === ".") return true;
    } else if (d === "*") {
      return true;
    }
  }
  return false;
}

const ipv4 = (ip: string): number => ip.split(".").reduce((n, b) => n * 256 + Number(b), 0);

/** Whether NO_PROXY (split, trimmed, lower case) sends `host:port` direct, as Node decides it:
 * `*`; the host, or host:port; `.example.com` for example.com and its subdomains (curl's rule,
 * as recent Node 24 releases do: never badexample.com); `*.example.com` for the subdomains; an IPv4 range
 * `a.b.c.d-e.f.g.h`. */
export function bypassed(noProxy: string[], hostname: string, port: number): boolean {
  const host = hostname.toLowerCase();
  for (const entry of noProxy) {
    if (entry === "*" || entry === host || entry === `${host}:${port}`) return true;
    if (entry.startsWith(".") && (host === entry.slice(1) || host.endsWith(entry))) return true;
    if (entry.startsWith("*.") && host.endsWith(entry.slice(1))) return true;
    if (entry.includes("-") && net.isIPv4(host)) {
      const [start = "", end = ""] = entry.split("-").map((e) => e.trim());
      if (net.isIPv4(start) && net.isIPv4(end) && ipv4(host) >= ipv4(start) && ipv4(host) <= ipv4(end)) return true;
    }
  }
  return false;
}

/** The CAs Node trusts by default (tls.getCACertificates is Node 22.15 or later). */
function defaultCa(): string[] {
  const get = (tls as { getCACertificates?: (type: "default") => string[] }).getCACertificates;
  return get ? get("default") : [...tls.rootCertificates];
}

const VERSIONS: (TlsVersion | undefined)[] = ["TLSv1.2", "TLSv1.3", undefined];

function secureContext(o: TlsOptions): tls.SecureContext {
  if ((o.cert === undefined) !== (o.key === undefined)) {
    throw new TypeError("tls: cert and key go together");
  }
  if (!VERSIONS.includes(o.minVersion) || !VERSIONS.includes(o.maxVersion)) {
    throw new TypeError("tls: versions are TLSv1.2 or TLSv1.3");
  }
  // Node takes a minimum above the maximum, and fails every handshake later.
  if (o.minVersion === "TLSv1.3" && o.maxVersion === "TLSv1.2") {
    throw new TypeError("tls: minVersion TLSv1.3 is above maxVersion TLSv1.2: no version left");
  }
  const ca = o.ca === undefined ? undefined : [...(o.includeDefaultCa ? defaultCa() : []), ...certificates(o.ca)];
  try {
    return tls.createSecureContext({
      ca,
      cert: o.cert,
      key: o.key,
      minVersion: o.minVersion ?? "TLSv1.2",
      maxVersion: o.maxVersion,
    });
  } catch (err) {
    throw new TypeError(`tls: ${(err as Error).message}`);
  }
}

/** The name to send in TLS SNI: none for an address. */
const servername = (host: string): string | undefined => (net.isIP(host) ? undefined : host);

/** The TLS, client certificate and proxy for connections to the server, built once from options. */
export class Transport {
  /** Trust, client certificate and versions: for every TLS handshake, the server's and an
   * https:// proxy's alike. */
  readonly #context: tls.SecureContext;
  /** The proxy the options name; else those of the environment, when Node is told to use them. */
  readonly #proxy: Proxy | undefined;
  readonly #env: EnvProxies | undefined;
  /** For fetch: the plain HTTP requests and downloads. */
  readonly dispatcher: Dispatcher;

  /** Rust mode: the environment Rust mode's proxy rule reads, when no proxy is named (and not
   * `false`); undefined otherwise. */
  readonly #rustEnv: NodeJS.ProcessEnv | undefined;
  /** `proxy.resolveLocally` without a `url`: for a SOCKS5 proxy the environment names. */
  readonly #resolveLocally: boolean;

  /** `rust`: Rust mode, whose answers to the core's proxy questions (proxyFor) follow the shared
   * rule (tests/shell-tables/README.md) rather than Node's convention. */
  constructor(options: ConnectionOptions = {}, platform: Platform = process, rust = false) {
    this.#context = secureContext(options.tls ?? {});
    const p = options.proxy;
    this.#proxy = p && p.url !== undefined ? parseProxy({ ...p, url: p.url }) : undefined;
    // No proxy named (and not `false`): the environment's, when Node is told to use it.
    const env = p === undefined || (p !== false && p.url === undefined);
    this.#resolveLocally = (p && p.resolveLocally) ?? false;
    this.#rustEnv = rust && env ? platform.env : undefined;
    // Under CGI, HTTP_PROXY may be a request's Proxy header (httpoxy): none of them is taken.
    // Set at all, even empty: the conservative reading (tests/shell-tables/proxy.json).
    const cgi = platform.env.REQUEST_METHOD !== undefined;
    this.#env =
      env && !rust && !cgi && envProxyOn(platform)
        ? envProxies(platform.env, (p && p.resolveLocally) ?? false)
        : undefined;
    const direct = buildConnector({ secureContext: this.#context });
    this.dispatcher = new Agent({
      connect: (o, callback) => {
        const secure = o.protocol === "https:";
        const port = Number(o.port) || (secure ? 443 : 80);
        const proxy = this.#route(o.hostname, port, secure);
        if (!proxy) {
          direct(o, callback);
          return;
        }
        this.#through(proxy, o.hostname, port, secure).then(
          (socket) => callback(null, socket),
          (err: Error) => callback(err, null),
        );
      },
    });
  }

  /** The proxy to reach `host:port` through, if any. */
  #route(host: string, port: number, secure: boolean): Proxy | undefined {
    if (this.#proxy || !this.#env) return this.#proxy;
    const proxy = secure ? this.#env.https : this.#env.http;
    return proxy && !bypassed(this.#env.noProxy, host, port) ? proxy : undefined;
  }

  /** Trust, client certificate and versions, for Rust mode's TLS on a stream. */
  get secureContext(): tls.SecureContext {
    return this.#context;
  }

  /** Rust mode: the proxy for `url` (http or https), if any: its URL, CONNECT headers, and
   * whether the core is to have the server's name resolved here. */
  proxyFor(url: string): { url: string; headers: Record<string, string>; resolveLocally: boolean } | undefined {
    if (this.#rustEnv !== undefined) {
      // The shared rule: the value as the environment gives it (the core reads it, and refuses
      // a scheme it can't use: the connection fails, never goes direct).
      const named = rustProxy(this.#rustEnv, url);
      if (named === undefined) return undefined;
      const socks = /^socks5h?:\/\//i.test(named);
      return { url: named, headers: {}, resolveLocally: this.#resolveLocally && socks };
    }
    const u = new URL(url);
    const secure = u.protocol === "https:" || u.protocol === "wss:";
    const host = u.hostname.replace(/^\[(.*)\]$/, "$1");
    const proxy = this.#route(host, Number(u.port) || (secure ? 443 : 80), secure);
    return proxy && { url: proxy.url, headers: proxy.headers, resolveLocally: proxy.resolveLocally };
  }

  /** Options for a WebSocket to `url`. */
  ws(url: string): WsOptions {
    const u = new URL(url);
    const secure = u.protocol === "wss:" || u.protocol === "https:";
    const host = u.hostname.replace(/^\[(.*)\]$/, "$1");
    const port = Number(u.port) || (secure ? 443 : 80);
    const proxy = this.#route(host, port, secure);
    // ws hands its options to tls.connect.
    if (!proxy) return { secureContext: this.#context };
    return {
      createConnection: (_o, done) => {
        this.#through(proxy, host, port, secure).then(
          (s) => done(null, s),
          (err: Error) => done(err, undefined as never),
        );
        return undefined;
      },
    };
  }

  /** A connection to `host:port` through the proxy for it, with TLS inside the tunnel when
   * `secure`: ready to use. (Direct connections are undici's and ws's own.) */
  async connect(host: string, port: number, secure: boolean): Promise<net.Socket> {
    const proxy = this.#route(host, port, secure);
    if (!proxy) throw new Error(`no proxy for ${host}:${port}`);
    return this.#through(proxy, host, port, secure);
  }

  async #through(proxy: Proxy, host: string, port: number, secure: boolean): Promise<net.Socket> {
    const raw = await this.#tunnel(proxy, host, port);
    if (!secure) return raw;
    return new Promise((resolve, reject) => {
      const s = tls.connect({ socket: raw, host, servername: servername(host), secureContext: this.#context });
      const fail = (err: Error) => {
        raw.destroy();
        reject(err);
      };
      s.once("error", fail);
      s.once("secureConnect", () => {
        s.off("error", fail);
        resolve(s);
      });
    });
  }

  /** A tunnel to `host:port` through proxy `p`. */
  async #tunnel(p: Proxy, host: string, port: number): Promise<net.Socket> {
    if (p.kind === "socks5") {
      const open = async (destination: string) => {
        const { socket } = await SocksClient.createConnection({
          proxy: {
            host: p.host,
            port: p.port,
            type: 5,
            ...(p.user || p.password ? { userId: p.user, password: p.password } : {}),
          },
          command: "connect",
          // A name goes as a name (the proxy resolves it), an address as an address.
          destination: { host: destination, port },
          timeout: PROXY_TIMEOUT_MS,
        });
        return socket;
      };
      if (!p.resolveLocally || net.isIP(host)) return open(host);
      // Resolved here: each address in turn while the proxy can't reach it, as a direct
      // connection would, and as Rust mode does (any other failure would be the same for each).
      const addresses = await lookup(host, { all: true });
      let last: unknown = new Error(`${host} resolved to no address`);
      for (const { address } of addresses) {
        try {
          return await open(address);
        } catch (err) {
          last = err;
          if (!/proxy rejected connection/i.test((err as Error).message)) break;
        }
      }
      throw last;
    }
    const target = net.isIPv6(host) ? `[${host}]:${port}` : `${host}:${port}`;
    const headers: Record<string, string> = { host: target, ...p.headers };
    if (p.user || p.password) {
      headers["proxy-authorization"] = `Basic ${Buffer.from(`${p.user}:${p.password}`).toString("base64")}`;
    }
    return new Promise((resolve, reject) => {
      const req = (p.secure ? https : http).request({
        host: p.host,
        port: p.port,
        method: "CONNECT",
        path: target,
        headers,
        agent: false,
        timeout: PROXY_TIMEOUT_MS,
        ...(p.secure ? { secureContext: this.#context, servername: servername(p.host) } : {}),
      });
      req.once("connect", (res, socket, head) => {
        res.resume();
        if (res.statusCode !== 200) {
          socket.destroy();
          reject(new Error(`the proxy refused the tunnel: ${res.statusCode} ${res.statusMessage ?? ""}`.trimEnd()));
          return;
        }
        socket.setTimeout(0);
        if (head.length > 0) socket.unshift(head);
        resolve(socket);
      });
      req.once("timeout", () => req.destroy(new Error("the proxy did not answer")));
      req.once("error", reject);
      req.end();
    });
  }

  /** Drop every connection. */
  close(): void {
    void this.dispatcher.destroy().catch(() => {});
  }
}
