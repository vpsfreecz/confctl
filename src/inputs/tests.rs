use super::*;
use crate::error::ConfctlError;
use serde_json::{Value, json};
fn before_lock() -> FlakeLock {
    FlakeLock {
        data: json!({"version":7,"root":"entry","nodes":{
            "entry":{"inputs":{"parent":"parent","alias":["parent","leaf"]}},
            "parent":{"locked":{"type":"github","owner":"fixture","repo":"parent","rev":"c".repeat(40)},"inputs":{"leaf":"source"}},
            "source":{"original":{"type":"github","owner":"fixture","repo":"nixpkgs","ref":"main"},"locked":{"type":"github","owner":"fixture","repo":"nixpkgs","rev":"a".repeat(40)}}
        }}),
    }
}
fn expected_set() -> FlakeLock {
    let mut lock = before_lock();
    lock.data["nodes"]["source"]["locked"]["rev"] = json!("b".repeat(40));
    lock
}
#[test]
fn aliases_resolve_complete_paths_and_set_restores_terminal_original() {
    let before = before_lock();
    let info = before.input_info("alias").unwrap();
    assert_eq!(info.rev, Some("a".repeat(40)));
    assert_eq!(
        before.override_url("alias", "next").unwrap(),
        "github:fixture/nixpkgs/next"
    );
    let mut candidate = expected_set();
    candidate.data["nodes"]["source"]["original"] = json!({"ref":"replaced"});
    candidate
        .restore_original("alias", before.original("alias").unwrap().as_ref())
        .unwrap();
    assert_eq!(candidate.data, expected_set().data);
    assert_eq!(
        InputChange::between(Some(&before), &candidate, &["alias".into()])
            .unwrap()
            .len(),
        1
    );
    assert!(
        InputChange::between(Some(&candidate), &candidate, &["alias".into()])
            .unwrap()
            .is_empty()
    );
    assert!(
        before
            .override_url("unknown", "next")
            .unwrap_err()
            .to_string()
            .contains("unknown input")
    );
}
#[test]
fn override_urls_keep_git_query_and_github_dir_without_encoding_revisions() {
    let input = FlakeLock {
        data: json!({"version":7,"root":"root","nodes":{"root":{"inputs":{"git":"git","github":"github"}},"git":{"locked":{},"original":{"type":"git","url":"git+https://example.invalid/repo?shallow=1"}},"github":{"locked":{},"original":{"type":"github","owner":"fixture","repo":"repo","dir":"nested/path"}}}}),
    };
    assert_eq!(
        input.override_url("git", "branch/name").unwrap(),
        "git+https://example.invalid/repo?shallow=1&rev=branch/name"
    );
    assert_eq!(
        input.override_url("github", "branch/name").unwrap(),
        "github:fixture/repo/branch/name?dir=nested/path"
    );
}
#[test]
fn cached_github_warning_remains_failure_even_after_successful_nix_exit() {
    assert!(check_nix_output("", "unrelated warning").is_ok());
    assert!(
        check_nix_output(
            "",
            "warning: '--update-input' is a deprecated alias for 'flake update'\n"
        )
        .is_ok()
    );
    let warning = "warning: unable to download 'https://api.github.com/repos/fixture/repo': rate limit exceeded;\nusing cached version";
    let error = check_nix_output("", warning).unwrap_err().to_string();
    assert!(error.starts_with("Nix could not refresh GitHub input metadata because"));
    let warning = "warning: error: unable to download 'https://api.github.com/repos/example/project/commits/main': HTTP error 503\n; using cached version\n";
    let error = check_nix_output("", warning).unwrap_err().to_string();
    assert!(error.contains("used a cached version"));
    assert!(!error.contains("rate limit"));
    assert!(
        check_nix_output(
            "unable to download 'https://other.invalid/' using cached version",
            ""
        )
        .is_ok()
    );
}

