use super::*;
use crate::process::Captured;
use chrono::DateTime;
use serde_json::json;
use std::{collections::BTreeMap, os::unix::process::ExitStatusExt, sync::Mutex, time::Instant};

fn generation(name: &str) -> BuildGeneration {
    BuildGeneration {
        name: name.into(),
        date: DateTime::parse_from_rfc3339("2020-01-01T00:00:00Z").unwrap(),
        toplevel: "/store/system".into(),
        auto_rollback: Some("/store/rollback".into()),
        inputs: Default::default(),
        inputs_info: Default::default(),
    }
}
fn inventory() -> Inventory {
    Inventory::from_value(json!({
        "a":{"name":"a","metaConfig":{"host":{"target":"a.invalid","port":"02_222suffix"}}},
        "local":{"name":"local","metaConfig":{"host":{"target":"localhost"}}},
        "g1":{"name":"carrier#one","carrier":"a","metaConfig":{"host":{"target":null},"autoRollback":{"enable":false}}},
        "g2":{"name":"carrier#two","carrier":"a","metaConfig":{"host":{"target":null}}},
        "g3":{"name":"other#one","carrier":"other","metaConfig":{"host":{"target":"unused.invalid"}}},
        "other":{"name":"other","metaConfig":{"host":{"target":"other.invalid"}}}
    })).unwrap()
}
#[test]
fn deploy_selector_preserves_exact_numeric_text_and_per_host_offset_semantics() {
    let saved = GenerationInventory {
        generations: vec![generation("old"), generation("new"), generation("+1")],
        current: Some("old".into()),
        ..Default::default()
    };
    for (text, name) in [
        ("0", Some("+1")),
        ("0001", Some("old")),
        ("-1", Some("new")),
        ("-0", Some("+1")),
        ("+1", Some("+1")),
        ("current", Some("old")),
        ("99999999999999999999999999999", None),
    ] {
        let selector = GenerationSelector::parse(text.into());
        if text.bytes().all(|c| c.is_ascii_digit()) {
            assert_eq!(selector, GenerationSelector::Offset(text.into()));
        }
        assert_eq!(
            saved_generation("host", &saved, &selector)
                .unwrap()
                .map(|g| g.name.as_str()),
            name
        );
    }
    let mut rejected = saved;
    rejected.rejected = BTreeMap::from([(
        "bad".into(),
        crate::generation::RejectedGeneration {
            path: "bad".into(),
            reason: "unsupported".into(),
        },
    )]);
    assert!(
        saved_generation(
            "host",
            &rejected,
            &GenerationSelector::parse("99999999999999999999999999999".into())
        )
        .is_err()
    );
    assert_eq!(
        saved_generation("host", &rejected, &GenerationSelector::parse("+1".into()))
            .unwrap()
            .unwrap()
            .name,
        "+1"
    );
    rejected.current_error = Some("broken current".into());
    assert!(saved_generation("host", &rejected, &GenerationSelector::Current).is_err());
}
#[test]
fn copy_spec_keeps_carrier_port_native_options_and_rollback_policy() {
    let inv = inventory();
    let gen_saved = generation("saved");
    let spec = copy_spec(
        &inv,
        &inv.machines[2],
        &gen_saved,
        AutoRollback::Configured,
        Some(OsStr::from_bytes(b"  -o Native=\xff  ")),
    )
    .unwrap()
    .unwrap();
    assert_eq!(spec.program, std::path::Path::new("nix-copy-closure"));
    assert_eq!(spec.args, ["--to", "root@a.invalid", "/store/system"]);
    assert_eq!(
        spec.env[0].1.as_ref().unwrap().as_bytes(),
        b"-o Native=\xff   -p 2222"
    );
    let enabled = copy_spec(
        &inv,
        &inv.machines[2],
        &gen_saved,
        AutoRollback::Enabled,
        None,
    )
    .unwrap()
    .unwrap();
    assert_eq!(enabled.args.last().unwrap(), "/store/rollback");
    assert!(
        copy_spec(
            &inv,
            &inv.machines[1],
            &gen_saved,
            AutoRollback::Configured,
            None
        )
        .unwrap()
        .is_none()
    );
    let direct = copy_spec(
        &inv,
        &inv.machines[5],
        &gen_saved,
        AutoRollback::Disabled,
        Some(OsStr::new("inherited")),
    )
    .unwrap()
    .unwrap();
    assert!(direct.env.is_empty());
    assert_eq!(direct.args.len(), 3);
}
#[test]
fn bulk_order_keeps_standalones_then_round_robins_carriers() {
    let inv = inventory();
    let jobs = [2, 3, 4, 0, 1, 5].map(|i| Job {
        machine: MachineId(i),
        generation: generation("saved"),
        task: TaskId(i as u64 + 1),
    });
    let ordered = copy_order(&inv, jobs.into());
    assert_eq!(
        ordered.iter().map(|j| j.machine.0).collect::<Vec<_>>(),
        [0, 1, 5, 2, 4, 3]
    );
}
struct Runner {
    calls: Mutex<Vec<TaskId>>,
}
impl ProcessRunner for Runner {
    async fn run(
        &self,
        _: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        self.calls.lock().unwrap().push(scope.task);
        Ok(ProcessOutcome {
            status: std::process::ExitStatus::from_raw(0),
            stdout: Captured::Memory(Vec::new()),
            stderr: Captured::Memory(Vec::new()),
            stdout_complete: true,
            stderr_complete: true,
            started: Instant::now(),
            ended: Instant::now(),
            termination: TerminationCause::Exited,
        })
    }
}
#[tokio::test]
async fn last_copy_final_ack_cancellation_preserves_success_and_cancels_unentered_work() {
    for reject in [false, true] {
        let tmp = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(tmp.path()).unwrap();
        let inv = inventory();
        let runner = Runner {
            calls: Mutex::new(Vec::new()),
        };
        let cancel = CancellationToken::new();
        let (events, mut rx) = EventSink::channel(1);
        let token = cancel.clone();
        let consumer = tokio::spawn(async move {
            let mut finals = Vec::new();
            while let Some(d) = rx.receive().await {
                let (e, a) = d.into_parts();
                if let UiEvent::TaskFinished { task, state } = e {
                    finals.push((task, state));
                    if task == TaskId(1) {
                        token.cancel();
                        if reject {
                            a.acknowledge(Err("controlled final failure".into()));
                            continue;
                        }
                    }
                }
                a.acknowledge(Ok(()));
            }
            finals
        });
        let copies = Copies {
            runner: &runner,
            events: &events,
            cancellation: &cancel,
            inventory: &inv,
            root: &root,
            rollback: AutoRollback::Configured,
            ssh_opts: None,
        };
        let jobs = [0, 5].map(|i| Job {
            machine: MachineId(i),
            generation: generation("saved"),
            task: TaskId(if i == 0 { 1 } else { 2 }),
        });
        let error = bulk_copy(&copies, &jobs, 1).await.unwrap_err();
        if reject {
            assert!(error.to_string().contains("controlled final failure"));
        } else {
            assert!(error.is_cancelled());
        }
        assert_eq!(*runner.calls.lock().unwrap(), [TaskId(1)]);
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

struct ConcurrentRunner {
    active: std::sync::atomic::AtomicUsize,
    peak: std::sync::atomic::AtomicUsize,
    completed: Mutex<Vec<TaskId>>,
}
impl ProcessRunner for ConcurrentRunner {
    async fn run(
        &self,
        _: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        use std::sync::atomic::Ordering;
        let active = self.active.fetch_add(1, Ordering::SeqCst) + 1;
        self.peak.fetch_max(active, Ordering::SeqCst);
        tokio::time::sleep(std::time::Duration::from_millis(
            if scope.task == TaskId(1) { 40 } else { 1 },
        ))
        .await;
        self.active.fetch_sub(1, Ordering::SeqCst);
        self.completed.lock().unwrap().push(scope.task);
        Ok(ProcessOutcome {
            status: std::process::ExitStatus::from_raw(0),
            stdout: Captured::Memory(Vec::new()),
            stderr: Captured::Memory(Vec::new()),
            stdout_complete: true,
            stderr_complete: true,
            started: Instant::now(),
            ended: Instant::now(),
            termination: TerminationCause::Exited,
        })
    }
}
#[tokio::test]
async fn bounded_bulk_joins_reverse_completions_and_reports_each_actual_final() {
    let tmp = tempfile::tempdir().unwrap();
    let root = ConfigRoot::new(tmp.path()).unwrap();
    let inv = inventory();
    let runner = ConcurrentRunner {
        active: Default::default(),
        peak: Default::default(),
        completed: Mutex::new(Vec::new()),
    };
    let cancel = CancellationToken::new();
    let (events, mut rx) = EventSink::channel(1);
    let consumer = tokio::spawn(async move {
        let mut finals = Vec::new();
        while let Some(d) = rx.receive().await {
            let (e, a) = d.into_parts();
            if let UiEvent::TaskFinished { task, state } = e {
                assert_eq!(state, TaskState::Succeeded);
                finals.push(task);
            }
            a.acknowledge(Ok(()));
        }
        finals
    });
    let copies = Copies {
        runner: &runner,
        events: &events,
        cancellation: &cancel,
        inventory: &inv,
        root: &root,
        rollback: AutoRollback::Configured,
        ssh_opts: None,
    };
    let jobs = [0, 5, 2]
        .into_iter()
        .enumerate()
        .map(|(i, id)| Job {
            machine: MachineId(id),
            generation: generation("saved"),
            task: TaskId(i as u64 + 1),
        })
        .collect::<Vec<_>>();
    bulk_copy(&copies, &jobs, 2).await.unwrap();
    assert_eq!(runner.peak.load(std::sync::atomic::Ordering::SeqCst), 2);
    assert_eq!(runner.active.load(std::sync::atomic::Ordering::SeqCst), 0);
    assert_eq!(
        *runner.completed.lock().unwrap(),
        [TaskId(2), TaskId(3), TaskId(1)]
    );
    drop(events);
    assert_eq!(consumer.await.unwrap(), [TaskId(2), TaskId(3), TaskId(1)]);
}
