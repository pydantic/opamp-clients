// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// An OpAMP client for Node.js.
//
// Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
// agent_disconnect, and the requests and responses themselves: headers, framing, Retry-After,
// size limits) is the shared OpAMP client core, run as WebAssembly. By default the core
// speaks HTTP and WebSocket itself too (Rust mode), over Node's TCP and TLS sockets; in Node mode
// this package carries out the core's requests with undici's fetch and the ws package instead.
// Either way it feeds back what happened, so it behaves exactly like the other languages'
// clients on the same core.

import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { basename, resolve } from "node:path";

import { create } from "@bufbuild/protobuf";

import { Core, TEST_CORE_MODULE, TEST_CORE_TAP } from "./core.js";
import { busyOn, type FileOps, journal, REAL, settled, TEST_FILE_OPS } from "./files.js";
import { CustomMessageSchema, PackageAvailableSchema } from "./gen/opamp/v1/opamp_pb.js";
import { Executor, TEST_PLATFORM, type TestPlatform, downloadPath, downloadsDir } from "./executor.js";
import { Streams } from "./streams.js";
import { Transport, type ProxyOptions, type TlsOptions } from "./transport.js";
import {
  ConfigSchema,
  EmptySchema,
  type Input,
  InputSchema,
  type Notification as CoreNotification,
  StatusField as CoreStatusField,
} from "./gen/opamp/core/v1/core_pb.js";
import type {
  AgentDescription,
  AgentRemoteConfig,
  AvailableComponents,
  CustomCapabilities,
  CustomMessage,
  ComponentHealth,
  ConnectionSettingsOffers,
  ConnectionSettingsStatus,
  EffectiveConfig,
  PackageAvailable,
  RemoteConfigStatus,
  ServerToAgentCommand,
} from "./gen/opamp/v1/opamp_pb.js";

export * from "./gen/opamp/v1/opamp_pb.js";
export * from "./gen/opamp/v1/anyvalue_pb.js";
export { CoreError } from "./core.js";
export type { ProxyOptions, TlsOptions, TlsVersion } from "./transport.js";
export { saveState } from "./executor.js";
// To build the messages above: create(AgentDescriptionSchema, {...}).
export { create, fromBinary, toBinary } from "@bufbuild/protobuf";

/** The client's settings. The client never keeps Node.js running by itself (its timers and its
 * WebSocket connection are unref'd): a program whose only work is the client keeps itself going,
 * and stops the client when it ends. */
