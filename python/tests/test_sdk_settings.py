# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The SDK settings layer through the module: what the Rust tests hold it to, reached from Python."""

import json
from pathlib import Path

import pytest
from opamp_client import anyvalue_pb2, opamp_pb2
from opamp_client.sdk import SdkSettings

APPLIED = opamp_pb2.RemoteConfigStatuses_APPLIED
FAILED = opamp_pb2.RemoteConfigStatuses_FAILED


def config(body: bytes) -> opamp_pb2.AgentRemoteConfig:
    return opamp_pb2.AgentRemoteConfig(config_hash=b"h", config=opamp_pb2.AgentConfigMap(
        config_map={"elastic": opamp_pb2.AgentConfigObject(body=body, content_type="application/json")}))


def effective(e: opamp_pb2.EffectiveConfig) -> bytes:
    return e.config_map.config_map["elastic"].body


def set_aside(e: opamp_pb2.EffectiveConfig) -> dict | None:
    """What the effective config names as set aside (its elastic.set_aside entry), if anything."""
    files = e.config_map.config_map
    assert set(files) <= {"elastic", "elastic.set_aside"}, set(files)
    if "elastic.set_aside" not in files:
        return None
    assert files["elastic.set_aside"].content_type == "application/json"
    return json.loads(files["elastic.set_aside"].body)


# Every setting: what a host declares when it carries out each one (each live one at once, each
# next-start one at the next start).
LIVE = ["custom", "debug_sampling_rate", "debug_send_logs_level", "debug_until", "drop_attributes", "mask_attributes",
        "sampling_rate", "sampling_rules", "sampling_traces_per_second", "send_logs", "send_logs_level",
        "send_logs_level_by_scope", "send_logs_per_second", "send_metrics", "send_traces"]
NEXT_START = ["deactivate_all_instrumentations", "deactivate_instrumentations", "opamp_polling_interval"]
NOW = 1_791_979_200_000  # 2026-10-14T12:00:00Z


def declaring():
    from opamp_client.sdk import settings_pb2

    return settings_pb2.Host(live=LIVE, next_start=NEXT_START)


def test_loads_decides_and_describes() -> None:
    layer = SdkSettings()
    loaded = layer.load(b'{"send_logs":"false"}', declaring())
    assert loaded.settings.HasField("send_logs") and not loaded.settings.send_logs
    assert effective(loaded.effective) == b'{"send_logs":"false"}'

    kept: list[bytes] = []
    d = layer.decide(config(b'{"sampling_rate":"0.25"}'), loaded.current, loaded.current, declaring(),
                     save=kept.append)
    assert d.status.status == APPLIED and d.status.last_remote_config_hash == b"h"
    assert d.apply.sampling_rate == 0.25 and kept == [b'{"sampling_rate":"0.25"}']
    assert effective(d.effective) == b'{"sampling_rate":"0.25"}'
    assert set_aside(d.effective) is None  # nothing set aside, no entry

    bad = layer.decide(config(b'{"sampling_rate":"x"}'), d.current, loaded.current, declaring(), save=kept.append)
    assert bad.status.status == FAILED and bad.status.error_message.startswith("sampling_rate:")
    assert not bad.HasField("apply") and bad.current == d.current
    assert len(kept) == 1  # refused: nothing to keep

    desc = layer.describe([
        anyvalue_pb2.KeyValue(key="service.name", value=anyvalue_pb2.AnyValue(string_value="checkout")),
        anyvalue_pb2.KeyValue(key="host.name", value=anyvalue_pb2.AnyValue(string_value="h")),
    ])
    assert [kv.key for kv in desc.identifying_attributes] == ["service.name"]
    assert [kv.key for kv in desc.non_identifying_attributes] == ["host.name"]


