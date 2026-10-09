"""Actual entry comparator and setup control flow, with no Nix/SSH/container run."""
import copy
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

ENTRY = Path(__file__).with_name("entry.sh").read_text()
FUNCTION = ENTRY.split("# BEGIN fixture preseed comparator\n", 1)[1].split(
    "# END fixture preseed comparator", 1
)[0]
COMPARATOR = FUNCTION.split("<<'RUBY'\n", 1)[1].split("\nRUBY\n", 1)[0]
SETUP = ENTRY.split("# BEGIN fixture preseed setup\n", 1)[1]
SCALES = ("1", "10", "100", "1000")
RUBY = os.environ.get("CONFCTL_FIXTURE_RUBY") or shutil.which("ruby")
BASH = shutil.which("bash")
if not BASH or not os.path.isabs(BASH):
    raise RuntimeError("Run in the declared environment with an absolute Bash executable on PATH")

NIX_STUB = r'''
import json, os, pathlib, sys
args = sys.argv[1:]
state = json.loads(pathlib.Path(os.environ["GATE_STUB_STATE"]).read_text())
root = pathlib.Path(os.environ["GATE_STUB_ROOT"])
count = pathlib.Path.cwd().name.removeprefix("config-")
with (root / "calls.jsonl").open("a") as f:
    f.write(json.dumps({"command": "nix", "args": args, "scale": count}) + "\n")
manifest = json.loads((root / "packages.json").read_text())
record = manifest["expectedRuntime"]["scales"][count]
if args[0] == "eval":
    assert args[:9] == ["eval", "--offline", "--json", "--no-write-lock-file",
                       "--no-update-lock-file", "--max-jobs", "0", ".#confctl", "--apply"]
    assert len(args) == 10 and "readFile" not in args[9]
    assert "builtins.hashString" in args[9] and "metadataSha256" not in args[9]
    assert "outputPath = d.outPath;" in args[9]
    assert ("c.build" in args[9]) == (count == "1")
    if state.get("evalFailure") == count:
        sys.exit(1)
    actual = json.loads((root / ("runtime-" + count + ".json")).read_text())
    if state.get("malformed") == count:
        print("invalid json")
    else:
        print(json.dumps(actual))
elif args[:2] == ["path-info", "--offline"]:
    assert args[:6] == ["path-info", "--offline", "--json", "--json-format", "1", "--recursive"]
    paths = [record["metadata"]["outputPath"]]
    if count == "1":
        paths += [record["seed"][k]["outputPath"] for k in ("toplevel", "autoRollback")]
    assert args[6:] == paths
    if state.get("queryFailure") == count:
        sys.exit(1)
    nodes = {p: {"references": []} for p in paths}
    if state.get("unavailable") == count:
        nodes[paths[0]] = None
    if state.get("missingNode") == count:
        del nodes[paths[0]]
    if state.get("missingReference") == count:
        nodes[paths[0]]["references"] = [str(root / "absent-reference")]
    if state.get("metadataAtQuery") == count:
        pathlib.Path(paths[0]).write_text("changed metadata")
    print(json.dumps(nodes))
elif args[0] == "build":
    assert args[1:7] == ["--offline", "--no-link", "--no-write-lock-file",
                        "--no-update-lock-file", "--max-jobs", "0"]
    if args[7] == ".#confctl.machinesJson":
        assert len(args) == 8
        if state.get("metadataAtCache") == count:
            pathlib.Path(record["metadata"]["outputPath"]).write_text("changed metadata")
        if state.get("lockAtCache") == count:
            pathlib.Path("flake.lock").write_text("changed lock")
    else:
        assert count == "1" and args[7:] == [
            ".#confctl.build." + record["seed"]["key"] + ".toplevel",
            ".#confctl.build." + record["seed"]["key"] + ".autoRollback"]
elif args[:2] == ["flake", "metadata"]:
    assert args == ["flake", "metadata", "--offline", "--json", "--no-write-lock-file", "--no-update-lock-file"]
    source = manifest["configurationSources"][count]
    value = {"path": source["sourcePath"], "locked": {"narHash": source["narHash"]}}
    mode = state.get("sourceMetadata", {}).get(count)
    if mode == "path":
        value["path"] += "-changed"
    elif mode == "narHash":
        value["locked"]["narHash"] += "-changed"
    elif mode == "missingPath":
        del value["path"]
    elif mode == "missingNarHash":
        del value["locked"]["narHash"]
    elif mode == "missingLocked":
        del value["locked"]
    print(json.dumps(value))
elif args[:4] == ["path-info", "--recursive", "--json", str(root / "clients")]:
    print("{}")
else:
    raise AssertionError("Unexpected Nix request: " + repr(args))
'''

