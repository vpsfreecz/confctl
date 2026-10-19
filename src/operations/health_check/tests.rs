use super::*;
use serde_json::json;
use std::{
    collections::VecDeque,
    os::unix::process::ExitStatusExt,
    sync::{
        Mutex,
        atomic::{AtomicUsize, Ordering},
    },
};

fn machine(checks: Value) -> Machine {
    Machine::from_value(json!({"name":"host","key":"flake-key","metaConfig":{
        "spin":"nixos","managed":true,"host":{"target":"localhost","port":2222},
        "replace":"{name}","count":23,"values":["a",true,null],"healthChecks":checks
    }}))
    .unwrap()
}
fn check() -> Check {
    from_metadata(&machine(
        json!({"builderCommands":[{"command":["check-tool"],"timeout":0,"cooldown":0}]}),
    ))
    .unwrap()
    .remove(0)
}
fn outcome(raw: i32, stdout: &[u8], stderr: &[u8]) -> ProcessOutcome {
    ProcessOutcome {
        status: std::process::ExitStatus::from_raw(raw),
        stdout: Captured::Memory(stdout.into()),
        stderr: Captured::Memory(stderr.into()),
        stdout_complete: true,
        stderr_complete: true,
        started: Instant::now(),
        ended: Instant::now(),
        termination: TerminationCause::Exited,
    }
}
#[test]
fn metadata_order_and_single_pass_ruby_attribute_expansion_keep_argv_boundaries() {
    let m = machine(
        json!({"machineCommands":[{"command":["remote","{checks}","{name}","{key}","{host.port}","x{missing}y","{replace}","{values}"]}],
        "builderCommands":[{"command":["builder","{checks}","{machineKey}-{flakeKey}","{}",""]}]}),
    );
    let checks = from_metadata(&m).unwrap();
    assert_eq!(checks.len(), 2);
    let Kind::Command(first) = &checks[0].kind else {
        panic!()
    };
    assert!(first.remote);
    assert_eq!(
        first.argv,
        [
            "remote",
            "0",
            "host",
            "flake-key",
            "2222",
            "xy",
            "{name}",
            "[\"a\", true, nil]"
        ]
    );
    let Kind::Command(second) = &checks[1].kind else {
        panic!()
    };
    assert!(!second.remote);
    assert_eq!(
        second.argv,
        ["builder", "1", "flake-key-flake-key", "{}", ""]
    );
    let spec = checks[1]
        .spec(Path::new("/conf"), Some(OsStr::new("unused")))
        .unwrap();
    assert_eq!(spec.program, Path::new("builder"));
    assert_eq!(spec.args[0], "1");
    for value in [
        json!([]),
        json!({"unknown":[]}),
        json!({"builderCommands":false}),
        json!({"builderCommands":[{"command":[]}]}),
        json!({"builderCommands":[{"command":["tool",2]}]}),
        json!({"builderCommands":[{"command":["tool"],"timeout":-1}]}),
        json!({"builderCommands":[{"command":["tool"],"standardOutput":{"include":false}}]}),
        json!({"builderCommands":[{"command":["tool","{count.key}"]}]}),
        json!({"builderCommands":[{"command":["tool\u{0}"]}]}),
    ] {
        assert!(from_metadata(&machine(value)).is_err());
    }
}
#[test]
fn byte_predicates_preserve_status_and_first_output_error_precedence() {
    let m = machine(
        json!({"builderCommands":[{"command":["tool"],"exitStatus":7,
        "standardOutput":{"match":"ok\n","include":["missing"],"exclude":["ok"]},
        "standardError":{"match":"expected"}}]}),
    );
    let mut c = from_metadata(&m).unwrap().remove(0);
    assert_eq!(compare(&c, outcome(0, b"ok", b"wrong")).unwrap().len(), 2);
    let errors = compare(&c, outcome(7 << 8, b"ok", b"wrong")).unwrap();
    assert_eq!(errors.len(), 1);
    assert!(errors[0].starts_with("stdout does not match"));
    let Kind::Command(cmd) = &mut c.kind else {
        panic!()
    };
    cmd.stdout.exact = None;
    let errors = compare(&c, outcome(7 << 8, b"ok\xff\n", b"wrong")).unwrap();
    assert!(errors[0].starts_with("stdout does not include"));
    let Kind::Command(cmd) = &mut c.kind else {
        panic!()
    };
    cmd.stdout.include = vec![b"ok".to_vec(), vec![0xff]];
    let errors = compare(&c, outcome(7 << 8, b"ok\xff\n", b"wrong")).unwrap();
    assert!(errors[0].starts_with("stdout includes"));
    let Kind::Command(cmd) = &mut c.kind else {
        panic!()
    };
    cmd.stdout.exclude.clear();
    cmd.stderr.exact = None;
    assert!(
        compare(&c, outcome(7 << 8, b"ok\xff\n", b"wrong"))
            .unwrap()
            .is_empty()
    );
    let signal = compare(&c, outcome(15, b"ok\xff\n", b"wrong")).unwrap();
    assert!(signal[0].contains("signal 15"));
    let mut partial = outcome(7 << 8, b"prefix", b"stderr");
    partial.stdout_complete = false;
    assert!(
        matches!(compare(&c,partial).unwrap_err(),ConfctlError::ProcessFailure {evidence,..} if !evidence.stdout_complete&&evidence.status.unwrap().code()==Some(7))
    );
    assert!(contains(b"", b""));
}
struct Runner {
    outcomes: Mutex<VecDeque<ProcessOutcome>>,
    calls: AtomicUsize,
}
impl ProcessRunner for Runner {
    async fn run(
        &self,
        spec: ProcessSpec,
        _: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        assert_eq!(spec.stdout, StreamPolicy::CaptureAndForward);
        assert_eq!(spec.stderr, StreamPolicy::CaptureAndForward);
        self.calls.fetch_add(1, Ordering::SeqCst);
        Ok(self.outcomes.lock().unwrap().pop_front().unwrap())
    }
}
#[tokio::test]
async fn retry_success_retains_failed_attempts_and_zero_timeout_still_runs_once() {
    for retry in [false, true] {
        let tmp = tempfile::tempdir().unwrap();
        let mut c = check();
        let Kind::Command(command) = &mut c.kind else {
            panic!()
        };
        if retry {
            command.timeout = Duration::from_secs(1);
            command.cooldown = Duration::from_millis(1);
        }
        let runner = Runner {
            outcomes: Mutex::new(VecDeque::from([
                outcome(1 << 8, b"failed", b"bad"),
                outcome(0, b"ok", b""),
            ])),
            calls: 0.into(),
        };
        let cancel = CancellationToken::new();
        let (events, mut rx) = EventSink::channel(1);
        let consumer = tokio::spawn(async move {
            let mut diagnostics = String::new();
            let mut finals = Vec::new();
            let mut started = 0;
            while let Some(d) = rx.receive().await {
                let (event, ack) = d.into_parts();
                match event {
                    UiEvent::Diagnostic { text, .. } => diagnostics.push_str(&text),
                    UiEvent::TaskStarted { .. } => started += 1,
                    UiEvent::TaskFinished { state, .. } => finals.push(state),
                    _ => {}
                }
                ack.acknowledge(Ok(()));
            }
            (diagnostics, finals, started)
        });
        let ctx = CheckContext {
            runner: &runner,
            events: &events,
            cancellation: &cancel,
            root: tmp.path(),
            ssh_config: None,
            log: tmp.path(),
            invocation: None,
        };
        let failures = run_checks(&ctx, &[c], 1, TaskId(7)).await.unwrap();
        assert_eq!(failures.is_empty(), retry);
        assert_eq!(
            runner.calls.load(Ordering::SeqCst),
            if retry { 2 } else { 1 }
        );
        drop(events);
        let (text, finals, started) = consumer.await.unwrap();
        assert_eq!(started, 1);
        assert!(text.contains("failed attempt 1"));
        assert_eq!(
            finals,
            [if retry {
                TaskState::Succeeded
            } else {
                TaskState::Failed
            }]
        );
    }
}
struct ParallelRunner {
    active: AtomicUsize,
    peak: AtomicUsize,
    calls: Mutex<Vec<TaskId>>,
    infrastructure: bool,
}
impl ProcessRunner for ParallelRunner {
    async fn run(
        &self,
        _: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        let active = self.active.fetch_add(1, Ordering::SeqCst) + 1;
        self.peak.fetch_max(active, Ordering::SeqCst);
        self.calls.lock().unwrap().push(scope.task);
        let canceled = tokio::select! {
            _=scope.cancellation.cancelled()=>true,
            _=tokio::time::sleep(Duration::from_millis(if scope.task==TaskId(1){20}else{1}))=>false,
        };
        self.active.fetch_sub(1, Ordering::SeqCst);
        if canceled {
            return Err(ConfctlError::Cancelled);
        }
        if self.infrastructure && scope.task == TaskId(2) {
            return Err(ConfctlError::State("controlled spawn failure".into()));
        }
        Ok(outcome(
            if scope.task == TaskId(2) { 1 << 8 } else { 0 },
            b"",
            b"",
        ))
    }
}
#[tokio::test]
async fn borrowed_batch_bounds_and_joins_with_completed_results_and_unentered_finals() {
    for (infrastructure, reject) in [(false, false), (true, false), (false, true)] {
        let tmp = tempfile::tempdir().unwrap();
        let cancel = CancellationToken::new();
        let (events, mut rx) = EventSink::channel(1);
        let runner = ParallelRunner {
            active: 0.into(),
            peak: 0.into(),
            calls: Mutex::new(Vec::new()),
            infrastructure,
        };
        let token = cancel.clone();
        let consumer = tokio::spawn(async move {
            let mut finals = Vec::new();
            while let Some(d) = rx.receive().await {
                let (event, ack) = d.into_parts();
                if let UiEvent::TaskFinished { task, state } = event {
                    finals.push((task, state));
                    if reject && task == TaskId(2) {
                        token.cancel();
                        ack.acknowledge(Err("controlled final delivery failure".into()));
                        continue;
                    }
                }
                ack.acknowledge(Ok(()));
            }
            finals
        });
        let ctx = CheckContext {
            runner: &runner,
            events: &events,
            cancellation: &cancel,
            root: tmp.path(),
            ssh_config: None,
            log: tmp.path(),
            invocation: None,
        };
        let result = run_checks(&ctx, &[check(), check(), check(), check()], 2, TaskId(1)).await;
        if infrastructure || reject {
            assert!(result.unwrap_err().to_string().contains(if infrastructure {
                "controlled spawn failure"
            } else {
                "controlled final delivery failure"
            }));
        } else {
            assert_eq!(
                result.unwrap().iter().map(|f| f.index).collect::<Vec<_>>(),
                [1]
            );
        }
        assert_eq!(runner.active.load(Ordering::SeqCst), 0);
        assert_eq!(runner.peak.load(Ordering::SeqCst), 2);
        if infrastructure || reject {
            assert_eq!(runner.calls.lock().unwrap().len(), 2);
        }
        drop(events);
        let finals = consumer.await.unwrap();
        assert_eq!(finals.len(), 4);
        if reject {
            assert!(finals.contains(&(TaskId(2), TaskState::Failed)));
        }
    }
}

