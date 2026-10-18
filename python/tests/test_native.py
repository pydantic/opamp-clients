# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The native core (pydantic_opamp_client._native) is held to the WebAssembly module it replaced: for runs
of configs and inputs, plausible and not, every reply is the module's, byte for byte. The module is
what the conformance suite, the taps and the other SDKs check, so this carries their checks over to
the native build. (dotnet's NativeCoreTests holds its build to the same.)"""

from __future__ import annotations

import os
import subprocess
import sys
import textwrap
from pathlib import Path

import pytest
import wasmtime
from _wasm import WasmCore, WasmSdkSettings
from hypothesis import HealthCheck, given, settings
from hypothesis import strategies as st

from pydantic_opamp_client import CoreError, _native
from pydantic_opamp_client._core import Core
from pydantic_opamp_client._proto.opamp.core.v1 import core_pb2 as corepb
from pydantic_opamp_client._proto.opamp.sdk.v1 import settings_pb2
from pydantic_opamp_client._proto.opamp.v1 import opamp_pb2 as pb

EXAMPLES = int(os.environ.get("OPAMP_NATIVE_EXAMPLES", "200"))

ids = st.integers(0, 5)
some_bytes = st.binary(max_size=40)


@st.composite
def server_message(draw) -> bytes:
    if draw(st.integers(0, 5)) == 0:
        return draw(some_bytes)  # garbage
    m = pb.ServerToAgent()
    if draw(st.booleans()):
        m.flags = pb.ServerToAgentFlags_ReportFullState
    if draw(st.booleans()):
        m.agent_identification.new_instance_uid = draw(st.binary(min_size=16, max_size=16))
    if draw(st.booleans()):
        m.remote_config.config_hash = draw(st.binary(min_size=1, max_size=4))
        m.remote_config.config.config_map["a"].body = draw(st.binary(max_size=8))
    if draw(st.booleans()):
        m.error_response.type = pb.ServerErrorResponseType_Unavailable
        m.error_response.retry_info.retry_after_nanoseconds = draw(st.integers(0, 5)) * 1_000_000_000
    if draw(st.booleans()):
        m.connection_settings.hash = draw(st.binary(min_size=1, max_size=4))
        m.connection_settings.opamp.heartbeat_interval_seconds = draw(st.integers(0, 60))
    return m.SerializeToString()


statuses = st.sampled_from([200, 200, 200, 503, 429, 301, 400, 500])
retry_after = st.sampled_from([[], [], [corepb.Header(name="Retry-After", value="7")],
                               [corepb.Header(name="Retry-After", value="Wed, 21 Oct 2026 07:28:00 GMT")]])


@st.composite
def http_bytes(draw) -> bytes:
    body = draw(server_message())
    whole = (f"HTTP/1.1 {draw(statuses)} X\r\nContent-Type: application/x-protobuf\r\n"
             f"Content-Length: {len(body)}\r\n\r\n").encode() + body
    return draw(st.one_of(st.just(whole), some_bytes, st.integers(0, len(whole)).map(lambda n: whole[:n])))


@st.composite
def an_input(draw) -> corepb.Input:
    kind = draw(st.integers(0, 16))
    i = draw(ids)
    if kind == 0:
        return corepb.Input(start=corepb.Empty())
    if kind in (1, 2):
        return corepb.Input(timer=corepb.Empty())
    if kind == 3:
        return corepb.Input(http_response=corepb.HttpResponse(id=i, status=draw(statuses), body=draw(server_message()),
                                                              headers=draw(retry_after)))
    if kind == 4:
        return corepb.Input(http_failed=corepb.Failed(id=i, detail="refused"))
    if kind == 5:
        return corepb.Input(ws_opened=corepb.Id(id=i))
    if kind == 6:
        return corepb.Input(ws_message=corepb.WsMessage(id=i, data=b"\x00" + draw(server_message())))
    if kind == 7:
        return corepb.Input(ws_closed=corepb.WsClosed(id=i, code=1000))
    if kind == 8:
        return corepb.Input(ws_lost=corepb.Failed(id=i, detail="reset"))
    if kind == 9:
        return corepb.Input(stored=corepb.Id(id=i))
    if kind == 10:
        return corepb.Input(set_health=pb.ComponentHealth(healthy=draw(st.booleans()), status="s"))
    if kind == 11:
        return corepb.Input(stream_opened=corepb.Id(id=i))
    if kind == 12:
        return corepb.Input(stream_tls_ready=corepb.Id(id=i))
    if kind == 13:
        return corepb.Input(stream_data=corepb.WsMessage(id=i, data=draw(http_bytes())))
    if kind == 14:
        return corepb.Input(stream_eof=corepb.Id(id=i))
    if kind == 15:
        return corepb.Input(proxy_answer=corepb.ProxyAnswer(id=i))
    return corepb.Input(stop=corepb.Empty())


@st.composite
def a_config(draw) -> corepb.Config:
    streams, persistence = draw(st.booleans()), draw(st.booleans())
    c = corepb.Config(
        endpoint=draw(st.sampled_from(["http://server.test/v1/opamp", "ws://server.test/v1/opamp"])),
        entropy=draw(st.binary(min_size=32, max_size=40)), streams=streams, persistence=persistence,
        accept_connection_settings=draw(st.booleans()), request_instance_uid=draw(st.booleans()),
        heartbeat_interval_ms=draw(st.integers(0, 60_000)), seed=draw(st.integers(0, 2**64 - 1)),
        accept_packages=persistence and draw(st.booleans()), splice_downloads=streams and draw(st.booleans()))
    if draw(st.booleans()):
        c.instance_uid = draw(st.binary(min_size=16, max_size=16))
    if draw(st.booleans()):
        c.headers.append(corepb.Header(name="Authorization", value="Bearer t"))
    return c


# Each step: an input after a delay, or (one in thirty) bytes that aren't a Step at all.
steps = st.lists(st.one_of(st.tuples(st.integers(0, 40_000), an_input()), some_bytes),
                 max_size=80)


def _show(reply: bytes) -> str:
    try:
        return str(corepb.Reply.FromString(reply))
    except Exception:  # noqa: BLE001 - garbage is shown as bytes
        return reply.hex()


@settings(max_examples=EXAMPLES, deadline=None, suppress_health_check=[HealthCheck.too_slow, HealthCheck.data_too_large])
@given(config=a_config(), run=steps)
def test_every_reply_is_the_modules_byte_for_byte(config: corepb.Config, run: list) -> None:
    native, wasm = _native.Core(), WasmCore()
    data = config.SerializeToString()
    assert native.init(data) == wasm.init(data)
    now = 0
    for n, item in enumerate(run):
        if isinstance(item, bytes):
            step = item
        else:
            delay, input_ = item
            now += delay
            step = corepb.Step(now_ms=now, unix_ms=1_760_000_000_000 + now, input=input_).SerializeToString()
        a, b = native.step(step), wasm.step(step)
        assert a == b, f"step {n}: the native reply differs from the module's\nnative: {_show(a)}\nmodule: {_show(b)}"


@settings(max_examples=EXAMPLES, deadline=None)
@given(request=st.one_of(st.binary(max_size=60),
                         st.just(settings_pb2.Request(load=settings_pb2.Load()).SerializeToString())))
def test_sdk_settings_replies_are_the_modules_byte_for_byte(request: bytes) -> None:
    assert _native.sdk_settings_call(request) == WasmSdkSettings().call(request)


def test_sessions_are_independent() -> None:
    a = Core(corepb.Config(instance_uid=bytes([1] * 16), endpoint="http://server/v1/opamp"))
    b = Core(corepb.Config(instance_uid=bytes([2] * 16), endpoint="http://server/v1/opamp"))
    start = corepb.Input(start=corepb.Empty())
    assert a.step(0, start).instance_uid == bytes([1] * 16)
    assert b.step(0, start).instance_uid == bytes([2] * 16)


def test_a_step_before_init_is_an_error_not_a_crash() -> None:
    reply = corepb.Reply.FromString(_native.Core().step(b""))
    assert reply.error == "step before init"


def test_a_trap_is_a_core_error() -> None:
    # A module that traps on every call stands in for a core that panics: either way, CoreError.
    trapping = bytes(wasmtime.wat2wasm("""(module (memory (export "memory") 1)
        (func (export "alloc") (param i32) (result i32) i32.const 0)
        (func (export "free") (param i32 i32))
        (func (export "init") (param i32 i32) (result i64) unreachable)
        (func (export "step") (param i32 i32) (result i64) unreachable))"""))
    with pytest.raises(CoreError, match="trap"):
        Core(corepb.Config(instance_uid=bytes([1] * 16), endpoint="http://server/v1/opamp"), _engine=WasmCore(trapping))


def _load(env: dict[str, str], code: str = "import pydantic_opamp_client._native as n; print(n._find())"
          ) -> subprocess.CompletedProcess:
    """Run code in a fresh interpreter with env, as a user's process would."""
    return subprocess.run([sys.executable, "-c", code], env={**os.environ, **env}, capture_output=True, text=True)


def _core_package(tmp_path: Path, library: bytes | None) -> dict[str, str]:
    """An environment whose pydantic_opamp_core is a package in tmp_path, holding library as its native
    library (none if None): no other library is looked for."""
    (tmp_path / "pydantic_opamp_core").mkdir()
    (tmp_path / "pydantic_opamp_core" / "__init__.py").write_text("")
    if library is not None:
        (tmp_path / "pydantic_opamp_core" / _native._lib_name()).write_bytes(library)
    return {"PYTHONPATH": os.pathsep.join([str(tmp_path), *filter(None, [os.environ.get("PYTHONPATH")])])}


def test_the_library_comes_from_the_core_package() -> None:
    found = _load({})
    assert found.returncode == 0, found.stderr
    assert Path(found.stdout.strip()).parent.name == "pydantic_opamp_core"


# Importing the package never fails because of the library; the calls that need it raise
# ImportError, every time with the same reason, and unavailable() says it.
_CALLS = textwrap.dedent("""
    import pydantic_opamp_client, pydantic_opamp_client.sdk, pydantic_opamp_client.setup
    from pydantic_opamp_client import _native
    print(_native.unavailable())
    for call in (_native.Core, lambda: _native.sdk_settings_call(b""), lambda: _native.setup_call(b"")):
        try:
            call()
        except ImportError as e:
            assert str(e) == _native.unavailable(), e
        else:
            raise AssertionError("no ImportError")
""")


@pytest.mark.parametrize(("library", "reason"), [(None, "isn't installed"), (b"not a library", "can't be loaded")])
def test_a_missing_or_corrupt_library_fails_its_calls_not_the_import(tmp_path: Path, library, reason) -> None:
    run = _load(_core_package(tmp_path, library), _CALLS)
    assert run.returncode == 0, run.stderr
    assert reason in run.stdout, run.stdout


def test_a_closed_session_frees_its_handle_once() -> None:
    core = _native.Core()
    core.__del__()
    core.__del__()  # and again at collection: the handle is gone, nothing is freed twice
    assert core._handle is None


def test_a_core_closed_refuses_every_call_rather_than_crash() -> None:
    # A client frees its session as it stops (Core.close): anything stepped after is an error, not
    # a call into the library with no session.
    core = Core(corepb.Config(instance_uid=bytes([1] * 16), endpoint="http://server/v1/opamp"))
    core.close()
    core.close()
    with pytest.raises(CoreError, match="closed"):
        core.step(0, corepb.Input(start=corepb.Empty()))
