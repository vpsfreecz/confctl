//! Copy saved generations or prepare standalone hosts for boot without reboot.
//! Earlier copies, kernel refreshes and activation effects survive later failure.
use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot, MutationLock, StateFiles},
    domain::{
        AutoRollback, DeployRequest, GenerationSelector, Inventory, Machine, MachineId,
        ManagedFilter, SelectionPolicy, SwitchAction, select,
    },
    error::ConfctlError,
    events::{
        EventSink, OutputStream, Style, TableFormat, TableLayout, TaskId, TaskState, UiEvent,
    },
    generation::{BuildGeneration, GenerationInventory},
    nix::{InvocationSettings, NixClient, NixOptions},
    process::{
        PartialProcessEvidence, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec,
        StreamPolicy, TerminationCause,
    },
    runtime_kernels::{FeatureContext, refresh},
    terminal::ConfirmPrompt,
    timing::Phase,
};
use std::{
    ffi::{OsStr, OsString},
    future::{Future, poll_fn},
    os::unix::ffi::{OsStrExt, OsStringExt},
    task::Poll,
};
use tokio_util::sync::CancellationToken;

pub(super) async fn run(
    request: &DeployRequest,
    root: ConfigRoot,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    let selector = request.generation.as_ref().ok_or_else(|| {
        ConfctlError::Configuration("deployment requires a saved generation".into())
    })?;
    let defaults = InvocationSettings::new(
        root.clone(),
        ctx.processes.clone(),
        ctx.cancellation.clone(),
    );
    let mut nix = NixClient::configured(
        root.clone(),
        ctx.processes.clone(),
        NixOptions::default(),
        ctx.cancellation.clone(),
        &defaults,
        ConfigEpoch::default(),
    )
    .await?;
    let snapshot = nix.inventory().await?;
    let inventory = snapshot.require_epoch(nix.epoch())?;
    let selected = select(
        inventory,
        &request.build.targets.selection,
        SelectionPolicy {
            managed: ManagedFilter::Managed,
            ..SelectionPolicy::ALL
        },
    )?;
    if selected.machines.is_empty() {
        return Err(ConfctlError::Selection("No machines to deploy".into()));
    }
    let serial = request.one_by_one || request.interactive;
    let limit = if serial {
        1
    } else {
        usize::try_from(request.max_concurrent_copy)
            .ok()
            .filter(|n| *n > 0)
            .ok_or_else(|| ConfctlError::Usage("max-concurrent-copy must be positive".into()))?
    };
    let settings = defaults.read().await?;
    confirm_targets(
        ctx,
        request,
        inventory,
        &selected,
        &settings.list_columns,
        selector,
    )
    .await?;
    check_cancel(&ctx.cancellation)?;
    let guard = MutationLock::acquire(&root, &ctx.cancellation).await?;
    let first_task = refresh_selected(
        ctx,
        &root,
        inventory,
        &selected.machines,
        &settings.runtime_kernels,
        &guard,
    )
    .await?;
    if first_task > 1 {
        nix.invalidate_configuration();
    }
    let (mut jobs, missing) = resolve(
        &root,
        inventory,
        &selected.machines,
        selector,
        &ctx.events,
        &ctx.cancellation,
    )
    .await?;
    if jobs.is_empty() {
        return Err(ConfctlError::State("No generation found".into()));
    }
    confirm_resolved(ctx, request, inventory, selector, &jobs, &missing).await?;
    check_cancel(&ctx.cancellation)?;
    if !request.copy_only {
        return boot(ctx, request, inventory, &root, jobs, first_task, limit).await;
    }
    if !serial {
        jobs = copy_order(inventory, jobs);
    }
    for (index, job) in jobs.iter_mut().enumerate() {
        job.task = TaskId(first_task + index as u64);
    }
    let ssh_opts = std::env::var_os("NIX_SSHOPTS");
    let copies = Copies {
        runner: &ctx.processes,
        events: &ctx.events,
        cancellation: &ctx.cancellation,
        inventory,
        root: &root,
        rollback: request.auto_rollback,
        ssh_opts: ssh_opts.as_deref(),
    };
    let result = async {
        admit(&copies, &jobs, ctx.log_path()).await?;
        if serial {
            serial_copy(&copies, &jobs, request.interactive.then_some(&ctx.ui))
                .await
                .map(|_| ())
        } else {
            bulk_copy(&copies, &jobs, limit).await
        }
    }
    .await;
    if let Err(error) = result {
        let later = ctx.events.emit(UiEvent::Diagnostic { stream: OutputStream::Stderr, style: Style::Failure, text: "Copy stopped; any copied store paths and earlier runtime-kernel state changes are retained. No activation was performed.\n".into() }).await;
        return Err(error.with_secondary(later.err().into_iter().collect()));
    }
    checked(
        &ctx.events,
        &ctx.cancellation,
        diagnostic("Saved-generation copy phase completed; no activation was performed.\n"),
    )
    .await
}

