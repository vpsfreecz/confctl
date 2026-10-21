use super::*;
use std::future::Future;

#[tokio::test]
async fn broker_does_not_read_ahead_and_cancellation_releases_pending_input() {
    let (input, mut peer) = UnixStream::pair().expect("input");
    let broker = Arc::new(InputBroker::new(&input).expect("broker"));
    peer.write_all(b"y\nnext\n").expect("input");
    assert_eq!(
        broker
            .line(false, &CancellationToken::new())
            .await
            .expect("answer"),
        b"y\n"
    );
    let mut remaining = [0; 5];
    rustix::io::read(&input, &mut remaining).expect("unread bytes");
    assert_eq!(&remaining, b"next\n");
    let cancel = CancellationToken::new();
    let run_broker = broker.clone();
    let run_cancel = cancel.clone();
    let pending = tokio::spawn(async move { run_broker.line(false, &run_cancel).await });
    tokio::task::yield_now().await;
    cancel.cancel();
    assert!(matches!(
        pending.await.expect("join"),
        Err(ConfctlError::Cancelled)
    ));
    peer.write_all(b"again\n").expect("input");
    assert_eq!(
        broker
            .line(false, &CancellationToken::new())
            .await
            .expect("new prompt"),
        b"again\n"
    );
    broker.shutdown().expect("join broker");
}

#[tokio::test]
async fn prompt_preserves_explicit_answers_and_owns_terminal_until_done() {
    let (input, mut peer) = UnixStream::pair().expect("input");
    let (events, mut receiver) = EventSink::channel(2);
    let ui = Ui::new(&input, events, CancellationToken::new(), None).expect("UI");
    ui.set_live(true).expect("live");
    let consumer = tokio::spawn(async move {
        let mut prompts = 0;
        while let Some(delivery) = receiver.receive().await {
            if matches!(&delivery.event, UiEvent::PromptFrame { text, .. } if text.contains("Continue?"))
            {
                prompts += 1;
            }
            delivery.acknowledge(Ok(()));
        }
        prompts
    });
    let prompt_ui = ui.clone();
    let prompt = tokio::spawn(async move { prompt_ui.confirm(ConfirmPrompt::default()).await });
    while ui.state() != TerminalState::Prompt {
        tokio::task::yield_now().await;
    }
    assert!(matches!(
        ui.foreground().await,
        Err(ConfctlError::TerminalBusy)
    ));
    peer.write_all(b"\nyes\ny\n").expect("answers");
    assert!(prompt.await.expect("join").expect("confirm"));
    assert_eq!(ui.state(), TerminalState::Live);
    let lease = ui.foreground().await.expect("foreground");
    lease.validate_process_handoff().expect("non-TTY fixture");
    assert_eq!(ui.state(), TerminalState::ExternalChild);
    drop(lease);
    ui.shutdown().expect("broker join");
    drop(ui);
    assert_eq!(consumer.await.expect("consumer join"), 3);
}

#[tokio::test]
async fn yes_skips_preview_and_eof_is_not_an_implicit_answer() {
    let (input, peer) = UnixStream::pair().expect("input");
    let (events, receiver) = EventSink::channel(1);
    let ui = Ui::new(&input, events, CancellationToken::new(), None).expect("UI");
    assert!(
        ui.confirm(ConfirmPrompt {
            assume_yes: true,
            ..ConfirmPrompt::default()
        })
        .await
        .expect("yes")
    );
    assert_eq!(ui.state(), TerminalState::Idle);
    drop(receiver);
    drop(peer);
    assert!(matches!(
        ui.session
            .broker
            .line(false, &CancellationToken::new())
            .await,
        Err(ConfctlError::InputEof)
    ));
    ui.shutdown().expect("join");
}

