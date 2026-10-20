use super::*;
use crate::native_tool;

fn shell(script: &str) -> ProcessSpec {
    let mut spec = ProcessSpec::new("sh", "fake local child");
    spec.args = vec!["-c".into(), script.into()];
    spec
}

fn owner(
    path: &Path,
    limit: usize,
    log: Option<ProcessLog>,
) -> (Supervisor, crate::events::EventReceiver) {
    let (events, receiver) = EventSink::channel(2);
    (
        Supervisor::new(
            events,
            path.to_path_buf(),
            limit,
            Duration::from_millis(100),
            log,
            None,
        ),
        receiver,
    )
}

#[tokio::test]
async fn nonzero_status_keeps_independent_binary_streams() {
    let directory = tempfile::tempdir().expect("spool directory");
    let (runner, _receiver) = owner(directory.path(), 100, None);
    let result = runner
        .run(
            shell("printf '\\000\\377out'; printf 'err' >&2; exit 7"),
            ProcessScope::new(CancellationToken::new(), TaskId(1)),
        )
        .await
        .expect("ordinary outcome");
    assert_eq!(result.code(), Some(7));
    assert_eq!(result.stdout.memory(), Some(&b"\0\xffout"[..]));
    assert_eq!(result.stderr.memory(), Some(&b"err"[..]));
    assert_eq!(result.termination, TerminationCause::Exited);
    runner.shutdown().await.expect("join");
}

#[tokio::test]
async fn merged_native_writes_keep_one_ordered_capture_relay_and_log() {
    for limit in [32, 65_536] {
        let directory = tempfile::tempdir().unwrap();
        let log_path = directory.path().join("command.log");
        let log = ProcessLog::new(File::create(&log_path).await.unwrap());
        let (runner, mut receiver) = owner(directory.path(), limit, Some(log));
        let mut spec = shell(
            "test /proc/self/fd/1 -ef /proc/self/fd/2 || exit 91; i=0; while [ $i -lt 256 ]; do printf 'out\\000'; printf 'err\\377' >&2; i=$((i+1)); done; exit 7",
        );
        spec.stdout = StreamPolicy::CaptureAndForward;
        spec.stderr = StreamPolicy::MergeToStdout;
        let consumer = tokio::spawn(async move {
            let mut bytes = Vec::new();
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                let UiEvent::Output {
                    task,
                    stream,
                    bytes: chunk,
                } = event
                else {
                    panic!("only merged output expected");
                };
                assert_eq!(task, TaskId(6));
                assert_eq!(stream, OutputStream::Stdout);
                bytes.extend(chunk);
                ack.acknowledge(Ok(()));
            }
            bytes
        });
        let outcome = tokio::time::timeout(
            Duration::from_secs(5),
            runner.run(spec, ProcessScope::new(CancellationToken::new(), TaskId(6))),
        )
        .await
        .unwrap()
        .unwrap();
        let expected = b"out\0err\xff".repeat(256);
        assert_eq!(outcome.code(), Some(7));
        assert_eq!(capture_bytes(&outcome.stdout), expected);
        assert!(outcome.stdout_complete);
        assert_eq!(outcome.stdout.spool_path().is_some(), limit == 32);
        assert!(matches!(outcome.stderr, Captured::Unavailable));
        assert!(!outcome.stderr_complete);
        runner.shutdown().await.unwrap();
        drop(runner);
        assert_eq!(consumer.await.unwrap(), expected);
        assert_eq!(std::fs::read(log_path).unwrap(), expected);
    }
}

#[tokio::test]
async fn merged_delivery_failure_and_cancellation_join_and_retain_the_single_prefix() {
    for reject in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let spools = directory.path().join("spools");
        std::fs::create_dir(&spools).unwrap();
        let marker = directory.path().join("leader");
        let (runner, mut receiver) = owner(&spools, 0, None);
        let mut spec = leader_fixture(
            &marker,
            "printf '%s' $$ > \"$1\"; printf out; printf err >&2; exec sleep 60",
        );
        // A failed forward-only delivery must retain its capture too.
        spec.stdout = if reject {
            StreamPolicy::Forward
        } else {
            StreamPolicy::CaptureAndForward
        };
        spec.stderr = StreamPolicy::MergeToStdout;
        let cancel = CancellationToken::new();
        let child_cancel = cancel.clone();
        let running = runner.clone();
        let run = tokio::spawn(async move {
            running
                .run(spec, ProcessScope::new(child_cancel, TaskId(7)))
                .await
        });
        let delivery = tokio::time::timeout(Duration::from_secs(5), receiver.receive())
            .await
            .unwrap()
            .unwrap();
        // Hold the UI ack until both writes are captured by the single drain.
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                let files = std::fs::read_dir(&spools)
                    .unwrap()
                    .collect::<Result<Vec<_>, _>>()
                    .unwrap();
                assert_eq!(files.len(), 1);
                if files[0].metadata().unwrap().len() == 6 {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        if reject {
            delivery.acknowledge(Err("merged delivery failed".into()));
        } else {
            cancel.cancel();
            delivery.acknowledge(Ok(()));
        }
        let result = tokio::time::timeout(Duration::from_secs(5), run)
            .await
            .unwrap()
            .unwrap();
        drop(receiver);
        if reject {
            let error = result.unwrap_err();
            assert!(error.to_string().contains("merged delivery failed"));
            let evidence = failure_evidence(&error);
            assert!(evidence.status.is_some());
            assert_eq!(capture_bytes(&evidence.stdout), b"outerr");
            assert!(!evidence.stdout_complete);
            assert!(matches!(evidence.stderr, Captured::Unavailable));
            assert!(!evidence.stderr_complete);
        } else {
            let outcome = result.unwrap();
            assert_eq!(outcome.termination, TerminationCause::Canceled);
            assert_eq!(capture_bytes(&outcome.stdout), b"outerr");
            assert!(!outcome.stdout_complete);
            assert!(matches!(outcome.stderr, Captured::Unavailable));
            assert!(!outcome.stderr_complete);
        }
        let pid = std::fs::read_to_string(marker).unwrap();
        assert!(
            !Path::new("/proc").join(pid).exists(),
            "leader must be reaped"
        );
        runner.shutdown().await.unwrap();
    }
}

#[tokio::test]
async fn invalid_merged_output_policies_fail_before_spawn() {
    let directory = tempfile::tempdir().unwrap();
    let marker = directory.path().join("effect");
    let (runner, _receiver) = owner(directory.path(), 128, None);
    for stdout in [StreamPolicy::MergeToStdout, StreamPolicy::Foreground] {
        let mut spec = leader_fixture(&marker, "touch \"$1\"");
        spec.stdout = stdout;
        spec.stderr = StreamPolicy::MergeToStdout;
        assert!(matches!(
            runner
                .run(spec, ProcessScope::new(CancellationToken::new(), TaskId(8)))
                .await,
            Err(ConfctlError::Configuration(_))
        ));
        assert!(!marker.exists());
    }
    runner.shutdown().await.unwrap();
}

