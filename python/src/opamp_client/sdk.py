# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""What an OpenTelemetry SDK does with the settings an OpAMP server sends it, decided the same way
in every language: the SDK settings layer, run natively
(opamp_client._native). It keeps nothing between calls; the caller keeps the settings documents
it is handed (``current``, and the one it saves) and passes them back. See docs/sdk-settings.md."""

from __future__ import annotations

from collections.abc import Callable

from . import _native
from ._proto.opamp.sdk.v1 import settings_pb2
from ._proto.opamp.v1 import anyvalue_pb2, opamp_pb2

__all__ = ["SdkSettings", "settings_pb2"]


class SdkSettings:
    """The layer. Thread-safe."""

    def load(self, saved: bytes | None, host: settings_pb2.Host | None = None,
             now_unix_ms: int = 0, core_state: bytes | None = None) -> settings_pb2.Loaded:
        """At start, before the SDK is built: the settings to start with, from the document saved at
        the last change (None: nothing saved). Put all of them in force, next-start ones too.
        `host`: the settings the SDK carries out, at once or at the next start. A policy that
        overrides one it leaves out is refused (None: none carried out), and a name that is no
        setting raises ValueError. `now_unix_ms`: the wall clock, which debug mode is decided by
        (0: none, and the SDK cannot carry out debug mode). `core_state`: the OpAMP core's saved state (what it asked the host
        to store; None: none). The settings then come from the last config applied in it, and
        `saved` is read only when it holds none (a document kept before the core kept the config
        applied)."""
        load = settings_pb2.Load(saved=saved) if saved is not None else settings_pb2.Load()
        if core_state is not None:
            load.core_state = core_state
        load.now_unix_ms = now_unix_ms
        if host is not None:
            load.host.CopyFrom(host)
        return self._call(settings_pb2.Request(load=load)).loaded

    def decide(self, config: opamp_pb2.AgentRemoteConfig, current: bytes, at_start: bytes,
               host: settings_pb2.Host | None = None, now_unix_ms: int = 0,
               save: Callable[[bytes], object] | None = None) -> settings_pb2.Decision:
        """How to answer a remote config. `current` is the document in force (from the last load or
        decision), `at_start` the one loaded at start, `host` and `now_unix_ms` as for load.

        The layer decides what to keep for the next start; `save` keeps it (whole or not at all,
        where load reads it then), and raises if it can't; the layer then answers from how that
        went: a config whose save failed is refused, saying why. None: the SDK keeps no state
        across restarts, so a config that changes a next-start setting is refused."""
        decide = settings_pb2.Decide(config=config, current=current, at_start=at_start, now_unix_ms=now_unix_ms)
        if host is not None:
            decide.host.CopyFrom(host)
        plan = self._call(settings_pb2.Request(decide=decide)).plan
        commit = settings_pb2.Commit(decide=decide)
        if plan.HasField("save") and save is not None:
            try:
                save(plan.save)
                commit.saved = settings_pb2.Commit.SAVED_DONE
            except Exception as e:  # noqa: BLE001 - whatever stopped the save, the config is refused for it
                commit.saved = settings_pb2.Commit.SAVED_FAILED
                commit.error = str(e) or type(e).__name__
        return self._call(settings_pb2.Request(commit=commit)).decision

    def refresh(self, current: bytes, at_start: bytes, now_unix_ms: int,
                host: settings_pb2.Host | None = None) -> settings_pb2.Refreshed:
        """What is in force at `now_unix_ms`, at the time a load, decision or refresh said to ask
        again (`refresh_at_unix_ms`): the end of debug mode. `current` and `at_start` as for decide."""
        refresh = settings_pb2.Refresh(current=current, at_start=at_start, now_unix_ms=now_unix_ms)
        if host is not None:
            refresh.host.CopyFrom(host)
        return self._call(settings_pb2.Request(refresh=refresh)).refreshed

    def health(self, counts: settings_pb2.ExportCounts, previous: settings_pb2.HealthReport | None,
               now_unix_ms: int, settings: settings_pb2.Settings | None = None) -> settings_pb2.HealthReport:
        """Export health, from the gates' counts since start: whether to send it now, and the entry
        for the agent health's component_health_map["exporters"]. `previous`: what the last call
        answered (None at the first); `settings`: those in force."""
        health = settings_pb2.Health(counts=counts, now_unix_ms=now_unix_ms)
        if previous is not None:
            health.previous.CopyFrom(previous)
        if settings is not None:
            health.settings.CopyFrom(settings)
        return self._call(settings_pb2.Request(health=health)).health_report

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

    def _call(self, request: settings_pb2.Request) -> settings_pb2.Reply:
        try:
            raw = _native.sdk_settings_call(request.SerializeToString())
        except _native.Trap as e:
            raise RuntimeError(f"the SDK settings layer trapped: {e}") from e
        reply = settings_pb2.Reply.FromString(raw)
        if reply.WhichOneof("result") == "error":
            raise ValueError(reply.error)
        return reply
