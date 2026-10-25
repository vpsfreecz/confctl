//! The six input mutations share one borrowed lock editor and one invocation guard.
mod message;
use super::{input_observation::attribute, inputs::channel_names};
use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot, MutationGuard, MutationLock},
    domain::{Action, InputChanges},
    error::ConfctlError,
    events::{OutputStream, TaskId, TaskState, UiEvent},
    inputs::{FlakeLock, InputEditor, validate_forward},
    nix::{InvocationSettings, NixClient, NixOptions, process_error},
    process::{
        ProcessRunner, ProcessScope, ProcessSpec, StdinPolicy, StreamPolicy, TerminationCause,
    },
};
use serde_json::{Map, Value};
use std::{
    collections::BTreeMap,
    io::{IsTerminal, Write},
    os::fd::AsFd,
};

pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    root.require_flake()?;
    let guard = MutationLock::acquire(&root, &ctx.cancellation).await?;
    check(ctx, &root, &guard)?;
    let description = action.name().to_owned();
    ctx.events
        .emit(UiEvent::TaskPending {
            task: TaskId(0),
            description: description.clone(),
        })
        .await?;
    let result = async {
        check(ctx, &root, &guard)?;
        ctx.events
            .emit(UiEvent::TaskStarted {
                task: TaskId(0),
                description,
            })
            .await?;
        check(ctx, &root, &guard)?;
        mutate(action, &root, &guard, ctx).await
    }
    .await;
    let state = match &result {
        Ok(()) => TaskState::Succeeded,
        Err(e) if e.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    };
    let finish = ctx
        .events
        .emit(UiEvent::TaskFinished {
            task: TaskId(0),
            state,
        })
        .await;
    match (result, finish) {
        (Err(e), Err(later)) => Err(e.with_secondary(vec![later])),
        (Err(e), _) | (_, Err(e)) => Err(e),
        (Ok(()), Ok(())) => check(ctx, &root, &guard),
    }
}

async fn mutate(
    action: &Action,
    root: &ConfigRoot,
    guard: &MutationGuard,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    let (options, revision) = match action {
        Action::InputsUpdate { changes, .. }
        | Action::ChannelUpdate { changes, .. }
        | Action::MachineUpdate { changes, .. } => (changes, None),
        Action::InputsSet {
            changes, revision, ..
        }
        | Action::ChannelSet {
            changes, revision, ..
        }
        | Action::MachineSet {
            changes, revision, ..
        } => (changes, Some(revision.as_str())),
        _ => unreachable!("input mutation route"),
    };
    let (inputs, targets) = select(action, root, ctx).await?;
    check(ctx, root, guard)?;
    let editor = InputEditor::new(
        root.clone(),
        ctx.processes.clone(),
        ctx.cancellation.clone(),
    );
    let mut prepared = None;
    let changes = if options.forward_only && revision.is_none() {
        let candidate = editor.prepare_forward(guard, &inputs).await?;
        let changes = validate_forward(&candidate.old, &candidate.candidate, |proof| {
            super::input_git::compare(ctx, proof)
        })
        .await?;
        editor
            .install_forward(guard, &candidate, !changes.is_empty())
            .await?;
        prepared = Some(candidate);
        Ok(changes)
    } else if let Some(revision) = revision {
        editor.set(guard, &inputs, revision).await
    } else {
        editor.update(guard, &inputs).await
    }
    .map_err(|e| retained(e, "Nix lock editing"))?;
    check(ctx, root, guard)?;
    if options.commit && !changes.is_empty() {
        let bytes = message::build(ctx, &changes, options, revision.is_some())
            .await
            .map_err(|e| retained(e, "commit message/changelog preparation"))?;
        check(ctx, root, guard)?;
        commit(ctx, root, guard, &bytes, options)
            .await
            .map_err(|e| retained(e, "Git commit/editor/hooks"))?;
    }
    check(ctx, root, guard)?;
    let summary = if targets.is_empty() {
        if changes.is_empty() {
            "No changes.\n".into()
        } else {
            let mut text = format!("Updated inputs ({}):\n", changes.len());
            for ch in &changes {
                text.push_str(&format!(
                    "  {}: {} -> {}\n",
                    ch.name,
                    ch.old_info
                        .as_ref()
                        .and_then(|i| i.short_rev.as_deref())
                        .unwrap_or("-"),
                    ch.new_info.short_rev.as_deref().unwrap_or("-")
                ));
            }
            text
        }
    } else {
        let lock = FlakeLock::load(&root.path().join("flake.lock"))?;
        targets
            .into_iter()
            .map(|(input, label)| {
                Ok(format!(
                    "{label} -> {}\n",
                    lock.input_info(&input)?.short_rev.as_deref().unwrap_or("-")
                ))
            })
            .collect::<Result<String, ConfctlError>>()?
    };
    // Keep the guarded Git-path lock until all optional hooks and summaries finish.
    let _prepared = prepared;
    ctx.events
        .emit(UiEvent::Output {
            task: TaskId(0),
            stream: OutputStream::Stdout,
            bytes: summary.into_bytes(),
        })
        .await?;
    check(ctx, root, guard)
}

