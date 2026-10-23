# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Tests for deps_scan.py: an ignore needs an id, a reason and an end at most IGNORE_DAYS away; the
repository's own osv-scanner.toml keeps to it; each platform we build on has a pinned binary.

    python3 -m unittest scripts/test_deps_scan.py
"""
from __future__ import annotations

import datetime as dt
import sys
import tomllib
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import deps_scan as ds  # noqa: E402

TODAY = dt.date(2026, 10, 23)


def ignore(**kw) -> dict:
    entry = {"id": "GHSA-xxxx-xxxx-xxxx", "reason": "no fixed release yet", "ignoreUntil": TODAY + dt.timedelta(days=30)}
    entry.update(kw)
    return {"IgnoredVulns": [{k: v for k, v in entry.items() if v is not None}]}


class Ignores(unittest.TestCase):
    def test_a_full_entry_passes(self):
        self.assertEqual(ds.problems(ignore(), TODAY), [])

    def test_no_ignores_passes(self):
        self.assertEqual(ds.problems({}, TODAY), [])

    def test_an_expired_entry_passes(self):
        # OSV-Scanner reports the advisory again; nothing to say here.
        self.assertEqual(ds.problems(ignore(ignoreUntil=TODAY - dt.timedelta(days=1)), TODAY), [])

    def test_no_reason_fails(self):
        self.assertEqual(len(ds.problems(ignore(reason="  "), TODAY)), 1)
        self.assertIn("no reason", ds.problems(ignore(reason=None), TODAY)[0])

    def test_no_end_fails(self):
        self.assertIn("no ignoreUntil", ds.problems(ignore(ignoreUntil=None), TODAY)[0])

    def test_an_end_too_far_away_fails(self):
        far = TODAY + dt.timedelta(days=ds.IGNORE_DAYS + 1)
        self.assertIn("more than", ds.problems(ignore(ignoreUntil=far), TODAY)[0])
        self.assertEqual(ds.problems(ignore(ignoreUntil=TODAY + dt.timedelta(days=ds.IGNORE_DAYS)), TODAY), [])

    def test_a_datetime_end_counts_as_its_date(self):
        until = dt.datetime(2026, 11, 1, 12, 0)
        self.assertEqual(ds.problems(ignore(ignoreUntil=until), TODAY), [])

    def test_no_id_fails(self):
        self.assertIn("no id", ds.problems(ignore(id=None), TODAY)[0])

    def test_the_repositorys_config_keeps_to_it(self):
        self.assertEqual(ds.problems(tomllib.loads(ds.CONFIG.read_text()), dt.date.today()), [])


class Platforms(unittest.TestCase):
    def test_linux_and_macos_have_a_pinned_binary(self):
        self.assertEqual(ds.asset("Linux", "x86_64"), "linux_amd64")
        self.assertEqual(ds.asset("Linux", "aarch64"), "linux_arm64")
        self.assertEqual(ds.asset("Darwin", "arm64"), "darwin_arm64")
        self.assertEqual(ds.asset("Darwin", "x86_64"), "darwin_amd64")

    def test_another_platform_says_so(self):
        with self.assertRaises(SystemExit):
            ds.asset("Windows", "AMD64")

    def test_every_pin_is_a_sha256(self):
        for name, digest in ds.SHA256.items():
            self.assertRegex(digest, r"^[0-9a-f]{64}$", name)


if __name__ == "__main__":
    unittest.main()
