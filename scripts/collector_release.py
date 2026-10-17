#!/usr/bin/env python3
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""The upstream Collector release that the collector extension and the Supervisor are forked from.

    scripts/collector_release.py pins       # every pin names one release (offline)
    scripts/collector_release.py latest     # and no newer contrib release exists (needs git)

`go/opampextension` and `go/opampsupervisor` are forks of opentelemetry-collector-contrib's
`extension/opampextension` and `cmd/opampsupervisor` at one release. ocb puts every component of a
collector on one release, so each pin of the collector's modules moves together: the go.mod
requires under go/, the ocb manifests, and each `builder@` that builds them. `pins` fails when
they name more than one 0.x release, or more than one 1.x release of the core's stable modules.

`latest` fails when contrib has a release newer than the pins. It prints the files of the forked
directories that upstream changed since our release, which is the diff to port before the pins
move. It reads contrib's tags with git (a shallow, blobless fetch), so it needs no token.
"""
from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
UPSTREAM = "https://github.com/open-telemetry/opentelemetry-collector-contrib"
# The upstream directories that this repository's Go modules are forked from.
FORKED = ["extension/opampextension", "cmd/opampsupervisor"]
# The files that pin the collector's release: Go modules, ocb manifests, and what runs ocb.
PINNING = ["go/**/go.mod", "go/**/*.yaml", "go/**/*.sh", "go/**/*.ps1", ".github/workflows/*.yml"]
MODULE = re.compile(r"(go\.opentelemetry\.io/collector[\w./-]*|github\.com/open-telemetry/opentelemetry-collector-contrib"
                    r"[\w./-]*)(?: |@)(v\d+\.\d+\.\d+)\b")
TAG = re.compile(r"refs/tags/(v0\.\d+\.\d+)$")


def version(tag: str) -> tuple[int, ...]:
    return tuple(int(part) for part in tag.lstrip("v").split("."))


def pins(root: Path) -> dict[str, dict[str, list[str]]]:
    """Each release pinned, by series ("0.x" or "1.x"): the places that pin it, as path:line."""
    found: dict[str, dict[str, list[str]]] = {"0.x": {}, "1.x": {}}
    paths = sorted({path for pattern in PINNING for path in root.glob(pattern) if path.is_file()})
    for path in paths:
        for number, line in enumerate(path.read_text(errors="replace").splitlines(), start=1):
            for match in MODULE.finditer(line):
                tag = match.group(2)
                series = "0.x" if tag.startswith("v0.") else "1.x"
                found[series].setdefault(tag, []).append(f"{path.relative_to(root)}:{number}")
    return found


def base(found: dict[str, dict[str, list[str]]]) -> str:
    """The one 0.x release pinned. Exits with every pin listed when the pins disagree."""
    problems = []
    for series, tags in found.items():
        if len(tags) > 1:
            lines = [f"  {tag}: {', '.join(places[:5])}{' ...' if len(places) > 5 else ''}"
                     for tag, places in sorted(tags.items(), key=lambda item: version(item[0]))]
            problems.append(f"the collector's {series} modules are pinned at {len(tags)} releases:\n" + "\n".join(lines))
    if not found["0.x"]:
        problems.append("no pin of the collector's 0.x modules found")
    if problems:
        sys.exit("refused: " + "\n".join(problems) + "\nMove every pin to one release together.")
    return next(iter(found["0.x"]))


def git(cwd: Path, *args: str) -> str:
    return subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True, text=True).stdout


def newest(tags_output: str) -> str:
    """The newest vX.Y.Z (0.x) tag in `git ls-remote --tags` output."""
    tags = [m.group(1) for line in tags_output.splitlines() if (m := TAG.search(line.split("\t")[-1]))]
    if not tags:
        sys.exit(f"refused: no v0.x.y tag in {UPSTREAM}")
    return max(tags, key=version)


def changed(ours: str, theirs: str) -> str:
    """`git diff --stat` of the forked directories between two contrib releases."""
    with tempfile.TemporaryDirectory() as tmp:
        repo = Path(tmp)
        git(repo, "init", "-q")
        refs = [f"refs/tags/{tag}:refs/tags/{tag}" for tag in (ours, theirs)]
        git(repo, "fetch", "-q", "--depth", "1", "--filter=blob:none", UPSTREAM, *refs)
        return git(repo, "diff", "--stat=120", ours, theirs, "--", *FORKED)


def summary(text: str) -> None:
    """Add `text` to the job's summary, when run in GitHub Actions."""
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if path:
        with open(path, "a") as out:
            out.write(text + "\n")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=["pins", "latest"])
    parser.add_argument("--report-only", action="store_true",
                        help="latest: print a newer release without failing (a pull request's run)")
    args = parser.parse_args()
    ours = base(pins(ROOT))
    if args.command == "pins":
        print(f"every pin names contrib {ours}")
        return 0
    theirs = newest(git(ROOT, "ls-remote", "--tags", "--refs", UPSTREAM, "refs/tags/v0.*"))
    if version(theirs) <= version(ours):
        print(f"contrib {ours} is the newest release")
        summary(f"Contrib {ours} is the newest release.")
        return 0
    diff = changed(ours, theirs)
    report = (f"contrib {theirs} is out, and the forks are at {ours}. What upstream changed in "
              f"{' and '.join(FORKED)} since then, to port before the pins move:\n\n{diff or '(nothing)'}\n"
              f"{UPSTREAM}/compare/{ours}...{theirs}")
    print(report)
    summary(f"### Contrib {theirs} is out (forks at {ours})\n\n```\n{diff}```\n\n{UPSTREAM}/compare/{ours}...{theirs}")
    return 0 if args.report_only else 1


if __name__ == "__main__":
    sys.exit(main())
