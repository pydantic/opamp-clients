#!/usr/bin/env python3
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""Differential runs of the SDKs: each scenario's core taps, client against client.

    conformance/differential/shells.py                 # every scenario and transport in conformance/out
    conformance/differential/shells.py --scenario packages --client go-core-platform,java-core-platform
    conformance/differential/shells.py --strict        # exit 1 on a difference shells.toml doesn't allow

Every SDK runs the same core, so on one scenario what each tells its core, and what the core decides,
should differ only in timing. For each scenario and transport that the reference client (REFERENCE,
or --reference) and another ran, `opamp-tap diff` reduces each client's taps to what doesn't
depend on timing (the tap checker's diff says what is kept and what is dropped) and says where
each parts from the reference, category by category, with the lines around it. A difference
`shells.toml` lists is allowed, with its reason; any other fails --strict, and the report shows
the scenario, the two clients and the first lines where they part. So does a run the reference has
none of: it was compared with nothing.

The report goes to stdout and to conformance/out/differential.txt (and .json).
"""
from __future__ import annotations

import argparse
import fnmatch
import json
import os
import re
import subprocess
import sys
import tomllib
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
OUT = ROOT / "conformance" / "out"
ALLOWED = HERE / "shells.toml"
# Where the core's sources are: OPAMP_CORE_ROOT (a checkout with them) if set, as in
# conformance/runner/run.py, else the root, which has none.
CORE_ROOT = Path(os.environ["OPAMP_CORE_ROOT"]).resolve() if os.environ.get("OPAMP_CORE_ROOT") else ROOT
# The client the others are compared with: the Rust client where the core's sources are to build it
# from, and go-core-platform where they aren't.
CORE_SOURCE = CORE_ROOT != ROOT
# opamp-tap: built from those sources, or the one scripts/fetch_core.py fetch put in the root's target/.
TAP = (CORE_ROOT if CORE_SOURCE else ROOT) / "target" / "release" / "opamp-tap"
REFERENCE = "rust" if CORE_SOURCE else "go-core-platform"


def _packages(line: str) -> tuple[str, frozenset[str]] | None:
    """A save's line with its packages' `failed` marks taken out, and the packages marked failed;
    None for a line that isn't a save with packages."""
    m = re.search(r"pkgs\(([^)]*)\)", line)
    if not line.startswith(("store ", "start from ")) or not m:
        return None
    entries = m.group(1).split(",")
    failed = frozenset(e.split(":", 1)[0] for e in entries if " failed" in e.split(">", 1)[-1])
    bare = ",".join(e.replace(" failed", "") for e in entries)
    return line[:m.start(1)] + bare + line[m.end(1):], failed


def package_failures_grouped(diff: str) -> bool:
    """Whether the saves of two clients differ only in how package failures were grouped into
    saves: failures that reach the core in one step make one save, in two steps two, and which
    share a save is timing. So every line that differs is a save of the same state but for which
    packages are marked failed, each answered stored; on each side, from the save both had before
    to the save both reach after, failures only accumulate (none is taken back); and both reach a
    save in common after (a difference at the end is where they ended, not how they got there).
    "The same state" is the state of the save before, or of the save after when that one records
    more than failures (a download started in the step that brought the last failure)."""
    segments: list[list[tuple[str, str]]] = [[]]
    for raw in diff.splitlines():
        if raw.strip() == "...":
            segments.append([])
        elif len(raw) >= 2 and raw[0] in " -+" and raw[1] == " ":
            segments[-1].append((raw[0], raw[2:]))
        else:
            return False
    found = False
    for seg in segments:
        i = 0
        while i < len(seg):
            if seg[i][0] == " ":
                i += 1
                continue
            j = i
            while j < len(seg) and seg[j][0] != " ":
                j += 1
            hunk = seg[i:j]
            before = next((p for _, ln in reversed(seg[:i]) if (p := _packages(ln))), None)
            after = next((p for _, ln in seg[j:] if (p := _packages(ln))), None)
            if after is None:
                return False
            # Each save is answered stored. When both made as many saves, the diff lines the last
            # answer up as one unchanged line after them: it may be missing from both sides alike,
            # and must then be the line that follows.
            sides = []
            for side in "-+":
                lines = [ln for m, ln in hunk if m == side]
                stores = [ln for ln in lines if ln.startswith("store ")]
                if len(stores) + lines.count("stored") != len(lines):
                    return False
                sides.append((stores, len(stores) - lines.count("stored")))
            unanswered = {n for _, n in sides}
            if unanswered == {1}:
                if j >= len(seg) or seg[j] != (" ", "stored"):
                    return False
            elif unanswered != {0}:
                return False
            for stores, _ in sides:
                chain = ([before] if before else []) + [_packages(ln) for ln in stores] + [after]
                if any(p is None for p in chain):
                    return False
                # But for failures, each is the state of the save before or of the save after (which
                # may record more: a download started), and none goes back to the one before.
                states = [p[0] for p in chain]
                moved = states.index(after[0])
                first = before[0] if before else after[0]
                if any(st != first for st in states[:moved]) or any(st != after[0] for st in states[moved:]):
                    return False
                if any(not a[1] <= b[1] for a, b in zip(chain, chain[1:])):
                    return False
            found = True
            i = j
    return found


#: Differences a rule allows by what its whole diff shows (`grouping` names one), beyond the
#: first lines its `first` matches.
GROUPINGS = {"package-failures": package_failures_grouped}


def load_rules(path: Path = ALLOWED) -> list[dict]:
    rules = tomllib.loads(path.read_text()).get("allow", []) if path.exists() else []
    for r in rules:
        if not r.get("reason"):
            sys.exit(f"{path}: every allowed difference needs a reason: {r}")
        if r.get("grouping", next(iter(GROUPINGS))) not in GROUPINGS:
            sys.exit(f"{path}: an unknown grouping (one of {sorted(GROUPINGS)}): {r}")
    return rules


def allowed(rules: list[dict], client: str, scenario: str, transport: str, category: str, first: str,
            diff: str = "") -> dict | None:
    """The rule that allows this difference, if one does: `diff` is the whole of it, for a rule
    with a `grouping`."""
    for r in rules:
        if not any(fnmatch.fnmatch(client, c) for c in r.get("clients", ["*"])):
            continue
        if not any(fnmatch.fnmatch(scenario, s) for s in r.get("scenarios", ["*"])):
            continue
        if transport not in r.get("transports", ["http", "ws"]):
            continue
        if r.get("category", category) != category:
            continue
        if "first" in r and not re.search(r["first"], first):
            continue
        if "grouping" in r and not GROUPINGS[r["grouping"]](diff):
            continue
        return r
    return None


def runs(out: Path, reference: str, clients: list[str], scenario_filter: str) -> dict[str, dict[str, Path]]:
    """scenario-transport -> client -> its run's directory, for the runs that wrote core taps."""
    found: dict[str, dict[str, Path]] = {}
    # bin holds the drivers, torture the nightly harness's runs (conformance/torture): no client's.
    for client_dir in sorted(p for p in out.iterdir() if p.is_dir() and p.name not in ("bin", "torture")):
        if client_dir.name.endswith("-server"):
            continue
        if clients and client_dir.name != reference and client_dir.name not in clients:
            continue
        for run in sorted(client_dir.iterdir()):
            if not run.is_dir() or not any(run.glob("core-*.tap*")):
                continue
            if scenario_filter and not any(f in run.name for f in scenario_filter.split(",")):
                continue
            found.setdefault(run.name, {})[client_dir.name] = run
    return found


def compare(out: Path, reference: str, clients: list[str], scenario_filter: str, rules: list[dict],
            tap: Path = TAP) -> tuple[list[str], list[dict]]:
    """The report's lines, and every difference (allowed or not)."""
    lines: list[str] = []
    found: list[dict] = []
    for name, by_client in sorted(runs(out, reference, clients, scenario_filter).items()):
        scenario, transport = name.rsplit("-", 1)
        others = [c for c in by_client if c != reference]
        if not others:
            continue
        # A run the reference has none of was compared with nothing: a difference no rule allows,
        # or a change to one SDK, whose family ran alone, would pass having compared nothing.
        if reference not in by_client:
            lines.append(f"{name}: no run of {reference} to compare {', '.join(others)} with")
            found += [{"scenario": scenario, "transport": transport, "client": c, "category": "no-reference",
                       "first": f"no run of {reference}", "allowed": None} for c in others]
            continue
        cmd = [str(tap), "diff", "--json", f"{reference}={by_client[reference]}"]
        cmd += [f"{c}={by_client[c]}" for c in others]
        proc = subprocess.run(cmd, capture_output=True, text=True)
        if proc.returncode != 0:
            lines.append(f"{name}: opamp-tap diff failed: {proc.stderr.strip()}")
            found.append({"scenario": scenario, "transport": transport, "client": "?", "category": "error",
                          "first": proc.stderr.strip(), "allowed": None})
            continue
        report = json.loads(proc.stdout)["clients"]
        same = [c for c in others if not report[c]["differences"] and not report[c]["config"]]
        head = f"{name}: {len(same)} of {len(others)} the same as {reference}"
        lines.append(head + (f" ({', '.join(same)})" if same else ""))
        for c in others:
            r = report[c]
            diffs = [("config", f"{k}: {a} | {b}", "") for k, a, b in r["config"]]
            diffs += [(d["category"], f"- {d['first'][0]} | + {d['first'][1]}", d["diff"]) for d in r["differences"]]
            for category, first, diff in diffs:
                rule = allowed(rules, c, scenario, transport, category, first, diff)
                found.append({"scenario": scenario, "transport": transport, "client": c, "category": category,
                              "first": first, "diff": diff, "allowed": rule and rule["reason"]})
                mark = "allowed" if rule else "DIFFERS"
                lines.append(f"  {mark} {c} {category}: {first}" + (f"  ({rule['reason']})" if rule else ""))
                if not rule and diff:
                    lines += [f"      {ln}" for ln in diff.splitlines()]
    return lines, found


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", default=str(OUT), help="the conformance output directory (default conformance/out)")
    ap.add_argument("--reference", default=REFERENCE, help=f"the client the others are compared with (default {REFERENCE})")
    ap.add_argument("--client", default="", help="comma-separated clients to compare (default: every one that ran)")
    ap.add_argument("--scenario", default="", help="comma-separated substrings of scenario names")
    ap.add_argument("--strict", action="store_true",
                    help="exit 1 on a difference shells.toml doesn't allow, or a run the reference has none of")
    args = ap.parse_args()
    out = Path(args.out)
    if not TAP.exists():
        if not CORE_SOURCE:
            sys.exit(f"no {TAP.relative_to(ROOT)}: run scripts/fetch_core.py fetch")
        subprocess.run(["cargo", "build", "--release", "-p", "opamp-tap"], cwd=CORE_ROOT, check=True)
    lines, found = compare(out, args.reference, [c for c in args.client.split(",") if c], args.scenario,
                           load_rules())
    bad = [d for d in found if not d["allowed"]]
    lines.append(f"\n{len(found)} differences, {len(found) - len(bad)} allowed, {len(bad)} not")
    text = "\n".join(lines)
    print(text)
    if out.exists():
        (out / "differential.txt").write_text(text + "\n")
        (out / "differential.json").write_text(json.dumps(found, indent=1))
    return 1 if args.strict and bad else 0


if __name__ == "__main__":
    sys.exit(main())
