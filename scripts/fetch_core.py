#!/usr/bin/env python3
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Fetch the OpAMP client core, compiled, from a release in pydantic/opamp-clients, and put it where
the clients load it. Its lock, opamp-core.lock.json, is at this repository's root.

    scripts/fetch_core.py lock 0.1.0        # pin a release: every file's sha256, from its manifest
    scripts/fetch_core.py fetch             # download what the lock pins, verify it, put it in place
    scripts/fetch_core.py fetch --all-platforms
                                            # also every platform's native library, for packaging
                                            # (dist/native/runtimes/<rid>/native/, as native_dist.py)
    scripts/fetch_core.py check             # what is in place is what the lock pins (CI)

The lock is the trust: fetch checks every byte against the sha256 the lock holds, not against
anything it downloads, and refuses a file that differs. `lock` takes the hashes from the release's
opamp-core.json and checks every file against them before writing, so a lock always names files
that were downloaded and matched. Bumping the core is `lock <version>`, `fetch`, and a PR with the
lock's diff.

Where from: the release `core/v<version>` of the lock's `source` (this repository's releases),
through the GitHub API (no token needed: GH_TOKEN or GITHUB_TOKEN is used if set, for the rate
limit), or --from DIR: a directory holding a release's files. Downloads are kept in
.opamp-core/<version>/.
"""
from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
import platform
import re
import shutil
import sys
import tarfile
import urllib.error
import urllib.request
from collections.abc import Mapping
from pathlib import Path

ROOT = Path(os.environ.get("OPAMP_CLIENTS_ROOT") or Path(__file__).resolve().parent.parent)
LOCK = "opamp-core.lock.json"
MANIFEST = "opamp-core.json"
TREE_TAR = "opamp-core-tree.tar.gz"
SOURCE = "pydantic/opamp-clients"
SCHEMA = 1
API = os.environ.get("GITHUB_API_URL", "https://api.github.com")

# Where each module goes in the clients' tree: the paths they load it from today, so moving them
# changes no client.
MODULES = {
    "opamp_core.wasm": [
        "go/opamp/internal/core/opamp_core.wasm",
        "node/opamp/core/opamp_core.wasm",
        "python/src/opamp_client/opamp_core.wasm",
        "java/opamp/src/main/wasm/opamp_core.wasm",
        "dotnet/Pydantic.OpAmp.Client/opamp_core.wasm",
    ],
    "opamp_sdk_settings.wasm": [
        "go/opamp/sdk/opamp_sdk_settings.wasm",
        "node/opamp/core/opamp_sdk_settings.wasm",
        "python/src/opamp_client/opamp_sdk_settings.wasm",
        "java/opamp/src/main/wasm/opamp_sdk_settings.wasm",
        "dotnet/Pydantic.OpAmp.Client/opamp_sdk_settings.wasm",
    ],
    # Every SDK's harness test finds it here (or at OPAMP_ADVERSARY_WASM).
    "opamp_core_adversary.wasm": ["testdata/core/opamp_core_adversary.wasm"],
}
# The tree's files land at their own paths, which must be under these: the .proto files (the -I
# paths the type generators use), the contract the clients are held to, and the shells' tables.
TREE_PLACES = ("spec/proto/", "proto/", "tests/shell-tables/", "testdata/sdk-settings/", "spec/shell-contract.md")
# The machine's own library goes where the .NET project and Python's build backend look for it
# (target/release); every platform's, with --all-platforms, where packaging does.
HOST_LIB_DIR = "target/release"
# Tools go there too, where the tests and the conformance runner look for them.
TOOLS = {"opamp-tap"}
RUNTIMES = "dist/native/runtimes"


# What a manifest may name: a release can't choose where a file lands beyond these.
RIDS = {f"{os_}-{arch}" for os_ in ("linux", "linux-musl", "osx", "win") for arch in ("x64", "arm64")}
LIBS = {"libopamp_native.so", "libopamp_native.dylib", "opamp_native.dll"}
NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*")


class Refused(Exception):
    """A file that isn't what it should be: never placed."""


