use super::*;
use crate::configuration::{ConfigRelativePath, ConfigRoot, MutationLock};
use serde_json::json;
use std::{
    collections::VecDeque,
    os::unix::process::ExitStatusExt,
    process::ExitStatus,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Instant,
};

type ProbeReply = (i32, Vec<u8>, Vec<u8>);

struct Probe {
    replies: Mutex<VecDeque<ProbeReply>>,
    calls: Mutex<Vec<(ProcessSpec, TaskId)>>,
}
impl Probe {
    fn new(replies: Vec<ProbeReply>) -> Self {
        Self {
            replies: Mutex::new(replies.into()),
            calls: Mutex::new(Vec::new()),
        }
    }
}
fn outcome(code: i32, out: Vec<u8>, err: Vec<u8>) -> ProcessOutcome {
    ProcessOutcome {
        status: ExitStatus::from_raw(code << 8),
        stdout: Captured::Memory(out),
        stderr: Captured::Memory(err),
        stdout_complete: true,
        stderr_complete: true,
        started: Instant::now(),
        ended: Instant::now(),
        termination: TerminationCause::Exited,
    }
}
impl ProcessRunner for Probe {
    async fn run(
        &self,
        spec: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        self.calls.lock().unwrap().push((spec, scope.task));
        let (code, out, err) = self
            .replies
            .lock()
            .unwrap()
            .pop_front()
            .expect("only expected probes");
        Ok(outcome(code, out, err))
    }
}
fn machine(name: &str, carrier: Option<&str>) -> Machine {
    Machine::from_value(json!({"name":name,"carrier":carrier,"metaConfig":{"spin":"vpsadminos","managed":false,"host":{"target":format!("{name}.invalid"),"port":22}}})).unwrap()
}
type Lifecycle = Arc<Mutex<Vec<UiEvent>>>;

fn assert_task_events(events: &[UiEvent], expected: &[(u64, &str, TaskState)]) {
    for (id, host, state) in expected {
        let task = TaskId(*id);
        let description = format!("query runtime kernel on {host}");
        let actual = events
            .iter()
            .filter(|event| match event {
                UiEvent::TaskPending { task: id, .. }
                | UiEvent::TaskStarted { task: id, .. }
                | UiEvent::TaskFinished { task: id, .. } => *id == task,
                _ => false,
            })
            .collect::<Vec<_>>();
        assert_eq!(
            actual,
            [
                &UiEvent::TaskPending {
                    task,
                    description: description.clone()
                },
                &UiEvent::TaskStarted { task, description },
                &UiEvent::TaskFinished {
                    task,
                    state: *state
                },
            ]
        );
    }
    assert_eq!(events.len(), expected.len() * 3);
}

fn events() -> (EventSink, tokio::task::JoinHandle<String>, Lifecycle) {
    let (events, mut receiver) = EventSink::channel(1);
    let lifecycle = Arc::new(Mutex::new(Vec::new()));
    let recorded = lifecycle.clone();
    let task = tokio::spawn(async move {
        let mut output = String::new();
        while let Some(delivery) = receiver.receive().await {
            let (event, ack) = delivery.into_parts();
            match event {
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    text,
                    ..
                } => output.push_str(&text),
                event @ (UiEvent::TaskPending { .. }
                | UiEvent::TaskStarted { .. }
                | UiEvent::TaskFinished { .. }) => recorded.lock().unwrap().push(event),
                _ => panic!("unexpected feature event"),
            }
            ack.acknowledge(Ok(()));
        }
        output
    });
    (events, task, lifecycle)
}
struct Fixture {
    dir: tempfile::TempDir,
    root: ConfigRoot,
    files: StateFiles,
    settings: RuntimeKernelSettings,
    inventory: Inventory,
}
impl Fixture {
    fn new(names: &[&str], initial: Option<&str>) -> Self {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("configs/node")).unwrap();
        if let Some(initial) = initial {
            std::fs::write(dir.path().join("configs/node/kernels.json"), initial).unwrap();
        }
        let root = ConfigRoot::new(dir.path()).unwrap();
        Self {
            files: StateFiles::new(root.clone()),
            root,
            dir,
            settings: RuntimeKernelSettings {
                enable: true,
                state_file: Some(ConfigRelativePath::new("configs/node/kernels.json").unwrap()),
                refresh_before_deploy: false,
            },
            inventory: Inventory {
                machines: names
                    .iter()
                    .map(|name| {
                        machine(
                            name,
                            if name.contains('#') {
                                Some("carrier")
                            } else {
                                None
                            },
                        )
                    })
                    .collect(),
            },
        }
    }
    fn state(&self) -> Option<String> {
        std::fs::read_to_string(self.dir.path().join("configs/node/kernels.json")).ok()
    }
    fn context<'a, P>(
        &'a self,
        runner: &'a P,
        events: &'a EventSink,
        cancel: &'a CancellationToken,
        guard: &'a MutationGuard,
    ) -> FeatureContext<'a, P> {
        FeatureContext {
            runner,
            events,
            files: &self.files,
            inventory: &self.inventory,
            settings: &self.settings,
            cancellation: cancel,
            mutation: guard,
            ssh_config: None,
        }
    }
}

