//! Thin invocation construction and joined shutdown. Operation adapters are
//! deliberately absent at this milestone; no Ruby fallback is hidden here.

mod diagnostic;
mod invocation_log;
mod signals;

use invocation_log::InvocationLog;

use crate::{
    cli::{self, CliDisplay, ParseOutcome},
    domain::{Action, ExitDisposition, Invocation},
    error::ConfctlError,
    events::EventSink,
    presentation::{OutputTargets, OutputWriter, Presentation, PresentationConfig},
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

type SharedWriter = OutputWriter;

/// Capture clocks at executable entry, before parsing or environment setup.
pub async fn run(argv: Vec<std::ffi::OsString>, timing: Timing) -> i32 {
    execute(
        argv,
        timing,
        &crate::operations::CoreOperations,
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
    let command = format!(
        "confctl {}",
        argv.iter()
            .map(|arg| arg.to_string_lossy())
            .collect::<Vec<_>>()
            .join(" ")
    );
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
    let (invocation_log, process_log) = match InvocationLog::open(&cwd, invocation.action.name()) {
        Ok(log) => log,
        Err(error) => return diagnose(&error, &stderr),
    };
    let spool = match tempfile::tempdir() {
        Ok(spool) => spool,
        Err(source) => {
            drop(process_log);
            return report_error(
                ConfctlError::Io {
                    operation: "create invocation spool directory",
                    path: None,
                    source,
                },
                Some(invocation_log),
                &stderr,
            );
        }
    };
    timing.admit(&invocation.action);
    let cancellation = CancellationToken::new();
    let signal_shutdown = CancellationToken::new();
    let (suspend_sender, suspend_receiver) = tokio::sync::mpsc::channel(1);
    let signal_owner = match signals::SignalOwner::new(
        suspend_receiver,
        cancellation.clone(),
        signal_shutdown.clone(),
    ) {
        Ok(owner) => owner,
        Err(error) => {
            drop(process_log);
            return report_error(error, Some(invocation_log), &stderr);
        }
    };
    let (events, receiver) = EventSink::channel(64);
    let ui = match Ui::new(
        io::stdin(),
        events.clone(),
        cancellation.clone(),
        Some(timing.clone()),
    ) {
        Ok(ui) => ui.with_foreground_suspend(suspend_sender),
        Err(error) => {
            drop(process_log);
            return report_error(error, Some(invocation_log), &stderr);
        }
    };
    let config = PresentationConfig::for_process(
        command,
        invocation.action.name().into(),
        invocation.global.color,
        spool.path().into(),
    );
    if let Err(error) = ui.set_live(config.policy.live) {
        drop(process_log);
        return report_error(
            error.with_secondary(ui.shutdown().err().into_iter().collect()),
            Some(invocation_log),
            &stderr,
        );
    }
    let (presentation_control, presentation) = match Presentation::new(
        receiver,
        ui.observe_state(),
        OutputTargets {
            stdout: stdout.clone(),
            stderr: stderr.clone(),
        },
        config,
        Some(timing.clone()),
    ) {
        Ok(runtime) => runtime,
        Err(error) => {
            drop(process_log);
            return report_error(
                error.with_secondary(ui.shutdown().err().into_iter().collect()),
                Some(invocation_log),
                &stderr,
            );
        }
    };
    let processes = Supervisor::new(
        events.clone(),
        spool.path().into(),
        1024 * 1024,
        Duration::from_secs(2),
        Some(process_log),
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
    let mut renderer = tokio::spawn(presentation.run());
    let signal_cancel = cancellation.clone();
    let signal = tokio::spawn(async move {
        let result = signal_owner.serve().await;
        if result.is_err() {
            signal_cancel.cancel();
        }
        result
    });
    let mut operation = Box::pin(operations.run(&invocation, &context));
    let mut renderer_early = None;
    let result = tokio::select! {biased;
        result=&mut operation=>Some(result),
        joined=&mut renderer=>{
            cancellation.cancel();renderer_early=Some(joined);None
        },
    };
    // A failed presentation cannot leave operations awaiting acknowledgements.
    // Dropping the caller preserves process/input errors for checked shutdown.
    drop(operation);
    // Never cancel/drop shutdown futures: they carry owned IO and evidence.
    let mut cleanup = Vec::new();
    // Release queued and blocked suspension callers before any checked join.
    // A worker may still need this response while restoring its child group.
    signal_shutdown.cancel();
    match signal.await {
        Ok(Ok(())) => {}
        Ok(Err(error)) => cleanup.push(error),
        Err(error) => cleanup.push(ConfctlError::EventDelivery(error.to_string())),
    }
    if let Err(error) = context.processes.shutdown().await {
        cleanup.push(error);
    }
    if let Err(error) = context.ui.shutdown() {
        cleanup.push(error);
    }
    drop(context);
    let early = renderer_early.is_some();
    let (finished, joined) = if let Some(joined) = renderer_early {
        // An aborted/failed loop has joined; checked finish owns its evidence.
        (presentation_control.finish().await, joined)
    } else {
        // Normal completion is driven by finish, so never await run first.
        tokio::join!(presentation_control.finish(), &mut renderer)
    };
    let rendered = presentation_result(finished, joined);
    let result = match result {
        Some(result) => {
            if let Some(error) = rendered {
                cleanup.push(error);
            }
            result
        }
        None => Err(rendered.unwrap_or_else(|| {
            ConfctlError::EventDelivery(
                if early {
                    "presentation completed before operation shutdown"
                } else {
                    "presentation completion source unavailable"
                }
                .into(),
            )
        })),
    };
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
            let exit = if error.is_cancelled() {
                ExitDisposition::canceled(130)
            } else {
                ExitDisposition::failed(1)
            };
            (exit, Some(error.with_secondary(cleanup)))
        }
    };
    if let Some(error) = error {
        report_error(error, Some(invocation_log), &stderr);
    } else if let Err(failure) = invocation_log.finish(None, false) {
        disposition = ExitDisposition::failed(1);
        report_retained_error(failure.source, failure.locator, false, &stderr);
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
    // Parser/setup diagnosis has no invocation log. Writes still propagate to
    // this nonzero disposition instead of ever producing false success.
    if let Ok(mut writer) = stderr.lock()
        && diagnostic::write_redacted(&mut *writer, error).is_err()
    {
        return 1;
    }
    1
}

fn report_error(mut error: ConfctlError, log: Option<InvocationLog>, stderr: &SharedWriter) -> i32 {
    let mut covered = false;
    let mut locator = log.as_ref().map(|log| log.path().to_owned());
    if let Some(log) = log {
        match log.finish(Some(&error), true) {
            Ok(()) => covered = true,
            Err(failure) => {
                locator = failure.locator;
                error = error.with_secondary(vec![failure.source]);
            }
        }
    }
    report_retained_error(error, locator, covered, stderr)
}
fn report_retained_error(
    error: ConfctlError,
    locator: Option<PathBuf>,
    covered: bool,
    stderr: &SharedWriter,
) -> i32 {
    let (error, paths) = retain_error_evidence_with_coverage(error, covered);
    let result = (|| {
        let mut writer = stderr
            .lock()
            .map_err(|_| io::Error::other("output writer poisoned"))?;
        if let Some(path) = &locator {
            writer.write_all(b"\nLog file: ")?;
            writer.write_all(path.as_os_str().as_encoded_bytes())?;
            writer.write_all(b"\n")?;
        }
        diagnostic::write_error(&mut *writer, &error)?;
        for path in paths {
            writer.write_all(b"Process evidence: ")?;
            writer.write_all(path.as_os_str().as_encoded_bytes())?;
            writer.write_all(b"\n")?;
        }
        writer.flush()
    })();
    if let Err(source) = result {
        // Already nonzero; keep primary provenance and attempt the checked
        // redacted fallback. Retained files/log outlive both error owners.
        let error = error.with_secondary(vec![ConfctlError::Io {
            operation: "write failure diagnostic",
            path: None,
            source,
        }]);
        diagnose(&error, stderr);
    }
    1
}

/// Task provenance distinguishes the run notification from the checked source.
/// Never report the same presentation failure twice or drop its owned captures.
fn presentation_result(
    finished: Result<(), ConfctlError>,
    joined: Result<Result<(), ConfctlError>, tokio::task::JoinError>,
) -> Option<ConfctlError> {
    match (finished, joined) {
        (Err(error), Err(join)) => {
            Some(error.with_secondary(vec![ConfctlError::EventDelivery(join.to_string())]))
        }
        (Err(error), _) => Some(error),
        (Ok(()), Ok(Err(notification))) => Some(notification),
        (Ok(()), Err(join)) => Some(ConfctlError::EventDelivery(join.to_string())),
        (Ok(()), Ok(Ok(()))) => None,
    }
}

/// Materialize owned captures before dropping temporary process evidence. The
/// retained directory is deliberately kept on failures; no cleanup is scheduled.
#[cfg(test)]
fn retain_error_evidence(error: ConfctlError) -> (ConfctlError, Vec<PathBuf>) {
    retain_error_evidence_with_coverage(error, false)
}

fn retain_error_evidence_with_coverage(
    mut error: ConfctlError,
    process_covered: bool,
) -> (ConfctlError, Vec<PathBuf>) {
    let mut paths = Vec::new();
    let mut failures = Vec::new();
    collect_evidence(&mut error, &mut paths, &mut failures, process_covered);
    (error.with_secondary(failures), paths)
}

fn collect_evidence(
    error: &mut ConfctlError,
    paths: &mut Vec<PathBuf>,
    failures: &mut Vec<ConfctlError>,
    process_covered: bool,
) {
    match error {
        ConfctlError::NixCommandFailure(failure) => {
            collect_evidence(&mut failure.source, paths, failures, process_covered)
        }
        ConfctlError::OutputFailure { source, pending } => {
            let mut retained = serde_json::Map::new();
            for (stream, capture, replayed) in [
                ("stdout", &pending.stdout, pending.stdout_replayed),
                ("stderr", &pending.stderr, pending.stderr_replayed),
            ] {
                let path = if matches!(capture, Captured::Unavailable) {
                    None
                } else {
                    match retain_capture(&format!("pending-{stream}"), capture) {
                        Ok(path) => {
                            paths.push(path.clone());
                            Some(path)
                        }
                        Err(source) => {
                            failures.push(ConfctlError::Io {
                                operation: "retain pending output",
                                path: None,
                                source,
                            });
                            None
                        }
                    }
                };
                retained.insert(stream.into(),serde_json::json!({"path":path,"confirmed_replayed":replayed,"captured_length":capture.len()}));
            }
            if let Some(chunk) = &pending.unspooled {
                let path = match retain_capture("pending-unspooled", &chunk.bytes) {
                    Ok(path) => {
                        paths.push(path.clone());
                        Some(path)
                    }
                    Err(source) => {
                        failures.push(ConfctlError::Io {
                            operation: "retain unspooled output chunk",
                            path: None,
                            source,
                        });
                        None
                    }
                };
                retained.insert("unspooled".into(),serde_json::json!({"path":path,"stream":chunk.stream.name(),"spool_offset":chunk.spool_offset,"overlap_possible":true}));
            }
            retained.insert(
                "verified_log".into(),
                serde_json::json!(pending.verified_log),
            );
            retained.insert("automatic_replay".into(), serde_json::Value::Bool(false));
            let metadata = Captured::Memory(
                serde_json::to_vec_pretty(&retained).expect("output metadata serializable"),
            );
            match retain_capture("pending-offsets-json", &metadata) {
                Ok(path) => paths.push(path),
                Err(source) => failures.push(ConfctlError::Io {
                    operation: "retain pending output offsets",
                    path: None,
                    source,
                }),
            }
            collect_evidence(source, paths, failures, process_covered);
        }
        ConfctlError::ProcessFailure { source, evidence } => {
            for (name, capture) in [
                ("stdout", &mut evidence.stdout),
                ("stderr", &mut evidence.stderr),
            ] {
                if process_covered {
                    continue;
                }
                if matches!(capture, Captured::Unavailable) {
                    continue;
                }
                match retain_capture(name, capture) {
                    Ok(path) => paths.push(path),
                    Err(source) => {
                        // A copy failure must not delete its original spool.
                        let path = if let Captured::Spool { file, .. } = capture {
                            file.disable_cleanup(true);
                            let path = file.path().to_owned();
                            paths.push(path.clone());
                            Some(path)
                        } else {
                            None
                        };
                        failures.push(ConfctlError::Io {
                            operation: "retain partial process evidence",
                            path,
                            source,
                        });
                    }
                }
            }
            collect_evidence(source, paths, failures, process_covered);
        }
        ConfctlError::Cleanup { primary, secondary } => {
            collect_evidence(primary, paths, failures, process_covered);
            for error in secondary {
                collect_evidence(error, paths, failures, process_covered);
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
mod diagnostic_boundary_tests {
    use super::*;
    use crate::process::PartialProcessEvidence;

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
    fn outputs() -> (SharedWriter, SharedWriter, Buffer) {
        let output = Buffer(Arc::new(Mutex::new(vec![])));
        (
            Arc::new(Mutex::new(Box::new(io::sink()))),
            Arc::new(Mutex::new(Box::new(output.clone()))),
            output,
        )
    }
    struct Failing;
    impl Operations for Failing {
        fn available(&self, _: &Action) -> bool {
            true
        }
        async fn run(
            &self,
            _: &Invocation,
            context: &Context,
        ) -> Result<ExitDisposition, ConfctlError> {
            let directory = context.cwd.join(".confctl/logs");
            assert_eq!(
                std::fs::read_dir(directory).unwrap().count(),
                1,
                "log must precede operation/defaults"
            );
            Err(diagnostic::tests::failure())
        }
    }
    const LOG_CHILD: &str = "CONFCTL_LOG_FINALIZATION_CHILD";
    #[test]
    fn successful_operation_log_child_fixture() {
        if std::env::var_os(LOG_CHILD).is_some() {
            println!("protected log child bytes");
        }
    }
    struct SuccessBeforeFinalizationFailure;
    impl Operations for SuccessBeforeFinalizationFailure {
        fn available(&self, _: &Action) -> bool {
            true
        }
        async fn run(
            &self,
            _: &Invocation,
            context: &Context,
        ) -> Result<ExitDisposition, ConfctlError> {
            use crate::process::{ProcessRunner, ProcessScope, ProcessSpec};
            let mut spec = ProcessSpec::new(std::env::current_exe().unwrap(), "owned log fixture");
            spec.args = [
                "--exact",
                "app::diagnostic_boundary_tests::successful_operation_log_child_fixture",
                "--nocapture",
            ]
            .map(Into::into)
            .to_vec();
            spec.env.push((LOG_CHILD.into(), Some("1".into())));
            let outcome = context
                .processes
                .run(
                    spec,
                    ProcessScope::new(context.cancellation.clone(), crate::events::TaskId(0)),
                )
                .await?;
            assert!(outcome.status.success());
            let path = std::fs::read_dir(context.cwd.join(".confctl/logs"))
                .unwrap()
                .next()
                .unwrap()
                .unwrap()
                .path();
            invocation_log::fail_finalization_once(path);
            Ok(ExitDisposition::completed())
        }
    }
    #[tokio::test]
    async fn successful_operation_with_finalization_failure_reports_existing_log_and_exits_one() {
        let scratch = tempfile::tempdir().unwrap();
        let (out, err, buffer) = outputs();
        let code = execute(
            vec!["build".into()],
            Timing::start(),
            &SuccessBeforeFinalizationFailure,
            Ok(scratch.path().into()),
            out,
            err,
        )
        .await;
        assert_eq!(code, 1);
        let bytes = buffer.0.lock().unwrap().clone();
        let text = String::from_utf8(bytes).unwrap();
        let locator = text
            .lines()
            .find_map(|s| s.strip_prefix("Log file: "))
            .expect("opened log locator lost on finalization failure");
        let log = std::fs::read(locator).unwrap();
        assert!(
            log.windows(b"protected log child bytes".len())
                .any(|s| s == b"protected log child bytes")
        );
        assert!(text.contains("error: finalize invocation log: injected finalization failure\n"));
        assert_eq!(text.matches("Timing: failed (exit 1)").count(), 1);
        assert!(!text.contains("Timing: completed"));
        assert!(
            text.find("error: finalize invocation log").unwrap()
                < text.find("Timing: failed").unwrap()
        );
    }
    #[tokio::test]
    async fn failure_keeps_verified_invocation_log_and_suppresses_only_process_locators() {
        let scratch = tempfile::tempdir().unwrap();
        let (out, err, buffer) = outputs();
        assert_eq!(
            execute(
                vec!["ls".into()],
                Timing::start(),
                &Failing,
                Ok(scratch.path().into()),
                out,
                err
            )
            .await,
            1
        );
        let bytes = buffer.0.lock().unwrap().clone();
        assert!(bytes.starts_with(b"\nLog file: "));
        assert!(
            !bytes
                .windows(b"Process evidence:".len())
                .any(|s| s == b"Process evidence:")
        );
        assert!(
            bytes
                .windows(b"error: Running `".len())
                .any(|s| s == b"error: Running `")
        );
        let logs = std::fs::read_dir(scratch.path().join(".confctl/logs"))
            .unwrap()
            .collect::<Result<Vec<_>, _>>()
            .unwrap();
        assert_eq!(logs.len(), 1);
        let logged = std::fs::read(logs[0].path()).unwrap();
        assert!(
            logged
                .windows(b" \0failure\xff \n".len())
                .any(|s| s == b" \0failure\xff \n")
        );
    }
    #[tokio::test]
    async fn parser_help_unavailable_and_cwd_failure_do_not_open_logs() {
        let scratch = tempfile::tempdir().unwrap();
        for argv in [vec!["--help"], vec!["--unknown"]] {
            let (out, err, _) = outputs();
            execute(
                argv.into_iter().map(Into::into).collect(),
                Timing::start(),
                &Failing,
                Ok(scratch.path().into()),
                out,
                err,
            )
            .await;
            assert!(!scratch.path().join(".confctl").exists());
        }
        let (out, err, _) = outputs();
        execute(
            vec!["ls".into()],
            Timing::start(),
            &PendingOperations,
            Ok(scratch.path().into()),
            out,
            err,
        )
        .await;
        assert!(!scratch.path().join(".confctl").exists());
        let (out, err, buffer) = outputs();
        assert_eq!(
            execute(
                vec!["ls".into()],
                Timing::start(),
                &Failing,
                Err(io::ErrorKind::NotFound.into()),
                out,
                err
            )
            .await,
            1
        );
        assert!(
            !buffer
                .0
                .lock()
                .unwrap()
                .windows(9)
                .any(|s| s == b"Log file:")
        );
    }
    #[tokio::test]
    async fn unverified_log_keeps_fallback_capture_and_original_primary() {
        let scratch = tempfile::tempdir().unwrap();
        let (log, process) = InvocationLog::open(scratch.path(), "ls").unwrap();
        drop(process);
        std::fs::remove_file(log.path()).unwrap();
        let (_, err, buffer) = outputs();
        report_error(diagnostic::tests::failure(), Some(log), &err);
        let bytes = String::from_utf8_lossy(&buffer.0.lock().unwrap()).into_owned();
        assert!(bytes.contains("exit status: 7"));
        assert!(bytes.contains("cleanup: verify finalized invocation log:"));
        assert!(
            !bytes.contains("Log file:"),
            "a missing path is not a retained log locator"
        );
        let paths = bytes
            .lines()
            .filter_map(|s| s.strip_prefix("Process evidence: "))
            .map(PathBuf::from)
            .collect::<Vec<_>>();
        assert_eq!(paths.len(), 2);
        assert_eq!(std::fs::read(&paths[1]).unwrap(), b" \0failure\xff \n");
        for path in paths {
            std::fs::remove_file(path).unwrap();
        }
    }
    #[test]
    fn verified_process_log_does_not_cover_pending_presentation_output() {
        let error = ConfctlError::OutputFailure {
            source: Box::new(ConfctlError::ProcessFailure {
                source: Box::new(ConfctlError::InputEof),
                evidence: Box::new(PartialProcessEvidence {
                    status: None,
                    stdout: Captured::Memory(b"process".to_vec()),
                    stderr: Captured::Unavailable,
                    stdout_complete: true,
                    stderr_complete: false,
                    log: None,
                }),
            }),
            pending: Box::new(crate::presentation::PendingOutput {
                stdout: Captured::Memory(b"presentation".to_vec()),
                stderr: Captured::Unavailable,
                stdout_replayed: 3,
                stderr_replayed: 0,
                verified_log: None,
                unspooled: None,
            }),
        };
        let (_, paths) = retain_error_evidence_with_coverage(error, true);
        assert_eq!(paths.len(), 2);
        assert_eq!(std::fs::read(&paths[0]).unwrap(), b"presentation");
        for path in paths {
            std::fs::remove_file(path).unwrap();
        }
    }
    #[test]
    fn failed_diagnostic_write_is_reported_after_original_primary() {
        #[derive(Clone)]
        struct FailOnce {
            bytes: Arc<Mutex<Vec<u8>>>,
            failed: Arc<Mutex<bool>>,
        }
        impl Write for FailOnce {
            fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
                let mut failed = self.failed.lock().unwrap();
                if !*failed {
                    *failed = true;
                    Err(io::ErrorKind::BrokenPipe.into())
                } else {
                    self.bytes.lock().unwrap().extend_from_slice(bytes);
                    Ok(bytes.len())
                }
            }
            fn flush(&mut self) -> io::Result<()> {
                Ok(())
            }
        }
        let bytes = Arc::new(Mutex::new(vec![]));
        let writer: SharedWriter = Arc::new(Mutex::new(Box::new(FailOnce {
            bytes: bytes.clone(),
            failed: Arc::new(Mutex::new(false)),
        })));
        report_error(ConfctlError::InputEof, None, &writer);
        let text = String::from_utf8(bytes.lock().unwrap().clone()).unwrap();
        assert!(text.starts_with("error: end of input\ncleanup: write failure diagnostic:"));
    }
    #[test]
    fn shortened_capture_retains_known_prefix_without_log_coverage() {
        let file = tempfile::NamedTempFile::new().unwrap();
        let path = file.path().to_owned();
        let error = ConfctlError::ProcessFailure {
            source: Box::new(ConfctlError::InputEof),
            evidence: Box::new(PartialProcessEvidence {
                status: None,
                stdout: Captured::Spool { file, length: 1 },
                stderr: Captured::Unavailable,
                stdout_complete: false,
                stderr_complete: false,
                log: None,
            }),
        };
        let (error, paths) = retain_error_evidence(error);
        assert_eq!(error.to_string(), "end of input");
        // A shortened source still survives as a conservative prefix. No log
        // coverage or complete-child formatter is inferred from this locator.
        assert_eq!(paths.len(), 1);
        drop(error);
        assert!(!path.exists());
        assert_eq!(std::fs::read(&paths[0]).unwrap(), b"");
        std::fs::remove_file(&paths[0]).unwrap();
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::events::{OutputStream, UiEvent};

    struct PartialFailure(bool);
    impl Write for PartialFailure {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            if !self.0 {
                self.0 = true;
                Ok(bytes.len().min(2))
            } else {
                Err(io::Error::new(
                    io::ErrorKind::BrokenPipe,
                    "owned output failure",
                ))
            }
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    #[tokio::test]
    async fn presentation_failure_joins_and_retains_partial_replay_without_duplicate_notification()
    {
        let scratch = tempfile::tempdir().unwrap();
        let err = Buffer(Arc::new(Mutex::new(vec![])));
        let code = execute(
            vec!["build".into()],
            Timing::start(),
            &FakeOperation,
            Ok(scratch.path().into()),
            Arc::new(Mutex::new(Box::new(PartialFailure(false)))),
            Arc::new(Mutex::new(Box::new(err.clone()))),
        )
        .await;
        assert_eq!(code, 1);
        let stderr = String::from_utf8(err.0.lock().unwrap().clone()).unwrap();
        assert!(!stderr.contains("checked finish retains its source"));
        let paths = stderr
            .lines()
            .filter_map(|line| line.strip_prefix("Process evidence: "))
            .map(PathBuf::from)
            .collect::<Vec<_>>();
        assert_eq!(paths.len(), 3);
        assert!(
            paths
                .iter()
                .any(|path| std::fs::read(path).unwrap() == b"operation result\n")
        );
        let metadata = paths
            .iter()
            .find(|path| {
                path.file_name()
                    .unwrap()
                    .as_encoded_bytes()
                    .windows(b"pending-offsets-json".len())
                    .any(|part| part == b"pending-offsets-json")
            })
            .unwrap();
        let value: serde_json::Value =
            serde_json::from_slice(&std::fs::read(metadata).unwrap()).unwrap();
        assert_eq!(value["stdout"]["confirmed_replayed"], 2);
        assert_eq!(stderr.matches("Timing: failed (exit 1)").count(), 1);
        for path in paths {
            std::fs::remove_file(path).unwrap();
        }
    }

    struct ParkedOperation;
    impl Operations for ParkedOperation {
        fn available(&self, _: &Action) -> bool {
            true
        }
        async fn run(
            &self,
            _: &Invocation,
            context: &Context,
        ) -> Result<ExitDisposition, ConfctlError> {
            let lease = context.ui.foreground().await?;
            context
                .events
                .emit(UiEvent::Output {
                    task: crate::events::TaskId(0),
                    stream: OutputStream::Stdout,
                    bytes: b"parked required output\n".to_vec(),
                })
                .await?;
            drop(lease);
            Ok(ExitDisposition::completed())
        }
    }
    #[tokio::test]
    async fn checked_finish_replays_parked_output_before_success_and_footer() {
        let scratch = tempfile::tempdir().unwrap();
        let out = Buffer(Arc::new(Mutex::new(vec![])));
        let err = Buffer(Arc::new(Mutex::new(vec![])));
        let code = execute(
            vec!["build".into()],
            Timing::start(),
            &ParkedOperation,
            Ok(scratch.path().into()),
            Arc::new(Mutex::new(Box::new(out.clone()))),
            Arc::new(Mutex::new(Box::new(err.clone()))),
        )
        .await;
        assert_eq!(code, 0);
        assert_eq!(*out.0.lock().unwrap(), b"parked required output\n");
        assert_eq!(
            String::from_utf8(err.0.lock().unwrap().clone())
                .unwrap()
                .matches("Timing: completed (exit 0)")
                .count(),
            1
        );
    }

    struct WrappedCancellation;
    impl Operations for WrappedCancellation {
        fn available(&self, _: &Action) -> bool {
            true
        }
        async fn run(&self, _: &Invocation, _: &Context) -> Result<ExitDisposition, ConfctlError> {
            Err(ConfctlError::ProcessFailure {
                source: Box::new(ConfctlError::Cancelled),
                evidence: Box::new(PartialProcessEvidence {
                    status: None,
                    stdout: Captured::Unavailable,
                    stderr: Captured::Unavailable,
                    stdout_complete: false,
                    stderr_complete: false,
                    log: None,
                }),
            })
        }
    }
    #[tokio::test]
    async fn wrapped_process_cancellation_exits130_after_checked_shutdown() {
        let scratch = tempfile::tempdir().unwrap();
        let err = Buffer(Arc::new(Mutex::new(vec![])));
        let out = Buffer(Arc::new(Mutex::new(vec![])));
        let code = execute(
            vec!["build".into()],
            Timing::start(),
            &WrappedCancellation,
            Ok(scratch.path().into()),
            Arc::new(Mutex::new(Box::new(out))),
            Arc::new(Mutex::new(Box::new(err.clone()))),
        )
        .await;
        assert_eq!(code, 130);
        assert_eq!(
            String::from_utf8(err.0.lock().unwrap().clone())
                .unwrap()
                .matches("Timing: canceled (exit 130)")
                .count(),
            1
        );
    }
    use crate::process::PartialProcessEvidence;

    #[test]
    fn pending_output_retains_offsets_and_unspooled_chunk_without_replaying() {
        use crate::presentation::{PendingOutput, UnspooledOutput};
        let error = ConfctlError::OutputFailure {
            source: Box::new(ConfctlError::EventDelivery("output flush failed".into())),
            pending: Box::new(PendingOutput {
                stdout: Captured::Memory(b"protected prefix".to_vec()),
                stderr: Captured::Unavailable,
                stdout_replayed: 5,
                stderr_replayed: 0,
                verified_log: None,
                unspooled: Some(UnspooledOutput {
                    stream: OutputStream::Stdout,
                    bytes: Captured::Memory(b"protected chunk".to_vec()),
                    spool_offset: 16,
                }),
            }),
        };
        let expected = error.to_string();
        let (error, paths) = retain_error_evidence(error);
        assert_eq!(error.to_string(), expected);
        assert!(!format!("{error:?}").contains("protected"));
        assert_eq!(paths.len(), 3);
        let metadata: serde_json::Value =
            serde_json::from_slice(&std::fs::read(paths.last().unwrap()).unwrap()).unwrap();
        assert_eq!(metadata["stdout"]["confirmed_replayed"], 5);
        assert_eq!(metadata["unspooled"]["spool_offset"], 16);
        assert_eq!(metadata["unspooled"]["overlap_possible"], true);
        assert_eq!(metadata["automatic_replay"], false);
        assert_eq!(std::fs::read(&paths[0]).unwrap(), b"protected prefix");
        assert_eq!(std::fs::read(&paths[1]).unwrap(), b"protected chunk");
        drop(error);
        for path in paths {
            std::fs::remove_file(path).unwrap();
        }
    }

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
        let scratch = tempfile::tempdir().unwrap();
        let out = Buffer(Arc::new(Mutex::new(vec![])));
        let err = Buffer(Arc::new(Mutex::new(vec![])));
        let code = execute(
            vec!["build".into()],
            Timing::start(),
            &FakeOperation,
            Ok(scratch.path().into()),
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
