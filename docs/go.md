# Go

`github.com/pydantic/opamp-clients/go/opamp` is this repository's OpAMP client for Go, in
[`go/opamp/`](../go/opamp/). It runs the shared core as WebAssembly in
[wazero](https://github.com/tetratelabs/wazero), embedded in the package: pure Go, nothing native
([how-it-works.md](how-it-works.md)). It speaks plain HTTP or WebSocket, with TLS and through
HTTP or SOCKS5 proxies, and uses opamp-go's `protobufs` types for OpAMP's messages. It needs Go
1.25 or later. Its package documentation (`go doc github.com/pydantic/opamp-clients/go/opamp`) has
the full reference.

## Install

```bash
go get github.com/pydantic/opamp-clients/go@latest
```

`@latest` resolves to the newest `go/vX.Y.Z` tag. Nothing else is needed: the core is embedded
in the module.

## Start and stop

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/pydantic/opamp-clients/go/opamp"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client, err := opamp.Start(ctx, opamp.Config{
		Endpoint:     os.Getenv("OPAMP_ENDPOINT"), // https://.../v1/opamp, or wss://... for WebSocket
		Header:       http.Header{"Authorization": {"Bearer " + os.Getenv("OPAMP_TOKEN")}},
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
	}, opamp.Status{
		Description: &protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{{
			Key:   "service.name",
			Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "checkout"}},
		}}},
		Health: &protobufs.ComponentHealth{Healthy: true},
	}, func(c *opamp.Client, n opamp.Notification) {
		switch n.Kind {
		case opamp.RemoteConfig:
			// Apply n.RemoteConfig here, then say how it went.
			c.SetRemoteConfigStatus(&protobufs.RemoteConfigStatus{
				LastRemoteConfigHash: n.RemoteConfig.ConfigHash,
				Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
			})
		case opamp.Stopped:
			if n.Err != nil {
				fmt.Fprintln(os.Stderr, "OpAMP client stopped:", n.Err)
			}
		}
	})
	if err != nil {
		panic(err)
	}

	select { // the application's work
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
	}

	// Sends agent_disconnect, then stops.
	if err := client.Stop(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
```

The callback is given the client: answer through `c`, since it may run before the caller has
stored what `Start` returned. Give the agent's status to `Start`, so the first message reports it
in full.

## Options

`Config` and `Status` are structs; every field is documented in `go/opamp/client.go` and
`go/opamp/options.go`.

| What | Option | More |
|---|---|---|
| Endpoint | `Config.Endpoint`: `http(s)://` polls, `ws(s)://` keeps one connection open | |
| Headers | `Config.Header`, sent with every request and WebSocket upgrade | [sdks.md](sdks.md#credentials-and-state) |
| TLS | `Config.TLS`: `CAPEM`, `IncludeSystemCAs`, `CertPEM` and `KeyPEM`, `MinVersion`, `MaxVersion`, and a `*tls.Config` to start from | package documentation, "TLS and proxies" |
| Proxy | `Config.Proxy`: `URL`, `ConnectHeader`, `ResolveLocally`, or `Direct` for none. Without one, the environment's | package documentation, "TLS and proxies" |
| Who speaks HTTP | `Config.Mode`: `opamp.ModeCore` (the default) or `opamp.ModePlatform` (net/http and gorilla/websocket; advanced); `Config.HTTPClient` needs platform mode named | [sdks.md](sdks.md#core-mode-and-platform-mode) |
| State file | `Config.StateFile`: kept across restarts, written 0600; `Config.StateSealer` encrypts it (`go/statekey` has one: `statekey.Load(spec, stateFile)`) | [state-file.md](state-file.md#encrypted-at-rest-go) |
| Instance UID | `Config.InstanceUID`, or zero for one the core makes; `Config.RequestInstanceUID` to have the server assign it; `client.InstanceUID()` is the one in use | [instance-uid.md](instance-uid.md) |
| Before connecting | `opamp.Open(ctx, cfg)` reads the state file and sends nothing: `InstanceUID()` and `Resumed()` (the config last applied, the one to work on, a refused one's status, the telemetry settings) say what the client starts from; then `Start(status, notify)` or `Close()`. `opamp.Start` is `Open` then `Start` | |
| Capabilities | `Config.Capabilities` (`AgentCapabilities` bits). `ReportsStatus`, `ReportsHeartbeat` and the `Reports*` bit of each status you set are added for you | |
| Heartbeat | `Config.HeartbeatInterval` until the server offers one (30 s) | |
| Description, health, effective config | `Status.Description`, `Status.Health`, `Status.EffectiveConfig` at start; `SetDescription`, `SetHealth`, `SetEffectiveConfig` later | [sdks.md](sdks.md#what-goes-where) |
| Remote config | `AcceptsRemoteConfig`; answer `RemoteConfig` with `SetRemoteConfigStatus` | [remote-config.md](remote-config.md) |
| Connection settings | `Config.AcceptConnectionSettings` (needs a state file), `Config.VerifyTimeout` | [connection-settings.md](connection-settings.md) |
| Telemetry settings | `ReportsOwnTraces`, `ReportsOwnMetrics`, `ReportsOwnLogs`; answer `ConnectionSettings` with `SetConnectionSettingsStatus` | [telemetry-settings.md](telemetry-settings.md) |
| Available components | `Status.AvailableComponents`, `SetAvailableComponents` | [available-components.md](available-components.md) |
| Packages, custom messages, commands, renewed auth headers | Not in this package: they are the Collector's, in `go/opamp/collector` (below) | [packages.md](packages.md), [custom-messages.md](custom-messages.md) |
| Limits | `RequestTimeout`, `ConnectTimeout`, `StopTimeout`, `MaxSendBytes`, `MaxReceiveBytes` | [how-it-works.md](how-it-works.md#effects-end) |

Options that can't work (PEM that doesn't parse, a key that doesn't match its certificate, a proxy
scheme the client doesn't know) make `Start` return an error, before anything is sent.

## What the client tells the application

The callback gets a `Notification`; `Kind` says which, and the fields that go with it are set.

| `Kind` | Fields | Answer with |
|---|---|---|
| `Connected`, `ConnectFailed` | `Status`, `Detail` | |
| `RemoteConfig` | `RemoteConfig` | `SetRemoteConfigStatus` |
| `ConnectionSettings` | `ConnectionSettings` | `SetConnectionSettingsStatus` |
| `InstanceUIDChanged`, `HeartbeatInterval` | `InstanceUID`, `HeartbeatInterval` | |
| `ErrorResponse`, `BadMessage`, `StatusTooLarge`, `SaveStalled` | `ErrorType`, `Message`, `Detail`, `Field`, `FieldName`, `Bytes`, `Stalled`, `Waited` | |
| `StateFromNewerVersion` | `StateVersion`: the format of a state a newer release saved, which is kept as it is; the client saves nothing this run | log it as a warning |
| `Stopped` | `Err`: why it stopped on its own, or nil after `Stop` | |

## The Collector's client

`go/opamp/collector` is the client the Go Supervisor and the Collector's extension use: package
`opamp`'s client with what only they need on top. An SDK uses package `opamp`.

| What | Option |
|---|---|
| Renewed auth headers | `collector.Config.AuthHeader`, a function asked for headers about every second (a token it renews) |
| Packages | `collector.Config.AcceptPackages` (needs a state file), `MaxPackageBytes`, `DownloadTimeout`, `NoSpliceDownloads` (core mode); kinds `InstallPackage`, `RemovePackage` (`PackageName`, `Package`, `Path`); answer `PackageInstalled`, `PackageRemoved`, an error wrapped by `collector.FailedForNow` or `collector.FailedLeavingUnknown` saying how it failed |
| Custom messages | `collector.Status.CustomCapabilities`, `SetCustomCapabilities`, `SendCustomMessage`; kinds `CustomMessage`, `ServerCustomCapabilities`, `CustomMessageNotSent` |
| Commands | kind `Command` (`Command`) |

`collector.Config` embeds `opamp.Config`, `collector.Notification` embeds `opamp.Notification`, and
`collector.Client` embeds `*opamp.Client`: everything above works the same on it.

## Goroutines and stop

- The client runs on a goroutine of its own. The callback runs there, one call at a time, never
  before `Start` returns and never after `Stopped`. Keep it quick: the client waits for it. Hand
  long work to a goroutine of yours, through a channel, as [examples/go](../examples/go/main.go)
  does.
- Every method may be called from the callback and from any goroutine. None but `Stop` blocks.
- `Stop(ctx)` sends `agent_disconnect` if a message can still be sent, and returns when the core
  says it has stopped, within `Config.StopTimeout` (5 s by default), or when `ctx` is done.
  Nothing the client started is running then, but a save already writing, which finishes by
  itself.
- Called from the callback, `Stop` asks for the stop and returns at once, `nil`; the client stops
  once the callback has returned.
- A callback that panics stops the client, not the process, and without the goodbye. `Stopped`
  carries the error in `Err`, and `Stop` returns it.

[sdk-contract.md](sdk-contract.md) has the contract every client here keeps.

## Tests

```bash
scripts/fetch_core.py fetch   # the core's test files and the adversary module
cd go
go test -race ./opamp/... ./statekey/...
```

The conformance suite runs this client in both modes: `python3 conformance/runner/run.py --client
go-core-platform,go-core` ([conformance/README.md](../conformance/README.md)).

## OpenTelemetry

To put an OpenTelemetry SDK under a server with one call, use `go/opampotel`: [go-otel.md](go-otel.md).
To wire this client to an SDK by hand, see [sdks.md](sdks.md) and
[examples/go](../examples/go/main.go). `go/opamp/sdksettings` runs the shared SDK settings layer
([sdk-settings.md](sdk-settings.md)). For the OpenTelemetry Collector, see
[collector.md](collector.md).
