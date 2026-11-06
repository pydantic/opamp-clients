// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// An OpAMP client for Node.js.
//
// Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
// agent_disconnect, and the requests and responses themselves: headers, framing, Retry-After, size
// limits) is the shared OpAMP client core, run as WebAssembly. By default the core speaks HTTP and
// WebSocket itself too (core mode), over Node's TCP and TLS sockets; in platform mode this package
// carries out the core's requests with undici's fetch and the ws package instead. Either way it
// feeds back what happened, so it behaves exactly like the other languages' clients on the same
// core.

import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { basename, resolve } from "node:path";

import { create } from "@bufbuild/protobuf";

import { Core, TEST_CORE_MODULE, TEST_CORE_TAP } from "./core.js";
import { busyOn, type FileOps, journal, REAL, settled, TEST_FILE_OPS } from "./files.js";
import { Executor, TEST_PLATFORM, type TestPlatform } from "./executor.js";
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
  ComponentHealth,
  ConnectionSettingsOffers,
  ConnectionSettingsStatus,
  EffectiveConfig,
  RemoteConfigStatus,
} from "./gen/opamp/v1/opamp_pb.js";

export * from "./gen/opamp/v1/opamp_pb.js";
export * from "./gen/opamp/v1/anyvalue_pb.js";
export { CoreError, coreUnavailable, identifySetup, startSetup } from "./core.js";
// The core's Setup call: an OpenTelemetry integration's start-up decisions (core.ts).
export {
  type Header as SetupHeader,
  HeaderSchema as SetupHeaderSchema,
  type SetupHints,
  type SetupIdentified,
  type SetupIdentity,
  SetupIdentitySchema,
  type SetupInCode,
  SetupInCodeSchema,
  SetupOs,
  type SetupStart,
  SetupStartSchema,
  type SetupStarted,
  SetupUidFrom,
  type Variable as SetupVariable,
  VariableSchema as SetupVariableSchema,
} from "./gen/opamp/core/v1/core_pb.js";
/** The files in a state directory's slot: the lock a process holds while it runs, and the UID. */
export const SETUP_LOCK_FILE = "lock";
export const SETUP_UID_FILE = "instance-uid";
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
  /** Capabilities the agent supports (AgentCapabilities bits), fixed for the run. ReportsStatus is
   * always added, and the core adds the bits of what it does itself (packages, connection
   * settings). Declare ReportsHeartbeat for heartbeats, and the Reports* bit of each status you
   * set: a status whose bit isn't declared isn't sent. */
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
  /** Ask the server to assign the instance UID (the RequestInstanceUid flag,
   * docs/instance-uid.md). The UID the client starts with (made up, or instanceUid) is the
   * temporary value until the server assigns one. With a stateFile the assigned UID is kept, and
   * a restart doesn't ask again; without one every start asks again, and the server assigns a new
   * UID each time: the agent is a new agent after every restart.
   * covers: OPAMP-UID-04 */
  requestInstanceUid?: boolean;
  /** TLS for https:// and wss:// endpoints, in both modes: the CAs to
   * trust, a client certificate to present, the versions to use. Default: Node's CAs, no client
   * certificate, TLS 1.2 or later.
   * covers: RULE-TLS-01, RULE-TLS-02 */
  tls?: TlsOptions;
  /** A proxy for every connection to the server (HTTP, HTTPS or SOCKS5).
   * `false`: none. Omitted (or without `url`): the proxies in the environment (HTTP_PROXY,
   * HTTPS_PROXY, ALL_PROXY, NO_PROXY), by the rule every SDK's core mode shares, never under CGI
   * (REQUEST_METHOD set: httpoxy). In platform mode: Node's default instead, the environment's
   * proxies only when NODE_USE_ENV_PROXY=1 or --use-env-proxy says to use them, read as Node
   * reads them. A SOCKS5 proxy resolves the server's name (`socks5://` and `socks5h://` alike,
   * unlike curl) unless `resolveLocally`.
   * covers: RULE-PROXY-01 */
  proxy?: ProxyOptions | false;
  /** Who speaks HTTP and WebSocket. "core" (the default): the shared core itself, over Node's
   * TCP and TLS sockets; Node only opens connections, starts TLS with the options above and
   * picks the proxy. "node": undici's fetch and the ws package, for an application that needs
   * Node's own stack (its convention for the environment's proxies, say). */
  mode?: "core" | "platform";
}