def sane(name: str, f: dict) -> None:
    """Refuse a file whose name, platform or library name would put it anywhere but its place."""
    if not NAME.fullmatch(name):
        raise Refused(f"{name!r}: not a plain file name")
    if f.get("kind") == "native" and (f.get("target") not in RIDS or f.get("lib") not in LIBS):
        raise Refused(f"{name}: native library {f.get('lib')!r} for {f.get('target')!r} isn't one this script places")
    if f.get("kind") == "tool" and (f.get("target") not in RIDS or f.get("bin") not in TOOLS):
        raise Refused(f"{name}: tool {f.get('bin')!r} for {f.get('target')!r} isn't one this script places")


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def tag(version: str) -> str:
    return f"core/v{version}"


def tree(tar_bytes: bytes) -> dict[str, bytes]:
    """The tar's files by path, refusing any that isn't a plain file under TREE_PLACES."""
    out = {}
    with tarfile.open(fileobj=io.BytesIO(tar_bytes), mode="r:gz") as tar:
        for m in tar.getmembers():
            parts = m.name.split("/")
            # (A backslash or a drive's colon is a separator or a root on Windows.)
            if not m.isfile() or m.name.startswith("/") or "\\" in m.name or ":" in m.name or ".." in parts or "" in parts \
                    or not any(m.name == p or (p.endswith("/") and m.name.startswith(p)) for p in TREE_PLACES):
                raise Refused(f"{TREE_TAR}: {m.name} isn't a file this script places")
            out[m.name] = tar.extractfile(m).read()
    return out


def host_rid() -> str:
    system, machine = platform.system(), platform.machine().lower()
    arch = {"x86_64": "x64", "amd64": "x64", "aarch64": "arm64", "arm64": "arm64"}.get(machine)
    if arch is None:
        sys.exit(f"no native core for {machine}")
    if system == "Linux":
        musl = any(Path("/lib").glob("ld-musl-*")) or platform.libc_ver()[0] == "musl"
        return f"linux-musl-{arch}" if musl else f"linux-{arch}"
    return {"Darwin": f"osx-{arch}", "Windows": f"win-{arch}"}[system]


class Source:
    """A release's files: from a directory, or from GitHub (kept in .opamp-core/<version>/)."""

    def __init__(self, repo: str, version: str, directory: Path | None):
        self.repo, self.version, self.directory = repo, version, directory
        self.cache = ROOT / ".opamp-core" / version
        self.assets: dict[str, str] | None = None

    def get(self, name: str) -> bytes:
        if self.directory is not None:
            path = self.directory / name
            if not path.is_file():
                raise Refused(f"{name}: not in {self.directory}")
            return path.read_bytes()
        cached = self.cache / name
        if cached.is_file():
            return cached.read_bytes()
        data = self.download(name)
        self.cache.mkdir(parents=True, exist_ok=True)
        cached.write_bytes(data)
        return data

    def download(self, name: str) -> bytes:
        if self.assets is None:
            release = json.loads(http(f"{API}/repos/{self.repo}/releases/tags/{tag(self.version)}",
                                      "application/vnd.github+json"))
            self.assets = {a["name"]: a["url"] for a in release["assets"]}
        if name not in self.assets:
            raise Refused(f"{name}: not attached to {self.repo} {tag(self.version)}")
        return http(self.assets[name], "application/octet-stream")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):  # noqa: ANN002, ANN003
        return None


