use super::*;
use crate::events::{EventSink, Style, TaskId};
use std::{
    future::Future,
    pin::Pin,
    task::{Context, Poll, Waker},
};
use tokio_util::sync::CancellationToken;

type Recorded = Arc<Mutex<Vec<u8>>>;
struct TestWriter {
    bytes: Recorded,
    remaining: Option<usize>,
    flush_fault: bool,
}
impl Write for TestWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        let count = self
            .remaining
            .map_or(bytes.len(), |left| left.min(bytes.len()));
        if count == 0 {
            return Err(io::Error::other("injected output write fault"));
        }
        self.bytes
            .lock()
            .expect("recorded bytes")
            .extend_from_slice(&bytes[..count]);
        if let Some(left) = &mut self.remaining {
            *left -= count;
        }
        Ok(count)
    }
    fn flush(&mut self) -> io::Result<()> {
        if self.flush_fault {
            Err(io::Error::other("injected output flush fault"))
        } else {
            Ok(())
        }
    }
}
fn writer(remaining: Option<usize>, flush_fault: bool) -> (OutputWriter, Recorded) {
    let bytes = Arc::new(Mutex::new(Vec::new()));
    (
        Arc::new(Mutex::new(Box::new(TestWriter {
            bytes: bytes.clone(),
            remaining,
            flush_fault,
        }))),
        bytes,
    )
}
fn config(directory: &Path, live: bool) -> PresentationConfig {
    PresentationConfig {
        command: "build node".into(),
        operation: "build".into(),
        spool_dir: directory.into(),
        policy: OutputPolicy {
            live,
            stdout_color: false,
            stderr_color: false,
        },
        initial_size: (80, 24),
        terminal: None,
        max_tasks: 5,
        tail_rows: 12,
        tail_line_bytes: 100,
    }
}
fn output(stream: OutputStream, bytes: &[u8]) -> UiEvent {
    UiEvent::Output {
        task: TaskId(1),
        stream,
        bytes: bytes.to_vec(),
    }
}
fn bytes(capture: &Captured) -> Vec<u8> {
    match capture {
        Captured::Memory(bytes) => bytes.clone(),
        Captured::Spool { file, length } => {
            let bytes = std::fs::read(file.path()).expect("capture");
            assert!(bytes.len() as u64 >= *length);
            bytes[..*length as usize].to_vec()
        }
        Captured::Unavailable => Vec::new(),
    }
}
fn pending(error: ConfctlError) -> (ConfctlError, PendingOutput) {
    if let ConfctlError::OutputFailure { source, pending } = error {
        (*source, *pending)
    } else {
        panic!("expected retained output error: {error:?}");
    }
}

#[test]
fn mode_and_color_policy_are_independent_for_each_original_stream() {
    for (stdout, stderr) in [(true, true), (false, true), (true, false), (false, false)] {
        for usable in [true, false] {
            for tty_enabled in [true, false] {
                for no_color in [true, false] {
                    for color in [ColorMode::Always, ColorMode::Never, ColorMode::Auto] {
                        let policy = OutputPolicy::resolve(
                            color,
                            stdout,
                            stderr,
                            usable,
                            tty_enabled,
                            no_color,
                        );
                        assert_eq!(policy.live, stdout && stderr && usable && tty_enabled);
                        for (tty, selected) in
                            [(stdout, policy.stdout_color), (stderr, policy.stderr_color)]
                        {
                            assert_eq!(
                                selected,
                                match color {
                                    ColorMode::Always => true,
                                    ColorMode::Never => false,
                                    ColorMode::Auto => tty && !no_color,
                                }
                            );
                        }
                    }
                }
            }
        }
    }
}

