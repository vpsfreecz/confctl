#!/usr/bin/env python3
"""Focused harness regressions; no Ruby, Nix, SSH or network is required."""

import base64
import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
import run


class EvidenceTest(unittest.TestCase):
    def test_unexpected_child_retains_raw_streams_status_and_effects(self):
        with tempfile.TemporaryDirectory(prefix="confctl-runner-test-") as directory:
            root = Path(directory)
            script = root / "refused.py"
            script.write_text(
                "import json, os, sys\n"
                "from pathlib import Path\n"
                "root = Path(os.environ['CONFCTL_COMPAT_ROOT'])\n"
                "(root / 'unexpected-child').write_text('unexpected fixture request')\n"
                "(root / 'events.jsonl').write_text(json.dumps({'program': 'refused', 'argv': ['x']}) + '\\n')\n"
                "(root / 'conf' / 'effect').write_bytes(b'file effect')\n"
                "sys.stdout.buffer.write(b'\\xffstdout')\n"
                "sys.stderr.buffer.write(b'\\x00stderr')\n"
                "sys.exit(17)\n"
            )
            case = dict(id="unexpected-child-regression", argv=[], stdin="", mode="normal", setup="empty")
            evidence = root / "evidence"
            with self.assertRaisesRegex(RuntimeError, "unexpected fixture request"):
                run.execute(case, [sys.executable, str(script)], root / "oracle", evidence)
            saved = json.loads((evidence / (case["id"] + ".json")).read_text())
            raw = saved["raw"]
            self.assertEqual(raw["exit"], 17)
            self.assertIsNone(raw["signal"])
            self.assertEqual(base64.b64decode(raw["stdout_b64"]), b"\xffstdout")
            self.assertEqual(base64.b64decode(raw["stderr_b64"]), b"\x00stderr")
            self.assertEqual(raw["events"], [dict(program="refused", argv=["x"])])
            self.assertEqual(base64.b64decode(raw["files"]["effect"]["bytes_b64"]), b"file effect")
            self.assertEqual(saved["normalized"]["files"]["effect"]["text"], "file effect")


class GarbageCollectionContractTest(unittest.TestCase):
    def result(self, code=0, stderr=""):
        return dict(exit=code, signal=None, stderr=stderr,
                    events=json.dumps([dict(program="ssh", argv=["alpha.invalid", "nix-collect-garbage"])]))

    def test_candidate_success_and_genuine_child_failure_have_distinct_exits(self):
        self.assertTrue(run.gc_contract(self.result(), True))
        self.assertFalse(run.gc_contract(self.result(1), True))
        self.assertTrue(run.gc_contract(self.result(1, "alpha failed"), True, child_failure=True))
        self.assertFalse(run.gc_contract(self.result(), True, child_failure=True))

    def test_candidate_contract_with_actual_success_and_nonzero_fake_child(self):
        # This tiny test driver validates the harness policy, not a Rust port.
        with tempfile.TemporaryDirectory(prefix="confctl-gc-contract-") as directory:
            root = Path(directory)
            script = root / "candidate.py"
            script.write_text(
                "import subprocess, sys\n"
                "child = subprocess.run(['ssh', '-l', 'root', '-p', '2222', 'alpha.invalid', 'nix-collect-garbage'])\n"
                "print('child exit', child.returncode)\n"
                "sys.exit(1 if child.returncode else 0)\n"
            )
            selected = {item["id"]: item for item in run.cases()}
            for name, failed, child_exit in (("gc-baseline-line-error", False, 0),
                                              ("gc-child-failure", True, 19)):
                with self.subTest(case=name):
                    actual = run.execute(selected[name], [sys.executable, str(script)], root / "oracle", None)
                    self.assertIn("child exit " + str(child_exit), actual["stdout"])
                    self.assertTrue(run.gc_contract(actual, True, child_failure=failed))
                    self.assertFalse(run.gc_contract(actual, True, child_failure=not failed))

    def test_candidate_requires_gc_child_and_rejects_crash_or_signal(self):
        result = self.result(1, "undefined method 'line'")
        self.assertFalse(run.gc_contract(result, True, child_failure=True))
        result = self.result(1)
        result["events"] = "[]"
        self.assertFalse(run.gc_contract(result, True, child_failure=True))
        result = self.result(1)
        result["signal"] = 15
        self.assertFalse(run.gc_contract(result, True, child_failure=True))

    def test_baseline_requires_exact_progress_defect(self):
        result = self.result(1, "undefined method 'line' for ConfCtl::NixCollectGarbage::Progress")
        self.assertTrue(run.gc_contract(result, False))
        self.assertTrue(run.gc_contract(result, False, child_failure=True))
        for diagnostic in ("unrelated failure", "undefined method 'other' for ConfCtl::NixCollectGarbage::Progress"):
            changed = copy.deepcopy(result)
            changed["stderr"] = diagnostic
            self.assertFalse(run.gc_contract(changed, False))


if __name__ == "__main__":
    unittest.main()
