# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Tests for redact_mutants.py: the mutants as written, and how one is put in.

    python3 -m unittest scripts/test_redact_mutants.py
"""
from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import redact_mutants as rm  # noqa: E402


class Mutants(unittest.TestCase):
    def test_every_mutant_applies_once_and_names_a_test(self):
        found = rm.mutants()
        self.assertEqual(rm.check(found), [])
        # Every noninterference test has mutants to catch: that of each SDK whose own options types
        # print what they hold (docs/sdk-contract.md, "Secrets").
        self.assertEqual({m["test"] for m in found}, set(rm.TESTS))
        self.assertEqual(set(rm.TESTS), {"python", "go", "dotnet"})

    def test_a_find_that_is_not_there_once_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "f.py").write_text("a a b")
            with self.assertRaisesRegex(ValueError, "matches 2 times"):
                rm.apply({"name": "m", "file": "f.py", "find": "a", "replace": "c"}, root)
            with self.assertRaisesRegex(ValueError, "matches 0 times"):
                rm.apply({"name": "m", "file": "f.py", "find": "z", "replace": "c"}, root)
            rm.apply({"name": "m", "file": "f.py", "find": "b", "replace": "c"}, root)
            self.assertEqual((root / "f.py").read_text(), "a a c")

    def test_a_mistake_in_the_list_is_named(self):
        bad = [{"name": "m", "file": "nowhere.py", "test": "ruby", "find": "x", "replace": "x"}]
        problems = rm.check(bad)
        self.assertTrue(any("not one of" in p for p in problems), problems)
        self.assertTrue(any("`replace` is `find`" in p for p in problems), problems)
        self.assertTrue(any("no file" in p for p in problems), problems)
        self.assertTrue(any("named twice" in p for p in rm.check(bad * 2)))


if __name__ == "__main__":
    unittest.main()