TRACE_STUB = r'''
import json, os, pathlib, stat, sys
root = pathlib.Path(os.environ["GATE_STUB_ROOT"])
state = json.loads(pathlib.Path(os.environ["GATE_STUB_STATE"]).read_text())
with (root / "calls.jsonl").open("a") as f:
    f.write(json.dumps({"command": "strace", "args": sys.argv[1:]}) + "\n")
manifest = json.loads((root / "packages.json").read_text())
if "native-process.trace" in " ".join(sys.argv):
    if state.get("metadataAtTrace"):
        count = state["metadataAtTrace"]
        pathlib.Path(manifest["expectedRuntime"]["scales"][count]["metadata"]["outputPath"]).write_text("changed metadata")
    if state.get("inputAtTrace"):
        (root / "work" / "config-10" / state["inputAtTrace"]).write_text("changed input")
    if state.get("ownerExecutableAtTrace"):
        path = root / "work" / "config-10" / state["ownerExecutableAtTrace"]
        path.chmod(path.stat().st_mode ^ stat.S_IXUSR)
    if state.get("inputSymlinkAtTrace"):
        path = root / "work" / "config-10" / state["inputSymlinkAtTrace"]
        referent = root / "working-input-referent.nix"
        referent.write_bytes(path.read_bytes())
        referent.chmod(stat.S_IMODE(path.stat().st_mode))
        path.unlink()
        path.symlink_to(referent)
    if state.get("trackedAtTrace"):
        import subprocess
        config = root / "work" / "config-10"
        (config / "unexpected.nix").write_text("{}"); subprocess.run(["git", "-C", str(config), "add", "unexpected.nix"], check=True)
    if state.get("inventoryAtTrace"):
        (root / "artifacts" / "original-inputs-10.json").write_text("{}")
    if state.get("runtimeAtTrace"):
        (root / "artifacts" / "runtime-preseed-10.json").write_text("{}")
    if state.get("missingSourceMetadataAtTrace"):
        count = state["missingSourceMetadataAtTrace"]
        (root / "artifacts" / ("config-" + count + "-source-metadata.json")).unlink()
'''


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


