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
        command: "confctl build -t api".into(),
        operation: "build".into(),
        spool_dir: directory.into(),
        configuration_dir: None,
        invocation_log: None,
        mode: if live {
            PresentationMode::Full
        } else {
            PresentationMode::Plain
        },
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
                        for requested_full in [false, true] {
                            assert_eq!(
                                PresentationMode::resolve(
                                    policy.live,
                                    stderr && usable && tty_enabled,
                                    requested_full
                                ),
                                if requested_full && policy.live {
                                    PresentationMode::Full
                                } else if stderr && usable && tty_enabled {
                                    PresentationMode::CommandSpinner
                                } else {
                                    PresentationMode::Plain
                                },
                            );
                        }
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
async fn plain_compatibility_bytes_preserve_stdout_and_native_stderr_exactly() {
    let directory = tempfile::tempdir().unwrap();
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
    .unwrap();
    let running = tokio::spawn(presentation.run());
    let compatibility = b"[alpha] [1/?] copy 1/?; copying path\n";
    let native = b"native \0\xff\r\n";
    sink.emit(UiEvent::ActivityOutput {
        task: TaskId(1),
        stream: OutputStream::Stderr,
        bytes: native.to_vec(),
    })
    .await
    .unwrap();
    sink.emit(UiEvent::PlainOutput {
        stream: OutputStream::Stdout,
        bytes: compatibility.to_vec(),
    })
    .await
    .unwrap();
    sink.emit(UiEvent::Diagnostic {
        stream: OutputStream::Stdout,
        style: Style::Plain,
        text: "ordinary diagnostic\n".into(),
    })
    .await
    .unwrap();
    control.finish().await.unwrap();
    running.await.unwrap().unwrap();
    assert_eq!(
        &*out.lock().unwrap(),
        &[compatibility.as_slice(), b"ordinary diagnostic\n"].concat()
    );
    assert_eq!(&*err.lock().unwrap(), native);
}

#[tokio::test]
async fn live_plain_output_has_no_effect_but_ordinary_diagnostics_remain_required() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, out) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let (control, mut presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), true),
        None,
    )
    .unwrap();
    for stream in [OutputStream::Stdout, OutputStream::Stderr] {
        presentation
            .event(UiEvent::PlainOutput {
                stream,
                bytes: b"plain-only \x1b[31m\xff\n".to_vec(),
            })
            .unwrap();
    }
    assert!(presentation.model.rows().is_empty());
    assert!(presentation.model.logs().is_empty());
    assert!(presentation.model.display_tail().is_empty());
    assert_eq!(
        (presentation.stdout.length, presentation.stderr.length),
        (0, 0)
    );
    assert!(!presentation.active_view);
    assert!(presentation.last_paint.is_none());
    assert!(presentation.last_geometry.is_none());
    assert!(out.lock().unwrap().is_empty());
    assert!(err.lock().unwrap().is_empty());
    let running = tokio::spawn(presentation.run());
    sink.emit(UiEvent::PlainOutput {
        stream: OutputStream::Stdout,
        bytes: b"acknowledged but hidden\n".to_vec(),
    })
    .await
    .unwrap();
    sink.emit(UiEvent::Diagnostic {
        stream: OutputStream::Stderr,
        style: Style::Plain,
        text: "required live diagnostic\n".into(),
    })
    .await
    .unwrap();
    control.finish().await.unwrap();
    running.await.unwrap().unwrap();
    assert!(out.lock().unwrap().is_empty());
    assert_eq!(&*err.lock().unwrap(), b"required live diagnostic\n");
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
    sink.emit(UiEvent::PlainOutput {
        stream: OutputStream::Stdout,
        bytes: b"first fault".to_vec(),
    })
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
async fn checkpoints_publish_each_unit_once_without_finalizing_other_work() {
    for live in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let (stdout, out) = writer(None, false);
        let (stderr, _) = writer(None, false);
        let (_sink, receiver) = EventSink::channel(1);
        let (_state, observe) = watch::channel(TerminalState::Live);
        let (_control, mut presentation) = Presentation::new(
            receiver,
            observe,
            OutputTargets { stdout, stderr },
            config(directory.path(), live),
            None,
        )
        .unwrap();
        for id in [1, 2] {
            presentation
                .event(UiEvent::TaskStarted {
                    task: TaskId(id),
                    description: format!("unit {id}"),
                })
                .unwrap();
        }
        presentation
            .event(UiEvent::ActivityOutput {
                task: TaskId(1),
                stream: OutputStream::Stderr,
                bytes: b"first unit native output\n".to_vec(),
            })
            .unwrap();
        presentation
            .event(UiEvent::ActivityOutput {
                task: TaskId(2),
                stream: OutputStream::Stderr,
                bytes: b"other unfinished fragment".to_vec(),
            })
            .unwrap();
        presentation
            .event(UiEvent::TaskFinished {
                task: TaskId(1),
                state: crate::events::TaskState::Succeeded,
            })
            .unwrap();
        let before = out.lock().unwrap().len();
        presentation
            .event(UiEvent::Checkpoint {
                key: TaskId(1),
                label: "Published group 1/2".into(),
                state: crate::events::TaskState::Succeeded,
                tasks: vec![TaskId(1)],
            })
            .unwrap();
        if live {
            let output = out.lock().unwrap();
            assert_eq!(
                String::from_utf8_lossy(&output[before..])
                    .matches("Published group 1/2: succeeded\n")
                    .count(),
                1
            );
            assert!(
                !String::from_utf8_lossy(&output[before..]).contains("first unit native output")
            );
            assert!(!presentation.model.finalized());
            assert_eq!(
                presentation.model.rows()[1].phase,
                model::TaskPhase::Executing
            );
            assert_eq!(
                presentation.model.current_tail_line(),
                "other unfinished fragment"
            );
        } else {
            assert_eq!(out.lock().unwrap().len(), before);
        }
        let after_first = out.lock().unwrap().len();
        presentation
            .event(UiEvent::Checkpoint {
                key: TaskId(1),
                label: "Published group 1/2".into(),
                state: crate::events::TaskState::Succeeded,
                tasks: vec![TaskId(1)],
            })
            .unwrap();
        assert_eq!(out.lock().unwrap().len(), after_first);
        presentation
            .event(UiEvent::TaskFinished {
                task: TaskId(2),
                state: crate::events::TaskState::Failed,
            })
            .unwrap();
        presentation
            .event(UiEvent::Checkpoint {
                key: TaskId(2),
                label: "Machine 2 (health failed)".into(),
                state: crate::events::TaskState::Failed,
                tasks: vec![TaskId(2)],
            })
            .unwrap();
        presentation.finalize_group().unwrap();
        let bytes = out.lock().unwrap();
        if live {
            let text = String::from_utf8_lossy(&bytes);
            assert_eq!(text.matches("Published group 1/2: succeeded\n").count(), 1);
            assert_eq!(
                text.matches("Machine 2 (health failed): failed\n").count(),
                1
            );
            assert!(presentation.model.visible_rows(10).is_empty());
            assert_eq!(text.matches("first unit native output").count(), 0);
        } else {
            assert!(bytes.is_empty());
        }
    }
}

