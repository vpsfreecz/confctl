#!/usr/bin/env python3
"""The narrow follows oracle's only accepted external call is flake lock."""

import json
import os
from pathlib import Path
import sys


def main():
    root = Path(os.environ["CONFCTL_COMPAT_ROOT"])
    args = sys.argv[1:]
    event = dict(program="nix", argv=args, stdin_hex="")
    with (root / "events.jsonl").open("a") as stream:
        stream.write(json.dumps(event) + "\n")
    prefix = ["flake", "lock", "--update-input", "alias", "--override-input", "alias",
              "github:fixture/nixpkgs/next", "--output-lock-file"]
    if len(args) != 9 or args[:8] != prefix:
        (root / "unexpected-child").write_text(repr(args))
        return 97
    output = Path(args[8])
    if output.parent != root / "conf" or not output.name.startswith("flake.lock.tmp-"):
        (root / "unexpected-child").write_text("unowned output path")
        return 97
    lock = json.loads((root / "conf/flake.lock").read_text())
    lock["nodes"]["nixpkgs"]["locked"]["rev"] = "b" * 40
    # Real Nix may rewrite original metadata. The real Ruby setter must restore
    # it on the first-segment node, leaving the deliberately different leaf alone.
    lock["nodes"]["nixpkgs"]["original"] = {"type": "github", "ref": "child-replaced"}
    output.write_text(json.dumps(lock, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
