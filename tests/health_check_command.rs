//! Public health checks through the actual parser/app and confined fake tools.
#![cfg(target_os = "linux")]
#[path = "support/native_tool.rs"]
mod native_tool;
use serde_json::{Value, json};
use std::{
    collections::BTreeSet,
    fs,
    io::Write,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::{Child, Command, Output, Stdio},
    thread,
    time::{Duration, Instant},
};
struct Fixture {
    _tmp: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
    saved: Vec<(PathBuf, Vec<u8>)>,
}
fn executable(path: &Path, text: &str) {
    fs::write(path, text).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}
fn command(argv: Value) -> Value {
    json!({"description":"configured command","command":argv,"timeout":0,"cooldown":0})
}
impl Fixture {
    fn new() -> Self {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("conf");
        let bin = tmp.path().join("bin");
        fs::create_dir(&root).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::create_dir(root.join("native")).unwrap();
        std::os::unix::fs::symlink(native_tool::resolve("sleep"), root.join("native/sleep"))
            .unwrap();

        fs::write(root.join("flake.nix"), "{}\n").unwrap();
        let remote = json!({"name":"remote","key":"remote-key","metaConfig":{"managed":true,"spin":"nixos","tags":["selected"],"host":{"target":"remote.invalid","port":"02_222suffix"},"replace":"{name}","healthChecks":{
            "builderCommands":[{"description":"builder argv","command":["builder-check","{name}","{key}","{host.target}","x{missing}y","{checks}","{replace}"],"exitStatus":7,"timeout":0,"cooldown":0,"standardOutput":{"match":"builder exact\n"},"standardError":{"include":["builder error"]}}],
            "systemd":{"enable":true,"systemProperties":[{"property":"SystemState","value":"running","timeout":1,"cooldown":0}],"unitProperties":{"unit 'literal.service":[{"property":"ActiveState","value":"active","timeout":1,"cooldown":0}]}},
            "machineCommands":[{"description":"machine argv","command":["machine-check","health space","$literal","","{checks}"],"timeout":1,"cooldown":0,"standardOutput":{"include":["healthy"],"exclude":["panic"]},"standardError":{"match":""}}]
        }}});
        let local = json!({"name":"local","metaConfig":{"managed":true,"spin":"vpsadminos","tags":["selected"],"host":{"target":"localhost"},"healthChecks":{"systemd":{"enable":true},"machineCommands":[command(json!(["machine-check","local"]))]}}});
        fs::write(root.join("machines.json"),serde_json::to_vec(&json!({"remote":remote,"local":local,
            "unmanaged":{"name":"unmanaged","metaConfig":{"managed":false,"host":{"target":"unmanaged.invalid"},"healthChecks":{"builderCommands":[command(json!(["must-not-run"]))]}}},
            "null":{"name":"null","metaConfig":{"managed":true,"host":{"target":null},"healthChecks":{"builderCommands":[command(json!(["must-not-run"]))]}}},
            "carried":{"name":"carrier#guest","carrier":"remote","metaConfig":{"managed":true,"host":{"target":"guest.invalid"},"healthChecks":{"builderCommands":[command(json!(["must-not-run"]))]}}},
            "empty":{"name":"empty","metaConfig":{"managed":true,"host":{"target":"empty.invalid"},"healthChecks":{"builderCommands":[]}}}
        })).unwrap()).unwrap();
        let mut saved = Vec::new();
        for (name, bytes) in [
            ("flake.lock", b"retained lock\n".as_slice()),
            ("kernels.json", b"{\"retained\":true}\n"),
            (".confctl/generations/retained", b"saved bytes\n"),
        ] {
            let path = root.join(name);
            fs::create_dir_all(path.parent().unwrap()).unwrap();
            fs::write(&path, bytes).unwrap();
            saved.push((path, bytes.to_vec()));
        }
        executable(
            &bin.join("nix"),
            r#"#!/bin/sh
printf 'nix %s\n' "$*" >> "$HC_ROOT/nix-calls"
last=
for arg do last="$arg"; done
case "$last" in
 '.#confctl.settings') printf '%s\n' '{}' ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$HC_ROOT" ;;
 *) exit 97 ;;
esac
"#,
        );
        executable(
            &bin.join("ssh"),
            r#"#!/bin/sh
printf '%s\0' ssh "$@" > "$HC_ROOT/ssh-args-$$"
while [ "$#" -gt 0 ]; do
 case "$1" in -F|-l|-p) shift;shift ;; *) break ;; esac
