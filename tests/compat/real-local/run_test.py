"""Actual launcher control flow with tiny retained stubs, never real Podman."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
import uuid

SLUG = "2026-10-09-evaluate-confctl-rewrite-language"
WORKSPACE = "/home/aither/workspace/ai/vpsfree.cz"
PREFIX = "/tmp/" + SLUG + "-real-storage."
IMAGE = "sha256:" + "a" * 64
LAUNCHER = Path(__file__).with_name("run.sh")

PODMAN_STUB = r'''
import hashlib, json, os, pathlib, signal, sys, time
a = sys.argv[1:]
state = json.loads(pathlib.Path(os.environ["STUB_STATE"]).read_text())
assert not any(k in os.environ for k in ("SSH_AUTH_SOCK", "CONTAINER_HOST", "CONTAINER_CONNECTION"))
if a == ["--version"]:
    command = "version"
    root = None
else:
    assert a[:12:2] == ["--root", "--runroot", "--tmpdir", "--storage-driver", "--cgroup-manager", "--events-backend"]
    root = pathlib.Path(a[1]).parent
    assert a[1:6:2] == [str(root / "storage"), str(root / "runroot"), str(root / "tmp")]
    assert a[7:12:2] == ["vfs", "cgroupfs", "file"]
    assert os.environ["XDG_RUNTIME_DIR"] == str(root / "runtime")
    a = a[12:]
    command = a[0]
with open(os.environ["STUB_LOG"], "a") as f:
    f.write(json.dumps({"command": command, "args": a, "root": str(root) if root else None}) + "\n")
if command == "version":
    print("podman version " + state.get("version", "5.8.8"))
elif command == "info":
    if state.get("failBeforeStart") and (root / "last-run.json").exists():
        marker = json.loads((root / "last-run.json").read_text())
        if marker["status"] == "running" and marker["containerId"]:
            print("synthetic pre-start failure")
            sys.exit(1)
    print(json.dumps({"host": {"security": {"rootless": state.get("rootless", True)},
                              "cgroupManager": "cgroupfs", "eventLogger": "file"},
                      "store": {"graphDriverName": state.get("driver", "vfs"),
                                "graphRoot": str(root / "storage"),
                                "runRoot": state.get("runRoot", str(root / "runroot"))},
                      "version": {"Version": state.get("version", "5.8.8")}}))
elif command == "load":
    assert a[1] == "--input"
    assert json.loads((root / "owner.json").read_text())["status"] == "initializing"
    if state.get("failLoad"):
        print("synthetic import failure")
        sys.exit(2)
    (root / "storage" / "image.id").write_text("a" * 64)
elif command == "image":
    assert a[1:] == ["inspect", "sha256:" + "a" * 64]
    if not (root / "storage" / "image.id").exists():
        sys.exit(1)
    print(json.dumps([{"Id": state.get("inspectId", "a" * 64)}]))
elif command == "create":
    cid = pathlib.Path(a[a.index("--cidfile") + 1])
    name = a[a.index("--name") + 1]
    assert cid.name == "podman.cid" and cid.parent.parent == pathlib.Path("/tmp")
    assert str(cid.parent).startswith("/tmp/2026-10-09-evaluate-confctl-rewrite-language-real-evidence.")
    expected = ["create", "--rm", "--pull", "never", "--name", name, "--cidfile", str(cid),
                "--no-hosts", "--network", "none", "--userns", "host", "--user", "0:0",
                "--cgroups", "disabled", "--security-opt", "no-new-privileges", "--cap-drop", "ALL"]
    for cap in ("CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "SYS_CHROOT", "KILL"):
        expected += ["--cap-add", cap]
    expected += ["--pids-limit", "0", "--ulimit", "nofile=65536:65536", "--mount",
                 "type=bind,src=" + str(cid.parent) + ",dst=/artifacts",
                 "sha256:" + "a" * 64, "/bin/fixture-run", "/artifacts/run.json"]
    assert a == expected, "Changed isolation command: " + repr(a)
    (cid.parent / "stub-fresh-container.json").write_text(json.dumps({"name": name, "cwd": str(cid.parent)}))
    actual_id = hashlib.sha256(os.urandom(32)).hexdigest()
    record = {"id": actual_id, "name": name, "cidfile": str(cid)}
    (root / "storage" / ("container-" + actual_id + ".json")).write_text(json.dumps(record))
    if not state.get("noCid"):
        cid.write_text(state.get("cidValue", actual_id) + "\n")
    if state.get("capacityLossAtCreate"):
        pathlib.Path(os.environ["STUB_CAPACITY_LOSS_MARKER"]).write_text(state["capacityLossAtCreate"])
    print(actual_id)
    if state.get("createSignal"):
        os.kill(os.getpid(), signal.SIGTERM)
    sys.exit(125 if state.get("failCreate") else 0)
elif command == "start":
    assert a[:3] == ["start", "--attach", "--sig-proxy=true"] and len(a) == 4
    record = json.loads((root / "storage" / ("container-" + a[3] + ".json")).read_text())
    assert a[3] == record["id"]
    cid = pathlib.Path(record["cidfile"])
    # Identity must already be durable before any fixture work can complete.
    assert (cid.parent / "container.id").read_text().strip() == record["id"]
    marker = json.loads((root / "last-run.json").read_text())
    assert marker["status"] == "running" and marker["containerId"] == record["id"]
    if state.get("hold"):
        pathlib.Path(state["started"]).write_text(record["name"])
        deadline = time.monotonic() + 12
        while not pathlib.Path(state["release"]).exists():
            assert time.monotonic() < deadline, "stub release deadline"
            time.sleep(.01)
    # Model Podman's --rm contract. The persistent capture must survive this.
    cid.unlink()
    if state.get("signal"):
        os.kill(os.getpid(), signal.SIGTERM)
    sys.exit(state.get("exitCode", 0))
else:
    raise AssertionError("Unexpected Podman operation: " + command)
'''

PYTHON_STUB = r'''
import os, sys
original = os.statvfs
def controlled(path):
    data = list(original(path))
    if os.environ.get("STUB_UNKNOWN_INODES"):
        data[5] = 0
    marker = os.environ.get("STUB_CAPACITY_LOSS_MARKER")
    if marker and os.path.isfile(marker):
        with open(marker) as f:
            resource = f.read()
        assert resource in ("bytes", "inodes")
        data[4 if resource == "bytes" else 7] = 0
    return os.statvfs_result(data)
os.statvfs = controlled
assert sys.argv[1] == "-c"
code = sys.argv[2]
sys.argv = ["-c", *sys.argv[3:]]
exec(compile(code, "launcher-inline", "exec"), {"__name__": "__main__"})
'''

DU_STUB = r'''
import json, os, pathlib, sys
state = json.loads(pathlib.Path(os.environ["STUB_STATE"]).read_text())
if state.get("capacityLoss"):
    pathlib.Path(os.environ["STUB_CAPACITY_LOSS_MARKER"]).write_text(state["capacityLoss"])
print("8\t" + sys.argv[-1])
'''


class LauncherTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if os.getuid() == 0:
            raise RuntimeError("Run launcher tests as the unprivileged rootless caller")
        cls.base = Path(tempfile.mkdtemp(prefix="/tmp/" + SLUG + "-real-launcher-tests."))
        cls.bin = cls.base / "bin"
        cls.bin.mkdir()
        interpreter = str(Path(sys.executable).resolve())
        for name, body in [("podman", PODMAN_STUB), ("python3", PYTHON_STUB), ("du", DU_STUB)]:
            path = cls.bin / name
            path.write_text("#!" + interpreter + "\n" + body)
            path.chmod(0o700)
        print("Retained launcher stub evidence: " + str(cls.base), file=sys.stderr)

    def setUp(self):
        self.case = self.base / self._testMethodName
        self.case.mkdir()
        self.log = self.case / "calls.jsonl"
        self.state = self.case / "stub-state.json"
        self.state.write_text("{}")
        self.archive = self.case / "image.tar.gz"
        self.archive.write_text("synthetic image archive; no real image")
        self.spec = self.case / "run.json"
        self.spec.write_text(json.dumps({"schema": 1, "tier": "real-local-container", "scales": [1]}))
        self.cap = self.case / "capacity.json"
        self.budget = {"schema": 1, "reserveBytes": 1, "reserveInodes": 1, "budgetBytes": 1,
                       "budgetInodes": 1, "basis": "Tiny command stubs only; not a real image capacity estimate"}
        self.cap.write_text(json.dumps(self.budget))
        self.env = {**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
                    "DEV_SESSION_SLUG": SLUG, "DEV_SESSION_WORKSPACE": WORKSPACE,
                    "STUB_STATE": str(self.state), "STUB_LOG": str(self.log),
                    "STUB_CAPACITY_LOSS_MARKER": str(self.case / "capacity-loss"),
                    "SSH_AUTH_SOCK": "test-only", "CONTAINER_HOST": "test-only", "CONTAINER_CONNECTION": "test-only"}
        self.sequence = 0

    def call(self, *args, env=None):
        self.sequence += 1
        result = subprocess.run(["bash", str(LAUNCHER), *map(str, args)], env=env or self.env,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=15)
        (self.case / (str(self.sequence) + ".stdout")).write_text(result.stdout)
        (self.case / (str(self.sequence) + ".stderr")).write_text(result.stderr)
        return result

    def init(self, success=True):
        result = self.call("init", self.archive, IMAGE, self.cap)
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        root = Path(result.stdout.splitlines()[0])
        self.assertTrue(str(root).startswith(PREFIX))
        (self.case / ("store-" + root.name + ".txt")).write_text(str(root))
        return root

    def events(self):
        return [json.loads(x) for x in self.log.read_text().splitlines()] if self.log.exists() else []

    def mutations(self):
        return [x for x in self.events() if x["command"] in ("load", "create", "start")]

    def change_state(self, **values):
        self.state.write_text(json.dumps(values))

    def change_owner(self, store_root, **values):
        p = store_root / "owner.json"
        original = json.loads(p.read_text())
        (self.case / ("owner-before-" + uuid.uuid4().hex + ".json")).write_text(p.read_text())
        p.write_text(json.dumps({**original, **values}))

    def refused_run(self, root, text=None):
        before = len(self.mutations())
        result = self.call("run", root, self.spec, self.cap)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.mutations()), before, result.stderr)
        if text:
            self.assertIn(text, result.stderr)

    def test_one_import_two_fresh_runs_and_isolation(self):
        root = self.init()
        owner = json.loads((root / "owner.json").read_text())
        self.assertEqual(owner["status"], "ready")
        self.assertEqual(owner["archive"]["sha256"], hashlib.sha256(self.archive.read_bytes()).hexdigest())
        evidence = []
        for _ in range(2):
            result = self.call("run", root, self.spec, self.cap)
            self.assertEqual(result.returncode, 0, result.stderr)
            p = Path(result.stdout.splitlines()[0])
            self.assertNotEqual(p.parent, root)
            evidence.append(p)
            status = json.loads((p / "run-status.json").read_text())
            self.assertEqual(status["status"], "completed")
            self.assertEqual(status["storeId"], owner["storeId"])
            self.assertEqual((p / "container.id").read_text().strip(), status["containerId"])
            self.assertFalse((p / "podman.cid").exists(), "stub must model --rm cidfile removal")
            self.assertEqual((p / "run.json").read_bytes(), self.spec.read_bytes())
            self.assertEqual((p / "capacity.json").read_bytes(), self.cap.read_bytes())
            self.assertTrue((p / "usage-before-container.json").is_file())
            self.assertTrue((p / "usage-after-container.json").is_file())
            self.assertTrue((p / "capacity-before-create.json").is_file())
            self.assertTrue((p / "capacity-before-start.json").is_file())
            reading = json.loads((p / "capacity-before.json").read_text())
            self.assertEqual(reading["capacity"], self.budget)
            self.assertGreater(reading["filesystems"][0]["availableInodes"], 0)
        self.assertNotEqual(evidence[0], evidence[1])
        ids = [(p / "container.id").read_text() for p in evidence]
        self.assertNotEqual(ids[0], ids[1])
        self.assertEqual([e["command"] for e in self.mutations()], ["load", "create", "start", "create", "start"])
        self.assertEqual(sum(e["command"] == "image" for e in self.events()), 3)
        self.assertEqual((root / "owner.json").read_text(), json.dumps(owner, indent=2) + "\n")

    def test_session_binding_refuses_before_podman(self):
        for key, value in [("DEV_SESSION_SLUG", "foreign"), ("DEV_SESSION_WORKSPACE", "/tmp/foreign")]:
            with self.subTest(key=key):
                env = {**self.env, key: value}
                self.assertNotEqual(self.call("init", self.archive, IMAGE, self.cap, env=env).returncode, 0)
                env.pop(key)
                self.assertNotEqual(self.call("init", self.archive, IMAGE, self.cap, env=env).returncode, 0)
        self.assertEqual(self.events(), [])

    def test_init_never_adopts_existing_path(self):
        self.assertNotEqual(self.call("init", self.case, IMAGE, self.cap).returncode, 0)
        self.assertEqual(self.events(), [])

    def test_arbitrary_and_failed_root_refused(self):
        for p in (self.case, "/tmp/" + SLUG + "-real.AVa9kJ"):
            self.refused_run(p, "Uninitialized or escaped")
        self.assertEqual(self.events(), [])

    def test_symlink_root_refused(self):
        root = self.init()
        link = Path(PREFIX + "symlink-" + uuid.uuid4().hex)
        link.symlink_to(root, target_is_directory=True)
        self.refused_run(link, "Symlink refused")

    def test_world_writable_root_refused(self):
        root = self.init()
        root.chmod(0o777)
        self.refused_run(root, "Non-private")

    def test_symlink_tuple_refused(self):
        root = self.init()
        (root / "runtime").rename(root / "runtime-retained")
        (root / "runtime").symlink_to(root / "runtime-retained", target_is_directory=True)
        self.refused_run(root, "Symlink refused")

    def test_foreign_rebound_and_root_identity_manifest_refused(self):
        for values in ({"uid": os.getuid() + 1}, {"gid": os.getgid() + 1}, {"session": "foreign"},
                       {"workspace": "/tmp/foreign"}, {"tuple": {"storage": "/tmp/foreign"}},
                       {"root": {"path": "/tmp/foreign", "device": 1, "inode": 1}}):
            with self.subTest(values=values):
                root = self.init()
                self.change_owner(root, **values)
                self.refused_run(root)

    def test_missing_malformed_and_nonready_owner_refused(self):
        for mode in ("missing", "malformed", "initializing", "unknown"):
            with self.subTest(mode=mode):
                root = self.init()
                p = root / "owner.json"
                if mode == "missing":
                    p.rename(root / "owner-retained.json")
                elif mode == "malformed":
                    p.rename(root / "owner-retained.json")
                    p.write_text("{")
                    p.chmod(0o600)
                else:
                    self.change_owner(root, status=mode)
                self.refused_run(root)

    def test_interrupted_unknown_marker_refuses(self):
        root = self.init()
        self.change_state(noCid=True)
        self.assertNotEqual(self.call("run", root, self.spec, self.cap).returncode, 0)
        self.assertEqual(json.loads((root / "last-run.json").read_text())["status"], "running")
        self.change_state()
        self.refused_run(root, "Interrupted or unknown")
        (root / "last-run.json").write_text("{}")
        self.refused_run(root, "Unknown prior run marker")

    def test_signal_and_podman_errors_keep_refusal_marker_even_with_id(self):
        for state in ({"signal": True}, {"exitCode": 125}, {"exitCode": 126},
                      {"exitCode": 127}, {"exitCode": 130}, {"exitCode": 137}):
            with self.subTest(state=state):
                self.change_state()
                root = self.init()
                self.change_state(**state)
                result = self.call("run", root, self.spec, self.cap)
                self.assertNotEqual(result.returncode, 0)
                marker = json.loads((root / "last-run.json").read_text())
                self.assertEqual(marker["status"], "running")
                self.assertTrue((Path(marker["evidence"]) / "container.id").is_file())
                self.change_state()
                self.refused_run(root, "Interrupted or unknown")

    def test_incomplete_create_and_prestart_failure_keep_known_id(self):
        for state in ({"failCreate": True}, {"createSignal": True}, {"failBeforeStart": True}):
            with self.subTest(state=state):
                self.change_state()
                root = self.init()
                self.change_state(**state)
                result = self.call("run", root, self.spec, self.cap)
                self.assertNotEqual(result.returncode, 0)
                marker = json.loads((root / "last-run.json").read_text())
                self.assertEqual(marker["status"], "running")
                p = Path(marker["evidence"])
                self.assertEqual((p / "container.id").read_text().strip(), marker["containerId"])
                self.assertTrue((p / "podman.cid").is_file())
                self.assertFalse(any(e["command"] == "start" and e["root"] == str(root) for e in self.events()))
                self.change_state()
                self.refused_run(root, "Interrupted or unknown")

    def test_invalid_id_refuses_start_without_guessing_stdout_or_name(self):
        root = self.init()
        self.change_state(cidValue="short-id")
        result = self.call("run", root, self.spec, self.cap)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Unknown container ID", result.stderr)
        marker = json.loads((root / "last-run.json").read_text())
        self.assertIsNone(marker["containerId"])
        self.assertFalse((Path(marker["evidence"]) / "container.id").exists())
        self.assertEqual([e["command"] for e in self.mutations()], ["load", "create"])

    def test_known_workload_failure_keeps_evidence_and_allows_fresh_run(self):
        root = self.init()
        self.change_state(exitCode=1)
        result = self.call("run", root, self.spec, self.cap)
        self.assertEqual(result.returncode, 1, result.stderr)
        evidence = Path(result.stdout.splitlines()[0])
        self.assertEqual(json.loads((evidence / "run-status.json").read_text())["exitCode"], 1)
        self.change_state()
        result = self.call("run", root, self.spec, self.cap)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotEqual(Path(result.stdout.splitlines()[0]), evidence)
        self.assertTrue((evidence / "run-status.json").is_file())

    def test_changed_archive_and_prior_evidence_refused(self):
        root = self.init()
        self.archive.write_text("different synthetic archive")
        self.refused_run(root, "Archive identity changed")
        root = self.init()
        result = self.call("run", root, self.spec, self.cap)
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = Path(result.stdout.splitlines()[0])
        (evidence / "run.json").write_text("{}")
        self.refused_run(root, "Prior run evidence changed")

    def test_missing_mismatched_image_never_reloads(self):
        for missing in (True, False):
            with self.subTest(missing=missing):
                root = self.init()
                if missing:
                    (root / "storage" / "image.id").rename(root / "storage" / "image-id-retained")
                else:
                    self.change_state(inspectId="b" * 64)
                self.refused_run(root, "Exact image unavailable" if missing else "Image ID mismatch")
                self.change_state()

    def test_live_tuple_rootless_driver_and_version_refused(self):
        root = self.init()
        for changes in ({"rootless": False}, {"driver": "overlay"}, {"runRoot": "/tmp/rebound"}, {"version": "9.0"}):
            with self.subTest(changes=changes):
                self.change_state(**changes)
                self.refused_run(root)
        self.change_state()

    def test_invalid_capacity_never_loads(self):
        variants = [{**self.budget, k: v} for k in ("reserveBytes", "reserveInodes", "budgetBytes", "budgetInodes")
                    for v in (0, -1, True, 1.0)]
        variants += [{**self.budget, "basis": " "}, {**self.budget, "schema": 2},
                     {k: v for k, v in self.budget.items() if k != "budgetInodes"}]
        for c in variants:
            with self.subTest(capacity=c):
                self.cap.write_text(json.dumps(c))
                self.assertNotEqual(self.call("init", self.archive, IMAGE, self.cap).returncode, 0)
        self.cap.write_text('{"schema":1,"schema":1}')
        self.assertNotEqual(self.call("init", self.archive, IMAGE, self.cap).returncode, 0)
        self.assertEqual(self.events(), [])

    def test_insufficient_bytes_and_inodes_never_load_or_run(self):
        root = self.init()
        v = os.statvfs(root)
        for k, amount in (("budgetBytes", v.f_bavail * v.f_frsize + (1 << 64)), ("budgetInodes", v.f_favail + (1 << 64))):
            with self.subTest(resource=k):
                self.cap.write_text(json.dumps({**self.budget, k: amount}))
                before = len(self.mutations())
                self.init(success=False)
                self.refused_run(root, "Insufficient available")
                self.assertEqual(len(self.mutations()), before)

    def test_unknown_inode_information_blocks(self):
        self.env["STUB_UNKNOWN_INODES"] = "1"
        root = self.init(success=False)
        self.assertEqual(json.loads((root / "owner.json").read_text())["status"], "initializing")
        self.assertEqual(self.mutations(), [])

    def test_capacity_lost_during_accounting_refuses_creation(self):
        for resource in ("bytes", "inodes"):
            with self.subTest(resource=resource):
                self.env["STUB_CAPACITY_LOSS_MARKER"] = str(self.case / ("loss-" + resource))
                self.change_state()
                root = self.init()
                self.change_state(capacityLoss=resource)
                before = len(self.mutations())
                result = self.call("run", root, self.spec, self.cap)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Insufficient available " + resource, result.stderr)
                self.assertEqual(len(self.mutations()), before, "must refuse before create/start")
                p = Path(result.stdout.splitlines()[0])
                initial = json.loads((p / "capacity-before.json").read_text())["filesystems"][0]
                final = json.loads((p / "capacity-before-create.json").read_text())["filesystems"][0]
                field = "availableBytes" if resource == "bytes" else "availableInodes"
                self.assertGreater(initial[field], 0)
                self.assertEqual(final[field], 0)
                self.assertTrue((p / "usage-before-container.json").is_file())

    def test_capacity_lost_during_creation_refuses_start_and_retains_id(self):
        for resource in ("bytes", "inodes"):
            with self.subTest(resource=resource):
                self.env["STUB_CAPACITY_LOSS_MARKER"] = str(self.case / ("loss-" + resource))
                self.change_state()
                root = self.init()
                self.change_state(capacityLossAtCreate=resource)
                result = self.call("run", root, self.spec, self.cap)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Insufficient available " + resource, result.stderr)
                calls = [e["command"] for e in self.mutations() if e["root"] == str(root)]
                self.assertEqual(calls, ["load", "create"], "must refuse before start")
                marker = json.loads((root / "last-run.json").read_text())
                self.assertEqual(marker["status"], "running")
                p = Path(marker["evidence"])
                self.assertEqual((p / "container.id").read_text().strip(), marker["containerId"])
                final = json.loads((p / "capacity-before-start.json").read_text())["filesystems"][0]
                self.assertEqual(final["availableBytes" if resource == "bytes" else "availableInodes"], 0)

    def test_failed_load_never_ready(self):
        self.change_state(failLoad=True)
        root = self.init(success=False)
        self.assertEqual(json.loads((root / "owner.json").read_text())["status"], "initializing")
        self.assertFalse((root / "init" / "image.json").exists())
        self.refused_run(root, "not ready")

    def test_lock_is_nonblocking_and_spans_foreground(self):
        root = self.init()
        started, release = self.case / "started", self.case / "release"
        self.change_state(hold=True, started=str(started), release=str(release))
        process = subprocess.Popen(["bash", str(LAUNCHER), "run", str(root), str(self.spec), str(self.cap)],
                                   env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 8
            while not started.exists():
                self.assertIsNone(process.poll(), "first launcher exited before stub run")
                self.assertLess(time.monotonic(), deadline)
                time.sleep(.01)
            self.refused_run(root, "Store is busy")
        finally:
            release.write_text("release test stub")
            stdout, stderr = process.communicate(timeout=15)
            (self.case / "foreground.stdout").write_text(stdout)
            (self.case / "foreground.stderr").write_text(stderr)
        self.assertEqual(process.returncode, 0, stderr)
        self.assertEqual([e["command"] for e in self.mutations()], ["load", "create", "start"])
        self.change_state()
        lock = os.open(root / "store.lock", os.O_RDWR)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.refused_run(root, "Store is busy")
        finally:
            os.close(lock)


if __name__ == "__main__":
    unittest.main(verbosity=2)
