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

#[test]
fn build_return_order_and_confirmed_bulk_order_are_distinct_for_partial_subsets() {
    let jobs = [2, 0, 4].map(|id| Job {
        machine: MachineId(id),
        generation: generation("saved"),
        task: TaskId(id as u64),
    });
    let ordered = selected_order(
        &jobs,
        &[MachineId(0), MachineId(1), MachineId(2), MachineId(4)],
    );
    assert_eq!(
        ordered.iter().map(|job| job.machine).collect::<Vec<_>>(),
        [MachineId(0), MachineId(2), MachineId(4)]
    );
    assert_eq!(
        jobs.iter().map(|job| job.machine).collect::<Vec<_>>(),
        [MachineId(2), MachineId(0), MachineId(4)]
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

#[test]
fn boot_and_switch_skip_requires_both_known_standalone_paths_but_retains_carrier_partial_current() {
    use super::super::host_status::{HostGeneration, HostStatus};
    let inv = inventory();
    let desired = generation("saved");
    let records = |current| {
        vec![HostGeneration {
            id: "1".into(),
            toplevel: desired.toplevel.clone(),
            date: chrono::Local::now(),
            kernel: None,
            current,
        }]
    };
    for action in [SwitchAction::Boot, SwitchAction::Switch] {
        let mut status = HostStatus::default();
        assert!(!already_current(
            &inv.machines[0],
            &desired,
            &status,
            action
        ));
        status.generations = Some(records(true));
        assert!(!already_current(
            &inv.machines[0],
            &desired,
            &status,
            action
        ));
        status.current_toplevel = Some(desired.toplevel.clone());
        assert!(already_current(&inv.machines[0], &desired, &status, action));
        status.generations = Some(records(false));
        assert!(!already_current(
            &inv.machines[0],
            &desired,
            &status,
            action
        ));
        status.generations = Some(vec![]);
        assert!(!already_current(
            &inv.machines[0],
            &desired,
            &status,
            action
        ));
        status.generations = None;
        assert!(!already_current(
            &inv.machines[0],
            &desired,
            &status,
            action
        ));
        assert!(already_current(&inv.machines[2], &desired, &status, action));
        status.current_toplevel = None;
        assert!(!already_current(
            &inv.machines[2],
            &desired,
            &status,
            action
        ));

        for spelling in ["/store//system", "/store/./system"] {
            status.current_toplevel = Some(spelling.into());
            status.generations = Some(records(true));
            assert!(!already_current(
                &inv.machines[0],
                &desired,
                &status,
                action
            ));
            assert!(!already_current(
                &inv.machines[2],
                &desired,
                &status,
                action
            ));

            status.current_toplevel = Some(desired.toplevel.clone());
            status.generations.as_mut().unwrap()[0].toplevel = spelling.into();
            assert!(!already_current(
                &inv.machines[0],
                &desired,
                &status,
                action
            ));
            // Carried boot only needs its exact current-profile path observation.
            assert!(already_current(&inv.machines[2], &desired, &status, action));
        }
    }
}

#[test]
fn boot_choices_have_no_default_or_retry_without_dry_first() {
    for text in [b"".as_slice(), b"\n", b"yes", b"continue", b"?", b"\xff"] {
        assert_eq!(boot_choice(text, true), None);
    }
    assert_eq!(boot_choice(b" r \n", false), None);
    assert_eq!(boot_choice(b" R\r\n", true), Some(BootChoice::Retry));
    assert_eq!(boot_choice(b"Y\n", false), Some(BootChoice::Continue));
    assert_eq!(boot_choice(b"s", true), Some(BootChoice::Skip));
    assert_eq!(boot_choice(b"a", false), Some(BootChoice::Abort));
}

struct ActivationRunner {
    calls: Mutex<Vec<ProcessSpec>>,
    fail_profile: bool,
}
impl ProcessRunner for ActivationRunner {
    async fn run(
        &self,
        spec: ProcessSpec,
        _: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        let failed = self.fail_profile && spec.program == std::path::Path::new("nix-env");
        self.calls.lock().unwrap().push(spec);
        Ok(ProcessOutcome {
            status: std::process::ExitStatus::from_raw(if failed { 9 << 8 } else { 0 }),
            stdout: Captured::Memory(b"native output".to_vec()),
            stderr: Captured::Memory(b"native stderr".to_vec()),
            stdout_complete: true,
            stderr_complete: true,
            started: Instant::now(),
            ended: Instant::now(),
            termination: TerminationCause::Exited,
        })
    }
}

#[tokio::test]
async fn activation_profile_failure_and_final_ack_cancellation_preserve_actual_effects_and_primary_errors()
 {
    for (action, fail_profile, reject_final) in [
        (SwitchAction::Boot, true, false),
        (SwitchAction::Boot, false, false),
        (SwitchAction::Boot, false, true),
        (SwitchAction::Switch, true, false),
        (SwitchAction::Switch, false, false),
        (SwitchAction::Switch, false, true),
        (SwitchAction::DryActivate, false, false),
        (SwitchAction::DryActivate, false, true),
    ] {
        let tmp = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(tmp.path()).unwrap();
        let inv = inventory();
        let runner = ActivationRunner {
            calls: Mutex::new(Vec::new()),
            fail_profile,
        };
        let cancel = CancellationToken::new();
        let (events, mut rx) = EventSink::channel(1);
        let token = cancel.clone();
        let consumer = tokio::spawn(async move {
            let mut finals = Vec::new();
            while let Some(d) = rx.receive().await {
                let (event, ack) = d.into_parts();
                if let UiEvent::TaskFinished { task, state } = event {
                    finals.push((task, state));
                    if !fail_profile {
                        token.cancel();
                    }
                    if reject_final {
                        ack.acknowledge(Err("controlled activation final delivery".into()));
                        continue;
                    }
                }
                ack.acknowledge(Ok(()));
            }
            finals
        });
        let copies = Copies {
            runner: &runner,
            events: &events,
            cancellation: &cancel,
            inventory: &inv,
            root: &root,
            rollback: AutoRollback::Disabled,
            ssh_opts: None,
        };
        let ui = crate::terminal::Ui::new(
            std::fs::File::open("/dev/null").unwrap(),
            events.clone(),
            cancel.clone(),
            None,
        )
        .unwrap();
        let activation = Activation {
            copies: &copies,
            ui: &ui,
            ssh_config: None,
            interactive: false,
            dry_first: true,
            action,
            log: tmp.path(),
        };
        let job = Job {
            machine: MachineId(1),
            generation: generation("saved"),
            task: TaskId(1),
        };
        let error = activate_one(&activation, &job, TaskId(9))
            .await
            .unwrap_err();
        if fail_profile {
            assert!(
                error
                    .to_string()
                    .contains(&format!("set profile after {}", action_name(action)))
            );
            assert!(
                matches!(error, ConfctlError::ProcessFailure { evidence, .. }
                if evidence.status.unwrap().code()==Some(9) && evidence.stdout_complete && evidence.stderr_complete)
            );
        } else if reject_final {
            assert!(
                error
                    .to_string()
                    .contains("controlled activation final delivery")
            );
        } else {
            assert!(error.is_cancelled());
        }
        {
            let calls = runner.calls.lock().unwrap();
            assert_eq!(calls[0].args, ["dry-activate"]);
            assert_eq!(calls[1].args, [action_name(action)]);
            if matches!(action, SwitchAction::Boot | SwitchAction::Switch) {
                assert_eq!(calls.len(), 3);
                assert_eq!(calls[2].program, std::path::Path::new("nix-env"));
                assert_eq!(
                    calls[2].args,
                    [
                        "-p",
                        "/nix/var/nix/profiles/system",
                        "--set",
                        "/store/system"
                    ]
                );
            } else {
                assert_eq!(calls.len(), 2);
                assert!(
                    calls
                        .iter()
                        .all(|spec| spec.program != std::path::Path::new("nix-env"))
                );
            }
            assert!(
                calls
                    .iter()
                    .all(|spec| spec.stdout == StreamPolicy::CaptureAndForward
                        && spec.stderr == StreamPolicy::CaptureAndForward)
            );
        }
        ui.shutdown().unwrap();
        drop(ui);
        drop(events);
        assert_eq!(
            consumer.await.unwrap(),
            [(
                TaskId(9),
                if fail_profile {
                    TaskState::Failed
                } else {
                    TaskState::Succeeded
                }
            )]
        );
    }
}

#[tokio::test]
async fn switch_profile_ack_cancellation_suppresses_profile_child_and_keeps_delivery_priority() {
    for reject in [false, true] {
        let tmp = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(tmp.path()).unwrap();
        let inv = inventory();
        let runner = ActivationRunner {
            calls: Mutex::new(Vec::new()),
            fail_profile: false,
        };
        let cancel = CancellationToken::new();
        let (events, mut receiver) = EventSink::channel(1);
        let token = cancel.clone();
        let consumer = tokio::spawn(async move {
            let mut finals = Vec::new();
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                if let UiEvent::Diagnostic { text, .. } = &event
                    && text.contains("set profile after switch")
                {
                    token.cancel();
                    if reject {
                        ack.acknowledge(Err("controlled profile delivery failure".into()));
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
        let copies = Copies {
            runner: &runner,
            events: &events,
            cancellation: &cancel,
            inventory: &inv,
            root: &root,
            rollback: AutoRollback::Disabled,
            ssh_opts: None,
        };
        let ui = crate::terminal::Ui::new(
            std::fs::File::open("/dev/null").unwrap(),
            events.clone(),
            cancel.clone(),
            None,
        )
        .unwrap();
        let activation = Activation {
            copies: &copies,
            ui: &ui,
            ssh_config: None,
            interactive: false,
            dry_first: false,
            action: SwitchAction::Switch,
            log: tmp.path(),
        };
        let job = Job {
            machine: MachineId(1),
            generation: generation("saved"),
            task: TaskId(1),
        };
        let error = activate_one(&activation, &job, TaskId(9))
            .await
            .unwrap_err();
        if reject {
            assert!(
                error
                    .to_string()
                    .contains("controlled profile delivery failure")
            );
        } else {
            assert!(error.is_cancelled());
        }
        {
            let calls = runner.calls.lock().unwrap();
            assert_eq!(calls.len(), 1);
            assert_eq!(calls[0].args, ["switch"]);
        }
        ui.shutdown().unwrap();
        drop(ui);
        drop(events);
        assert_eq!(
            consumer.await.unwrap(),
            [(
                TaskId(9),
                if reject {
                    TaskState::Failed
                } else {
                    TaskState::Canceled
                }
            )]
        );
    }
}

#[tokio::test]
async fn carried_switch_has_one_logical_profile_task_and_retains_success_after_final_ack_cancel() {
    for cancel_final in [false, true] {
        let tmp = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(tmp.path()).unwrap();
        let inv = Inventory::from_value(json!({
            "carrier":{"name":"carrier","metaConfig":{"managed":false,"host":{"target":"localhost"}}},
            "guest":{"name":"carrier#guest","carrier":"carrier","alias":"rack/guest 'alias","metaConfig":{"host":{"target":null}}}
        })).unwrap();
        let runner = ActivationRunner {
            calls: Mutex::new(Vec::new()),
            fail_profile: false,
        };
        let cancel = CancellationToken::new();
        let (events, mut receiver) = EventSink::channel(1);
        let token = cancel.clone();
        let consumer = tokio::spawn(async move {
            let mut seen = Vec::new();
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                if matches!(event, UiEvent::TaskFinished { .. }) && cancel_final {
                    token.cancel();
                }
                seen.push(event);
                ack.acknowledge(Ok(()));
            }
            seen
        });
        let copies = Copies {
            runner: &runner,
            events: &events,
            cancellation: &cancel,
            inventory: &inv,
            root: &root,
            rollback: AutoRollback::Enabled,
            ssh_opts: None,
        };
        let ui = crate::terminal::Ui::new(
            std::fs::File::open("/dev/null").unwrap(),
            events.clone(),
            cancel.clone(),
            None,
        )
        .unwrap();
        let activation = Activation {
            copies: &copies,
            ui: &ui,
            ssh_config: None,
            interactive: true,
            dry_first: true,
            action: SwitchAction::Switch,
            log: tmp.path(),
        };
        let job = Job {
            machine: MachineId(1),
            generation: generation("saved"),
            task: TaskId(1),
        };
        let result = activate_one(&activation, &job, TaskId(9)).await;
        if cancel_final {
            assert!(result.unwrap_err().is_cancelled());
        } else {
            assert!(result.unwrap());
        }
        {
            let calls = runner.calls.lock().unwrap();
            assert_eq!(calls.len(), 1);
            assert_eq!(calls[0].program, std::path::Path::new("carrier-env"));
            assert_eq!(
                calls[0].args,
                [
                    "-p",
                    "/nix/var/nix/profiles/confctl-rack:guest 'alias",
                    "--set",
                    "/store/system"
                ]
            );
            assert_eq!(calls[0].stdout, StreamPolicy::CaptureAndForward);
            assert_eq!(calls[0].stderr, StreamPolicy::CaptureAndForward);
        }
        ui.shutdown().unwrap();
        drop(ui);
        drop(events);
        let seen = consumer.await.unwrap();
        let phases = seen
            .iter()
            .filter_map(|event| match event {
                UiEvent::TaskPending { task, description } => {
                    Some((*task, "pending", description.as_str()))
                }
                UiEvent::TaskStarted { task, description } => {
                    Some((*task, "started", description.as_str()))
                }
                _ => None,
            })
            .collect::<Vec<_>>();
        assert_eq!(
            phases,
            [
                (
                    TaskId(9),
                    "pending",
                    "set carrier profile for carrier#guest on carrier"
                ),
                (
                    TaskId(9),
                    "started",
                    "set carrier profile for carrier#guest on carrier"
                )
            ]
        );
        assert!(seen.iter().any(|e| matches!(
            e,
            UiEvent::TaskFinished {
                task: TaskId(9),
                state: TaskState::Succeeded
            }
        )));
    }
}

#[tokio::test]
async fn test_activation_uses_shared_rollback_policy_without_inventing_helper_success() {
    for (policy, helper) in [
        (AutoRollback::Disabled, true),
        (AutoRollback::Enabled, false),
        (AutoRollback::Configured, true),
    ] {
        let tmp = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(tmp.path()).unwrap();
        let inv = Inventory::from_value(json!({"local":{"name":"local","metaConfig":{"host":{"target":"localhost"},"autoRollback":{"enable":true,"timeout":-11}}}})).unwrap();
        let runner = ActivationRunner {
            calls: Mutex::new(Vec::new()),
            fail_profile: false,
        };
        let cancel = CancellationToken::new();
        let (events, mut receiver) = EventSink::channel(1);
        let consumer = tokio::spawn(async move {
            let mut seen = Vec::new();
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                seen.push(event);
                ack.acknowledge(Ok(()));
            }
            seen
        });
        let copies = Copies {
            runner: &runner,
            events: &events,
            cancellation: &cancel,
            inventory: &inv,
            root: &root,
            rollback: policy,
            ssh_opts: None,
        };
        let ui = crate::terminal::Ui::new(
            std::fs::File::open("/dev/null").unwrap(),
            events.clone(),
            cancel.clone(),
            None,
        )
        .unwrap();
        let activation = Activation {
            copies: &copies,
            ui: &ui,
            ssh_config: None,
            interactive: false,
            dry_first: false,
            action: SwitchAction::Test,
            log: std::path::Path::new("log"),
        };
        let mut generation = generation("saved");
        if !helper {
            generation.auto_rollback = None;
        }
        let job = Job {
            machine: MachineId(0),
            generation,
            task: TaskId(1),
        };
        assert_eq!(
            copy_paths(&inv.machines[0], &job.generation, policy)
                .unwrap()
                .len(),
            if policy == AutoRollback::Configured {
                2
            } else {
                1
            }
        );
        let result = activate_one(&activation, &job, TaskId(10)).await;
        let uses_helper = policy == AutoRollback::Configured;
        if uses_helper {
            assert!(
                result
                    .unwrap_err()
                    .to_string()
                    .contains("without successful confirmation")
            );
        } else {
            result.unwrap();
        }
        {
            let calls = runner.calls.lock().unwrap();
            assert_eq!(calls.len(), 1);
            assert_eq!(
                calls[0].program,
                std::path::Path::new(if uses_helper {
                    "/store/rollback"
                } else {
                    "/store/system/bin/switch-to-configuration"
                })
            );
            if uses_helper {
                assert_eq!(&calls[0].args[..4], ["-t", "-11", "/store/system", "test"]);
                let path = calls[0].args[4]
                    .to_str()
                    .unwrap()
                    .strip_prefix("/run/confctl-confirm-")
                    .unwrap();
                assert_eq!(path.len(), 6);
                assert!(path.bytes().all(|c| c.is_ascii_hexdigit()));
            } else {
                assert_eq!(calls[0].args, ["test"]);
            }
        }
        ui.shutdown().unwrap();
        drop(ui);
        drop(events);
        let seen = consumer.await.unwrap();
        assert!(seen.iter().any(|e| matches!(e, UiEvent::TaskFinished { task: TaskId(10), state } if *state == if uses_helper { TaskState::Failed } else { TaskState::Succeeded })));
        if uses_helper {
            assert!(seen.iter().any(|e| matches!(e, UiEvent::TaskStarted { description, .. } if description.contains("rollback-helper protocol"))));
        }
        if !helper {
            assert!(seen.iter().any(|e| matches!(e, UiEvent::Diagnostic { text, .. } if text.contains("without rollback protection"))));
        }
    }
}

struct StatusRunner {
    active: std::sync::atomic::AtomicUsize,
    peak: std::sync::atomic::AtomicUsize,
    fail: bool,
}
impl ProcessRunner for StatusRunner {
    async fn run(
        &self,
        spec: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        use std::sync::atomic::Ordering;
        let active = self.active.fetch_add(1, Ordering::SeqCst) + 1;
        self.peak.fetch_max(active, Ordering::SeqCst);
        let canceled = tokio::select! {
            _=scope.cancellation.cancelled()=>true,
            _=tokio::time::sleep(std::time::Duration::from_millis(10))=>false,
        };
        self.active.fetch_sub(1, Ordering::SeqCst);
        if canceled {
            return Err(ConfctlError::Cancelled);
        }
        if self.fail && scope.task == TaskId(1) {
            return Err(ConfctlError::State(
                "controlled query infrastructure failure".into(),
            ));
        }
        let bytes = match spec.program.to_str().unwrap() {
            "cat" => b"123.5 0\n".to_vec(),
            "realpath" => b"/store/system\n".to_vec(),
            "bash" => {
                b"/store/system\n/nix/var/nix/profiles/system-1-link;/store/system;1700000000;\n"
                    .to_vec()
            }
            other => panic!("unexpected query {other}"),
        };
        Ok(ProcessOutcome {
            status: std::process::ExitStatus::from_raw(0),
            stdout: Captured::Memory(bytes),
            stderr: Captured::Memory(vec![]),
            stdout_complete: true,
            stderr_complete: true,
            started: Instant::now(),
            ended: Instant::now(),
            termination: TerminationCause::Exited,
        })
    }
}

#[tokio::test]
async fn boot_status_queries_are_bounded_and_join_before_error_or_cancellation_return() {
    for (fail, reject) in [(false, false), (true, false), (false, true)] {
        let tmp = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(tmp.path()).unwrap();
        let mut data = serde_json::Map::new();
        for i in 0..8 {
            data.insert(
                format!("host{i}"),
                json!({"name":format!("host{i}"),"metaConfig":{"host":{"target":"localhost"}}}),
            );
        }
        let inv = Inventory::from_value(data.into()).unwrap();
        let runner = StatusRunner {
            active: 0.into(),
            peak: 0.into(),
            fail,
        };
        let cancel = CancellationToken::new();
        let (events, mut rx) = EventSink::channel(1);
        let consumer = tokio::spawn(async move {
            let mut finals = Vec::new();
            while let Some(delivery) = rx.receive().await {
                let (event, ack) = delivery.into_parts();
                if reject
                    && matches!(
                        event,
                        UiEvent::TaskStarted {
                            task: TaskId(1),
                            ..
                        }
                    )
                {
                    ack.acknowledge(Err("controlled status delivery failure".into()));
                    continue;
                }
                if let UiEvent::TaskFinished { task, state } = event {
                    finals.push((task, state));
                }
                ack.acknowledge(Ok(()));
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
        let jobs = (0..8)
            .map(|i| Job {
                machine: MachineId(i),
                generation: generation("saved"),
                task: TaskId(i as u64 + 1),
            })
            .collect::<Vec<_>>();
        let result = target_statuses(&copies, &jobs, None, SwitchAction::Boot).await;
        if fail || reject {
            let text = result.unwrap_err().to_string();
            assert!(
                text.contains(if fail {
                    "controlled query infrastructure failure"
                } else {
                    "controlled status delivery failure"
                }),
                "{text}"
            );
        } else {
            let statuses = result.unwrap();
            assert_eq!(statuses.len(), 8);
            assert!(statuses.iter().all(|s| already_current(
                &inv.machines[0],
                &generation("saved"),
                s,
                SwitchAction::Boot
            )));
            assert_eq!(runner.peak.load(std::sync::atomic::Ordering::SeqCst), 5);
        }
        assert!(runner.peak.load(std::sync::atomic::Ordering::SeqCst) <= 5);
        assert_eq!(runner.active.load(std::sync::atomic::Ordering::SeqCst), 0);
        drop(events);
        let finals = consumer.await.unwrap();
        if !fail && !reject {
            assert_eq!(finals.len(), 8);
        } else {
            assert!(finals.len() <= 5);
            assert!(finals.iter().any(|(_, state)| *state == TaskState::Failed));
        }
    }
}

#[test]
fn dry_and_test_skip_need_only_exact_known_running_path_including_after_profile_failure() {
    for action in [SwitchAction::DryActivate, SwitchAction::Test] {
        use super::super::host_status::HostStatus;
        let inv = inventory();
        let desired = generation("saved");
        let mut status = HostStatus::default();
        assert!(!already_current(
            &inv.machines[0],
            &desired,
            &status,
            action
        ));
        status.current_toplevel = Some(desired.toplevel.clone());
        assert!(already_current(&inv.machines[0], &desired, &status, action));
        assert!(!already_current(
            &inv.machines[0],
            &desired,
            &status,
            SwitchAction::Boot
        ));
        status.generations = Some(vec![]);
        assert!(already_current(&inv.machines[0], &desired, &status, action));
        for spelling in ["/store//system", "/store/./system"] {
            status.current_toplevel = Some(spelling.into());
            assert!(!already_current(
                &inv.machines[0],
                &desired,
                &status,
                action
            ));
            assert!(!already_current(
                &inv.machines[2],
                &desired,
                &status,
                action
            ));
        }
    }
}
