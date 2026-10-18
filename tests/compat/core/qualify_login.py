#!/usr/bin/env python3
"""Observe only GCRoot.dir native-byte joining, with injected login/root and no writes."""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
import run as core

HERE = Path(__file__).resolve().parent
CASES = {
    "empty": b"", "relative": b"fixture", "leading": b"/fixture",
    "embedded": b"fixture/middle", "trailing": b"fixture/",
    "repeated": b"//fixture///middle//", "dots": b"./fixture/../login",
    "root": b"/", "non-utf8": b"f\xffx", "nil": None,
}
PROGRAM = r'''
require 'json'
module ConfCtl
  module Utils; end
  module ConfDir
    def self.short_hash = 'fixture'
  end
end
require 'confctl/gcroot'
login = ENV['CONFCTL_TEST_LOGIN_B64']&.unpack1('m0')&.b
Etc.define_singleton_method(:getlogin) { login }
class << File
  alias_method :confctl_original_join, :join
  def join(*parts)
    if parts.first == '/nix/var/nix/gcroots/per-user'
      parts[0] = ENV.fetch('CONFCTL_TEST_PER_USER_ROOT')
    end
    confctl_original_join(*parts)
  end
end
begin
  puts JSON.generate(path_b64: [ConfCtl::GCRoot.dir.b].pack('m0'))
rescue => e
  puts JSON.generate(error: {class: e.class.to_s, message: e.message})
end
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--record-output", type=Path, help="write JSON to this owned evidence directory")
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    results = {}
    with tempfile.TemporaryDirectory(prefix="confctl-login-join-") as scratch:
        root = Path(scratch)
        oracle = core.export_baseline(root / "oracle")
        program = args.evidence / "probe.rb"
        program.write_text(PROGRAM)
        per_user = root / "owned-per-user"
        for name, login in CASES.items():
            env = {key: value for key, value in os.environ.items()
                   if key in ("PATH", "GEM_HOME", "GEM_PATH", "BUNDLE_GEMFILE", "BUNDLE_PATH")}
            env["CONFCTL_TEST_PER_USER_ROOT"] = str(per_user)
            if login is not None:
                env["CONFCTL_TEST_LOGIN_B64"] = base64.b64encode(login).decode()
            proc = subprocess.run([shutil.which("ruby"), "-I", str(oracle / "lib"), str(program.resolve())],
                                  env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            raw = dict(exit=proc.returncode, stdout_b64=base64.b64encode(proc.stdout).decode(),
                       stderr_b64=base64.b64encode(proc.stderr).decode())
            core.write_json(args.evidence / (name + ".json"), raw)
            assert proc.returncode == 0 and not proc.stderr, name
            value = json.loads(proc.stdout)
            if "path_b64" in value:
                native = base64.b64decode(value["path_b64"])
                assert native.startswith(os.fsencode(per_user)), name
                normalized = native.replace(os.fsencode(per_user), b"<ROOT>", 1)
                value["path_b64"] = base64.b64encode(normalized).decode()
                value["path_hex"] = normalized.hex()
            results[name] = dict(login_b64=base64.b64encode(login).decode() if login is not None else None,
                                 **value)
        assert not per_user.exists(), "path qualification must not create any GC root"
    actual = dict(baseline=core.BASELINE, cases=results)
    expected = HERE / "expected-login-join.json"
    if args.record_output:
        args.record_output.mkdir(parents=True, exist_ok=True)
        core.write_json(args.record_output / expected.name, actual)
    else:
        assert actual == json.loads(expected.read_text())
    core.write_json(args.evidence / "qualification.json", dict(
        baseline=core.BASELINE, cases=len(results),
        archive_sha256=hashlib.sha256((HERE / "baseline.tar.gz").read_bytes()).hexdigest()))
    print(f"PASS {len(results)} native-byte join observations; no GC effects")


if __name__ == "__main__":
    main()
