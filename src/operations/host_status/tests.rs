use super::*;
use serde_json::json;
use std::{collections::VecDeque, os::unix::process::ExitStatusExt, sync::Mutex, time::Instant};

#[test]
fn profile_records_keep_numeric_id_order_current_and_invalid_link_diagnostics() {
    let profile = "/nix/var/nix/profiles/system";
    let text = format!(
        "/nix/store/new\n{profile}-10-link;/nix/store/new;1700000000;/nix/store/abc-linux-6.12.3/bzImage\n{profile}-2-link;/nix/store/old;1600000000;\n{profile}-99999999999999999999999999-link;/nix/store/huge;1700000000;\ninvalid-link;/nix/store/ignored;0;\n"
    );
    let (generations, diagnostics) = decode_profile(profile, &text).unwrap();
    assert_eq!(
        generations
            .iter()
            .map(|g| g.id.as_str())
            .collect::<Vec<_>>(),
        ["2", "10", "99999999999999999999999999"]
    );
    assert_eq!(generations[1].date.timestamp(), 1700000000);
    assert!(generations[1].current);
    assert!(!generations[0].current);
    assert_eq!(generations[1].kernel.as_deref(), Some("6.12.3"));
    assert_eq!(
        diagnostics,
        ["Invalid profile generation link 'invalid-link'"]
    );
    assert!(
        decode_profile(profile, "/nix/store/current\n")
            .unwrap()
            .0
            .is_empty()
    );
    assert!(
        decode_profile(
            profile,
            &format!("/nix/store/current\n{profile}-1-link;/saved;99999999999999999999999999;\n")
        )
        .is_err()
    );
}

#[test]
fn carried_routing_uses_exact_inventory_carrier_and_safe_alias() {
    let inventory = Inventory::from_value(json!({
        "carrier": {"name":"carrier", "carrier":"other", "metaConfig":{"host":{"target":"carrier.invalid", "port":2222}}},
        "other": {"name":"other", "metaConfig":{"host":{"target":"other.invalid"}}},
        "guest": {"name":"carrier#guest", "carrier":"carrier", "clusterName":"nested/guest", "alias":"alias/guest 'quoted'", "metaConfig":{}}
    })).unwrap();
    let (target, profile) = routing(&inventory, &inventory.machines[2]).unwrap();
    assert_eq!(target.name, "carrier");
    assert_eq!(
        profile,
        "/nix/var/nix/profiles/confctl-alias:guest 'quoted'"
    );
    let mut missing = inventory.machines[2].clone();
    missing.carrier = Some("absent".into());
    assert!(routing(&inventory, &missing).is_err());
    missing.extra.as_object_mut().unwrap().remove("alias");
    missing.carrier = Some("carrier".into());
    assert_eq!(
        routing(&inventory, &missing).unwrap().1,
        "/nix/var/nix/profiles/confctl-nested:guest"
    );
}

struct Runner(Mutex<VecDeque<ProcessOutcome>>);
impl ProcessRunner for Runner {
    async fn run(&self, _: ProcessSpec, _: ProcessScope) -> Result<ProcessOutcome, ConfctlError> {
        Ok(self.0.lock().unwrap().pop_front().unwrap())
    }
}

struct RecordingRunner {
    outcomes: Mutex<VecDeque<ProcessOutcome>>,
    specs: Mutex<Vec<ProcessSpec>>,
}

#[tokio::test]
async fn uptime_only_reads_one_native_phase_without_current_or_profile_observations() {
    let inventory = Inventory::from_value(
        json!({"host":{"name":"host","metaConfig":{"host":{"target":"localhost"}}}}),
    )
    .unwrap();
    for code in [0, 255] {
        let runner = RecordingRunner {
            outcomes: Mutex::new(vec![outcome(code, b"1.5 0\n")].into()),
            specs: Mutex::new(Vec::new()),
        };
        let status = uptime_only(
            &inventory,
            &inventory.machines[0],
            &runner,
            &CancellationToken::new(),
            TaskId(9),
            None,
        )
        .await
        .unwrap();
        assert_eq!(status.uptime, if code == 0 { Some(1.5) } else { None });
        assert_eq!(status.failure.is_some(), code != 0);
        assert!(status.current_toplevel.is_none() && status.generations.is_none());
        let calls = runner.specs.lock().unwrap();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0].program, PathBuf::from("cat"));
        assert_eq!(calls[0].args, ["/proc/uptime"]);
    }
}
impl ProcessRunner for RecordingRunner {
    async fn run(
        &self,
        spec: ProcessSpec,
        _: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        self.specs.lock().unwrap().push(spec);
        Ok(self
            .outcomes
            .lock()
            .unwrap()
            .pop_front()
            .expect("only expected host queries"))
    }
}

