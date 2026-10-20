//! Saved boot routes use confined fake queries, copies and activation executables.
#![cfg(target_os = "linux")]
#[path = "support/native_tool.rs"]
mod native_tool;
use confctl::generation::BuildGeneration;
use serde_json::json;
use std::{
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
impl Fixture {
    fn new() -> Self {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("conf");
        let bin = tmp.path().join("bin");
        fs::create_dir(&root).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::create_dir(root.join("native")).unwrap();
        std::os::unix::fs::symlink(native_tool::resolve("cat"), root.join("native/cat")).unwrap();
        std::os::unix::fs::symlink(native_tool::resolve("sleep"), root.join("native/sleep"))
            .unwrap();

        fs::write(root.join("flake.nix"), "{}\n").unwrap();
        fs::write(root.join("flake.lock"), "unchanged lock\n").unwrap();
        fs::write(root.join("machines.json"), serde_json::to_vec(&json!({
            "remote":{"name":"remote","metaConfig":{"managed":true,"spin":"nixos","host":{"target":"remote.invalid","port":"02_222tail"}}},
            "guest":{"name":"carrier#guest","clusterName":"guest","carrier":"carrier","metaConfig":{"managed":true,"host":{"target":null}}},
            "local":{"name":"local","metaConfig":{"managed":true,"host":{"target":"localhost"}}},
            "carrier":{"name":"carrier","metaConfig":{"managed":false,"host":{"target":"carrier.invalid","port":2201}}}
        })).unwrap()).unwrap();
        let mut saved = Vec::new();
        for (host, native) in [
            ("remote", "remote"),
            ("carrier#guest", "guest"),
            ("local", "local"),
        ] {
            let top = root.join("toplevels").join(format!("{native} 'saved"));
            fs::create_dir_all(top.join("bin")).unwrap();
            executable(
                &top.join("bin/switch-to-configuration"),
                r#"#!/bin/sh
printf 'effect %s %s\n' "$1" "$BOOT_HOST" >> "$BOOT_ROOT/sequence"
printf 'native %s on %s\n' "$1" "$BOOT_HOST"
if { [ "$BOOT_MODE" = slow-boot ] && [ "$1" = boot ]; } || { [ "$BOOT_MODE" = slow-dry ] && [ "$1" = dry-activate ]; }; then
 printf '%s' "$$" > "$BOOT_ROOT/pid"
 exec "$BOOT_ROOT/native/sleep" 60
fi
if [ "$BOOT_MODE" = boot-fail ] && [ "$1" = boot ]; then exit 7; fi
if [ "$BOOT_MODE" = dry-fail ] && [ "$1" = dry-activate ]; then exit 8; fi
if [ "$1" = boot ]; then printf 'prepared\n' > "$BOOT_ROOT/booted-$BOOT_HOST"; fi
"#,
            );
            let dir = root.join(".confctl/generations").join(host).join("saved");
            fs::create_dir_all(&dir).unwrap();
            let generation = BuildGeneration {
                name: "saved".into(),
                date: chrono::DateTime::parse_from_rfc3339("2020-01-01T00:00:00Z").unwrap(),
                toplevel: top,
                auto_rollback: Some(root.join("never-run-rollback")),
                inputs: Default::default(),
                inputs_info: Default::default(),
            };
            let path = dir.join("generation.json");
            let bytes = serde_json::to_vec(&generation.encode()).unwrap();
            fs::write(&path, &bytes).unwrap();
            saved.push((path, bytes));
            std::os::unix::fs::symlink("saved", dir.parent().unwrap().join("current")).unwrap();
        }
        executable(
            &bin.join("nix"),
            r#"#!/bin/sh
printf 'nix %s\n' "$*" >> "$BOOT_ROOT/sequence"
last=
for arg do last="$arg"; done
case "$last" in
 '.#confctl.settings') printf '%s\n' '{}' ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$BOOT_ROOT" ;;
 *) printf 'unexpected nix\n' >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &bin.join("ssh"),
            r#"#!/bin/sh
printf 'ssh' >> "$BOOT_ROOT/ssh-args"
printf '\0%s' "$@" >> "$BOOT_ROOT/ssh-args"
printf '\n' >> "$BOOT_ROOT/ssh-args"
while [ "$#" -gt 0 ]; do
 case "$1" in -F|-l|-p) shift; shift ;; *) break ;; esac
done
case "$1" in remote.invalid) BOOT_HOST=remote ;; carrier.invalid) BOOT_HOST=carrier ;; *) exit 96 ;; esac
export BOOT_HOST
shift
[ "$#" = 1 ] || exit 95
exec /bin/sh -c "$1"
"#,
        );
        executable(
            &bin.join("cat"),
            r#"#!/bin/sh
[ "$1" = /proc/uptime ] || exit 94
printf 'query uptime %s\n' "$BOOT_HOST" >> "$BOOT_ROOT/sequence"
if [ "$BOOT_MODE" = uptime-fail ]; then printf 'offline\n' >&2; exit 255; fi
printf '123.5 0\n'
"#,
        );
        executable(
            &bin.join("realpath"),
            r#"#!/bin/sh
[ "$1" = -e ] && [ "$2" = -- ] || exit 93
host=$BOOT_HOST
case "$3" in /nix/var/nix/profiles/confctl-guest) host=guest ;; esac
printf 'query current %s\n' "$host" >> "$BOOT_ROOT/sequence"
case "$BOOT_MODE" in unknown) printf 'missing current\n' >&2; exit 1 ;; decode-fail) printf '\n'; exit 0 ;; esac
case "$BOOT_MODE" in known|running-only|status-profile-fail|empty-profile)
 printf "%s/toplevels/%s 'saved\n" "$BOOT_ROOT" "$host" ;;
 *) printf '/other-current\n' ;;
esac
"#,
        );
        executable(
            &bin.join("bash"),
            r#"#!/bin/sh
script=$("$BOOT_ROOT/native/cat")
host=$BOOT_HOST
profile=/nix/var/nix/profiles/system
case "$script" in *confctl-guest*) host=guest; profile=/nix/var/nix/profiles/confctl-guest ;; esac
printf 'query profile %s\n' "$host" >> "$BOOT_ROOT/sequence"
if [ "$BOOT_MODE" = status-profile-fail ]; then printf 'cannot read profile\n' >&2; exit 7; fi
case "$BOOT_MODE" in known|profile-only|empty-profile)
 printf "%s/toplevels/%s 'saved\n" "$BOOT_ROOT" "$host" ;;
 *) printf '/other-profile\n' ;;