#[tokio::test]
async fn parked_checkpoint_acknowledges_owned_bytes_and_replays_before_the_next_panel() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, out) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (state, observe) = watch::channel(TerminalState::Prompt);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), true),
        None,
    )
    .unwrap();
    let running = tokio::spawn(presentation.run());
    sink.emit(UiEvent::TaskStarted {
        task: TaskId(1),
        description: "published build".into(),
    })
    .await
    .unwrap();
    sink.emit(UiEvent::TaskFinished {
        task: TaskId(1),
        state: crate::events::TaskState::Succeeded,
    })
    .await
    .unwrap();
    sink.emit(UiEvent::Checkpoint {
        key: TaskId(1),
        label: "Published unit".into(),
        state: crate::events::TaskState::Succeeded,
        tasks: vec![TaskId(1)],
    })
    .await
    .unwrap();
    assert!(out.lock().unwrap().is_empty());
    state.send_replace(TerminalState::Live);
    sink.emit(UiEvent::TerminalState(TerminalState::Live))
        .await
        .unwrap();
    control.finish().await.unwrap();
    running.await.unwrap().unwrap();
    let output = out.lock().unwrap();
    let text = String::from_utf8_lossy(&output);
    assert!(text.starts_with("Published unit: succeeded\npublished build: succeeded 0\n"));
    assert_eq!(text.matches("Published unit: succeeded").count(), 1);
    assert!(
        !text.contains("Command: confctl"),
        "a fully snapshotted unit must not reopen an empty panel"
    );
}

#[tokio::test]
async fn checkpoint_write_failure_retains_exact_snapshot_without_marking_it_delivered() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, _) = writer(Some(0), false);
    let (stderr, _) = writer(None, false);
    let (_sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let (_control, mut presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), true),
        None,
    )
    .unwrap();
    presentation
        .event(UiEvent::TaskStarted {
            task: TaskId(1),
            description: "published build".into(),
        })
        .unwrap();
    presentation
        .event(UiEvent::TaskFinished {
            task: TaskId(1),
            state: crate::events::TaskState::Succeeded,
        })
        .unwrap();
    assert!(
        presentation
            .event(UiEvent::Checkpoint {
                key: TaskId(1),
                label: "Published unit".into(),
                state: crate::events::TaskState::Succeeded,
                tasks: vec![TaskId(1)]
            })
            .is_err()
    );
    assert!(!presentation.model.was_snapshotted(TaskId(1)));
    assert!(!presentation.seen_checkpoints.contains(&TaskId(1)));
    let pending = presentation.pending();
    assert_eq!(pending.stdout_replayed, 0);
    assert_eq!(
        bytes(&pending.stdout),
        b"Published unit: succeeded\npublished build: succeeded 0\n"
    );
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