#[tokio::test]
async fn query_order_optional_current_and_partial_unknowns_are_explicit() {
    let inventory = Inventory::from_value(
        json!({"host":{"name":"host","metaConfig":{"host":{"target":"localhost"}}}}),
    )
    .unwrap();
    for (read_current, outcomes, expected, failure) in [
        (
            true,
            vec![
                outcome(0, b"123.5 0\n"),
                outcome(0, b"/current\n"),
                outcome(0, b"/current\n"),
            ],
            vec!["cat", "realpath", "bash"],
            None,
        ),
        (
            false,
            vec![outcome(0, b"123.5 0\n"), outcome(0, b"/current\n")],
            vec!["cat", "bash"],
            None,
        ),
        (
            true,
            vec![outcome(255, b"partial")],
            vec!["cat"],
            Some("uptime"),
        ),
        (
            true,
            vec![outcome(0, b"123.5 0\n"), outcome(1, b"partial")],
            vec!["cat", "realpath"],
            Some("current toplevel"),
        ),
        (
            true,
            vec![
                outcome(0, b"123.5 0\n"),
                outcome(0, b"/current\n"),
                outcome(1, b"partial"),
            ],
            vec!["cat", "realpath", "bash"],
            Some("profile generations"),
        ),
    ] {
        let runner = RecordingRunner {
            outcomes: Mutex::new(outcomes.into()),
            specs: Mutex::new(Vec::new()),
        };
        let status = query(
            &inventory,
            &inventory.machines[0],
            &runner,
            &CancellationToken::new(),
            TaskId(7),
            None,
            read_current,
        )
        .await
        .unwrap();
        let specs = runner.specs.lock().unwrap();
        assert_eq!(
            specs
                .iter()
                .map(|s| s.program.to_str().unwrap())
                .collect::<Vec<_>>(),
            expected
        );
        assert_eq!(status.failure.as_ref().map(|f| f.phase), failure);
        assert_eq!(
            status.uptime,
            if failure == Some("uptime") {
                None
            } else {
                Some(123.5)
            }
        );
        assert_eq!(
            status.current_toplevel.is_some(),
            read_current && !matches!(failure, Some("uptime" | "current toplevel"))
        );
        if failure.is_some() {
            assert!(status.generations.is_none());
        } else {
            assert!(status.generations.unwrap().is_empty());
        }
        if let Some(spec) = specs
            .iter()
            .find(|s| s.program == std::path::Path::new("bash"))
        {
            assert_eq!(spec.args, ["--norc"]);
            let StdinPolicy::Bytes(bytes) = &spec.stdin else {
                panic!("profile script must use native stdin bytes")
            };
            assert!(String::from_utf8_lossy(bytes).contains("realpath -e --"));
        }
        if let Some(spec) = specs
            .iter()
            .find(|s| s.program == std::path::Path::new("realpath"))
        {
            assert_eq!(spec.args, ["-e", "--", "/run/current-system"]);
        }
    }
}

