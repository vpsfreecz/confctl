//! Native controlling-session fixtures. The outer driver owns the PTY master;
//! the reexecuted test owns fd0/the slave. No test helper reads application stdin.

use std::{
    io,
    os::fd::OwnedFd,
    process::{ExitStatus, Stdio},
    time::{Duration, Instant},
};

pub(crate) const CASE_ENV: &str = "CONFCTL_NATIVE_PTY_CASE";
pub(crate) const CHILD_ENV: &str = "CONFCTL_NATIVE_PTY_CHILD";

pub(crate) struct Transcript {
    pub bytes: Vec<u8>,
    pub status: ExitStatus,
}

pub(crate) fn open_pty() -> (OwnedFd, OwnedFd) {
    use rustix::pty::{OpenptFlags, grantpt, ioctl_tiocgptpeer, openpt, unlockpt};
    let flags = OpenptFlags::RDWR | OpenptFlags::NOCTTY | OpenptFlags::CLOEXEC;
    let master = openpt(flags).expect("PTY master");
    grantpt(&master).expect("grant PTY");
    unlockpt(&master).expect("unlock PTY");
    let slave = ioctl_tiocgptpeer(&master, flags).expect("PTY slave");
    rustix::termios::tcsetwinsize(
        &slave,
        rustix::termios::Winsize {
            ws_row: 24,
            ws_col: 80,
            ws_xpixel: 0,
            ws_ypixel: 0,
        },
    )
    .expect("initial size");
    (master, slave)
}

pub(crate) fn enter_controlling_session() {
    rustix::process::setsid().expect("isolated session");
    rustix::process::ioctl_tiocsctty(io::stdin()).expect("own controlling tty");
    assert_eq!(
        rustix::termios::tcgetpgrp(io::stdin()).expect("foreground"),
        rustix::process::getpgrp()
    );
}

pub(crate) fn run_fixture(test: &str, case: &str, input: &[u8]) -> Transcript {
    run_fixture_after_output(test, case, input, None)
}

pub(crate) fn run_fixture_after_output(
    test: &str,
    case: &str,
    input: &[u8],
    after: Option<(&[u8], &[u8])>,
) -> Transcript {
    let (master, slave) = open_pty();
    let mut child = std::process::Command::new(std::env::current_exe().expect("test executable"))
        .args(["--exact", test, "--nocapture"])
        .env(CASE_ENV, case)
        .env("TERM", "xterm-256color")
        .env("CONFCTL_TTY", "1")
        .stdin(Stdio::from(slave.try_clone().expect("stdin")))
        .stdout(Stdio::from(slave.try_clone().expect("stdout")))
        .stderr(Stdio::from(slave))
        .spawn()
        .expect("controlling fixture");
    let mut remaining = input;
    while !remaining.is_empty() {
        let count = rustix::io::write(&master, remaining).expect("typeahead");
        assert!(count > 0);
        remaining = &remaining[count..];
    }
    let flags = rustix::fs::fcntl_getfl(&master).expect("owned master flags");
    rustix::fs::fcntl_setfl(&master, flags | rustix::fs::OFlags::NONBLOCK)
        .expect("owned master nonblocking");
    let started = Instant::now();
    let mut bytes = Vec::new();
    let mut after = after;
    loop {
        drain(&master, &mut bytes);
        if let Some((marker, send)) = after
            && bytes.windows(marker.len()).any(|bytes| bytes == marker)
        {
            assert_eq!(
                rustix::io::write(&master, send).expect("terminal input after readiness"),
                send.len()
            );
            after = None;
        }
        if let Some(status) = child.try_wait().expect("fixture status") {
            drain(&master, &mut bytes);
            return Transcript { bytes, status };
        }
        if started.elapsed() > Duration::from_secs(15) {
            // Killing the isolated session leader hangs up its controlling tty;
            // fixture children retain default HUP behavior and bounded reads.
            child.kill().expect("cancel owned hung fixture");
            let _ = child.wait();
            panic!(
                "native fixture exceeded test watchdog: {}",
                String::from_utf8_lossy(&bytes)
            );
        }
        std::thread::sleep(Duration::from_millis(5));
    }
}
fn drain(master: &OwnedFd, output: &mut Vec<u8>) {
    loop {
        let mut bytes = [0; 4096];
        match rustix::io::read(master, &mut bytes) {
            Ok(0) | Err(rustix::io::Errno::AGAIN) | Err(rustix::io::Errno::IO) => break,
            Ok(count) => {
                output.extend_from_slice(&bytes[..count]);
                assert!(output.len() < 4 * 1024 * 1024, "bounded fixture transcript");
            }
            Err(rustix::io::Errno::INTR) => {}
            Err(error) => panic!("PTY transcript: {error}"),
        }
    }
}

