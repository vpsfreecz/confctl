#!/usr/bin/env python3
"""Record/replay the immutable Ruby CLI without external Nix or SSH effects."""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile

sys.dont_write_bytecode = True
from cases import BASELINE, OPTION_GROUPS, ROUTES, cases
from api import MockAPI

HERE = Path(__file__).resolve().parent
GC_BASELINE_OBSERVATIONS = ("gc-baseline-line-error", "gc-child-failure")


def export_baseline(target):
    metadata = json.loads((HERE / "baseline.json").read_text())
    archive = HERE / "baseline.tar.gz"
    assert metadata["revision"] == BASELINE
    assert hashlib.sha256(archive.read_bytes()).hexdigest() == metadata["sha256"]
    target.mkdir(parents=True, exist_ok=True)
    if any(target.iterdir()):
        raise ValueError("baseline export requires an empty directory")
    with tarfile.open(archive) as source:
        source.extractall(target, filter="data")
    return target


def seed(root, case):
    conf = root / "conf"
    conf.mkdir()
    for name in ("home", "store/system", "store/previous", "store/input", "store/rollback", "fake-bin"):
        (root / name).mkdir(parents=True)
    for name in ("nix", "nix-copy-closure", "nix-shell", "ssh", "git", "reboot", "kexec", "curl"):
        (root / "fake-bin" / name).symlink_to(HERE / "child.py")
    (root / "scenario.json").write_text(json.dumps(case))
    if case["setup"] == "empty":
        return conf
    (conf / "flake.nix").write_text("{ outputs = _: {}; }\n")
    for machine in ("alpha", "nested/beta"):
        target = conf / "cluster" / machine
        target.mkdir(parents=True)
        (target / "module.nix").write_text("{}\n")
        (target / "config.nix").write_text("{}\n")
    lock = {"version": 7, "root": "root", "nodes": {
        "root": {"inputs": {"nixpkgs": "nixpkgs"}},
        "nixpkgs": {"original": {"type": "github", "owner": "fixture", "repo": "nixpkgs", "ref": "main"},
                    "locked": {"type": "github", "owner": "fixture", "repo": "nixpkgs", "rev": "a" * 40}}}}
    (conf / "flake.lock").write_text(json.dumps(lock, indent=2) + "\n")
    machines = {}
    output = dict(match=None, include=[], exclude=[])
    check = dict(description="fixture health", command=["fixture-check"], exitStatus=0,
                 standardOutput=output, standardError=output, timeout=1, cooldown=0)
    for name, managed, target, tags in [("alpha", True, "alpha.invalid", ["prod"]),
                                         ("reference", False, "reference.invalid", ["other"]),
                                         ("no-target", True, None, [])]:
        machines[name] = dict(name=name, key=name + "-key", clusterName=name,
                              metaConfig=dict(managed=managed, spin="nixos", tags=tags,
                                              host=dict(target=target, port=2222 if name == "alpha" else 22),
                                              autoRollback=dict(enable=False, timeout=1),
                                              buildGenerations={}, hostGenerations={},
                                              healthChecks=dict(builderCommands=[], machineCommands=[check],
                                                                systemd=dict(enable=False))))
    (root / "machines.json").write_text(json.dumps(machines))
    if case["mode"].startswith("api"):
        machines["alpha"]["metaConfig"].update(container=dict(id=1))
        machines["alpha"]["metaConfig"]["host"].update(name="alpha", location="test", domain="example.invalid")
        (root / "machines.json").write_text(json.dumps(machines))
        (conf / "data/vpsadmin/networks").mkdir(parents=True)
    directory = conf / ".confctl/generations/alpha/2000-01-01--00-00-00"
    directory.mkdir(parents=True)
    info = {"nixpkgs": {"input": "nixpkgs", "rev": "a" * 40, "shortRev": "a" * 8,
                         "url": "github:fixture/nixpkgs"}}
    generation = dict(mode="flakes", date="2000-01-01T00:00:00Z",
                      toplevel=str(root / "store/system"), auto_rollback=str(root / "store/rollback"),
                      inputs={"nixpkgs": str(root / "store/input")}, inputs_info=info)
    (directory / "generation.json").write_text(json.dumps(generation, indent=2) + "\n")
    for name, target in [("toplevel", "system"), ("auto_rollback", "rollback"), ("nixpkgs.input", "input")]:
        (directory / name).symlink_to(root / "store" / target)
    current = "missing" if case["mode"] == "invalid-current" else directory.name
    (directory.parent / "current").symlink_to(current)
    if case["mode"] == "unsupported":
        rejected = directory.parent / "1999-01-01--00-00-00"
        rejected.mkdir()
        (rejected / "generation.json").write_text('{"mode":"swpins"}\n')
    return conf