struct Reply {
    complete: bool,
    cancel: bool,
    code: i32,
    stderr: String,
    lock: Option<Value>,
}
struct FakeEditor {
    calls: std::sync::Arc<std::sync::Mutex<Vec<crate::process::ProcessSpec>>>,
    replies: std::sync::Mutex<std::collections::VecDeque<Reply>>,
}
impl crate::process::ProcessRunner for FakeEditor {
    async fn run(
        &self,
        spec: crate::process::ProcessSpec,
        scope: crate::process::ProcessScope,
    ) -> Result<crate::process::ProcessOutcome, crate::error::ConfctlError> {
        use crate::process::{Captured, ProcessOutcome, TerminationCause};
        use std::{os::unix::process::ExitStatusExt, time::Instant};
        let reply = self
            .replies
            .lock()
            .unwrap()
            .pop_front()
            .expect("undeclared input child");
        if let Some(lock) = reply.lock {
            let target = spec
                .args
                .iter()
                .position(|arg| arg == "--output-lock-file")
                .map(|index| std::path::PathBuf::from(&spec.args[index + 1]))
                .unwrap_or_else(|| spec.cwd.as_ref().unwrap().join("flake.lock"));
            std::fs::write(target, serde_json::to_vec_pretty(&lock).unwrap()).unwrap();
        }
        self.calls.lock().unwrap().push(spec);
        if reply.cancel {
            scope.cancellation.cancel();
        }
        Ok(ProcessOutcome {
            status: std::process::ExitStatus::from_raw(reply.code << 8),
            stdout: Captured::Memory(vec![]),
            stderr: Captured::Memory(reply.stderr.into_bytes()),
            stdout_complete: reply.complete,
            stderr_complete: reply.complete,
            started: Instant::now(),
            ended: Instant::now(),
            termination: TerminationCause::Exited,
        })
    }
}
type Calls = std::sync::Arc<std::sync::Mutex<Vec<crate::process::ProcessSpec>>>;
fn editor(replies: Vec<Reply>) -> (tempfile::TempDir, InputEditor<FakeEditor>, Calls) {
    let scratch = tempfile::tempdir().unwrap();
    std::fs::write(scratch.path().join("flake.nix"), "{}\n").unwrap();
    std::fs::write(scratch.path().join("flake.lock"), before_lock().pretty()).unwrap();
    let calls = Calls::default();
    let runner = FakeEditor {
        calls: calls.clone(),
        replies: std::sync::Mutex::new(replies.into()),
    };
    let editor = InputEditor::new(
        crate::configuration::ConfigRoot::new(scratch.path()).unwrap(),
        runner,
        tokio_util::sync::CancellationToken::new(),
    );
    (scratch, editor, calls)
}
fn child_lock() -> Value {
    let mut lock = expected_set().data;
    lock["nodes"]["source"]["original"] = json!({"type":"github","ref":"child-replaced"});
    lock
}
#[tokio::test]
async fn setter_child_argv_fallback_and_original_restoration_match_oracle_effects() {
    let (scratch, editor, calls) = editor(vec![
        Reply {
            code: 1,
            complete: true,
            cancel: false,
            stderr: "experimental feature flakes disabled".into(),
            lock: None,
        },
        Reply {
            code: 0,
            complete: true,
            cancel: false,
            stderr: String::new(),
            lock: Some(child_lock()),
        },
    ]);
    let root = crate::configuration::ConfigRoot::new(scratch.path()).unwrap();
    let guard = crate::configuration::MutationLock::acquire(
        &root,
        &tokio_util::sync::CancellationToken::new(),
    )
    .await
    .unwrap();
    let changes = editor.set(&guard, &["alias".into()], "next").await.unwrap();
    assert_eq!(changes.len(), 1);
    assert_eq!(
        FlakeLock::load(&scratch.path().join("flake.lock"))
            .unwrap()
            .data,
        expected_set().data
    );
    let calls = calls.lock().unwrap();
    assert_eq!(calls.len(), 2);
    let words = calls[0]
        .args
        .iter()
        .map(|arg| arg.to_str().unwrap())
        .collect::<Vec<_>>();
    assert_eq!(
        &words[..8],
        [
            "flake",
            "lock",
            "--update-input",
            "alias",
            "--override-input",
            "alias",
            "github:fixture/nixpkgs/next",
            "--output-lock-file"
        ]
    );
    let name = std::path::Path::new(words[8])
        .file_name()
        .unwrap()
        .to_str()
        .unwrap();
    assert!(name.starts_with(&format!("flake.lock.tmp-{}-", std::process::id())));
    assert_eq!(name.rsplit('-').next().unwrap().len(), 12);
    assert_eq!(
        &calls[1].args[..4],
        [
            "--extra-experimental-features",
            "nix-command",
            "--extra-experimental-features",
            "flakes"
        ]
    );
    assert!(
        calls
            .iter()
            .all(|call| call.cwd.as_deref() == Some(scratch.path()))
    );
    assert_eq!(std::fs::read_dir(scratch.path()).unwrap().count(), 3);
}
#[tokio::test]
async fn failed_setter_child_keeps_previous_lock_and_removes_owned_temporary_output() {
    let (scratch, editor, calls) = editor(vec![Reply {
        code: 23,
        complete: true,
        cancel: false,
        stderr: "actual child failure".into(),
        lock: Some(child_lock()),
    }]);
    let path = scratch.path().join("flake.lock");
    let before = std::fs::read(&path).unwrap();
    let root = crate::configuration::ConfigRoot::new(scratch.path()).unwrap();
    let guard = crate::configuration::MutationLock::acquire(
        &root,
        &tokio_util::sync::CancellationToken::new(),
    )
    .await
    .unwrap();
    let error = editor
        .set(&guard, &["alias".into()], "next")
        .await
        .unwrap_err();
    assert!(matches!(
        error,
        crate::error::ConfctlError::ProcessFailure { .. }
    ));
    assert_eq!(std::fs::read(path).unwrap(), before);
    assert_eq!(calls.lock().unwrap().len(), 1);
    assert_eq!(std::fs::read_dir(scratch.path()).unwrap().count(), 3);
}
#[tokio::test]
async fn updater_cached_metadata_error_preserves_actual_nix_lock_side_effect() {
    let warning = "unable to download 'https://api.github.com/repos/fixture/repo': rate limit exceeded; using cached version";
    let expected = child_lock();
    let (scratch, editor, calls) = editor(vec![Reply {
        code: 0,
        complete: true,
        cancel: false,
        stderr: warning.into(),
        lock: Some(expected.clone()),
    }]);
    let root = crate::configuration::ConfigRoot::new(scratch.path()).unwrap();
    let guard = crate::configuration::MutationLock::acquire(
        &root,
        &tokio_util::sync::CancellationToken::new(),
    )
    .await
    .unwrap();
    let error = editor.update(&guard, &["alias".into()]).await.unwrap_err();
    assert!(error.to_string().contains("rate limit was exceeded"));
    assert_eq!(
        FlakeLock::load(&scratch.path().join("flake.lock"))
            .unwrap()
            .data,
        expected
    );
    assert_eq!(calls.lock().unwrap()[0].args, ["flake", "update", "alias"]);
}