#[tokio::test]
async fn normalization_and_insertion_order_match_preserved_ruby_site_capture() {
    // Exact bytes: accepted M0 site-normalization observation, unchanged oracle.
    let fixture = Fixture::new(
        &["nodes/a", "nodes/b", "carrier#b", "nodes/off"],
        Some("{\"carrier#b\":\"keep\"}"),
    );
    let runner = Probe::new(vec![
        (
            0,
            b" 6.9..extra.more\n".to_vec(),
            b"ignored stderr".to_vec(),
        ),
        (0, Vec::new(), Vec::new()),
        (0, b"error\n".to_vec(), Vec::new()),
        (0, b"1.2...\n".to_vec(), Vec::new()),
    ]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output, lifecycle) = events();
    let report = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[
            (MachineId(0), TaskId(1)),
            (MachineId(1), TaskId(2)),
            (MachineId(2), TaskId(3)),
            (MachineId(3), TaskId(4)),
        ],
    )
    .await
    .unwrap();
    assert_eq!(
        fixture.state().unwrap(),
        "{\n  \"carrier#b\": \"keep\",\n  \"nodes/a\": \"6.9.\",\n  \"nodes/b\": \"\",\n  \"nodes/off\": \"1.2\"\n}"
    );
    assert_eq!(report.updated, ["nodes/a", "nodes/b", "nodes/off"]);
    assert_eq!(report.unchanged, ["carrier#b"]);
    assert!(report.deleted.is_empty());
    assert_eq!(
        runner.calls.lock().unwrap().len(),
        4,
        "unmanaged/carried are not silently filtered"
    );
    assert_eq!(
        runner
            .calls
            .lock()
            .unwrap()
            .iter()
            .map(|(_, task)| *task)
            .collect::<Vec<_>>(),
        [TaskId(1), TaskId(2), TaskId(3), TaskId(4)]
    );
    drop(events);
    let output = output.await.unwrap();
    assert!(output.starts_with(&format!("{:<50} KERNEL\n", "HOST")));
    assert!(!output.contains("ignored stderr"));
    assert_task_events(
        &lifecycle.lock().unwrap(),
        &[
            (1, "nodes/a", TaskState::Succeeded),
            (2, "nodes/b", TaskState::Succeeded),
            (3, "carrier#b", TaskState::Succeeded),
            (4, "nodes/off", TaskState::Succeeded),
        ],
    );
}