#[test]
fn path_display_escapes_native_bytes_without_losing_printable_unicode() {
    use std::{ffi::OsString, os::unix::ffi::OsStringExt};
    for (native, expected) in [
        (b"/logs/a\\n".to_vec(), r"/logs/a\\n"),
        (b"/logs/a\n".to_vec(), r"/logs/a\n"),
        (b"/logs/a\"b".to_vec(), r#"/logs/a\"b"#),
        (
            b"/logs/a\nb\rc\td\x1be\\f\xff".to_vec(),
            r"/logs/a\nb\rc\td\x1be\\f\xff",
        ),
        (b"/logs/\0\x7f\xfe".to_vec(), r"/logs/\x00\x7f\xfe"),
        ("/logs/界e\u{301}".as_bytes().to_vec(), "/logs/界e\u{301}"),
        ("/logs/�".as_bytes().to_vec(), "/logs/�"),
        (
            "/logs/\u{85}\u{2028}\u{202e}".as_bytes().to_vec(),
            r"/logs/\u{85}\u{2028}\u{202e}",
        ),
    ] {
        let path = PathBuf::from(OsString::from_vec(native));
        let escaped = display_path(&path);
        assert_eq!(escaped, format!("\"{expected}\""));
        assert!(!escaped.chars().any(char::is_control));
    }
}

#[tokio::test]
async fn live_log_wraps_or_uses_scrollback_without_promising_success_retention() {
    let directory = tempfile::tempdir().unwrap();
    for size in [(80, 24), (19, 8)] {
        let (stdout, out) = writer(None, false);
        let (stderr, _) = writer(None, false);
        let (_sink, receiver) = EventSink::channel(1);
        let (_state, observe) = watch::channel(TerminalState::Idle);
        let mut settings = config(directory.path(), true);
        settings.initial_size = size;
        settings.configuration_dir = Some("/configuration".into());
        let path = PathBuf::from(format!(
            "/configuration/.confctl/logs/{}",
            "full-name-".repeat(8)
        ));
        settings.invocation_log = Some(path.clone());
        let (control, mut presentation) = Presentation::new(
            receiver,
            observe,
            OutputTargets { stdout, stderr },
            settings,
            None,
        )
        .unwrap();
        presentation
            .command_starting(TaskId(0), "nix eval --json .#settings".into())
            .await
            .unwrap();
        let active = String::from_utf8(out.lock().unwrap().clone()).unwrap();
        let display = display_path(path.strip_prefix("/configuration").unwrap());
        if size.0 == 80 {
            for piece in live::wrap(&format!("Full log: {display}"), 79) {
                assert!(active.contains(&piece));
            }
        } else {
            assert!(active.contains(&format!("Full log: {display}\n")));
        }
        let before = out.lock().unwrap().len();
        let running = tokio::spawn(presentation.run());
        control.finish().await.unwrap();
        running.await.unwrap().unwrap();
        let bytes = out.lock().unwrap();
        let final_output = String::from_utf8_lossy(&bytes[before..]);
        assert!(!final_output.contains("Log file:"));
        assert!(!final_output.contains("Output source"));
        assert!(!final_output.contains("task 0"));
    }
}

#[tokio::test]
async fn command_context_has_no_phantom_task_and_activity_never_replays_in_live_mode() {
    for live in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let (stdout, out) = writer(None, false);
        let (stderr, err) = writer(None, false);
        let (sink, receiver) = EventSink::channel(1);
        let (_state, observe) = watch::channel(TerminalState::Idle);
        let (control, presentation) = Presentation::new(
            receiver,
            observe,
            OutputTargets { stdout, stderr },
            config(directory.path(), live),
            None,
        )
        .unwrap();
        let running = tokio::spawn(presentation.run());
        sink.emit(UiEvent::CommandStarting {
            command: crate::events::CommandId(10001),
            task: TaskId(0),
            display: "nix eval [arguments redacted]".into(),
        })
        .await
        .unwrap();
        let first = out.lock().unwrap().clone();
        if live {
            assert!(String::from_utf8_lossy(&first).contains("nix eval [arguments redacted]"));
        } else {
            assert!(first.is_empty());
        }
        for chunk in b"\x1b[31mactivity-fragment\x1b[0m\n".chunks(3) {
            sink.emit(UiEvent::ActivityOutput {
                task: TaskId(0),
                stream: OutputStream::Stderr,
                bytes: chunk.to_vec(),
            })
            .await
            .unwrap();
        }
        sink.emit(output(OutputStream::Stdout, b"required report\n"))
            .await
            .unwrap();
        control.finish().await.unwrap();
        running.await.unwrap().unwrap();
        let out = out.lock().unwrap();
        let err = err.lock().unwrap();
        assert_eq!(
            out.windows(16)
                .filter(|w| *w == b"required report\n")
                .count(),
            1
        );
        if live {
            assert!(err.is_empty());
            let text = String::from_utf8_lossy(&out);
            assert!(!text.contains("Output source"));
            assert!(!text.contains("task 0"));
            assert!(!text.contains("succeeded"));
            assert!(
                !text.contains("activity-fragment"),
                "finish must not replay an unpainted tail"
            );
        } else {
            assert_eq!(&*err, b"\x1b[31mactivity-fragment\x1b[0m\n");
        }
    }
}

#[tokio::test]
async fn each_live_command_is_flushed_before_acknowledgement_and_fast_activity() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, out) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Live);
    let mut settings = config(directory.path(), true);
    settings.tail_rows = 2;
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        settings,
        None,
    )
    .unwrap();
    let running = tokio::spawn(presentation.run());
    for (id, display) in [
        (0, "nix eval .#settings"),
        (1, "nix build .#first"),
        (2, "nix build .#second"),
    ] {
        let start = out.lock().unwrap().len();
        sink.emit(UiEvent::CommandStarting {
            command: crate::events::CommandId(10002),
            task: TaskId(id),
            display: display.into(),
        })
        .await
        .unwrap();
        // This is the actual spawn acknowledgement, before any child output.
        let written = out.lock().unwrap().clone();
        let admitted = String::from_utf8_lossy(&written[start..]);
        assert_eq!(admitted.matches(&format!("$ {display}")).count(), 1);
        if id != 0 {
            sink.emit(UiEvent::ActivityOutput {
                task: TaskId(id),
                stream: OutputStream::Stderr,
                bytes: "fast native activity\n".repeat(32).into_bytes(),
            })
            .await
            .unwrap();
        }
    }
    control.finish().await.unwrap();
    running.await.unwrap().unwrap();
    assert!(err.lock().unwrap().is_empty(), "activity stays in the view");
}

