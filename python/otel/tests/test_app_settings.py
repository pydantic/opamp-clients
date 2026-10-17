# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The application's own settings (docs/app-settings.md): what ``opamp_otel.app_settings`` gives the
application to read, on its own and through the agent and the layer, with the shared vectors
(testdata/sdk-settings/custom.json)."""

from __future__ import annotations

import json
import logging
import threading
import time
from pathlib import Path

import pytest
from opamp_client import opamp_pb2
from opamp_client.sdk import SdkSettings, settings_pb2

import opamp_otel
from _policy import FILE, config_map, overrides, policy
from kept import keep_applied
from opamp_otel import Agent, app_settings
from opamp_otel.app_settings import AppSettings

APPLIED = opamp_pb2.RemoteConfigStatuses_APPLIED
FAILED = opamp_pb2.RemoteConfigStatuses_FAILED
LAYER = SdkSettings()
VECTORS = json.loads((Path(__file__).resolve().parents[3] / "testdata" / "sdk-settings"
                      / "custom.json").read_text())


@pytest.fixture(autouse=True)
def _own_state_home(tmp_path_factory, monkeypatch):
    home = tmp_path_factory.mktemp("state-home")
    monkeypatch.setenv("XDG_STATE_HOME", str(home))
    monkeypatch.setenv("HOME", str(home))


class Told:
    """A callback that records what it is told."""

    def __init__(self) -> None:
        self.calls: list[tuple[dict, frozenset]] = []

    def __call__(self, settings, changed) -> None:
        self.calls.append((dict(settings), changed))


def config(body: str | None) -> opamp_pb2.AgentRemoteConfig:
    return opamp_pb2.AgentRemoteConfig(config=config_map(body), config_hash=(body or "").encode())


def values(values: dict, **controls) -> str:
    """A policy whose app_settings control sends `values`, with other controls overridden as given."""
    return policy(app_settings={"values": values}, **controls)


def prepared(tmp_path=None, **extra) -> Agent:
    env = {"OTEL_OPAMP_ENDPOINT": "http://server/v1/opamp", **extra}
    if tmp_path is not None:
        env["OTEL_OPAMP_STATE_DIR"] = str(tmp_path)
    agent = Agent(app=AppSettings())
    agent.prepare(env)
    return agent


def effective(agent: Agent) -> dict:
    """The controls the agent's effective config reports overridden."""
    return overrides(agent.effective)


# --- reading -------------------------------------------------------------------------------------


def test_before_any_config_every_read_is_its_default():
    app = AppSettings()
    assert app.get("checkout.new_flow") is None
    assert app.get("checkout.new_flow", "off") == "off"
    assert app.get_bool("flag", True) is True
    assert app.get_int("limit", 7) == 7
    assert app.get_float("ratio", 0.5) == 0.5
    assert dict(app.snapshot()) == {}


def test_a_value_in_force_is_read_as_sent_and_a_key_left_out_is_its_default():
    app = AppSettings()
    app.put({"greeting": "  Hello ", "empty": ""})
    assert app.get("greeting", "x") == "  Hello "
    assert app.get("empty", "x") == ""  # an empty value is a value
    assert app.get("Greeting", "x") == "x"  # keys are case-sensitive
    assert app.get("missing", "x") == "x"
    assert all(isinstance(v, str) for v in app.snapshot().values())


def test_a_read_never_raises():
    app = AppSettings()
    app.put({"a": "1"})
    assert app.get(["not", "hashable"], "d") == "d"
    assert app.get(None, "d") == "d"
    assert app.get_int(["x"], 3) == 3
    assert app.get_bool(object(), False) is False


@pytest.mark.parametrize("t", VECTORS["typed"], ids=lambda t: repr(t["value"]))
def test_the_typed_reads_take_what_the_shared_vectors_say(t):
    app = AppSettings()
    app.put({"k": t["value"]})
    missing = object()
    for read, want in ((app.get_bool, t["bool"]), (app.get_int, t["int"]), (app.get_float, t["float"])):
        got = read("k", missing)
        assert (None if got is missing else got) == want, (read.__name__, t["value"])
        if want is not None:
            assert type(got) is type(want)  # a bool is not an int, nor an int a float


def test_the_snapshot_is_read_only_and_stays_as_it_was():
    app = AppSettings()
    app.put({"a": "1"})
    before = app.snapshot()
    with pytest.raises(TypeError):
        before["a"] = "2"  # type: ignore[index]
    app.put({"a": "2", "b": "3"})
    assert dict(before) == {"a": "1"}
    assert dict(app.snapshot()) == {"a": "2", "b": "3"}


# --- being told ----------------------------------------------------------------------------------


