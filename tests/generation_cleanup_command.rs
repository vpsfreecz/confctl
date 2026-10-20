#[path = "support/cleanup_fixture.rs"]
pub mod fixture;
use fixture::{Fixture, success};
use serde_json::json;
use std::{fs, path::Path};
fn binary() -> &'static Path {
    Path::new(env!("CARGO_BIN_EXE_confctl-rust"))
}
fn remote(f: &Fixture) {
    f.profiles(json!({"/nix/var/nix/profiles/system":[{"id":1,"path":"/saved/old","time":1},{"id":2,"path":"/saved/current","time":2}],"/nix/var/nix/profiles/confctl-guest:one":[{"id":3,"path":"/guest","time":3}]}));
}
#[test]
fn remote_rm_preserves_global_selector_current_and_exact_carrier_commands() {
    for selector in ["current", "old", "0", "1", "-1", "*", "1d"] {
        let f = Fixture::new();
        remote(&f);
        let out = f.run(
            binary(),
            &[
                "generation",
                "rm",
                "--remote",
                "--no-gc",
                "--yes",
                "--tag=chosen",
                "*",
                selector,
            ],
            &[],
        );
        success(&out);
        let effects = fs::read_to_string(f.root.join("effects")).unwrap();
        let count = effects.lines().count();
        assert_eq!(
            count,
            match selector {
                "current" | "old" => 2,
                "*" | "1d" => 4,
                _ => 1,
            }
        );
        assert!(!f.calls().contains("nix-collect-garbage"));
        assert!(!f.calls().contains("realpath"));
    }
    let f = Fixture::new();
    remote(&f);
    success(&f.run(
        binary(),
        &[
            "generation",
            "rm",
            "--remote",
            "--no-gc",
            "--yes",
            "carrier#one",
        ],
        &[],
    ));
    let calls = f.calls();
    assert!(
        calls.contains("2222")
            && calls.contains("carrier.invalid")
            && calls.contains(
                "carrier-env -p /nix/var/nix/profiles/confctl-guest:one --delete-generations 3"
            )
    );
}
#[test]
fn remote_failure_decline_empty_limits_and_changed_carrier_gc_are_distinct() {
    let f = Fixture::new();
    remote(&f);
    let out = f.run(
        binary(),
        &["generation", "rm", "--remote", "--no-gc", "alpha"],
        b"n\n",
    );
    assert!(!out.status.success());
    f.no_deletions();
    assert!(f.root.join(".confctl/mutation.lock").exists());
    let f = Fixture::new();
    success(&f.run(
        binary(),
        &["generation", "rm", "--remote", "--yes", "--tag=chosen"],
        &[],
    ));
    f.no_deletions();
    let f = Fixture::new();
    remote(&f);
    let out = f.run(
        binary(),
        &[
            "generation",
            "rm",
            "--remote",
            "--yes",
            "--max-concurrent-gc=0",
            "alpha",
        ],
        &[],
    );
    assert!(!out.status.success());
    f.no_deletions();
    assert!(!f.calls().contains("ssh"));
    let f = Fixture::new();
    remote(&f);
    fs::write(f.root.join("delete-fail"), b"").unwrap();
    let out = f.run(
        binary(),
        &["generation", "rm", "--remote", "--yes", "--tag=chosen"],
        &[],
    );
    assert!(!out.status.success());
    assert_eq!(
        fs::read_to_string(f.root.join("effects"))
            .unwrap()
            .lines()
            .count(),
        1
    );
    assert!(!f.calls().contains("nix-collect-garbage"));
    assert!(String::from_utf8_lossy(&out.stderr).contains("exit Some(9)"));
    let f = Fixture::new();
    f.profiles(json!({"/nix/var/nix/profiles/confctl-guest:one":[{"id":3,"path":"/one","time":3}],"/nix/var/nix/profiles/confctl-guest:two":[{"id":4,"path":"/two","time":4}]}));
    success(&f.run(
        binary(),
        &["generation", "rm", "--remote", "--yes", "--tag=guest"],
        &[],
    ));
    assert_eq!(f.calls().matches("nix-collect-garbage").count(), 1);
    assert_eq!(
        fs::read_to_string(f.root.join("effects"))
            .unwrap()
            .lines()
            .count(),
        2
    );
}
#[test]
fn local_rejected_and_broken_current_never_reach_roots_or_remote_deletion() {
    for selector in ["bad", "0", "current", "old"] {
        let f = Fixture::new();
        f.save("alpha", "good", 1, "/saved");
        let dir = f.root.join(".confctl/generations/alpha");
        fs::create_dir(dir.join("bad")).unwrap();
        fs::write(dir.join("bad/generation.json"), b"{broken").unwrap();
        std::os::unix::fs::symlink("bad", dir.join("current")).unwrap();
        let out = f.run(
            binary(),
            &["generation", "rm", "--yes", "alpha", selector],
            &[],
        );
        assert!(!out.status.success());
        f.no_deletions();
        assert_eq!(
            fs::read(dir.join("bad/generation.json")).unwrap(),
            b"{broken"
        );
        assert_eq!(
            fs::read_link(dir.join("current")).unwrap(),
            Path::new("bad")
        );
        assert!(!f.calls().contains("logname"));
    }
}