def test_a_host_says_what_it_carries_out() -> None:
    from opamp_client.sdk import settings_pb2

    layer = SdkSettings()
    # The instrumentation switch at once, and no signal's sending: send_traces isn't declared.
    host = settings_pb2.Host(live=["deactivate_instrumentations", "sampling_rate"])
    d = layer.decide(config(b'{"send_traces":"false","deactivate_instrumentations":"urllib","send_trace":"x"}'),
                     b"", b"", host)
    assert d.status.status == APPLIED
    assert list(d.apply.unsupported) == ["send_traces"]
    assert list(d.apply.ignored) == ["send_trace"]  # unknown, told apart
    assert list(d.apply.deactivate_instrumentations.names) == ["urllib"]  # live: in force now, no state needed
    assert effective(d.effective) == b'{"deactivate_instrumentations":"urllib"}'
    # Named for the server beside what is in force (#215), by Decide, Load and Refresh alike, and
    # still after a FAILED, which leaves the document in force.
    names = {"ignored": ["send_trace"], "unsupported": ["send_traces"]}
    assert set_aside(d.effective) == names and d.status.error_message == ""
    assert set_aside(layer.load(d.current, host).effective) == names
    assert set_aside(layer.refresh(d.current, b"", NOW, host).effective) == names
    failed = layer.decide(config(b'{"sampling_rate":"7"}'), d.current, b"", host)
    assert failed.status.status == FAILED and set_aside(failed.effective) == names
    assert not layer.load(b'{"send_traces":"false"}', host).settings.HasField("send_traces")
    # Declared nothing, nothing is carried out.
    nothing = layer.decide(config(b'{"sampling_rate":"0.5"}'), b"", b"", save=lambda doc: None)
    assert not nothing.apply.HasField("sampling_rate") and effective(nothing.effective) == b"{}"
    # A name that is no setting is the host's bug: refused, saying why.
    with pytest.raises(ValueError, match="send_metric"):
        layer.load(b"{}", settings_pb2.Host(live=["send_metric"]))


def test_the_save_the_layer_asks_for_decides_the_answer() -> None:
    """Decide, then commit (#215): the integration saves what the layer asks it to keep, and the
    layer answers from how that went, the same in every language."""
    layer = SdkSettings()
    current = b'{"sampling_rate":"0.5"}'

    def full(doc: bytes) -> None:
        raise OSError(28, "No space left on device")

    d = layer.decide(config(b'{"sampling_rate":"0.25"}'), current, current, declaring(), save=full)
    assert d.status.status == FAILED
    assert d.status.error_message == "can't save the settings: [Errno 28] No space left on device"
    assert not d.HasField("apply") and d.current == current
    assert effective(d.effective) == b'{"sampling_rate":"0.5"}'
    # Whatever stops the save refuses the config.
    d = layer.decide(config(b'{"sampling_rate":"0.25"}'), current, current, declaring(), save=lambda doc: 1 / 0)
    assert (d.status.status, d.status.error_message) == (FAILED, "can't save the settings: division by zero")
    # Nowhere to keep state: a live change is applied, a next-start one refused.
    live = layer.decide(config(b'{"sampling_rate":"0.25"}'), current, current, declaring())
    assert live.status.status == APPLIED
    later = layer.decide(config(b'{"opamp_polling_interval":"1m"}'), current, current, declaring())
    assert later.status.error_message == ("opamp_polling_interval take effect at the next start, which needs "
                                          "somewhere to keep state")
    kept: list[bytes] = []
    saved = layer.decide(config(b'{"opamp_polling_interval":"1m"}'), current, current, declaring(), save=kept.append)
    assert saved.status.status == APPLIED and list(saved.pending_restart) == ["opamp_polling_interval"]
    assert kept == [b'{"opamp_polling_interval":"1m"}']


def test_a_log_level_arrives_as_the_lowest_severity_number_to_export() -> None:
    layer = SdkSettings()
    d = layer.decide(config(b'{"send_logs_level":"Warning"}'), b"", b"", declaring())
    assert d.status.status == APPLIED
    assert d.apply.HasField("send_logs_min_severity") and d.apply.send_logs_min_severity == 13
    assert effective(d.effective) == b'{"send_logs_level":"warn"}'
    assert not layer.load(b"{}", declaring()).settings.HasField("send_logs_min_severity")  # unset: every severity


def test_the_settings_that_match_telemetry_arrive_typed_where_the_host_declares_them() -> None:
    from opamp_client.sdk import settings_pb2

    layer = SdkSettings()
    doc = (b'{"sampling_rules":[{"rate":0,"kind":"server","path":"/health*"}],'
           b'"send_logs_level_by_scope":"com.acme.db*=debug","drop_attributes":"*token*",'
           b'"mask_attributes":"User.Email","sampling_traces_per_second":"50","send_logs_per_second":"0.5"}')
    d = layer.decide(config(doc), b"", b"", declaring(), NOW)
    assert d.status.status == APPLIED
    s = d.apply
    [rule] = s.sampling_rules
    assert (rule.rate, rule.kind, rule.path) == (0, settings_pb2.SPAN_KIND_SERVER, "/health*")
    assert [(lv.pattern, lv.min_severity) for lv in s.send_logs_scope_levels] == [("com.acme.db*", 5)]
    assert (list(s.drop_attributes), list(s.mask_attributes)) == (["*token*"], ["user.email"])
    assert (s.sampling_traces_per_second, s.send_logs_per_second) == (50, 0.5)
    # Undeclared: ignored, never in force.
    plain = layer.decide(config(doc), b"", b"")
    assert not plain.apply.sampling_rules and len(plain.apply.unsupported) == 6
    assert effective(plain.effective) == b"{}"


