//! Real CLI with fake inventory, nix-shell and cssh; no GUI or host access.
#![cfg(target_os = "linux")]

use serde_json::{Value, json};
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
    _scratch: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let bin = scratch.path().join("bin");
        fs::create_dir_all(root.join(".confctl/generations/local")).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::write(root.join("flake.nix"), "{ outputs = _: {}; }\n").unwrap();
        fs::write(root.join("flake.lock"), "retained lock\n").unwrap();
        fs::write(
            root.join(".confctl/generations/local/saved"),
            "retained generation\n",
        )
        .unwrap();
        std::os::unix::fs::symlink("saved", root.join(".confctl/generations/local/current"))
            .unwrap();
        let fixture = Self {
            _scratch: scratch,
            root,
            bin,
        };
        fixture.inventory(json!({
            "z":{"name":"nodes/local","key":"local-key","metaConfig":{"managed":true,"spin":"vpsadminos","host":{"target":"localhost","port":false},"tags":["chosen"]}},
            "a":{"name":"nodes/remote","key":"remote-key","metaConfig":{"managed":true,"spin":"nixos","host":{"target":"remote.invalid","port":2222},"tags":["chosen"]}},
            "fallback":{"name":"fallback.invalid","metaConfig":{"managed":true,"spin":"other","tags":["chosen"]}},
            "reference":{"name":"reference","metaConfig":{"managed":false,"spin":"nixos","host":{"target":"reference.invalid"},"tags":["reference"]}},
            "carried":{"name":"carrier#guest","carrier":"carrier","metaConfig":{"managed":true,"host":{"target":"carried.invalid"}}},
            "null":{"name":"no-target","metaConfig":{"managed":true,"host":{"target":null}}}
        }));
        executable(
            &fixture.bin.join("nix"),
            r#"#!/bin/sh
printf '%s\0' "$@" >> "$CSSH_ROOT/nix-calls"
printf '\n' >> "$CSSH_ROOT/nix-calls"
last=
for arg do last="$arg"; done
case "$last" in
 '.#confctl.settings') printf '%s\n' '{"list":{"columns":["name","spin","host.target"]},"nix":{"maxJobs":"auto","impureEval":false,"legacyNixPath":false}}' ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$CSSH_ROOT" ;;
 *) printf 'unexpected Nix target: %s\n' "$last" >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &fixture.bin.join("nix-shell"),
            r#"#!/bin/sh
printf '%s\0' "$@" > "$CSSH_ROOT/launcher-argv"
printf '%s\0' "${shellHook+set}" "$DISPLAY" "$XAUTHORITY" "$SSH_AUTH_SOCK" "$CSSH_OTHER" "$CONFCTL_SSH_CONFIG" "$PWD" > "$CSSH_ROOT/launcher-env"
if IFS= read -r line; then printf 'unexpected launcher stdin\n' >&2; exit 98; fi
exec /bin/sh -c "exec $4"
"#,
        );
        executable(
            &fixture.bin.join("cssh"),
            r#"#!/bin/sh
printf '%s\0' "$@" > "$CSSH_ROOT/cssh-argv"
printf 'launcher stdout\377\n'
printf 'launcher stderr\376\n' >&2
case "$CSSH_MODE" in
 fail) exit 7 ;;
 signal) kill -TERM "$$" ;;
 slow) printf '%s\n' "$$" > "$CSSH_ROOT/launcher-pid"; exec /run/current-system/sw/bin/sleep 60 ;;
esac
"#,
        );
        fixture
    }
    fn inventory(&self, value: Value) {
        fs::write(
            self.root.join("machines.json"),
            serde_json::to_vec(&value).unwrap(),
        )
        .unwrap();
    }
    fn command(&self, args: &[&str], mode: &str) -> Command {
        let mut command = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        command
            .current_dir(&self.root)
            .arg("--color=never")
            .arg("cssh")
            .args(args)
            .env("PATH", &self.bin)
            .env("NO_COLOR", "1")
            .env("CONFCTL_TTY", "0")
            .env("CSSH_ROOT", &self.root)
            .env("CSSH_MODE", mode)
            .env("shellHook", "must be absent")
            .env("DISPLAY", ":97.0")
            .env("XAUTHORITY", "literal auth path")
            .env("SSH_AUTH_SOCK", "literal agent socket")
            .env("CSSH_OTHER", "retained environment")
            .env("CONFCTL_SSH_CONFIG", "ignored ssh config")
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
    fn fields(&self, file: &str) -> Vec<Vec<u8>> {
        let bytes = fs::read(self.root.join(file)).unwrap();
        assert_eq!(bytes.last(), Some(&0));
        bytes[..bytes.len() - 1]
            .split(|byte| *byte == 0)
            .map(Vec::from)
            .collect()
    }
    fn assert_unchanged(&self) {
        assert_eq!(
            fs::read(self.root.join("flake.lock")).unwrap(),
            b"retained lock\n"
        );
        assert_eq!(
            fs::read(self.root.join(".confctl/generations/local/saved")).unwrap(),
            b"retained generation\n"
        );
        assert_eq!(
            fs::read_link(self.root.join(".confctl/generations/local/current")).unwrap(),
            Path::new("saved")
        );
        assert!(!self.root.join(".confctl/mutation.lock").exists());
        assert!(!self.root.join("configs/node/kernels.json").exists());
        assert!(!self.root.join("cluster/netbootable.nix").exists());
        let calls = fs::read_to_string(self.root.join("nix-calls")).unwrap();
        assert!(
            calls
                .lines()
                .all(|line| line.contains("--no-write-lock-file")
                    && (line.ends_with(".#confctl.settings\0")
                        || line.ends_with(".#confctl.machinesJson\0"))),
            "{calls:?}"
        );
    }
}

