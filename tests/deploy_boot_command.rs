//! Saved boot routes use confined fake queries, copies and activation executables.
#![cfg(target_os = "linux")]
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
 exec /run/current-system/sw/bin/sleep 60
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
script=$(/run/current-system/sw/bin/cat)
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
    for args in [
        vec!["--generation=saved", "--yes", "remote"],
        vec!["--generation=saved", "--yes", "remote", "switch"],
        vec!["--generation=saved", "--yes", "remote", "test"],
        vec!["--yes", "remote", "dry-activate"],
        vec!["--yes", "remote", "boot"],
        vec!["--generation=saved", "--reboot", "--yes", "remote", "boot"],
    ] {
        let f = Fixture::new();
        let out = f.run(&args, "", b"");
        assert_eq!(out.status.code(), Some(1), "{args:?}: {out:?}");
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
