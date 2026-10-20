//! Lazy report paging through the existing process and terminal owners.
pub(super) use crate::process::WrittenOrClosed;
use crate::{
    app::Context,
    error::ConfctlError,
    events::{OutputStream, Style, TaskId, TaskState, UiEvent},
    process::{
        PagerInput, PagerWriter, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec,
        StdinPolicy, StreamPolicy, TerminationCause,
    },
};
use std::{
    ffi::{OsStr, OsString},
    future::Future,
    io::IsTerminal,
    os::{
        fd::{AsFd, OwnedFd},
        unix::ffi::OsStrExt,
    },
    pin::Pin,
};

type Request<'a> = Pin<Box<dyn Future<Output = Result<ProcessOutcome, ConfctlError>> + Send + 'a>>;

pub(super) struct Pager<'a> {
    context: &'a Context,
    task: TaskId,
    command: Option<OsString>,
    output_tty: Option<OwnedFd>,
    writer: Option<PagerWriter>,
    request: Option<Request<'a>>,
    result: Option<Result<ProcessOutcome, ConfctlError>>,
    admitted: bool,
    closed: bool,
    failed_write: bool,
}

impl<'a> Pager<'a> {
    /// Freeze eligibility and command selection without starting a child.
    pub(super) fn new(context: &'a Context, task: TaskId) -> Result<Self, ConfctlError> {
        let command = configured(
            std::io::stdout().is_terminal(),
            std::io::stderr().is_terminal(),
            std::env::var_os("CONFCTL_TTY").as_deref(),
            std::env::var_os("PAGER").as_deref(),
            std::env::var_os("GIT_PAGER").as_deref(),
        );
        let output_tty = if command.is_some() {
            Some(
                std::io::stdout()
                    .as_fd()
                    .try_clone_to_owned()
                    .map_err(|source| ConfctlError::Io {
                        operation: "duplicate pager output terminal",
                        path: None,
                        source,
                    })?,
            )
        } else {
            None
        };
        Ok(Self {
            context,
            task,
            command,
            output_tty,
            writer: None,
            request: None,
            result: None,
            admitted: false,
            closed: false,
            failed_write: false,
        })
    }

    async fn start(&mut self) -> Result<(), ConfctlError> {
        let description = "page report".to_owned();
        self.context
            .events
            .emit(UiEvent::TaskPending {
                task: self.task,
                description: description.clone(),
            })
            .await?;
        self.admitted = true;
        self.check_cancel()?;
        self.context
            .events
            .emit(UiEvent::TaskStarted {
                task: self.task,
                description,
            })
            .await?;
        self.check_cancel()?;
        let foreground = self
            .context
            .ui
            .pager_foreground(self.output_tty.take().expect("frozen output terminal"))
            .await?;
        self.check_cancel()?;
        let (writer, input) = PagerInput::channel();
        let mut spec = ProcessSpec::new("sh", "configured report pager");
        // Only this operator-configured string is shell code. Report bytes are stdin.
        spec.args = vec![
            "-c".into(),
            self.command.take().expect("frozen pager command"),
        ];
        spec.cwd = Some(self.context.cwd.clone());
        spec.stdin = StdinPolicy::Pager(input);
        spec.stdout = StreamPolicy::Foreground;
        spec.stderr = StreamPolicy::Foreground;
        let mut scope = ProcessScope::new(self.context.cancellation.clone(), self.task);
        scope.foreground = Some(foreground);
        self.writer = Some(writer);
        self.request = Some(Box::pin(self.context.processes.run(spec, scope)));
        Ok(())
    }

    pub(super) async fn write(&mut self, bytes: &[u8]) -> Result<WrittenOrClosed, ConfctlError> {
        let result = self.write_inner(bytes).await;
        if result.is_err() {
            self.failed_write = true;
        }
        result
    }

    async fn write_inner(&mut self, bytes: &[u8]) -> Result<WrittenOrClosed, ConfctlError> {
        self.check_cancel()?;
        if self.closed {
            return Ok(WrittenOrClosed::Closed);
        }
        if bytes.is_empty() {
            return Ok(WrittenOrClosed::Written);
        }
        if self.command.is_none() && !self.admitted {
            self.context
                .events
                .emit(UiEvent::Output {
                    task: self.task,
                    stream: OutputStream::Stdout,
                    bytes: bytes.to_vec(),
                })
                .await?;
            self.check_cancel()?;
            return Ok(WrittenOrClosed::Written);
        }
        if !self.admitted {
            self.start().await?;
        }
        let Some(request) = self.request.as_mut() else {
            return Err(ConfctlError::EventDelivery(
                "pager request has already finished".into(),
            ));
        };
        let writer = self.writer.as_mut().expect("started pager input");
        let result = tokio::select! {
            // A queued actual EPIPE acknowledgement is stronger than leader exit.
            biased;
            result = writer.write(bytes, &self.context.cancellation) => result,
            result = request => {
                self.result = Some(result);
                self.request = None;
                Err(ConfctlError::EventDelivery("pager request ended before a pipe-write acknowledgement".into()))
            }
        }?;
        self.check_cancel()?;
        if result == WrittenOrClosed::Closed {
            self.closed = true;
        }
        Ok(result)
    }

