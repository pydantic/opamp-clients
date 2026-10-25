# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The SDK settings layer through the module: what the Rust tests hold it to, reached from Python."""

import json
from pathlib import Path

import pytest
from pydantic_opamp_client import anyvalue_pb2, opamp_pb2
from pydantic_opamp_client.sdk import SdkSettings

from _policy import FILE, V1, config, effective, full, policy

APPLIED = opamp_pb2.RemoteConfigStatuses_APPLIED
FAILED = opamp_pb2.RemoteConfigStatuses_FAILED

# Every setting: what a host declares when it carries out each one (each live one at once, each
# next-start one at the next start).
LIVE = ["custom", "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
        "sampling_rate", "sampling_rules", "sampling_traces_per_second", "scrub_defaults", "scrub_patterns", "send_logs",
        "send_logs_level",
        "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces"]
NEXT_START = ["deactivate_all_instrumentations", "deactivate_instrumentations"]
NOW = 1_791_979_200_000  # 2026-10-14T12:00:00Z


def declaring():
    from pydantic_opamp_client.sdk import settings_pb2

    return settings_pb2.Host(live=LIVE, next_start=NEXT_START)


def test_loads_decides_and_describes() -> None:
    layer = SdkSettings()
    logs_off = policy(signal_exports={"logs": {"enabled": False}})
    loaded = layer.load(logs_off, declaring())
    assert loaded.settings.HasField("send_logs") and not loaded.settings.send_logs
    assert effective(loaded.effective) == full(logs_off)

    kept: list[bytes] = []
    quarter = policy(head_sampling={"ratio": 0.25})
    d = layer.decide(config(quarter), loaded.current, loaded.current, declaring(), save=kept.append)
    assert d.status.status == APPLIED and d.status.last_remote_config_hash == b"h"
    assert d.apply.sampling_rate == 0.25 and kept == [quarter]
    assert effective(d.effective) == full(quarter)

    bad = layer.decide(config(policy(head_sampling={"ratio": "x"})), d.current, loaded.current, declaring(),
                       save=kept.append)
    assert bad.status.status == FAILED and bad.status.error_message.startswith("head_sampling:")
    assert not bad.HasField("apply") and bad.current == d.current
    assert len(kept) == 1  # refused: nothing to keep

    desc = layer.describe([
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
    from pydantic_opamp_client.sdk import settings_pb2

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
    from pydantic_opamp_client.sdk import settings_pb2

    layer = SdkSettings()
    # The instrumentation switch at once, and no signal's sending: send_traces is not declared.
    host = settings_pb2.Host(live=["deactivate_instrumentations", "sampling_rate"])
    switch = policy(instrumentations={"disabled": ["urllib"]})
    d = layer.decide(config(switch), b"", b"", host)
    assert d.status.status == APPLIED
    assert list(d.apply.deactivate_instrumentations.names) == ["urllib"]  # live: in force now, no state needed
    assert effective(d.effective) == full(switch)
    # An override the host does not carry out refuses the whole policy, and what was in force stays.
    traces_off = policy(signal_exports={"traces": {"enabled": False}})
    off = layer.decide(config(traces_off), d.current, b"", host)
    assert off.status.status == FAILED and not off.HasField("apply")
    assert off.status.error_message == "signal_exports.traces: this SDK cannot apply the requested override"
    assert off.current == d.current and effective(off.effective) == full(switch)
    # Version 2 names the field too.
    rules = policy(sampling_rules={"rules": [{"rate": 0}]})
    assert (layer.decide(config(rules), d.current, b"", host).status.error_message
            == "sampling_rules: rules: this SDK cannot apply the requested override")
    # A field nothing here knows refuses it too.
    unknown = json.loads(switch) | {"send_trace": "x"}
    assert (layer.decide(config(json.dumps(unknown).encode()), d.current, b"", host).status.error_message
            == 'policy: unknown field "send_trace"')
    # Saved under another host, a setting this one does not carry out is not in force.
    assert not layer.load(traces_off, host).settings.HasField("send_traces")
    # Declared nothing, nothing is carried out: every override is refused.
    nothing = layer.decide(config(policy(head_sampling={"ratio": 0.5})), b"", b"", save=lambda doc: None)
    assert nothing.status.status == FAILED
    assert nothing.status.error_message == "head_sampling: this SDK cannot apply the requested override"
    # A name that is no setting is the host's bug: refused, saying why.
    with pytest.raises(ValueError, match="send_metric"):
        layer.load(policy(), settings_pb2.Host(live=["send_metric"]))


def test_a_policy_is_the_configs_only_file() -> None:
    layer = SdkSettings()
    beside = config(policy())
    beside.config.config_map["other.json"].CopyFrom(opamp_pb2.AgentConfigObject(body=b"{}",
                                                                               content_type="application/json"))
    d = layer.decide(beside, b"", b"", declaring())
    assert d.status.status == FAILED and d.status.error_message.startswith("config_map:")
    none = opamp_pb2.AgentRemoteConfig(config_hash=b"h", config=opamp_pb2.AgentConfigMap())
    d = layer.decide(none, b"", b"", declaring())
    assert d.status.status == FAILED and d.status.error_message.startswith("config_map:")


def test_the_save_the_layer_asks_for_decides_the_answer() -> None:
    """Decide, then commit (#215): the integration saves what the layer asks it to keep, and the
    layer answers from how that went, the same in every language."""
    layer = SdkSettings()
    current = policy(head_sampling={"ratio": 0.5})
    quarter = policy(head_sampling={"ratio": 0.25})

    def disk_full(doc: bytes) -> None:
        raise OSError(28, "No space left on device")

    d = layer.decide(config(quarter), current, current, declaring(), save=disk_full)
    assert d.status.status == FAILED
    assert d.status.error_message == "can't save the settings: [Errno 28] No space left on device"
    assert not d.HasField("apply") and d.current == current
    assert effective(d.effective) == full(current)
    # Whatever stops the save refuses the config.
    d = layer.decide(config(quarter), current, current, declaring(), save=lambda doc: 1 / 0)
    assert (d.status.status, d.status.error_message) == (FAILED, "can't save the settings: division by zero")
    # Nowhere to keep state: a live change is applied, a next-start one refused.
    live = layer.decide(config(quarter), current, current, declaring())
    assert live.status.status == APPLIED
    redis = policy(instrumentations={"disabled": ["redis"]})
    later = layer.decide(config(redis), current, current, declaring())
    assert later.status.error_message == ("instrumentations: takes effect at the next start, which needs "
                                          "somewhere to keep state")
    kept: list[bytes] = []
    saved = layer.decide(config(redis), current, current, declaring(), save=kept.append)
    assert saved.status.status == APPLIED and list(saved.pending_restart) == ["deactivate_instrumentations"]
    assert kept == [redis]


def test_a_log_level_arrives_as_the_lowest_severity_number_to_export() -> None:
    layer = SdkSettings()
    d = layer.decide(config(policy(log_levels={"level": "Warning"})), b"", b"", declaring())
    assert d.status.status == APPLIED
    assert d.apply.HasField("send_logs_min_severity") and d.apply.send_logs_min_severity == 13
    assert effective(d.effective)["log_levels"] == {"mode": "override", "level": "warn"}
    assert not layer.load(policy(), declaring()).settings.HasField("send_logs_min_severity")  # unset: every severity


def test_the_settings_that_match_telemetry_arrive_typed_where_the_host_declares_them() -> None:
    from pydantic_opamp_client.sdk import settings_pb2

    layer = SdkSettings()
    doc = policy(sampling_rules={"rules": [{"rate": 0, "kind": "server", "path": "/health*"}]},
                 log_levels={"scopes": {"com.acme.db*": "debug"}},
                 attribute_redaction={"drop": ["*token*"], "mask": ["User.Email"]},
                 trace_rate_limit={"traces_per_second": 50}, log_rate_limit={"records_per_second": 0.5})
    d = layer.decide(config(doc), b"", b"", declaring(), NOW)
    assert d.status.status == APPLIED
    s = d.apply
    [rule] = s.sampling_rules
    assert (rule.rate, rule.kind, rule.path) == (0, settings_pb2.SPAN_KIND_SERVER, "/health*")
    assert [(lv.pattern, lv.min_severity) for lv in s.send_logs_scope_levels] == [("com.acme.db*", 5)]
    assert (list(s.drop_attributes), list(s.mask_attributes)) == (["*token*"], ["user.email"])
    assert (s.sampling_traces_per_second, s.send_logs_per_second) == (50, 0.5)
    # Undeclared: refused, never in force.
    plain = layer.decide(config(doc), b"", b"")
    assert plain.status.status == FAILED and not plain.HasField("apply")
    assert plain.status.error_message == "sampling_rules: rules: this SDK cannot apply the requested override"


VECTORS = Path(__file__).resolve().parents[2] / "testdata" / "sdk-settings"


def host_of(case: dict):
    """A case's host, as Host's fields; without one, a host that declares every setting."""
    from pydantic_opamp_client.sdk import settings_pb2

    host = case.get("host")
    if host is None:
        return declaring()
    return settings_pb2.Host(live=host.get("live", []), next_start=host.get("next_start", []))


def test_the_applications_settings_arrive_as_a_map_where_the_host_declares_them() -> None:
    """custom (docs/app-settings.md), and the shared vectors (testdata/sdk-settings/custom.json) through the module."""
    from pydantic_opamp_client.sdk import settings_pb2

    layer = SdkSettings()
    doc = policy(app_settings={"values": {"checkout.new_flow": "on", "limit": 25}})
    d = layer.decide(config(doc), b"", b"", declaring(), NOW)
    assert d.status.status == APPLIED
    assert dict(d.apply.custom) == {"checkout.new_flow": "on", "limit": "25"}
    assert effective(d.effective)["app_settings"] == {"mode": "override",
                                                      "values": {"checkout.new_flow": "on", "limit": "25"}}
    # Undeclared: refused, never in force.
    plain = layer.decide(config(doc), b"", b"", settings_pb2.Host(live=["sampling_rate"]))
    assert plain.status.status == FAILED and not plain.HasField("apply")
    assert plain.status.error_message == "app_settings: values: this SDK cannot apply the requested override"
    for case in json.loads((VECTORS / "custom.json").read_text(encoding="utf-8"))["cases"]:
        d = layer.decide(config(case["document"].encode()), b"", b"", host_of(case), NOW, save=lambda doc: None)
        if "refused" in case:
            assert d.status.error_message == case["refused"], case["name"]
            continue
        assert d.status.status == APPLIED, case["name"]
        assert dict(d.apply.custom) == case["custom"], case["name"]
        assert d.effective.config_map.config_map[FILE].body.decode() == case["effective"], case["name"]


SETTINGS = json.loads((VECTORS / "settings.json").read_text(encoding="utf-8"))["cases"]


@pytest.mark.parametrize("case", SETTINGS, ids=[c["name"] for c in SETTINGS])
def test_the_settings_vectors(case: dict) -> None:
    """testdata/sdk-settings/settings.json: how the layer reads a policy, through the module. The
    policy is also the one in force at start, so a next-start setting is in force at once."""
    doc = case["document"].encode()
    d = SdkSettings().decide(config(doc), b"", doc, host_of(case), case.get("now_unix_ms", 0), save=lambda doc: None)
    if "refused" in case:
        assert (d.status.status, d.status.error_message) == (FAILED, case["refused"])
        return
    assert d.status.status == APPLIED, d.status.error_message
    assert d.effective.config_map.config_map[FILE].body.decode() == case["effective"]
    if "refresh_at_unix_ms" in case:
        assert d.refresh_at_unix_ms == case["refresh_at_unix_ms"]


def test_debug_mode_arrives_folded_in_and_a_refresh_at_its_end_ends_it() -> None:
    layer = SdkSettings()
    doc = policy(head_sampling={"ratio": 0.1}, debug={"until": "2026-10-14T12:30:00Z", "log_level": "info"})
    d = layer.decide(config(doc), b"", b"", declaring(), NOW, save=lambda doc: None)
    assert (d.apply.sampling_rate, d.apply.send_logs_min_severity) == (1, 9)
    end = NOW + 30 * 60_000
    assert d.refresh_at_unix_ms == end
    r = layer.refresh(d.current, b"", end, declaring())
    assert (r.settings.sampling_rate, r.refresh_at_unix_ms) == (0.1, 0)
    assert effective(r.effective) == full(policy(head_sampling={"ratio": 0.1}, every=True))
    assert layer.load(d.current, declaring(), NOW).refresh_at_unix_ms == end


def test_export_health_is_sent_at_the_first_call_and_on_a_failure_and_held_while_nothing_changes() -> None:
    from pydantic_opamp_client.sdk import settings_pb2

    layer = SdkSettings()
    first = layer.health(settings_pb2.ExportCounts(), None, NOW)
    assert first.send and sorted(first.exporters.component_health_map) == ["logs", "metrics", "traces"]
    assert first.exporters.status == "ok"
    assert not layer.health(settings_pb2.ExportCounts(), first, NOW + 30_000).send
    failed = layer.health(
        settings_pb2.ExportCounts(traces=settings_pb2.SignalCounts(
            exports_failed=1, items_failed=512, last_failed=True, last_error="connection refused")),
        first, NOW + 30_000, settings_pb2.Settings(send_logs=False))
    assert failed.send and not failed.exporters.healthy
    assert failed.exporters.component_health_map["traces"].last_error == "connection refused"
    assert failed.exporters.component_health_map["logs"].status == "off"


COMPONENTS = json.loads((VECTORS / "components.json").read_text(encoding="utf-8"))["cases"]


def inventory_of(case: dict):
    """A case's inventory, as Inventory's fields."""
    from pydantic_opamp_client.sdk import settings_pb2

    inv = case.get("inventory", {})
    return settings_pb2.Inventory(
        integration=inv.get("integration", ""), integration_version=inv.get("integration_version", ""),
        sdk_language=inv.get("sdk_language", ""), sdk_version=inv.get("sdk_version", ""),
        instrumentations=[settings_pb2.Instrumentation(name=i.get("name", ""), version=i.get("version", ""))
                          for i in inv.get("instrumentations", [])],
        exporters=[settings_pb2.Exporter(signal=e.get("signal", ""), name=e.get("name", ""),
                                         gated=e.get("gated", False)) for e in inv.get("exporters", [])])


def components_json(c: opamp_pb2.AvailableComponents) -> dict:
    """The components as the vectors give them: sdk (key to text), instrumentations (name to
    version or None), exporters (signal to name to gated) and settings (key to applies)."""

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
    # No host: one that declares nothing, as for load and decide.
    got = components_json(SdkSettings().components(inventory_of({}), None, NOW))
    assert got["settings"] == {}


START = json.loads((VECTORS / "start.json").read_text(encoding="utf-8"))["cases"]


@pytest.mark.parametrize("case", START, ids=[c["name"] for c in START])
def test_the_start_vectors(case: dict) -> None:
    """testdata/sdk-settings/start.json: the settings to start with come from the last config
    applied in the core's saved state, else from the document kept before."""
    core_state = bytes.fromhex(case["core_state"]) if "core_state" in case else None
    saved = case["saved"].encode() if "saved" in case else None
    loaded = SdkSettings().load(saved, declaring(), 0, core_state=core_state)
    assert loaded.current == case["want"].encode()
    assert bool(loaded.warning) == case["warns"], loaded.warning
