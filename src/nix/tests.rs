use super::*;
use std::{collections::VecDeque, os::unix::process::ExitStatusExt, sync::Arc, time::Instant};

type ChildReply = (i32, Vec<u8>, Vec<u8>);
#[derive(Clone)]
struct Fake {
    calls: Arc<Mutex<Vec<ProcessSpec>>>,
    results: Arc<Mutex<VecDeque<ChildReply>>>,
}
impl ProcessRunner for Fake {
    async fn run(
        &self,
        spec: ProcessSpec,
        _: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        self.calls.lock().unwrap().push(spec);
        // Concurrent readers must not both pass an uninitialized settings cache.
        tokio::task::yield_now().await;
        let (code, out, err) = self
            .results
            .lock()
            .unwrap()
            .pop_front()
            .expect("undeclared child call");
        Ok(ProcessOutcome {
            status: std::process::ExitStatus::from_raw(code << 8),
            stdout: Captured::Memory(out),
            stderr: Captured::Memory(err),
            stdout_complete: true,
            stderr_complete: true,
            started: Instant::now(),
            ended: Instant::now(),
            termination: TerminationCause::Exited,
        })
    }
}
struct Fixture {
    _scratch: tempfile::TempDir,
    root: ConfigRoot,
    runner: Fake,
    defaults: InvocationSettings<Fake>,
}
impl Fixture {
    fn new(results: Vec<(i32, &str, &str)>) -> Self {
        let scratch = tempfile::tempdir().unwrap();
        std::fs::write(scratch.path().join("flake.nix"), "{}").unwrap();
        let root = ConfigRoot::new(scratch.path()).unwrap();
        let runner = Fake {
            calls: Arc::new(Mutex::new(vec![])),
            results: Arc::new(Mutex::new(
                results
                    .into_iter()
                    .map(|(code, out, err)| {
                        (code, out.as_bytes().to_vec(), err.as_bytes().to_vec())
                    })
                    .collect(),
            )),
        };
        let defaults =
            InvocationSettings::new(root.clone(), runner.clone(), CancellationToken::new());
        Self {
            _scratch: scratch,
            root,
            runner,
            defaults,
        }
    }
    async fn configured(&self, options: NixOptions, epoch: ConfigEpoch) -> NixClient<Fake> {
        NixClient::configured(
            self.root.clone(),
            self.runner.clone(),
            options,
            CancellationToken::new(),
            &self.defaults,
            epoch,
        )
        .await
        .unwrap()
    }
    fn calls(&self) -> Vec<Vec<String>> {
        self.runner
            .calls
            .lock()
            .unwrap()
            .iter()
            .map(|call| {
                assert_eq!(call.cwd.as_deref(), Some(self.root.path()));
                call.args
                    .iter()
                    .map(|s| s.to_str().unwrap().to_owned())
                    .collect()
            })
            .collect()
    }
}

#[tokio::test]
async fn pure_bootstrap_and_exact_nix_fallback_sequence() {
    let fixture = Fixture::new(vec![
        (0, r#"{"nix":{"maxJobs":2}}"#, ""),
        (0, r#"{"nix":{"maxJobs":99,"impureEval":true}}"#, ""),
        (1, "", "unknown flag --no-update-lock-file"),
        (1, "", "experimental feature nix-command disabled"),
        (0, "[1]", ""),
    ]);
    assert!(fixture.calls().is_empty());
    let client = fixture
        .configured(NixOptions::default(), ConfigEpoch(0))
        .await;
    assert_eq!(
        client.eval_json(".#confctl.machineNames").await.unwrap(),
        serde_json::json!([1])
    );
    let calls = fixture.calls();
    assert_eq!(calls.len(), 5);
    assert_eq!(
        calls[0],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--option",
            "max-jobs",
            "auto",
            ".#confctl.settings"
        ]
    );
    assert_eq!(
        calls[1],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--option",
            "max-jobs",
            "2",
            ".#confctl.settings"
        ]
    );
    assert_eq!(
        calls[2],
        [
            "eval",
            "--json",
            "--impure",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--option",
            "max-jobs",
            "2",
            ".#confctl.machineNames"
        ]
    );
    assert!(!calls[3].iter().any(|arg| arg == "--no-update-lock-file"));
    assert_eq!(
        &calls[4][2..6],
        [
            "--extra-experimental-features",
            "nix-command",
            "--extra-experimental-features",
            "flakes"
        ]
    );
}

