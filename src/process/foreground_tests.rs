//! REAL Supervisor route tests after native attachment qualification.
//! The fake app reply
//! verifies worker/terminal ordering; owner0 separately proves real self-stop.

use super::*;
use crate::{
    events::TaskState,
    presentation::{OutputTargets, Presentation, PresentationConfig},
    terminal::{
        Ui,
        foreground::{ForegroundResume, ForegroundSuspendRequest},
        pty_tests::{
            CASE_ENV, CHILD_ENV, assert_modes, child_read_line, enter_controlling_session,
            run_fixture, run_fixture_after_output,
        },
    },
};
use std::{io::Write, sync::atomic::AtomicUsize};

fn child_role(role: &str) {
    match role {
        "reader" | "cancel" | "early-read" => child_read_line(),
        "early-write" => {
            let modes = rustix::termios::tcgetattr(std::io::stdin()).expect("child modes");
            // A real background tcsetattr produces TTOU before worker attach.
            rustix::termios::tcsetattr(
                std::io::stdin(),
                rustix::termios::OptionalActions::Now,
                &modes,
            )
            .expect("continued foreground tcsetattr");
        }
        "initial-tstp" => {
            ::nix::sys::signal::raise(::nix::sys::signal::Signal::SIGTSTP)
                .expect("pre-attach TSTP");
        }
        "initial-exit" => std::process::exit(0),
        "ctrl-c" => {
            println!("CHILD_ETX_READY");
            std::io::stdout().flush().expect("ready flushed");
            child_read_line();
        }
        "relay-fault" => {
            std::io::stdout()
                .write_all(b"FAULT_CAPTURE_PRIVATE\0\xff")
                .expect("fault prefix");
            std::io::stdout().flush().expect("prefix flush");
            child_read_line();
        }
        "exit7" => {
            std::io::stdout()
                .write_all(b"captured-out\0\xff\n")
                .expect("stdout");
            std::io::stderr()
                .write_all(b"captured-err\n")
                .expect("stderr");
            std::process::exit(7);
        }
        "tail" | "initial-tail" | "initial-cancel-tail" => {
            let mut descendant = std::process::Command::new(std::env::current_exe().expect("test executable"))
                .args(["--exact","process::foreground_tests::supervisor_foreground_reads_exits_and_retains_descendant_tail","--nocapture"])
                .env(CHILD_ENV,"tail-writer").stdin(Stdio::null()).stdout(Stdio::inherit()).stderr(Stdio::inherit()).spawn().expect("finite descendant tail");
            assert!(
                descendant
                    .try_wait()
                    .expect("tail writer live at leader exit")
                    .is_none()
            );
            // Deliberately exit before the finite inherited-pipe writer.
            std::process::exit(0);
        }
        "tail-writer" => {
            std::thread::sleep(Duration::from_millis(80));
            std::io::stdout()
                .write_all(b"DESCENDANT_TAIL\0\xff\n")
                .expect("tail");
        }
        "later-stops" => {
            let go =
                PathBuf::from(std::env::var_os("CONFCTL_PTY_GO").expect("test handshake path"));
            let started = Instant::now();
            while !go.exists() {
                assert!(
                    started.elapsed() < Duration::from_secs(5),
                    "parent first-yield handshake"
                );
                std::thread::sleep(Duration::from_millis(1));
            }
            let mut modes =
                rustix::termios::tcgetattr(std::io::stdin()).expect("child terminal modes");
            modes.local_modes.remove(rustix::termios::LocalModes::ECHO);
            rustix::termios::tcsetattr(
                std::io::stdin(),
                rustix::termios::OptionalActions::Now,
                &modes,
            )
            .expect("foreground mode mutation");
            for signal in [
                ::nix::sys::signal::Signal::SIGTTIN,
                ::nix::sys::signal::Signal::SIGTTOU,
                ::nix::sys::signal::Signal::SIGTSTP,
                ::nix::sys::signal::Signal::SIGTTIN,
            ] {
                ::nix::sys::signal::raise(signal).expect("owned deliberate later stop");
                assert!(
                    !rustix::termios::tcgetattr(std::io::stdin())
                        .expect("child modes before continued execution")
                        .local_modes
                        .contains(rustix::termios::LocalModes::ECHO)
                );
            }
            println!("LATER_STOPS_DONE");
        }
        _ => panic!("unknown fixture child role"),
    }
}

