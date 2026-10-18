#!/usr/bin/env python3
"""Only the two settings reads and one channels evaluation are admitted."""

import json
import os
from pathlib import Path
import sys

sys.dont_write_bytecode = True
import child


def main():
    args = sys.argv[1:]
    if args[:2] != ["eval", "--json"] or args[-1:] not in (
            [".#confctl.settings"], [".#confctl.channels"]):
        return child.unknown("nix", args)
    root = Path(os.environ["CONFCTL_COMPAT_ROOT"])
    scenario = json.loads((root / "scenario.json").read_text())
    events = root / "events.jsonl"
    previous = [json.loads(line) for line in events.read_text().splitlines()] if events.exists() else []
    target = args[-1]
    index = sum(event["argv"][-1:] == [target] for event in previous)
    with events.open("a") as output:
        output.write(json.dumps(dict(program="nix", argv=args, stdin_hex="", cwd=os.getcwd())) + "\n")
    replies = scenario["settings_replies"] if target.endswith(".settings") else [scenario["channels_reply"]]
    if index >= len(replies):
        return child.unknown("nix", args)
    reply = replies[index]
    if reply.get("exit", 0):
        print(reply["error"], file=sys.stderr)
        return reply["exit"]
    child.emit(reply["value"])
    return 0


if __name__ == "__main__":
    sys.exit(main())
