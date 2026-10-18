//! Real application/SignalOwner/Presentation/Supervisor channel-route fixtures.

use super::*;
use crate::configuration::{ConfigRoot, MutationLock};
use std::{
    fs,
    os::unix::{fs::MetadataExt, fs::PermissionsExt},
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
    time::Instant,
};

const ROLE: &str = "CONFCTL_CHANNEL_APP_ROLE";
const ROOT: &str = "CONFCTL_CHANNEL_APP_ROOT";
const FIXTURE: &str = "app::channel_tests::actual_channel_application_fixture";

struct FailingOutput;
impl Write for FailingOutput {
    fn write(&mut self, _: &[u8]) -> io::Result<usize> {
        Err(io::Error::other("injected channel output failure"))
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}
struct FinalizationFault;
impl Operations for FinalizationFault {
    fn available(&self, action: &Action) -> bool {
        crate::operations::CoreOperations.available(action)
    }
    async fn run(
        &self,
        invocation: &Invocation,
        context: &Context,
    ) -> Result<ExitDisposition, ConfctlError> {
        let result = crate::operations::CoreOperations
            .run(invocation, context)
            .await?;
        let path = fs::read_dir(context.cwd.join(".confctl/logs"))
            .unwrap()
            .next()
            .unwrap()
            .unwrap()
            .path();
        invocation_log::fail_finalization_once(path);
        Ok(result)
    }
}

#[test]
fn actual_channel_application_fixture() {
    let Some(role) = std::env::var_os(ROLE) else {
        return;
    };
    let cwd = PathBuf::from(std::env::var_os(ROOT).unwrap());
    let stdout: SharedWriter = if role == "output" {
        Arc::new(Mutex::new(Box::new(FailingOutput)))
    } else {
        Arc::new(Mutex::new(Box::new(io::stdout())))
    };
    let stderr: SharedWriter = Arc::new(Mutex::new(Box::new(io::stderr())));
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let argv = ["inputs", "channel", "ls"].map(Into::into).to_vec();
    let code = if role == "log" {
        runtime.block_on(execute(
            argv,
            Timing::start(),
            &FinalizationFault,
            Ok(cwd),
            stdout,
            stderr,
        ))
    } else {
        runtime.block_on(execute(
            argv,
            Timing::start(),
            &crate::operations::CoreOperations,
            Ok(cwd),
            stdout,
            stderr,
        ))
    };
    std::process::exit(code);
}

struct Fixture {
    _scratch: tempfile::TempDir,
    root: PathBuf,
    fake: PathBuf,
    sources: Vec<(PathBuf, Vec<u8>)>,
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let fake = scratch.path().join("fake-bin");
        fs::create_dir_all(&root).unwrap();
        fs::create_dir_all(&fake).unwrap();
        let root = root.canonicalize().unwrap();
        fs::write(root.join("flake.nix"), "{ outputs = _: {}; }\n").unwrap();
        fs::write(root.join("flake.lock"), serde_json::to_vec(&serde_json::json!({
            "version": 7,
            "nodes": {
                "root": {"inputs": {"nixpkgs": "nixpkgs"}},
                "nixpkgs": {"locked": {"type": "github", "owner": "fixture", "repo": "nixpkgs", "rev": "a".repeat(40)}}
            }
        })).unwrap()).unwrap();
        let script = r#"#!/bin/sh
printf '%s\n' "$$" >> "$CONFCTL_CHANNEL_APP_ROOT/child-pids"
printf '%s\n' "$*" >> "$CONFCTL_CHANNEL_APP_ROOT/child-calls"
last=
for arg do last="$arg"; done
case "$last" in
  '.#confctl.settings')
    printf '%s\n' '{"list":{"columns":["name"]},"nix":{"maxJobs":"auto","impureEval":false,"legacyNixPath":false},"buildGenerations":{"min":1,"max":10,"maxAge":9999999999},"hostGenerations":{"min":1,"max":10,"maxAge":9999999999}}'
    ;;
  '.#confctl.channels')
    if [ "$CONFCTL_CHANNEL_APP_ROLE" = cancel ]; then
      printf '%s\n' "$$" > "$CONFCTL_CHANNEL_APP_ROOT/blocked-pid"
      exec sleep 120
    fi
    printf '%s\n' '{"base":{"nixpkgs":"nixpkgs"}}'
    ;;
  *) printf '%s\n' "unexpected child target: $last" >&2; exit 97 ;;