fn launch_fixture(test: &str, case: &str, input: &[u8]) -> Option<Vec<u8>> {
    if let Some(role) = std::env::var_os(CHILD_ENV) {
        child_role(role.to_str().expect("role"));
        return Some(Vec::new());
    }
    if std::env::var_os(CASE_ENV).is_none() {
        let transcript = run_fixture(test, case, input);
        assert!(
            transcript.status.success(),
            "{}",
            String::from_utf8_lossy(&transcript.bytes)
        );
        return Some(transcript.bytes);
    }
    None
}

async fn fixture(test: &str, role: &str) {
    let original = rustix::termios::tcgetattr(std::io::stdin()).expect("parent modes");
    let directory = tempfile::tempdir().expect("spools");
    let (events, receiver) = EventSink::channel(1);
    let cancel = CancellationToken::new();
    let (request, mut requests) = mpsc::channel::<ForegroundSuspendRequest>(1);
    let ui = Ui::new(std::io::stdin(), events.clone(), cancel.clone(), None)
        .expect("input owner")
        .with_foreground_suspend(request);
    ui.set_live(true).expect("live");
    let config = PresentationConfig::for_process(
        "native fixture".into(),
        "foreground".into(),
        crate::domain::ColorMode::Never,
        directory.path().into(),
    );
    let (control, presentation) = Presentation::new(
        receiver,
        ui.observe_state(),
        OutputTargets::standard(),
        config,
        None,
    )
    .expect("presentation");
    let consumer = tokio::spawn(presentation.run());
    events
        .emit(UiEvent::TaskStarted {
            task: TaskId(1),
            description: "native foreground".into(),
        })
        .await
        .expect("task");
    let count = Arc::new(AtomicUsize::new(0));
    let observed = count.clone();
    let stop_service = CancellationToken::new();
    let stop = stop_service.clone();
    let parent_modes = original.clone();
    let replies = tokio::spawn(async move {
        loop {
            tokio::select! {
                _=stop.cancelled() => { requests.close();while let Ok(request)=requests.try_recv() {let _=request.reply.send(Err(ConfctlError::Cancelled));}break; },
                request=requests.recv() => { let Some(request)=request else {break;};
                    assert_modes(&parent_modes,&rustix::termios::tcgetattr(&request.tty).expect("parent modes before app request"));
                    assert_eq!(rustix::termios::tcgetpgrp(&request.tty).expect("foreground before app request"),request.expected_foreground);
                    observed.fetch_add(1,Ordering::SeqCst);
                    // No self-stop is simulated here. Only app-owned signal tests
                    // can prove the fresh SIGCONT/direct-parent pidfd contract.
                    let _=request.reply.send(Ok(ForegroundResume {foreground:request.expected_foreground}));
                },
            }
        }
    });
    let runner = Supervisor::new(
        events.clone(),
        directory.path().into(),
        64,
        Duration::from_millis(50),
        None,
        None,
    );
    let mut spec = ProcessSpec::new(
        std::env::current_exe().expect("test executable"),
        "native fixture child",
    );
    spec.args = vec!["--exact".into(), test.into(), "--nocapture".into()];
    spec.env.push((CHILD_ENV.into(), Some(role.into())));
    spec.before_attach_stop = match role {
        "early-read" => Some(rustix::process::Signal::TTIN),
        "early-write" => Some(rustix::process::Signal::TTOU),
        "initial-tstp" => Some(rustix::process::Signal::TSTP),
        _ => None,
    };
    spec.before_attach_exit = matches!(
        role,
        "initial-exit" | "initial-tail" | "initial-cancel-tail"
    );
    let go = directory.path().join("after-first-yield");
    if role == "later-stops" {
        spec.env
            .push(("CONFCTL_PTY_GO".into(), Some(go.as_os_str().to_owned())));
    }
    spec.stdin = StdinPolicy::Foreground;
    if matches!(
        role,
        "exit7" | "tail" | "initial-exit" | "initial-tail" | "initial-cancel-tail"
    ) {
        spec.stdout = StreamPolicy::Capture;
        spec.stderr = StreamPolicy::Capture;
    } else {
        spec.stdout = StreamPolicy::Foreground;
        spec.stderr = StreamPolicy::Foreground;
    }
    let mut scope = ProcessScope::new(cancel.clone(), TaskId(1));
    scope.foreground = Some(ui.foreground().await.expect("real handoff barrier"));
    let running = runner.clone();
    let job = tokio::spawn(async move { running.run(spec, scope).await });
    if matches!(role, "cancel" | "later-stops" | "initial-cancel-tail") {
        let started = Instant::now();
        while rustix::termios::tcgetpgrp(std::io::stdin()).expect("foreground ownership")
            == rustix::process::getpgrp()
        {
            assert!(
                started.elapsed() < Duration::from_secs(5),
                "child attachment watchdog"
            );
            tokio::task::yield_now().await;
        }
        // This fixture uses one runtime thread. Regaining execution after the
        // worker attached means it has yielded after its ONE initial probe.
        if matches!(role, "cancel" | "initial-cancel-tail") {
            cancel.cancel();
        } else {
            std::fs::write(&go, b"go").expect("first-yield handshake");
        }
    }
    let outcome = job
        .await
        .expect("operation join")
        .expect("ordinary outcome");
    assert_eq!(
        rustix::termios::tcgetpgrp(std::io::stdin()).expect("parent foreground restored"),
        rustix::process::getpgrp()
    );
    assert_modes(
        &original,
        &rustix::termios::tcgetattr(std::io::stdin()).expect("parent termios restored"),
    );
    match role {
        "exit7" => {
            assert_eq!(outcome.code(), Some(7));
            assert!(outcome.stdout_complete && outcome.stderr_complete);
            assert!(
                capture(&outcome.stdout)
                    .windows(b"captured-out\0\xff\n".len())
                    .any(|bytes| bytes == b"captured-out\0\xff\n")
            );
            assert!(capture(&outcome.stderr).ends_with(b"captured-err\n"));
        }
        "tail" | "initial-tail" => {
            assert_eq!(outcome.code(), Some(0));
            assert!(outcome.stdout_complete && outcome.stderr_complete);
            assert!(
                capture(&outcome.stdout)
                    .windows(b"DESCENDANT_TAIL\0\xff\n".len())
                    .any(|bytes| bytes == b"DESCENDANT_TAIL\0\xff\n")
            );
        }
        "cancel" => {
            assert_eq!(outcome.termination, TerminationCause::Canceled);
            assert!(!outcome.stdout_complete && !outcome.stderr_complete);
            assert!(matches!(&outcome.stdout, Captured::Unavailable));
        }
        "initial-exit" => {
            assert_eq!(outcome.code(), Some(0));
            assert!(outcome.stdout_complete && outcome.stderr_complete);
            assert_eq!(count.load(Ordering::SeqCst), 0);
        }
        "initial-cancel-tail" => {
            assert_eq!(outcome.termination, TerminationCause::Canceled);
            assert!(!outcome.stdout_complete && !outcome.stderr_complete);
        }
        "reader" | "early-read" | "early-write" | "initial-tstp" => {
            assert_eq!(outcome.code(), Some(0));
            assert!(!outcome.stdout_complete && !outcome.stderr_complete);
            assert!(matches!(&outcome.stdout, Captured::Unavailable));
            assert_eq!(
                count.load(Ordering::SeqCst),
                usize::from(role == "initial-tstp")
            );
        }
        "ctrl-c" => {
            use std::os::unix::process::ExitStatusExt;
            assert_eq!(
                outcome.status.signal(),
                Some(rustix::process::Signal::INT.as_raw())
            );
            assert_eq!(outcome.termination, TerminationCause::Exited);
        }
        "later-stops" => {
            assert_eq!(outcome.code(), Some(0));
            assert_eq!(
                count.load(Ordering::SeqCst),
                4,
                "every later stop used app boundary"
            );
            assert_eq!(
                ui.protected_line("After child: ".into())
                    .await
                    .expect("input still queued"),
                b"next-input\n"
            );
        }
        _ => unreachable!(),
    }
    stop_service.cancel();
    replies
        .await
        .expect("close/reject request service before joins");
    runner.shutdown().await.expect("supervisor checked join");
    ui.shutdown().expect("input checked join");
    // Cancellation stops Ui emit; final state is delivered through the same
    // event sink after work has joined, independently of that operation token.
    events
        .emit(UiEvent::TaskFinished {
            task: TaskId(1),
            state: if matches!(role, "cancel" | "initial-cancel-tail") {
                TaskState::Canceled
            } else if outcome.status.success() {
                TaskState::Succeeded
            } else {
                TaskState::Failed
            },
        })
        .await
        .expect("final explicit state");
    control.finish().await.expect("checked presentation finish");
    consumer
        .await
        .expect("presentation join")
        .expect("presentation run");
}

