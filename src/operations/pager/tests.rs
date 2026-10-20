use super::*;
use std::os::unix::ffi::OsStringExt;

#[test]
fn eligibility_is_only_two_output_ttys_exact_disable_and_nonblank_pager() {
    for stdout in [false, true] {
        for stderr in [false, true] {
            for tty in [None, Some("0"), Some("00"), Some("false")] {
                for pager in [
                    None,
                    Some(""),
                    Some(" \t\n\r\0\u{b}\u{c}"),
                    Some("pager --keep-args"),
                ] {
                    let actual = configured(
                        stdout,
                        stderr,
                        tty.map(OsStr::new),
                        pager.map(OsStr::new),
                        Some(OsStr::new("git-pager")),
                    );
                    let expected =
                        stdout && stderr && tty != Some("0") && pager == Some("pager --keep-args");
                    assert_eq!(
                        actual.is_some(),
                        expected,
                        "{stdout}/{stderr}/{tty:?}/{pager:?}"
                    );
                }
            }
        }
    }
}

#[test]
fn configured_native_shell_string_keeps_priority_whitespace_and_arguments() {
    let pager = OsString::from_vec(b"  pager --native=\xff ; printf 'shell code'\n".to_vec());
    let git = OsString::from_vec(b"GIT_PAGER --flag=\xfe".to_vec());
    for blank in [None, Some(OsStr::new(" \t\0\n"))] {
        assert_eq!(
            configured(true, true, None, Some(&pager), blank),
            Some(pager.clone())
        );
    }
    assert_eq!(
        configured(true, true, None, Some(&pager), Some(&git)),
        Some(git)
    );
    assert!(configured(true, true, None, None, Some(&pager)).is_none());
    // Non-ASCII whitespace is not Ruby String#strip whitespace.
    assert!(configured(true, true, None, Some(OsStr::new("\u{a0}")), None).is_some());
}

use crate::{
    events::{EventReceiver, EventSink},
    process::{Captured, Supervisor},
    terminal::{TerminalState, Ui},
    timing::Timing,
};
use std::{io, time::Duration};
use tokio_util::sync::CancellationToken;

fn context(directory: &std::path::Path) -> (Context, EventReceiver) {
    let (events, receiver) = EventSink::channel(1);
    let cancel = CancellationToken::new();
    let ui = Ui::new(io::stdin(), events.clone(), cancel.clone(), None).unwrap();
    let runner = Supervisor::new(
        events.clone(),
        directory.into(),
        64,
        Duration::from_millis(30),
        None,
        None,
    );
    (
        Context::for_test(
            directory.into(),
            cancel,
            events,
            runner,
            ui,
            Timing::start(),
            directory.join("invocation.log"),
        ),
        receiver,
    )
}

#[tokio::test]
async fn unpaged_facade_waits_for_raw_stdout_ack_and_preserves_output_error_and_cancel() {
    for mode in ["written", "output-error", "cancel"] {
        let directory = tempfile::tempdir().unwrap();
        let (context, mut receiver) = context(directory.path());
        let mut pager = Pager::new(&context, TaskId(37)).unwrap();
        pager.command = None;
        pager.output_tty = None;
        let mut writing = Box::pin(pager.write(b"report\0\xff"));
        let mut poll = std::task::Context::from_waker(std::task::Waker::noop());
        assert!(writing.as_mut().poll(&mut poll).is_pending());
        let delivery = receiver.receive().await.unwrap();
        assert_eq!(
            delivery.event,
            UiEvent::Output {
                task: TaskId(37),
                stream: OutputStream::Stdout,
                bytes: b"report\0\xff".to_vec()
            }
        );
        if mode == "cancel" {
            context.cancellation.cancel();
        }
        delivery.acknowledge(if mode == "output-error" {
            Err("stdout write failed".into())
        } else {
            Ok(())
        });
        let result = writing.await;
        match mode {
            "written" => assert_eq!(result.unwrap(), WrittenOrClosed::Written),
            "output-error" => assert!(
                result
                    .unwrap_err()
                    .to_string()
                    .contains("stdout write failed")
            ),
            _ => assert!(result.unwrap_err().is_cancelled()),
        }
        let finished = pager.finish().await;
        if mode == "cancel" {
            assert!(finished.unwrap_err().is_cancelled());
        } else {
            assert!(finished.unwrap().is_none());
        }
        context.processes.shutdown().await.unwrap();
        context.ui.shutdown().unwrap();
        drop(context);
        assert!(receiver.receive().await.is_none());
    }
}

