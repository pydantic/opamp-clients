# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
"""sdk_release.py's stamp, on copies of the real manifests, and its check, on made-up packages."""
from __future__ import annotations

import io
import json
import shutil
import sys
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import sdk_release  # noqa: E402


class Stamp(unittest.TestCase):
    def setUp(self) -> None:
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root)
        for rel in [*sdk_release.PYPROJECTS, *sdk_release.PACKAGE_JSONS, *sdk_release.POMS, *sdk_release.CSPROJS]:
            (self.root / rel).parent.mkdir(parents=True, exist_ok=True)
            shutil.copy(sdk_release.ROOT / rel, self.root / rel)
        sdk_release.stamp("7.8.9", self.root)

    def read(self, rel: str) -> str:
        return (self.root / rel).read_text()

    def test_every_manifest_is_at_the_version(self) -> None:
        for rel in sdk_release.PYPROJECTS:
            self.assertIn('\nversion = "7.8.9"\n', self.read(rel), rel)
        for rel in sdk_release.PACKAGE_JSONS:
            self.assertEqual("7.8.9", json.loads(self.read(rel))["version"], rel)
        for rel in sdk_release.POMS:
            self.assertRegex(self.read(rel), r"</artifactId>\s*<version>7\.8\.9</version>", rel)
            self.assertNotIn("SNAPSHOT", self.read(rel), rel)
        for rel in sdk_release.CSPROJS:
            self.assertIn("<Version>7.8.9</Version>", self.read(rel), rel)

    # A release never mixes versions: each of ours asks for the others at exactly this one.
    def test_ours_ask_for_each_other_exactly(self) -> None:
        self.assertIn('"pydantic-opamp-core==7.8.9"', self.read("python/pyproject.toml"))
        self.assertIn('"pydantic-opamp-client==7.8.9"', self.read("python/otel/pyproject.toml"))
        otel = json.loads(self.read("node/otel/package.json"))
        client = json.loads(self.read("node/opamp/package.json"))["name"]
        self.assertEqual("7.8.9", otel["dependencies"][client])
        self.assertRegex(self.read("java/opamp-javaagent/pom.xml"),
                         r"<artifactId>opamp-client</artifactId>\s*<version>7\.8\.9</version>")

    # The pin rewrites dependencies only: a stamped `name` is no project name at all.
    def test_names_stay_as_they_are(self) -> None:
        for rel in sdk_release.PYPROJECTS:
            self.assertRegex(self.read(rel), r'\nname = "pydantic-opamp-(core|client|otel)"\n', rel)

    def test_npm_packages_are_publishable(self) -> None:
        for rel in sdk_release.PACKAGE_JSONS:
            self.assertNotIn("private", json.loads(self.read(rel)), rel)

    def test_refuses_a_prerelease(self) -> None:
        with self.assertRaises(SystemExit):
            sdk_release.stamp("1.0.0-rc.1", self.root)


def write_zip(path: Path, names: list[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(path, "w") as z:
        for n in names:
            z.writestr(n, "")


def write_tgz(path: Path, names: list[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(path, "w:gz") as t:
        for n in names:
            t.addfile(tarfile.TarInfo(n), io.BytesIO(b""))


class Check(unittest.TestCase):
    def setUp(self) -> None:
        tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, tmp)
        self.dist, self.native = tmp / "sdk", tmp / "native"
        for name in ("core-linux.whl", "client.whl"):
            write_zip(self.native / "wheels" / name, ["m.py"])
        write_zip(self.dist / "wheels" / "otel.whl", ["m.py"])
        for name in ("client.tgz", "otel.tgz"):
            write_tgz(self.dist / "npm" / name, ["package/index.js", "package/core/opamp_core.wasm"])
        for name in ("client.jar", "ext.jar"):
            write_zip(self.dist / "maven" / name, ["A.class"])
        for name in ("core.nupkg", "client.nupkg"):
            write_zip(self.native / "nuget" / name, ["lib/a.dll"])
        write_zip(self.dist / "nuget" / "plugin.nupkg", ["lib/b.dll"])

    def test_a_whole_release_passes(self) -> None:
        self.assertEqual([], sdk_release.check(self.dist, self.native))

    def test_a_package_with_the_cores_sources_fails(self) -> None:
        write_tgz(self.dist / "npm" / "otel.tgz", ["package/index.js", "package/src/lib.rs"])
        write_zip(self.dist / "maven" / "ext.jar", ["Cargo.toml"])
        self.assertEqual(["ext.jar: ships Cargo.toml", "otel.tgz: ships package/src/lib.rs"],
                         sdk_release.check(self.dist, self.native))

    def test_a_source_distribution_fails(self) -> None:
        write_tgz(self.native / "wheels" / "core.tar.gz", ["PKG-INFO"])
        self.assertEqual(["core.tar.gz: a source distribution"], sdk_release.check(self.dist, self.native))

    def test_a_missing_registry_fails(self) -> None:
        (self.dist / "npm" / "otel.tgz").unlink()
        self.assertEqual(["1 npm tarballs, not at least 2"], sdk_release.check(self.dist, self.native))


if __name__ == "__main__":
    unittest.main()