esac
"#;
        fs::write(fake.join("nix"), script).unwrap();
        fs::set_permissions(fake.join("nix"), fs::Permissions::from_mode(0o755)).unwrap();
        let sources = ["flake.nix", "flake.lock"]
            .map(|name| {
                let path = root.join(name);
                let bytes = fs::read(&path).unwrap();
                (path, bytes)
            })
            .to_vec();
        Self {
            _scratch: scratch,
            root,
            fake,
            sources,
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
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        // Fixture watchdog, never a timeout imposed on a product operation.
        let deadline = Instant::now() + Duration::from_secs(15);
        let mut blocked_child = None;
        if role == "cancel" {
            let ready = self.root.join("blocked-pid");
            while !ready.exists() && Instant::now() < deadline {
                if let Some(status) = app.try_wait().unwrap() {
                    panic!("application exited before channel child readiness: {status}");
                }
                std::thread::sleep(Duration::from_millis(10));
            }
            if !ready.exists() {
                let _ = app.kill();
                let output = app.wait_with_output().unwrap();
                panic!("fixture readiness watchdog: {output:?}");
            }
            // Own the blocked fixture process identity for watchdog cleanup;
            // never signal a saved numerical pid after it could be reused.
            let pid = fs::read_to_string(&ready).unwrap().trim().parse().unwrap();
            blocked_child = Some(
                rustix::process::pidfd_open(
                    rustix::process::Pid::from_raw(pid).unwrap(),
                    rustix::process::PidfdFlags::empty(),
                )
                .unwrap(),
            );
            nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(app.id().try_into().unwrap()),
                nix::sys::signal::Signal::SIGINT,
            )
            .unwrap();
        }
        loop {
            if app.try_wait().unwrap().is_some() {
                return app.wait_with_output().unwrap();
            }
            if Instant::now() >= deadline {
                if let Some(child) = &blocked_child {
                    let _ =
                        rustix::process::pidfd_send_signal(child, rustix::process::Signal::KILL);
                }
                let _ = app.kill();
                let output = app.wait_with_output().unwrap();
                panic!("application fixture watchdog: {output:?}");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }
    fn assert_all_three_children_ended(&self) {
        for (path, bytes) in &self.sources {
            assert_eq!(&fs::read(path).unwrap(), bytes);
        }
        assert!(!self.root.join(".confctl/generations").exists());
        let calls = fs::read_to_string(self.root.join("child-calls")).unwrap();
        let calls: Vec<_> = calls.lines().collect();
        assert_eq!(calls.len(), 3);
        assert!(calls[0].ends_with(".#confctl.settings"));
        assert!(calls[1].ends_with(".#confctl.settings"));
        assert!(calls[2].ends_with(".#confctl.channels"));
        let pids = fs::read_to_string(self.root.join("child-pids")).unwrap();
        for pid in pids.lines() {
            let pid = nix::unistd::Pid::from_raw(pid.parse().unwrap());
            assert_eq!(
                nix::sys::signal::kill(pid, None),
                Err(nix::errno::Errno::ESRCH)
            );
        }
    }
    fn assert_no_mutation_lock(&self) {
        assert!(!self.root.join(".confctl/mutation.lock").exists());
    }
}