#[tokio::test]
async fn setter_refuses_incomplete_or_postjoin_cancel_before_publication() {
    for cancel in [false, true] {
        let (scratch, editor, calls) = editor(vec![Reply {
            code: 0,
            stderr: String::new(),
            lock: Some(child_lock()),
            complete: cancel,
            cancel,
        }]);
        let root = crate::configuration::ConfigRoot::new(scratch.path()).unwrap();
        let guard = crate::configuration::MutationLock::acquire(
            &root,
            &tokio_util::sync::CancellationToken::new(),
        )
        .await
        .unwrap();
        let before = std::fs::read(scratch.path().join("flake.lock")).unwrap();
        let error = editor
            .set(&guard, &["alias".into()], "next")
            .await
            .unwrap_err();
        assert_eq!(error.is_cancelled(), cancel);
        if !cancel {
            assert!(error.to_string().contains("capture is incomplete"));
        }
        assert_eq!(calls.lock().unwrap().len(), 1);
        assert_eq!(
            std::fs::read(scratch.path().join("flake.lock")).unwrap(),
            before
        );
        assert!(std::fs::read_dir(scratch.path()).unwrap().all(|e| {
            !e.unwrap()
                .file_name()
                .to_string_lossy()
                .starts_with("flake.lock.tmp-")
        }));
    }
}
#[tokio::test]
async fn editor_refuses_foreign_guard_before_native_effects() {
    let (_scratch, editor, calls) = editor(vec![]);
    let other = tempfile::tempdir().unwrap();
    let root = crate::configuration::ConfigRoot::new(other.path()).unwrap();
    let guard = crate::configuration::MutationLock::acquire(
        &root,
        &tokio_util::sync::CancellationToken::new(),
    )
    .await
    .unwrap();
    assert!(
        editor
            .update(&guard, &["alias".into()])
            .await
            .unwrap_err()
            .to_string()
            .contains("different configuration root")
    );
    assert!(calls.lock().unwrap().is_empty());
}