#[test]
fn paged_facade_is_lazy_runs_once_and_preserves_close_native_and_finish_results() {
    use crate::terminal::{
        foreground::ForegroundSuspendRequest,
        pty_tests::{CASE_ENV, assert_modes, enter_controlling_session, run_fixture},
    };
    const TEST: &str = "operations::pager::tests::paged_facade_is_lazy_runs_once_and_preserves_close_native_and_finish_results";
    if std::env::var_os(CASE_ENV).is_none() {
        for mode in [
            "empty",
            "normal",
            "closed",
            "nonzero",
            "native-delivery-error",
            "cancel",
            "post-ack-cancel",
            "producer-error",
        ] {
            let transcript = run_fixture(TEST, mode, b"");
            assert!(
                transcript.status.success(),
                "{}",
                String::from_utf8_lossy(&transcript.bytes)
            );
        }
        return;
    }
    enter_controlling_session();
    let mode = std::env::var(CASE_ENV).unwrap();
    let original = rustix::termios::tcgetattr(io::stdout()).unwrap();
    tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap().block_on(async {
        let directory = tempfile::tempdir().unwrap();
        let (mut context, mut receiver) = context(directory.path());
        let (suspend, _requests) = tokio::sync::mpsc::channel::<ForegroundSuspendRequest>(1);
        context.ui = context.ui.with_foreground_suspend(suspend);
        let cancel = context.cancellation.clone();
        let delivery_mode = mode.clone();
        let consumer = tokio::spawn(async move {
            let mut observed = Vec::new();
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                let final_event = matches!(event, UiEvent::TaskFinished { .. });
                observed.push(event);
                if final_event && delivery_mode == "post-ack-cancel" { cancel.cancel(); }
                ack.acknowledge(if final_event && delivery_mode == "native-delivery-error" { Err("final delivery failed".into()) } else { Ok(()) });
            }
            observed
        });
        let mut pager = Pager::new(&context, TaskId(41)).unwrap();
        // Concrete command fixture; production policy has no test/runtime knob.
        pager.command = Some(match mode.as_str() {
            "closed" => "printf x >> starts; exec 0<&-; exit 7",
            "cancel" => "printf x >> starts; exec sleep 30",
            "producer-error" => "bad\0argument",
            "nonzero" | "native-delivery-error" => "printf x >> starts; cat > report; exit 7",
            _ => "printf x >> starts; cat > report",
        }.into());
        pager.output_tty = Some(io::stdout().as_fd().try_clone_to_owned().unwrap());
        assert_eq!(pager.write(&[]).await.unwrap(), WrittenOrClosed::Written);
        assert!(!directory.path().join("starts").exists());
        let finished = match mode.as_str() {
            "empty" => pager.finish().await,
            "closed" => {
                assert_eq!(pager.write(&vec![b'x'; 1024 * 1024]).await.unwrap(), WrittenOrClosed::Closed);
                assert!(!context.cancellation.is_cancelled());
                // Closed persists without another native request or future work.
                assert_eq!(pager.write(b"later").await.unwrap(), WrittenOrClosed::Closed);
                pager.finish().await
            }
            "cancel" => {
                let bytes = vec![b'x'; 1024 * 1024];
                let mut writing = Box::pin(pager.write(&bytes));
                tokio::select! { result = &mut writing => panic!("unexpected completed write: {result:?}"), _ = tokio::time::sleep(Duration::from_millis(50)) => {} }
                context.cancellation.cancel();
                assert!(writing.await.unwrap_err().is_cancelled());
                pager.finish().await
            }
            "producer-error" => {
                let primary = pager.write(b"report").await.unwrap_err();
                let secondary = pager.finish().await.unwrap_err();
                let combined = primary.with_secondary(vec![secondary]);
                let ConfctlError::Cleanup { primary, secondary } = combined else { panic!() };
                assert!(matches!(*primary, ConfctlError::EventDelivery(_)));
                assert!(matches!(secondary[0], ConfctlError::Spawn { .. }));
                // The producer's earlier failure stays primary to its finish error.
                Err(*primary)
            }
            _ => {
                assert_eq!(pager.write(b"report\0\xff").await.unwrap(), WrittenOrClosed::Written);
                pager.finish().await
            }
        };
        match mode.as_str() {
            "empty" => assert!(finished.unwrap().is_none()),
            "cancel" | "post-ack-cancel" => {
                let error = finished.unwrap_err();
                assert!(error.is_cancelled());
                let ConfctlError::ProcessFailure { evidence, .. } = error else { panic!() };
                assert!(evidence.status.is_some());
                assert!(matches!(evidence.stdout, Captured::Unavailable));
            }
            "producer-error" => assert!(finished.is_err()),
            "native-delivery-error" => {
                let ConfctlError::ProcessFailure { source, evidence } = finished.unwrap_err() else { panic!() };
                assert_eq!(evidence.status.unwrap().code(), Some(7));
                let ConfctlError::Cleanup { primary, secondary } = *source else { panic!() };
                assert!(matches!(*primary, ConfctlError::Child { code: Some(7), .. }));
                assert!(secondary[0].to_string().contains("final delivery failed"));
            }
            _ => {
                let outcome = finished.unwrap().unwrap();
                assert_eq!(outcome.code(), Some(if matches!(mode.as_str(), "closed" | "nonzero") { 7 } else { 0 }));
                assert!(matches!(outcome.stdout, Captured::Unavailable) && !outcome.stdout_complete);
                assert!(matches!(outcome.stderr, Captured::Unavailable) && !outcome.stderr_complete);
            }
        }
        if mode != "empty" && mode != "producer-error" { assert_eq!(std::fs::read(directory.path().join("starts")).unwrap(), b"x"); }
        if matches!(mode.as_str(), "normal" | "nonzero") { assert_eq!(std::fs::read(directory.path().join("report")).unwrap(), b"report\0\xff"); }
        assert_eq!(context.ui.state(), TerminalState::Idle);
        assert_eq!(rustix::termios::tcgetpgrp(io::stdout()).unwrap(), rustix::process::getpgrp());
        assert_modes(&original, &rustix::termios::tcgetattr(io::stdout()).unwrap());
        context.processes.shutdown().await.unwrap();
        context.ui.shutdown().unwrap();
        drop(context);
        let observed = consumer.await.unwrap();
        if mode == "empty" { assert!(observed.is_empty()); }
        else {
            assert!(matches!(observed[0], UiEvent::TaskPending { task: TaskId(41), .. }));
            assert!(matches!(observed[1], UiEvent::TaskStarted { task: TaskId(41), .. }));
            assert!(matches!(observed.last().unwrap(), UiEvent::TaskFinished { task: TaskId(41), state } if *state == match mode.as_str() { "cancel" => TaskState::Canceled, "closed" | "nonzero" | "native-delivery-error" | "producer-error" => TaskState::Failed, _ => TaskState::Succeeded }));
        }
    });
}