#[tokio::test]
async fn both_pipes_spool_while_terminal_acknowledgement_is_held() {
    let directory = tempfile::tempdir().expect("spool directory");
    let (runner, mut receiver) = owner(directory.path(), 128, None);
    let mut spec = shell(
        "i=0; while [ $i -lt 16384 ]; do printf '0123456789abcdef'; printf 'fedcba9876543210' >&2; i=$((i+1)); done",
    );
    spec.stdout = StreamPolicy::CaptureAndForward;
    spec.stderr = StreamPolicy::CaptureAndForward;
    let running = runner.clone();
    let run = tokio::spawn(async move {
        running
            .run(spec, ProcessScope::new(CancellationToken::new(), TaskId(2)))
            .await
    });
    let held = receiver.receive().await.expect("first delivery");
    // Both streams exceed pipe and memory limits without a single UI ack.
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            let mut lengths = Vec::new();
            for entry in std::fs::read_dir(directory.path()).expect("spools") {
                lengths.push(entry.expect("entry").metadata().expect("metadata").len());
            }
            if lengths.len() == 2 && lengths.iter().all(|length| *length == 262_144) {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("UI must not block pipe drains");
    held.acknowledge(Ok(()));
    let consume = tokio::spawn(async move {
        while let Some(delivery) = receiver.receive().await {
            delivery.acknowledge(Ok(()));
        }
    });
    let result = tokio::time::timeout(Duration::from_secs(5), run)
        .await
        .expect("finish")
        .expect("join")
        .expect("outcome");
    assert_eq!(result.stdout.len(), 262_144);
    assert_eq!(result.stderr.len(), 262_144);
    assert!(result.stdout.spool_path().is_some());
    assert!(result.stderr.spool_path().is_some());
    runner.shutdown().await.expect("join");
    drop(runner);
    consume.await.expect("consumer join");
}

#[tokio::test]
async fn cancellation_joins_group_and_dropping_run_does_not_detach_it() {
    let directory = tempfile::tempdir().expect("spool directory");
    let (runner, _receiver) = owner(directory.path(), 128, None);
    let cancel = CancellationToken::new();
    let running = runner.clone();
    let child_cancel = cancel.clone();
    let run = tokio::spawn(async move {
        running
            .run(
                shell("sleep 60 & wait"),
                ProcessScope::new(child_cancel, TaskId(3)),
            )
            .await
    });
    tokio::time::sleep(Duration::from_millis(30)).await;
    cancel.cancel();
    let result = tokio::time::timeout(Duration::from_secs(5), run)
        .await
        .expect("bounded cancel")
        .expect("join")
        .expect("outcome");
    assert_eq!(result.termination, TerminationCause::Canceled);
    let running = runner.clone();
    let dropped = tokio::spawn(async move {
        running
            .run(
                shell("sleep 60 & wait"),
                ProcessScope::new(CancellationToken::new(), TaskId(4)),
            )
            .await
    });
    tokio::time::sleep(Duration::from_millis(30)).await;
    dropped.abort();
    let _ = dropped.await;
    tokio::time::timeout(Duration::from_secs(5), runner.shutdown())
        .await
        .expect("join dropped caller work")
        .expect("shutdown");
}

#[tokio::test]
async fn spawn_error_is_distinct_from_child_failure() {
    let directory = tempfile::tempdir().expect("spool directory");
    let (runner, _receiver) = owner(directory.path(), 128, None);
    let result = runner
        .run(
            ProcessSpec::new(directory.path().join("missing"), "redacted fixture"),
            ProcessScope::new(CancellationToken::new(), TaskId(5)),
        )
        .await;
    assert!(
        matches!(result, Err(ConfctlError::Spawn { description, .. }) if description == "redacted fixture")
    );
    runner.shutdown().await.expect("join");
}

fn capture_bytes(capture: &Captured) -> Vec<u8> {
    match capture {
        Captured::Memory(bytes) => bytes.clone(),
        Captured::Spool { file, .. } => std::fs::read(file.path()).expect("owned capture"),
        Captured::Unavailable => panic!("available evidence was discarded"),
    }
}

fn failure_evidence(error: &ConfctlError) -> &PartialProcessEvidence {
    match error {
        ConfctlError::ProcessFailure { evidence, .. } => evidence,
        ConfctlError::Cleanup { primary, .. } => failure_evidence(primary),
        _ => panic!("missing process evidence: {error}"),
    }
}

fn leader_fixture(marker: &Path, tail: &str) -> ProcessSpec {
    let mut spec = shell(tail);
    spec.args.push("fixture".into());
    spec.args.push(marker.as_os_str().to_owned());
    spec
}

async fn zombie_leader(marker: &Path) {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if let Ok(pid) = std::fs::read_to_string(marker)
                && let Ok(stat) = std::fs::read_to_string(format!("/proc/{}/stat", pid.trim()))
                && stat
                    .rsplit_once(')')
                    .is_some_and(|(_, rest)| rest.trim_start().starts_with('Z'))
            {
                return;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .expect("leader retained unreaped");
}

#[tokio::test]
async fn ordinary_leader_exit_keeps_delayed_descendant_tail() {
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 128, None);
    let result = tokio::time::timeout(
        Duration::from_secs(5),
        runner.run(
            shell("printf head; (sleep 0.15; printf tail; printf errtail >&2) & exit 0"),
            ProcessScope::new(CancellationToken::new(), TaskId(10)),
        ),
    )
    .await
    .expect("tail finishes")
    .expect("normal outcome");
    assert_eq!(result.code(), Some(0));
    assert_eq!(capture_bytes(&result.stdout), b"headtail");
    assert_eq!(capture_bytes(&result.stderr), b"errtail");
    assert!(result.stdout_complete && result.stderr_complete);
    runner.shutdown().await.expect("join");
}

#[tokio::test]
async fn cancellation_after_leader_exit_joins_inherited_pipe_writers() {
    let directory = tempfile::tempdir().expect("spools");
    let marker = directory.path().join("leader");
    let (runner, _receiver) = owner(directory.path(), 128, None);
    let cancel = CancellationToken::new();
    let child_cancel = cancel.clone();
    let running = runner.clone();
    let spec = leader_fixture(
        &marker,
        "printf '%s' $$ > \"$1\"; (trap '' TERM; sleep 60) & exit 0",
    );
    let run = tokio::spawn(async move {
        running
            .run(spec, ProcessScope::new(child_cancel, TaskId(11)))
            .await
    });
    zombie_leader(&marker).await;
    cancel.cancel();
    let result = tokio::time::timeout(Duration::from_secs(5), run)
        .await
        .expect("cancel joins")
        .expect("task")
        .expect("outcome");
    assert_eq!(result.code(), Some(0));
    assert_eq!(result.termination, TerminationCause::Canceled);
    runner.shutdown().await.expect("join");
}

#[tokio::test]
async fn relay_failure_after_leader_exit_retains_owned_prefix() {
    let directory = tempfile::tempdir().expect("spools");
    let marker = directory.path().join("leader");
    let (runner, mut receiver) = owner(directory.path(), 0, None);
    let mut spec = leader_fixture(
        &marker,
        "printf '%s' $$ > \"$1\"; (printf secret-prefix; sleep 60) & exit 0",
    );
    spec.stdout = StreamPolicy::Forward;
    let running = runner.clone();
    let run = tokio::spawn(async move {
        running
            .run(
                spec,
                ProcessScope::new(CancellationToken::new(), TaskId(12)),
            )
            .await
    });
    let delivery = receiver.receive().await.expect("prefix delivery");
    zombie_leader(&marker).await;
    drop(receiver);
    delivery.acknowledge(Err("fixture render failed".into()));
    let error = tokio::time::timeout(Duration::from_secs(5), run)
        .await
        .expect("failure joins")
        .expect("task")
        .expect_err("required delivery");
    let evidence = failure_evidence(&error);
    assert_eq!(evidence.status.and_then(|status| status.code()), Some(0));
    assert_eq!(capture_bytes(&evidence.stdout), b"secret-prefix");
    assert!(!evidence.stdout_complete);
    assert!(!format!("{error:?} {error}").contains("secret-prefix"));
    let retained = evidence
        .stdout
        .spool_path()
        .expect("owned spool")
        .to_path_buf();
    assert!(retained.exists());
    drop(error);
    assert!(!retained.exists(), "unused evidence releases its spool");
    runner.shutdown().await.expect("join");
}

struct BrokenLog {
    flush_only: bool,
    failed: Arc<tokio::sync::Notify>,
}
impl AsyncWrite for BrokenLog {
    fn poll_write(
        self: std::pin::Pin<&mut Self>,
        _: &mut std::task::Context<'_>,
        bytes: &[u8],
    ) -> std::task::Poll<io::Result<usize>> {
        if self.flush_only {
            return std::task::Poll::Ready(Ok(bytes.len()));
        }
        self.failed.notify_one();
        std::task::Poll::Ready(Err(io::Error::other("fixture log write failed")))
    }
    fn poll_flush(
        self: std::pin::Pin<&mut Self>,
        _: &mut std::task::Context<'_>,
    ) -> std::task::Poll<io::Result<()>> {
        self.failed.notify_one();
        std::task::Poll::Ready(Err(io::Error::other("fixture log flush failed")))
    }
    fn poll_shutdown(
        self: std::pin::Pin<&mut Self>,
        _: &mut std::task::Context<'_>,
    ) -> std::task::Poll<io::Result<()>> {
        std::task::Poll::Ready(Ok(()))
    }
}

#[tokio::test]
async fn log_write_and_flush_faults_preserve_prefix_and_locator() {
    for flush_only in [false, true] {
        let directory = tempfile::tempdir().expect("spools");
        let locator = directory.path().join("command.log");
        let log = ProcessLog::new(BrokenLog {
            flush_only,
            failed: Arc::new(tokio::sync::Notify::new()),
        })
        .with_locator(locator.clone());
        let (runner, _receiver) = owner(directory.path(), 0, Some(log));
        let error = tokio::time::timeout(
            Duration::from_secs(5),
            runner.run(
                shell("printf private-prefix; sleep 60"),
                ProcessScope::new(CancellationToken::new(), TaskId(13)),
            ),
        )
        .await
        .expect("log fault joins")
        .expect_err("log required");
        let evidence = failure_evidence(&error);
        assert_eq!(evidence.log.as_ref(), Some(&locator));
        assert_eq!(capture_bytes(&evidence.stdout), b"private-prefix");
        assert!(!evidence.stdout_complete);
        assert!(!format!("{error:?} {error}").contains("private-prefix"));
        assert!(error.to_string().contains("write command log"));
        runner.shutdown().await.expect("join");
    }
}

#[tokio::test]
async fn dropped_caller_failure_is_owned_until_checked_shutdown() {
    let directory = tempfile::tempdir().expect("spools");
    let failed = Arc::new(tokio::sync::Notify::new());
    let log = ProcessLog::new(BrokenLog {
        flush_only: false,
        failed: failed.clone(),
    });
    let (runner, _receiver) = owner(directory.path(), 0, Some(log));
    let running = runner.clone();
    let run = tokio::spawn(async move {
        running
            .run(
                shell("printf orphan-prefix; sleep 60"),
                ProcessScope::new(CancellationToken::new(), TaskId(14)),
            )
            .await
    });
    tokio::time::timeout(Duration::from_secs(5), failed.notified())
        .await
        .expect("log failed");
    run.abort();
    let _ = run.await;
    let error = tokio::time::timeout(Duration::from_secs(5), runner.shutdown())
        .await
        .expect("joins")
        .expect_err("unobserved failure reported");
    let evidence = failure_evidence(&error);
    assert_eq!(capture_bytes(&evidence.stdout), b"orphan-prefix");
    let path = evidence
        .stdout
        .spool_path()
        .expect("owned spool")
        .to_path_buf();
    assert!(path.exists());
    drop(error);
    assert!(!path.exists());
    runner
        .shutdown()
        .await
        .expect("failure already transferred");
}

#[tokio::test]
async fn canceled_blocked_stdin_is_joined_without_prompt_limit() {
    let directory = tempfile::tempdir().expect("spools");
    let (runner, mut receiver) = owner(directory.path(), 128, None);
    let cancel = CancellationToken::new();
    let child_cancel = cancel.clone();
    let mut spec = shell("printf ready; sleep 60");
    spec.stdin = StdinPolicy::Bytes(vec![b'x'; 2 * 1024 * 1024]);
    spec.stdout = StreamPolicy::CaptureAndForward;
    let running = runner.clone();
    let run = tokio::spawn(async move {
        running
            .run(spec, ProcessScope::new(child_cancel, TaskId(15)))
            .await
    });
    receiver
        .receive()
        .await
        .expect("child ready")
        .acknowledge(Ok(()));
    cancel.cancel();
    let result = tokio::time::timeout(Duration::from_secs(5), run)
        .await
        .expect("stdin cancel joins")
        .expect("task")
        .expect("canceled outcome");
    assert_eq!(result.termination, TerminationCause::Canceled);
    assert_eq!(capture_bytes(&result.stdout), b"ready");
    runner.shutdown().await.expect("join");
}

#[tokio::test]
async fn spool_write_fault_retains_confirmed_prefix_and_marks_incomplete() {
    use std::io::Write;
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 0, None);
    let mut prepared = prepare_stream(&runner.owner, StreamPolicy::Capture).expect("stream");
    prepared
        .spool
        .as_file_mut()
        .write_all(b"confirmed-prefix")
        .expect("prefix");
    prepared.writer = std::fs::File::open(prepared.spool.path()).expect("read-only fault fd");
    let (errors, mut failures) = mpsc::unbounded_channel();
    let (pump, _) = start_stream(
        &runner.owner,
        prepared,
        &b"unwritten"[..],
        (OutputStream::Stdout, TaskId(16)),
        (None, None),
        CancellationToken::new(),
        errors,
    );
    assert!(!pump.handle.await.expect("join"));
    let failure = failures.recv().await.expect("deferred write fault");
    eprintln!("deferred spool write fault: {failure:?}");
    // Tokio fs::File accepts the buffer before the blocking OS write finishes;
    // flush observes EBADF from this read-only descriptor.
    assert!(
        matches!(failure, ConfctlError::Io { operation: "flush stream spool", source, .. } if source.raw_os_error() == Some(rustix::io::Errno::BADF.raw_os_error()))
    );
    assert_eq!(
        std::fs::read(pump.spool.path()).expect("prefix retained"),
        b"confirmed-prefix"
    );
    runner.shutdown().await.expect("join");
}

