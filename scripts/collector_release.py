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

    scripts/collector_release.py diverged           # how each fork differs from its release
    scripts/collector_release.py port v0.164.0      # merge upstream's changes since into the forks

`diverged` compares each fork with upstream's directory at the pinned release (import paths
rewritten), file by file, and fails when a changed .go file other than a test doesn't say
"Changed here" in its header, or an upstream file is missing that `DROPPED` doesn't name. `port`
three-way merges what upstream changed between the pinned release and a newer one into every
forked file (git merge-file: the pinned release is the base), adds upstream's new files and lists
the ones it deleted; the conflicts it leaves are where both sides changed the same lines. Move
the pins after.
"""
from __future__ import annotations

import argparse
import difflib
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
# Each fork: upstream's directory, this repository's, and the Go module paths that differ.
FORKS = {
    "cmd/opampsupervisor": ("go/opampsupervisor", "github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor",
                            "github.com/pydantic/opamp-clients/go/opampsupervisor"),
    "extension/opampextension": ("go/opampextension", "github.com/open-telemetry/opentelemetry-collector-contrib/extension/opampextension",
                                 "github.com/pydantic/opamp-clients/go/opampextension"),
}
# Upstream files a fork doesn't carry, and why (the forks' READMEs say more).
DROPPED = {
    "cmd/opampsupervisor/Dockerfile": "contrib's build",
    "cmd/opampsupervisor/Makefile": "contrib's build",
    "cmd/opampsupervisor/metadata.yaml": "contrib's mdatagen; the files it generated are checked in",
    "cmd/opampsupervisor/supervisor/packages.go": "agent_update.go installs packages",
    "cmd/opampsupervisor/testdata/supervisor/supervisor_packages_cap.yaml": "packages.go's test config",
    "extension/opampextension/Makefile": "contrib's build",
    "extension/opampextension/config.schema.yaml": "contrib's schema tooling",
}
# Files that differ by nature: the module's own.
OWN = {"go.mod", "go.sum"}


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


def fetch(repo: Path, tag: str) -> None:
    """Upstream's forked directories at tag, checked out in repo (a shallow, sparse clone)."""
    if not (repo / ".git").exists():
        git(repo, "init", "-q")
        git(repo, "sparse-checkout", "set", "--no-cone", *[f"/{d}/" for d in FORKED])
    git(repo, "fetch", "-q", "--depth", "1", "--filter=blob:none", UPSTREAM, f"refs/tags/{tag}:refs/tags/{tag}")
    git(repo, "checkout", "-q", "--force", tag)


def rewrite(text: str, theirs_module: str, ours_module: str) -> str:
    """Upstream's text with its module path rewritten to this repository's."""
    return text.replace(theirs_module, ours_module)


def body(text: str, name: str) -> str:
    """A Go file without its header comment (license, and what changed here), else text. Build
    constraints and other directives (`//go:build`, `// +build`) are code, and stay."""
    if not name.endswith(".go"):
        return text
    lines = text.splitlines(keepends=True)
    directives = [line for line in lines if line.startswith(("//go:", "// +build"))]
    lines = [re.sub(r'^(package \w+) // import ".*"$', r"\1", line.rstrip("\n")) + "\n" if line.startswith("package ") else line
             for line in lines]
    for i, line in enumerate(lines):
        if not line.startswith("//") and line.strip():
            return "".join(directives) + "".join(line for line in lines[i:] if line not in directives)
    return text


def files(directory: Path) -> set[str]:
    return {p.relative_to(directory).as_posix() for p in directory.rglob("*") if p.is_file()}