#[tokio::test]
async fn visible_line_preserves_echo_bytes_and_exclusive_prompt_ownership() {
    let (master, slave) = local_pty();
    let original = rustix::termios::tcgetattr(&slave).expect("termios");
    let (events, mut receiver) = EventSink::channel(1);
    let ui = Ui::new(&slave, events, CancellationToken::new(), None).expect("UI");
    ui.set_live(true).expect("live");
    let mut line = Box::pin(ui.line("Choice: ".into()));
    let mut context = std::task::Context::from_waker(std::task::Waker::noop());
    assert!(line.as_mut().poll(&mut context).is_pending());
    let parked = receiver.receive().await.expect("park event");
    assert_eq!(parked.event, UiEvent::TerminalState(TerminalState::Prompt));
    parked.acknowledge(Ok(()));
    assert!(line.as_mut().poll(&mut context).is_pending());
    let prompt = receiver.receive().await.expect("prompt event");
    assert_eq!(
        prompt.event,
        UiEvent::PromptFrame {
            preview: Vec::new(),
            text: "Choice: ".into(),
        }
    );
    prompt.acknowledge(Ok(()));
    assert!(line.as_mut().poll(&mut context).is_pending());
    assert!(matches!(
        ui.foreground().await,
        Err(ConfctlError::TerminalBusy)
    ));
    // A second broker request confirms the first read has entered and remains
    // pending, so a mistaken protected read cannot hide its echo change.
    assert!(matches!(
        ui.session
            .broker
            .line(false, &CancellationToken::new())
            .await,
        Err(ConfctlError::TerminalBusy)
    ));
    pty_tests::assert_modes(
        &original,
        &rustix::termios::tcgetattr(&slave).expect("termios"),
    );
    rustix::io::write(&master, b" r \n").expect("choice");
    assert_eq!(line.await.expect("choice"), b" r \n");
    // This raw PTY retains ECHOCTL, which displays newline as ^J. Check the
    // literal answer prefix without imposing another terminal echo policy.
    let mut echo = [0; 3];
    let mut master = std::fs::File::from(master);
    master.read_exact(&mut echo).expect("echoed choice");
    assert_eq!(&echo, b" r ");
    assert_eq!(ui.state(), TerminalState::Live);
    pty_tests::assert_modes(
        &original,
        &rustix::termios::tcgetattr(&slave).expect("termios"),
    );
    ui.shutdown().expect("broker joined");
}

#[tokio::test]
async fn visible_line_requires_successful_park_and_prompt_ack_before_reading() {
    for fail_prompt in [false, true] {
        for missing_ack in [false, true] {
            let (input, mut peer) = UnixStream::pair().expect("input");
            peer.write_all(b"\xffr\n").expect("unread choice");
            let (events, mut receiver) = EventSink::channel(1);
            let ui = Ui::new(&input, events, CancellationToken::new(), None).expect("UI");
            ui.set_live(true).expect("live");
            let mut line = Box::pin(ui.line("Choice: ".into()));
            let mut context = std::task::Context::from_waker(std::task::Waker::noop());
            assert!(line.as_mut().poll(&mut context).is_pending());
            let mut delivery = receiver.receive().await.expect("park event");
            if fail_prompt {
                delivery.acknowledge(Ok(()));
                assert!(line.as_mut().poll(&mut context).is_pending());
                delivery = receiver.receive().await.expect("prompt event");
            }
            assert!(line.as_mut().poll(&mut context).is_pending());
            assert_eq!(ui.state(), TerminalState::Prompt);
            if missing_ack {
                drop(delivery);
            } else {
                delivery.acknowledge(Err("fixture output failure".into()));
            }
            assert!(matches!(line.await, Err(ConfctlError::EventDelivery(_))));
            assert_eq!(ui.state(), TerminalState::Live);
            let mut ready = [rustix::event::PollFd::new(
                &input,
                rustix::event::PollFlags::IN,
            )];
            let zero = rustix::event::Timespec {
                tv_sec: 0,
                tv_nsec: 0,
            };
            assert_eq!(
                rustix::event::poll(&mut ready, Some(&zero)).expect("unread input poll"),
                1
            );
            let mut unread = [0; 3];
            rustix::io::read(&input, &mut unread).expect("input was not consumed");
            assert_eq!(&unread, b"\xffr\n");
            ui.shutdown().expect("broker joined");
        }
    }
}

