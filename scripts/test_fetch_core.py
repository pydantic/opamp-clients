# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Tests for fetch_core.py, on a release from a directory laid out as the core's release is: lock,
fetch and check agree; a file that isn't what the lock or the manifest says is refused, and so is
one that would land anywhere but its place; and the settings cookbook's policies go to
examples/settings/ as the tree has them.

    python3 -m unittest scripts/test_fetch_core.py
"""
from __future__ import annotations

import contextlib
import gzip
import hashlib
import io
import json
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import fetch_core as fc  # noqa: E402

# The tree's files of the release in these tests, each at its path.
TREE = [*fc.LICENSES, "proto/opamp/core/v1/core.proto", "spec/proto/opamp/v1/opamp.proto", "spec/shell-contract.md",
        "tests/shell-tables/urls.json", "testdata/sdk-settings/caps.json",
        "testdata/settings-examples/quiet-logs.json", "testdata/settings-examples/traces-only.json"]
LIBS = [("linux-x64", "libopamp_native.so"), ("win-x64", "opamp_native.dll")]


def module(name: str) -> bytes:
    return b"\0asm\x01\0\0\0" + name.encode()


def tree_tar(files: dict[str, bytes]) -> bytes:
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w") as tar:
        for name, data in sorted(files.items()):
            info = tarfile.TarInfo(name)
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
    return gzip.compress(raw.getvalue())


def release(dist: Path, tree: dict[str, bytes], version: str = "0.1.0") -> None:
    """dist as the core's release has it: each module, the tree, opamp-tap, a library per LIBS, and
    the manifest naming them."""
    dist.mkdir(parents=True, exist_ok=True)
    files = []

    def put(name: str, data: bytes, kind: str, **extra: str) -> None:
        (dist / name).write_bytes(data)
        files.append({"name": name, "kind": kind, "sha256": hashlib.sha256(data).hexdigest(), "size": len(data), **extra})

    for name in fc.MODULES:
        put(name, module(name), "wasm")
    put(fc.TREE_TAR, tree_tar(tree), "tree")
    put("tool-linux-x64-opamp-tap", b"opamp-tap", "tool", target="linux-x64", bin="opamp-tap")
    for rid, lib in LIBS:
        put(f"native-{rid}-{lib}", f"lib {rid}".encode(), "native", target=rid, lib=lib)
    manifest = {"schema": fc.SCHEMA, "version": version, "commit": "c0ffee" * 6 + "c0ff",
                "files": sorted(files, key=lambda f: f["name"])}
    (dist / fc.MANIFEST).write_text(json.dumps(manifest, indent=2) + "\n")


class Base(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.clients, self.dist = Path(tmp.name) / "opamp-clients", Path(tmp.name) / "dist"
        self.clients.mkdir()
        self.tree = {rel: f"// {rel}\n".encode() for rel in TREE}
        release(self.dist, self.tree)
        old, fc.ROOT = fc.ROOT, self.clients
        self.addCleanup(setattr, fc, "ROOT", old)

    def quiet(self):  # noqa: ANN201
        return contextlib.redirect_stdout(io.StringIO())

    def fetched(self, all_platforms: bool = False, rid: str = "linux-x64") -> dict:
        pinned = fc.lock("0.1.0", fc.SOURCE, self.dist)
        with self.quiet():
            fc.fetch(pinned, self.dist, all_platforms=all_platforms, rid=rid)
        return pinned


class Fetch(Base):
    def test_lock_fetch_check_round_trip(self) -> None:
        pinned = fc.lock("0.1.0", fc.SOURCE, self.dist)
        self.assertEqual(set(pinned["files"]), {f["name"] for f in json.loads((self.dist / fc.MANIFEST).read_text())["files"]})
        self.assertEqual(set(pinned["tree"]), set(TREE))
        with self.quiet():
            fc.fetch(pinned, self.dist, all_platforms=True, rid="linux-x64")
        self.assertEqual(fc.check(pinned, "linux-x64"), [])
        # Every module where the clients load it, the same bytes as the release's.
        for name, places in fc.MODULES.items():
            for c in places:
                self.assertEqual((self.clients / c).read_bytes(), module(name))
        for rel, data in self.tree.items():
            self.assertEqual((self.clients / rel).read_bytes(), data)
        tap = self.clients / "target/release/opamp-tap"
        self.assertEqual(tap.read_bytes(), b"opamp-tap")
        self.assertTrue(tap.stat().st_mode & 0o100)
        self.assertEqual((self.clients / "target/release/libopamp_native.so").read_bytes(), b"lib linux-x64")
        self.assertEqual((self.clients / "dist/native/runtimes/win-x64/native/opamp_native.dll").read_bytes(),
                         b"lib win-x64")
        self.assertFalse((self.clients / "target/release/opamp_native.dll").exists())

    def test_fetch_removes_what_an_earlier_release_had_and_this_one_hasnt(self) -> None:
        pinned = self.fetched()
        old = self.clients / "tests/shell-tables/renamed.json"
        old.write_text("{}\n")
        self.assertEqual(fc.check(pinned, "linux-x64"), ["tests/shell-tables/renamed.json: not in 0.1.0 (fetch removes it)"])
        with self.quiet():
            fc.fetch(pinned, self.dist, all_platforms=False, rid="linux-x64")
        self.assertFalse(old.exists())
        self.assertEqual(fc.check(pinned, "linux-x64"), [])

    def test_the_machines_library_only_without_all_platforms(self) -> None:
        self.fetched(rid="win-x64")
        self.assertEqual((self.clients / "target/release/opamp_native.dll").read_bytes(), b"lib win-x64")
        self.assertFalse((self.clients / "dist").exists())

    def test_fetch_refuses_a_file_that_changed_after_the_lock(self) -> None:
        pinned = fc.lock("0.1.0", fc.SOURCE, self.dist)
        (self.dist / "opamp_core.wasm").write_bytes(b"\0asm evil")
        with self.quiet(), self.assertRaisesRegex(fc.Refused, "opamp_core.wasm: sha256 .* pins"):
            fc.fetch(pinned, self.dist, all_platforms=False, rid="linux-x64")
        self.assertFalse((self.clients / "go/opamp/internal/core/opamp_core.wasm").exists())

    def test_fetch_refuses_a_lock_naming_a_path(self) -> None:
        pinned = fc.lock("0.1.0", fc.SOURCE, self.dist)
        pinned["files"]["../evil"] = {"sha256": "0" * 64, "kind": "wasm"}
        with self.quiet(), self.assertRaisesRegex(fc.Refused, "not a plain file name"):
            fc.fetch(pinned, self.dist, all_platforms=False, rid="linux-x64")


class Lock(Base):
    def test_lock_refuses_a_file_that_isnt_what_the_manifest_says(self) -> None:
        (self.dist / "native-linux-x64-libopamp_native.so").write_bytes(b"lib swapped")
        with self.assertRaisesRegex(fc.Refused, "native-linux-x64-libopamp_native.so: sha256"):
            fc.lock("0.1.0", fc.SOURCE, self.dist)

    def test_lock_refuses_another_version(self) -> None:
        with self.assertRaisesRegex(fc.Refused, "version 0.1.0, not 0.2.0"):
            fc.lock("0.2.0", fc.SOURCE, self.dist)

    def test_lock_refuses_a_native_library_that_would_land_elsewhere(self) -> None:
        self.edit_manifest(lambda f: f.update(lib="../../../.bashrc") if f.get("target") == "win-x64" else None)
        with self.assertRaisesRegex(fc.Refused, "isn't one this script places"):
            fc.lock("0.1.0", fc.SOURCE, self.dist)

    def test_lock_refuses_a_tool_it_doesnt_place(self) -> None:
        self.edit_manifest(lambda f: f.update(bin="../../.bashrc") if f["kind"] == "tool" else None)
        with self.assertRaisesRegex(fc.Refused, "isn't one this script places"):
            fc.lock("0.1.0", fc.SOURCE, self.dist)

    def test_lock_refuses_a_tree_without_the_cores_license(self) -> None:
        release(self.dist, {k: v for k, v in self.tree.items() if k != fc.LICENSES[1]})
        with self.assertRaisesRegex(fc.Refused, f"no {fc.LICENSES[1]} in 0.1.0"):
            fc.lock("0.1.0", fc.SOURCE, self.dist)

    def test_a_tree_tar_naming_a_file_outside_its_places_is_refused(self) -> None:
        for name in ("../../etc/evil.proto", "proto/../../evil", ".github/workflows/x.yml", "spec/shell-contract.md/x",
                     "spec/proto/..\\..\\outside", "proto/C:/evil", "examples/settings/quiet-logs.json"):
            with self.subTest(name), self.assertRaisesRegex(fc.Refused, "isn't a file this script places"):
                fc.tree(tree_tar({name: b"x"}))

    def edit_manifest(self, change) -> None:  # noqa: ANN001
        manifest = json.loads((self.dist / fc.MANIFEST).read_text())
        for f in manifest["files"]:
            change(f)
        (self.dist / fc.MANIFEST).write_text(json.dumps(manifest))


class Check(Base):
    def test_check_finds_a_module_changed_in_the_tree(self) -> None:
        pinned = self.fetched()
        (self.clients / "java/opamp/src/main/wasm/opamp_sdk_settings.wasm").write_bytes(b"\0asm edited")
        self.assertEqual(fc.check(pinned, "linux-x64"), ["java/opamp/src/main/wasm/opamp_sdk_settings.wasm: "
                                                         "not opamp_sdk_settings.wasm of 0.1.0"])

    def test_check_finds_a_native_library_changed_after_fetch(self) -> None:
        pinned = self.fetched(all_platforms=True)
        (self.clients / "target/release/libopamp_native.so").write_bytes(b"lib stale")
        (self.clients / "dist/native/runtimes/win-x64/native/opamp_native.dll").write_bytes(b"lib stale")
        self.assertEqual(sorted(fc.check(pinned, "linux-x64")), [
            "dist/native/runtimes/win-x64/native/opamp_native.dll: not native-win-x64-opamp_native.dll of 0.1.0",
            "target/release/libopamp_native.so: not native-linux-x64-libopamp_native.so of 0.1.0",
        ])

    def test_check_finds_a_tool_that_lost_its_execute_bit(self) -> None:
        pinned = self.fetched()
        (self.clients / "target/release/opamp-tap").chmod(0o644)
        self.assertEqual(fc.check(pinned, "linux-x64"), ["target/release/opamp-tap: not executable"])


class Cookbook(Base):
    def test_the_policies_are_checked_in_beside_the_readme_as_the_tree_has_them(self) -> None:
        readme = self.clients / "examples/settings/README.md"
        readme.parent.mkdir(parents=True)
        readme.write_text("# the cookbook\n")
        pinned = self.fetched()
        for name in ("quiet-logs.json", "traces-only.json"):
            self.assertEqual((self.clients / "examples/settings" / name).read_bytes(),
                             self.tree[f"testdata/settings-examples/{name}"])
        self.assertEqual(readme.read_text(), "# the cookbook\n")
        self.assertEqual(fc.check(pinned, "linux-x64"), [])

    def test_check_finds_a_policy_edited_here_and_fetch_puts_it_back(self) -> None:
        pinned = self.fetched()
        policy = self.clients / "examples/settings/quiet-logs.json"
        policy.write_text("{}\n")
        self.assertEqual(fc.check(pinned, "linux-x64"),
                         ["examples/settings/quiet-logs.json: not testdata/settings-examples/quiet-logs.json of 0.1.0"])
        with self.quiet():
            fc.fetch(pinned, self.dist, all_platforms=False, rid="linux-x64")
        self.assertEqual(fc.check(pinned, "linux-x64"), [])

    def test_a_policy_the_tree_hasnt_is_found_and_removed(self) -> None:
        pinned = self.fetched()
        extra = self.clients / "examples/settings/made-up-here.json"
        extra.write_text("{}\n")
        self.assertEqual(fc.check(pinned, "linux-x64"), ["examples/settings/made-up-here.json: not in 0.1.0 (fetch removes it)"])
        with self.quiet():
            fc.fetch(pinned, self.dist, all_platforms=False, rid="linux-x64")
        self.assertFalse(extra.exists())

    def test_a_release_without_the_cookbook_leaves_the_directory_alone(self) -> None:
        release(self.dist, {k: v for k, v in self.tree.items() if not k.startswith(fc.COOKBOOK)})
        mine = self.clients / "examples/settings/quiet-logs.json"
        mine.parent.mkdir(parents=True)
        mine.write_text("{}\n")
        pinned = self.fetched()
        self.assertEqual(mine.read_text(), "{}\n")
        self.assertEqual(fc.check(pinned, "linux-x64"), [])


if __name__ == "__main__":
    unittest.main()