pub(crate) fn assert_modes(expected: &rustix::termios::Termios, actual: &rustix::termios::Termios) {
    assert_eq!(expected.input_modes, actual.input_modes);
    assert_eq!(expected.output_modes, actual.output_modes);
    assert_eq!(expected.control_modes, actual.control_modes);
    assert_eq!(expected.local_modes, actual.local_modes);
    assert_eq!(expected.input_speed(), actual.input_speed());
    assert_eq!(expected.output_speed(), actual.output_speed());
    assert_eq!(
        format!("{:?}", expected.special_codes),
        format!("{:?}", actual.special_codes)
    );
    assert_eq!(expected.line_discipline, actual.line_discipline);
}

pub(crate) fn child_read_line() {
    let input = io::stdin();
    let mut line = Vec::new();
    loop {
        let mut polling = [rustix::event::PollFd::new(
            &input,
            rustix::event::PollFlags::IN,
        )];
        assert!(
            rustix::event::poll(
                &mut polling,
                Some(&rustix::event::Timespec {
                    tv_sec: 5,
                    tv_nsec: 0
                })
            )
            .expect("bounded child input readiness")
                > 0,
            "child input watchdog"
        );
        let mut byte = [0];
        assert_eq!(
            rustix::io::read(&input, &mut byte).expect("foreground child read"),
            1
        );
        line.push(byte[0]);
        if byte[0] == b'\n' {
            break;
        }
        assert!(line.len() < 100);
    }
    println!(
        "CHILD_READ:{}",
        String::from_utf8(line).expect("test answer").trim_end()
    );
}

