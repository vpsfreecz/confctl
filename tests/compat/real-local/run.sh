#!/usr/bin/env bash
set -euo pipefail
# Keep caller stdin for the foreground Podman process; Python code is argv data.
exec python3 -c "$(cat <<'PY'
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
import uuid

SLUG = "2026-10-09-evaluate-confctl-rewrite-language"
WORKSPACE = "/home/aither/workspace/ai/vpsfree.cz"
PREFIX = "/tmp/" + SLUG + "-real-storage."
EVIDENCE_PREFIX = "/tmp/" + SLUG + "-real-evidence."
TUPLE = ("storage", "runroot", "runtime", "tmp")
CONFIG = {"rootless": True, "driver": "vfs", "cgroupManager": "cgroupfs", "eventsBackend": "file"}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def object_pairs(pairs):
    result = {}
    for k, v in pairs:
        require(k not in result, "Duplicate JSON key: " + k)
        result[k] = v
    return result


def json_bytes(raw):
    return json.loads(raw, object_pairs_hook=object_pairs,
                      parse_constant=lambda v: require(False, "Invalid JSON constant: " + v))


def read_json(path):
    return json_bytes(path.read_bytes())


def atomic_json(path, value):
    tmp = path.with_name(path.name + ".next-" + uuid.uuid4().hex)
    with open(tmp, "x") as f:
        json.dump(value, f, indent=2)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def private(path, directory=False):
    s = path.lstat()
    require(not stat.S_ISLNK(s.st_mode), "Symlink refused: " + str(path))
    require(stat.S_ISDIR(s.st_mode) if directory else stat.S_ISREG(s.st_mode),
            "Wrong file type: " + str(path))
    require(s.st_uid == os.getuid() and s.st_gid == os.getgid(), "Foreign owner: " + str(path))
    require(stat.S_IMODE(s.st_mode) == (0o700 if directory else 0o600),
            "Non-private permissions: " + str(path))
    if not directory:
        require(s.st_nlink == 1, "Linked state file refused: " + str(path))
    require(path.resolve() == path, "Escaped path refused: " + str(path))
    return s


def capacity(raw):
    c = json_bytes(raw)
    require(type(c) is dict and set(c) == {"schema", "reserveBytes", "reserveInodes", "budgetBytes", "budgetInodes", "basis"},
            "Capacity record fields must be explicit")
    require(type(c["schema"]) is int and c["schema"] == 1, "Capacity schema must be 1")
    for k in ("reserveBytes", "reserveInodes", "budgetBytes", "budgetInodes"):
        require(type(c[k]) is int and c[k] > 0, "Positive integer capacity required: " + k)
    require(type(c["basis"]) is str and c["basis"].strip(), "Capacity basis required")
    return c


def capacity_gate(paths, c, output, enforce=True):
    readings = []
    seen = set()
    for p in paths:
        device = p.stat().st_dev
        if device in seen:
            continue
        seen.add(device)
        v = os.statvfs(p)
        readings.append({"path": str(p), "device": device,
                         "fragmentBytes": v.f_frsize, "availableBlocks": v.f_bavail,
                         "availableBytes": v.f_bavail * v.f_frsize,
                         "totalInodes": v.f_files, "availableInodes": v.f_favail})
    atomic_json(output, {"schema": 1, "capacity": c, "filesystems": readings})
    for r in readings:
        require(r["fragmentBytes"] > 0 and r["availableBlocks"] >= 0,
                "Available byte capacity unknown")
        require(r["totalInodes"] > 0 and r["availableInodes"] >= 0,
                "Available inode capacity unknown")
        if enforce:
            require(r["availableBytes"] >= c["reserveBytes"] + c["budgetBytes"],
                    "Insufficient available bytes for reserve plus budget")
            require(r["availableInodes"] >= c["reserveInodes"] + c["budgetInodes"],
                    "Insufficient available inodes for reserve plus budget")


