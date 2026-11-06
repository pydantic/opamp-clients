# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The SDK settings layer through the module: what the Rust tests hold it to, reached from Python."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from logfire_fleet_client import anyvalue_pb2, opamp_pb2
from logfire_fleet_client.sdk import SdkSettings, settings_pb2

from _policy import FILE, V1, config, effective, full, policy

APPLIED = opamp_pb2.RemoteConfigStatuses_APPLIED
FAILED = opamp_pb2.RemoteConfigStatuses_FAILED

# Every setting: what a host declares when it carries out each one (each live one at once, each
# next-start one at the next start).
LIVE = ["custom", "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
        "sampling_rate", "sampling_rules", "sampling_traces_per_second", "scrub_defaults", "scrub_patterns", "send_logs",
        "send_logs_level",
        "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_self_metrics", "send_traces"]
NEXT_START = ["deactivate_all_instrumentations", "deactivate_instrumentations"]
NOW = 1_791_979_200_000  # 2026-10-14T12:00:00Z


def declaring():
    from logfire_fleet_client.sdk import settings_pb2

    return settings_pb2.Host(live=LIVE, next_start=NEXT_START)


class Flow:
    """The layer's agent flow, stepped by a test, on clocks that move together from NOW."""

    def __init__(self, host=None, state_dir: bool = True, core_state: bytes | None = None, now: int = NOW) -> None:
        self.layer = SdkSettings()
        start = settings_pb2.AgentStart(state_dir=state_dir, endpoint="http://127.0.0.1:1", instance_uid=bytes(16),
                                        now_unix_ms=now, now_mono_ms=now)
        if host is not None:
            start.host.CopyFrom(host)
        if core_state is not None:
            start.core_state = core_state
        self.now = now
        self.out = self.layer.start(start)

    def step(self, ms: int = 0, **input) -> Flow:
        self.out = self.layer.step(settings_pb2.AgentStep(agent=self.out.agent, now_unix_ms=self.now + ms,
                                                          now_mono_ms=self.now + ms, **input))
        return self

    def decide(self, c, ms: int = 1) -> Flow:
        return self.step(ms, remote_config=c)

    def _effect(self, kind: str):
        found = [getattr(e, kind) for e in self.out.effects if e.WhichOneof("effect") == kind]
        return found[-1] if found else None

    @property
    def status(self) -> opamp_pb2.RemoteConfigStatus:
        return self._effect("status") or opamp_pb2.RemoteConfigStatus()

    @property
    def apply(self):
        return self._effect("apply")

    @property
    def health(self):
        return self._effect("health")

    @property
    def effective(self) -> opamp_pb2.EffectiveConfig:
        return self.out.effective

    @property
    def warned(self) -> bool:
        return any(e.log.level >= settings_pb2.AgentLog.WARN for e in self.out.effects if e.WhichOneof("effect") == "log")

    @property
    def state(self):
        return settings_pb2.AgentState.FromString(self.out.agent)


def test_starts_decides_and_describes() -> None:
    f = Flow(declaring())
    assert f.apply is not None and effective(f.effective) == full(policy())
    quarter = policy(head_sampling={"ratio": 0.25})
    f.decide(config(quarter))
    assert f.status.status == APPLIED and f.status.last_remote_config_hash == b"h"
    assert f.apply.sampling_rate == 0.25 and effective(f.effective) == full(quarter)
    f.decide(config(policy(head_sampling={"ratio": "x"})), 2)
    assert f.status.status == FAILED and f.status.error_message.startswith("head_sampling:")
    assert f.apply is None and effective(f.effective) == full(quarter)
    # A step without an input, or on a state that isn't one, is an error.
    with pytest.raises(ValueError):
        f.layer.step(settings_pb2.AgentStep(agent=f.out.agent))
    with pytest.raises(ValueError):
        f.layer.step(settings_pb2.AgentStep(agent=b"\xff", timer=settings_pb2.ExportCounts()))

    desc = f.layer.describe([
        anyvalue_pb2.KeyValue(key="service.name", value=anyvalue_pb2.AnyValue(string_value="checkout")),
        anyvalue_pb2.KeyValue(key="host.name", value=anyvalue_pb2.AnyValue(string_value="h")),
    ], declaring(), NOW)
    assert [kv.key for kv in desc.identifying_attributes] == ["service.name"]
    assert [kv.key for kv in desc.non_identifying_attributes][0] == "host.name"


