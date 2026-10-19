"""Unrun Linux watchdog driver for the accepted harness-free fixture artifact.

Records native identity/status/effects. It never targets the host current system
or invokes Cargo/Nix. The fixture's isolated subreaper owns all adopted waits.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import threading
import time


def identity(pid):
    raw = Path(f"/proc/{pid}/stat").read_text()
    fields = raw[raw.rfind(")") + 2:].split()
    return {"pid": pid, "ppid": int(fields[1]), "pgid": int(fields[2]),
            "sid": int(fields[3]), "tty_nr": int(fields[4]), "start": int(fields[19])}


def same_process(left, right):
    return (left["pid"], left["start"]) == (right["pid"], right["start"])


def pin_child(pid, parent_pid):
    before = identity(pid)
    if before["ppid"] != parent_pid:
        raise RuntimeError("fixture child no longer belongs to witnessed parent")
    fd = os.pidfd_open(pid)
    try:
        after = identity(pid)
        if not same_process(before, after) or after["ppid"] != parent_pid:
            raise RuntimeError("fixture child identity changed while pinning")
        return fd
    except BaseException:
        os.close(fd)
        raise


def kill_pinned(fd):
    try:
        if not select.select([fd], [], [], 0)[0]:
            signal.pidfd_send_signal(fd, signal.SIGKILL)
    except ProcessLookupError:
        pass  # Exit between readiness and pidfd signal; never retry by PID.


def barrier_identity(pid):
    before = identity(pid)
    tasks = sorted(int(path.name) for path in Path(f"/proc/{pid}/task").iterdir())
    after = identity(pid)
    if before != after or tasks != [pid]:
        raise RuntimeError("barrier worker identity changed or had more than one task")
    return before | {"task_ids": tasks, "task_count": len(tasks)}


def read_json_lines(path):
    return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []


def assert_no_deployment_effects(root):
    if any((root / name).exists() for name in ["check", "switch-events.jsonl", "switch-completed.jsonl"]):
        raise RuntimeError("deployment effect occurred before successful detach")
    if os.readlink(root / "current-system") != str(root / "old"):
        raise RuntimeError("fixture current-system link changed")


def wait_for(path, guardian, seconds=15, content=None):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if path.exists() and (content is None or path.read_bytes() == content):
            return
        if guardian.poll() is not None:
            raise RuntimeError(f"guardian ended before {path.name}")
        time.sleep(0.005)
    raise TimeoutError(f"fixture did not reach {path.name}")


def drain(pipe, data, stop, errors):
    os.set_blocking(pipe.fileno(), False)
    try:
        while not stop.is_set():
            try:
                chunk = os.read(pipe.fileno(), 65536)
            except BlockingIOError:
                stop.wait(0.005)
                continue
            if not chunk:
                return
            data.extend(chunk)
            if len(data) > 16 * 1024 * 1024:
                raise RuntimeError("fixture output exceeds 16MiB bound")
    except Exception as error:
        errors.append(repr(error))


def pin_owned_descendants(guardian_pid):
    """Pin only the fixture guardian's current descendants, including new SIDs."""
    processes = {}
    for entry in Path("/proc").iterdir():
        if entry.name.isdigit():
            try:
                row = identity(int(entry.name))
            except (FileNotFoundError, ProcessLookupError):
                continue
            processes[row["pid"]] = row
    owned = {guardian_pid}
    while True:
        expanded = owned | {pid for pid, row in processes.items() if row["ppid"] in owned}
        if expanded == owned:
            break
        owned = expanded
    pinned = []
    for pid in sorted(owned - {guardian_pid}):
        try:
            fd = os.pidfd_open(pid)
            if not same_process(identity(pid), processes[pid]):
                os.close(fd)
                continue
            pinned.append(fd)
        except (FileNotFoundError, ProcessLookupError):
            continue
    return pinned