def diverged(root: Path, upstream: Path) -> tuple[list[str], list[str]]:
    """How each fork differs from upstream at the release checked out in upstream: a report, and
    the problems (a changed file that doesn't say so, an upstream file missing unexplained)."""
    report, problems = [], []
    for theirs_dir, (ours_dir, theirs_module, ours_module) in FORKS.items():
        theirs, ours = upstream / theirs_dir, root / ours_dir
        same, changed_files, missing = 0, [], []
        for name in sorted(files(theirs)):
            path = ours / name
            if not path.exists():
                missing.append(name)
                continue
            if name in OWN:
                continue
            up = rewrite((theirs / name).read_text(errors="replace"), theirs_module, ours_module)
            mine = path.read_text(errors="replace")
            if body(up, name) == body(mine, name):
                same += 1
                continue
            lines = sum(1 for line in difflib.unified_diff(up.splitlines(), mine.splitlines(), lineterm="", n=0)
                        if line[:1] in "+-" and not line.startswith(("+++", "---")))
            changed_files.append((name, lines))
            if name.endswith(".go") and not name.endswith("_test.go") and "Changed here" not in "\n".join(mine.splitlines()[:20]):
                problems.append(f"{ours_dir}/{name} differs from upstream ({lines} lines) and its header doesn't say "
                                f"\"Changed here\" and what")
        for name in missing:
            if f"{theirs_dir}/{name}" not in DROPPED:
                problems.append(f"upstream's {theirs_dir}/{name} is missing from {ours_dir}, and DROPPED doesn't say why")
        new = sorted(files(ours) - files(theirs))
        report.append(f"{ours_dir} ({theirs_dir}): {same} files as upstream's, {len(changed_files)} changed "
                      f"({sum(n for _, n in changed_files)} lines), {len(missing)} not carried, {len(new)} of its own")
        report += [f"  {n:5d}  {name}" for name, n in sorted(changed_files, key=lambda item: -item[1])]
    return report, problems


def port(root: Path, old: Path, upstream: Path, ours_tag: str, theirs_tag: str) -> list[str]:
    """Three-way merge of upstream's changes from ours_tag (checked out in old) to theirs_tag
    (in upstream) into the forks: what happened to each file upstream changed."""
    with tempfile.TemporaryDirectory() as tmp:
        out = []
        for theirs_dir, (ours_dir, theirs_module, ours_module) in FORKS.items():
            before, after, ours = old / theirs_dir, upstream / theirs_dir, root / ours_dir
            for name in sorted(files(before) | files(after)):
                if name in OWN or f"{theirs_dir}/{name}" in DROPPED:
                    continue
                a, b = before / name, after / name
                text_a = rewrite(a.read_text(errors="replace"), theirs_module, ours_module) if a.exists() else None
                text_b = rewrite(b.read_text(errors="replace"), theirs_module, ours_module) if b.exists() else None
                if text_a == text_b:
                    continue
                mine = ours / name
                if text_b is None:
                    out.append(f"deleted upstream: {ours_dir}/{name}")
                elif not mine.exists():
                    mine.parent.mkdir(parents=True, exist_ok=True)
                    mine.write_text(text_b)
                    out.append(f"added: {ours_dir}/{name}")
                else:
                    base_file, theirs_file = Path(tmp) / "base", Path(tmp) / "theirs"
                    base_file.write_text(text_a or "")
                    theirs_file.write_text(text_b)
                    merged = subprocess.run(["git", "merge-file", "-L", "ours", "-L", ours_tag, "-L", theirs_tag,
                                             str(mine), str(base_file), str(theirs_file)], capture_output=True)
                    out.append(f"{'CONFLICT' if merged.returncode else 'merged'}: {ours_dir}/{name}")
        return out


def summary(text: str) -> None:
    """Add `text` to the job's summary, when run in GitHub Actions."""
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if path:
        with open(path, "a") as out:
            out.write(text + "\n")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=["pins", "latest", "diverged", "port"])
    parser.add_argument("release", nargs="?", help="port: the contrib release to merge in")
    parser.add_argument("--report-only", action="store_true",
                        help="latest: print a newer release without failing (a pull request's run)")
    args = parser.parse_args()
    ours = base(pins(ROOT))
    if args.command == "pins":
        print(f"every pin names contrib {ours}")
        return 0
    if args.command in ("diverged", "port"):
        with tempfile.TemporaryDirectory() as tmp:
            upstream = Path(tmp)
            if args.command == "port":
                if not args.release or version(args.release) <= version(ours):
                    sys.exit(f"refused: port needs a contrib release newer than {ours}")
                old = Path(tmp) / "old"
                new = Path(tmp) / "new"
                old.mkdir()
                new.mkdir()
                fetch(old, ours)
                fetch(new, args.release)
                for line in (out := port(ROOT, old, new, ours, args.release)):
                    print(line)
                conflicts = sum(line.startswith("CONFLICT") for line in out)
                print(f"{conflicts} files with conflicts to resolve; then move the pins to {args.release}")
                return 1 if conflicts else 0
            fetch(upstream, ours)
            report, problems = diverged(ROOT, upstream)
        print(f"the forks against contrib {ours}:")
        print("\n".join(report))
        summary("```\n" + "\n".join(report) + "\n```")
        if problems:
            print("refused:\n" + "\n".join(f"  {p}" for p in problems))
            return 1
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