def test_on_change_is_told_once_per_change_with_the_map_and_the_keys_that_changed():
    app = AppSettings()
    told = Told()
    app.on_change(told)
    app.put({"a": "1", "b": "2"})
    app.put({"a": "1", "b": "2"})  # the same again: nothing changed
    app.put({"a": "1", "b": "3"})
    app.put({"a": "1"})  # b left out: back to its default
    app.put({})
    assert told.calls == [
        ({"a": "1", "b": "2"}, frozenset({"a", "b"})),
        ({"a": "1", "b": "3"}, frozenset({"b"})),
        ({"a": "1"}, frozenset({"b"})),
        ({}, frozenset({"a"})),
    ]


def test_a_callback_sees_the_new_values_through_get_too():
    app = AppSettings()
    seen = []
    app.on_change(lambda settings, changed: seen.append((app.get("a"), settings is app.snapshot())))
    app.put({"a": "1"})
    assert seen == [("1", True)]


def test_callbacks_are_told_in_the_order_they_were_added_and_one_added_twice_twice():
    app = AppSettings()
    order = []
    first = lambda s, c: order.append("first")
    app.on_change(first)
    app.on_change(lambda s, c: order.append("second"))
    app.on_change(first)
    app.put({"a": "1"})
    assert order == ["first", "second", "first"]


def test_a_callback_removed_is_told_nothing_more_and_removing_it_again_does_nothing():
    app = AppSettings()
    told, other = Told(), Told()
    remove = app.on_change(told)
    app.on_change(other)
    app.put({"a": "1"})
    remove()
    remove()
    app.put({"a": "2"})
    assert len(told.calls) == 1 and len(other.calls) == 2


def test_removing_one_of_a_callback_added_twice_leaves_the_other():
    app = AppSettings()
    told = Told()
    remove = app.on_change(told)
    app.on_change(told)
    remove()
    app.put({"a": "1"})
    assert len(told.calls) == 1


def test_a_callback_may_remove_itself_or_add_another_while_it_is_told():
    app = AppSettings()
    told = Told()
    removes = []

    def once(settings, changed):
        removes[0]()
        app.on_change(told)

    removes.append(app.on_change(once))
    app.put({"a": "1"})  # once runs and goes; told is added, and hears from the next change on
    app.put({"a": "2"})
    assert told.calls == [({"a": "2"}, frozenset({"a"}))]


def test_a_callback_that_raises_is_logged_and_the_rest_are_still_told(caplog):
    app = AppSettings()
    told = Told()

    def broken(settings, changed):
        raise RuntimeError("the application's bug")

    app.on_change(broken)
    app.on_change(told)
    with caplog.at_level(logging.ERROR, logger="opamp_otel"):
        app.put({"a": "1"})
        app.put({"a": "2"})
    assert [c[0] for c in told.calls] == [{"a": "1"}, {"a": "2"}]
    assert app.get("a") == "2"  # in force all the same
    errors = [r for r in caplog.records if r.levelno == logging.ERROR]
    assert len(errors) == 2 and "broken" in errors[0].getMessage()
    assert errors[0].exc_info and "the application's bug" in str(errors[0].exc_info[1])


