//! Build groups and compatible generation publication. Earlier publications are
//! retained on failure; this is deliberately not an all-host transaction.

use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot, MutationGuard, MutationLock, NixSettings},
    domain::{BuildRequest, Inventory, MachineId, ManagedFilter, SelectionPolicy, select},
    error::ConfctlError,
    events::{BuildPhase, EventSink, OutputStream, Style, TaskId, TaskState, UiEvent},
    generation::{BuildGeneration, GcRoots, GenerationStore},
    nix::{InvocationSettings, NixClient, NixOptions, selected_apply},
    process::ProcessRunner,
    terminal::ConfirmPrompt,
};
use chrono::{DateTime, FixedOffset};
use serde_json::{Map, Value};
use std::path::PathBuf;
use tokio_util::sync::CancellationToken;

#[derive(Clone, Debug)]
struct Host {
    name: String,
    key: String,
    inputs: Map<String, Value>,
}

pub(super) async fn run(
    request: &BuildRequest,
    root: ConfigRoot,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    let defaults = InvocationSettings::new(
        root.clone(),
        ctx.processes.clone(),
        ctx.cancellation.clone(),
    );
    let inventory_nix = NixClient::configured(
        root.clone(),
        ctx.processes.clone(),
        NixOptions::default(),
        ctx.cancellation.clone(),
        &defaults,
        ConfigEpoch::default(),
    )
    .await?;
    let inventory = inventory_nix.inventory().await?;
    let inventory = inventory.require_epoch(inventory_nix.epoch())?;
    let selected = select(
        inventory,
        &request.targets.selection,
        SelectionPolicy {
            managed: ManagedFilter::Managed,
            ..SelectionPolicy::ALL
        },
    )?;
    if selected.machines.is_empty() {
        return Err(ConfctlError::State("No machines to build".into()));
    }
    let settings = defaults.read().await?;
    if !ctx
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Heading,
                    text: "The following machines will be built:\n".into(),
                },
                super::machines::table(inventory, &selected, &settings.list_columns, true),
            ],
            assume_yes: request.targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    let nix = NixClient::configured(
        root.clone(),
        ctx.processes.clone(),
        NixOptions {
            show_trace: request.show_trace,
            max_jobs: request.nix.max_jobs.clone(),
            cores: request.nix.cores.clone(),
        },
        ctx.cancellation.clone(),
        &defaults,
        ConfigEpoch::default(),
    )
    .await?;
    let guard = MutationLock::acquire(&root, &ctx.cancellation).await?;
    let mut next_task = 1;
    build_selected(
        ctx,
        &nix,
        inventory,
        &selected.machines,
        &guard,
        &mut next_task,
    )
    .await?;
    Ok(())
}