#[tokio::test]
async fn signal_error_then_single_reap_disarms_every_numeric_signal() {
    let _notifications =
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::child()).expect("SIGCHLD");
    let mut child = Command::new("sh")
        .args(["-c", "exit 0"])
        .process_group(0)
        .spawn()
        .expect("child");
    let mut group =
        Group::new(rustix::process::Pid::from_raw(child.id().expect("PID") as i32).expect("PID"));
    observe_exit(&mut group).await.expect("reserved zombie");
    assert!(
        group
            .signal_using(rustix::process::Signal::TERM, |_, _| Err(
                rustix::io::Errno::PERM
            ))
            .is_err()
    );
    assert!(
        final_wait(&mut child, &mut group)
            .await
            .expect("one final reap")
            .success()
    );
    group
        .signal_using(rustix::process::Signal::KILL, |_, _| {
            panic!("numeric signal after reap")
        })
        .expect("disarmed");
    assert_eq!(group.transitions, ["numeric-signal", "disarm", "wait"]);
    assert_eq!(
        group.probe().expect_err("already reaped").raw_os_error(),
        Some(10)
    );
    group
        .signal_using(rustix::process::Signal::KILL, |_, _| {
            panic!("numeric signal after ECHILD")
        })
        .expect("ownership lost");
}

