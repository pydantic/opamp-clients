# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The settings documents in examples/settings against their README and the settings' table: the
README names each and shows it as it is, the one document with keys that are no setting shows what
an instance reports of them, and between them the documents use every setting.

The settings, and which take effect at the next start, are docs/sdk-settings.md's table.

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
# A row of the table: | `name` | value | takes effect |
ROW = re.compile(r"^\| `([a-z_]+)` \| .* \| ([^|]+) \|$")


def settings() -> tuple[set[str], set[str]]:
    """The settings of docs/sdk-settings.md's table: (live, next start)."""
    lines = TABLE.read_text().splitlines()
    start = lines.index("| Setting | Value | Takes effect |")
    live, next_start = set(), set()
    for line in lines[start + 2:]:
        if not line.startswith("|"):
            break
        m = ROW.match(line)
        if m is None:
            raise AssertionError(f"{TABLE.name}: a row of the settings' table that doesn't read: {line}")
        (next_start if m.group(2).strip() == "at the next start" else live).add(m.group(1))
    return live, next_start


def examples() -> list[tuple[str, str]]:
    """The documents in examples/settings, by file name, as written."""
    return sorted((p.name, p.read_text()) for p in COOKBOOK.glob("*.json"))


def set_aside(doc: dict, known: set[str]) -> str | None:
    """What an instance that carries out every setting reports of the document's other keys (its
    effective config's `elastic.set_aside` entry), or None when it sets nothing aside."""
    ignored = sorted(k for k in doc if k not in known)
    if not ignored:
        return None
    return json.dumps({"ignored": ignored, "unsupported": []}, separators=(",", ":"))


class SettingsCookbook(unittest.TestCase):
    def setUp(self) -> None:
        self.readme = README.read_text()
        self.live, self.next_start = settings()

    def test_the_table_reads(self) -> None:
        self.assertGreaterEqual(len(self.live), 10, sorted(self.live))
        self.assertGreaterEqual(len(self.next_start), 2, sorted(self.next_start))
        self.assertIn("sampling_rate", self.live)
        self.assertIn("opamp_polling_interval", self.next_start)

    def test_the_readme_names_and_shows_each_document(self) -> None:
        docs = examples()
        self.assertGreaterEqual(len(docs), 8, f"{COOKBOOK} has {len(docs)} documents")
        for name, text in docs:
            with self.subTest(name):
                self.assertIsInstance(json.loads(text), dict, f"{name} is no JSON object")
                self.assertTrue(f"`{name}`" in self.readme, f"the README doesn't name {name}")
                self.assertTrue(f"```json\n{text}```" in self.readme, f"the README doesn't show {name} as it is")

    def test_one_document_shows_what_is_set_aside(self) -> None:
        shown = []
        for name, text in examples():
            names = set_aside(json.loads(text), self.live | self.next_start)
            if names is not None:
                self.assertTrue(f"```json\n{names}\n```" in self.readme, f"{name}: the README doesn't show {names}")
                shown.append(name)
        self.assertEqual(len(shown), 1, f"one example shows what is set aside, not {shown}")

    def test_the_documents_use_every_setting(self) -> None:
        used = {k for _, text in examples() for k in json.loads(text)}
        for key in sorted(self.live | self.next_start):
            with self.subTest(key):
                self.assertIn(key, used, f"no example document uses {key}")


if __name__ == "__main__":
    unittest.main()
