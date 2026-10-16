"""The client's saved state (opamp-state) holding a config applied, as the client leaves it once
the server has heard APPLIED: what the next start's settings come from. A test without a client
writes it here.

The bytes are a state the client saved before it kept the config applied (the shared vectors'
testdata/sdk-settings/start.json), with the config applied added: its field 16, a message, which
protobuf takes from the end of the encoding."""
from __future__ import annotations

import json
from pathlib import Path

from opamp_client import opamp_pb2

START = Path(__file__).resolve().parents[3] / "testdata" / "sdk-settings" / "start.json"
BEFORE = "a core state saved before the core kept the config applied: the document kept before carries over"


def _varint(n: int) -> bytes:
    out = bytearray()
    while True:
        byte, n = n & 0x7F, n >> 7
        out.append(byte | (0x80 if n else 0))
        if not n:
            return bytes(out)


def core_state(document: bytes) -> bytes:
    """A client state whose config applied has `document` as its settings entry."""
    base = next(c for c in json.loads(START.read_text())["cases"] if c["name"] == BEFORE)["core_state"]
    m = opamp_pb2.AgentConfigMap()
    m.config_map["elastic"].CopyFrom(opamp_pb2.AgentConfigObject(body=document, content_type="application/json"))
    applied = opamp_pb2.AgentRemoteConfig(config=m, config_hash=b"applied").SerializeToString()
    return bytes.fromhex(base) + b"\x82\x01" + _varint(len(applied)) + applied


def keep_applied(state_dir: Path, document: bytes) -> None:
    """Leave in `state_dir` what the client would after a config with `document` was applied."""
    (state_dir / "opamp-state").write_bytes(core_state(document))
