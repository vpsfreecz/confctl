//! Build or select generations, copy, activate and optionally reboot/wait.
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

mod health;
mod reboot;
mod rollback;

pub(super) async fn run(
    request: &DeployRequest,
    root: ConfigRoot,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    let selector = request.generation.as_ref();
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
    let limit = copy_limit(request)?;
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
    let mut first_task = refresh_selected(
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
    let mut jobs = if let Some(selector) = selector {
        let (jobs, missing) = resolve(
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
        jobs
    } else {
        let build_nix = NixClient::configured(
            root.clone(),
            ctx.processes.clone(),
            NixOptions {
                show_trace: request.build.show_trace,
                max_jobs: request.build.nix.max_jobs.clone(),
                cores: request.build.nix.cores.clone(),
            },
            ctx.cancellation.clone(),
            &defaults,
            nix.epoch(),
        )
        .await?;
        super::build::build_selected(
            ctx,
            &build_nix,
            inventory,
            &selected.machines,
            &guard,
            &mut first_task,
        )
        .await?
        .into_iter()
        .map(|(machine, generation)| Job {
            machine,
            generation,
            task: TaskId(0),
        })
        .collect()
    };
    check_cancel(&ctx.cancellation)?;
    if !request.copy_only {
        return activate_generations(
            ctx,
            request,
            inventory,
            &root,
            jobs,
            &selected.machines,
            first_task,
        )
        .await;
    }
    if !serial {
        jobs = copy_order(inventory, selected_order(&jobs, &selected.machines));
    }
    task_end(first_task, jobs.len())?;
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
    let mut finished = Vec::new();
    let result = async {
        admit(&copies, &jobs, ctx.log_path()).await?;
        if serial {
            serial_copy(
                &copies,
                &jobs,
                request.interactive.then_some(&ctx.ui),
                &mut finished,
            )
            .await
            .map(|_| ())
        } else {
            bulk_copy(&copies, &jobs, limit, &mut finished).await
        }
    }
    .await;
    let mut deliveries = Vec::new();
    for job in &jobs {
        if let Some((_, state)) = finished.iter().find(|(id, _)| *id == job.machine)
            && let Err(error) = ctx
                .events
                .emit(UiEvent::Checkpoint {
                    key: job.task,
                    label: format!(
                        "{}: {}",
                        inventory.get(job.machine).name,
                        match state {
                            TaskState::Succeeded => "copied only; no activation",
                            TaskState::Skipped => "copy skipped",
                            _ => "copy stopped; partial effects retained",
                        }
                    ),
                    state: *state,
                    tasks: vec![job.task],
                })
                .await
        {
            deliveries.push(error);
        }
    }
    let result = combine_deliveries(result, deliveries);
    if let Err(error) = result {
        let later = ctx.events.emit(UiEvent::Diagnostic { stream: OutputStream::Stderr, style: Style::Failure, text: "Copy stopped; any local build publications, copied store paths and earlier runtime-kernel state changes are retained. No activation was performed.\n".into() }).await;
        return Err(error.with_secondary(later.err().into_iter().collect()));
    }
    checked(
        &ctx.events,
        &ctx.cancellation,
        diagnostic(&format!(
            "{} copy phase completed; no activation was performed.\n",
            deployment_name(request)
        )),
    )
    .await
}

async fn confirm_targets(
    ctx: &Context,
    request: &DeployRequest,
    inventory: &Inventory,
    selected: &crate::domain::Selection,
    columns: &[String],
    selector: Option<&GenerationSelector>,
) -> Result<(), ConfctlError> {
    let action = action_name(request.action);
    if !ctx
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                diagnostic_style(Style::Heading, match (selector.is_some(), request.copy_only) {
                    (true, true) => "The following machines will have saved generations copied:\n",
                    (true, false) => {
                        "The following machines will receive saved generations for activation:\n"
                    }
                    (false, true) => "The following machines will be BUILT and copied:\n",
                    (false, false) => "The following machines will be BUILT and deployed:\n",
                }),
                super::machines::table(inventory, selected, columns, true),
                diagnostic(&format!(
                    "Generation: {}\nTarget action: {action}{}{}\n",
                    selector.map(selector_text).unwrap_or("new build"),
                    if request.reboot { " + reboot" } else { "" },
                    if request.copy_only {
                        " (copy only)"
                    } else if request.reboot {
                        ""
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
        diagnostic_style(Style::Heading, "Resolved host generations:\n"),
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
fn action_name(action: SwitchAction) -> &'static str {
    match action {
        SwitchAction::Switch => "switch",
        SwitchAction::Boot => "boot",
        SwitchAction::Test => "test",
        SwitchAction::DryActivate => "dry-activate",
    }
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
    diagnostic_style(Style::Plain, text)
}
fn diagnostic_style(style: Style, text: &str) -> UiEvent {
    UiEvent::Diagnostic {
        stream: OutputStream::Stdout,
        style,
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
fn result_state<T>(result: &Result<T, ConfctlError>) -> TaskState {
    match result {
        Ok(_) => TaskState::Succeeded,
        Err(error) if error.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    }
}
fn combine_deliveries<T>(
    result: Result<T, ConfctlError>,
    mut errors: Vec<ConfctlError>,
) -> Result<T, ConfctlError> {
    if errors.is_empty() {
        return result;
    }
    match result {
        Err(error) => Err(error.with_secondary(errors)),
        Ok(_) => Err(errors.remove(0).with_secondary(errors)),
    }
}

// One machine's owning route only. These facts do not infer completion from
// presentation events, and never change execution order or retry policy.
struct MachineFinish {
    tasks: Vec<TaskId>,
    state: TaskState,
    qualification: String,
    reboot_unverified: bool,
}
impl MachineFinish {
    fn copied(task: TaskId, state: TaskState) -> Self {
        Self {
            tasks: vec![task],
            reboot_unverified: false,
            state: if state == TaskState::Succeeded {
                TaskState::Incomplete
            } else {
                state
            },
            qualification: if state == TaskState::Skipped {
                "copy skipped"
            } else {
                "copy completed or stopped; activation unverified"
            }
            .into(),
        }
    }
}
async fn machine_checkpoint(
    ctx: &Context,
    job: &Job,
    inventory: &Inventory,
    finish: &MachineFinish,
) -> Result<(), ConfctlError> {
    ctx.events
        .emit(UiEvent::Checkpoint {
            key: job.task,
            label: format!(
                "{}: {}",
                inventory.get(job.machine).name,
                finish.qualification
            ),
            state: finish.state,
            tasks: finish.tasks.clone(),
        })
        .await
}
fn apply_health(finish: &mut MachineFinish, detail: &health::HostResult) {
    finish.tasks.extend(&detail.tasks);
    if finish.state != TaskState::Failed && finish.state != TaskState::Canceled {
        finish.state = if finish.reboot_unverified && detail.state() == TaskState::Succeeded {
            TaskState::Incomplete
        } else {
            detail.state()
        };
    }
    finish
        .qualification
        .push_str(&format!("; {}", detail.label()));
}
fn reboot_result(
    finish: &mut MachineFinish,
    completion: reboot::Completion,
) -> Result<reboot::Outcome, ConfctlError> {
    finish.tasks.extend(completion.terminal_task);
    completion.outcome
}
fn apply_reboot(finish: &mut MachineFinish, outcome: &reboot::Outcome) -> bool {
    match outcome {
        reboot::Outcome::Skipped => {
            finish.reboot_unverified = true;
            finish.state = TaskState::Incomplete;
            finish
                .qualification
                .push_str("; reboot skipped, unverified");
            false
        }
        reboot::Outcome::NoWait {
            native_success: false,
        } => {
            finish.state = TaskState::Failed;
            finish
                .qualification
                .push_str("; reboot request failed, remote effect unknown");
            true
        }
        reboot::Outcome::NoWait {
            native_success: true,
        } => {
            finish.reboot_unverified = true;
            finish.state = TaskState::Incomplete;
            finish
                .qualification
                .push_str("; reboot requested without online verification");
            true
        }
        reboot::Outcome::Online => {
            finish
                .qualification
                .push_str("; lower uptime observed, running generation unverified");
            true
        }
    }
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
    saved.select_saved(host, selector)
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
    if rollback_enabled(machine, rollback)?
        && let Some(path) = &generation.auto_rollback
    {
        paths.push(path.as_os_str().into());
    }
    Ok(paths)
}
fn rollback_enabled(machine: &Machine, rollback: AutoRollback) -> Result<bool, ConfctlError> {
    Ok(match rollback {
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
    })
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
) -> (TaskState, Result<(), ConfctlError>) {
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
    let result = match (result, finish) {
        (result, Ok(())) => result,
        (Ok(()), Err(e)) => Err(e),
        (Err(e), Err(later)) => Err(e.with_secondary(vec![later])),
    }
    .and_then(|()| check_cancel(token));
    (state, result)
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
    finished: &mut Vec<(MachineId, TaskState)>,
) -> Result<Vec<MachineId>, ConfctlError> {
    let mut copied = Vec::new();
    for (index, job) in jobs.iter().enumerate() {
        let mut finalized = false;
        let result = async {
            check_cancel(copies.cancellation)?;
            if let Some(ui) = ui {
                let accepted = ui
                    .confirm(ConfirmPrompt {
                        preview: vec![diagnostic_style(
                            Style::Machine,
                            &format!(
                                "Copying configuration to {}\n",
                                copies.inventory.get(job.machine).name
                            ),
                        )],
                        always: true,
                        ..ConfirmPrompt::default()
                    })
                    .await?;
                check_cancel(copies.cancellation)?;
                if !accepted {
                    finalized = true;
                    finished.push((job.machine, TaskState::Skipped));
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
            let (state, result) = copy_one(copies, job, copies.cancellation).await;
            finished.push((job.machine, state));
            result?;
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
    finished: &mut Vec<(MachineId, TaskState)>,
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
                Poll::Ready((state, result)) => {
                    let (index, _) = active.remove(i);
                    finished.push((jobs[index].machine, state));
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

async fn activate_generations(
    ctx: &Context,
    request: &DeployRequest,
    inventory: &Inventory,
    root: &ConfigRoot,
    mut jobs: Vec<Job>,
    confirmed: &[MachineId],
    first_task: u64,
) -> Result<(), ConfctlError> {
    let limit = copy_limit(request)?;
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
    let first_copy = task_end(first_task, jobs.len())?;
    for (index, job) in jobs.iter_mut().enumerate() {
        job.task = TaskId(first_task + index as u64);
    }
    let statuses = target_statuses(&copies, &jobs, ssh_config.as_deref(), request.action).await?;
    let status_tasks = jobs
        .iter()
        .map(|job| (job.machine, job.task))
        .collect::<Vec<_>>();
    let mut remaining = Vec::new();
    for (job, status) in jobs.into_iter().zip(&statuses) {
        if already_current(
            inventory.get(job.machine),
            &job.generation,
            status,
            request.action,
        ) {
            checked(
                &ctx.events,
                &ctx.cancellation,
                diagnostic_style(
                    Style::Incomplete,
                    &format!(
                        "Skipping {}: already using target generation {}\n",
                        inventory.get(job.machine).name,
                        job.generation.name
                    ),
                ),
            )
            .await?;
            ctx.events
                .emit(UiEvent::Checkpoint {
                    key: job.task,
                    label: format!(
                        "{}: already current; skipped",
                        inventory.get(job.machine).name
                    ),
                    state: TaskState::Skipped,
                    tasks: vec![job.task],
                })
                .await?;
        } else {
            remaining.push(job);
        }
    }
    let first_activation = task_end(first_copy, remaining.len())?;
    let first_reboot = task_end(first_activation, remaining.len())?;
    let mut next_health_id = if request.reboot {
        task_end(first_reboot, remaining.len())?
    } else {
        first_reboot
    };
    for (index, job) in remaining.iter_mut().enumerate() {
        job.task = TaskId(first_copy + index as u64);
    }
    let activation = Activation {
        copies: &copies,
        ui: &ctx.ui,
        ssh_config: ssh_config.as_deref(),
        interactive: request.interactive,
        dry_first: request.dry_activate_first,
        action: request.action,
        log: ctx.log_path(),
    };
    let mut finishes: Vec<Option<MachineFinish>> = (0..remaining.len()).map(|_| None).collect();
    let mut snapshotted = vec![false; remaining.len()];
    let result = async {
        admit(&copies, &remaining, ctx.log_path()).await?;
        let mut completed = Vec::new();
        let mut health_outcomes = Vec::new();
        if request.one_by_one {
            for (index, job) in remaining.iter().enumerate() {
                let mut copy_results = Vec::new();
                let result: Result<(), ConfctlError> = async {
                    let copy = serial_copy(&copies, std::slice::from_ref(job), request.interactive.then_some(&ctx.ui), &mut copy_results).await;
                    if let Some((_, state)) = copy_results.first() {
                        let mut finish = MachineFinish::copied(job.task, *state);
                        if let Some((_, task)) = status_tasks.iter().find(|(id, _)| *id == job.machine) { finish.tasks.insert(0, *task); }
                        finishes[index] = Some(finish);
                    }
                    let copied = copy?;
                    if !copied.is_empty() {
                        let finish = finishes[index].as_mut().expect("completed copy");
                        if inventory.get(job.machine).carrier.is_none() || request.action == SwitchAction::Switch {
                            finish.tasks.push(TaskId(first_activation + index as u64));
                        }
                        if activate_one(&activation, job, TaskId(first_activation + index as u64)).await? {
                            completed.push(job);
                            finish.state = TaskState::Succeeded;
                            finish.qualification = format!("{} action completed; helper protocol does not witness hidden switch/rollback outcome", action_name(request.action));
                            let health_ready = if request.reboot {
                                let completion = reboot::run(ctx, inventory.get(job.machine), ssh_config.as_deref(), TaskId(first_reboot + index as u64), &request.wait_online, request.interactive).await;
                                let outcome = reboot_result(finish, completion)?;
                                apply_reboot(finish, &outcome)
                            } else { true };
                            if health_ready && needs_health(request) {
                                let health = health::run(ctx, request, inventory, root, &[job.machine], &mut next_health_id).await;
                                for detail in &health.hosts { apply_health(finish, detail); }
                                health_outcomes.push(health.outcome?);
                            }
                        } else if inventory.get(job.machine).carrier.is_some() && request.action != SwitchAction::Switch {
                            finish.state = TaskState::Succeeded;
                            finish.qualification = "carried target copied only; no activation".into();
                        } else {
                            finish.state = TaskState::Skipped;
                            finish.qualification = "activation skipped; copy retained".into();
                        }
                    }
                    Ok(())
                }.await;
                if let Err(error) = &result
                    && let Some(finish) = finishes[index].as_mut()
                {
                    finish.state = if error.is_cancelled() { TaskState::Canceled } else { TaskState::Failed };
                    finish.qualification.push_str("; stopped, earlier effects retained");
                }
                let delivery = if let Some(finish) = &finishes[index] {
                    snapshotted[index] = true;
                    machine_checkpoint(ctx, job, inventory, finish).await.err().into_iter().collect()
                } else { Vec::new() };
                if let Err(error) = combine_deliveries(result, delivery) {
                    return Err(unentered(&copies, &remaining[index + 1..], error).await);
                }
                check_cancel(&ctx.cancellation)?;
            }
        } else {
            // Preserve the complete bulk copy phase before any activation.
            let mut copy_results = Vec::new();
            let copy = if request.interactive {
                serial_copy(&copies, &remaining, Some(&ctx.ui), &mut copy_results).await
            } else {
                let ordered = copy_order(inventory, selected_order(&remaining, confirmed));
                bulk_copy(&copies, &ordered, limit, &mut copy_results).await.map(|()| remaining.iter().map(|job| job.machine).collect())
            };
            for (index, job) in remaining.iter().enumerate() {
                if let Some((_, state)) = copy_results.iter().find(|(id, _)| *id == job.machine) {
                    let mut finish = MachineFinish::copied(job.task, *state);
                        if let Some((_, task)) = status_tasks.iter().find(|(id, _)| *id == job.machine) { finish.tasks.insert(0, *task); }
                        finishes[index] = Some(finish);
                }
            }
            let copied = copy?;
            for (index, job) in remaining.iter().enumerate() {
                if copied.contains(&job.machine) {
                    let finish = finishes[index].as_mut().expect("completed copy");
                    if inventory.get(job.machine).carrier.is_none() || request.action == SwitchAction::Switch { finish.tasks.push(TaskId(first_activation + index as u64)); }
                    let activation_result = activate_one(&activation, job, TaskId(first_activation + index as u64)).await;
                    if let Err(error) = &activation_result {
                        finish.state = result_state(&activation_result);
                        finish.qualification.push_str("; activation stopped, earlier effects retained");
                        if error.is_cancelled() { finish.state = TaskState::Canceled; }
                    }
                    if activation_result? {
                        completed.push(job);
                        finish.state = TaskState::Succeeded;
                        finish.qualification = format!("{} action completed; helper protocol does not witness hidden switch/rollback outcome", action_name(request.action));
                        // Other machines may still stop this bulk route before health.
                        if needs_health(request) { finish.state = TaskState::Incomplete; }
                    } else if inventory.get(job.machine).carrier.is_some() && request.action != SwitchAction::Switch {
                        finish.state = TaskState::Succeeded;
                        finish.qualification = "carried target copied only; no activation".into();
                    } else { finish.state = TaskState::Skipped; finish.qualification = "activation skipped; copy retained".into(); }
                }
            }
            let mut health_hosts = Vec::new();
            for (index, job) in remaining.iter().enumerate() {
                if completed.iter().any(|done| done.machine == job.machine) {
                    let finish = finishes[index].as_mut().expect("activated copy");
                    let ready = if request.reboot {
                        let completion = reboot::run(ctx, inventory.get(job.machine), ssh_config.as_deref(), TaskId(first_reboot + index as u64), &request.wait_online, request.interactive).await;
                        match reboot_result(finish, completion) {
                            Ok(outcome) => apply_reboot(finish, &outcome),
                            Err(error) => { finish.state = if error.is_cancelled() { TaskState::Canceled } else { TaskState::Failed }; finish.qualification.push_str("; reboot stopped, effect unknown"); return Err(error); }
                        }
                    } else { true };
                    let _ = ready;
                    health_hosts.push(job.machine);
                }
            }
            if needs_health(request) {
                // Retry-all can rerun a passed host; snapshot only after the final decision.
                let hosts = confirmed.iter().copied().filter(|id| health_hosts.contains(id)).collect::<Vec<_>>();
                let health = health::run(ctx, request, inventory, root, &hosts, &mut next_health_id).await;
                for (index, job) in remaining.iter().enumerate() {
                    if let (Some(finish), Some(detail)) = (finishes[index].as_mut(), health.hosts.iter().find(|detail| detail.machine == job.machine)) { apply_health(finish, detail); }
                }
                health_outcomes.push(health.outcome?);
            }
        }
        let summary = if request.reboot {
            format!("{} boot actions completed; eligible reboot/wait work completed or was explicitly skipped. {} Carried hosts were copied only; localhost was not rebooted. Native reboot failures and lower-uptime observations do not prove command effect or the running generation.\n", deployment_name(request), health::summary(&health_outcomes))
        } else if matches!(request.action, SwitchAction::Test | SwitchAction::Switch) {
            let mut helpers = 0;
            let mut carried = 0;
            for job in &completed {
                let machine = inventory.get(job.machine);
                if machine.carrier.is_some() { carried += 1; }
                else if rollback_enabled(machine, request.auto_rollback)? && job.generation.auto_rollback.is_some() { helpers += 1; }
            }
            let effects = if request.action == SwitchAction::Switch {
                format!("{carried} carried profile updates including their installed callbacks. Standalone profiles were set after successful actions; no reboot.")
            } else {
                "Carried hosts were copied only; no profile setting or reboot.".into()
            };
            format!("{} {} completed: {} direct {} actions, {helpers} confirmed rollback-helper protocols. {} {effects} Helper protocols and health do not witness hidden switch or rollback status.\n", deployment_name(request), action_name(request.action), completed.len() - helpers - carried, action_name(request.action), health::summary(&health_outcomes))
        } else {
            format!("{} {} completed; carried hosts were copied only. No reboot or health check was performed.\n", deployment_name(request), action_name(request.action))
        };
        checked(&ctx.events, &ctx.cancellation, diagnostic(&summary)).await
    }.await;
    let mut deliveries = Vec::new();
    for (index, job) in remaining.iter().enumerate() {
        if !snapshotted[index]
            && let Some(finish) = &finishes[index]
            && let Err(error) = machine_checkpoint(ctx, job, inventory, finish).await
        {
            deliveries.push(error);
        }
    }
    let result = combine_deliveries(result, deliveries);
    if let Err(error) = result {
        let later = ctx.events.emit(UiEvent::Diagnostic {
            stream: OutputStream::Stderr, style: Style::Failure,
            text: if request.reboot {
                format!("{} boot/reboot stopped; earlier local publications, refreshes, copies, bootloader/profile changes, reboot requests and health-program effects are retained. Inspect target state and connectivity before an explicit rerun; no automatic second reboot, compensation or remote cancellation was requested.\n", deployment_name(request))
            } else if matches!(request.action, SwitchAction::Test | SwitchAction::Switch) {
                format!("{} {} stopped; earlier local publications, refreshes, copies, script{} and health-program effects are retained. Remote watchdog state and hidden switch/rollback status may be unknown. Inspect the target before an explicit rerun; no compensating activation, confirmation or profile setting was requested.\n", deployment_name(request), action_name(request.action), if request.action == SwitchAction::Switch { ", profile, carrier callback" } else { "" })
            } else { format!("{} {} stopped; earlier local publications, copies, runtime-kernel state and script effects{} are retained. Inspect target state before retrying; no rollback or reboot was performed.\n", deployment_name(request), action_name(request.action), if request.action == SwitchAction::Boot { ", including bootloader changes and profile updates," } else { "" }) },
        }).await;
        return Err(error.with_secondary(later.err().into_iter().collect()));
    }
    Ok(())
}

fn needs_health(request: &DeployRequest) -> bool {
    matches!(request.action, SwitchAction::Test | SwitchAction::Switch)
        || (request.action == SwitchAction::Boot && request.reboot)
}

fn copy_limit(request: &DeployRequest) -> Result<usize, ConfctlError> {
    if request.one_by_one || request.interactive {
        Ok(1)
    } else {
        usize::try_from(request.max_concurrent_copy)
            .ok()
            .filter(|n| *n > 0)
            .ok_or_else(|| ConfctlError::Usage("max-concurrent-copy must be positive".into()))
    }
}

fn deployment_name(request: &DeployRequest) -> &'static str {
    if request.generation.is_some() {
        "Saved-generation"
    } else {
        "New-build"
    }
}
fn selected_order(jobs: &[Job], confirmed: &[MachineId]) -> Vec<Job> {
    confirmed
        .iter()
        .filter_map(|id| jobs.iter().find(|job| job.machine == *id))
        .cloned()
        .collect()
}

fn task_end(first: u64, count: usize) -> Result<u64, ConfctlError> {
    u64::try_from(count)
        .ok()
        .and_then(|count| first.checked_add(count))
        .ok_or_else(|| ConfctlError::State("Deployment task IDs overflow".into()))
}

fn already_current(
    machine: &Machine,
    generation: &BuildGeneration,
    status: &super::host_status::HostStatus,
    action: SwitchAction,
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
        || matches!(action, SwitchAction::DryActivate | SwitchAction::Test)
        || status.generations.as_ref().is_some_and(|records| {
            records.iter().any(|record| {
                record.current && record.toplevel.as_os_str() == generation.toplevel.as_os_str()
            })
        })
}

async fn target_statuses<P: ProcessRunner>(
    copies: &Copies<'_, P>,
    jobs: &[Job],
    ssh_config: Option<&OsStr>,
    action: SwitchAction,
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
                Box::pin(target_status(
                    copies,
                    &jobs[next],
                    &token,
                    ssh_config,
                    action,
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
        .map(|status| status.expect("joined deployment status"))
        .collect())
}

async fn target_status<P: ProcessRunner>(
    copies: &Copies<'_, P>,
    job: &Job,
    token: &CancellationToken,
    ssh_config: Option<&OsStr>,
    action: SwitchAction,
) -> Result<super::host_status::HostStatus, ConfctlError> {
    let host = &copies.inventory.get(job.machine).name;
    let description = format!("query {} state on {host}", action_name(action));
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
    action: SwitchAction,
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
) -> Result<bool, ConfctlError> {
    let copies = activation.copies;
    let machine = copies.inventory.get(job.machine);
    let host = &machine.name;
    if machine.carrier.is_some() && activation.action != SwitchAction::Switch {
        checked(
            copies.events,
            copies.cancellation,
            diagnostic_style(Style::Incomplete, &format!(
                "{host}: carried {} copies store paths only; no activation or profile change.\n",
                action_name(activation.action)
            )),
        )
        .await?;
        return Ok(false);
    }
    let description = if let Some(carrier) = &machine.carrier {
        format!("set carrier profile for {host} on {carrier}")
    } else {
        format!(
            "{}{} on {host}",
            action_name(activation.action),
            if matches!(activation.action, SwitchAction::Test | SwitchAction::Switch)
                && rollback_enabled(copies.inventory.get(job.machine), copies.rollback)?
                && job.generation.auto_rollback.is_some()
            {
                " rollback-helper protocol"
            } else {
                ""
            }
        )
    };
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
        if machine.carrier.is_some() {
            activation_command(
                activation,
                job,
                task,
                &description,
                &mut entered,
                "set carrier profile",
            )
            .await?;
            return Ok(TaskState::Succeeded);
        }
        loop {
            if activation.dry_first {
                activation_command(
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
                    diagnostic_style(
                        Style::Machine,
                        &format!(
                            "Run {} for generation {} on {host}\n",
                            action_name(activation.action),
                            job.generation.name
                        ),
                    ),
                )
                .await?;
                match choose_boot(activation).await? {
                    BootChoice::Retry => continue,
                    BootChoice::Skip => return Ok(TaskState::Skipped),
                    BootChoice::Abort => {
                        return Err(ConfctlError::State(format!(
                            "Aborting {}",
                            action_name(activation.action)
                        )));
                    }
                    BootChoice::Continue => {}
                }
            }
            activation_command(
                activation,
                job,
                task,
                &description,
                &mut entered,
                action_name(activation.action),
            )
            .await?;
            if matches!(activation.action, SwitchAction::Boot | SwitchAction::Switch) {
                activation_command(
                    activation,
                    job,
                    task,
                    &description,
                    &mut entered,
                    &format!("set profile after {}", action_name(activation.action)),
                )
                .await?;
            }
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
        (Ok(state), Ok(())) => {
            check_cancel(copies.cancellation)?;
            Ok(state == TaskState::Succeeded)
        }
    }
}

async fn activation_command<P: ProcessRunner>(
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
        diagnostic_style(
            Style::Machine,
            &if phase == "set carrier profile" {
                format!("{description}\n")
            } else {
                format!("{}: {phase}\n", machine.name)
            },
        ),
    )
    .await?;
    if matches!(activation.action, SwitchAction::Test | SwitchAction::Switch)
        && phase == action_name(activation.action)
        && rollback_enabled(machine, copies.rollback)?
    {
        if let Some(helper) = &job.generation.auto_rollback {
            return rollback::run(activation, job, task, helper).await;
        }
        checked(copies.events, copies.cancellation, diagnostic_style(Style::Incomplete, &format!(
            "{}: saved rollback helper unavailable; running direct {phase} without rollback protection.\n",
            machine.name
        ))).await?;
    }
    let (target, argv) = if phase == "set carrier profile" {
        let (carrier, profile) = super::host_status::routing(copies.inventory, machine)?;
        (
            carrier,
            vec![
                "carrier-env".into(),
                "-p".into(),
                profile.into(),
                "--set".into(),
                job.generation.toplevel.as_os_str().into(),
            ],
        )
    } else if matches!(phase, "set profile after boot" | "set profile after switch") {
        (
            machine,
            vec![
                "nix-env".into(),
                "-p".into(),
                "/nix/var/nix/profiles/system".into(),
                "--set".into(),
                job.generation.toplevel.as_os_str().into(),
            ],
        )
    } else {
        (
            machine,
            vec![
                job.generation
                    .toplevel
                    .join("bin/switch-to-configuration")
                    .into_os_string(),
                phase.into(),
            ],
        )
    };
    let mut spec = super::connection::command_spec(target, activation.ssh_config, &argv)?;
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