#[test]
fn zero_and_partial_append_failure_keep_prefix_current_chunk_and_overlap_offset() {
    for partial in [0, 2] {
        let directory = tempfile::tempdir().expect("spool directory");
        let mut stream = DeferredStream::new(directory.path()).expect("spool");
        stream.append(b"acknowledged").expect("earlier ownership");
        let spool_offset = stream.length;
        let current = b"secret-current-chunk".to_vec();
        let mut first = true;
        let result = stream.append_using(&current, |file, bytes| {
            if first && partial > 0 {
                first = false;
                file.write(&bytes[..partial])
            } else {
                Err(io::Error::other("injected append fault"))
            }
        });
        assert!(result.is_err());
        let evidence = PendingOutput {
            stdout: stream.capture(),
            stderr: Captured::Unavailable,
            stdout_replayed: 0,
            stderr_replayed: 0,
            verified_log: None,
            unspooled: Some(UnspooledOutput {
                stream: OutputStream::Stdout,
                bytes: Captured::Memory(current),
                spool_offset,
            }),
        };
        assert_eq!(
            bytes(&evidence.stdout),
            [&b"acknowledged"[..], &b"secret-current-chunk"[..partial]].concat()
        );
        let chunk = evidence.unspooled.as_ref().expect("whole current chunk");
        assert_eq!(bytes(&chunk.bytes), b"secret-current-chunk");
        assert_eq!(chunk.spool_offset, 12);
        assert_eq!(evidence.stdout.len(), 12 + partial as u64);
        assert!(!format!("{evidence:?}").contains("secret-current-chunk"));
        let path = evidence.stdout.spool_path().expect("owned path").to_owned();
        assert!(path.exists());
        drop(evidence);
        assert!(!path.exists(), "unretained evidence cleans up by ownership");
    }
}

#[test]
fn partial_replay_and_flush_failure_retain_full_spool_and_confirmed_offsets() {
    for flush_fault in [false, true] {
        let directory = tempfile::tempdir().expect("directory");
        let mut stream = DeferredStream::new(directory.path()).expect("spool");
        stream.append(b"all-required-bytes").expect("owned bytes");
        let (writer, written) = writer(if flush_fault { None } else { Some(4) }, flush_fault);
        assert!(stream.replay(&writer).is_err());
        assert_eq!(stream.replayed, if flush_fault { 18 } else { 4 });
        assert_eq!(stream.length, 18);
        assert_eq!(
            written.lock().expect("output").len() as u64,
            stream.replayed
        );
        let capture = stream.capture();
        assert_eq!(bytes(&capture), b"all-required-bytes");
    }
    let directory = tempfile::tempdir().expect("directory");
    let mut stream = DeferredStream::new(directory.path()).expect("spool");
    let path = stream.file.as_ref().expect("file").path().to_owned();
    stream.append(b"delivered").expect("append");
    let (writer, recorded) = writer(None, false);
    stream.replay(&writer).expect("delivery and flush");
    assert_eq!(*recorded.lock().expect("output"), b"delivered");
    assert_eq!(stream.length, 0);
    assert_eq!(std::fs::metadata(&path).expect("truncated spool").len(), 0);
    drop(stream);
    assert!(!path.exists());
}

#[tokio::test]
async fn plain_streams_are_exact_and_finish_ignores_retained_sender_clones() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, out) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    sink.emit(output(OutputStream::Stdout, b"raw\x00\xff\x1b[31m\n"))
        .await
        .expect("stdout");
    sink.emit(output(OutputStream::Stderr, b"error\r\n"))
        .await
        .expect("stderr");
    control
        .finish()
        .await
        .expect("finish while sink still retained");
    running.await.expect("join").expect("run");
    assert_eq!(*out.lock().expect("stdout"), b"raw\x00\xff\x1b[31m\n");
    assert_eq!(*err.lock().expect("stderr"), b"error\r\n");
    assert!(
        sink.emit(output(OutputStream::Stdout, b"late"))
            .await
            .is_err()
    );
    control.finish().await.expect("repeated successful finish");
    assert_eq!(
        std::fs::read_dir(directory.path())
            .expect("directory")
            .count(),
        0
    );
}

