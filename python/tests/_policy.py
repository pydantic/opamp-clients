# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Logfire SDK policies for the tests, as a server sends them (docs/sdk-settings.md)."""

from __future__ import annotations

import json

from pydantic_opamp_client import opamp_pb2

FILE = "logfire-sdk-policy.json"
V1 = "logfire-sdk-policy/v1"
SIGNALS = ("traces", "metrics", "logs")
# The controls a policy may leave out (as local).
MORE_CONTROLS = ("sampling_rules", "trace_rate_limit", "log_levels", "log_rate_limit", "attribute_redaction", "debug",
               "app_settings", "instrumentations")


def policy(every: bool | None = None, **overrides: dict) -> bytes:
    """A policy with every control local but the ones given. Each one given is its override's
    fields, and signal_exports is each signal's. The policy is the first four controls alone when it
    overrides only head_sampling and signal_exports, else every control, unless `every` says."""
    unknown = set(overrides) - {"head_sampling", "signal_exports", *MORE_CONTROLS}
    assert not unknown, unknown
    if every is None:
        every = not set(overrides) <= {"head_sampling", "signal_exports"}

    def control(fields: dict | None) -> dict:
        return {"mode": "local"} if fields is None else {"mode": "override", **fields}

    signals = overrides.get("signal_exports", {})
    doc = {
        "schema": V1,
        "head_sampling": control(overrides.get("head_sampling")),
        "signal_exports": {s: control(signals.get(s)) for s in SIGNALS},
    }
    if every:
        doc.update({name: control(overrides.get(name)) for name in MORE_CONTROLS})
    return json.dumps(doc, separators=(",", ":")).encode()



def full(body: str | bytes) -> dict:
    """A policy, parsed as the effective config reports it: every control, those left out local."""
    doc = json.loads(body)
    return doc | {name: doc.get(name, {"mode": "local"}) for name in MORE_CONTROLS}

def config(body: bytes, config_hash: bytes = b"h") -> opamp_pb2.AgentRemoteConfig:
    """A remote config whose only file is the policy `body`."""
    return opamp_pb2.AgentRemoteConfig(config_hash=config_hash, config=opamp_pb2.AgentConfigMap(
        config_map={FILE: opamp_pb2.AgentConfigObject(body=body, content_type="application/json")}))


def effective(e: opamp_pb2.EffectiveConfig) -> dict:
    """The policy an effective config reports, parsed. It is the effective config's only file."""
    files = e.config_map.config_map
    assert list(files) == [FILE], list(files)
    assert files[FILE].content_type == "application/json"
    return json.loads(files[FILE].body)
