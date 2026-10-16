# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""The policies in examples/settings against their README and the controls' table: each file is a
whole Logfire SDK policy (version 1 where it overrides only version 1 controls), the README names
each and shows it as it is, and between them the files override every control.

The controls, and which take effect at the next start, are docs/sdk-settings.md's table.

    python3 -m unittest examples/test_settings_cookbook.py
"""
from __future__ import annotations

import json
import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
COOKBOOK = ROOT / "examples" / "settings"
README = COOKBOOK / "README.md"
TABLE = ROOT / "docs" / "sdk-settings.md"
# A row of the table: | `control` | override fields | takes effect |
ROW = re.compile(r"^\| `([a-z_.]+)` \| .* \| ([^|]+) \|$")

V1 = "logfire-sdk-policy/v1"
V2 = "logfire-sdk-policy/v2-draft"
SIGNALS = ("traces", "metrics", "logs")
V1_CONTROLS = ("head_sampling", "signal_exports")
V2_CONTROLS = V1_CONTROLS + ("sampling_rules", "trace_rate_limit", "log_levels", "log_rate_limit",
                             "attribute_redaction", "debug", "app_settings", "instrumentations", "polling_interval")
# The controls a version 1 policy can override, a signal's export as `signal_exports.logs`.
V1_OVERRIDES = {"head_sampling", *(f"signal_exports.{s}" for s in SIGNALS)}


def controls() -> tuple[set[str], set[str]]:
    """The controls of docs/sdk-settings.md's table, a signal's export as `signal_exports.logs`:
    (at once, at the next start)."""
    lines = TABLE.read_text().splitlines()
    start = lines.index("| Control | Override fields | Takes effect |")
    live, next_start = set(), set()
    for line in lines[start + 2:]:
        if not line.startswith("|"):
            break
        m = ROW.match(line)
        if m is None:
            raise AssertionError(f"{TABLE.name}: a row of the controls' table that does not read: {line}")
        (next_start if m.group(2).strip() == "at the next start" else live).add(m.group(1))
    return live, next_start


def examples() -> list[tuple[str, str]]:
    """The policies in examples/settings, by file name, as written."""
    return sorted((p.name, p.read_text()) for p in COOKBOOK.glob("*.json"))


def overrides(policy: dict) -> set[str]:
    """The controls that a policy overrides, a signal's export as `signal_exports.logs`."""
    found = {f"signal_exports.{s}" for s, c in policy.get("signal_exports", {}).items() if c.get("mode") == "override"}
    return found | {k for k, c in policy.items() if k not in ("schema", "signal_exports") and c.get("mode") == "override"}


class SettingsCookbook(unittest.TestCase):
    def setUp(self) -> None:
        self.readme = README.read_text()
        self.live, self.next_start = controls()

    def test_the_table_reads(self) -> None:
        self.assertEqual(self.next_start, {"instrumentations", "polling_interval"})
        self.assertEqual(self.live | self.next_start, V1_OVERRIDES | set(V2_CONTROLS[2:]))

    def test_each_file_is_a_whole_policy(self) -> None:
        docs = examples()
        self.assertGreaterEqual(len(docs), 8, f"{COOKBOOK} has {len(docs)} policies")
        for name, text in docs:
            with self.subTest(name):
                policy = json.loads(text)
                self.assertIsInstance(policy, dict, f"{name} is no JSON object")
                self.assertIn(policy.get("schema"), (V1, V2), name)
                self.assertEqual(list(policy), ["schema", *(V1_CONTROLS if policy["schema"] == V1 else V2_CONTROLS)],
                                 f"{name} does not have every control of its version, in order")
                self.assertEqual(list(policy["signal_exports"]), list(SIGNALS), name)
                each = [c for k, c in policy.items() if k not in ("schema", "signal_exports")]
                for control in each + list(policy["signal_exports"].values()):
                    self.assertIn(control.get("mode"), ("local", "override"), name)
                    if control["mode"] == "local":
                        self.assertEqual(control, {"mode": "local"}, f"{name}: a local control has no fields")
                self.assertEqual(policy["schema"], V1 if overrides(policy) <= V1_OVERRIDES else V2,
                                 f"{name}: version 1 where only version 1 controls are overridden, else version 2")

    def test_the_readme_names_and_shows_each_file(self) -> None:
        for name, text in examples():
            with self.subTest(name):
                self.assertTrue(f"`{name}`" in self.readme, f"the README does not name {name}")
                self.assertTrue(f"```json\n{text}```" in self.readme, f"the README does not show {name} as it is")

    def test_the_files_override_every_control(self) -> None:
        used = set().union(*(overrides(json.loads(text)) for _, text in examples()))
        for control in sorted(self.live | self.next_start):
            with self.subTest(control):
                self.assertIn(control, used, f"no example policy overrides {control}")

    def test_one_file_puts_every_control_back_to_local(self) -> None:
        local = [name for name, text in examples() if not overrides(json.loads(text))]
        self.assertEqual(local, ["back-to-defaults.json"])
        self.assertEqual(json.loads(dict(examples())["back-to-defaults.json"])["schema"], V1)


if __name__ == "__main__":
    unittest.main()