export interface Config {
  /** The server URL: http(s)://... for plain HTTP, ws(s)://... for WebSocket. */
  endpoint: string;
  /** The instance UID to start with (16 bytes). The server may assign another. Omitted: the core
   * makes one (a UUIDv7 of the time and random bytes), and with a stateFile keeps it across
   * restarts (the one in use is told at start, as instanceUidChanged). */
  instanceUid?: Uint8Array;
  /** Capabilities the agent supports (AgentCapabilities bits). ReportsStatus and
   * ReportsHeartbeat are always added, and the Reports* bits for status you set. */
  capabilities?: bigint;
  /** Heartbeat (WebSocket) or polling (HTTP) interval until the server offers one; default 30 s. */
  heartbeatIntervalMs?: number;
  /** Sent with every request or upgrade (Authorization, say). */
  headers?: Record<string, string>;
  /** How long a plain HTTP request may take; default 30 s. */
  requestTimeoutMs?: number;
  /** How long a WebSocket connection may take to open; default 30 s. */
  connectTimeoutMs?: number;
  /** How long stop may take (the goodbye and the closing handshake), and any closing handshake;
   * default 5 s. Whatever is still running then is given up on. */
  stopTimeoutMs?: number;
  /** The largest message sent, encoded; default 64 MiB, and at least 1 KiB. A status that doesn't
   * fit is left out (statusTooLarge).
   * covers: OPAMP-SIZE-02, OPAMP-SIZE-08 */
  maxSendBytes?: number;
  /** The largest response or message accepted; default 64 MiB.
   * covers: OPAMP-SIZE-02, OPAMP-SIZE-08 */
  maxReceiveBytes?: number;
  /** Where the client keeps state across restarts; none if unset. It holds secrets (credentials
   * the server issued, private keys of telemetry settings' certificates), so it is written
   * readable by the agent's user only (mode 0600). On Windows it takes its directory's
   * permissions: keep it on a local path only the agent's user can read. */
  stateFile?: string;
  /** Take offers of new connection settings from the server (docs/connection-settings.md).
   * Offers are refused without a stateFile: a restart would lose them. */
  acceptConnectionSettings?: boolean;
  /** How long offered settings have to prove themselves; default 60 s. */
  verifyTimeoutMs?: number;
  /** Take packages from the server (docs/packages.md): the client downloads them to
   * `${stateFile}.downloads` and the application installs them (installPackage, removePackage).
   * Only with a stateFile: a restart would forget what the agent has. */
  acceptPackages?: boolean;
  /** Ask the server to assign the instance UID (the RequestInstanceUid flag,
   * docs/instance-uid.md). The UID the client starts with (made up, or instanceUid) is the
   * temporary value until the server assigns one. With a stateFile the assigned UID is kept, and
   * a restart doesn't ask again; without one every start asks again, and the server assigns a new
   * UID each time: the agent is a new agent after every restart.
   * covers: OPAMP-UID-04 */
  requestInstanceUid?: boolean;
  /** The largest package file downloaded; default 1 GiB. */
  maxPackageBytes?: number;
  /** How long a download may take; default 10 minutes. */
  downloadTimeoutMs?: number;
  /** TLS for https:// and wss:// endpoints and package downloads, in both modes: the CAs to
   * trust, a client certificate to present, the versions to use. Default: Node's CAs, no client
   * certificate, TLS 1.2 or later.
   * covers: RULE-TLS-01, RULE-TLS-02 */
  tls?: TlsOptions;
  /** A proxy for every connection to the server and every download (HTTP, HTTPS or SOCKS5).
   * `false`: none. Omitted (or without `url`): the proxies in the environment (HTTP_PROXY,
   * HTTPS_PROXY, ALL_PROXY, NO_PROXY), by the rule every SDK's Rust mode shares, never under CGI
   * (REQUEST_METHOD set: httpoxy). In Node mode: Node's default instead, the environment's
   * proxies only when NODE_USE_ENV_PROXY=1 or --use-env-proxy says to use them, read as Node
   * reads them. A SOCKS5 proxy resolves the server's name (`socks5://` and `socks5h://` alike,
   * unlike curl) unless `resolveLocally`.
   * covers: RULE-PROXY-01 */
  proxy?: ProxyOptions | false;
  /** Who speaks HTTP and WebSocket. "rust" (the default): the shared core itself, over Node's
   * TCP and TLS sockets; Node only opens connections, starts TLS with the options above and
   * picks the proxy. "node": undici's fetch and the ws package, for an application that needs
   * Node's own stack (its convention for the environment's proxies, say). */
  mode?: "rust" | "node";
  /** Rust mode: package files go from the socket to the file without passing through the core
   * (where the response says how long the body is). Default true. It is Rust mode's: `false`
   * in Node mode throws. */
  rustSplice?: boolean;
}

/** Whether `config` runs in Rust mode: yes, unless it asks for Node mode by name. What the mode
 * it runs in doesn't honour throws, saying what to set instead: nothing is ignored. */
function rustMode(config: Config): boolean {
  const mode: unknown = config.mode;
  if (mode !== undefined && mode !== "rust" && mode !== "node") {
    throw new TypeError(`mode is "rust" or "node", not ${JSON.stringify(mode)}`);
  }
  if (mode === "node" && config.rustSplice === false) {
    throw new TypeError(
      'rustSplice in Node mode: it is Rust mode\'s (undici writes package files itself); leave it out, or leave mode out (or "rust")',
    );
  }
  return mode !== "node";
}