def usage(paths, output):
    # Recursive accounting is outside the foreground workload, never during it.
    result = []
    for i, p in enumerate(paths):
        values = {}
        for label, flags in (("allocatedBytes", ["--block-size=1"]), ("inodes", ["--inodes"])):
            raw = subprocess.check_output(["du", "--summarize", "--one-file-system", *flags, str(p)])
            output.with_name(output.name + "." + str(i) + "." + label + ".raw").write_bytes(raw)
            number = raw.split()[0]
            require(number.isdigit(), "Unknown allocated usage: " + str(p))
            values[label] = int(number)
        result.append({"path": str(p), **values})
    atomic_json(output, {"schema": 1, "paths": result})


def root_identity(root):
    s = private(root, directory=True)
    return {"path": str(root), "device": s.st_dev, "inode": s.st_ino}


def podman(root, lock, args, output):
    private(root / "owner.json")
    owner = read_json(root / "owner.json")
    require(owner["root"] == root_identity(root) and owner["tuple"] == {k: str(root / k) for k in TUPLE},
            "Changed owned tuple")
    require(owner["session"] == SLUG and owner["workspace"] == WORKSPACE and
            owner["uid"] == os.getuid() and owner["gid"] == os.getgid() and
            owner["configuration"] == CONFIG and owner["status"] in ("initializing", "ready"),
            "Changed store ownership")
    for name in TUPLE:
        private(root / name, directory=True)
    env = dict(os.environ)
    for name in ("SSH_AUTH_SOCK", "CONTAINER_HOST", "CONTAINER_CONNECTION"):
        env.pop(name, None)
    env["XDG_RUNTIME_DIR"] = str(root / "runtime")
    argv = ["podman", "--root", str(root / "storage"), "--runroot", str(root / "runroot"),
            "--tmpdir", str(root / "tmp"), "--storage-driver", "vfs",
            "--cgroup-manager", "cgroupfs", "--events-backend", "file", *args]
    with open(output, "xb") as f:
        return subprocess.run(argv, env=env, stdout=f, stderr=subprocess.STDOUT,
                              pass_fds=(lock,)).returncode


def version():
    env = dict(os.environ)
    for name in ("SSH_AUTH_SOCK", "CONTAINER_HOST", "CONTAINER_CONNECTION"):
        env.pop(name, None)
    value = subprocess.check_output(["podman", "--version"], env=env, text=True).strip()
    require(value.startswith("podman version "), "Unknown Podman version")
    return value


def live_info(root, lock, output):
    require(podman(root, lock, ["info", "--format", "json"], output) == 0, "Podman info failed")
    info = read_json(output)
    h, s = info["host"], info["store"]
    require(h["security"]["rootless"] is True, "Requires rootless Podman")
    require(h["cgroupManager"] == "cgroupfs" and h["eventLogger"] == "file", "Changed Podman configuration")
    require(s["graphDriverName"] == "vfs", "Requires vfs driver")
    require(s["graphRoot"] == str(root / "storage") and s["runRoot"] == str(root / "runroot"),
            "Changed live Podman tuple")
    require("podman version " + info["version"]["Version"] == read_json(root / "owner.json")["podmanVersion"],
            "Live Podman version changed")


def inspect(root, lock, image, output):
    require(podman(root, lock, ["image", "inspect", image], output) == 0, "Exact image unavailable")
    info = read_json(output)
    require(type(info) is list and len(info) == 1, "Ambiguous image inspection")
    actual = info[0]["Id"]
    if type(actual) is str and re.fullmatch("[0-9a-f]{64}", actual):
        actual = "sha256:" + actual
    require(actual == image, "Image ID mismatch")