// Test-only scheduling barrier: observe real early-I/O/TSTP kernel notification
// without consuming it. The actual Supervisor's one initial probe consumes it.
pub(super) fn wait_before_attachment(
    pid: rustix::process::Pid,
    expected: rustix::process::Signal,
) -> Result<(), ConfctlError> {
    let start = Instant::now();
    loop {
        let notification = rustix::process::waitid(
            rustix::process::WaitId::Pid(pid),
            rustix::process::WaitIdOptions::STOPPED
                | rustix::process::WaitIdOptions::NOWAIT
                | rustix::process::WaitIdOptions::NOHANG,
        )
        .map_err(|source| io_error("fixture observe pre-attach stop", None, source.into()))?;
        if let Some(notification) = notification {
            assert_eq!(notification.stopping_signal(), Some(expected.as_raw()));
            return Ok(());
        }
        assert!(
            start.elapsed() < Duration::from_secs(5),
            "pre-attach kernel-stop test watchdog"
        );
        std::thread::sleep(Duration::from_millis(1));
    }
}

pub(super) fn wait_for_exit_before_attachment(group: &mut Group) -> Result<(), ConfctlError> {
    let start = Instant::now();
    loop {
        if group
            .probe()
            .map_err(|source| io_error("fixture reserve exited child", None, source))?
            .is_some()
        {
            return Ok(());
        }
        assert!(
            start.elapsed() < Duration::from_secs(5),
            "unreaped exit test watchdog"
        );
        std::thread::sleep(Duration::from_millis(1));
    }
}
fn capture(value: &Captured) -> Vec<u8> {
    match value {
        Captured::Memory(bytes) => bytes.clone(),
        Captured::Spool { file, length } => {
            std::fs::read(file.path()).expect("capture")[..*length as usize].to_vec()
        }
        Captured::Unavailable => Vec::new(),
    }
}