async fn confirm_targets(
    ctx: &Context,
    request: &DeployRequest,
    inventory: &Inventory,
    selected: &crate::domain::Selection,
    columns: &[String],
    selector: &GenerationSelector,
) -> Result<(), ConfctlError> {
    let action = match request.action {
        SwitchAction::Switch => "switch",
        SwitchAction::Boot => "boot",
        SwitchAction::Test => "test",
        SwitchAction::DryActivate => "dry-activate",
    };
    if !ctx
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                diagnostic(if request.copy_only {
                    "The following machines will have saved generations copied:\n"
                } else {
                    "The following machines will receive saved generations for boot:\n"
                }),
                super::machines::table(inventory, selected, columns, true),
                diagnostic(&format!(
                    "Generation: {}\nTarget action: {action}{}{}\n",
                    selector_text(selector),
                    if request.reboot { " + reboot" } else { "" },
                    if request.copy_only {
                        " (copy only)"
                    } else {
                        " (without reboot)"
                    }
                )),
            ],
            assume_yes: request.build.targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    check_cancel(&ctx.cancellation)
}

async fn refresh_selected(
    ctx: &Context,
    root: &ConfigRoot,
    inventory: &Inventory,
    ids: &[MachineId],
    settings: &crate::configuration::RuntimeKernelSettings,
    guard: &crate::configuration::MutationGuard,
) -> Result<u64, ConfctlError> {
    let refresh_ids = ids
        .iter()
        .copied()
        .filter(|id| {
            let m = inventory.get(*id);
            m.carrier.is_none() && m.spin() == Some("vpsadminos")
        })
        .collect::<Vec<_>>();
    let mut first_task = 1;
    if settings.refresh_before_deploy && !refresh_ids.is_empty() {
        let tasks = refresh_ids
            .iter()
            .enumerate()
            .map(|(i, id)| (*id, TaskId(i as u64 + 1)))
            .collect::<Vec<_>>();
        let files = StateFiles::new(root.clone());
        let ssh_config = std::env::var_os("CONFCTL_SSH_CONFIG");
        refresh(
            &FeatureContext {
                runner: &ctx.processes,
                events: &ctx.events,
                files: &files,
                inventory,
                settings,
                cancellation: &ctx.cancellation,
                mutation: guard,
                ssh_config: ssh_config.as_deref(),
            },
            &tasks,
        )
        .await?;
        first_task += tasks.len() as u64;
        checked(&ctx.events, &ctx.cancellation, diagnostic("Runtime-kernel state refreshed before generation lookup; this change is retained if copying later fails.\n")).await?;
    }
    Ok(first_task)
}

async fn confirm_resolved(
    ctx: &Context,
    request: &DeployRequest,
    inventory: &Inventory,
    selector: &GenerationSelector,
    jobs: &[Job],
    missing: &[MachineId],
) -> Result<(), ConfctlError> {
    checked(
        &ctx.events,
        &ctx.cancellation,
        diagnostic("Resolved host generations:\n"),
    )
    .await?;
    checked(
        &ctx.events,
        &ctx.cancellation,
        resolved_table(inventory, jobs, missing)?,
    )
    .await?;
    if request.interactive
        && !ctx
            .ui
            .confirm(ConfirmPrompt {
                always: true,
                ..ConfirmPrompt::default()
            })
            .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    if !missing.is_empty()
        && !ctx
            .ui
            .confirm(ConfirmPrompt {
                preview: vec![diagnostic(&format!(
                    "Generation '{}' was not found on:\n{}These hosts will be ignored.\n",
                    selector_text(selector),
                    missing
                        .iter()
                        .map(|id| format!("  {}\n", inventory.get(*id).name))
                        .collect::<String>()
                ))],
                assume_yes: request.build.targets.yes,
                always: request.interactive,
                ..ConfirmPrompt::default()
            })
            .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    check_cancel(&ctx.cancellation)
}

#[derive(Clone)]
struct Job {
    machine: MachineId,
    generation: BuildGeneration,
    task: TaskId,
}
fn selector_text(selector: &GenerationSelector) -> &str {
    match selector {
        GenerationSelector::Current => "current",
        GenerationSelector::Offset(text) | GenerationSelector::Name(text) => text,
    }
}
fn check_cancel(token: &CancellationToken) -> Result<(), ConfctlError> {
    if token.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}
fn diagnostic(text: &str) -> UiEvent {
    UiEvent::Diagnostic {
        stream: OutputStream::Stdout,
        style: Style::Plain,
        text: text.into(),
    }
}
async fn checked(
    events: &EventSink,
    token: &CancellationToken,
    event: UiEvent,
) -> Result<(), ConfctlError> {
    events.emit(event).await?;
    check_cancel(token)
}
fn description(inventory: &Inventory, job: &Job) -> String {
    format!(
        "copy {} generation {}",
        inventory.get(job.machine).name,
        job.generation.name
    )
}

