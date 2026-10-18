# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Fuzz target (Atheris): the C interface's buffer handling (pydantic_opamp_client._native and
pydantic_opamp_client._core), with any bytes on both sides of it.

One input is a session of the native core driven through this package's own wrappers: a Config
(any bytes, or a valid one with any saved state), then steps (any bytes, or valid Steps whose inputs
carry any server-to-agent bytes as a response, a WebSocket message or a stream's data), and
calls into the SDK settings layer (sdk.SdkSettings: load, decide on a remote config, describe)
with any bytes. What must hold:

- every call returns bytes the wrapper copied out and freed: never a crash, a leak the RSS limit
  sees, or a reply that isn't one;
- the core and the SDK settings layer never trap (a refusal comes back as a Reply's error, and
  the SDK settings wrapper raises it as ValueError).

    uv run --with atheris python tests/fuzz/fuzz_native.py [libFuzzer flags] [corpus]"""

from __future__ import annotations

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(HERE), str(HERE.parent)]

try:
    import atheris
except ImportError:
    atheris = None

if atheris is not None and __name__ == "__main__":
    with atheris.instrument_imports(include=["pydantic_opamp_client"]):
        import pydantic_opamp_client._core  # noqa: F401

from google.protobuf.message import DecodeError  # noqa: E402

from _fuzzlib import Reader  # noqa: E402
from pydantic_opamp_client import _native  # noqa: E402
from pydantic_opamp_client._proto.opamp.core.v1 import core_pb2 as corepb  # noqa: E402
from pydantic_opamp_client._proto.opamp.sdk.v1 import settings_pb2 as sdkpb  # noqa: E402
from pydantic_opamp_client import anyvalue_pb2, opamp_pb2 as pb  # noqa: E402
from pydantic_opamp_client.sdk import SdkSettings  # noqa: E402


class Problem(AssertionError):
    """A property broken: the fuzzer's crash."""


def _reply(raw: bytes, what: str) -> corepb.Reply:
    try:
        return corepb.Reply.FromString(raw)
    except DecodeError as e:
        raise Problem(f"{what}: {len(raw)} bytes back that aren't a Reply: {e}") from e


def _config(r: Reader) -> bytes:
    if r.byte() & 1:
        return r.blob(2048)
    c = corepb.Config(endpoint=r.pick(["http://s.test/v1/opamp", "ws://s.test/v1/opamp", r.text(40)]),
                      entropy=bytes(32), unix_ms=1_790_000_000_000, seed=r.u64(), capabilities=r.u64(),
                      persistence=bool(r.byte() & 1), accept_packages=bool(r.byte() & 1),
                      accept_connection_settings=bool(r.byte() & 1), streams=bool(r.byte() & 1),
                      max_receive_bytes=r.pick([0, 1, 1024, 2**64 - 1]))
    if c.persistence:
        c.saved_state = r.blob(1024)
    return c.SerializeToString()


def _step(r: Reader, now: int) -> bytes:
    k = r.byte() % 8
    if k == 0:
        return r.blob(2048)
    i = corepb.Input()
    if k == 1:
        i.start.SetInParent()
    elif k == 2:
        i.timer.SetInParent()
    elif k == 3:
        i.http_response.id = r.byte()
        i.http_response.status = r.pick([200, 200, 204, 404, 503, r.u32()])
        i.http_response.body = r.blob(4096)  # any bytes a server may send
    elif k == 4:
        i.ws_message.id = r.byte()
        i.ws_message.data = r.blob(4096)
    elif k == 5:
        i.stop.SetInParent()
    elif k == 6:
        i.stream_data.id = r.byte()
        i.stream_data.data = r.blob(4096)
    else:
        i.ws_opened.id = r.byte()
    return corepb.Step(now_ms=now, unix_ms=1_790_000_000_000 + now, input=i).SerializeToString()


def _sdk(r: Reader, layer: SdkSettings) -> None:
    """The SDK settings layer through its wrapper (the remote config handler of every SDK set up
    by this package): any saved document, any remote config, any resource. A refusal is a
    ValueError; a trap (RuntimeError) is a bug."""
    k = r.byte() % 4
    try:
        if k == 0:
            raw = _native.sdk_settings_call(r.blob(1024))
            try:
                sdkpb.Reply.FromString(raw)
            except DecodeError as e:
                raise Problem(f"the SDK settings layer answered {len(raw)} bytes that aren't a Reply: {e}") from e
        elif k == 1:
            layer.load(r.blob(1024) if r.byte() & 1 else None)
        elif k == 2:
            config = pb.AgentRemoteConfig(config_hash=r.blob(32))
            for _ in range(r.byte() % 3):
                obj = config.config.config_map[r.pick(["logfire-sdk-policy.json", r.text(16)])]
                obj.body = r.blob(1024)
                obj.content_type = r.pick(["", "application/json", "text/yaml", r.text(16)])
            layer.decide(config, r.blob(512), r.blob(512), save=(lambda doc: None) if bool(r.byte() & 1) else None)
        else:
            names = ["sampling_rate", "send_logs", "custom", r.text(16)]
            host = sdkpb.Host(live=[r.pick(names) for _ in range(r.byte() % 3)]) if r.byte() & 1 else None
            layer.describe([anyvalue_pb2.KeyValue(key=r.text(16), value=anyvalue_pb2.AnyValue(string_value=r.text(32)))
                            for _ in range(r.byte() % 3)], host, r.byte() * 1_000_000_000)
    except ValueError:
        pass  # refused, with the reason
    except (_native.Trap, RuntimeError) as e:
        raise Problem(f"the SDK settings layer trapped: {e}") from e


def one(data: bytes) -> None:
    r = Reader(data)
    core = _native.Core()
    layer = SdkSettings()
    config = _config(r)
    try:
        _reply(core.init(config), "init")
    except _native.Trap as e:
        raise Problem(f"init trapped: {e}") from e
    now = 0
    while not r.empty:
        if r.byte() % 5 == 0:
            _sdk(r, layer)
            continue
        now += r.byte() * 100
        step = _step(r, now)
        try:
            _reply(core.step(step), "step")
        except _native.Trap as e:
            raise Problem(f"step trapped: {e}") from e
    del core


def TestOneInput(data: bytes) -> None:  # noqa: N802 - Atheris's name
    one(data)


SEEDS = [
    b"",
    b"\x00\x00\x00" + b"\x01\x01" + b"\x01\x01\x03\x00\x00" + b"\x05\x0a\x00",
    b"\x01\x05\x0a\x03abc" + b"\x01\x00\x00\x10" + bytes(16),
    b"\x00\x02\x00" + b"\x01\x01\x01\x01\x00" + b"\x01\x01\x03\x01\x00\x08\x0a\x02\x01\x02",
]


def main() -> None:
    if atheris is None:
        sys.exit("atheris isn't installed: uv run --with atheris python tests/fuzz/fuzz_native.py")
    atheris.Setup(sys.argv, TestOneInput)
    atheris.Fuzz()


if __name__ == "__main__":
    main()
