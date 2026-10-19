//! Saved-generation copy route with confined fake Nix/host/copy executables.
#![cfg(target_os = "linux")]
use confctl::generation::BuildGeneration;
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
    _tmp: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
    initial: Vec<(PathBuf, Vec<u8>)>,
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
        fs::create_dir_all(&root).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::write(root.join("flake.nix"), "{}\n").unwrap();
        fs::write(root.join("flake.lock"), "retained lock\n").unwrap();
        let machines = json!({
            "g1":{"name":"c1#one","carrier":"c1","metaConfig":{"managed":true,"spin":"vpsadminos","host":{"target":null},"tags":["chosen"],"autoRollback":{"enable":false}}},
            "g2":{"name":"c1#two","carrier":"c1","metaConfig":{"managed":true,"spin":"nixos","host":{"target":null},"tags":["chosen"]}},
            "g3":{"name":"c2#one","carrier":"c2","metaConfig":{"managed":true,"spin":"other","host":{"target":null},"tags":["chosen"]}},
            "direct":{"name":"direct","metaConfig":{"managed":true,"spin":"vpsadminos","host":{"target":"direct.invalid","port":"02_222suffix"},"tags":["chosen"]}},
            "local":{"name":"local","metaConfig":{"managed":true,"spin":"vpsadminos","host":{"target":"localhost"},"tags":["chosen"]}},
            "missing":{"name":"missing","metaConfig":{"managed":true,"spin":"nixos","host":{"target":"missing.invalid"},"tags":["chosen"]}},
            "c1":{"name":"c1","metaConfig":{"managed":false,"host":{"target":"carrier-one.invalid","port":2201}}},
            "c2":{"name":"c2","metaConfig":{"managed":false,"host":{"target":"carrier-two.invalid"}}},
            "unmanaged":{"name":"excluded","metaConfig":{"managed":false,"host":{"target":"excluded.invalid"},"tags":["chosen"]}}
        });
        fs::write(
            root.join("machines.json"),
            serde_json::to_vec(&machines).unwrap(),
        )
        .unwrap();
        let mut initial = Vec::new();
        for host in ["c1#one", "c1#two", "c2#one", "direct", "local", "excluded"] {
            for (name, year) in [("old", 2000), ("new", 2001), ("+1", 2002)] {
                let dir = root.join(".confctl/generations").join(host).join(name);
                fs::create_dir_all(&dir).unwrap();
                let generation = BuildGeneration {
                    name: name.into(),
                    date: chrono::DateTime::parse_from_rfc3339(&format!("{year}-01-01T00:00:00Z"))
                        .unwrap(),
                    toplevel: PathBuf::from(format!("/store/{host}-{name}")),
                    auto_rollback: Some(format!("/store/{host}-{name}-rollback").into()),
                    inputs: Default::default(),
                    inputs_info: serde_json::from_value(json!({"nixpkgs":{"rev":"abcdef012345"}}))
                        .unwrap(),
                };
                let path = dir.join("generation.json");
                let bytes = serde_json::to_vec(&generation.encode()).unwrap();
                fs::write(&path, &bytes).unwrap();
                initial.push((path, bytes));
            }
            std::os::unix::fs::symlink(
                "old",
                root.join(".confctl/generations").join(host).join("current"),
            )
            .unwrap();
        }
        executable(
            &bin.join("nix"),
            r#"#!/bin/sh
printf 'nix %s\n' "$*" >> "$COPY_ROOT/sequence"
last=
for arg do last="$arg"; done
case "$last" in
 '.#confctl.settings')
  if [ "$COPY_MODE" = refresh ] || [ "$COPY_MODE" = refresh-failure ]; then
   printf '%s\n' '{"runtimeKernels":{"enable":true,"refreshBeforeDeploy":true,"stateFile":"kernels.json"}}'
  else printf '%s\n' '{}'; fi ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$COPY_ROOT" ;;
 *) printf 'UNEXPECTED NIX\n' >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &bin.join("ssh"),
            r#"#!/bin/sh
