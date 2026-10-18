# Java

`dev.pydantic:opamp-client` is this repository's OpAMP client for Java, in
[`java/opamp/`](../java/opamp/). It runs the shared core with [Chicory](https://chicory.dev), the
module compiled to JVM classes at build time: plain Java, nothing native
([how-it-works.md](how-it-works.md)). It speaks plain HTTP or WebSocket, with TLS and through
HTTP or SOCKS5 proxies. Its only dependencies are Chicory and protobuf-javalite. It needs Java 17
or later. Its [README](../java/opamp/README.md) has the full reference.

## Install

```xml
<dependency>
  <groupId>dev.pydantic</groupId>
  <artifactId>opamp-client</artifactId>
  <version>0.1.0-SNAPSHOT</version>
</dependency>
```

The artifact is not on Maven Central yet ([sdk-registries.md](sdk-registries.md)). Until it is,
install it into your local Maven repository from a checkout, and depend on it as above:

```bash
cd java/opamp
mvn install -DskipTests
```

Gradle finds it there with `mavenLocal()` in its repositories.

## Start and stop

```java
import dev.pydantic.opamp.Notification;
import dev.pydantic.opamp.OpampClient;
import java.util.concurrent.CompletableFuture;
import opamp.proto.v1.Anyvalue;
import opamp.proto.v1.Opamp;

public final class Example {
  public static void main(String[] args) throws Exception {
    OpampClient.Config config = OpampClient.Config
        .builder(System.getenv("OPAMP_ENDPOINT")) // https://.../v1/opamp, or wss://... for WebSocket
        .header("Authorization", "Bearer " + System.getenv("OPAMP_TOKEN"))
        .capabilities(Opamp.AgentCapabilities.AgentCapabilities_AcceptsRemoteConfig_VALUE)
        .build();
    Opamp.AgentDescription description = Opamp.AgentDescription.newBuilder()
        .addIdentifyingAttributes(Anyvalue.KeyValue.newBuilder()
            .setKey("service.name")
            .setValue(Anyvalue.AnyValue.newBuilder().setStringValue("checkout")))
        .build();
    OpampClient.Status status = new OpampClient.Status(
        description, Opamp.ComponentHealth.newBuilder().setHealthy(true).build(), null, null);

    // The callback can't name `client` itself: it takes it from a future completed below.
    CompletableFuture<OpampClient> started = new CompletableFuture<>();
    OpampClient client = OpampClient.start(config, status, n -> {
      if (n instanceof Notification.RemoteConfig rc) {
        // Apply rc.config() here, then say how it went.
        started.join().setRemoteConfigStatus(Opamp.RemoteConfigStatus.newBuilder()
            .setLastRemoteConfigHash(rc.config().getConfigHash())
            .setStatus(Opamp.RemoteConfigStatuses.RemoteConfigStatuses_APPLIED)
            .build());
      } else if (n instanceof Notification.Stopped s && s.error() != null) {
        System.err.println("OpAMP client stopped: " + s.error());
      }
    });
    started.complete(client);

    Thread.sleep(3000); // the application's work
    client.stop().join(); // sends agent_disconnect, then stops
  }
}
```

The first notification comes after `start` has returned, but perhaps before the caller has stored
what it returned: the callback takes the client from a future the caller completes. Give the
agent's status to `start`, so the first message reports it in full.

## Options

`OpampClient.Config.builder(endpoint)` (or `builder(endpoint, instanceUid)`) returns a builder;
`build()` checks the settings. `OpampClient.Status` is a record. Both are in
`java/opamp/src/main/java/dev/pydantic/opamp/OpampClient.java`.

| What | Option | More |
|---|---|---|
| Endpoint | `builder(endpoint)`: `http(s)://` polls, `ws(s)://` keeps one connection open | |
| Headers | `header(name, value)`, sent with every request and WebSocket upgrade | |
| TLS | `tls(Tls.builder()...build())`: `trustedCa`, `includeSystemCas`, `clientCertificate`, `minVersion`, `maxVersion` | [README](../java/opamp/README.md#tls-and-proxies) |
| Proxy | `proxy(url)`, `proxyResolveLocally(true)`, or `directConnection()` for none. Without one, the environment's | [README](../java/opamp/README.md#tls-and-proxies) |
| Who speaks HTTP | `mode(OpampClient.Mode.CORE)` (the default) or `mode(OpampClient.Mode.PLATFORM)` (`java.net.http.HttpClient`); `httpClient(...)` selects platform mode | [sdks.md](sdks.md#core-mode-and-platform-mode) |
| State file | `stateFile(Path)`: kept across restarts, for the agent's user alone | [state-file.md](state-file.md) |
| Instance UID | `builder(endpoint, instanceUid)` (16 bytes), or `builder(endpoint)` for one the core makes; `requestInstanceUid(true)` to have the server assign it; `client.instanceUid()` is the one in use | [instance-uid.md](instance-uid.md) |
| Capabilities | `capabilities(long)` (`AgentCapabilities` bits). `ReportsStatus`, `ReportsHeartbeat` and the `Reports*` bit of each status you set are added for you | |
| Heartbeat | `heartbeatInterval(Duration)` until the server offers one (30 s) | |
| Description, health, effective config | the first three of `Status` at start; `setDescription`, `setHealth`, `setEffectiveConfig` later | [sdks.md](sdks.md#what-goes-where) |
| Remote config | `AcceptsRemoteConfig`; answer `Notification.RemoteConfig` with `setRemoteConfigStatus` | [remote-config.md](remote-config.md) |
| Connection settings | `acceptConnectionSettings(true)` (needs a state file), `verifyTimeout(Duration)` | [connection-settings.md](connection-settings.md) |
| Telemetry settings | `ReportsOwnTraces`, `ReportsOwnMetrics`, `ReportsOwnLogs`; answer `Notification.ConnectionSettings` with `setConnectionSettingsStatus` | [telemetry-settings.md](telemetry-settings.md) |
| Available components | `Status` (the fifth component), `setAvailableComponents` | [available-components.md](available-components.md) |
| Packages | `acceptPackages(true)` (needs a state file), `maxPackageBytes`, `downloadTimeout`; answer with `packageInstalled`, `packageRemoved` | [packages.md](packages.md) |
| Custom messages | `Status` (the sixth component), `setCustomCapabilities`, `sendCustomMessage` | [custom-messages.md](custom-messages.md) |
| Limits | `requestTimeout`, `connectTimeout`, `stopTimeout`, `maxSendBytes`, `maxReceiveBytes` | [how-it-works.md](how-it-works.md#effects-end) |

Certificates and keys are read when given, and the proxy URL and the mode by `build()`, so
mistakes fail there.

### Proxies in platform mode

In platform mode the JDK's `HttpClient` carries everything, and two of its limits apply:

- HTTP proxies only. `HttpClient` has no SOCKS and no `https://` proxies, so the client refuses
  those URLs. Without a proxy, platform mode uses `ProxySelector.getDefault()` (`http.proxyHost` and
  the other proxy properties), not the environment.
- The JDK sends no Basic proxy credentials through a CONNECT tunnel (`https://`, `ws://`,
  `wss://`) unless the JVM runs with `-Djdk.http.auth.tunneling.disabledSchemes=`. It is the JVM's
  setting, so the client leaves it to the application, and logs a warning at start.

Core mode, the default, has neither limit: the core speaks to the proxy itself.

## What the client tells the application

The callback gets a `Notification`, a sealed interface of records.

| Record | Components | Answer with |
|---|---|---|
| `Connected`, `ConnectFailed` | `status`, `detail` | |
| `RemoteConfig` | `config` | `setRemoteConfigStatus` |
| `ConnectionSettings` | `settings` | `setConnectionSettingsStatus` |
| `InstallPackage`, `RemovePackage` | `name`, `pkg`, `path` | `packageInstalled`, `packageRemoved` |
| `CustomMessage`, `ServerCustomCapabilities`, `CustomMessageNotSent` | `message`, `capabilities`, `reason` | |
| `InstanceUidChanged`, `HeartbeatInterval`, `Command` | `instanceUid`, `interval`, `command` | |
| `ErrorResponse`, `BadMessage`, `StatusTooLarge`, `SaveStalled` | `type`, `message`, `detail`, `field`, `bytes`, `stalled`, `waited` | |
| `Stopped` | `error`: why it stopped on its own, or null after `stop()` | |

## Threads and stop

- The client runs on a daemon thread of its own, `opamp-client`, so it doesn't keep the JVM
  running. The callback runs there, one call at a time, never before `start` returns and never
  after `Stopped`. Keep it quick: the client waits for it.
- Every method may be called from the callback and from any thread. None of them blocks.
- `stop()` sends `agent_disconnect` if a message can still be sent, and returns a
  `CompletableFuture<Void>` that completes when the core says it has stopped, within
  `stopTimeout` (5 s by default). Nothing the client started is running then; a save already
  writing finishes whole.
- From the callback, ask for the stop without waiting for it: `client.stop()`, never
  `client.stop().join()`.
- A callback that throws (any `Throwable`) stops the client, without the goodbye. `Stopped`
  carries the error, and the future `stop()` returns completes exceptionally with it. A
  `VirtualMachineError` is rethrown on the client's thread after the client has ended.

[sdk-contract.md](sdk-contract.md) has the contract every client here keeps.

## Tests

```bash
scripts/fetch_core.py fetch   # the core's test files and the adversary module
cd java/opamp
mvn package                   # compile and test
```

`mvn -Pfray test -Dfray.only=true` explores thread schedules with Fray; the
[README](../java/opamp/README.md#development) says what it needs. The conformance suite runs this
client in both modes: `python3 conformance/runner/run.py --client java-core-platform,java-core`
([conformance/README.md](../conformance/README.md)).

## OpenTelemetry

To put an application under a server without writing code, use the extension for the
OpenTelemetry Java agent: [javaagent.md](javaagent.md). To wire this client to an SDK by hand, see
[sdks.md](sdks.md) and [examples/java](../examples/java/src/main/java/Example.java).
`dev.pydantic.opamp.sdk.SdkSettings` runs the shared SDK settings layer
([sdk-settings.md](sdk-settings.md)).
