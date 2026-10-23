#!/usr/bin/env python3
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Known vulnerabilities in what the clients build and ship with: every lockfile and manifest here
(each go.mod, package-lock.json, uv.lock, pom.xml and .csproj) against the OSV database, with
OSV-Scanner at a pinned release.

    scripts/deps_scan.py              # scan; fails on any advisory osv-scanner.toml doesn't ignore
    scripts/deps_scan.py --offline    # with OSV's downloadable databases (where api.osv.dev is blocked)
    scripts/deps_scan.py --check-config   # only the ignore list's rules

CI's `core` job runs it on every pull request and push to main, so a lockfile can't take in a
version with a known advisory, and the nightly's `dependencies` job runs it on main, so an advisory
published since the last change is found the next morning. The fix is nearly always a version
bump in the lockfile or manifest that has it (`go get <module>@<fixed>` and `go mod tidy`,
`npm update <package>`, `uv lock --upgrade-package <package>`, the pom's or .csproj's version).

An advisory that can't be fixed yet (no fixed release, or it doesn't apply to how we use the
package) goes in osv-scanner.toml as an [[IgnoredVulns]] entry with its id, a reason, and an
ignoreUntil at most IGNORE_DAYS days away: no ignore is permanent, so each is looked at again.
SECURITY.md says how a vulnerability is reported and handled.

The binary is fetched once into .cache/ from OSV-Scanner's GitHub release and checked against the
sha256 pinned here; a new release is a change to VERSION and SHA256.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import os
import platform
import stat
import subprocess
import sys
import tomllib
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CONFIG = ROOT / "osv-scanner.toml"
CACHE = ROOT / ".cache" / "osv-scanner"
VERSION = "2.6.0"
# osv-scanner_SHA256SUMS of the release, for the platforms we build on.
SHA256 = {
    "linux_amd64": "ca69b3d3cd08f889a49dc0a383122f71cc528b83803671df5fd874d97485b108",
    "linux_arm64": "2c71403eb443d05891c4f268c3ad771cf4f16e5443463fd7851ef8f454d3c7e4",
    "darwin_amd64": "60c5296637e977b28eeda5c7f13573e447659a632922737f94d11fa7e30ad6ca",
    "darwin_arm64": "98c460dcd37de25819babd757d04542045b6243113e209edcd4d89fedb0256b4",
}
# The longest an advisory may be ignored before someone looks at it again.
IGNORE_DAYS = 90


def asset(system: str, machine: str) -> str:
    """The release's binary for this platform (`platform.system()`, `platform.machine()`)."""
    os_name = {"Linux": "linux", "Darwin": "darwin"}.get(system)
    arch = {"x86_64": "amd64", "amd64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(machine)
    if os_name is None or arch is None or f"{os_name}_{arch}" not in SHA256:
        raise SystemExit(f"no pinned OSV-Scanner for {system} {machine}: add its sha256 to SHA256")
    return f"{os_name}_{arch}"


def problems(config: dict, today: dt.date) -> list[str]:
    """What is wrong with osv-scanner.toml's ignore list: an entry without an id or a reason, or
    without an ignoreUntil, or one more than IGNORE_DAYS days away (an expired one needs nothing
    here: OSV-Scanner reports the advisory again)."""
    out = []
    for i, entry in enumerate(config.get("IgnoredVulns", [])):
        name = entry.get("id") or f"IgnoredVulns[{i}]"
        if not entry.get("id"):
            out.append(f"{name}: no id")
        if not str(entry.get("reason", "")).strip():
            out.append(f"{name}: no reason (say why it doesn't apply, or what it waits on)")
        until = entry.get("ignoreUntil")
        if isinstance(until, dt.datetime):
            until = until.date()
        if not isinstance(until, dt.date):
            out.append(f"{name}: no ignoreUntil (no ignore is permanent; at most {IGNORE_DAYS} days)")
        elif until > today + dt.timedelta(days=IGNORE_DAYS):
            out.append(f"{name}: ignoreUntil {until} is more than {IGNORE_DAYS} days away")
    return out


def binary() -> Path:
    name = asset(platform.system(), platform.machine())
    path = CACHE / f"osv-scanner-{VERSION}-{name}"
    if not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != SHA256[name]:
        url = f"https://github.com/google/osv-scanner/releases/download/v{VERSION}/osv-scanner_{name}"
        print(f"fetching {url}", flush=True)
        with urllib.request.urlopen(url, timeout=120) as r:
            data = r.read()
        if (got := hashlib.sha256(data).hexdigest()) != SHA256[name]:
            raise SystemExit(f"{url}: sha256 {got}, pinned {SHA256[name]}")
        CACHE.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        path.chmod(path.stat().st_mode | stat.S_IXUSR)
    return path


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--offline", action="store_true", help="use OSV's downloadable databases, not api.osv.dev")
    p.add_argument("--check-config", action="store_true", help="only check osv-scanner.toml's ignore list")
    args = p.parse_args()
    if bad := problems(tomllib.loads(CONFIG.read_text()), dt.date.today()):
        print(*(f"{CONFIG.name}: {b}" for b in bad), sep="\n")
        return 1
    if args.check_config:
        print(f"{CONFIG.name}: every ignore has a reason and ends within {IGNORE_DAYS} days")
        return 0
    # -r: every lockfile below the root; files git ignores (node_modules/, target/, the fetched core) are skipped.
    cmd = [str(binary()), "scan", "source", "-r", "--config", str(CONFIG)]
    if args.offline:
        cmd += ["--offline-vulnerabilities", "--download-offline-databases"]
    print("+", " ".join(cmd[1:]), flush=True)
    # 0: nothing found; 1: an advisory; 128: no lockfile found, which is a mistake here too.
    return subprocess.run([*cmd, "."], cwd=ROOT, env=dict(os.environ)).returncode


if __name__ == "__main__":
    sys.exit(main())