printf 'probe %s\n' "$*" >> "$COPY_ROOT/sequence"
printf '6.12.3-test\n'
"#,
        );
        executable(
            &bin.join("uname"),
            r#"#!/bin/sh
printf 'uname %s\n' "$*" >> "$COPY_ROOT/sequence"
printf '6.12.3-local\n'
"#,
        );
        executable(
            &bin.join("nix-copy-closure"),
            r#"#!/bin/sh
record="$COPY_ROOT/copy-call-$$"
{
 printf 'copy\0%s\0' "${NIX_SSHOPTS-UNSET}"
 printf '%s\0' "$@"
} > "$record"
printf '%s\n' "$$" >> "$COPY_ROOT/call-order"
printf 'copy %s\n' "$*" >> "$COPY_ROOT/sequence"
if [ "$COPY_MODE" = slow ]; then printf '%s' "$$" > "$COPY_ROOT/pid"; exec /run/current-system/sw/bin/sleep 60; fi
printf "copying 2 paths...\ncopying path '/store/first'...\nraw copy log\ncopying path '/store/second'...\n" >&2
if [ "$COPY_MODE" = carrier-failure ]; then exit 7; fi
if [ "$COPY_MODE" = failure ] || [ "$COPY_MODE" = refresh-failure ]; then
 for arg do if [ "$arg" = root@direct.invalid ]; then exit 7; fi; done
fi
"#,
        );
        Self {
            _tmp: tmp,
            root,
            bin,
            initial,
        }
    }
    fn command(&self, args: &[&str], mode: &str) -> Command {
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        cmd.current_dir(&self.root)
            .args(["--color=never", "deploy"])
            .args(args)
            .env("PATH", &self.bin)
            .env("CONFCTL_TTY", "0")
            .env("COPY_ROOT", &self.root)
            .env("COPY_MODE", mode)
            .env("NIX_SSHOPTS", "-o Existing=yes")
            .env("CONFCTL_SSH_CONFIG", "not-for-copy")
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
    fn raw_calls(&self) -> Vec<(String, Vec<u8>)> {
        let mut records = fs::read_dir(&self.root)
            .unwrap()
            .filter_map(|entry| {
                let entry = entry.unwrap();
                let name = entry.file_name().to_str().unwrap().to_owned();
                name.starts_with("copy-call-")
                    .then(|| (name, fs::read(entry.path()).unwrap()))
            })
            .collect::<Vec<_>>();
        records.sort_by(|a, b| a.0.cmp(&b.0)); // Diagnostic order only, never execution order.
        records
    }
    fn calls(&self) -> Vec<Vec<Vec<u8>>> {
        let records = self.raw_calls();
        let index = fs::read_to_string(self.root.join("call-order")).unwrap_or_default();
        let order = index.lines().collect::<Vec<_>>();
        assert_eq!(
            order.len(),
            records.len(),
            "call index {index:?}; raw records {records:?}"
        );
        let mut seen = std::collections::HashSet::new();
        order
            .into_iter()
            .map(|pid| {
                assert!(
                    pid.parse::<u32>().is_ok_and(|n| n > 0) && seen.insert(pid),
                    "invalid call index {index:?}; raw records {records:?}"
                );
                let name = format!("copy-call-{pid}");
                let (_, bytes) = records.iter().find(|(n, _)| n == &name).unwrap_or_else(|| {
                    panic!("missing {name}; index {index:?}; raw records {records:?}")
                });
                let bytes = bytes.strip_suffix(&[0]).unwrap_or_else(|| {
                    panic!("unterminated record {name}; raw records {records:?}")
                });
                let fields = bytes.split(|b| *b == 0).map(Vec::from).collect::<Vec<_>>();
                assert!(
                    fields.len() >= 5 && fields[0] == b"copy" && fields[2] == b"--to",
                    "invalid record {name}; raw records {records:?}"
                );
                fields
            })
            .collect()
    }
    fn unchanged(&self) {
        for (p, b) in &self.initial {
            assert_eq!(&fs::read(p).unwrap(), b);
        }
        for host in ["c1#one", "c1#two", "c2#one", "direct", "local", "excluded"] {
            assert_eq!(
                fs::read_link(
                    self.root
                        .join(".confctl/generations")
                        .join(host)
                        .join("current")
                )
                .unwrap(),
                Path::new("old")
            );
        }
        assert_eq!(
            fs::read(self.root.join("flake.lock")).unwrap(),
            b"retained lock\n"
        );
        assert!(!self.root.join("roots").exists());
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
                "copy fixture timeout: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        thread::sleep(Duration::from_millis(10));
    }
}
#[test]
fn copy_saved_generations_preserves_paths_carrier_order_env_and_read_only_state() {
    let f = Fixture::new();
    let out = f.run(
        &[
            "--copy-only",
            "--generation=current",
            "--yes",
            "--tag=chosen",
            "--max-concurrent-copy=1",
        ],
        "",
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let calls = f.calls();
    assert_eq!(calls.len(), 4, "{calls:?}");
    assert_eq!(
        calls
            .iter()
            .map(|r| String::from_utf8_lossy(&r[3]).into_owned())
            .collect::<Vec<_>>(),
        [
            "root@direct.invalid",
            "root@carrier-one.invalid",
            "root@carrier-two.invalid",
            "root@carrier-one.invalid"
        ]
    );
    assert_eq!(calls[0][1], b"-o Existing=yes -p 2222");
    assert_eq!(calls[1][1], b"-o Existing=yes -p 2201");
    assert_eq!(calls[2][1], b"-o Existing=yes");
    assert_eq!(calls[1].len(), 5);
    assert_eq!(calls[1][4], b"/store/c1#one-old");
    assert_eq!(calls[3][5], b"/store/c1#two-old-rollback");
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(
        text.contains("missing")
            && text.contains("not found")
            && text.contains("abcdef01")
            && text.contains("no activation"),
        "{text}"
    );
    assert!(
        text.contains("[c1#one] [1/2]") && text.contains("[direct] [2/2]"),
        "{text}"
    );
    assert!(String::from_utf8_lossy(&out.stderr).contains("raw copy log"));
    f.unchanged();
    assert!(!f.root.join("kernels.json").exists());
}
#[test]
fn decline_unsupported_empty_and_nonpositive_admission_have_no_mutating_effects() {
    for args in [
        vec![],
        vec!["--generation=current"],
        vec!["--copy-only"],
        vec!["--copy-only", "--generation=current", "absent*", "--yes"],
        vec![
            "--copy-only",
            "--generation=current",
            "--max-concurrent-copy=0",
            "--yes",
        ],
        vec!["--copy-only", "--generation=current"],
    ] {
        let f = Fixture::new();
        let out = f.run(&args, "refresh", b"n\n");
        assert_ne!(out.status.code(), Some(0), "{args:?}: {out:?}");
        assert!(f.calls().is_empty());
        assert!(!f.root.join("kernels.json").exists());
        assert!(!f.root.join(".confctl/mutation.lock").exists());
        f.unchanged();
    }
}
#[test]
fn per_host_numeric_spelling_and_interactive_yes_still_requires_each_confirmation() {
    let f = Fixture::new();
    let out = f.run(
        &["--copy-only", "--generation=+1", "--yes", "direct"],
        "",
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(f.calls()[0][4], b"/store/direct-+1");
    f.unchanged();
    let f = Fixture::new();
    let out = f.run(
        &[
            "--copy-only",
            "--generation=0001",
            "--yes",
            "--interactive",
            "direct",
        ],
        "",
        b"y\nn\n",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(f.calls().is_empty());
    assert!(
        String::from_utf8_lossy(&out.stdout)
            .matches("Continue?")
            .count()
            >= 2
    );
    f.unchanged();
    let f = Fixture::new();
    let out = f.run(
        &[
            "--copy-only",
            "--generation=99999999999999999999999999",
            "--yes",
            "direct",
        ],
        "",
        b"",
    );
    assert_eq!(out.status.code(), Some(1));
    assert!(f.calls().is_empty());
}
#[test]
fn bulk_failure_finishes_other_copies_while_serial_failure_stops_later_hosts() {
    for serial in [false, true] {
        let f = Fixture::new();
        let path = f.root.join("machines.json");
        let mut machines: serde_json::Map<String, Value> =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        let direct = machines.shift_remove("direct").unwrap();
        let mut ordered = serde_json::Map::new();
        ordered.insert("direct".into(), direct);
        ordered.extend(machines);
        fs::write(path, serde_json::to_vec(&ordered).unwrap()).unwrap();
        let mut args = vec![
            "--copy-only",
            "--generation=current",
            "--yes",
            "--tag=chosen",
            "--max-concurrent-copy=2",
        ];
        if serial {
            args.push("--one-by-one");
        }
        let out = f.run(&args, "failure", b"");
        assert_eq!(out.status.code(), Some(1), "{out:?}");
        assert_eq!(
            f.calls().len(),
            if serial { 1 } else { 4 },
            "raw call records: {:?}; output: {out:?}",
            f.raw_calls()
        );
        assert!(String::from_utf8_lossy(&out.stderr).contains("retained"));
        f.unchanged();
    }
}
#[test]
fn refresh_precedes_lookup_and_survives_later_missing_generation() {
    let f = Fixture::new();
    let out = f.run(
        &[
            "--copy-only",
            "--generation=absent",
            "--yes",
            "--tag=chosen",
        ],
        "refresh",
        b"",
    );
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert!(f.calls().is_empty());
    let value: Value =
        serde_json::from_slice(&fs::read(f.root.join("kernels.json")).unwrap()).unwrap();
    assert_eq!(
        value,
        json!({"direct":"6.12.3-test","local":"6.12.3-local"})
    );
    let sequence = fs::read_to_string(f.root.join("sequence")).unwrap();
    assert_eq!(
        sequence
            .lines()
            .filter(|line| line.starts_with("probe "))
            .count(),
        1
    );
    assert!(sequence.contains("not-for-copy"));
    f.unchanged();
}
#[test]
fn cancellation_joins_active_copy_and_does_not_admit_later_hosts() {
    let f = Fixture::new();
    let mut child = f
        .command(
            &[
                "--copy-only",
                "--generation=current",
                "--yes",
                "--tag=chosen",
                "--max-concurrent-copy=1",
            ],
            "slow",
        )
        .spawn()
        .unwrap();
    drop(child.stdin.take());
    let until = Instant::now() + Duration::from_secs(5);
    let pid = loop {
        if let Ok(contents) = fs::read_to_string(f.root.join("pid"))
            && let Ok(pid) = contents.trim().parse::<u32>()
            && pid > 0
        {
            break pid;
        }
        assert!(Instant::now() < until);
        thread::sleep(Duration::from_millis(5));
    };
    rustix::process::kill_process(
        rustix::process::Pid::from_raw(child.id() as i32).unwrap(),
        rustix::process::Signal::INT,
    )
    .unwrap();
    let out = wait(child);
    assert_eq!(out.status.code(), Some(130), "{out:?}");
    assert_eq!(f.calls().len(), 1);
    assert!(!Path::new("/proc").join(pid.to_string()).exists());
    f.unchanged();
}

#[test]
fn explicit_null_target_fails_without_native_attempt_but_carried_null_uses_carrier() {
    for skip in [false, true] {
        let f = Fixture::new();
        let path = f.root.join("machines.json");
        let mut machines: serde_json::Map<String, Value> =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        machines.insert("null".into(), json!({"name":"null","metaConfig":{"managed":true,"spin":"nixos","host":{"target":null}}}));
        fs::write(path, serde_json::to_vec(&machines).unwrap()).unwrap();
        let dir = f.root.join(".confctl/generations/null/old");
        fs::create_dir_all(&dir).unwrap();
        let bytes = fs::read(
            f.root
                .join(".confctl/generations/direct/old/generation.json"),
        )
        .unwrap();
        fs::write(dir.join("generation.json"), &bytes).unwrap();
        let mut args = vec!["--copy-only", "--generation=old", "--yes", "null"];
        if skip {
            args.insert(0, "--interactive");
        }
        let out = f.run(&args, "", if skip { b"y\nn\n" } else { b"" });
        assert_eq!(out.status.code(), Some(if skip { 0 } else { 1 }), "{out:?}");
        assert!(f.calls().is_empty());
        assert!(String::from_utf8_lossy(&out.stdout).contains("null"));
        if !skip {
            assert!(
                String::from_utf8_lossy(&out.stderr).contains("null has no target host"),
                "{out:?}"
            );
        }
        assert_eq!(fs::read(dir.join("generation.json")).unwrap(), bytes);
        f.unchanged();
    }
    let f = Fixture::new();
    let out = f.run(
        &["--copy-only", "--generation=current", "--yes", "c1#one"],
        "carrier-failure",
        b"",
    );
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert_eq!(
        f.calls(),
        vec![vec![
            b"copy".to_vec(),
            b"-o Existing=yes -p 2201".to_vec(),
            b"--to".to_vec(),
            b"root@carrier-one.invalid".to_vec(),
            b"/store/c1#one-old".to_vec()
        ]]
    );
    assert!(String::from_utf8_lossy(&out.stderr).contains("c1#one"));
    f.unchanged();
}

#[test]
fn missing_and_interactive_resolved_prompts_precede_all_copy_effects() {
    let f = Fixture::new();
    let out = f.run(
        &["--copy-only", "--generation=current", "--tag=chosen"],
        "",
        b"y\nn\n",
    );
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert!(f.calls().is_empty());
    let stdout = String::from_utf8_lossy(&out.stdout);
    assert!(
        stdout.contains("missing")
            && stdout.contains("ignored")
            && stdout.matches("Continue?").count() >= 2,
        "{stdout}"
    );
    f.unchanged();
    let f = Fixture::new();
    let out = f.run(
        &[
            "--copy-only",
            "--generation=current",
            "--tag=chosen",
            "--interactive",
            "--yes",
        ],
        "",
        b"y\nn\n",
    );
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert!(f.calls().is_empty());
    f.unchanged();
}