esac
if [ "$BOOT_MODE" != empty-profile ]; then
 printf "%s-1-link;%s/toplevels/%s 'saved;1700000000;\n" "$profile" "$BOOT_ROOT" "$host"
fi
"#,
        );
        executable(
            &bin.join("nix-copy-closure"),
            r#"#!/bin/sh
printf 'effect copy %s\n' "$2" >> "$BOOT_ROOT/sequence"
printf 'copy\0%s\0' "$NIX_SSHOPTS" >> "$BOOT_ROOT/copy-args"
printf '%s\0' "$@" >> "$BOOT_ROOT/copy-args"
printf '\n' >> "$BOOT_ROOT/copy-args"
printf "copying path '/store/test'...\n" >&2
if [ "$BOOT_MODE" = copy-fail ] && [ "$2" = root@carrier.invalid ]; then exit 7; fi
"#,
        );
        executable(
            &bin.join("nix-env"),
            r#"#!/bin/sh
printf 'effect profile %s\n' "$BOOT_HOST" >> "$BOOT_ROOT/sequence"
printf 'profile' >> "$BOOT_ROOT/profile-args"
printf '\0%s' "$@" >> "$BOOT_ROOT/profile-args"
printf '\n' >> "$BOOT_ROOT/profile-args"
if [ "$BOOT_MODE" = set-profile-fail ]; then printf 'profile write failed\n' >&2; exit 9; fi
printf 'saved\n' > "$BOOT_ROOT/profile-$BOOT_HOST"
"#,
        );
        Self {
            _tmp: tmp,
            root,
            bin,
            saved,
        }
    }
    fn reboot_setup(&self) {
        let path = self.root.join("machines.json");
        let mut machines: serde_json::Value =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        for key in ["remote", "local"] {
            machines[key]["metaConfig"]["healthChecks"] = json!({"builderCommands":[{"command":["health-tool","{name}"],"timeout":0,"cooldown":0}]});
        }
        fs::write(path, serde_json::to_vec(&machines).unwrap()).unwrap();
        executable(
            &self.bin.join("health-tool"),
            r#"#!/bin/sh
printf 'effect health %s\n' "$1" >> "$BOOT_ROOT/sequence"
"#,
        );
        executable(
            &self.bin.join("ssh"),
            r#"#!/bin/sh
printf '%s\0' "$@" > "$BOOT_ROOT/reboot-ssh-$$"
while [ "$#" -gt 0 ]; do
 case "$1" in -F|-l|-p|-o) shift; shift ;; *) break ;; esac
done
case "$1" in remote.invalid) BOOT_HOST=remote ;; carrier.invalid) BOOT_HOST=carrier ;; *) exit 96 ;; esac
export BOOT_HOST
shift
[ "$#" = 1 ] || exit 95
exec /bin/sh -c "$1"
"#,
        );
        executable(
            &self.bin.join("reboot"),
            r#"#!/bin/sh
printf 'effect reboot %s\n' "$BOOT_HOST" >> "$BOOT_ROOT/sequence"
printf rebooted > "$BOOT_ROOT/rebooted-$BOOT_HOST"
printf 'native reboot output on %s\n' "$BOOT_HOST"
if [ "$BOOT_MODE" = slow-reboot ]; then printf '%s' "$$" > "$BOOT_ROOT/reboot-pid"; exec "$BOOT_ROOT/native/sleep" 60; fi
if [ "$BOOT_MODE" = reboot-fail ]; then printf 'reboot failed marker\n' >&2; exit 7; fi
if [ "$BOOT_MODE" = reboot-signal ]; then kill -TERM "$$"; fi
"#,
        );
        executable(
            &self.bin.join("cat"),
            r#"#!/bin/sh
[ "$1" = /proc/uptime ] || exit 94
if [ ! -e "$BOOT_ROOT/profile-$BOOT_HOST" ]; then
 printf 'query uptime %s\n' "$BOOT_HOST" >> "$BOOT_ROOT/sequence"
 printf '123.5 0\n'; exit 0
fi
if [ ! -e "$BOOT_ROOT/rebooted-$BOOT_HOST" ]; then
 printf 'baseline %s\n' "$BOOT_HOST" >> "$BOOT_ROOT/sequence"
 case "$BOOT_MODE" in initial-fail) exit 255 ;; invalid-initial) printf 'bad\n'; exit 0 ;; nonfinite-initial) printf 'NaN\n'; exit 0 ;; negative-initial) printf -- '-1\n'; exit 0 ;; esac
 printf '123.5 0\n'; exit 0
fi
n=0; if [ -e "$BOOT_ROOT/polls-$BOOT_HOST" ]; then read -r n < "$BOOT_ROOT/polls-$BOOT_HOST"; fi
n=$((n+1)); printf '%s\n' "$n" > "$BOOT_ROOT/polls-$BOOT_HOST"
printf 'poll %s %s\n' "$BOOT_HOST" "$n" >> "$BOOT_ROOT/sequence"
case "$BOOT_MODE" in
 unchanged) printf '123.5 0\n'; exit 0 ;;
 unavailable) if [ "$n" -lt 3 ]; then printf 'offline marker\n' >&2; exit 255; fi ;;
 invalid-poll) printf 'garbage\n'; exit 0 ;;
 nonfinite-poll) printf 'inf\n'; exit 0 ;;
 negative-poll) printf -- '-0.5\n'; exit 0 ;;
 slow-poll) printf '%s' "$$" > "$BOOT_ROOT/reboot-pid"; exec "$BOOT_ROOT/native/sleep" 60 ;;
 blocked-poll) "$BOOT_ROOT/native/sleep" 0.8 ;;