#[test]
fn supervisor_foreground_reads_exits_and_retains_descendant_tail() {
    const TEST: &str =
        "process::foreground_tests::supervisor_foreground_reads_exits_and_retains_descendant_tail";
    if std::env::var_os(CASE_ENV).is_none() && std::env::var_os(CHILD_ENV).is_none() {
        for case in ["reader", "exit7", "tail"] {
            let bytes = launch_fixture(
                TEST,
                case,
                if case == "reader" {
                    b"child-answer\n"
                } else {
                    b""
                },
            )
            .expect("driver");
            if case == "reader" {
                assert!(String::from_utf8_lossy(&bytes).contains("CHILD_READ:child-answer"));
            }
        }
        return;
    }
    if launch_fixture(TEST, "", b"").is_some() {
        return;
    }
    let role = std::env::var(CASE_ENV).expect("case");
    enter_controlling_session();
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime")
        .block_on(fixture(TEST, &role));
}

#[test]
fn supervisor_foreground_cancellation_restores_terminal_and_keeps_normal_canceled_outcome() {
    const TEST: &str = "process::foreground_tests::supervisor_foreground_cancellation_restores_terminal_and_keeps_normal_canceled_outcome";
    if launch_fixture(TEST, "cancel", b"").is_some() {
        return;
    }
    enter_controlling_session();
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime")
        .block_on(fixture(TEST, "cancel"));
}

#[test]
fn supervisor_later_ttin_ttou_tstp_and_repeated_stop_preserve_child_modes() {
    const TEST: &str = "process::foreground_tests::supervisor_later_ttin_ttou_tstp_and_repeated_stop_preserve_child_modes";
    if launch_fixture(TEST, "later-stops", b"next-input\n").is_some() {
        return;
    }
    enter_controlling_session();
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime")
        .block_on(fixture(TEST, "later-stops"));
}

#[test]
fn supervisor_queued_early_io_uses_single_initial_probe_but_tstp_requests_app() {
    const TEST: &str = "process::foreground_tests::supervisor_queued_early_io_uses_single_initial_probe_but_tstp_requests_app";
    if std::env::var_os(CASE_ENV).is_none() && std::env::var_os(CHILD_ENV).is_none() {
        for case in ["early-read", "early-write", "initial-tstp"] {
            launch_fixture(
                TEST,
                case,
                if case == "early-read" {
                    b"early-input\n"
                } else {
                    b""
                },
            )
            .expect("driver");
        }
        return;
    }
    if launch_fixture(TEST, "", b"").is_some() {
        return;
    }
    let role = std::env::var(CASE_ENV).expect("case");
    enter_controlling_session();
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime")
        .block_on(fixture(TEST, &role));
}