def http(url: str, accept: str) -> bytes:
    """GET with the token; an asset's redirect (to storage that refuses a second credential) without it."""
    headers = {"Accept": accept, "User-Agent": "opamp-clients-fetch-core", "X-GitHub-Api-Version": "2022-11-28"}
    if token := os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN"):
        headers["Authorization"] = f"Bearer {token}"
    opener = urllib.request.build_opener(NoRedirect)
    try:
        with opener.open(urllib.request.Request(url, headers=headers), timeout=60) as r:
            return r.read()
    except urllib.error.HTTPError as e:
        if e.code in (301, 302, 303, 307, 308) and (location := e.headers.get("Location")):
            req = urllib.request.Request(location, headers={"User-Agent": headers["User-Agent"]})
            with urllib.request.urlopen(req, timeout=300) as r:
                return r.read()
        raise SystemExit(f"GET {url}: HTTP {e.code}{why(e.code, e.headers)}") from e


def why(code: int, headers: Mapping[str, str]) -> str:
    """What a refused GET most likely means, as far as GitHub's answer says."""
    if code == 429 or (code == 403 and headers.get("X-RateLimit-Remaining") == "0"):
        return " (the rate limit: set GH_TOKEN)"
    if code == 404:
        return " (not found: no such release or file, or a repository you can't see without GH_TOKEN)"
    return ""


def lock(version: str, repo: str, directory: Path | None) -> dict:
    """The lock for `version`: every file of its manifest, downloaded and matched to it."""
    source = Source(repo, version, directory)
    manifest = json.loads(source.get(MANIFEST))
    if manifest.get("schema") != SCHEMA:
        raise Refused(f"{MANIFEST}: schema {manifest.get('schema')}, this script reads {SCHEMA}")
    if manifest.get("version") != version:
        raise Refused(f"{MANIFEST}: version {manifest.get('version')}, not {version}")
    files = {}
    for f in manifest["files"]:
        sane(f["name"], f)
        data = source.get(f["name"])
        if sha256(data) != f["sha256"] or len(data) != f["size"]:
            raise Refused(f"{f['name']}: sha256 {sha256(data)}, the manifest says {f['sha256']}")
        files[f["name"]] = {k: f[k] for k in ("sha256", "kind", "target", "lib", "bin") if k in f}
    for name in (*MODULES, TREE_TAR):
        if name not in files:
            raise Refused(f"{name}: not in the release")
    return {
        "source": repo,
        "version": version,
        "commit": manifest["commit"],
        "files": files,
        "tree": {n: sha256(d) for n, d in sorted(tree(source.get(TREE_TAR)).items())},
    }


def verified(source: Source, name: str, want: str) -> bytes:
    data = source.get(name)
    if (got := sha256(data)) != want:
        raise Refused(f"{name}: sha256 {got}, {LOCK} pins {want}")
    return data


def write(rel: str, data: bytes) -> None:
    path = ROOT / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    print(f"+ {rel}")


def stale(pinned: dict) -> list[str]:
    """Files under the tree's directories that the pinned tree doesn't have: left by an earlier
    release that had them (a .proto or a table since removed or renamed)."""
    have = [p for d in TREE_PLACES if d.endswith("/") and (ROOT / d).is_dir() for p in (ROOT / d).rglob("*")]
    return sorted(rel for p in have if p.is_file() and (rel := p.relative_to(ROOT).as_posix()) not in pinned["tree"])