// All Nix read views live only inside selection, before InputEditor enters.
type SelectedInputs = (Vec<String>, Vec<(String, String)>);
async fn select(
    action: &Action,
    root: &ConfigRoot,
    ctx: &Context,
) -> Result<SelectedInputs, ConfctlError> {
    match action {
        Action::InputsUpdate { inputs, all, .. } => {
            let inputs = if *all {
                match FlakeLock::load(&root.path().join("flake.lock")) {
                    Err(ConfctlError::Io { source, .. })
                        if source.kind() == std::io::ErrorKind::NotFound =>
                    {
                        return Err(config("flake.lock missing; specify inputs explicitly"));
                    }
                    lock => lock?.root_inputs(),
                }
            } else {
                inputs.clone()
            };
            if inputs.is_empty() {
                return Err(config("no inputs selected (specify input names or --all)"));
            }
            return Ok((inputs, vec![]));
        }
        Action::InputsSet { input, .. } => return Ok((vec![input.clone()], vec![])),
        _ => {}
    }
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
    let setting = matches!(
        action,
        Action::ChannelSet { .. } | Action::MachineSet { .. }
    );
    let verb = if setting { "Configuring" } else { "Updating" };
    let targets = match action {
        Action::ChannelUpdate { channels, role, .. } => {
            let value = nix.eval_json(".#confctl.channels").await?;
            channel_targets(&value, channels, role.as_deref())?.0
        }
        Action::ChannelSet {
            channels,
            role,
            allow_shared,
            ..
        } => {
            let value = nix.eval_json(".#confctl.channels").await?;
            let (targets, selected) = channel_targets(&value, channels, Some(role))?;
            let warning = shared_uses(&value, &targets, &selected, role)?;
            if let Some(warning) = warning {
                if !allow_shared {
                    return Err(config(&format!(
                        "{warning} (use --allow-shared to proceed)"
                    )));
                }
                ctx.events
                    .emit(UiEvent::Output {
                        task: TaskId(0),
                        stream: OutputStream::Stdout,
                        bytes: format!("Warning: {warning}\n").into_bytes(),
                    })
                    .await?;
                canceled(ctx)?;
            }
            targets
        }
        Action::MachineUpdate { machine, role, .. } | Action::MachineSet { machine, role, .. } => {
            let key = nix.machine_key(machine).await?;
            let value = nix
                .eval_json(&format!(".#confctl.inputsInfo.{}", attribute(&key)))
                .await?;
            let info = value.as_object().ok_or_else(|| {
                config(&format!("inputs info unavailable for machine '{machine}'"))
            })?;
            let mapping = roles(
                info.get(role),
                &format!("machine '{machine}' role '{role}'"),
            )?;
            let input = input_name(
                mapping.and_then(|v| v.get("input")),
                &format!("machine '{machine}' role '{role}'"),
            )?
            .ok_or_else(|| config(&format!("machine '{machine}' has no role '{role}'")))?;
            vec![(input.to_owned(), role.clone(), machine.clone())]
        }
        _ => unreachable!(),
    };
    if targets.is_empty() {
        return Err(config("no inputs selected (check role name?)"));
    }
    let mut inputs = Vec::new();
    let mut summaries = Vec::new();
    for (input, role, owner) in targets {
        if !inputs.contains(&input) {
            inputs.push(input.clone());
        }
        summaries.push((input, format!("{verb} {role} in {owner}")));
    }
    Ok((inputs, summaries))
}
type Target = (String, String, String); // input, role, channel/machine
fn channel_targets(
    value: &Value,
    selector: &str,
    role: Option<&str>,
) -> Result<(Vec<Target>, Vec<String>), ConfctlError> {
    let empty = Map::new();
    let channels = value.as_object().unwrap_or(&empty);
    let selected: Vec<_> = channel_names(channels, Some(selector))
        .into_iter()
        .map(str::to_owned)
        .collect();
    if selected.is_empty() {
        return Err(config(&format!("no channels matched '{selector}'")));
    }
    let mut targets = Vec::new();
    for channel in &selected {
        let Some(mapping) = roles(channels.get(channel), &format!("channel '{channel}'"))? else {
            continue;
        };
        if let Some(role) = role {
            if let Some(input) = input_name(
                mapping.get(role),
                &format!("channel '{channel}' role '{role}'"),
            )? {
                targets.push((input.into(), role.into(), channel.clone()));
            }
        } else {
            for (role, input) in mapping {
                let input = input.as_str().ok_or_else(|| {
                    config(&format!(
                        "channel '{channel}' role '{role}' input name is not a string"
                    ))
                })?;
                targets.push((input.into(), role.clone(), channel.clone()));
            }
        }
    }
    Ok((targets, selected))
}
fn roles<'a>(
    value: Option<&'a Value>,
    label: &str,
) -> Result<Option<&'a Map<String, Value>>, ConfctlError> {
    match value {
        None | Some(Value::Null | Value::Bool(false)) => Ok(None),
        Some(Value::Object(map)) => Ok(Some(map)),
        _ => Err(config(&format!("{label} mapping is not an object"))),
    }
}
fn input_name<'a>(value: Option<&'a Value>, label: &str) -> Result<Option<&'a str>, ConfctlError> {
    match value {
        None | Some(Value::Null | Value::Bool(false)) => Ok(None),
        Some(Value::String(name)) => Ok(Some(name)),
        _ => Err(config(&format!("{label} input name is not a string"))),
    }
}
fn shared_uses(
    value: &Value,
    targets: &[Target],
    selected: &[String],
    role: &str,
) -> Result<Option<String>, ConfctlError> {
    let Some(channels) = value.as_object() else {
        return Ok(None);
    };
    let mut shared: BTreeMap<&str, Vec<String>> = BTreeMap::new();
    for (channel, value) in channels {
        let Some(mapping) = roles(Some(value), &format!("channel '{channel}'"))? else {
            continue;
        };
        for (r, value) in mapping {
            let Some(input) = input_name(Some(value), &format!("channel '{channel}' role '{r}'"))?
            else {
                continue;
            };
            if targets.iter().any(|t| t.0 == input) && !(selected.contains(channel) && r == role) {
                shared
                    .entry(input)
                    .or_default()
                    .push(format!("{channel}/{r}"));
            }
        }
    }
    if shared.is_empty() {
        return Ok(None);
    }
    let details = shared
        .into_iter()
        .map(|(input, mut uses)| {
            uses.sort();
            format!("{input} (also used by {})", uses.join(", "))
        })
        .collect::<Vec<_>>()
        .join("; ");
    Ok(Some(format!(
        "selected input(s) are shared outside the requested channel/role: {details}"
    )))
}
async fn commit(
    ctx: &Context,
    root: &ConfigRoot,
    guard: &MutationGuard,
    bytes: &[u8],
    options: &InputChanges,
) -> Result<(), ConfctlError> {
    check(ctx, root, guard)?;
    let mut file = tempfile::Builder::new()
        .prefix("confctl-input-message-")
        .tempfile_in(root.path())
        .map_err(|source| ConfctlError::Io {
            operation: "create Git message file",
            path: Some(root.path().into()),
            source,
        })?;
    file.write_all(bytes)
        .and_then(|()| file.flush())
        .map_err(|source| ConfctlError::Io {
            operation: "write Git message file",
            path: Some(file.path().into()),
            source,
        })?;
    check(ctx, root, guard)?;
    let tty = std::io::stdout().is_terminal();
    let lease = if tty && !std::io::stdin().is_terminal() {
        let fd = std::io::stdout()
            .as_fd()
            .try_clone_to_owned()
            .map_err(|source| ConfctlError::Io {
                operation: "duplicate commit output terminal",
                path: None,
                source,
            })?;
        ctx.ui.pager_foreground(fd).await?
    } else {
        ctx.ui.foreground().await?
    };
    check(ctx, root, guard)?;
    let mut spec = ProcessSpec::new("git", "commit input lock changes");
    spec.args.push("commit".into());
    if options.editor && tty {
        spec.args.push("-e".into());
    }
    spec.args.extend([
        "-F".into(),
        file.path().as_os_str().to_owned(),
        "--".into(),
        "flake.lock".into(),
    ]);
    spec.cwd = Some(root.path().into());
    spec.stdin = StdinPolicy::Foreground;
    spec.stdout = StreamPolicy::Foreground;
    spec.stderr = StreamPolicy::Foreground;
    let mut scope = ProcessScope::new(ctx.cancellation.clone(), TaskId(0));
    scope.foreground = Some(lease);
    let outcome = ctx.processes.run(spec, scope).await?;
    // The owned message file lives through the complete native child and terminal restoration.
    drop(file);
    if outcome.termination == TerminationCause::Canceled || ctx.cancellation.is_cancelled() {
        return Err(process_error(ConfctlError::Cancelled, outcome));
    }
    if !outcome.status.success() {
        return Err(process_error(
            ConfctlError::Child {
                description: "Git commit".into(),
                code: outcome.code(),
                signal: outcome.signal(),
                log: Some(ctx.log_path().into()),
            },
            outcome,
        ));
    }
    check(ctx, root, guard).map_err(|e| process_error(e, outcome))
}
fn canceled(ctx: &Context) -> Result<(), ConfctlError> {
    if ctx.cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}