#[tokio::test]
async fn live_command_admission_rechecks_parking_after_waiting_for_a_paint_slot() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, out) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (_sink, receiver) = EventSink::channel(1);
    let (state, observe) = watch::channel(TerminalState::Live);
    let (_control, mut presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), true),
        None,
    )
    .unwrap();
    presentation
        .command_starting(TaskId(0), "quiet first command".into())
        .await
        .unwrap();
    let before = out.lock().unwrap().len();
    let mut attempt = Box::pin(presentation.command_starting(TaskId(1), "later command".into()));
    assert!(
        attempt
            .as_mut()
            .poll(&mut Context::from_waker(Waker::noop()))
            .is_pending()
    );
    state.send_replace(TerminalState::Prompt);
    attempt.await.unwrap();
    {
        // Parking clears only the owned panel; no announcement reaches the prompt.
        let bytes = out.lock().unwrap();
        assert!(!String::from_utf8_lossy(&bytes[before..]).contains("later command"));
    }
    assert!(
        !presentation
            .model
            .tail()
            .iter()
            .any(|line| line.contains("later command"))
    );
    state.send_replace(TerminalState::Live);
    let before_resume = out.lock().unwrap().len();
    presentation.transition(TerminalState::Live).unwrap();
    assert_eq!(&out.lock().unwrap()[before_resume..], b"$ later command\n");
    presentation.paint_now().unwrap();
    let bytes = out.lock().unwrap();
    assert_eq!(
        String::from_utf8_lossy(&bytes)
            .matches("$ later command")
            .count(),
        1
    );
}

#[tokio::test]
async fn tiny_or_bounded_tail_commands_transfer_once_to_required_output() {
    for (size, tail_bytes) in [((3, 4), 100), ((80, 24), 8), ((2, 3), 100)] {
        let directory = tempfile::tempdir().unwrap();
        let (stdout, out) = writer(None, false);
        let (stderr, _) = writer(None, false);
        let (sink, receiver) = EventSink::channel(1);
        let (_state, observe) = watch::channel(TerminalState::Idle);
        let mut settings = config(directory.path(), true);
        settings.initial_size = size;
        settings.tail_line_bytes = tail_bytes;
        let (control, presentation) = Presentation::new(
            receiver,
            observe,
            OutputTargets { stdout, stderr },
            settings,
            None,
        )
        .unwrap();
        let running = tokio::spawn(presentation.run());
        sink.emit(UiEvent::CommandStarting {
            command: crate::events::CommandId(10003),
            task: TaskId(0),
            display: "nix build .#node".into(),
        })
        .await
        .unwrap();
        assert!(out.lock().unwrap().starts_with(b"$ nix build .#node\n"));
        control.finish().await.unwrap();
        running.await.unwrap().unwrap();
        assert_eq!(
            String::from_utf8_lossy(&out.lock().unwrap())
                .matches("$ nix build .#node")
                .count(),
            1
        );
    }
}

#[tokio::test]
async fn command_paint_failure_rejects_admission_and_retains_checked_failure() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, _) = writer(Some(0), false);
    let (stderr, _) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), true),
        None,
    )
    .unwrap();
    let running = tokio::spawn(presentation.run());
    assert!(
        sink.emit(UiEvent::CommandStarting {
            command: crate::events::CommandId(10004),
            task: TaskId(0),
            display: "must not spawn".into(),
        })
        .await
        .is_err()
    );
    assert!(running.await.unwrap().is_err());
    let (source, _) = pending(control.finish().await.unwrap_err());
    assert!(source.to_string().contains("paint live presentation"));
}

#[tokio::test]
async fn required_chunks_without_lf_survive_the_next_live_anchor_on_either_stream() {
    for stream in [OutputStream::Stdout, OutputStream::Stderr] {
        let directory = tempfile::tempdir().unwrap();
        let (terminal, bytes) = writer(None, false);
        let (sink, receiver) = EventSink::channel(1);
        let (_state, observe) = watch::channel(TerminalState::Idle);
        let (control, presentation) = Presentation::new(
            receiver,
            observe,
            OutputTargets {
                stdout: terminal.clone(),
                stderr: terminal,
            },
            config(directory.path(), true),
            None,
        )
        .unwrap();
        let running = tokio::spawn(presentation.run());
        sink.emit(output(stream, b"unfinished report chunk"))
            .await
            .unwrap();
        assert_eq!(&*bytes.lock().unwrap(), b"unfinished report chunk");
        sink.emit(UiEvent::CommandStarting {
            command: crate::events::CommandId(10005),
            task: TaskId(0),
            display: "next command".into(),
        })
        .await
        .unwrap();
        let written = bytes.lock().unwrap().clone();
        let next = &written[b"unfinished report chunk".len()..];
        assert!(
            next.iter().position(|b| *b == b'\n').unwrap()
                < next.windows(4).position(|b| b == b"\x1b[2K").unwrap()
        );
        control.finish().await.unwrap();
        running.await.unwrap().unwrap();
    }
}

