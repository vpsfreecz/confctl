use super::*;
use crate::{
    domain::{ManagedFilter, Targets},
    events::EventReceiver,
};
use serde_json::json;
use std::{
    collections::VecDeque,
    ffi::OsString,
    os::unix::{ffi::OsStringExt, process::ExitStatusExt},
    sync::{Arc, Mutex},
    time::Instant,
};

fn machine(name: &str, target: &str) -> Machine {
    Machine::from_value(json!({"name":name,"metaConfig":{"managed":true,"host":{"target":target}}}))
        .unwrap()
}
fn request(parallel: bool, aggregate: bool) -> SshRequest {
    SshRequest {
        targets: Targets::default(),
        managed: ManagedFilter::Managed,
        command: vec!["requested".into()],
        parallel,
        aggregate,
        input: SshInput::None,
    }
}
fn outcome(status: i32, output: &[u8]) -> ProcessOutcome {
    ProcessOutcome {
        status: std::process::ExitStatus::from_raw(status),
        stdout: Captured::Memory(output.to_vec()),
        stderr: Captured::Unavailable,
        stdout_complete: true,
        stderr_complete: false,
        started: Instant::now(),
        ended: Instant::now(),
        termination: TerminationCause::Exited,
    }
}
struct Runner {
    results: Mutex<VecDeque<Result<ProcessOutcome, ConfctlError>>>,
    calls: Mutex<Vec<TaskId>>,
    barrier: Option<tokio::sync::Barrier>,
}
impl ProcessRunner for Runner {
    async fn run(
        &self,
        spec: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        assert_eq!(scope.phase, Some(Phase::SshCommands));
        assert_eq!(spec.stderr, StreamPolicy::MergeToStdout);
        assert!(matches!(spec.stdin, StdinPolicy::Null));
        self.calls.lock().unwrap().push(scope.task);
        if let Some(barrier) = &self.barrier {
            barrier.wait().await;
        }
        self.results
            .lock()
            .unwrap()
            .pop_front()
            .expect("only admitted hosts run")
    }
}
async fn collect(mut receiver: EventReceiver, events: Arc<Mutex<Vec<UiEvent>>>) {
    while let Some(delivery) = receiver.receive().await {
        let (event, ack) = delivery.into_parts();
        events.lock().unwrap().push(event);
        ack.acknowledge(Ok(()));
    }
}
fn row(host: &str, status: i32, bytes: &[u8]) -> Row {
    Row {
        task: TaskId(1),
        host: host.into(),
        status: NativeStatus {
            code: Some(status),
            signal: None,
        },
        output: bytes.to_vec(),
    }
}

#[test]
fn interactive_local_uses_injected_passwd_shell_and_remote_does_not_lookup_it() {
    let shell = PathBuf::from(OsString::from_vec(b"/passwd-shell-\xff".to_vec()));
    let spec = interactive_spec(
        &machine("local", "localhost"),
        Some(OsStr::new("ignored")),
        || Ok(shell.clone()),
    )
    .unwrap();
    assert_eq!(spec.program, shell);
    assert!(spec.args.is_empty());
    assert!(matches!(spec.stdin, StdinPolicy::Foreground));
    assert_eq!(spec.stdout, StreamPolicy::Foreground);
    assert_eq!(spec.stderr, StreamPolicy::Foreground);
    let fallback = Machine::from_value(json!({"name":"localhost","metaConfig":{}})).unwrap();
    assert_eq!(
        interactive_spec(&fallback, None, || Ok("/fallback-passwd-shell".into()))
            .unwrap()
            .program,
        Path::new("/fallback-passwd-shell")
    );
    let spec = interactive_spec(&machine("remote", "remote.invalid"), None, || {
        panic!("remote shell must not query local passwd")
    })
    .unwrap();
    assert_eq!(spec.program, Path::new("ssh"));
    assert_eq!(spec.args, ["-l", "root", "remote.invalid"]);
}

