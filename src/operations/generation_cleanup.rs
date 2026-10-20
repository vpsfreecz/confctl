//! Exact read/confirm/remove plans and source-specific retention.
use super::{
    collect_garbage::{check, checked, message},
    generation_selection::{self, Sources},
    host_status::HostGeneration,
};
use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot, MutationLock, Settings},
    domain::{Action, Inventory, Machine, MachineId, SelectionPolicy, select},
    error::ConfctlError,
    events::{OutputStream, TableFormat, TableLayout, TaskId, TaskState, UiEvent},
    generation::{BuildGeneration, GcRoots, GenerationStore},
    nix::{InvocationSettings, NixClient, NixOptions, process_error},
    process::{ProcessRunner, ProcessScope, TerminationCause},
    terminal::ConfirmPrompt,
};
use chrono::{DateTime, Utc};
use serde_json::Value;

struct Removal<'a> {
    host: &'a str,
    name: String,
    build: Option<&'a BuildGeneration>,
    remote: Option<&'a HostGeneration>,
}
impl Removal<'_> {
    fn kind(&self) -> &'static str {
        match (self.build.is_some(), self.remote.is_some()) {
            (true, true) => "build+host",
            (true, false) => "build",
            _ => "host",
        }
    }
}

pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    let (targets, locations, gc, selector) = match action {
        Action::GenerationRemove {
            targets,
            locations,
            gc,
            selector,
        } => (targets, *locations, gc, Some(selector)),
        Action::GenerationRotate {
            targets,
            locations,
            gc,
        } => (targets, *locations, gc, None),
        _ => unreachable!(),
    };
    let now = Utc::now();
    let gc_limit = if locations.remote && gc.enabled {
        Some(super::collect_garbage::limit(gc.max_concurrent)?)
    } else {
        None
    };
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
    let selected = select(inventory, &targets.selection, SelectionPolicy::ALL)?;
    let settings = defaults.read().await?;
    let guard = MutationLock::acquire(&root, &ctx.cancellation).await?;
    let mut next_task = 1;
    let sources = generation_selection::read_sources(
        &root,
        inventory,
        &selected.machines,
        locations,
        ctx,
        &mut next_task,
    )
    .await?;
    let (plan, preview) = if let Some(selector) = selector {
        let entries = generation_selection::unified(&sources, selector, now)?;
        let preview = generation_selection::table(&entries);
        let plan = entries
            .into_iter()
            .map(|entry| Removal {
                host: entry.host,
                name: entry.name,
                build: entry.build.map(|(g, _)| g),
                remote: entry.remote,
            })
            .collect::<Vec<_>>();
        (plan, preview)
    } else {
        let plan = rotate(&sources, inventory, &settings, now)?;
        let preview = rotation_table(&plan);
        (plan, preview)
    };
    check(&ctx.cancellation)?;
    if plan.is_empty() {
        return checked(
            ctx,
            message(OutputStream::Stdout, "No generations to remove\n".into()),
        )
        .await;
    }
    if !ctx
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                message(
                    OutputStream::Stdout,
                    "The following generations will be removed:\n".into(),
                ),
                preview,
                message(
                    OutputStream::Stdout,
                    format!(
                        "\nGarbage collection: {}\n",
                        if gc_limit.is_some() {
                            if selector.is_some() {
                                "yes"
                            } else {
                                "when enabled in configuration"
                            }
                        } else {
                            "no"
                        }
                    ),
                ),
            ],
            assume_yes: targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    check(&ctx.cancellation)?;
    let mut store = None;
    let mut changed = Vec::new();
    for item in &plan {
        let task = TaskId(next_task);
        next_task = next_task
            .checked_add(1)
            .ok_or_else(|| ConfctlError::State("removal task IDs overflow".into()))?;
        let result = async {
            ctx.events
                .emit(UiEvent::TaskPending {
                    task,
                    description: format!("remove generation {}@{}", item.host, item.name),
                })
                .await?;
            check(&ctx.cancellation)?;
            checked(
                ctx,
                UiEvent::TaskStarted {
                    task,
                    description: format!("remove generation {}@{}", item.host, item.name),
                },
            )
            .await?;
            checked(
                ctx,
                message(
                    OutputStream::Stdout,
                    format!(
                        "Removing {} generation {}@{}\n",
                        item.kind(),
                        item.host,
                        item.name
                    ),
                ),
            )
            .await?;
            guard.require_root(&root)?;
            if let Some(generation) = item.build {
                if store.is_none() {
                    store = Some(GenerationStore::new(root.clone(), roots(&root, ctx).await?));
                }
                store.as_ref().unwrap().destroy(
                    &guard,
                    item.host,
                    generation,
                    &ctx.cancellation,
                )?;
            }
            if let Some(generation) = item.remote {
                remove_remote(ctx, inventory, item.host, generation, task).await?;
            }
            check(&ctx.cancellation)
        }
        .await;
        let state = match &result {
            Ok(()) => TaskState::Succeeded,
            Err(error) if error.is_cancelled() => TaskState::Canceled,
            Err(_) => TaskState::Failed,
        };
        let delivery = checked(ctx, UiEvent::TaskFinished { task, state }).await;
        let result = match (result, delivery) {
            (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
            (_, Err(error)) => Err(error),
            (result, Ok(())) => result,
        };
        if let Err(error) = result {
            return Err(error.with_secondary(vec![ConfctlError::State(format!("Removal of {}@{} stopped; earlier root, record or profile deletions may remain. Inspect state before an explicit new command.", item.host, item.name))]));
        }
        if !changed.contains(&item.host) {
            changed.push(item.host);
        }
    }
    if let Some(limit) = gc_limit {
        let machines = gc_targets(
            inventory,
            &sources.selected,
            &changed,
            selector.is_none(),
            &settings,
        )?;
        super::collect_garbage::run_selected(ctx, &root, &guard, &machines, limit, &mut next_task)
            .await?;
    }
    check(&ctx.cancellation)
}

