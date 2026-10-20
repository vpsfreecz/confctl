#[path = "support/cleanup_fixture.rs"]
pub mod fixture;
use fixture::Fixture;
use std::{
    fs,
    path::Path,
    time::{Duration, Instant},
};
fn binary() -> &'static Path {
    Path::new(env!("CARGO_BIN_EXE_confctl-rust"))
}
#[test]
fn gc_uses_runnable_unmanaged_targets_and_native_failures_continue_then_aggregate() {
    let f = Fixture::new();
    fs::write(f.root.join("a.fail"), b"").unwrap();
    let out = f.run(
        binary(),
        &[
            "collect-garbage",
            "--yes",
            "--max-concurrent-gc=1",
            "--tag=chosen",
        ],
        &[],
    );
    assert!(!out.status.success());
    let calls = f.calls();
    assert_eq!(calls.matches("nix-collect-garbage").count(), 2);
    assert!(!calls.contains(" -d"));
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(
        text.contains("alpha> deleting")
            && text.contains("beta> deleting")
            && text.contains("alpha: error")
            && text.contains("beta: 2 store paths deleted")
    );
    assert!(String::from_utf8_lossy(&out.stderr).contains("alpha"));
    assert!(
        fs::read_to_string(f.root.join("concurrency"))
            .unwrap()
            .lines()
            .all(|v| v == "1")
    );
    let f = Fixture::new();
    let out = f.run(binary(), &["collect-garbage", "alpha"], b"n\n");
    assert!(!out.status.success());
    assert!(!f.root.join(".confctl/mutation.lock").exists());
    f.no_deletions();
    for limit in ["0", "-1"] {
        let f = Fixture::new();
        assert!(
            !f.run(
                binary(),
                &[
                    "collect-garbage",
                    "--yes",
                    "--max-concurrent-gc",
                    limit,
                    "alpha"
                ],
                &[]
            )
            .status
            .success()
        );
        f.no_deletions();
    }
    let f = Fixture::new();
    assert!(
        !f.run(binary(), &["collect-garbage", "--yes", "--tag=guest"], &[])
            .status
            .success()
    );
    f.no_deletions();
}
#[test]
fn both_streams_are_labelled_before_two_gc_children_exit_and_cancel_joins_under_guard() {
    for cancel in [false, true] {
        let f = Fixture::new();
        fs::write(f.root.join("gate"), b"").unwrap();
        let stdout = f.root.join("stdout");
        let stderr = f.root.join("stderr");
        let mut child = f
            .command(
                binary(),
                &[
                    "collect-garbage",
                    "--yes",
                    "--max-concurrent-gc=2",
                    "--tag=chosen",
                ],
            )
            .stdout(fs::File::create(&stdout).unwrap())
            .stderr(fs::File::create(&stderr).unwrap())
            .spawn()
            .unwrap();
        drop(child.stdin.take());
        let deadline = Instant::now() + Duration::from_secs(8);
        loop {
            let out = fs::read(&stdout).unwrap();
            let err = fs::read(&stderr).unwrap();
            if [b"alpha> deleting".as_slice(), b"beta> deleting"]
                .iter()
                .all(|s| {
                    out.windows(s.len()).any(|w| w == *s) && err.windows(s.len()).any(|w| w == *s)
                })
            {
                break;
            }
            if Instant::now() >= deadline {
                child.kill().unwrap();
                panic!(
                    "live output missing: {} / {}",
                    String::from_utf8_lossy(&out),
                    String::from_utf8_lossy(&err)
                );
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        let lock = fs::File::open(f.root.join(".confctl/mutation.lock")).unwrap();
        assert!(
            rustix::fs::flock(&lock, rustix::fs::FlockOperation::NonBlockingLockExclusive).is_err()
        );
        assert!(child.try_wait().unwrap().is_none());
        assert!(f.root.join("active-a").exists() && f.root.join("active-b").exists());
        if cancel {
            nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(child.id() as i32),
                nix::sys::signal::Signal::SIGINT,
            )
            .unwrap();
        } else {
            fs::remove_file(f.root.join("gate")).unwrap();
        }
        let status = child.wait().unwrap();
        rustix::fs::flock(&lock, rustix::fs::FlockOperation::NonBlockingLockExclusive).unwrap();
        assert_eq!(status.success(), !cancel);
        assert!(!f.root.join("active-a").exists() && !f.root.join("active-b").exists());
        assert!(f.root.join(".confctl/mutation.lock").exists());
        if !cancel {
            let bytes = fs::read(stdout).unwrap();
            assert!(bytes.contains(&0xff));
            let text = String::from_utf8_lossy(&bytes);
            assert_eq!(text.matches("alpha> deleting").count(), 1);
            assert_eq!(text.matches("beta> deleting").count(), 1);
        }
    }
}

#[test]
fn native_gc_signal_is_aggregated_and_output_failure_stops_new_admission() {
    let f = Fixture::new();
    fs::write(f.root.join("a.signal"), b"").unwrap();
    let out = f.run(
        binary(),
        &[
            "collect-garbage",
            "--yes",
            "--max-concurrent-gc=1",
            "--tag=chosen",
        ],
        &[],
    );
    assert!(!out.status.success());
    assert!(String::from_utf8_lossy(&out.stderr).contains("signal Some(15)"));
    assert_eq!(f.calls().matches("nix-collect-garbage").count(), 2);
    let f = Fixture::new();
    let path = f.root.join("machines.json");
    let mut machines: serde_json::Value =
        serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    machines["z"] = serde_json::json!({"name":"zed","metaConfig":{"host":{"target":"z.invalid"},"tags":["chosen"]}});
    fs::write(path, serde_json::to_vec(&machines).unwrap()).unwrap();
    fs::write(f.root.join("gate"), b"").unwrap();
    let out = f
        .command(
            binary(),
            &[
                "collect-garbage",
                "--yes",
                "--max-concurrent-gc=2",
                "--tag=chosen",
            ],
        )
        .stdout(fs::File::create("/dev/full").unwrap())
        .output()
        .unwrap();
    assert!(!out.status.success());
    assert!(!f.calls().contains("z.invalid"));
    assert!(!f.root.join("active-a").exists() && !f.root.join("active-b").exists());
}
