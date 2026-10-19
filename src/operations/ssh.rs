//! Requested SSH commands share the process supervisor and terminal owner.

use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{Action, Machine, SelectionPolicy, SshInput, SshRequest, select},
    error::ConfctlError,
    events::{EventSink, OutputStream, Style, TaskId, TaskState, UiEvent},
    nix::{InvocationSettings, NixClient, NixOptions},
    process::{
        Captured, PartialProcessEvidence, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec,
        StdinPolicy, StreamPolicy, TerminationCause,
    },
    terminal::{ConfirmPrompt, TerminalState, Ui},
    timing::Phase,
};
use std::{
    ffi::OsStr,
    future::{Future, poll_fn},
    io,
    path::{Path, PathBuf},
    sync::atomic::{AtomicUsize, Ordering},
    task::Poll,
};
use tokio_util::sync::CancellationToken;

pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    context: &Context,
) -> Result<(), ConfctlError> {
    let Action::Ssh(request) = action else {
        unreachable!("SSH adapter receives its typed action")
    };
    let defaults = InvocationSettings::new(
        root.clone(),
        context.processes.clone(),
        context.cancellation.clone(),
    );
    let nix = NixClient::configured(
        root.clone(),
        context.processes.clone(),
        NixOptions::default(),
        context.cancellation.clone(),
        &defaults,
        ConfigEpoch::default(),
    )
    .await?;
    let inventory = nix.inventory().await?;
    let inventory = inventory.require_epoch(nix.epoch())?;
    let selected = select(
        inventory,
        &request.targets.selection,
        SelectionPolicy {
            managed: request.managed,
            runnable: true,
            ..SelectionPolicy::ALL
        },
    )?;
    if selected.machines.is_empty() {
        return Err(ConfctlError::Selection("No machines to ssh to".into()));
    }
    let interactive = request.command.is_empty();
    if interactive && selected.machines.len() != 1 {
        return Err(ConfctlError::Usage("missing command".into()));
    }
    let settings = defaults.read().await?;
    let mut preview = vec![
        UiEvent::Diagnostic {
            stream: OutputStream::Stdout,
            style: Style::Plain,
            text: if interactive {
                "Open interactive shell on the following machine:\n"
            } else {
                "Run command over SSH on the following machines:\n"
            }
            .into(),
        },
        super::machines::table(inventory, &selected, &settings.list_columns, true),
    ];
    if !interactive {
        let command = super::connection::shell_join(&request.command);
        preview.push(UiEvent::Diagnostic {
            stream: OutputStream::Stdout,
            style: Style::Plain,
            text: format!("\nCommand: {}\n", command.to_string_lossy()),
        });
    }
    if !context
        .ui
        .confirm(ConfirmPrompt {
            preview,
            assume_yes: request.targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    let machines = selected
        .machines
        .iter()
        .map(|id| inventory.get(*id))
        .collect::<Vec<_>>();
    let config = std::env::var_os("CONFCTL_SSH_CONFIG");
    let execution = Execution {
        runner: &context.processes,
        events: &context.events,
        cancellation: &context.cancellation,
        root: root.path(),
        config: config.as_deref(),
        live: *context.ui.observe_state().borrow() == TerminalState::Live,
    };
    if interactive {
        interactive_shell(&execution, &context.ui, machines[0], root_shell).await
    } else {
        commands(&execution, &machines, request).await
    }
}

fn root_shell() -> Result<PathBuf, ConfctlError> {
    nix::unistd::User::from_uid(nix::unistd::Uid::from_raw(0))
        .map_err(|source| ConfctlError::Io {
            operation: "look up root shell",
            path: None,
            source: source.into(),
        })?
        .map(|user| user.shell)
        .ok_or_else(|| ConfctlError::Configuration("UID 0 has no passwd entry".into()))
}

fn interactive_spec(
    machine: &Machine,
    config: Option<&OsStr>,
    lookup: impl FnOnce() -> Result<PathBuf, ConfctlError>,
) -> Result<ProcessSpec, ConfctlError> {
    let local = match machine.metadata.get("host") {
        None => machine.name == "localhost",
        Some(serde_json::Value::Object(host)) => host
            .get("target")
            .map_or(machine.name == "localhost", |target| {
                target.as_str() == Some("localhost")
            }),
        _ => false,
    };
    let mut spec = if local {
        ProcessSpec::new(lookup()?, description(machine))
    } else {
        super::connection::command_spec(machine, config, &[])?
    };
    spec.redacted_description = description(machine);
    spec.stdin = StdinPolicy::Foreground;
    spec.stdout = StreamPolicy::Foreground;
    spec.stderr = StreamPolicy::Foreground;
    Ok(spec)
}

struct Execution<'a, P> {
    runner: &'a P,
    events: &'a EventSink,
    cancellation: &'a CancellationToken,
    root: &'a Path,
    config: Option<&'a OsStr>,
    live: bool,
}

async fn interactive_shell<P: ProcessRunner>(
    execution: &Execution<'_, P>,
    ui: &Ui,
    machine: &Machine,
    lookup: impl FnOnce() -> Result<PathBuf, ConfctlError>,
) -> Result<(), ConfctlError> {
    let Execution {
        runner,
        events,
        cancellation,
        root,
        config,
        ..
    } = execution;
    let task = TaskId(1);
    check_cancel(cancellation)?;
    events
        .emit(UiEvent::TaskPending {
            task,
            description: description(machine),
        })
        .await?;
    let result = async {
        check_cancel(cancellation)?;
        events
            .emit(UiEvent::TaskStarted {
                task,
                description: description(machine),
            })
            .await?;
        check_cancel(cancellation)?;
        let mut spec = interactive_spec(machine, *config, lookup)?;
        spec.cwd = Some((*root).into());
        let lease = ui.foreground().await?;
        check_cancel(cancellation)?;
        let mut scope = ProcessScope::new((*cancellation).clone(), task);
        scope.foreground = Some(lease);
        scope.phase = Some(Phase::SshCommands);
        let outcome = runner.run(spec, scope).await?;
        if outcome.termination == TerminationCause::Canceled {
            return Err(ConfctlError::Cancelled);
        }
        Ok(outcome.status.success())
    }
    .await;
    let state = match &result {
        Ok(true) => TaskState::Succeeded,
        Ok(false) => TaskState::Failed,
        Err(error) if error.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    };
    let finished = events.emit(UiEvent::TaskFinished { task, state }).await;
    match (result, finished) {
        (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
        (Err(error), _) | (_, Err(error)) => Err(error),
        (Ok(_), Ok(())) => check_cancel(cancellation),
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct NativeStatus {
    code: Option<i32>,
    signal: Option<i32>,
}
impl NativeStatus {
    fn success(self) -> bool {
        self.code == Some(0)
    }
    fn label(self) -> String {
        self.code.map_or_else(
            || format!("Signal: {}", self.signal.unwrap_or(0)),
            |code| format!("Exit status: {code}"),
        )
    }
}
struct Row {
    task: TaskId,
    host: String,
    status: NativeStatus,
    output: Vec<u8>,
}
struct Completion {
    row: Option<Row>,
    error: Option<ConfctlError>,
}
struct Progress {
    completed: AtomicUsize,
    failed: AtomicUsize,
    total: usize,
}

async fn commands<P: ProcessRunner>(
    execution: &Execution<'_, P>,
    machines: &[&Machine],
    request: &SshRequest,
) -> Result<(), ConfctlError> {
    let events = execution.events;
    let cancellation = execution.cancellation;
    let group = TaskId(0);
    check_cancel(cancellation)?;
    events
        .emit(UiEvent::TaskPending {
            task: group,
            description: "run SSH commands".into(),
        })
        .await?;
    let entered = async {
        check_cancel(cancellation)?;
        events
            .emit(UiEvent::TaskStarted {
                task: group,
                description: "run SSH commands".into(),
            })
            .await?;
        check_cancel(cancellation)
    }
    .await;
    let local = cancellation.child_token();
    let progress = Progress {
        completed: AtomicUsize::new(0),
        failed: AtomicUsize::new(0),
        total: machines.len(),
    };
    let host_execution = Execution {
        cancellation: &local,
        ..*execution
    };
    let mut registered = 0;
    let mut finished = 0;
    let mut error = entered.err();
    for (index, machine) in machines.iter().enumerate() {
        if error.is_some() {
            break;
        }
        if let Err(failure) = check_cancel(cancellation) {
            record(&mut error, failure);
            break;
        }
        match events
            .emit(UiEvent::TaskPending {
                task: TaskId(index as u64 + 1),
                description: description(machine),
            })
            .await
        {
            Ok(()) => {
                registered += 1;
                if let Err(failure) = check_cancel(cancellation) {
                    record(&mut error, failure);
                    break;
                }
            }
            Err(failure) => {
                record(&mut error, failure);
                break;
            }
        }
    }
    let mut rows = Vec::new();
    if error.is_none() {
        if request.parallel {
            let mut pending: Vec<_> = machines
                .iter()
                .enumerate()
                .map(|(index, machine)| {
                    Some(Box::pin(command_host(
                        &host_execution,
                        machine,
                        TaskId(index as u64 + 1),
                        request,
                        &progress,
                    )))
                })
                .collect();
            poll_fn(|cx| {
                let mut waiting = false;
                for slot in &mut pending {
                    let Some(future) = slot else { continue };
                    match future.as_mut().poll(cx) {
                        Poll::Pending => waiting = true,
                        Poll::Ready(completion) => {
                            *slot = None;
                            if let Some(row) = completion.row {
                                rows.push(row);
                            }
                            if let Some(failure) = completion.error {
                                record(&mut error, failure);
                                local.cancel();
                            }
                        }
                    }
                }
                if waiting {
                    Poll::Pending
                } else {
                    Poll::Ready(())
                }
            })
            .await;
            finished = machines.len();
        } else {
            for (index, machine) in machines.iter().enumerate() {
                let completion = command_host(
                    &host_execution,
                    machine,
                    TaskId(index as u64 + 1),
                    request,
                    &progress,
                )
                .await;
                if let Some(row) = completion.row {
                    rows.push(row);
                }
                if let Some(failure) = completion.error {
                    record(&mut error, failure);
                    local.cancel();
                    finished = index + 1;
                    break;
                }
                finished = index + 1;
            }
        }
    }
    // Serial failures leave later admitted work unentered. Admission failure
    // leaves only its acknowledged prefix registered.
    for index in finished..registered {
        if let Err(failure) = events
            .emit(UiEvent::TaskFinished {
                task: TaskId(index as u64 + 1),
                state: if local.is_cancelled() {
                    TaskState::Canceled
                } else {
                    TaskState::Skipped
                },
            })
            .await
        {
            record(&mut error, failure);
        }
    }
    if request.aggregate
        && let Err(failure) = aggregate(events, &rows).await
    {
        record(&mut error, failure);
    }
    if error.is_none()
        && let Err(failure) = check_cancel(cancellation)
    {
        record(&mut error, failure);
    }
    let state = if error.as_ref().is_some_and(ConfctlError::is_cancelled) {
        TaskState::Canceled
    } else if error.is_some() || progress.failed.load(Ordering::SeqCst) > 0 {
        TaskState::Failed
    } else {
        TaskState::Succeeded
    };
    if let Err(failure) = events
        .emit(UiEvent::TaskFinished { task: group, state })
        .await
    {
        record(&mut error, failure);
    }
    if error.is_none() {
        check_cancel(cancellation)?;
    }
    match error {
        Some(error) => Err(error),
        None => Ok(()),
    }
}

async fn command_host<P: ProcessRunner>(
    execution: &Execution<'_, P>,
    machine: &Machine,
    task: TaskId,
    request: &SshRequest,
    progress: &Progress,
) -> Completion {
    let Execution {
        runner,
        events,
        cancellation,
        root,
        config,
        live,
    } = execution;
    let result = async {
        check_cancel(cancellation)?;
        events
            .emit(UiEvent::TaskStarted {
                task,
                description: description(machine),
            })
            .await?;
        check_cancel(cancellation)?;
        let mut spec = super::connection::command_spec(machine, *config, &request.command)?;
        spec.redacted_description = description(machine);
        spec.cwd = Some((*root).into());
        spec.stdin = match &request.input {
            SshInput::None => StdinPolicy::Null,
            SshInput::Bytes(bytes) => StdinPolicy::Bytes(bytes.as_encoded_bytes().to_vec()),
            SshInput::File(path) => StdinPolicy::File(root.join(path)),
        };
        spec.stdout = if *live {
            StreamPolicy::CaptureAndForward
        } else {
            StreamPolicy::Capture
        };
        spec.stderr = StreamPolicy::MergeToStdout;
        let mut scope = ProcessScope::new((*cancellation).clone(), task);
        scope.phase = Some(Phase::SshCommands);
        let outcome = runner.run(spec, scope).await?;
        decode(machine, task, outcome)
    }
    .await;
    let state = match &result {
        Ok(row) if row.status.success() => TaskState::Succeeded,
        Ok(_) => TaskState::Failed,
        Err(error) if error.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    };
    let (row, mut error) = match result {
        Ok(row) => (Some(row), None),
        Err(error) => (None, Some(error)),
    };
    if let Some(row) = &row {
        let completed = progress.completed.fetch_add(1, Ordering::SeqCst) + 1;
        let failed = progress
            .failed
            .fetch_add(usize::from(!row.status.success()), Ordering::SeqCst)
            + usize::from(!row.status.success());
        if let Err(failure) = events
            .emit(UiEvent::TaskProgress {
                task: TaskId(0),
                completed: completed as u64,
                total: Some(progress.total as u64),
                detail: format!("{completed} completed, {failed} failed"),
            })
            .await
        {
            record(&mut error, failure);
        }
    }
    if let Err(failure) = events.emit(UiEvent::TaskFinished { task, state }).await {
        record(&mut error, failure);
    }
    if !request.aggregate
        && let Some(row) = &row
        && let Err(failure) = output(events, row, &[row.host.as_str()], false).await
    {
        record(&mut error, failure);
    }
    if error.is_none()
        && let Err(failure) = check_cancel(cancellation)
    {
        record(&mut error, failure);
    }
    Completion { row, error }
}

fn decode(machine: &Machine, task: TaskId, outcome: ProcessOutcome) -> Result<Row, ConfctlError> {
    let result = || {
        if outcome.termination == TerminationCause::Canceled {
            return Err(ConfctlError::Cancelled);
        }
        if !outcome.stdout_complete {
            return Err(ConfctlError::Io {
                operation: "read SSH output",
                path: None,
                source: io::Error::other("incomplete merged capture"),
            });
        }
        let output = read_capture(&outcome.stdout).map_err(|source| ConfctlError::Io {
            operation: "read SSH output",
            path: None,
            source,
        })?;
        Ok(Row {
            task,
            host: machine.name.clone(),
            status: NativeStatus {
                code: outcome.code(),
                signal: outcome.signal(),
            },
            output,
        })
    };
    result().map_err(|source| ConfctlError::ProcessFailure {
        source: Box::new(source),
        evidence: Box::new(PartialProcessEvidence {
            status: Some(outcome.status),
            stdout: outcome.stdout,
            stderr: outcome.stderr,
            stdout_complete: outcome.stdout_complete,
            stderr_complete: outcome.stderr_complete,
            log: None,
        }),
    })
}
fn read_capture(capture: &Captured) -> io::Result<Vec<u8>> {
    match capture {
        Captured::Memory(bytes) => Ok(bytes.clone()),
        Captured::Spool { file, length } => {
            use io::Read;
            let mut bytes = Vec::new();
            file.reopen()?.take(*length).read_to_end(&mut bytes)?;
            if bytes.len() as u64 != *length {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "short SSH capture",
                ));
            }
            Ok(bytes)
        }
        Captured::Unavailable => Err(io::Error::other("SSH output was not captured")),
    }
}
async fn aggregate(events: &EventSink, rows: &[Row]) -> Result<(), ConfctlError> {
    let mut groups: Vec<(&Row, Vec<&str>)> = Vec::new();
    for row in rows {
        if let Some((_, hosts)) = groups
            .iter_mut()
            .find(|(other, _)| other.status == row.status && other.output == row.output)
        {
            hosts.push(&row.host);
        } else {
            groups.push((row, vec![&row.host]));
        }
    }
    for (row, mut hosts) in groups {
        hosts.sort_unstable();
        output(events, row, &hosts, true).await?;
    }
    Ok(())
}
async fn output(
    events: &EventSink,
    row: &Row,
    hosts: &[&str],
    aggregated: bool,
) -> Result<(), ConfctlError> {
    let mut bytes = format!("{}:\n", hosts.join(", ")).into_bytes();
    if aggregated || !row.status.success() {
        bytes.extend_from_slice(row.status.label().as_bytes());
        bytes.push(b'\n');
    }
    bytes.extend_from_slice(&row.output);
    if !bytes.ends_with(b"\n") {
        bytes.push(b'\n');
    }
    bytes.push(b'\n');
    events
        .emit(UiEvent::Output {
            task: row.task,
            stream: OutputStream::Stdout,
            bytes,
        })
        .await
}
fn record(error: &mut Option<ConfctlError>, later: ConfctlError) {
    *error = Some(match error.take() {
        Some(primary) => primary.with_secondary(vec![later]),
        None => later,
    });
}
fn check_cancel(cancellation: &CancellationToken) -> Result<(), ConfctlError> {
    if cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}
fn description(machine: &Machine) -> String {
    format!("SSH on {}", machine.name)
}

#[cfg(all(test, target_os = "linux"))]
mod pty_tests;
#[cfg(test)]
mod tests;