#[tokio::test]
async fn visible_line_cancel_and_drop_restore_ownership_at_prompt_and_read() {
    for reading in [false, true] {
        for drop_future in [false, true] {
            let (input, mut peer) = UnixStream::pair().expect("input");
            let cancel = CancellationToken::new();
            let (events, mut receiver) = EventSink::channel(1);
            let ui = Ui::new(&input, events, cancel.clone(), None).expect("UI");
            ui.set_live(true).expect("live");
            let mut line = Box::pin(ui.line("Choice: ".into()));
            let mut context = std::task::Context::from_waker(std::task::Waker::noop());
            assert!(line.as_mut().poll(&mut context).is_pending());
            receiver
                .receive()
                .await
                .expect("park event")
                .acknowledge(Ok(()));
            assert!(line.as_mut().poll(&mut context).is_pending());
            let prompt = receiver.receive().await.expect("prompt event");
            if reading {
                prompt.acknowledge(Ok(()));
                assert!(line.as_mut().poll(&mut context).is_pending());
            }
            if drop_future {
                drop(line);
            } else {
                cancel.cancel();
                assert!(matches!(line.await, Err(ConfctlError::Cancelled)));
            }
            assert_eq!(ui.state(), TerminalState::Live);
            peer.write_all(b"next\n").expect("next owner's input");
            assert_eq!(
                ui.session
                    .broker
                    .line(false, &CancellationToken::new())
                    .await
                    .expect("input released"),
                b"next\n"
            );
            ui.shutdown().expect("broker joined");
        }
    }
}

async fn bounded_answer(bytes: Vec<u8>, eof: bool) -> Result<Vec<u8>, ConfctlError> {
    let (input, mut peer) = UnixStream::pair().expect("input");
    let broker = InputBroker::new(&input).expect("broker");
    let producer = thread::spawn(move || {
        peer.write_all(&bytes).expect("answer");
        if eof {
            peer.shutdown(std::net::Shutdown::Write).expect("EOF");
        }
        peer
    });
    let result = tokio::time::timeout(
        std::time::Duration::from_secs(20),
        broker.line(false, &CancellationToken::new()),
    )
    .await
    .expect("bounded input");
    broker.shutdown().expect("join broker");
    drop(input);
    let _ = producer.join().expect("producer");
    result
}

#[tokio::test]
async fn prompt_limit_counts_exact_bytes_with_newline_eof_and_multibyte() {
    for length in [PROMPT_LINE_LIMIT - 1, PROMPT_LINE_LIMIT] {
        let mut newline = vec![b'x'; length - 1];
        newline.push(b'\n');
        assert_eq!(
            bounded_answer(newline, false)
                .await
                .expect("within newline bound")
                .len(),
            length
        );
        let mut eof = "é".repeat(length / 2).into_bytes();
        if eof.len() < length {
            eof.push(b'x');
        }
        assert_eq!(
            bounded_answer(eof, true)
                .await
                .expect("within EOF byte bound")
                .len(),
            length
        );
    }
    let mut over = vec![b'x'; PROMPT_LINE_LIMIT];
    over.push(b'\n');
    assert!(matches!(
        bounded_answer(over, false).await,
        Err(ConfctlError::InputTooLong {
            limit: PROMPT_LINE_LIMIT
        })
    ));
    let mut over = "é".repeat(PROMPT_LINE_LIMIT / 2).into_bytes();
    over.push(b'x');
    assert!(matches!(
        bounded_answer(over, true).await,
        Err(ConfctlError::InputTooLong {
            limit: PROMPT_LINE_LIMIT
        })
    ));
}