def snapshot(conf):
    result = {}
    for path in sorted(conf.rglob("*")):
        name = str(path.relative_to(conf))
        if name.startswith(".confctl/logs"):
            continue
        if path.is_symlink():
            result[name] = dict(link=os.readlink(path))
        elif path.is_file():
            result[name] = dict(bytes_b64=base64.b64encode(path.read_bytes()).decode(),
                                mode=oct(path.stat().st_mode & 0o777))
    return result


def normalize(text, root, oracle):
    text = text.replace(str(root), "<CASE>").replace(str(oracle), "<RUBY>")
    text = text.replace(str(HERE), "<HARNESS>")
    # Only volatile filenames used by logger/setter/configuration replacement.
    text = re.sub(r"\d{4}-\d\d-\d\d--\d\d-\d\d-\d\d-confctl-", "<LOG-TIME>-confctl-", text)
    text = re.sub(r"flake\.lock\.tmp-\d+-[0-9a-f]{12}", "flake.lock.tmp-<NONCE>", text)
    text = re.sub(r"\.new-[0-9a-f]{6}", ".new-<NONCE>", text)
    # GLI derives executable name from $0. Both programs are invoked as confctl.
    return text


def normalized_capture(raw, root, oracle):
    result = {key: raw[key] for key in ("exit", "signal")}
    for stream in ("stdout", "stderr"):
        data = base64.b64decode(raw[stream + "_b64"])
        result[stream] = normalize(data.decode("utf-8", errors="surrogateescape"), root, oracle)
    result["events"] = normalize(json.dumps(raw["events"], sort_keys=True), root, oracle)
    files = {}
    for name, value in raw["files"].items():
        value = dict(value)
        if "bytes_b64" in value:
            data = base64.b64decode(value.pop("bytes_b64")).decode("utf-8", errors="surrogateescape")
            value["text"] = normalize(data, root, oracle)
        if "link" in value:
            value["link"] = normalize(value["link"], root, oracle)
        files[name] = value
    result["files"] = files
    if "api" in raw:
        result["api"] = raw["api"]
    return result


def execute(case, command, oracle, evidence):
    with tempfile.TemporaryDirectory(prefix="confctl-core-") as scratch:
        root = Path(scratch)
        conf = seed(root, case)
        env = {key: value for key, value in os.environ.items()
               if key in ("PATH", "GEM_HOME", "GEM_PATH", "BUNDLE_GEMFILE", "BUNDLE_PATH", "SSL_CERT_FILE")}
        env.update(HOME=str(root / "home"), XDG_CACHE_HOME=str(root / "home/cache"),
                   CONFCTL_COMPAT_ROOT=str(root), CONFCTL_TTY="0", NO_COLOR="1", PAGER="",
                   RUBYLIB=str(oracle / "lib"), TZ="UTC", LC_ALL="C.UTF-8", COLUMNS="80",
                   PATH=str(root / "fake-bin") + os.pathsep + env.get("PATH", ""),
                   HTTP_PROXY="http://127.0.0.1:1", HTTPS_PROXY="http://127.0.0.1:1", NO_PROXY="")
        api = None
        if case["mode"].startswith("api"):
            api = MockAPI(case["mode"])
            env.update(CONFCTL_COMPAT_API_URL=api.start(),
                       RUBYOPT="-r" + str(HERE / "api_transport.rb"))
            # Defined uppercase HTTP_PROXY, even empty, makes Ruby warn.
            # Origin/socket guards confine this API transport instead.
            env.pop("HTTP_PROXY", None)
            env.pop("HTTPS_PROXY", None)
            if case["mode"].startswith("api-guard-"):
                env["CONFCTL_COMPAT_API_GUARD_PROBE"] = case["mode"].removeprefix("api-guard-")
        proc = subprocess.Popen(command + case["argv"], cwd=conf, env=env, stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        timed_out = False
        try:
            out, err = proc.communicate(case["stdin"].encode(), timeout=15)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            out, err = proc.communicate()
            timed_out = True
        finally:
            if api:
                api.stop()
        raw = dict(argv=case["argv"], stdin_b64=base64.b64encode(case["stdin"].encode()).decode(),
                   exit=proc.returncode if proc.returncode >= 0 else None,
                   signal=-proc.returncode if proc.returncode < 0 else None,
                   stdout_b64=base64.b64encode(out).decode(), stderr_b64=base64.b64encode(err).decode(),
                   events=[json.loads(line) for line in (root / "events.jsonl").read_text().splitlines()]
                   if (root / "events.jsonl").exists() else [], files=snapshot(conf))
        if api:
            raw["stdin_b64"] = base64.b64encode(b"<synthetic authentication input omitted>").decode()
            raw["api"] = dict(events=api.events, errors=api.errors,
                              origin=json.loads((root / "api-origin.json").read_text())
                              if (root / "api-origin.json").exists() else None)
            if api.errors:
                raise RuntimeError(case["id"] + ": mock API contract violation " + repr(api.errors))
            if b"fixture-password" in out + err:
                raise RuntimeError(case["id"] + ": protected synthetic credential was echoed")
        normalized = normalized_capture(raw, root, oracle)
        if evidence:
            evidence.mkdir(parents=True, exist_ok=True)
            write_json(evidence / (case["id"] + ".json"), dict(raw=raw, normalized=normalized))
        if (root / "unexpected-child").exists():
            raise RuntimeError(case["id"] + ": " + (root / "unexpected-child").read_text())
        if timed_out:
            raise RuntimeError(case["id"] + ": fake-only case exceeded 15s; retained available capture")
        return normalized


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True, ensure_ascii=True) + "\n")