#[tokio::test]
async fn aggregation_requires_equal_native_status_and_exact_bytes_with_sorted_labels() {
    let (sink, receiver) = EventSink::channel(1);
    let events = Arc::new(Mutex::new(Vec::new()));
    let consumer = tokio::spawn(collect(receiver, events.clone()));
    aggregate(
        &sink,
        &[
            row("z", 0, b"same\0\xff"),
            row("a", 0, b"same\0\xff"),
            row("failed", 7, b"same\0\xff"),
            row("newline", 0, b"same\0\xff\n"),
        ],
    )
    .await
    .unwrap();
    drop(sink);
    consumer.await.unwrap();
    let events = events.lock().unwrap();
    let outputs = events
        .iter()
        .filter_map(|event| match event {
            UiEvent::Output { bytes, .. } => Some(bytes.as_slice()),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(outputs.len(), 3);
    assert_eq!(outputs[0], b"a, z:\nExit status: 0\nsame\0\xff\n\n");
    assert!(outputs[1].starts_with(b"failed:\nExit status: 7\n"));
    assert!(outputs[2].starts_with(b"newline:\nExit status: 0\n"));
    assert_ne!(
        NativeStatus {
            code: Some(15),
            signal: None
        },
        NativeStatus {
            code: None,
            signal: Some(15)
        }
    );
}

#[tokio::test]
async fn borrowed_parallel_futures_join_and_report_actual_completed_and_failed_counts() {
    let runner = Runner {
        results: Mutex::new(VecDeque::from([
            Ok(outcome(0, b"first")),
            Ok(outcome(7 << 8, b"failure")),
        ])),
        calls: Mutex::new(Vec::new()),
        barrier: Some(tokio::sync::Barrier::new(2)),
    };
    let machines = [machine("a", "a.invalid"), machine("b", "b.invalid")];
    let (sink, receiver) = EventSink::channel(1);
    let events = Arc::new(Mutex::new(Vec::new()));
    let consumer = tokio::spawn(collect(receiver, events.clone()));
    let cancellation = CancellationToken::new();
    commands(
        &Execution {
            runner: &runner,
            events: &sink,
            cancellation: &cancellation,
            root: Path::new("/tmp"),
            config: None,
            live: false,
        },
        &machines.iter().collect::<Vec<_>>(),
        &request(true, false),
    )
    .await
    .unwrap();
    drop(sink);
    consumer.await.unwrap();
    assert_eq!(runner.calls.lock().unwrap().len(), 2);
    let events = events.lock().unwrap();
    let progress = events
        .iter()
        .filter_map(|event| match event {
            UiEvent::TaskProgress {
                task: TaskId(0),
                completed,
                total,
                ..
            } => Some((*completed, *total)),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(progress, [(1, Some(2)), (2, Some(2))]);
    assert!(events.contains(&UiEvent::TaskFinished {
        task: TaskId(0),
        state: TaskState::Failed
    }));
    assert!(events.iter().any(|event|matches!(event,UiEvent::Output{bytes,..} if bytes.windows(b"failure".len()).any(|part|part==b"failure"))));
}

#[tokio::test]
async fn first_serial_spawn_failure_finishes_later_admitted_hosts_without_entry() {
    let runner = Runner {
        results: Mutex::new(VecDeque::from([Err(ConfctlError::Spawn {
            description: "requested".into(),
            source: io::Error::other("fixture"),
        })])),
        calls: Mutex::new(Vec::new()),
        barrier: None,
    };
    let machines = [
        machine("a", "a.invalid"),
        machine("b", "b.invalid"),
        machine("c", "c.invalid"),
    ];
    let (sink, receiver) = EventSink::channel(1);
    let events = Arc::new(Mutex::new(Vec::new()));
    let consumer = tokio::spawn(collect(receiver, events.clone()));
    let cancellation = CancellationToken::new();
    let result = commands(
        &Execution {
            runner: &runner,
            events: &sink,
            cancellation: &cancellation,
            root: Path::new("/tmp"),
            config: None,
            live: false,
        },
        &machines.iter().collect::<Vec<_>>(),
        &request(false, false),
    )
    .await;
    drop(sink);
    consumer.await.unwrap();
    assert!(matches!(result, Err(ConfctlError::Spawn { .. })));
    assert_eq!(*runner.calls.lock().unwrap(), [TaskId(1)]);
    let events = events.lock().unwrap();
    for task in [TaskId(2), TaskId(3)] {
        assert!(events.contains(&UiEvent::TaskFinished {
            task,
            state: TaskState::Canceled
        }));
        assert!(
            !events.iter().any(
                |event| matches!(event,UiEvent::TaskStarted{task:started,..} if *started==task)
            )
        );
    }
}

#[tokio::test]
async fn admission_failure_finishes_only_acknowledged_prefix_and_preserves_delivery_error() {
    let runner = Runner {
        results: Mutex::new(VecDeque::new()),
        calls: Mutex::new(Vec::new()),
        barrier: None,
    };
    let machines = [
        machine("a", "a.invalid"),
        machine("b", "b.invalid"),
        machine("c", "c.invalid"),
    ];
    let (sink, mut receiver) = EventSink::channel(1);
    let events = Arc::new(Mutex::new(Vec::new()));
    let collected = events.clone();
    let consumer = tokio::spawn(async move {
        while let Some(delivery) = receiver.receive().await {
            let (event, ack) = delivery.into_parts();
            let fail = matches!(
                event,
                UiEvent::TaskPending {
                    task: TaskId(2),
                    ..
                }
            );
            collected.lock().unwrap().push(event);
            ack.acknowledge(if fail {
                Err("fixture admission delivery".into())
            } else {
                Ok(())
            });
        }
    });
    let cancellation = CancellationToken::new();
    let result = commands(
        &Execution {
            runner: &runner,
            events: &sink,
            cancellation: &cancellation,
            root: Path::new("/tmp"),
            config: None,
            live: false,
        },
        &machines.iter().collect::<Vec<_>>(),
        &request(false, false),
    )
    .await;
    drop(sink);
    consumer.await.unwrap();
    assert!(
        matches!(result,Err(ConfctlError::EventDelivery(message)) if message=="fixture admission delivery")
    );
    assert!(runner.calls.lock().unwrap().is_empty());
    let events = events.lock().unwrap();
    assert!(events.contains(&UiEvent::TaskFinished {
        task: TaskId(1),
        state: TaskState::Skipped
    }));
    assert!(!events.iter().any(|event| matches!(
        event,
        UiEvent::TaskStarted {
            task: TaskId(1..),
            ..
        }
    )));
}

#[tokio::test]
async fn cancellation_after_group_admission_host_final_or_output_never_reports_success() {
    for boundary in ["group", "admission", "host", "output", "batch-final"] {
        let runner = Runner {
            results: Mutex::new(VecDeque::from([
                Ok(outcome(0, b"done")),
                Ok(outcome(0, b"later")),
            ])),
            calls: Mutex::new(Vec::new()),
            barrier: None,
        };
        let machines = [machine("a", "a.invalid"), machine("b", "b.invalid")];
        let (sink, mut receiver) = EventSink::channel(1);
        let cancellation = CancellationToken::new();
        let consumer_cancel = cancellation.clone();
        let events = Arc::new(Mutex::new(Vec::new()));
        let collected = events.clone();
        let consumer = tokio::spawn(async move {
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                let cancel = match boundary {
                    "group" => matches!(
                        event,
                        UiEvent::TaskStarted {
                            task: TaskId(0),
                            ..
                        }
                    ),
                    "admission" => matches!(
                        event,
                        UiEvent::TaskPending {
                            task: TaskId(1),
                            ..
                        }
                    ),
                    "host" => matches!(
                        event,
                        UiEvent::TaskFinished {
                            task: TaskId(1),
                            ..
                        }
                    ),
                    "output" => matches!(event, UiEvent::Output { .. }),
                    "batch-final" => matches!(
                        event,
                        UiEvent::TaskFinished {
                            task: TaskId(0),
                            ..
                        }
                    ),
                    _ => unreachable!(),
                };
                collected.lock().unwrap().push(event);
                if cancel {
                    consumer_cancel.cancel();
                }
                ack.acknowledge(Ok(()));
            }
        });
        let result = commands(
            &Execution {
                runner: &runner,
                events: &sink,
                cancellation: &cancellation,
                root: Path::new("/tmp"),
                config: None,
                live: false,
            },
            &machines.iter().collect::<Vec<_>>(),
            &request(false, false),
        )
        .await;
        drop(sink);
        consumer.await.unwrap();
        assert!(result.unwrap_err().is_cancelled(), "{boundary}");
        assert_eq!(
            runner.calls.lock().unwrap().len(),
            match boundary {
                "group" | "admission" => 0,
                "batch-final" => 2,
                _ => 1,
            },
            "{boundary}"
        );
        if matches!(boundary, "host" | "output") {
            assert!(events.lock().unwrap().contains(&UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Succeeded
            }));
        }
    }
}

struct InteractiveRunner;
impl ProcessRunner for InteractiveRunner {
    async fn run(
        &self,
        spec: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        assert_eq!(spec.stdout, StreamPolicy::Foreground);
        assert_eq!(spec.stderr, StreamPolicy::Foreground);
        assert!(scope.foreground.is_some());
        Ok(outcome(0, b""))
    }
}
#[tokio::test]
async fn interactive_final_acknowledgement_cancellation_keeps_observed_success() {
    let (events, mut receiver) = EventSink::channel(1);
    let cancellation = CancellationToken::new();
    let consumer_cancel = cancellation.clone();
    let observed = Arc::new(Mutex::new(Vec::new()));
    let collected = observed.clone();
    let consumer = tokio::spawn(async move {
        while let Some(delivery) = receiver.receive().await {
            let (event, ack) = delivery.into_parts();
            if matches!(
                event,
                UiEvent::TaskFinished {
                    task: TaskId(1),
                    ..
                }
            ) {
                consumer_cancel.cancel();
            }
            collected.lock().unwrap().push(event);
            ack.acknowledge(Ok(()));
        }
    });
    let ui = Ui::new(
        tempfile::tempfile().unwrap(),
        events.clone(),
        cancellation.clone(),
        None,
    )
    .unwrap();
    let result = interactive_shell(
        &Execution {
            runner: &InteractiveRunner,
            events: &events,
            cancellation: &cancellation,
            root: Path::new("/tmp"),
            config: None,
            live: false,
        },
        &ui,
        &machine("local", "localhost"),
        || Ok("/passwd-shell".into()),
    )
    .await;
    ui.shutdown().unwrap();
    drop(ui);
    drop(events);
    consumer.await.unwrap();
    assert!(result.unwrap_err().is_cancelled());
    assert!(observed.lock().unwrap().contains(&UiEvent::TaskFinished {
        task: TaskId(1),
        state: TaskState::Succeeded
    }));
}