#[test]
fn graph_uses_declared_root_chains_shared_paths_and_rejects_cycles() {
    let mut lock = before_lock();
    lock.data["nodes"]["entry"]["inputs"]["chain"] = json!(["alias"]);
    lock.data["nodes"]["entry"]["inputs"]["other"] = json!("parent");
    let graph = LockGraph::new(&lock).unwrap();
    assert_eq!(
        graph.resolve_path("chain").unwrap().terminal_path,
        "parent/leaf"
    );
    assert_eq!(
        graph
            .update_inputs(&["chain".into(), "alias".into(), "other/leaf".into()])
            .unwrap(),
        ["parent/leaf", "other/leaf"]
    );
    assert!(graph.entries.contains_key("other/leaf"));
    assert_eq!(graph.entries.len(), 6);
    assert!(graph.resolve_path("parent//leaf").is_err());
    for kind in 0..8 {
        let mut bad = lock.clone();
        match kind {
            0 => bad.data["version"] = json!(7.0),
            1 => bad.data["root"] = Value::Null,
            2 => {
                bad.data["nodes"]
                    .as_object_mut()
                    .unwrap()
                    .shift_remove("source");
            }
            3 => bad.data["nodes"]["entry"]["inputs"]["alias"] = json!(["chain"]),
            4 => bad.data["nodes"]["source"]["inputs"] = json!({"cycle":"parent"}),
            5 => bad.data["nodes"]["parent"]["inputs"] = json!([]),
            6 => bad.data["nodes"]["source"]["flake"] = json!("false"),
            _ => bad.data["nodes"]["entry"]["inputs"]["alias"] = json!([]),
        }
        assert!(LockGraph::new(&bad).is_err(), "case {kind}");
    }
    let mut unused = lock;
    unused.data["nodes"]["unused"] = json!({"inputs":{"bad":"absent"}});
    assert!(LockGraph::new(&unused).is_ok());
}

#[tokio::test]
async fn forward_policy_covers_all_paths_and_deduplicates_success_and_failed_evidence() {
    use std::cell::Cell;
    let before = before_lock();
    let mut candidate = before.clone();
    candidate.data["nodes"]["source"]["locked"]["rev"] = json!("b".repeat(40));
    for direction in [Ancestry::Ahead, Ancestry::Behind, Ancestry::Diverged] {
        let calls = Cell::new(0);
        let outcome = validate_forward(&before, &candidate, |proof| {
            calls.set(calls.get() + 1);
            assert_eq!(proof.repo, "nixpkgs");
            std::future::ready(Ok(direction))
        })
        .await;
        assert_eq!(calls.get(), 1);
        if direction == Ancestry::Ahead {
            assert_eq!(
                outcome
                    .unwrap()
                    .iter()
                    .map(|c| c.name.as_str())
                    .collect::<Vec<_>>(),
                ["alias", "parent/leaf"]
            );
        } else {
            assert!(outcome.unwrap_err().policy_rejected());
        }
    }
    let calls = Cell::new(0);
    let error = validate_forward(&before, &candidate, |_| {
        calls.set(calls.get() + 1);
        std::future::ready(Err(crate::error::ConfctlError::Configuration(
            "unavailable evidence".into(),
        )))
    })
    .await
    .unwrap_err();
    assert_eq!(calls.get(), 1);
    assert!(!error.policy_rejected());
    assert!(error.to_string().contains("alias:") && error.to_string().contains("parent/leaf:"));
}