fn machine<'a>(inventory: &'a Inventory, host: &str) -> Result<&'a Machine, ConfctlError> {
    inventory
        .machines
        .iter()
        .find(|m| m.name == host)
        .ok_or_else(|| {
            ConfctlError::Configuration(format!("Machine {host} not in selected inventory"))
        })
}
fn truthy(value: Option<&Value>) -> bool {
    value.is_some_and(|v| !matches!(v, Value::Null | Value::Bool(false)))
}
fn gc_targets<'a>(
    inventory: &'a Inventory,
    selected: &[MachineId],
    changed: &[&str],
    rotating: bool,
    settings: &Settings,
) -> Result<Vec<&'a Machine>, ConfctlError> {
    let mut result: Vec<&Machine> = Vec::new();
    for id in selected {
        let logical = inventory.get(*id);
        if !changed.contains(&logical.name.as_str()) {
            continue;
        }
        let (target, _) = super::host_status::routing(inventory, logical)?;
        let target_host = target.metadata.get("host").and_then(|v| v.get("target"));
        let has_target = if target.metadata.get("host").is_none() || target_host.is_none() {
            true
        } else {
            truthy(target_host)
        };
        if !has_target || rotating && !rotate_gc(target, settings) {
            continue;
        }
        if !result.iter().any(|m| m.name == target.name) {
            result.push(target);
        }
    }
    Ok(result)
}
fn rotate_gc(target: &Machine, settings: &Settings) -> bool {
    truthy(target.metadata.pointer("/buildGenerations/collectGarbage"))
        || truthy(settings.host_generations.get("collectGarbage"))
}