#[tokio::test]
async fn nonfatal_hosts_delete_only_selected_keys_and_keep_unknown_values_ordered() {
    let fixture = Fixture::new(
        &["nodes/a", "nodes/b"],
        Some("{\"nodes/a\":\"old\",\"nodes/b\":\"old\",\"unselected\":{\"z\":false,\"a\":[1,2]}}"),
    );
    let runner = Probe::new(vec![
        (255, Vec::new(), b"fixture SSH failure".to_vec()),
        (0, b"6.12.3-extra.more\n".to_vec(), Vec::new()),
    ]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output, lifecycle) = events();
    let report = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[(MachineId(0), TaskId(1)), (MachineId(1), TaskId(2))],
    )
    .await
    .unwrap();
    assert_eq!(report.deleted, ["nodes/a"]);
    assert_eq!(report.rows[0].kernel, "error");
    assert!(
        report.rows[0]
            .error
            .as_ref()
            .unwrap()
            .contains("exit status: 255")
    );
    assert_eq!(
        fixture.state().unwrap(),
        "{\n  \"nodes/b\": \"6.12.3-extra\",\n  \"unselected\": {\n    \"z\": false,\n    \"a\": [\n      1,\n      2\n    ]\n  }\n}"
    );
    drop(events);
    assert!(output.await.unwrap().contains("Error on nodes/a:"));
    assert_task_events(
        &lifecycle.lock().unwrap(),
        &[
            (1, "nodes/a", TaskState::Failed),
            (2, "nodes/b", TaskState::Succeeded),
        ],
    );
}

#[tokio::test]
async fn zero_selected_preserves_scalar_and_literal_error_keeps_old_state() {
    let fixture = Fixture::new(&[], Some("17"));
    let runner = Probe::new(vec![]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output, _) = events();
    let report = refresh(&fixture.context(&runner, &events, &cancel, &guard), &[])
        .await
        .unwrap();
    assert!(report.rows.is_empty());
    assert_eq!(fixture.state().as_deref(), Some("17"));
    assert!(runner.calls.lock().unwrap().is_empty());
    drop(events);
    assert_eq!(output.await.unwrap(), format!("{:<50} KERNEL\n", "HOST"));
    assert_eq!(normalize_kernel("\0 \t1.2...\r\n"), "1.2");
    assert_eq!(normalize_kernel("\u{a0}1.2.3"), "\u{a0}1.2.3");
    let mut scalar = json!(17);
    assert!(assign(&mut scalar, "host", "1.2.3").is_err());
    assert_eq!(scalar, json!(17));
    let mut array = json!(["host", 7, "host", false]);
    assert!(delete(&mut array, "host").unwrap());
    assert_eq!(array, json!([7, false]));
    let mut string = json!("prefix host suffix");
    assert!(assign(&mut string, "host", "6.12").unwrap());
    assert_eq!(string, json!("prefix 6.12 suffix"));
    assert!(!delete(&mut string, "x").unwrap());
}

#[tokio::test]
async fn malformed_state_disabled_and_wrong_guard_fail_before_any_probe_or_output() {
    let mut fixture = Fixture::new(&["nodes/a"], Some("{bad"));
    let runner = Probe::new(vec![]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output, _) = events();
    assert!(
        refresh(
            &fixture.context(&runner, &events, &cancel, &guard),
            &[(MachineId(0), TaskId(1))]
        )
        .await
        .is_err()
    );
    assert_eq!(fixture.state().as_deref(), Some("{bad"));
    fixture.settings.enable = false;
    assert!(
        refresh(&fixture.context(&runner, &events, &cancel, &guard), &[])
            .await
            .is_err()
    );
    fixture.settings.enable = true;
    let other = Fixture::new(&[], None);
    let other_guard = MutationLock::acquire(&other.root, &cancel).await.unwrap();
    assert!(
        refresh(
            &fixture.context(&runner, &events, &cancel, &other_guard),
            &[]
        )
        .await
        .is_err()
    );
    assert!(runner.calls.lock().unwrap().is_empty());
    drop(events);
    assert_eq!(output.await.unwrap(), "");
}

