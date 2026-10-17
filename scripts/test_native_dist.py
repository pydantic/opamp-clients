# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Tests for native_dist.py: what its check refuses (the cores' sources anywhere, a binary in the
client, a core wheel holding anything but its library), and the core's wheels it writes.

    python3 -m unittest scripts/test_native_dist.py
"""
from __future__ import annotations

import base64
import contextlib
import hashlib
import io
import os
import sys
import tempfile
import time
import unittest
import unittest.mock
import zipfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import native_dist as nd  # noqa: E402

nd.TARGETS_ALL = list(nd.TARGETS)  # (OPAMP_NATIVE_TARGETS unset)
CLIENT = "wheels/pydantic_opamp_client-0.1.0-py3-none-any.whl"
HOSTING_DLL = "lib/net8.0/Pydantic.OpAmp.Extensions.Hosting.dll"


class CompiledOnly(unittest.TestCase):
    def setUp(self) -> None:
        self.dist = Path(tempfile.mkdtemp())
        (self.dist / "wheels").mkdir()
        (self.dist / "nuget").mkdir()
        self._saved = nd.DIST, nd.TARGETS, nd.file_says
        nd.DIST, nd.TARGETS = self.dist, []
        nd.file_says = lambda path: "ELF 64-bit LSB shared object, x86-64"
        self.archive(CLIENT, "opamp_client/__init__.py", "opamp_client/_native.py")

    def tearDown(self) -> None:
        nd.DIST, nd.TARGETS, nd.file_says = self._saved

    def archive(self, name: str, *members: str) -> None:
        with zipfile.ZipFile(self.dist / name, "w") as z:
            for m in members:
                z.writestr(m, "")

    def problems(self) -> str:
        err = io.StringIO()
        with contextlib.redirect_stderr(err), contextlib.redirect_stdout(io.StringIO()):
            try:
                nd.check()
            except SystemExit:
                return err.getvalue()
        return ""

    def core_wheel(self, *extra: str) -> None:
        lib = self.dist / "runtimes" / "linux-x64" / "native" / "libopamp_native.so"
        lib.parent.mkdir(parents=True)
        lib.write_bytes(b"\x7fELF")
        nd.TARGETS = [t for t in nd.TARGETS_ALL if t.rid == "linux-x64"]
        path = nd.build_backend.wheel(lib, nd.TARGETS[0].plat, self.dist / "wheels")
        with zipfile.ZipFile(path, "a") as z:
            for m in extra:
                z.writestr(m, "")

    def packages(self, *client_extra: str, core: tuple[str, ...] = ("runtimes/linux-x64/native/libopamp_native.so",),
                 depends: str | None = None, assembly: str = "lib/net8.0/Pydantic.OpAmp.Client.dll",
                 hosting: tuple[str, ...] = (HOSTING_DLL, "lib/net8.0/Pydantic.OpAmp.Extensions.Hosting.xml"),
                 hosting_depends: str = "Pydantic.OpAmp.Client") -> None:
        nuspec = f'<dependency id="Pydantic.OpAmp.Core.Native" version="{depends or f"[{nd.core_version()}]"}" />'
        with zipfile.ZipFile(self.dist / "nuget" / "Pydantic.OpAmp.Client.0.1.0.nupkg", "w") as z:
            z.writestr("Pydantic.OpAmp.Client.nuspec", nuspec)
            for m in (assembly, *client_extra):
                z.writestr(m, "")
        with zipfile.ZipFile(self.dist / "nuget" / "Pydantic.OpAmp.Extensions.Hosting.0.1.0.nupkg", "w") as z:
            z.writestr("Pydantic.OpAmp.Extensions.Hosting.nuspec", f'<dependency id="{hosting_depends}" version="0.1.0" />')
            for m in hosting:
                z.writestr(m, "")
        self.archive("nuget/Pydantic.OpAmp.Core.Native.0.1.0.nupkg", "Pydantic.OpAmp.Core.Native.nuspec",
                     "[Content_Types].xml", "_rels/.rels", "package/services/metadata/core-properties/x.psmdcp", *core)

    def test_binaries_alone_pass(self) -> None:
        self.core_wheel()
        self.packages()
        self.assertEqual(self.problems(), "")

    def test_the_library_in_the_client_package_fails(self) -> None:
        self.core_wheel()
        self.packages("runtimes/linux-x64/native/libopamp_native.so")
        self.assertIn("Pydantic.OpAmp.Client.0.1.0.nupkg: ships runtimes/linux-x64/native/libopamp_native.so",
                      self.problems())

    def test_a_client_package_without_its_assembly_fails(self) -> None:
        self.core_wheel()
        self.packages(assembly="lib/net8.0/Other.dll")
        self.assertIn("Pydantic.OpAmp.Client.0.1.0.nupkg: lacks lib/net8.0/Pydantic.OpAmp.Client.dll", self.problems())

    def test_a_missing_package_fails(self) -> None:
        self.core_wheel()
        for name in ("Pydantic.OpAmp.Core.Native", "Pydantic.OpAmp.Client", "Pydantic.OpAmp.Extensions.Hosting"):
            self.packages()
            (self.dist / "nuget" / f"{name}.0.1.0.nupkg").unlink()
            self.assertIn(f"0 {name} packages, not 1", self.problems())

    def test_a_client_package_not_on_the_core_package_fails(self) -> None:
        self.core_wheel()
        self.packages(depends=nd.core_version())
        self.assertIn(f"doesn't depend on Pydantic.OpAmp.Core.Native {nd.core_version()} exactly", self.problems())

    def test_a_core_package_missing_a_library_or_holding_anything_else_fails(self) -> None:
        self.core_wheel()
        lib = "runtimes/linux-x64/native/libopamp_native.so"
        for core in ((), (lib, "lib/net8.0/x.dll"), (lib, "content/x.txt"), (lib, "x.txt"), (lib, "contentFiles/any/any/x.rs")):
            self.packages(core=core)
            self.assertIn("Pydantic.OpAmp.Core.Native.0.1.0.nupkg: holds", self.problems())

    def test_the_hosting_package_is_its_assembly_on_the_client_package(self) -> None:
        self.core_wheel()
        hosting = "Pydantic.OpAmp.Extensions.Hosting.0.1.0.nupkg"
        # The client's assembly, a library, or the shared sources would be shipped twice, or as source.
        for extra in ("lib/net8.0/Pydantic.OpAmp.Client.dll", "runtimes/linux-x64/native/libopamp_native.so",
                      "contentFiles/cs/any/shared/Controls.cs"):
            self.packages(hosting=(HOSTING_DLL, extra))
            self.assertIn(f"{hosting}: ships {extra}", self.problems())
        self.packages(hosting=("lib/net8.0/Other.dll",))
        self.assertIn(f"{hosting}: lacks {HOSTING_DLL}", self.problems())
        self.packages(hosting_depends="Pydantic.OpAmp.Core.Native")
        self.assertIn(f"{hosting}: doesn't depend on Pydantic.OpAmp.Client", self.problems())

    def test_a_binary_in_the_client_fails(self) -> None:
        self.archive(CLIENT, "opamp_client/__init__.py", "opamp_client/_native.abi3.so")
        self.assertIn("ships opamp_client/_native.abi3.so", self.problems())

    def test_python_in_the_core_wheel_fails(self) -> None:
        self.core_wheel("opamp_core/__init__.py")
        self.assertIn("not opamp_core/libopamp_native.so alone", self.problems())

    def test_a_target_without_its_core_wheel_fails(self) -> None:
        self.core_wheel()
        (next((self.dist / "wheels").glob("pydantic_opamp_core-*.whl"))).unlink()
        self.assertIn("0 core wheels", self.problems())

    def test_a_source_distribution_fails(self) -> None:
        (self.dist / "wheels" / "opamp-0.1.0.tar.gz").write_bytes(b"")
        self.assertIn("a source distribution", self.problems())

    def test_rust_sources_in_a_wheel_fail(self) -> None:
        self.archive(CLIENT, "opamp_client/__init__.py", "src/lib.rs")
        self.assertIn("ships src/lib.rs", self.problems())

    def test_a_crate_manifest_in_the_package_fails(self) -> None:
        self.archive("nuget/Pydantic.OpAmp.Core.Native.0.1.0.nupkg", "runtimes/linux-x64/native/libopamp_native.so",
                     "core/Cargo.toml")
        self.assertIn("ships core/Cargo.toml", self.problems())


class CoreWheel(unittest.TestCase):
    def test_the_wheel_is_the_library_and_a_record_that_matches_it(self) -> None:
        d = Path(tempfile.mkdtemp())
        (d / "libopamp_native.so").write_bytes(b"compiled")
        path = nd.build_backend.wheel(d / "libopamp_native.so", "musllinux_1_2_aarch64", d / "out")
        self.assertEqual(path.name, "pydantic_opamp_core-0.1.0-py3-none-musllinux_1_2_aarch64.whl")
        with zipfile.ZipFile(path) as z:
            names = z.namelist()
            self.assertEqual(z.read("opamp_core/libopamp_native.so"), b"compiled")
            self.assertIn("Tag: py3-none-musllinux_1_2_aarch64", z.read(names[2]).decode())
            self.assertIn("Root-Is-Purelib: false", z.read(names[2]).decode())
            for line in z.read(names[-1]).decode().splitlines()[:-1]:
                name, digest, size = line.split(",")
                data = z.read(name)
                want = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
                self.assertEqual((digest, int(size)), (f"sha256={want}", len(data)), name)
        self.assertEqual(names, ["opamp_core/libopamp_native.so", *(f"pydantic_opamp_core-0.1.0.dist-info/{f}"
                                                                     for f in ("METADATA", "WHEEL", "RECORD"))])

    # PyPI shows the description; `twine check --strict` (the Registries workflow) refuses a wheel without one.
    def test_the_metadata_carries_the_readme(self) -> None:
        d = Path(tempfile.mkdtemp())
        (d / "libopamp_native.so").write_bytes(b"compiled")
        path = nd.build_backend.wheel(d / "libopamp_native.so", "musllinux_1_2_aarch64", d / "out")
        with zipfile.ZipFile(path) as z:
            head, _, body = z.read("pydantic_opamp_core-0.1.0.dist-info/METADATA").decode().partition("\n\n")
        self.assertIn("Description-Content-Type: text/markdown", head.splitlines())
        self.assertEqual(body, (nd.build_backend.HERE / "README.md").read_text())


class TargetDir(unittest.TestCase):
    def test_cargos_target_dir_relative_to_the_repository_where_cargo_runs(self) -> None:
        root = nd.build_backend.ROOT
        for env, want in ((None, root / "target"), ("out/t", root / "out" / "t"), ("/abs/t", Path("/abs/t"))):
            with unittest.mock.patch.dict("os.environ", {} if env is None else {"CARGO_TARGET_DIR": env}):
                if env is None:
                    os.environ.pop("CARGO_TARGET_DIR", None)
                self.assertEqual(nd.build_backend.target_dir(), want, env)


class OnlyTargets(unittest.TestCase):
    def test_known_targets_narrow_the_list(self) -> None:
        self.assertEqual([t.rid for t in nd.only(nd.TARGETS, "linux-x64,osx-arm64")], ["linux-x64", "osx-arm64"])

    def test_an_unknown_target_is_refused(self) -> None:
        with self.assertRaises(SystemExit) as e:
            nd.only(nd.TARGETS, "linux-x64,linux-x86")
        self.assertIn("linux-x86", str(e.exception.code))


class SmokeLimit(unittest.TestCase):
    def test_a_run_past_its_limit_is_stopped_and_fails_saying_so(self) -> None:
        stopped = []
        began = time.monotonic()
        code, out = nd.run_limited([sys.executable, "-c", "print('started', flush=True); import time; time.sleep(30)"],
                                   0.5, on_timeout=lambda: stopped.append(True))
        self.assertLess(time.monotonic() - began, 10)
        self.assertNotEqual(code, 0)
        self.assertIn("started", out)
        self.assertIn("still running after 0.5 s", out)
        self.assertEqual(stopped, [True])

    def test_a_run_within_its_limit_is_its_own(self) -> None:
        self.assertEqual(nd.run_limited([sys.executable, "-c", "print('hi'); raise SystemExit(3)"], 30),
                         (3, "hi\n"))


class Pinned(unittest.TestCase):
    def test_each_platform_s_image_is_pulled_then_named_by_its_id_one_after_another(self) -> None:
        calls = []

        def docker(cmd, **kw):
            calls.append(cmd)
            platform = next((c[c.index("--platform") + 1] for c in reversed(calls) if "pull" in c), "")
            return unittest.mock.Mock(stdout=f"sha256:{platform}-{cmd[-1]}\n")

        ids = nd.pinned([("linux/amd64", "py"), ("linux/arm64", "py"), ("linux/amd64", "py")], docker=docker)
        self.assertEqual(ids, {("linux/amd64", "py"): "sha256:linux/amd64-py", ("linux/arm64", "py"): "sha256:linux/arm64-py"})
        self.assertEqual([c[1] for c in calls], ["pull", "image", "pull", "image"])


class SmokeCmds(unittest.TestCase):
    def test_dotnet_under_emulation_runs_with_write_xor_execute_off(self) -> None:
        ids = {(p, i): f"id:{p}:{i}" for p, py, dn in nd.SMOKE for i in (py, dn)}
        cmds = nd.smoke_cmds(ids, "0.1.0")
        self.assertEqual(len(cmds), 2 * len(nd.SMOKE))
        for cmd in cmds:
            platform = cmd[cmd.index("--platform") + 1]
            dotnet = "dotnet run" in cmd[-1]
            self.assertEqual("DOTNET_EnableWriteXorExecute=0" in cmd, dotnet and platform != "linux/amd64", cmd)
            self.assertTrue(any(a.startswith(f"id:{platform}:") for a in cmd), cmd)


if __name__ == "__main__":
    unittest.main()