#[tokio::test]
async fn omitted_and_shortened_task_rows_keep_full_final_source_associations() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, out) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let mut settings = config(directory.path(), true);
    settings.initial_size = (33, 12);
    settings.max_tasks = 1;
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        settings,
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    for (id, description) in [
        (u64::MAX - 1, "node-a activation with detailed error"),
        (u64::MAX, "node-b activation with detailed error"),
    ] {
        sink.emit(UiEvent::TaskStarted {
            task: TaskId(id),
            description: description.into(),
        })
        .await
        .expect("task");
        sink.emit(UiEvent::TaskFinished {
            task: TaskId(id),
            state: crate::events::TaskState::Failed,
        })
        .await
        .expect("result");
    }
    control.finish().await.expect("finish");
    running.await.expect("join").expect("run");
    let rendered = String::from_utf8(out.lock().expect("stdout").clone()).expect("UTF-8");
    assert!(!rendered.contains("tasks not shown in full"));
    assert!(rendered.contains("node-a activation with detailed error: failed 0\n"));
    assert!(rendered.contains("node-b activation with detailed error: failed 0\n"));
}

#[tokio::test]
async fn checked_finish_keeps_missing_pending_and_executing_results_incomplete() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, out) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), true),
        None,
    )
    .unwrap();
    let running = tokio::spawn(presentation.run());
    sink.emit(UiEvent::TaskPending {
        task: TaskId(1),
        description: "node-a".into(),
    })
    .await
    .unwrap();
    sink.emit(UiEvent::TaskStarted {
        task: TaskId(2),
        description: "node-b".into(),
    })
    .await
    .unwrap();
    control.finish().await.unwrap();
    running.await.unwrap().unwrap();
    let rendered = String::from_utf8(out.lock().unwrap().clone()).unwrap();
    // Every paint begins with Hide. Earlier timer paints may truthfully show
    // execution; only the checked final panel must show incomplete results.
    let final_panel = rendered.rsplit("\x1b[?25l").next().unwrap();
    assert!(final_panel.contains("node-a: incomplete (last pending) 0"));
    assert!(final_panel.contains("node-b: incomplete (last running) 0"));
    assert!(!final_panel.contains("succeeded"));
    assert!(!final_panel.contains("canceled"));
    assert!(!final_panel.contains("running @2"));
}

#[tokio::test]
async fn tiny_unregistered_output_retains_source_without_inventing_task_state() {
    let directory = tempfile::tempdir().expect("directory");
    let (stdout, out) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Idle);
    let mut settings = config(directory.path(), true);
    settings.initial_size = (9, 8);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        settings,
        None,
    )
    .expect("presentation");
    let running = tokio::spawn(presentation.run());
    sink.emit(UiEvent::Output {
        task: TaskId(u64::MAX),
        stream: OutputStream::Stderr,
        bytes: b"failure\n".to_vec(),
    })
    .await
    .expect("output without registration");
    control.finish().await.expect("finish");
    running.await.expect("join").expect("run");
    assert_eq!(*err.lock().expect("stderr"), b"failure\n");
    let rendered = String::from_utf8(out.lock().expect("stdout").clone()).expect("UTF-8");
    assert!(!rendered.contains("Output source"));
    assert!(!rendered.contains("tasks not shown"));
    assert!(!rendered.contains("succeeded"));
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
async fn styled_diagnostics_are_atomic_frame_values_with_original_stream_color_policy() {
    for (mode, no_color) in [
        (ColorMode::Always, true),
        (ColorMode::Never, false),
        (ColorMode::Auto, true),
    ] {
        for live in [false, true] {
            let directory = tempfile::tempdir().unwrap();
            let (stdout, out) = writer(None, false);
            let (stderr, err) = writer(None, false);
            let (_sink, receiver) = EventSink::channel(1);
            let (state, observe) = watch::channel(TerminalState::Prompt);
            let mut settings = config(directory.path(), live);
            let policy = OutputPolicy::resolve(mode, true, false, true, true, no_color);
            settings.policy.stdout_color = policy.stdout_color;
            settings.policy.stderr_color = policy.stderr_color;
            let (_control, mut presentation) = Presentation::new(
                receiver,
                observe,
                OutputTargets { stdout, stderr },
                settings,
                None,
            )
            .unwrap();
            let parts = vec![
                (Style::Heading, "Built generations\n".into()),
                (Style::Generation, "node: gen\n".into()),
            ];
            let event = || UiEvent::StyledDiagnostic {
                stream: OutputStream::Stdout,
                parts: parts.clone(),
            };
            validate_frame(&[event()]).unwrap();
            presentation
                .event(UiEvent::OperationGroup {
                    group: 1,
                    description: "styled group".into(),
                    prelude: vec![event()],
                })
                .unwrap();
            assert!(out.lock().unwrap().is_empty(), "parked frame stays owned");
            let expected = plain::styled_diagnostic(&parts, policy.stdout_color);
            assert_eq!(
                std::fs::read(presentation.stdout.file.as_ref().unwrap().path()).unwrap(),
                expected
            );
            presentation
                .event(UiEvent::StyledDiagnostic {
                    stream: OutputStream::Stderr,
                    parts: parts.clone(),
                })
                .unwrap();
            assert_eq!(
                std::fs::read(presentation.stderr.file.as_ref().unwrap().path()).unwrap(),
                plain::styled_diagnostic(&parts, policy.stderr_color)
            );
            state.send_replace(TerminalState::Idle);
            presentation.transition(TerminalState::Idle).unwrap();
            assert_eq!(*out.lock().unwrap(), expected);
            assert_eq!(
                *err.lock().unwrap(),
                plain::styled_diagnostic(&parts, policy.stderr_color)
            );
            presentation.finalize_group().unwrap();
            presentation.cleaned = true;
        }
    }
}