def test_changes_are_told_one_at_a_time_in_order_whichever_thread_puts_them():
    app = AppSettings()
    busy, overlaps, seen = threading.Lock(), [], []

    def slow(settings, changed):
        if not busy.acquire(blocking=False):
            overlaps.append(changed)
            return
        try:
            seen.append(settings["n"])
            time.sleep(0.001)
        finally:
            busy.release()

    app.on_change(slow)
    threads = [threading.Thread(target=lambda i=i: [app.put({"n": f"{i}-{j}"}) for j in range(50)]) for i in range(4)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert not overlaps
    assert seen[-1] == app.get("n")  # the last told is what is in force


def test_readers_on_other_threads_always_see_one_whole_map():
    app = AppSettings()
    stop = threading.Event()
    torn = []

    def read():
        while not stop.is_set():
            s = app.snapshot()
            if len(set(s.values())) > 1:
                torn.append(dict(s))

    readers = [threading.Thread(target=read) for _ in range(3)]
    for r in readers:
        r.start()
    for n in range(2000):
        app.put({k: str(n) for k in "abcdef"})
    stop.set()
    for r in readers:
        r.join()
    assert not torn


def test_callbacks_added_and_removed_from_other_threads_while_changes_are_told():
    app = AppSettings()
    errors = []

    def churn():
        try:
            for _ in range(300):
                app.on_change(lambda s, c: None)()
        except Exception as e:  # noqa: BLE001
            errors.append(e)

    threads = [threading.Thread(target=churn) for _ in range(3)]
    for t in threads:
        t.start()
    for n in range(300):
        app.put({"n": str(n)})
    for t in threads:
        t.join()
    assert not errors


# --- through the agent and the layer -------------------------------------------------------------


@pytest.mark.parametrize("case", VECTORS["cases"], ids=[c["name"] for c in VECTORS["cases"]])
def test_the_shared_vectors_reach_the_application_as_the_layer_decides(case):
    host = opamp_otel.sdk_host(["otlp"])
    if "host" in case:
        host = settings_pb2.Host(live=case["host"].get("live", []), next_start=case["host"].get("next_start", []))
    d = LAYER.decide(config(case["document"]), policy().encode(), policy().encode(), host)
    if "refused" in case:
        assert (d.status.status, d.status.error_message) == (FAILED, case["refused"])
        assert not d.HasField("apply")
        return
    assert d.status.status == APPLIED, d.status.error_message
    agent = Agent(app=AppSettings())
    agent.controls.apply(d.apply)
    assert dict(agent.controls.app.snapshot()) == case["custom"]
    assert d.effective.config_map.config_map[FILE].body.decode() == case["effective"]


def test_a_config_puts_the_values_in_force_and_the_effective_config_reports_them(tmp_path):
    agent = prepared(tmp_path)
    app, told = agent.controls.app, Told()
    app.on_change(told)
    s = agent.remote_config(config(values({"checkout.new_flow": "on", "limit": 25}, head_sampling={"ratio": 0.5})))
    assert s.status == APPLIED
    assert app.get("checkout.new_flow", "off") == "on"
    assert app.get_int("limit", 0) == 25
    assert agent.controls.sampler._rate == 0.5  # the SDK's settings beside them
    assert effective(agent) == {"head_sampling": {"ratio": 0.5},
                                "app_settings": {"values": {"checkout.new_flow": "on", "limit": "25"}}}
    assert told.calls == [({"checkout.new_flow": "on", "limit": "25"}, frozenset({"checkout.new_flow", "limit"}))]


def test_the_same_values_sent_again_or_another_way_tell_nothing(tmp_path):
    agent = prepared(tmp_path)
    told = Told()
    agent.controls.app.on_change(told)
    agent.remote_config(config(values({"a": "1", "b": "2"})))
    # A new config (another hash) whose map is the same: keys in another order, a number for its
    # text, another SDK setting beside it.
    assert agent.remote_config(config(values({"b": "2", "a": 1}))).status == APPLIED
    assert agent.remote_config(config(values({"a": "1", "b": "2"}, head_sampling={"ratio": 1}))).status == APPLIED
    assert len(told.calls) == 1


def test_a_wrong_custom_entry_fails_the_config_and_nothing_changes(tmp_path):
    agent = prepared(tmp_path)
    told = Told()
    agent.controls.app.on_change(told)
    agent.remote_config(config(values({"a": "1"}, head_sampling={"ratio": 0.5})))
    twice = values({"a": "2", "b": "3"}).replace('"b":"3"', '"a":"3"')
    for body, why in ((values({"a": "2", "b": ["x"]}, head_sampling={"ratio": 1}), "app_settings: values must be"),
                      (values({"a b": "2"}), 'app_settings: values: "a b" is not a key'),
                      (twice, "app_settings: values.a: sent more than once"),
                      (values({"a": "x" * 4097}), "app_settings: values.a: 4097 bytes, at most 4096")):
        s = agent.remote_config(config(body))
        assert s.status == FAILED and s.error_message.startswith(why), s.error_message
    assert agent.controls.app.get("a") == "1"
    assert agent.controls.sampler._rate == 0.5
    assert effective(agent) == {"app_settings": {"values": {"a": "1"}}, "head_sampling": {"ratio": 0.5}}
    assert len(told.calls) == 1
    assert not (tmp_path / "sdk-settings.json").exists()  # the client keeps the config applied


def test_app_settings_left_local_put_every_key_back_to_its_default(tmp_path):
    agent = prepared(tmp_path)
    told = Told()
    agent.controls.app.on_change(told)
    agent.remote_config(config(values({"a": "1"})))
    agent.remote_config(config(policy(head_sampling={"ratio": 1}, every=True)))
    assert agent.controls.app.get("a", "default") == "default"
    assert told.calls[-1] == ({}, frozenset({"a"}))
    agent.remote_config(config(values({"a": "1"})))
    agent.remote_config(config(policy()))  # version 1, which has no app_settings control
    assert agent.controls.app.get("a", "default") == "default"
    # A remote config with no policy at all is refused, and the values stay.
    agent.remote_config(config(values({"a": "1"})))
    s = agent.remote_config(config(None))
    assert s.status == FAILED and s.error_message.startswith("config_map:")
    assert agent.controls.app.get("a", "default") == "1"


def test_a_callback_that_raises_does_not_break_the_answer(tmp_path, caplog):
    agent = prepared(tmp_path)

    def broken(settings, changed):
        raise ValueError("nope")

    agent.controls.app.on_change(broken)
    with caplog.at_level(logging.ERROR, logger="opamp_otel"):
        s = agent.remote_config(config(values({"a": "1"})))
    assert s.status == APPLIED
    assert agent.controls.app.get("a") == "1"
    assert effective(agent) == {"app_settings": {"values": {"a": "1"}}}
    assert any("broken" in r.getMessage() for r in caplog.records)


def test_values_survive_a_restart_with_a_state_directory(tmp_path):
    agent = prepared(tmp_path)
    doc = values({"checkout.new_flow": "on"})
    assert agent.remote_config(config(doc)).status == APPLIED
    keep_applied(tmp_path, doc.encode())  # as the client does, before APPLIED goes out
    again = prepared(tmp_path)  # the next start: before any config arrives
    assert again.controls.app.get("checkout.new_flow", "off") == "on"
    assert effective(again) == {"app_settings": {"values": {"checkout.new_flow": "on"}}}


def test_without_a_state_directory_a_restart_starts_from_the_defaults(tmp_path):
    agent = prepared(OTEL_OPAMP_STATE_DIR="none")
    assert agent.remote_config(config(values({"a": "1"}))).status == APPLIED  # live: no state needed
    assert agent.controls.app.get("a") == "1"
    assert prepared(OTEL_OPAMP_STATE_DIR="none").controls.app.get("a", "default") == "default"


def test_a_callback_added_before_the_saved_values_load_is_told_of_them(tmp_path):
    assert prepared(tmp_path).remote_config(config(values({"a": "1"}))).status == APPLIED
    keep_applied(tmp_path, values({"a": "1"}).encode())
    app, told = AppSettings(), Told()
    app.on_change(told)
    agent = Agent(app=app)
    agent.prepare({"OTEL_OPAMP_ENDPOINT": "http://server/v1/opamp", "OTEL_OPAMP_STATE_DIR": str(tmp_path)})
    assert told.calls == [({"a": "1"}, frozenset({"a"}))]


def test_under_another_configurator_the_application_still_reads_its_settings(tmp_path):
    agent = prepared(tmp_path)
    agent.host = opamp_otel.sdk_host(["otlp"], configured=False)
    # The rate is not this package's to carry out here: a policy that overrides it is refused.
    s = agent.remote_config(config(values({"a": "1"}, head_sampling={"ratio": 0.5})))
    assert s.status == FAILED
    assert s.error_message == "head_sampling: this SDK cannot apply the requested override"
    s = agent.remote_config(config(values({"a": "1"})))
    assert s.status == APPLIED
    assert agent.controls.app.get("a") == "1"
    assert effective(agent) == {"app_settings": {"values": {"a": "1"}}}


def test_a_forked_worker_follows_the_application_settings_its_parent_is_given(tmp_path):
    agent = Agent(app=AppSettings())
    told = Told()
    agent.controls.app.on_change(told)
    relay = tmp_path / "live-settings.pb"
    relay.write_bytes(settings_pb2.Settings(custom={"a": "1"}).SerializeToString())
    follower = threading.Thread(target=agent._follow, args=(relay, 0.01), daemon=True)
    follower.start()
    deadline = time.monotonic() + 5
    while agent.controls.app.get("a") != "1" and time.monotonic() < deadline:
        time.sleep(0.01)
    assert agent.controls.app.get("a") == "1"
    relay.unlink()
    follower.join(5)
    assert told.calls == [({"a": "1"}, frozenset({"a"}))]


def test_without_a_server_every_read_is_its_default():
    agent = Agent(app=AppSettings())
    agent.prepare({})
    assert not agent.on
    assert agent.controls.app.get("a", "d") == "d"


def test_the_module_reads_what_the_package_puts_in_force():
    assert opamp_otel.AGENT.controls.app is app_settings.SETTINGS
    assert opamp_otel.app_settings is app_settings
    for name in ("get", "get_bool", "get_int", "get_float", "snapshot", "on_change"):
        assert getattr(app_settings, name).__self__ is app_settings.SETTINGS
    # Through the package's agent, as the server's config would.
    told = Told()
    remove = app_settings.on_change(told)
    try:
        opamp_otel.AGENT.controls.apply(settings_pb2.Settings(custom={"module.check": "on"}))
        assert app_settings.get("module.check", "off") == "on"
        assert app_settings.get_bool("module.check", True) is True  # "on" isn't a boolean: the default
    finally:
        opamp_otel.AGENT.controls.apply(settings_pb2.Settings())
        remove()
    assert app_settings.get("module.check", "off") == "off"
    assert [c[1] for c in told.calls] == [frozenset({"module.check"})] * 2
