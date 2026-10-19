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



class BehavioralComparisonTest(unittest.TestCase):
    def case(self):
        return dict(id="ls-default", route="ls", argv=["ls"], stdin="")

    def frame(self):
        return dict(exit=0, signal=None, stdout="NAME SPIN HOST.TARGET\nalpha nixos alpha.invalid\nbeta nixos beta.invalid\n",
                    stderr="", events=json.dumps([
                        dict(program="nix", argv=["eval", ".#confctl.settings"], stdin_hex="", cwd="<CASE>/conf"),
                        dict(program="nix", argv=["eval", ".#confctl.machinesJson"], stdin_hex="", cwd="<CASE>/conf")]),
                    files={"generation.json": dict(mode="0o644", text='{"generation":9007199254740993,"current":true,"note":null}')})

    def compare(self, expected, actual, case=None):
        return run.compare_frame(case or self.case(), expected, actual, candidate=True)

    def test_layout_row_order_and_json_serialization_preserve_information(self):
        expected = self.frame()
        actual = copy.deepcopy(expected)
        actual["stdout"] = "Machine   OS   Target\nbeta  nixos beta.invalid\nalpha nixos alpha.invalid\n\n"
        actual["files"]["generation.json"]["text"] = '{ "note": null, "current": true, "generation": 9007199254740993 }\n'
        actual["events"] = json.dumps(json.loads(expected["events"]), indent=2)
        self.assertEqual(self.compare(expected, actual)["outcome"], "MATCH")
        self.assertNotEqual(expected["stdout"], actual["stdout"])
        self.assertNotEqual(expected["files"], actual["files"])


    def test_explicit_field_output_keeps_header_and_record_meaning(self):
        case = dict(id="synthetic-unit-ls-output", route="ls", argv=["ls", "--output", "name"])
        expected = self.frame()
        expected["stdout"] = "NAME\nalpha\nbeta\n"
        actual = copy.deepcopy(expected)
        actual["stdout"] = "NAME alpha beta\n"
        self.assertEqual(self.compare(expected, actual, case)["outcome"], "DIFFERENT")
        actual["stdout"] = "NAME\n alpha \n beta \n"
        self.assertEqual(self.compare(expected, actual, case)["outcome"], "MATCH")

    def test_named_condition_allows_new_prose_and_records_actual_coordinates(self):
        case = dict(id="channels-corrupt-lock", argv=["inputs", "channel", "ls"])
        expected = self.frame()
        expected.update(exit=1, stdout="", stderr="Log file: <CASE>/failure.log\nerror: invalid JSON in flake.lock at line 1 column 2\n")
        actual = copy.deepcopy(expected)
        actual["stderr"] = "Full log: <CASE>/failure.log\nerror: cannot parse JSON in flake.lock (line 3, column 5)\n"
        result = self.compare(expected, actual, case)
        self.assertEqual(result["outcome"], "MATCH")
        self.assertEqual(result["observations"]["stderr"]["actual"]["observed_coordinates"], (3, 5))
        self.assertNotEqual(expected["stderr"], actual["stderr"])

    def test_wrong_target_and_extra_or_reordered_effects_are_rejected(self):
        expected = self.frame()
        for mutation in ("target", "extra", "order"):
            with self.subTest(mutation=mutation):
                actual = copy.deepcopy(expected)
                events = json.loads(actual["events"])
                if mutation == "target":
                    events[1]["argv"][-1] = ".#wrong-machine"
                elif mutation == "extra":
                    events.append(dict(program="ssh", argv=["beta.invalid", "reboot"], stdin_hex="", cwd="<CASE>/conf"))
                else:
                    events.reverse()
                actual["events"] = json.dumps(events)
                self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")

    def test_native_status_signal_and_boolean_status_are_not_presentation(self):
        expected = self.frame()
        for field, value in (("exit", 1), ("signal", 15), ("exit", False)):
            with self.subTest(field=field, value=value):
                actual = copy.deepcopy(expected)
                actual[field] = value
                self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")

    def test_structured_values_preserve_large_numbers_types_null_and_absence(self):
        expected = self.frame()
        for text in ('{"generation":9007199254740992,"current":true,"note":null}',
                     '{"generation":"9007199254740993","current":true,"note":null}',
                     '{"generation":true,"current":true,"note":null}',
                     '{"generation":9007199254740993,"current":true}',
                     '{"generation":9007199254740993,"current":true,"note":""}'):
            with self.subTest(text=text):
                actual = copy.deepcopy(expected)
                actual["files"]["generation.json"]["text"] = text
                self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")


    def test_decimal_values_do_not_round_through_float_or_decimal_context(self):
        expected = self.frame()
        actual = copy.deepcopy(expected)
        expected["files"]["generation.json"]["text"] = '{"number":1.00000000000000000000000000001}'
        actual["files"]["generation.json"]["text"] = '{"number":1.00000000000000000000000000002}'
        self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")
        expected["files"]["generation.json"]["text"] = '{"number":1.00}'
        actual["files"]["generation.json"]["text"] = '{"number":1e0}'
        self.assertEqual(self.compare(expected, actual)["outcome"], "MATCH")
        actual["files"]["generation.json"]["text"] = '{"number":1}'
        self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")

    def test_ambiguous_json_cannot_be_normalized_to_a_match(self):
        expected = self.frame()
        actual = copy.deepcopy(expected)
        actual["files"]["generation.json"]["text"] = '{"generation":0,"generation":9007199254740993,"current":true,"note":null}'
        self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")

    def test_paths_modes_links_and_missing_files_are_effects(self):
        expected = self.frame()
        for mutation in ("path", "mode", "link"):
            with self.subTest(mutation=mutation):
                actual = copy.deepcopy(expected)
                if mutation == "path":
                    actual["files"]["other.json"] = actual["files"].pop("generation.json")
                elif mutation == "mode":
                    actual["files"]["generation.json"]["mode"] = "0o600"
                else:
                    actual["files"]["generation.json"] = dict(link="../wrong-generation")
                self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")

    def test_lost_host_result_and_moved_error_information_are_rejected(self):
        expected = self.frame()
        actual = copy.deepcopy(expected)
        actual["stdout"] = "NAME SPIN HOST.TARGET\nalpha nixos alpha.invalid\n"
        self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")
        expected.update(exit=1, stderr="error: Aborted\n")
        actual = copy.deepcopy(expected)
        actual["stdout"] += actual.pop("stderr")
        actual["stderr"] = ""
        self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")

    def test_unmapped_prose_and_mixed_child_payload_remain_unqualified(self):
        expected = self.frame()
        actual = copy.deepcopy(expected)
        expected["stderr"] = "unsupported condition alpha\n"
        actual["stderr"] = "a different explanation alpha\n"
        self.assertEqual(self.compare(expected, actual)["outcome"], "UNQUALIFIED")
        case = dict(id="ssh-success", route="ssh", argv=["ssh", "alpha", "uptime"])
        expected["stderr"] = actual["stderr"] = ""
        expected["stdout"] = "alpha:\nraw child output\n"
        actual["stdout"] = "alpha:\nraw  child output\n"
        self.assertEqual(self.compare(expected, actual, case)["outcome"], "UNQUALIFIED")

    def test_baseline_integrity_is_not_a_candidate_presentation_rule(self):
        expected = self.frame()
        actual = copy.deepcopy(expected)
        actual["stdout"] = actual["stdout"].replace(" ", "  ")
        self.assertEqual(run.compare_frame(self.case(), expected, actual)["outcome"], "DIFFERENT")
        self.assertEqual(self.compare(expected, actual)["outcome"], "MATCH")

    def fixture(self, name, directory="expected"):
        return json.loads((run.HERE / directory / (name + ".json")).read_text())

    def test_actual_ls_frame_cannot_collapse_split_or_lose_association(self):
        case = next(item for item in run.cases() if item["id"] == "ls-default")
        expected = self.fixture("ls-default")
        for text in (
                expected["stdout"].replace("\n", " "),
                expected["stdout"].replace("alpha       nixos", "alpha\nnixos"),
                expected["stdout"].replace("alpha.invalid", "-").replace("no-target   nixos   -",
                                                                            "no-target   nixos   alpha.invalid")):
            with self.subTest(text=text):
                actual = copy.deepcopy(expected)
                actual["stdout"] = text
                self.assertEqual(self.compare(expected, actual, case)["outcome"], "DIFFERENT")
        actual = copy.deepcopy(expected)
        actual["stdout"] = "Machine\tOS\tTarget\r\nno-target nixos -\r\nalpha nixos alpha.invalid\r\n"
        self.assertEqual(self.compare(expected, actual, case)["outcome"], "MATCH")

    def test_table_duplicate_multiplicity_is_not_a_set(self):
        expected = self.frame()
        expected["stdout"] += "alpha nixos alpha.invalid\n"
        actual = copy.deepcopy(expected)
        actual["stdout"] = actual["stdout"].rsplit("alpha nixos alpha.invalid\n", 1)[0]
        self.assertEqual(self.compare(expected, actual)["outcome"], "DIFFERENT")

    def test_bare_cr_and_control_boundaries_are_not_lf_records(self):
        expected = self.fixture("ls-default")
        for separator in ("\r", "\v", "\f", "\x85", "\u2028", "\x1b"):
            with self.subTest(separator=repr(separator)):
                actual = copy.deepcopy(expected)
                actual["stdout"] = expected["stdout"].replace("\n", separator)
                result = self.compare(expected, actual)
                self.assertEqual(result["outcome"], "UNQUALIFIED")
                self.assertIn("unsupported control/record separator", result["unqualified"][0])

    def test_named_diagnostics_reject_folded_missing_or_changed_information(self):
        cases = [
            (dict(id="channels-corrupt-lock", argv=["inputs", "channel", "ls"]),
             "Log file: <CASE>/with  space/failure.log\n"
             "error: invalid JSON in flake.lock at line 1 column 2\n"),
            (dict(id="settings-build-declined", argv=[
                "build", "--show-trace", "--max-jobs", "2", "--cores", "3", "alpha"]),
             self.fixture("settings-build-declined", "expected-settings")["stderr"]),
        ]
        for case, text in cases:
            expected = self.frame()
            expected.update(exit=1, stdout="", stderr=text)
            for changed, reason in (
                    (text.strip("\n").replace("\n", " ") + "\n", "folded locator/condition boundary"),
                    (text[:text.index("error:")], "required condition missing"),
                    (text.replace("error:", "warning:"), "meaning changed"),
                    (text.replace(".log", "-wrong.log"), "meaning changed")):
                with self.subTest(case=case["id"], changed=changed):
                    actual = copy.deepcopy(expected)
                    actual["stderr"] = changed
                    result = self.compare(expected, actual, case)
                    self.assertEqual(result["outcome"], "DIFFERENT")
                    self.assertTrue(any(reason in item for item in result["differences"]))
            actual = copy.deepcopy(expected)
            actual["stderr"] = text.replace("Aborted", "Operation cancelled")
            self.assertEqual(self.compare(expected, actual, case)["outcome"], "MATCH")
        expected = self.frame()
        case = cases[0][0]
        expected.update(exit=1, stdout="", stderr=cases[0][1])
        for text in (expected["stderr"].replace("with  space", "with space"),
                     expected["stderr"].replace("flake.lock", "other.lock")):
            actual = copy.deepcopy(expected)
            actual["stderr"] = text
            self.assertEqual(self.compare(expected, actual, case)["outcome"], "DIFFERENT")

    def test_unprofiled_paths_and_new_diagnostic_syntax_have_no_lexical_fallback(self):
        expected = self.frame()
        expected["stderr"] = "inspect <CASE>/with  space/failure.log\n"
        actual = copy.deepcopy(expected)
        actual["stderr"] = expected["stderr"].replace("with  space", "with space")
        self.assertEqual(self.compare(expected, actual)["outcome"], "UNQUALIFIED")
        case = dict(id="channels-corrupt-lock", argv=["inputs", "channel", "ls"])
        expected.update(exit=1, stdout="", stderr=
                        "Log file: <CASE>/failure.log\nerror: invalid JSON in flake.lock at line 1 column 2\n")
        actual = copy.deepcopy(expected)
        actual["stderr"] = "Log file: <CASE>/failure.log\nnew unsupported diagnostic representation\n"
        self.assertEqual(self.compare(expected, actual, case)["outcome"], "UNQUALIFIED")

    def test_actual_option_ls_output_is_help_not_operational_fields(self):
        case = next(item for item in run.cases() if item["id"] == "option-ls-output")
        self.assertEqual(case["argv"], ["ls", "--output", "name", "--help"])
        expected = self.fixture("option-ls-output")
        self.assertIn("COMMAND OPTIONS\n", expected["stdout"])
        actual = copy.deepcopy(expected)
        old = "Filter by attribute (may be used more than once,\n                             default: none)"
        self.assertIn(old, actual["stdout"])
        actual["stdout"] = actual["stdout"].replace(
            old, "Filter by attribute (may be used\n                             more than once, default: none)")
        result = self.compare(expected, actual, case)
        self.assertEqual(result["outcome"], "UNQUALIFIED")
        self.assertEqual(result["differences"], [])
        self.assertIn("help-usage", result["unqualified"][0])

    def test_actual_operational_records_and_lists_keep_complete_grouping(self):
        specs = [
            (next(item for item in run.cases() if item["id"] == "ls-filter"), "expected"),
            (next(item for item in run.cases() if item["id"] == "ls-negative-filters"), "expected"),
            (next(item for item in run.cases() if item["id"] == "ls-list-attributes"), "expected"),
            (dict(id="settings-output-columns", argv=["ls", "-H", "-o", "name,managed"]), "expected-settings"),
            (dict(id="settings-attributes", argv=["ls", "--show-trace", "-L"]), "expected-settings"),
        ]
        for case, directory in specs:
            expected = self.fixture(case["id"], directory)
            actual = copy.deepcopy(expected)
            actual["stdout"] = "\n".join("  " + line + "  " for line in expected["stdout"].split("\n")
                                           if line) + "\n"
            with self.subTest(case=case["id"]):
                self.assertEqual(self.compare(expected, actual, case)["outcome"], "MATCH")
                if case["id"] == "ls-filter":
                    actual["stdout"] = "alpha\nalpha.invalid\n"
                else:
                    actual["stdout"] = expected["stdout"].replace("\n", " ")
                self.assertEqual(self.compare(expected, actual, case)["outcome"], "DIFFERENT")
                actual["stdout"] = "\n".join(expected["stdout"].split("\n")[1:])
                self.assertEqual(self.compare(expected, actual, case)["outcome"], "DIFFERENT")
        # Header loss belongs to operational records, never to similarly named help.
        case = specs[1][0]
        expected = self.fixture(case["id"])
        actual = copy.deepcopy(expected)
        actual["stdout"] = expected["stdout"].split("\n", 1)[1]
        self.assertEqual(self.compare(expected, actual, case)["outcome"], "DIFFERENT")

    def test_decimal_signed_zero_and_array_order_remain_exact(self):
        expected = self.frame()
        expected["files"]["generation.json"]["text"] = '{"n":-0.0,"items":[1,2]}'
        for text, outcome in (
                ('{"items":[1,2],"n":-0e2}', "MATCH"),
                ('{"n":0.0,"items":[1,2]}', "DIFFERENT"),
                ('{"n":-0.0,"items":[2,1]}', "DIFFERENT")):
            actual = copy.deepcopy(expected)
            actual["files"]["generation.json"]["text"] = text
            self.assertEqual(self.compare(expected, actual)["outcome"], outcome)

    def test_json_conversion_and_depth_limits_return_reasoned_unqualified(self):
        expected = self.frame()
        for text in ('{"n":1e999999999999999999999999999999}',
                     "[" * (sys.getrecursionlimit() + 100) + "0" +
                     "]" * (sys.getrecursionlimit() + 100)):
            with self.subTest(prefix=text[:40]):
                actual = copy.deepcopy(expected)
                actual["files"]["generation.json"]["text"] = text
                result = self.compare(expected, actual)
                self.assertEqual(result["outcome"], "UNQUALIFIED")
                self.assertTrue(any("JSON reader conversion/depth limit" in reason
                                    for reason in result["unqualified"]))
                actual = copy.deepcopy(expected)
                actual["events"] = text
                result = self.compare(expected, actual)
                self.assertEqual(result["outcome"], "UNQUALIFIED")
                self.assertIn("events: JSON reader conversion/depth limit", result["unqualified"])

    def test_signature_depth_limit_preserves_three_outcomes(self):
        expected = self.frame()
        actual = copy.deepcopy(expected)
        nested = 0
        for _ in range(sys.getrecursionlimit() + 100):
            nested = [nested]
        expected["extra"] = actual["extra"] = nested
        for candidate in (False, True):
            result = run.compare_frame(self.case(), expected, actual, candidate=candidate)
            self.assertEqual(result["outcome"], "UNQUALIFIED")
            self.assertTrue(any("typed reader conversion/depth limit" in reason
                                for reason in result["unqualified"]))

    def test_malformed_duplicate_and_nonfinite_are_not_reader_limits(self):
        expected = self.frame()
        for text, reason in (
                ('{"n":}', "Expecting"),
                ('{"n":1,"n":2}', "duplicate object key"),
                ('{"n":NaN}', "nonfinite JSON number"),
                ('{"n":Infinity}', "nonfinite JSON number")):
            with self.subTest(text=text):
                actual = copy.deepcopy(expected)
                actual["files"]["generation.json"]["text"] = text
                result = self.compare(expected, actual)
                self.assertEqual(result["outcome"], "DIFFERENT")
                self.assertTrue(any(reason in item for item in result["differences"]))
                self.assertFalse(any("conversion/depth limit" in item for item in result["unqualified"]))
                actual = copy.deepcopy(expected)
                actual["events"] = text
                result = self.compare(expected, actual)
                self.assertEqual(result["outcome"], "UNQUALIFIED")
                self.assertTrue(any("invalid structured effect evidence" in item and reason in item
                                    for item in result["unqualified"]))

    def test_actual_lock_override_and_declined_frames_cannot_fold_or_drop_condition(self):
        lock = self.fixture("LOCK-JSON-01", "expected-channels-candidate")["expected"]
        declined = self.fixture("settings-build-declined", "expected-settings")
        for case, expected in (
                (dict(id="channels-corrupt-lock", argv=["inputs", "channel", "ls"]), lock),
                (dict(id="settings-build-declined", argv=[
                    "build", "--show-trace", "--max-jobs", "2", "--cores", "3", "alpha"]), declined)):
            for text in (
                    expected["stderr"].strip("\n").replace("\n", " ") + "\n",
                    expected["stderr"][:expected["stderr"].index("error:")]):
                actual = copy.deepcopy(expected)
                actual["stderr"] = text
                result = self.compare(expected, actual, case)
                self.assertEqual(result["outcome"], "DIFFERENT")
                self.assertTrue(any("folded locator/condition boundary" in reason or
                                    "required condition missing" in reason
                                    for reason in result["differences"]))

if __name__ == "__main__":
    unittest.main()