#[tokio::test]
async fn styled_prompt_preview_is_one_atomic_sanitized_presentation_value() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, out) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (_sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Prompt);
    let (_control, mut presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), true),
        None,
    )
    .unwrap();
    presentation
        .event(UiEvent::PromptFrame {
            preview: vec![UiEvent::StyledDiagnostic {
                stream: OutputStream::Stdout,
                parts: vec![
                    (Style::Heading, "Plan: ".into()),
                    (Style::Generation, "\x1b[31mgen\x1b[0m\n".into()),
                ],
            }],
            text: "Continue? ".into(),
        })
        .unwrap();
    assert_eq!(&*out.lock().unwrap(), b"Plan: gen\nContinue? ");
    presentation.cleaned = true;
}

#[tokio::test]
async fn compact_commands_use_unique_ids_suppress_visible_output_and_never_resurrect_peers() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, out) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (_sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Live);
    let mut settings = config(directory.path(), false);
    settings.mode = PresentationMode::CommandSpinner;
    let (_control, mut presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        settings,
        None,
    )
    .unwrap();
    for (id, display) in [
        (1, "nix eval --json safe"),
        (2, "second capture-only command"),
    ] {
        presentation
            .event(UiEvent::CommandStarting {
                command: crate::events::CommandId(id),
                task: TaskId(0),
                display: display.into(),
            })
            .unwrap();
    }
    assert!(out.lock().unwrap().is_empty(), "transients are stderr-only");
    let shown = String::from_utf8(err.lock().unwrap().clone()).unwrap();
    assert!(shown.contains("nix eval --json safe") && shown.contains("+1 running"));
    assert!(!shown.contains("Command:") && !shown.contains("Live view"));
    // A capture-only child emits no output event. Completion of its own ID
    // clears it while the other ID remains admitted despite shared TaskId0.
    presentation
        .event(UiEvent::CommandFinished {
            command: crate::events::CommandId(2),
        })
        .unwrap();
    assert_eq!(presentation.commands.len(), 1);
    presentation
        .event(UiEvent::Output {
            task: TaskId(0),
            stream: OutputStream::Stdout,
            bytes: b"exact-no-LF".to_vec(),
        })
        .unwrap();
    assert_eq!(&*out.lock().unwrap(), b"exact-no-LF");
    assert!(
        presentation
            .commands
            .iter()
            .all(|command| command.suppressed)
    );
    let bytes = err.lock().unwrap().len();
    presentation.paint_now().unwrap();
    presentation
        .event(UiEvent::CommandFinished {
            command: crate::events::CommandId(1),
        })
        .unwrap();
    presentation
        .event(UiEvent::CommandFinished {
            command: crate::events::CommandId(99),
        })
        .unwrap();
    presentation
        .event(UiEvent::CommandStarting {
            command: crate::events::CommandId(3),
            task: TaskId(0),
            display: "must stay suppressed at uncertain cursor".into(),
        })
        .unwrap();
    assert_eq!(err.lock().unwrap().len(), bytes);
    presentation
        .event(UiEvent::Output {
            task: TaskId(0),
            stream: OutputStream::Stdout,
            bytes: b"\n".to_vec(),
        })
        .unwrap();
    presentation.paint_now().unwrap();
    assert_eq!(
        err.lock().unwrap().len(),
        bytes,
        "same command cannot revive after visible output"
    );
    presentation
        .event(UiEvent::CommandFinished {
            command: crate::events::CommandId(3),
        })
        .unwrap();
    presentation
        .event(output(OutputStream::Stderr, b"stderr anchor\n"))
        .unwrap();
    presentation
        .event(UiEvent::CommandStarting {
            command: crate::events::CommandId(4),
            task: TaskId(0),
            display: "later independent quiet child".into(),
        })
        .unwrap();
    assert!(
        String::from_utf8(err.lock().unwrap().clone())
            .unwrap()
            .contains("later independent quiet child")
    );
    presentation.finalize_group().unwrap();
    assert!(
        !presentation
            .spinner_painter
            .as_ref()
            .unwrap()
            .has_viewport()
    );
    presentation.cleaned = true;
}