/** The agent's status as the client starts. The first message reports it in full. */
export interface Status {
  description?: AgentDescription;
  health?: ComponentHealth;
  effectiveConfig?: EffectiveConfig;
  remoteConfigStatus?: RemoteConfigStatus;
  /** The components the agent has (leave `hash` empty: the core computes it). The server gets the
   * hash, and the components in full when it asks. */
  availableComponents?: AvailableComponents;
  /** The custom capabilities the agent supports: custom messages for them reach the application,
   * and it may send them. */
  customCapabilities?: CustomCapabilities;
}

/** A status field. */
export type StatusField =
  | "description"
  | "health"
  | "effectiveConfig"
  | "remoteConfigStatus"
  | "connectionSettingsStatus"
  | "availableComponents"
  | "customCapabilities"
  | "packageStatuses";

/** What the application is told. */
export type Notification =
  | { kind: "connected" }
  | { kind: "connectFailed"; status?: number; detail: string }
  | { kind: "errorResponse"; type: number; message: string }
  | { kind: "remoteConfig"; config: AgentRemoteConfig }
  /** Own telemetry and other connection settings, all of them; answer with setConnectionSettingsStatus. */
  | { kind: "connectionSettings"; settings: ConnectionSettingsOffers }
  /** A custom message from the server, for a custom capability the agent declared. */
  | { kind: "customMessage"; message: CustomMessage }
  /** The custom capabilities the server supports changed. */
  | { kind: "serverCustomCapabilities"; capabilities: string[] }
  /** A custom message the application sent won't be, and why. */
  | { kind: "customMessageNotSent"; message: CustomMessage; reason: string }
  /** Install a package from the file at `path` (checking its signature is the application's job);
   * answer with packageInstalled. The client deletes the file after the answer. */
  | { kind: "installPackage"; name: string; package: PackageAvailable; path: string }
  /** Remove a package no longer offered; answer with packageRemoved. */
  | { kind: "removePackage"; name: string }
  | { kind: "command"; command: ServerToAgentCommand }
  | { kind: "instanceUidChanged"; instanceUid: Uint8Array }
  | { kind: "heartbeatInterval"; ms: number }
  | { kind: "statusTooLarge"; field: StatusField; bytes: number }
  | { kind: "badMessage"; detail: string }
  /** The save under way has run for its stall time without ending (`stalled`), and the client
   * waits on it still, saving nothing else meanwhile; or it has ended at last (not `stalled`).
   * `waitedMs`: how long it had run. A disk that hangs is the application's to see to. */
  | { kind: "saveStalled"; stalled: boolean; waitedMs: number }
  /** The client has stopped: after stop(), or on its own because of `error` (which stop() throws too). */
  | { kind: "stopped"; error?: unknown };

const FIELDS: Record<CoreStatusField, StatusField> = {
  [CoreStatusField.DESCRIPTION]: "description",
  [CoreStatusField.HEALTH]: "health",
  [CoreStatusField.EFFECTIVE_CONFIG]: "effectiveConfig",
  [CoreStatusField.REMOTE_CONFIG_STATUS]: "remoteConfigStatus",
  [CoreStatusField.CONNECTION_SETTINGS_STATUS]: "connectionSettingsStatus",
  [CoreStatusField.AVAILABLE_COMPONENTS]: "availableComponents",
  [CoreStatusField.CUSTOM_CAPABILITIES]: "customCapabilities",
  [CoreStatusField.PACKAGE_STATUSES]: "packageStatuses",
};