struct FailingAndBlocked {
    started: AtomicUsize,
    joined: AtomicUsize,
    entered: tokio::sync::Barrier,
}
impl ProcessRunner for FailingAndBlocked {
    async fn run(
        &self,
        _: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        let index = self.started.fetch_add(1, Ordering::SeqCst);
        self.entered.wait().await;
        let result = if index == 0 {
            Err(ConfctlError::Io {
                operation: "fixture spawn fault",
                path: None,
                source: io::Error::other("fixture"),
            })
        } else {
            scope.cancellation.cancelled().await;
            Err(ConfctlError::Cancelled)
        };
        self.joined.fetch_add(1, Ordering::SeqCst);
        result
    }
}
#[tokio::test]
async fn infrastructure_failure_cancels_and_joins_every_probe_without_publication() {
    let fixture = Fixture::new(&["a", "b"], Some("{\"old\":true}"));
    let runner = FailingAndBlocked {
        started: AtomicUsize::new(0),
        joined: AtomicUsize::new(0),
        entered: tokio::sync::Barrier::new(2),
    };
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output, _) = events();
    let error = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[(MachineId(0), TaskId(1)), (MachineId(1), TaskId(2))],
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("fixture spawn fault"));
    assert_eq!(runner.started.load(Ordering::SeqCst), 2);
    assert_eq!(runner.joined.load(Ordering::SeqCst), 2);
    assert_eq!(fixture.state().as_deref(), Some("{\"old\":true}"));
    drop(events);
    assert_eq!(output.await.unwrap(), "");
}

#[tokio::test]
async fn failed_output_ack_never_publishes_a_completed_merge() {
    let fixture = Fixture::new(&["a"], Some("{\"a\":\"old\"}"));
    let runner = Probe::new(vec![(0, b"6.12.3\n".to_vec(), Vec::new())]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, mut receiver) = EventSink::channel(1);
    let consumer = tokio::spawn(async move {
        while let Some(delivery) = receiver.receive().await {
            let reject = matches!(delivery.event, UiEvent::Diagnostic { .. });
            delivery.acknowledge(if reject {
                Err("output fault".into())
            } else {
                Ok(())
            });
            if reject {
                break;
            }
        }
    });
    assert!(
        refresh(
            &fixture.context(&runner, &events, &cancel, &guard),
            &[(MachineId(0), TaskId(1))]
        )
        .await
        .is_err()
    );
    consumer.await.unwrap();
    assert_eq!(fixture.state().as_deref(), Some("{\"a\":\"old\"}"));
}

struct BlockedProbes {
    entered: tokio::sync::Barrier,
    joined: AtomicUsize,
    second_succeeds: bool,
}
impl ProcessRunner for BlockedProbes {
    async fn run(
        &self,
        _: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        self.entered.wait().await;
        let result = if self.second_succeeds && scope.task == TaskId(2) {
            Ok(outcome(0, b"6.12.3\n".to_vec(), Vec::new()))
        } else {
            scope.cancellation.cancelled().await;
            Err(ConfctlError::Cancelled)
        };
        self.joined.fetch_add(1, Ordering::SeqCst);
        result
    }
}

#[tokio::test]
async fn cancellation_joins_running_probes_and_reports_canceled_without_publication() {
    let fixture = Fixture::new(&["a", "b"], Some("{\"old\":true}"));
    let runner = BlockedProbes {
        entered: tokio::sync::Barrier::new(3),
        joined: AtomicUsize::new(0),
        second_succeeds: false,
    };
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output, lifecycle) = events();
    let context = fixture.context(&runner, &events, &cancel, &guard);
    let (result, ()) = tokio::join!(
        refresh(
            &context,
            &[(MachineId(0), TaskId(1)), (MachineId(1), TaskId(2))]
        ),
        async {
            runner.entered.wait().await;
            cancel.cancel();
        },
    );
    assert!(result.unwrap_err().is_cancelled());
    assert_eq!(runner.joined.load(Ordering::SeqCst), 2);
    assert_eq!(fixture.state().as_deref(), Some("{\"old\":true}"));
    drop(events);
    assert!(output.await.unwrap().is_empty());
    assert_task_events(
        &lifecycle.lock().unwrap(),
        &[(1, "a", TaskState::Canceled), (2, "b", TaskState::Canceled)],
    );
}