def validate_manifest(root, m):
    fields = {"schema", "status", "session", "workspace", "uid", "gid", "storeId", "root", "tuple",
              "archive", "imageId", "podmanVersion", "configuration", "load"}
    require(type(m) is dict and set(m) == fields and type(m["schema"]) is int and m["schema"] == 1,
            "Unknown owner manifest")
    require(m["status"] == "ready", "Store is not ready; diagnosis required")
    require(m["session"] == SLUG and m["workspace"] == WORKSPACE, "Foreign session manifest")
    require(type(m["uid"]) is int and type(m["gid"]) is int and m["uid"] == os.getuid() and m["gid"] == os.getgid(),
            "Foreign uid/gid manifest")
    require(type(m["storeId"]) is str and re.fullmatch("[0-9a-f]{32}", m["storeId"]), "Invalid store ID")
    require(m["root"] == root_identity(root), "Root device/inode/path mismatch")
    require(m["tuple"] == {k: str(root / k) for k in TUPLE}, "Rebound tuple refused")
    for name in TUPLE:
        private(root / name, directory=True)
    require(type(m["configuration"]) is dict and m["configuration"] == CONFIG and
            m["configuration"]["rootless"] is True, "Changed owner configuration")
    require(type(m["imageId"]) is str and re.fullmatch("sha256:[0-9a-f]{64}", m["imageId"]), "Invalid image ID")
    a = m["archive"]
    require(type(a) is dict and set(a) == {"path", "sha256"}, "Invalid archive record")
    p = Path(a["path"])
    require(p.is_absolute() and p.resolve() == p and p.is_file() and sha(p) == a["sha256"],
            "Archive identity changed")
    load = m["load"]
    require(type(load) is dict and set(load) == {"log", "inspect", "inspectSHA256"}, "Invalid load record")
    require(load["log"] == str(root / "init" / "load.log") and load["inspect"] == str(root / "init" / "image.json"),
            "Rebound load evidence")
    private(root / "init", directory=True)
    private(Path(load["log"]))
    private(Path(load["inspect"]))
    require(sha(load["inspect"]) == load["inspectSHA256"], "Load image evidence changed")
    require(m["podmanVersion"] == version(), "Podman version changed")


def previous_run(root, store_id):
    path = root / "last-run.json"
    if not os.path.lexists(path):
        return
    private(path)
    m = read_json(path)
    fields = {"schema", "storeId", "runId", "evidence", "containerName", "status", "exitCode", "containerId",
              "runSpecSHA256", "capacitySHA256"}
    require(type(m) is dict and set(m) == fields and type(m["schema"]) is int and m["schema"] == 1,
            "Unknown prior run marker; diagnosis required")
    require(m["storeId"] == store_id and m["status"] == "completed" and type(m["exitCode"]) is int and m["exitCode"] >= 0,
            "Interrupted or unknown prior run; diagnosis required")
    require(type(m["runId"]) is str and re.fullmatch("[0-9a-f]{32}", m["runId"]) and
            m["containerName"] == "confctl-real-" + m["runId"] and
            type(m["containerId"]) is str and re.fullmatch("[0-9a-f]{64}", m["containerId"]),
            "Invalid prior run identity")
    p = Path(m["evidence"])
    require(str(p).startswith(EVIDENCE_PREFIX) and p.parent == Path("/tmp"), "Rebound evidence path")
    private(p, directory=True)
    private(p / "run-status.json")
    private(p / "run.json")
    private(p / "capacity.json")
    require(read_json(p / "run-status.json") == m, "Unknown prior run completion evidence")
    require(sha(p / "run.json") == m["runSpecSHA256"] and sha(p / "capacity.json") == m["capacitySHA256"] and
            (p / "container.id").read_text().strip() == m["containerId"], "Prior run evidence changed")