function fromCore(n: CoreNotification, stateFile: string | undefined): Notification | undefined {
  const k = n.kind;
  switch (k.case) {
    case "connected":
      return { kind: "connected" };
    case "connectFailed":
      return { kind: "connectFailed", status: k.value.status, detail: k.value.detail };
    case "errorResponse":
      return { kind: "errorResponse", type: k.value.kind, message: k.value.message };
    case "remoteConfig":
      return { kind: "remoteConfig", config: k.value };
    case "connectionSettings":
      return { kind: "connectionSettings", settings: k.value };
    case "customMessage":
      return { kind: "customMessage", message: k.value };
    case "serverCustomCapabilities":
      return { kind: "serverCustomCapabilities", capabilities: k.value.capabilities };
    case "customMessageNotSent":
      return {
        kind: "customMessageNotSent",
        message: k.value.message ?? create(CustomMessageSchema),
        reason: k.value.reason,
      };
    case "installPackage":
      return {
        kind: "installPackage",
        name: k.value.name,
        package: k.value.package ?? create(PackageAvailableSchema),
        path: downloadPath(stateFile ?? "", k.value.download),
      };
    case "removePackage":
      return { kind: "removePackage", name: k.value.name };
    case "command":
      return { kind: "command", command: k.value };
    case "instanceUidChanged":
      return { kind: "instanceUidChanged", instanceUid: k.value };
    case "heartbeatIntervalMs":
      return { kind: "heartbeatInterval", ms: Number(k.value) };
    case "statusTooLarge":
      return { kind: "statusTooLarge", field: FIELDS[k.value.field], bytes: Number(k.value.bytes) };
    case "badMessage":
      return { kind: "badMessage", detail: k.value.detail };
    case "saveStalled":
      return { kind: "saveStalled", stalled: k.value.stalled, waitedMs: Number(k.value.waitedMs) };
    case "stopped":
      return { kind: "stopped" };
    default:
      return undefined;
  }
}

const EMPTY = create(EmptySchema);

/** The longest delay a Node timer takes (setTimeout takes a longer one as 1 ms). */
const MAX_DELAY_MS = 2 ** 31 - 1;
/** Once the core has stopped, at least this for what it gave up on to unwind (a download's file
 * deleted once the write in flight is over), however much of the stop timeout the core took: stop
 * returns within the stop timeout and this. 250 ms was too little on a loaded machine: a core that
 * took all of its stop timeout and then stopped with a file being written left the file to be
 * deleted after stop had returned (measured: up to 1.1 s on one CPU shared with 28 processes). */
const STOP_GRACE_MS = 1000;

/** Effects that open something new: a connection, a download, a stream. */
const STARTS = new Set(["wsOpen", "download", "streamOpen"]);

/** Milliseconds or bytes for the core. A negative one reaches it as its two's complement, which the
 * core refuses at start, naming the field and the number meant. */
const ms = (v: number | undefined): bigint => BigInt.asUintN(64, BigInt(Math.trunc(v ?? 0)));

/** A running OpAMP client. */
export class Client {
  /** Set once the client has started (at once, unless work of a client before it on the state
   * file was still under way: then once that is over). */
  #core: Core | undefined;
  #x: Executor | undefined;
  /** Rust mode's sockets and files; undefined otherwise. */
  #streams: Streams | undefined;
  readonly #notify: (n: Notification) => void;
  readonly #stateFile: string | undefined;
  /** Notifications from while the client was starting: delivered once `start` has returned. */
  #held: Notification[] | undefined = [];
  /** The monotonic clock (performance.now, unless a test's), and its time at start. */
  readonly #clock: () => number;
  readonly #start: number;
  readonly #queue: Input[] = [];
  #draining = false;
  #timer: NodeJS.Timeout | undefined;
  #done: Promise<void>;
  #finish!: (err?: unknown) => void;
  #finished = false;
  /** When stop() was first called (the client's clock). */
  #stopAt: number | undefined;
  /** What keeps Node.js running from stop() until the client has stopped. */
  #stopHold: NodeJS.Timeout | undefined;
  /** How long all of stop may take: the core's stop_timeout_ms, as given or by default (its
   * start reports it), from stop(). Once the core has stopped, what is left of it bounds the wait
   * for what it started to unwind (RULE-SHELL-07); a file write that hangs is let go rather than
   * holding stop up. 0 until the core has started. */
  #stopMs = 0;
  #saidStopped = false;