def assert_wait_ownership(root, evidence, case):
    result = json.loads((root / "guardian-result.json").read_text())
    evidence["guardian_result"] = result
    parent = result["parent"]
    parent_pid = evidence["parent_pid"]
    waits = [row for path in sorted(root.glob("waits-*.jsonl")) for row in read_json_lines(path)]
    switches = read_json_lines(root / "switch-events.jsonl")
    completed = read_json_lines(root / "switch-completed.jsonl")
    evidence["successful_common_waits"] = waits
    evidence["switches"] = switches
    evidence["switch_completed"] = completed
    if (evidence["guardian_returncode"] != 0 or not result["no_children"]
            or result["retained_error_kinds"] or parent is None or parent["pid"] != parent_pid
            or parent["wait_api"] != "std::process::Child::wait"):
        raise RuntimeError("guardian did not complete its sole direct wait and error-free reaping")
    if case == "detach-failure":
        assert_no_deployment_effects(root)
        if parent["exit_status"] != 1 or parent["signal"] is not None or waits or result["adopted"]:
            raise RuntimeError("negative group-leader worker did not fail detach without child effects")
        if (root / "barrier" / "post-setsid.ready").exists() or Path(f"/proc/{parent_pid}").exists():
            raise RuntimeError("failed-detach worker reached post barrier or remains present")
        return
    worker_pid = evidence["worker_pre_setsid"]["pid"]
    hup = case in {"pre-detach-hup", "post-detach-hup"}
    expected_code = 0 if case == "confirm" else 1
    worker_signal = signal.SIGHUP if case == "pre-detach-hup" else None
    worker_code = None if worker_signal is not None else expected_code
    adopted = result["adopted"]
    worker_waits = [row for row in waits if row["kind"] == "worker"]
    switch_waits = [row for row in waits if row["kind"] == "switch"]
    if len(waits) != len(worker_waits) + len(switch_waits):
        raise RuntimeError("unexpected common wait kind")
    if hup:
        if (parent["signal"] != signal.SIGHUP or parent["exit_status"] is not None
                or worker_waits or len(adopted) != 1 or adopted[0]["pid"] != worker_pid
                or adopted[0]["signal"] != worker_signal or adopted[0]["exit_status"] != worker_code):
            raise RuntimeError("HUP case did not leave exactly one worker reap with the guardian")
    elif (adopted or len(worker_waits) != 1
          or worker_waits[0]["owner_pid"] != parent_pid or worker_waits[0]["child_pid"] != worker_pid
          or worker_waits[0]["exit_status"] != expected_code or worker_waits[0]["signal"] is not None
          or parent["exit_status"] != expected_code or parent["signal"] is not None):
        raise RuntimeError("helper parent did not perform exactly one successful worker wait")
    labels = [] if case == "pre-detach-hup" else (["new"] if case == "confirm" else ["new", "old"])
    if [row["label"] for row in switches] != labels or [row["label"] for row in completed] != labels:
        raise RuntimeError("switch effects/completion did not follow confirmation or rollback order")
    if len(switch_waits) != len(switches):
        raise RuntimeError("not exactly one successful common wait per switch child")
    switch_pids = [row["pid"] for row in switches]
    if len(set(switch_pids)) != len(switch_pids):
        raise RuntimeError("switch identity reused; fixture cannot attribute waits uniquely")
    for event, completion, waited in zip(switches, completed, switch_waits):
        if (event["pid"] != completion["pid"] or event["ppid"] != worker_pid
                or event["start"] != completion["start"]
                or event["pgid"] != worker_pid or event["sid"] != worker_pid
                or event["task_count"] != 1 or event["argv"] != ["boot"]
                or completion["stdout_bytes"] != 2 * 1024 * 1024
                or completion["stderr_bytes"] != 2 * 1024 * 1024
                or event["check"] != ("switching" if event["label"] == "new" else "switched")
                or waited["child_pid"] != event["pid"] or waited["owner_pid"] != worker_pid
                or waited["exit_status"] != 7 or waited["signal"] is not None):
            raise RuntimeError("switch identity, protocol, native status or successful wait did not match")
    if any(row["pid"] in switch_pids for row in adopted):
        raise RuntimeError("guardian unexpectedly adopted a switch child")
    if case in {"pre-detach-hup", "confirm"}:
        if (root / "check").exists():
            raise RuntimeError("check file unexpectedly survived no-effect or confirmation case")
    elif (root / "check").read_bytes() != b"switched":
        raise RuntimeError("rollback did not retain the switched check-file state")
    if case == "pre-detach-hup" and (root / "barrier" / "post-setsid.ready").exists():
        raise RuntimeError("worker reached post barrier after controlled pre-detach HUP")
    for pid in [parent_pid, worker_pid] + switch_pids:
        if Path(f"/proc/{pid}").exists():
            raise RuntimeError("fixture identity still present or PID reused; child-gone proof unavailable")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--case", required=True, choices=[
        "confirm", "timeout", "post-detach-hup", "pre-detach-hup", "broken-outputs", "detach-failure"])
    options = parser.parse_args()
    if not options.fixture.is_absolute() or not options.fixture.is_file():
        parser.error("an existing absolute harness-free fixture binary is required")
    if not hasattr(os, "pidfd_open") or not hasattr(signal, "pidfd_send_signal"):
        parser.error("Linux pidfd support is required for owned signal fences")
    root = options.output.resolve()
    root.mkdir(parents=True, exist_ok=True)
    if any(root.iterdir()):
        parser.error("output must be an empty owned directory")
    (root / "barrier").mkdir()
    for label in ["old", "new"]:
        system = root / label / "bin"
        system.mkdir(parents=True)
        program = system / "switch-to-configuration"
        program.write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib, sys\n"
            f"root = pathlib.Path({str(root)!r})\n"
            f"label = {label!r}\n"
            "fields = pathlib.Path('/proc/self/stat').read_text().rsplit(')',1)[1].split()\n"
            "identity = {'ppid':os.getppid(),'pgid':os.getpgrp(),'sid':os.getsid(0),'start':int(fields[19]),'task_count':len(list(pathlib.Path('/proc/self/task').iterdir()))}\n"
            "with (root/'switch-events.jsonl').open('a') as record:\n"
            " record.write(json.dumps(identity | {'label':label,'pid':os.getpid(),'argv':sys.argv[1:],'check':(root/'check').read_text()})+'\\n')\n"
            "chunk = b'x'*16384\n"
            "for _ in range(128):\n"
            " for fd in (1,2):\n"
            "  data=chunk\n"
            "  while data:\n"
            "   count=os.write(fd,data); data=data[count:]\n"
            "with (root/'switch-completed.jsonl').open('a') as record:\n"
            " record.write(json.dumps({'label':label,'pid':os.getpid(),'start':identity['start'],'stdout_bytes':128*len(chunk),'stderr_bytes':128*len(chunk)})+'\\n')\n"
            "sys.exit(7)\n")
        program.chmod(0o755)
    (root / "current-system").symlink_to(root / "old")
    environment = os.environ.copy() | {
        "CONFCTL_TEST_ROLLBACK_ROOT": str(root), "CONFCTL_TEST_ROLLBACK_GUARDIAN": "1",
        "CONFCTL_TEST_ROLLBACK_PAUSE": "both",
    }
    argv = [str(options.fixture), "-t", "60" if options.case == "confirm" else "0",
            str(root / "new"), "boot", str(root / "check")]
    if options.case == "detach-failure":
        # Only this direct private worker fixture is deliberately a group
        # leader, so the actual setsid call fails before deployment effects.
        argv.insert(1, "--__confctl-auto-rollback-worker")
    guardian = subprocess.Popen(argv, env=environment, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    stop = threading.Event()
    stdout, stderr, errors = bytearray(), bytearray(), []
    collectors = [threading.Thread(target=drain, args=(pipe, data, stop, errors))
                  for pipe, data in [(guardian.stdout, stdout), (guardian.stderr, stderr)]]
    for thread in collectors:
        thread.start()
    fds = []
    evidence = {"status": "OBSERVATIONS ONLY; no runtime acceptance inferred", "case": options.case,
                "fixture_sha256": hashlib.sha256(options.fixture.read_bytes()).hexdigest(), "argv": argv,
                "signal_scope": "explicit owned group HUP in guardian session without controlling TTY; not real sshd proof"}
    try:
        wait_for(root / "guardian-parent.json", guardian)
        parent_pid = json.loads((root / "guardian-parent.json").read_text())["pid"]
        evidence["parent_pid"] = parent_pid
        parent_fd = pin_child(parent_pid, guardian.pid)
        fds.append(parent_fd)
        ready = root / "barrier" / "pre-setsid.ready"
        wait_for(ready, guardian)
        worker_pid = int(ready.read_text())
        worker_fd = pin_child(worker_pid, guardian.pid if options.case == "detach-failure" else parent_pid)
        fds.append(worker_fd)
        evidence["worker_pre_setsid"] = barrier_identity(worker_pid)
        evidence["parent_pre_setsid"] = identity(parent_pid)
        evidence["guardian"] = identity(guardian.pid)
        pre = evidence["worker_pre_setsid"]
        parent = evidence["parent_pre_setsid"]
        guardian_identity = evidence["guardian"]
        if (parent["pgid"] != parent_pid or parent["sid"] != guardian_identity["sid"]
                or parent["ppid"] != guardian.pid or guardian_identity["sid"] != guardian.pid
                or any(row["tty_nr"] != 0 for row in [pre, parent, guardian_identity])):
            raise RuntimeError("fixture parent did not become a group leader in guardian session")
        assert_no_deployment_effects(root)
        if options.case == "detach-failure":
            if worker_pid != parent_pid or pre["pgid"] != worker_pid:
                raise RuntimeError("negative fixture worker was not the direct group leader")
            (root / "barrier" / "pre-setsid.release").write_bytes(b"")
        else:
            if (pre["ppid"] != parent_pid or pre["pgid"] != parent_pid
                    or pre["sid"] != parent["sid"] or worker_pid == parent_pid):
                raise RuntimeError("common worker did not inherit parent identity/group before detach")
            if options.case != "pre-detach-hup":
                (root / "barrier" / "pre-setsid.release").write_bytes(b"")
                ready = root / "barrier" / "post-setsid.ready"
                wait_for(ready, guardian)
                if int(ready.read_text()) != worker_pid:
                    raise RuntimeError("post-setsid barrier changed worker PID")
                post = barrier_identity(worker_pid)
                evidence["worker_post_setsid"] = post
                if (not same_process(pre, post) or post["ppid"] != parent_pid
                        or post["sid"] != worker_pid or post["pgid"] != worker_pid):
                    raise RuntimeError("same single-task worker did not become session/group leader")
                assert_no_deployment_effects(root)
            if options.case in {"post-detach-hup", "broken-outputs"}:
                stop.set()
                for thread in collectors:
                    thread.join()
                guardian.stdout.close()
                guardian.stderr.close()
            if options.case in {"post-detach-hup", "pre-detach-hup"}:
                if select.select([parent_fd], [], [], 0)[0] or identity(parent_pid) != parent:
                    raise RuntimeError("owned parent signal fence no longer holds")
                os.killpg(parent_pid, signal.SIGHUP)
            if options.case != "pre-detach-hup":
                (root / "barrier" / "post-setsid.release").write_bytes(b"")
            if options.case == "confirm":
                wait_for(root / "check", guardian, content=b"switched")
                (root / "check").write_bytes(b"confirmed")
        evidence["guardian_returncode"] = guardian.wait(timeout=45)
        assert_wait_ownership(root, evidence, options.case)
        evidence["fixture_assertions"] = "PASS; named native protocol/identity/wait assertions only"
    except Exception as error:
        evidence["driver_failure"] = repr(error)
    finally:
        if guardian.poll() is None:
            # Each pidfd was opened while the corresponding owned fixture
            # process was witnessed alive. Never signal an unpinned reused PID.
            for fd in fds:
                kill_pinned(fd)
            # Also cover a driver fault before a barrier witness was consumed.
            # Do not kill the subreaper while it still owns detached workers.
            while guardian.poll() is None:
                pinned = pin_owned_descendants(guardian.pid)
                try:
                    for fd in pinned:
                        kill_pinned(fd)
                    # An empty /proc snapshot can race spawn/reparenting. The
                    # guardian remains alive to complete its own checked waits;
                    # never kill it merely because no descendant was observed.
                    try:
                        evidence["guardian_cleanup_returncode"] = guardian.wait(timeout=1)
                    except subprocess.TimeoutExpired:
                        continue
                finally:
                    for fd in pinned:
                        os.close(fd)
        for thread in collectors:
            thread.join(timeout=5)
        stop.set()
        for thread in collectors:
            thread.join()
        for pipe in [guardian.stdout, guardian.stderr]:
            if not pipe.closed:
                pipe.close()
        for fd in fds:
            os.close(fd)
        (root / "stdout.raw").write_bytes(stdout)
        (root / "stderr.raw").write_bytes(stderr)
        evidence["collector_errors"] = errors
        evidence["check_bytes"] = list((root / "check").read_bytes()) if (root / "check").exists() else None
        for name in ["guardian-result.json", "switch-events.jsonl", "switch-completed.jsonl"]:
            if (root / name).exists():
                evidence[name] = (root / name).read_text()
        (root / "record.json").write_text(json.dumps(evidence, indent=2) + "\n")
    if "driver_failure" in evidence or errors:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
