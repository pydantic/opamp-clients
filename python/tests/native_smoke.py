# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Smoke test of an installed wheel (scripts/native_dist.py smoke): the native core loads, starts a
session and answers as it should, and so does the SDK settings layer. Not a pytest module: it runs
in a bare interpreter in each platform's container, with nothing but the wheel installed."""

import platform
import sys

from opamp_client import CoreError
from opamp_client._core import Core
from opamp_client._proto.opamp.core.v1 import core_pb2 as corepb
from opamp_client.sdk import SdkSettings

uid = bytes([7] * 16)
core = Core(corepb.Config(instance_uid=uid, endpoint="http://server/v1/opamp"))
out = core.step(0, corepb.Input(start=corepb.Empty()))
assert out.instance_uid == uid, out
assert out.effects[0].WhichOneof("kind") == "http_request", out
try:
    Core(corepb.Config(instance_uid=b"\x01\x02"))
    raise AssertionError("a 2-byte UID was taken")
except CoreError:
    pass
SdkSettings().describe([])
gil = sys._is_gil_enabled() if hasattr(sys, "_is_gil_enabled") else True
print(f"ok: {platform.python_implementation()} {platform.python_version()} on {platform.machine()}, GIL {'on' if gil else 'off'}")
