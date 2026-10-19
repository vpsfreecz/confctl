#!/usr/bin/env python3
"""Focused full channel-list routes against the immutable Ruby baseline."""

import argparse
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile

sys.dont_write_bytecode = True
import run as core

HERE = Path(__file__).resolve().parent
seed_original = core.seed
snapshot_original = core.snapshot
observed_logs = []
held_writer = None
writer_identity = None


def settings(jobs="auto", impure=False):
    return dict(list=dict(columns=["name"]),
                nix=dict(maxJobs=jobs, impureEval=impure, legacyNixPath=False),
                buildGenerations=dict(min=1, max=10, maxAge=9999999999),
                hostGenerations=dict(min=1, max=10, maxAge=9999999999))


def cases():
    dense = {"zeta": {"second": "alias", "first": "missing", "empty": "empty"},
             "alpha": {"nixpkgs": "nixpkgs"}, "null": None, "false": False}
    rows = [
        ("sorted", [], {}),
        ("glob", ["a*"], {}),
        ("no-match", ["does-not-match*"], {}),
        ("explicit-order", [" { zeta, alpha, zeta,, absent } "], {}),
        ("comma-order", ["zeta,alpha"], {}),
        ("non-object", [], dict(channels=[])),
        ("empty-mappings", [], dict(channels={"null": None, "false": False})),
        ("non-string-inputs", [], dict(channels={"base": {"null": None, "false": False, "number": 22, "array": [], "object": {}}})),
        ("mapping-scalar", [], dict(channels={"broken": 7}, expected_exit=1)),
        ("mapping-array", [], dict(channels={"base": [["nixpkgs", "nixpkgs"]]})),
        ("settings-lifetimes", [], dict(settings=[settings(4), settings(9, True)])),
        ("null-jobs", [], dict(settings=[settings(None), settings(9, True)])),
        ("bootstrap-failure", [], dict(fail_at=0, expected_exit=1)),
        ("client-failure", [], dict(fail_at=1, expected_exit=1)),
        ("eval-failure", [], dict(fail_at=2, expected_exit=1)),
        ("missing-lock", [], dict(lock="missing", expected_exit=1)),
        ("corrupt-lock", [], dict(lock="corrupt", expected_exit=1)),
        ("writer-held", [], dict(writer_held=True)),
        ("surplus-valid", ["base", "unexpected"], dict(expected_exit=64)),
        ("surplus-missing-flake", ["base", "unexpected"], dict(setup="empty", expected_exit=1)),
    ]
    result = []
    for name, arguments, options in rows:
        replies = [dict(value=value) for value in options.get("settings", [settings(), settings()])]
        channel_reply = dict(value=options.get("channels", dense))
        if "fail_at" in options:
            fail_at = options["fail_at"]
            failure = dict(exit=7 + fail_at, error=f"fixture channel stage {fail_at} failure")
            if fail_at < 2:
                replies[fail_at] = failure
            else:
                channel_reply = failure
        result.append(dict(id="channels-" + name, argv=["inputs", "channel", "ls", *arguments],
            setup=options.get("setup", "cluster"), stdin="", mode="normal",
            settings_replies=replies, channels_reply=channel_reply, lock=options.get("lock", "dense"),
            fail_at=options.get("fail_at"), writer_held=options.get("writer_held", False),
            expected_exit=options.get("expected_exit", 0)))
    return result


def seed(root, case):
    global held_writer, writer_identity
    conf = seed_original(root, case)
    (root / "fake-bin/nix").unlink()
    (root / "fake-bin/nix").symlink_to(HERE / "child_channels.py")
    if case["setup"] == "empty" or "surplus" in case["id"]:
        return conf
    lock_path = conf / "flake.lock"
    if case["lock"] == "missing":
        lock_path.unlink()
    elif case["lock"] == "corrupt":
        lock_path.write_text("{bad\n")
    else:
        lock = json.loads(lock_path.read_text())
        lock["nodes"]["root"]["inputs"].update(alias=["target", "ignored"], empty="empty")
        lock["nodes"]["target"] = dict(locked=dict(rev="abcdefgh-more", url="git+https://target.invalid"))
        lock["nodes"]["empty"] = dict(locked=dict(rev="", url=""))
        lock_path.write_text(json.dumps(lock, indent=2) + "\n")
    if case["writer_held"]:
        path = conf / ".confctl/mutation.lock"
        path.write_text("cooperating writer fixture\n")
        held_writer = path.open("r+")
        fcntl.flock(held_writer, fcntl.LOCK_EX | fcntl.LOCK_NB)
        stat = path.stat()
        writer_identity = (stat.st_dev, stat.st_ino, path.read_bytes())
    return conf


def snapshot(conf):
    for path in sorted((conf / ".confctl/logs").glob("*")):
        if path.is_file():
            observed_logs.append(dict(path=str(path.relative_to(conf)), mode=oct(path.stat().st_mode & 0o777),
                bytes_b64=base64.b64encode(path.read_bytes()).decode()))
    if held_writer:
        path = conf / ".confctl/mutation.lock"
        stat = path.stat()
        assert (stat.st_dev, stat.st_ino, path.read_bytes()) == writer_identity
    return snapshot_original(conf)


