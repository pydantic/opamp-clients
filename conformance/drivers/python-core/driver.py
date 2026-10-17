# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Runs our Python client (python/: the shared core, natively) under the conformance driver
contract (conformance/DRIVERS.md)."""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import signal
import sys
import time

from opamp_client import TLS, Client, Config, Notification, Status, anyvalue_pb2, opamp_pb2 as pb


RUST = os.environ.get("OPAMP_PYTHON_MODE") == "rust"

# The custom capability DRIVERS.md names: ping is answered with pong.
ECHO = "io.opentelemetry.echo"


def components() -> pb.AvailableComponents:
    """The components DRIVERS.md names: an OTLP receiver (with its version) and a debug exporter."""
    version = anyvalue_pb2.KeyValue(key="version", value=anyvalue_pb2.AnyValue(string_value="1.0.0"))
    return pb.AvailableComponents(components={
        "receivers": pb.ComponentDetails(sub_component_map={"otlp": pb.ComponentDetails(metadata=[version])}),
        "exporters": pb.ComponentDetails(sub_component_map={"debug": pb.ComponentDetails()}),
    })


def emit(event: str, **fields) -> None:
    print(json.dumps({**fields, "event": event, "wall": time.time()}), flush=True)


def env_float(key: str, default: float) -> float:
    try:
        return float(os.environ.get(key, ""))
    except ValueError:
        return default


def instance_uid() -> bytes | None:
    """The 32 hex characters given, all zeros (``zero``), or none (``random``, ``none``): the core
    makes one, a UUIDv7."""
    v = os.environ.get("DRIVER_UID", "random")
    if v == "zero":
        return bytes(16)
    if v not in ("", "random", "none"):
        return bytes.fromhex(v)
    return None


def tls() -> TLS:
    """The CA to trust instead of the system's, and the client certificate, when given: for
    messages and package downloads alike."""
    return TLS(ca_file=os.environ.get("DRIVER_CA_FILE") or None,
               cert_file=os.environ.get("DRIVER_CLIENT_CERT") or None,
               key_file=os.environ.get("DRIVER_CLIENT_KEY") or None)


def description(change: int) -> pb.AgentDescription:
    def kv(k: str, v: str) -> anyvalue_pb2.KeyValue:
        return anyvalue_pb2.KeyValue(key=k, value=anyvalue_pb2.AnyValue(string_value=v))

    non_identifying = [kv("driver.change", str(change))]
    # DRIVER_DESCRIPTION_PAD: a description that long (with DRIVER_MAX_SEND_BYTES, too large to send).
    if os.environ.get("DRIVER_DESCRIPTION_PAD"):
        non_identifying.append(kv("driver.pad", "x" * int(os.environ["DRIVER_DESCRIPTION_PAD"])))
    return pb.AgentDescription(identifying_attributes=[kv("service.name", "conformance-python-core")],
                               non_identifying_attributes=non_identifying)


async def main() -> None:
    loop = asyncio.get_running_loop()
    client: Client | None = None
    # DRIVER_THROW_ON_CONFIG: the callback itself raises on the first remote config (RULE-SHELL-02).
    throw_on_config = os.environ.get("DRIVER_THROW_ON_CONFIG") == "1"
    thrown = False
    # The client stopped on its own: the driver no longer stops it, and exits 1.
    failed = asyncio.Event()

    def on_note(n: Notification) -> None:
        nonlocal thrown
        if n.kind == "stopped" and n.error is not None:
            # The client stopped on its own. Stay DRIVER_LINGER_S, still reporting what the client
            # says (nothing, if it keeps RULE-SHELL-02: a second stopped is a second fatal), then exit 1.
            emit("fatal", error=str(n.error))
            if not failed.is_set():
                failed.set()
                loop.call_later(env_float("DRIVER_LINGER_S", 0), os._exit, 1)
            return
        if n.kind == "remote_config" and throw_on_config and not thrown:
            thrown = True
            emit("throwing")
            raise RuntimeError("callback-throws: the driver's callback raised on the first remote config")
        if n.kind == "connected":
            emit("connect")
        elif n.kind == "connect_failed":
            emit("connect_failed", error=n.detail, status=n.status)
        elif n.kind == "error_response":
            emit("error_response", type=n.error_type, message=n.message)
        elif n.kind == "remote_config":
            emit("message", remote_config_hash=n.remote_config.config_hash.hex())
            # Answered after this callback returns: the client is still handling the message.
            status = pb.RemoteConfigStatus(last_remote_config_hash=n.remote_config.config_hash,
                                           status=pb.RemoteConfigStatuses_APPLIED)
            loop.call_soon(lambda: client.set_remote_config_status(status))
        elif n.kind == "instance_uid_changed":
            emit("message", new_instance_uid=n.instance_uid.hex())
        elif n.kind == "custom_message":
            m = n.custom_message
            emit("custom_message", capability=m.capability, type=m.type, data=m.data.hex())
            if m.type == "ping":
                pong = pb.CustomMessage(capability=ECHO, type="pong", data=m.data)
                loop.call_soon(lambda: client.send_custom_message(pong))
        elif n.kind == "custom_message_not_sent":
            emit("custom_message_not_sent", reason=n.detail)
        elif n.kind == "install_package":
            name, pkg = n.package_name, n.package
            try:
                with open(n.path, "rb") as f:
                    sha, error = hashlib.sha256(f.read()).hexdigest(), None
            except OSError as e:
                sha, error = None, str(e)
            emit("install_package", name=name, version=pkg.version, sha256=sha, error=error)
            loop.call_soon(lambda: client.package_installed(name, pkg.hash, error))
        elif n.kind == "remove_package":
            name = n.package_name
            emit("remove_package", name=name)
            loop.call_soon(lambda: client.package_removed(name))
        elif n.kind == "server_custom_capabilities":
            emit("server_custom_capabilities", capabilities=n.custom_capabilities)
        elif n.kind == "connection_settings":
            s = n.connection_settings
            metrics = s.own_metrics.destination_endpoint if s.HasField("own_metrics") else None
            emit("telemetry_settings", hash=s.hash.hex(), metrics_endpoint=metrics)
            answer = pb.ConnectionSettingsStatus(last_connection_settings_hash=s.hash,
                                                 status=pb.ConnectionSettingsStatuses_APPLIED)
            loop.call_soon(lambda: client.set_connection_settings_status(answer))
        elif n.kind == "heartbeat_interval":
            emit("connection_settings", heartbeat_interval_seconds=n.heartbeat_interval_ms // 1000)

    token = os.environ.get("DRIVER_TOKEN", "")
    # The status to start from, a restart after a crash going on from where the application was
    # (DRIVER_FIRST_CHANGE): the changes count on from it.
    first = int(os.environ.get("DRIVER_FIRST_CHANGE") or 0)
    accept = os.environ.get("DRIVER_ACCEPT_CONNECTION_SETTINGS") == "1"
    try:
        client = Client.start(
            Config(endpoint=os.environ.get("OPAMP_ENDPOINT", ""),
                   instance_uid=instance_uid(),
                   capabilities=pb.AgentCapabilities_AcceptsRemoteConfig | (
                       pb.AgentCapabilities_ReportsOwnMetrics | pb.AgentCapabilities_ReportsOwnTraces
                       | pb.AgentCapabilities_ReportsOwnLogs
                       if os.environ.get("DRIVER_OWN_TELEMETRY") == "1" else 0),
                   heartbeat_interval_ms=int(env_float("DRIVER_POLL_S", 2) * 1000),
                   max_send_bytes=int(os.environ.get("DRIVER_MAX_SEND_BYTES") or 0),
                   headers={"Authorization": f"Bearer {token}"} if token else {},
                   accept_connection_settings=accept,
                   accept_packages=os.environ.get("DRIVER_PACKAGES") == "1",
                   request_instance_uid=os.environ.get("DRIVER_REQUEST_UID") == "1",
                   state_file=os.environ.get("DRIVER_STATE_FILE") or None,
                   tls=tls(),
                   proxy=os.environ.get("DRIVER_PROXY") or None,
                   # The mode is always asked for by name, never left to the client's default:
                   # python-core is Python mode (aiohttp), python-core-rust (OPAMP_PYTHON_MODE=rust)
                   # Rust mode, where the core speaks HTTP and WebSocket over asyncio's streams.
                   mode="rust" if RUST else "python"),
            Status(description=description(first),
                   health=pb.ComponentHealth(healthy=True, start_time_unix_nano=time.time_ns()),
                   effective_config=pb.EffectiveConfig(config_map=pb.AgentConfigMap()),
                   available_components=components() if os.environ.get("DRIVER_COMPONENTS") == "1" else None,
                   custom_capabilities=pb.CustomCapabilities(capabilities=[ECHO])
                   if os.environ.get("DRIVER_CUSTOM") == "1" else None),
            on_note,
        )
    except Exception as e:  # noqa: BLE001 - the contract reports anything as fatal
        emit("fatal", error=str(e))
        sys.exit(1)
    emit("started", uid=client.instance_uid.hex(), client=f"opamp-python-core (ours{', rust mode' if RUST else ''})")

    changes = [float(s) for s in os.environ.get("DRIVER_CHANGES_AT_S", "").split(",") if s.strip()]
    for i, at in enumerate(changes, start=first + 1):
        def change(i: int = i) -> None:
            emit("status_change", change=i)
            client.set_description(description(i))
        loop.call_later(at, change)

    done = asyncio.Event()
    loop.call_later(env_float("DRIVER_RUN_S", 30), done.set)
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, done.set)
    await done.wait()
    if failed.is_set():
        await asyncio.Event().wait()  # exits 1 when DRIVER_LINGER_S is over
    emit("stopping")
    error = None
    try:
        await client.stop()  # bounded by the core (stop_timeout_ms)
    except Exception as e:  # noqa: BLE001
        error = str(e) or type(e).__name__
    emit("stopped", error=error)
    # Stay alive, doing nothing, so the server can see anything the client left running.
    linger = env_float("DRIVER_LINGER_S", 0)
    if linger > 0:
        await asyncio.sleep(linger)


if __name__ == "__main__":
    asyncio.run(main())