def described(desc: opamp_pb2.AgentDescription) -> dict:
    """The description's policy attributes: a string's text, an array's strings."""
    out = {}
    for kv in desc.non_identifying_attributes:
        if kv.key.startswith("logfire.opamp."):
            v = kv.value
            out[kv.key] = ([x.string_value for x in v.array_value.values] if v.WhichOneof("value") == "array_value"
                           else v.string_value)
    return out


def test_the_description_names_the_policy_formats_and_the_controls_the_host_carries_out() -> None:
    from logfire_fleet_client.sdk import settings_pb2

    layer = SdkSettings()
    host = settings_pb2.Host(live=["sampling_rate", "send_logs", "custom"], next_start=["deactivate_instrumentations"])
    got = described(layer.describe([], host, NOW))
    assert got == {
        "logfire.opamp.config_format": V1,
        "logfire.opamp.supported_controls": ["head_sampling", "signal_exports.logs", "app_settings",
                                             "instrumentations"],
    }
    # Every setting declared: every control. No host: none.
    every = described(layer.describe([], declaring(), NOW))["logfire.opamp.supported_controls"]
    assert {"head_sampling", "sampling_rules", "debug", "app_settings", "instrumentations"} <= set(every)
    assert described(layer.describe([]))["logfire.opamp.supported_controls"] == []


def test_a_host_says_what_it_carries_out() -> None:
    # The instrumentation switch at once, and no signal's sending: send_traces is not declared.
    host = settings_pb2.Host(live=["deactivate_instrumentations", "sampling_rate"])
    switch = policy(instrumentations={"disabled": ["urllib"]})
    f = Flow(host, state_dir=False).decide(config(switch))
    assert f.status.status == APPLIED
    assert list(f.apply.deactivate_instrumentations.names) == ["urllib"]  # live: in force now, no state needed
    assert effective(f.effective) == full(switch)
    # An override the host does not carry out refuses the whole policy, and what was in force stays.
    f.decide(config(policy(signal_exports={"traces": {"enabled": False}})), 2)
    assert f.status.status == FAILED and f.apply is None
    assert f.status.error_message == "signal_exports.traces: this SDK cannot apply the requested override"
    assert effective(f.effective) == full(switch)
    # Version 2 names the field too.
    assert (f.decide(config(policy(sampling_rules={"rules": [{"rate": 0}]})), 3).status.error_message
            == "sampling_rules: rules: this SDK cannot apply the requested override")
    # A field nothing here knows refuses it too.
    unknown = json.loads(switch) | {"send_trace": "x"}
    assert f.decide(config(json.dumps(unknown).encode()), 4).status.error_message == 'policy: unknown field "send_trace"'
    # Declared nothing, nothing is carried out: every override is refused.
    nothing = Flow().decide(config(policy(head_sampling={"ratio": 0.5})))
    assert nothing.status.status == FAILED
    assert nothing.status.error_message == "head_sampling: this SDK cannot apply the requested override"
    # A name that is no setting is the host's bug: refused, saying why.
    with pytest.raises(ValueError, match="send_metric"):
        Flow(settings_pb2.Host(live=["send_metric"]))


def test_a_policy_is_the_configs_only_file() -> None:
    beside = config(policy())
    beside.config.config_map["other.json"].CopyFrom(opamp_pb2.AgentConfigObject(body=b"{}",
                                                                               content_type="application/json"))
    f = Flow(declaring()).decide(beside)
    assert f.status.status == FAILED and f.status.error_message.startswith("config_map:")
    none = opamp_pb2.AgentRemoteConfig(config_hash=b"h", config=opamp_pb2.AgentConfigMap())
    f.decide(none, 2)
    assert f.status.status == FAILED and f.status.error_message.startswith("config_map:")


