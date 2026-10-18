use super::*;
use serde_json::{Value, json};

fn snapshot(variation: &str, action: &str) -> Value {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(format!(
        "tests/compat/core/expected-follows/follows-{variation}-{action}.json"
    ));
    serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap()
}
fn lock(snapshot: &Value) -> FlakeLock {
    FlakeLock::decode(
        snapshot["files"]["flake.lock"]["text"]
            .as_str()
            .unwrap()
            .as_bytes(),
    )
    .unwrap()
}
#[test]
fn immutable_ruby_follows_info_and_override_argv_use_first_segment() {
    for variation in ["single", "nested", "different-suffix"] {
        let listing = snapshot(variation, "ls");
        let before = lock(&listing);
        let info = before.input_info("alias");
        assert_eq!(info.kind, "github");
        assert_eq!(info.short_rev.as_deref(), Some("aaaaaaaa"));
        assert_eq!(
            info.url.as_deref(),
            Some("https://github.com/fixture/nixpkgs")
        );
        let setting = snapshot(variation, "set");
        let events: Value = serde_json::from_str(setting["events"].as_str().unwrap()).unwrap();
        assert_eq!(
            before.override_url("alias", "next").unwrap(),
            events[0]["argv"][6]
        );
        assert_eq!(before.root_inputs(), ["alias"]);
    }
}
#[test]
fn immutable_ruby_missing_first_segment_never_resolves_nested_leaf() {
    let listing = snapshot("missing-first", "ls");
    let before = lock(&listing);
    assert_eq!(before.input_info("alias").kind, "-");
    assert!(before.input_info("alias").rev.is_none());
    let error = before.override_url("alias", "next").unwrap_err();
    let setting = snapshot("missing-first", "set");
    assert!(
        setting["stderr"]
            .as_str()
            .unwrap()
            .ends_with(&format!("error: {error}\n"))
    );
    assert_eq!(before.data, lock(&setting).data);
    assert!(
        before
            .override_url("unknown", "next")
            .unwrap_err()
            .to_string()
            .contains("unknown input")
    );
}
#[test]
fn setter_restores_original_on_temporary_first_node_and_preserves_leaf() {
    let before = lock(&snapshot("nested", "ls"));
    let expected = lock(&snapshot("nested", "set"));
    let mut temporary = before.clone();
    temporary.data["nodes"]["nixpkgs"]["locked"]["rev"] = json!("b".repeat(40));
    temporary.data["nodes"]["nixpkgs"]["original"] =
        json!({"type":"github","ref":"child-replaced"});
    temporary
        .restore_original("alias", before.original("alias").as_ref())
        .unwrap();
    assert_eq!(temporary.data, expected.data);
    let changes = InputChange::between(Some(&before), &temporary, &["alias".into()]);
    assert_eq!(changes.len(), 1);
    assert_eq!(changes[0].new_info.short_rev.as_deref(), Some("bbbbbbbb"));
    assert_eq!(temporary.pretty(), expected.pretty());
    assert!(InputChange::between(Some(&temporary), &temporary, &["alias".into()]).is_empty());
}
#[test]
fn override_urls_keep_git_query_and_github_dir_without_encoding_revisions() {
    let input = FlakeLock {
        data: json!({"nodes":{"root":{"inputs":{"git":"git","github":"github"}},"git":{"original":{"type":"git","url":"git+https://example.invalid/repo?shallow=1"}},"github":{"original":{"type":"github","owner":"fixture","repo":"repo","dir":"nested/path"}}}}),
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
    let warning = "warning: unable to download 'https://api.github.com/repos/fixture/repo': rate limit exceeded;\nusing cached version";
    let error = check_nix_output("", warning).unwrap_err().to_string();
    assert!(error.starts_with("Nix could not refresh GitHub input metadata because"));
    assert!(
        check_nix_output(
            "unable to download 'https://other.invalid/' using cached version",
            ""
        )
        .is_ok()
    );
}

struct Reply {
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
        _: crate::process::ProcessScope,
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
        Ok(ProcessOutcome {
            status: std::process::ExitStatus::from_raw(reply.code << 8),
            stdout: Captured::Memory(vec![]),
            stderr: Captured::Memory(reply.stderr.into_bytes()),
            stdout_complete: true,
            stderr_complete: true,
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
    std::fs::write(
        scratch.path().join("flake.lock"),
        lock(&snapshot("nested", "ls")).pretty(),
    )
    .unwrap();
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
    let mut lock = lock(&snapshot("nested", "set")).data;
    lock["nodes"]["nixpkgs"]["original"] = json!({"type":"github","ref":"child-replaced"});
    lock
}
#[tokio::test]
async fn setter_child_argv_fallback_and_original_restoration_match_oracle_effects() {
    let (scratch, editor, calls) = editor(vec![
        Reply {
            code: 1,
            stderr: "experimental feature flakes disabled".into(),
            lock: None,
        },
        Reply {
            code: 0,
            stderr: String::new(),
            lock: Some(child_lock()),
        },
    ]);
    let changes = editor.set(&["alias".into()], "next").await.unwrap();
    assert_eq!(changes.len(), 1);
    assert_eq!(
        FlakeLock::load(&scratch.path().join("flake.lock"))
            .unwrap()
            .data,
        lock(&snapshot("nested", "set")).data
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
    assert_eq!(std::fs::read_dir(scratch.path()).unwrap().count(), 2);
}
#[tokio::test]
async fn failed_setter_child_keeps_previous_lock_and_removes_owned_temporary_output() {
    let (scratch, editor, calls) = editor(vec![Reply {
        code: 23,
        stderr: "actual child failure".into(),
        lock: Some(child_lock()),
    }]);
    let path = scratch.path().join("flake.lock");
    let before = std::fs::read(&path).unwrap();
    let error = editor.set(&["alias".into()], "next").await.unwrap_err();
    assert!(matches!(
        error,
        crate::error::ConfctlError::ProcessFailure { .. }
    ));
    assert_eq!(std::fs::read(path).unwrap(), before);
    assert_eq!(calls.lock().unwrap().len(), 1);
    assert_eq!(std::fs::read_dir(scratch.path()).unwrap().count(), 2);
}
#[tokio::test]
async fn updater_cached_metadata_error_preserves_actual_nix_lock_side_effect() {
    let warning = "unable to download 'https://api.github.com/repos/fixture/repo': rate limit exceeded; using cached version";
    let expected = child_lock();
    let (scratch, editor, calls) = editor(vec![Reply {
        code: 0,
        stderr: warning.into(),
        lock: Some(expected.clone()),
    }]);
    let error = editor.update(&["alias".into()]).await.unwrap_err();
    assert!(error.to_string().contains("rate limit was exceeded"));
    assert_eq!(
        FlakeLock::load(&scratch.path().join("flake.lock"))
            .unwrap()
            .data,
        expected
    );
    assert_eq!(calls.lock().unwrap()[0].args, ["flake", "update", "alias"]);
}
