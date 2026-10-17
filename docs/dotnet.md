# .NET

`Pydantic.OpAmp.Client` is this repository's OpAMP client for .NET, in
[`dotnet/`](../dotnet/). It runs the shared core natively, as a library called through
`LibraryImport` ([how-it-works.md](how-it-works.md)), and speaks plain HTTP or WebSocket, with
TLS and through HTTP or SOCKS5 proxies. It needs .NET 8 or later. Its
[README](../dotnet/README.md) has the full reference.

## Install

```bash
dotnet add package Pydantic.OpAmp.Client
```

The package is not on NuGet yet ([sdk-registries.md](sdk-registries.md)). It depends on
`Pydantic.OpAmp.Core.Native`, which holds the core's library for each platform and nothing else.
Until both are published, reference the project from a checkout:

```bash
scripts/fetch_core.py fetch   # the core release the lock pins, verified: this machine's library in target/release
```

```xml
<ItemGroup>
  <ProjectReference Include="/path/to/checkout/dotnet/Pydantic.OpAmp.Client/Pydantic.OpAmp.Client.csproj" />
</ItemGroup>
```

The build copies this machine's library to your project's output. It carries no other
platform's.

## Start and stop

```csharp
using OpAmp.Proto.V1;
using Pydantic.OpAmp.Client;

var client = OpAmpClient.Start(
    new OpAmpClientOptions
    {
        Endpoint = new Uri(Environment.GetEnvironmentVariable("OPAMP_ENDPOINT")!), // https://.../v1/opamp, or wss://...
        Headers = new Dictionary<string, string> { ["Authorization"] = "Bearer " + Environment.GetEnvironmentVariable("OPAMP_TOKEN") },
        Capabilities = (ulong)AgentCapabilities.AcceptsRemoteConfig,
    },
    new OpAmpStatus
    {
        Description = new AgentDescription
        {
            IdentifyingAttributes = { new KeyValue { Key = "service.name", Value = new AnyValue { StringValue = "checkout" } } },
        },
        Health = new ComponentHealth { Healthy = true },
    },
    (c, n) =>
    {
        switch (n)
        {
            case OpAmpNotification.RemoteConfig r:
                // Apply r.Config here, then say how it went.
                c.SetRemoteConfigStatus(new RemoteConfigStatus
                {
                    LastRemoteConfigHash = r.Config.ConfigHash,
                    Status = RemoteConfigStatuses.Applied,
                });
                break;
            case OpAmpNotification.Stopped { Error: not null } s:
                Console.Error.WriteLine($"OpAmp client stopped: {s.Error}");
                break;
        }
    });

await Task.Delay(TimeSpan.FromSeconds(3)); // the application's work
await client.StopAsync(); // sends agent_disconnect, then stops
```

The two-argument callback is given the client: answer through `c`, since it may run before the
caller has stored what `Start` returned. (`Start` also takes a callback of the notification
alone.) Give the agent's status to `Start`, so the first message reports it in full.

## Options

`OpAmpClientOptions`, `OpAmpTlsOptions` and `OpAmpStatus` are records with `init` properties, in
`dotnet/Pydantic.OpAmp.Client/OpAmpClient.cs` and `ClientTransport.cs`.

