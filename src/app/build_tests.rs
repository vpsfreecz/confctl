//! Actual build command path with temporary configuration and fake Nix outputs.
use super::*;
use crate::{
    configuration::ConfigRoot,
    generation::{BuildGeneration, GcRoots},
};
use serde_json::{Value, json};
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
    time::Instant,
};
const ROLE: &str = "CONFCTL_BUILD_APP_ROLE";
const ROOT: &str = "CONFCTL_BUILD_APP_ROOT";
const FIXTURE: &str = "app::build_tests::actual_build_application_fixture";
#[test]
fn actual_build_application_fixture() {
    let Some(role) = std::env::var_os(ROLE) else {
        return;
    };
    let root = PathBuf::from(std::env::var_os(ROOT).unwrap());
    let config = ConfigRoot::new(&root).unwrap();
    crate::operations::set_build_test_roots(
        GcRoots::new(&config, root.join("roots"), "test").unwrap(),
    );
    let mut argv = vec![
        "--color=never",
        "build",
        "--show-trace",
        "--max-jobs",
        "2",
        "--cores",
        "3",
    ];
    if role != "decline" {
        argv.push("--yes");
    }
    if role == "empty" {
        argv.push("absent*");
    }
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let code = runtime.block_on(execute(
        argv.into_iter().map(Into::into).collect(),
        Timing::start(),
        &crate::operations::CoreOperations,
        Ok(root),
        Arc::new(Mutex::new(Box::new(io::stdout()))),
        Arc::new(Mutex::new(Box::new(io::stderr()))),
    ));
    std::process::exit(code);
}
struct Fixture {
    _scratch: tempfile::TempDir,
    root: PathBuf,
    fake: PathBuf,
    lock: Vec<u8>,
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let fake = scratch.path().join("fake");
        fs::create_dir_all(root.join("replies")).unwrap();
        fs::create_dir(&fake).unwrap();
        fs::write(root.join("flake.nix"), "{ outputs = _: {}; }\n").unwrap();
        let lock = b"{\"version\":7,\"nodes\":{\"root\":{\"inputs\":{}}}}\n".to_vec();
        fs::write(root.join("flake.lock"), &lock).unwrap();
        for name in ["reused", "new", "rollback", "input"] {
            fs::create_dir_all(root.join("store").join(name)).unwrap();
        }
        let input = root.join("store/input");
        let machines = json!({"a":{"name":"alpha","key":"alpha-key","metaConfig":{"managed":true,"spin":"vpsadminos","host":{"target":"alpha.invalid"}}},"b":{"name":"carrier#beta","flakeKey":"beta/key","carrier":"carrier","metaConfig":{"managed":true,"spin":"nixos","host":{"target":null}}},"skip":{"name":"unmanaged","metaConfig":{"managed":false,"spin":"vpsadminos"}}});
        let plan = json!({"alpha":{"machineKey":"alpha-key","inputs":{"nixpkgs":input}},"carrier#beta":{"key":"beta/key","inputs":{"nixpkgs":input}}});
        let outputs = json!({"alpha-key":{"toplevel":root.join("store/reused"),"autoRollback":root.join("store/rollback")},"beta/key":{"toplevel":root.join("store/new"),"autoRollback":root.join("store/rollback")}});
        let defaults = json!({"list":{"columns":["name","spin"]},"nix":{"maxJobs":4,"impureEval":false,"legacyNixPath":false}});
        let build_settings = json!({"nix":{"maxJobs":99,"impureEval":true,"legacyNixPath":false}});
        for (name, value) in [
            ("machines", machines),
            ("plan", plan),
            ("outputs", outputs),
            ("defaults", defaults),
            ("build-settings", build_settings),
            (
                "infos",
                json!({"beta/key":{"nixpkgs":{"rev":"a".repeat(40),"unknown":false}}}),
            ),
        ] {
            fs::write(
                root.join(format!("replies/{name}.json")),
                serde_json::to_vec(&value).unwrap(),
            )
            .unwrap();
        }
        let host = root.join(".confctl/generations/alpha");
        for name in ["2000-01-01--00-00-00", "2001-01-01--00-00-00"] {
            fs::create_dir_all(host.join(name)).unwrap();
            let generation = BuildGeneration {
                name: name.into(),
                date: chrono::DateTime::parse_from_rfc3339(&format!("{}T00:00:00Z", &name[..10]))
                    .unwrap(),
                toplevel: root.join("store/reused"),
                auto_rollback: Some(root.join("store/rollback")),
                inputs: serde_json::from_value(json!({"nixpkgs":input})).unwrap(),
                inputs_info: Default::default(),
            };
            fs::write(
                host.join(name).join("generation.json"),
                serde_json::to_vec_pretty(&generation.encode()).unwrap(),
            )
            .unwrap();
        }
        std::os::unix::fs::symlink("2001-01-01--00-00-00", host.join("current")).unwrap();
        let script = r#"#!/bin/sh
printf 'CALL\000' >> "$CONFCTL_BUILD_APP_ROOT/argv"
printf '%s\000' "$@" >> "$CONFCTL_BUILD_APP_ROOT/argv"
last=
for arg do last="$arg"; done
case "$last" in
 '.#confctl.settings')
  count=0
  if [ -f "$CONFCTL_BUILD_APP_ROOT/settings-count" ]; then read -r count < "$CONFCTL_BUILD_APP_ROOT/settings-count"; fi
  count=$((count+1)); printf '%s\n' "$count" > "$CONFCTL_BUILD_APP_ROOT/settings-count"
  if [ "$count" -lt 3 ]; then cat "$CONFCTL_BUILD_APP_ROOT/replies/defaults.json"; else cat "$CONFCTL_BUILD_APP_ROOT/replies/build-settings.json"; fi
  ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/replies/machines.json"}}]\n' "$CONFCTL_BUILD_APP_ROOT" ;;
 '.#confctl.buildPlan') cat "$CONFCTL_BUILD_APP_ROOT/replies/plan.json" ;;
 '.#confctl.build') cat "$CONFCTL_BUILD_APP_ROOT/replies/outputs.json" ;;
 '.#confctl.inputsInfo') cat "$CONFCTL_BUILD_APP_ROOT/replies/infos.json" ;;
 *)
  if [ "$1" != build ]; then printf 'unexpected Nix eval: %s\n' "$last" >&2; exit 97; fi
  printf "these 2 derivations will be built:\nthis path will be fetched (1 MiB)\nbuilding '/nix/store/fixture.drv'...\nunknown build log\n" >&2
  if [ "$CONFCTL_BUILD_APP_ROLE" = failure ]; then printf 'fixture build failed\n' >&2; exit 7; fi
  printf '[{"outputs":{"out":"not-the-selected-output"}}]\n'
 ;;