/** Whether `config` runs in core mode: yes, unless it asks for platform mode by name. What the mode
 * it runs in doesn't honour throws, saying what to set instead: nothing is ignored. */
function runsInCoreMode(config: Config): boolean {
  const mode: unknown = config.mode;
  if (mode !== undefined && mode !== "core" && mode !== "platform") {
    throw new TypeError(`mode is "core" or "platform", not ${JSON.stringify(mode)}`);
  }
  return mode !== "platform";
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
  | { kind: "errorResponse"; errorType: number; message: string }
  | { kind: "remoteConfig"; remoteConfig: AgentRemoteConfig }
  /** Own telemetry and other connection settings, all of them; answer with setConnectionSettingsStatus. */
  | { kind: "connectionSettings"; connectionSettings: ConnectionSettingsOffers }
  | { kind: "instanceUidChanged"; instanceUid: Uint8Array }
  | { kind: "heartbeatInterval"; heartbeatIntervalMs: number }
  | { kind: "statusTooLarge"; field: StatusField; bytes: number }
  | { kind: "badMessage"; detail: string }
  /** The save under way has run for its stall time without ending (`stalled`), and the client
   * waits on it still, saving nothing else meanwhile; or it has ended at last (not `stalled`).
   * `waitedMs`: how long it had run. A disk that hangs is the application's to see to. */
  | { kind: "saveStalled"; stalled: boolean; waitedMs: number }
  /** The saved state is a newer core's, in a format (`version`) this one can't read: the client
   * starts from its configuration, as on first use, and saves nothing (no offered connection
   * settings), leaving that state as it is. Worth a warning: an older release was
   * started on a newer one's state file. */
  | { kind: "stateFromNewerVersion"; version: number }
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

function fromCore(n: CoreNotification): Notification | undefined {
  const k = n.kind;
  switch (k.case) {
    case "connected":
      return { kind: "connected" };
    case "connectFailed":
      return { kind: "connectFailed", status: k.value.status, detail: k.value.detail };
    case "errorResponse":
      return { kind: "errorResponse", errorType: k.value.kind, message: k.value.message };
    case "remoteConfig":
      return { kind: "remoteConfig", remoteConfig: k.value };
    case "connectionSettings":
      return { kind: "connectionSettings", connectionSettings: k.value };
    case "instanceUidChanged":
      return { kind: "instanceUidChanged", instanceUid: k.value };
    case "heartbeatIntervalMs":
      return { kind: "heartbeatInterval", heartbeatIntervalMs: Number(k.value) };
    case "statusTooLarge":
      return { kind: "statusTooLarge", field: FIELDS[k.value.field], bytes: Number(k.value.bytes) };
    case "badMessage":
      return { kind: "badMessage", detail: k.value.detail };
    case "saveStalled":
      return { kind: "saveStalled", stalled: k.value.stalled, waitedMs: Number(k.value.waitedMs) };
    case "stateFromNewerVersion":
      return { kind: "stateFromNewerVersion", version: k.value.version };
    case "stopped":
      return { kind: "stopped" };
    default:
      return undefined;
  }
}

const EMPTY = create(EmptySchema);

/** The longest delay a Node timer takes (setTimeout takes a longer one as 1 ms). */
const MAX_DELAY_MS = 2 ** 31 - 1;
/** Effects that open something new: a connection, a stream. */
const STARTS = new Set(["wsOpen", "streamOpen"]);

/** Milliseconds or bytes for the core. A negative one reaches it as its two's complement, which the
 * core refuses at start, naming the field and the number meant. */
const ms = (v: number | undefined): bigint => BigInt.asUintN(64, BigInt(Math.trunc(v ?? 0)));

/** A running OpAMP client. */
export class Client {
  /** Set once the client has started (at once, unless work of a client before it on the state
   * file was still under way: then once that is over). */
  #core: Core | undefined;
  #x: Executor | undefined;
  /** Core mode's sockets; undefined otherwise. */
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
  /** What keeps Node.js running from stop() until the client has stopped. */
  #stopHold: NodeJS.Timeout | undefined;
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
    const transport = new Transport({ tls: config.tls, proxy: config.proxy }, process, runsInCoreMode(config));
    this.#notify = notify;
    this.#stateFile = config.stateFile;
    this.#done = new Promise((resolve, reject) => {
      this.#finish = (err) => (err === undefined ? resolve() : reject(err));
    });
    // Nobody awaits the promise until stop(); don't report a failure before then as unhandled.
    this.#done.catch(() => {});
    if (config.stateFile !== undefined && busyOn(config.stateFile)) {
      // A client before this one on the state file left work under way there (a save still
      // writing): this one starts once it is over, from
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

  /** Read the state file and start the core. */
  #boot(config: Config, transport: Transport): void {
    const coreMode = runsInCoreMode(config);
    const tests = config as {
      [TEST_CORE_MODULE]?: Uint8Array | WebAssembly.Module;
      [TEST_CORE_TAP]?: string;
      [TEST_FILE_OPS]?: FileOps;
      [TEST_PLATFORM]?: TestPlatform;
    };
    const core = new Core(
      // The core takes the transport from the endpoint, makes the instance UID if none is given
      // (from the entropy and the wall clock), keeps every effect's time (this client carries out
      // its cancels). It makes the backoff's jitter
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
        requestInstanceUid: config.requestInstanceUid ?? false,
        streams: coreMode,
        entropy: new Uint8Array(randomBytes(32)),
        unixMs: BigInt(Date.now()),
        proxy: coreMode ? transport.coreProxyChoice() : undefined,
      }),
      tests[TEST_CORE_MODULE],
      tests[TEST_CORE_TAP],
    );
    this.#core = core;
    let ops = tests[TEST_FILE_OPS] ?? REAL;
    if (config.stateFile !== undefined) {
      if (core.tapped) {
        // The tap records the client's file operations: the state file is named first.
        core.file({ op: "state", path: basename(resolve(config.stateFile)) });
        ops = journal(ops, config.stateFile, (r) => core.file(r));
      }
    }
    const platform = tests[TEST_PLATFORM];
    this.#x = new Executor((input) => this.#feed(input), config.stateFile, transport, ops, platform?.executor);
    this.#streams = coreMode
      ? new Streams((input) => this.#feed(input), transport, platform?.streams)
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
   * core mode, where the core makes the requests itself (the configured one is all there is to
   * name then). The URL is as the core gave it: redact it before logging. */
  get endpoint(): string | undefined {
    return this.#streams === undefined ? this.#x?.used : undefined;
  }

  /** Send agent_disconnect if a message can still be sent, then stop. Stop is the application's
   * work: from here until the client has stopped (within the stop timeout) it does keep Node.js
   * running, so a program that ends by awaiting stop() says its goodbye first. (The client's own
   * timers and connections never do.) */
  stop(): Promise<void> {
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
            const n = fromCore(e.kind.value);
            if (n?.kind === "stopped") stopped = n;
            else if (n) this.#tell(n);
          } else if (out.stopped && STARTS.has(e.kind.case)) {
            // An output that says stopped starts nothing new (RULE-SHELL-07): what it would start
            // could only be given up on.
          } else if (!this.#streams?.do(e) || e.kind.case === "cancel") {
            // (A cancel names a request, a connection or a save: the Executor's.)
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
    void (async () => {
      // Whatever is still under way (the client stopped on its own) is given up on.
      await x?.shutdown();
      this.#streams?.shutdown();
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
