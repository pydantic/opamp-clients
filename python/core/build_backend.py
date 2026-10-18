# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Build pydantic-opamp-core: a wheel holding pydantic_opamp_core/<the native library>, and the core's
license and notices, and nothing else.

A PEP 517 backend for building from the repository (uv sync builds one for this machine from the
library scripts/fetch_core.py fetched); scripts/native_dist.py calls wheel() with each platform's library and tag. There is no source
distribution: the core ships compiled only.
"""
from __future__ import annotations

import base64
import hashlib
import os
import sys
import sysconfig
import tomllib
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent


def _project() -> dict:
    return tomllib.loads((HERE / "pyproject.toml").read_text())["project"]


def lib_name(platform: str = sys.platform) -> str:
    """The library's file name on a platform (sys.platform's spelling)."""
    if platform.startswith("win"):
        return "opamp_native.dll"
    return "libopamp_native.dylib" if platform == "darwin" else "libopamp_native.so"


def wheel(lib: Path, tag: str, out: Path) -> Path:
    """Write the wheel for one platform: lib as pydantic_opamp_core/<lib's name>, tagged py3-none-<tag>,
    with the license files (the core's license and notices) in .dist-info/licenses/."""
    p = _project()
    dist = p["name"].replace("-", "_")
    info = f"{dist}-{p['version']}.dist-info"
    if missing := [f for f in p["license-files"] if not (HERE / f).is_file()]:
        sys.exit(f"{', '.join(missing)} missing from {HERE}: run scripts/fetch_core.py fetch")
    files = {
        f"pydantic_opamp_core/{lib.name}": lib.read_bytes(),
        f"{info}/METADATA": (f"Metadata-Version: 2.4\nName: {p['name']}\nVersion: {p['version']}\n"
                             f"Summary: {p['description']}\nLicense-Expression: {p['license']}\n"
                             + "".join(f"License-File: {f}\n" for f in p["license-files"]) +
                             f"Requires-Python: {p['requires-python']}\n"
                             f"Description-Content-Type: text/markdown\n\n{(HERE / p['readme']).read_text()}").encode(),
        f"{info}/WHEEL": (f"Wheel-Version: 1.0\nGenerator: pydantic-opamp-core\nRoot-Is-Purelib: false\n"
                          f"Tag: py3-none-{tag}\n").encode(),
        **{f"{info}/licenses/{f}": (HERE / f).read_bytes() for f in p["license-files"]},
    }
    record = [f"{n},sha256={base64.urlsafe_b64encode(hashlib.sha256(b).digest()).rstrip(b'=').decode()},{len(b)}"
              for n, b in files.items()]
    files[f"{info}/RECORD"] = ("\n".join([*record, f"{info}/RECORD,,"]) + "\n").encode()
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{dist}-{p['version']}-py3-none-{tag}.whl"
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
        for name, data in files.items():
            info_ = zipfile.ZipInfo(name, date_time=(2026, 1, 1, 0, 0, 0))
            info_.external_attr = (0o755 if name.startswith("pydantic_opamp_core/") else 0o644) << 16
            z.writestr(info_, data, zipfile.ZIP_DEFLATED)
    return path


def target_dir() -> Path:
    """Where this machine's library is: the repository's target/ (scripts/fetch_core.py puts it in
    target/release), or CARGO_TARGET_DIR (relative to the repository) if set, as
    scripts/redact_mutants.py sets it for a copy of the repository."""
    return ROOT / os.environ.get("CARGO_TARGET_DIR", "target")


def build_wheel(wheel_directory: str, config_settings: dict | None = None, metadata_directory: str | None = None) -> str:
    """This machine's wheel: the library scripts/fetch_core.py put in target/release."""
    lib = target_dir() / "release" / lib_name()
    if not lib.exists():
        sys.exit(f"{lib} is missing: run scripts/fetch_core.py fetch")
    tag = sysconfig.get_platform().replace("-", "_").replace(".", "_")
    return wheel(lib, tag, Path(wheel_directory)).name


def get_requires_for_build_wheel(config_settings: dict | None = None) -> list[str]:
    return []