#[test]
fn supervisor_terminal_ctrl_c_targets_child_and_restores_parent() {
    const TEST: &str =
        "process::foreground_tests::supervisor_terminal_ctrl_c_targets_child_and_restores_parent";
    if let Some(role) = std::env::var_os(CHILD_ENV) {
        child_role(role.to_str().expect("child role"));
        return;
    }
    if std::env::var_os(CASE_ENV).is_none() {
        let transcript =
            run_fixture_after_output(TEST, "ctrl-c", b"", Some((b"CHILD_ETX_READY", b"\x03")));
        assert!(
            transcript.status.success(),
            "{}",
            String::from_utf8_lossy(&transcript.bytes)
        );
        return;
    }
    enter_controlling_session();
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime")
        .block_on(fixture(TEST, "ctrl-c"));
}

#[test]
fn supervisor_foreground_relay_fault_retains_evidence_and_restores_terminal() {
    const TEST: &str = "process::foreground_tests::supervisor_foreground_relay_fault_retains_evidence_and_restores_terminal";
    if launch_fixture(TEST, "relay-fault", b"").is_some() {
        return;
    }
    enter_controlling_session();
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime")
        .block_on(async {
            let original = rustix::termios::tcgetattr(std::io::stdin()).expect("parent modes");
            let directory = tempfile::tempdir().expect("spools");
            let (events, mut receiver) = EventSink::channel(1);
            let consumer = tokio::spawn(async move {
                let mut rejected = false;
                let prefix = b"FAULT_CAPTURE_PRIVATE\0\xff";
                let mut observed = Vec::new();
                while let Some(delivery) = receiver.receive().await {
                    // libtest can emit its banner in an earlier pipe chunk.
                    // Inject only after the asserted capture prefix is real.
                    let fail = if let UiEvent::Output {
                        stream: OutputStream::Stdout,
                        bytes,
                        ..
                    } = &delivery.event
                    {
                        observed.extend_from_slice(bytes);
                        let present = observed.windows(prefix.len()).any(|bytes| bytes == prefix);
                        if observed.len() > prefix.len() {
                            observed.drain(..observed.len() - prefix.len());
                        }
                        present
                    } else {
                        false
                    };
                    if fail {
                        rejected = true;
                    }
                    delivery.acknowledge(if fail {
                        Err("injected real relay failure".into())
                    } else {
                        Ok(())
                    });
                }
                assert!(rejected, "fault must be real relay delivery");
            });
            let (request, _requests) = mpsc::channel::<ForegroundSuspendRequest>(1);
            let ui = Ui::new(
                std::io::stdin(),
                events.clone(),
                CancellationToken::new(),
                None,
            )
            .expect("UI")
            .with_foreground_suspend(request);
            let runner = Supervisor::new(
                events.clone(),
                directory.path().into(),
                64,
                Duration::from_millis(20),
                None,
                None,
            );
            let mut spec = ProcessSpec::new(
                std::env::current_exe().expect("test executable"),
                "relay fault",
            );
            spec.args = vec!["--exact".into(), TEST.into(), "--nocapture".into()];
            spec.env
                .push((CHILD_ENV.into(), Some("relay-fault".into())));
            spec.stdin = StdinPolicy::Foreground;
            spec.stdout = StreamPolicy::CaptureAndForward;
            spec.stderr = StreamPolicy::Capture;
            let mut scope = ProcessScope::new(CancellationToken::new(), TaskId(1));
            scope.foreground = Some(ui.foreground().await.expect("lease"));
            let failure = runner
                .run(spec, scope)
                .await
                .expect_err("actual relay failure");
            let ConfctlError::ProcessFailure { source, evidence } = &failure else {
                panic!("owned process evidence: {failure}");
            };
            assert!(matches!(
                source.as_ref(),
                ConfctlError::EventDelivery(_) | ConfctlError::Cleanup { .. }
            ));
            assert!(
                capture(&evidence.stdout)
                    .windows(b"FAULT_CAPTURE_PRIVATE\0\xff".len())
                    .any(|b| b == b"FAULT_CAPTURE_PRIVATE\0\xff")
            );
            assert!(!format!("{failure:?}").contains("FAULT_CAPTURE_PRIVATE"));
            assert!(
                evidence.status.is_some(),
                "single final reap observed status"
            );
            assert_eq!(
                rustix::termios::tcgetpgrp(std::io::stdin()).expect("restored group"),
                rustix::process::getpgrp()
            );
            assert_modes(
                &original,
                &rustix::termios::tcgetattr(std::io::stdin()).expect("restored modes"),
            );
            runner.shutdown().await.expect("process joined");
            ui.shutdown().expect("input joined");
            drop(ui);
            drop(runner);
            drop(events);
            consumer.await.expect("relay joined");
        });
}