def test_the_applications_settings_arrive_as_a_map_where_the_host_declares_them() -> None:
    """custom (docs/app-settings.md), and the shared vectors (testdata/sdk-settings/custom.json) through the module."""
    import json
    from pathlib import Path

    from opamp_client.sdk import settings_pb2

    layer = SdkSettings()
    d = layer.decide(config(b'{"custom":{"checkout.new_flow":"on","limit":25,"gone":null}}'), b"", b"", 
                     declaring(), NOW)
    assert d.status.status == APPLIED
    assert dict(d.apply.custom) == {"checkout.new_flow": "on", "limit": "25"}
    assert effective(d.effective) == b'{"custom":"{\\"checkout.new_flow\\":\\"on\\",\\"limit\\":\\"25\\"}"}'
    # Undeclared: set aside, unchecked, never in force.
    plain = layer.decide(config(b'{"custom":[1]}'), b"", b"", settings_pb2.Host(live=["sampling_rate"]))
    assert plain.status.status == APPLIED and list(plain.apply.unsupported) == ["custom"] and not plain.apply.custom
    vectors = json.loads((Path(__file__).resolve().parents[2] / "testdata/sdk-settings/custom.json")
                         .read_text())["cases"]
    for case in vectors:
        h = case.get("host")
        host = (settings_pb2.Host(live=h.get("live", []), next_start=h.get("next_start", [])) if h is not None
                else declaring())
        d = layer.decide(config(case["document"].encode()), b"", b"", host, NOW, save=lambda doc: None)
        if "refused" in case:
            assert d.status.error_message == case["refused"], case["name"]
            continue
        assert dict(d.apply.custom) == case["custom"], case["name"]
        assert json.loads(effective(d.effective)) == case["effective"], case["name"]
        assert set_aside(d.effective) == case.get("set_aside"), case["name"]


def test_debug_mode_arrives_folded_in_and_a_refresh_at_its_end_ends_it() -> None:
    layer = SdkSettings()
    doc = b'{"sampling_rate":"0.1","debug_until":"2026-10-14T12:30:00Z","debug_send_logs_level":"info"}'
    d = layer.decide(config(doc), b"", b"", declaring(), NOW, save=lambda doc: None)
    assert (d.apply.sampling_rate, d.apply.send_logs_min_severity) == (1, 9)
    end = NOW + 30 * 60_000
    assert d.refresh_at_unix_ms == end
    r = layer.refresh(d.current, b"", end, declaring())
    assert (r.settings.sampling_rate, r.refresh_at_unix_ms) == (0.1, 0)
    assert effective(r.effective) == b'{"sampling_rate":"0.1"}'
    assert layer.load(d.current, declaring(), NOW).refresh_at_unix_ms == end


def test_export_health_is_sent_at_the_first_call_and_on_a_failure_and_held_while_nothing_changes() -> None:
    from opamp_client.sdk import settings_pb2

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


VECTORS = Path(__file__).resolve().parents[2] / "testdata" / "sdk-settings"
COMPONENTS = json.loads((VECTORS / "components.json").read_text())["cases"]


def inventory_of(case: dict):
    """A case's inventory, as Inventory's fields."""
    from opamp_client.sdk import settings_pb2

    inv = case.get("inventory", {})
    return settings_pb2.Inventory(
        integration=inv.get("integration", ""), integration_version=inv.get("integration_version", ""),
        sdk_language=inv.get("sdk_language", ""), sdk_version=inv.get("sdk_version", ""),
        instrumentations=[settings_pb2.Instrumentation(name=i.get("name", ""), version=i.get("version", ""))
                          for i in inv.get("instrumentations", [])],
        exporters=[settings_pb2.Exporter(signal=e.get("signal", ""), name=e.get("name", ""),
                                         gated=e.get("gated", False)) for e in inv.get("exporters", [])])


def host_of(case: dict):
    """A case's host, as Host's fields; without one, a host that declares every setting."""
    from opamp_client.sdk import settings_pb2

    host = case.get("host")
    if host is None:
        return declaring()
    return settings_pb2.Host(live=host.get("live", []), next_start=host.get("next_start", []))


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