def test_a_next_start_change_needs_somewhere_to_keep_state() -> None:
    quarter = policy(head_sampling={"ratio": 0.25})
    assert Flow(declaring(), state_dir=False).decide(config(quarter)).status.status == APPLIED  # live: no state needed
    redis = policy(instrumentations={"disabled": ["redis"]})
    later = Flow(declaring(), state_dir=False).decide(config(redis))
    assert later.status.error_message == ("instrumentations: takes effect at the next start, which needs "
                                          "somewhere to keep state")
    assert Flow(declaring()).decide(config(redis)).status.status == APPLIED


def test_a_log_level_arrives_as_the_lowest_severity_number_to_export() -> None:
    f = Flow(declaring())
    assert not f.apply.HasField("send_logs_min_severity")  # unset: every severity
    f.decide(config(policy(log_levels={"level": "Warning"})))
    assert f.status.status == APPLIED
    assert f.apply.HasField("send_logs_min_severity") and f.apply.send_logs_min_severity == 13
    assert effective(f.effective)["log_levels"] == {"mode": "override", "level": "warn"}


def test_the_settings_that_match_telemetry_arrive_typed_where_the_host_declares_them() -> None:
    from logfire_fleet_client.sdk import settings_pb2

    layer = SdkSettings()
    doc = policy(sampling_rules={"rules": [{"rate": 0, "kind": "server", "path": "/health*"}]},
                 log_levels={"scopes": {"com.acme.db*": "debug"}},
                 attribute_redaction={"drop": ["*token*"], "mask": ["User.Email"]},
                 trace_rate_limit={"traces_per_second": 50}, log_rate_limit={"records_per_second": 0.5})
    d = Flow(declaring()).decide(config(doc))
    assert d.status.status == APPLIED
    s = d.apply
    [rule] = s.sampling_rules
    assert (rule.rate, rule.kind, rule.path) == (0, settings_pb2.SPAN_KIND_SERVER, "/health*")
    assert [(lv.pattern, lv.min_severity) for lv in s.send_logs_scope_levels] == [("com.acme.db*", 5)]
    assert (list(s.drop_attributes), list(s.mask_attributes)) == (["*token*"], ["user.email"])
    assert (s.sampling_traces_per_second, s.send_logs_per_second) == (50, 0.5)
    # Undeclared: refused, never in force.
    plain = Flow().decide(config(doc))
    assert plain.status.status == FAILED and plain.apply is None
    assert plain.status.error_message == "sampling_rules: rules: this SDK cannot apply the requested override"


VECTORS = Path(__file__).resolve().parents[2] / "testdata" / "sdk-settings"


def host_of(case: dict):
    """A case's host, as Host's fields; without one, a host that declares every setting."""
    from logfire_fleet_client.sdk import settings_pb2

    host = case.get("host")
    if host is None:
        return declaring()
    return settings_pb2.Host(live=host.get("live", []), next_start=host.get("next_start", []))


def test_the_applications_settings_arrive_as_a_map_where_the_host_declares_them() -> None:
    """custom (docs/app-settings.md), and the shared vectors (testdata/sdk-settings/custom.json) through the module."""
    from logfire_fleet_client.sdk import settings_pb2

    layer = SdkSettings()
    doc = policy(app_settings={"values": {"checkout.new_flow": "on", "limit": 25}})
    d = Flow(declaring()).decide(config(doc))
    assert d.status.status == APPLIED
    assert dict(d.apply.custom) == {"checkout.new_flow": "on", "limit": "25"}
    assert effective(d.effective)["app_settings"] == {"mode": "override",
                                                      "values": {"checkout.new_flow": "on", "limit": "25"}}
    # Undeclared: refused, never in force.
    plain = Flow(settings_pb2.Host(live=["sampling_rate"])).decide(config(doc))
    assert plain.status.status == FAILED and plain.apply is None
    assert plain.status.error_message == "app_settings: values: this SDK cannot apply the requested override"
    for case in json.loads((VECTORS / "custom.json").read_text(encoding="utf-8"))["cases"]:
        d = Flow(host_of(case)).decide(config(case["document"].encode()))
        if "refused" in case:
            assert d.status.error_message == case["refused"], case["name"]
            continue
        assert d.status.status == APPLIED, case["name"]
        assert dict(d.apply.custom) == case["custom"], case["name"]
        assert d.effective.config_map.config_map[FILE].body.decode() == case["effective"], case["name"]


