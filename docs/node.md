# Node.js

`@pydantic/opamp-client` is this repository's OpAMP client for Node.js, in
[`node/opamp/`](../node/opamp/). It runs the shared core in Node's own WebAssembly engine:
nothing native to install ([how-it-works.md](how-it-works.md)). It speaks plain HTTP or
WebSocket, with TLS and through HTTP or SOCKS5 proxies, and is written in TypeScript, with
protobuf-es types for OpAMP's messages. It needs Node.js 22 or later. Its
[README](../node/opamp/README.md) has the full reference.

## Install

```bash
npm install @pydantic/opamp-client
```

The package is not on npm yet ([sdk-registries.md](sdk-registries.md)). Until it is, build it in a
checkout and install it from there:

```bash
cd node/opamp
npm ci
npm run build                          # compiles to dist/
cd /path/to/your/app
npm install /path/to/checkout/node/opamp
```

## Start and stop

```ts
import {
  AgentCapabilities,
  AgentDescriptionSchema,
  Client,
  ComponentHealthSchema,
  RemoteConfigStatusSchema,
  RemoteConfigStatuses,
  create,
} from "@pydantic/opamp-client";

const client = Client.start(
  {
    endpoint: process.env.OPAMP_ENDPOINT!, // https://.../v1/opamp, or wss://... for WebSocket
    headers: { Authorization: `Bearer ${process.env.OPAMP_TOKEN}` },
    capabilities: BigInt(AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig),
  },
  {
    description: create(AgentDescriptionSchema, {
      identifyingAttributes: [{ key: "service.name", value: { value: { case: "stringValue", value: "checkout" } } }],
    }),
    health: create(ComponentHealthSchema, { healthy: true }),
  },
  (n) => {
    if (n.kind === "remoteConfig") {
      // Apply n.remoteConfig here, then say how it went.
      client.setRemoteConfigStatus(
        create(RemoteConfigStatusSchema, {
          lastRemoteConfigHash: n.remoteConfig.configHash,
          status: RemoteConfigStatuses.RemoteConfigStatuses_APPLIED,
        }),
      );
    } else if (n.kind === "stopped" && n.error !== undefined) {
      console.error("OpAMP client stopped:", n.error);
    }
  },
);

await new Promise((resolve) => setTimeout(resolve, 3000)); // the application's work
await client.stop(); // sends agent_disconnect, then stops
```

The callback can use `client`: it never runs before `start` has returned. Build messages with
`create()`; protobuf-es won't encode a plain object pushed into a message's repeated field. Give
the agent's status to `start`, so the first message reports it in full.

## Options

The first argument is a `Config`, the second a `Status`; both are interfaces in
`node/opamp/src/index.ts`.

