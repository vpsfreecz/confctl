//! Sequential input comparisons, with Git effects only for requested changelogs.

use super::{
    input_observation::{self, Comparison, InputObservation, Inputs},
    pager::{Pager, WrittenOrClosed},
};
use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{
        Action, Inventory, MachineId, ManagedFilter, SelectionPolicy, pattern_matches, select,
    },
    error::ConfctlError,
    events::{OutputStream, Style, TaskId, TaskState, UiEvent},
    nix::{InvocationSettings, NixClient, NixOptions},
    terminal::ConfirmPrompt,
};
use serde_json::Value;
use std::collections::BTreeSet;

mod git;

struct Target {
    machine: MachineId,
    inputs: InputObservation,
    observed: super::host_status::HostStatus,
    deployed: InputObservation,
}
#[derive(Clone, Copy)]
struct Format<'a> {
    role_pattern: Option<&'a str>,
    changelog: bool,
    downgrade: bool,
    verbose: bool,
    patch: bool,
}

pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    context: &Context,
) -> Result<(), ConfctlError> {
    let (request, format) = match action {
        Action::Diff(request) => (
            request,
            Format {
                role_pattern: request.role_pattern.as_deref(),
                changelog: false,
                downgrade: request.downgrade,
                verbose: false,
                patch: false,
            },
        ),
        Action::Changelog {
            change,
            verbose,
            patch,
        } => (
            change,
            Format {
                role_pattern: change.role_pattern.as_deref(),
                changelog: true,
                downgrade: change.downgrade,
                verbose: *verbose,
                patch: *patch,
            },
        ),
        _ => unreachable!("comparison adapter receives diff or changelog"),
    };
    let cancellation = &context.cancellation;
    let defaults = InvocationSettings::new(
        root.clone(),
        context.processes.clone(),
        cancellation.clone(),
    );
    let nix = NixClient::configured(
        root.clone(),
        context.processes.clone(),
        NixOptions::default(),
        cancellation.clone(),
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
            managed: ManagedFilter::Managed,
            ..SelectionPolicy::ALL
        },
    )?;
    let settings = defaults.read().await?;
    let generation = request
        .generation
        .as_ref()
        .map(|selector| match selector {
            crate::domain::GenerationSelector::Current => "current",
            crate::domain::GenerationSelector::Offset(text)
            | crate::domain::GenerationSelector::Name(text) => text,
        })
        .unwrap_or("current configuration");
    if !context
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: "Compare inputs on the following machines:\n".into(),
                },
                super::machines::table(inventory, &selected, &settings.list_columns, true),
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: format!("\nGeneration: {generation}\n"),
                },
            ],
            assume_yes: request.targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    check_cancel(context)?;
    let mut targets = Vec::new();
    if let Some(selector) = &request.generation {
        for (machine, generation, _) in super::status::resolve_saved(
            &root,
            inventory,
            &selected.machines,
            selector,
            request.targets.yes,
            context,
        )
        .await?
        {
            targets.push(Target {
                machine,
                inputs: InputObservation {
                    value: input_observation::normalize(&Value::Object(generation.inputs_info)),
                    ..InputObservation::default()
                },
                observed: Default::default(),
                deployed: Default::default(),
            });
        }
    } else {
        for id in &selected.machines {
            check_cancel(context)?;
            targets.push(Target {
                machine: *id,
                inputs: input_observation::configured(&nix, inventory.get(*id)).await?,
                observed: Default::default(),
                deployed: Default::default(),
            });
            check_cancel(context)?;
        }
    }
    u64::try_from(targets.len())
        .ok()
        .and_then(|count| count.checked_add(2))
        .ok_or_else(|| ConfctlError::State("Comparison task IDs overflow".into()))?;
    // Nix owns task 0; one lazy pager owns task 1, and observed hosts use 2+.
    let mut pager = Pager::new(context, TaskId(1))?;
    let result = produce(context, inventory, &mut targets, format, &mut pager).await;
    let finish = pager.finish().await;
    match (result, finish) {
        (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
        (Err(error), _) | (_, Err(error)) => Err(error),
        (Ok(()), Ok(_)) => check_cancel(context),
    }
}

