use super::*;
use std::{
    os::unix::process::ExitStatusExt,
    sync::{Arc, Mutex},
    time::Instant,
};

fn outcome(status: i32) -> ProcessOutcome {
    ProcessOutcome {
        status: std::process::ExitStatus::from_raw(status),
        stdout: Captured::Memory(b"launcher stdout".to_vec()),
        stderr: Captured::Memory(b"launcher stderr".to_vec()),
        stdout_complete: true,
        stderr_complete: true,
        started: Instant::now(),
        ended: Instant::now(),
        termination: TerminationCause::Exited,
    }
}

struct Runner {
    calls: Mutex<usize>,
    result: Mutex<Option<Result<ProcessOutcome, ConfctlError>>>,
}
impl ProcessRunner for Runner {
    async fn run(
        &self,
        spec: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        assert_eq!(spec.program, Path::new("nix-shell"));
        assert_eq!(scope.task, TaskId(1));
        assert_eq!(scope.phase, Some(Phase::SshCommands));
        assert_eq!(spec.stdout, StreamPolicy::CaptureAndForward);
        assert_eq!(spec.stderr, StreamPolicy::CaptureAndForward);
        assert!(matches!(spec.stdin, crate::process::StdinPolicy::Null));
        assert_eq!(spec.env, [("shellHook".into(), None)]);
        assert!(!spec.clear_env);
        *self.calls.lock().unwrap() += 1;
        self.result.lock().unwrap().take().unwrap()
    }
}

#[tokio::test]
async fn launcher_lifecycle_preserves_native_results_and_checks_acknowledged_cancellation() {
    for (boundary, native_status) in [
        ("pending", 0),
        ("started", 0),
        ("log", 0),
        ("finished", 0),
        ("diagnostic", 7 << 8),
        ("none", 0),
        ("none", 7 << 8),
        ("none", 15),
    ] {
        let runner = Runner {
            calls: Mutex::new(0),
            result: Mutex::new(Some(Ok(outcome(native_status)))),
        };
        let cancel = CancellationToken::new();
        let consumer_cancel = cancel.clone();
        let (events, mut receiver) = EventSink::channel(1);
        let recorded = Arc::new(Mutex::new(Vec::new()));
        let seen = recorded.clone();
        let consumer = tokio::spawn(async move {
            while let Some(delivery) = receiver.receive().await {
                let cancel_here = match &delivery.event {
                    UiEvent::TaskPending { .. } => boundary == "pending",
                    UiEvent::TaskStarted { .. } => boundary == "started",
                    UiEvent::LogAvailable { .. } => boundary == "log",
                    UiEvent::TaskFinished { .. } => boundary == "finished",
                    UiEvent::Diagnostic { .. } => boundary == "diagnostic",
                    _ => false,
                };
                if cancel_here {
                    consumer_cancel.cancel();
                }
                let (event, ack) = delivery.into_parts();
                seen.lock().unwrap().push(event);
                ack.acknowledge(Ok(()));
            }
        });
        let spec = launcher_spec(&[], Path::new(".")).unwrap();
        let result = launch(&runner, &events, &cancel, spec, Path::new("launcher.log")).await;
        if boundary == "none" {
            result.expect("ordinary native status keeps compatible invocation success");
        } else {
            assert!(result.unwrap_err().is_cancelled());
        }
        drop(events);
        consumer.await.unwrap();
        let entered = !matches!(boundary, "pending" | "started");
        assert_eq!(*runner.calls.lock().unwrap(), usize::from(entered));
        let recorded = recorded.lock().unwrap();
        assert_eq!(
            recorded
                .iter()
                .filter(|event| matches!(
                    event,
                    UiEvent::TaskPending {
                        task: TaskId(1),
                        ..
                    }
                ))
                .count(),
            1
        );
        assert_eq!(
            recorded
                .iter()
                .filter(|event| matches!(
                    event,
                    UiEvent::TaskStarted {
                        task: TaskId(1),
                        ..
                    }
                ))
                .count(),
            usize::from(boundary != "pending")
        );
        let finals = recorded
            .iter()
            .filter_map(|event| match event {
                UiEvent::TaskFinished {
                    task: TaskId(1),
                    state,
                } => Some(*state),
                _ => None,
            })
            .collect::<Vec<_>>();
        assert_eq!(
            finals,
            [if !entered {
                TaskState::Canceled
            } else if native_status == 0 {
                TaskState::Succeeded
            } else {
                TaskState::Failed
            }]
        );
    }
}

#[tokio::test]
async fn delivery_failure_remains_an_invocation_error_after_native_success() {
    let runner = Runner {
        calls: Mutex::new(0),
        result: Mutex::new(Some(Ok(outcome(0)))),
    };
    let (events, mut receiver) = EventSink::channel(1);
    let consumer = tokio::spawn(async move {
        let mut final_state = None;
        while let Some(delivery) = receiver.receive().await {
            let reject = matches!(delivery.event, UiEvent::LogAvailable { .. });
            if let UiEvent::TaskFinished { state, .. } = delivery.event {
                final_state = Some(state);
            }
            delivery.acknowledge(if reject {
                Err("fixture log display failure".into())
            } else {
                Ok(())
            });
        }
        final_state
    });
    let error = launch(
        &runner,
        &events,
        &CancellationToken::new(),
        launcher_spec(&[], Path::new(".")).unwrap(),
        Path::new("launcher.log"),
    )
    .await
    .unwrap_err();
    assert!(
        matches!(error, ConfctlError::EventDelivery(message) if message == "fixture log display failure")
    );
    drop(events);
    assert_eq!(consumer.await.unwrap(), Some(TaskState::Succeeded));
}

#[test]
fn capture_and_cancel_failures_keep_the_original_partial_process_evidence() {
    for canceled in [false, true] {
        let mut result = outcome(7 << 8);
        result.stderr_complete = false;
        if canceled {
            result.termination = TerminationCause::Canceled;
        }
        let error = launcher_result(result).unwrap_err();
        assert_eq!(error.is_cancelled(), canceled);
        let ConfctlError::ProcessFailure { evidence, .. } = error else {
            panic!("lost capture");
        };
        assert_eq!(evidence.status.unwrap().code(), Some(7));
        assert!(matches!(&evidence.stdout, Captured::Memory(bytes) if bytes == b"launcher stdout"));
        assert!(matches!(&evidence.stderr, Captured::Memory(bytes) if bytes == b"launcher stderr"));
        assert!(!evidence.stderr_complete);
    }
    assert!(!launcher_result(outcome(15)).unwrap());
    assert!(!launcher_result(outcome(7 << 8)).unwrap());
    assert!(launcher_result(outcome(0)).unwrap());
}