esac
"#;
        fs::write(fake.join("nix"), script).unwrap();
        fs::set_permissions(fake.join("nix"), fs::Permissions::from_mode(0o755)).unwrap();
        Self {
            _scratch: scratch,
            root,
            fake,
            lock,
        }
    }
    fn run(&self, role: &str) -> Output {
        let inherited = std::env::var_os("PATH").unwrap_or_default();
        let path = std::env::join_paths(
            std::iter::once(self.fake.clone()).chain(std::env::split_paths(&inherited)),
        )
        .unwrap();
        let mut app = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", FIXTURE, "--nocapture"])
            .env(ROLE, role)
            .env(ROOT, &self.root)
            .env("PATH", path)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        if role == "decline" {
            app.stdin.take().unwrap().write_all(b"n\n").unwrap();
        } else {
            drop(app.stdin.take());
        }
        let deadline = Instant::now() + Duration::from_secs(15);
        loop {
            if app.try_wait().unwrap().is_some() {
                return app.wait_with_output().unwrap();
            }
            if Instant::now() >= deadline {
                let _ = app.kill();
                let output = app.wait_with_output().unwrap();
                panic!("build command fixture watchdog: {output:?}");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }
    fn calls(&self) -> Vec<Vec<String>> {
        let bytes = fs::read(self.root.join("argv")).unwrap();
        let mut calls: Vec<Vec<String>> = Vec::new();
        for word in bytes.split(|b| *b == 0).filter(|b| !b.is_empty()) {
            if word == b"CALL" {
                calls.push(Vec::new());
            } else {
                calls
                    .last_mut()
                    .unwrap()
                    .push(String::from_utf8(word.to_vec()).unwrap());
            }
        }
        calls
    }
    fn unchanged(&self) {
        assert_eq!(fs::read(self.root.join("flake.lock")).unwrap(), self.lock);
    }
}
#[test]
fn build_application_reuses_then_publishes_actual_selected_outputs_with_exact_options() {
    let fixture = Fixture::new();
    let output = fixture.run("yes");
    assert_eq!(output.status.code(), Some(0), "{output:?}");
    fixture.unchanged();
    let calls = fixture.calls();
    assert_eq!(calls.len(), 8, "{calls:?}");
    assert_eq!(
        calls[0],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--option",
            "max-jobs",
            "auto",
            ".#confctl.settings"
        ]
    );
    assert_eq!(
        calls[1],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--option",
            "max-jobs",
            "4",
            ".#confctl.settings"
        ]
    );
    assert_eq!(
        calls[2],
        [
            "build",
            "--json",
            "--no-link",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--option",
            "max-jobs",
            "4",
            ".#confctl.machinesJson"
        ]
    );
    assert_eq!(
        calls[3],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--show-trace",
            "--option",
            "max-jobs",
            "2",
            "--option",
            "cores",
            "3",
            ".#confctl.settings"
        ]
    );
    assert_eq!(calls[4].last().unwrap(), ".#confctl.buildPlan");
    assert_eq!(
        calls[5],
        [
            "build",
            "--json",
            "--no-link",
            "--impure",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--show-trace",
            "--option",
            "max-jobs",
            "2",
            "--option",
            "cores",
            "3",
            ".#confctl.build.alpha-key.toplevel",
            ".#confctl.build.alpha-key.autoRollback",
            ".#confctl.build.\"beta/key\".toplevel",
            ".#confctl.build.\"beta/key\".autoRollback"
        ]
    );
    assert!(
        calls[6]
            .iter()
            .any(|s| s.contains("[ \"alpha-key\" \"beta/key\" ]"))
    );
    assert!(calls[7].iter().any(|s| s.contains("[ \"beta/key\" ]")));
    let host = fixture.root.join(".confctl/generations");
    assert_eq!(
        fs::read_link(host.join("alpha/current")).unwrap(),
        Path::new("2000-01-01--00-00-00")
    );
    let name = fs::read_link(host.join("carrier#beta/current")).unwrap();
    let dir = host.join("carrier#beta").join(&name);
    let record: Value =
        serde_json::from_slice(&fs::read(dir.join("generation.json")).unwrap()).unwrap();
    assert_eq!(
        record["toplevel"],
        fixture.root.join("store/new").to_str().unwrap()
    );
    assert_eq!(
        fs::read_link(dir.join("auto_rollback")).unwrap(),
        fixture.root.join("store/rollback")
    );
    assert_eq!(
        fs::read_link(dir.join("nixpkgs.input")).unwrap(),
        fixture.root.join("store/input")
    );
    let roots = GcRoots::new(
        &ConfigRoot::new(&fixture.root).unwrap(),
        fixture.root.join("roots"),
        "test",
    )
    .unwrap();
    assert_eq!(fs::read_dir(roots.directory()).unwrap().count(), 3);
    assert!(!host.join("unmanaged").exists());
    assert!(fixture.root.join(".confctl/mutation.lock").is_file());
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(
        stdout.contains("alpha: 2000-01-01--00-00-00 (reused)")
            && stdout.contains("carrier#beta:")
            && stdout.contains("Built generations:"),
        "{stdout}"
    );
    assert!(String::from_utf8_lossy(&output.stderr).contains("unknown build log"));
    assert_eq!(
        fs::read_dir(fixture.root.join(".confctl/logs"))
            .unwrap()
            .count(),
        0
    );
}
#[test]
fn build_application_decline_and_empty_selection_never_admit_build_or_mutate_generations() {
    for role in ["decline", "empty"] {
        let fixture = Fixture::new();
        let output = fixture.run(role);
        assert_eq!(output.status.code(), Some(1), "{output:?}");
        fixture.unchanged();
        assert_eq!(fixture.calls().len(), 3);
        assert!(
            String::from_utf8_lossy(&output.stderr).contains(if role == "decline" {
                "Aborted"
            } else {
                "No machines to build"
            })
        );
        assert_eq!(
            fs::read_link(fixture.root.join(".confctl/generations/alpha/current")).unwrap(),
            Path::new("2001-01-01--00-00-00")
        );
        assert!(
            !fixture.root.join("roots").exists()
                && !fixture.root.join(".confctl/mutation.lock").exists()
        );
    }
}
#[test]
fn build_application_child_failure_retains_log_and_publishes_no_generations() {
    let fixture = Fixture::new();
    let output = fixture.run("failure");
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    fixture.unchanged();
    assert_eq!(fixture.calls().len(), 6);
    assert!(
        !fixture
            .root
            .join(".confctl/generations/carrier#beta")
            .exists()
    );
    assert!(!fixture.root.join("roots").exists());
    assert_eq!(
        fs::read_link(fixture.root.join(".confctl/generations/alpha/current")).unwrap(),
        Path::new("2001-01-01--00-00-00")
    );
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stderr.contains("fixture build failed"), "{stderr}");
    assert!(stderr.contains("exit status: 7"), "{stderr}");
    let locator = stderr
        .lines()
        .find_map(|line| line.strip_prefix("Log file: "))
        .unwrap();
    let log = fs::read(locator).unwrap();
    assert!(
        log.windows(b"unknown build log".len())
            .any(|w| w == b"unknown build log")
    );
}
