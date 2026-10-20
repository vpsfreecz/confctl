use super::*;
use serde_json::json;
use std::{fs, path::Path};
#[path = "../../../tests/support/cleanup_fixture.rs"]
pub mod fixture;
#[test]
fn retention_keeps_signed_zero_strict_age_and_current_semantics() {
    for (min, max, age, expected) in [
        (1, 2, 10, vec![0]),
        (0, 0, 0, vec![0, 2]),
        (-1, -1, 0, vec![0, 2]),
        (4, 1, -1, vec![]),
        (1, 9, 10, vec![0]),
        (1, 9, 11, vec![]),
    ] {
        let policy = Retention { min, max, age };
        let values = [(false, 0), (true, 0), (false, 10)];
        assert_eq!(policy.choose(&values, 11).unwrap(), expected);
    }
    let machine = Machine::from_value(
        json!({"name":"h","metaConfig":{"buildGenerations":{"min":0,"max":false,"maxAge":null}}}),
    )
    .unwrap();
    let p = Retention::read(
        &machine,
        &json!({"min":5,"max":-2,"maxAge":-1}),
        "buildGenerations",
    )
    .unwrap();
    assert_eq!((p.min, p.max, p.age), (0, -2, -1_000_000_000));
    let machine =
        Machine::from_value(json!({"name":"h","metaConfig":{"buildGenerations":{"min":"wrong"}}}))
            .unwrap();
    assert!(
        Retention::read(
            &machine,
            &json!({"min":1,"max":2,"maxAge":3}),
            "buildGenerations"
        )
        .is_err()
    );
}
#[test]
fn rotation_gc_preserves_target_build_or_global_host_truthiness_and_carrier_dedup() {
    let scratch = tempfile::tempdir().unwrap();
    let root = ConfigRoot::new(scratch.path()).unwrap();
    for target in [Value::Null, json!(false), json!(true), json!(0)] {
        for global in [Value::Null, json!(false), json!(true), json!(0)] {
            let machine=Machine::from_value(json!({"name":"m","metaConfig":{"buildGenerations":{"collectGarbage":target},"hostGenerations":{"collectGarbage":true}}})).unwrap();
            let settings =
                Settings::decode(json!({"hostGenerations":{"collectGarbage":global}}), &root)
                    .unwrap();
            assert_eq!(
                rotate_gc(&machine, &settings),
                truthy(Some(&target)) || truthy(Some(&global))
            );
        }
    }
    let inv=Inventory::from_value(json!({"a":{"name":"c#one","carrier":"carrier","metaConfig":{"host":{"target":null}}},"b":{"name":"c#two","carrier":"carrier","metaConfig":{"host":{"target":null}}},"c":{"name":"carrier","metaConfig":{"host":{"target":"carrier.invalid"}}}})).unwrap();
    let settings =
        Settings::decode(json!({"hostGenerations":{"collectGarbage":true}}), &root).unwrap();
    let targets = gc_targets(
        &inv,
        &[MachineId(0), MachineId(1)],
        &["c#one", "c#two"],
        true,
        &settings,
    )
    .unwrap();
    assert_eq!(targets.len(), 1);
    assert_eq!(targets[0].name, "carrier");
}
const FIXTURE: &str = "operations::generation_cleanup::tests::actual_cleanup_fixture";
#[test]
fn actual_cleanup_fixture() {
    let Some(root) = std::env::var_os("CLEANUP_ROOT") else {
        return;
    };
    let root = std::path::PathBuf::from(root);
    let args = fs::read_to_string(root.join("command-args.json")).unwrap();
    let config = ConfigRoot::new(&root).unwrap();
    *TEST_ROOTS.lock().unwrap() =
        Some(GcRoots::new(&config, root.join("roots"), "fixture").unwrap());
    let args: Vec<String> = serde_json::from_str(&args).unwrap();
    if let Ok(mode) = fs::read_to_string(root.join("cancel-at")) {
        controlled_cancel(&root, &args, &mode);
        return;
    }
    let code = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(crate::app::run(
            args.into_iter().map(Into::into).collect(),
            crate::timing::Timing::start(),
        ));
    std::process::exit(code);
}
fn run(f: &fixture::Fixture, args: &[&str]) -> std::process::Output {
    fs::write(
        f.root.join("command-args.json"),
        serde_json::to_vec(args).unwrap(),
    )
    .unwrap();
    f.run(
        &std::env::current_exe().unwrap(),
        &["--exact", FIXTURE, "--nocapture"],
        &[],
    )
}
#[test]
fn actual_local_then_remote_unified_removal_keeps_current_and_store_targets() {
    let f = fixture::Fixture::new();
    let target = f.root.join("store-object");
    fs::create_dir(&target).unwrap();
    fs::write(target.join("bytes"), b"store survives").unwrap();
    f.save("alpha", "saved", 1, target.to_str().unwrap());
    let dir = f.root.join(".confctl/generations/alpha");
    std::os::unix::fs::symlink("saved", dir.join("current")).unwrap();
    f.profiles(json!({"/nix/var/nix/profiles/system":[{"id":8,"path":target,"time":1}]}));
    fs::write(f.root.join("check-local-removed"), b"").unwrap();
    fixture::success(&run(
        &f,
        &[
            "generation",
            "rm",
            "--local",
            "--remote",
            "--no-gc",
            "--yes",
            "alpha",
            "current",
        ],
    ));
    assert!(!dir.join("saved").exists());
    assert_eq!(
        fs::read_link(dir.join("current")).unwrap(),
        Path::new("saved")
    );
    assert_eq!(fs::read(target.join("bytes")).unwrap(), b"store survives");
    assert_eq!(
        fs::read_to_string(f.root.join("effects"))
            .unwrap()
            .lines()
            .count(),
        1
    );
    assert!(!f.calls().contains("logname"));
    assert!(!f.calls().contains("nix-collect-garbage"));
}
#[test]
fn actual_partial_local_failure_stops_remote_and_gc_and_rotation_ignores_unused_limit() {
    let f = fixture::Fixture::new();
    f.save("alpha", "saved", 1, "/saved");
    fs::write(
        f.root.join(".confctl/generations/alpha/saved/extra"),
        b"retain",
    )
    .unwrap();
    f.profiles(json!({"/nix/var/nix/profiles/system":[{"id":1,"path":"/saved","time":1}]}));
    let out = run(
        &f,
        &["generation", "rm", "--local", "--remote", "--yes", "alpha"],
    );
    assert!(!out.status.success());
    f.no_deletions();
    assert!(
        f.root
            .join(".confctl/generations/alpha/saved/extra")
            .is_file()
    );
    assert!(
        !f.root
            .join(".confctl/generations/alpha/saved/generation.json")
            .exists()
    );
    let f = fixture::Fixture::new();
    for (name, day) in [("old", 1), ("middle", 2), ("new", 3)] {
        f.save("alpha", name, day, "/saved");
    }
    fixture::success(&run(
        &f,
        &[
            "generation",
            "rotate",
            "--local",
            "--yes",
            "--max-concurrent-gc=0",
            "alpha",
        ],
    ));
    assert!(!f.root.join(".confctl/generations/alpha/old").exists());
    assert!(f.root.join(".confctl/generations/alpha/new").is_dir());
    assert!(!f.calls().contains("nix-collect-garbage"));
}