pub(super) async fn build_selected<P: ProcessRunner>(
    ctx: &Context,
    nix: &NixClient<P>,
    inventory: &Inventory,
    ids: &[MachineId],
    guard: &MutationGuard,
    next_task: &mut u64,
) -> Result<Vec<(MachineId, BuildGeneration)>, ConfctlError> {
    guard.require_root(nix.root())?;
    if *next_task == 0 {
        return Err(ConfctlError::State("Build task IDs must be nonzero".into()));
    }
    check_cancel(&ctx.cancellation)?;
    let full_ui_available = ctx.full_ui_available();
    if full_ui_available {
        ctx.events
            .emit(UiEvent::BuildPhase(BuildPhase::Begin))
            .await?;
    }
    let result = async {
        check_cancel(&ctx.cancellation)?;
        let settings = nix.settings().await?.value;
        let plan = nix.cached_json(".#confctl.buildPlan").await?.value;
        let hosts = plan_hosts(nix, inventory, ids, &plan).await?;
        let groups = groups(hosts, settings.nix.legacy_nix_path);
        let first = TaskId(*next_task);
        *next_task = next_task
            .checked_add(
                u64::try_from(groups.len())
                    .map_err(|_| ConfctlError::State("Build task IDs overflow".into()))?,
            )
            .ok_or_else(|| ConfctlError::State("Build task IDs overflow".into()))?;
        let roots = roots(nix.root(), ctx).await?;
        let store = GenerationStore::new(nix.root().clone(), roots);
        let publication = Publication {
            store: &store,
            guard,
            events: &ctx.events,
            cancellation: &ctx.cancellation,
            time: chrono::Local::now().fixed_offset(),
        };
        let saved = build_groups(
            nix,
            &groups,
            &settings.nix,
            &publication,
            ctx.log_path(),
            first,
            full_ui_available,
        )
        .await?;
        let mut returned = Vec::new();
        for (host, generation) in &saved {
            let id = ids
                .iter()
                .find(|id| inventory.get(**id).name == *host)
                .ok_or_else(|| ConfctlError::State("Build returned an unselected host".into()))?;
            if returned.iter().any(|(previous, _)| previous == id) {
                return Err(ConfctlError::State("Build returned a host twice".into()));
            }
            returned.push((*id, generation.clone()));
        }
        if returned.len() != ids.len() {
            return Err(ConfctlError::State(
                "Build did not return every selected host".into(),
            ));
        }
        Ok((saved, returned))
    }
    .await;
    // End is checked cleanup after all native/group joins and checkpoints. It
    // cannot be selected away by cancellation or replace an earlier failure.
    let result = if full_ui_available {
        match (
            result,
            ctx.events.emit(UiEvent::BuildPhase(BuildPhase::End)).await,
        ) {
            (result, Ok(())) => result,
            (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
            (Ok(_), Err(error)) => Err(error),
        }
    } else {
        result
    };
    let (saved, returned) = result?;
    report_saved(&ctx.events, &ctx.cancellation, &saved).await?;
    Ok(returned)
}

async fn report_saved(
    events: &EventSink,
    cancellation: &CancellationToken,
    saved: &[(String, BuildGeneration)],
) -> Result<(), ConfctlError> {
    check_cancel(cancellation)?;
    events
        .emit(UiEvent::Diagnostic {
            stream: OutputStream::Stdout,
            style: Style::Heading,
            text: "Built generations:\n".into(),
        })
        .await?;
    check_cancel(cancellation)?;
    for (host, generation) in saved {
        events
            .emit(UiEvent::StyledDiagnostic {
                stream: OutputStream::Stdout,
                parts: vec![
                    (Style::Plain, format!("  {host}: ")),
                    (Style::Generation, generation.name.clone()),
                    (Style::Plain, "\n".into()),
                ],
            })
            .await?;
        check_cancel(cancellation)?;
    }
    Ok(())
}

async fn build_groups<P: ProcessRunner>(
    nix: &NixClient<P>,
    groups: &[Vec<Host>],
    settings: &NixSettings,
    publication: &Publication<'_>,
    log_path: &std::path::Path,
    first: TaskId,
    live_progress: bool,
) -> Result<Vec<(String, BuildGeneration)>, ConfctlError> {
    let mut saved = Vec::new();
    for (index, group) in groups.iter().enumerate() {
        let task = TaskId(first.0 + index as u64);
        publication
            .events
            .emit(UiEvent::TaskPending {
                task,
                description: description(index, groups.len(), group),
            })
            .await?;
        publication
            .events
            .emit(UiEvent::LogAvailable {
                task,
                path: log_path.into(),
            })
            .await?;
    }
    for (index, group) in groups.iter().enumerate() {
        let task = TaskId(first.0 + index as u64);
        let mut entered = false;
        let result = async {
            check_cancel(publication.cancellation)?;
            publication
                .events
                .emit(UiEvent::TaskStarted {
                    task,
                    description: description(index, groups.len(), group),
                })
                .await?;
            entered = true;
            if live_progress {
                publication
                    .events
                    .emit(UiEvent::BuildProgress {
                        task,
                        building: None,
                        fetching: None,
                    })
                    .await?;
                check_cancel(publication.cancellation)?;
            }
            build_group(
                nix,
                group,
                settings,
                BuildTask {
                    id: task,
                    live_progress,
                },
                publication,
                &mut saved,
            )
            .await
        }
        .await;
        let state = match &result {
            Ok(()) => TaskState::Succeeded,
            Err(e) if e.is_cancelled() => TaskState::Canceled,
            Err(_) => TaskState::Failed,
        };
        let final_result = publication
            .events
            .emit(UiEvent::TaskFinished { task, state })
            .await;
        // The boundary is publication, not native exit or TaskFinished delivery.
        // Deliver the checkpoint even if acknowledgement canceled the invocation.
        let checkpoint = if entered {
            publication
                .events
                .emit(UiEvent::Checkpoint {
                    key: task,
                    label: format!(
                        "{}: {}",
                        description(index, groups.len(), group),
                        if state == TaskState::Succeeded {
                            "generations published"
                        } else {
                            "stopped; earlier publications retained"
                        }
                    ),
                    state,
                    tasks: vec![task],
                })
                .await
        } else {
            Ok(())
        };
        let result = [final_result, checkpoint]
            .into_iter()
            .fold(result, |result, delivery| match (result, delivery) {
                (r, Ok(())) => r,
                (Ok(()), Err(e)) => Err(e),
                (Err(e), Err(later)) => Err(e.with_secondary(vec![later])),
            })
            // Cancellation during the acknowledgement cancels the invocation,
            // without rewriting the group result already delivered above.
            .and_then(|()| check_cancel(publication.cancellation));
        if let Err(error) = result {
            let mut later = Vec::new();
            for next in index + 1..groups.len() {
                let state = if publication.cancellation.is_cancelled() {
                    TaskState::Canceled
                } else {
                    TaskState::Skipped
                };
                if let Err(e) = publication
                    .events
                    .emit(UiEvent::TaskFinished {
                        task: TaskId(first.0 + next as u64),
                        state,
                    })
                    .await
                {
                    later.push(e);
                }
            }
            if let Err(e) = diagnostic(publication.events, OutputStream::Stderr, format!("Build stopped after saving {} host generation(s); any earlier generation/root/current-link changes are retained.\n", saved.len())).await { later.push(e); }
            return Err(error.with_secondary(later));
        }
    }
    Ok(saved)
}

fn description(index: usize, total: usize, group: &[Host]) -> String {
    format!(
        "build group {}/{} ({} machine{})",
        index + 1,
        total,
        group.len(),
        if group.len() == 1 { "" } else { "s" }
    )
}
fn check_cancel(cancellation: &CancellationToken) -> Result<(), ConfctlError> {
    if cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}
async fn diagnostic(
    events: &EventSink,
    stream: OutputStream,
    text: String,
) -> Result<(), ConfctlError> {
    events
        .emit(UiEvent::Diagnostic {
            stream,
            style: Style::Plain,
            text,
        })
        .await
}
fn object(value: &Value, label: &str) -> Result<Map<String, Value>, ConfctlError> {
    value
        .as_object()
        .cloned()
        .ok_or_else(|| ConfctlError::State(format!("{label} is not an object")))
}
async fn plan_hosts<P: ProcessRunner>(
    nix: &NixClient<P>,
    inventory: &Inventory,
    selected: &[crate::domain::MachineId],
    plan: &Value,
) -> Result<Vec<Host>, ConfctlError> {
    let mut hosts = Vec::new();
    for id in selected {
        let name = &inventory.get(*id).name;
        let row = plan
            .get(name)
            .ok_or_else(|| ConfctlError::State(format!("missing build plan for {name}")))?;
        let key = ["key", "machineKey", "flakeKey"]
            .iter()
            .filter_map(|field| row.get(field))
            .find(|v| !matches!(v, Value::Null | Value::Bool(false)));
        let key = match key {
            Some(Value::String(s)) => s.clone(),
            Some(_) => {
                return Err(ConfctlError::State(
                    "build-plan machine key is not a string".into(),
                ));
            }
            None => nix.machine_key(name).await?,
        };
        let inputs = match row.get("inputs") {
            None | Some(Value::Null | Value::Bool(false)) => Map::new(),
            Some(value) => object(value, "build-plan inputs")?,
        };
        if inputs.values().any(|v| !v.is_string()) {
            return Err(ConfctlError::State(
                "build-plan input path is not a string".into(),
            ));
        }
        hosts.push(Host {
            name: name.clone(),
            key,
            inputs,
        });
    }
    Ok(hosts)
}
fn groups(hosts: Vec<Host>, legacy: bool) -> Vec<Vec<Host>> {
    if !legacy {
        return vec![hosts];
    }
    let mut groups: Vec<Vec<Host>> = Vec::new();
    for host in hosts {
        if let Some(group) = groups.iter_mut().find(|g| g[0].inputs == host.inputs) {
            group.push(host);
        } else {
            groups.push(vec![host]);
        }
    }
    groups
}
fn attr(key: &str) -> String {
    if !key.is_empty()
        && key.chars().enumerate().all(|(i, c)| {
            c.is_ascii_alphabetic()
                || c == '_'
                || (i > 0 && (c.is_ascii_digit() || c == '-' || c == '\''))
        })
    {
        key.into()
    } else {
        format!(
            "\"{}\"",
            key.replace('\\', "\\\\")
                .replace('"', "\\\"")
                .replace("${", "\\${")
        )
    }
}
async fn keyed<P: ProcessRunner>(
    nix: &NixClient<P>,
    target: &str,
    hosts: &[Host],
) -> Result<Value, ConfctlError> {
    nix.eval_apply(
        target,
        Some(&selected_apply(
            &hosts.iter().map(|h| h.key.clone()).collect::<Vec<_>>(),
        )),
    )
    .await
}
async fn legacy_args<P: ProcessRunner>(
    nix: &NixClient<P>,
    hosts: &[Host],
    settings: &NixSettings,
) -> Result<Vec<String>, ConfctlError> {
    if !settings.legacy_nix_path {
        return Ok(Vec::new());
    }
    if !settings.impure_eval {
        return Err(ConfctlError::Configuration(
            "legacyNixPath requires impureEval".into(),
        ));
    }
    let mut hosts = hosts.to_vec();
    for host in &mut hosts {
        host.key = nix.machine_key(&host.name).await?;
    }
    let by_key = keyed(nix, ".#confctl.inputs", &hosts).await?;
    let mut merged = Map::new();
    for host in &hosts {
        let inputs = match by_key.get(&host.key) {
            None | Some(Value::Null | Value::Bool(false)) => {
                nix.eval_json(&format!(".#confctl.inputs.{}", attr(&host.key)))
                    .await?
            }
            Some(value) => value.clone(),
        };
        for (name, path) in object(&inputs, "legacy inputs")? {
            if path.as_str().is_none_or(|s| s.is_empty()) {
                continue;
            }
            if merged.get(&name).is_some_and(|previous| *previous != path) {
                return Err(ConfctlError::Configuration(format!(
                    "legacyNixPath requires consistent input {name} across hosts; build hosts separately"
                )));
            }
            merged.insert(name, path);
        }
    }
    Ok(settings
        .legacy_names
        .iter()
        .filter_map(|name| {
            merged
                .get(name)
                .and_then(Value::as_str)
                .map(|path| vec!["-I".into(), format!("{name}={path}")])
        })
        .flatten()
        .collect())
}

struct Publication<'a> {
    store: &'a GenerationStore,
    guard: &'a MutationGuard,
    events: &'a EventSink,
    cancellation: &'a CancellationToken,
    time: DateTime<FixedOffset>,
}
#[derive(Clone, Copy)]
struct BuildTask {
    id: TaskId,
    live_progress: bool,
}
async fn build_group<P: ProcessRunner>(
    nix: &NixClient<P>,
    hosts: &[Host],
    settings: &NixSettings,
    task: BuildTask,
    publication: &Publication<'_>,
    saved: &mut Vec<(String, BuildGeneration)>,
) -> Result<(), ConfctlError> {
    let BuildTask {
        id: task,
        live_progress,
    } = task;
    let legacy = legacy_args(nix, hosts, settings).await?;
    let installables = hosts
        .iter()
        .flat_map(|host| {
            [
                format!(".#confctl.build.{}.toplevel", attr(&host.key)),
                format!(".#confctl.build.{}.autoRollback", attr(&host.key)),
            ]
        })
        .collect::<Vec<_>>();
    nix.build_json_for_task(&installables, &legacy, task, live_progress)
        .await?;
    let outputs = keyed(nix, ".#confctl.build", hosts).await?;
    let mut pending = Vec::new();
    // Ruby updates all reused records before obtaining metadata/publishing new ones.
    for host in hosts {
        check_cancel(publication.cancellation)?;
        let output = outputs.get(&host.key).ok_or_else(|| {
            ConfctlError::State(format!("missing build outputs for {}", host.key))
        })?;
        let path = |field| {
            output
                .get(field)
                .and_then(Value::as_str)
                .filter(|s| !s.is_empty())
                .map(PathBuf::from)
                .ok_or_else(|| {
                    ConfctlError::State(format!("invalid build output {field} for {}", host.key))
                })
        };
        let toplevel = path("toplevel")?;
        let rollback = path("autoRollback")?;
        let inventory = publication.store.load(&host.name)?;
        for message in &inventory.diagnostics {
            diagnostic(
                publication.events,
                OutputStream::Stderr,
                format!("{message}\n"),
            )
            .await?;
        }
        if let Some(generation) = inventory
            .generations
            .iter()
            .find(|generation| generation.toplevel == toplevel && generation.inputs == host.inputs)
        {
            publication
                .store
                .set_current(publication.guard, &host.name, &generation.name)?;
            saved.push((host.name.clone(), generation.clone()));
            saved_notice(publication.events, &host.name, &generation.name, true).await?;
            check_cancel(publication.cancellation)?;
        } else {
            pending.push((host, toplevel, rollback));
        }
    }
    if !pending.is_empty() {
        let pending_hosts = pending
            .iter()
            .map(|(host, _, _)| (*host).clone())
            .collect::<Vec<_>>();
        let infos = keyed(nix, ".#confctl.inputsInfo", &pending_hosts).await?;
        for (host, toplevel, rollback) in pending {
            check_cancel(publication.cancellation)?;
            let info = match infos.get(&host.key) {
                None | Some(Value::Null | Value::Bool(false)) => {
                    nix.eval_json(&format!(
                        ".#confctl.inputsInfo.{}",
                        attr(&nix.machine_key(&host.name).await?)
                    ))
                    .await?
                }
                Some(value) => value.clone(),
            };
            let generation = BuildGeneration {
                name: publication.time.format("%Y-%m-%d--%H-%M-%S").to_string(),
                date: publication.time,
                toplevel,
                auto_rollback: Some(rollback),
                inputs: host.inputs.clone(),
                inputs_info: object(&info, "inputsInfo")?,
            };
            check_cancel(publication.cancellation)?;
            publication
                .store
                .publish(publication.guard, &host.name, &generation)?;
            saved.push((host.name.clone(), generation.clone()));
            saved_notice(publication.events, &host.name, &generation.name, false).await?;
            check_cancel(publication.cancellation)?;
        }
    }
    check_cancel(publication.cancellation)
}

async fn saved_notice(
    events: &EventSink,
    host: &str,
    generation: &str,
    reused: bool,
) -> Result<(), ConfctlError> {
    events
        .emit(UiEvent::StyledDiagnostic {
            stream: OutputStream::Stdout,
            parts: vec![
                (Style::Plain, format!("Saved {host}: ")),
                (Style::Generation, generation.into()),
                (
                    Style::Plain,
                    if reused { " (reused)\n" } else { "\n" }.into(),
                ),
            ],
        })
        .await
}

#[cfg(test)]
static TEST_ROOTS: std::sync::Mutex<Option<GcRoots>> = std::sync::Mutex::new(None);
#[cfg(test)]
pub(super) fn set_test_roots(roots: GcRoots) {
    *TEST_ROOTS.lock().unwrap() = Some(roots);
}
async fn roots(root: &ConfigRoot, ctx: &Context) -> Result<GcRoots, ConfctlError> {
    #[cfg(test)]
    if let Some(roots) = TEST_ROOTS.lock().unwrap().take() {
        return Ok(roots);
    }
    GcRoots::for_process(root, &ctx.processes, ctx.cancellation.clone()).await
}

#[cfg(test)]
mod tests;
