use super::*;
use crate::{
    configuration::ConfigRoot,
    process::{Captured, ProcessOutcome, ProcessScope, ProcessSpec, TerminationCause},
};
use serde_json::json;
use std::{
    collections::VecDeque,
    os::unix::process::ExitStatusExt,
    sync::{Arc, Mutex},
    time::Instant,
};

fn host(name: &str, input: &str) -> Host {
    Host {
        name: name.into(),
        key: name.into(),
        inputs: object(&json!({"nixpkgs":input}), "fixture").unwrap(),
    }
}
#[test]
fn build_groups_keep_first_seen_input_equivalence_and_safe_attributes() {
    let hosts = vec![host("a", "/one"), host("b", "/two"), host("c", "/one")];
    let actual = groups(hosts.clone(), true)
        .iter()
        .map(|g| g.iter().map(|h| h.name.clone()).collect::<Vec<_>>())
        .collect::<Vec<_>>();
    assert_eq!(actual, [vec!["a", "c"], vec!["b"]]);
    let grouped = groups(hosts.clone(), true);
    assert_eq!(
        description(0, grouped.len(), &grouped[0]),
        "build group 1/2 (2 machines)"
    );
    assert_eq!(
        description(1, grouped.len(), &grouped[1]),
        "build group 2/2 (1 machine)"
    );
    assert_eq!(groups(hosts, false).len(), 1);
    assert_eq!(attr("alpha-key"), "alpha-key");
    assert_eq!(attr("nested/name.\"\\"), "\"nested/name.\\\"\\\\\"");
}
struct Fake {
    replies: Mutex<VecDeque<Value>>,
    calls: Arc<Mutex<Vec<Vec<String>>>>,
}
impl ProcessRunner for Fake {
    async fn run(
        &self,
        spec: ProcessSpec,
        _: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        self.calls.lock().unwrap().push(
            spec.args
                .iter()
                .map(|s| s.to_str().unwrap().into())
                .collect(),
        );
        let value = self
            .replies
            .lock()
            .unwrap()
            .pop_front()
            .expect("declared Nix call");
        Ok(ProcessOutcome {
            status: std::process::ExitStatus::from_raw(0),
            stdout: Captured::Memory(serde_json::to_vec(&value).unwrap()),
            stderr: Captured::Memory(Vec::new()),
            stdout_complete: true,
            stderr_complete: true,
            started: Instant::now(),
            ended: Instant::now(),
            termination: TerminationCause::Exited,
        })
    }
}
fn consume() -> (EventSink, tokio::task::JoinHandle<Vec<UiEvent>>) {
    let (events, mut receiver) = EventSink::channel(1);
    let task = tokio::spawn(async move {
        let mut events = Vec::new();
        while let Some(d) = receiver.receive().await {
            let (e, a) = d.into_parts();
            events.push(e);
            a.acknowledge(Ok(()));
        }
        events
    });
    (events, task)
}
fn diagnostic_text(event: &UiEvent) -> Option<String> {
    match event {
        UiEvent::Diagnostic { text, .. } => Some(text.clone()),
        UiEvent::StyledDiagnostic { parts, .. } => {
            Some(parts.iter().map(|(_, text)| text.as_str()).collect())
        }
        _ => None,
    }
}
#[tokio::test]
async fn selected_build_scope_precedes_preparation_and_always_ends_before_reporting() {
    use crate::{process::Supervisor, terminal::Ui, timing::Timing};
    for case in [
        "plain",
        "success",
        "plan failure",
        "group failure",
        "begin cancel",
        "group cancel",
        "end reject",
        "plan failure end reject",
        "begin reject",
        "pre canceled",
    ] {
        let scratch = tempfile::tempdir().unwrap();
        std::fs::write(scratch.path().join("flake.nix"), "{}").unwrap();
        let root = ConfigRoot::new(scratch.path()).unwrap();
        let cancellation = CancellationToken::new();
        let guard = MutationLock::acquire(&root, &cancellation).await.unwrap();
        let (events, mut receiver) = EventSink::channel(1);
        let calls = Arc::new(Mutex::new(Vec::new()));
        let recorded = calls.clone();
        let cancel = cancellation.clone();
        let consumer = tokio::spawn(async move {
            let mut seen = Vec::new();
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                if event == UiEvent::BuildPhase(BuildPhase::Begin) {
                    assert!(
                        recorded.lock().unwrap().is_empty(),
                        "Begin precedes settings/plan admission"
                    );
                    if case == "begin cancel" {
                        cancel.cancel();
                    }
                }
                if matches!(event, UiEvent::TaskStarted { .. }) && case == "group cancel" {
                    cancel.cancel();
                }
                let reject = (event == UiEvent::BuildPhase(BuildPhase::Begin)
                    && case == "begin reject")
                    || (event == UiEvent::BuildPhase(BuildPhase::End)
                        && case.ends_with("end reject"));
                seen.push(event);
                ack.acknowledge(if reject {
                    Err("scope rejected".into())
                } else {
                    Ok(())
                });
            }
            seen
        });
        let processes = Supervisor::new(
            events.clone(),
            scratch.path().into(),
            1024,
            std::time::Duration::from_millis(30),
            None,
            None,
        );
        let ui = Ui::new(
            std::fs::File::open("/dev/null").unwrap(),
            events.clone(),
            cancellation.clone(),
            None,
        )
        .unwrap();
        let mut ctx = Context::for_test(
            scratch.path().into(),
            cancellation.clone(),
            events.clone(),
            processes.clone(),
            ui,
            Timing::start(),
            scratch.path().join("log"),
        );
        ctx.set_full_ui_available(case != "plain");
        let preparation_failure = case.starts_with("plan failure");
        let client = NixClient::stateless(root.clone(), Fake {
            replies: Mutex::new([
                if case == "group failure" { json!({"nix":{"legacyNixPath":true,"impureEval":false}}) } else { json!({}) },
                if preparation_failure { json!({}) } else { json!({"a":{"key":"a","inputs":{}}}) },
                json!([]),
                json!({"a":{"toplevel":scratch.path().join("system"),"autoRollback":scratch.path().join("rollback")}}),
                json!({"a":{}}),
            ].into()), calls: calls.clone(),
        }, NixOptions::default(), cancellation.clone(), ConfigEpoch::default());
        let inventory = Inventory::from_value(json!({"a":{"name":"a"}})).unwrap();
        let mut zero = 0;
        assert!(
            build_selected(
                &ctx,
                &client,
                &inventory,
                &[MachineId(0)],
                &guard,
                &mut zero
            )
            .await
            .is_err()
        );
        assert!(
            calls.lock().unwrap().is_empty(),
            "task-ID precheck stays outside the scope"
        );
        if case == "pre canceled" {
            cancellation.cancel();
        }
        if !preparation_failure && !case.starts_with("begin ") && case != "pre canceled" {
            std::fs::create_dir(scratch.path().join("system")).unwrap();
            std::fs::write(scratch.path().join("rollback"), "helper").unwrap();
            set_test_roots(GcRoots::new(&root, scratch.path().join("roots"), "test").unwrap());
        }
        let mut next = 1;
        let result = build_selected(
            &ctx,
            &client,
            &inventory,
            &[MachineId(0)],
            &guard,
            &mut next,
        )
        .await;
        let succeeded = matches!(case, "success" | "plain");
        assert_eq!(result.is_ok(), succeeded, "{case}: {result:?}");
        if case.ends_with("cancel") || case == "pre canceled" {
            assert!(result.as_ref().unwrap_err().is_cancelled());
        }
        if case == "plan failure end reject" {
            assert!(
                matches!(&result, Err(ConfctlError::Cleanup { primary, secondary }) if primary.to_string().contains("missing build plan") && secondary.len() == 1)
            );
        }
        let published = scratch
            .path()
            .join(".confctl/generations/a/current")
            .is_symlink();
        assert_eq!(
            published,
            succeeded || case == "end reject",
            "{case}: no rollback/repeated publication"
        );
        if succeeded {
            ctx.events
                .emit(UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: "status continuation\n".into(),
                })
                .await
                .unwrap();
        }
        ctx.ui.shutdown().unwrap();
        processes.shutdown().await.unwrap();
        drop(client);
        drop(ctx);
        drop(processes);
        drop(events);
        let seen = consumer.await.unwrap();
        let begins = seen
            .iter()
            .filter(|e| **e == UiEvent::BuildPhase(BuildPhase::Begin))
            .count();
        let ends = seen
            .iter()
            .filter(|e| **e == UiEvent::BuildPhase(BuildPhase::End))
            .count();
        assert_eq!(
            (begins, ends),
            if matches!(case, "plain" | "pre canceled") {
                (0, 0)
            } else if case == "begin reject" {
                (1, 0)
            } else {
                (1, 1)
            },
            "{case}"
        );
        let header = seen
            .iter()
            .position(|e| diagnostic_text(e).is_some_and(|t| t == "Built generations:\n"));
        assert_eq!(header.is_some(), succeeded, "{case}");
        if let Some(header) = header {
            assert_eq!(
                seen.iter()
                    .filter(|e| diagnostic_text(e).is_some_and(|t| t == "Built generations:\n"))
                    .count(),
                1
            );
            let continuation = seen
                .iter()
                .position(|e| diagnostic_text(e).is_some_and(|t| t == "status continuation\n"))
                .unwrap();
            assert!(continuation > header);
            if case != "plain" {
                let end = seen
                    .iter()
                    .position(|e| *e == UiEvent::BuildPhase(BuildPhase::End))
                    .unwrap();
                let checkpoint = seen
                    .iter()
                    .position(|e| matches!(e, UiEvent::Checkpoint { .. }))
                    .unwrap();
                assert!(checkpoint < end && end < header);
            }
        }
        let calls = calls.lock().unwrap();
        for call in calls
            .iter()
            .filter(|c| c.first().is_some_and(|a| a == "build"))
        {
            assert_eq!(
                call.windows(2)
                    .any(|w| w == ["--log-format", "internal-json"]),
                case != "plain"
            );
        }
        if case.starts_with("begin ") {
            assert!(calls.is_empty());
        }
    }
}

