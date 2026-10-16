# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Logfire SDK policies for the tests, as a server sends them (docs/sdk-settings.md)."""

from __future__ import annotations

import json

from opamp_client import opamp_pb2

FILE = "logfire-sdk-policy.json"
V1 = "logfire-sdk-policy/v1"
SIGNALS = ("traces", "metrics", "logs")
# The controls a policy may leave out (as local).
MORE_CONTROLS = ("sampling_rules", "trace_rate_limit", "log_levels", "log_rate_limit", "attribute_redaction", "debug",
               "app_settings", "instrumentations", "polling_interval")


def policy(every: bool | None = None, **overrides: dict) -> str:
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
    return json.dumps(doc, separators=(",", ":"))



def full(body: str | bytes) -> dict:
    """A policy, parsed as the effective config reports it: every control, those left out local."""
    doc = json.loads(body)
    return doc | {name: doc.get(name, {"mode": "local"}) for name in MORE_CONTROLS}

def config_map(body: str | bytes | None) -> opamp_pb2.AgentConfigMap:
    """A remote config's files: the policy `body` alone, or none."""
    m = opamp_pb2.AgentConfigMap()
    if body is not None:
        m.config_map[FILE].CopyFrom(opamp_pb2.AgentConfigObject(
            body=body.encode() if isinstance(body, str) else body, content_type="application/json"))
    return m


def reported(e: opamp_pb2.EffectiveConfig) -> dict:
    """The policy an effective config reports, parsed. It is the effective config's only file."""
    files = e.config_map.config_map
    assert list(files) == [FILE], list(files)
    return json.loads(files[FILE].body)


def overrides(e: opamp_pb2.EffectiveConfig) -> dict:
    """The controls an effective config reports overridden, each with its fields but the mode.
    signal_exports is each signal's."""
    return overrides_in(reported(e))


def overrides_in(doc: dict) -> dict:
    """The controls a policy (parsed) overrides, each with its fields but the mode. signal_exports
    is each signal's."""
    out = {}
    for name, value in doc.items():
        if name == "schema":
            continue
        if name == "signal_exports":
            signals = {s: {k: v for k, v in c.items() if k != "mode"} for s, c in value.items()
                       if c["mode"] == "override"}
            if signals:
                out[name] = signals
        elif value["mode"] == "override":
            out[name] = {k: v for k, v in value.items() if k != "mode"}
    return out


def off(*signals: str) -> dict:
    """signal_exports's overrides that stop each signal named."""
    return {s: {"enabled": False} for s in signals}
