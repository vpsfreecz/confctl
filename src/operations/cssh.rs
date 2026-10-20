//! One supervised ClusterSSH launcher; it does not own the opened host sessions.

use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{Action, Machine, SelectionPolicy, select},
    error::ConfctlError,
    events::{EventSink, OutputStream, Style, TaskId, TaskState, UiEvent},
    nix::{InvocationSettings, NixClient, NixOptions},
    process::{
        Captured, PartialProcessEvidence, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec,
        StreamPolicy, TerminationCause,
    },
    terminal::ConfirmPrompt,
    timing::Phase,
};
use std::{ffi::OsString, io, path::Path};
use tokio_util::sync::CancellationToken;

pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    context: &Context,
) -> Result<(), ConfctlError> {
    let Action::Cssh { targets, managed } = action else {
        unreachable!("ClusterSSH adapter receives its typed action")
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
        &targets.selection,
        SelectionPolicy {
            managed: *managed,
            runnable: true,
            ..SelectionPolicy::ALL
        },
    )?;
    if selected.machines.is_empty() {
        return Err(ConfctlError::Selection(
            "No machines to open cssh to".into(),
        ));
    }
    let machines = selected
        .machines
        .iter()
        .map(|id| inventory.get(*id))
        .collect::<Vec<_>>();
    let spec = launcher_spec(&machines, root.path())?;
    let settings = defaults.read().await?;
    if !context
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: "Open cssh to the following machines:\n".into(),
                },
                super::machines::table(inventory, &selected, &settings.list_columns, true),
            ],
            assume_yes: targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    context.keep_log();
    launch(
        &context.processes,
        &context.events,
        &context.cancellation,
        spec,
        context.log_path(),
    )
    .await
}

fn launcher_spec(machines: &[&Machine], root: &Path) -> Result<ProcessSpec, ConfctlError> {
    let mut argv = vec![OsString::from("cssh"), "-l".into(), "root".into()];
    for machine in machines {
        argv.push(super::connection::target_host(machine)?.into());
    }
    let mut spec = ProcessSpec::new("nix-shell", "ClusterSSH launcher");
    spec.args = vec![
        "-p".into(),
        "perlPackages.AppClusterSSH".into(),
        "--command".into(),
        super::connection::shell_join(&argv),
    ];
    spec.cwd = Some(root.into());
    spec.env.push(("shellHook".into(), None));
    spec.stdout = StreamPolicy::CaptureAndForward;
    spec.stderr = StreamPolicy::CaptureAndForward;
    Ok(spec)
}

async fn launch<P: ProcessRunner>(
    runner: &P,
    events: &EventSink,
    cancellation: &CancellationToken,
    spec: ProcessSpec,
    log_path: &Path,
) -> Result<(), ConfctlError> {
    let task = TaskId(1);
    let description = "ClusterSSH launcher";
    check_cancel(cancellation)?;
    events
        .emit(UiEvent::TaskPending {
            task,
            description: description.into(),
        })
        .await?;
    let mut observed = None;
    let result = async {
        check_cancel(cancellation)?;
        events
            .emit(UiEvent::TaskStarted {
                task,
                description: description.into(),
            })
            .await?;
        check_cancel(cancellation)?;
        let mut scope = ProcessScope::new(cancellation.clone(), task);
        scope.phase = Some(Phase::SshCommands);
        let outcome = runner.run(spec, scope).await?;
        let status = outcome.status;
        let success = launcher_result(outcome)?;
        observed = Some(if success {
            TaskState::Succeeded
        } else {
            TaskState::Failed
        });
        check_cancel(cancellation)?;
        if !success {
            events
                .emit(UiEvent::Diagnostic {
                    stream: OutputStream::Stderr,
                    style: Style::Failure,
                    text: format!("ClusterSSH launcher failed with {status}\n"),
                })
                .await?;
            check_cancel(cancellation)?;
        }
        events
            .emit(UiEvent::LogAvailable {
                task,
                path: log_path.into(),
            })
            .await?;
        check_cancel(cancellation)
    }
    .await;
    let state = observed.unwrap_or_else(|| match &result {
        Err(error) if error.is_cancelled() => TaskState::Canceled,
        _ => TaskState::Failed,
    });
    let finished = events.emit(UiEvent::TaskFinished { task, state }).await;
    match (result, finished) {
        (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
        (Err(error), _) | (_, Err(error)) => Err(error),
        (Ok(()), Ok(())) => check_cancel(cancellation),
    }
}

fn launcher_result(outcome: ProcessOutcome) -> Result<bool, ConfctlError> {
    let source = if outcome.termination == TerminationCause::Canceled {
        Some(ConfctlError::Cancelled)
    } else if !outcome.stdout_complete
        || !outcome.stderr_complete
        || matches!(outcome.stdout, Captured::Unavailable)
        || matches!(outcome.stderr, Captured::Unavailable)
    {
        Some(ConfctlError::Io {
            operation: "capture ClusterSSH launcher output",
            path: None,
            source: io::Error::other("incomplete launcher capture"),
        })
    } else {
        None
    };
    match source {
        Some(source) => Err(ConfctlError::ProcessFailure {
            source: Box::new(source),
            evidence: Box::new(PartialProcessEvidence {
                status: Some(outcome.status),
                stdout: outcome.stdout,
                stderr: outcome.stderr,
                stdout_complete: outcome.stdout_complete,
                stderr_complete: outcome.stderr_complete,
                log: None,
            }),
        }),
        None => Ok(outcome.status.success()),
    }
}

fn check_cancel(cancellation: &CancellationToken) -> Result<(), ConfctlError> {
    if cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests;
