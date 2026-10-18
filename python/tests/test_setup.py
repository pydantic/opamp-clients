# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The core's Setup call, through the native library: off without an endpoint; the endpoint, the
headers and the heartbeat as it reads them; the instance UID saved, or made and to be saved."""

import pytest

from pydantic_opamp_client import CoreError, setup


def test_setup_is_the_cores_call():
    assert setup.start(setup.SetupStart()).off
    s = setup.start(setup.SetupStart(env=[
        setup.Variable(name="OTEL_OPAMP_ENDPOINT", value="https://user:pw@opamp.example.com/v1/opamp?k=v"),
        setup.Variable(name="OTEL_OPAMP_HEADERS", value="Authorization=Bearer%20k"),
        setup.Variable(name="OTEL_OPAMP_HEARTBEAT_INTERVAL", value="30s"),
        setup.Variable(name="OTEL_OPAMP_STATE_DIR", value="none"),
    ]))
    assert not s.off and s.shown_endpoint == "https://opamp.example.com/v1/opamp"
    assert [(h.name, h.value) for h in s.headers] == [("Authorization", "Bearer k")]
    assert (s.heartbeat_interval_ms, s.state_dir, list(s.default_slots)) == (30_000, "", [])

    saved = "0199e3a0-0000-7000-8000-000000000007"
    got = setup.identify(setup.SetupIdentity(state_dir=True, saved=saved + "\n", entropy=bytes(16)))
    assert getattr(got, "from") == setup.UID_SAVED and not got.save and got.text == saved
    made = setup.identify(setup.SetupIdentity(state_dir=True, entropy=bytes(16), unix_ms=1))
    assert getattr(made, "from") == setup.UID_MADE and made.save and len(made.instance_uid) == 16
    with pytest.raises(CoreError):
        setup.identify(setup.SetupIdentity())  # no entropy: the core refuses the call
