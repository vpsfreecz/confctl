//! Real controlling-terminal tests for the application signal service. A
//! guardian outside the session reaps the app if its direct witness exits;
//! a separate session keeper prevents that exit from tearing down the PTY.
//! Direct signal-service cases and full app/Supervisor cases are separate
//! gates. Both use the actual stop policy, without bypasses or fake grants.

use super::*;
use crate::terminal::pty_tests::{assert_modes, enter_controlling_session, open_pty};
use std::{
    io::{self, BufRead, Write},
    os::{fd::AsFd, unix::process::CommandExt},
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    time::Instant,
};

const ROLE: &str = "CONFCTL_TEST_APP_STOP_ROLE";
const CASE: &str = "CONFCTL_TEST_APP_STOP_CASE";
const CONTROL: &str = "CONFCTL_TEST_APP_STOP_CONTROL";

macro_rules! native_case {
    ($name:ident,$case:literal) => {
        #[test]
        fn $name() {
            run_case(
                concat!("app::signals::native_tests::", stringify!($name)),
                $case,
            );
        }
    };
}
native_case!(
    native_stop_fg_requires_new_continuation_and_actual_foreground,
    "fg"
);
native_case!(
    native_background_continue_remains_parked_until_foreground,
    "bg"
);
native_case!(native_ignored_tstp_is_refused_before_stop, "ignored");
native_case!(native_blocked_tstp_is_refused_before_stop, "blocked");
native_case!(native_caught_tstp_is_refused_before_stop, "caught");
native_case!(
    native_parent_exit_after_validation_before_raise_never_grants,
    "parent-exit"
);
native_case!(
    native_background_request_cancellation_releases_service,
    "cancel"
);
native_case!(
    native_full_app_foreground_stop_resumes_child_and_typeahead,
    "route-fg"
);
native_case!(
    native_full_app_background_then_foreground_preserves_child_modes,
    "route-bg"
);
native_case!(
    native_full_app_background_interrupt_joins_before_canceled_footer,
    "route-cancel"
);
native_case!(
    native_full_app_child_failure_restores_terminal_before_failed_footer,
    "route-failure"
);