#[tokio::test]
async fn overflow_poisons_suffix_prompts_and_foreground_without_waiting_for_newline() {
    let (input, mut peer) = UnixStream::pair().expect("input");
    let (events, mut receiver) = EventSink::channel(2);
    let ui = Ui::new(&input, events, CancellationToken::new(), None).expect("UI");
    let consumer = tokio::spawn(async move {
        while let Some(delivery) = receiver.receive().await {
            delivery.acknowledge(Ok(()));
        }
    });
    let producer = thread::spawn(move || {
        peer.write_all(&vec![b'p'; PROMPT_LINE_LIMIT + 1])
            .expect("overlong answer");
        peer
    });
    let error = tokio::time::timeout(
        std::time::Duration::from_secs(20),
        ui.protected_line("Password: ".into()),
    )
    .await
    .expect("no newline needed")
    .expect_err("overflow");
    assert!(matches!(
        error,
        ConfctlError::InputTooLong {
            limit: PROMPT_LINE_LIMIT
        }
    ));
    assert_eq!(
        error.to_string(),
        format!("prompt input exceeds {PROMPT_LINE_LIMIT} bytes")
    );
    assert_eq!(ui.state(), TerminalState::Idle);
    let mut peer = producer.join().expect("producer");
    peer.write_all(b"y\n").expect("unread suffix");
    assert!(matches!(
        ui.session
            .broker
            .line(false, &CancellationToken::new())
            .await,
        Err(ConfctlError::InputTooLong { .. })
    ));
    assert!(matches!(
        ui.foreground().await,
        Err(ConfctlError::InputTooLong { .. })
    ));
    let mut suffix = [0; 2];
    rustix::io::read(&input, &mut suffix).expect("broker did not consume suffix");
    assert_eq!(&suffix, b"y\n");
    ui.shutdown().expect("join");
    drop(ui);
    consumer.await.expect("consumer");
}

#[tokio::test]
async fn overflow_stops_reading_a_blocked_producer_and_remains_cancelable() {
    let (input, mut peer) = UnixStream::pair().expect("input");
    peer.set_write_timeout(Some(std::time::Duration::from_secs(2)))
        .expect("bounded producer");
    let broker = InputBroker::new(&input).expect("broker");
    let producer = thread::spawn(move || peer.write_all(&vec![b'x'; 8 * PROMPT_LINE_LIMIT]));
    assert!(matches!(
        tokio::time::timeout(
            std::time::Duration::from_secs(20),
            broker.line(false, &CancellationToken::new())
        )
        .await
        .expect("overflow finishes"),
        Err(ConfctlError::InputTooLong { .. })
    ));
    assert!(
        producer.join().expect("producer joined").is_err(),
        "broker must stop reading before producer finishes"
    );
    let cancel = CancellationToken::new();
    cancel.cancel();
    assert!(matches!(
        broker.line(true, &cancel).await,
        Err(ConfctlError::InputTooLong { .. })
    ));
    broker.shutdown().expect("poisoned broker joins");
}

fn local_pty() -> (OwnedFd, OwnedFd) {
    use rustix::pty::{OpenptFlags, grantpt, ioctl_tiocgptpeer, openpt, unlockpt};
    let flags = OpenptFlags::RDWR | OpenptFlags::NOCTTY | OpenptFlags::CLOEXEC;
    let master = openpt(flags).expect("local PTY master");
    grantpt(&master).expect("grant PTY");
    unlockpt(&master).expect("unlock PTY");
    let slave = ioctl_tiocgptpeer(&master, flags).expect("local PTY slave");
    let mut original = rustix::termios::tcgetattr(&slave).expect("termios");
    // A noncanonical test permits a pathological line longer than the kernel's
    // canonical line buffer. ECHO remains on for broker restoration assertions.
    original.make_raw();
    original
        .local_modes
        .insert(rustix::termios::LocalModes::ECHO);
    rustix::termios::tcsetattr(&slave, rustix::termios::OptionalActions::Now, &original)
        .expect("test terminal");
    (master, slave)
}

