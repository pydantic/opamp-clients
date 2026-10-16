# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Logfire SDK policies for the tests, as a server sends them (docs/sdk-settings.md)."""

from __future__ import annotations

import json

from opamp_client import opamp_pb2

FILE = "logfire-sdk-policy.json"
V1 = "logfire-sdk-policy/v1"
V2 = "logfire-sdk-policy/v2-draft"
SIGNALS = ("traces", "metrics", "logs")
V2_CONTROLS = ("sampling_rules", "trace_rate_limit", "log_levels", "log_rate_limit", "attribute_redaction", "debug",
               "app_settings", "instrumentations", "polling_interval")


def policy(version: int | None = None, **overrides: dict) -> bytes:
    """A policy with every control local but the ones given. Each one given is its override's
    fields, and signal_exports is each signal's. The policy is version 1 when it overrides only
    head_sampling and signal_exports, else version 2, unless `version` says."""
    unknown = set(overrides) - {"head_sampling", "signal_exports", *V2_CONTROLS}
    assert not unknown, unknown
    if version is None:
        version = 1 if set(overrides) <= {"head_sampling", "signal_exports"} else 2

    def control(fields: dict | None) -> dict:
        return {"mode": "local"} if fields is None else {"mode": "override", **fields}

    signals = overrides.get("signal_exports", {})
    doc = {
        "schema": V1 if version == 1 else V2,
        "head_sampling": control(overrides.get("head_sampling")),
        "signal_exports": {s: control(signals.get(s)) for s in SIGNALS},
    }
    if version == 2:
        doc.update({name: control(overrides.get(name)) for name in V2_CONTROLS})
    return json.dumps(doc, separators=(",", ":")).encode()


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