#[tokio::test]
async fn ordinary_failure_is_not_retried_and_retains_capture() {
    let fixture = Fixture::new(vec![
        (0, "{}", ""),
        (0, "{}", ""),
        (7, "prefix", "real evaluator error"),
    ]);
    let client = fixture
        .configured(NixOptions::default(), ConfigEpoch(0))
        .await;
    let error = client
        .eval_json(".#confctl.machineNames")
        .await
        .unwrap_err();
    let ConfctlError::NixCommandFailure(failure) = error else {
        panic!()
    };
    let ConfctlError::ProcessFailure { source, evidence } = *failure.source else {
        panic!()
    };
    assert!(matches!(*source, ConfctlError::Child { code: Some(7), .. }));
    assert_eq!(evidence.stdout.memory(), Some(b"prefix".as_slice()));
    assert_eq!(fixture.calls().len(), 3);
}

#[tokio::test]
async fn audited_command_display_keeps_options_and_local_targets_but_redacts_expressions_urls_and_legacy_values()
 {
    let fixture = Fixture::new(vec![(0, "{}", ""), (0, "[]", ""), (0, "[]", "")]);
    let client = fixture
        .configured(
            NixOptions {
                max_jobs: Some("2".into()),
                show_trace: true,
                cores: Some("3".into()),
            },
            ConfigEpoch(0),
        )
        .await;
    client
        .eval_apply(
            "https://user:url-secret@example.invalid/source",
            Some("x: expression-secret"),
        )
        .await
        .unwrap();
    client
        .build_json_for_task(
            &[
                ".#confctl.build.alpha.toplevel".into(),
                "git+ssh://url-secret@example.invalid/source".into(),
            ],
            &["-I".into(), "private=legacy-secret".into()],
            TaskId(4),
            false,
        )
        .await
        .unwrap();
    let calls = fixture.runner.calls.lock().unwrap();
    assert_eq!(calls.len(), 3);
    assert!(calls[1].args.iter().any(|a| a == "x: expression-secret"));
    assert!(
        calls[1]
            .args
            .iter()
            .any(|a| a == "https://user:url-secret@example.invalid/source")
    );
    assert!(calls[2].args.iter().any(|a| a == "private=legacy-secret"));
    for call in calls.iter() {
        let display = call.display_command.as_deref().unwrap();
        assert!(display.starts_with("nix "));
        for secret in ["expression-secret", "url-secret", "legacy-secret"] {
            assert!(!display.contains(secret), "{display}");
        }
        assert!(
            display.contains("--show-trace")
                && display.contains("--option max-jobs 2")
                && display.contains("--option cores 3")
        );
    }
    assert!(
        calls[0]
            .display_command
            .as_ref()
            .unwrap()
            .contains(".#confctl.settings")
    );
    assert!(
        calls[1]
            .display_command
            .as_ref()
            .unwrap()
            .contains("--apply '<expression omitted>' '<target omitted>'")
    );
    let build = calls[2].display_command.as_ref().unwrap();
    assert!(
        build.contains("build --json --no-link")
            && build.contains(".#confctl.build.alpha.toplevel")
            && build.contains("<legacy input arguments omitted>")
    );
}

