# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

import os

import pytest
from hypothesis import settings

# Hypothesis loads its "ci" profile on a CI runner, which derandomizes: every run draws the same
# examples, so a nightly's thousands would be the same thousands every night. A scheduled or manual
# run (or OPAMP_EXPLORE=1) draws new ones; a pull request's stay fixed, so its result is its code's.
# A failure prints the blob that replays it (@reproduce_failure).
if os.environ.get("GITHUB_EVENT_NAME") in ("schedule", "workflow_dispatch") or os.environ.get("OPAMP_EXPLORE"):
    settings.register_profile("explore", settings.get_profile("ci"), derandomize=False)
    settings.load_profile("explore")


def pytest_report_header() -> str:
    return f"hypothesis: derandomize={settings.default.derandomize}"


@pytest.fixture(autouse=True)
def no_environment_proxies(monkeypatch) -> None:
    """Tests start with no proxy in the environment (the client would use it): a test that wants
    one sets it."""
    for name in list(os.environ):
        if name.lower().endswith("_proxy"):
            monkeypatch.delenv(name)