def gc_contract(actual, candidate, child_failure=False):
    """Adjudicated defect, kept separate from byte-exact differential output."""
    calls = json.loads(actual["events"])
    reached = any(item["program"] == "ssh" and item["argv"][-1] == "nix-collect-garbage"
                  for item in calls)
    if candidate:
        expected_exit = 1 if child_failure else 0
        return (actual["exit"] == expected_exit and actual["signal"] is None and
                reached and "undefined method" not in actual["stderr"])
    return (actual["exit"] == 1 and reached and
            "undefined method 'line'" in actual["stderr"] and
            "ConfCtl::NixCollectGarbage::Progress" in actual["stderr"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--export-baseline", type=Path)
    parser.add_argument("--record", action="store_true", help="explicitly replace expected baseline results")
    parser.add_argument("--record-output", type=Path, help="write recorded expectations outside source for later reviewed promotion")
    parser.add_argument("--candidate", type=Path, help="candidate executable, default preserved Ruby oracle")
    parser.add_argument("--expected-dir", type=Path, default=HERE / "expected", help="read expectations from this directory")
    parser.add_argument("--case", action="append", help="exact case ID; repeatable")
    parser.add_argument("--evidence", type=Path, help="retain raw bytes/effects as JSON outside fixtures")
    parser.add_argument("--manifest", action="store_true", help="refresh declarative coverage without running confctl")
    args = parser.parse_args()
    if args.export_baseline:
        export_baseline(args.export_baseline.resolve())
        print(args.export_baseline.resolve())
        return 0
    selected = cases()
    if args.manifest:
        write_json(HERE / "coverage.json", dict(baseline_revision=BASELINE,
                   globals={"color": {"aliases": ["c"], "default": "auto", "values": ["always", "never", "auto"]}},
                   baseline_defects={"GC-01": {"cases": GC_BASELINE_OBSERVATIONS,
                                     "candidate_success_exit": 0, "candidate_child_failure_exit": 1}},
                   groups=OPTION_GROUPS,
                   routes=[dict(path=route, arguments=arguments, options=options,
                                cases=[item["id"] for item in selected if item["route"] == route])
                           for route, arguments, options in ROUTES],
                   cases=selected))
        return 0
    if args.case:
        unknown = set(args.case) - {item["id"] for item in selected}
        if unknown:
            parser.error("unknown cases: " + repr(sorted(unknown)))
        selected = [item for item in selected if item["id"] in args.case]
    if args.record and args.candidate:
        parser.error("record only from preserved Ruby baseline")
    if args.candidate and any(item["mode"].startswith("api") for item in selected):
        parser.error("API candidate replay requires the owning Rust adapter's confined transport; select non-API --case IDs")
    failures = []
    with tempfile.TemporaryDirectory(prefix="confctl-ruby-oracle-") as scratch:
        oracle = export_baseline(Path(scratch))
        command = [str(args.candidate.resolve())] if args.candidate else [shutil.which("ruby"), str(oracle / "bin/confctl")]
        expected_dir = args.record_output if args.record else args.expected_dir
        expected_dir = expected_dir or HERE / "expected"
        for item in selected:
            actual = execute(item, command, oracle, args.evidence)
            expected = expected_dir / (item["id"] + ".json")
            if item["id"] in GC_BASELINE_OBSERVATIONS:
                if not gc_contract(actual, bool(args.candidate), item["mode"] == "gc-failure"):
                    failures.append(item["id"])
                # Raw observation is already in --evidence. Never promote a
                # volatile worker traceback as a candidate success contract.
            elif args.record:
                expected_dir.mkdir(parents=True, exist_ok=True)
                write_json(expected, actual)
            elif not expected.exists() or json.loads(expected.read_text()) != actual:
                failures.append(item["id"])
            print(("FAIL " if item["id"] in failures else "PASS ") + item["id"], flush=True)
    print(f"{len(selected)} cases, {len(failures)} failures")
    return bool(failures)


if __name__ == "__main__":
    sys.exit(main())