def kept_applied(doc: bytes) -> bytes:
    """The client's saved state with a config applied whose only policy file is doc: a state saved
    before the client kept the config applied (start.json), with the config applied added (its
    field 16, a message, which protobuf takes from the end of the encoding)."""
    [base] = [bytes.fromhex(c["core_state"]) for c in START
              if c["name"].startswith("a core state saved before the core kept the config applied")]
    applied = config(doc).SerializeToString()
    size, n = bytearray(), len(applied)
    while True:
        size.append((n & 0x7F) | (0x80 if n > 0x7F else 0))
        n >>= 7
        if not n:
            break
    return base + bytes([16 << 3 | 2, 1]) + bytes(size) + applied


START = json.loads((VECTORS / "start.json").read_text(encoding="utf-8"))["cases"]


SETTINGS = json.loads((VECTORS / "settings.json").read_text(encoding="utf-8"))["cases"]


@pytest.mark.parametrize("case", SETTINGS, ids=[c["name"] for c in SETTINGS])
def test_the_settings_vectors(case: dict) -> None:
    """testdata/sdk-settings/settings.json: how the layer reads a policy, through the module. The
    policy is also the one in force at start, so a next-start setting is in force at once."""
    doc = case["document"].encode()
    if "now_unix_ms" not in case and b'"debug":{"mode":"override"' in doc:
        pytest.skip("a host that gives no time: this SDK always has a clock")
    # The policy kept applied too, so a next-start setting is in force at once.
    d = Flow(host_of(case), core_state=kept_applied(doc), now=case.get("now_unix_ms", NOW)).decide(config(doc), 0)
    if "refused" in case:
        assert (d.status.status, d.status.error_message) == (FAILED, case["refused"])
        return
    assert d.status.status == APPLIED, d.status.error_message
    assert d.effective.config_map.config_map[FILE].body.decode() == case["effective"]
    if "refresh_at_unix_ms" in case:
        assert d.state.refresh_at_unix_ms == case["refresh_at_unix_ms"]


def test_debug_mode_arrives_folded_in_and_the_timer_at_its_end_ends_it() -> None:
    doc = policy(head_sampling={"ratio": 0.1}, debug={"until": "2026-10-14T12:30:00Z", "log_level": "info"})
    f = Flow(declaring()).decide(config(doc), 0)
    assert (f.apply.sampling_rate, f.apply.send_logs_min_severity) == (1, 9)
    end = 30 * 60_000
    assert f.state.refresh_at_unix_ms == NOW + end and 0 < f.out.wake_at_mono_ms <= NOW + end
    f.step(end, timer=settings_pb2.ExportCounts())
    assert (f.apply.sampling_rate, f.state.refresh_at_unix_ms) == (0.1, 0)
    assert effective(f.effective) == full(policy(head_sampling={"ratio": 0.1}, every=True))


def test_export_health_is_sent_when_built_and_on_a_failure_and_held_while_nothing_changes() -> None:
    def exporters(counts=None, *listed):
        return {"exporters": settings_pb2.AgentExporters(exporters=listed, counts=counts or settings_pb2.ExportCounts())}

    f = Flow(declaring())
    f.step(built=settings_pb2.AgentBuilt(service="checkout"))
    f.step(**exporters())
    first = f.health.component_health_map["exporters"]
    assert sorted(first.component_health_map) == ["logs", "metrics", "traces"] and first.status == "ok"
    assert f.step(30_000, **exporters()).health is None
    failing = settings_pb2.ExportCounts(traces=settings_pb2.SignalCounts(
        exports_failed=1, items_failed=512, last_failed=True, last_error="connection refused"))
    f.step(30_000, **exporters(failing))
    sent = f.health.component_health_map["exporters"]
    assert not sent.healthy and sent.component_health_map["traces"].last_error == "connection refused"


COMPONENTS = json.loads((VECTORS / "components.json").read_text(encoding="utf-8"))["cases"]