#[tokio::test]
async fn redirected_stdout_lf_cannot_reopen_spinner_over_unfinished_required_stderr() {
    for full_available in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let (stdout, out) = writer(None, false);
        let (stderr, err) = writer(None, false);
        let (_sink, receiver) = EventSink::channel(1);
        let (_state, observe) = watch::channel(TerminalState::Live);
        let mut settings = config(directory.path(), full_available);
        settings.mode = PresentationMode::CommandSpinner;
        let (_control, mut presentation) = Presentation::new(
            receiver,
            observe,
            OutputTargets { stdout, stderr },
            settings,
            None,
        )
        .unwrap();
        presentation
            .event(UiEvent::CommandStarting {
                command: crate::events::CommandId(1),
                task: TaskId(1),
                display: "child A".into(),
            })
            .unwrap();
        presentation
            .event(output(OutputStream::Stderr, b"required stderr without LF"))
            .unwrap();
        presentation
            .event(UiEvent::CommandFinished {
                command: crate::events::CommandId(1),
            })
            .unwrap();
        assert!(err.lock().unwrap().ends_with(b"required stderr without LF"));
        let before = err.lock().unwrap().clone();
        presentation
            .event(output(OutputStream::Stdout, b"report to redirected file\n"))
            .unwrap();
        presentation
            .event(UiEvent::CommandStarting {
                command: crate::events::CommandId(2),
                task: TaskId(2),
                display: "child B".into(),
            })
            .unwrap();
        presentation.paint_now().unwrap();
        presentation
            .event(UiEvent::CommandFinished {
                command: crate::events::CommandId(2),
            })
            .unwrap();
        presentation.finalize_group().unwrap();
        assert_eq!(&*out.lock().unwrap(), b"report to redirected file\n");
        assert_eq!(
            *err.lock().unwrap(),
            before,
            "no transient write may erase the required stderr prefix"
        );
        presentation.cleaned = true;
    }
}

#[tokio::test]
async fn compact_prompt_foreground_resize_and_finish_keep_the_existing_terminal_owner() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, _) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (_sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Live);
    let mut settings = config(directory.path(), true);
    settings.mode = PresentationMode::CommandSpinner;
    let (_control, mut presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        settings,
        None,
    )
    .unwrap();
    for (id, state) in [
        (1, TerminalState::Prompt),
        (2, TerminalState::ExternalChild),
    ] {
        presentation
            .event(UiEvent::CommandStarting {
                command: crate::events::CommandId(id),
                task: TaskId(id),
                display: "quiet child".into(),
            })
            .unwrap();
        presentation.transition(state).unwrap();
        assert!(
            !presentation
                .spinner_painter
                .as_ref()
                .unwrap()
                .has_viewport()
        );
        let parked = err.lock().unwrap().len();
        presentation.paint_now().unwrap();
        assert_eq!(err.lock().unwrap().len(), parked);
        presentation.transition(TerminalState::Live).unwrap();
        presentation.paint_now().unwrap();
        assert_eq!(
            err.lock().unwrap().len(),
            parked,
            "foreign cursor cannot be guessed"
        );
        presentation
            .event(output(OutputStream::Stderr, b"visible line\n"))
            .unwrap();
        presentation
            .event(UiEvent::CommandFinished {
                command: crate::events::CommandId(id),
            })
            .unwrap();
    }
    presentation
        .event(UiEvent::CommandStarting {
            command: crate::events::CommandId(3),
            task: TaskId(3),
            display: "resize child".into(),
        })
        .unwrap();
    presentation.config.initial_size = (40, 12);
    presentation.paint_now().unwrap();
    assert!(
        !presentation
            .spinner_painter
            .as_ref()
            .unwrap()
            .has_viewport()
    );
    presentation.finalize_group().unwrap();
    presentation.cleaned = true;
}

#[tokio::test]
async fn build_scope_restores_compact_before_reports_without_finalizing_unrelated_work() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, out) = writer(None, false);
    let (stderr, _) = writer(None, false);
    let (_sink, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Live);
    let mut settings = config(directory.path(), true);
    settings.mode = PresentationMode::CommandSpinner;
    settings.operation = "status".into();
    let (_control, mut presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        settings,
        None,
    )
    .unwrap();
    presentation
        .event(UiEvent::TaskStarted {
            task: TaskId(99),
            description: "unrelated peer".into(),
        })
        .unwrap();
    presentation
        .event(UiEvent::BuildPhase(crate::events::BuildPhase::Begin))
        .unwrap();
    assert_eq!(presentation.mode, PresentationMode::Full);
    assert!(
        presentation
            .event(UiEvent::BuildPhase(crate::events::BuildPhase::Begin))
            .is_err()
    );
    presentation
        .event(UiEvent::OperationGroup {
            group: 1,
            description: "build".into(),
            prelude: vec![],
        })
        .unwrap();
    presentation
        .event(UiEvent::TaskStarted {
            task: TaskId(1),
            description: "build group".into(),
        })
        .unwrap();
    presentation
        .event(UiEvent::BuildProgress {
            task: TaskId(1),
            building: None,
            fetching: None,
        })
        .unwrap();
    presentation
        .event(UiEvent::TaskFinished {
            task: TaskId(1),
            state: crate::events::TaskState::Failed,
        })
        .unwrap();
    presentation
        .event(UiEvent::BuildPhase(crate::events::BuildPhase::End))
        .unwrap();
    assert_eq!(presentation.mode, PresentationMode::CommandSpinner);
    assert_eq!(presentation.config.operation, "status");
    assert!(!presentation.model.finalized());
    assert_eq!(
        presentation.model.rows()[0].phase,
        model::TaskPhase::Executing
    );
    assert!(presentation.model.was_snapshotted(TaskId(1)));
    assert!(!presentation.model.was_snapshotted(TaskId(99)));
    assert!(!presentation.painter.as_ref().unwrap().has_viewport());
    let before = out.lock().unwrap().len();
    presentation
        .event(UiEvent::Diagnostic {
            stream: OutputStream::Stdout,
            style: Style::Heading,
            text: "Built generations\nactual result\n".into(),
        })
        .unwrap();
    presentation.paint_now().unwrap();
    assert_eq!(
        &out.lock().unwrap()[before..],
        b"Built generations\nactual result\n"
    );
    assert!(
        presentation
            .event(UiEvent::BuildPhase(crate::events::BuildPhase::End))
            .is_err()
    );
    presentation.cleaned = true;
}

