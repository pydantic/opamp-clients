// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Runs our Node.js client (node/opamp: the shared core in Node's WebAssembly engine) under the
// conformance driver contract (conformance/DRIVERS.md).

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

import {
  AgentCapabilities,
  AgentDescriptionSchema,
  AvailableComponentsSchema,
  Client,
  CustomCapabilitiesSchema,
  CustomMessageSchema,
  ComponentHealthSchema,
  ConnectionSettingsStatusSchema,
  ConnectionSettingsStatuses,
  EffectiveConfigSchema,
  RemoteConfigStatusSchema,
  RemoteConfigStatuses,
  create,
} from "../../../node/opamp/dist/src/index.js";

/** The components DRIVERS.md names: an OTLP receiver (with its version) and a debug exporter. */
function components() {
  const version = { key: "version", value: { value: { case: "stringValue", value: "1.0.0" } } };
  const group = (id, c) => ({ subComponentMap: { [id]: c } });
  return create(AvailableComponentsSchema, {
    components: { receivers: group("otlp", { metadata: [version] }), exporters: group("debug", {}) },
  });
}

/** The custom capability DRIVERS.md names: ping is answered with pong. */
const ECHO = "io.opentelemetry.echo";

function emit(event, fields = {}) {
  process.stdout.write(`${JSON.stringify({ ...fields, event, wall: Date.now() / 1000 })}\n`);
}

const envFloat = (key, def) => {
  const v = Number.parseFloat(process.env[key] ?? "");
  return Number.isFinite(v) ? v : def;
};

const hex = (b) => Buffer.from(b).toString("hex");

/** The 32 hex characters given, all zeros (`zero`), or none (`random`, `none`): the core makes
 * one, a UUIDv7. */
function instanceUid() {
  const v = process.env.DRIVER_UID ?? "random";
  if (v === "zero") return new Uint8Array(16);
  if (v !== "random" && v !== "none" && v !== "") return new Uint8Array(Buffer.from(v, "hex"));
  return undefined;
}

// The status to start from, a restart after a crash going on from where the application was
// (DRIVER_FIRST_CHANGE): the changes count on from it.
const firstChange = Number.parseInt(process.env.DRIVER_FIRST_CHANGE ?? "", 10) || 0;

function description(change) {
  const str = (key, value) => ({ key, value: { value: { case: "stringValue", value } } });
  const nonIdentifying = [str("driver.change", String(change))];
  // DRIVER_DESCRIPTION_PAD: a description that long (with DRIVER_MAX_SEND_BYTES, too large to send).
  if (process.env.DRIVER_DESCRIPTION_PAD) {
    nonIdentifying.push(str("driver.pad", "x".repeat(Number(process.env.DRIVER_DESCRIPTION_PAD))));
  }
  return create(AgentDescriptionSchema, {
    identifyingAttributes: [str("service.name", "conformance-node")],
    nonIdentifyingAttributes: nonIdentifying,
  });
}

/** The PEM file an environment variable names, if it names one. */
const pem = (key) => (process.env[key] ? readFileSync(process.env[key], "utf8") : undefined);

/** The CA to trust instead of Node's, and the client certificate to present (DRIVERS.md). */
function tls() {
  const [ca, cert, key] = [pem("DRIVER_CA_FILE"), pem("DRIVER_CLIENT_CERT"), pem("DRIVER_CLIENT_KEY")];
  return ca || cert ? { ca, cert, key } : undefined;
}

const endpoint = process.env.OPAMP_ENDPOINT ?? "";
const uid = instanceUid();
let client;
// DRIVER_THROW_ON_CONFIG: the callback itself throws on the first remote config (RULE-SHELL-02).
const throwOnConfig = process.env.DRIVER_THROW_ON_CONFIG === "1";
let thrown = false;
// The client stopped on its own: the driver no longer stops it, and exits 1.
let failed = false;
const onNote = (n) => {
  switch (n.kind) {
    case "stopped":
      if (n.error !== undefined) {
        // The client stopped on its own. Stay DRIVER_LINGER_S, still reporting what the client says
        // (nothing, if it keeps RULE-SHELL-02: a second stopped is a second fatal), then exit 1.
        emit("fatal", { error: String(n.error) });
        if (!failed) {
          failed = true;
          setTimeout(() => process.exit(1), envFloat("DRIVER_LINGER_S", 0) * 1000);
        }
      }
      break;
    case "connected":
      emit("connect");
      break;
    case "connectFailed":
      emit("connect_failed", { error: n.detail, status: n.status ?? null });
      break;
    case "errorResponse":
      emit("error_response", { type: n.errorType, message: n.message });
      break;
    case "remoteConfig":
      if (throwOnConfig && !thrown) {
        thrown = true;
        emit("throwing");
        throw new Error("callback-throws: the driver's callback threw on the first remote config");
      }
      emit("message", { remote_config_hash: hex(n.remoteConfig.configHash) });
      // Answered after this callback returns: the client is still handling the message.
      setImmediate(() =>
        client.setRemoteConfigStatus(
          create(RemoteConfigStatusSchema, {
            lastRemoteConfigHash: n.remoteConfig.configHash,
            status: RemoteConfigStatuses.RemoteConfigStatuses_APPLIED,
          }),
        ),
      );
      break;
    case "customMessage": {
      const m = n.customMessage;
      emit("custom_message", { capability: m.capability, type: m.type, data: hex(m.data) });
      if (m.type === "ping") {
        setImmediate(() =>
          client.sendCustomMessage(create(CustomMessageSchema, { capability: ECHO, type: "pong", data: m.data })),
        );
      }
      break;
    }
    case "customMessageNotSent":
      emit("custom_message_not_sent", { reason: n.detail });
      break;
    case "installPackage": {
      let sha = null;
      let error;
      try {
        sha = createHash("sha256").update(readFileSync(n.path)).digest("hex");
      } catch (e) {
        error = String(e);
      }
      emit("install_package", { name: n.packageName, version: n.package.version, sha256: sha, error: error ?? null });
      setImmediate(() => client.packageInstalled(n.packageName, n.package.hash, error));
      break;
    }
    case "removePackage":
      emit("remove_package", { name: n.packageName });
      setImmediate(() => client.packageRemoved(n.packageName));
      break;
    case "serverCustomCapabilities":
      emit("server_custom_capabilities", { capabilities: n.customCapabilities });
      break;
    case "connectionSettings": {
      const s = n.connectionSettings;
      emit("telemetry_settings", { hash: hex(s.hash), metrics_endpoint: s.ownMetrics?.destinationEndpoint ?? null });
      setImmediate(() =>
        client.setConnectionSettingsStatus(
          create(ConnectionSettingsStatusSchema, {
            lastConnectionSettingsHash: s.hash,
            status: ConnectionSettingsStatuses.ConnectionSettingsStatuses_APPLIED,
          }),
        ),
      );
      break;
    }
    case "instanceUidChanged":
      emit("message", { new_instance_uid: hex(n.instanceUid) });
      break;
    case "heartbeatInterval":
      emit("connection_settings", { heartbeat_interval_seconds: Math.floor(n.heartbeatIntervalMs / 1000) });
      break;
    default:
      break;
  }
};