    /// Mandatory on every producer exit. Close input and join the same request.
    /// Ordinary native nonzero remains an outcome, with a failed pager task.
    pub(super) async fn finish(mut self) -> Result<Option<ProcessOutcome>, ConfctlError> {
        self.writer.take();
        let result = if let Some(result) = self.result.take() {
            result.map(Some)
        } else if let Some(request) = self.request.take() {
            request.await.map(Some)
        } else {
            Ok(None)
        };
        let result = match result {
            Ok(Some(outcome))
                if outcome.termination == TerminationCause::Canceled
                    || self.context.cancellation.is_cancelled() =>
            {
                Err(self.outcome_error(ConfctlError::Cancelled, outcome))
            }
            Ok(None) if self.context.cancellation.is_cancelled() => Err(ConfctlError::Cancelled),
            other => other,
        };
        if !self.admitted {
            return result;
        }
        let state = match &result {
            Err(error) if error.is_cancelled() => TaskState::Canceled,
            Err(_) => TaskState::Failed,
            Ok(Some(outcome)) if !outcome.status.success() || self.failed_write => {
                TaskState::Failed
            }
            Ok(_) if self.failed_write => TaskState::Failed,
            Ok(_) => TaskState::Succeeded,
        };
        let mut delivery_errors = Vec::new();
        if let Ok(Some(outcome)) = &result
            && !outcome.status.success()
        {
            self.context.keep_log();
            if let Err(error) = self.context.events.emit(UiEvent::Diagnostic {
                stream: OutputStream::Stderr, style: Style::Plain,
                text: format!("Pager exited with code {:?}, signal {:?}; report display may be incomplete.\n", outcome.code(), outcome.signal()),
            }).await { delivery_errors.push(error); }
        }
        if let Err(error) = self
            .context
            .events
            .emit(UiEvent::TaskFinished {
                task: self.task,
                state,
            })
            .await
        {
            delivery_errors.push(error);
        }
        match result {
            Err(error) => Err(error.with_secondary(delivery_errors)),
            Ok(outcome) if !delivery_errors.is_empty() => {
                let error = delivery_errors.remove(0).with_secondary(delivery_errors);
                Err(match outcome {
                    Some(outcome) => self.outcome_error(error, outcome),
                    None => error,
                })
            }
            Ok(outcome) if self.context.cancellation.is_cancelled() => Err(match outcome {
                Some(outcome) => self.outcome_error(ConfctlError::Cancelled, outcome),
                None => ConfctlError::Cancelled,
            }),
            Ok(outcome) => Ok(outcome),
        }
    }

    fn outcome_error(&self, error: ConfctlError, outcome: ProcessOutcome) -> ConfctlError {
        self.context.keep_log();
        let error = if !outcome.status.success() && !error.is_cancelled() {
            ConfctlError::Child {
                description: "configured report pager".into(),
                code: outcome.code(),
                signal: outcome.signal(),
                log: Some(self.context.log_path().into()),
            }
            .with_secondary(vec![error])
        } else {
            error
        };
        let mut error = crate::nix::process_error(error, outcome);
        if let ConfctlError::ProcessFailure { evidence, .. } = &mut error {
            evidence.log = Some(self.context.log_path().into());
        }
        error
    }

    fn check_cancel(&self) -> Result<(), ConfctlError> {
        if self.context.cancellation.is_cancelled() {
            Err(ConfctlError::Cancelled)
        } else {
            Ok(())
        }
    }
}

fn nonblank(value: &OsStr) -> bool {
    value
        .as_bytes()
        .iter()
        .any(|byte| !matches!(byte, 0 | b' ' | b'\t' | b'\n' | b'\r' | 11 | 12))
}
fn configured(
    stdout_tty: bool,
    stderr_tty: bool,
    tty: Option<&OsStr>,
    pager: Option<&OsStr>,
    git_pager: Option<&OsStr>,
) -> Option<OsString> {
    if !stdout_tty || !stderr_tty || tty == Some(OsStr::new("0")) || !pager.is_some_and(nonblank) {
        return None;
    }
    git_pager
        .filter(|value| nonblank(value))
        .or(pager)
        .map(OsStr::to_os_string)
}

#[cfg(test)]
mod tests;