  /** Start a client. `notify` is called for every notification, never before `start` returns (a
   * saved remote config is handed over at start): keep it quick. One that throws stops the client
   * with the error (stopped says it, and stop() rejects with it). An async one isn't waited for,
   * but its rejection is a throw too: the client stops with it, rather than leave Node.js an
   * unhandled rejection (which ends the process). */
  static start(config: Config, status: Status, notify?: (n: Notification) => void): Client {
    return new Client(config, status, notify ?? (() => {}));
  }

  private constructor(config: Config, status: Status, notify: (n: Notification) => void) {
    const platform = (config as { [TEST_PLATFORM]?: TestPlatform })[TEST_PLATFORM];
    this.#clock = platform?.now ?? (() => performance.now());
    this.#start = this.#clock();
    // Bad TLS or proxy settings throw here, before anything is sent.
    const transport = new Transport({ tls: config.tls, proxy: config.proxy }, process, rustMode(config));
    this.#notify = notify;
    this.#stateFile = config.stateFile;
    this.#done = new Promise((resolve, reject) => {
      this.#finish = (err) => (err === undefined ? resolve() : reject(err));
    });
    // Nobody awaits the promise until stop(); don't report a failure before then as unhandled.
    this.#done.catch(() => {});
    if (config.stateFile !== undefined && busyOn(config.stateFile)) {
      // A client before this one on the state file left work under way there (a save still
      // writing, a cancelled download deleting its file): this one starts once it is over, from
      // what it left. What the application asks meanwhile waits for it.
      const file = config.stateFile;
      void settled(file).then(() => {
        if (this.#finished) return;
        try {
          this.#boot(config, transport);
        } catch (err) {
          return void this.#end(err);
        }
        this.#drain();
      });
    } else {
      this.#boot(config, transport);
    }
    if (status.description) this.#feed(input({ case: "setDescription", value: status.description }));
    if (status.health) this.#feed(input({ case: "setHealth", value: status.health }));
    if (status.effectiveConfig) this.#feed(input({ case: "setEffectiveConfig", value: status.effectiveConfig }));
    if (status.remoteConfigStatus) {
      this.#feed(input({ case: "setRemoteConfigStatus", value: status.remoteConfigStatus }));
    }
    if (status.availableComponents) {
      this.#feed(input({ case: "setAvailableComponents", value: status.availableComponents }));
    }
    if (status.customCapabilities) {
      this.#feed(input({ case: "setCustomCapabilities", value: status.customCapabilities }));
    }
    this.#feed(input({ case: "start", value: EMPTY }));
    // Deliver what came while starting once the caller has the client.
    queueMicrotask(() => {
      const held = this.#held ?? [];
      this.#held = undefined;
      try {
        for (const n of held) this.#call(n);
      } catch (err) {
        // As from any other callback: the client stops with the error (spec/shell-contract.md).
        if (!this.#finished) this.#end(thrown(err));
      }
    });
  }

  /** Read the state file, start the core, and clear the downloads of before. */
  #boot(config: Config, transport: Transport): void {
    const rust = rustMode(config);
    const tests = config as {
      [TEST_CORE_MODULE]?: Uint8Array | WebAssembly.Module;
      [TEST_CORE_TAP]?: string;
      [TEST_FILE_OPS]?: FileOps;
      [TEST_PLATFORM]?: TestPlatform;
    };
    const core = new Core(
      // The core takes the transport from the endpoint, makes the instance UID if none is given
      // (from the entropy and the wall clock), keeps every effect's time (this client carries out
      // its cancels) and decides what a download's response means. It makes the backoff's jitter
      // seed from the entropy too (seed left 0: two clients started in the same tick don't retry
      // in step).
      create(ConfigSchema, {
        instanceUid: config.instanceUid,
        capabilities: config.capabilities ?? 0n,
        heartbeatIntervalMs: ms(config.heartbeatIntervalMs),
        endpoint: config.endpoint,
        headers: Object.entries(config.headers ?? {}).map(([name, value]) => ({ name, value })),
        requestTimeoutMs: ms(config.requestTimeoutMs),
        connectTimeoutMs: ms(config.connectTimeoutMs),
        stopTimeoutMs: ms(config.stopTimeoutMs),
        maxSendBytes: ms(config.maxSendBytes),
        maxReceiveBytes: ms(config.maxReceiveBytes),
        persistence: config.stateFile !== undefined,
        savedState: config.stateFile === undefined ? undefined : readState(config.stateFile),
        acceptConnectionSettings: config.acceptConnectionSettings ?? false,
        verifyTimeoutMs: ms(config.verifyTimeoutMs),
        acceptPackages: config.acceptPackages ?? false,
        requestInstanceUid: config.requestInstanceUid ?? false,
        maxPackageBytes: ms(config.maxPackageBytes),
        downloadTimeoutMs: ms(config.downloadTimeoutMs),
        streams: rust,
        entropy: new Uint8Array(randomBytes(32)),
        unixMs: BigInt(Date.now()),
        spliceDownloads: rust && (config.rustSplice ?? true),
        proxy: rust ? transport.rustProxyChoice() : undefined,
      }),
      tests[TEST_CORE_MODULE],
      tests[TEST_CORE_TAP],
    );
    this.#core = core;
    this.#stopMs = Number(core.limits?.stopTimeoutMs ?? 0n);
    let ops = tests[TEST_FILE_OPS] ?? REAL;
    if (config.stateFile !== undefined) {
      if (core.tapped) {
        // The tap records the client's file operations: the state file is named first.
        core.file({ op: "state", path: basename(resolve(config.stateFile)) });
        ops = journal(ops, config.stateFile, (r) => core.file(r));
      }
      // Files downloaded before a restart: the core downloads again what it still needs.
      ops.clear(downloadsDir(config.stateFile));
    }
    const platform = tests[TEST_PLATFORM];
    this.#x = new Executor((input) => this.#feed(input), config.stateFile, transport, ops, platform?.executor);
    this.#streams = rust
      ? new Streams((input) => this.#feed(input), transport, config.stateFile, ops, platform?.streams)
      : undefined;
  }

  /** Change the agent description. */
  setDescription(value: AgentDescription): void {
    this.#feed(input({ case: "setDescription", value }));
  }

  /** Change the health. */
  setHealth(value: ComponentHealth): void {
    this.#feed(input({ case: "setHealth", value }));
  }

  /** Change the effective config. */
  setEffectiveConfig(value: EffectiveConfig): void {
    this.#feed(input({ case: "setEffectiveConfig", value }));
  }

  /** Change the custom capabilities the agent supports. */
  setCustomCapabilities(value: CustomCapabilities): void {
    this.#feed(input({ case: "setCustomCapabilities", value }));
  }

  /** Send a custom message, for one of the agent's custom capabilities. It goes with the next
   * message the server takes, and again if that message is lost; if it can't be sent at all, a
   * customMessageNotSent notification says why. */
  sendCustomMessage(value: CustomMessage): void {
    this.#feed(input({ case: "sendCustomMessage", value }));
  }

  /** Change the components the agent has (leave `hash` empty: the core computes it). */
  setAvailableComponents(value: AvailableComponents): void {
    this.#feed(input({ case: "setAvailableComponents", value }));
  }

  /** Report on a remote config (the answer to a remoteConfig notification). */
  setRemoteConfigStatus(value: RemoteConfigStatus): void {
    this.#feed(input({ case: "setRemoteConfigStatus", value }));
  }

  /** Report on telemetry settings (the answer to a connectionSettings notification). */
  setConnectionSettingsStatus(value: ConnectionSettingsStatus): void {
    this.#feed(input({ case: "setConnectionSettingsStatus", value }));
  }

  /** Answer an installPackage notification: the package at `hash` is installed, or (`error`) why
   * it isn't. */
  packageInstalled(name: string, hash: Uint8Array, error?: string): void {
    this.#feed(input({ case: "packageInstalled", value: { name, hash, ok: error === undefined, detail: error ?? "" } } as never));
  }

  /** Answer a removePackage notification: the package is removed. */
  packageRemoved(name: string): void {
    this.#feed(input({ case: "packageRemoved", value: { name } } as never));
  }

  /** The instance UID in use: the one given, the one the core made, or one the server assigned
   * since (told as instanceUidChanged). Undefined until the client has started (once the work of a
   * client before it on the state file is over). */
  get instanceUid(): Uint8Array | undefined {
    const uid = this.#core?.instanceUid;
    return uid && uid.length === 16 ? uid.slice() : undefined;
  }

  /** The endpoint the client uses now: where its latest OpAMP request or WebSocket connection
   * went, which after the server moved the agent (an offer of connection settings) is the offered
   * one, or the configured one again once an offer failed. Undefined before any outcome, and in
   * Rust mode, where the core makes the requests itself (the configured one is all there is to
   * name then). The URL is as the core gave it: redact it before logging. */
  get endpoint(): string | undefined {
    return this.#streams === undefined ? this.#x?.used : undefined;
  }

  /** Send agent_disconnect if a message can still be sent, then stop. Stop is the application's
   * work: from here until the client has stopped (within the stop timeout) it does keep Node.js
   * running, so a program that ends by awaiting stop() says its goodbye first. (The client's own
   * timers and connections never do.) */
  stop(): Promise<void> {
    this.#stopAt ??= this.#now();
    if (this.#stopHold === undefined && !this.#finished) {
      const hold = setInterval(() => {}, MAX_DELAY_MS);
      this.#stopHold = hold;
      const release = () => clearInterval(hold);
      this.#done.then(release, release);
    }
    this.#feed(input({ case: "stop", value: EMPTY }));
    return this.#done;
  }

  /** Once the file work under way on the state file has ended: a save still writing when stop()
   * resolved (stop doesn't wait for it; a client started next on the file does) has landed, and
   * what is saved there is what a client started next would start from. At once without a state
   * file. Never rejects. */
  async settled(): Promise<void> {
    if (this.#stateFile !== undefined) await settled(this.#stateFile);
  }

  #now(): number {
    return this.#clock() - this.#start;
  }

  /** Step the core with `next` and every input queued before it, in order, carrying out the
   * effects (once the client has started: until then they wait). */
  #feed(next: Input): void {
    if (this.#finished) return;
    this.#queue.push(next);
    if (this.#draining || this.#core === undefined) return;
    this.#drain();
  }

  #drain(): void {
    const core = this.#core as Core;
    const x = this.#x as Executor;
    this.#draining = true;
    try {
      for (let i = this.#queue.shift(); i !== undefined && !this.#finished; i = this.#queue.shift()) {
        const out = core.step(this.#now(), i);
        // Stopped is told last: nothing reaches the application after stopped (RULE-SHELL-01),
        // whatever order an output gives (the core now says it last itself).
        let stopped: Notification | undefined;
        for (const e of out.effects) {
          // A newer module than this client: fail loudly rather than skip what the core asked for.
          if (e.kind.case === undefined) throw new Error("the core asked for an effect this client doesn't know: the client is older than its core");
          if (e.kind.case === "notify") {
            const n = fromCore(e.kind.value, this.#stateFile);
            if (n?.kind === "stopped") stopped = n;
            else if (n) this.#tell(n);
          } else if (out.stopped && STARTS.has(e.kind.case)) {
            // An output that says stopped starts nothing new (RULE-SHELL-07): what it would start
            // could only be given up on.
          } else if (!this.#streams?.do(e) || e.kind.case === "cancel") {
            // (A cancel names a download, Streams', or a save, the Executor's.)
            x.do(e);
          }
        }
        if (stopped) this.#tell(stopped);
        clearTimeout(this.#timer);
        this.#timer = undefined;
        if (out.wakeAtMs !== undefined) this.#arm(Number(out.wakeAtMs));
        // The core has ended its own goodbye and closing handshakes, within its stop time, and
        // cancelled whatever was left: nothing it started is running.
        if (out.stopped) this.#end();
      }
    } catch (err) {
      this.#end(thrown(err));
    } finally {
      this.#draining = false;
    }
  }

  /** Step the core with a timer at `at` (the client's clock), never before (RULE-SHELL-08): a time
   * further than a Node timer holds (setTimeout takes a longer delay as 1 ms) is waited for in
   * pieces. The client doesn't keep Node.js running (as the other clients' daemon threads): the
   * application's own work does, and its end is the time to stop(). */
  #arm(at: number): void {
    const delay = Math.max(0, at - this.#now());
    this.#timer =
      delay > MAX_DELAY_MS
        ? setTimeout(() => this.#arm(at), MAX_DELAY_MS).unref()
        : setTimeout(() => this.#feed(input({ case: "timer", value: EMPTY })), delay).unref();
  }

  /** Tell the application, or hold it until `start` has returned. */
  #tell(n: Notification): void {
    // Stopped is the last word, said once: a callback that fails on it fails after it.
    if (this.#saidStopped) return;
    this.#saidStopped = n.kind === "stopped";
    if (this.#held) this.#held.push(n);
    else this.#call(n);
  }

  /** Call the application's callback with `n`. What it throws, the caller handles; if it is async,
   * its rejection stops the client as a throw does (once the client has stopped, what is said is
   * said: it goes, as a second failure would). */
  #call(n: Notification): void {
    const r: unknown = this.#notify(n);
    if (r !== null && typeof r === "object" && typeof (r as PromiseLike<void>).then === "function") {
      (r as PromiseLike<void>).then(undefined, (err: unknown) => {
        if (!this.#finished) this.#end(thrown(err));
      });
    }
  }

  #end(err?: unknown): void {
    this.#finished = true;
    clearTimeout(this.#timer);
    if (err !== undefined) {
      // The client stopped on its own: the application hears it now, not only from stop(). If
      // its callback is what failed, it may fail again: that is already the error reported.
      try {
        this.#tell({ kind: "stopped", error: err });
      } catch {}
    }
    this.#release(err);
  }

  /** Let everything go once what is in flight is done. */
  #release(err?: unknown): void {
    clearTimeout(this.#timer);
    const x = this.#x;
    const core = this.#core;
    // All of stop takes one stop timeout from stop() (RULE-SHELL-07).
    const begun = this.#stopAt ?? this.#now();
    // One deadline for all of it: the grace is given once, not to each part.
    const releasedBy = Math.max(begun + this.#stopMs, this.#now() + STOP_GRACE_MS);
    const left = () => Math.max(0, releasedBy - this.#now());
    void (async () => {
      // Whatever is still under way (the client stopped on its own) is given up on; downloads
      // given up on have deleted what they wrote, or the bound has passed.
      await x?.shutdown(left());
      // Rust mode: files still being written are deleted once their write in flight is over.
      await this.#streams?.shutdown(left());
      this.#finish(err);
      // A save still writing ends whole (stop doesn't wait for it, a client started next on the
      // state file does): the tap records it, then closes.
      void (x?.settled() ?? Promise.resolve()).then(() => core?.close());
    })();
  }
}

/** What was thrown, as the error the client stops with: `undefined` (which JavaScript lets anyone
 * throw) would read as no error at all, and the client would stop without a word (RULE-SHELL-02). */
function thrown(err: unknown): unknown {
  return err === undefined ? new Error("a callback threw undefined") : err;
}

function input(kind: Input["kind"]): Input {
  return create(InputSchema, { kind } as never);
}

/** What was saved at `path`: nothing if there is no file yet. */
function readState(path: string): Uint8Array | undefined {
  try {
    return readFileSync(path);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw err;
  }
}
