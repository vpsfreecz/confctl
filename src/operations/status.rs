//! Status against a newly built target, configured inputs or saved generations.
use super::host_status::HostStatus;
use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot, MutationLock},
    domain::{
        Action, GenerationSelector, Inventory, Machine, MachineId, ManagedFilter, SelectionPolicy,
        select,
    },
    error::ConfctlError,
    events::{OutputStream, Style, TableFormat, TableLayout, TaskId, TaskState, UiEvent},
    generation::{BuildGeneration, GenerationInventory},
    nix::{InvocationSettings, NixClient, NixOptions, bytes, process_error},
    process::{Captured, ProcessOutcome, ProcessRunner, ProcessScope, TerminationCause},
    terminal::ConfirmPrompt,
};
use serde_json::{Map, Value};
use std::{
    collections::BTreeSet,
    ffi::OsStr,
    future::{Future, poll_fn},
    path::PathBuf,
    task::Poll,
};
use tokio_util::sync::CancellationToken;

type Inputs = Map<String, Value>;
struct Row {
    host: String,
    observed: HostStatus,
    target: Option<Inputs>,
    target_toplevel: Option<PathBuf>,
    deployed: Option<Inputs>,
    local_count: usize,
    failed: bool,
    diagnostics: Vec<String>,
    // Retain ordinary failures until their diagnostics/table have been delivered.
    native_failures: Vec<ProcessOutcome>,
    eval_failures: Vec<ConfctlError>,
}

pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    context: &Context,
) -> Result<(), ConfctlError> {
    let Action::Status {
        targets,
        generation,
        skip_toplevel,
    } = action
    else {
        unreachable!("status adapter requires status action")
    };
    let building = !*skip_toplevel && generation.is_none();
    let generation = generation.as_ref().filter(|_| !*skip_toplevel);
    let generation_text = match generation {
        Some(GenerationSelector::Current) => "current",
        Some(GenerationSelector::Offset(text) | GenerationSelector::Name(text)) => text,
        None if building => "new build",
        None => "none",
    };
    let cancellation = context.cancellation.child_token();
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
    let mut selected = select(
        inventory,
        &targets.selection,
        SelectionPolicy {
            managed: ManagedFilter::Managed,
            ..SelectionPolicy::ALL
        },
    )?;
    selected.machines.retain(|id| eligible(inventory.get(*id)));
    if selected.machines.is_empty() {
        return Err(ConfctlError::Selection("No machines to check".into()));
    }
    let settings = defaults.read().await?;
    if !context
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: if building {
                        "The following machines will be BUILT and then checked:\n"
                    } else {
                        "The following machines will be checked:\n"
                    }
                    .into(),
                },
                super::machines::table(inventory, &selected, &settings.list_columns, true),
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: format!("\nGeneration: {generation_text}\n"),
                },
            ],
            assume_yes: targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    check_cancel(&context.cancellation)?;
    let guard = if building {
        Some(MutationLock::acquire(&root, &context.cancellation).await?)
    } else {
        None
    };
    let mut first_task = 1;
    let jobs = if building {
        let built = super::build::build_selected(
            context,
            &nix,
            inventory,
            &selected.machines,
            guard.as_ref().expect("default status owns its guard"),
            &mut first_task,
        )
        .await?;
        selected
            .machines
            .iter()
            .map(|id| Job {
                machine: *id,
                generation: Some(
                    built
                        .iter()
                        .find(|(host, _)| host == id)
                        .expect("complete selected build")
                        .1
                        .clone(),
                ),
                use_saved_inputs: false,
                local_count: 0,
            })
            .collect()
    } else if let Some(selector) = generation {
        resolve_saved(
            &root,
            inventory,
            &selected.machines,
            selector,
            targets.yes,
            context,
        )
        .await?
    } else {
        selected
            .machines
            .iter()
            .map(|id| Job {
                machine: *id,
                generation: None,
                use_saved_inputs: false,
                local_count: 0,
            })
            .collect()
    };
    first_task
        .checked_add(
            u64::try_from(jobs.len())
                .map_err(|_| ConfctlError::State("Status task IDs overflow".into()))?,
        )
        .ok_or_else(|| ConfctlError::State("Status task IDs overflow".into()))?;
    for job in &jobs {
        super::host_status::routing(inventory, inventory.get(job.machine))?;
    }
    let ssh_config = std::env::var_os("CONFCTL_SSH_CONFIG");
    let mut pending = jobs
        .iter()
        .enumerate()
        .map(|(index, job)| {
            let cancellation = &cancellation;
            let ssh_config = ssh_config.as_deref();
            let nix = &nix;
            Some(Box::pin(async move {
                let machine = inventory.get(job.machine);
                let task = TaskId(first_task + index as u64);
                let description = format!("check status on {}", machine.name);
                let result: Result<Row, ConfctlError> = async {
                    context
                        .events
                        .emit(UiEvent::TaskPending {
                            task,
                            description: description.clone(),
                        })
                        .await?;
                    check_cancel(cancellation)?;
                    context
                        .events
                        .emit(UiEvent::TaskStarted { task, description })
                        .await?;
                    check_cancel(cancellation)?;
                    let row = observe(inventory, job, nix, context, cancellation, task, ssh_config)
                        .await?;
                    for diagnostic in &row.diagnostics {
                        context
                            .events
                            .emit(UiEvent::Diagnostic {
                                stream: OutputStream::Stderr,
                                style: Style::Plain,
                                text: format!("{diagnostic}\n"),
                            })
                            .await?;
                        check_cancel(cancellation)?;
                    }
                    Ok(row)
                }
                .await;
                let state = match &result {
                    Ok(row) if !row.failed => TaskState::Succeeded,
                    Ok(_) => TaskState::Failed,
                    Err(e) if e.is_cancelled() => TaskState::Canceled,
                    Err(_) => TaskState::Failed,
                };
                let delivery = context
                    .events
                    .emit(UiEvent::TaskFinished { task, state })
                    .await;
                match (result, delivery) {
                    (Err(e), Err(delivery)) => Err(e.with_secondary(vec![delivery])),
                    (_, Err(e)) => Err(e),
                    (Err(e), Ok(())) => Err(e),
                    (Ok(row), Ok(())) => {
                        check_cancel(cancellation)?;
                        Ok(row)
                    }
                }
            }))
        })
        .collect::<Vec<_>>();
    let mut rows = std::iter::repeat_with(|| None)
        .take(pending.len())
        .collect::<Vec<_>>();
    let mut failures = Vec::new();
    poll_fn(|cx| {
        let mut waiting = false;
        for (index, slot) in pending.iter_mut().enumerate() {
            let Some(future) = slot else { continue };
            match future.as_mut().poll(cx) {
                Poll::Pending => waiting = true,
                Poll::Ready(result) => {
                    *slot = None;
                    match result {
                        Ok(row) => rows[index] = Some(row),
                        Err(e) => {
                            failures.push(e);
                            cancellation.cancel();
                        }
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
    if !failures.is_empty() {
        return Err(failures.remove(0).with_secondary(failures));
    }
    check_cancel(&context.cancellation)?;
    let mut rows = rows
        .into_iter()
        .map(|r| r.expect("joined status query"))
        .collect::<Vec<_>>();
    for row in rows.iter_mut().filter(|_| generation.is_none()) {
        let local = GenerationInventory::load_local(&root, &row.host)?;
        row.local_count = local.generations.len();
        for diagnostic in local.diagnostics {
            context
                .events
                .emit(UiEvent::Diagnostic {
                    stream: OutputStream::Stderr,
                    style: Style::Plain,
                    text: format!("{diagnostic}\n"),
                })
                .await?;
            check_cancel(&context.cancellation)?;
        }
    }
    context.events.emit(table(&rows)).await?;
    check_cancel(&context.cancellation)
}

struct Job {
    machine: MachineId,
    generation: Option<BuildGeneration>,
    use_saved_inputs: bool,
    local_count: usize,
}

async fn resolve_saved(
    root: &ConfigRoot,
    inventory: &Inventory,
    ids: &[MachineId],
    selector: &GenerationSelector,
    assume_yes: bool,
    context: &Context,
) -> Result<Vec<Job>, ConfctlError> {
    let mut jobs = Vec::new();
    let mut missing = Vec::new();
    for id in ids {
        check_cancel(&context.cancellation)?;
        let host = &inventory.get(*id).name;
        let local = GenerationInventory::load_local(root, host)?;
        for diagnostic in &local.diagnostics {
            context
                .events
                .emit(UiEvent::Diagnostic {
                    stream: OutputStream::Stderr,
                    style: Style::Plain,
                    text: format!("{diagnostic}\n"),
                })
                .await?;
            check_cancel(&context.cancellation)?;
        }
        match local.select_saved(host, selector)? {
            Some(generation) => jobs.push(Job {
                machine: *id,
                generation: Some(generation.clone()),
                use_saved_inputs: true,
                local_count: local.generations.len(),
            }),
            None => missing.push(*id),
        }
    }
    context
        .events
        .emit(saved_table(inventory, &jobs, &missing)?)
        .await?;
    check_cancel(&context.cancellation)?;
    if jobs.is_empty() {
        return Err(ConfctlError::Selection(
            "Generation not found on any selected machine".into(),
        ));
    }
    if !missing.is_empty()
        && !context
            .ui
            .confirm(ConfirmPrompt {
                preview: vec![UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: format!(
                        "Generation was not found on:\n{}These hosts will be ignored.\n",
                        missing
                            .iter()
                            .map(|id| format!("  {}\n", inventory.get(*id).name))
                            .collect::<String>()
                    ),
                }],
                assume_yes,
                ..ConfirmPrompt::default()
            })
            .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    check_cancel(&context.cancellation)?;
    Ok(jobs)
}

fn saved_table(
    inventory: &Inventory,
    jobs: &[Job],
    missing: &[MachineId],
) -> Result<UiEvent, ConfctlError> {
    let roles = jobs
        .iter()
        .filter_map(|job| job.generation.as_ref())
        .flat_map(|generation| generation.inputs_info.keys().cloned())
        .collect::<BTreeSet<_>>();
    let mut columns = vec!["NAME".into(), "GENERATION".into(), "KERNEL".into()];
    columns.extend(roles.iter().map(|r| r.to_uppercase()));
    let mut rows = Vec::new();
    for job in jobs {
        let generation = job.generation.as_ref().expect("resolved saved generation");
        let mut row = vec![
            inventory.get(job.machine).name.clone(),
            generation.name.clone(),
            super::generation_list::kernel_version(&generation.toplevel)?.unwrap_or_default(),
        ];
        row.extend(
            roles.iter().map(|role| {
                super::generation_list::input_revision(generation.inputs_info.get(role))
            }),
        );
        rows.push(row);
    }
    rows.extend(missing.iter().map(|id| {
        let mut row = vec![inventory.get(*id).name.clone(), "not found".into()];
        row.resize(columns.len(), String::new());
        row
    }));
    Ok(UiEvent::Table {
        stream: OutputStream::Stdout,
        columns,
        rows,
        cell_styles: Vec::new(),
        format: TableFormat {
            layout: TableLayout::Columns,
            ..TableFormat::default()
        },
    })
}

fn eligible(machine: &Machine) -> bool {
    machine.carrier.is_some()
        || machine
            .metadata
            .get("host")
            .and_then(|v| v.get("target"))
            .is_none_or(|v| !matches!(v, Value::Null | Value::Bool(false)))
}
fn check_cancel(cancellation: &CancellationToken) -> Result<(), ConfctlError> {
    if cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}

async fn observe<P: ProcessRunner>(
    inventory: &Inventory,
    job: &Job,
    nix: &NixClient<P>,
    context: &Context,
    cancellation: &CancellationToken,
    task: TaskId,
    ssh_config: Option<&OsStr>,
) -> Result<Row, ConfctlError> {
    let machine = inventory.get(job.machine);
    let mut row = Row {
        host: machine.name.clone(),
        observed: HostStatus::default(),
        target: None,
        target_toplevel: job.generation.as_ref().map(|g| g.toplevel.clone()),
        deployed: None,
        local_count: job.local_count,
        failed: false,
        diagnostics: Vec::new(),
        native_failures: Vec::new(),
        eval_failures: Vec::new(),
    };
    if job.use_saved_inputs {
        let generation = job
            .generation
            .as_ref()
            .expect("saved inputs need a saved generation");
        row.target = normalize(&Value::Object(generation.inputs_info.clone()));
        if row.target.as_ref().is_some_and(Inputs::is_empty) {
            row.diagnostics.push(format!(
                "{}: saved inputs info has no roles to compare",
                machine.name
            ));
        }
    } else {
        let key = nix.machine_key(&machine.name).await?;
        match nix
            .eval_json(&format!(".#confctl.inputsInfo.{}", attribute(&key)))
            .await
        {
            Ok(value) => {
                row.target = normalize(&value);
                if row.target.is_none() {
                    row.failed = true;
                    row.diagnostics.push(format!(
                        "{}: target inputs info is not an object",
                        machine.name
                    ));
                } else if row.target.as_ref().is_some_and(Inputs::is_empty) {
                    row.diagnostics.push(format!(
                        "{}: target inputs info has no roles to compare",
                        machine.name
                    ));
                }
            }
            Err(error) if native_eval_failure(&error) => {
                let ConfctlError::NixCommandFailure(failure) = &error else {
                    unreachable!()
                };
                let ConfctlError::ProcessFailure { evidence, .. } = failure.source.as_ref() else {
                    unreachable!()
                };
                let stderr = match bytes(&evidence.stderr).await {
                    Ok(bytes) => bytes,
                    Err(source) => return Err(error.with_secondary(vec![source])),
                };
                row.diagnostics.push(format!(
                    "{}: target inputs evaluation failed: {error}\n{}",
                    machine.name,
                    String::from_utf8_lossy(&stderr)
                ));
                row.failed = true;
                row.eval_failures.push(error);
            }
            Err(error) => return Err(error),
        }
    }
    check_cancel(cancellation)?;
    row.observed = super::host_status::query(
        inventory,
        machine,
        &context.processes,
        cancellation,
        task,
        ssh_config,
        job.generation.is_some(),
    )
    .await?;
    row.diagnostics
        .extend(row.observed.diagnostics.iter().cloned());
    if let Some(failure) = &row.observed.failure {
        row.failed = true;
        row.diagnostics.push(failure.diagnostic(&machine.name));
    } else {
        if row
            .observed
            .uptime
            .is_some_and(|u| !u.is_finite() || u < 0.0)
        {
            row.observed.uptime = None;
            row.failed = true;
            row.diagnostics
                .push(format!("{}: invalid uptime observation", machine.name));
        }
        row.deployed = deployed_inputs(
            inventory,
            machine,
            context,
            cancellation,
            task,
            ssh_config,
            &mut row,
        )
        .await?;
    }
    check_cancel(cancellation)?;
    Ok(row)
}

fn native_eval_failure(error: &ConfctlError) -> bool {
    let ConfctlError::NixCommandFailure(failure) = error else {
        return false;
    };
    let ConfctlError::ProcessFailure { source, evidence } = failure.source.as_ref() else {
        return false;
    };
    matches!(source.as_ref(), ConfctlError::Child { .. })
        && evidence.status.is_some_and(|status| !status.success())
        && evidence.stdout_complete
        && evidence.stderr_complete
        && !matches!(evidence.stdout, Captured::Unavailable)
        && !matches!(evidence.stderr, Captured::Unavailable)
}

async fn read_file(
    machine: &Machine,
    path: &str,
    context: &Context,
    cancellation: &CancellationToken,
    task: TaskId,
    ssh_config: Option<&OsStr>,
) -> Result<Result<Vec<u8>, ProcessOutcome>, ConfctlError> {
    check_cancel(cancellation)?;
    let mut spec =
        super::connection::command_spec(machine, ssh_config, &["cat".into(), path.into()])?;
    spec.redacted_description = "read deployed inputs info".into();
    let outcome = context
        .processes
        .run(spec, ProcessScope::new(cancellation.clone(), task))
        .await?;
    if outcome.termination == TerminationCause::Canceled || cancellation.is_cancelled() {
        return Err(process_error(ConfctlError::Cancelled, outcome));
    }
    if !outcome.stdout_complete
        || !outcome.stderr_complete
        || matches!(outcome.stdout, Captured::Unavailable)
        || matches!(outcome.stderr, Captured::Unavailable)
    {
        return Err(process_error(
            ConfctlError::State("deployed inputs capture is incomplete".into()),
            outcome,
        ));
    }
    if !outcome.status.success() {
        return Ok(Err(outcome));
    }
    match bytes(&outcome.stdout).await {
        Ok(bytes) if bytes.len() as u64 == outcome.stdout.len() => Ok(Ok(bytes)),
        Ok(_) => Err(process_error(
            ConfctlError::State("deployed inputs capture length changed".into()),
            outcome,
        )),
        Err(e) => Err(process_error(e, outcome)),
    }
}

async fn deployed_inputs(
    inventory: &Inventory,
    machine: &Machine,
    context: &Context,
    cancellation: &CancellationToken,
    task: TaskId,
    ssh_config: Option<&OsStr>,
    row: &mut Row,
) -> Result<Option<Inputs>, ConfctlError> {
    let (target, profile) = super::host_status::routing(inventory, machine)?;
    let earlier_failed = row.failed;
    if machine.carrier.is_some() {
        match read_file(
            target,
            &format!("{profile}/machine.json"),
            context,
            cancellation,
            task,
            ssh_config,
        )
        .await?
        {
            Ok(bytes) => match serde_json::from_slice::<Value>(&bytes) {
                Ok(value) => {
                    if let Some(selected) = value
                        .get("inputs-info")
                        .filter(|v| !matches!(v, Value::Null | Value::Bool(false)))
                    {
                        let info = payload(selected.clone());
                        if info.is_none() {
                            row.failed = true;
                            row.diagnostics.push(format!(
                                "{}: selected machine.json inputs-info is invalid",
                                machine.name
                            ));
                        }
                        return Ok(info);
                    }
                }
                Err(_) => {
                    row.failed = true;
                    row.diagnostics.push(format!(
                        "{}: invalid machine.json; reading profile inputs info",
                        machine.name
                    ));
                }
            },
            Err(outcome) => record_failure(row, "machine.json", outcome).await?,
        }
    }
    let path = if machine.carrier.is_some() {
        format!("{profile}/etc/confctl/inputs-info.json")
    } else {
        "/etc/confctl/inputs-info.json".into()
    };
    match read_file(target, &path, context, cancellation, task, ssh_config).await? {
        Ok(bytes) => {
            let info = serde_json::from_slice::<Value>(&bytes)
                .ok()
                .and_then(payload);
            if info.is_none() {
                row.failed = true;
                row.diagnostics
                    .push(format!("{}: invalid deployed inputs info", machine.name));
            } else if machine.carrier.is_some() {
                row.failed = earlier_failed;
                row.diagnostics.push(format!(
                    "{}: deployed inputs read successfully from profile fallback",
                    machine.name
                ));
            }
            Ok(info)
        }
        Err(outcome) => {
            record_failure(row, "deployed inputs info", outcome).await?;
            Ok(None)
        }
    }
}

async fn record_failure(
    row: &mut Row,
    phase: &str,
    outcome: ProcessOutcome,
) -> Result<(), ConfctlError> {
    let stderr = match bytes(&outcome.stderr).await {
        Ok(bytes) => bytes,
        Err(e) => return Err(process_error(e, outcome)),
    };
    row.diagnostics.push(format!(
        "{}: {phase} failed (exit {:?}, signal {:?})\n{}",
        row.host,
        outcome.code(),
        outcome.signal(),
        String::from_utf8_lossy(&stderr)
    ));
    row.failed = true;
    row.native_failures.push(outcome);
    Ok(())
}

fn payload(value: Value) -> Option<Inputs> {
    let value = match value {
        Value::String(json) => serde_json::from_str(&json).ok()?,
        value => value,
    };
    normalize(&value)
}
fn normalize(value: &Value) -> Option<Inputs> {
    let object = value.as_object()?;
    Some(
        object
            .iter()
            .map(|(role, value)| {
                let mut info = Map::new();
                match value {
                    Value::String(rev) => {
                        info.insert("rev".into(), value.clone());
                        info.insert(
                            "shortRev".into(),
                            Value::String(rev.chars().take(8).collect()),
                        );
                    }
                    Value::Object(values) => {
                        for (key, value) in values {
                            match key.as_str() {
                                "input" | "url" | "rev" | "shortRev" | "lastModified" => {
                                    info.insert(key.clone(), value.clone());
                                }
                                "short_rev" => {
                                    info.insert("shortRev".into(), value.clone());
                                }
                                _ => {}
                            }
                        }
                    }
                    _ => {}
                }
                if info.get("shortRev").is_none_or(Value::is_null)
                    && let Some(Value::String(rev)) = info.get("rev")
                {
                    info.insert(
                        "shortRev".into(),
                        Value::String(rev.chars().take(8).collect()),
                    );
                }
                (role.clone(), Value::Object(info))
            })
            .collect(),
    )
}

#[derive(Clone, Copy, Debug, PartialEq)]
enum Comparison {
    Same,
    Changed,
    Unknown,
}
fn comparison(target: Option<&Value>, deployed: Option<&Value>) -> Comparison {
    let rev = |info: Option<&Value>| {
        info.and_then(|v| v.get("rev"))
            .filter(|v| !v.is_null())
            .cloned()
    };
    match (rev(target), rev(deployed)) {
        (Some(a), Some(b)) if a == b => Comparison::Same,
        (Some(_), Some(_)) => Comparison::Changed,
        _ => Comparison::Unknown,
    }
}
fn table(rows: &[Row]) -> UiEvent {
    let roles = rows
        .iter()
        .flat_map(|r| r.target.iter().chain(r.deployed.iter()))
        .flat_map(|map| map.keys().cloned())
        .collect::<BTreeSet<_>>();
    let mut columns = ["HOST", "ONLINE", "UPTIME", "STATUS", "GENERATIONS"]
        .map(str::to_owned)
        .to_vec();
    columns.extend(roles.iter().map(|s| s.to_uppercase()));
    let mut cells = Vec::new();
    let mut styles = Vec::new();
    for row in rows {
        let online = row
            .observed
            .uptime
            .is_some_and(|u| u.is_finite() && u >= 0.0);
        let ok = online
            && row.target_toplevel.as_ref().is_none_or(|target| {
                row.observed
                    .current_toplevel
                    .as_ref()
                    .is_some_and(|current| current.as_os_str() == target.as_os_str())
            })
            && row.target.as_ref().is_some_and(|target| {
                !target.is_empty()
                    && target.iter().all(|(role, info)| {
                        comparison(Some(info), row.deployed.as_ref().and_then(|d| d.get(role)))
                            == Comparison::Same
                    })
            });
        let mut cells_row = vec![
            row.host.clone(),
            if online { "yes" } else { "unknown" }.into(),
            row.observed
                .uptime
                .map(duration)
                .unwrap_or_else(|| "unknown".into()),
            if ok { "ok" } else { "outdated" }.into(),
            format!(
                "{}:{}",
                row.local_count,
                row.observed
                    .generations
                    .as_ref()
                    .map(|g| g.len().to_string())
                    .unwrap_or_else(|| "unknown".into())
            ),
        ];
        let mut styles_row = vec![
            Style::Machine,
            if online {
                Style::Success
            } else {
                Style::Incomplete
            },
            Style::Plain,
            if ok { Style::Success } else { Style::Failure },
            Style::Plain,
        ];
        for role in &roles {
            let deployed = row.deployed.as_ref().and_then(|d| d.get(role));
            let state = comparison(row.target.as_ref().and_then(|t| t.get(role)), deployed);
            let revision = super::generation_list::input_revision(deployed);
            let (label, style) = match state {
                Comparison::Same => ("same", Style::Success),
                Comparison::Changed => ("changed", Style::Failure),
                Comparison::Unknown => ("unknown", Style::Incomplete),
            };
            cells_row.push(format!(
                "{} ({label})",
                if revision.is_empty() {
                    "unknown"
                } else {
                    &revision
                }
            ));
            styles_row.push(style);
        }
        cells.push(cells_row);
        styles.push(styles_row);
    }
    UiEvent::Table {
        stream: OutputStream::Stdout,
        columns,
        rows: cells,
        cell_styles: styles,
        format: TableFormat {
            layout: TableLayout::Columns,
            ..TableFormat::default()
        },
    }
}
fn duration(uptime: f64) -> String {
    if !uptime.is_finite() || uptime < 0.0 {
        return "unknown".into();
    }
    if uptime < 1.0 {
        return format!("{uptime:.3}s");
    }
    let seconds = uptime.floor();
    format!(
        "{:.0}d {:.0}h {:.0}m {:.0}s",
        (seconds / 86400.0).floor(),
        ((seconds % 86400.0) / 3600.0).floor(),
        ((seconds % 3600.0) / 60.0).floor(),
        seconds % 60.0
    )
}
fn attribute(key: &str) -> String {
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

#[cfg(test)]
mod tests;