async fn resolve(
    root: &ConfigRoot,
    inventory: &Inventory,
    ids: &[MachineId],
    selector: &GenerationSelector,
    events: &EventSink,
    token: &CancellationToken,
) -> Result<(Vec<Job>, Vec<MachineId>), ConfctlError> {
    let mut jobs = Vec::new();
    let mut missing = Vec::new();
    for id in ids {
        check_cancel(token)?;
        let name = &inventory.get(*id).name;
        let saved = GenerationInventory::load_local(root, name)?;
        for text in &saved.diagnostics {
            checked(
                events,
                token,
                UiEvent::Diagnostic {
                    stream: OutputStream::Stderr,
                    style: Style::Plain,
                    text: format!("{text}\n"),
                },
            )
            .await?;
        }
        match saved_generation(name, &saved, selector)? {
            Some(generation) => jobs.push(Job {
                machine: *id,
                generation: generation.clone(),
                task: TaskId(0),
            }),
            None => missing.push(*id),
        }
    }
    Ok((jobs, missing))
}
fn saved_generation<'a>(
    host: &str,
    saved: &'a GenerationInventory,
    selector: &GenerationSelector,
) -> Result<Option<&'a BuildGeneration>, ConfctlError> {
    let name = match selector {
        GenerationSelector::Current => {
            saved.require_resolved_current(host)?;
            saved.current.as_deref()
        }
        GenerationSelector::Offset(text) => {
            if !saved.rejected.is_empty() {
                return Err(ConfctlError::State(format!(
                    "Numeric generation selection for {host} is ambiguous because records were excluded; select an explicit supported generation name"
                )));
            }
            let negative = text.starts_with('-');
            let digits = text.trim_start_matches('-').trim_start_matches('0');
            let magnitude = if digits.is_empty() {
                Some(0)
            } else {
                digits.parse::<usize>().ok()
            };
            let index = magnitude.and_then(|n| {
                if n == 0 {
                    saved.generations.len().checked_sub(1)
                } else if negative {
                    saved.generations.len().checked_sub(n)?.checked_sub(1)
                } else {
                    Some(n - 1)
                }
            });
            return Ok(index.and_then(|i| saved.generations.get(i)));
        }
        GenerationSelector::Name(text) => Some(text.as_str()),
    };
    if let Some(name) = name
        && let Some(rejected) = saved.rejected.get(name)
    {
        return Err(ConfctlError::State(format!(
            "Cannot select generation {name} for {host}: {}",
            rejected.reason
        )));
    }
    Ok(name.and_then(|name| saved.generations.iter().find(|g| g.name == name)))
}
fn resolved_table(
    inventory: &Inventory,
    jobs: &[Job],
    missing: &[MachineId],
) -> Result<UiEvent, ConfctlError> {
    let mut roles = Vec::new();
    for job in jobs {
        for role in job.generation.inputs_info.keys() {
            if !roles.contains(role) {
                roles.push(role.clone());
            }
        }
    }
    let mut rows = Vec::new();
    for job in jobs {
        let mut row = vec![
            inventory.get(job.machine).name.clone(),
            job.generation.name.clone(),
            super::generation_list::kernel_version(&job.generation.toplevel)?.unwrap_or_default(),
        ];
        row.extend(roles.iter().map(|role| {
            super::generation_list::input_revision(job.generation.inputs_info.get(role))
        }));
        rows.push(row);
    }
    rows.extend(missing.iter().map(|id| {
        let mut row = vec![inventory.get(*id).name.clone(), "not found".into()];
        row.resize(3 + roles.len(), String::new());
        row
    }));
    rows.sort_by(|a, b| a[..2].cmp(&b[..2]));
    let mut columns = vec!["NAME".into(), "GENERATION".into(), "KERNEL".into()];
    columns.extend(roles.iter().map(|r| r.to_uppercase()));
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
fn copy_order(inventory: &Inventory, jobs: Vec<Job>) -> Vec<Job> {
    let (standalone, carried): (Vec<_>, Vec<_>) = jobs
        .into_iter()
        .partition(|j| inventory.get(j.machine).carrier.is_none());
    let mut groups: Vec<(String, std::collections::VecDeque<Job>)> = Vec::new();
    for job in carried {
        let carrier = inventory.get(job.machine).carrier.as_ref().unwrap();
        if let Some((_, group)) = groups.iter_mut().find(|(name, _)| name == carrier) {
            group.push_back(job);
        } else {
            groups.push((carrier.clone(), [job].into()));
        }
    }
    let mut ordered = standalone;
    while groups.iter().any(|(_, g)| !g.is_empty()) {
        for (_, group) in &mut groups {
            if let Some(job) = group.pop_front() {
                ordered.push(job);
            }
        }
    }
    ordered
}

fn copy_spec(
    inventory: &Inventory,
    machine: &Machine,
    generation: &BuildGeneration,
    rollback: AutoRollback,
    ssh_opts: Option<&OsStr>,
) -> Result<Option<ProcessSpec>, ConfctlError> {
    let paths = copy_paths(machine, generation, rollback)?;
    let carried_null_target = machine.carrier.is_some()
        && machine
            .metadata
            .pointer("/host/target")
            .is_some_and(serde_json::Value::is_null);
    if !carried_null_target && super::connection::target_host(machine)? == "localhost" {
        return Ok(None);
    }
    let target = if let Some(carrier) = &machine.carrier {
        inventory
            .machines
            .iter()
            .find(|m| &m.name == carrier)
            .ok_or_else(|| {
                ConfctlError::Configuration(format!("Carrier {carrier} not found in machine list"))
            })?
    } else {
        machine
    };
    let mut spec = ProcessSpec::new(
        "nix-copy-closure",
        format!("copy generation to {}", machine.name),
    );
    spec.args = vec![
        "--to".into(),
        format!("root@{}", super::connection::target_host(target)?).into(),
    ];
    spec.args.extend(paths);
    let port = super::connection::target_port(target)?;
    if port != "22" {
        let mut value = ssh_opts.unwrap_or(OsStr::new("")).as_bytes().to_vec();
        value.extend_from_slice(format!(" -p {port}").as_bytes());
        let value = value.trim_ascii();
        spec.env.push((
            "NIX_SSHOPTS".into(),
            Some(OsString::from_vec(value.to_vec())),
        ));
    }
    spec.stderr = StreamPolicy::CaptureAndForward;
    Ok(Some(spec))
}
fn copy_paths(
    machine: &Machine,
    generation: &BuildGeneration,
    rollback: AutoRollback,
) -> Result<Vec<OsString>, ConfctlError> {
    let mut paths = vec![generation.toplevel.as_os_str().into()];
    let enabled = match rollback {
        AutoRollback::Disabled => false,
        AutoRollback::Enabled => true,
        AutoRollback::Configured => match machine.metadata.get("autoRollback") {
            None => true,
            Some(serde_json::Value::Object(values)) => !matches!(
                values.get("enable"),
                Some(serde_json::Value::Null | serde_json::Value::Bool(false))
            ),
            _ => {
                return Err(ConfctlError::Configuration(
                    "autoRollback metadata does not support key lookup".into(),
                ));
            }
        },
    };
    if enabled && let Some(path) = &generation.auto_rollback {
        paths.push(path.as_os_str().into());
    }
    Ok(paths)
}
struct Copies<'a, P> {
    runner: &'a P,
    events: &'a EventSink,
    cancellation: &'a CancellationToken,
    inventory: &'a Inventory,
    root: &'a ConfigRoot,
    rollback: AutoRollback,
    ssh_opts: Option<&'a OsStr>,
}
async fn copy_one<P: ProcessRunner>(
    copies: &Copies<'_, P>,
    job: &Job,
    token: &CancellationToken,
) -> Result<(), ConfctlError> {
    let result = async {
        check_cancel(token)?;
        let mut spec = copy_spec(
            copies.inventory,
            copies.inventory.get(job.machine),
            &job.generation,
            copies.rollback,
            copies.ssh_opts,
        )?;
        checked(
            copies.events,
            token,
            UiEvent::TaskStarted {
                task: job.task,
                description: description(copies.inventory, job),
            },
        )
        .await?;
        if let Some(spec) = spec.as_mut() {
            spec.cwd = Some(copies.root.path().into());
        }
        let Some(spec) = spec else {
            return Ok(());
        };
        let mut scope = ProcessScope::new(token.clone(), job.task);
        scope.phase = Some(Phase::Copy);
        scope.nix_progress = Some(crate::nix::progress::Kind::Copy {
            host: copies.inventory.get(job.machine).name.clone(),
        });
        let outcome = copies.runner.run(spec, scope).await?;
        let error = if outcome.termination == TerminationCause::Canceled {
            Some(ConfctlError::Cancelled)
        } else if !outcome.stdout_complete || !outcome.stderr_complete {
            Some(ConfctlError::State("incomplete copy capture".into()))
        } else if !outcome.status.success() {
            Some(ConfctlError::Child {
                description: format!("copy to {}", copies.inventory.get(job.machine).name),
                code: outcome.code(),
                signal: outcome.signal(),
                log: None,
            })
        } else {
            None
        };
        if let Some(error) = error {
            return Err(process_error(error, outcome));
        }
        Ok(())
    }
    .await;
    let state = match &result {
        Ok(()) => TaskState::Succeeded,
        Err(e) if e.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    };
    let finish = copies
        .events
        .emit(UiEvent::TaskFinished {
            task: job.task,
            state,
        })
        .await;
    match (result, finish) {
        (result, Ok(())) => result,
        (Ok(()), Err(e)) => Err(e),
        (Err(e), Err(later)) => Err(e.with_secondary(vec![later])),
    }
    .and_then(|()| check_cancel(token))
}
fn process_error(source: ConfctlError, outcome: ProcessOutcome) -> ConfctlError {
    ConfctlError::ProcessFailure {
        source: Box::new(source),
        evidence: Box::new(PartialProcessEvidence {
            status: Some(outcome.status),
            stdout: outcome.stdout,
            stderr: outcome.stderr,
            stdout_complete: outcome.stdout_complete,
            stderr_complete: outcome.stderr_complete,
            log: None,
        }),
    }
}
fn ordinary(error: &ConfctlError) -> bool {
    matches!(error, ConfctlError::ProcessFailure { source, .. } if matches!(source.as_ref(), ConfctlError::Child { .. }))
}
async fn unentered<P>(copies: &Copies<'_, P>, jobs: &[Job], error: ConfctlError) -> ConfctlError {
    let state = if copies.cancellation.is_cancelled() || error.is_cancelled() {
        TaskState::Canceled
    } else {
        TaskState::Skipped
    };
    let mut later = Vec::new();
    for job in jobs {
        if let Err(e) = copies
            .events
            .emit(UiEvent::TaskFinished {
                task: job.task,
                state,
            })
            .await
        {
            later.push(e);
        }
    }
    error.with_secondary(later)
}
async fn admit<P>(
    copies: &Copies<'_, P>,
    jobs: &[Job],
    log: &std::path::Path,
) -> Result<(), ConfctlError> {
    let mut admitted = 0;
    let result = async {
        for job in jobs {
            copies
                .events
                .emit(UiEvent::TaskPending {
                    task: job.task,
                    description: description(copies.inventory, job),
                })
                .await?;
            admitted += 1;
            check_cancel(copies.cancellation)?;
            checked(
                copies.events,
                copies.cancellation,
                UiEvent::LogAvailable {
                    task: job.task,
                    path: log.into(),
                },
            )
            .await?;
        }
        Ok(())
    }
    .await;
    match result {
        Ok(()) => Ok(()),
        Err(error) => Err(unentered(copies, &jobs[..admitted], error).await),
    }
}
async fn serial_copy<P: ProcessRunner>(
    copies: &Copies<'_, P>,
    jobs: &[Job],
    ui: Option<&crate::terminal::Ui>,
) -> Result<Vec<MachineId>, ConfctlError> {
    let mut copied = Vec::new();
    for (index, job) in jobs.iter().enumerate() {
        let mut finalized = false;
        let result = async {
            check_cancel(copies.cancellation)?;
            if let Some(ui) = ui {
                let accepted = ui
                    .confirm(ConfirmPrompt {
                        preview: vec![diagnostic(&format!(
                            "Copying configuration to {}\n",
                            copies.inventory.get(job.machine).name
                        ))],
                        always: true,
                        ..ConfirmPrompt::default()
                    })
                    .await?;
                check_cancel(copies.cancellation)?;
                if !accepted {
                    finalized = true;
                    return checked(
                        copies.events,
                        copies.cancellation,
                        UiEvent::TaskFinished {
                            task: job.task,
                            state: TaskState::Skipped,
                        },
                    )
                    .await;
                }
            }
            finalized = true;
            copy_one(copies, job, copies.cancellation).await?;
            copied.push(job.machine);
            Ok(())
        }
        .await;
        if let Err(error) = result {
            return Err(unentered(copies, &jobs[index + usize::from(finalized)..], error).await);
        }
    }
    Ok(copied)
}
async fn bulk_copy<P: ProcessRunner>(
    copies: &Copies<'_, P>,
    jobs: &[Job],
    limit: usize,
) -> Result<(), ConfctlError> {
    let token = copies.cancellation.child_token();
    let mut next = 0;
    let mut active = Vec::new();
    let mut failed = Vec::new();
    poll_fn(|cx| {
        while active.len() < limit && next < jobs.len() && !token.is_cancelled() {
            active.push((next, Box::pin(copy_one(copies, &jobs[next], &token))));
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
                        failed.push((index, error));
                    }
                }
            }
        }
        if active.is_empty() && (next == jobs.len() || token.is_cancelled()) {
            Poll::Ready(())
        } else {
            if active.len() < limit && next < jobs.len() {
                cx.waker().wake_by_ref();
            }
            Poll::Pending
        }
    })
    .await;
    if !failed.is_empty() {
        let (index, error) = failed.remove(0);
        let error = if ordinary(&error) && !token.is_cancelled() {
            ConfctlError::State(format!(
                "Copy failed to: {}",
                std::iter::once(index)
                    .chain(failed.iter().map(|(i, _)| *i))
                    .map(|i| copies.inventory.get(jobs[i].machine).name.clone())
                    .collect::<Vec<_>>()
                    .join(", ")
            ))
            .with_secondary(vec![error])
        } else {
            error
        };
        return Err(unentered(
            copies,
            &jobs[next..],
            error.with_secondary(failed.into_iter().map(|(_, e)| e).collect()),
        )
        .await);
    }
    if let Err(error) = check_cancel(copies.cancellation) {
        return Err(unentered(copies, &jobs[next..], error).await);
    }
    Ok(())
}

