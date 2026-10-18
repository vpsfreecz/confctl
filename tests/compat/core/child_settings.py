#!/usr/bin/env python3
"""Only settings responses vary; all other child contracts stay in child.py."""

import json
import os
from pathlib import Path
import sys

sys.dont_write_bytecode = True
import child


def main():
    args = sys.argv[1:]
    if args[:2] != ["eval", "--json"] or args[-1:] != [".#confctl.settings"]:
        return child.main()
    root = Path(os.environ["CONFCTL_COMPAT_ROOT"])
    scenario = json.loads((root / "scenario.json").read_text())
    events = root / "events.jsonl"
    previous = [json.loads(line) for line in events.read_text().splitlines()] if events.exists() else []
    index = sum(event["argv"][-1:] == [".#confctl.settings"] for event in previous)
    with events.open("a") as output:
        output.write(json.dumps(dict(program="nix", argv=args, stdin_hex="")) + "\n")
    if index >= len(scenario["settings_replies"]):
        (root / "unexpected-child").write_text("undeclared settings invocation: " + repr(args))
        return 97
    reply = scenario["settings_replies"][index]
    if reply.get("exit", 0):
        print(reply["error"], file=sys.stderr)
        return reply["exit"]
    child.emit(reply)
    return 0


if __name__ == "__main__":
    sys.exit(main())
