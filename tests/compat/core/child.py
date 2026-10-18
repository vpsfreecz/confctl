#!/usr/bin/env python3
"""Closed fake transport: unknown commands fail instead of reaching a host."""

import json
import os
from pathlib import Path
import shlex
import sys


def main():
    root = Path(os.environ["CONFCTL_COMPAT_ROOT"])
    config = json.loads((root / "scenario.json").read_text())
    program = Path(sys.argv[0]).name
    args = sys.argv[1:]
    command = shlex.split(args[-1]) if program == "ssh" and args else []
    # tty-command intentionally leaves stdin open when no input was supplied.
    # Only consumers of an explicit script/payload read it to EOF.
    data = sys.stdin.buffer.read() if program == "ssh" and command in (["bash", "--norc"], ["cat"]) else b""
    event = dict(program=program, argv=args, stdin_hex=data.hex())
    with (root / "events.jsonl").open("a") as stream:
        stream.write(json.dumps(event) + "\n")
    mode = config["mode"]
    store = str(root / "store")
    info = {"nixpkgs": {"input": "nixpkgs", "rev": "a" * 40, "shortRev": "a" * 8,
                        "url": "github:fixture/nixpkgs"}}
    if program == "nix":
        if "eval" in args:
            target = args[-1]
            if target == ".#confctl.settings":
                emit({"list": {"columns": ["name", "spin", "host.target"]},
                      "nix": {"maxJobs": "auto", "impureEval": False, "legacyNixPath": False},
                      "buildGenerations": {"min": 1, "max": 10, "maxAge": 9999999999},
                      "hostGenerations": {"min": 1, "max": 10, "maxAge": 9999999999}})
            elif target == ".#confctl.channels":
                emit({"base": {"nixpkgs": "nixpkgs"}, "other": {"nixpkgs": "nixpkgs"}})
            elif target == ".#confctl.moduleOptions":
                emit([{"name": "cluster.<name>.spin"}, {"name": "cluster.<name>.host.target"}])
            elif target == ".#confctl.machineKeys":
                emit({"alpha": "alpha-key", "reference": "reference-key", "no-target": "no-target-key"})
            elif target == ".#confctl.buildPlan":
                emit({"alpha": {"key": "alpha-key", "inputs": {"nixpkgs": store + "/input"}}})
            elif target == ".#confctl.build":
                emit({"alpha-key": {"toplevel": store + "/system", "autoRollback": store + "/rollback"}})
            elif target == ".#confctl.inputsInfo":
                emit({"alpha-key": info})
            elif target == ".#confctl.inputsInfo.alpha-key":
                emit(info)
            else:
                return unknown(program, args)
        elif "build" in args:
            if ".#confctl.machinesJson" in args:
                emit([{"outputs": {"out": str(root / "machines.json")}}])
            elif mode == "build-failure":
                print("fixture Nix build failure", file=sys.stderr)
                return 23
            elif any(arg.startswith(".#confctl.build.") for arg in args):
                emit([{"outputs": {"out": store + "/system"}}])
            else:
                return unknown(program, args)
        elif "flake" in args and ("update" in args or "lock" in args):
            lock = json.loads((root / "conf" / "flake.lock").read_text())
            lock["nodes"]["nixpkgs"]["locked"]["rev"] = "b" * 40
            output = root / "conf" / "flake.lock"
            if "--output-lock-file" in args:
                output = Path(args[args.index("--output-lock-file") + 1])
            output.write_text(json.dumps(lock, indent=2) + "\n")
            if mode == "cached-warning":
                print("warning: unable to download 'https://api.github.com/repos/fixture/nixpkgs/commits/main': rate limit exceeded; using cached version", file=sys.stderr)
        else:
            return unknown(program, args)
    elif program == "ssh":
        if "alpha.invalid" not in args:
            return unknown(program, args)
        command = shlex.split(args[-1]) if args[-1] != "alpha.invalid" else []
        if mode == "offline":
            print("fixture connection refused", file=sys.stderr)
            return 255
        if command == ["cat", "/proc/uptime"]:
            print("123.0 0.0")
        elif command == ["cat", "/etc/confctl/inputs-info.json"]:
            emit(info)
        elif command and command[0] == "realpath":
            print(store + ("/system" if mode == "current" else "/previous"))
        elif command == ["bash", "--norc"]:
            profile = "/nix/var/nix/profiles/system"
            print(store + ("/system" if mode == "current" else "/previous"))
            print(f"{profile}-1-link;{store}/previous;946684800;/nix/store/fixture-linux-6.0/bzImage")
        elif command == ["nix-collect-garbage"]:
            if mode == "gc-failure":
                return 19
            print("1 store paths deleted, 1.00 MiB freed")
        elif mode == "ssh-failure":
            print("fixture remote failure", file=sys.stderr)
            return 7
        elif command and command[0] == "printf":
            print(command[-1], end="")
        elif command == ["cat"]:
            sys.stdout.buffer.write(data)
        elif not command or command[0] in (store + "/system/bin/switch-to-configuration", "nix-env"):
            pass
        elif command == ["fixture-check"]:
            print("healthy")
        else:
            return unknown(program, args)
    elif program == "nix-copy-closure":
        if "root@alpha.invalid" not in args:
            return unknown(program, args)
    elif program == "nix-shell":
        if not any("cssh" in arg for arg in args):
            return unknown(program, args)
    elif program == "git":
        if not args or args[0] != "commit":
            return unknown(program, args)
        print("[fixture] update inputs")
    else:
        return unknown(program, args)
    return 0


def emit(value):
    print(json.dumps(value))


def unknown(program, args):
    (Path(os.environ["CONFCTL_COMPAT_ROOT"]) / "unexpected-child").write_text(repr([program, *args]))
    print("UNEXPECTED FAKE CHILD: " + repr([program, *args]), file=sys.stderr)
    return 97


if __name__ == "__main__":
    sys.exit(main())