fn check(ctx: &Context, root: &ConfigRoot, guard: &MutationGuard) -> Result<(), ConfctlError> {
    canceled(ctx)?;
    guard.require_root(root)
}
fn config(text: &str) -> ConfctlError {
    ConfctlError::Configuration(text.into())
}
fn retained(error: ConfctlError, phase: &str) -> ConfctlError {
    error.with_secondary(vec![ConfctlError::State(format!("input mutation stopped during {phase}; completed lock/cache/commit effects may remain; inspect flake.lock and Git state before explicitly retrying or restoring files"))])
}

/// Offline lock inspection needs neither Nix nor cluster outputs nor mutation admission.
pub(super) async fn check_update(
    root: &ConfigRoot,
    old: &std::path::Path,
    new: &std::path::Path,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    let old = FlakeLock::load(&root.path().join(old))?;
    let new = FlakeLock::load(&root.path().join(new))?;
    let changes =
        validate_forward(&old, &new, |proof| super::input_git::compare(ctx, proof)).await?;
    let mut text = if changes.is_empty() {
        "No changes.\n".into()
    } else {
        format!("Updated inputs ({}):\n", changes.len())
    };
    for change in changes {
        text.push_str(&format!(
            "  {}: {} -> {}\n",
            change.name,
            change
                .old_info
                .as_ref()
                .and_then(|i| i.short_rev.as_deref())
                .unwrap_or("-"),
            change.new_info.short_rev.as_deref().unwrap_or("-")
        ));
    }
    ctx.events
        .emit(UiEvent::Output {
            task: TaskId(0),
            stream: OutputStream::Stdout,
            bytes: text.into_bytes(),
        })
        .await
}
