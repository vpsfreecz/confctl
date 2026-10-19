//! Actual command routing; every executable is temporary and no host is contacted.
#![cfg(target_os = "linux")]
use serde_json::json;
use std::{
    ffi::OsString,
    fs,
    io::Write,
    os::unix::{
        ffi::{OsStrExt, OsStringExt},
        fs::PermissionsExt,
    },
    path::{Path, PathBuf},
    process::{Child, Command, Output, Stdio},
    time::{Duration, Instant},
};
struct Fixture {
    _scratch: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
    config: OsString,
}
fn tool(name: &str) -> PathBuf {
    std::env::split_paths(&std::env::var_os("PATH").unwrap())
        .map(|path| path.join(name))
        .find(|path| path.is_file())
        .unwrap()
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let bin = scratch.path().join("bin");
        fs::create_dir(&root).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::write(root.join("flake.nix"), "{ outputs = _: {}; }\n").unwrap();
        fs::write(root.join("flake.lock"), "unchanged lock\n").unwrap();
        fs::write(root.join("machines.json"),serde_json::to_vec(&json!({
            "zeta":{"name":"zeta","key":"zeta-key","metaConfig":{"managed":true,"spin":"nixos","host":{"target":"zeta.invalid"},"tags":["chosen"]}},
            "alpha":{"name":"alpha","key":"alpha-key","metaConfig":{"managed":true,"spin":"vpsadminos","host":{"target":"alpha.invalid","port":2222},"tags":["chosen"]}},
            "failed":{"name":"failed","key":"failed-key","metaConfig":{"managed":true,"host":{"target":"failed.invalid"},"tags":["chosen"]}},
            "local":{"name":"local","key":"local-key","metaConfig":{"managed":true,"host":{"target":"localhost"},"tags":["local"]}},
            "reference":{"name":"reference","metaConfig":{"managed":false,"host":{"target":"reference.invalid"},"tags":["other"]}},
            "carried":{"name":"carrier#guest","carrier":"carrier","metaConfig":{"managed":true,"host":{"target":"carried.invalid"},"tags":["chosen"]}},
            "null":{"name":"no-target","metaConfig":{"managed":true,"host":{"target":null},"tags":["chosen"]}}
        })).unwrap()).unwrap();
        executable(
            &bin.join("nix"),
            r#"#!/bin/sh
last=
for arg do last="$arg"; done
printf '%s\n' "$last" >> "$SSH_ROOT/nix-calls"
case "$last" in
 '.#confctl.settings') printf '%s\n' '{"list":{"columns":["name","spin","host.target"]},"nix":{"maxJobs":"auto","impureEval":false,"legacyNixPath":false}}' ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$SSH_ROOT" ;;
 *) exit 97 ;;
esac
"#,
        );
        let child = r#"#!/bin/sh
host=local
for arg do case "$arg" in *.invalid) host="$arg" ;; esac; done
printf '%s\0' "$@" > "$SSH_ROOT/args-$host"
printf '%s\n' "$host" >> "$SSH_ROOT/order"
printf '%s\n' "$$" > "$SSH_ROOT/pid-$host"
printf started > "$SSH_ROOT/started-$host"
if [ "$SSH_MODE" = interactive ]; then
  read -r answer
  printf 'SHELL_INPUT:%s\n' "$answer"
  exit 0
fi
"$SSH_CAT" > "$SSH_ROOT/input-$host"
if [ "$SSH_MODE" = barrier ]; then
  tries=0
  while [ ! -e "$SSH_ROOT/started-zeta.invalid" ] || [ ! -e "$SSH_ROOT/started-alpha.invalid" ] || [ ! -e "$SSH_ROOT/started-failed.invalid" ]; do
    tries=$((tries+1))
    if [ "$tries" -gt 300 ]; then printf 'parallel did not start peers\n'; exit 96; fi
    "$SSH_SLEEP" 0.01
  done