#[tokio::test]
async fn build_legacy_inputs_fallback_conflicts_and_configured_role_order() {
    let scratch = tempfile::tempdir().unwrap();
    std::fs::write(scratch.path().join("flake.nix"), "{}").unwrap();
    let runner = Fake {
        replies: Mutex::new(
            [
                json!({}),
                json!({"a":"a","b":"b"}),
                json!({"a":{"nixpkgs":"/one","vpsadmin":"/v"}}),
                json!({"nixpkgs":"/one","vpsadmin":"/v"}),
                json!({"a":{"nixpkgs":"/one"},"b":{"nixpkgs":"/two"}}),
            ]
            .into(),
        ),
        calls: Arc::new(Mutex::new(Vec::new())),
    };
    let recorded = runner.calls.clone();
    let client = NixClient::stateless(
        ConfigRoot::new(scratch.path()).unwrap(),
        runner,
        NixOptions::default(),
        CancellationToken::new(),
        ConfigEpoch::default(),
    );
    let settings=crate::configuration::Settings::decode(json!({"nix":{"legacyNixPath":true,"impureEval":true,"legacyNixPathMap":["vpsadmin","nixpkgs"]}}),client.root()).unwrap().nix;
    let hosts = vec![host("a", "/one"), host("b", "/one")];
    assert_eq!(
        legacy_args(&client, &hosts, &settings).await.unwrap(),
        ["-I", "vpsadmin=/v", "-I", "nixpkgs=/one"]
    );
    assert!(
        legacy_args(&client, &hosts, &settings)
            .await
            .unwrap_err()
            .to_string()
            .contains("consistent input nixpkgs")
    );
    let mut pure = settings;
    pure.impure_eval = false;
    assert!(
        legacy_args(&client, &hosts, &pure)
            .await
            .unwrap_err()
            .to_string()
            .contains("requires impureEval")
    );
    let calls = recorded.lock().unwrap();
    assert_eq!(calls.len(), 5);
    assert!(calls[2].iter().any(|s| s.contains("[ \"a\" \"b\" ]")));
    assert_eq!(calls[3].last().unwrap(), ".#confctl.inputs.b");
}
#[tokio::test]
async fn build_publication_failure_keeps_reused_current_and_partial_new_record() {
    let scratch = tempfile::tempdir().unwrap();
    std::fs::write(scratch.path().join("flake.nix"), "{}").unwrap();
    let root = ConfigRoot::new(scratch.path()).unwrap();
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&root, &cancel).await.unwrap();
    let roots = GcRoots::new(&root, scratch.path().join("roots"), "test").unwrap();
    let store = GenerationStore::new(root.clone(), roots.clone());
    let date = DateTime::parse_from_rfc3339("2000-01-01T00:00:00Z").unwrap();
    let old = BuildGeneration {
        name: "old".into(),
        date,
        toplevel: "/system-a".into(),
        auto_rollback: Some("/rollback".into()),
        inputs: Map::new(),
        inputs_info: Map::new(),
    };
    store.publish(&guard, "a", &old).unwrap();
    let current = store.host_directory("a").unwrap().join("current");
    std::fs::remove_file(&current).unwrap();
    std::os::unix::fs::symlink("missing", &current).unwrap();
    let time = DateTime::parse_from_rfc3339("2020-01-01T00:00:00Z").unwrap();
    let collision = roots
        .directory()
        .join("b-generation-2020-01-01--00-00-00-toplevel");
    std::fs::write(collision, b"occupied root").unwrap();
    let client=NixClient::stateless(root,Fake{replies:Mutex::new([json!({}),json!([]),json!({"a":{"toplevel":"/system-a","autoRollback":"/rollback"},"b":{"toplevel":"/system-b","autoRollback":"/rollback"}}),json!({"b":{}})].into()),calls:Arc::new(Mutex::new(Vec::new()))},NixOptions::default(),cancel.clone(),ConfigEpoch::default());
    let (events, consumer) = consume();
    let mut saved = Vec::new();
    let publication = Publication {
        store: &store,
        guard: &guard,
        events: &events,
        cancellation: &cancel,
        time,
    };
    let settings = crate::configuration::Settings::decode(json!({}), client.root())
        .unwrap()
        .nix;
    let hosts = [
        Host {
            name: "a".into(),
            key: "a".into(),
            inputs: Map::new(),
        },
        Host {
            name: "b".into(),
            key: "b".into(),
            inputs: Map::new(),
        },
    ];
    let error = build_group(
        &client,
        &hosts,
        &settings,
        BuildTask {
            id: TaskId(1),
            live_progress: false,
        },
        &publication,
        &mut saved,
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("create generation GC root"));
    assert_eq!(
        saved
            .iter()
            .map(|(host, generation)| (host.as_str(), generation.name.as_str()))
            .collect::<Vec<_>>(),
        [("a", "old")]
    );
    assert_eq!(saved[0].1.encode(), old.encode());
    assert_eq!(std::fs::read_link(current).unwrap(), PathBuf::from("old"));
    let new = store.host_directory("b").unwrap();
    assert!(new.join("2020-01-01--00-00-00/generation.json").is_file());
    assert!(!new.join("current").exists());
    drop(events);
    assert!(
        consumer
            .await
            .unwrap()
            .iter()
            .any(|e| diagnostic_text(e).is_some_and(|text| text.contains("a: old (reused)")))
    );
}

