# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""Tests for collector_release.py: the pins it finds, the disagreements it refuses, and the newest
contrib tag it reads. These tests need no network.

    python3 -m unittest scripts/test_collector_release.py
"""
from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import collector_release as cr  # noqa: E402

GO_MOD = """module example.com/ours

require (
	github.com/open-telemetry/opentelemetry-collector-contrib/pkg/status {unstable}
	go.opentelemetry.io/collector/component {stable}
	go.opentelemetry.io/collector/extension {unstable}
	go.opentelemetry.io/otel v1.40.0
)
"""
MANIFEST = """extensions:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver {unstable}
providers:
  - gomod: go.opentelemetry.io/collector/confmap/provider/envprovider {stable}
"""
BUILD = "go run go.opentelemetry.io/collector/cmd/builder@{builder} --config collector.yaml\n"


def tree(root: Path, unstable: str = "v0.162.0", stable: str = "v1.68.0", builder: str = "v0.162.0") -> Path:
    (root / "go/ext").mkdir(parents=True)
    (root / ".github/workflows").mkdir(parents=True)
    (root / "go/ext/go.mod").write_text(GO_MOD.format(unstable=unstable, stable=stable))
    (root / "go/ext/collector.yaml").write_text(MANIFEST.format(unstable="v0.162.0", stable="v1.68.0"))
    (root / ".github/workflows/ci.yml").write_text(BUILD.format(builder=builder))
    return root


class Pins(unittest.TestCase):
    def test_one_release_everywhere(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            found = cr.pins(tree(Path(tmp)))
        self.assertEqual(set(found["0.x"]), {"v0.162.0"})
        self.assertEqual(set(found["1.x"]), {"v1.68.0"})
        self.assertEqual(len(found["0.x"]["v0.162.0"]), 4, "go.mod twice, the manifest, the builder")
        self.assertEqual(cr.base(found), "v0.162.0")

    def test_other_modules_are_not_pins(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            found = cr.pins(tree(Path(tmp)))
        self.assertNotIn("v1.40.0", found["1.x"], "go.opentelemetry.io/otel is not the collector")

    def test_a_go_mod_on_another_release_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            found = cr.pins(tree(Path(tmp), unstable="v0.163.0"))
        with self.assertRaises(SystemExit) as refused:
            cr.base(found)
        self.assertIn("v0.163.0: go/ext/go.mod:4", str(refused.exception))

    def test_a_builder_on_another_release_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            found = cr.pins(tree(Path(tmp), builder="v0.161.0"))
        with self.assertRaisesRegex(SystemExit, r"v0\.161\.0: \.github/workflows/ci\.yml:1"):
            cr.base(found)

    def test_a_stable_module_on_another_release_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            found = cr.pins(tree(Path(tmp), stable="v1.69.0"))
        with self.assertRaisesRegex(SystemExit, "1.x modules are pinned at 2 releases"):
            cr.base(found)

    def test_no_pin_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            found = cr.pins(Path(tmp))
        with self.assertRaisesRegex(SystemExit, "no pin"):
            cr.base(found)


class Newest(unittest.TestCase):
    def test_the_highest_release_by_number(self) -> None:
        tags = "\n".join(f"{i:040x}\trefs/tags/{tag}" for i, tag in enumerate(
            ["v0.99.0", "v0.162.0", "v0.163.0", "v0.163.0-rc1", "v0.9.0", "v0.100.1"]))
        self.assertEqual(cr.newest(tags), "v0.163.0")

    def test_no_release_is_refused(self) -> None:
        with self.assertRaises(SystemExit):
            cr.newest("0000\trefs/tags/cmd/builder/v0.1.0\n")


if __name__ == "__main__":
    unittest.main()