fn executable(path: &Path, script: &str) {
    fs::write(path, script).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}
fn wait(mut child: Child) -> Output {
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        if child.try_wait().unwrap().is_some() {
            return child.wait_with_output().unwrap();
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            panic!(
                "CSSH command did not finish: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        thread::sleep(Duration::from_millis(10));
    }
}

#[test]
fn managed_runnable_selection_uses_one_launcher_and_preserves_environment_and_logs() {
    let fixture = Fixture::new();
    let output = fixture.run(&["--yes"], "", b"");
    assert_eq!(output.status.code(), Some(0), "{output:?}");
    assert_eq!(
        fixture.fields("launcher-argv"),
        [
            "-p",
            "perlPackages.AppClusterSSH",
            "--command",
            "cssh -l root localhost remote.invalid fallback.invalid"
        ]
        .map(|s| s.as_bytes().to_vec())
    );
    assert_eq!(
        fixture.fields("cssh-argv"),
        [
            "-l",
            "root",
            "localhost",
            "remote.invalid",
            "fallback.invalid"
        ]
        .map(|s| s.as_bytes().to_vec())
    );
    let mut expected = [
        "",
        ":97.0",
        "literal auth path",
        "literal agent socket",
        "retained environment",
        "ignored ssh config",
    ]
    .map(|s| s.as_bytes().to_vec())
    .to_vec();
    expected.push(fixture.root.as_os_str().as_encoded_bytes().to_vec());
    assert_eq!(fixture.fields("launcher-env"), expected);
    assert!(
        output
            .stdout
            .windows(b"launcher stdout\xff\n".len())
            .any(|s| s == b"launcher stdout\xff\n"),
        "{output:?}"
    );
    assert!(
        output
            .stderr
            .windows(b"launcher stderr\xfe\n".len())
            .any(|s| s == b"launcher stderr\xfe\n"),
        "{output:?}"
    );
    assert!(!String::from_utf8_lossy(&output.stdout).contains("Continue?"));
    let logs = fs::read_dir(fixture.root.join(".confctl/logs"))
        .unwrap()
        .collect::<Result<Vec<_>, _>>()
        .unwrap();
    assert_eq!(logs.len(), 1);
    let log = fs::read(logs[0].path()).unwrap();
    for bytes in [
        b"launcher stdout\xff\n".as_slice(),
        b"launcher stderr\xfe\n".as_slice(),
    ] {
        assert!(log.windows(bytes.len()).any(|s| s == bytes));
    }
    fixture.assert_unchanged();
}

#[test]
fn filters_keep_managed_overrides_patterns_attributes_tags_and_keys() {
    for (args, target) in [
        (vec!["--yes", "--managed=no"], "reference.invalid"),
        (
            vec!["--yes", "--managed=all", "--tag=reference"],
            "reference.invalid",
        ),
        (
            vec!["--yes", "--attr=spin=nixos", "--tag=chosen", "remote-key"],
            "remote.invalid",
        ),
        (vec!["--yes", "fallback*"], "fallback.invalid"),
    ] {
        let fixture = Fixture::new();
        let output = fixture.run(&args, "", b"");
        assert_eq!(output.status.code(), Some(0), "{output:?}");
        assert_eq!(
            fixture.fields("cssh-argv"),
            ["-l", "root", target].map(|s| s.as_bytes().to_vec())
        );
        fixture.assert_unchanged();
    }
}

#[test]
fn confirmation_decline_and_empty_selection_never_launch() {
    for args in [
        vec![],
        vec!["--yes", "absent*"],
        vec!["--yes", "carrier#guest"],
        vec!["--yes", "no-target"],
    ] {
        let fixture = Fixture::new();
        let output = fixture.run(&args, "", b"n\n");
        assert_eq!(output.status.code(), Some(1), "{output:?}");
        if args.is_empty() {
            let stdout = String::from_utf8_lossy(&output.stdout);
            assert!(
                stdout.contains("Open cssh to the following machines:")
                    && stdout.contains("NAME")
                    && stdout.contains("Continue?"),
                "{output:?}"
            );
            assert!(String::from_utf8_lossy(&output.stderr).contains("Aborted"));
        } else {
            assert!(
                String::from_utf8_lossy(&output.stderr).contains("No machines to open cssh to")
            );
        }
        assert!(!fixture.root.join("launcher-argv").exists());
        fixture.assert_unchanged();
    }
    let fixture = Fixture::new();
    assert_eq!(
        fixture.run(&["local-key"], "", b"y\n").status.code(),
        Some(0)
    );
}

#[test]
fn target_quoting_round_trips_literal_shell_characters_and_rejects_invalid_metadata() {
    let fixture = Fixture::new();
    let targets = ["a b'\"$HOME;printf INJECTED", "line\nsecond", ""];
    fixture.inventory(Value::Object(targets.iter().enumerate().map(|(i, target)| (format!("node{i}"), json!({"name":format!("node{i}"), "metaConfig":{"managed":true, "host":{"target":target}}}))).collect()));
    let output = fixture.run(&["--yes"], "", b"");
    assert_eq!(output.status.code(), Some(0), "{output:?}");
    let mut expected = vec![b"-l".to_vec(), b"root".to_vec()];
    expected.extend(targets.map(|s| s.as_bytes().to_vec()));
    assert_eq!(fixture.fields("cssh-argv"), expected);
    assert_eq!(
        fixture.fields("launcher-argv")[3],
        b"cssh -l root a\\ b\\'\\\"\\$HOME\\;printf\\ INJECTED line'\n'second ''"
    );
    fixture.assert_unchanged();
    for host in [
        json!({"target":true}),
        json!(false),
        json!({"target":"nul\u{0000}host"}),
    ] {
        let fixture = Fixture::new();
        fixture
            .inventory(json!({"node":{"name":"node", "metaConfig":{"managed":true,"host":host}}}));
        let output = fixture.run(&["--yes"], "", b"");
        assert_eq!(output.status.code(), Some(1), "{output:?}");
        assert!(!fixture.root.join("launcher-argv").exists());
        fixture.assert_unchanged();
    }
}

#[test]
fn nonzero_and_signal_are_launcher_failures_with_compatible_invocation_success() {
    for (mode, diagnostic) in [("fail", "exit status: 7"), ("signal", "signal: 15")] {
        let fixture = Fixture::new();
        let output = fixture.run(&["--yes", "local-key"], mode, b"");
        assert_eq!(output.status.code(), Some(0), "{output:?}");
        let stderr = String::from_utf8_lossy(&output.stderr);
        assert!(
            stderr.contains("ClusterSSH launcher failed with") && stderr.contains(diagnostic),
            "{output:?}"
        );
        fixture.assert_unchanged();
    }
    let fixture = Fixture::new();
    fs::remove_file(fixture.bin.join("nix-shell")).unwrap();
    let output = fixture.run(&["--yes"], "", b"");
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    assert!(!fixture.root.join("cssh-argv").exists());
    fixture.assert_unchanged();
}

#[test]
fn cancellation_joins_the_owned_launcher_without_claiming_gui_session_cleanup() {
    let fixture = Fixture::new();
    let mut child = fixture.command(&["--yes"], "slow").spawn().unwrap();
    drop(child.stdin.take());
    let deadline = Instant::now() + Duration::from_secs(10);
    let pid = loop {
        if let Ok(text) = fs::read_to_string(fixture.root.join("launcher-pid"))
            && let Ok(pid) = text.trim().parse::<i32>()
            && pid > 1
        {
            break pid;
        }
        if child.try_wait().unwrap().is_some() {
            panic!(
                "exited before launcher: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        assert!(
            Instant::now() < deadline,
            "launcher never wrote a valid PID"
        );
        thread::sleep(Duration::from_millis(10));
    };
    let stat = fs::read_to_string(format!("/proc/{pid}/stat")).unwrap();
    let start = stat
        .rsplit_once(") ")
        .unwrap()
        .1
        .split_whitespace()
        .nth(19)
        .unwrap()
        .to_owned();
    nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(child.id() as i32),
        nix::sys::signal::Signal::SIGINT,
    )
    .unwrap();
    let output = wait(child);
    assert_eq!(output.status.code(), Some(130), "{output:?}");
    if let Ok(stat) = fs::read_to_string(format!("/proc/{pid}/stat")) {
        assert_ne!(
            stat.rsplit_once(") ")
                .unwrap()
                .1
                .split_whitespace()
                .nth(19)
                .unwrap(),
            start,
            "owned launcher survived joined CLI exit"
        );
    }
    fixture.assert_unchanged();
}
