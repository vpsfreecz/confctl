//! The SSH adapter uses the existing input broker and real foreground owner.
use super::*;
use crate::{
    presentation::{OutputTargets, Presentation, PresentationConfig},
    process::Supervisor,
    terminal::pty_tests::{
        CASE_ENV, CHILD_ENV, assert_modes, child_read_line, enter_controlling_session, run_fixture,
    },
};
use std::{io::Write, time::Duration};

const TEST: &str = "operations::ssh::pty_tests::interactive_ssh_parks_painting_preserves_typeahead_and_restores_terminal";
struct FixtureRunner(Supervisor);
impl ProcessRunner for FixtureRunner {
    async fn run(
        &self,
        mut spec: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        assert_eq!(spec.program, Path::new("ssh"));
        assert_eq!(spec.args, ["-l", "root", "remote.invalid"]);
        spec.program = std::env::current_exe().unwrap();
        spec.args = vec![
            "--exact".into(),
            TEST.into(),
            "--nocapture".into(),
            "--color=never".into(),
        ];
        spec.env.push((CHILD_ENV.into(), Some("ssh-child".into())));
        self.0.run(spec, scope).await
    }
}
#[test]
fn interactive_ssh_parks_painting_preserves_typeahead_and_restores_terminal() {
    if std::env::var_os(CHILD_ENV).is_some() {
        println!("SSH_CHILD_BEGIN");
        io::stdout().flush().unwrap();
        let mut modes = rustix::termios::tcgetattr(io::stdin()).unwrap();
        modes.local_modes.remove(rustix::termios::LocalModes::ECHO);
        rustix::termios::tcsetattr(io::stdin(), rustix::termios::OptionalActions::Now, &modes)
            .unwrap();
        child_read_line();
        println!("SSH_CHILD_END");
        io::stdout().flush().unwrap();
        match std::env::var(CASE_ENV).unwrap().as_str() {
            "exit7" => std::process::exit(7),
            "signal" => {
                nix::sys::signal::raise(nix::sys::signal::Signal::SIGTERM).unwrap();
                unreachable!()
            }
            _ => std::process::exit(0),
        }
    }
    if std::env::var_os(CASE_ENV).is_none() {
        for case in ["success", "exit7", "signal"] {
            let transcript = run_fixture(TEST, case, b"y\nchild-answer\nparent-answer\n");
            let text = String::from_utf8_lossy(&transcript.bytes);
            assert!(transcript.status.success(), "{case}: {text}");
            assert!(
                text.contains("CHILD_READ:child-answer") && text.contains("SSH_PARENT_RESTORED"),
                "{text}"
            );
            let begin = text.find("SSH_CHILD_BEGIN").unwrap();
            let end = text.find("SSH_CHILD_END").unwrap();
            assert!(
                !text[begin..end].contains("\x1b["),
                "painting during foreground input: {text}"
            );
        }
        return;
    }
    enter_controlling_session();
    let modes = rustix::termios::tcgetattr(io::stdin()).unwrap();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(async {
        let scratch = tempfile::tempdir().unwrap();
        let (events, receiver) = EventSink::channel(1);
        let cancel = CancellationToken::new();
        let (suspend, mut requests) = tokio::sync::mpsc::channel(1);
        let ui = Ui::new(io::stdin(), events.clone(), cancel.clone(), None)
            .unwrap()
            .with_foreground_suspend(suspend);
        ui.set_live(true).unwrap();
        let (control, presentation) = Presentation::new(
            receiver,
            ui.observe_state(),
            OutputTargets::standard(),
            PresentationConfig::for_process(
                "ssh remote".into(),
                "ssh".into(),
                crate::domain::ColorMode::Never,
                scratch.path().into(),
            ),
            None,
        )
        .unwrap();
        let painter = tokio::spawn(presentation.run());
        assert!(ui.confirm(ConfirmPrompt::default()).await.unwrap());
        let runner = FixtureRunner(Supervisor::new(
            events.clone(),
            scratch.path().into(),
            64,
            Duration::from_millis(100),
            None,
            None,
        ));
        let machine = Machine::from_value(
            serde_json::json!({"name":"remote","metaConfig":{"host":{"target":"remote.invalid"}}}),
        )
        .unwrap();
        interactive_shell(
            &Execution {
                runner: &runner,
                events: &events,
                cancellation: &cancel,
                root: scratch.path(),
                config: None,
                live: true,
            },
            &ui,
            &machine,
            || panic!("remote does not resolve local shell"),
        )
        .await
        .unwrap();
        runner.0.shutdown().await.unwrap();
        assert!(matches!(
            requests.try_recv(),
            Err(tokio::sync::mpsc::error::TryRecvError::Empty)
        ));
        assert_modes(&modes, &rustix::termios::tcgetattr(io::stdin()).unwrap());
        assert_eq!(
            rustix::termios::tcgetpgrp(io::stdin()).unwrap(),
            rustix::process::getpgrp()
        );
        assert_eq!(*ui.observe_state().borrow(), TerminalState::Live);
        assert_eq!(
            ui.protected_line("Restored input: ".into()).await.unwrap(),
            b"parent-answer\n"
        );
        ui.shutdown().unwrap();
        control.finish().await.unwrap();
        painter.await.unwrap().unwrap();
        println!("SSH_PARENT_RESTORED");
    });
}
