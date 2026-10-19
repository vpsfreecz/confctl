#!/usr/bin/env python3
"""Focused settings lifetime routes against the immutable Ruby baseline."""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import sys
import tempfile

sys.dont_write_bytecode = True
import run as core

HERE = Path(__file__).resolve().parent
seed_default = core.seed


def settings(jobs, *, columns=None, impure=False):
    return dict(list=dict(columns=columns or ["name", "spin", "host.target"]),
                nix=dict(maxJobs=jobs, impureEval=impure, legacyNixPath=False),
                buildGenerations=dict(min=1, max=10, maxAge=9999999999),
                hostGenerations=dict(min=1, max=10, maxAge=9999999999))


def cases():
    normal = [settings(4, columns=["name"]), settings(9, columns=["spin"], impure=True)]
    rows = [
        ("two-read", ["ls"], normal, ""),
        ("null-jobs", ["ls"], [settings(None), settings(9)], ""),
        ("selection-trace-ignored", ["ls", "--show-trace"], normal, ""),
        ("output-columns", ["ls", "-H", "-o", "name,managed"], normal, ""),
        ("attributes", ["ls", "--show-trace", "-L"], normal, ""),
        ("bootstrap-failure", ["ls"], [dict(exit=7, error="fixture bootstrap failure")], ""),
        ("client-failure", ["ls"], [normal[0], dict(exit=8, error="fixture client settings failure")], ""),
        ("build-third", ["build", "--yes", "--show-trace", "--max-jobs", "2", "--cores", "3", "alpha"],
         [settings(4), settings(9), settings(13, impure=True)], ""),
        ("build-declined", ["build", "--show-trace", "--max-jobs", "2", "--cores", "3", "alpha"],
         [settings(4), settings(9)], "n\n"),
        ("lock-only", ["inputs", "ls"], [], ""),
    ]
    return [dict(id="settings-" + name, argv=argv, setup="cluster", stdin=stdin,
                 mode="normal", settings_replies=replies) for name, argv, replies, stdin in rows]


def seed(root, case):
    conf = seed_default(root, case)
    (root / "fake-bin/nix").unlink()
    (root / "fake-bin/nix").symlink_to(HERE / "child_settings.py")
    return conf


def validate(actual, case):
    events = json.loads(actual["events"])
    reads = [event for event in events if event["argv"][-1:] == [".#confctl.settings"]]
    assert len(reads) == len(case["settings_replies"]), case["id"]
    assert actual["signal"] is None
    if not reads:
        assert events == [] and actual["exit"] == 0
        return
    pure = ["eval", "--json", "--no-write-lock-file", "--no-update-lock-file"]
    assert reads[0]["argv"] == pure + ["--option", "max-jobs", "auto", ".#confctl.settings"]
    if case["id"] == "settings-bootstrap-failure":
        assert len(events) == 1 and actual["exit"] == 1
        return
    jobs = case["settings_replies"][0]["nix"]["maxJobs"]
    captured = [] if jobs is None else ["--option", "max-jobs", str(jobs)]
    assert reads[1]["argv"] == pure + captured + [".#confctl.settings"]
    if case["id"] == "settings-client-failure":
        assert len(events) == 2 and actual["exit"] == 1
        return
    if case["id"] == "settings-build-declined":
        assert actual["exit"] == 1 and len(events) == 3
        # Candidate diagnostic information is checked by compare_frame, not Ruby prose.
        assert all("--show-trace" not in event["argv"] and "cores" not in event["argv"] for event in events)
        return
    assert actual["exit"] == 0
    if case["id"] == "settings-build-third":
        assert reads[2]["argv"] == pure + ["--show-trace", "--option", "max-jobs", "2",
                                                    "--option", "cores", "3", ".#confctl.settings"]
        assert events[2]["argv"][-1] == ".#confctl.machinesJson"
        assert events[3] == reads[2]
        assert all("--show-trace" in event["argv"] for event in events[3:])
        assert all("13" not in event["argv"] for event in events[3:])
    else:
        assert all("--show-trace" not in event["argv"] and "cores" not in event["argv"] for event in events)
    if case["id"] in ("settings-two-read", "settings-selection-trace-ignored"):
        # Candidate field/host information is checked by compare_frame.
        assert "--impure" in events[-1]["argv"]
        assert "9" not in events[-1]["argv"]
    if case["id"] == "settings-null-jobs":
        assert "max-jobs" not in events[-1]["argv"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--record-output", type=Path)
    parser.add_argument("--expected-dir", type=Path, default=HERE / "expected-settings")
    parser.add_argument("--candidate", type=Path)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--case", action="append", default=[])
    args = parser.parse_args()
    if args.record_output and args.candidate:
        parser.error("record only from the preserved Ruby baseline")
    args.evidence.mkdir(parents=True, exist_ok=True)
    if args.record_output:
        args.record_output.mkdir(parents=True, exist_ok=True)
    selected = [case for case in cases() if not args.case or case["id"] in args.case]
    if args.case and set(args.case) != {case["id"] for case in selected}:
        parser.error("unknown case ID")
    core.seed = seed
    executed = []
    failures, unqualified_cases = [], []
    with tempfile.TemporaryDirectory(prefix="confctl-settings-oracle-") as scratch:
        oracle = core.export_baseline(Path(scratch))
        command = [str(args.candidate.resolve())] if args.candidate else [shutil.which("ruby"), str(oracle / "bin/confctl")]
        for case in selected:
            actual = core.execute(case, command, oracle, args.evidence / "raw")
            try:
                validate(actual, case)
            except AssertionError:
                failures.append(case["id"])
                executed.append(case["id"])
                print("FAIL validation " + case["id"], flush=True)
                continue
            target = (args.record_output or args.expected_dir) / (case["id"] + ".json")
            if args.record_output:
                core.write_json(target, actual)
            else:
                comparison = core.compare_frame(case, json.loads(target.read_text()), actual,
                                                candidate=bool(args.candidate))
                if comparison["outcome"] == "DIFFERENT":
                    failures.append(case["id"])
                elif comparison["outcome"] == "UNQUALIFIED":
                    unqualified_cases.append(case["id"])
                if comparison["observations"] or comparison["outcome"] != "MATCH":
                    print("DETAIL " + case["id"] + ": " + json.dumps(comparison, sort_keys=True), flush=True)
            label = "FAIL " if case["id"] in failures else "UNQUALIFIED " if case["id"] in unqualified_cases else "PASS "
            print(label + case["id"], flush=True)
            executed.append(case["id"])
    core.write_json(args.evidence / "qualification.json", dict(
        baseline=core.BASELINE, candidate=str(args.candidate) if args.candidate else None,
        cases=executed, failures=failures, unqualified=unqualified_cases, sources={name: hashlib.sha256((HERE / name).read_bytes()).hexdigest()
                                for name in ("baseline.tar.gz", "qualify_settings.py", "child_settings.py")}))
    return bool(failures or unqualified_cases)


if __name__ == "__main__":
    sys.exit(main())