#[test]
fn supervisor_controlling_tty_without_app_policy_refuses_before_spawn() {
    const TEST: &str = "process::foreground_tests::supervisor_controlling_tty_without_app_policy_refuses_before_spawn";
    if launch_fixture(TEST, "missing-policy", b"").is_some() {
        return;
    }
    enter_controlling_session();
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime")
        .block_on(async {
            let directory = tempfile::tempdir().expect("spools");
            let (events, mut receiver) = EventSink::channel(1);
            let consumer = tokio::spawn(async move {
                while let Some(delivery) = receiver.receive().await {
                    delivery.acknowledge(Ok(()));
                }
            });
            let ui = Ui::new(
                std::io::stdin(),
                events.clone(),
                CancellationToken::new(),
                None,
            )
            .expect("UI without app owner");
            let runner = Supervisor::new(
                events.clone(),
                directory.path().into(),
                64,
                Duration::from_millis(20),
                None,
                None,
            );
            let mut spec =
                ProcessSpec::new(directory.path().join("must-not-spawn"), "missing policy");
            spec.stdin = StdinPolicy::Foreground;
            let mut scope = ProcessScope::new(CancellationToken::new(), TaskId(1));
            scope.foreground = Some(ui.foreground().await.expect("lease"));
            assert!(matches!(
                runner.run(spec, scope).await,
                Err(ConfctlError::ForegroundJobControlUnavailable)
            ));
            runner.shutdown().await.expect("process joined");
            ui.shutdown().expect("input joined");
            drop(ui);
            drop(runner);
            drop(events);
            consumer.await.expect("events joined");
        });
}

#[tokio::test]
async fn stop_notification_echild_disarms_cont_and_single_final_reap() {
    let mut child = Command::new("sh")
        .args(["-c", "exit 0"])
        .spawn()
        .expect("child");
    let pid = rustix::process::Pid::from_raw(child.id().expect("PID") as i32).expect("owned PID");
    let mut group = Group::new(pid);
    assert!(
        final_wait(&mut child, &mut group)
            .await
            .expect("single reap")
            .success()
    );
    match group.transition() {
        Err(error) => assert_eq!(
            error.raw_os_error(),
            Some(rustix::io::Errno::CHILD.raw_os_error())
        ),
        Ok(_) => panic!("ECHILD after reap"),
    }
    group
        .signal_using(rustix::process::Signal::CONT, |_, _| {
            panic!("no numeric signal after reap/ECHILD")
        })
        .expect("inactive signal");
    assert_eq!(
        group
            .transitions
            .iter()
            .filter(|value| **value == "wait")
            .count(),
        1
    );
    assert!(!group.transitions.contains(&"numeric-signal"));
}

#[tokio::test]
async fn notification_echild_for_reserved_zombie_preserves_single_final_reap() {
    let _notifications = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::child())
        .expect("pre-spawn SIGCHLD owner");
    let mut child = Command::new("sh")
        .args(["-c", "exit 0"])
        .process_group(0)
        .spawn()
        .expect("child");
    let pid = rustix::process::Pid::from_raw(child.id().expect("PID") as i32).expect("owned PID");
    let mut group = Group::new(pid);
    assert!(
        observe_exit(&mut group)
            .await
            .expect("nonconsuming exit observation")
            .success()
    );
    let notification = rustix::process::waitid(
        rustix::process::WaitId::Pid(pid),
        rustix::process::WaitIdOptions::STOPPED
            | rustix::process::WaitIdOptions::CONTINUED
            | rustix::process::WaitIdOptions::NOHANG,
    );
    assert_eq!(
        notification.expect_err("notification eligibility excludes zombie"),
        rustix::io::Errno::CHILD
    );
    assert!(
        group
            .transition()
            .expect("independent ownership confirmation")
            .is_none()
    );
    assert!(
        group.active,
        "notification-only ECHILD did not disarm reserved identity"
    );
    assert!(
        group
            .probe()
            .expect("still reserved")
            .expect("zombie")
            .success()
    );
    assert!(
        group.transitions.is_empty(),
        "ordinary zombie never signaled/disarmed before final wait"
    );
    assert!(
        final_wait(&mut child, &mut group)
            .await
            .expect("one final consuming wait")
            .success()
    );
    assert_eq!(group.transitions, ["disarm", "wait"]);
    group
        .signal_using(rustix::process::Signal::CONT, |_, _| {
            panic!("numeric signal after final reap")
        })
        .expect("disarmed");
    assert!(
        group.transition().is_err(),
        "disarmed group is never rearmed"
    );
    assert!(!group.active);
}

