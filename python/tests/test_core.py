# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT

import email.utils
import os
import struct

import pytest

from pydantic_opamp_client import CoreError
from pydantic_opamp_client._core import Core
from pydantic_opamp_client._proto.opamp.core.v1 import core_pb2 as corepb
from pydantic_opamp_client._proto.opamp.v1 import opamp_pb2 as pb

START = corepb.Input(start=corepb.Empty())
TIMER = corepb.Input(timer=corepb.Empty())


def test_start_sends_the_first_report_and_honours_a_retry_after_date() -> None:
    uid = bytes([7] * 16)
    core = Core(corepb.Config(instance_uid=uid, endpoint="http://server/v1/opamp"))
    out = core.step(0, START)
    assert len(out.effects) == 1
    req = out.effects[0].http_request
    assert req.url == "http://server/v1/opamp"
    msg = pb.AgentToServer.FromString(req.body)
    assert msg.instance_uid == uid and msg.capabilities & 1
    assert out.HasField("wake_at_ms")

    # A 503 with Retry-After as a date, 30 s after the response came: nothing before then, and the
    # next request within the client's own spread after it (a quarter of the wait: 7.5 s).
    came = 1_790_000_000
    busy = corepb.HttpResponse(id=req.id, status=503, unix_ms=came * 1000, headers=[
        corepb.Header(name="Retry-After", value=email.utils.formatdate(came + 30, usegmt=True))])
    core.step(10, corepb.Input(http_response=busy))

    def sends(out: corepb.Output) -> bool:
        return any(e.WhichOneof("kind") == "http_request" for e in out.effects)

    for at in (30_000, 30_009):
        out = core.step(at, TIMER)
        assert not sends(out), at
    now = 30_009
    for _ in range(10):
        wake = out.wake_at_ms
        assert wake > now
        assert not sends(core.step(wake - 1, TIMER)), wake - 1
        out, now = core.step(wake, TIMER), wake
        if sends(out):
            break
    assert sends(out) and 30_010 <= now <= 30_010 + 7_500, now


def test_a_bad_config_is_an_error() -> None:
    with pytest.raises(CoreError):
        Core(corepb.Config(instance_uid=b"\x01\x02"))


def test_cores_are_independent() -> None:
    a = Core(corepb.Config(instance_uid=bytes([1] * 16), endpoint="http://server/v1/opamp"))
    b = Core(corepb.Config(instance_uid=bytes([2] * 16), endpoint="http://server/v1/opamp"))
    assert a.step(0, START).instance_uid[0] == 1
    assert b.step(0, START).instance_uid[0] == 2


def _records(path) -> list[tuple[int, int, bytes]]:
    """The tap's records: (kind, nanos, bytes)."""
    data, out = path.read_bytes(), []
    while data:
        kind, n, nanos = struct.unpack_from("<BIQ", data)
        assert len(data) >= 13 + n, "a whole record"
        out.append((kind, nanos, data[13:13 + n]))
        data = data[13 + n:]
    return out


def test_the_core_tap_records_every_crossing(tmp_path, monkeypatch) -> None:
    tap = tmp_path / "core.tap"
    monkeypatch.setenv("OPAMP_CORE_TAP", str(tap))
    monkeypatch.setattr("pydantic_opamp_client._core._taps", 0)
    config = corepb.Config(instance_uid=bytes([7] * 16), endpoint="http://server/v1/opamp")
    core = Core(config)
    core.step(5, START)
    core.close()

    records = _records(tap)
    assert [k for k, _, _ in records] == [1, 2, 3, 4]
    assert records[0][2] == config.SerializeToString()
    assert corepb.Reply.FromString(records[1][2]).output.instance_uid == bytes([7] * 16)
    step = corepb.Step.FromString(records[2][2])
    # (With the wall clock, which core mode reads.)
    assert step == corepb.Step(now_ms=5, input=START, unix_ms=step.unix_ms) and step.unix_ms > 0
    assert corepb.Reply.FromString(records[3][2]).output.effects[0].WhichOneof("kind") == "http_request"
    nanos = [n for _, n, _ in records]
    assert nanos == sorted(nanos)

    # A later core in the process writes a file of its own.
    second = Core(config)
    second.close()
    assert [k for k, _, _ in _records(tmp_path / f"core.tap.{os.getpid()}.1")] == [1, 2]
    assert len(_records(tap)) == 4

    # Unset: no file at all.
    monkeypatch.delenv("OPAMP_CORE_TAP")
    before = sorted(os.listdir(tmp_path))
    Core(config).step(0, START)
    assert sorted(os.listdir(tmp_path)) == before


def test_a_tap_that_cant_be_opened_is_skipped(tmp_path, monkeypatch, capsys) -> None:
    monkeypatch.setenv("OPAMP_CORE_TAP", str(tmp_path / "missing" / "core.tap"))
    monkeypatch.setattr("pydantic_opamp_client._core._taps", 0)
    Core(corepb.Config(instance_uid=bytes([7] * 16), endpoint="http://server/v1/opamp")).step(0, START)
    assert "OPAMP_CORE_TAP" in capsys.readouterr().err
