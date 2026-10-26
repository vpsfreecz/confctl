//! Shared joined GC batch. Selection, consent and the mutation guard belong to callers.
use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot, MutationGuard, MutationLock},
    domain::{Action, Machine, SelectionPolicy, select},
    error::ConfctlError,
    events::{OutputStream, Style, TaskId, TaskState, UiEvent},
    nix::{InvocationSettings, NixClient, NixOptions, process_error},
    process::{ProcessRunner, ProcessScope, StreamPolicy, TerminationCause, gc_output::GcOutput},
    terminal::ConfirmPrompt,
    timing::Phase,
};
use std::{
    future::{Future, poll_fn},
    task::Poll,
};
use tokio_util::sync::CancellationToken;

pub(super) fn limit(value: i64) -> Result<usize, ConfctlError> {
    usize::try_from(value)
        .ok()
        .filter(|n| *n > 0)
        .ok_or_else(|| {
            ConfctlError::Usage("max-concurrent-gc must be a positive representable integer".into())
        })
}
pub(super) fn check(cancel: &CancellationToken) -> Result<(), ConfctlError> {
    if cancel.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}
pub(super) async fn checked(ctx: &Context, event: UiEvent) -> Result<(), ConfctlError> {
    ctx.events.emit(event).await?;
    check(&ctx.cancellation)
}
pub(super) fn message(stream: OutputStream, text: String) -> UiEvent {
    UiEvent::Diagnostic {
        stream,
        style: Style::Plain,
        text,
    }
}
pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    let Action::CollectGarbage {
        targets,
        max_concurrent,
    } = action
    else {
        unreachable!()
    };
    let limit = limit(*max_concurrent)?;
    let defaults = InvocationSettings::new(
        root.clone(),
        ctx.processes.clone(),
        ctx.cancellation.clone(),
    );
    let nix = NixClient::configured(
        root.clone(),
        ctx.processes.clone(),
        NixOptions::default(),
        ctx.cancellation.clone(),
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
            runnable: true,
            ..SelectionPolicy::ALL
        },
    )?;
    if selected.machines.is_empty() {
        return Err(ConfctlError::Selection(
            "No machines to collect garbage on".into(),
        ));
    }
    let settings = defaults.read().await?;
    if !ctx
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Heading,
                    text: "Collect garbage on the following machines:\n".into(),
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
    check(&ctx.cancellation)?;
    let guard = MutationLock::acquire(&root, &ctx.cancellation).await?;
    let machines = selected
        .machines
        .iter()
        .map(|id| inventory.get(*id))
        .collect::<Vec<_>>();
    run_selected(ctx, &root, &guard, &machines, limit, &mut 1).await
}
fn ordinary(error: &ConfctlError) -> bool {
    matches!(error, ConfctlError::ProcessFailure { source, .. } if matches!(source.as_ref(), ConfctlError::Child { .. }))
}

pub(super) async fn run_selected(
    ctx: &Context,
    root: &ConfigRoot,
    guard: &MutationGuard,
    machines: &[&Machine],
    limit: usize,
    next_task: &mut u64,
) -> Result<(), ConfctlError> {
    guard.require_root(root)?;
    if machines.is_empty() {
        return Ok(());
    }
    if limit == 0 {
        return Err(ConfctlError::Usage(
            "GC concurrency must be positive".into(),
        ));
    }
    check(&ctx.cancellation)?;
    let first = *next_task;
    *next_task = next_task
        .checked_add(machines.len() as u64)
        .ok_or_else(|| ConfctlError::State("GC task IDs overflow".into()))?;
    let token = ctx.cancellation.child_token();
    let mut announced = 0;
    let mut admission_error = None;
    for (i, machine) in machines.iter().enumerate() {
        if let Err(error) = ctx
            .events
            .emit(UiEvent::TaskPending {
                task: TaskId(first + i as u64),
                description: format!("collect garbage on {}", machine.name),
            })
            .await
        {
            admission_error = Some(error);
            break;
        }
        announced += 1;
        if let Err(error) = check(&token) {
            admission_error = Some(error);
            break;
        }
    }
    let mut next = 0;
    let mut active = Vec::new();
    let mut failures = Vec::new();
    if let Some(error) = admission_error {
        failures.push((0, error));
        token.cancel();
    }
    poll_fn(|cx| {
        while active.len() < limit && next < announced && !token.is_cancelled() {
            active.push((
                next,
                Box::pin(one(
                    ctx,
                    root,
                    guard,
                    machines[next],
                    TaskId(first + next as u64),
                    &token,
                )),
            ));
            next += 1;
        }
        let mut i = 0;
        while i < active.len() {
            match active[i].1.as_mut().poll(cx) {
                Poll::Pending => i += 1,
                Poll::Ready(result) => {
                    let (index, _) = active.remove(i);
                    if let Err(error) = result {
                        if !ordinary(&error) {
                            token.cancel();
                        }
                        failures.push((index, error));
                    }
                }
            }
        }
        if active.is_empty() && (next == announced || token.is_cancelled()) {
            Poll::Ready(())
        } else {
            if active.len() < limit && next < announced {
                cx.waker().wake_by_ref();
            }
            Poll::Pending
        }
    })
    .await;
    for i in next..announced {
        if let Err(error) = ctx
            .events
            .emit(UiEvent::TaskFinished {
                task: TaskId(first + i as u64),
                state: if token.is_cancelled() {
                    TaskState::Canceled
                } else {
                    TaskState::Skipped
                },
            })
            .await
        {
            failures.push((i, error));
        }
    }
    if failures.is_empty() {
        return check(&ctx.cancellation);
    }
    if failures.iter().all(|(_, error)| ordinary(error)) && !token.is_cancelled() {
        let hosts = failures
            .iter()
            .map(|(i, error)| format!("{}: {error}", machines[*i].name))
            .collect::<Vec<_>>()
            .join(", ");
        return Err(ConfctlError::State(format!(
            "Garbage collection failed on {hosts}; earlier store deletions may remain"
        ))
        .with_secondary(failures.into_iter().map(|(_, e)| e).collect()));
    }
    let primary = failures
        .iter()
        .position(|(_, e)| !ordinary(e) && !e.is_cancelled())
        .unwrap_or(0);
    let (_, error) = failures.remove(primary);
    Err(error.with_secondary(failures.into_iter().map(|(_, e)| e).collect()))
}