#[tokio::test]
async fn echild_before_signal_disables_numeric_group_operations() {
    let _notifications =
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::child()).expect("SIGCHLD");
    let mut child = Command::new("sh")
        .args(["-c", "exit 0"])
        .process_group(0)
        .spawn()
        .expect("child");
    let mut group =
        Group::new(rustix::process::Pid::from_raw(child.id().expect("PID") as i32).expect("PID"));
    child
        .wait()
        .await
        .expect("simulate competing consuming wait");
    assert_eq!(
        group
            .signal_using(rustix::process::Signal::TERM, |_, _| panic!(
                "numeric signal on ownership loss"
            ))
            .expect_err("ECHILD")
            .raw_os_error(),
        Some(10)
    );
    group
        .signal_using(rustix::process::Signal::KILL, |_, _| {
            panic!("numeric signal after ownership loss")
        })
        .expect("disabled");
    assert_eq!(group.transitions, ["disarm"]);
}

#[test]
fn inherited_sigchld_ignore_is_replaced_before_spawn() {
    const MARKER: &str = "CONFCTL_TEST_INHERITED_SIGCHLD_IGN";
    if std::env::var_os(MARKER).is_none() {
        let result = std::process::Command::new("sh")
            .args(["-c", "trap '' CHLD; exec \"$1\" --exact process::tests::inherited_sigchld_ignore_is_replaced_before_spawn --nocapture", "fixture"])
            .arg(std::env::current_exe().expect("test binary")).env(MARKER, "1").output().expect("isolated inherited-IGN fixture");
        assert!(
            result.status.success(),
            "{}{}",
            String::from_utf8_lossy(&result.stdout),
            String::from_utf8_lossy(&result.stderr)
        );
        return;
    }
    let error = std::process::Command::new("sh")
        .args(["-c", "exit 0"])
        .spawn()
        .expect("probe child")
        .wait()
        .expect_err("inherited IGN auto-reaps before Tokio ownership");
    assert_eq!(error.raw_os_error(), Some(10));
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime")
        .block_on(async {
            let directory = tempfile::tempdir().expect("spools");
            let (runner, _receiver) = owner(directory.path(), 128, None);
            let outcome = runner
                .run(
                    shell("(sleep 0.1; printf tail) & exit 0"),
                    ProcessScope::new(CancellationToken::new(), TaskId(17)),
                )
                .await
                .expect("leader reservation survives inherited IGN");
            assert_eq!(outcome.code(), Some(0));
            assert_eq!(capture_bytes(&outcome.stdout), b"tail");
            runner.shutdown().await.expect("join");
        });
}