fi
if [ "$SSH_MODE" = cancel ] || { [ "$SSH_MODE" = partial-cancel ] && [ "$host" = alpha.invalid ]; }; then exec "$SSH_SLEEP" 60; fi
printf 'out\n'
printf 'err\n' >&2
printf 'tail\n'
if [ "$SSH_MODE" = signal ]; then kill -TERM "$$"; fi
if [ "$host" = failed.invalid ]; then exit 7; fi
"#;
        executable(&bin.join("ssh"), child);
        executable(&bin.join("requested"), child);
        let mut config = root.join("ssh config-").into_os_string().into_vec();
        config.push(0xff);
        Self {
            _scratch: scratch,
            root,
            bin,
            config: OsString::from_vec(config),
        }
    }
    fn command(&self, args: &[OsString], mode: &str) -> Command {
        let mut command = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        command
            .current_dir(&self.root)
            .args(["--color=never", "ssh"])
            .args(args)
            .env("PATH", &self.bin)
            .env("CONFCTL_SSH_CONFIG", &self.config)
            .env("SSH_ROOT", &self.root)
            .env("SSH_MODE", mode)
            .env("SSH_CAT", tool("cat"))
            .env("SSH_SLEEP", tool("sleep"))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        command
    }
    fn run(&self, args: &[&str], mode: &str, input: &[u8]) -> Output {
        self.run_os(
            &args.iter().map(OsString::from).collect::<Vec<_>>(),
            mode,
            input,
        )
    }
    fn run_os(&self, args: &[OsString], mode: &str, input: &[u8]) -> Output {
        let mut child = self.command(args, mode).spawn().unwrap();
        child.stdin.take().unwrap().write_all(input).unwrap();
        wait(child)
    }
    fn args(&self, host: &str) -> Vec<Vec<u8>> {
        let bytes = fs::read(self.root.join(format!("args-{host}"))).unwrap();
        bytes
            .strip_suffix(&[0])
            .unwrap()
            .split(|b| *b == 0)
            .map(Vec::from)
            .collect()
    }
    fn unchanged(&self) {
        assert_eq!(
            fs::read(self.root.join("flake.lock")).unwrap(),
            b"unchanged lock\n"
        );
        assert!(!self.root.join(".confctl/mutation.lock").exists());
        assert!(!self.root.join(".confctl/generations").exists());
    }
}
fn executable(path: &Path, text: &str) {
    fs::write(path, text).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
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
                "SSH fixture did not exit: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}
fn success(output: &Output) {
    assert_eq!(output.status.code(), Some(0), "{output:?}");
}

#[test]
fn explicit_local_and_remote_commands_keep_native_argv_quoting_and_input_bytes() {
    let payload = [
        OsString::from(""),
        "two words".into(),
        "quote'\"".into(),
        "$(touch unexpected)".into(),
        "line\nbreak".into(),
        OsString::from_vec(b"native\xff".to_vec()),
    ];
    for (target, host) in [("local-key", "local"), ("alpha-key", "alpha.invalid")] {
        let fixture = Fixture::new();
        let mut args = vec![
            "--yes".into(),
            "--input-string".into(),
            OsString::from_vec(b"hello\xff\n".to_vec()),
            target.into(),
            "requested".into(),
        ];
        args.extend(payload.clone());
        let output = fixture.run_os(&args, "", b"");
        success(&output);
        assert_eq!(
            fs::read(fixture.root.join(format!("input-{host}"))).unwrap(),
            b"hello\xff\n"
        );
        let actual = fixture.args(host);
        if host == "local" {
            assert_eq!(
                actual,
                payload
                    .iter()
                    .map(|arg| arg.as_bytes().to_vec())
                    .collect::<Vec<_>>()
            );
        } else {
            let mut expected = vec![
                b"-F".to_vec(),
                fixture.config.clone().into_vec(),
                b"-l".to_vec(),
                b"root".to_vec(),
                b"-p".to_vec(),
                b"2222".to_vec(),
                b"alpha.invalid".to_vec(),
            ];
            expected.push(b"requested '' two\\ words quote\\'\\\" \\$\\(touch\\ unexpected\\) line'\n'break native\\\xff".to_vec());
            assert_eq!(actual, expected);
        }
        let stdout = String::from_utf8_lossy(&output.stdout);
        assert!(stdout.contains("out\nerr\ntail\n"), "{stdout}");
        fixture.unchanged();
    }
}

#[test]
fn serial_and_parallel_aggregation_preserve_merged_order_status_and_sorted_names() {
    for parallel in [false, true] {
        let fixture = Fixture::new();
        let mut args = vec!["--yes", "--aggregate", "--tag=chosen"];
        if parallel {
            args.push("--parallel");
        }
        args.extend(["*", "requested"]);
        let output = fixture.run(&args, if parallel { "barrier" } else { "" }, b"");
        success(&output);
        let stdout = String::from_utf8_lossy(&output.stdout);
        assert!(
            stdout.contains("alpha, zeta:\nExit status: 0\nout\nerr\ntail\n\n"),
            "{stdout}"
        );
        assert!(
            stdout.contains("failed:\nExit status: 7\nout\nerr\ntail\n\n"),
            "{stdout}"
        );
        assert_eq!(stdout.matches("out\nerr\ntail\n").count(), 2, "{stdout}");
        if !parallel {
            assert_eq!(
                fs::read_to_string(fixture.root.join("order")).unwrap(),
                "zeta.invalid\nalpha.invalid\nfailed.invalid\n"
            );
        }
        fixture.unchanged();
    }
}

#[test]
fn each_host_reads_its_own_relative_file_and_failures_retain_merged_output() {
    let fixture = Fixture::new();
    fs::write(fixture.root.join("input"), b"file\0\xff\n").unwrap();
    let output = fixture.run(
        &[
            "--yes",
            "--parallel",
            "--input-file=input",
            "--tag=chosen",
            "*",
            "requested",
        ],
        "",
        b"",
    );
    success(&output);
    for host in ["zeta.invalid", "alpha.invalid", "failed.invalid"] {
        assert_eq!(
            fs::read(fixture.root.join(format!("input-{host}"))).unwrap(),
            b"file\0\xff\n"
        );
    }
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(
        stdout.contains("failed:\nExit status: 7\nout\nerr\ntail\n"),
        "{stdout}"
    );
    fixture.unchanged();
}

#[test]
fn selection_confirmation_empty_and_commandless_usage_are_enforced_before_children() {
    for (args, expected) in [
        (vec!["--yes", "absent", "requested"], 1),
        (vec!["--yes", "carrier#guest", "requested"], 1),
        (vec!["--yes", "no-target", "requested"], 1),
        (vec!["--yes", "--tag=chosen", "*"], 64),
        (vec!["*", "requested"], 1),
    ] {
        let fixture = Fixture::new();
        let output = fixture.run(&args, "", b"n\n");
        assert_eq!(output.status.code(), Some(expected), "{output:?}");
        assert!(!fixture.root.join("order").exists());
        if !args.contains(&"--yes") {
            let text = String::from_utf8_lossy(&output.stdout);
            assert!(
                text.contains("NAME")
                    && text.contains("Command: requested")
                    && text.contains("Continue?"),
                "{text}"
            );
        }
        fixture.unchanged();
    }
    for args in [
        vec!["--yes", "--managed=no", "*", "requested"],
        vec!["--yes", "--managed=all", "--tag=other", "*", "requested"],
        vec![
            "--yes",
            "--attr=spin=vpsadminos",
            "--tag=chosen",
            "*",
            "requested",
        ],
    ] {
        let fixture = Fixture::new();
        let output = fixture.run(&args, "", b"");
        success(&output);
        let order = fs::read_to_string(fixture.root.join("order")).unwrap();
        assert!(
            order == "reference.invalid\n" || order == "alpha.invalid\n",
            "{order}"
        );
        fixture.unchanged();
    }
}

#[test]
fn interactive_remote_uses_plain_ssh_and_ignores_piped_command_modes() {
    let fixture = Fixture::new();
    let output = fixture.run(
        &[
            "--yes",
            "--parallel",
            "--aggregate",
            "--input-file=missing",
            "alpha-key",
        ],
        "interactive",
        b"typed-shell-input\n",
    );
    success(&output);
    assert!(
        String::from_utf8_lossy(&output.stdout).contains("SHELL_INPUT:typed-shell-input"),
        "{output:?}"
    );
    let args = fixture.args("alpha.invalid");
    assert_eq!(
        args,
        vec![
            b"-F".to_vec(),
            fixture.config.clone().into_vec(),
            b"-l".to_vec(),
            b"root".to_vec(),
            b"-p".to_vec(),
            b"2222".to_vec(),
            b"alpha.invalid".to_vec()
        ]
    );
    fixture.unchanged();
}

#[test]
fn cancellation_joins_all_started_parallel_children_and_keeps_serial_results() {
    for (parallel, partial) in [(false, false), (true, false), (false, true)] {
        let fixture = Fixture::new();
        let mut args = vec!["--yes", "--tag=chosen"];
        if parallel {
            args.push("--parallel");
        }
        args.extend(["*", "requested"]);
        let mut child = fixture
            .command(
                &args.iter().map(OsString::from).collect::<Vec<_>>(),
                if partial { "partial-cancel" } else { "cancel" },
            )
            .spawn()
            .unwrap();
        child.stdin.take();
        let deadline = Instant::now() + Duration::from_secs(10);
        let count = if parallel {
            3
        } else if partial {
            2
        } else {
            1
        };
        loop {
            let started = fs::read_dir(&fixture.root)
                .unwrap()
                .filter_map(Result::ok)
                .filter(|entry| entry.file_name().as_bytes().starts_with(b"pid-"))
                .count();
            if started == count {
                break;
            }
            if child.try_wait().unwrap().is_some() {
                panic!("early SSH exit: {:?}", child.wait_with_output().unwrap());
            }
            assert!(Instant::now() < deadline, "children did not enter");
            std::thread::sleep(Duration::from_millis(10));
        }
        nix::sys::signal::kill(
            nix::unistd::Pid::from_raw(child.id() as i32),
            nix::sys::signal::Signal::SIGINT,
        )
        .unwrap();
        let output = wait(child);
        assert_eq!(output.status.code(), Some(130), "{output:?}");
        for entry in fs::read_dir(&fixture.root)
            .unwrap()
            .filter_map(Result::ok)
            .filter(|entry| entry.file_name().as_bytes().starts_with(b"pid-"))
        {
            let pid = fs::read_to_string(entry.path()).unwrap();
            assert!(
                !Path::new(&format!("/proc/{}", pid.trim())).exists(),
                "orphan {pid}"
            );
        }
        if !parallel {
            assert_eq!(
                fs::read_to_string(fixture.root.join("order")).unwrap(),
                if partial {
                    "zeta.invalid\nalpha.invalid\n"
                } else {
                    "zeta.invalid\n"
                }
            );
            if partial {
                assert!(
                    String::from_utf8_lossy(&output.stdout).contains("zeta:\nout\nerr\ntail"),
                    "{output:?}"
                );
            }
        }
        fixture.unchanged();
    }
}

#[test]
fn normal_signaled_child_is_a_completed_host_result_and_missing_file_is_invocation_failure() {
    let fixture = Fixture::new();
    let output = fixture.run(&["--yes", "zeta-key", "requested"], "signal", b"");
    success(&output);
    assert!(String::from_utf8_lossy(&output.stdout).contains("Signal: 15\nout\nerr\ntail"));
    let fixture = Fixture::new();
    let output = fixture.run(
        &["--yes", "--input-file=missing", "zeta-key", "requested"],
        "",
        b"",
    );
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    fixture.unchanged();
}