#[tokio::test]
async fn cancellation_after_admission_does_not_invent_probe_entry_or_success() {
    let fixture = Fixture::new(&["a", "b"], Some("{}"));
    let runner = Probe::new(vec![]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, mut receiver) = EventSink::channel(1);
    let cancellation = cancel.clone();
    let consumer = tokio::spawn(async move {
        let mut recorded = Vec::new();
        while let Some(delivery) = receiver.receive().await {
            if matches!(
                delivery.event,
                UiEvent::TaskPending {
                    task: TaskId(2),
                    ..
                }
            ) {
                cancellation.cancel();
            }
            let (event, ack) = delivery.into_parts();
            recorded.push(event);
            ack.acknowledge(Ok(()));
        }
        recorded
    });
    let error = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[(MachineId(0), TaskId(1)), (MachineId(1), TaskId(2))],
    )
    .await
    .unwrap_err();
    assert!(error.is_cancelled());
    assert!(runner.calls.lock().unwrap().is_empty());
    assert_eq!(fixture.state().as_deref(), Some("{}"));
    drop(events);
    assert_eq!(
        consumer.await.unwrap(),
        [
            UiEvent::TaskPending {
                task: TaskId(1),
                description: "query runtime kernel on a".into()
            },
            UiEvent::TaskPending {
                task: TaskId(2),
                description: "query runtime kernel on b".into()
            },
            UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Canceled
            },
            UiEvent::TaskFinished {
                task: TaskId(2),
                state: TaskState::Canceled
            },
        ]
    );
}

#[tokio::test]
async fn rejected_final_cancels_and_joins_peer_without_publishing() {
    let fixture = Fixture::new(&["a", "b"], Some("{\"old\":true}"));
    let runner = BlockedProbes {
        entered: tokio::sync::Barrier::new(2),
        joined: AtomicUsize::new(0),
        second_succeeds: true,
    };
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, mut receiver) = EventSink::channel(1);
    let consumer = tokio::spawn(async move {
        let mut finals = Vec::new();
        while let Some(delivery) = receiver.receive().await {
            let reject = matches!(
                delivery.event,
                UiEvent::TaskFinished {
                    task: TaskId(2),
                    ..
                }
            );
            if let UiEvent::TaskFinished { task, state } = &delivery.event {
                finals.push((*task, *state));
            }
            delivery.acknowledge(if reject {
                Err("final output fault".into())
            } else {
                Ok(())
            });
        }
        finals
    });
    let error = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[(MachineId(0), TaskId(1)), (MachineId(1), TaskId(2))],
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("final output fault"));
    assert_eq!(runner.joined.load(Ordering::SeqCst), 2);
    assert_eq!(fixture.state().as_deref(), Some("{\"old\":true}"));
    drop(events);
    assert_eq!(
        consumer.await.unwrap(),
        [
            (TaskId(2), TaskState::Succeeded),
            (TaskId(1), TaskState::Canceled)
        ]
    );
}

#[tokio::test]
async fn duplicate_host_or_task_and_invalid_specs_fail_before_admission() {
    let mut fixture = Fixture::new(&["a", "b"], Some("{}"));
    let runner = Probe::new(vec![]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output, lifecycle) = events();
    for tasks in [
        vec![(MachineId(0), TaskId(1)), (MachineId(0), TaskId(2))],
        vec![(MachineId(0), TaskId(1)), (MachineId(1), TaskId(1))],
        vec![(MachineId(0), TaskId(0))],
        vec![(MachineId(99), TaskId(1))],
    ] {
        assert!(
            refresh(&fixture.context(&runner, &events, &cancel, &guard), &tasks)
                .await
                .is_err()
        );
    }
    fixture.inventory.machines[1].name = "a".into();
    assert!(
        refresh(
            &fixture.context(&runner, &events, &cancel, &guard),
            &[(MachineId(0), TaskId(1)), (MachineId(1), TaskId(2))]
        )
        .await
        .is_err()
    );
    fixture.inventory.machines[0].metadata["host"]["target"] = Value::Null;
    assert!(
        refresh(
            &fixture.context(&runner, &events, &cancel, &guard),
            &[(MachineId(0), TaskId(1))]
        )
        .await
        .is_err()
    );
    assert!(runner.calls.lock().unwrap().is_empty());
    drop(events);
    assert!(output.await.unwrap().is_empty());
    assert!(lifecycle.lock().unwrap().is_empty());
    assert_eq!(fixture.state().as_deref(), Some("{}"));
}