#[test]
fn systemd_native_failure_signal_and_incomplete_capture_precede_property_assessment() {
    let checks = from_metadata(&machine(
        json!({"systemd":{"enable":true,"systemProperties":[
        {"property":"SystemState","value":"running","timeout":90,"cooldown":5},
        {"property":"NFailedUnits","value":"0","timeout":0,"cooldown":0}
    ],"unitProperties":{}}}),
    ))
    .unwrap();
    let c = &checks[0];
    assert_eq!(c.timeout(), Duration::ZERO);
    assert_eq!(c.cooldown(), Duration::ZERO);
    for raw in [2 << 8, 15] {
        let errors = compare(c, outcome(raw, b"", b"native failure")).unwrap();
        assert_eq!(errors.len(), 1);
        assert!(errors[0].starts_with("expected exit status 0"));
    }
    let mut partial = outcome(0, b"SystemState=running\n", b"");
    partial.stderr_complete = false;
    assert!(matches!(
        compare(c, partial),
        Err(ConfctlError::ProcessFailure { .. })
    ));
    let errors = compare(c, outcome(0, b"SystemState=degraded\n", b"")).unwrap();
    assert_eq!(errors.len(), 2);
}

#[tokio::test]
async fn success_diagnostic_ack_cancellation_preserves_completed_result_and_stops_later_checks() {
    for reject in [false, true] {
        let tmp = tempfile::tempdir().unwrap();
        let cancel = CancellationToken::new();
        let token = cancel.clone();
        let runner = Runner {
            outcomes: Mutex::new(VecDeque::from([outcome(0, b"", b"")])),
            calls: 0.into(),
        };
        let (events, mut rx) = EventSink::channel(1);
        let consumer = tokio::spawn(async move {
            let mut finals = Vec::new();
            while let Some(d) = rx.receive().await {
                let (event, ack) = d.into_parts();
                if matches!(&event,UiEvent::Diagnostic {text,..} if text.contains("succeeded on attempt"))
                {
                    token.cancel();
                    if reject {
                        ack.acknowledge(Err("controlled success diagnostic failure".into()));
                        continue;
                    }
                }
                if let UiEvent::TaskFinished { task, state } = event {
                    finals.push((task, state));
                }
                ack.acknowledge(Ok(()));
            }
            finals
        });
        let ctx = CheckContext {
            runner: &runner,
            events: &events,
            cancellation: &cancel,
            root: tmp.path(),
            ssh_config: None,
            log: tmp.path(),
            invocation: None,
        };
        let error = run_checks(&ctx, &[check(), check()], 1, TaskId(1))
            .await
            .unwrap_err();
        if reject {
            assert!(
                error
                    .to_string()
                    .contains("controlled success diagnostic failure")
            );
        } else {
            assert!(error.is_cancelled());
        }
        assert_eq!(runner.calls.load(Ordering::SeqCst), 1);
        drop(events);
        assert_eq!(
            consumer.await.unwrap(),
            [
                (TaskId(1), TaskState::Succeeded),
                (TaskId(2), TaskState::Canceled)
            ]
        );
    }
}