#[tokio::test]
async fn notification_ownership_loss_disarms_before_any_numeric_signal() {
    let mut child = Command::new("sh")
        .args(["-c", "exit 0"])
        .process_group(0)
        .spawn()
        .expect("child");
    let pid = rustix::process::Pid::from_raw(child.id().expect("PID") as i32).expect("owned PID");
    let mut group = Group::new(pid);
    child
        .wait()
        .await
        .expect("simulate external consuming wait");
    assert!(group.active, "ownership loss not yet observed");
    match group.transition() {
        Err(error) => assert_eq!(
            error.raw_os_error(),
            Some(rustix::io::Errno::CHILD.raw_os_error())
        ),
        Ok(_) => panic!("true ownership-probe ECHILD must fail"),
    }
    assert!(!group.active);
    assert_eq!(group.transitions, ["disarm"]);
    group
        .signal_using(rustix::process::Signal::CONT, |_, _| {
            panic!("numeric signal after ownership loss")
        })
        .expect("disarmed");
}

#[test]
fn supervisor_exit_before_initial_attach_preserves_completion_tail_and_cancellation() {
    const TEST: &str = "process::foreground_tests::supervisor_exit_before_initial_attach_preserves_completion_tail_and_cancellation";
    if std::env::var_os(CASE_ENV).is_none() && std::env::var_os(CHILD_ENV).is_none() {
        for case in ["initial-exit", "initial-tail", "initial-cancel-tail"] {
            launch_fixture(TEST, case, b"").expect("driver");
        }
        return;
    }
    if launch_fixture(TEST, "", b"").is_some() {
        return;
    }
    let role = std::env::var(CASE_ENV).expect("case");
    enter_controlling_session();
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime")
        .block_on(fixture(TEST, &role));
}

