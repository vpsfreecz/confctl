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
        "build group 1/2 (2 machines): a, c"
    );
    assert_eq!(
        description(1, grouped.len(), &grouped[1]),
        "build group 2/2 (1 machine): b"
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
        TaskId(1),
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
            .any(|e| matches!(e,UiEvent::Diagnostic{text,..} if text.contains("a: old (reused)")))
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
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("missing build outputs"));
    drop(events);
    let events = consumer.await.unwrap();
    assert!(
        matches!(&events[0],UiEvent::TaskPending{task:TaskId(41),description} if description.contains("a"))
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
                            text,
                            ..
                        } => match boundary {
                            "saved" => text.starts_with("Saved a:"),
                            "summary header" => text == "Built generations:\n",
                            "summary row" => text == &format!("  a: {expected_name}\n"),
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
            )
            .await
            {
                Ok(saved) => report_saved(&publication, &saved).await,
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
