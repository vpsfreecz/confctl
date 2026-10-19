use super::*;
use crate::events::EventReceiver;
use serde_json::json;
use std::{
    collections::VecDeque,
    ffi::OsString,
    os::unix::{ffi::OsStringExt, process::ExitStatusExt},
    sync::{Arc, Mutex},
    time::Instant,
};

fn machine(name: &str, host: Value) -> Machine {
    Machine::from_value(json!({"name":name,"metaConfig":{"host":host}})).unwrap()
}

#[test]
fn ssh_uses_literal_native_config_root_port_and_one_remote_argument() {
    let config = OsString::from_vec(b"config with spaces\xff".to_vec());
    let remote = machine(
        "name",
        json!({"target":"host.invalid", "port":"\u{000b}+02_222suffix"}),
    );
    let spec = probe_spec(&remote, Some(&config)).unwrap();
    assert_eq!(spec.program, Path::new("ssh"));
    let mut expected = vec!["-F".into(), config];
    expected.extend(
        [
            "-l",
            "root",
            "-p",
            "2222",
            "-o",
            "ConnectTimeout=3",
            "-o",
            "ServerAliveInterval=3",
            "-o",
            "ServerAliveCountMax=1",
            "host.invalid",
            "cat /proc/uptime",
        ]
        .map(OsString::from),
    );
    assert_eq!(spec.args, expected);
    assert!(matches!(spec.stdin, crate::process::StdinPolicy::Null));
    assert_eq!(spec.stdout, crate::process::StreamPolicy::Capture);
    assert_eq!(spec.stderr, crate::process::StreamPolicy::Capture);
    let spec = probe_spec(
        &machine("fallback.invalid", json!({})),
        Some(OsStr::new("")),
    )
    .unwrap();
    assert!(!spec.args.iter().any(|arg| arg == "-F" || arg == "-p"));
    assert_eq!(
        &spec.args[spec.args.len() - 2..],
        ["fallback.invalid", "cat /proc/uptime"]
    );
}

#[test]
fn local_probe_ignores_ssh_config_and_port() {
    let spec = probe_spec(
        &machine("local", json!({"target":"localhost", "port":false})),
        Some(OsStr::new("config")),
    )
    .unwrap();
    assert_eq!(spec.program, Path::new("cat"));
    assert_eq!(spec.args, ["/proc/uptime"]);
}

#[test]
fn ports_preserve_ruby_conversion_without_a_port_sized_integer() {
    for (value, expected) in [
        (Value::Null, "0"),
        (json!("nonsense"), "0"),
        (json!("\u{000b}-0002_222suffix"), "-2222"),
        (json!("22__33"), "22"),
        (json!(2222.9), "2222"),
        (
            json!("999999999999999999999999999"),
            "999999999999999999999999999",
        ),
    ] {
        assert_eq!(ruby_port(&value).unwrap(), expected);
    }
    assert!(ruby_port(&Value::Bool(false)).is_err());
}

fn outcome(status: i32) -> ProcessOutcome {
    ProcessOutcome {
        status: std::process::ExitStatus::from_raw(status),
        stdout: Captured::Memory(b"uptime stays private".to_vec()),
        stderr: Captured::Memory(b"refused".to_vec()),
        stdout_complete: true,
        stderr_complete: true,
        started: Instant::now(),
        ended: Instant::now(),
        termination: TerminationCause::Exited,
    }
}

#[test]
fn ordinary_signal_is_a_host_failure_and_incomplete_capture_retains_evidence() {
    assert!(
        decode_probe(outcome(15))
            .unwrap()
            .unwrap()
            .contains("signal: 15")
    );
    let mut partial = outcome(0);
    partial.stderr_complete = false;
    let Err(ConfctlError::ProcessFailure { evidence, .. }) = decode_probe(partial) else {
        panic!()
    };
    assert_eq!(evidence.stdout.memory().unwrap(), b"uptime stays private");
    assert!(!evidence.stderr_complete);
    let mut canceled = outcome(0);
    canceled.termination = TerminationCause::Canceled;
    assert!(decode_probe(canceled).unwrap_err().is_cancelled());

    let mut short = outcome(0);
    let file = tempfile::NamedTempFile::new().unwrap();
    short.stdout = Captured::Spool { file, length: 1 };
    assert!(matches!(
        decode_probe(short),
        Err(ConfctlError::ProcessFailure { .. })
    ));
}