async fn one(
    ctx: &Context,
    root: &ConfigRoot,
    guard: &MutationGuard,
    machine: &Machine,
    task: TaskId,
    cancel: &CancellationToken,
) -> Result<(), ConfctlError> {
    let output = GcOutput::new(&machine.name);
    let native = async {
        guard.require_root(root)?;
        check(cancel)?;
        ctx.events
            .emit(UiEvent::TaskStarted {
                task,
                description: format!("collect garbage on {}", machine.name),
            })
            .await?;
        check(cancel)?;
        let config = std::env::var_os("CONFCTL_SSH_CONFIG");
        let mut spec = super::connection::command_spec(
            machine,
            config.as_deref(),
            &["nix-collect-garbage".into()],
        )?;
        spec.redacted_description = format!("collect garbage on {}", machine.name);
        spec.cwd = Some(root.path().into());
        spec.stdout = StreamPolicy::CaptureAndForward;
        spec.stderr = StreamPolicy::CaptureAndForward;
        let mut scope = ProcessScope::new(cancel.clone(), task);
        scope.phase = Some(Phase::GarbageCollection);
        scope.gc_output = Some(output.clone());
        ctx.processes.run(spec, scope).await
    }
    .await;
    let mut later = Vec::new();
    let (state, text) = match &native {
        Ok(outcome)
            if outcome.termination == TerminationCause::Canceled || cancel.is_cancelled() =>
        {
            (
                TaskState::Canceled,
                format!(
                    "{}: canceled; earlier store deletions may remain\n",
                    machine.name
                ),
            )
        }
        Ok(outcome) if outcome.status.success() => (
            TaskState::Succeeded,
            format!(
                "{}: {}\n",
                machine.name,
                output.summary().await.unwrap_or_else(|| "done".into())
            ),
        ),
        Ok(outcome) => (
            TaskState::Failed,
            format!(
                "{}: error (exit {:?}, signal {:?}); earlier store deletions may remain\n",
                machine.name,
                outcome.code(),
                outcome.signal()
            ),
        ),
        Err(error) => (
            if error.is_cancelled() {
                TaskState::Canceled
            } else {
                TaskState::Failed
            },
            format!(
                "{}: incomplete: {error}; earlier store deletions may remain\n",
                machine.name
            ),
        ),
    };
    if let Err(error) = ctx.events.emit(message(OutputStream::Stdout, text)).await {
        later.push(error);
    } else if let Err(error) = check(cancel) {
        later.push(error);
    }
    if let Err(error) = ctx
        .events
        .emit(UiEvent::TaskFinished {
            task,
            state: if !later.is_empty() && state == TaskState::Succeeded {
                TaskState::Incomplete
            } else {
                state
            },
        })
        .await
    {
        later.push(error);
    } else if let Err(error) = check(cancel) {
        later.push(error);
    }
    match native {
        Err(error) => Err(error.with_secondary(later)),
        Ok(outcome) => {
            if outcome.termination == TerminationCause::Canceled {
                return Err(process_error(
                    ConfctlError::Cancelled.with_secondary(later),
                    outcome,
                ));
            }
            if !outcome.status.success() {
                ctx.keep_log();
                return Err(process_error(
                    ConfctlError::Child {
                        description: format!("collect garbage on {}", machine.name),
                        code: outcome.code(),
                        signal: outcome.signal(),
                        log: Some(ctx.log_path().into()),
                    }
                    .with_secondary(later),
                    outcome,
                ));
            }
            if !later.is_empty() {
                let error = later.remove(0).with_secondary(later);
                return Err(process_error(error, outcome));
            }
            Ok(())
        }
    }
}
