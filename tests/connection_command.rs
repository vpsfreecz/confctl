//! Real executable routing; every Nix/SSH/local probe is a temporary fake.
#![cfg(target_os = "linux")]

use serde_json::json;
use std::{
    ffi::OsString,
    fs,
    io::Write,
    os::unix::{ffi::OsStringExt, fs::PermissionsExt},
    path::{Path, PathBuf},
    process::{Child, Command, Output, Stdio},
    thread,
    time::{Duration, Instant},
};

struct Fixture {
    _scratch: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
    config: OsString,
}

impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let bin = scratch.path().join("bin");
        fs::create_dir_all(root.join(".confctl/generations/local")).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::write(root.join("flake.nix"), "{ outputs = _: {}; }\n").unwrap();
        fs::write(root.join("flake.lock"), b"unchanged lock bytes\n").unwrap();
        fs::write(
            root.join(".confctl/generations/local/retained"),
            b"retained state\n",
        )
        .unwrap();
        std::os::unix::fs::symlink("retained", root.join(".confctl/generations/local/current"))
            .unwrap();
        fs::write(root.join("machines.json"), serde_json::to_vec(&json!({
            "local":{"name":"nodes/local","key":"local-key","metaConfig":{"managed":true,"spin":"vpsadminos","host":{"target":"localhost"},"tags":["chosen"]}},
            "remote":{"name":"nodes/remote","key":"remote-key","metaConfig":{"managed":true,"spin":"nixos","host":{"target":"remote.invalid","port":"02222suffix"},"tags":["chosen"]}},
            "fallback":{"name":"fallback.invalid","metaConfig":{"managed":true,"spin":"other","tags":["chosen"]}},
            "unmanaged":{"name":"reference","metaConfig":{"managed":false,"spin":"nixos","host":{"target":"reference.invalid"},"tags":["reference"]}},
            "carried":{"name":"carrier#guest","carrier":"carrier","metaConfig":{"managed":true,"host":{"target":"carried.invalid"},"tags":["chosen"]}},
            "null":{"name":"no-target","metaConfig":{"managed":true,"host":{"target":null},"tags":["chosen"]}}
        })).unwrap()).unwrap();
        executable(
            &bin.join("nix"),
            r#"#!/bin/sh
printf '%s\0' "$@" >> "$CONNECTION_ROOT/nix-calls"
printf '\n' >> "$CONNECTION_ROOT/nix-calls"
last=
for arg do last="$arg"; done
case "$last" in
  '.#confctl.settings') printf '%s\n' '{"list":{"columns":["name","spin","host.target"]},"nix":{"maxJobs":"auto","impureEval":false,"legacyNixPath":false}}' ;;
  '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$CONNECTION_ROOT" ;;
  *) printf '%s\n' "unexpected Nix target: $last" >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &bin.join("cat"),
            r#"#!/bin/sh