class GateTest(unittest.TestCase):
    def setUp(self):
        if not RUBY:
            self.fail("Run in the declared environment with CONFCTL_FIXTURE_RUBY or Ruby on PATH")
        self.root = Path(tempfile.mkdtemp(
            prefix="2026-10-09-evaluate-confctl-rewrite-language-preseed-tests."))
        self.work = self.root / "work"
        self.artifacts = self.root / "artifacts"
        self.bin = self.root / "bin"
        for directory in (self.work, self.artifacts, self.bin, self.root / "source"):
            directory.mkdir()
        self.manifest_path = self.root / "packages.json"
        self.records = {}
        configs = {}
        seed = self.root / "seed"
        (seed / "etc/confctl").mkdir(parents=True)
        (seed / "etc/confctl/inputs-info.json").write_text("{}")
        rollback = self.root / "auto-rollback.rb"
        rollback.write_text("# no-op")
        for count in SCALES:
            source = self.root / "source" / count
            source.mkdir()
            (source / "cluster").mkdir()
            files = {"flake.nix": "{}\n", "flake.lock": "{}\n", "count.nix": count + "\n",
                     "cluster/inventory.nix": "{}\n", ".gitignore": ".confctl/\n"}
            for name, content in files.items():
                (source / name).write_text(content)
            shutil.copytree(source, self.work / ("config-" + count))
            configs[count] = str(source)
            metadata = self.root / ("machine-list-" + count + ".json")
            metadata.write_text('{"lab/nodes/node0001":{"name":"lab/nodes/node0001"}}')
            self.records[count] = {"settings": {"maxJobs": 0, "names": ["one", "two"]},
                                   "machineKeys": {"lab/nodes/node0001": "fixture-key"},
                                   "metadata": {"outputPath": str(metadata), "drvPathSha256": "a" * 64},
                                   "metadataSha256": sha(metadata), "seed": None}
        self.records["1"]["seed"] = {"key": "fixture-key",
                                     "toplevel": {"outputPath": str(seed), "drvPathSha256": "b" * 64},
                                     "autoRollback": {"outputPath": str(rollback), "drvPathSha256": "c" * 64}}
        self.manifest = {"configs": configs, "expectedRuntime": {"schema": 1, "scales": self.records},
                         "configurationSources": {count: {"sourcePath": configs[count], "narHash": "sha256-fixture-" + count}
                                                   for count in SCALES}}
        self.write_manifest()
        self.env = os.environ.copy()
        self.env.update(CONFCTL_FIXTURE_KEY="fixture-key", CONFCTL_FIXTURE_SEED=str(seed),
                        CONFCTL_FIXTURE_ROLLBACK=str(rollback), GATE_STUB_ROOT=str(self.root),
                        GATE_STUB_STATE=str(self.root / "state.json"), HOME=str(self.root), USER="root",
                        GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null")
        for count in SCALES:
            self.runtime_file(count).write_text(json.dumps(self.projection(count)))
        (self.root / "state.json").write_text("{}")

    def write_manifest(self):
        self.manifest_path.write_text(json.dumps(self.manifest))

    def projection(self, count):
        return {k: copy.deepcopy(v) for k, v in self.records[count].items() if k != "metadataSha256"}

    def runtime_file(self, count):
        return self.root / ("runtime-" + count + ".json")

    def compare(self, count="1", value=None):
        if value is not None:
            self.runtime_file(count).write_text(json.dumps(value))
        return self.gate("compare", count, self.runtime_file(count))

    def gate(self, *args):
        return subprocess.run([RUBY, "-", str(self.manifest_path), *map(str, args)],
                              input=COMPARATOR, text=True, capture_output=True, env=self.env, timeout=15)

    def write_command(self, name, body):
        path = self.bin / name
        path.write_text("#!" + sys.executable + "\n" + body)
        path.chmod(0o755)
        return path

    def run_setup(self, **state):
        (self.root / "state.json").write_text(json.dumps(state))
        shutil.copyfile(self.manifest_path, self.artifacts / "packages.json")
        self.write_command("nix", NIX_STUB)
        self.write_command("strace", TRACE_STUB)
        self.write_command("compat-real", '''import hashlib, json, os, pathlib
root = pathlib.Path(os.environ["GATE_STUB_ROOT"])
receipt = json.loads((root / "artifacts/preseed-gate.json").read_text())
assert receipt["schema"] == 1 and receipt["status"] == "passed"
assert receipt["packageManifestSha256"] == hashlib.sha256((root / "packages.json").read_bytes()).hexdigest()
for field in ("runtimeRecordSha256s", "lockSha256s", "originalFileInventorySha256s"):
    assert set(receipt[field]) == {"1", "10", "100", "1000"}
(root / "sentinel").write_text("reached")
''')
        ruby_wrapper = self.write_command("fixture-ruby", "import os, sys\n"
            "if sys.argv[1:3] == ['-retc', '-e']:\n print('root')\n"
            "else:\n os.execv(" + repr(RUBY) + ", [" + repr(RUBY) + "] + sys.argv[1:])\n")
        clients = self.root / "clients/bin"
        clients.mkdir(parents=True)
        for name in ("oracle", "native"):
            path = clients / name
            path.write_text("#!" + BASH + "\nexit 0\n")
            path.chmod(0o755)
        self.env.update(CONFCTL_FIXTURE_RUBY=str(ruby_wrapper), CONFCTL_REAL_ORACLE=str(clients / "oracle"),
                        CONFCTL_REAL_NATIVE=str(clients / "native"), PATH=str(self.bin) + ":" + self.env["PATH"])
        # Rebind only fixed filesystem locations in this actual source segment.
        # SSH/namespace startup is outside the owning gate unit, never simulated.
        script = "set -euo pipefail\n" + FUNCTION + SETUP
        seed_source = '"$CONFCTL_FIXTURE_SEED/etc/confctl/inputs-info.json"'
        seed_copy = "cp " + seed_source + " /etc/confctl/inputs-info.json\n"
        rebound_seed_copy = "cp " + seed_source + ' "' + str(self.root / "inputs-info.json") + '"\n'
        self.assertEqual(script.count(seed_copy), 1)
        script = script.replace(seed_copy, rebound_seed_copy, 1)
        replacements = {"/etc/confctl-fixture-packages.json": str(self.manifest_path),
                        "/nix/var/nix/profiles/system": str(self.root / "profile-system"),
                        "/run/current-system": str(self.root / "current-system"),
                        "/artifacts": str(self.artifacts), "/work": str(self.work)}
        for old, new in replacements.items():
            script = script.replace(old, new)
        self.assertIn(rebound_seed_copy, script)
        self.assertEqual(script.count(seed_source), 1)
        (self.root / "actual-setup.sh").write_text(script)
        return subprocess.run(["bash", str(self.root / "actual-setup.sh"), str(self.artifacts / "run.json")],
                              text=True, capture_output=True, env=self.env, timeout=45)

    def calls(self):
        path = self.root / "calls.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def setup_diagnostics(self, result):
        diagnostic = result.stdout + result.stderr
        seed_stderr = self.artifacts / "seed-build.stderr"
        if seed_stderr.is_file() and seed_stderr.stat().st_size:
            diagnostic += "\n" + str(seed_stderr) + ":\n" + seed_stderr.read_text()
        return diagnostic

    def assert_refused(self, result, field=None):
        diagnostic = self.setup_diagnostics(result)
        self.assertNotEqual(result.returncode, 0, diagnostic)
        self.assertFalse((self.root / "sentinel").exists())
        self.assertFalse((self.artifacts / "preseed-gate.json").exists())
        if field:
            self.assertIn(field, result.stderr, diagnostic)

    def test_actual_comparator_matches_all_scales(self):
        for count in SCALES:
            with self.subTest(count=count):
                result = self.compare(count)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_each_identity_field_mismatch(self):
        changes = [("settings", "maxJobs"), ("settings", "names"),
                   ("machineKeys", "lab/nodes/node0001"),
                   ("metadata", "outputPath"), ("metadata", "drvPathSha256"),
                   ("seed", "key"), ("seed", "toplevel", "outputPath"),
                   ("seed", "toplevel", "drvPathSha256"), ("seed", "autoRollback", "outputPath"),
                   ("seed", "autoRollback", "drvPathSha256")]
        for fields in changes:
            with self.subTest(field=fields):
                value = self.projection("1")
                target = value
                for name in fields[:-1]:
                    target = target[name]
                old = target[fields[-1]]
                target[fields[-1]] = old + "/changed" if isinstance(old, str) and old.startswith("/") else (
                    list(reversed(old)) if isinstance(old, list) else (1 if isinstance(old, int) else "d" * 64))
                self.assert_refused(self.compare(value=value), ".".join(fields))

    def test_missing_and_malformed_runtime_records(self):
        for content in ("bad json", "{}", "null", "[]"):
            with self.subTest(content=content):
                self.runtime_file("1").write_text(content)
                self.assert_refused(self.compare())
        self.runtime_file("1").unlink()
        self.assert_refused(self.compare())

    def test_exact_schema_and_scalar_types(self):
        value = self.projection("1")
        value["settings"]["maxJobs"] = 0.0
        self.assert_refused(self.compare(value=value), "settings.maxJobs")
        value = self.projection("1")
        value["extra"] = True
        self.assert_refused(self.compare(value=value), "expected fields")
        self.records["1"]["metadataSha256"] = "invalid"
        self.write_manifest()
        self.assert_refused(self.compare(), "metadataSha256")

    def test_old_string_and_outpath_identity_records_are_rejected(self):
        for fields in (("metadata",), ("seed", "toplevel"), ("seed", "autoRollback")):
            for old_shape in ("string", "outPath"):
                with self.subTest(field=fields, shape=old_shape):
                    value = self.projection("1")
                    parent = value
                    for name in fields[:-1]:
                        parent = parent[name]
                    record = parent[fields[-1]]
                    parent[fields[-1]] = record["outputPath"] if old_shape == "string" else {
                        "outPath": record["outputPath"], "drvPathSha256": record["drvPathSha256"]}
                    self.assert_refused(self.compare(value=value), ".".join(fields))

    def test_missing_and_malformed_expected_records(self):
        for value in ({}, {"configs": self.manifest["configs"], "expectedRuntime": None},
                      {"configs": self.manifest["configs"], "expectedRuntime": {"schema": 1, "scales": {}}}):
            with self.subTest(value=value):
                self.manifest_path.write_text(json.dumps(value))
                self.assert_refused(self.compare())
        self.manifest_path.write_text("bad json")
        self.assert_refused(self.compare())
        self.manifest_path.unlink()
        self.assert_refused(self.compare())

    def test_seed_environment_identity_is_checked(self):
        for name in ("CONFCTL_FIXTURE_KEY", "CONFCTL_FIXTURE_SEED", "CONFCTL_FIXTURE_ROLLBACK"):
            with self.subTest(name=name):
                old = self.env[name]
                self.env[name] += "-changed"
                self.assert_refused(self.compare(), "seed.environment")
                self.env[name] = old

    def test_configuration_source_manifest_requires_exact_scales_and_records(self):
        valid = copy.deepcopy(self.manifest["configurationSources"])
        changes = [None, {}, {key: value for key, value in valid.items() if key != "1000"},
                   {**valid, "2": valid["1"]}]
        for field, value in (("sourcePath", ""), ("sourcePath", None), ("narHash", ""), ("narHash", None)):
            sources = copy.deepcopy(valid)
            sources["1"][field] = value
            changes.append(sources)
        for field in ("sourcePath", "narHash"):
            sources = copy.deepcopy(valid)
            del sources["1"][field]
            changes.append(sources)
        for sources in changes:
            with self.subTest(sources=sources):
                self.manifest["configurationSources"] = sources
                self.write_manifest()
                self.assert_refused(self.compare(), "configurationSources")

    def test_configuration_source_receipts_refuse_before_sentinel(self):
        for mode in ("path", "narHash", "missingPath", "missingNarHash", "missingLocked"):
            with self.subTest(mode=mode):
                case = GateTest()
                case.setUp()
                case.assert_refused(case.run_setup(sourceMetadata={"1000": mode}), "configurationSource")
        case = GateTest()
        case.setUp()
        case.assert_refused(case.run_setup(missingSourceMetadataAtTrace="10"), "config-10-source-metadata.json")

    def test_setup_matches_before_cache_and_writes_fresh_receipt(self):
        # Immutable inputs can be read-only while the copied working tree is writable.
        original = Path(self.manifest["configs"]["1"]) / "cluster/inventory.nix"
        original.chmod(original.stat().st_mode & ~stat.S_IWUSR)
        (self.artifacts / "preseed-gate.json").write_text('{"status":"stale"}')
        result = self.run_setup()
        self.assertEqual(result.returncode, 0, self.setup_diagnostics(result))
        self.assertTrue((self.root / "sentinel").exists())
        receipt = json.loads((self.artifacts / "preseed-gate.json").read_text())
        self.assertEqual(receipt["packageManifestSha256"], sha(self.manifest_path))
        calls = self.calls()
        first_query = next(i for i, call in enumerate(calls) if call["args"][:2] == ["path-info", "--offline"])
        self.assertEqual([call["scale"] for call in calls[:first_query] if call["args"][0] == "eval"], list(SCALES))
        for count in SCALES:
            self.assertEqual(receipt["lockSha256s"][count], sha(self.work / ("config-" + count) / "flake.lock"))
            self.assertEqual(receipt["runtimeRecordSha256s"][count], sha(self.artifacts / ("runtime-preseed-" + count + ".json")))

    def test_readonly_rootfs_manifest_symlink_writes_exact_receipt_before_sentinel(self):
        manifest_bytes = self.manifest_path.read_bytes()
        referent = self.root / "nix/store/fixture-rootfs/etc/confctl-fixture-packages.json"
        referent.parent.mkdir(parents=True)
        referent.write_bytes(manifest_bytes)
        referent.chmod(0o444)
        referent.parent.chmod(0o555)
        self.manifest_path.unlink()
        self.manifest_path.symlink_to(referent)
        self.assertTrue(self.manifest_path.is_symlink())
        self.assertTrue(stat.S_ISREG(referent.lstat().st_mode))
        self.assertFalse(referent.stat().st_mode & 0o222)
        self.assertFalse((self.artifacts / "preseed-gate.json").exists())

        result = self.run_setup()

        self.assertEqual(result.returncode, 0, self.setup_diagnostics(result))
        receipt = json.loads((self.artifacts / "preseed-gate.json").read_text())
        self.assertEqual(receipt["packageManifestSha256"], hashlib.sha256(manifest_bytes).hexdigest())
        # The actual compat-real call reads and validates this fresh receipt
        # and its exact manifest byte digest before writing the sentinel.
        self.assertEqual((self.root / "sentinel").read_text(), "reached")
        self.assertEqual(referent.read_bytes(), manifest_bytes)
        self.assertTrue(self.manifest_path.is_symlink())

    def test_immutable_source_input_symlink_refuses_before_setup_effects(self):
        path = Path(self.manifest["configs"]["1"]) / "cluster/inventory.nix"
        before = path.read_bytes()
        referent = self.root / "immutable-input-referent.nix"
        referent.write_bytes(before)
        path.unlink()
        path.symlink_to(referent)

        self.assert_refused(self.run_setup(), "expected regular input file")

        self.assertTrue(path.is_symlink())
        self.assertEqual(path.read_bytes(), before)
        self.assertEqual(self.calls(), [])

    def test_final_working_input_symlink_refuses_before_receipt_and_sentinel(self):
        path = self.work / "config-10/cluster/inventory.nix"
        before = path.read_bytes()

        result = self.run_setup(inputSymlinkAtTrace="cluster/inventory.nix")

        self.assert_refused(result, "expected regular file")
        self.assertIn(str(path), result.stderr)
        self.assertTrue(path.is_symlink())
        self.assertEqual(path.read_bytes(), before)
        self.assertTrue(any(call["command"] == "strace" and
                            "native-process.trace" in " ".join(call["args"]) for call in self.calls()))

    def test_last_scale_identity_mismatch_prevents_all_queries_and_builds(self):
        value = self.projection("1000")
        value["metadata"]["drvPathSha256"] = "d" * 64
        self.runtime_file("1000").write_text(json.dumps(value))
        self.assert_refused(self.run_setup(), "scale 1000.metadata.drvPathSha256")
        self.assertFalse(any(call["args"][0] in ("path-info", "build") for call in self.calls()))

    def test_stale_passed_receipt_does_not_replace_current_assertions(self):
        stale = '{"schema":1,"status":"passed"}'
        (self.artifacts / "preseed-gate.json").write_text(stale)
        value = self.projection("1000")
        value["settings"]["maxJobs"] = 1
        self.runtime_file("1000").write_text(json.dumps(value))
        result = self.run_setup()
        self.assertNotEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.root / "sentinel").exists())
        self.assertEqual((self.artifacts / "preseed-gate.json").read_text(), stale)

    def test_query_failure_and_unavailable_outputs_prevent_cache_build(self):
        for mode in ("queryFailure", "unavailable", "missingNode", "missingReference"):
            with self.subTest(mode=mode):
                # Each subcase needs fresh Git/work/evidence state.
                case = GateTest()
                case.setUp()
                case.assert_refused(case.run_setup(**{mode: "1"}))
                self.assertFalse(any(call["args"][0] == "build" for call in case.calls()))

    def test_eval_failure_and_malformed_record_prevent_effects(self):
        for mode in ("evalFailure", "malformed"):
            with self.subTest(mode=mode):
                case = GateTest()
                case.setUp()
                case.assert_refused(case.run_setup(**{mode: "10"}))
                self.assertFalse(any(call["args"][0] in ("path-info", "build") for call in case.calls()))

    def test_metadata_mutation_at_each_barrier_refuses(self):
        for mode in ("metadataAtQuery", "metadataAtCache", "metadataAtTrace"):
            with self.subTest(mode=mode):
                case = GateTest()
                case.setUp()
                case.assert_refused(case.run_setup(**{mode: "1"}), "metadataSha256")
                if mode == "metadataAtQuery":
                    self.assertFalse(any(call["args"][0] == "build" for call in case.calls()))

    def test_lock_and_original_input_mutations_refuse_before_sentinel(self):
        for change in ({"lockAtCache": "1"}, {"inputAtTrace": "flake.lock"},
                       {"inputAtTrace": "cluster/inventory.nix"}, {"trackedAtTrace": True},
                       {"inventoryAtTrace": True}, {"runtimeAtTrace": True}):
            with self.subTest(change=change):
                case = GateTest()
                case.setUp()
                case.assert_refused(case.run_setup(**change))

    def test_initial_working_inputs_must_equal_immutable_source(self):
        (self.work / "config-1/flake.lock").write_text("different lock")
        self.assert_refused(self.run_setup(), "initialInputs")
        self.assertEqual(self.calls(), [])

    def test_owner_executable_mutation_during_trace_refuses_before_sentinel(self):
        path = self.work / "config-10/cluster/inventory.nix"
        before_bytes = path.read_bytes()
        before_mode = stat.S_IMODE(path.stat().st_mode)
        result = self.run_setup(ownerExecutableAtTrace="cluster/inventory.nix")
        self.assert_refused(result, "scale 10.inputs.cluster/inventory.nix.ownerExecutable")
        self.assertEqual(path.read_bytes(), before_bytes)
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), before_mode ^ stat.S_IXUSR)
        self.assertTrue(any(call["command"] == "strace" and
                            "native-process.trace" in " ".join(call["args"]) for call in self.calls()))

    def test_initial_owner_executable_must_equal_immutable_source(self):
        path = self.work / "config-1/cluster/inventory.nix"
        path.chmod(path.stat().st_mode ^ stat.S_IXUSR)
        self.assert_refused(self.run_setup(), "initialInputs.cluster/inventory.nix.ownerExecutable")
        self.assertEqual(self.calls(), [])


if __name__ == "__main__":
    unittest.main()
