# Python

`pydantic-opamp-client` is this repository's OpAMP client for Python (asyncio), in
[`python/`](../python/). It runs the shared core natively, as a library loaded through `ctypes`
([how-it-works.md](how-it-works.md)), and speaks plain HTTP or WebSocket, with TLS and through
HTTP or SOCKS5 proxies. It needs CPython 3.11 or later, free-threaded builds included. Its
[README](../python/README.md) has the full reference.

## Install

```bash
pip install pydantic-opamp-client
```

The package is not on PyPI yet ([sdk-registries.md](sdk-registries.md)). It depends on
`pydantic-opamp-core`, a wheel per platform that holds the core's library and nothing else. Until
both are published, install them from a checkout:

```bash
scripts/fetch_core.py fetch               # the core release the lock pins, verified
pip install ./python/core ./python        # the core's wheel for this machine, then the client
```

## Start and stop

```python
import asyncio
import os

from pydantic_opamp_client import Client, Config, Notification, Status, anyvalue_pb2, opamp_pb2


async def main() -> None:
    def notify(n: Notification) -> None:
        if n.kind == "remote_config":
            # Apply n.remote_config here, then say how it went.
            client.set_remote_config_status(opamp_pb2.RemoteConfigStatus(
                last_remote_config_hash=n.remote_config.config_hash,
                status=opamp_pb2.RemoteConfigStatuses_APPLIED))
        elif n.kind == "stopped" and n.error is not None:
            print("OpAMP client stopped:", n.error)

    client = Client.start(
        Config(
            endpoint=os.environ["OPAMP_ENDPOINT"],  # https://.../v1/opamp, or wss://... for WebSocket
            headers={"Authorization": "Bearer " + os.environ["OPAMP_TOKEN"]},
            capabilities=opamp_pb2.AgentCapabilities_AcceptsRemoteConfig,
        ),
        Status(
            description=opamp_pb2.AgentDescription(identifying_attributes=[
                anyvalue_pb2.KeyValue(key="service.name",
                                      value=anyvalue_pb2.AnyValue(string_value="checkout"))]),
            health=opamp_pb2.ComponentHealth(healthy=True),
        ),
        notify,
    )
    try:
        await asyncio.sleep(3)  # the application's work
    finally:
        await client.stop()  # sends agent_disconnect, then stops


asyncio.run(main())
```

`Client.start` is synchronous and must be called on a running event loop. The callback can use
`client`: it never runs before `start` has returned. Give the agent's status to `start`, so the
first message reports it in full.

## Options

`Config` and `Status` are dataclasses. Every option is in `python/src/pydantic_opamp_client/_client.py`.