printf 'cat\0' >> "$CONNECTION_ROOT/probes"
printf '%s\0' "$@" >> "$CONNECTION_ROOT/probes"
printf '\n' >> "$CONNECTION_ROOT/probes"
if [ "$CONNECTION_MODE" = all-fail ]; then printf 'local refused\n' >&2; exit 1; fi
printf 'UPTIME_PRIVATE_LOCAL 99.0\n'
"#,
        );
        executable(
            &bin.join("ssh"),
            r#"#!/bin/sh
printf 'ssh\0' >> "$CONNECTION_ROOT/probes"
printf '%s\0' "$@" >> "$CONNECTION_ROOT/probes"
printf '\n' >> "$CONNECTION_ROOT/probes"
if [ "$CONNECTION_MODE" = slow ]; then
  printf '%s\n' "$$" > "$CONNECTION_ROOT/probe-pid"
  exec "$CONNECTION_SLEEP" 60
fi
if [ "$CONNECTION_MODE" = signal ]; then kill -TERM "$$"; fi
for arg do
  if [ "$arg" = remote.invalid ] || [ "$CONNECTION_MODE" = all-fail ]; then
    printf 'failed probe stdout\n'
    printf 'fixture SSH refused\n' >&2
    exit 255
  fi
done
printf 'UPTIME_PRIVATE_REMOTE 88.0\n'
"#,
        );
        let mut config = root.join("ssh config ").into_os_string().into_vec();
        config.push(0xff);
        Self {
            _scratch: scratch,
            root,
            bin,
            config: OsString::from_vec(config),
        }
    }

    fn command(&self, args: &[&str], mode: &str) -> Command {
        // No host PATH fallback: a missed fake must fail instead of contacting a host.
        let mut command = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        command
            .current_dir(&self.root)
            .arg("--color=never")
            .arg("test-connection")
            .args(args)
            .env("PATH", &self.bin)
            .env("NO_COLOR", "1")
            .env("CONFCTL_TTY", "0")
            .env("CONNECTION_ROOT", &self.root)
            .env("CONNECTION_MODE", mode)
            .env("CONFCTL_SSH_CONFIG", &self.config)
            .env("CONNECTION_SLEEP", "/run/current-system/sw/bin/sleep")
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

    fn probes(&self) -> Vec<Vec<Vec<u8>>> {
        fs::read(self.root.join("probes"))
            .unwrap_or_default()
            .split(|byte| *byte == b'\n')
            .filter(|line| !line.is_empty())
            .map(|line| {
                line.split(|byte| *byte == 0)
                    .filter(|arg| !arg.is_empty())
                    .map(Vec::from)
                    .collect()
            })
            .collect()
    }

    fn assert_unchanged(&self) {
        assert_eq!(
            fs::read(self.root.join("flake.lock")).unwrap(),
            b"unchanged lock bytes\n"
        );
        assert_eq!(
            fs::read(self.root.join(".confctl/generations/local/retained")).unwrap(),
            b"retained state\n"
        );
        assert_eq!(
            fs::read_link(self.root.join(".confctl/generations/local/current")).unwrap(),
            Path::new("retained")
        );
        assert!(!self.root.join(".confctl/mutation.lock").exists());
        assert!(!self.root.join("configs/node/kernels.json").exists());
        assert!(!self.root.join("cluster/netbootable.nix").exists());
    }
}

fn executable(path: &Path, content: &str) {
    fs::write(path, content).unwrap();
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
                "connection command did not finish: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        thread::sleep(Duration::from_millis(10));
    }
}

#[test]
fn real_command_checks_managed_runnable_hosts_in_order_and_preserves_state() {
    let fixture = Fixture::new();
    let output = fixture.run(&["--yes"], "", b"");
    assert_eq!(output.status.code(), Some(0), "{output:?}");
    assert!(
        String::from_utf8_lossy(&output.stderr).starts_with("\nTiming: completed (exit 0)\n"),
        "{output:?}"
    );
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(
        stdout.contains("Result: 2 successful, 1 failed\n\nFailed machines:\n  nodes/remote\n"),
        "{stdout}"
    );
    assert!(
        stdout.contains("Unable to connect to nodes/remote:")
            && stdout.contains("exit status: 255")
            && stdout.contains("failed probe stdout")
            && stdout.contains("fixture SSH refused"),
        "{stdout}"
    );
    assert!(
        !stdout.contains("UPTIME_PRIVATE")
            && !stdout.contains("Continue?")
            && !stdout.contains("\x1b[")
    );
    let calls = fixture.probes();
    assert_eq!(calls.len(), 3, "{calls:?}");
    assert_eq!(calls[0], [b"cat".to_vec(), b"/proc/uptime".to_vec()]);
    let mut expected = vec![
        b"ssh".to_vec(),
        b"-F".to_vec(),
        fixture.config.clone().into_vec(),
    ];
    expected.extend(
        [
            "-l",
            "root",
            "-p",
            "2222",
            "-o",
            "ConnectTimeout=3",
            "-o",
            "ServerAliveInterval=3",
            "-o",
            "ServerAliveCountMax=1",
            "remote.invalid",
            "cat /proc/uptime",
        ]
        .map(|arg| arg.as_bytes().to_vec()),
    );
    assert_eq!(calls[1], expected);
    assert_eq!(
        calls[2][calls[2].len() - 2..],
        [b"fallback.invalid".to_vec(), b"cat /proc/uptime".to_vec()]
    );
    assert!(!calls[2].iter().any(|arg| arg == b"-p"));
    let nix = fs::read_to_string(fixture.root.join("nix-calls")).unwrap();
    assert_eq!(nix.lines().count(), 3);
    assert!(
        nix.lines()
            .all(|line| line.contains("--no-write-lock-file") && !line.contains("--show-trace"))
    );
    fixture.assert_unchanged();
}