fn run_case(test: &str, case: &str) {
    match std::env::var(ROLE).ok().as_deref() {
        None => {
            let directory = tempfile::tempdir().expect("owned native app control");
            let output = command(test, "guardian", case, directory.path())
                .output()
                .expect("isolated app guardian");
            if !output.status.success() {
                let retained = directory.keep();
                panic!(
                    "native app fixture failed; evidence {}\nstdout={}\nstderr={}",
                    retained.display(),
                    String::from_utf8_lossy(&output.stdout),
                    String::from_utf8_lossy(&output.stderr)
                );
            }
        }
        Some("guardian") => guardian(test, case),
        Some("session") => session_keeper(test, case),
        Some("witness") => witness_shell(test, case),
        Some("app") if case.starts_with("route-") => route_app(test, case),
        Some("app") => app(case),
        Some("operation-child") => route_child(case),
        Some(role) => panic!("unknown native app role {role}"),
    }
}
fn control() -> PathBuf {
    std::env::var_os(CONTROL)
        .expect("owned control directory")
        .into()
}
fn command(test: &str, role: &str, case: &str, directory: &Path) -> Command {
    let executable = std::env::current_exe().expect("native test executable");
    let mut command = if role == "app" && matches!(case, "ignored" | "blocked") {
        // Set an inherited unsupported disposition in this isolated fixture,
        // then exec the real test binary. No production disposition is changed.
        let setup = if case == "ignored" {
            "import os,signal,sys; signal.signal(signal.SIGTSTP,signal.SIG_IGN); os.execv(sys.argv[1],sys.argv[1:])"
        } else {
            "import os,signal,sys; signal.pthread_sigmask(signal.SIG_BLOCK,{signal.SIGTSTP}); os.execv(sys.argv[1],sys.argv[1:])"
        };
        let mut command = Command::new("python3");
        command.args(["-c", setup]);
        command.arg(executable);
        command
    } else {
        Command::new(executable)
    };
    command
        .args(["--exact", test, "--nocapture", "--test-threads=1"])
        .env(ROLE, role)
        .env(CASE, case)
        .env(CONTROL, directory)
        .env("TERM", "xterm-256color")
        .env("CONFCTL_TTY", "1");
    command
}
fn attach(group: Pid) {
    let mut block = SigSet::empty();
    block.add(Signal::SIGTTOU);
    let mut original = SigSet::empty();
    pthread_sigmask(SigmaskHow::SIG_BLOCK, Some(&block), Some(&mut original))
        .expect("test attach mask");
    let result = rustix::termios::tcsetpgrp(io::stdin(), group);
    pthread_sigmask(SigmaskHow::SIG_SETMASK, Some(&original), None)
        .expect("exact test mask restore");
    result.expect("test foreground group");
}
fn pid(child: &Child) -> Pid {
    Pid::from_raw(child.id() as i32).expect("owned test PID")
}
fn write_pid(name: &str, child: &Child) {
    let temporary = control().join(format!("{name}.new"));
    std::fs::write(&temporary, child.id().to_string()).unwrap();
    std::fs::rename(temporary, control().join(name)).unwrap();
}
fn wait_file(name: &str) {
    let start = Instant::now();
    while !control().join(name).exists() {
        assert!(
            start.elapsed() < Duration::from_secs(8),
            "test barrier {name}"
        );
        std::thread::sleep(Duration::from_millis(2));
    }
}
fn drain(master: &OwnedFd, bytes: &mut Vec<u8>) {
    loop {
        let mut chunk = [0; 4096];
        match rustix::io::read(master, &mut chunk) {
            Ok(0) | Err(rustix::io::Errno::AGAIN) | Err(rustix::io::Errno::IO) => break,
            Ok(count) => {
                bytes.extend_from_slice(&chunk[..count]);
                assert!(bytes.len() < 4 * 1024 * 1024);
            }
            Err(rustix::io::Errno::INTR) => {}
            Err(error) => panic!("native app transcript: {error}"),
        }
    }
}
fn guardian(test: &str, case: &str) {
    // This isolated guardian remains outside the PTY session. Adoption therefore
    // cannot itself keep the app's session/process group non-orphaned.
    rustix::process::set_child_subreaper(Pid::from_raw(1)).expect("isolated test subreaper");
    let (master, slave) = open_pty();
    let mut keeper = command(test, "session", case, &control())
        .stdin(Stdio::from(slave.try_clone().unwrap()))
        .stdout(Stdio::from(slave.try_clone().unwrap()))
        .stderr(Stdio::from(slave))
        .spawn()
        .expect("session keeper");
    let flags = rustix::fs::fcntl_getfl(&master).unwrap();
    rustix::fs::fcntl_setfl(&master, flags | rustix::fs::OFlags::NONBLOCK).unwrap();
    let mut owned = Vec::<(Pid, OwnedFd)>::new();
    let mut bytes = Vec::new();
    let start = Instant::now();
    let mut adopted_reaped = false;
    let mut typeahead_sent = false;
    let status = loop {
        drain(&master, &mut bytes);
        for name in ["witness-pid", "app-pid", "route-child-pid"] {
            if let Ok(raw) = std::fs::read_to_string(control().join(name)) {
                let child = Pid::from_raw(raw.parse().unwrap()).unwrap();
                // The keeper's unreaped PID pins this test session ID. Do
                // not acquire a pidfd for an unrelated recycled PID.
                if !owned.iter().any(|(id, _)| *id == child)
                    && let Ok(before) = Identity::read(child.as_raw_nonzero().get() as u32)
                    && before.session == keeper.id()
                    && before.group == before.pid
                    && let Ok(fd) = pidfd_open(child, PidfdFlags::empty())
                    && let Ok(after) = Identity::read(before.pid)
                    && same_identity(&before, &after)
                {
                    owned.push((child, fd));
                }
            }
        }
        if case.starts_with("route-") && control().join("route-parked").exists() && !typeahead_sent
        {
            let queued = b"queued-after-stop\n";
            assert_eq!(rustix::io::write(&master, queued).unwrap(), queued.len());
            typeahead_sent = true;
            std::fs::write(control().join("typeahead-sent"), b"").unwrap();
        }
        if case == "parent-exit" && control().join("witness-reaped").exists() && !adopted_reaped {
            let child = Pid::from_raw(
                std::fs::read_to_string(control().join("app-pid"))
                    .unwrap()
                    .parse()
                    .unwrap(),
            )
            .unwrap();
            if let Some(status) = rustix::process::waitid(
                rustix::process::WaitId::Pid(child),
                rustix::process::WaitIdOptions::EXITED | rustix::process::WaitIdOptions::NOHANG,
            )
            .expect("adopted app belongs to guardian")
            {
                assert_eq!(status.exit_status(), Some(0));
                adopted_reaped = true;
                std::fs::write(control().join("adopted-reaped"), b"").unwrap();
            }
        }
        if let Some(status) = keeper.try_wait().expect("session status") {
            drain(&master, &mut bytes);
            break status;
        }
        if start.elapsed() > Duration::from_secs(12) {
            // Test watchdog only. pidfds cannot signal an unrelated reused PID.
            kill_owned(&owned);
            let _ = keeper.kill();
            let _ = keeper.wait();
            reap_adopted();
            panic!(
                "native app test watchdog; {}",
                String::from_utf8_lossy(&bytes)
            );
        }
        std::thread::sleep(Duration::from_millis(2));
    };
    if !status.success() {
        kill_owned(&owned);
        reap_adopted();
    }
    assert!(status.success(), "{}", String::from_utf8_lossy(&bytes));
    assert!(control().join("terminal-restored").exists());
    assert_eq!(adopted_reaped, case == "parent-exit");
    let result: serde_json::Value =
        serde_json::from_slice(&std::fs::read(control().join("result.json")).expect("app result"))
            .unwrap();
    if case.starts_with("route-") {
        assert!(typeahead_sent);
        let expected = match case {
            "route-cancel" => 130,
            "route-failure" => 1,
            _ => 0,
        };
        assert_eq!(result["code"], expected);
        assert!(control().join("footer-checked").exists());
        assert!(control().join("operation-joined").exists());
        assert_eq!(
            control().join("child-resumed").exists(),
            case != "route-cancel"
        );
        if case != "route-cancel" {
            assert_eq!(
                std::fs::read(control().join("child-typeahead")).unwrap(),
                b"queued-after-stop\n"
            );
        }
    } else if matches!(case, "fg" | "bg") {
        assert_eq!(result["granted"], true);
    } else {
        assert_eq!(result["granted"], false);
    }
    if case == "parent-exit" {
        assert!(
            result["error"]
                .as_str()
                .unwrap()
                .contains("lost live parent witness")
        );
    }
    if case == "cancel" {
        assert_eq!(result["canceled"], true);
    }
    if matches!(case, "ignored" | "caught") {
        assert!(
            result["error"]
                .as_str()
                .unwrap()
                .contains("default uncaught and unignored disposition")
        );
    }
    if case == "blocked" {
        assert!(
            result["error"]
                .as_str()
                .unwrap()
                .contains("blocked on the calling thread")
        );
    }
    println!("native app {case}: {result}; all fixture children reaped");
}
fn kill_owned(owned: &[(Pid, OwnedFd)]) {
    for (_, fd) in owned {
        let _ = rustix::process::pidfd_send_signal(fd, rustix::process::Signal::KILL);
    }
}
fn reap_adopted() {
    let start = Instant::now();
    loop {
        match rustix::process::wait(rustix::process::WaitOptions::NOHANG) {
            Ok(Some(_)) => {}
            Err(rustix::io::Errno::CHILD) => return,
            Ok(None) if start.elapsed() < Duration::from_secs(3) => {
                std::thread::sleep(Duration::from_millis(2))
            }
            result => panic!("owned native fixture reap incomplete: {result:?}"),
        }
    }
}
fn session_keeper(test: &str, case: &str) {
    enter_controlling_session();
    let modes = rustix::termios::tcgetattr(io::stdin()).unwrap();
    let mut witness = command(test, "witness", case, &control())
        .process_group(0)
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .spawn()
        .expect("direct witness shell");
    write_pid("witness-pid", &witness);
    assert!(witness.wait().expect("single witness reap").success());
    std::fs::write(control().join("witness-reaped"), b"").unwrap();
    if case == "parent-exit" {
        wait_file("adopted-reaped");
    }
    attach(rustix::process::getpgrp());
    assert_modes(&modes, &rustix::termios::tcgetattr(io::stdin()).unwrap());
    std::fs::write(control().join("terminal-restored"), b"").unwrap();
}
fn witness_shell(test: &str, case: &str) {
    let parent_modes = rustix::termios::tcgetattr(io::stdin()).unwrap();
    let mut app = command(test, "app", case, &control())
        .process_group(0)
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .spawn()
        .expect("app group");
    write_pid("app-pid", &app);
    attach(pid(&app));
    std::fs::write(control().join("attached"), b"").unwrap();
    if case == "parent-exit" {
        wait_file("validated-before-raise");
        // Deliberately leave the direct app parent; guardian will adopt/reap.
        std::process::exit(0);
    }
    let start = Instant::now();
    let mut stops = 0;
    loop {
        let stopped = match rustix::process::waitid(
            rustix::process::WaitId::Pid(pid(&app)),
            rustix::process::WaitIdOptions::STOPPED | rustix::process::WaitIdOptions::NOHANG,
        ) {
            Ok(status) => status,
            // STOPPED-only waitid can report ECHILD for a final exit. The
            // Child remains the sole exit reaper; accept only its native status.
            Err(rustix::io::Errno::CHILD) => {
                assert!(
                    app.try_wait()
                        .expect("final app status")
                        .expect("ECHILD must have final exit")
                        .success()
                );
                break;
            }
            Err(error) => panic!("observe own app stop: {error}"),
        };
        if let Some(status) = stopped {
            stops += 1;
            assert_eq!(stops, 1);
            assert_eq!(status.stopping_signal(), Some(Signal::SIGTSTP as i32));
            assert!(
                !control().join("result.json").exists(),
                "no grant before continuation"
            );
            assert_eq!(rustix::termios::tcgetpgrp(io::stdin()).unwrap(), pid(&app));
            assert_modes(
                &parent_modes,
                &rustix::termios::tcgetattr(io::stdin()).unwrap(),
            );
            attach(rustix::process::getpgrp());
            if case.starts_with("route-") {
                std::fs::write(control().join("route-parked"), b"").unwrap();
                wait_file("typeahead-sent");
            }
            if matches!(case, "bg" | "cancel" | "route-bg" | "route-cancel") {
                rustix::process::kill_process_group(pid(&app), rustix::process::Signal::CONT)
                    .unwrap();
                // A fixture observation window, never an application expiry.
                std::thread::sleep(Duration::from_millis(350));
                assert!(
                    !control().join("result.json").exists(),
                    "background continuation cannot grant"
                );
                assert!(
                    !control().join("child-resumed").exists(),
                    "child remains stopped while app is background"
                );
                if case == "cancel" {
                    std::fs::write(control().join("cancel"), b"").unwrap();
                }
                if case == "route-cancel" {
                    std::fs::write(
                        control().join("footer-foreground"),
                        rustix::process::getpgrp()
                            .as_raw_nonzero()
                            .get()
                            .to_string(),
                    )
                    .unwrap();
                    rustix::process::kill_process_group(pid(&app), rustix::process::Signal::INT)
                        .unwrap();
                }
            }
            if !matches!(case, "cancel" | "route-cancel") {
                attach(pid(&app));
                rustix::process::kill_process_group(pid(&app), rustix::process::Signal::CONT)
                    .unwrap();
            }
        }
        if let Some(status) = app.try_wait().expect("single final app reap") {
            assert!(status.success());
            break;
        }
        assert!(
            start.elapsed() < Duration::from_secs(8),
            "owned app exit watchdog"
        );
        std::thread::sleep(Duration::from_millis(2));
    }
    assert_eq!(
        stops,
        usize::from(matches!(case, "fg" | "bg" | "cancel") || case.starts_with("route-"))
    );
}
fn expected_footer_foreground() -> Pid {
    std::fs::read_to_string(control().join("footer-foreground"))
        .ok()
        .map(|raw| Pid::from_raw(raw.parse().unwrap()).unwrap())
        .unwrap_or_else(rustix::process::getpgrp)
}
fn require_route_child_gone() {
    let identity: serde_json::Value = serde_json::from_slice(
        &std::fs::read(control().join("route-child-identity")).expect("owned child identity"),
    )
    .unwrap();
    match Identity::read(identity["pid"].as_u64().unwrap() as u32) {
        Ok(current) => assert_ne!(
            current.start,
            identity["start"].as_u64().unwrap(),
            "owned child must be reaped before final footer"
        ),
        Err(ConfctlError::Io { source, .. }) if source.kind() == io::ErrorKind::NotFound => {}
        Err(error) => panic!("verify owned child disappearance: {error}"),
    }
}
struct FooterWriter {
    original: rustix::termios::Termios,
    bytes: std::sync::Arc<std::sync::Mutex<Vec<u8>>>,
    checked: bool,
}
impl Write for FooterWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        let mut all = self.bytes.lock().unwrap();
        all.extend_from_slice(bytes);
        if !self.checked
            && all
                .windows(b"\nTiming:".len())
                .any(|part| part == b"\nTiming:")
        {
            assert!(control().join("operation-joined").exists());
            assert_modes(
                &self.original,
                &rustix::termios::tcgetattr(io::stdin()).unwrap(),
            );
            assert_eq!(
                rustix::termios::tcgetpgrp(io::stdin()).unwrap(),
                expected_footer_foreground()
            );
            require_route_child_gone();
            std::fs::write(control().join("footer-checked"), b"").unwrap();
            self.checked = true;
        }
        drop(all);
        io::stderr().write_all(bytes)?;
        Ok(bytes.len())
    }
    fn flush(&mut self) -> io::Result<()> {
        io::stderr().flush()
    }
}
struct ForegroundOperation<'a> {
    test: &'a str,
    case: &'a str,
}
impl crate::app::Operations for ForegroundOperation<'_> {
    fn available(&self, action: &crate::domain::Action) -> bool {
        matches!(action, crate::domain::Action::Ssh(_))
    }
    async fn run(
        &self,
        _: &crate::domain::Invocation,
        context: &crate::app::Context,
    ) -> Result<crate::domain::ExitDisposition, ConfctlError> {
        use crate::{
            events::TaskId,
            process::{
                ProcessRunner, ProcessScope, ProcessSpec, StdinPolicy, StreamPolicy,
                TerminationCause,
            },
            timing::Phase,
        };
        let original = rustix::termios::tcgetattr(io::stdin()).unwrap();
        let child = command(self.test, "operation-child", self.case, &control());
        let mut spec = ProcessSpec::new(
            PathBuf::from(child.get_program()),
            "actual app foreground fixture",
        );
        spec.args = child.get_args().map(ToOwned::to_owned).collect();
        spec.env = child
            .get_envs()
            .map(|(key, value)| (key.to_owned(), value.map(ToOwned::to_owned)))
            .collect();
        spec.stdin = StdinPolicy::Foreground;
        spec.stdout = StreamPolicy::CaptureAndForward;
        spec.stderr = StreamPolicy::CaptureAndForward;
        let mut scope = ProcessScope::new(context.cancellation.clone(), TaskId(51));
        scope.phase = Some(Phase::SshCommands);
        scope.foreground = Some(context.ui.foreground().await?);
        let runner = context.processes.clone();
        let job = tokio::spawn(async move { runner.run(spec, scope).await });
        while rustix::termios::tcgetpgrp(io::stdin()).unwrap() == rustix::process::getpgrp()
            && !job.is_finished()
        {
            tokio::task::yield_now().await;
        }
        // Same current-thread runtime: regaining execution after attach means
        // the real worker has completed/disarmed its sole initial stop probe.
        std::fs::write(control().join("route-worker-ready"), b"").unwrap();
        let result = job.await.expect("actual operation/process join");
        assert_modes(&original, &rustix::termios::tcgetattr(io::stdin()).unwrap());
        assert_eq!(
            rustix::termios::tcgetpgrp(io::stdin()).unwrap(),
            expected_footer_foreground()
        );
        require_route_child_gone();
        std::fs::write(control().join("operation-joined"), b"").unwrap();
        let outcome = result?;
        if outcome.termination == TerminationCause::Canceled {
            return Err(crate::nix::process_error(ConfctlError::Cancelled, outcome));
        }
        if !outcome.status.success() {
            return Err(crate::nix::process_error(
                ConfctlError::Child {
                    description: "actual app foreground fixture".into(),
                    code: outcome.code(),
                    signal: outcome.signal(),
                    log: None,
                },
                outcome,
            ));
        }
        assert!(outcome.stdout_complete && outcome.stderr_complete);
        let captured = crate::nix::bytes(&outcome.stdout).await?;
        assert!(
            captured
                .windows(b"ROUTE_CHILD_DONE".len())
                .any(|part| part == b"ROUTE_CHILD_DONE")
        );
        Ok(crate::domain::ExitDisposition::completed())
    }
}
fn route_app(test: &str, case: &str) {
    wait_file("attached");
    let original = rustix::termios::tcgetattr(io::stdin()).unwrap();
    let stderr = std::sync::Arc::new(std::sync::Mutex::new(Vec::new()));
    let writer = FooterWriter {
        original: original.clone(),
        bytes: stderr.clone(),
        checked: false,
    };
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let code = runtime.block_on(crate::app::execute(
        ["--color", "never", "ssh", "-y", "fixture", "fixture-child"]
            .map(Into::into)
            .to_vec(),
        crate::timing::Timing::start(),
        &ForegroundOperation { test, case },
        Ok(control()),
        std::sync::Arc::new(std::sync::Mutex::new(Box::new(io::stdout()))),
        std::sync::Arc::new(std::sync::Mutex::new(Box::new(writer))),
    ));
    // execute has checked and joined SignalOwner, Supervisor, input broker and
    // Presentation before rendering its sole footer and returning this status.
    assert_modes(&original, &rustix::termios::tcgetattr(io::stdin()).unwrap());
    require_route_child_gone();
    let stderr = String::from_utf8(stderr.lock().unwrap().clone()).unwrap();
    let state = match case {
        "route-cancel" => "canceled (exit 130)",
        "route-failure" => "failed (exit 1)",
        _ => "completed (exit 0)",
    };
    assert!(stderr.contains(&format!("\nTiming: {state}\n")), "{stderr}");
    assert_eq!(stderr.matches("\nTiming:").count(), 1);
    assert!(stderr.contains("  SSH commands:"));
    std::fs::write(
        control().join("result.json"),
        serde_json::to_vec(&serde_json::json!({"code":code})).unwrap(),
    )
    .unwrap();
}
fn route_child(case: &str) {
    let own = Identity::read(std::process::id()).unwrap();
    let temporary = control().join("route-child-pid.new");
    std::fs::write(&temporary, own.pid.to_string()).unwrap();
    std::fs::rename(temporary, control().join("route-child-pid")).unwrap();
    std::fs::write(
        control().join("route-child-identity"),
        serde_json::to_vec(&serde_json::json!({"pid":own.pid,"start":own.start})).unwrap(),
    )
    .unwrap();
    wait_file("route-worker-ready");
    assert_eq!(
        rustix::termios::tcgetpgrp(io::stdin()).unwrap(),
        rustix::process::getpgrp()
    );
    let mut child_modes = rustix::termios::tcgetattr(io::stdin()).unwrap();
    child_modes
        .local_modes
        .remove(rustix::termios::LocalModes::ECHO);
    rustix::termios::tcsetattr(
        io::stdin(),
        rustix::termios::OptionalActions::Now,
        &child_modes,
    )
    .unwrap();
    raise(Signal::SIGTSTP).unwrap();
    assert_modes(
        &child_modes,
        &rustix::termios::tcgetattr(io::stdin()).unwrap(),
    );
    assert_eq!(
        rustix::termios::tcgetpgrp(io::stdin()).unwrap(),
        rustix::process::getpgrp()
    );
    std::fs::write(control().join("child-resumed"), b"").unwrap();
    let mut line = Vec::new();
    io::stdin().lock().read_until(b'\n', &mut line).unwrap();
    assert_eq!(line, b"queued-after-stop\n");
    std::fs::write(control().join("child-typeahead"), line).unwrap();
    println!("ROUTE_CHILD_DONE");
    io::stdout().flush().unwrap();
    if case == "route-failure" {
        std::process::exit(7);
    }
}
fn app(case: &str) {
    wait_file("attached");
    let original = rustix::termios::tcgetattr(io::stdin()).unwrap();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(async {
        let _caught=if case=="caught" {Some(signal(SignalKind::from_raw(Signal::SIGTSTP as i32)).unwrap())}else{None};
        let (sender,receiver)=mpsc::channel(1);let cancellation=CancellationToken::new();let shutdown=CancellationToken::new();
        let owner=SignalOwner::new(receiver,cancellation.clone(),shutdown.clone()).unwrap();
        let service=tokio::spawn(owner.serve());
        let (reply,mut received)=tokio::sync::oneshot::channel();
        let request=ForegroundSuspendRequest{tty:io::stdin().as_fd().try_clone_to_owned().unwrap(),expected_foreground:rustix::process::getpgrp(),cancellation:cancellation.clone(),reply};
        assert!(sender.send(request).await.is_ok());
        let result=loop {tokio::select! {
            result=&mut received=>break result.expect("checked app reply"),
            _=tokio::time::sleep(Duration::from_millis(5))=>{if control().join("cancel").exists(){cancellation.cancel();}},
        }};
        let result=match result {Ok(resume)=>{assert_eq!(resume.foreground,rustix::process::getpgrp());serde_json::json!({"granted":true,"canceled":false})},Err(error)=>serde_json::json!({"granted":false,"canceled":error.is_cancelled(),"error":error.to_string()})};
        std::fs::write(control().join("result.json"),serde_json::to_vec(&result).unwrap()).unwrap();
        shutdown.cancel();service.await.unwrap().unwrap();drop(sender);
    });
    assert_modes(&original, &rustix::termios::tcgetattr(io::stdin()).unwrap());
}

/// The only fault seam is compiled into tests. It pauses after the actual final
/// preflight and waits for the owned parent pidfd to prove death; production
/// then still follows the normal raise/readiness path, without a fake grant.
pub(super) fn before_raise(parent: &OwnedFd) -> Result<(), ConfctlError> {
    if std::env::var(ROLE).ok().as_deref() != Some("app")
        || std::env::var(CASE).ok().as_deref() != Some("parent-exit")
    {
        return Ok(());
    }
    std::fs::write(control().join("validated-before-raise"), b"").map_err(|source| {
        ConfctlError::Io {
            operation: "native test validation barrier",
            path: None,
            source,
        }
    })?;
    let mut fds = [PollFd::new(parent, PollFlags::IN)];
    loop {
        match poll(&mut fds, None) {
            Ok(ready)
                if ready > 0 && fds[0].revents().intersects(PollFlags::IN | PollFlags::HUP) =>
            {
                return Ok(());
            }
            Err(rustix::io::Errno::INTR) => {}
            result => {
                return Err(ConfctlError::State(format!(
                    "native parent-exit barrier failed: {result:?}"
                )));
            }
        }
    }
}