try {
  client = Client.start(
    {
      endpoint,
      // random (the default) or none: the client makes one up, as every driver lets it.
      instanceUid: uid,
      capabilities:
        BigInt(AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig) |
        (process.env.DRIVER_OWN_TELEMETRY === "1"
          ? BigInt(
              AgentCapabilities.AgentCapabilities_ReportsOwnMetrics |
                AgentCapabilities.AgentCapabilities_ReportsOwnTraces |
                AgentCapabilities.AgentCapabilities_ReportsOwnLogs,
            )
          : 0n),
      heartbeatIntervalMs: envFloat("DRIVER_POLL_S", 2) * 1000,
      maxSendBytes: process.env.DRIVER_MAX_SEND_BYTES ? Number(process.env.DRIVER_MAX_SEND_BYTES) : undefined,
      headers: process.env.DRIVER_TOKEN ? { Authorization: `Bearer ${process.env.DRIVER_TOKEN}` } : {},
      stateFile: process.env.DRIVER_STATE_FILE || undefined,
      acceptConnectionSettings: process.env.DRIVER_ACCEPT_CONNECTION_SETTINGS === "1",
      acceptPackages: process.env.DRIVER_PACKAGES === "1",
      requestInstanceUid: process.env.DRIVER_REQUEST_UID === "1",
      tls: tls(),
      proxy: process.env.DRIVER_PROXY ? { url: process.env.DRIVER_PROXY } : undefined,
      // The mode is always asked for by name, never left to the client's default: node-core is core
      // mode, where the core speaks HTTP and WebSocket over Node's sockets, node-core-platform
      // (OPAMP_NODE_MODE=platform) platform mode (fetch and ws).
      mode: process.env.OPAMP_NODE_MODE === "platform" ? "platform" : "core",
    },
    {
      description: description(firstChange),
      health: create(ComponentHealthSchema, { healthy: true, startTimeUnixNano: BigInt(Date.now()) * 1_000_000n }),
      effectiveConfig: create(EffectiveConfigSchema, { configMap: {} }),
      availableComponents: process.env.DRIVER_COMPONENTS === "1" ? components() : undefined,
      customCapabilities:
        process.env.DRIVER_CUSTOM === "1" ? create(CustomCapabilitiesSchema, { capabilities: [ECHO] }) : undefined,
    },
    onNote,
  );
} catch (e) {
  emit("fatal", { error: String(e) });
  process.exit(1);
}
emit("started", { uid: hex(client.instanceUid ?? uid ?? new Uint8Array()), client: `opamp-node (ours${process.env.OPAMP_NODE_MODE === "platform" ? ", platform mode" : ""})` });

const changes = (process.env.DRIVER_CHANGES_AT_S ?? "")
  .split(",")
  .map((s) => Number.parseFloat(s.trim()))
  .filter(Number.isFinite);
changes.forEach((at, i) =>
  setTimeout(() => {
    emit("status_change", { change: firstChange + i + 1 });
    client.setDescription(description(firstChange + i + 1));
  }, at * 1000),
);

let stopping = false;
async function stop() {
  if (stopping || failed) return;
  stopping = true;
  emit("stopping");
  let error = null;
  try {
    // Bounded by the core (stopTimeoutMs, 5 s by default).
    await client.stop();
  } catch (e) {
    error = String(e);
  }
  emit("stopped", { error });
  // DRIVER_LINGER_S: stay alive a while, so what the client left running shows in the server's log.
  await new Promise((resolve) => setTimeout(resolve, envFloat("DRIVER_LINGER_S", 0) * 1000));
  process.exit(0);
}
setTimeout(stop, envFloat("DRIVER_RUN_S", 30) * 1000);
process.on("SIGTERM", stop);
process.on("SIGINT", stop);