async fn produce(
    context: &Context,
    inventory: &Inventory,
    targets: &mut [Target],
    format: Format<'_>,
    pager: &mut Pager<'_>,
) -> Result<(), ConfctlError> {
    for (index, target) in targets.iter_mut().enumerate() {
        check_cancel(context)?;
        let task = TaskId(index as u64 + 2);
        let description = format!("compare inputs on {}", inventory.get(target.machine).name);
        context
            .events
            .emit(UiEvent::TaskPending {
                task,
                description: description.clone(),
            })
            .await?;
        let mut observed_failure = target.inputs.failed;
        let result = async {
            check_cancel(context)?;
            context
                .events
                .emit(UiEvent::TaskStarted { task, description })
                .await?;
            check_cancel(context)?;
            host(
                context,
                inventory,
                target,
                task,
                format,
                pager,
                &mut observed_failure,
            )
            .await
        }
        .await;
        if observed_failure {
            context.keep_log();
        }
        let state = match &result {
            Err(error) if error.is_cancelled() => TaskState::Canceled,
            Err(_) => TaskState::Failed,
            Ok(_) if observed_failure => TaskState::Failed,
            Ok(_) => TaskState::Succeeded,
        };
        let finish = context
            .events
            .emit(UiEvent::TaskFinished { task, state })
            .await;
        let closed = match (result, finish) {
            (Err(error), Err(later)) => return Err(error.with_secondary(vec![later])),
            (Err(error), _) | (_, Err(error)) => return Err(error),
            (Ok(closed), Ok(())) => closed,
        };
        check_cancel(context)?;
        if closed {
            return Ok(());
        }
    }
    Ok(())
}