#[tokio::test]
async fn parked_background_is_spooled_prompt_bypasses_and_original_stream_order_replays() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, out) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (state, observe) = watch::channel(TerminalState::Idle);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    state.send_replace(TerminalState::Prompt);
    sink.emit(UiEvent::TerminalState(TerminalState::Prompt))
        .await
        .expect("park barrier");
    let producer = sink.clone();
    let background = tokio::spawn(async move {
        for _ in 0..32 {
            producer
                .emit(output(OutputStream::Stdout, &vec![b'o'; 16 * 1024]))
                .await
                .expect("owned stdout");
            producer
                .emit(output(OutputStream::Stderr, &vec![b'e'; 16 * 1024]))
                .await
                .expect("owned stderr");
        }
    });
    sink.emit(UiEvent::PromptFrame {
        preview: vec![UiEvent::Diagnostic {
            stream: OutputStream::Stdout,
            style: Style::Plain,
            text: "preview\n".into(),
        }],
        text: "Answer: ".into(),
    })
    .await
    .expect("prompt bypasses background");
    background
        .await
        .expect("background is not blocked by prompt input");
    assert_eq!(*out.lock().expect("stdout"), b"preview\nAnswer: ");
    assert!(err.lock().expect("stderr").is_empty());
    state.send_replace(TerminalState::Idle);
    sink.emit(UiEvent::TerminalState(TerminalState::Idle))
        .await
        .expect("resume and replay");
    control.finish().await.expect("finish");
    running.await.expect("join").expect("run");
    assert_eq!(
        out.lock().expect("stdout")[16..],
        vec![b'o'; 32 * 16 * 1024]
    );
    assert_eq!(*err.lock().expect("stderr"), vec![b'e'; 32 * 16 * 1024]);
}

#[tokio::test]
async fn external_child_ownership_defers_all_confctl_output_until_restored() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, out) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (state, observe) = watch::channel(TerminalState::Idle);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    state.send_replace(TerminalState::ExternalChild);
    sink.emit(UiEvent::TerminalState(TerminalState::ExternalChild))
        .await
        .expect("handoff barrier");
    sink.emit(output(OutputStream::Stdout, b"after-child-out"))
        .await
        .expect("deferred stdout");
    sink.emit(output(OutputStream::Stderr, b"after-child-err"))
        .await
        .expect("deferred stderr");
    assert!(out.lock().expect("stdout").is_empty());
    assert!(err.lock().expect("stderr").is_empty());
    state.send_replace(TerminalState::Idle);
    sink.emit(UiEvent::TerminalState(TerminalState::Idle))
        .await
        .expect("restored");
    control.finish().await.expect("finish");
    running.await.expect("join").expect("run");
    assert_eq!(*out.lock().expect("stdout"), b"after-child-out");
    assert_eq!(*err.lock().expect("stderr"), b"after-child-err");
}

#[tokio::test]
async fn dropped_run_future_retains_parked_bytes_until_checked_finish() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, _) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (state, observe) = watch::channel(TerminalState::Prompt);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    sink.emit(output(OutputStream::Stdout, b"secret-pending-output"))
        .await
        .expect("spool owns bytes");
    running.abort();
    assert!(running.await.expect_err("aborted").is_cancelled());
    let (_, evidence) = pending(control.finish().await.expect_err("checked aborted loop"));
    assert_eq!(bytes(&evidence.stdout), b"secret-pending-output");
    assert!(!format!("{evidence:?}").contains("secret-pending-output"));
    assert!(
        matches!(control.finish().await, Err(ConfctlError::EventDelivery(_))),
        "failure stays sticky after transfer"
    );
    drop(state);
}