| What | Option | More |
|---|---|---|
| Endpoint | `Endpoint`: `http(s)://` polls, `ws(s)://` keeps one connection open | |
| Headers | `Headers`, sent with every request and WebSocket upgrade | |
| TLS | `Tls = new OpAmpTlsOptions { ... }`: `CaPem`, `IncludeSystemCaCerts`, `ClientCertPem` and `ClientKeyPem`, `MinVersion`, `MaxVersion` | [README](../dotnet/README.md#tls-and-proxies) |
| Proxy | `Proxy` (a `Uri`), `ProxyUseLocalResolver`, or `DirectConnection` for none. Without one, the environment's | [README](../dotnet/README.md#tls-and-proxies) |
| Who speaks HTTP | `Mode = OpAmpMode.Rust` (the default) or `OpAmpMode.DotNet` (`HttpClient` and `ClientWebSocket`); setting `HttpClient` selects .NET mode | [sdks.md](sdks.md#rust-mode-and-platform-mode) |
| State file | `StateFile`: kept across restarts, for the agent's user alone | [state-file.md](state-file.md) |
| Instance UID | `InstanceUid` (16 bytes), or null for one the core makes; `RequestInstanceUid` to have the server assign it; `client.InstanceUid` is the one in use | [instance-uid.md](instance-uid.md) |
| Capabilities | `Capabilities` (`AgentCapabilities` bits, as a `ulong`). `ReportsStatus`, `ReportsHeartbeat` and the `Reports*` bit of each status you set are added for you | |
| Heartbeat | `HeartbeatInterval` until the server offers one (30 s) | |
| Description, health, effective config | `Description`, `Health`, `EffectiveConfig` of `OpAmpStatus` at start; `SetDescription`, `SetHealth`, `SetEffectiveConfig` later | [sdks.md](sdks.md#what-goes-where) |
| Remote config | `AcceptsRemoteConfig`; answer `OpAmpNotification.RemoteConfig` with `SetRemoteConfigStatus` | [remote-config.md](remote-config.md) |
| Connection settings | `AcceptConnectionSettings` (needs a state file), `VerifyTimeout` | [connection-settings.md](connection-settings.md) |
| Telemetry settings | `ReportsOwnTraces`, `ReportsOwnMetrics`, `ReportsOwnLogs`; answer `OpAmpNotification.ConnectionSettings` with `SetConnectionSettingsStatus` | [telemetry-settings.md](telemetry-settings.md) |
| Available components | `OpAmpStatus.AvailableComponents`, `SetAvailableComponents` | [available-components.md](available-components.md) |
| Packages | `AcceptPackages` (needs a state file), `MaxPackageBytes`, `DownloadTimeout`; answer with `PackageInstalled`, `PackageRemoved` | [packages.md](packages.md) |
| Custom messages | `OpAmpStatus.CustomCapabilities`, `SetCustomCapabilities`, `SendCustomMessage` | [custom-messages.md](custom-messages.md) |
| Limits | `RequestTimeout`, `ConnectTimeout`, `StopTimeout`, `MaxSendBytes`, `MaxReceiveBytes` | [how-it-works.md](how-it-works.md#effects-end) |

Bad settings throw `ArgumentException` from `Start`, before anything is sent.

## What the client tells the application

The callback gets an `OpAmpNotification`, a record with one nested record for each kind.

| Record | Properties | Answer with |
|---|---|---|
| `Connected`, `ConnectFailed` | `Status`, `Detail` | |
| `RemoteConfig` | `Config` | `SetRemoteConfigStatus` |
| `ConnectionSettings` | `Settings` | `SetConnectionSettingsStatus` |
| `InstallPackage`, `RemovePackage` | `Name`, `Package`, `Path` | `PackageInstalled`, `PackageRemoved` |
| `CustomMessage`, `ServerCustomCapabilities`, `CustomMessageNotSent` | `Message`, `Capabilities`, `Reason` | |
| `InstanceUidChanged`, `HeartbeatInterval`, `Command` | `InstanceUid`, `Interval`, `Value` | |
| `ErrorResponse`, `BadMessage`, `StatusTooLarge`, `SaveStalled` | `Type`, `Message`, `Detail`, `Field`, `Bytes`, `Stalled`, `Waited` | |
| `Stopped` | `Error`: why it stopped on its own, or null after `StopAsync` | |

## Threads and stop

- The client runs on a loop of its own: a thread-pool task, one at a time. The callback runs
  there, one call at a time, never before `Start` returns and never after `Stopped`. Keep it
  quick: the client waits for it.
- Every method may be called from the callback and from any thread. None of them blocks.
- Saves run beside the client's loop, one at a time.
- `StopAsync()` sends `agent_disconnect` if a message can still be sent, and its task completes
  when the core says it has stopped, within `StopTimeout` (5 s by default). Nothing the client
  started is running then, but a save already writing, which finishes whole or not at all.
  `OpAmpClient.ReadState(stateFile)` waits for that save before it reads the file.
- From the callback, ask for the stop without waiting for it: `_ = client.StopAsync()`, never
  `client.StopAsync().Wait()`.
- A callback that throws stops the client, without the goodbye. `Stopped` carries the exception,
  and the task `StopAsync()` returns faults with it.

[sdk-contract.md](sdk-contract.md) has the contract every client here keeps.

## Tests

```bash
scripts/fetch_core.py fetch   # the core's library, its test files and the adversary module
cd dotnet
dotnet test Pydantic.OpAmp.Client.Tests
```

The conformance suite runs this client in both modes: `python3 conformance/runner/run.py --client
dotnet-core,dotnet-core-rust` ([conformance/README.md](../conformance/README.md)).

## OpenTelemetry

To put automatic instrumentation under a server without writing code, use the plugin
`Pydantic.OpAmp.AutoInstrumentation`; for an SDK set up in code, `services.AddOpenTelemetry().WithOpAmp()`
from `Pydantic.OpAmp.Extensions.Hosting`. Both are in [dotnet-otel.md](dotnet-otel.md). To wire
this client to an SDK by hand, see [sdks.md](sdks.md) and [examples/dotnet](../examples/dotnet/Program.cs).
`Pydantic.OpAmp.Client.Sdk.SdkSettings` runs the shared SDK settings layer
([sdk-settings.md](sdk-settings.md)).