#[tokio::test]
async fn build_group_failure_finishes_shared_task_and_skips_unentered_groups() {
    let scratch = tempfile::tempdir().unwrap();
    std::fs::write(scratch.path().join("flake.nix"), "{}").unwrap();
    let root = ConfigRoot::new(scratch.path()).unwrap();
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&root, &cancel).await.unwrap();
    let roots = GcRoots::new(&root, scratch.path().join("roots"), "test").unwrap();
    let store = GenerationStore::new(root.clone(), roots);
    let client = NixClient::stateless(
        root,
        Fake {
            replies: Mutex::new([json!({}), json!([]), json!({})].into()),
            calls: Arc::new(Mutex::new(Vec::new())),
        },
        NixOptions::default(),
        cancel.clone(),
        ConfigEpoch::default(),
    );
    let (events, consumer) = consume();
    let publication = Publication {
        store: &store,
        guard: &guard,
        events: &events,
        cancellation: &cancel,
        time: DateTime::parse_from_rfc3339("2020-01-01T00:00:00Z").unwrap(),
    };
    let settings = crate::configuration::Settings::decode(json!({}), client.root())
        .unwrap()
        .nix;
    let error = build_groups(
        &client,
        &[vec![host("a", "/one")], vec![host("b", "/two")]],
        &settings,
        &publication,
        &scratch.path().join("log"),
        TaskId(41),
        false,
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("missing build outputs"));
    drop(events);
    let events = consumer.await.unwrap();
    assert!(
        matches!(&events[0],UiEvent::TaskPending{task:TaskId(41),description} if description == "build group 1/2 (1 machine)")
    );
    assert!(events.iter().any(|event| matches!(
        event,
        UiEvent::TaskPending {
            task: TaskId(42),
            ..
        }
    )));
    assert!(events.iter().any(|event| matches!(
        event,
        UiEvent::TaskStarted {
            task: TaskId(41),
            ..
        }
    )));
    assert!(!events.iter().any(|event| matches!(
        event,
        UiEvent::TaskStarted {
            task: TaskId(42),
            ..
        }
    )));
    assert!(events.iter().any(|event| matches!(
        event,
        UiEvent::TaskFinished {
            task: TaskId(41),
            state: TaskState::Failed
        }
    )));
    assert!(events.iter().any(|event| matches!(
        event,
        UiEvent::TaskFinished {
            task: TaskId(42),
            state: TaskState::Skipped
        }
    )));
}