#[tokio::test]
async fn current_path_missing_or_dangling_keeps_native_failure_and_successful_uptime() {
    use std::os::unix::fs::symlink;
    let scratch = tempfile::tempdir().unwrap();
    let missing = scratch.path().join("missing");
    let dangling = scratch.path().join("dangling");
    symlink(&missing, &dangling).unwrap();
    let inventory = Inventory::from_value(
        json!({"host":{"name":"host","metaConfig":{"host":{"target":"localhost"}}}}),
    )
    .unwrap();
    for path in [&missing, &dangling] {
        let native = std::process::Command::new("realpath")
            .args(["-e", "--"])
            .arg(path)
            .output()
            .unwrap();
        assert!(!native.status.success());
        let code = native.status.code();
        let stderr = native.stderr.clone();
        let mut failed = outcome(0, &native.stdout);
        failed.status = native.status;
        failed.stderr = Captured::Memory(native.stderr);
        let runner = RecordingRunner {
            outcomes: Mutex::new(VecDeque::from([outcome(0, b"123.5 0\n"), failed])),
            specs: Mutex::new(Vec::new()),
        };
        let status = query(
            &inventory,
            &inventory.machines[0],
            &runner,
            &CancellationToken::new(),
            TaskId(1),
            None,
            true,
        )
        .await
        .unwrap();
        assert_eq!(status.uptime, Some(123.5));
        assert!(status.current_toplevel.is_none() && status.generations.is_none());
        let failure = status.failure.unwrap();
        assert_eq!(failure.phase, "current toplevel");
        assert_eq!(failure.outcome.code(), code);
        assert_eq!(failure.outcome.stderr.memory().unwrap(), stderr);
        let specs = runner.specs.lock().unwrap();
        assert_eq!(
            specs.len(),
            2,
            "failed current resolution stops before profile read"
        );
        assert_eq!(specs[1].args, ["-e", "--", "/run/current-system"]);
    }
}

#[tokio::test]
async fn empty_successful_current_response_is_a_decode_error_and_keeps_capture() {
    let inventory = Inventory::from_value(
        json!({"host":{"name":"host","metaConfig":{"host":{"target":"localhost"}}}}),
    )
    .unwrap();
    for empty in [b"".as_slice(), b" \t\r\n\0"] {
        let runner = RecordingRunner {
            outcomes: Mutex::new(VecDeque::from([
                outcome(0, b"123.5 0\n"),
                outcome(0, empty),
            ])),
            specs: Mutex::new(Vec::new()),
        };
        let error = query(
            &inventory,
            &inventory.machines[0],
            &runner,
            &CancellationToken::new(),
            TaskId(1),
            None,
            true,
        )
        .await
        .unwrap_err();
        let ConfctlError::ProcessFailure { source, evidence } = error else {
            panic!("empty realpath response must be a decode infrastructure failure")
        };
        assert!(
            source
                .to_string()
                .contains("current toplevel response has no path")
        );
        assert_eq!(evidence.status.unwrap().code(), Some(0));
        assert_eq!(evidence.stdout.memory().unwrap(), empty);
        assert_eq!(runner.specs.lock().unwrap().len(), 2);
    }
}

#[tokio::test]
async fn carried_current_path_uses_strict_native_arguments_and_keeps_profile_failure_partial() {
    let inventory = Inventory::from_value(json!({
        "carrier":{"name":"carrier","metaConfig":{"host":{"target":"localhost"}}},
        "guest":{"name":"carrier#guest","carrier":"carrier","alias":"nested/guest 'quoted'","metaConfig":{}}
    })).unwrap();
    let runner = RecordingRunner {
        outcomes: Mutex::new(VecDeque::from([
            outcome(0, b"123.5 0\n"),
            outcome(0, b" /saved/current\n"),
            outcome(1, b"partial profile"),
        ])),
        specs: Mutex::new(Vec::new()),
    };
    let status = query(
        &inventory,
        &inventory.machines[1],
        &runner,
        &CancellationToken::new(),
        TaskId(1),
        None,
        true,
    )
    .await
    .unwrap();
    assert_eq!(
        status.current_toplevel.as_deref(),
        Some(std::path::Path::new("/saved/current"))
    );
    assert_eq!(status.uptime, Some(123.5));
    assert!(status.generations.is_none());
    assert_eq!(status.failure.unwrap().phase, "profile generations");
    let specs = runner.specs.lock().unwrap();
    assert_eq!(
        specs[1].args,
        [
            "-e",
            "--",
            "/nix/var/nix/profiles/confctl-nested:guest 'quoted'"
        ]
    );
}

