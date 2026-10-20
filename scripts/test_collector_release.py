# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
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


THEIRS_MODULE = "example.com/upstream/sup"
OURS_MODULE = "example.com/ours/sup"


def write(directory: Path, files: dict[str, str]) -> None:
    for name, text in files.items():
        (directory / name).parent.mkdir(parents=True, exist_ok=True)
        (directory / name).write_text(text)


class Forks(unittest.TestCase):
    """diverged and port, on a fork of one upstream directory."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        base = Path(self.tmp.name)
        self.root, self.upstream, self.old = base / "root", base / "upstream", base / "old"
        forks = {"cmd/sup": ("go/sup", THEIRS_MODULE, OURS_MODULE)}
        for name, value in (("FORKS", forks), ("DROPPED", {"cmd/sup/Makefile": "contrib's build"})):
            original = getattr(cr, name)
            setattr(cr, name, value)
            self.addCleanup(setattr, cr, name, original)

    def upstream_at(self, directory: Path, files: dict[str, str]) -> None:
        write(directory / "cmd/sup", files)

    def ours(self, files: dict[str, str]) -> None:
        write(self.root / "go/sup", files)

    def test_a_copy_with_the_module_rewritten_is_upstreams(self) -> None:
        self.upstream_at(self.upstream, {"a.go": f"// upstream\npackage a\n\nimport \"{THEIRS_MODULE}/b\"\n",
                                         "Makefile": "all:\n"})
        self.ours({"a.go": f"// upstream, our header\npackage a\n\nimport \"{OURS_MODULE}/b\"\n", "own.go": "package a\n"})
        report, problems = cr.diverged(self.root, self.upstream)
        self.assertEqual(problems, [])
        self.assertIn("1 files as upstream's, 0 changed (0 lines), 1 not carried, 1 of its own", report[0])

    def test_a_change_its_header_doesnt_name_is_refused(self) -> None:
        self.upstream_at(self.upstream, {"a.go": "package a\n\nfunc A() {}\n", "a_test.go": "package a\n"})
        self.ours({"a.go": "package a\n\nfunc A() { hook() }\n", "a_test.go": "package a\n\nfunc TestA() {}\n"})
        _, problems = cr.diverged(self.root, self.upstream)
        self.assertEqual(len(problems), 1)
        self.assertIn("go/sup/a.go differs", problems[0])
        self.ours({"a.go": "// Changed here: A calls hook.\npackage a\n\nfunc A() { hook() }\n"})
        self.assertEqual(cr.diverged(self.root, self.upstream)[1], [])

    def test_a_changed_build_constraint_is_a_change(self) -> None:
        self.upstream_at(self.upstream, {"a.go": "// upstream\n//go:build linux\n\npackage a\n"})
        self.ours({"a.go": "// upstream\n//go:build linux || darwin\n\npackage a\n"})
        _, problems = cr.diverged(self.root, self.upstream)
        self.assertEqual(len(problems), 1)
        self.assertIn("go/sup/a.go differs", problems[0])

    def test_a_missing_upstream_file_needs_a_reason(self) -> None:
        self.upstream_at(self.upstream, {"a.go": "package a\n", "Makefile": "all:\n", "packages.go": "package a\n"})
        self.ours({"a.go": "package a\n"})
        _, problems = cr.diverged(self.root, self.upstream)
        self.assertEqual(problems, ["upstream's cmd/sup/packages.go is missing from go/sup, and DROPPED doesn't say why"])

    def test_port_merges_upstreams_change_into_ours(self) -> None:
        self.upstream_at(self.old, {"a.go": "package a\n\nfunc A() {}\n\nfunc B() {}\n", "gone.go": "package a\n"})
        self.upstream_at(self.upstream, {"a.go": "package a\n\nfunc A() {}\n\nfunc B() { fixed() }\n",
                                         "new.go": f"package a\n\nimport \"{THEIRS_MODULE}/c\"\n"})
        self.ours({"a.go": "// Changed here: A calls hook.\npackage a\n\nfunc A() { hook() }\n\nfunc B() {}\n",
                   "gone.go": "package a\n"})
        out = cr.port(self.root, self.old, self.upstream, "v0.1.0", "v0.2.0")
        self.assertEqual(out, ["merged: go/sup/a.go", "deleted upstream: go/sup/gone.go", "added: go/sup/new.go"])
        self.assertEqual((self.root / "go/sup/a.go").read_text(),
                         "// Changed here: A calls hook.\npackage a\n\nfunc A() { hook() }\n\nfunc B() { fixed() }\n")
        self.assertIn(OURS_MODULE, (self.root / "go/sup/new.go").read_text())

    def test_port_leaves_a_conflict_where_both_changed_a_line(self) -> None:
        self.upstream_at(self.old, {"a.go": "package a\n\nfunc A() {}\n"})
        self.upstream_at(self.upstream, {"a.go": "package a\n\nfunc A() { fixed() }\n"})
        self.ours({"a.go": "package a\n\nfunc A() { hook() }\n"})
        self.assertEqual(cr.port(self.root, self.old, self.upstream, "v0.1.0", "v0.2.0"), ["CONFLICT: go/sup/a.go"])
        merged = (self.root / "go/sup/a.go").read_text()
        self.assertIn("<<<<<<< ours", merged)
        self.assertIn(">>>>>>> v0.2.0", merged)



if __name__ == "__main__":
    unittest.main()