#[tokio::test]
async fn dropped_run_error_and_finish_waiter_do_not_discard_owned_failure() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, _) = writer(Some(3), false);
    let (stderr, _) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    assert!(
        sink.emit(output(OutputStream::Stdout, b"secret-evidence"))
            .await
            .is_err()
    );
    drop(
        running
            .await
            .expect("join")
            .expect_err("failure notification"),
    );
    let (source, evidence) = pending(control.finish().await.expect_err("actual source"));
    assert!(source.to_string().contains("replay deferred stdout"));
    assert_eq!(bytes(&evidence.stdout), b"secret-evidence");
    assert_eq!(evidence.stdout_replayed, 3);
    assert!(evidence.unspooled.is_none());

    let (stdout, _) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (_sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Prompt);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let mut finish = Box::pin(control.finish());
    let mut context = Context::from_waker(Waker::noop());
    assert!(matches!(
        Pin::as_mut(&mut finish).poll(&mut context),
        Poll::Pending
    ));
    drop(finish);
    assert!(
        tokio::spawn(presentation.run())
            .await
            .expect("join")
            .is_err()
    );
    let (source, _) = pending(
        control
            .finish()
            .await
            .expect_err("dropped waiter did not take source"),
    );
    assert!(matches!(source, ConfctlError::TerminalBusy));
}

#[tokio::test]
async fn concurrent_finish_transfers_evidence_once_and_preserves_source_cleanup_order() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, _) = writer(Some(0), false);
    let (stderr, _) = writer(Some(0), false);
    let (sink, receiver) = EventSink::channel(1);
    let (state, observe) = watch::channel(TerminalState::Prompt);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    sink.emit(output(OutputStream::Stdout, b"first fault"))
        .await
        .expect("owned stdout");
    sink.emit(output(OutputStream::Stderr, b"second fault"))
        .await
        .expect("owned stderr");
    state.send_replace(TerminalState::Idle);
    assert!(running.await.expect("join").is_err());
    let (first, second) = tokio::join!(control.finish(), control.finish());
    let errors = [
        first.expect_err("sticky failure"),
        second.expect_err("sticky failure"),
    ];
    assert_eq!(
        errors
            .iter()
            .filter(|error| matches!(error, ConfctlError::OutputFailure { .. }))
            .count(),
        1
    );
    let retained = errors
        .into_iter()
        .find(|error| matches!(error, ConfctlError::OutputFailure { .. }))
        .expect("sole retained evidence");
    let (source, evidence) = pending(retained);
    let ConfctlError::Cleanup { primary, secondary } = source else {
        panic!("both output errors retained");
    };
    assert_eq!(
        primary.to_string(),
        "replay deferred stdout: injected output write fault"
    );
    assert_eq!(secondary.len(), 1);
    assert_eq!(
        secondary[0].to_string(),
        "replay deferred stderr: injected output write fault"
    );
    assert_eq!(bytes(&evidence.stdout), b"first fault");
    assert_eq!(bytes(&evidence.stderr), b"second fault");
}

#[tokio::test]
async fn group_prelude_is_once_and_duplicate_frames_are_validated_before_ignored() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, out) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    for _ in 0..2 {
        sink.emit(UiEvent::OperationGroup {
            group: 1,
            description: "build".into(),
            prelude: vec![UiEvent::Diagnostic {
                stream: OutputStream::Stdout,
                style: Style::Plain,
                text: "Machines: node\n".into(),
            }],
        })
        .await
        .expect("group");
    }
    assert_eq!(*out.lock().expect("stdout"), b"Machines: node\n");
    assert!(
        sink.emit(UiEvent::OperationGroup {
            group: 1,
            description: "build".into(),
            prelude: vec![output(OutputStream::Stdout, b"not a prelude event")]
        })
        .await
        .is_err()
    );
    assert!(running.await.expect("join").is_err());
    assert!(control.finish().await.is_err());
}

#[tokio::test]
async fn live_stream_sanitization_is_incremental_and_final_panel_does_not_replay_log() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, out) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), true),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    for chunk in b"\x1b]52;c;secret\x07\x1b[31mred\x1b[0m \xe7\x95\x8c\n".chunks(2) {
        sink.emit(output(OutputStream::Stderr, chunk))
            .await
            .expect("live output");
    }
    control.finish().await.expect("finish");
    running.await.expect("join").expect("run");
    assert_eq!(*err.lock().expect("stderr"), "red 界\n".as_bytes());
    let rendered = out.lock().expect("stdout");
    assert!(!rendered.windows(4).any(|bytes| bytes == b"\x1b[6n"));
    assert!(!rendered.windows(6).any(|bytes| bytes == b"secret"));
    assert!(!rendered.windows(8).any(|bytes| bytes == b"\x1b[?1049h"));
}