async fn boot(
    ctx: &Context,
    request: &DeployRequest,
    inventory: &Inventory,
    root: &ConfigRoot,
    mut jobs: Vec<Job>,
    first_task: u64,
    limit: usize,
) -> Result<(), ConfctlError> {
    let ssh_opts = std::env::var_os("NIX_SSHOPTS");
    let ssh_config = std::env::var_os("CONFCTL_SSH_CONFIG");
    let copies = Copies {
        runner: &ctx.processes,
        events: &ctx.events,
        cancellation: &ctx.cancellation,
        inventory,
        root,
        rollback: request.auto_rollback,
        ssh_opts: ssh_opts.as_deref(),
    };
    for (index, job) in jobs.iter_mut().enumerate() {
        job.task = TaskId(first_task + index as u64);
    }
    let statuses = boot_statuses(&copies, &jobs, ssh_config.as_deref()).await?;
    let first_copy = first_task + jobs.len() as u64;
    let mut remaining = Vec::new();
    for (job, status) in jobs.into_iter().zip(&statuses) {
        if already_booted(inventory.get(job.machine), &job.generation, status) {
            checked(
                &ctx.events,
                &ctx.cancellation,
                diagnostic(&format!(
                    "Skipping {}: already using target generation {}\n",
                    inventory.get(job.machine).name,
                    job.generation.name
                )),
            )
            .await?;
        } else {
            remaining.push(job);
        }
    }
    for (index, job) in remaining.iter_mut().enumerate() {
        job.task = TaskId(first_copy + index as u64);
    }
    let activation = Activation {
        copies: &copies,
        ui: &ctx.ui,
        ssh_config: ssh_config.as_deref(),
        interactive: request.interactive,
        dry_first: request.dry_activate_first,
        log: ctx.log_path(),
    };
    let result = async {
        admit(&copies, &remaining, ctx.log_path()).await?;
        let first_activation = first_copy + remaining.len() as u64;
        if request.one_by_one {
            for (index, job) in remaining.iter().enumerate() {
                let result = async {
                    let copied = serial_copy(&copies, std::slice::from_ref(job),
                        request.interactive.then_some(&ctx.ui)).await?;
                    if !copied.is_empty() {
                        activate_one(&activation, job, TaskId(first_activation + index as u64)).await?;
                    }
                    Ok(())
                }.await;
                if let Err(error) = result {
                    return Err(unentered(&copies, &remaining[index + 1..], error).await);
                }
            }
        } else {
            // Interactive bulk remains a complete copy phase before activation.
            let copied = if request.interactive {
                serial_copy(&copies, &remaining, Some(&ctx.ui)).await?
            } else {
                let ordered = copy_order(inventory, remaining.clone());
                bulk_copy(&copies, &ordered, limit).await?;
                remaining.iter().map(|job| job.machine).collect()
            };
            for (index, job) in remaining.iter().enumerate() {
                if copied.contains(&job.machine) {
                    activate_one(&activation, job, TaskId(first_activation + index as u64)).await?;
                }
            }
        }
        checked(&ctx.events, &ctx.cancellation, diagnostic(
            "Saved-generation boot preparation completed; carried hosts were copied only. No reboot or health check was performed.\n"
        )).await
    }.await;
    if let Err(error) = result {
        let later = ctx.events.emit(UiEvent::Diagnostic {
            stream: OutputStream::Stderr, style: Style::Failure,
            text: "Boot preparation stopped; earlier copies, runtime-kernel state, bootloader changes and profile updates are retained. Inspect target state before retrying; no rollback or reboot was performed.\n".into(),
        }).await;
        return Err(error.with_secondary(later.err().into_iter().collect()));
    }
    Ok(())
}