struct Retention {
    min: i128,
    max: i128,
    age: i128,
}
impl Retention {
    fn read(machine: &Machine, global: &Value, group: &str) -> Result<Self, ConfctlError> {
        let integer = |key: &str| {
            let value = machine
                .metadata
                .get(group)
                .and_then(|v| v.get(key))
                .filter(|v| !matches!(v, Value::Null | Value::Bool(false)))
                .or_else(|| global.get(key));
            let parsed = value
                .and_then(Value::as_number)
                .and_then(|n| n.to_string().parse::<i128>().ok());
            parsed.ok_or_else(|| {
                ConfctlError::Configuration(format!(
                    "{}: {group}.{key} must be a representable integer",
                    machine.name
                ))
            })
        };
        Ok(Self {
            min: integer("min")?,
            max: integer("max")?,
            age: integer("maxAge")?
                .checked_mul(1_000_000_000)
                .ok_or_else(|| {
                    ConfctlError::Configuration("generation maxAge is not representable".into())
                })?,
        })
    }
    fn choose(&self, values: &[(bool, i128)], now: i128) -> Result<Vec<usize>, ConfctlError> {
        let mut remaining = values.len() as i128;
        let mut result = Vec::new();
        if remaining <= self.min {
            return Ok(result);
        }
        for (index, (current, date)) in values.iter().enumerate() {
            if *current {
                continue;
            }
            let expires = date.checked_add(self.age).ok_or_else(|| {
                ConfctlError::Configuration("generation expiry is not representable".into())
            })?;
            if remaining > self.max || expires < now {
                result.push(index);
                remaining -= 1;
            }
            if remaining <= self.min {
                break;
            }
        }
        Ok(result)
    }
}
fn nanos<T: chrono::TimeZone>(date: DateTime<T>) -> i128 {
    date.timestamp() as i128 * 1_000_000_000 + date.timestamp_subsec_nanos() as i128
}
fn rotate<'a>(
    sources: &'a Sources,
    inventory: &Inventory,
    settings: &Settings,
    now: DateTime<Utc>,
) -> Result<Vec<Removal<'a>>, ConfctlError> {
    let mut result = Vec::new();
    for (host, status) in &sources.remote {
        let Some(generations) = &status.generations else {
            continue;
        };
        let retention = Retention::read(
            machine(inventory, host)?,
            &settings.host_generations,
            "hostGenerations",
        )?;
        let values = generations
            .iter()
            .map(|g| (g.current, nanos(g.date)))
            .collect::<Vec<_>>();
        for i in retention.choose(&values, nanos(now))? {
            let g = &generations[i];
            result.push(Removal {
                host,
                name: g.date.format("%Y-%m-%d--%H-%M-%S").to_string(),
                build: None,
                remote: Some(g),
            });
        }
    }
    for (host, local) in &sources.local {
        local.require_resolved_current(host)?;
        let retention = Retention::read(
            machine(inventory, host)?,
            &settings.build_generations,
            "buildGenerations",
        )?;
        let values = local
            .generations
            .iter()
            .map(|g| (local.current.as_deref() == Some(&g.name), nanos(g.date)))
            .collect::<Vec<_>>();
        for i in retention.choose(&values, nanos(now))? {
            let g = &local.generations[i];
            result.push(Removal {
                host,
                name: g.name.clone(),
                build: Some(g),
                remote: None,
            });
        }
    }
    Ok(result)
}
fn rotation_table(plan: &[Removal<'_>]) -> UiEvent {
    UiEvent::Table {
        stream: OutputStream::Stdout,
        columns: ["HOST", "NAME", "TYPE", "ID"].map(str::to_owned).to_vec(),
        rows: plan
            .iter()
            .map(|g| {
                vec![
                    g.host.into(),
                    g.name.clone(),
                    g.kind().into(),
                    g.remote.map(|r| r.id.clone()).unwrap_or_default(),
                ]
            })
            .collect(),
        cell_styles: Vec::new(),
        format: TableFormat {
            layout: TableLayout::Columns,
            ..TableFormat::default()
        },
    }
}
async fn remove_remote(
    ctx: &Context,
    inventory: &Inventory,
    host: &str,
    generation: &HostGeneration,
    task: TaskId,
) -> Result<(), ConfctlError> {
    check(&ctx.cancellation)?;
    let logical = machine(inventory, host)?;
    let (target, profile) = super::host_status::routing(inventory, logical)?;
    let config = std::env::var_os("CONFCTL_SSH_CONFIG");
    let argv = vec![
        if logical.carrier.is_some() {
            "carrier-env"
        } else {
            "nix-env"
        }
        .into(),
        "-p".into(),
        profile.into(),
        "--delete-generations".into(),
        generation.id.clone().into(),
    ];
    let spec = super::connection::command_spec(target, config.as_deref(), &argv)?;
    let outcome = ctx
        .processes
        .run(spec, ProcessScope::new(ctx.cancellation.clone(), task))
        .await?;
    if outcome.termination == TerminationCause::Canceled || ctx.cancellation.is_cancelled() {
        return Err(process_error(ConfctlError::Cancelled, outcome));
    }
    if !outcome.status.success() {
        return Err(process_error(
            ConfctlError::Child {
                description: format!("remove profile generation {host} id {}", generation.id),
                code: outcome.code(),
                signal: outcome.signal(),
                log: Some(ctx.log_path().into()),
            },
            outcome,
        ));
    }
    Ok(())
}
async fn roots(root: &ConfigRoot, ctx: &Context) -> Result<GcRoots, ConfctlError> {
    #[cfg(test)]
    if let Some(roots) = TEST_ROOTS.lock().unwrap().as_ref() {
        return Ok(roots.clone());
    }
    GcRoots::for_process(root, &ctx.processes, ctx.cancellation.clone()).await
}
#[cfg(test)]
static TEST_ROOTS: std::sync::Mutex<Option<GcRoots>> = std::sync::Mutex::new(None);
#[cfg(test)]
mod tests;
