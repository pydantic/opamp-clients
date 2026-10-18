# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""sdk_release.py's stamp, on copies of the real manifests, and its check, on made-up packages."""
from __future__ import annotations

import hashlib
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest import mock

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

    # Every NuGet package native_dist.py packs is stamped, the hosting package too.
    def test_every_packed_nuget_package_is_stamped(self) -> None:
        stamped = {Path(rel).stem for rel in sdk_release.CSPROJS}
        for name in ("Pydantic.OpAmp.Core.Native", "Pydantic.OpAmp.Client", "Pydantic.OpAmp.Extensions.Hosting"):
            self.assertIn(name, stamped)

    # pack copies each classified jar the pom really builds, by its name there.
    def test_classified_jars_are_ones_the_poms_build(self) -> None:
        for rel, classifiers in sdk_release.CLASSIFIERS.items():
            for c in classifiers:
                self.assertIn(f"<classifier>{c}</classifier>", self.read(rel), rel)

    # Central refuses a pom without its metadata: both, stamped, carry all of it.
    def test_the_poms_carry_what_maven_central_requires(self) -> None:
        import xml.etree.ElementTree as ET
        for rel in sdk_release.POMS:
            project = ET.fromstring(self.read(rel))
            for path in sdk_release.POM_REQUIRED:
                found = project.findtext("m:" + path.replace("/", "/m:"), namespaces=sdk_release.POM_NS)
                self.assertTrue((found or "").strip(), f"{rel}: {path}")
            self.assertEqual(("dev.pydantic", "7.8.9"), sdk_release.coordinates(self.read(rel), rel)[::2], rel)

    # pack builds the sources and javadoc jars in the poms' release profile.
    def test_the_release_profile_builds_sources_and_javadoc(self) -> None:
        for rel in sdk_release.POMS:
            pom = self.read(rel)
            self.assertRegex(pom, r"<id>release</id>[\s\S]*maven-source-plugin[\s\S]*maven-javadoc-plugin", rel)
        # The shaded jar's pom is the reduced one, written where pack takes it.
        for rel in sdk_release.SHADED:
            self.assertIn("<createDependencyReducedPom>true</createDependencyReducedPom>", self.read(rel))
            self.assertIn("${project.build.directory}/dependency-reduced-pom.xml", self.read(rel))

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


POM = """<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>dev.pydantic</groupId>
  <artifactId>{artifact}</artifactId>
  <version>1.2.3</version>
  <name>n</name>
  <description>d</description>
  <url>https://example.com</url>
  <licenses><license><name>MIT License</name></license></licenses>
  <developers><developer><name>Pydantic</name></developer></developers>
  <scm><connection>scm:git:https://example.com/x.git</connection><url>https://example.com/x</url></scm>
  <dependencies><dependency><groupId>g</groupId><artifactId>dep</artifactId><version>9</version></dependency></dependencies>
</project>
"""


def write_maven(maven: Path) -> None:
    maven.mkdir(parents=True, exist_ok=True)
    for artifact, classifiers in (("client", []), ("ext", ["app-settings"])):
        (maven / f"{artifact}-1.2.3.pom").write_text(POM.format(artifact=artifact))
        for c in ["", "-sources", "-javadoc", *(f"-{c}" for c in classifiers)]:
            write_zip(maven / f"{artifact}-1.2.3{c}.jar", ["A.class"])


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
            write_tgz(self.dist / "npm" / name, ["package/index.js", "package/core/opamp_core.wasm",
                                                 "package/core/LICENSE.txt", "package/core/THIRD_PARTY_NOTICES.txt"])
        write_maven(self.dist / "maven")
        for name in ("core.nupkg", "client.nupkg", "hosting.nupkg"):
            write_zip(self.native / "nuget" / name, ["lib/a.dll"])
        write_zip(self.dist / "nuget" / "plugin.nupkg", ["lib/b.dll"])

    def test_a_whole_release_passes(self) -> None:
        self.assertEqual([], sdk_release.check(self.dist, self.native))

    def test_a_package_with_the_cores_sources_fails(self) -> None:
        write_tgz(self.dist / "npm" / "otel.tgz", ["package/index.js", "package/src/lib.rs"])
        write_zip(self.dist / "maven" / "ext-1.2.3.jar", ["Cargo.toml"])
        self.assertEqual(["ext-1.2.3.jar: ships Cargo.toml", "otel.tgz: ships package/src/lib.rs"],
                         sdk_release.check(self.dist, self.native))

    # Each package carrying the core, in whatever form, has the core's license and notices together.
    def test_a_package_carrying_the_core_without_its_license_fails(self) -> None:
        write_tgz(self.dist / "npm" / "client.tgz", ["package/core/opamp_core.wasm", "package/core/LICENSE.txt"])
        write_zip(self.dist / "maven" / "client-1.2.3.jar", ["dev/pydantic/opamp/core/CoreModule.class"])
        write_zip(self.native / "nuget" / "core.nupkg", ["runtimes/linux-x64/native/libopamp_native.so",
                                                         "LICENSE.txt", "x/THIRD_PARTY_NOTICES.txt"])
        write_zip(self.native / "wheels" / "core-linux.whl", ["pydantic_opamp_core/libopamp_native.so",
                                                              "c.dist-info/licenses/LICENSE.txt",
                                                              "c.dist-info/licenses/THIRD_PARTY_NOTICES.txt"])
        self.assertEqual([f"{n}: carries the core without LICENSE.txt and THIRD_PARTY_NOTICES.txt"
                          for n in ("core.nupkg", "client-1.2.3.jar", "client.tgz")],
                         sdk_release.check(self.dist, self.native))

    def test_a_source_distribution_fails(self) -> None:
        write_tgz(self.native / "wheels" / "core.tar.gz", ["PKG-INFO"])
        self.assertEqual(["core.tar.gz: a source distribution"], sdk_release.check(self.dist, self.native))

    def test_a_missing_app_settings_jar_fails(self) -> None:
        (self.dist / "maven" / "ext-1.2.3-app-settings.jar").unlink()
        self.assertEqual(["6 Maven jars, not at least 7"], sdk_release.check(self.dist, self.native))

    # Central refuses an artifact without its sources or javadoc jar, or a pom without the
    # metadata it requires.
    def test_what_maven_central_refuses_fails(self) -> None:
        (self.dist / "maven" / "client-1.2.3-javadoc.jar").unlink()
        write_zip(self.dist / "maven" / "client-1.2.3-tests.jar", ["A.class"])
        pom = self.dist / "maven" / "ext-1.2.3.pom"
        pom.write_text(pom.read_text().replace("<developers><developer><name>Pydantic</name></developer></developers>", "")
                       .replace("<url>https://example.com/x</url>", "").replace("1.2.3", "1.2.3-SNAPSHOT"))
        self.assertEqual(["client-1.2.3.pom: no client-1.2.3-javadoc.jar", "ext-1.2.3.pom: no developers/developer/name",
                          "ext-1.2.3.pom: no scm/url", "ext-1.2.3.pom: a snapshot",
                          "ext-1.2.3.pom: no ext-1.2.3-SNAPSHOT.jar", "ext-1.2.3.pom: no ext-1.2.3-SNAPSHOT-sources.jar",
                          "ext-1.2.3.pom: no ext-1.2.3-SNAPSHOT-javadoc.jar"],
                         sdk_release.check(self.dist, self.native))

    def test_a_missing_nuget_package_fails(self) -> None:
        (self.native / "nuget" / "hosting.nupkg").unlink()
        self.assertEqual(["3 NuGet packages, not at least 4"], sdk_release.check(self.dist, self.native))

    def test_a_missing_registry_fails(self) -> None:
        (self.dist / "npm" / "otel.tgz").unlink()
        self.assertEqual(["1 npm tarballs, not at least 2"], sdk_release.check(self.dist, self.native))