fn already_booted(
    machine: &Machine,
    generation: &BuildGeneration,
    status: &super::host_status::HostStatus,
) -> bool {
    if status
        .current_toplevel
        .as_ref()
        .map(|path| path.as_os_str())
        != Some(generation.toplevel.as_os_str())
    {
        return false;
    }
    // Ruby uses the carrier profile observation alone, even after a later read failure.
    machine.carrier.is_some()
        || status.generations.as_ref().is_some_and(|records| {
            records.iter().any(|record| {
                record.current && record.toplevel.as_os_str() == generation.toplevel.as_os_str()
            })
        })
}

async fn boot_statuses<P: ProcessRunner>(
    copies: &Copies<'_, P>,
    jobs: &[Job],
    ssh_config: Option<&OsStr>,
) -> Result<Vec<super::host_status::HostStatus>, ConfctlError> {
    let token = copies.cancellation.child_token();
    let mut next = 0;
    let mut active = Vec::new();
    let mut statuses: Vec<_> = std::iter::repeat_with(|| None).take(jobs.len()).collect();
    let mut errors = Vec::new();
    poll_fn(|cx| {
        while active.len() < 5 && next < jobs.len() && !token.is_cancelled() {
            active.push((
                next,
                Box::pin(boot_status(copies, &jobs[next], &token, ssh_config)),
            ));
            next += 1;
        }
        let mut i = 0;
        while i < active.len() {
            match active[i].1.as_mut().poll(cx) {
                Poll::Pending => i += 1,
                Poll::Ready(result) => {
                    let (index, _) = active.remove(i);
                    match result {
                        Ok(status) => statuses[index] = Some(status),
                        Err(error) => {
                            errors.push(error);
                            token.cancel();
                        }
                    }
                }
            }
        }
        if active.is_empty() && (next == jobs.len() || token.is_cancelled()) {
            Poll::Ready(())
        } else {
            if active.len() < 5 && next < jobs.len() {
                cx.waker().wake_by_ref();
            }
            Poll::Pending
        }
    })
    .await;
    if !errors.is_empty() {
        return Err(errors.remove(0).with_secondary(errors));
    }
    check_cancel(copies.cancellation)?;
    Ok(statuses
        .into_iter()
        .map(|status| status.expect("joined boot status"))
        .collect())
}