async fn host(
    context: &Context,
    inventory: &Inventory,
    target: &mut Target,
    task: TaskId,
    format: Format<'_>,
    pager: &mut Pager<'_>,
    failed: &mut bool,
) -> Result<bool, ConfctlError> {
    let machine = inventory.get(target.machine);
    let config = std::env::var_os("CONFCTL_SSH_CONFIG");
    diagnostics(context, &target.inputs.diagnostics).await?;
    target.observed = super::host_status::uptime_only(
        inventory,
        machine,
        &context.processes,
        &context.cancellation,
        task,
        config.as_deref(),
    )
    .await?;
    if let Some(failure) = &target.observed.failure {
        *failed = true;
        diagnostics(context, &[failure.diagnostic(&machine.name)]).await?;
    }
    if !target
        .observed
        .uptime
        .is_some_and(|uptime| uptime.is_finite() && uptime >= 0.0)
    {
        *failed = true;
        return write(pager, format!("{} is offline\n", machine.name)).await;
    }
    target.deployed = input_observation::deployed(
        context,
        inventory,
        machine,
        &context.cancellation,
        task,
        config.as_deref(),
    )
    .await?;
    *failed |= target.deployed.failed;
    diagnostics(context, &target.deployed.diagnostics).await?;
    let roles = roles(target.inputs.value.as_ref(), target.deployed.value.as_ref());
    if roles.is_empty() {
        return write(pager, format!("{}: no inputs info\n\n", machine.name)).await;
    }
    let mut output = false;
    let mut selected = false;
    for role in roles {
        if format
            .role_pattern
            .is_some_and(|pattern| !pattern_matches(pattern, &role))
        {
            continue;
        }
        selected = true;
        let wanted = target
            .inputs
            .value
            .as_ref()
            .and_then(|inputs| inputs.get(&role));
        let current = target
            .deployed
            .value
            .as_ref()
            .and_then(|inputs| inputs.get(&role));
        match input_observation::comparison(wanted, current) {
            Comparison::Same => {}
            Comparison::Unknown => {
                output = true;
                if write(
                    pager,
                    format!("{} @ {role} in unknown state\n", machine.name),
                )
                .await?
                {
                    return Ok(true);
                }
            }
            Comparison::Changed => {
                output = true;
                if format.changelog {
                    if write(pager, format!("{} @ {role}:\n", machine.name)).await? {
                        return Ok(true);
                    }
                    match git::log(context, wanted, current, format, task).await? {
                        git::Log::Unavailable(message) => {
                            *failed = true;
                            if write(pager, format!("(changelog unavailable: {message})\n")).await?
                            {
                                return Ok(true);
                            }
                        }
                        git::Log::Failed(outcome) => {
                            *failed = true;
                            let delivery = async {
                                if write(pager, format!("Git failed with {}\n", outcome.status))
                                    .await?
                                {
                                    return Ok(true);
                                }
                                git::stream(pager, &outcome.stderr).await
                            }
                            .await;
                            if delivery
                                .map_err(|source| crate::nix::process_error(source, outcome))?
                            {
                                return Ok(true);
                            }
                        }
                        git::Log::Ready { outcome, range } => {
                            let delivery = async {
                                if write(pager, format!("git log for {range}\n")).await? {
                                    return Ok(true);
                                }
                                git::stream(pager, &outcome.stdout).await
                            }
                            .await;
                            if delivery
                                .map_err(|source| crate::nix::process_error(source, outcome))?
                            {
                                return Ok(true);
                            }
                        }
                    }
                    if write(pager, "\n".into()).await? {
                        return Ok(true);
                    }
                } else {
                    let (mut old, mut new) = (short(current), short(wanted));
                    if format.downgrade {
                        std::mem::swap(&mut old, &mut new);
                    }
                    let url = chosen_url(wanted, current).map(crate::domain::ruby_string);
                    let label = url.map_or_else(|| role.clone(), |url| format!("{role} ({url})"));
                    if write(
                        pager,
                        format!("{} @ {label}: {old} -> {new}\n", machine.name),
                    )
                    .await?
                    {
                        return Ok(true);
                    }
                }
            }
        }
    }
    if selected && !output && write(pager, format!("{}: no changes\n", machine.name)).await? {
        return Ok(true);
    }
    if selected {
        return write(pager, "\n".into()).await;
    }
    Ok(false)
}

fn roles(target: Option<&Inputs>, deployed: Option<&Inputs>) -> BTreeSet<String> {
    target
        .into_iter()
        .chain(deployed)
        .flat_map(|inputs| inputs.keys().cloned())
        .collect()
}
fn chosen_url<'a>(target: Option<&'a Value>, deployed: Option<&'a Value>) -> Option<&'a Value> {
    target
        .and_then(|info| info.get("url"))
        .filter(|value| !matches!(value, Value::Null | Value::Bool(false)))
        .or_else(|| deployed.and_then(|info| info.get("url")))
        .filter(|value| !matches!(value, Value::Null | Value::Bool(false)))
}
fn short(info: Option<&Value>) -> String {
    if let Some(value) = info
        .and_then(|info| info.get("shortRev"))
        .filter(|value| !matches!(value, Value::Null | Value::Bool(false)))
    {
        return crate::domain::ruby_string(value);
    }
    info.and_then(|info| info.get("rev"))
        .and_then(Value::as_str)
        .map(|value| value.chars().take(8).collect())
        .unwrap_or_else(|| "unknown".into())
}
async fn write(pager: &mut Pager<'_>, text: String) -> Result<bool, ConfctlError> {
    pager
        .write(text.as_bytes())
        .await
        .map(|result| matches!(result, WrittenOrClosed::Closed))
}
async fn diagnostics(context: &Context, messages: &[String]) -> Result<(), ConfctlError> {
    for message in messages {
        context
            .events
            .emit(UiEvent::Diagnostic {
                stream: OutputStream::Stderr,
                style: Style::Plain,
                text: format!("{message}\n"),
            })
            .await?;
        check_cancel(context)?;
    }
    Ok(())
}
fn check_cancel(context: &Context) -> Result<(), ConfctlError> {
    if context.cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests;