#[tokio::test]
async fn forward_policy_rejects_removed_changed_source_and_equal_sha_attributes() {
    let before = before_lock();
    for kind in 0..6 {
        let mut candidate = before.clone();
        match kind {
            0 => {
                candidate.data["nodes"]["entry"]["inputs"]
                    .as_object_mut()
                    .unwrap()
                    .shift_remove("alias");
            }
            1 => candidate.data["nodes"]["source"]["locked"]["repo"] = json!("other"),
            2 => candidate.data["nodes"]["source"]["locked"]["dir"] = json!("subdir"),
            3 => candidate.data["nodes"]["source"]["locked"]["narHash"] = json!("changed"),
            4 => candidate.data["nodes"]["source"]["locked"]["lastModified"] = json!(123),
            _ => candidate.data["nodes"]["source"]["flake"] = json!(false),
        }
        let error = validate_forward(&before, &candidate, |_| async {
            panic!("unexpected ancestry request")
        })
        .await
        .unwrap_err();
        assert!(error.policy_rejected(), "{kind}: {error}");
    }
    let mut renumbered = before.clone();
    let node = renumbered.data["nodes"]
        .as_object_mut()
        .unwrap()
        .shift_remove("source")
        .unwrap();
    renumbered.data["nodes"]["arbitrary"] = node;
    renumbered.data["nodes"]["parent"]["inputs"]["leaf"] = json!("arbitrary");
    assert!(
        validate_forward(&before, &renumbered, |_| async {
            panic!("unchanged source")
        })
        .await
        .unwrap()
        .is_empty()
    );
}
#[tokio::test]
async fn forward_policy_allows_unchanged_unsupported_but_errors_on_new_or_changed_unsupported() {
    let mut before = before_lock();
    before.data["nodes"]["source"]["locked"] = json!({"type":"path","path":"/fixture"});
    assert!(
        validate_forward(&before, &before, |_| async {
            panic!("unchanged unsupported")
        })
        .await
        .unwrap()
        .is_empty()
    );
    let mut changed = before.clone();
    changed.data["nodes"]["source"]["locked"]["path"] = json!("/changed");
    assert!(
        !validate_forward(&before, &changed, |_| async { panic!("unsupported") })
            .await
            .unwrap_err()
            .policy_rejected()
    );
    let mut candidate = before_lock();
    candidate.data["nodes"]["entry"]["inputs"]["new"] = json!("new");
    candidate.data["nodes"]["new"] =
        json!({"locked":{"type":"tarball","url":"https://fixture.invalid"}});
    assert!(
        !validate_forward(&before_lock(), &candidate, |_| async {
            panic!("unsupported new")
        })
        .await
        .unwrap_err()
        .policy_rejected()
    );
    candidate.data["nodes"]["new"]["locked"] =
        json!({"type":"github","owner":"fixture","repo":"new","rev":"f".repeat(40)});
    assert_eq!(
        validate_forward(&before_lock(), &candidate, |_| async {
            panic!("new immutable")
        })
        .await
        .unwrap()
        .len(),
        1
    );
}