async fn boot_status<P: ProcessRunner>(
    copies: &Copies<'_, P>,
    job: &Job,
    token: &CancellationToken,
    ssh_config: Option<&OsStr>,
) -> Result<super::host_status::HostStatus, ConfctlError> {
    let host = &copies.inventory.get(job.machine).name;
    let description = format!("query boot state on {host}");
    copies
        .events
        .emit(UiEvent::TaskPending {
            task: job.task,
            description: description.clone(),
        })
        .await?;
    let result: Result<super::host_status::HostStatus, ConfctlError> = async {
        check_cancel(token)?;
        checked(
            copies.events,
            token,
            UiEvent::TaskStarted {
                task: job.task,
                description,
            },
        )
        .await?;
        let status = super::host_status::query(
            copies.inventory,
            copies.inventory.get(job.machine),
            copies.runner,
            token,
            job.task,
            ssh_config,
            true,
        )
        .await?;
        for message in &status.diagnostics {
            checked(
                copies.events,
                token,
                UiEvent::Diagnostic {
                    stream: OutputStream::Stderr,
                    style: Style::Plain,
                    text: format!("{host}: {message}\n"),
                },
            )
            .await?;
        }
        if let Some(failure) = &status.failure {
            checked(
                copies.events,
                token,
                UiEvent::Diagnostic {
                    stream: OutputStream::Stderr,
                    style: Style::Failure,
                    text: format!("{}\n", failure.diagnostic(host)),
                },
            )
            .await?;
        }
        Ok(status)
    }
    .await;
    let state = match &result {
        Ok(status) if status.failure.is_none() => TaskState::Succeeded,
        Ok(_) => TaskState::Failed,
        Err(error) if error.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    };
    let delivery = copies
        .events
        .emit(UiEvent::TaskFinished {
            task: job.task,
            state,
        })
        .await;
    match (result, delivery) {
        (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
        (_, Err(error)) | (Err(error), _) => Err(error),
        (Ok(status), Ok(())) => {
            check_cancel(token)?;
            Ok(status)
        }
    }
}

struct Activation<'a, P> {
    copies: &'a Copies<'a, P>,
    ui: &'a crate::terminal::Ui,
    ssh_config: Option<&'a OsStr>,
    interactive: bool,
    dry_first: bool,
    log: &'a std::path::Path,
}