def main():
    require(os.getuid() != 0 and os.geteuid() == os.getuid(), "Requires unprivileged rootless caller")
    require(os.environ.get("DEV_SESSION_SLUG") == SLUG and os.environ.get("DEV_SESSION_WORKSPACE") == WORKSPACE,
            "Explicit matching DEV_SESSION_SLUG/DEV_SESSION_WORKSPACE required")
    require(len(sys.argv) == 5 and sys.argv[1] in ("init", "run"),
            "usage: run.sh init IMAGE_TAR IMAGE_ID CAPACITY_JSON | run.sh run STORAGE_ROOT RUN_JSON CAPACITY_JSON")
    os.umask(0o077)
    mode, first, second, capacity_path = sys.argv[1:]
    capacity_path = Path(capacity_path).resolve(strict=True)
    capacity_bytes = capacity_path.read_bytes()
    c = capacity(capacity_bytes)
    if mode == "init":
        archive = Path(first).resolve(strict=True)
        require(archive.is_file() and re.fullmatch("sha256:[0-9a-f]{64}", second),
                "Archive file and exact sha256 image ID required")
        root = Path(tempfile.mkdtemp(prefix=PREFIX))
        print(root, flush=True)
        for name in (*TUPLE, "init"):
            (root / name).mkdir(mode=0o700)
    else:
        root = Path(first)
        require(root.is_absolute() and root.parent == Path("/tmp") and
                re.fullmatch(re.escape(PREFIX) + "[A-Za-z0-9_-]{6,}", str(root)), "Uninitialized or escaped storage root")
        root_identity(root)
    lock_path = root / "store.lock"
    lock = os.open(lock_path, os.O_RDWR | os.O_NOFOLLOW | (os.O_CREAT | os.O_EXCL if mode == "init" else 0), 0o600)
    try:
        private(lock_path)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError("Store is busy; concurrent use refused")
        owner = root / "owner.json"
        if mode == "init":
            setup = root / "init"
            (setup / "capacity.json").write_bytes(capacity_bytes)
            m = {"schema": 1, "status": "initializing", "session": SLUG, "workspace": WORKSPACE,
                 "uid": os.getuid(), "gid": os.getgid(), "storeId": uuid.uuid4().hex,
                 "root": root_identity(root), "tuple": {k: str(root / k) for k in TUPLE},
                 "archive": {"path": str(archive), "sha256": sha(archive)}, "imageId": second,
                 "podmanVersion": version(), "configuration": CONFIG,
                 "load": {"log": str(setup / "load.log"), "inspect": str(setup / "image.json"), "inspectSHA256": None}}
            atomic_json(owner, m)
            capacity_gate([root, *(root / k for k in TUPLE), setup], c, setup / "capacity-before.json")
            live_info(root, lock, setup / "podman-info.json")
            require(podman(root, lock, ["load", "--input", str(archive)], setup / "load.log") == 0,
                    "Image load failed; initializing store retained")
            inspect(root, lock, second, setup / "image.json")
            usage([root], setup / "usage-after-import.json")
            m["status"] = "ready"
            m["load"]["inspectSHA256"] = sha(setup / "image.json")
            atomic_json(owner, m)
            print("Ready loaded-image store: " + str(root), flush=True)
            return 0
        private(owner)
        m = read_json(owner)
        validate_manifest(root, m)
        previous_run(root, m["storeId"])
        run_spec = Path(second).resolve(strict=True)
        run_bytes = run_spec.read_bytes()
        require(type(json_bytes(run_bytes)) is dict, "Run specification must be a JSON object")
        evidence = Path(tempfile.mkdtemp(prefix=EVIDENCE_PREFIX))
        print(evidence, flush=True)
        (evidence / "run.json").write_bytes(run_bytes)
        (evidence / "capacity.json").write_bytes(capacity_bytes)
        for source, target in (("/proc/self/status", "host-status-before"), ("/proc/self/cgroup", "host-cgroup"),
                               ("/proc/loadavg", "host-load-before")):
            (evidence / target).write_bytes(Path(source).read_bytes())
        capacity_gate([root, *(root / k for k in TUPLE), evidence], c, evidence / "capacity-before.json")
        live_info(root, lock, evidence / "podman-info.json")
        inspect(root, lock, m["imageId"], evidence / "image.json")
        usage([root, evidence], evidence / "usage-before-container.json")
        marker = {"schema": 1, "storeId": m["storeId"], "runId": uuid.uuid4().hex, "evidence": str(evidence),
                  "containerName": "", "status": "running", "exitCode": None, "containerId": None,
                  "runSpecSHA256": sha(evidence / "run.json"), "capacitySHA256": sha(evidence / "capacity.json")}
        marker["containerName"] = "confctl-real-" + marker["runId"]
        provenance = {"schema": 1, "storeId": m["storeId"],
                    "ownerManifestSHA256": sha(owner), "imageId": m["imageId"],
                    "imageInspectionSHA256": sha(evidence / "image.json"), "run": marker}
        atomic_json(evidence / "provenance.json", provenance)
        atomic_json(root / "last-run.json", marker)
        atomic_json(evidence / "run-status.json", marker)
        cid = evidence / "podman.cid"
        # Recheck after recursive accounting, directly before container creation.
        capacity_gate([root, *(root / k for k in TUPLE), evidence], c, evidence / "capacity-before-create.json")
        create_rc = podman(root, lock, ["create", "--rm", "--pull", "never", "--name", marker["containerName"],
                    "--cidfile", str(cid), "--no-hosts", "--network", "none", "--userns", "host", "--user", "0:0",
                    "--cgroups", "disabled", "--security-opt", "no-new-privileges", "--cap-drop", "ALL",
                    "--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--cap-add", "SETUID",
                    "--cap-add", "SETGID", "--cap-add", "SYS_CHROOT", "--cap-add", "KILL", "--pids-limit", "0",
                    "--ulimit", "nofile=65536:65536", "--mount", "type=bind,src=" + str(evidence) + ",dst=/artifacts",
                    m["imageId"], "/bin/fixture-run", "/artifacts/run.json"], evidence / "container-create.log")
        # create does not start the fixture. Copy its actual full ID before start
        # can exit and --rm removes Podman's cidfile; never derive an ID from name.
        if os.path.lexists(cid):
            require(cid.resolve() == cid and cid.is_file(), "Unknown container ID; marker retained for diagnosis")
            cid_bytes = cid.read_bytes()
            container_id = cid_bytes.decode().strip()
            require(re.fullmatch("[0-9a-f]{64}", container_id), "Unknown container ID; marker retained for diagnosis")
            with open(evidence / "container.id", "xb") as f:
                f.write(cid_bytes)
                f.flush()
                os.fsync(f.fileno())
            marker["containerId"] = container_id
            atomic_json(evidence / "provenance.json", provenance)
            atomic_json(root / "last-run.json", marker)
            atomic_json(evidence / "run-status.json", marker)
        require(create_rc == 0, "Container creation failed; marker retained for diagnosis")
        require(marker["containerId"] is not None, "Unknown container ID; marker retained for diagnosis")
        live_info(root, lock, evidence / "podman-info-before-start.json")
        capacity_gate([root, *(root / k for k in TUPLE), evidence], c, evidence / "capacity-before-start.json")
        rc = podman(root, lock, ["start", "--attach", "--sig-proxy=true", container_id], evidence / "container.log")
        # Podman reserves 125-127 for launcher errors; signal-style returns are
        # ambiguous for this fixed fixture. Keep their marker for diagnosis.
        require(0 <= rc < 125,
                "Interrupted or unknown container result; marker retained for diagnosis")
        marker.update(status="completed", exitCode=rc)
        for source, target in (("/proc/self/status", "host-status-after"), ("/proc/loadavg", "host-load-after")):
            (evidence / target).write_bytes(Path(source).read_bytes())
        usage([root, evidence], evidence / "usage-after-container.json")
        capacity_gate([root, *(root / k for k in TUPLE), evidence], c, evidence / "capacity-after.json", enforce=False)
        atomic_json(evidence / "run-status.json", marker)
        atomic_json(root / "last-run.json", marker)
        print("Retained real-tier evidence: " + str(evidence), flush=True)
        return rc
    finally:
        os.close(lock)


try:
    sys.exit(main())
except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError, KeyboardInterrupt) as error:
    print("error: " + str(error), file=sys.stderr)
    sys.exit(1)
PY
)" "$@"