#[tokio::test]
async fn build_completion_ack_cancellation_retains_publication_and_actual_group_finals() {
    for (boundary, reuse, later_group) in [
        ("saved", false, false),
        ("saved", true, false),
        ("finished", false, false),
        ("finished", false, true),
        ("summary header", false, false),
        ("summary row", false, false),
    ] {
        // A delivery failure at the same boundary must remain the primary error.
        for reject_ack in [false, true] {
            let scratch = tempfile::tempdir().unwrap();
            std::fs::write(scratch.path().join("flake.nix"), "{}").unwrap();
            let root = ConfigRoot::new(scratch.path()).unwrap();
            let cancel = CancellationToken::new();
            let guard = MutationLock::acquire(&root, &cancel).await.unwrap();
            let roots = GcRoots::new(&root, scratch.path().join("roots"), "test").unwrap();
            let store = GenerationStore::new(root.clone(), roots.clone());
            let time = DateTime::parse_from_rfc3339("2020-01-01T00:00:00Z").unwrap();
            let first_host = host("a", "/one");
            let expected_name = if reuse { "old" } else { "2020-01-01--00-00-00" };
            if reuse {
                store
                    .publish(
                        &guard,
                        "a",
                        &BuildGeneration {
                            name: expected_name.into(),
                            date: time,
                            toplevel: "/system-a".into(),
                            auto_rollback: Some("/rollback".into()),
                            inputs: first_host.inputs.clone(),
                            inputs_info: Map::new(),
                        },
                    )
                    .unwrap();
                let current = store.host_directory("a").unwrap().join("current");
                std::fs::remove_file(&current).unwrap();
                std::os::unix::fs::symlink("missing", current).unwrap();
            }
            let client = NixClient::stateless(
                root,
                Fake {
                    replies: Mutex::new(
                        [
                            json!({}),
                            json!([]),
                            json!({"a":{"toplevel":"/system-a","autoRollback":"/rollback"}}),
                            json!({"a":{}}),
                        ]
                        .into(),
                    ),
                    calls: Arc::new(Mutex::new(Vec::new())),
                },
                NixOptions::default(),
                cancel.clone(),
                ConfigEpoch::default(),
            );
            let (events, mut receiver) = EventSink::channel(1);
            let consumer_cancel = cancel.clone();
            let consumer = tokio::spawn(async move {
                let mut seen = Vec::new();
                let mut triggered = false;
                while let Some(delivery) = receiver.receive().await {
                    let (event, ack) = delivery.into_parts();
                    let at_boundary = match &event {
                        UiEvent::TaskFinished {
                            task: TaskId(1),
                            state: TaskState::Succeeded,
                        } => boundary == "finished",
                        UiEvent::Diagnostic {
                            stream: OutputStream::Stdout,
                            ..
                        }
                        | UiEvent::StyledDiagnostic {
                            stream: OutputStream::Stdout,
                            ..
                        } => match boundary {
                            "saved" => diagnostic_text(&event).unwrap().starts_with("Saved a:"),
                            "summary header" => {
                                diagnostic_text(&event).unwrap() == "Built generations:\n"
                            }
                            "summary row" => {
                                diagnostic_text(&event).unwrap()
                                    == format!("  a: {expected_name}\n")
                            }
                            _ => false,
                        },
                        _ => false,
                    };
                    seen.push(event);
                    if at_boundary {
                        assert!(!triggered);
                        triggered = true;
                        consumer_cancel.cancel();
                        if reject_ack {
                            ack.acknowledge(Err("controlled completion failure".into()));
                            continue;
                        }
                    }
                    ack.acknowledge(Ok(()));
                }
                assert!(triggered, "boundary not reached: {boundary}");
                seen
            });
            let publication = Publication {
                store: &store,
                guard: &guard,
                events: &events,
                cancellation: &cancel,
                time,
            };
            let settings = crate::configuration::Settings::decode(json!({}), client.root())
                .unwrap()
                .nix;
            let mut groups = vec![vec![first_host]];
            if later_group {
                groups.push(vec![host("b", "/two")]);
            }
            let result = match build_groups(
                &client,
                &groups,
                &settings,
                &publication,
                &scratch.path().join("log"),
                TaskId(1),
                false,
            )
            .await
            {
                Ok(saved) => {
                    report_saved(publication.events, publication.cancellation, &saved).await
                }
                Err(error) => Err(error),
            };
            let error = result.unwrap_err();
            if reject_ack {
                assert!(
                    matches!(&error, ConfctlError::EventDelivery(message) if message == "controlled completion failure"),
                    "{error:?}"
                );
            } else {
                assert!(error.is_cancelled(), "{error:?}");
            }
            drop(events);
            let events = consumer.await.unwrap();
            let group_state = if boundary == "saved" {
                if reject_ack {
                    TaskState::Failed
                } else {
                    TaskState::Canceled
                }
            } else {
                TaskState::Succeeded
            };
            let mut expected_finals = vec![(TaskId(1), group_state)];
            if later_group {
                expected_finals.push((TaskId(2), TaskState::Canceled));
                assert!(!events.iter().any(|event| matches!(
                    event,
                    UiEvent::TaskStarted {
                        task: TaskId(2),
                        ..
                    }
                )));
                assert!(!store.host_directory("b").unwrap().exists());
            }
            assert_eq!(
                events
                    .iter()
                    .filter_map(|event| match event {
                        UiEvent::TaskFinished { task, state } => Some((*task, *state)),
                        _ => None,
                    })
                    .collect::<Vec<_>>(),
                expected_finals
            );
            assert!(events.iter().any(|event| matches!(event,
                UiEvent::Checkpoint { key: TaskId(1), state, tasks, .. } if *state == group_state && tasks == &[TaskId(1)])));
            assert!(
                !events
                    .iter()
                    .any(|event| matches!(event, UiEvent::Checkpoint { key: TaskId(2), .. })),
                "unentered group has no checkpoint"
            );
            let inventory = store.load("a").unwrap();
            assert_eq!(inventory.current.as_deref(), Some(expected_name));
            assert_eq!(inventory.generations.len(), 1);
            assert_eq!(inventory.generations[0].inputs, groups[0][0].inputs);
            let directory = store.host_directory("a").unwrap().join(expected_name);
            assert!(directory.join("generation.json").is_file());
            assert_eq!(
                std::fs::read_link(directory.join("toplevel")).unwrap(),
                PathBuf::from("/system-a")
            );
            assert_eq!(std::fs::read_dir(roots.directory()).unwrap().count(), 3);
            for suffix in ["toplevel", "auto_rollback", "input.nixpkgs"] {
                let link = if suffix == "input.nixpkgs" {
                    "nixpkgs.input"
                } else {
                    suffix
                };
                assert_eq!(
                    std::fs::read_link(
                        roots
                            .directory()
                            .join(format!("a-generation-{expected_name}-{suffix}"))
                    )
                    .unwrap(),
                    directory.join(link)
                );
            }
        }
    }
}