#[tokio::test]
async fn canceled_open_reader_retains_incomplete_prefix_without_waiting_for_eof() {
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 0, None);
    let prepared = prepare_stream(&runner.owner, StreamPolicy::Capture).expect("stream");
    let (mut producer, reader) = tokio::io::duplex(128);
    let cancel = CancellationToken::new();
    let (errors, _failures) = mpsc::unbounded_channel();
    let (pump, _) = start_stream(
        &runner.owner,
        prepared,
        reader,
        (OutputStream::Stdout, TaskId(18)),
        (None, None),
        cancel.clone(),
        errors,
    );
    producer.write_all(b"blocked-prefix").await.expect("prefix");
    tokio::time::timeout(Duration::from_secs(5), async {
        while pump.spool.as_file().metadata().expect("metadata").len() != 14 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("prefix drained");
    cancel.cancel();
    assert!(
        !tokio::time::timeout(Duration::from_secs(5), pump.handle)
            .await
            .expect("open writer cannot block cancel")
            .expect("joined")
    );
    assert_eq!(
        std::fs::read(pump.spool.path()).expect("prefix"),
        b"blocked-prefix"
    );
    drop(producer);
    runner.shutdown().await.expect("join");
}

#[tokio::test]
async fn canceled_outcome_marks_only_interrupted_stream_incomplete() {
    let directory = tempfile::tempdir().expect("spools");
    let (runner, mut receiver) = owner(directory.path(), 128, None);
    let cancel = CancellationToken::new();
    let child_cancel = cancel.clone();
    let mut spec = shell("printf finished >&2; exec 2>&-; printf prefix; sleep 60");
    spec.stdout = StreamPolicy::CaptureAndForward;
    let running = runner.clone();
    let run = tokio::spawn(async move {
        running
            .run(spec, ProcessScope::new(child_cancel, TaskId(19)))
            .await
    });
    receiver.receive().await.expect("ready").acknowledge(Ok(()));
    // Allow the independent already-closed stderr drain to record EOF before
    // canceling the still-open stdout reader.
    tokio::time::sleep(Duration::from_millis(30)).await;
    cancel.cancel();
    let result = tokio::time::timeout(Duration::from_secs(5), run)
        .await
        .expect("joins")
        .expect("task")
        .expect("cancellation is an outcome");
    assert_eq!(result.termination, TerminationCause::Canceled);
    assert_eq!(capture_bytes(&result.stdout), b"prefix");
    assert_eq!(capture_bytes(&result.stderr), b"finished");
    assert!(!result.stdout_complete);
    assert!(result.stderr_complete);
    runner.shutdown().await.expect("join");
}

struct GatedBrokenLog(oneshot::Receiver<()>);
impl AsyncWrite for GatedBrokenLog {
    fn poll_write(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
        _: &[u8],
    ) -> std::task::Poll<io::Result<usize>> {
        match std::pin::Pin::new(&mut self.0).poll(context) {
            std::task::Poll::Pending => std::task::Poll::Pending,
            std::task::Poll::Ready(_) => {
                std::task::Poll::Ready(Err(io::Error::other("gated log fault")))
            }
        }
    }
    fn poll_flush(
        self: std::pin::Pin<&mut Self>,
        _: &mut std::task::Context<'_>,
    ) -> std::task::Poll<io::Result<()>> {
        std::task::Poll::Ready(Ok(()))
    }
    fn poll_shutdown(
        self: std::pin::Pin<&mut Self>,
        _: &mut std::task::Context<'_>,
    ) -> std::task::Poll<io::Result<()>> {
        std::task::Poll::Ready(Ok(()))
    }
}

#[tokio::test]
async fn log_failure_after_leader_exit_stops_descendants_and_retains_evidence() {
    let directory = tempfile::tempdir().expect("spools");
    let marker = directory.path().join("leader");
    let (release, wait) = oneshot::channel();
    let (runner, _receiver) = owner(
        directory.path(),
        0,
        Some(ProcessLog::new(GatedBrokenLog(wait))),
    );
    let running = runner.clone();
    let spec = leader_fixture(
        &marker,
        "printf '%s' $$ > \"$1\"; (printf retained; sleep 60) & exit 0",
    );
    let run = tokio::spawn(async move {
        running
            .run(
                spec,
                ProcessScope::new(CancellationToken::new(), TaskId(20)),
            )
            .await
    });
    zombie_leader(&marker).await;
    release.send(()).expect("release failure after leader exit");
    let error = tokio::time::timeout(Duration::from_secs(5), run)
        .await
        .expect("joined descendants")
        .expect("task")
        .expect_err("log fault");
    assert_eq!(capture_bytes(&failure_evidence(&error).stdout), b"retained");
    assert!(error.to_string().contains("write command log"));
    runner.shutdown().await.expect("join");
}

#[tokio::test]
async fn inherited_foreground_streams_are_unavailable_and_incomplete() {
    use std::os::unix::net::UnixStream;
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 128, None);
    let (input, _peer) = UnixStream::pair().expect("non-TTY input");
    let (events, mut receiver) = EventSink::channel(2);
    let ui = crate::terminal::Ui::new(&input, events, CancellationToken::new(), None).expect("UI");
    let consume = tokio::spawn(async move {
        while let Some(delivery) = receiver.receive().await {
            delivery.acknowledge(Ok(()));
        }
    });
    let mut spec = shell("exit 0");
    spec.stdout = StreamPolicy::Foreground;
    spec.stderr = StreamPolicy::Foreground;
    let mut scope = ProcessScope::new(CancellationToken::new(), TaskId(21));
    scope.foreground = Some(ui.foreground().await.expect("lease"));
    let outcome = runner
        .run(spec, scope)
        .await
        .expect("non-TTY inherited streams");
    assert!(matches!(outcome.stdout, Captured::Unavailable));
    assert!(matches!(outcome.stderr, Captured::Unavailable));
    assert!(!outcome.stdout_complete && !outcome.stderr_complete);
    ui.shutdown().expect("join input");
    drop(ui);
    consume.await.expect("consumer");
    runner.shutdown().await.expect("join");
}

struct PrefixThenDrainFault(bool);
impl AsyncRead for PrefixThenDrainFault {
    fn poll_read(
        mut self: std::pin::Pin<&mut Self>,
        _: &mut std::task::Context<'_>,
        buffer: &mut tokio::io::ReadBuf<'_>,
    ) -> std::task::Poll<io::Result<()>> {
        if self.0 {
            return std::task::Poll::Ready(Err(io::Error::other("fixture drain fault")));
        }
        self.0 = true;
        buffer.put_slice(b"read-prefix");
        std::task::Poll::Ready(Ok(()))
    }
}

#[tokio::test]
async fn drain_fault_retains_prefix_and_marks_incomplete() {
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 0, None);
    let prepared = prepare_stream(&runner.owner, StreamPolicy::Capture).expect("stream");
    let (errors, mut failures) = mpsc::unbounded_channel();
    let (pump, _) = start_stream(
        &runner.owner,
        prepared,
        PrefixThenDrainFault(false),
        (OutputStream::Stdout, TaskId(22)),
        (None, None),
        CancellationToken::new(),
        errors,
    );
    assert!(!pump.handle.await.expect("joined"));
    assert!(matches!(
        failures.recv().await,
        Some(ConfctlError::Drain {
            stream: "stdout",
            ..
        })
    ));
    assert_eq!(
        std::fs::read(pump.spool.path()).expect("retained prefix"),
        b"read-prefix"
    );
    runner.shutdown().await.expect("join");
}

#[tokio::test]
async fn child_input_and_output_are_not_subject_to_prompt_limit() {
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 128, None);
    let bytes = vec![b'x'; 2 * 1024 * 1024];
    let mut spec = shell("cat");
    spec.stdin = StdinPolicy::Bytes(bytes.clone());
    let outcome = tokio::time::timeout(
        Duration::from_secs(5),
        runner.run(
            spec,
            ProcessScope::new(CancellationToken::new(), TaskId(23)),
        ),
    )
    .await
    .expect("unbounded child stream finishes")
    .expect("outcome");
    assert_eq!(outcome.code(), Some(0));
    assert!(outcome.stdout_complete && outcome.stderr_complete);
    assert_eq!(capture_bytes(&outcome.stdout), bytes);
    assert!(outcome.stdout.spool_path().is_some());
    runner.shutdown().await.expect("joined");
}

fn retained_fixture_error(directory: &Path) -> ConfctlError {
    use std::io::Write;
    let mut spool = NamedTempFile::new_in(directory).expect("owned evidence");
    spool.write_all(b"queued-private-prefix").expect("prefix");
    ConfctlError::ProcessFailure {
        source: Box::new(
            ConfctlError::Drain {
                stream: "stdout",
                source: io::Error::other("fixture primary"),
            }
            .with_secondary(vec![
                ConfctlError::InputEof,
                ConfctlError::EventDelivery("later".into()),
            ]),
        ),
        evidence: Box::new(PartialProcessEvidence {
            status: None,
            stdout: Captured::Spool {
                file: spool,
                length: 21,
            },
            stderr: Captured::Memory(b"private stderr".to_vec()),
            stdout_complete: false,
            stderr_complete: true,
            log: None,
        }),
    }
}

#[tokio::test]
async fn process_reply_close_before_send_retains_failure_once() {
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 0, None);
    let (send, receive) = oneshot::channel();
    drop(ProcessResponse {
        receive,
        owner: runner.owner.clone(),
        armed: true,
    });
    deliver_result(
        &runner.owner,
        send,
        Err(retained_fixture_error(directory.path())),
    );
    assert_eq!(runner.owner.unobserved.lock().expect("sink").len(), 1);
    let error = runner.shutdown().await.expect_err("unobserved failure");
    assert_eq!(
        capture_bytes(&failure_evidence(&error).stdout),
        b"queued-private-prefix"
    );
    runner.shutdown().await.expect("transferred exactly once");
}