#[test]
fn script_distinguishes_missing_dangling_empty_and_profile_read_failures() {
    use std::{
        io::Write,
        os::unix::fs::symlink,
        process::{Command, Stdio},
    };
    let temp = tempfile::tempdir().unwrap();
    let profile = temp.path().join("profile with 'quote'\nand space");
    let run = || {
        let mut command = Command::new("bash")
            .arg("--norc")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        command
            .stdin
            .take()
            .unwrap()
            .write_all(&profile_script(profile.to_str().unwrap()))
            .unwrap();
        command.wait_with_output().unwrap()
    };
    assert!(!run().status.success());
    symlink(temp.path().join("absent"), &profile).unwrap();
    assert!(!run().status.success());
    std::fs::remove_file(&profile).unwrap();
    let system = temp.path().join("system");
    std::fs::create_dir(&system).unwrap();
    symlink(&system, &profile).unwrap();
    let output = run();
    assert!(output.status.success());
    assert!(
        decode_profile(
            profile.to_str().unwrap(),
            std::str::from_utf8(&output.stdout).unwrap()
        )
        .unwrap()
        .0
        .is_empty()
    );
    let link = PathBuf::from(format!("{}-2-link", profile.display()));
    symlink(&system, &link).unwrap();
    symlink(
        "/nix/store/hash-linux-6.12.3/bzImage",
        system.join("bzImage"),
    )
    .unwrap();
    let output = run();
    assert!(output.status.success());
    let (generations, _) = decode_profile(
        profile.to_str().unwrap(),
        std::str::from_utf8(&output.stdout).unwrap(),
    )
    .unwrap();
    // Newlines in a profile path preserve shell bytes, but cannot be represented
    // by the existing newline/semicolon observation format.
    assert!(generations.is_empty());
    std::fs::remove_file(&link).unwrap();
    std::fs::write(&link, "regular file is an invalid generation link").unwrap();
    assert!(!run().status.success());
    assert!(decode_profile("/profile", "").is_err());
}
fn outcome(code: i32, stdout: &[u8]) -> ProcessOutcome {
    ProcessOutcome {
        status: std::process::ExitStatus::from_raw(code << 8),
        stdout: Captured::Memory(stdout.into()),
        stderr: Captured::Memory(b"native failure\n".to_vec()),
        stdout_complete: true,
        stderr_complete: true,
        started: Instant::now(),
        ended: Instant::now(),
        termination: TerminationCause::Exited,
    }
}

#[tokio::test]
async fn failed_observation_keeps_phase_and_native_outcome_instead_of_empty_success() {
    let runner = Runner(Mutex::new(VecDeque::from([outcome(255, b"partial")])));
    let result = observe(
        &runner,
        ProcessSpec::new("fixture", "fixture"),
        &CancellationToken::new(),
        TaskId(1),
        "profile generations",
        |text| decode_profile("/profile", text),
    )
    .await
    .unwrap();
    let failure = result.unwrap_err();
    assert_eq!(failure.phase, "profile generations");
    assert_eq!(failure.outcome.code(), Some(255));
    assert_eq!(failure.outcome.stdout.memory().unwrap(), b"partial");
    assert!(failure.diagnostic("host").contains("native failure"));
}

#[tokio::test]
async fn decode_incomplete_capture_and_cancellation_keep_process_evidence() {
    for mut native in [outcome(0, b"\xff"), outcome(0, b"valid")] {
        if native.stdout.memory() == Some(b"valid") {
            native.stdout_complete = false;
        }
        let runner = Runner(Mutex::new(VecDeque::from([native])));
        assert!(matches!(
            observe(
                &runner,
                ProcessSpec::new("fixture", "fixture"),
                &CancellationToken::new(),
                TaskId(1),
                "uptime",
                |_| Ok(())
            )
            .await,
            Err(ConfctlError::ProcessFailure { .. })
        ));
    }
    let mut native = outcome(0, b"complete");
    native.termination = TerminationCause::Canceled;
    let runner = Runner(Mutex::new(VecDeque::from([native])));
    let error = observe(
        &runner,
        ProcessSpec::new("fixture", "fixture"),
        &CancellationToken::new(),
        TaskId(1),
        "uptime",
        |_| Ok(()),
    )
    .await
    .unwrap_err();
    assert!(error.is_cancelled());
}