esac
printf '1.25 0\n'
"#,
        );
    }
    fn reboot_records(&self, out: &Output) -> Vec<Vec<Vec<u8>>> {
        let raw = fs::read_dir(&self.root)
            .unwrap()
            .map(|p| p.unwrap().path())
            .filter(|p| {
                p.file_name()
                    .unwrap()
                    .as_encoded_bytes()
                    .starts_with(b"reboot-ssh-")
            })
            .map(|p| {
                let bytes = fs::read(&p).unwrap();
                (p, bytes)
            })
            .collect::<Vec<_>>();
        raw.iter()
            .map(|(_, b)| {
                b.strip_suffix(&[0])
                    .unwrap_or_else(|| panic!("{raw:?}; {out:?}"))
                    .split(|b| *b == 0)
                    .map(Vec::from)
                    .collect()
            })
            .collect()
    }
    fn command(&self, args: &[&str], mode: &str) -> Command {
        let mut command = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        command
            .current_dir(&self.root)
            .args(["--color=never", "deploy"])
            .args(args)
            .env("PATH", &self.bin)
            .env("CONFCTL_TTY", "0")
            .env("BOOT_ROOT", &self.root)
            .env("BOOT_MODE", mode)
            .env("BOOT_HOST", "local")
            .env("NIX_SSHOPTS", "-o Existing=yes")
            .env("CONFCTL_SSH_CONFIG", "native ssh config")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        command
    }
    fn run(&self, args: &[&str], mode: &str, input: &[u8]) -> Output {
        let mut child = self.command(args, mode).spawn().unwrap();
        child.stdin.take().unwrap().write_all(input).unwrap();
        wait(child)
    }
    fn effects(&self) -> Vec<String> {
        fs::read_to_string(self.root.join("sequence"))
            .unwrap_or_default()
            .lines()
            .filter(|line| line.starts_with("effect "))
            .map(str::to_owned)
            .collect()
    }
    fn unchanged(&self) {
        for (path, bytes) in &self.saved {
            assert_eq!(&fs::read(path).unwrap(), bytes);
        }
        for host in ["remote", "carrier#guest", "local"] {
            assert_eq!(
                fs::read_link(
                    self.root
                        .join(".confctl/generations")
                        .join(host)
                        .join("current")
                )
                .unwrap(),
                Path::new("saved")
            );
        }
        assert_eq!(
            fs::read(self.root.join("flake.lock")).unwrap(),
            b"unchanged lock\n"
        );
        assert!(!self.root.join("roots").exists());
    }
}
fn wait(mut child: Child) -> Output {
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        if child.try_wait().unwrap().is_some() {
            return child.wait_with_output().unwrap();
        }
        if Instant::now() > deadline {
            let _ = child.kill();
            panic!(
                "boot fixture timeout: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        thread::sleep(Duration::from_millis(10));
    }
}

#[test]
fn actual_bulk_boot_routes_native_arguments_and_retains_read_only_saved_state() {
    let f = Fixture::new();
    let out = f.run(
        &[
            "--generation=saved",
            "--yes",
            "--max-concurrent-copy=1",
            "--enable-auto-rollback",
            "--health-checks",
            "*",
            "boot",
        ],
        "",
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(
        f.effects(),
        [
            "effect copy root@remote.invalid",
            "effect copy root@carrier.invalid",
            "effect boot remote",
            "effect profile remote",
            "effect boot local",
            "effect profile local"
        ]
    );
    let copies = fs::read(f.root.join("copy-args")).unwrap();
    assert!(
        copies
            .windows(b"-o Existing=yes -p 2222".len())
            .any(|b| b == b"-o Existing=yes -p 2222")
    );
    assert!(
        copies
            .windows(b"-o Existing=yes -p 2201".len())
            .any(|b| b == b"-o Existing=yes -p 2201")
    );
    assert!(
        !copies
            .windows(b"native ssh config".len())
            .any(|b| b == b"native ssh config")
    );
    let ssh = fs::read(f.root.join("ssh-args")).unwrap();
    assert!(
        ssh.windows(b"-F\0native ssh config\0-l\0root\0-p\x002222\0remote.invalid".len())
            .any(|b| b == b"-F\0native ssh config\0-l\0root\0-p\x002222\0remote.invalid")
    );
    let profile = fs::read(f.root.join("profile-args")).unwrap();
    assert!(
        profile
            .windows(b"-p\0/nix/var/nix/profiles/system\0--set\0".len())
            .any(|b| b == b"-p\0/nix/var/nix/profiles/system\0--set\0")
    );
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(
        text.contains("carried boot copies store paths only")
            && text.contains("native boot on remote"),
        "{text}"
    );
    assert!(!f.root.join("profile-guest").exists());
    f.unchanged();
}

#[test]
fn standalone_skip_requires_known_running_and_profile_current_while_unknown_proceeds() {
    for mode in [
        "known",
        "running-only",
        "profile-only",
        "unknown",
        "uptime-fail",
        "empty-profile",
        "status-profile-fail",
    ] {
        let f = Fixture::new();
        let out = f.run(
            &["--generation=saved", "--yes", "remote", "boot"],
            mode,
            b"",
        );
        assert_eq!(out.status.code(), Some(0), "{mode}: {out:?}");
        assert_eq!(
            f.effects().len(),
            if mode == "known" { 0 } else { 3 },
            "{mode}"
        );
        if mode == "known" {
            assert!(String::from_utf8_lossy(&out.stdout).contains("Skipping remote"));
        }
        if ["unknown", "uptime-fail", "status-profile-fail"].contains(&mode) {
            assert!(String::from_utf8_lossy(&out.stderr).contains("Unable to read"));
        }
        f.unchanged();
    }
    let f = Fixture::new();
    let out = f.run(
        &["--generation=saved", "--yes", "remote", "boot"],
        "decode-fail",
        b"",
    );
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert!(f.effects().is_empty());
    f.unchanged();
}

#[test]
fn carried_known_current_survives_later_profile_failure_but_unknown_only_copies() {
    for mode in ["known", "status-profile-fail", "unknown"] {
        let f = Fixture::new();
        let out = f.run(
            &[
                "--generation=saved",
                "--yes",
                "--dry-activate-first",
                "--interactive",
                "carrier#guest",
                "boot",
            ],
            mode,
            b"y\ny\n",
        );
        assert_eq!(out.status.code(), Some(0), "{mode}: {out:?}");
        assert_eq!(
            f.effects(),
            if mode == "unknown" {
                vec!["effect copy root@carrier.invalid".to_owned()]
            } else {
                vec![]
            }
        );
        assert!(!f.root.join("booted-guest").exists());
        f.unchanged();
    }
}

#[test]
fn bulk_copy_barrier_and_one_by_one_retain_different_honest_partial_effects() {
    for one in [false, true] {
        let f = Fixture::new();
        let mut args = vec![
            "--generation=saved",
            "--yes",
            "--max-concurrent-copy=1",
            "*",
            "boot",
        ];
        if one {
            args.insert(0, "--one-by-one");
        }
        let out = f.run(&args, "copy-fail", b"");
        assert_eq!(out.status.code(), Some(1), "{out:?}");
        assert_eq!(
            f.effects(),
            if one {
                vec![
                    "effect copy root@remote.invalid",
                    "effect boot remote",
                    "effect profile remote",
                    "effect copy root@carrier.invalid",
                ]
            } else {
                vec![
                    "effect copy root@remote.invalid",
                    "effect copy root@carrier.invalid",
                ]
            }
        );
        assert_eq!(f.root.join("profile-remote").exists(), one);
        assert!(!f.root.join("booted-local").exists());
        f.unchanged();
    }
}

#[test]
fn interactive_bulk_is_not_one_by_one_and_yes_does_not_bypass_activation_choices() {
    let f = Fixture::new();
    let out = f.run(
        &["--generation=saved", "--yes", "--interactive", "*", "boot"],
        "",
        b"y\ny\ny\ny\ny\ny\n",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(
        f.effects(),
        [
            "effect copy root@remote.invalid",
            "effect copy root@carrier.invalid",
            "effect boot remote",
            "effect profile remote",
            "effect boot local",
            "effect profile local"
        ]
    );
    for (dry, input, expected, status) in [
        (
            true,
            b"y\ny\nr\ns\n".as_slice(),
            vec![
                "effect copy root@remote.invalid",
                "effect dry-activate remote",
                "effect dry-activate remote",
            ],
            0,
        ),
        (
            false,
            b"y\ny\n\nr\ny\n".as_slice(),
            vec![
                "effect copy root@remote.invalid",
                "effect boot remote",
                "effect profile remote",
            ],
            0,
        ),
        (
            false,
            b"y\ny\na\n".as_slice(),
            vec!["effect copy root@remote.invalid"],
            1,
        ),
        (false, b"y\nn\n".as_slice(), vec![], 0),
    ] {
        let f = Fixture::new();
        let mut args = vec![
            "--generation=saved",
            "--yes",
            "--interactive",
            "remote",
            "boot",
        ];
        if dry {
            args.insert(0, "--dry-activate-first");
        }
        let out = f.run(&args, "", input);
        assert_eq!(out.status.code(), Some(status), "{out:?}");
        assert_eq!(f.effects(), expected);
        f.unchanged();
    }
}

#[test]
fn dry_boot_and_profile_failures_stop_later_commands_without_undoing_earlier_effects() {
    for (mode, effects, booted) in [
        (
            "dry-fail",
            vec![
                "effect copy root@remote.invalid",
                "effect dry-activate remote",
            ],
            false,
        ),
        (
            "boot-fail",
            vec![
                "effect copy root@remote.invalid",
                "effect dry-activate remote",
                "effect boot remote",
            ],
            false,
        ),
        (
            "set-profile-fail",
            vec![
                "effect copy root@remote.invalid",
                "effect dry-activate remote",
                "effect boot remote",
                "effect profile remote",
            ],
            true,
        ),
    ] {
        let f = Fixture::new();
        let out = f.run(
            &[
                "--generation=saved",
                "--yes",
                "--one-by-one",
                "--dry-activate-first",
                "*",
                "boot",
            ],
            mode,
            b"",
        );
        assert_eq!(out.status.code(), Some(1), "{out:?}");
        assert_eq!(f.effects(), effects);
        assert_eq!(f.root.join("booted-remote").exists(), booted);
        assert!(!f.root.join("profile-remote").exists());
        assert!(!f.root.join("booted-local").exists());
        assert!(String::from_utf8_lossy(&out.stderr).contains("retained"));
        f.unchanged();
    }
}

#[test]
fn unsupported_boot_variants_and_declined_confirmations_do_not_query_or_copy() {
    for (args, code) in [
        (vec!["--yes", "--reboot", "remote"], 64),
        (
            vec![
                "--generation=saved",
                "--yes",
                "--reboot",
                "remote",
                "dry-activate",
            ],
            64,
        ),
    ] {
        let f = Fixture::new();
        let out = f.run(&args, "", b"");
        assert_eq!(out.status.code(), Some(code), "{args:?}: {out:?}");
        if code == 64 {
            assert!(
                String::from_utf8_lossy(&out.stderr)
                    .contains("--reboot can be used only with switch-action boot"),
                "{out:?}"
            );
        }
        assert!(!f.root.join("sequence").exists());
        f.unchanged();
    }
    for args in [
        vec!["--generation=saved", "remote", "boot"],
        vec![
            "--generation=saved",
            "--yes",
            "--interactive",
            "remote",
            "boot",
        ],
    ] {
        let f = Fixture::new();
        let out = f.run(&args, "", b"n\n");
        assert_eq!(out.status.code(), Some(1), "{out:?}");
        let sequence = fs::read_to_string(f.root.join("sequence")).unwrap_or_default();
        assert!(!sequence.contains("query "));
        assert!(f.effects().is_empty());
        f.unchanged();
    }
}

#[test]
fn cancellation_joins_active_boot_and_prevents_profile_or_later_host_entry() {
    let f = Fixture::new();
    let mut child = f
        .command(
            &["--generation=saved", "--yes", "--one-by-one", "*", "boot"],
            "slow-boot",
        )
        .spawn()
        .unwrap();
    drop(child.stdin.take());
    let deadline = Instant::now() + Duration::from_secs(5);
    let pid = loop {
        if let Ok(text) = fs::read_to_string(f.root.join("pid"))
            && let Ok(pid) = text.trim().parse::<u32>()
            && pid > 0
        {
            break pid;
        }
        assert!(Instant::now() < deadline);
        thread::sleep(Duration::from_millis(5));
    };
    rustix::process::kill_process(
        rustix::process::Pid::from_raw(child.id() as i32).unwrap(),
        rustix::process::Signal::INT,
    )
    .unwrap();
    let out = wait(child);
    assert_eq!(out.status.code(), Some(130), "{out:?}");
    assert_eq!(
        f.effects(),
        ["effect copy root@remote.invalid", "effect boot remote"]
    );
    assert!(!Path::new("/proc").join(pid.to_string()).exists());
    f.unchanged();
}

#[test]
fn saved_dry_argv_has_no_profile_and_skip_uses_known_running_only() {
    for mode in [
        "known",
        "running-only",
        "status-profile-fail",
        "empty-profile",
        "profile-only",
        "unknown",
        "uptime-fail",
    ] {
        let f = Fixture::new();
        let out = f.run(
            &["--generation=saved", "--yes", "remote", "dry-activate"],
            mode,
            b"",
        );
        assert_eq!(out.status.code(), Some(0), "{mode}: {out:?}");
        let skip = [
            "known",
            "running-only",
            "status-profile-fail",
            "empty-profile",
        ]
        .contains(&mode);
        assert_eq!(
            f.effects(),
            if skip {
                vec![]
            } else {
                vec![
                    "effect copy root@remote.invalid",
                    "effect dry-activate remote",
                ]
            },
            "{mode}"
        );
        if mode == "status-profile-fail" {
            assert!(
                String::from_utf8_lossy(&out.stderr).contains("Unable to read profile generations")
            );
        }
        assert!(!f.root.join("profile-args").exists());
        assert!(!f.root.join("booted-remote").exists());
        f.unchanged();
    }
    let f = Fixture::new();
    let out = f.run(
        &[
            "--generation=saved",
            "--yes",
            "--health-checks",
            "--enable-auto-rollback",
            "*",
            "dry-activate",
        ],
        "",
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let effects = f.effects();
    assert_eq!(effects.len(), 4, "{effects:?}");
    assert!(
        effects[..2]
            .iter()
            .all(|line| line.starts_with("effect copy "))
    );
    assert_eq!(
        &effects[2..],
        ["effect dry-activate remote", "effect dry-activate local"]
    );
    assert!(!f.root.join("profile-args").exists());
    assert!(
        String::from_utf8_lossy(&out.stdout)
            .contains("carried dry-activate copies store paths only")
    );
    f.unchanged();
    let f = Fixture::new();
    let out = f.run(
        &[
            "--generation=saved",
            "--yes",
            "--reboot",
            "remote",
            "dry-activate",
        ],
        "",
        b"",
    );
    assert!(!out.status.success(), "{out:?}");
    assert!(!f.root.join("sequence").exists());
    f.unchanged();
}

#[test]
fn saved_dry_first_continue_retry_skip_and_abort_keep_exact_native_attempts() {
    for (input, count, status) in [
        (b"y\ny\ny\n".as_slice(), 2, 0),
        (b"y\ny\nr\ny\n".as_slice(), 3, 0),
        (b"y\ny\ns\n".as_slice(), 1, 0),
        (b"y\ny\na\n".as_slice(), 1, 1),
    ] {
        let f = Fixture::new();
        let out = f.run(
            &[
                "--generation=saved",
                "--yes",
                "--interactive",
                "--dry-activate-first",
                "remote",
                "dry-activate",
            ],
            "",
            input,
        );
        assert_eq!(out.status.code(), Some(status), "{out:?}");
        let effects = f.effects();
        assert_eq!(effects[0], "effect copy root@remote.invalid");
        assert_eq!(effects.len(), 1 + count, "{effects:?}");
        assert!(
            effects[1..]
                .iter()
                .all(|s| s == "effect dry-activate remote")
        );
        assert!(!f.root.join("profile-args").exists());
        f.unchanged();
    }
    let f = Fixture::new();
    let out = f.run(
        &[
            "--generation=saved",
            "--yes",
            "--interactive",
            "remote",
            "dry-activate",
        ],
        "",
        b"y\ny\n\nr\ny\n",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(
        f.effects(),
        [
            "effect copy root@remote.invalid",
            "effect dry-activate remote"
        ]
    );
    let f = Fixture::new();
    let out = f.run(
        &[
            "--generation=saved",
            "--yes",
            "--interactive",
            "remote",
            "dry-activate",
        ],
        "",
        b"y\nn\n",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(f.effects().is_empty());
    f.unchanged();
    // Carried targets have no activation prompt or preliminary dry command.
    let f = Fixture::new();
    let out = f.run(
        &[
            "--generation=saved",
            "--yes",
            "--interactive",
            "--dry-activate-first",
            "carrier#guest",
            "dry-activate",
        ],
        "unknown",
        b"y\ny\n",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(f.effects(), ["effect copy root@carrier.invalid"]);
    f.unchanged();
}

#[test]
fn saved_dry_bulk_barrier_one_by_one_and_native_failure_keep_partial_effects() {
    for (mode, one, expected) in [
        (
            "copy-fail",
            false,
            vec![
                "effect copy root@remote.invalid",
                "effect copy root@carrier.invalid",
            ],
        ),
        (
            "copy-fail",
            true,
            vec![
                "effect copy root@remote.invalid",
                "effect dry-activate remote",
                "effect copy root@carrier.invalid",
            ],
        ),
        (
            "dry-fail",
            false,
            vec![
                "effect copy root@remote.invalid",
                "effect copy root@carrier.invalid",
                "effect dry-activate remote",
            ],
        ),
        (
            "dry-fail",
            true,
            vec![
                "effect copy root@remote.invalid",
                "effect dry-activate remote",
            ],
        ),
    ] {
        let f = Fixture::new();
        let mut args = vec![
            "--generation=saved",
            "--yes",
            "--max-concurrent-copy=1",
            "*",
            "dry-activate",
        ];
        if one {
            args.insert(0, "--one-by-one");
        }
        let out = f.run(&args, mode, b"");
        assert_eq!(out.status.code(), Some(1), "{out:?}");
        assert_eq!(f.effects(), expected);
        assert!(!f.root.join("profile-args").exists());
        assert!(String::from_utf8_lossy(&out.stderr).contains("retained"));
        f.unchanged();
    }
}

#[test]
fn saved_dry_cancellation_joins_entered_script_before_later_host_entry() {
    let f = Fixture::new();
    let mut child = f
        .command(
            &[
                "--generation=saved",
                "--yes",
                "--one-by-one",
                "*",
                "dry-activate",
            ],
            "slow-dry",
        )
        .spawn()
        .unwrap();
    drop(child.stdin.take());
    let deadline = Instant::now() + Duration::from_secs(5);
    let pid = loop {
        if let Ok(text) = fs::read_to_string(f.root.join("pid"))
            && let Ok(pid) = text.trim().parse::<u32>()
            && pid > 0
        {
            break pid;
        }
        assert!(Instant::now() < deadline);
        thread::sleep(Duration::from_millis(5));
    };
    rustix::process::kill_process(
        rustix::process::Pid::from_raw(child.id() as i32).unwrap(),
        rustix::process::Signal::INT,
    )
    .unwrap();
    let out = wait(child);
    assert_eq!(out.status.code(), Some(130), "{out:?}");
    assert_eq!(
        f.effects(),
        [
            "effect copy root@remote.invalid",
            "effect dry-activate remote"
        ]
    );
    assert!(!f.root.join("profile-args").exists());
    assert!(!Path::new("/proc").join(pid.to_string()).exists());
    f.unchanged();
}

#[test]
fn saved_reboot_waits_once_with_probe_only_ssh_options_and_healthy_order() {
    for (mode, budget) in [
        ("", "0"),
        ("", "default"),
        ("blocked-poll", "0"),
        ("unavailable", "3"),
        ("reboot-fail", "0"),
        ("", "wait"),
        ("", "9999999999999999999999999999999999999999999"),
    ] {
        let f = Fixture::new();
        f.reboot_setup();
        let mut args = vec![
            "--generation=saved",
            "--yes",
            "--reboot",
            "--enable-auto-rollback",
        ];
        if budget != "default" {
            args.extend(["--wait-online", budget]);
        }
        args.extend(["remote", "boot"]);
        let out = f.run(&args, mode, b"");
        assert_eq!(out.status.code(), Some(0), "{mode}/{budget}: {out:?}");
        assert_eq!(
            f.effects(),
            [
                "effect copy root@remote.invalid",
                "effect boot remote",
                "effect profile remote",
                "effect reboot remote",
                "effect health remote"
            ]
        );
        let sequence = fs::read_to_string(f.root.join("sequence")).unwrap();
        assert_eq!(sequence.matches("baseline remote").count(), 1);
        assert_eq!(sequence.matches("effect reboot remote").count(), 1);
        let text = String::from_utf8_lossy(&out.stdout);
        assert!(text.contains("lower uptime observed"), "{out:?}");
        assert!(!text.contains("(without reboot)"));
        let records = f.reboot_records(&out);
        for argv in &records {
            assert_eq!(
                &argv[..7],
                [
                    b"-F".to_vec(),
                    b"native ssh config".to_vec(),
                    b"-l".to_vec(),
                    b"root".to_vec(),
                    b"-p".to_vec(),
                    b"2222".to_vec(),
                    if argv.len() == 14 {
                        b"-o".to_vec()
                    } else {
                        b"remote.invalid".to_vec()
                    }
                ]
            );
        }
        let polls = records
            .iter()
            .filter(|a| a.iter().any(|v| v == b"ConnectTimeout=3"))
            .collect::<Vec<_>>();
        assert_eq!(
            polls.len(),
            if mode == "unavailable" { 3 } else { 1 },
            "{records:?}; {out:?}"
        );
        for argv in polls {
            assert_eq!(
                &argv[6..],
                [
                    b"-o".to_vec(),
                    b"ConnectTimeout=3".to_vec(),
                    b"-o".to_vec(),
                    b"ServerAliveInterval=3".to_vec(),
                    b"-o".to_vec(),
                    b"ServerAliveCountMax=1".to_vec(),
                    b"remote.invalid".to_vec(),
                    b"cat /proc/uptime".to_vec()
                ]
            );
        }
        if mode == "reboot-fail" {
            assert!(String::from_utf8_lossy(&out.stderr).contains("effect unknown"));
        }
        if mode == "unavailable" {
            assert!(
                text.contains("first unavailable") && text.contains("still unavailable"),
                "{out:?}"
            );
        }
        f.unchanged();
    }
}
#[test]
fn nowait_retains_failed_native_outcome_and_still_runs_health_without_uptime_waits() {
    for mode in ["", "reboot-fail", "reboot-signal"] {
        let f = Fixture::new();
        f.reboot_setup();
        let out = f.run(
            &[
                "--generation=saved",
                "--yes",
                "--reboot",
                "--wait-online=nowait",
                "remote",
                "boot",
            ],
            mode,
            b"",
        );
        assert_eq!(out.status.code(), Some(0), "{mode}: {out:?}");
        let s = fs::read_to_string(f.root.join("sequence")).unwrap();
        assert!(!s.contains("baseline") && !s.contains("poll "), "{s}");
        assert!(s.contains("effect health remote"));
        assert!(String::from_utf8_lossy(&out.stdout).contains("online state unverified"));
        assert!(
            String::from_utf8_lossy(&out.stdout).contains("native reboot output on remote"),
            "{out:?}"
        );
        if mode == "reboot-fail" {
            assert!(
                String::from_utf8_lossy(&out.stderr).contains("reboot failed marker"),
                "{out:?}"
            );
        }
        if !mode.is_empty() {
            assert!(
                String::from_utf8_lossy(&out.stderr).contains("reboot command failed"),
                "{out:?}"
            );
            let logs = fs::read_dir(f.root.join(".confctl/logs")).unwrap().count();
            assert!(logs > 0);
        }
        f.unchanged();
    }
}
#[test]
fn initial_invalid_poll_and_timeout_failures_keep_profile_effects_and_stop_health() {
    for mode in [
        "initial-fail",
        "invalid-initial",
        "nonfinite-initial",
        "negative-initial",
        "invalid-poll",
        "nonfinite-poll",
        "negative-poll",
        "unchanged",
    ] {
        let f = Fixture::new();
        f.reboot_setup();
        let out = f.run(
            &[
                "--generation=saved",
                "--yes",
                "--reboot",
                "--wait-online=0",
                "remote",
                "boot",
            ],
            mode,
            b"",
        );
        assert_eq!(out.status.code(), Some(1), "{mode}: {out:?}");
        assert!(f.root.join("profile-remote").exists());
        assert!(!f.effects().iter().any(|e| e.starts_with("effect health")));
        assert_eq!(
            f.root.join("rebooted-remote").exists(),
            !mode.ends_with("initial") && mode != "initial-fail"
        );
        let text = String::from_utf8_lossy(&out.stderr);
        assert!(text.contains("retained"), "{out:?}");
        if mode == "unchanged" {
            assert_eq!(
                fs::read_to_string(f.root.join("polls-remote")).unwrap(),
                "1\n"
            );
            assert!(text.contains("timed out"));
        }
        f.unchanged();
    }
}
#[test]
fn reboot_local_carried_known_and_profile_failure_preserve_phase_exclusions() {
    for (mode, pattern) in [
        ("", "*"),
        ("known", "remote"),
        ("set-profile-fail", "remote"),
    ] {
        let f = Fixture::new();
        f.reboot_setup();
        let out = f.run(
            &[
                "--generation=saved",
                "--yes",
                "--reboot",
                "--wait-online=0",
                "--max-concurrent-copy=1",
                pattern,
                "boot",
            ],
            mode,
            b"",
        );
        assert_eq!(
            out.status.code(),
            Some(if mode == "set-profile-fail" { 1 } else { 0 }),
            "{out:?}"
        );
        assert!(
            !f.root.join("rebooted-local").exists() && !f.root.join("rebooted-carrier").exists()
        );
        if !mode.is_empty() {
            assert!(!f.root.join("rebooted-remote").exists());
        } else {
            let effects = f.effects();
            assert!(
                effects
                    .iter()
                    .position(|v| v == "effect profile local")
                    .unwrap()
                    < effects
                        .iter()
                        .position(|v| v == "effect reboot remote")
                        .unwrap()
            );
            let mut health = effects
                .iter()
                .filter(|v| v.starts_with("effect health "))
                .map(String::as_str)
                .collect::<Vec<_>>();
            health.sort_unstable();
            assert_eq!(
                health,
                ["effect health local", "effect health remote"],
                "{out:?}"
            );
            assert!(
                effects
                    .iter()
                    .position(|v| v == "effect reboot remote")
                    .unwrap()
                    < effects
                        .iter()
                        .position(|v| v.starts_with("effect health "))
                        .unwrap(),
                "{out:?}"
            );
        }
        f.unchanged();
    }
}
#[test]
fn interactive_reboot_decline_is_not_bypassed_by_yes_and_one_by_one_skips_health() {
    for serial in [false, true] {
        for answer in [b"\n".as_slice(), b" n \n"] {
            let f = Fixture::new();
            f.reboot_setup();
            let mut args = vec![
                "--generation=saved",
                "--yes",
                "--interactive",
                "--reboot",
                "--wait-online=0",
            ];
            if serial {
                args.push("--one-by-one");
            }
            args.extend(["remote", "boot"]);
            let mut input = b"y\ny\ny\n".to_vec();
            input.extend_from_slice(answer);
            if !serial {
                input.extend_from_slice(b"y\n");
            }
            let out = f.run(&args, "", &input);
            assert_eq!(out.status.code(), Some(0), "{serial}/{answer:?}: {out:?}");
            assert!(!f.root.join("rebooted-remote").exists());
            assert_eq!(
                f.effects().iter().any(|s| s == "effect health remote"),
                !serial,
                "{out:?}"
            );
            assert!(String::from_utf8_lossy(&out.stdout).contains("Reboot remote? [y/N]"));
            f.unchanged();
        }
    }
}
#[test]
fn interactive_reboot_repeats_invalid_reply_then_accepts_explicit_case_folded_yes() {
    let f = Fixture::new();
    f.reboot_setup();
    let out = f.run(
        &[
            "--generation=saved",
            "--yes",
            "--interactive",
            "--reboot",
            "--wait-online=0",
            "--no-health-checks",
            "remote",
            "boot",
        ],
        "",
        b"y\ny\ny\n?\n Y \n",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(
        String::from_utf8_lossy(&out.stdout)
            .matches("Reboot remote? [y/N]:")
            .count(),
        2,
        "{out:?}"
    );
    assert_eq!(
        f.effects()
            .iter()
            .filter(|s| s.starts_with("effect reboot"))
            .count(),
        1
    );
    f.unchanged();
}

#[test]
fn reboot_cancellation_joins_native_reboot_poll_and_interrupts_wait_sleep() {
    for mode in ["slow-reboot", "slow-poll", "unchanged"] {
        let f = Fixture::new();
        f.reboot_setup();
        let mut child = f
            .command(
                &[
                    "--generation=saved",
                    "--yes",
                    "--reboot",
                    "--wait-online=wait",
                    "remote",
                    "boot",
                ],
                mode,
            )
            .spawn()
            .unwrap();
        drop(child.stdin.take());
        let deadline = Instant::now() + Duration::from_secs(5);
        let pid = loop {
            if mode == "unchanged" && f.root.join("polls-remote").exists() {
                break None;
            }
            if let Ok(t) = fs::read_to_string(f.root.join("reboot-pid"))
                && let Ok(n) = t.parse::<u32>()
                && n > 0
            {
                break Some(n);
            }
            assert!(Instant::now() < deadline, "{mode}");
            thread::sleep(Duration::from_millis(5));
        };
        rustix::process::kill_process(
            rustix::process::Pid::from_raw(child.id() as i32).unwrap(),
            rustix::process::Signal::INT,
        )
        .unwrap();
        let out = wait(child);
        assert_eq!(out.status.code(), Some(130), "{mode}: {out:?}");
        if let Some(pid) = pid {
            assert!(!Path::new("/proc").join(pid.to_string()).exists());
        }
        assert!(f.root.join("profile-remote").exists());
        assert!(!f.effects().iter().any(|s| s.starts_with("effect health")));
        f.unchanged();
    }
}

#[test]
fn copy_only_reboot_flags_and_serial_local_skip_do_not_admit_later_phases() {
    let f = Fixture::new();
    f.reboot_setup();
    let out = f.run(
        &[
            "--generation=saved",
            "--yes",
            "--copy-only",
            "--reboot",
            "--wait-online=0",
            "remote",
            "boot",
        ],
        "",
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(f.effects(), ["effect copy root@remote.invalid"]);
    assert!(f.reboot_records(&out).is_empty());
    f.unchanged();
    let f = Fixture::new();
    f.reboot_setup();
    let out = f.run(
        &[
            "--generation=saved",
            "--yes",
            "--one-by-one",
            "--reboot",
            "--wait-online=0",
            "*",
            "boot",
        ],
        "",
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(
        f.effects(),
        [
            "effect copy root@remote.invalid",
            "effect boot remote",
            "effect profile remote",
            "effect reboot remote",
            "effect health remote",
            "effect copy root@carrier.invalid",
            "effect boot local",
            "effect profile local"
        ]
    );
    f.unchanged();
}

#[test]
fn actual_reboot_pty_consent_live_wait_and_terminal_restoration() {
    use std::{io, os::fd::OwnedFd};
    const ROLE: &str = "CONFCTL_REBOOT_PTY_CHILD";
    const TEST: &str = "actual_reboot_pty_consent_live_wait_and_terminal_restoration";
    if std::env::var_os(ROLE).is_some() {
        rustix::process::setsid().unwrap();
        rustix::process::ioctl_tiocsctty(io::stdin()).unwrap();
        let modes = rustix::termios::tcgetattr(io::stdin()).unwrap();
        let group = rustix::process::getpgrp();
        let f = Fixture::new();
        f.reboot_setup();
        let status = f
            .command(
                &[
                    "--generation=saved",
                    "--yes",
                    "--interactive",
                    "--reboot",
                    "--wait-online=0",
                    "remote",
                    "boot",
                ],
                "blocked-poll",
            )
            .env("CONFCTL_TTY", "1")
            .env("TERM", "xterm")
            .stdin(Stdio::inherit())
            .stdout(Stdio::inherit())
            .stderr(Stdio::inherit())
            .spawn()
            .unwrap()
            .wait()
            .unwrap();
        assert_eq!(status.code(), Some(0));
        assert_eq!(rustix::termios::tcgetpgrp(io::stdin()).unwrap(), group);
        let restored = rustix::termios::tcgetattr(io::stdin()).unwrap();
        assert_eq!(modes.local_modes, restored.local_modes);
        assert_eq!(modes.input_modes, restored.input_modes);
        assert_eq!(modes.output_modes, restored.output_modes);
        f.unchanged();
        println!("REBOOT_PTY_OK");
        return;
    }
    use rustix::pty::{OpenptFlags, grantpt, ioctl_tiocgptpeer, openpt, unlockpt};
    let flags = OpenptFlags::RDWR | OpenptFlags::NOCTTY | OpenptFlags::CLOEXEC;
    let master = openpt(flags).unwrap();
    grantpt(&master).unwrap();
    unlockpt(&master).unwrap();
    let slave = ioctl_tiocgptpeer(&master, flags).unwrap();
    rustix::termios::tcsetwinsize(
        &slave,
        rustix::termios::Winsize {
            ws_row: 40,
            ws_col: 160,
            ws_xpixel: 0,
            ws_ypixel: 0,
        },
    )
    .unwrap();
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args(["--exact", TEST, "--nocapture"])
        .env(ROLE, "1")
        .stdin(Stdio::from(slave.try_clone().unwrap()))
        .stdout(Stdio::from(slave.try_clone().unwrap()))
        .stderr(Stdio::from(slave))
        .spawn()
        .unwrap();
    let flags = rustix::fs::fcntl_getfl(&master).unwrap();
    rustix::fs::fcntl_setfl(&master, flags | rustix::fs::OFlags::NONBLOCK).unwrap();
    fn drain(master: &OwnedFd, bytes: &mut Vec<u8>) {
        loop {
            let mut chunk = [0; 4096];
            match rustix::io::read(master, &mut chunk) {
                Ok(0) | Err(rustix::io::Errno::AGAIN) | Err(rustix::io::Errno::IO) => break,
                Ok(n) => bytes.extend_from_slice(&chunk[..n]),
                Err(rustix::io::Errno::INTR) => {}
                Err(e) => panic!("PTY read {e}"),
            }
        }
    }
    let until = Instant::now() + Duration::from_secs(20);
    let mut bytes = Vec::new();
    let mut sent = 0;
    let status = loop {
        drain(&master, &mut bytes);
        let text = String::from_utf8_lossy(&bytes);
        let prompts = text.matches("Continue? [y/N]:").count()
            + text.matches("[y] Continue, [s] Skip, [a] Abort:").count()
            + text.matches("Reboot remote? [y/N]:").count();
        while sent < prompts {
            rustix::io::write(&master, b"y\n").unwrap();
            sent += 1;
        }
        if let Some(status) = child.try_wait().unwrap() {
            drain(&master, &mut bytes);
            break status;
        }
        if Instant::now() >= until {
            child.kill().unwrap();
            let _ = child.wait();
            panic!("reboot PTY hung: {text}");
        }
        thread::sleep(Duration::from_millis(5));
    };
    let text = String::from_utf8_lossy(&bytes);
    assert!(status.success(), "{text}");
    assert!(
        text.contains("REBOOT_PTY_OK") && text.contains("Reboot remote? [y/N]"),
        "{text}"
    );
    assert!(
        text.contains("waiting for uptime")
            && text.contains("elapsed")
            && text.contains("remaining"),
        "{text}"
    );
    assert!(
        text.contains("0.3s elapsed") || text.contains("0.6s elapsed"),
        "elapsed failed to update during blocked poll: {text}"
    );
}