| What | Option | More |
|---|---|---|
| Endpoint | `endpoint`: `http(s)://` polls, `ws(s)://` keeps one connection open | |
| Headers | `headers`, sent with every request and WebSocket upgrade | |
| TLS | `tls`: `ca`, `includeSystemCas`, `cert` and `key`, `minVersion`, `maxVersion` | [README](../node/opamp/README.md#tls-and-proxies) |
| Proxy | `proxy`: `{ url, headers, resolveLocally }`, or `false` for none. Without one, the environment's | [README](../node/opamp/README.md#tls-and-proxies) |
| Who speaks HTTP | `mode`: `"core"` (the default) or `"platform"` (undici's `fetch` and `ws`) | [sdks.md](sdks.md#core-mode-and-platform-mode) |
| State file | `stateFile`: kept across restarts, written 0600 | [state-file.md](state-file.md) |
| Instance UID | `instanceUid` (16 bytes), or omitted for one the core makes; `requestInstanceUid` to have the server assign it; `client.instanceUid` is the one in use | [instance-uid.md](instance-uid.md) |
| Capabilities | `capabilities` (`AgentCapabilities` bits, as a `bigint`). `ReportsStatus`, `ReportsHeartbeat` and the `Reports*` bit of each status you set are added for you | |
| Heartbeat | `heartbeatIntervalMs` until the server offers one (30 s) | |
| Description, health, effective config | `description`, `health`, `effectiveConfig` at start; `setDescription`, `setHealth`, `setEffectiveConfig` later | [sdks.md](sdks.md#what-goes-where) |
| Remote config | `AcceptsRemoteConfig`; answer `remoteConfig` with `setRemoteConfigStatus` | [remote-config.md](remote-config.md) |
| Connection settings | `acceptConnectionSettings` (needs a state file), `verifyTimeoutMs` | [connection-settings.md](connection-settings.md) |
| Telemetry settings | `ReportsOwnTraces`, `ReportsOwnMetrics`, `ReportsOwnLogs`; answer `connectionSettings` with `setConnectionSettingsStatus` | [telemetry-settings.md](telemetry-settings.md) |
| Available components | `availableComponents`, `setAvailableComponents` | [available-components.md](available-components.md) |
| Packages | `acceptPackages` (needs a state file), `maxPackageBytes`, `downloadTimeoutMs`; answer with `packageInstalled`, `packageRemoved` | [packages.md](packages.md) |
| Custom messages | `customCapabilities`, `setCustomCapabilities`, `sendCustomMessage` | [custom-messages.md](custom-messages.md) |
| Limits | `requestTimeoutMs`, `connectTimeoutMs`, `stopTimeoutMs`, `maxSendBytes`, `maxReceiveBytes` | [how-it-works.md](how-it-works.md#effects-end) |

TLS and proxy settings the client can't use throw from `Client.start`, before anything is sent.

## What the client tells the application

The callback gets a `Notification`, a union discriminated by `kind`.

| `kind` | Fields | Answer with |
|---|---|---|
| `connected`, `connectFailed` | `status`, `detail` | |
| `remoteConfig` | `remoteConfig` | `setRemoteConfigStatus` |
| `connectionSettings` | `connectionSettings` | `setConnectionSettingsStatus` |
| `installPackage`, `removePackage` | `packageName`, `package`, `path` | `packageInstalled`, `packageRemoved` |
| `customMessage`, `serverCustomCapabilities`, `customMessageNotSent` | `customMessage`, `customCapabilities`, `detail` | |
| `instanceUidChanged`, `heartbeatInterval`, `command` | `instanceUid`, `heartbeatIntervalMs`, `command` | |
| `errorResponse`, `badMessage`, `statusTooLarge`, `saveStalled` | `errorType`, `message`, `detail`, `field`, `bytes`, `stalled`, `waitedMs` | |
| `stateFromNewerVersion` | `version`: the format of a state a newer release saved, which is kept as it is; the client saves nothing this run | log it as a warning |
| `stopped` | `error`: why it stopped on its own, or `undefined` after `stop()` | |

## The event loop and stop

- The client runs on Node's event loop. The callback runs there, one call at a time, never before
  `start` returns (the first is a microtask, once the code that called `start` yields) and never
  after `stopped`. Keep it quick: nothing else on the loop runs until it returns.
- Every method may be called from the callback and from anywhere on Node's thread. None of them
  blocks.
- State saves are written off the event loop, one at a time.
- The client doesn't keep Node.js running: its timers and connections are unref'd. The
  application's own work does. From `stop()` until the client has stopped the client does hold
  Node.js, so a program that ends with `await client.stop()` says its goodbye first.
- `stop()` sends `agent_disconnect` if a message can still be sent, and resolves when the core
  says it has stopped, within `stopTimeoutMs` (5 s by default). Nothing the client started is
  running then. `client.settled()` resolves once a save still writing has landed.
- From the callback, ask for the stop without waiting for it: `void client.stop()`.
- A callback that throws, or an async callback whose promise rejects, stops the client, without
  the goodbye. `stopped` carries the error, and `stop()` rejects with it. Nothing keeps a Node
  process alive once the client has stopped, so watch for it.

[sdk-contract.md](sdk-contract.md) has the contract every client here keeps.

## Tests

```bash
scripts/fetch_core.py fetch   # the core's test files and the adversary module
cd node/opamp
npm ci
npm test                      # compile, then node --test
```

The conformance suite runs this client in both modes: `python3 conformance/runner/run.py --client
node-core-platform,node-core` ([conformance/README.md](../conformance/README.md)).

## OpenTelemetry

To put the OpenTelemetry SDK under a server without writing code, use `@pydantic/opamp-otel`:
[node-otel.md](node-otel.md). To wire this client to an SDK by hand, see [sdks.md](sdks.md) and
[examples/node](../examples/node/example.mjs). `@pydantic/opamp-client/sdk` runs the shared SDK
settings layer ([sdk-settings.md](sdk-settings.md)).