#[tokio::test]
async fn forward_failure_aggregation_owns_native_spools_and_secondaries_through_aliases() {
    use crate::process::{Captured, PartialProcessEvidence};
    use std::{
        cell::{Cell, RefCell},
        io::Write,
        os::unix::process::ExitStatusExt,
    };
    let root = tempfile::tempdir().unwrap();
    let mut spool = tempfile::NamedTempFile::new_in(root.path()).unwrap();
    spool.write_all(b"native stdout evidence").unwrap();
    let path = spool.path().to_owned();
    let failure = RefCell::new(Some(
        ConfctlError::ProcessFailure {
            source: Box::new(ConfctlError::Child {
                description: "Git comparison".into(),
                code: Some(17),
                signal: None,
                log: None,
            }),
            evidence: Box::new(PartialProcessEvidence {
                status: Some(std::process::ExitStatus::from_raw(17 << 8)),
                stdout: Captured::Spool {
                    file: spool,
                    length: 22,
                },
                stderr: Captured::Memory(b"native stderr evidence".to_vec()),
                stdout_complete: true,
                stderr_complete: false,
                log: None,
            }),
        }
        .with_secondary(vec![ConfctlError::State(
            "secondary evidence failure".into(),
        )]),
    ));
    let before = before_lock();
    let candidate = expected_set();
    let calls = Cell::new(0);
    let error = validate_forward(&before, &candidate, |_| {
        calls.set(calls.get() + 1);
        std::future::ready(Err(failure
            .borrow_mut()
            .take()
            .expect("one failure per source/revisions")))
    })
    .await
    .unwrap_err();
    assert_eq!(calls.get(), 1);
    assert!(!error.policy_rejected());
    assert!(!error.is_cancelled());
    assert!(error.to_string().contains("alias:") && error.to_string().contains("parent/leaf:"));
    assert!(path.exists(), "aggregation dropped the only owned spool");
    let ConfctlError::Cleanup { primary, secondary } = &error else {
        panic!("typed evidence owner missing")
    };
    assert!(matches!(primary.as_ref(), ConfctlError::Configuration(_)));
    assert_eq!(secondary.len(), 1);
    let ConfctlError::Cleanup { primary, secondary } = &secondary[0] else {
        panic!("original secondary lost")
    };
    assert!(matches!(&secondary[..],[ConfctlError::State(s)] if s=="secondary evidence failure"));
    let ConfctlError::ProcessFailure { evidence, .. } = primary.as_ref() else {
        panic!("original native failure lost")
    };
    assert_eq!(evidence.status.unwrap().code(), Some(17));
    assert!(evidence.stdout_complete && !evidence.stderr_complete);
    assert_eq!(
        evidence.stderr.memory(),
        Some(b"native stderr evidence".as_slice())
    );
    let Captured::Spool { file, .. } = &evidence.stdout else {
        panic!("spool changed")
    };
    assert_eq!(
        std::fs::read(file.path()).unwrap(),
        b"native stdout evidence"
    );
    drop(error);
    assert!(!path.exists(), "original owner did not release its spool");
}

#[tokio::test]
async fn forward_cancellation_retains_preceding_failures_and_its_own_evidence() {
    use crate::process::{Captured, PartialProcessEvidence};
    use std::cell::RefCell;
    let before = before_lock();
    let mut candidate = expected_set();
    candidate.data["nodes"]["parent"]["locked"]["rev"] = json!("d".repeat(40));
    let failure = |source, bytes: &[u8]| ConfctlError::ProcessFailure {
        source: Box::new(source),
        evidence: Box::new(PartialProcessEvidence {
            status: None,
            stdout: Captured::Memory(bytes.into()),
            stderr: Captured::Unavailable,
            stdout_complete: false,
            stderr_complete: false,
            log: None,
        }),
    };
    let errors = RefCell::new(std::collections::VecDeque::from([
        failure(
            ConfctlError::State("first comparison failed".into()),
            b"first evidence",
        ),
        failure(ConfctlError::Cancelled, b"cancellation evidence"),
    ]));
    let error = validate_forward(&before, &candidate, |_| {
        std::future::ready(Err(errors.borrow_mut().pop_front().expect("no retry")))
    })
    .await
    .unwrap_err();
    assert!(errors.borrow().is_empty());
    assert!(error.is_cancelled());
    let ConfctlError::Cleanup { primary, secondary } = error else {
        panic!("earlier error lost on cancellation")
    };
    let ConfctlError::ProcessFailure { evidence, .. } = *primary else {
        panic!("cancellation evidence lost")
    };
    assert_eq!(
        evidence.stdout.memory(),
        Some(b"cancellation evidence".as_slice())
    );
    let [ConfctlError::ProcessFailure { evidence, .. }] = &secondary[..] else {
        panic!("earlier evidence lost")
    };
    assert_eq!(evidence.stdout.memory(), Some(b"first evidence".as_slice()));
}