#[test]
fn controlling_pty_attachment_preserves_typeahead_and_restores_parent_modes() {
    const TEST: &str = "terminal::pty_tests::controlling_pty_attachment_preserves_typeahead_and_restores_parent_modes";
    if std::env::var_os(CHILD_ENV).is_some() {
        child_read_line();
        return;
    }
    if std::env::var_os(CASE_ENV).is_none() {
        let transcript = run_fixture(TEST, "attachment", b"queued-answer\n");
        assert!(
            transcript.status.success(),
            "{}",
            String::from_utf8_lossy(&transcript.bytes)
        );
        assert!(
            transcript
                .bytes
                .windows(b"CHILD_READ:queued-answer".len())
                .any(|bytes| bytes == b"CHILD_READ:queued-answer")
        );
        return;
    }
    enter_controlling_session();
    let original = rustix::termios::tcgetattr(io::stdin()).expect("original termios");
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime");
    runtime.block_on(async {
        use crate::{events::EventSink, terminal::Ui};
        use std::os::unix::process::CommandExt;
        let (sink, mut receiver) = EventSink::channel(1);
        let ui = Ui::new(
            io::stdin(),
            sink.clone(),
            tokio_util::sync::CancellationToken::new(),
            None,
        )
        .expect("UI");
        let consumer = tokio::spawn(async move {
            while let Some(delivery) = receiver.receive().await {
                delivery.acknowledge(Ok(()));
            }
        });
        let mut lease = ui.foreground().await.expect("exclusive foreground lease");
        let _sigchld = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::child())
            .expect("child notification owner");
        let mut child =
            std::process::Command::new(std::env::current_exe().expect("test executable"))
                .args(["--exact", TEST, "--nocapture"])
                .env(CHILD_ENV, "read")
                .stdin(Stdio::inherit())
                .stdout(Stdio::inherit())
                .stderr(Stdio::inherit())
                .process_group(0)
                .spawn()
                .expect("early-reader child");
        let pid = rustix::process::Pid::from_raw(child.id() as i32).expect("owned child PID");
        let deadline = Instant::now();
        loop {
            let stopped = rustix::process::waitid(
                rustix::process::WaitId::Pid(pid),
                rustix::process::WaitIdOptions::STOPPED
                    | rustix::process::WaitIdOptions::NOWAIT
                    | rustix::process::WaitIdOptions::NOHANG,
            )
            .expect("observe early I/O stop without consuming");
            if let Some(status) = stopped {
                assert_eq!(
                    status.stopping_signal(),
                    Some(rustix::process::Signal::TTIN.as_raw())
                );
                break;
            }
            assert!(deadline.elapsed() < Duration::from_secs(5), "early TTIN");
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        let attachment = lease.attach(pid).expect("real tcsetpgrp attachment");
        assert!(attachment.owns_terminal().expect("child foreground"));
        rustix::process::kill_process_group(pid, rustix::process::Signal::CONT)
            .expect("owned early-I/O resume");
        let deadline = Instant::now();
        loop {
            if rustix::process::waitid(
                rustix::process::WaitId::Pid(pid),
                rustix::process::WaitIdOptions::EXITED
                    | rustix::process::WaitIdOptions::NOWAIT
                    | rustix::process::WaitIdOptions::NOHANG,
            )
            .expect("reserve exit")
            .is_some()
            {
                break;
            }
            assert!(deadline.elapsed() < Duration::from_secs(5), "child exit");
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        attachment
            .restore()
            .expect("restore parent group and modes");
        assert_eq!(
            rustix::termios::tcgetpgrp(io::stdin()).expect("parent foreground"),
            rustix::process::getpgrp()
        );
        assert_modes(
            &original,
            &rustix::termios::tcgetattr(io::stdin()).expect("restored termios"),
        );
        assert!(child.wait().expect("single final child reap").success());
        drop(lease);
        ui.shutdown().expect("input join");
        drop(ui);
        drop(sink);
        consumer.await.expect("event join");
    });
}

#[test]
fn live_pty_resize_prompt_foreground_and_final_footer_preserve_input_modes() {
    const TEST: &str = "terminal::pty_tests::live_pty_resize_prompt_foreground_and_final_footer_preserve_input_modes";
    if std::env::var_os(CHILD_ENV).is_some() {
        child_read_line();
        return;
    }
    if std::env::var_os(CASE_ENV).is_none() {
        let transcript = run_fixture(TEST, "live", b"y\nqueued-child\n");
        assert!(
            transcript.status.success(),
            "{}",
            String::from_utf8_lossy(&transcript.bytes)
        );
        assert!(
            !transcript.bytes.windows(4).any(|bytes| bytes == b"\x1b[6n"),
            "no cursor query"
        );
        assert!(
            !transcript
                .bytes
                .windows(8)
                .any(|bytes| bytes == b"\x1b[?1049h"),
            "normal screen"
        );
        assert!(
            transcript
                .bytes
                .windows(b"CHILD_READ:queued-child".len())
                .any(|bytes| bytes == b"CHILD_READ:queued-child"),
            "child typeahead retained"
        );
        let text = String::from_utf8_lossy(&transcript.bytes);
        assert_eq!(text.matches("PRELUDE node").count(), 1);
        assert_eq!(text.matches("Timing: completed (exit 0)").count(), 1);
        assert!(
            text.rfind("succeeded").expect("final task panel")
                < text.find("Timing: completed").expect("final footer")
        );
        assert!(text.contains("Continue? [y/N]: "));
        return;
    }
    enter_controlling_session();
    let original = rustix::termios::tcgetattr(io::stdin()).expect("original termios");
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime");
    runtime.block_on(async {
        use crate::{
            domain::{Action, ColorMode, ExitDisposition, Targets},
            events::{EventSink, OutputStream, Style, TaskId, TaskState, UiEvent},
            presentation::{OutputTargets, Presentation, PresentationConfig},
            terminal::{ConfirmPrompt, Ui},
            timing::Timing,
        };
        use std::os::unix::process::CommandExt;
        let timing = Timing::start();
        timing.admit(&Action::CollectGarbage {
            targets: Targets::default(),
            max_concurrent: 1,
        });
        let directory = tempfile::tempdir().expect("transfer spools");
        let (events, receiver) = EventSink::channel(1);
        let ui = Ui::new(
            io::stdin(),
            events.clone(),
            tokio_util::sync::CancellationToken::new(),
            Some(timing.clone()),
        )
        .expect("UI");
        ui.set_live(true).expect("live state");
        let config = PresentationConfig::for_process(
            "build node".into(),
            "build".into(),
            ColorMode::Never,
            directory.path().into(),
        );
        assert!(config.policy.live, "real stdout and stderr TTY");
        let (control, presentation) = Presentation::new(
            receiver,
            ui.observe_state(),
            OutputTargets::standard(),
            config,
            Some(timing.clone()),
        )
        .expect("presentation");
        let consumer = tokio::spawn(presentation.run());
        events
            .emit(UiEvent::OperationGroup {
                group: 1,
                description: "build".into(),
                prelude: vec![UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Heading,
                    text: "PRELUDE node\n".into(),
                }],
            })
            .await
            .expect("prelude");
        events
            .emit(UiEvent::TaskStarted {
                task: TaskId(1),
                description: "node-task".into(),
            })
            .await
            .expect("task");
        events
            .emit(UiEvent::TaskProgress {
                task: TaskId(1),
                completed: 1,
                total: Some(2),
                detail: "half".into(),
            })
            .await
            .expect("progress");
        events
            .emit(UiEvent::Output {
                task: TaskId(1),
                stream: OutputStream::Stderr,
                bytes: b"log-before\n".to_vec(),
            })
            .await
            .expect("reliable stderr");
        tokio::time::sleep(Duration::from_millis(120)).await;
        assert_modes(
            &original,
            &rustix::termios::tcgetattr(io::stdin()).expect("live did not change raw mode"),
        );
        rustix::termios::tcsetwinsize(
            io::stdin(),
            rustix::termios::Winsize {
                ws_row: 8,
                ws_col: 14,
                ws_xpixel: 0,
                ws_ypixel: 0,
            },
        )
        .expect("narrow resize");
        tokio::time::sleep(Duration::from_millis(120)).await;
        assert_modes(
            &original,
            &rustix::termios::tcgetattr(io::stdin()).expect("resize did not change raw mode"),
        );
        assert!(
            ui.confirm(ConfirmPrompt::default())
                .await
                .expect("typed answer")
        );
        let mut lease = ui.foreground().await.expect("handoff barrier");
        let _sigchld = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::child())
            .expect("child notifications");
        let mut child =
            std::process::Command::new(std::env::current_exe().expect("test executable"))
                .args(["--exact", TEST, "--nocapture"])
                .env(CHILD_ENV, "read")
                .stdin(Stdio::inherit())
                .stdout(Stdio::inherit())
                .stderr(Stdio::inherit())
                .process_group(0)
                .spawn()
                .expect("foreground fixture child");
        let pid = rustix::process::Pid::from_raw(child.id() as i32).expect("owned child");
        let attachment = lease.attach(pid).expect("real child attachment");
        // This primitive harness drives its one owned child's resume directly;
        // the Supervisor's stricter single initial probe has separate gates.
        rustix::process::kill_process_group(pid, rustix::process::Signal::CONT)
            .expect("primitive child continuation");
        let deadline = Instant::now();
        loop {
            if rustix::process::waitid(
                rustix::process::WaitId::Pid(pid),
                rustix::process::WaitIdOptions::EXITED
                    | rustix::process::WaitIdOptions::NOWAIT
                    | rustix::process::WaitIdOptions::NOHANG,
            )
            .expect("exit reservation")
            .is_some()
            {
                break;
            }
            assert!(deadline.elapsed() < Duration::from_secs(5), "child read");
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        attachment.restore().expect("restore before UI resume");
        assert!(child.wait().expect("single final reap").success());
        drop(lease);
        assert_modes(
            &original,
            &rustix::termios::tcgetattr(io::stdin()).expect("parent restored"),
        );
        events
            .emit(UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Succeeded,
            })
            .await
            .expect("result");
        ui.shutdown().expect("input join");
        control.finish().await.expect("checked presentation finish");
        consumer.await.expect("presentation join").expect("run");
        timing
            .finish(ExitDisposition::completed())
            .expect("one footer")
            .write(&mut io::stderr())
            .expect("footer after joins");
    });
}