async fn echo_disabled(slave: &OwnedFd) {
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while rustix::termios::tcgetattr(slave)
            .expect("termios")
            .local_modes
            .contains(rustix::termios::LocalModes::ECHO)
        {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("protected read disables echo");
}

fn assert_echo_restored_and_input_empty(slave: &OwnedFd) {
    assert!(
        rustix::termios::tcgetattr(slave)
            .expect("termios")
            .local_modes
            .contains(rustix::termios::LocalModes::ECHO)
    );
    let mut poll = [rustix::event::PollFd::new(
        slave,
        rustix::event::PollFlags::IN,
    )];
    let zero = rustix::event::Timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    assert_eq!(
        rustix::event::poll(&mut poll, Some(&zero)).expect("input poll"),
        0,
        "protected suffix must be flushed"
    );
}

#[tokio::test]
async fn protected_pty_overflow_restores_echo_flushes_suffix_and_poisons() {
    let (master, slave) = local_pty();
    let broker = Arc::new(InputBroker::new(&slave).expect("broker"));
    let pending_broker = broker.clone();
    let pending =
        tokio::spawn(async move { pending_broker.line(true, &CancellationToken::new()).await });
    echo_disabled(&slave).await;
    let producer = thread::spawn(move || {
        let mut master = std::fs::File::from(master);
        master
            .write_all(&vec![b's'; PROMPT_LINE_LIMIT + 1])
            .expect("protected overflow");
        master
    });
    let result = tokio::time::timeout(std::time::Duration::from_secs(20), pending)
        .await
        .expect("overflow finishes")
        .expect("task");
    assert!(matches!(result, Err(ConfctlError::InputTooLong { .. })));
    let master = producer.join().expect("producer");
    assert_echo_restored_and_input_empty(&slave);
    let mut output = [rustix::event::PollFd::new(
        &master,
        rustix::event::PollFlags::IN,
    )];
    let zero = rustix::event::Timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    assert_eq!(
        rustix::event::poll(&mut output, Some(&zero)).expect("echo poll"),
        0,
        "protected value must not echo"
    );
    assert!(matches!(
        broker.line(false, &CancellationToken::new()).await,
        Err(ConfctlError::InputTooLong { .. })
    ));
    broker.shutdown().expect("joined");
    drop(master);
}

#[tokio::test]
async fn protected_pty_cancel_and_dropped_future_restore_echo_and_flush_input() {
    for drop_future in [false, true] {
        let (master, slave) = local_pty();
        let broker = Arc::new(InputBroker::new(&slave).expect("broker"));
        let cancel = CancellationToken::new();
        let pending_cancel = cancel.clone();
        let pending_broker = broker.clone();
        let pending = tokio::spawn(async move { pending_broker.line(true, &pending_cancel).await });
        echo_disabled(&slave).await;
        // ECHO alone does not acknowledge that the worker installed Pending.
        assert!(matches!(
            tokio::time::timeout(
                std::time::Duration::from_secs(5),
                broker.line(true, &CancellationToken::new()),
            )
            .await
            .expect("protected read installed before cancellation"),
            Err(ConfctlError::TerminalBusy)
        ));
        rustix::io::write(&master, b"partial-secret").expect("partial input");
        if drop_future {
            pending.abort();
            assert!(pending.await.expect_err("dropped task").is_cancelled());
        } else {
            cancel.cancel();
            assert!(matches!(
                pending.await.expect("task"),
                Err(ConfctlError::Cancelled)
            ));
        }
        assert_echo_restored_and_input_empty(&slave);
        broker.shutdown().expect("joined");
    }
}

fn restoration_fixture_error() -> ConfctlError {
    ConfctlError::Cancelled.with_secondary(vec![
        ConfctlError::Io {
            operation: "flush protected input",
            path: None,
            source: io::Error::other("fixture flush failed"),
        },
        ConfctlError::Io {
            operation: "restore terminal input",
            path: None,
            source: io::Error::other("fixture restore failed"),
        },
    ])
}

fn assert_restoration_order(error: &ConfctlError) {
    let ConfctlError::Cleanup { primary, secondary } = error else {
        panic!("missing restoration errors: {error:?}");
    };
    assert!(matches!(
        primary.as_ref(),
        ConfctlError::Cancelled
            | ConfctlError::InputEof
            | ConfctlError::Io {
                operation: "read terminal input",
                ..
            }
    ));
    assert!(matches!(
        secondary.as_slice(),
        [
            ConfctlError::Io {
                operation: "flush protected input",
                ..
            },
            ConfctlError::Io {
                operation: "restore terminal input",
                ..
            }
        ]
    ));
}

#[tokio::test]
async fn discarded_input_errors_survive_failed_send_and_queued_receiver_drop() {
    for before_send in [false, true] {
        let (input, _peer) = UnixStream::pair().expect("input");
        let broker = InputBroker::new(&input).expect("broker");
        let (send, receive) = oneshot::channel();
        if before_send {
            drop(receive);
            reply(send, Err(restoration_fixture_error()), &broker.unobserved);
        } else {
            reply(send, Err(restoration_fixture_error()), &broker.unobserved);
            assert!(broker.unobserved.lock().expect("sink").is_empty());
            drop(receive);
        }
        assert_eq!(broker.unobserved.lock().expect("sink").len(), 1);
        assert_restoration_order(&broker.shutdown().expect_err("discarded restoration error"));
        broker.shutdown().expect("transferred once");
    }
}

#[tokio::test]
async fn consumed_input_error_and_clean_canceled_reply_are_not_reported_again() {
    let (input, _peer) = UnixStream::pair().expect("input");
    let broker = InputBroker::new(&input).expect("broker");
    let (send, receive) = oneshot::channel();
    reply(send, Err(restoration_fixture_error()), &broker.unobserved);
    assert_restoration_order(
        &receive
            .await
            .expect("reply")
            .consume()
            .expect_err("consumed failure"),
    );
    let (send, receive) = oneshot::channel();
    reply(send, Err(ConfctlError::Cancelled), &broker.unobserved);
    drop(receive);
    broker
        .shutdown()
        .expect("no duplicate or synthetic cancel failure");
}

#[tokio::test]
async fn protected_pty_hangup_then_queued_reply_drop_retains_native_cleanup_failures() {
    let (master, slave) = local_pty();
    let broker = InputBroker::new(&slave).expect("broker");
    let original = rustix::termios::tcgetattr(&slave).expect("original termios");
    let mut protected = original.clone();
    protected
        .local_modes
        .remove(rustix::termios::LocalModes::ECHO);
    rustix::termios::tcsetattr(&slave, rustix::termios::OptionalActions::Now, &protected)
        .expect("protected termios installed before hangup");
    assert!(
        !rustix::termios::tcgetattr(&slave)
            .expect("protected termios")
            .local_modes
            .contains(rustix::termios::LocalModes::ECHO)
    );
    let (send, receive) = oneshot::channel();
    let pending = Pending {
        id: 1,
        bytes: b"partial-secret".to_vec(),
        response: send,
        original: Some(original),
        unobserved: broker.unobserved.clone(),
    };
    drop(master);
    // The idle worker has no Line request to complete before this precondition.
    // A parallel fork may retain the CLOEXEC master until exec.
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        loop {
            match rustix::termios::tcgetattr(&slave) {
                Err(rustix::io::Errno::IO) => break,
                Ok(_) => tokio::task::yield_now().await,
                Err(error) => panic!("observe PTY hangup: {error}"),
            }
        }
    })
    .await
    .expect("PTY hangup before native cleanup");
    complete(&slave, pending, Err(ConfctlError::Cancelled));
    assert!(broker.unobserved.lock().expect("sink").is_empty());
    // The successfully queued reply owns the real flush/restore failures until
    // its unconsumed receiver is dropped.
    drop(receive);
    assert_eq!(broker.unobserved.lock().expect("sink").len(), 1);
    let error = broker
        .shutdown()
        .expect_err("hung-up terminal restore failed");
    let ConfctlError::Cleanup { primary, secondary } = &error else {
        panic!("missing native cleanup errors: {error:?}");
    };
    assert!(matches!(primary.as_ref(), ConfctlError::Cancelled));
    assert!(matches!(
        secondary.as_slice(),
        [
            ConfctlError::Io {
                operation: "flush protected input",
                source: flush,
                ..
            },
            ConfctlError::Io {
                operation: "restore terminal input",
                source: restore,
                ..
            }
        ] if flush.raw_os_error() == Some(rustix::io::Errno::IO.raw_os_error())
            && restore.raw_os_error() == Some(rustix::io::Errno::IO.raw_os_error())
    ));
    assert!(!format!("{error:?}").contains("partial-secret"));
    assert!(broker.worker.lock().expect("worker").is_none());
    assert!(broker.unobserved.lock().expect("sink").is_empty());
    broker.shutdown().expect("failure transferred once");
}