#[tokio::test]
async fn prompt_frame_requires_lease_and_cancelled_prompt_releases_deferred_output() {
    let directory = tempfile::tempdir().expect("directory");
    let (input, _peer) = std::os::unix::net::UnixStream::pair().expect("input");
    let (stdout, out) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let cancellation = CancellationToken::new();
    let ui = crate::terminal::Ui::new(&input, sink.clone(), cancellation.clone(), None)
        .expect("input owner");
    let (control, presentation) = Presentation::new(
        receiver,
        ui.observe_state(),
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    let prompt_ui = ui.clone();
    let prompt = tokio::spawn(async move { prompt_ui.protected_line("Password: ".into()).await });
    while !out.lock().expect("stdout").ends_with(b"Password: ") {
        tokio::task::yield_now().await;
    }
    sink.emit(output(OutputStream::Stdout, b"deferred\n"))
        .await
        .expect("background");
    cancellation.cancel();
    assert!(matches!(
        prompt.await.expect("prompt join"),
        Err(ConfctlError::Cancelled)
    ));
    ui.shutdown().expect("broker cleanup");
    control.finish().await.expect("finish after input cleanup");
    running.await.expect("join").expect("run");
    assert_eq!(*out.lock().expect("stdout"), b"Password: deferred\n");
}

#[tokio::test]
async fn typed_table_cells_follow_color_policy_without_coloring_headers_or_padding() {
    for (color, tty, no_color, colored) in [
        (ColorMode::Always, false, true, true),
        (ColorMode::Never, true, false, false),
        (ColorMode::Auto, true, false, true),
        (ColorMode::Auto, true, true, false),
        (ColorMode::Auto, false, false, false),
    ] {
        let directory = tempfile::tempdir().expect("directory");
        let (stdout, out) = writer(None, false);
        let (stderr, _) = writer(None, false);
        let (sink, receiver) = EventSink::channel(1);
        let (_state, observe) = watch::channel(TerminalState::Idle);
        let mut config = config(directory.path(), false);
        config.policy = OutputPolicy::resolve(color, tty, tty, false, true, no_color);
        let (control, presentation) = Presentation::new(
            receiver,
            observe,
            OutputTargets { stdout, stderr },
            config,
            None,
        )
        .expect("presentation");
        let running = tokio::spawn(presentation.run());
        sink.emit(UiEvent::Table {
            stream: OutputStream::Stdout,
            columns: vec!["N".into()],
            rows: vec![vec!["x".into()]],
            cell_styles: vec![vec![Style::Success]],
            format: crate::events::TableFormat {
                layout: crate::events::TableLayout::Columns,
                ..Default::default()
            },
        })
        .await
        .expect("typed table");
        control.finish().await.expect("finish");
        running.await.expect("join").expect("run");
        assert_eq!(
            *out.lock().expect("stdout"),
            if colored {
                b"N\n\x1b[32mx\x1b[0m\n".to_vec()
            } else {
                b"N\nx\n".to_vec()
            }
        );
    }
}

#[tokio::test]
async fn frame_style_shape_is_checked_before_any_prelude_output_even_for_duplicate_group() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, out) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    sink.emit(UiEvent::OperationGroup {
        group: 1,
        description: "group".into(),
        prelude: Vec::new(),
    })
    .await
    .expect("initial empty group");
    assert!(
        sink.emit(UiEvent::OperationGroup {
            group: 1,
            description: "group".into(),
            prelude: vec![
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: "must not be printed".into()
                },
                UiEvent::Table {
                    stream: OutputStream::Stdout,
                    columns: vec!["N".into()],
                    rows: vec![vec!["x".into()]],
                    cell_styles: vec![vec![Style::Plain, Style::Failure]],
                    format: Default::default()
                }
            ]
        })
        .await
        .is_err()
    );
    assert!(running.await.expect("join").is_err());
    assert!(control.finish().await.is_err());
    assert!(out.lock().expect("stdout").is_empty());
}
