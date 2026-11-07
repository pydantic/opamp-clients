# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""What an OpenTelemetry SDK does with the settings an OpAMP server sends it, decided the same way
in every language: the SDK settings layer, run natively
(logfire_fleet_client._native). It keeps nothing between calls; the caller keeps the agent flow's
state it is handed and passes it back with the next step. See docs/sdk-settings.md."""

from __future__ import annotations

from . import _native
from ._proto.opamp.sdk.v1 import settings_pb2
from ._proto.opamp.v1 import anyvalue_pb2, opamp_pb2

__all__ = ["SdkSettings", "settings_pb2"]


class SdkSettings:
    """The layer. Thread-safe."""

    def start(self, start: settings_pb2.AgentStart) -> settings_pb2.AgentOutput:
        """The agent flow's start, before the SDK is built: its state, the effects to carry out in
        order (the settings to put in force first), and when to step it next. See
        docs/sdk-settings.md, "The agent flow". A host naming what is no setting raises ValueError."""
        return self._call(settings_pb2.Request(agent_start=start)).agent_output

    def step(self, step: settings_pb2.AgentStep) -> settings_pb2.AgentOutput:
        """One input to the agent flow (`step.agent`: the state the last output gave): the new
        state, the effects to carry out in order, and when to step it next."""
        return self._call(settings_pb2.Request(agent_step=step)).agent_output

    def describe(self, resource: list[anyvalue_pb2.KeyValue], host: settings_pb2.Host | None = None,
                 now_unix_ms: int = 0) -> opamp_pb2.AgentDescription:
        """The agent description, from the SDK's resource attributes. It also names the policy
        formats the SDK reads and the controls `host` carries out. `host` and `now_unix_ms` as for
        load."""
        describe = settings_pb2.Describe(resource=resource, now_unix_ms=now_unix_ms)
        if host is not None:
            describe.host.CopyFrom(host)
        return self._call(settings_pb2.Request(describe=describe)).description

    def components(self, inventory: settings_pb2.Inventory, host: settings_pb2.Host | None = None,
                   now_unix_ms: int = 0) -> opamp_pb2.AvailableComponents:
        """The inventory as the agent's available components, for the client's
        set_available_components (its hash left for the client to compute): what the integration
        built, and the settings `host` carries out. `host` and `now_unix_ms` as for load. See
        docs/available-components.md."""
        components = settings_pb2.Components(inventory=inventory, now_unix_ms=now_unix_ms)
        if host is not None:
            components.host.CopyFrom(host)
        return self._call(settings_pb2.Request(components=components)).available_components

    def exporters(self, found: list[settings_pb2.FoundExporter], env: dict[str, str] | None = None,
                  default_protocol: str = "") -> settings_pb2.ExportersDecided:
        """What the layer decides of the exporters found on the SDK's providers, the same in every
        language: their inventory entries (name and destination), the signals governed or not,
        and a warning per ungoverned exporter. `env`: the process's environment (only
        OTEL_EXPORTER_* is read); `default_protocol`: the SDK's OTLP protocol when none is set.
        Call it at start and when the providers change. See docs/available-components.md."""
        exporters = settings_pb2.Exporters(found=found, default_protocol=default_protocol,
                                           env=[settings_pb2.EnvVar(name=k, value=v) for k, v in (env or {}).items()
                                                if k.startswith("OTEL_EXPORTER_")])
        return self._call(settings_pb2.Request(exporters=exporters)).exporters_decided

    def _call(self, request: settings_pb2.Request) -> settings_pb2.Reply:
        try:
            raw = _native.sdk_settings_call(request.SerializeToString())
        except _native.Trap as e:
            raise RuntimeError(f"the SDK settings layer trapped: {e}") from e
        reply = settings_pb2.Reply.FromString(raw)
        if reply.WhichOneof("result") == "error":
            raise ValueError(reply.error)
        return reply
