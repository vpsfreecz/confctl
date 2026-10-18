//! Thin invocation construction and joined shutdown. Operation adapters are
//! deliberately absent at this milestone; no Ruby fallback is hidden here.

use crate::{
    cli::{self, CliDisplay, ParseOutcome},
    domain::{Action, ExitDisposition, Invocation},
    error::ConfctlError,
    events::{EventReceiver, EventSink, OutputStream, UiEvent},
    process::{Captured, Supervisor},
    terminal::Ui,
    timing::Timing,
};
use std::{
    future::Future,
    io::{self, Write},
    path::PathBuf,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio_util::sync::CancellationToken;

/// Small construction seam for fake operations; dispatch never reparses argv.
pub trait Operations {
    fn available(&self, action: &Action) -> bool;
    fn run(
        &self,
        invocation: &Invocation,
        context: &Context,
    ) -> impl Future<Output = Result<ExitDisposition, ConfctlError>>;
}

pub struct PendingOperations;
impl Operations for PendingOperations {
    fn available(&self, _: &Action) -> bool {
        false
    }
    async fn run(
        &self,
        invocation: &Invocation,
        _: &Context,
    ) -> Result<ExitDisposition, ConfctlError> {
        Err(pending(&invocation.action))
    }
}
fn pending(action: &Action) -> ConfctlError {
    ConfctlError::Configuration(format!(
        "Rust operation '{}' is not implemented yet",
        action.name()
    ))
}

pub struct Context {
    pub cwd: PathBuf,
    pub cancellation: CancellationToken,
    pub events: EventSink,
    pub processes: Supervisor,
    pub ui: Ui,
    pub timing: Timing,
}

type SharedWriter = Arc<Mutex<Box<dyn Write + Send>>>;

/// Capture clocks at executable entry, before parsing or environment setup.
pub async fn run(argv: Vec<std::ffi::OsString>, timing: Timing) -> i32 {
    execute(
        argv,
        timing,
        &PendingOperations,
        std::env::current_dir(),
        Arc::new(Mutex::new(Box::new(io::stdout()))),
        Arc::new(Mutex::new(Box::new(io::stderr()))),
    )
    .await
}

async fn execute<O: Operations>(
    argv: Vec<std::ffi::OsString>,
    timing: Timing,
    operations: &O,
    cwd: io::Result<PathBuf>,
    stdout: SharedWriter,
    stderr: SharedWriter,
) -> i32 {
    let invocation = match cli::parse(argv) {
        Ok(ParseOutcome::Display(display)) => return write_display(display, &stdout, &stderr),
        Err(failure) => return write_display(failure.display, &stdout, &stderr),
        Ok(ParseOutcome::Run(invocation)) => invocation,
    };
    // Unavailable development stubs do not admit operation timing or create
    // configuration/log state. Every supported final route must replace these.
    if !operations.available(&invocation.action) {
        return diagnose(&pending(&invocation.action), &stderr);
    }
    let cwd = match cwd {
        Ok(cwd) => cwd,
        Err(source) => {
            return diagnose(
                &ConfctlError::Io {
                    operation: "resolve configuration directory",
                    path: None,
                    source,
                },
                &stderr,
            );
        }
    };
    let spool = match tempfile::tempdir() {
        Ok(spool) => spool,
        Err(source) => {
            return diagnose(
                &ConfctlError::Io {
                    operation: "create invocation spool directory",
                    path: None,
                    source,
                },
                &stderr,
            );
        }
    };
    timing.admit(&invocation.action);
    let cancellation = CancellationToken::new();
    let (events, receiver) = EventSink::channel(64);
    let ui = match Ui::new(
        io::stdin(),
        events.clone(),
        cancellation.clone(),
        Some(timing.clone()),
    ) {
        Ok(ui) => ui,
        Err(error) => return diagnose(&error, &stderr),
    };
    let processes = Supervisor::new(
        events.clone(),
        spool.path().into(),
        1024 * 1024,
        Duration::from_secs(2),
        None,
        Some(timing.clone()),
    );
    let context = Context {
        cwd,
        cancellation: cancellation.clone(),
        events,
        processes,
        ui,
        timing: timing.clone(),
    };
    let renderer = tokio::spawn(render(receiver, stdout.clone(), stderr.clone()));
    let signal_cancel = cancellation.clone();
    let signal = tokio::spawn(async move {
        if tokio::signal::ctrl_c().await.is_ok() {
            signal_cancel.cancel();
        }
    });
    let result = operations.run(&invocation, &context).await;
    // Never cancel/drop shutdown futures: they carry owned IO and evidence.
    let mut cleanup = Vec::new();
    if let Err(error) = context.processes.shutdown().await {
        cleanup.push(error);
    }
    if let Err(error) = context.ui.shutdown() {
        cleanup.push(error);
    }
    drop(context);
    signal.abort();
    let _ = signal.await;
    match renderer.await {
        Ok(Ok(())) => {}
        Ok(Err(error)) => cleanup.push(error),
        Err(error) => cleanup.push(ConfctlError::EventDelivery(error.to_string())),
    }
    let (mut disposition, error) = match result {
        Ok(exit) if cleanup.is_empty() => (exit, None),
        Ok(_) => {
            let first = cleanup.remove(0);
            (
                ExitDisposition::failed(1),
                Some(first.with_secondary(cleanup)),
            )
        }
        Err(error) => {
            let exit = if matches!(error, ConfctlError::Cancelled) {
                ExitDisposition::canceled(130)
            } else {
                ExitDisposition::failed(1)
            };
            (exit, Some(error.with_secondary(cleanup)))
        }
    };
    if let Some(error) = error {
        let (error, paths) = retain_error_evidence(error);
        diagnose(&error, &stderr);
        for path in paths {
            let _ = write_bytes(
                &stderr,
                format!("Process evidence: {}\n", path.display()).as_bytes(),
            );
        }
    }
    // All child/input/renderer owners have joined before freezing the report.
    if let Some(report) = timing.finish(disposition) {
        match stderr.lock() {
            Ok(mut writer) => {
                if let Err(error) = report.write(&mut *writer) {
                    disposition.code = error.code;
                }
            }
            Err(_) => {
                if disposition.code == 0 {
                    disposition = ExitDisposition::failed(1);
                }
            }
        }
    }
    disposition.code
}

fn write_bytes(writer: &SharedWriter, bytes: &[u8]) -> io::Result<()> {
    let mut writer = writer
        .lock()
        .map_err(|_| io::Error::other("output writer poisoned"))?;
    writer.write_all(bytes)?;
    writer.flush()
}
fn write_display(display: CliDisplay, stdout: &SharedWriter, stderr: &SharedWriter) -> i32 {
    let result =
        write_bytes(stdout, &display.stdout).and_then(|()| write_bytes(stderr, &display.stderr));
    if result.is_err() && display.code == 0 {
        1
    } else {
        display.code
    }
}
fn diagnose(error: &ConfctlError, stderr: &SharedWriter) -> i32 {
    let _ = write_bytes(stderr, format!("error: {error}\n").as_bytes());
    1
}

async fn render(
    mut receiver: EventReceiver,
    stdout: SharedWriter,
    stderr: SharedWriter,
) -> Result<(), ConfctlError> {
    while let Some(delivery) = receiver.receive().await {
        let result = match &delivery.event {
            UiEvent::Output { stream, bytes, .. } => write_bytes(
                if *stream == OutputStream::Stdout {
                    &stdout
                } else {
                    &stderr
                },
                bytes,
            ),
            UiEvent::Diagnostic { stream, text, .. } => write_bytes(
                if *stream == OutputStream::Stdout {
                    &stdout
                } else {
                    &stderr
                },
                text.as_bytes(),
            ),
            // Full table/live presentation belongs to the later renderer
            // package. Refuse it explicitly, never silently acknowledge loss.
            _ => Err(io::Error::other(
                "Rust event presentation is not implemented yet",
            )),
        };
        let failure = result.as_ref().err().map(ToString::to_string);
        delivery.acknowledge(result.map_err(|error| error.to_string()));
        if let Some(error) = failure {
            receiver.close();
            return Err(ConfctlError::EventDelivery(error));
        }
    }
    Ok(())
}

/// Materialize owned captures before dropping temporary process evidence. The
/// retained directory is deliberately kept on failures; no cleanup is scheduled.
fn retain_error_evidence(error: ConfctlError) -> (ConfctlError, Vec<PathBuf>) {
    let mut paths = Vec::new();
    let mut failures = Vec::new();
    collect_evidence(&error, &mut paths, &mut failures);
    (error.with_secondary(failures), paths)
}

fn collect_evidence(
    error: &ConfctlError,
    paths: &mut Vec<PathBuf>,
    failures: &mut Vec<ConfctlError>,
) {
    match error {
        ConfctlError::ProcessFailure { source, evidence } => {
            for (name, capture) in [("stdout", &evidence.stdout), ("stderr", &evidence.stderr)] {
                if matches!(capture, Captured::Unavailable) {
                    continue;
                }
                match retain_capture(name, capture) {
                    Ok(path) => paths.push(path),
                    Err(source) => failures.push(ConfctlError::Io {
                        operation: "retain partial process evidence",
                        path: None,
                        source,
                    }),
                }
            }
            collect_evidence(source, paths, failures);
        }
        ConfctlError::Cleanup { primary, secondary } => {
            collect_evidence(primary, paths, failures);
            for error in secondary {
                collect_evidence(error, paths, failures);
            }
        }
        _ => {}
    }
}
fn retain_capture(name: &str, capture: &Captured) -> io::Result<PathBuf> {
    let mut file = tempfile::Builder::new()
        .prefix(&format!("confctl-failure-{name}-"))
        .suffix(".raw")
        .tempfile()?;
    match capture {
        Captured::Memory(bytes) => file.write_all(bytes)?,
        Captured::Spool { file: source, .. } => {
            io::copy(&mut source.reopen()?, &mut file)?;
        }
        Captured::Unavailable => return Err(io::Error::other("no capture to retain")),
    }
    file.as_file().sync_all()?;
    let (file, path) = file.keep().map_err(|error| error.error)?;
    drop(file);
    Ok(path)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::PartialProcessEvidence;

    struct FakeOperation;
    impl Operations for FakeOperation {
        fn available(&self, _: &Action) -> bool {
            true
        }
        async fn run(
            &self,
            _: &Invocation,
            context: &Context,
        ) -> Result<ExitDisposition, ConfctlError> {
            context
                .events
                .emit(UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: crate::events::Style::Plain,
                    text: "operation result\n".into(),
                })
                .await?;
            Ok(ExitDisposition::completed())
        }
    }
    #[derive(Clone)]
    struct Buffer(Arc<Mutex<Vec<u8>>>);
    impl Write for Buffer {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(bytes);
            Ok(bytes.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    #[tokio::test]
    async fn available_operation_output_precedes_single_footer_after_shutdown() {
        let out = Buffer(Arc::new(Mutex::new(vec![])));
        let err = Buffer(Arc::new(Mutex::new(vec![])));
        let code = execute(
            vec!["build".into()],
            Timing::start(),
            &FakeOperation,
            std::env::current_dir(),
            Arc::new(Mutex::new(Box::new(out.clone()))),
            Arc::new(Mutex::new(Box::new(err.clone()))),
        )
        .await;
        assert_eq!(code, 0);
        assert_eq!(*out.0.lock().unwrap(), b"operation result\n");
        let stderr = String::from_utf8(err.0.lock().unwrap().clone()).unwrap();
        assert_eq!(stderr.matches("Timing: completed (exit 0)").count(), 1);
    }
    #[test]
    fn failure_boundary_retains_raw_bytes_before_releasing_spool_owner() {
        let scratch = tempfile::tempdir().unwrap();
        let mut source = tempfile::NamedTempFile::new_in(scratch.path()).unwrap();
        source.write_all(b"protected failure prefix").unwrap();
        let source_path = source.path().to_owned();
        let error = ConfctlError::ProcessFailure {
            source: Box::new(ConfctlError::EventDelivery("closed".into())),
            evidence: Box::new(PartialProcessEvidence {
                status: None,
                stdout: Captured::Spool {
                    file: source,
                    length: 24,
                },
                stderr: Captured::Unavailable,
                stdout_complete: false,
                stderr_complete: false,
                log: None,
            }),
        };
        let (error, paths) = retain_error_evidence(error);
        assert_eq!(paths.len(), 1);
        assert_eq!(
            std::fs::read(&paths[0]).unwrap(),
            b"protected failure prefix"
        );
        assert!(!format!("{error:?}").contains("protected"));
        drop(error);
        assert!(!source_path.exists());
        assert!(paths[0].exists());
        // This test owns its retained fixture file, not any session lifecycle.
        std::fs::remove_file(&paths[0]).unwrap();
    }
}