class Bundle(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.maven = self.tmp / "maven"
        write_maven(self.maven)

    # Central's layout: each file under its coordinates, with its checksums and its signature.
    def test_every_file_is_laid_out_checksummed_and_signed(self) -> None:
        out = self.tmp / "bundle.zip"
        names = sdk_release.bundle(self.maven, out, lambda p: b"signed " + p.name.encode())
        with zipfile.ZipFile(out) as z:
            self.assertEqual(sorted(names), sorted(z.namelist()))
            base = "dev/pydantic/ext/1.2.3/"
            files = ["ext-1.2.3.pom", "ext-1.2.3.jar", "ext-1.2.3-sources.jar", "ext-1.2.3-javadoc.jar",
                     "ext-1.2.3-app-settings.jar"]
            for f in files:
                data = z.read(base + f)
                self.assertEqual(hashlib.md5(data).hexdigest().encode(), z.read(base + f + ".md5"), f)
                self.assertEqual(hashlib.sha1(data).hexdigest().encode(), z.read(base + f + ".sha1"), f)
                self.assertEqual(b"signed " + f.encode(), z.read(base + f + ".asc"), f)
            self.assertEqual(sorted(base + f + x for f in files for x in ("", ".md5", ".sha1", ".asc")),
                             sorted(n for n in z.namelist() if n.startswith(base)))
            self.assertEqual(36, len(z.namelist()))  # 2 poms and 7 jars, 4 files each

    def test_an_unsigned_bundle_has_no_signatures(self) -> None:
        names = sdk_release.bundle(self.maven, self.tmp / "bundle.zip", None)
        self.assertEqual(27, len(names))
        self.assertFalse([n for n in names if n.endswith(".asc")])

    def test_what_central_would_refuse_is_not_bundled(self) -> None:
        (self.maven / "client-1.2.3-sources.jar").unlink()
        with self.assertRaises(SystemExit):
            sdk_release.bundle(self.maven, self.tmp / "bundle.zip", None)
        self.assertFalse((self.tmp / "bundle.zip").exists())

    # The real signer, with a key made here, when gpg is installed: Central checks the .asc.
    @unittest.skipUnless(shutil.which("gpg"), "no gpg")
    def test_gpg_signs_what_gpg_verifies(self) -> None:
        home = self.tmp / "gnupg"
        home.mkdir(mode=0o700)
        env = {**os.environ, "GNUPGHOME": str(home)}
        subprocess.run(["gpg", "--batch", "--passphrase", "", "--quick-gen-key", "test@example.com", "ed25519", "sign", "1d"],
                       env=env, check=True, capture_output=True)
        with mock.patch.dict(os.environ, {"GNUPGHOME": str(home), "MAVEN_GPG_PASSPHRASE": ""}):
            sig = sdk_release.gpg_sign(self.maven / "client-1.2.3.jar")
        (self.tmp / "client.asc").write_bytes(sig)
        subprocess.run(["gpg", "--batch", "--verify", str(self.tmp / "client.asc"), str(self.maven / "client-1.2.3.jar")],
                       env=env, check=True, capture_output=True)


if __name__ == "__main__":
    unittest.main()
