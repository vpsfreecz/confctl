#!/usr/bin/env python3
"""Focused immutable Ruby follows observations; independent of the 372-case set."""

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
VARIATIONS = {
    "single": ["nixpkgs"],
    "nested": ["nixpkgs", "child"],
    "different-suffix": ["nixpkgs", "does-not-exist", "deeper"],
    "missing-first": ["absent", "child"],
}
seed_default = core.seed


def seed(root, case):
    conf = seed_default(root, case)
    path = conf / "flake.lock"
    lock = json.loads(path.read_text())
    lock["nodes"]["root"]["inputs"] = {"alias": VARIATIONS[case["variation"]]}
    lock["nodes"]["nixpkgs"]["inputs"] = {"child": "leaf"}
    lock["nodes"]["leaf"] = {
        "original": {"type": "git", "url": "https://leaf.invalid/repo", "ref": "leaf"},
        "locked": {"type": "git", "url": "https://leaf.invalid/repo", "rev": "c" * 40},
    }
    path.write_text(json.dumps(lock, indent=2) + "\n")
    (root / "fake-bin/nix").unlink()
    (root / "fake-bin/nix").symlink_to(HERE / "child_follows.py")
    return conf


def validate(actual, case):
    """Check discriminating effects before an owner accepts recorded bytes."""
    lock = json.loads(actual["files"]["flake.lock"]["text"])
    assert lock["nodes"]["root"]["inputs"]["alias"] == VARIATIONS[case["variation"]]
    assert lock["nodes"]["leaf"]["locked"]["rev"] == "c" * 40
    missing = case["variation"] == "missing-first"
    setting = case["action"] == "set"
    assert actual["exit"] == (1 if missing and setting else 0)
    assert actual["signal"] is None
    assert len(json.loads(actual["events"])) == (1 if setting and not missing else 0)
    assert lock["nodes"]["nixpkgs"]["locked"]["rev"] == ("b" if setting and not missing else "a") * 40
    assert lock["nodes"]["nixpkgs"]["original"] == {
        "type": "github", "owner": "fixture", "repo": "nixpkgs", "ref": "main"}
    if missing and setting:
        assert actual["stdout"] == ""
        assert actual["stderr"] == (
            "\nLog file: <CASE>/conf/.confctl/logs/<LOG-TIME>-confctl-inputs-set.log\n"
            "error: input 'alias' has unsupported type '-' for set\n")
    elif setting:
        assert actual["stdout"] == "Updated inputs (1):\n  alias: aaaaaaaa -> bbbbbbbb\n"
        assert actual["stderr"] == ""
    elif not missing:
        assert "github" in actual["stdout"] and "aaaaaaaa" in actual["stdout"]
        assert "leaf.invalid" not in actual["stdout"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--record-output", type=Path, help="write observations to an owned evidence directory")
    parser.add_argument("--expected-dir", type=Path, default=HERE / "expected-follows")
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--case", action="append", default=[], help="select exact case IDs")
    args = parser.parse_args()
    if args.record_output:
        args.record_output.mkdir(parents=True, exist_ok=True)
    core.seed = seed
    results = []
    with tempfile.TemporaryDirectory(prefix="confctl-follows-oracle-") as scratch:
        oracle = core.export_baseline(Path(scratch))
        command = [shutil.which("ruby"), str(oracle / "bin/confctl")]
        for variation in VARIATIONS:
            for action in ("ls", "set"):
                argv = ["inputs", "ls"] if action == "ls" else ["inputs", "set", "--no-commit", "--no-editor", "alias", "next"]
                case = dict(id=f"follows-{variation}-{action}", argv=argv, setup="cluster",
                            stdin="", mode="normal", action=action, variation=variation)
                if args.case and case["id"] not in args.case:
                    continue
                actual = core.execute(case, command, oracle, args.evidence)
                validate(actual, case)
                target = (args.record_output or args.expected_dir) / (case["id"] + ".json")
                if args.record_output:
                    core.write_json(target, actual)
                else:
                    assert actual == json.loads(target.read_text()), case["id"]
                results.append(case["id"])
                print("PASS " + case["id"], flush=True)
    if args.case:
        assert set(results) == set(args.case), "unknown or unexecuted case ID"
    core.write_json(args.evidence / "qualification.json", dict(
        baseline=core.BASELINE, cases=results,
        sources={name: hashlib.sha256((HERE / name).read_bytes()).hexdigest()
                 for name in ("baseline.tar.gz", "qualify_inputs.py", "child_follows.py")}))


if __name__ == "__main__":
    main()