#[test]
fn pager_output_lease_owns_tty_with_piped_input_and_joins_stops_and_cancel() {
    use std::io::Read;
    const TEST: &str = "process::foreground_tests::pager_output_lease_owns_tty_with_piped_input_and_joins_stops_and_cancel";
    if let Some(role) = std::env::var_os(CHILD_ENV) {
        let role = role.to_str().unwrap();
        if role == "early-exit" {
            std::process::exit(7);
        }
        let mut tty = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open("/dev/tty")
            .unwrap();
        if role == "stops" {
            let go = PathBuf::from(std::env::var_os("CONFCTL_PAGER_GO").unwrap());
            let deadline = Instant::now();
            while !go.exists() {
                assert!(deadline.elapsed() < Duration::from_secs(5));
                std::thread::sleep(Duration::from_millis(1));
            }
            let mut modes = rustix::termios::tcgetattr(&tty).unwrap();
            modes.local_modes.remove(rustix::termios::LocalModes::ECHO);
            rustix::termios::tcsetattr(&tty, rustix::termios::OptionalActions::Now, &modes)
                .unwrap();
            for signal in [
                ::nix::sys::signal::Signal::SIGTSTP,
                ::nix::sys::signal::Signal::SIGTTIN,
            ] {
                ::nix::sys::signal::raise(signal).unwrap();
                assert!(
                    !rustix::termios::tcgetattr(&tty)
                        .unwrap()
                        .local_modes
                        .contains(rustix::termios::LocalModes::ECHO)
                );
            }
        }
        let mut answer = [0; 2];
        tty.read_exact(&mut answer).unwrap();
        assert_eq!(&answer, b"y\n");
        let mut report = Vec::new();
        std::io::stdin().read_to_end(&mut report).unwrap();
        assert_eq!(report, b"report\0\xff");
        println!("PAGER_TTY_READ");
        return;
    }
    if std::env::var_os(CASE_ENV).is_none() {
        for role in ["piped", "tty-input", "stops", "cancel", "early-exit"] {
            let transcript = run_fixture(
                TEST,
                role,
                if matches!(role, "cancel" | "early-exit") {
                    b""
                } else {
                    b"y\n"
                },
            );
            assert!(
                transcript.status.success(),
                "{}",
                String::from_utf8_lossy(&transcript.bytes)
            );
            if !matches!(role, "cancel" | "early-exit") {
                assert!(String::from_utf8_lossy(&transcript.bytes).contains("PAGER_TTY_READ"));
            }
        }
        return;
    }
    enter_controlling_session();
    let role = std::env::var(CASE_ENV).unwrap();
    let original = rustix::termios::tcgetattr(std::io::stdout()).unwrap();
    let pipe = if role != "tty-input" {
        let (read, write) = rustix::pipe::pipe().unwrap();
        rustix::stdio::dup2_stdin(read).unwrap();
        Some(write)
    } else {
        None
    };
    tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap().block_on(async {
        let directory = tempfile::tempdir().unwrap();
        let (events, mut receiver) = EventSink::channel(1);
        let consumer = tokio::spawn(async move { while let Some(delivery) = receiver.receive().await { delivery.acknowledge(Ok(())); } });
        let cancel = CancellationToken::new();
        let (sender, mut requests) = mpsc::channel::<ForegroundSuspendRequest>(1);
        let ui = Ui::new(std::io::stdin(), events.clone(), cancel.clone(), None).unwrap().with_foreground_suspend(sender);
        let stop = CancellationToken::new();
        let service_stop = stop.clone();
        let modes = original.clone();
        let service = tokio::spawn(async move {
            let mut count = 0;
            loop { tokio::select! {
                _ = service_stop.cancelled() => break,
                request = requests.recv() => {
                    let request = request.unwrap();
                    assert_eq!(rustix::termios::tcgetpgrp(&request.tty).unwrap(), request.expected_foreground);
                    assert_modes(&modes, &rustix::termios::tcgetattr(&request.tty).unwrap());
                    count += 1;
                    request.reply.send(Ok(ForegroundResume { foreground: request.expected_foreground })).unwrap_or_else(|_| panic!("resume request lost"));
                }
            }}
            count
        });
        let runner = Supervisor::new(events.clone(), directory.path().into(), 64, Duration::from_millis(30), None, None);
        let mut spec = ProcessSpec::new(std::env::current_exe().unwrap(), "pager PTY fixture");
        spec.args = vec!["--exact".into(), TEST.into(), "--nocapture".into()];
        spec.env.push((CHILD_ENV.into(), Some(role.clone().into())));
        let go = directory.path().join("go");
        spec.env.push(("CONFCTL_PAGER_GO".into(), Some(go.as_os_str().to_owned())));
        spec.stdout = StreamPolicy::Foreground;
        spec.stderr = StreamPolicy::Foreground;
        let (mut writer, input) = PagerInput::channel();
        spec.stdin = StdinPolicy::Pager(input);
        if role == "piped" { spec.before_attach_stop = Some(rustix::process::Signal::TTIN); }
        if role == "early-exit" { spec.before_attach_exit = true; }
        let mut scope = ProcessScope::new(cancel.clone(), TaskId(1));
        scope.foreground = Some(ui.pager_foreground(std::io::stdout().as_fd().try_clone_to_owned().unwrap()).await.unwrap());
        let running = runner.clone();
        let job = tokio::spawn(async move { running.run(spec, scope).await });
        let producer = async {
            let result = writer.write(b"report\0\xff", &cancel).await;
            if role == "cancel" { assert!(result.is_ok() || result.unwrap_err().is_cancelled()); }
            else if role == "early-exit" { assert_eq!(result.unwrap(), WrittenOrClosed::Closed); }
            else { assert_eq!(result.unwrap(), WrittenOrClosed::Written); }
            drop(writer);
        };
        let coordinate = async {
            if matches!(role.as_str(), "stops" | "cancel") {
                let deadline = Instant::now();
                while rustix::termios::tcgetpgrp(std::io::stdout()).unwrap() == rustix::process::getpgrp() {
                    assert!(deadline.elapsed() < Duration::from_secs(5));
                    tokio::task::yield_now().await;
                }
                if role == "cancel" { cancel.cancel(); } else { std::fs::write(go, b"go").unwrap(); }
            }
            job.await.unwrap().unwrap()
        };
        let (outcome, ()) = tokio::join!(coordinate, producer);
        if role == "cancel" { assert_eq!(outcome.termination, TerminationCause::Canceled); }
        else if role == "early-exit" { assert_eq!(outcome.code(), Some(7)); }
        else { assert!(outcome.status.success()); }
        assert!(matches!(outcome.stdout, Captured::Unavailable) && !outcome.stdout_complete);
        assert!(matches!(outcome.stderr, Captured::Unavailable) && !outcome.stderr_complete);
        assert_eq!(rustix::termios::tcgetpgrp(std::io::stdout()).unwrap(), rustix::process::getpgrp());
        assert_modes(&original, &rustix::termios::tcgetattr(std::io::stdout()).unwrap());
        assert_eq!(ui.state(), crate::terminal::TerminalState::Idle);
        stop.cancel();
        assert_eq!(service.await.unwrap(), if role == "stops" { 2 } else { 0 });
        runner.shutdown().await.unwrap();
        ui.shutdown().unwrap();
        drop(ui); drop(runner); drop(events);
        consumer.await.unwrap();
    });
    drop(pipe);
}