done
[ "$1" = remote.invalid ] || exit 96
shift
[ "$#" = 1 ] || exit 95
HC_HOST=remote;export HC_HOST
exec /bin/sh -c "exec $1"
"#,
        );
        executable(
            &bin.join("builder-check"),
            r#"#!/bin/sh
printf 'start %s\n' "$$" >> "$HC_ROOT/events"
printf '%s\0' builder "$@" > "$HC_ROOT/argv-builder-$$"
"$HC_ROOT/native/sleep" 0.03
printf 'builder exact\n'
printf 'builder error\n' >&2
printf 'end %s\n' "$$" >> "$HC_ROOT/events"
exit 7
"#,
        );
        executable(
            &bin.join("machine-check"),
            r#"#!/bin/sh
printf 'start %s\n' "$$" >> "$HC_ROOT/events"
printf '%s\0' "machine $HC_HOST" "$@" > "$HC_ROOT/argv-machine-$$"
if [ "$HC_MODE" = slow ]; then printf '%s' "$$" > "$HC_ROOT/pid";exec "$HC_ROOT/native/sleep" 60;fi
"$HC_ROOT/native/sleep" 0.03
if [ "$HC_MODE" = retry ] && [ ! -e "$HC_ROOT/attempt-$HC_HOST" ]; then
 printf 'seen' > "$HC_ROOT/attempt-$HC_HOST";printf 'end %s\n' "$$" >> "$HC_ROOT/events"
 printf 'failed stdout\377\n';printf 'failed stderr\033\n' >&2;exit 5
fi
if [ "$HC_MODE" = retry ];then printf 'later stderr\376\n' >&2;fi
if [ "$HC_MODE" = output-retry ] && [ ! -e "$HC_ROOT/output-attempt" ];then
 printf 'seen' > "$HC_ROOT/output-attempt";printf 'end %s\n' "$$" >> "$HC_ROOT/events"
 printf 'unhealthy stdout\377\n';printf 'unhealthy stderr\033\n' >&2;exit 0
fi
if [ "$HC_MODE" = output-retry ];then printf 'later stderr\376\n' >&2;fi
printf 'end %s\n' "$$" >> "$HC_ROOT/events"
if [ "$HC_MODE" = exhaust ] || [ "$HC_MODE" = cooldown ]; then printf 'still failing\n' >&2;exit 5;fi
if [ "$HC_MODE" = signal ];then trap - TERM;kill -TERM "$$";exit 94;fi
if [ "$HC_MODE" = exit143 ];then exit 143;fi
printf 'healthy\377\n'
"#,
        );
        executable(
            &bin.join("systemctl"),
            r#"#!/bin/sh
printf 'start %s\n' "$$" >> "$HC_ROOT/events"
printf '%s\0' systemctl "$@" > "$HC_ROOT/argv-systemctl-$$"
"$HC_ROOT/native/sleep" 0.03
printf 'end %s\n' "$$" >> "$HC_ROOT/events"
[ "$1" = show ] || exit 93
if [ "$HC_MODE" = system-code ];then exit 2;fi
if [ "$HC_MODE" = system-signal ];then trap - TERM;kill -TERM "$$";exit 92;fi
if [ "$HC_MODE" = system-retry ] && [ ! -e "$HC_ROOT/system-attempt" ];then
 printf 'seen' > "$HC_ROOT/system-attempt";printf 'SystemState=starting\n';exit 0
fi
if [ "$HC_MODE" = system-fail ];then printf 'SystemState=degraded\nActiveState=failed\n';exit 0;fi
printf 'SystemState=running\nActiveState=active\n'
"#,
        );
        Self {
            _tmp: tmp,
            root,
            bin,
            saved,
        }
    }
    fn edit(&self, edit: impl FnOnce(&mut Value)) {
        let path = self.root.join("machines.json");
        let mut value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        edit(&mut value);
        fs::write(path, serde_json::to_vec(&value).unwrap()).unwrap();
    }
    fn command(&self, args: &[&str], mode: &str) -> Command {
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        cmd.current_dir(&self.root)
            .args(["--color=never", "health-check"])
            .args(args)
            .env("PATH", &self.bin)
            .env("CONFCTL_TTY", "0")
            .env("CONFCTL_SSH_CONFIG", "health native config")
            .env("HC_ROOT", &self.root)
            .env("HC_HOST", "local")
            .env("HC_MODE", mode)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        cmd
    }
    fn run(&self, args: &[&str], mode: &str, input: &[u8]) -> Output {
        let mut child = self.command(args, mode).spawn().unwrap();
        child.stdin.take().unwrap().write_all(input).unwrap();
        wait(child)
    }
    fn unchanged(&self) {
        for (p, b) in &self.saved {
            assert_eq!(&fs::read(p).unwrap(), b);
        }
        assert!(!self.root.join(".confctl/mutation.lock").exists());
        assert!(!self.root.join("roots").exists());
    }
    fn attempts(&self) -> usize {
        fs::read_to_string(self.root.join("events"))
            .unwrap_or_default()
            .lines()
            .filter(|s| s.starts_with("start "))
            .count()
    }
    fn argv_records(&self, prefix: &str) -> Vec<(String, Vec<u8>)> {
        let mut records = fs::read_dir(&self.root)
            .unwrap()
            .filter_map(|entry| {
                let entry = entry.unwrap();
                let name = entry.file_name().to_str().unwrap().to_owned();
                name.starts_with(prefix)
                    .then(|| (name, fs::read(entry.path()).unwrap()))
            })
            .collect::<Vec<_>>();
        // Stable diagnostics only; filenames do not establish execution order.
        records.sort_by(|a, b| a.0.cmp(&b.0));
        assert!(
            records.iter().all(|(_, bytes)| bytes.ends_with(&[0])),
            "unterminated argv record: {records:?}"
        );
        records
    }
    fn assert_remote_leaders_joined(&self) {
        let ssh = self.argv_records("ssh-args-");
        let machine = self.argv_records("argv-machine-");
        let system = self.argv_records("argv-systemctl-");
        assert_eq!(ssh.len(), 3);
        assert_eq!(machine.len(), 1);
        assert_eq!(system.len(), 2);
        assert!(machine[0].1.starts_with(b"machine remote\0"));
        let pid = |name: &str| {
            let pid = name.rsplit_once('-').unwrap().1.parse::<u32>().unwrap();
            assert!(pid > 0, "{name}");
            pid
        };
        let ssh_pids = ssh
            .iter()
            .map(|(name, _)| pid(name))
            .collect::<BTreeSet<_>>();
        // The builder is local; only the three remote checkers must exec as SSH.
        let checker_pids = machine
            .iter()
            .chain(&system)
            .map(|(name, _)| pid(name))
            .collect::<BTreeSet<_>>();
        assert_eq!(checker_pids.len(), 3);
        assert_eq!(ssh_pids, checker_pids);
        for pid in checker_pids {
            assert!(!Path::new("/proc").join(pid.to_string()).exists(), "{pid}");
        }
    }
}
fn wait(mut child: Child) -> Output {
    let until = Instant::now() + Duration::from_secs(15);
    loop {
        if child.try_wait().unwrap().is_some() {
            return child.wait_with_output().unwrap();
        }
        if Instant::now() > until {
            let _ = child.kill();
            panic!(
                "health fixture timeout: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        thread::sleep(Duration::from_millis(10));
    }
}
#[test]
fn actual_three_kinds_keep_selection_native_argv_streams_and_state_unchanged() {
    let f = Fixture::new();
    let out = f.run(&["--yes", "--max-jobs=2", "--tag=selected"], "", b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(text.contains("5 checks passed, 0 failed"), "{text}");
    assert!(!text.contains("\x1b["));
    let argv = f.argv_records("argv-");
    assert_eq!(argv.len(), 5, "raw argv records: {argv:?}; {out:?}");
    let builder = b"builder\0remote\0remote-key\0remote.invalid\0xy\x000\0{name}\0";
    assert!(argv.iter().any(|(_, bytes)| bytes == builder), "{argv:?}");
    let machine = b"machine remote\0health space\0$literal\0\x003\0";
    assert!(argv.iter().any(|(_, bytes)| bytes == machine), "{argv:?}");
    for expected in [
        b"machine local\0local\0".as_slice(),
        b"systemctl\0show\0",
        b"systemctl\0show\0unit 'literal.service\0",
    ] {
        assert!(
            argv.iter().any(|(_, bytes)| bytes == expected),
            "missing {expected:?}; raw argv records: {argv:?}"
        );
    }
    let ssh = f.argv_records("ssh-args-");
    assert_eq!(ssh.len(), 3, "raw SSH records: {ssh:?}; {out:?}");
    for (_, bytes) in &ssh {
        let fields = bytes
            .strip_suffix(&[0])
            .unwrap()
            .split(|b| *b == 0)
            .collect::<Vec<_>>();
        assert_eq!(fields.len(), 9, "raw SSH records: {ssh:?}");
        assert_eq!(
            &fields[..8],
            [
                b"ssh".as_slice(),
                b"-F",
                b"health native config",
                b"-l",
                b"root",
                b"-p",
                b"2222",
                b"remote.invalid"
            ],
            "raw SSH records: {ssh:?}"
        );
    }
    assert!(
        out.stdout
            .windows(b"healthy\xff\n".len())
            .any(|s| s == b"healthy\xff\n")
    );
    assert!(String::from_utf8_lossy(&out.stderr).contains("builder error"));
    let mut active = 0;
    let mut peak = 0;
    for line in fs::read_to_string(f.root.join("events")).unwrap().lines() {
        if line.starts_with("start ") {
            active += 1;
            peak = peak.max(active);
        } else {
            active -= 1;
        }
    }
    assert_eq!(active, 0, "raw argv records: {argv:?}; {out:?}");
    assert!(
        (1..=2).contains(&peak),
        "peak {peak}; raw argv records: {argv:?}; {out:?}"
    );
    assert_eq!(f.attempts(), 5, "raw argv records: {argv:?}; {out:?}");
    assert_eq!(
        fs::read_dir(f.root.join(".confctl/logs")).unwrap().count(),
        0
    );
    f.unchanged();
}
#[test]
fn consent_empty_selection_bad_metadata_and_nonpositive_jobs_launch_no_checks() {
    for args in [
        vec![],
        vec!["--yes", "null"],
        vec!["--yes", "carrier#guest"],
        vec!["--yes", "unmanaged"],
        vec!["--yes", "empty"],
        vec!["--yes", "--max-jobs=0"],
    ] {
        let f = Fixture::new();
        let out = f.run(&args, "", b"n\n");
        assert!(!out.status.success(), "{args:?}: {out:?}");
        assert_eq!(f.attempts(), 0);
        f.unchanged();
        if args.is_empty() {
            let text = String::from_utf8_lossy(&out.stdout);
            assert!(
                text.contains("CHECKS") && text.contains("5 checks in total"),
                "{text}"
            );
        }
    }
    let f = Fixture::new();
    f.edit(|v| v["remote"]["metaConfig"]["healthChecks"] = json!({"unknown":[]}));
    let out = f.run(&["--yes"], "", b"");
    assert_eq!(out.status.code(), Some(1));
    assert_eq!(f.attempts(), 0);
    f.unchanged();
}
#[test]
fn command_and_systemd_retries_keep_failed_attempt_diagnostics_and_expected_nonzero_success() {
    for mode in ["retry", "system-retry"] {
        let f = Fixture::new();
        if mode == "retry" {
            f.edit(|v| {
                v["remote"]["metaConfig"]["healthChecks"]["machineCommands"][0]["standardError"] =
                    json!({})
            });
        }
        let out = f.run(&["--yes", "--max-jobs=1", "remote"], mode, b"");
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        let text = String::from_utf8_lossy(&out.stdout);
        assert!(
            text.contains("failed attempt 1") && text.contains("succeeded on attempt 2"),
            "{text}"
        );
        assert_eq!(f.attempts(), 5);
        f.unchanged();
    }
    for mode in [
        "exhaust",
        "system-fail",
        "signal",
        "exit143",
        "system-code",
        "system-signal",
    ] {
        let f = Fixture::new();
        f.edit(|v|{v["remote"]["metaConfig"]["healthChecks"]["machineCommands"][0]["timeout"]=0.into();
            v["remote"]["metaConfig"]["healthChecks"]["systemd"]["systemProperties"][0]["timeout"]=0.into();
            v["remote"]["metaConfig"]["healthChecks"]["systemd"]["unitProperties"]["unit 'literal.service"][0]["timeout"]=0.into();});
        let out = f.run(&["--yes", "remote"], mode, b"");
        assert_eq!(out.status.code(), Some(1), "{out:?}");
        let text = String::from_utf8_lossy(&out.stdout);
        assert!(text.contains("failed"));
        match mode {
            "signal" | "exit143" => {
                let native = if mode == "signal" {
                    "signal 15"
                } else {
                    "exit status 143"
                };
                let expected = format!(
                    "remote: machine argv: failed attempt 1 ({native}): expected exit status 0, received {native}"
                );
                assert!(text.contains(&expected), "{mode}: {text}");
            }
            "system-code" | "system-signal" => {
                let native = if mode == "system-code" {
                    "exit status 2"
                } else {
                    "signal 15"
                };
                for check in [
                    "systemd: SystemState=running",
                    "unit 'literal.service: ActiveState=active",
                ] {
                    let expected = format!(
                        "remote: {check}: failed attempt 1 ({native}): expected exit status 0, received {native}"
                    );
                    assert!(text.contains(&expected), "{mode}: {text}");
                }
            }
            _ => {}
        }
        if matches!(mode, "signal" | "system-signal") {
            assert!(!text.contains("exit status 143"), "{mode}: {text}");
        } else if mode == "exit143" {
            assert!(!text.contains("signal 15"), "{text}");
        }
        assert_eq!(f.attempts(), 4);
        f.assert_remote_leaders_joined();
        f.unchanged();
    }
}
#[test]
fn cancellation_interrupts_cooldown_or_joins_running_child_without_another_attempt() {
    for mode in ["cooldown", "slow"] {
        let f = Fixture::new();
        f.edit(|v| {
            v["local"]["metaConfig"]["healthChecks"]["machineCommands"][0]["timeout"] = 30.into();
            v["local"]["metaConfig"]["healthChecks"]["machineCommands"][0]["cooldown"] = 30.into();
        });
        let mut child = f.command(&["--yes", "local"], mode).spawn().unwrap();
        drop(child.stdin.take());
        let until = Instant::now() + Duration::from_secs(5);
        let mut native_pid = None;
        loop {
            if mode == "slow" {
                if let Ok(text) = fs::read_to_string(f.root.join("pid"))
                    && let Ok(pid) = text.trim().parse::<u32>()
                    && pid > 0
                {
                    native_pid = Some(pid);
                    break;
                }
            } else if fs::read_to_string(f.root.join("events"))
                .unwrap_or_default()
                .contains("end ")
            {
                break;
            }
            assert!(Instant::now() < until);
            thread::sleep(Duration::from_millis(5));
        }
        rustix::process::kill_process(
            rustix::process::Pid::from_raw(child.id() as i32).unwrap(),
            rustix::process::Signal::INT,
        )
        .unwrap();
        let out = wait(child);
        assert_eq!(out.status.code(), Some(130), "{out:?}");
        assert_eq!(f.attempts(), 1);
        if let Some(pid) = native_pid {
            assert!(!Path::new("/proc").join(pid.to_string()).exists());
        }
        f.unchanged();
    }
}

#[test]
fn eventual_success_retains_both_attempts_raw_log_for_exit_or_output_failures() {
    for mode in ["retry", "output-retry"] {
        let f = Fixture::new();
        f.edit(|v|v["local"]["metaConfig"]["healthChecks"]["machineCommands"][0]=json!({
            "command":["machine-check"],"description":"retained attempt","timeout":2,"cooldown":0,
            "standardOutput":{"include":["healthy"],"exclude":["unhealthy"]}
        }));
        let out = f.run(&["--yes", "local"], mode, b"");
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        assert_eq!(f.attempts(), 2);
        let text = String::from_utf8_lossy(&out.stdout);
        assert!(
            text.contains("failed attempt 1")
                && text.contains("succeeded on attempt 2")
                && text.contains("1 checks passed, 0 failed"),
            "{text}"
        );
        let logs = fs::read_dir(f.root.join(".confctl/logs"))
            .unwrap()
            .map(|e| e.unwrap().path())
            .collect::<Vec<_>>();
        assert_eq!(logs.len(), 1);
        let bytes = fs::read(&logs[0]).unwrap();
        let (first_out, first_err) = if mode == "retry" {
            (
                b"failed stdout\xff\n".as_slice(),
                b"failed stderr\x1b\n".as_slice(),
            )
        } else {
            (
                b"unhealthy stdout\xff\n".as_slice(),
                b"unhealthy stderr\x1b\n".as_slice(),
            )
        };
        for marker in [
            first_out,
            first_err,
            b"healthy\xff\n",
            b"later stderr\xfe\n",
        ] {
            assert!(
                bytes.windows(marker.len()).any(|b| b == marker),
                "{marker:?} not retained"
            );
        }
        f.unchanged();
    }
}