#[test]
fn filters_support_unmanaged_attributes_tags_and_keys_without_a_spin_filter() {
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
        let calls = fixture.probes();
        assert_eq!(calls.len(), 1, "{calls:?}");
        assert_eq!(calls[0][calls[0].len() - 2], target.as_bytes());
        fixture.assert_unchanged();
    }
}

#[test]
fn decline_and_empty_selection_do_not_admit_or_probe_hosts() {
    for args in [
        vec![],
        vec!["--yes", "absent*"],
        vec!["--yes", "carrier#guest"],
        vec!["--yes", "no-target"],
    ] {
        let fixture = Fixture::new();
        let output = fixture.run(&args, "", b"n\n");
        assert_eq!(output.status.code(), Some(1), "{output:?}");
        let stdout = String::from_utf8(output.stdout).unwrap();
        let stderr = String::from_utf8(output.stderr).unwrap();
        if args.is_empty() {
            assert!(
                stdout.contains("Test SSH connection to the following machines:")
                    && stdout.contains("NAME")
                    && stdout.contains("Continue?"),
                "{stdout}"
            );
            assert!(stderr.contains("Aborted"), "{stderr}");
        } else {
            assert!(stderr.contains("No machines to test"), "{stderr}");
        }
        assert!(
            !stdout.contains("pending")
                && !stdout.contains("test connection to")
                && !stdout.contains("Result:")
        );
        assert!(fixture.probes().is_empty());
        fixture.assert_unchanged();
    }
}

#[test]
fn ordinary_all_failed_or_signaled_hosts_still_complete_with_exit_zero() {
    for (args, mode, result) in [
        (vec!["--yes"], "all-fail", "Result: 0 successful, 3 failed"),
        (vec!["--yes", "remote-key"], "signal", "signal: 15"),
    ] {
        let fixture = Fixture::new();
        let output = fixture.run(&args, mode, b"");
        assert_eq!(output.status.code(), Some(0), "{output:?}");
        assert!(
            String::from_utf8_lossy(&output.stdout).contains(result),
            "{output:?}"
        );
        fixture.assert_unchanged();
    }
    let fixture = Fixture::new();
    let output = fixture.run(&["--yes", "local-key"], "", b"");
    assert_eq!(output.status.code(), Some(0), "{output:?}");
    assert!(
        String::from_utf8_lossy(&output.stdout)
            .contains("Result: 1 successful, 0 failed\n\nFailed machines:\n")
    );
}

#[test]
fn sigint_joins_probe_and_does_not_enter_later_host() {
    let fixture = Fixture::new();
    let mut child = fixture
        .command(&["--yes", "--attr=spin!=vpsadminos"], "slow")
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    let pid = loop {
        if let Ok(text) = fs::read_to_string(fixture.root.join("probe-pid"))
            && let Ok(pid) = text.trim().parse::<i32>()
        {
            break pid;
        }
        if child.try_wait().unwrap().is_some() {
            panic!(
                "command exited before probe: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        assert!(Instant::now() < deadline, "probe did not start");
        thread::sleep(Duration::from_millis(10));
    };
    nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(child.id() as i32),
        nix::sys::signal::Signal::SIGINT,
    )
    .unwrap();
    let output = wait(child);
    assert_eq!(output.status.code(), Some(130), "{output:?}");
    assert_eq!(fixture.probes().len(), 1);
    assert!(
        !Path::new(&format!("/proc/{pid}")).exists(),
        "probe still running after joined app exit"
    );
    assert!(!String::from_utf8_lossy(&output.stdout).contains("Result:"));
    fixture.assert_unchanged();
}