#[tokio::test]
async fn queued_process_error_drop_retains_captures_and_cleanup_order() {
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 0, None);
    let (send, receive) = oneshot::channel();
    let guard = ProcessResponse {
        receive,
        owner: runner.owner.clone(),
        armed: true,
    };
    deliver_result(
        &runner.owner,
        send,
        Err(retained_fixture_error(directory.path())),
    );
    assert!(
        runner.owner.unobserved.lock().expect("sink").is_empty(),
        "queued reply still owns evidence"
    );
    drop(guard);
    assert_eq!(runner.owner.unobserved.lock().expect("sink").len(), 1);
    let error = runner.shutdown().await.expect_err("queued error retained");
    let path = failure_evidence(&error)
        .stdout
        .spool_path()
        .expect("owned spool")
        .to_path_buf();
    assert!(path.exists());
    assert_eq!(
        capture_bytes(&failure_evidence(&error).stdout),
        b"queued-private-prefix"
    );
    assert!(!format!("{error:?}").contains("queued-private-prefix"));
    let ConfctlError::ProcessFailure { source, .. } = &error else {
        panic!("failure evidence wrapper");
    };
    let ConfctlError::Cleanup { primary, secondary } = source.as_ref() else {
        panic!("cleanup wrapper");
    };
    assert!(matches!(primary.as_ref(), ConfctlError::Drain { .. }));
    assert!(matches!(
        secondary.as_slice(),
        [ConfctlError::InputEof, ConfctlError::EventDelivery(_)]
    ));
    runner.shutdown().await.expect("no duplicate");
    drop(error);
    assert!(!path.exists());
}

#[tokio::test]
async fn consumed_process_error_is_not_reported_again() {
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 0, None);
    let (send, receive) = oneshot::channel();
    let guard = ProcessResponse {
        receive,
        owner: runner.owner.clone(),
        armed: true,
    };
    deliver_result(
        &runner.owner,
        send,
        Err(retained_fixture_error(directory.path())),
    );
    let error = guard.receive().await.expect_err("caller consumes error");
    assert_eq!(
        capture_bytes(&failure_evidence(&error).stderr),
        b"private stderr"
    );
    runner
        .shutdown()
        .await
        .expect("consumed error not duplicated");
}

#[tokio::test]
async fn queued_canceled_outcome_drop_does_not_create_failure() {
    use std::os::unix::process::ExitStatusExt;
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 0, None);
    let (send, receive) = oneshot::channel();
    let guard = ProcessResponse {
        receive,
        owner: runner.owner.clone(),
        armed: true,
    };
    deliver_result(
        &runner.owner,
        send,
        Ok(ProcessOutcome {
            status: ExitStatus::from_raw(0),
            stdout: Captured::Memory(b"canceled prefix".to_vec()),
            stderr: Captured::Unavailable,
            stdout_complete: false,
            stderr_complete: false,
            started: Instant::now(),
            ended: Instant::now(),
            termination: TerminationCause::Canceled,
        }),
    );
    drop(guard);
    runner
        .shutdown()
        .await
        .expect("canceled outcome stays normal");
}

#[tokio::test]
async fn login_probe_preserves_original_stdin_flags_identity_and_unread_typeahead() {
    const MARKER: &str = "CONFCTL_TEST_ORIGINAL_LOGIN_STDIN";
    if std::env::var_os(MARKER).is_none() {
        use std::io::Write;
        let (input, mut peer) =
            std::os::unix::net::UnixStream::pair().expect("known original stdin");
        peer.write_all(b"unread-typeahead\n").expect("typeahead");
        let descriptor: OwnedFd = input.into();
        let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
            .args(["--exact","process::tests::login_probe_preserves_original_stdin_flags_identity_and_unread_typeahead","--nocapture"])
            .env(MARKER,"1").stdin(Stdio::from(descriptor)).output().expect("isolated original fd0 fixture");
        assert!(
            output.status.success(),
            "isolated probe failed: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        return;
    }
    let input = std::io::stdin();
    let before = rustix::fs::fstat(&input).expect("original fd0 identity");
    let flags = rustix::fs::fcntl_getfl(&input).expect("original fd0 flags");
    let probe = LoginProbeStdin::original().expect("owned original duplicate");
    let duplicate = rustix::fs::fstat(&probe.0).expect("duplicate identity");
    assert_eq!(
        (before.st_dev, before.st_ino, before.st_mode),
        (duplicate.st_dev, duplicate.st_ino, duplicate.st_mode)
    );
    assert_eq!(
        rustix::fs::fcntl_getfl(&probe.0).expect("duplicate flags"),
        flags
    );
    let directory = tempfile::tempdir().expect("spools");
    let (runner, _receiver) = owner(directory.path(), 128, None);
    // Native lexical applet entry; resolving the symlink changes argv0.
    let mut spec = ProcessSpec::new(
        native_tool::resolve("logname"),
        "native no-read login probe",
    );
    spec.stdin = StdinPolicy::LoginProbe(probe);
    spec.env = vec![
        ("LC_ALL".into(), Some("C".into())),
        ("LANG".into(), Some("C".into())),
    ];
    let result = runner
        .run(spec, ProcessScope::new(CancellationToken::new(), TaskId(1)))
        .await
        .expect("ordinary native login outcome");
    assert!(matches!(result.code(), Some(0 | 1)));
    assert!(result.stdout_complete && result.stderr_complete);
    assert_eq!(
        rustix::fs::fcntl_getfl(&input).expect("unchanged original flags"),
        flags
    );
    let mut remaining = [0; 17];
    let count = rustix::io::read(&input, &mut remaining).expect("queued input remains");
    assert_eq!(&remaining[..count], b"unread-typeahead\n");
    runner.shutdown().await.expect("probe supervisor joined");
}

#[tokio::test]
async fn nix_build_progress_uses_existing_relay_before_child_exit_and_retains_raw_bytes() {
    let directory = tempfile::tempdir().unwrap();
    let release = directory.path().join("release");
    let (runner, mut receiver) = owner(directory.path(), 1024, None);
    let mut spec = shell(
        "printf 'build' >&2; printf \"ing '/nix/store/a.drv'...\\nunknown raw\\n\" >&2; while [ ! -f \"$1\" ]; do sleep 0.01; done; printf '[1]'",
    );
    spec.args
        .extend(["fixture".into(), release.as_os_str().into()]);
    spec.stderr = StreamPolicy::CaptureAndForward;
    let mut scope = ProcessScope::new(CancellationToken::new(), TaskId(9));
    scope.nix_progress = Some(crate::nix::progress::Kind::Build);
    let running = runner.clone();
    let child = tokio::spawn(async move { running.run(spec, scope).await });
    let mut raw = Vec::new();
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            let delivery = receiver.receive().await.unwrap();
            let progress = matches!(
                delivery.event,
                UiEvent::TaskProgress {
                    task: TaskId(9),
                    completed: 1,
                    total: None,
                    ..
                }
            );
            if let UiEvent::Output {
                task,
                stream,
                bytes,
            } = &delivery.event
            {
                assert_eq!(*task, TaskId(9));
                assert_eq!(*stream, OutputStream::Stderr);
                raw.extend_from_slice(bytes);
            }
            assert!(
                !child.is_finished(),
                "progress must precede controlled child completion"
            );
            delivery.acknowledge(Ok(()));
            if progress {
                break;
            }
        }
    })
    .await
    .unwrap();
    std::fs::write(release, b"go").unwrap();
    let consumer = tokio::spawn(async move {
        while let Some(delivery) = receiver.receive().await {
            delivery.acknowledge(Ok(()));
        }
    });
    let outcome = child.await.unwrap().unwrap();
    assert_eq!(outcome.stdout.memory(), Some(&b"[1]"[..]));
    assert_eq!(
        outcome.stderr.memory(),
        Some(&b"building '/nix/store/a.drv'...\nunknown raw\n"[..])
    );
    assert!(raw.starts_with(b"building '/nix/store/a.drv'...\n"));
    runner.shutdown().await.unwrap();
    drop(runner);
    consumer.await.unwrap();
}