#[tokio::test]
async fn two_build_groups_checkpoint_after_each_real_publication() {
    let scratch = tempfile::tempdir().unwrap();
    std::fs::write(scratch.path().join("flake.nix"), "{}").unwrap();
    let root = ConfigRoot::new(scratch.path()).unwrap();
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&root, &cancel).await.unwrap();
    let roots = GcRoots::new(&root, scratch.path().join("roots"), "test").unwrap();
    let check_store = GenerationStore::new(root.clone(), roots.clone());
    let store = GenerationStore::new(root.clone(), roots);
    let calls = Arc::new(Mutex::new(Vec::new()));
    struct PreparedFake {
        inner: Fake,
        ready: Arc<std::sync::atomic::AtomicBool>,
    }
    impl ProcessRunner for PreparedFake {
        async fn run(
            &self,
            spec: ProcessSpec,
            scope: ProcessScope,
        ) -> Result<ProcessOutcome, ConfctlError> {
            assert!(
                self.ready.load(std::sync::atomic::Ordering::SeqCst),
                "Nix/evaluation admitted before the group's acknowledged unknown progress"
            );
            self.inner.run(spec, scope).await
        }
    }
    let ready = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let client = NixClient::stateless(
        root,
        PreparedFake {
            ready: ready.clone(),
            inner: Fake {
                replies: Mutex::new(
                    [
                        json!({}),
                        json!([]),
                        json!({"a":{"toplevel":"/system-a","autoRollback":"/rollback"}}),
                        json!({"a":{}}),
                        json!([]),
                        json!({"b":{"toplevel":"/system-b","autoRollback":"/rollback"}}),
                        json!({"b":{}}),
                    ]
                    .into(),
                ),
                calls: calls.clone(),
            },
        },
        NixOptions::default(),
        cancel.clone(),
        ConfigEpoch::default(),
    );
    let (events, mut receiver) = EventSink::channel(1);
    let consumer = tokio::spawn(async move {
        let mut seen = Vec::new();
        while let Some(delivery) = receiver.receive().await {
            let (event, ack) = delivery.into_parts();
            match &event {
                UiEvent::TaskStarted { .. } => {
                    ready.store(false, std::sync::atomic::Ordering::SeqCst)
                }
                UiEvent::BuildProgress {
                    building: None,
                    fetching: None,
                    ..
                } => ready.store(true, std::sync::atomic::Ordering::SeqCst),
                _ => {}
            }
            if let UiEvent::Checkpoint { key, state, .. } = &event {
                assert_eq!(*state, TaskState::Succeeded);
                let host = if *key == TaskId(1) { "a" } else { "b" };
                assert_eq!(check_store.load(host).unwrap().generations.len(), 1);
            }
            seen.push(event);
            ack.acknowledge(Ok(()));
        }
        seen
    });
    let publication = Publication {
        store: &store,
        guard: &guard,
        events: &events,
        cancellation: &cancel,
        time: DateTime::parse_from_rfc3339("2020-01-01T00:00:00Z").unwrap(),
    };
    let settings = crate::configuration::Settings::decode(json!({}), client.root())
        .unwrap()
        .nix;
    let saved = build_groups(
        &client,
        &[vec![host("a", "/one")], vec![host("b", "/two")]],
        &settings,
        &publication,
        &scratch.path().join("log"),
        TaskId(1),
        true,
    )
    .await
    .unwrap();
    assert_eq!(saved.len(), 2);
    report_saved(publication.events, publication.cancellation, &saved)
        .await
        .unwrap();
    drop(events);
    let events = consumer.await.unwrap();
    for task in [TaskId(1), TaskId(2)] {
        let start = events
            .iter()
            .position(|event| matches!(event, UiEvent::TaskStarted { task: id, .. } if *id == task))
            .unwrap();
        assert!(
            matches!(events[start + 1], UiEvent::BuildProgress { task: id, building: None, fetching: None } if id == task)
        );
    }
    let reports = events
        .iter()
        .filter(|event| {
            diagnostic_text(event).is_some_and(|text| {
                text == "Built generations:\n"
                    || text.starts_with("  a:")
                    || text.starts_with("  b:")
            })
        })
        .collect::<Vec<_>>();
    assert_eq!(
        reports.len(),
        3,
        "one header and one existing line per saved generation"
    );
    assert!(matches!(
        reports[0],
        UiEvent::Diagnostic {
            style: Style::Heading,
            ..
        }
    ));
    for (event, (host, generation)) in reports[1..].iter().zip(&saved) {
        let UiEvent::StyledDiagnostic {
            stream: OutputStream::Stdout,
            parts,
        } = event
        else {
            panic!("styled generation line")
        };
        assert_eq!(
            parts,
            &vec![
                (Style::Plain, format!("  {host}: ")),
                (Style::Generation, generation.name.clone()),
                (Style::Plain, "\n".into()),
            ]
        );
        assert_eq!(
            diagnostic_text(event).unwrap(),
            format!("  {host}: {}\n", generation.name)
        );
    }
    let checkpoints = events
        .iter()
        .enumerate()
        .filter_map(|(index, event)| match event {
            UiEvent::Checkpoint { key, tasks, .. } => Some((index, *key, tasks)),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(checkpoints.len(), 2);
    for (index, key, tasks) in checkpoints {
        assert_eq!(tasks, &[key]);
        assert!(
            matches!(events[index-1], UiEvent::TaskFinished { task, state: TaskState::Succeeded } if task == key)
        );
    }
    let first = events
        .iter()
        .position(|event| matches!(event, UiEvent::Checkpoint { key: TaskId(1), .. }))
        .unwrap();
    let second = events
        .iter()
        .position(|event| {
            matches!(
                event,
                UiEvent::TaskStarted {
                    task: TaskId(2),
                    ..
                }
            )
        })
        .unwrap();
    assert!(first < second);
    let calls = calls.lock().unwrap();
    for call in calls
        .iter()
        .filter(|call| call.first().is_some_and(|arg| arg == "build"))
    {
        assert!(
            call.windows(2)
                .any(|pair| pair == ["--log-format", "internal-json"])
        );
    }
}

#[tokio::test]
async fn unknown_group_preparation_precedes_legacy_failure_and_honors_acknowledgement() {
    for stop in [
        "legacy failure",
        "cancel acknowledgement",
        "reject acknowledgement",
    ] {
        let scratch = tempfile::tempdir().unwrap();
        std::fs::write(scratch.path().join("flake.nix"), "{}").unwrap();
        let root = ConfigRoot::new(scratch.path()).unwrap();
        let cancel = CancellationToken::new();
        let guard = MutationLock::acquire(&root, &cancel).await.unwrap();
        let store = GenerationStore::new(
            root.clone(),
            GcRoots::new(&root, scratch.path().join("roots"), "test").unwrap(),
        );
        let calls = Arc::new(Mutex::new(Vec::new()));
        let client = NixClient::stateless(
            root,
            Fake {
                replies: Mutex::new(VecDeque::new()),
                calls: calls.clone(),
            },
            NixOptions::default(),
            cancel.clone(),
            ConfigEpoch::default(),
        );
        let (events, mut receiver) = EventSink::channel(1);
        let token = cancel.clone();
        let consumer = tokio::spawn(async move {
            let mut seen = Vec::new();
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                let unknown = matches!(
                    event,
                    UiEvent::BuildProgress {
                        building: None,
                        fetching: None,
                        ..
                    }
                );
                seen.push(event);
                if unknown && stop == "cancel acknowledgement" {
                    token.cancel();
                }
                ack.acknowledge(if unknown && stop == "reject acknowledgement" {
                    Err("preparation refused".into())
                } else {
                    Ok(())
                });
            }
            seen
        });
        let publication = Publication {
            store: &store,
            guard: &guard,
            events: &events,
            cancellation: &cancel,
            time: DateTime::parse_from_rfc3339("2020-01-01T00:00:00Z").unwrap(),
        };
        let settings = crate::configuration::Settings::decode(
            json!({"nix":{"legacyNixPath":true,"impureEval":false}}),
            client.root(),
        )
        .unwrap()
        .nix;
        let error = build_groups(
            &client,
            &[vec![host("a", "/one")], vec![host("b", "/two")]],
            &settings,
            &publication,
            &scratch.path().join("log"),
            TaskId(1),
            true,
        )
        .await
        .unwrap_err();
        assert!(
            calls.lock().unwrap().is_empty(),
            "no Nix admitted after preparation rejection or invalid legacy settings"
        );
        if stop == "legacy failure" {
            assert!(error.to_string().contains("requires impureEval"));
        }
        if stop == "cancel acknowledgement" {
            assert!(error.is_cancelled());
        }
        if stop == "reject acknowledgement" {
            assert!(error.to_string().contains("preparation refused"));
        }
        drop(events);
        let events = consumer.await.unwrap();
        let start = events
            .iter()
            .position(|event| {
                matches!(
                    event,
                    UiEvent::TaskStarted {
                        task: TaskId(1),
                        ..
                    }
                )
            })
            .unwrap();
        assert!(matches!(
            events[start + 1],
            UiEvent::BuildProgress {
                task: TaskId(1),
                building: None,
                fetching: None
            }
        ));
        assert!(!events.iter().any(|event| matches!(
            event,
            UiEvent::BuildProgress {
                task: TaskId(2),
                ..
            } | UiEvent::TaskFinished {
                state: TaskState::Succeeded,
                ..
            }
        )));
        assert!(!events.iter().any(|event| {
            diagnostic_text(event).is_some_and(|text| text.starts_with("Built generations:"))
        }));
        assert!(events.iter().any(|event| matches!(event, UiEvent::TaskFinished { task: TaskId(1), state } if *state == if stop == "cancel acknowledgement" { TaskState::Canceled } else { TaskState::Failed })));
    }
}