#[test]
fn local_success_with_unknown_remote_observation_still_marks_host_for_gc() {
    let f = fixture::Fixture::new();
    f.save("alpha", "saved", 1, "/saved");
    fs::write(f.root.join("a.offline"), b"").unwrap();
    let out = run(
        &f,
        &["generation", "rm", "--local", "--remote", "--yes", "alpha"],
    );
    fixture::success(&out);
    assert!(!f.root.join(".confctl/generations/alpha/saved").exists());
    assert!(f.calls().contains("nix-collect-garbage"));
    assert!(!f.root.join("effects").exists());
    assert!(String::from_utf8_lossy(&out.stderr).contains("uptime for alpha"));
}

fn controlled_cancel(root: &Path, args: &[String], mode: &str) {
    use crate::{
        app::Operations, events::EventSink, process::Supervisor, terminal::Ui, timing::Timing,
    };
    use std::time::Duration;
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(async {
        let (events, mut receiver) = EventSink::channel(1);
        let cancel = tokio_util::sync::CancellationToken::new();
        let token = cancel.clone();
        let mode = mode.to_owned();
        let consumer = tokio::spawn(async move {
            let mut pending = Vec::new();
            let mut finished = Vec::new();
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                let stop = match &event {
                    UiEvent::TaskPending { task, description } => {
                        pending.push(*task);
                        mode == "pending" && description.starts_with("remove generation")
                    }
                    UiEvent::TaskFinished { task, .. } => {
                        finished.push(*task);
                        mode == "final" && *task == TaskId(2)
                    }
                    _ => false,
                };
                if stop {
                    token.cancel();
                }
                ack.acknowledge(Ok(()));
            }
            for task in pending {
                assert_eq!(finished.iter().filter(|id| **id == task).count(), 1);
            }
        });
        let ui = Ui::new(std::io::stdin(), events.clone(), cancel.clone(), None).unwrap();
        let processes = Supervisor::new(
            events.clone(),
            root.into(),
            64,
            Duration::from_millis(30),
            None,
            None,
        );
        let ctx = Context::for_test(
            root.into(),
            cancel,
            events,
            processes,
            ui,
            Timing::start(),
            root.join("test.log"),
        );
        let crate::cli::ParseOutcome::Run(invocation) =
            crate::cli::parse(args.iter().map(Into::into).collect()).unwrap()
        else {
            panic!()
        };
        let error = crate::operations::CoreOperations
            .run(&invocation, &ctx)
            .await
            .unwrap_err();
        assert!(error.is_cancelled(), "{error:?}");
        ctx.processes.shutdown().await.unwrap();
        ctx.ui.shutdown().unwrap();
        drop(ctx);
        consumer.await.unwrap();
    });
}
#[test]
fn removal_post_ack_cancellation_settles_task_and_stops_later_gc() {
    for mode in ["pending", "final"] {
        let f = fixture::Fixture::new();
        f.profiles(json!({"/nix/var/nix/profiles/system":[{"id":1,"path":"/saved","time":1}]}));
        fs::write(f.root.join("cancel-at"), mode).unwrap();
        let out = run(&f, &["generation", "rm", "--remote", "--yes", "alpha"]);
        fixture::success(&out);
        assert_eq!(f.root.join("effects").exists(), mode == "final");
        assert!(!f.calls().contains("nix-collect-garbage"));
    }
}
#[test]
fn actual_rotation_plans_remote_before_local_and_keeps_rejected_records() {
    let f = fixture::Fixture::new();
    for (name, day) in [("old", 1), ("middle", 2), ("new", 3)] {
        f.save("alpha", name, day, "/saved");
    }
    let bad = f.root.join(".confctl/generations/alpha/rejected");
    fs::create_dir(&bad).unwrap();
    fs::write(bad.join("generation.json"), b"{legacy").unwrap();
    f.profiles(json!({"/nix/var/nix/profiles/system":[{"id":1,"path":"/current","time":1},{"id":2,"path":"/old","time":2},{"id":3,"path":"/new","time":3}]}));
    fs::write(f.root.join("check-rotation-local"), b"").unwrap();
    fixture::success(&run(
        &f,
        &[
            "generation",
            "rotate",
            "--local",
            "--remote",
            "--yes",
            "alpha",
        ],
    ));
    assert!(!f.root.join(".confctl/generations/alpha/old").exists());
    assert!(f.root.join(".confctl/generations/alpha/new").is_dir());
    assert!(!f.root.join(".confctl/generations/alpha/current").exists());
    assert_eq!(fs::read(bad.join("generation.json")).unwrap(), b"{legacy");
    let effects = fs::read_to_string(f.root.join("effects")).unwrap();
    assert!(effects.contains("delete-generations") && effects.contains("2"));
    assert_eq!(f.calls().matches("nix-collect-garbage").count(), 1);
}