| What | Option | More |
|---|---|---|
| Endpoint | `Config.endpoint`: `http(s)://` polls, `ws(s)://` keeps one connection open | |
| Headers | `Config.headers`, sent with every request and WebSocket upgrade | |
| TLS | `Config.tls`: a `TLS` (`ca_file` or `ca_pem`, `include_system_cas`, `cert_file` and `key_file` or `cert_pem` and `key_pem`, `key_password`, `min_version`, `max_version`), or an `ssl.SSLContext` used as it is | [README](../python/README.md#tls-and-proxies) |
| Proxy | `Config.proxy`: a `Proxy(url, headers=..., resolve_locally=...)` or just its URL; `Proxy.DIRECT` for none. Without one, the environment's | [README](../python/README.md#tls-and-proxies) |
| Who speaks HTTP | `Config.mode`: `"core"` (the default) or `"platform"` (aiohttp) | [sdks.md](sdks.md#core-mode-and-platform-mode) |
| State file | `Config.state_file`: kept across restarts, written 0600 | [state-file.md](state-file.md) |
| Instance UID | `Config.instance_uid` (16 bytes), or `None` for one the core makes; `Config.request_instance_uid` to have the server assign it; `client.instance_uid` is the one in use | [instance-uid.md](instance-uid.md) |
| Capabilities | `Config.capabilities` (`AgentCapabilities` bits). `ReportsStatus`, `ReportsHeartbeat` and the `Reports*` bit of each status you set are added for you | |
| Heartbeat | `Config.heartbeat_interval_ms` until the server offers one (30 s) | |
| Description, health, effective config | `Status.description`, `Status.health`, `Status.effective_config` at start; `set_description`, `set_health`, `set_effective_config` later | [sdks.md](sdks.md#what-goes-where) |
| Remote config | `AcceptsRemoteConfig`; answer `remote_config` with `set_remote_config_status` | [remote-config.md](remote-config.md) |
| Connection settings | `Config.accept_connection_settings` (needs a state file), `Config.verify_timeout_ms` | [connection-settings.md](connection-settings.md) |
| Telemetry settings | `ReportsOwnTraces`, `ReportsOwnMetrics`, `ReportsOwnLogs`; answer `connection_settings` with `set_connection_settings_status` | [telemetry-settings.md](telemetry-settings.md) |
| Available components | `Status.available_components`, `set_available_components` | [available-components.md](available-components.md) |
| Packages | `Config.accept_packages` (needs a state file), `max_package_bytes`, `download_timeout_ms`; answer with `package_installed`, `package_removed` | [packages.md](packages.md) |
| Custom messages | `Status.custom_capabilities`, `set_custom_capabilities`, `send_custom_message` | [custom-messages.md](custom-messages.md) |
| Limits | `request_timeout_ms`, `connect_timeout_ms`, `stop_timeout_ms`, `max_send_bytes`, `max_receive_bytes` | [how-it-works.md](how-it-works.md#effects-end) |

Settings that can't work (a missing file, a bad key, a proxy URL without a host) raise in
`Client.start`, before anything is sent.

## What the client tells the application

The callback gets a `Notification`; `kind` says which, and the fields that go with it are set.

| `kind` | Fields | Answer with |
|---|---|---|
| `connected`, `connect_failed` | `status`, `detail` | |
| `remote_config` | `remote_config` | `set_remote_config_status` |
| `connection_settings` | `connection_settings` | `set_connection_settings_status` |
| `install_package`, `remove_package` | `package_name`, `package`, `path` | `package_installed`, `package_removed` |
| `custom_message`, `server_custom_capabilities`, `custom_message_not_sent` | `custom_message`, `custom_capabilities`, `detail` | |
| `instance_uid_changed`, `heartbeat_interval`, `command` | `instance_uid`, `heartbeat_interval_ms`, `command` | |
| `error_response`, `bad_message`, `status_too_large`, `save_stalled` | `error_type`, `message`, `detail`, `field`, `bytes`, `stalled`, `waited_ms` | |
| `stopped` | `error`: why it stopped on its own, or `None` after `stop()` | |

## Threads and stop

- The client runs on the event loop it was started on. The callback runs there, one call at a
  time, never before `start` returns and never after `stopped`. Keep it quick: nothing else on
  the loop runs until it returns.
- Every method may be called from the callback and from any thread. A call from another thread is
  handed to the loop, so it returns before the core has seen it.
- State saves run on a thread beside the loop, one at a time.
- `await client.stop()` sends `agent_disconnect` if a message can still be sent, and returns when
  the core says it has stopped, within `stop_timeout_ms` (5 s by default). Nothing the client
  started is running then. Awaited on another thread's loop, it is carried out on the client's.
- From the callback, ask for the stop without waiting for it: `asyncio.ensure_future(client.stop())`.
- A callback that raises stops the client, without the goodbye. `stopped` carries the error, and
  `stop()` raises it.

[sdk-contract.md](sdk-contract.md) has the contract every client here keeps.

## Tests

```bash
scripts/fetch_core.py fetch   # the core, its test files and the adversary module
cd python
uv run pytest
```

The conformance suite runs this client in both modes: `python3 conformance/runner/run.py --client
python-core-platform,python-core` ([conformance/README.md](../conformance/README.md)).

## OpenTelemetry

To put an OpenTelemetry SDK under a server without writing code, use `pydantic-opamp-otel`:
[python-otel.md](python-otel.md). To wire this client to an SDK by hand, see [sdks.md](sdks.md)
and [examples/python](../examples/python/example.py). `pydantic_opamp_client.sdk` runs the shared SDK
settings layer ([sdk-settings.md](sdk-settings.md)).
