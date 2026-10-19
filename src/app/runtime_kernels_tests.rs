//! Actual parser/CoreOperations/application route with local test executables.

use super::*;
use serde_json::json;
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
    time::Instant,
};

const ROLE: &str = "CONFCTL_KERNEL_APP_ROLE";
const ROOT: &str = "CONFCTL_KERNEL_APP_ROOT";
const FIXTURE: &str = "app::runtime_kernels_tests::actual_runtime_kernels_application_fixture";

#[test]
fn actual_runtime_kernels_application_fixture() {
    let Some(role) = std::env::var_os(ROLE) else {
        return;
    };
    let mut argv = vec![
        "--color=never",
        "runtime-kernels",
        "update",
        "--show-trace",
        "--tag=chosen",
    ];
    if role == "yes" {
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
        Ok(PathBuf::from(std::env::var_os(ROOT).unwrap())),
        Arc::new(Mutex::new(Box::new(io::stdout()))),
        Arc::new(Mutex::new(Box::new(io::stderr()))),
    ));
    std::process::exit(code);
}

struct Fixture {
    _scratch: tempfile::TempDir,
    root: PathBuf,
    fake: PathBuf,
    initial: Vec<u8>,
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let fake = scratch.path().join("fake-bin");
        fs::create_dir_all(root.join("configs/node")).unwrap();
        fs::create_dir(&fake).unwrap();
        fs::write(root.join("flake.nix"), "{ outputs = _: {}; }\n").unwrap();
        fs::write(root.join("machines.json"), serde_json::to_vec(&json!({
            "local": {"name":"nodes/local", "key":"local-key", "metaConfig":{
                "spin":"vpsadminos", "managed":true, "host":{"target":"localhost"}, "tags":["chosen"]}},
            "remote": {"name":"carrier#remote", "key":"remote-key", "carrier":"carrier", "metaConfig":{
                "spin":"vpsadminos", "managed":false, "host":{"target":"remote.invalid", "port":2222}, "tags":["chosen"]}},
            "excluded": {"name":"nodes/excluded", "metaConfig":{
                "spin":"vpsadminos", "managed":true, "host":{"target":"excluded.invalid"}, "tags":["other"]}},
            "nixos": {"name":"nodes/nixos", "metaConfig":{
                "spin":"nixos", "managed":true, "host":{"target":"nixos.invalid"}, "tags":["chosen"]}}
        })).unwrap()).unwrap();
        let initial = br#"{"nodes/local":"old","carrier#remote":"old","nodes/excluded":{"keep":[false,7]},"nodes/nixos":"unchanged"}"#.to_vec();
        fs::write(root.join("configs/node/kernels.json"), &initial).unwrap();
        executable(
            &fake.join("nix"),
            r#"#!/bin/sh
printf '%s\n' "$*" >> "$CONFCTL_KERNEL_APP_ROOT/nix-calls"
last=
for arg do last="$arg"; done
case "$last" in
  '.#confctl.settings')
    printf '%s\n' '{"list":{"columns":["name","spin","host.target"]},"nix":{"maxJobs":"auto","impureEval":false,"legacyNixPath":false},"runtimeKernels":{"enable":true,"stateFile":"configs/node/kernels.json"}}'
    ;;
  '.#confctl.machinesJson')
    printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$CONFCTL_KERNEL_APP_ROOT"
    ;;
  *) printf '%s\n' "unexpected Nix target: $last" >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &fake.join("uname"),
            r#"#!/bin/sh