struct ReverseProbe {
    second: Arc<tokio::sync::Notify>,
    completed: Mutex<Vec<String>>,
}
impl ProcessRunner for ReverseProbe {
    async fn run(
        &self,
        spec: ProcessSpec,
        _: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        let name = spec.args[2].to_str().unwrap();
        if name == "a.invalid" {
            self.second.notified().await;
        }
        self.completed.lock().unwrap().push(name.into());
        Ok(outcome(0, b"6.12.3\n".to_vec(), Vec::new()))
    }
}
#[tokio::test]
async fn concurrent_reverse_completion_still_renders_and_saves_selection_order() {
    let fixture = Fixture::new(&["a", "b"], None);
    let runner = ReverseProbe {
        second: Arc::new(tokio::sync::Notify::new()),
        completed: Mutex::new(Vec::new()),
    };
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, mut receiver) = EventSink::channel(1);
    let release = runner.second.clone();
    let state = fixture.dir.path().join("configs/node/kernels.json");
    let output = tokio::spawn(async move {
        let mut output = String::new();
        let mut lifecycle = Vec::new();
        while let Some(delivery) = receiver.receive().await {
            let (event, ack) = delivery.into_parts();
            if let UiEvent::Diagnostic { text, .. } = event {
                output.push_str(&text);
            } else {
                if event
                    == (UiEvent::TaskFinished {
                        task: TaskId(2),
                        state: TaskState::Succeeded,
                    })
                {
                    assert!(!state.exists(), "a completed probe is not a saved refresh");
                    release.notify_one(); // First runner cannot finish before this final is delivered.
                }
                lifecycle.push(event);
            }
            ack.acknowledge(Ok(()));
        }
        (output, lifecycle)
    });
    let report = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[(MachineId(0), TaskId(1)), (MachineId(1), TaskId(2))],
    )
    .await
    .unwrap();
    assert_eq!(
        *runner.completed.lock().unwrap(),
        ["b.invalid", "a.invalid"]
    );
    assert_eq!(
        report
            .rows
            .iter()
            .map(|row| row.machine)
            .collect::<Vec<_>>(),
        [MachineId(0), MachineId(1)]
    );
    assert_eq!(
        fixture.state().unwrap(),
        "{\n  \"a\": \"6.12.3\",\n  \"b\": \"6.12.3\"\n}"
    );
    drop(events);
    let (output, lifecycle) = output.await.unwrap();
    let finals = lifecycle
        .iter()
        .filter_map(|event| match event {
            UiEvent::TaskFinished { task, .. } => Some(*task),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(finals, [TaskId(2), TaskId(1)]);
    assert_task_events(
        &lifecycle,
        &[
            (1, "a", TaskState::Succeeded),
            (2, "b", TaskState::Succeeded),
        ],
    );
    assert_eq!(
        output
            .lines()
            .skip(1)
            .map(|line| line.split_whitespace().next().unwrap().to_string())
            .collect::<Vec<_>>(),
        ["a", "b"]
    );
}

#[test]
fn uname_route_is_local_or_exact_root_ssh_without_hidden_filters_or_timeout() {
    let mut machine = machine("carrier#child", Some("carrier"));
    machine.metadata["host"]["port"] = json!(2222);
    let spec = probe_spec(&machine, Some(OsStr::new("/fixture/ssh.conf"))).unwrap();
    assert_eq!(spec.program, std::path::Path::new("ssh"));
    assert_eq!(
        spec.args,
        [
            "-F",
            "/fixture/ssh.conf",
            "-l",
            "root",
            "-p",
            "2222",
            "carrier#child.invalid",
            "uname -r"
        ]
        .map(std::ffi::OsString::from)
    );
    machine.metadata["host"]["target"] = json!("localhost");
    let spec = probe_spec(&machine, None).unwrap();
    assert_eq!(spec.program, std::path::Path::new("uname"));
    assert_eq!(spec.args, [std::ffi::OsString::from("-r")]);
    machine.metadata = json!({"spin":"vpsadminos"});
    let spec = probe_spec(&machine, None).unwrap();
    assert_eq!(spec.args.last().unwrap(), "uname -r");
    assert_eq!(spec.args[2], "carrier#child");
}

#[test]
fn uname_port_conversion_matches_original_ruby_and_local_target_ignores_unused_port() {
    let fixture: Value =
        serde_json::from_str(include_str!("../remote/json/ruby-conversions.json")).unwrap();
    for case in fixture["port_cases"].as_array().unwrap() {
        let converted = ruby_port(&case["value"]);
        match case.get("converted") {
            Some(expected) => assert_eq!(
                converted.unwrap(),
                expected.as_str().unwrap(),
                "{}",
                case["value"]
            ),
            None => assert!(converted.is_err(), "{}", case["value"]),
        }
    }
    let mut machine = machine("carrier#child", Some("carrier"));
    for bad in [Value::Null, json!(false), json!([]), json!("host")] {
        machine.metadata["host"] = bad;
        assert!(probe_spec(&machine, None).is_err());
    }
    for bad in [Value::Null, json!(false), json!([]), json!({})] {
        machine.metadata["host"] = json!({"target":bad});
        assert!(probe_spec(&machine, None).is_err());
    }
    machine.metadata["host"] = json!({"target":"localhost","port":{}});
    assert_eq!(
        probe_spec(&machine, None).unwrap().args,
        [std::ffi::OsString::from("-r")]
    );
    machine.metadata["host"] = json!({"target":"remote","port":"2_2"});
    let spec = probe_spec(&machine, Some(OsStr::new(""))).unwrap();
    assert_eq!(
        spec.args,
        ["-l", "root", "remote", "uname -r"].map(std::ffi::OsString::from)
    );
}

#[tokio::test]
async fn refresh_keeps_unselected_huge_integers_float_bytes_nested_empties_and_order() {
    let fixture = Fixture::new(
        &["a"],
        Some(
            "{\"unselected\":{\"z\":184467440737095516160001,\"a\":-184467440737095516160001,\"empty\":[[],{}],\"f\":[1e15,-0.0,5e-324]},\"a\":\"old\"}",
        ),
    );
    let runner = Probe::new(vec![(0, b"6.12.3\n".to_vec(), Vec::new())]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output, _) = events();
    refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[(MachineId(0), TaskId(1))],
    )
    .await
    .unwrap();
    let original: Value =
        serde_json::from_str(include_str!("../remote/json/ruby-conversions.json")).unwrap();
    let expected = original["json_cases"].as_array().unwrap().last().unwrap()["pretty"]
        .as_str()
        .unwrap();
    let expected = format!(
        "{},\n  \"a\": \"6.12.3\"\n}}",
        expected.strip_suffix("\n}").unwrap()
    );
    assert_eq!(fixture.state().unwrap(), expected);
    drop(events);
    output.await.unwrap();
}

#[tokio::test]
async fn encoding_failure_after_joined_probe_or_empty_selection_never_publishes() {
    for selected in [false, true] {
        let initial = "{\"unselected\":1e400,\"a\":\"old\"}";
        let fixture = Fixture::new(&["a"], Some(initial));
        let runner = Probe::new(if selected {
            vec![(0, b"6.12.3\n".to_vec(), Vec::new())]
        } else {
            vec![]
        });
        let cancel = CancellationToken::new();
        let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
        let (events, output, _) = events();
        let ids = if selected {
            &[(MachineId(0), TaskId(1))][..]
        } else {
            &[]
        };
        let error = refresh(&fixture.context(&runner, &events, &cancel, &guard), ids)
            .await
            .unwrap_err();
        assert!(error.to_string().contains("nonfinite"));
        assert_eq!(fixture.state().as_deref(), Some(initial));
        assert_eq!(runner.calls.lock().unwrap().len(), usize::from(selected));
        drop(events);
        output.await.unwrap();
    }
}