#[test]
fn successful_read_with_failed_restore_uses_cleanup_error_as_primary() {
    let (master, slave) = local_pty();
    let original = rustix::termios::tcgetattr(&slave).expect("termios");
    let retained = Arc::new(Mutex::new(Vec::new()));
    let (send, mut receive) = oneshot::channel();
    drop(master);
    complete(
        &slave,
        Pending {
            id: 1,
            bytes: Vec::new(),
            response: send,
            original: Some(original),
            unobserved: retained.clone(),
        },
        Ok(b"private-value".to_vec()),
    );
    let error = receive
        .try_recv()
        .expect("reply")
        .consume()
        .expect_err("restore failed");
    assert!(matches!(
        error,
        ConfctlError::Io {
            operation: "restore terminal input",
            ..
        }
    ));
    assert!(!format!("{error:?}").contains("private-value"));
    assert!(retained.lock().expect("sink").is_empty());
}

#[test]
fn shutdown_keeps_retained_primary_and_wake_join_failures_in_order() {
    let (requests, receive) = mpsc::channel();
    drop(receive);
    let (wake, peer) = UnixStream::pair().expect("wake socket");
    drop(peer);
    let broker = InputBroker {
        requests,
        wake: Mutex::new(wake),
        worker: Mutex::new(Some(thread::spawn(|| panic!("fixture broker panic")))),
        next: AtomicU64::new(0),
        poisoned: Arc::new(AtomicBool::new(false)),
        unobserved: Arc::new(Mutex::new(vec![ConfctlError::InputTooLong {
            limit: PROMPT_LINE_LIMIT,
        }])),
    };
    let error = broker.shutdown().expect_err("all errors preserved");
    let ConfctlError::Cleanup { primary, secondary } = error else {
        panic!("cleanup wrapper");
    };
    assert!(matches!(
        primary.as_ref(),
        ConfctlError::InputTooLong { .. }
    ));
    assert_eq!(secondary.len(), 2);
    assert!(secondary[0].to_string().contains("input broker stopped"));
    assert!(secondary[1].to_string().contains("input broker panicked"));
    broker.shutdown().expect("no duplicate failures");
}

#[tokio::test]
async fn pager_rejects_noncontrolling_output_without_replacing_prompt_input() {
    let (input, mut peer) = UnixStream::pair().unwrap();
    let (events, _receiver) = EventSink::channel(1);
    let ui = Ui::new(&input, events, CancellationToken::new(), None).unwrap();
    let (_master, slave) = pty_tests::open_pty();
    assert!(ui.pager_foreground(slave).await.is_err());
    assert_eq!(ui.state(), TerminalState::Idle);
    peer.write_all(b"untouched\n").unwrap();
    assert_eq!(
        ui.session
            .broker
            .line(false, &CancellationToken::new())
            .await
            .unwrap(),
        b"untouched\n"
    );
    ui.shutdown().unwrap();
}