#[tokio::test]
async fn invalidation_preserves_invocation_defaults_and_jobs_and_rejects_prior_views() {
    let fixture = Fixture::new(vec![
        (
            0,
            r#"{"nix":{"maxJobs":4},"list":{"columns":["name"]}}"#,
            "",
        ),
        (0, r#"{"nix":{"maxJobs":9}}"#, ""),
        (0, "[1]", ""),
        (0, r#"{"nix":{"maxJobs":20,"impureEval":true}}"#, ""),
        (0, "[2]", ""),
        (0, r#"{"nix":{"maxJobs":30}}"#, ""),
    ]);
    let mut client = fixture
        .configured(NixOptions::default(), ConfigEpoch(7))
        .await;
    let old = client.settings().await.unwrap();
    client.cached_json(".#confctl.machineNames").await.unwrap();
    client.cached_json(".#confctl.machineNames").await.unwrap();
    assert_eq!(fixture.calls().len(), 3);
    let epoch = client.invalidate_configuration();
    assert_eq!(epoch, ConfigEpoch(8));
    assert!(old.require_epoch(epoch).is_err());
    assert_eq!(
        client
            .cached_json(".#confctl.machineNames")
            .await
            .unwrap()
            .value,
        serde_json::json!([2])
    );
    let rebuilt = fixture.configured(NixOptions::default(), epoch).await;
    let current = rebuilt.settings().await.unwrap();
    assert_eq!(rebuilt.epoch(), epoch);
    assert!(old.require_epoch(rebuilt.epoch()).is_err());
    assert_eq!(
        current
            .require_epoch(epoch)
            .unwrap()
            .nix
            .max_jobs
            .as_deref(),
        Some("30")
    );
    let defaults = fixture.defaults.read().await.unwrap();
    assert_eq!(defaults.list_columns, ["name"]);
    assert_eq!(defaults.nix.max_jobs.as_deref(), Some("4"));
    let calls = fixture.calls();
    assert_eq!(calls.len(), 6);
    for call in &calls[1..] {
        assert!(
            call.windows(3)
                .any(|words| words == ["--option", "max-jobs", "4"])
        );
    }
    assert!(calls[4].iter().any(|word| word == "--impure"));
    assert!(!calls[3].iter().any(|word| word == "--impure"));
}

#[tokio::test]
async fn explicit_jobs_bypass_defaults_including_empty_string() {
    for jobs in ["2", ""] {
        let fixture = Fixture::new(vec![
            (0, r#"{"nix":{"maxJobs":99,"impureEval":true}}"#, ""),
            (0, "[]", ""),
            (0, r#"{"nix":{"maxJobs":6}}"#, ""),
            (0, "{}", ""),
        ]);
        let client = fixture
            .configured(
                NixOptions {
                    max_jobs: Some(jobs.into()),
                    show_trace: true,
                    cores: Some("3".into()),
                },
                ConfigEpoch(0),
            )
            .await;
        assert!(fixture.calls().is_empty());
        client.eval_json(".#confctl.machineNames").await.unwrap();
        assert_eq!(
            fixture.calls()[0],
            [
                "eval",
                "--json",
                "--no-write-lock-file",
                "--no-update-lock-file",
                "--show-trace",
                "--option",
                "max-jobs",
                jobs,
                "--option",
                "cores",
                "3",
                ".#confctl.settings"
            ]
        );
        assert_eq!(
            fixture
                .defaults
                .read()
                .await
                .unwrap()
                .nix
                .max_jobs
                .as_deref(),
            Some("6")
        );
        let default_client = fixture
            .configured(NixOptions::default(), ConfigEpoch(0))
            .await;
        default_client.settings().await.unwrap();
        let calls = fixture.calls();
        assert_eq!(calls.len(), 4);
        assert_eq!(
            &calls[2][4..],
            ["--option", "max-jobs", "auto", ".#confctl.settings"]
        );
        assert_eq!(
            &calls[3][4..],
            ["--option", "max-jobs", "6", ".#confctl.settings"]
        );
    }
}

#[tokio::test]
async fn null_default_jobs_remain_absent_even_when_client_settings_change_them() {
    let fixture = Fixture::new(vec![
        (0, r#"{"nix":{"maxJobs":null}}"#, ""),
        (0, r#"{"nix":{"maxJobs":8}}"#, ""),
        (0, "[]", ""),
    ]);
    let client = fixture
        .configured(NixOptions::default(), ConfigEpoch(0))
        .await;
    client.eval_json(".#confctl.machineNames").await.unwrap();
    let calls = fixture.calls();
    assert!(calls[0].iter().any(|word| word == "max-jobs"));
    assert_eq!(
        calls[1],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            ".#confctl.settings"
        ]
    );
    assert_eq!(
        calls[2],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            ".#confctl.machineNames"
        ]
    );
}

#[tokio::test]
async fn concurrent_initialization_is_serialized_per_snapshot_not_between_clients() {
    let fixture = Fixture::new(vec![(0, "{}", ""), (0, "{}", ""), (0, "{}", "")]);
    let (left, right) = tokio::join!(fixture.defaults.read(), fixture.defaults.read());
    left.unwrap();
    right.unwrap();
    assert_eq!(fixture.calls().len(), 1);
    let first = fixture
        .configured(NixOptions::default(), ConfigEpoch(0))
        .await;
    let (left, right) = tokio::join!(first.settings(), first.settings());
    left.unwrap();
    right.unwrap();
    assert_eq!(fixture.calls().len(), 2);
    fixture
        .configured(NixOptions::default(), ConfigEpoch(0))
        .await
        .settings()
        .await
        .unwrap();
    assert_eq!(fixture.calls().len(), 3);
}

#[tokio::test]
async fn failed_bootstrap_or_client_settings_stop_at_the_original_stage_and_can_retry() {
    let fixture = Fixture::new(vec![
        (7, "", "bootstrap failure"),
        (0, "{}", ""),
        (8, "", "client settings failure"),
        (0, "{}", ""),
        (0, "[]", ""),
    ]);
    let failed = NixClient::configured(
        fixture.root.clone(),
        fixture.runner.clone(),
        NixOptions::default(),
        CancellationToken::new(),
        &fixture.defaults,
        ConfigEpoch(0),
    )
    .await;
    assert!(matches!(failed, Err(ConfctlError::NixCommandFailure(_))));
    assert_eq!(fixture.calls().len(), 1);
    let client = fixture
        .configured(NixOptions::default(), ConfigEpoch(0))
        .await;
    assert!(matches!(
        client.eval_json(".#confctl.machineNames").await,
        Err(ConfctlError::NixCommandFailure(_))
    ));
    assert_eq!(fixture.calls().len(), 3);
    assert!(
        fixture
            .calls()
            .iter()
            .all(|call| call.last().unwrap() == ".#confctl.settings")
    );
    client.eval_json(".#confctl.machineNames").await.unwrap();
    assert_eq!(fixture.calls().len(), 5);
}

#[test]
fn apply_keys_escape_without_changing_authoritative_values() {
    assert_eq!(
        selected_apply(&["a\"b".into(), "a\"b".into(), "a\\b".into()]),
        r#"x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ "a\"b" "a\\b" ])"#
    );
    assert_eq!(
        selected_apply(&["${name}".into()]),
        r#"x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ "\${name}" ])"#
    );
}

#[tokio::test]
async fn unsuccessful_settings_decoding_is_not_cached_in_either_snapshot() {
    let fixture = Fixture::new(vec![
        (0, "[]", ""),
        (0, r#"{"nix":{"maxJobs":4}}"#, ""),
        (0, r#"{"nix":{"maxJobs":"invalid"}}"#, ""),
        (0, "{}", ""),
    ]);
    assert!(fixture.defaults.read().await.is_err());
    let client = fixture
        .configured(NixOptions::default(), ConfigEpoch(0))
        .await;
    assert!(client.settings().await.is_err());
    client.settings().await.unwrap();
    fixture.defaults.read().await.unwrap();
    client.settings().await.unwrap();
    assert_eq!(fixture.calls().len(), 4);
}

#[tokio::test]
async fn only_tasked_live_builds_select_structured_logging_and_decoded_compatibility_errors() {
    for live in [false, true] {
        let fixture = Fixture::new(vec![
            (0, "{}", ""),
            (0, "{}", ""),
            (
                1,
                "",
                r#"@nix {"action":"msg","level":0,"msg":"error: unknown flag --no-update-lock-file"}"#,
            ),
            (0, "[]", ""),
            (0, "[]", ""),
        ]);
        let client = fixture
            .configured(NixOptions::default(), ConfigEpoch(0))
            .await;
        client
            .build_json_for_task(&[".#confctl.build.a.toplevel".into()], &[], TaskId(3), live)
            .await
            .unwrap();
        client
            .build_json(&[".#confctl.machinesJson".into()], &[])
            .await
            .unwrap();
        let calls = fixture.calls();
        assert_eq!(calls.len(), 5);
        for call in &calls[2..4] {
            assert_eq!(
                call.windows(2)
                    .any(|pair| pair == ["--log-format", "internal-json"]),
                live
            );
        }
        assert!(!calls[4].iter().any(|arg| arg == "--log-format"));
        assert!(!calls[3].iter().any(|arg| arg == "--no-update-lock-file"));
    }
    let raw = r#"@nix {"action":"msg","msg":"error: builder failed"}
@nix malformed"#;
    let fixture = Fixture::new(vec![(0, "{}", ""), (0, "{}", ""), (7, "prefix", raw)]);
    let client = fixture
        .configured(NixOptions::default(), ConfigEpoch(0))
        .await;
    let error = client
        .build_json_for_task(&[".#confctl.build.a.toplevel".into()], &[], TaskId(4), true)
        .await
        .unwrap_err();
    assert_eq!(
        fixture.calls().len(),
        3,
        "ordinary or malformed protocol failure is never retried"
    );
    let ConfctlError::NixCommandFailure(failure) = error else {
        panic!()
    };
    let ConfctlError::ProcessFailure { evidence, .. } = *failure.source else {
        panic!()
    };
    assert_eq!(evidence.stderr.memory(), Some(raw.as_bytes()));
}