#[tokio::test]
async fn nix_build_progress_rejection_joins_child_and_retains_raw_stderr() {
    let directory = tempfile::tempdir().unwrap();
    let (runner, mut receiver) = owner(directory.path(), 1024, None);
    let mut spec = shell("printf \"building '/nix/store/a.drv'...\\n\" >&2; exec sleep 120");
    spec.stderr = StreamPolicy::CaptureAndForward;
    let mut scope = ProcessScope::new(CancellationToken::new(), TaskId(10));
    scope.nix_progress = Some(crate::nix::progress::Kind::Build);
    let consumer = tokio::spawn(async move {
        while let Some(delivery) = receiver.receive().await {
            let reject = matches!(delivery.event, UiEvent::TaskProgress { .. });
            delivery.acknowledge(if reject {
                Err("injected progress output failure".into())
            } else {
                Ok(())
            });
        }
    });
    let error = tokio::time::timeout(Duration::from_secs(5), runner.run(spec, scope))
        .await
        .unwrap()
        .unwrap_err();
    assert!(
        error
            .to_string()
            .contains("injected progress output failure")
    );
    let ConfctlError::ProcessFailure { evidence, .. } = error else {
        panic!("retained process evidence")
    };
    assert!(evidence.status.is_some());
    assert_eq!(
        evidence.stderr.memory(),
        Some(&b"building '/nix/store/a.drv'...\n"[..])
    );
    runner.shutdown().await.unwrap();
    drop(runner);
    consumer.await.unwrap();
}

#[tokio::test]
async fn copy_progress_arrives_before_child_exit_and_failure_joins_it() {
    for reject_line in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let (runner, mut receiver) = owner(directory.path(), 1024, None);
        let mut spec = shell(
            "printf \"copying 2 paths...\\ncopying path '/store/one'...\\n\" >&2; exec sleep 60",
        );
        spec.stderr = StreamPolicy::CaptureAndForward;
        let mut scope = ProcessScope::new(CancellationToken::new(), TaskId(13));
        scope.nix_progress = Some(crate::nix::progress::Kind::Copy {
            host: "logical-host".into(),
        });
        let running = runner.clone();
        let run = tokio::spawn(async move { running.run(spec, scope).await });
        let mut saw_copy_line = false;
        loop {
            let d = tokio::time::timeout(Duration::from_secs(5), receiver.receive())
                .await
                .unwrap()
                .unwrap();
            if matches!(&d.event, UiEvent::Diagnostic {text,..} if text.contains("[logical-host] [1/2]"))
            {
                saw_copy_line = true;
            }
            let task_update = matches!(
                d.event,
                UiEvent::TaskProgress {
                    completed: 1,
                    total: Some(2),
                    ..
                }
            );
            let plain_line = matches!(&d.event, UiEvent::Diagnostic {text,..} if text.contains("[logical-host] [1/2]"));
            if (reject_line && plain_line) || (!reject_line && task_update) {
                assert!(
                    saw_copy_line,
                    "host-attributed plain progress precedes the task update"
                );
                assert!(
                    !run.is_finished(),
                    "progress is observed while the child remains running"
                );
                d.acknowledge(Err("copy progress rejected".into()));
                break;
            }
            d.acknowledge(Ok(()));
        }
        let error = tokio::time::timeout(Duration::from_secs(5), run)
            .await
            .unwrap()
            .unwrap()
            .unwrap_err();
        assert!(error.to_string().contains("copy progress rejected"));
        let evidence = failure_evidence(&error);
        assert!(evidence.status.is_some());
        assert_eq!(
            capture_bytes(&evidence.stderr),
            b"copying 2 paths...\ncopying path '/store/one'...\n"
        );
        runner.shutdown().await.unwrap();
    }
}

#[tokio::test]
async fn pager_actual_epipe_is_closed_and_keeps_nonzero_native_outcome() {
    let directory = tempfile::tempdir().unwrap();
    let (runner, _receiver) = owner(directory.path(), 64, None);
    let cancel = CancellationToken::new();
    let (mut writer, input) = PagerInput::channel();
    let mut spec = shell("exec 0<&-; printf 'real pager output'; exit 7");
    spec.stdin = StdinPolicy::Pager(input);
    let produce = async {
        assert_eq!(
            writer
                .write(&vec![b'x'; 1024 * 1024], &cancel)
                .await
                .unwrap(),
            WrittenOrClosed::Closed
        );
        assert!(!cancel.is_cancelled());
        drop(writer);
    };
    let (result, ()) = tokio::join!(
        runner.run(spec, ProcessScope::new(cancel.clone(), TaskId(1))),
        produce
    );
    let result = result.unwrap();
    assert_eq!(result.code(), Some(7));
    assert_eq!(result.termination, TerminationCause::Exited);
    assert_eq!(result.stdout.memory().unwrap(), b"real pager output");
    runner.shutdown().await.unwrap();
}

#[tokio::test]
async fn pager_chunk_delivery_is_acknowledged_and_finish_closes_stdin() {
    let directory = tempfile::tempdir().unwrap();
    let (runner, _receiver) = owner(directory.path(), 64, None);
    let (mut writer, input) = PagerInput::channel();
    let bytes = b"report\0\xff\n".repeat(CHUNK);
    let mut spec = shell("cat");
    spec.stdin = StdinPolicy::Pager(input);
    let produce = async {
        assert_eq!(
            writer
                .write(&bytes, &CancellationToken::new())
                .await
                .unwrap(),
            WrittenOrClosed::Written
        );
        drop(writer);
    };
    let (outcome, ()) = tokio::join!(
        runner.run(spec, ProcessScope::new(CancellationToken::new(), TaskId(1))),
        produce
    );
    let outcome = outcome.unwrap();
    assert!(outcome.status.success());
    assert_eq!(crate::nix::bytes(&outcome.stdout).await.unwrap(), bytes);
    runner.shutdown().await.unwrap();
}