#[derive(Debug, Eq, PartialEq)]
enum BootChoice {
    Continue,
    Retry,
    Skip,
    Abort,
}
fn boot_choice(line: &[u8], dry_first: bool) -> Option<BootChoice> {
    match line.trim_ascii().to_ascii_lowercase().as_slice() {
        b"y" => Some(BootChoice::Continue),
        b"r" if dry_first => Some(BootChoice::Retry),
        b"s" => Some(BootChoice::Skip),
        b"a" => Some(BootChoice::Abort),
        _ => None,
    }
}
async fn choose_boot<P>(activation: &Activation<'_, P>) -> Result<BootChoice, ConfctlError> {
    loop {
        let prompt = if activation.dry_first {
            "[y] Continue, [r] Retry dry activation, [s] Skip, [a] Abort: "
        } else {
            "[y] Continue, [s] Skip, [a] Abort: "
        };
        let line = activation.ui.line(prompt.into()).await?;
        check_cancel(activation.copies.cancellation)?;
        if let Some(choice) = boot_choice(&line, activation.dry_first) {
            checked(
                activation.copies.events,
                activation.copies.cancellation,
                diagnostic("\n"),
            )
            .await?;
            return Ok(choice);
        }
    }
}

async fn activate_one<P: ProcessRunner>(
    activation: &Activation<'_, P>,
    job: &Job,
    task: TaskId,
) -> Result<(), ConfctlError> {
    let copies = activation.copies;
    let host = &copies.inventory.get(job.machine).name;
    if copies.inventory.get(job.machine).carrier.is_some() {
        return checked(
            copies.events,
            copies.cancellation,
            diagnostic(&format!(
                "{host}: carried boot copies store paths only; no activation or profile change.\n"
            )),
        )
        .await;
    }
    let description = format!("prepare boot on {host}");
    copies
        .events
        .emit(UiEvent::TaskPending {
            task,
            description: description.clone(),
        })
        .await?;
    let result = async {
        checked(
            copies.events,
            copies.cancellation,
            UiEvent::LogAvailable {
                task,
                path: activation.log.into(),
            },
        )
        .await?;
        let mut entered = false;
        loop {
            if activation.dry_first {
                boot_command(
                    activation,
                    job,
                    task,
                    &description,
                    &mut entered,
                    "dry-activate",
                )
                .await?;
            }
            if activation.interactive {
                checked(
                    copies.events,
                    copies.cancellation,
                    diagnostic(&format!(
                        "Prepare generation {} for boot on {host}\n",
                        job.generation.name
                    )),
                )
                .await?;
                match choose_boot(activation).await? {
                    BootChoice::Retry => continue,
                    BootChoice::Skip => return Ok(TaskState::Skipped),
                    BootChoice::Abort => {
                        return Err(ConfctlError::State("Aborting boot preparation".into()));
                    }
                    BootChoice::Continue => {}
                }
            }
            boot_command(activation, job, task, &description, &mut entered, "boot").await?;
            boot_command(
                activation,
                job,
                task,
                &description,
                &mut entered,
                "set profile after boot",
            )
            .await?;
            return Ok(TaskState::Succeeded);
        }
    }
    .await;
    let state = match &result {
        Ok(state) => *state,
        Err(error) if error.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    };
    let delivery = copies
        .events
        .emit(UiEvent::TaskFinished { task, state })
        .await;
    match (result, delivery) {
        (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
        (_, Err(error)) | (Err(error), _) => Err(error),
        (Ok(_), Ok(())) => check_cancel(copies.cancellation),
    }
}

async fn boot_command<P: ProcessRunner>(
    activation: &Activation<'_, P>,
    job: &Job,
    task: TaskId,
    description: &str,
    entered: &mut bool,
    phase: &str,
) -> Result<(), ConfctlError> {
    let copies = activation.copies;
    let machine = copies.inventory.get(job.machine);
    check_cancel(copies.cancellation)?;
    if !*entered {
        checked(
            copies.events,
            copies.cancellation,
            UiEvent::TaskStarted {
                task,
                description: description.into(),
            },
        )
        .await?;
        *entered = true;
    }
    checked(
        copies.events,
        copies.cancellation,
        diagnostic(&format!("{}: {phase}\n", machine.name)),
    )
    .await?;
    let argv = if phase == "set profile after boot" {
        vec![
            "nix-env".into(),
            "-p".into(),
            "/nix/var/nix/profiles/system".into(),
            "--set".into(),
            job.generation.toplevel.as_os_str().into(),
        ]
    } else {
        vec![
            job.generation
                .toplevel
                .join("bin/switch-to-configuration")
                .into_os_string(),
            phase.into(),
        ]
    };
    let mut spec = super::connection::command_spec(machine, activation.ssh_config, &argv)?;
    spec.redacted_description = format!("{phase} on {}", machine.name);
    spec.cwd = Some(copies.root.path().into());
    spec.stdout = StreamPolicy::CaptureAndForward;
    spec.stderr = StreamPolicy::CaptureAndForward;
    let mut scope = ProcessScope::new(copies.cancellation.clone(), task);
    scope.phase = Some(Phase::Deployment);
    let outcome = copies.runner.run(spec, scope).await?;
    let failure = if outcome.termination == TerminationCause::Canceled {
        Some(ConfctlError::Cancelled)
    } else if !outcome.stdout_complete || !outcome.stderr_complete {
        Some(ConfctlError::State(format!(
            "incomplete {phase} capture on {}",
            machine.name
        )))
    } else if !outcome.status.success() {
        Some(ConfctlError::Child {
            description: format!("{phase} on {}", machine.name),
            code: outcome.code(),
            signal: outcome.signal(),
            log: None,
        })
    } else {
        None
    };
    if let Some(error) = failure {
        return Err(process_error(error, outcome));
    }
    check_cancel(copies.cancellation)
}

#[cfg(test)]
mod tests;