#[test]
fn channel_application_does_not_wait_for_writer_or_create_a_fresh_mutation_lock() {
    for writer in [false, true] {
        let fixture = Fixture::new();
        let root = ConfigRoot::new(&fixture.root).unwrap();
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        let cancellation = CancellationToken::new();
        let guard = writer.then(|| {
            runtime
                .block_on(MutationLock::acquire(&root, &cancellation))
                .unwrap()
        });
        let path = fixture.root.join(".confctl/mutation.lock");
        let before = writer.then(|| {
            let metadata = fs::metadata(&path).unwrap();
            (metadata.dev(), metadata.ino(), fs::read(&path).unwrap())
        });
        let output = fixture.run("read");
        assert_eq!(output.status.code(), Some(0), "{output:?}");
        assert!(
            output
                .stdout
                .windows(b"CHANNEL".len())
                .any(|bytes| bytes == b"CHANNEL")
        );
        assert!(output.stderr.is_empty(), "{output:?}");
        fixture.assert_all_three_children_ended();
        assert_eq!(
            fs::read_dir(fixture.root.join(".confctl/logs"))
                .unwrap()
                .count(),
            0
        );
        if let Some(guard) = guard {
            guard.require_root(&root).unwrap();
            let metadata = fs::metadata(&path).unwrap();
            assert_eq!(
                before.unwrap(),
                (metadata.dev(), metadata.ino(), fs::read(path).unwrap())
            );
        } else {
            fixture.assert_no_mutation_lock();
        }
    }
}

#[test]
fn channel_application_cancellation_joins_blocked_child_and_all_owners() {
    let fixture = Fixture::new();
    let output = fixture.run("cancel");
    assert_eq!(output.status.code(), Some(130), "{output:?}");
    assert!(
        !output
            .stdout
            .windows(b"CHANNEL".len())
            .any(|bytes| bytes == b"CHANNEL")
    );
    assert!(String::from_utf8_lossy(&output.stderr).contains("operation canceled"));
    assert!(!String::from_utf8_lossy(&output.stderr).contains("Timing:"));
    fixture.assert_all_three_children_ended();
    fixture.assert_no_mutation_lock();
}

fn retained_log(root: &Path, output: &Output) -> Vec<u8> {
    let stderr = String::from_utf8_lossy(&output.stderr);
    let path = stderr
        .lines()
        .find_map(|line| line.strip_prefix("Log file: "))
        .expect("retained invocation log locator");
    assert!(Path::new(path).starts_with(root.join(".confctl/logs")));
    fs::read(path).unwrap()
}

#[test]
fn channel_application_output_failure_retains_primary_evidence_and_joins() {
    let fixture = Fixture::new();
    let output = fixture.run("output");
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(
        stderr.contains("injected channel output failure"),
        "{output:?}"
    );
    let retained: Vec<_> = stderr
        .lines()
        .filter_map(|line| line.strip_prefix("Process evidence: "))
        .collect();
    assert!(
        retained.iter().any(|path| {
            fs::read(path)
                .unwrap()
                .windows(b"CHANNEL".len())
                .any(|bytes| bytes == b"CHANNEL")
        }),
        "{output:?}"
    );
    assert!(!stderr.contains("Timing:"));
    assert!(!retained_log(&fixture.root, &output).is_empty());
    fixture.assert_all_three_children_ended();
    fixture.assert_no_mutation_lock();
}

#[test]
fn channel_application_finalization_failure_after_real_route_reports_retained_log() {
    let fixture = Fixture::new();
    let output = fixture.run("log");
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stderr.contains("error: finalize invocation log: injected finalization failure"));
    assert!(!stderr.contains("Timing:"));
    let log = retained_log(&fixture.root, &output);
    assert!(
        log.windows(b"{\"base\":{\"nixpkgs\":\"nixpkgs\"}}".len())
            .any(|bytes| bytes == b"{\"base\":{\"nixpkgs\":\"nixpkgs\"}}")
    );
    fixture.assert_all_three_children_ended();
    fixture.assert_no_mutation_lock();
}