struct Runner {
    results: Mutex<VecDeque<Result<ProcessOutcome, ConfctlError>>>,
    calls: Mutex<Vec<(TaskId, String, Option<Phase>)>>,
}
impl ProcessRunner for Runner {
    async fn run(
        &self,
        spec: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        self.calls
            .lock()
            .unwrap()
            .push((scope.task, spec.redacted_description, scope.phase));
        self.results
            .lock()
            .unwrap()
            .pop_front()
            .expect("only entered hosts run")
    }
}

async fn collect(mut receiver: EventReceiver, events: Arc<Mutex<Vec<UiEvent>>>) {
    while let Some(delivery) = receiver.receive().await {
        let (event, ack) = delivery.into_parts();
        events.lock().unwrap().push(event);
        ack.acknowledge(Ok(()));
    }
}

#[tokio::test]
async fn actual_lifecycle_tracks_serial_hosts_and_stops_on_infrastructure_failure() {
    let runner = Runner {
        results: Mutex::new(VecDeque::from([
            Ok(outcome(255 << 8)),
            Err(ConfctlError::Spawn {
                description: "probe".into(),
                source: io::Error::other("fixture failure"),
            }),
        ])),
        calls: Mutex::new(Vec::new()),
    };
    let machines = [
        machine("one", json!({})),
        machine("two", json!({})),
        machine("three", json!({})),
    ];
    let (sink, receiver) = EventSink::channel(1);
    let events = Arc::new(Mutex::new(Vec::new()));
    let collector = tokio::spawn(collect(receiver, events.clone()));
    let result = check_hosts(
        &runner,
        &sink,
        &CancellationToken::new(),
        Path::new("/tmp"),
        &machines.iter().collect::<Vec<_>>(),
        None,
    )
    .await;
    assert!(matches!(result, Err(ConfctlError::Spawn { .. })));
    drop(sink);
    collector.await.unwrap();
    assert_eq!(
        *runner.calls.lock().unwrap(),
        [
            (
                TaskId(1),
                "test connection to one".into(),
                Some(Phase::ConnectionTests)
            ),
            (
                TaskId(2),
                "test connection to two".into(),
                Some(Phase::ConnectionTests)
            )
        ]
    );
    let events = events.lock().unwrap();
    let lifecycle = events
        .iter()
        .filter_map(|event| match event {
            UiEvent::TaskPending { task, .. } => Some((task.0, "pending")),
            UiEvent::TaskStarted { task, .. } => Some((task.0, "started")),
            UiEvent::TaskFinished { task, state } => Some((
                task.0,
                match state {
                    TaskState::Failed => "failed",
                    TaskState::Skipped => "skipped",
                    _ => panic!(),
                },
            )),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(
        lifecycle,
        [
            (1, "pending"),
            (2, "pending"),
            (3, "pending"),
            (1, "started"),
            (1, "failed"),
            (2, "started"),
            (2, "failed"),
            (3, "skipped")
        ]
    );
}

#[tokio::test]
async fn canceled_probe_cancels_later_admitted_hosts_without_entering_them() {
    let mut canceled = outcome(15);
    canceled.termination = TerminationCause::Canceled;
    let runner = Runner {
        results: Mutex::new(VecDeque::from([Ok(canceled)])),
        calls: Mutex::new(Vec::new()),
    };
    let machines = [machine("one", json!({})), machine("two", json!({}))];
    let (sink, receiver) = EventSink::channel(1);
    let events = Arc::new(Mutex::new(Vec::new()));
    let collector = tokio::spawn(collect(receiver, events.clone()));
    assert!(
        check_hosts(
            &runner,
            &sink,
            &CancellationToken::new(),
            Path::new("/tmp"),
            &machines.iter().collect::<Vec<_>>(),
            None
        )
        .await
        .unwrap_err()
        .is_cancelled()
    );
    drop(sink);
    collector.await.unwrap();
    assert_eq!(runner.calls.lock().unwrap().len(), 1);
    let events = events.lock().unwrap();
    for task in [TaskId(1), TaskId(2)] {
        assert!(events.contains(&UiEvent::TaskFinished {
            task,
            state: TaskState::Canceled
        }));
    }
    assert!(!events.iter().any(|event| matches!(
        event,
        UiEvent::TaskStarted {
            task: TaskId(2),
            ..
        }
    )));
}

#[tokio::test]
async fn consumer_cancellation_at_last_result_or_summary_cancels_the_invocation() {
    for after_summary in [false, true] {
        for last_status in [0, 255 << 8] {
            let runner = Runner {
                results: Mutex::new(VecDeque::from([Ok(outcome(0)), Ok(outcome(last_status))])),
                calls: Mutex::new(Vec::new()),
            };
            let machines = [machine("one", json!({})), machine("two", json!({}))];
            let cancellation = CancellationToken::new();
            let consumer_cancellation = cancellation.clone();
            let (sink, mut receiver) = EventSink::channel(1);
            let events = Arc::new(Mutex::new(Vec::new()));
            let collected = events.clone();
            let collector = tokio::spawn(async move {
                while let Some(delivery) = receiver.receive().await {
                    let (event, ack) = delivery.into_parts();
                    let cancel = if after_summary {
                        matches!(&event, UiEvent::Diagnostic { text, .. } if text.starts_with("\nResult:"))
                    } else {
                        matches!(
                            &event,
                            UiEvent::TaskFinished {
                                task: TaskId(2),
                                ..
                            }
                        )
                    };
                    collected.lock().unwrap().push(event);
                    if cancel {
                        consumer_cancellation.cancel();
                    }
                    ack.acknowledge(Ok(()));
                }
            });
            let result = check_hosts(
                &runner,
                &sink,
                &cancellation,
                Path::new("/tmp"),
                &machines.iter().collect::<Vec<_>>(),
                None,
            )
            .await;
            drop(sink);
            collector.await.unwrap();
            assert!(result.unwrap_err().is_cancelled());
            assert_eq!(runner.calls.lock().unwrap().len(), 2);
            let events = events.lock().unwrap();
            let finished = events
                .iter()
                .filter_map(|event| match event {
                    UiEvent::TaskFinished { task, state } => Some((*task, *state)),
                    _ => None,
                })
                .collect::<Vec<_>>();
            assert_eq!(
                finished,
                [
                    (TaskId(1), TaskState::Succeeded),
                    (
                        TaskId(2),
                        if last_status == 0 {
                            TaskState::Succeeded
                        } else {
                            TaskState::Failed
                        }
                    ),
                ]
            );
            assert_eq!(events.iter().any(|event| matches!(event, UiEvent::Diagnostic { text, .. } if text.starts_with("\nResult:"))), after_summary);
        }
    }
}

#[tokio::test]
async fn failed_event_delivery_does_not_enter_any_probe() {
    let runner = Runner {
        results: Mutex::new(VecDeque::new()),
        calls: Mutex::new(Vec::new()),
    };
    let machine = machine("one", json!({}));
    let (sink, receiver) = EventSink::channel(1);
    drop(receiver);
    assert!(matches!(
        check_hosts(
            &runner,
            &sink,
            &CancellationToken::new(),
            Path::new("/tmp"),
            &[&machine],
            None
        )
        .await,
        Err(ConfctlError::EventDelivery(_))
    ));
    assert!(runner.calls.lock().unwrap().is_empty());
}