#[tokio::test]
async fn pager_lost_channel_and_non_epipe_write_and_shutdown_are_errors() {
    use std::{
        pin::Pin,
        task::{Context, Poll},
    };
    struct FailedWrite {
        error: Option<io::Error>,
        shutdown: bool,
    }
    impl AsyncWrite for FailedWrite {
        fn poll_write(
            mut self: Pin<&mut Self>,
            _: &mut Context<'_>,
            bytes: &[u8],
        ) -> Poll<io::Result<usize>> {
            if self.shutdown {
                Poll::Ready(Ok(bytes.len()))
            } else {
                Poll::Ready(Err(self.error.take().unwrap()))
            }
        }
        fn poll_flush(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<io::Result<()>> {
            Poll::Ready(Ok(()))
        }
        fn poll_shutdown(mut self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<io::Result<()>> {
            Poll::Ready(Err(self.error.take().unwrap()))
        }
    }
    for error in [
        io::Error::from_raw_os_error(::nix::libc::EIO),
        io::Error::new(io::ErrorKind::BrokenPipe, "not a pipe EPIPE"),
    ] {
        let (mut writer, input) = PagerInput::channel();
        let mut failing = FailedWrite {
            error: Some(error),
            shutdown: false,
        };
        let cancel = CancellationToken::new();
        let (native, acknowledged) = tokio::join!(
            pager_input(&mut failing, input),
            writer.write(b"report", &cancel)
        );
        assert!(native.is_err() && acknowledged.is_err());
    }
    let (writer, input) = PagerInput::channel();
    drop(writer);
    let mut failing = FailedWrite {
        error: Some(io::Error::from_raw_os_error(::nix::libc::EIO)),
        shutdown: true,
    };
    assert!(pager_input(&mut failing, input).await.is_err());
    let (mut writer, input) = PagerInput::channel();
    drop(input);
    assert!(
        writer
            .write(b"report", &CancellationToken::new())
            .await
            .is_err()
    );
    let (mut writer, mut input) = PagerInput::channel();
    let consume = async {
        drop(input.0.recv().await.unwrap());
    };
    let cancel = CancellationToken::new();
    let ((), result) = tokio::join!(consume, writer.write(b"report", &cancel));
    assert!(result.is_err());
}

#[tokio::test]
async fn pager_cancellation_wakes_pending_chunk_and_joins_child() {
    let directory = tempfile::tempdir().unwrap();
    let (runner, _receiver) = owner(directory.path(), 64, None);
    let cancel = CancellationToken::new();
    let (mut writer, input) = PagerInput::channel();
    let mut spec = shell("exec sleep 30");
    spec.stdin = StdinPolicy::Pager(input);
    let produce = async {
        let bytes = vec![b'x'; 1024 * 1024];
        let pending = writer.write(&bytes, &cancel);
        tokio::pin!(pending);
        tokio::select! { result = &mut pending => panic!("unexpected result: {result:?}"), _ = tokio::time::sleep(Duration::from_millis(30)) => {} }
        cancel.cancel();
        assert!(pending.await.unwrap_err().is_cancelled());
    };
    let (outcome, ()) = tokio::join!(
        runner.run(spec, ProcessScope::new(cancel.clone(), TaskId(1))),
        produce
    );
    assert_eq!(outcome.unwrap().termination, TerminationCause::Canceled);
    runner.shutdown().await.unwrap();
}

#[tokio::test]
async fn ordinary_bytes_file_epipe_and_source_failure_remain_failures() {
    for file in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let (runner, _receiver) = owner(directory.path(), 64, None);
        let bytes = vec![b'x'; 1024 * 1024];
        let source = directory.path().join("input");
        std::fs::write(&source, &bytes).unwrap();
        let mut spec = shell("exec 0<&-; exit 0");
        spec.stdin = if file {
            StdinPolicy::File(source)
        } else {
            StdinPolicy::Bytes(bytes)
        };
        assert!(
            runner
                .run(spec, ProcessScope::new(CancellationToken::new(), TaskId(1)))
                .await
                .is_err()
        );
        runner.shutdown().await.unwrap();
    }
    let directory = tempfile::tempdir().unwrap();
    let (runner, _receiver) = owner(directory.path(), 64, None);
    let mut spec = shell("cat");
    spec.stdin = StdinPolicy::File(directory.path().join("missing"));
    assert!(
        runner
            .run(spec, ProcessScope::new(CancellationToken::new(), TaskId(1)))
            .await
            .is_err()
    );
    runner.shutdown().await.unwrap();
}

#[tokio::test]
async fn gc_labelled_relays_preserve_exact_raw_captures_and_log_without_ui_duplicate() {
    let directory = tempfile::tempdir().unwrap();
    let log_path = directory.path().join("native.log");
    let log = ProcessLog::new(File::create(&log_path).await.unwrap());
    let (runner, mut receiver) = owner(directory.path(), 64, Some(log));
    let output = GcOutput::new("host");
    let consumer = tokio::spawn(async move {
        let mut events = Vec::new();
        while let Some(delivery) = receiver.receive().await {
            let (event, ack) = delivery.into_parts();
            events.push(event);
            ack.acknowledge(Ok(()));
        }
        events
    });
    let mut spec = shell(
        "printf \"deleting '/nix/store/out'\\nordinary\\377\\n\"; printf \"deleting '/nix/store/err'\\n2 store paths deleted\\n\" >&2; exit 7",
    );
    spec.stdout = StreamPolicy::CaptureAndForward;
    spec.stderr = StreamPolicy::CaptureAndForward;
    let mut scope = ProcessScope::new(CancellationToken::new(), TaskId(12));
    scope.gc_output = Some(output.clone());
    let result = runner.run(spec, scope).await.unwrap();
    assert_eq!(result.code(), Some(7));
    assert_eq!(
        crate::nix::bytes(&result.stdout).await.unwrap(),
        b"deleting '/nix/store/out'\nordinary\xff\n"
    );
    assert_eq!(
        crate::nix::bytes(&result.stderr).await.unwrap(),
        b"deleting '/nix/store/err'\n2 store paths deleted\n"
    );
    assert_eq!(
        output.summary().await.as_deref(),
        Some("2 store paths deleted")
    );
    runner.shutdown().await.unwrap();
    drop(runner);
    let events = consumer.await.unwrap();
    let counts = events
        .iter()
        .filter_map(|event| {
            if let UiEvent::TaskProgress { completed, .. } = event {
                Some(*completed)
            } else {
                None
            }
        })
        .collect::<Vec<_>>();
    assert_eq!(counts, [1, 2]);
    assert!(
        events
            .iter()
            .filter_map(|event| if let UiEvent::Output { bytes, .. } = event {
                Some(bytes)
            } else {
                None
            })
            .all(|bytes| bytes.starts_with(b"host> "))
    );
    let raw = std::fs::read(log_path).unwrap();
    assert!(!raw.windows(6).any(|b| b == b"host> "));
    assert!(raw.windows(9).any(|b| b == b"ordinary\xff"));
}