def validate(actual, case):
    assert actual["signal"] is None
    assert actual["exit"] == case["expected_exit"]
    events = json.loads(actual["events"])
    no_children = "surplus" in case["id"] or case["lock"] in ("missing", "corrupt")
    count = 0 if no_children else (case["fail_at"] + 1 if case["fail_at"] is not None else 3)
    assert len(events) == count
    pure = ["eval", "--json", "--no-write-lock-file", "--no-update-lock-file"]
    jobs = case["settings_replies"][0].get("value", {}).get("nix", {}).get("maxJobs")
    captured = [] if jobs is None else ["--option", "max-jobs", str(jobs)]
    expected = [pure + ["--option", "max-jobs", "auto", ".#confctl.settings"],
                pure + captured + [".#confctl.settings"]]
    impure = case["settings_replies"][1].get("value", {}).get("nix", {}).get("impureEval")
    expected.append(["eval", "--json"] + (["--impure"] if impure else []) + pure[2:] + captured + [".#confctl.channels"])
    for index, event in enumerate(events):
        assert event == dict(program="nix", argv=expected[index], stdin_hex="", cwd="<CASE>/conf")
    assert (".confctl/mutation.lock" in actual["files"]) == case["writer_held"]
    assert len(actual["retained_logs"]) == (1 if case["expected_exit"] else 0)


def expected_frame(directory, case, candidate):
    path = directory / (case["id"] + ".json")
    raw = path.read_bytes()
    expected = json.loads(raw)
    if not candidate or case["id"] != "channels-corrupt-lock":
        return expected, None
    # This named decision replaces one complete expectation. Actual stderr and
    # the immutable Ruby truth are never rewritten or filtered.
    manifest = json.loads((HERE / "channels-manifest.json").read_bytes())
    binding = manifest["candidate_overrides"][case["id"]]
    assert binding["decision"] == "LOCK-JSON-01"
    override_raw = (HERE / binding["path"]).read_bytes()
    assert hashlib.sha256(override_raw).hexdigest() == binding["sha256"]
    override = json.loads(override_raw)
    assert override["decision"] == "LOCK-JSON-01" and override["case"] == case["id"]
    assert hashlib.sha256(raw).hexdigest() == override["baseline_sha256"]
    assert override["changed_fields"] == ["stderr"]
    assert {key for key in expected if expected[key] != override["expected"][key]} == {"stderr"}
    assert set(expected) == set(override["expected"])
    return override["expected"], override["decision"]


def main():
    global held_writer, writer_identity
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--record-output", type=Path)
    parser.add_argument("--expected-dir", type=Path, default=HERE / "expected-channels")
    parser.add_argument("--candidate", type=Path)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--case", action="append", default=[])
    args = parser.parse_args()
    if args.record_output and args.candidate:
        parser.error("record only from the archived Ruby baseline")
    selected = [case for case in cases() if not args.case or case["id"] in args.case]
    if args.case and set(args.case) != {case["id"] for case in selected}:
        parser.error("unknown case ID")
    args.evidence.mkdir(parents=True, exist_ok=True)
    if args.record_output:
        args.record_output.mkdir(parents=True, exist_ok=True)
    core.seed = seed
    core.snapshot = snapshot
    failures, unqualified_cases = [], []
    overrides = {}
    with tempfile.TemporaryDirectory(prefix="confctl-channels-oracle-") as scratch:
        oracle = core.export_baseline(Path(scratch))
        command = [str(args.candidate.resolve())] if args.candidate else [shutil.which("ruby"), str(oracle / "bin/confctl")]
        for case in selected:
            observed_logs.clear()
            try:
                actual = core.execute(case, command, oracle, args.evidence / "raw")
                actual["retained_logs"] = [dict(path=core.normalize(log["path"], Path("/unused"), oracle),
                                               mode=log["mode"]) for log in observed_logs]
                core.write_json(args.evidence / (case["id"] + "-logs.json"), list(observed_logs))
                validate(actual, case)
                if args.record_output:
                    core.write_json(args.record_output / (case["id"] + ".json"), actual)
                else:
                    expected, decision = expected_frame(args.expected_dir, case, args.candidate)
                    if decision:
                        overrides[case["id"]] = decision
                    comparison = core.compare_frame(case, expected, actual, candidate=bool(args.candidate))
                    if comparison["outcome"] == "DIFFERENT":
                        failures.append(case["id"])
                    elif comparison["outcome"] == "UNQUALIFIED":
                        unqualified_cases.append(case["id"])
                    if comparison["observations"] or comparison["outcome"] != "MATCH":
                        print("DETAIL " + case["id"] + ": " + json.dumps(comparison, sort_keys=True), flush=True)
            except (AssertionError, RuntimeError) as error:
                failures.append(case["id"])
                print(f"FAIL {case['id']}: {error}", flush=True)
            finally:
                if held_writer:
                    held_writer.close()
                    held_writer = None
                    writer_identity = None
            label = "FAIL " if case["id"] in failures else "UNQUALIFIED " if case["id"] in unqualified_cases else "PASS "
            print(label + case["id"], flush=True)
    core.write_json(args.evidence / "qualification.json", dict(baseline=core.BASELINE,
        cases=[case["id"] for case in selected], failures=failures, unqualified=unqualified_cases, candidate=str(args.candidate) if args.candidate else None,
        candidate_overrides=overrides,
        sources={name: hashlib.sha256((HERE / name).read_bytes()).hexdigest()
                 for name in ["baseline.tar.gz", "qualify_channels.py", "child_channels.py"]},
        log_scope="Exact retained log names/modes; original raw log contents kept separately, without content normalization/parity claim"))
    return bool(failures or unqualified_cases)


if __name__ == "__main__":
    sys.exit(main())