def fetch(pinned: dict, directory: Path | None, all_platforms: bool, rid: str) -> None:
    source = Source(pinned["source"], pinned["version"], directory)
    files = pinned["files"]
    for name, f in files.items():
        sane(name, f)
    for name, dests in MODULES.items():
        data = verified(source, name, files[name]["sha256"])
        for d in dests:
            write(d, data)
    got = tree(verified(source, TREE_TAR, files[TREE_TAR]["sha256"]))
    if {n: sha256(d) for n, d in got.items()} != pinned["tree"]:
        raise Refused(f"{TREE_TAR}: its files aren't the ones {LOCK} pins")
    for name, data in got.items():
        write(name, data)
    for rel in stale(pinned):
        (ROOT / rel).unlink()
        print(f"- {rel}")
    for name, f in sorted(files.items()):
        if f["kind"] == "tool" and f["target"] == rid:
            write(f"{HOST_LIB_DIR}/{f['bin']}", verified(source, name, f["sha256"]))
            (ROOT / HOST_LIB_DIR / f["bin"]).chmod(0o755)
    natives = {f["target"]: (n, f) for n, f in files.items() if f["kind"] == "native"}
    if rid not in natives:
        print(f"no native core for {rid} in {pinned['version']}: Python and .NET won't build here", file=sys.stderr)
    for target, (name, f) in sorted(natives.items()):
        if target == rid or all_platforms:
            data = verified(source, name, f["sha256"])
            if target == rid:
                write(f"{HOST_LIB_DIR}/{f['lib']}", data)
            if all_platforms:
                write(f"{RUNTIMES}/{target}/native/{f['lib']}", data)


def check(pinned: dict, rid: str) -> list[str]:
    """What in the tree isn't what the lock pins: the modules and the tree's files (and no other
    file under its directories), and any native
    library or tool fetch put in place (none is checked in, so one that is absent is not a problem)."""
    problems = []
    for name, f in pinned["files"].items():
        if f["kind"] == "native":
            places = [f"{RUNTIMES}/{f['target']}/native/{f['lib']}"]
            if f["target"] == rid:
                places.append(f"{HOST_LIB_DIR}/{f['lib']}")
        elif f["kind"] == "tool" and f["target"] == rid:
            places = [f"{HOST_LIB_DIR}/{f['bin']}"]
        else:
            continue
        for rel in places:
            path = ROOT / rel
            if path.is_file() and sha256(path.read_bytes()) != f["sha256"]:
                problems.append(f"{rel}: not {name} of {pinned['version']}")
            elif path.is_file() and f["kind"] == "tool" and os.name != "nt" and not path.stat().st_mode & 0o100:
                problems.append(f"{rel}: not executable")
    for name, dests in MODULES.items():
        for d in dests:
            path = ROOT / d
            if not path.is_file() or sha256(path.read_bytes()) != pinned["files"][name]["sha256"]:
                problems.append(f"{d}: not {name} of {pinned['version']}")
    for name, want in pinned["tree"].items():
        path = ROOT / name
        if not path.is_file() or sha256(path.read_bytes()) != want:
            problems.append(f"{name}: not the one {pinned['version']} has")
    problems += [f"{rel}: not in {pinned['version']} (fetch removes it)" for rel in stale(pinned)]
    return problems


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="step", required=True)
    lk = sub.add_parser("lock", help="pin a release")
    lk.add_argument("version")
    lk.add_argument("--source", default=SOURCE)
    lk.add_argument("--from", dest="directory", type=Path)
    fe = sub.add_parser("fetch", help="download, verify and place what the lock pins")
    fe.add_argument("--from", dest="directory", type=Path)
    fe.add_argument("--all-platforms", action="store_true")
    fe.add_argument("--rid", default=None, help="the platform whose library goes in target/release")
    ck = sub.add_parser("check", help="the tree holds what the lock pins")
    ck.add_argument("--rid", default=None, help="the platform whose library is in target/release")
    args = p.parse_args()
    try:
        if args.step == "lock":
            (ROOT / LOCK).write_text(json.dumps(lock(args.version, args.source, args.directory), indent=2) + "\n")
            print(f"{LOCK}: {args.version}")
            return 0
        pinned = json.loads((ROOT / LOCK).read_text())
        if args.step == "fetch":
            fetch(pinned, args.directory, args.all_platforms, args.rid or host_rid())
            return 0
        if problems := check(pinned, args.rid or host_rid()):
            print(*problems, "run scripts/fetch_core.py fetch", sep="\n")
            return 1
        print(f"the core in the tree is {pinned['version']} ({pinned['commit'][:12]}), as {LOCK} pins")
        return 0
    except Refused as e:
        print(f"refused: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