def inventory_of(case: dict):
    """A case's inventory, as Inventory's fields."""
    from logfire_fleet_client.sdk import settings_pb2

    inv = case.get("inventory", {})
    return settings_pb2.Inventory(
        integration=inv.get("integration", ""), integration_version=inv.get("integration_version", ""),
        sdk_language=inv.get("sdk_language", ""), sdk_version=inv.get("sdk_version", ""),
        instrumentations=[settings_pb2.Instrumentation(name=i.get("name", ""), version=i.get("version", ""))
                          for i in inv.get("instrumentations", [])],
        exporters=[settings_pb2.Exporter(signal=e.get("signal", ""), name=e.get("name", ""),
                                         gated=e.get("gated", False), destination=e.get("destination", ""))
                   for e in inv.get("exporters", [])])


def components_json(c: opamp_pb2.AvailableComponents) -> dict:
    """The components as the vectors give them: sdk (key to text), instrumentations (name to
    version or None), exporters (signal to identity to gated), destinations (signal to identity to
    destination, for those with one) and settings (key to applies)."""

    def text(d: opamp_pb2.ComponentDetails, key: str) -> str | None:
        found = [kv.value for kv in d.metadata if kv.key == key]
        if not found:
            return None
        assert found[0].WhichOneof("value") == "string_value", f"{key}: not text"
        return found[0].string_value

    def gated(d: opamp_pb2.ComponentDetails) -> bool:
        [kv] = [kv for kv in d.metadata if kv.key == "gated"]
        assert kv.value.WhichOneof("value") == "bool_value"
        return kv.value.bool_value

    parts = c.components
    return {
        "sdk": {kv.key: text(parts["sdk"], kv.key) for kv in parts["sdk"].metadata},
        "instrumentations": {n: text(d, "version") for n, d in parts["instrumentations"].sub_component_map.items()},
        "exporters": {s: {n: gated(e) for n, e in d.sub_component_map.items()}
                      for s, d in parts["exporters"].sub_component_map.items()},
        "destinations": {s: to for s, d in parts["exporters"].sub_component_map.items()
                         if (to := {n: text(e, "destination") for n, e in d.sub_component_map.items()
                                    if text(e, "destination") is not None})},
        "settings": {k: text(d, "applies") for k, d in parts["settings"].sub_component_map.items()},
    }


@pytest.mark.parametrize("case", COMPONENTS, ids=[c["name"] for c in COMPONENTS])
def test_the_inventory_is_the_components_the_shared_vectors_give(case) -> None:
    got = SdkSettings().components(inventory_of(case), host_of(case), case.get("now_unix_ms", 0))
    assert got.hash == b"", "the client computes the hash"
    assert sorted(got.components) == ["exporters", "instrumentations", "sdk", "settings"]
    got_json = components_json(got)
    for part, want in case["want"].items():
        assert got_json[part] == want, part


def test_the_inventory_without_a_host_lists_no_setting() -> None:
    # No host: one that declares nothing.
    got = components_json(SdkSettings().components(inventory_of({}), None, NOW))
    assert got["settings"] == {}


EXPORTERS = json.loads((VECTORS / "exporters.json").read_text(encoding="utf-8"))["cases"]


@pytest.mark.parametrize("case", EXPORTERS, ids=[c["name"] for c in EXPORTERS])
def test_the_exporters_vectors(case: dict) -> None:
    """testdata/sdk-settings/exporters.json through the module: what is decided of the exporters found."""
    found = [settings_pb2.FoundExporter(**f) for f in case["found"]]
    got = SdkSettings().exporters(found, case.get("env", {}), case.get("default_protocol", ""))
    assert {
        "exporters": [{"signal": e.signal, "name": e.name, "gated": e.gated, "destination": e.destination}
                      for e in got.exporters],
        "governed": list(got.governed), "ungoverned": list(got.ungoverned), "warnings": list(got.warnings),
    } == case["want"]




@pytest.mark.parametrize("case", START, ids=[c["name"] for c in START])
def test_the_start_vectors(case: dict) -> None:
    """testdata/sdk-settings/start.json: the settings to start with come from the last config
    applied in the core's saved state, else the SDK's own."""
    core_state = bytes.fromhex(case["core_state"]) if "core_state" in case else None
    f = Flow(declaring(), core_state=core_state)
    assert f.state.current == case["want"].encode()
    assert f.warned == case["warns"]