#[tokio::test]
async fn compact_completion_clears_owned_view_and_never_reopens_after_generations() {
    for state in [
        crate::events::TaskState::Succeeded,
        crate::events::TaskState::Failed,
        crate::events::TaskState::Canceled,
    ] {
        let directory = tempfile::tempdir().unwrap();
        let (stdout, out) = writer(None, false);
        let (stderr, _) = writer(None, false);
        let (_sink, receiver) = EventSink::channel(1);
        let (_state, observe) = watch::channel(TerminalState::Live);
        let (_control, mut presentation) = Presentation::new(
            receiver,
            observe,
            OutputTargets { stdout, stderr },
            config(directory.path(), true),
            None,
        )
        .unwrap();
        presentation
            .event(UiEvent::TaskStarted {
                task: TaskId(1),
                description: "build group 1/1".into(),
            })
            .unwrap();
        presentation
            .event(UiEvent::BuildProgress {
                task: TaskId(1),
                building: Some(crate::events::NixActivityProgress {
                    done: 2,
                    expected: 2,
                    running: 0,
                    failed: 0,
                }),
                fetching: Some(crate::events::NixActivityProgress::default()),
            })
            .unwrap();
        presentation
            .event(UiEvent::ActivityOutput {
                task: TaskId(1),
                stream: OutputStream::Stderr,
                bytes: b"must-not-be-retained-native-tail\n".to_vec(),
            })
            .unwrap();
        presentation.paint_now().unwrap();
        assert!(presentation.painter.as_ref().unwrap().has_viewport());
        presentation
            .event(UiEvent::TaskFinished {
                task: TaskId(1),
                state,
            })
            .unwrap();
        let before = out.lock().unwrap().len();
        presentation
            .event(UiEvent::Checkpoint {
                key: TaskId(1),
                label: "Published group".into(),
                state,
                tasks: vec![TaskId(1)],
            })
            .unwrap();
        assert!(
            !presentation.painter.as_ref().unwrap().has_viewport(),
            "full-height panel must be cleared, not retained"
        );
        assert!(!presentation.active_view);
        let after_checkpoint = out.lock().unwrap().len();
        let retained = String::from_utf8(out.lock().unwrap()[before..].to_vec()).unwrap();
        let compact = retained.split_once("Published group:").unwrap().1;
        assert!(
            compact.contains("Building")
                && compact.contains("Fetching")
                && compact.contains("Total")
        );
        assert_eq!(
            compact.lines().count(),
            5,
            "outcome, task and three bars only"
        );
        assert!(compact.lines().all(|line| !line.trim().is_empty()));
        assert!(!compact.contains("must-not-be-retained") && !compact.contains("Live view"));
        presentation
            .event(UiEvent::Diagnostic {
                stream: OutputStream::Stdout,
                style: Style::Heading,
                text: "Built generations\nnode: gen\n".into(),
            })
            .unwrap();
        presentation.paint_now().unwrap();
        presentation.finalize_group().unwrap();
        assert!(!presentation.painter.as_ref().unwrap().has_viewport());
        assert_eq!(
            &out.lock().unwrap()[after_checkpoint..],
            b"Built generations\nnode: gen\n"
        );
        presentation.cleaned = true;
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

#[tokio::test]
async fn gc_plain_labels_and_unknown_progress_keep_both_stream_output_once() {
    let directory = tempfile::tempdir().unwrap();
    let (stdout, out) = writer(None, false);
    let (stderr, err) = writer(None, false);
    let (events, receiver) = EventSink::channel(1);
    let (_state, observe) = watch::channel(TerminalState::Live);
    let (control, presentation) = Presentation::new(
        receiver,
        observe,
        OutputTargets { stdout, stderr },
        config(directory.path(), false),
        None,
    )
    .unwrap();
    let job = tokio::spawn(presentation.run());
    for (task, host) in [(TaskId(1), "alpha"), (TaskId(2), "beta")] {
        events
            .emit(UiEvent::TaskStarted {
                task,
                description: format!("GC {host}"),
            })
            .await
            .unwrap();
        let output = crate::process::gc_output::GcOutput::new(host);
        output
            .forward(
                &events,
                (task, OutputStream::Stdout),
                b"deleting '/nix/store/stdout'\n",
                true,
                &CancellationToken::new(),
            )
            .await
            .unwrap();
        output
            .forward(
                &events,
                (task, OutputStream::Stderr),
                b"deleting '/nix/store/stderr'\n",
                true,
                &CancellationToken::new(),
            )
            .await
            .unwrap();
        events
            .emit(UiEvent::TaskFinished {
                task,
                state: crate::events::TaskState::Succeeded,
            })
            .await
            .unwrap();
    }
    control.finish().await.unwrap();
    job.await.unwrap().unwrap();
    let out = out.lock().unwrap();
    let err = err.lock().unwrap();
    for host in ["alpha", "beta"] {
        let a = format!("{host}> deleting '/nix/store/stdout'\n");
        let b = format!("{host}> deleting '/nix/store/stderr'\n");
        assert_eq!(
            out.windows(a.len()).filter(|w| *w == a.as_bytes()).count(),
            1
        );
        assert_eq!(
            err.windows(b.len()).filter(|w| *w == b.as_bytes()).count(),
            1
        );
    }
}