printf '%s\n' "$@" > "$CONFCTL_KERNEL_APP_ROOT/uname-args"
printf ' 6.12.3-extra.more\n'
"#,
        );
        executable(
            &fake.join("ssh"),
            r#"#!/bin/sh
printf '%s\n' "$@" > "$CONFCTL_KERNEL_APP_ROOT/ssh-args"
printf 'fixture SSH refused\n' >&2
exit 255
"#,
        );
        Self {
            _scratch: scratch,
            root,
            fake,
            initial,
        }
    }

    fn run(&self, role: &str, answer: &[u8]) -> Output {
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
            .env("CONFCTL_SSH_CONFIG", self.root.join("ssh config"))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        app.stdin.take().unwrap().write_all(answer).unwrap();
        let deadline = Instant::now() + Duration::from_secs(15);
        loop {
            if app.try_wait().unwrap().is_some() {
                return app.wait_with_output().unwrap();
            }
            if Instant::now() >= deadline {
                let _ = app.kill();
                let output = app.wait_with_output().unwrap();
                panic!("runtime kernel application fixture watchdog: {output:?}");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    fn state(&self) -> Vec<u8> {
        fs::read(self.root.join("configs/node/kernels.json")).unwrap()
    }
    fn assert_inventory_calls(&self) {
        let calls = fs::read_to_string(self.root.join("nix-calls")).unwrap();
        let calls = calls.lines().collect::<Vec<_>>();
        assert_eq!(calls.len(), 3, "{calls:?}");
        assert!(calls[0].ends_with(".#confctl.settings"));
        assert!(!calls[0].contains("--show-trace")); // Invocation defaults are stateless.
        assert!(calls[1].ends_with(".#confctl.settings") && calls[1].contains("--show-trace"));
        assert!(calls[2].ends_with(".#confctl.machinesJson") && calls[2].contains("--show-trace"));
    }
}

fn executable(path: &Path, bytes: &str) {
    fs::write(path, bytes).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}

#[test]
fn runtime_kernels_application_queries_all_selected_vpsadminos_and_saves_host_results() {
    let fixture = Fixture::new();
    let output = fixture.run("yes", b"");
    assert_eq!(output.status.code(), Some(0), "{output:?}");
    assert!(output.stderr.is_empty(), "{output:?}");
    fixture.assert_inventory_calls();
    assert_eq!(fs::read(fixture.root.join("uname-args")).unwrap(), b"-r\n");
    assert_eq!(
        fs::read_to_string(fixture.root.join("ssh-args")).unwrap(),
        format!(
            "-F\n{}\n-l\nroot\n-p\n2222\nremote.invalid\nuname -r\n",
            fixture.root.join("ssh config").display()
        )
    );
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(
        stdout.contains("HOST") && stdout.contains("KERNEL"),
        "{stdout}"
    );
    assert!(stdout.contains("nodes/local") && stdout.contains("6.12.3-extra"));
    assert!(
        stdout.contains("Error on carrier#remote:")
            && stdout.contains("exit status: 255")
            && stdout.contains("fixture SSH refused")
    );
    assert!(
        !stdout.contains("Continue?")
            && !stdout.contains("nodes/excluded")
            && !stdout.contains("nodes/nixos")
    );
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&fixture.state()).unwrap(),
        json!({
            "nodes/local":"6.12.3-extra", "nodes/excluded":{"keep":[false,7]}, "nodes/nixos":"unchanged"
        })
    );
}

#[test]
fn runtime_kernels_application_decline_shows_selected_preview_without_probes_or_writes() {
    let fixture = Fixture::new();
    let output = fixture.run("decline", b"n\n");
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    assert!(String::from_utf8_lossy(&output.stderr).contains("Aborted"));
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(
        stdout.contains("The following machines will be queried:")
            && stdout.contains("Continue? [y/N]:"),
        "{stdout}"
    );
    assert!(stdout.contains("HOST.TARGET") && stdout.contains("SPIN"));
    assert!(stdout.contains("nodes/local") && stdout.contains("carrier#remote"));
    assert!(
        !stdout.contains("nodes/excluded")
            && !stdout.contains("nodes/nixos")
            && !stdout.contains("KERNEL")
    );
    fixture.assert_inventory_calls();
    assert!(!fixture.root.join("uname-args").exists() && !fixture.root.join("ssh-args").exists());
    assert!(!fixture.root.join(".confctl/mutation.lock").exists());
    assert_eq!(fixture.state(), fixture.initial);
}

#[test]
fn runtime_kernels_application_empty_selection_confirms_and_reserializes_without_probes() {
    let fixture = Fixture::new();
    let output = fixture.run("empty", b"y\n");
    assert_eq!(output.status.code(), Some(0), "{output:?}");
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(
        stdout.contains("Continue? [y/N]:") && stdout.contains("KERNEL"),
        "{stdout}"
    );
    assert!(!fixture.root.join("uname-args").exists() && !fixture.root.join("ssh-args").exists());
    assert_ne!(fixture.state(), fixture.initial);
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&fixture.state()).unwrap(),
        serde_json::from_slice::<serde_json::Value>(&fixture.initial).unwrap()
    );
}
