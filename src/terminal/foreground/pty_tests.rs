use super::*;
use crate::terminal::pty_tests::{
    CASE_ENV, CHILD_ENV, assert_modes, child_read_line, enter_controlling_session, run_fixture,
};
use std::{
    os::unix::process::CommandExt,
    process::Stdio,
    time::{Duration, Instant},
};

async fn stopped(pid: Pid, expected: rustix::process::Signal) {
    let deadline = Instant::now();
    loop {
        let event = rustix::process::waitid(
            rustix::process::WaitId::Pid(pid),
            rustix::process::WaitIdOptions::STOPPED
                | rustix::process::WaitIdOptions::CONTINUED
                | rustix::process::WaitIdOptions::NOHANG,
        )
        .expect("sole stop notification consumer");
        if let Some(event) = event
            && event.stopped()
        {
            assert_eq!(event.stopping_signal(), Some(expected.as_raw()));
            return;
        }
        assert!(
            deadline.elapsed() < Duration::from_secs(5),
            "stop notification watchdog"
        );
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
}

#[test]
fn stopped_child_modes_roundtrip_through_parent_restore_and_resume_request() {
    const TEST: &str = "terminal::foreground::pty_tests::stopped_child_modes_roundtrip_through_parent_restore_and_resume_request";
    if std::env::var_os(CHILD_ENV).is_some() {
        let mut attributes = rustix::termios::tcgetattr(io::stdin()).expect("child modes");
        attributes
            .local_modes
            .remove(rustix::termios::LocalModes::ECHO);
        rustix::termios::tcsetattr(io::stdin(), OptionalActions::Now, &attributes)
            .expect("child owns mode mutation after initial TTOU resume");
        ::nix::sys::signal::raise(Signal::SIGTSTP).expect("deliberate child stop");
        assert!(
            !rustix::termios::tcgetattr(io::stdin())
                .expect("resumed child modes")
                .local_modes
                .contains(rustix::termios::LocalModes::ECHO)
        );
        child_read_line();
        return;
    }
    if std::env::var_os(CASE_ENV).is_none() {
        let transcript = run_fixture(TEST, "child-modes", b"after-resume\n");
        assert!(
            transcript.status.success(),
            "{}",
            String::from_utf8_lossy(&transcript.bytes)
        );
        assert!(String::from_utf8_lossy(&transcript.bytes).contains("CHILD_READ:after-resume"));
        return;
    }
    enter_controlling_session();
    let original = rustix::termios::tcgetattr(io::stdin()).expect("parent modes");
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime");
    runtime.block_on(async {
        let (events, mut receiver) = crate::events::EventSink::channel(1);
        let consumer = tokio::spawn(async move {
            while let Some(delivery) = receiver.receive().await {
                delivery.acknowledge(Ok(()));
            }
        });
        let (requests, mut suspend) = tokio::sync::mpsc::channel::<ForegroundSuspendRequest>(1);
        let ui =
            crate::terminal::Ui::new(io::stdin(), events.clone(), CancellationToken::new(), None)
                .expect("UI")
                .with_foreground_suspend(requests);
        let mut lease = ui.foreground().await.expect("input lease");
        let _sigchld = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::child())
            .expect("child notifications");
        let mut child =
            std::process::Command::new(std::env::current_exe().expect("test executable"))
                .args(["--exact", TEST, "--nocapture"])
                .env(CHILD_ENV, "child-modes")
                .stdin(Stdio::inherit())
                .stdout(Stdio::inherit())
                .stderr(Stdio::inherit())
                .process_group(0)
                .spawn()
                .expect("mode-changing child");
        let pid = Pid::from_raw(child.id() as i32).expect("owned child");
        stopped(pid, rustix::process::Signal::TTOU).await;
        let mut attachment = lease.attach(pid).expect("attach after early TTOU");
        rustix::process::kill_process_group(pid, rustix::process::Signal::CONT)
            .expect("resume owned early stop");
        stopped(pid, rustix::process::Signal::TSTP).await;
        assert!(
            !rustix::termios::tcgetattr(io::stdin())
                .expect("stopped child modes")
                .local_modes
                .contains(rustix::termios::LocalModes::ECHO)
        );
        let expected_parent = original.clone();
        let responder = tokio::spawn(async move {
            let request = suspend.recv().await.expect("app request");
            assert_eq!(
                rustix::termios::tcgetpgrp(&request.tty).expect("parent foreground before request"),
                request.expected_foreground
            );
            assert_modes(
                &expected_parent,
                &rustix::termios::tcgetattr(&request.tty).expect("parent modes before request"),
            );
            // This primitive fixture verifies the typed boundary/restoration;
            // real app SIGTSTP/CONT/parent-pidfd proof is owner0's separate gate.
            request
                .reply
                .send(Ok(ForegroundResume {
                    foreground: request.expected_foreground,
                }))
                .unwrap_or_else(|_| panic!("resume receiver"));
        });
        attachment
            .request_suspend(&CancellationToken::new())
            .await
            .expect("restored then requested");
        responder.await.expect("app boundary");
        attachment
            .reattach()
            .expect("reattach and restore child modes");
        assert!(attachment.owns_terminal().expect("child foreground"));
        assert!(
            !rustix::termios::tcgetattr(io::stdin())
                .expect("child modes before CONT")
                .local_modes
                .contains(rustix::termios::LocalModes::ECHO)
        );
        rustix::process::kill_process_group(pid, rustix::process::Signal::CONT)
            .expect("continue only after child modes restored");
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
            assert!(
                deadline.elapsed() < Duration::from_secs(5),
                "child exit watchdog"
            );
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        attachment.restore().expect("restore parent terminal");
        assert_modes(
            &original,
            &rustix::termios::tcgetattr(io::stdin()).expect("parent modes after exit"),
        );
        assert!(child.wait().expect("single final reap").success());
        drop(lease);
        ui.shutdown().expect("input join");
        drop(ui);
        drop(events);
        consumer.await.expect("event join");
    });
}
