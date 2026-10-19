//! Configured health commands, compared without changing raw stream bytes.
use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{Action, Machine, ManagedFilter, Selection, SelectionPolicy, ruby_string, select},
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
use serde_json::Value;
use std::{
    ffi::{OsStr, OsString},
    future::{Future, poll_fn},
    io::{self, Read},
    path::Path,
    task::Poll,
    time::{Duration, Instant},
};
use tokio_util::sync::CancellationToken;

pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    let Action::HealthCheck { targets, max_jobs } = action else {
        unreachable!("typed health-check adapter")
    };
    let max_jobs = usize::try_from(*max_jobs)
        .ok()
        .filter(|n| *n > 0)
        .ok_or_else(|| ConfctlError::Usage("max-jobs must be positive".into()))?;
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
            managed: ManagedFilter::Managed,
            runnable: true,
            ..SelectionPolicy::ALL
        },
    )?;
    let mut eligible = Selection::default();
    let mut counts = Vec::new();
    let mut checks = Vec::new();
    for id in selected.machines {
        canceled(&ctx.cancellation)?;
        let host_checks = from_metadata(inventory.get(id))?;
        if !host_checks.is_empty() {
            eligible.machines.push(id);
            counts.push(host_checks.len());
            checks.extend(host_checks);
        }
    }
    if checks.is_empty() {
        return Err(ConfctlError::Selection(
            "No runnable machines to check or no health checks configured".into(),
        ));
    }
    let settings = defaults.read().await?;
    let mut table = super::machines::table(inventory, &eligible, &settings.list_columns, true);
    if let UiEvent::Table { columns, rows, .. } = &mut table {
        columns.insert(0, "CHECKS".into());
        for (row, count) in rows.iter_mut().zip(&counts) {
            row.insert(0, count.to_string());
        }
    }
    if !ctx
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                message(
                    Style::Plain,
                    "Health checks will be run on the following machines:\n".into(),
                ),
                table,
                message(Style::Plain, format!("{} checks in total\n", checks.len())),
            ],
            assume_yes: targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    canceled(&ctx.cancellation)?;
    let ssh_config = std::env::var_os("CONFCTL_SSH_CONFIG");
    let execution = CheckContext {
        runner: &ctx.processes,
        events: &ctx.events,
        cancellation: &ctx.cancellation,
        root: root.path(),
        ssh_config: ssh_config.as_deref(),
        log: ctx.log_path(),
        invocation: Some(ctx),
    };
    let failures = run_checks(&execution, &checks, max_jobs, TaskId(1)).await?;
    emit(
        &ctx.events,
        &ctx.cancellation,
        Style::Plain,
        format!(
            "{} checks passed, {} failed\n",
            checks.len() - failures.len(),
            failures.len()
        ),
    )
    .await?;
    for failure in &failures {
        emit(
            &ctx.events,
            &ctx.cancellation,
            Style::Failure,
            format!(
                "{}: {}: {}\n",
                checks[failure.index].machine.name,
                checks[failure.index].description(),
                failure.messages.join("; ")
            ),
        )
        .await?;
    }
    if !failures.is_empty() {
        return Err(ConfctlError::State("Health checks failed".into()));
    }
    canceled(&ctx.cancellation)
}

pub(super) struct Check {
    machine: Machine,
    kind: Kind,
}
enum Kind {
    Command(CommandCheck),
    Systemd(super::health_systemd::Check),
}
struct CommandCheck {
    description: String,
    argv: Vec<OsString>,
    remote: bool,
    exit_status: u64,
    stdout: OutputCheck,
    stderr: OutputCheck,
    timeout: Duration,
    cooldown: Duration,
}
#[derive(Default)]
struct OutputCheck {
    exact: Option<Vec<u8>>,
    include: Vec<Vec<u8>>,
    exclude: Vec<Vec<u8>>,
}

fn configuration(text: impl Into<String>) -> ConfctlError {
    ConfctlError::Configuration(text.into())
}
fn duration(value: Option<&Value>, default: u64, field: &str) -> Result<Duration, ConfctlError> {
    match value {
        None => Ok(Duration::from_secs(default)),
        Some(v) => v.as_u64().map(Duration::from_secs).ok_or_else(|| {
            configuration(format!(
                "Health check {field} must be a nonnegative integer"
            ))
        }),
    }
}
fn string(value: Option<&Value>, default: &str, field: &str) -> Result<String, ConfctlError> {
    match value {
        None => Ok(default.into()),
        Some(v) => v
            .as_str()
            .map(str::to_owned)
            .ok_or_else(|| configuration(format!("Health check {field} must be a string"))),
    }
}
fn strings(value: Option<&Value>, field: &str) -> Result<Vec<Vec<u8>>, ConfctlError> {
    let Some(value) = value else {
        return Ok(vec![]);
    };
    value
        .as_array()
        .ok_or_else(|| configuration(format!("Health check {field} must be a string list")))?
        .iter()
        .map(|v| {
            v.as_str()
                .map(|s| s.as_bytes().to_vec())
                .ok_or_else(|| configuration(format!("Health check {field} must contain strings")))
        })
        .collect()
}
impl OutputCheck {
    fn from_value(value: Option<&Value>) -> Result<Self, ConfctlError> {
        let Some(value) = value else {
            return Ok(Self::default());
        };
        let map = value
            .as_object()
            .ok_or_else(|| configuration("Health check output predicates must be an object"))?;
        let exact = match map.get("match") {
            None | Some(Value::Null) => None,
            Some(v) => Some(
                v.as_str()
                    .ok_or_else(|| {
                        configuration("Health check output match must be a string or null")
                    })?
                    .as_bytes()
                    .to_vec(),
            ),
        };
        Ok(Self {
            exact,
            include: strings(map.get("include"), "output include")?,
            exclude: strings(map.get("exclude"), "output exclude")?,
        })
    }
    fn error(&self, bytes: &[u8], stream: &str) -> Option<String> {
        if let Some(expected) = &self.exact
            && bytes != expected
        {
            return Some(format!("{stream} does not match {expected:?}"));
        }
        if let Some(fragment) = self.include.iter().find(|s| !contains(bytes, s)) {
            return Some(format!("{stream} does not include {fragment:?}"));
        }
        self.exclude
            .iter()
            .find(|s| contains(bytes, s))
            .map(|fragment| format!("{stream} includes {fragment:?}"))
    }
}
fn contains(bytes: &[u8], fragment: &[u8]) -> bool {
    fragment.is_empty()
        || bytes
            .windows(fragment.len())
            .any(|window| window == fragment)
}
fn expand(machine: &Machine, text: &str, count: usize) -> Result<OsString, ConfctlError> {
    let mut expanded = String::new();
    let mut rest = text;
    while let Some(open) = rest.find('{') {
        expanded.push_str(&rest[..open]);
        let Some(close) = rest[open + 1..].find('}').map(|n| open + 1 + n) else {
            expanded.push_str(&rest[open..]);
            rest = "";
            break;
        };
        let key = &rest[open + 1..close];
        if key.is_empty() {
            expanded.push_str("{}");
        } else if key == "checks" {
            expanded.push_str(&count.to_string());
        } else {
            expanded.push_str(&ruby_string(&attribute(machine, key)?));
        }
        rest = &rest[close + 1..];
    }
    expanded.push_str(rest);
    if expanded.contains('\0') {
        return Err(configuration("Health check argv contains a NUL byte"));
    }
    Ok(expanded.into())
}
fn attribute(machine: &Machine, key: &str) -> Result<Value, ConfctlError> {
    if !key.contains('.') {
        return Ok(machine.attribute(key));
    }
    let mut value = &machine.metadata;
    for part in key.split('.') {
        match value {
            Value::Null => return Ok(Value::Null),
            Value::Object(fields) => match fields.get(part) {
                Some(next) => value = next,
                None => return Ok(Value::Null),
            },
            _ => {
                return Err(configuration(format!(
                    "Health check attribute {key} traverses a non-object"
                )));
            }
        }
    }
    Ok(value.clone())
}
fn command(
    machine: &Machine,
    value: &Value,
    remote: bool,
    count: usize,
) -> Result<CommandCheck, ConfctlError> {
    let map = value
        .as_object()
        .ok_or_else(|| configuration("Health check command must be an object"))?;
    let argv = map
        .get("command")
        .and_then(Value::as_array)
        .ok_or_else(|| configuration("Health check command must have an argv list"))?
        .iter()
        .map(|v| {
            expand(
                machine,
                v.as_str()
                    .ok_or_else(|| configuration("Health check argv must contain strings"))?,
                count,
            )
        })
        .collect::<Result<Vec<_>, _>>()?;
    if argv.first().is_none_or(|arg| arg.is_empty()) {
        return Err(configuration(
            "Health check argv must have a nonempty executable",
        ));
    }
    let description = string(map.get("description"), "", "description")?;
    let exit_status = match map.get("exitStatus") {
        None => 0,
        Some(v) => v.as_u64().ok_or_else(|| {
            configuration("Health check exitStatus must be a nonnegative integer")
        })?,
    };
    Ok(CommandCheck {
        description,
        argv,
        remote,
        exit_status,
        stdout: OutputCheck::from_value(map.get("standardOutput"))?,
        stderr: OutputCheck::from_value(map.get("standardError"))?,
        timeout: duration(map.get("timeout"), 60, "timeout")?,
        cooldown: duration(map.get("cooldown"), 3, "cooldown")?,
    })
}
pub(super) fn from_metadata(machine: &Machine) -> Result<Vec<Check>, ConfctlError> {
    let mut checks = Vec::new();
    let Some(metadata) = machine
        .metadata
        .get("healthChecks")
        .filter(|v| !v.is_null())
    else {
        return Ok(checks);
    };
    let metadata = metadata
        .as_object()
        .ok_or_else(|| configuration("healthChecks must be an object"))?;
    for (kind, value) in metadata {
        match kind.as_str() {
            "systemd" => {
                for check in super::health_systemd::from_metadata(machine, value)? {
                    checks.push(Check {
                        machine: machine.clone(),
                        kind: Kind::Systemd(check),
                    });
                }
            }
            "builderCommands" | "machineCommands" => {
                let values = value
                    .as_array()
                    .ok_or_else(|| configuration(format!("Health check {kind} must be a list")))?;
                for value in values {
                    let check = command(machine, value, kind == "machineCommands", checks.len())?;
                    checks.push(Check {
                        machine: machine.clone(),
                        kind: Kind::Command(check),
                    });
                }
            }
            _ => {
                return Err(configuration(format!(
                    "Unsupported health-check type {kind:?}"
                )));
            }
        }
    }
    Ok(checks)
}
impl Check {
    pub(super) fn host_name(&self) -> &str {
        &self.machine.name
    }
    pub(super) fn description(&self) -> String {
        match &self.kind {
            Kind::Command(c) if c.description.is_empty() => super::connection::shell_join(&c.argv)
                .to_string_lossy()
                .into_owned(),
            Kind::Command(c) => c.description.clone(),
            Kind::Systemd(c) => c.description.clone(),
        }
    }
    fn timeout(&self) -> Duration {
        match &self.kind {
            Kind::Command(c) => c.timeout,
            Kind::Systemd(c) => c.timeout,
        }
    }
    fn cooldown(&self) -> Duration {
        match &self.kind {
            Kind::Command(c) => c.cooldown,
            Kind::Systemd(c) => c.cooldown,
        }
    }
    fn spec(&self, root: &Path, ssh_config: Option<&OsStr>) -> Result<ProcessSpec, ConfctlError> {
        let mut spec = match &self.kind {
            Kind::Command(c) if !c.remote => {
                let mut s = ProcessSpec::new(&c.argv[0], self.description());
                s.args = c.argv[1..].to_vec();
                s
            }
            Kind::Command(c) => {
                super::connection::command_spec(&self.machine, ssh_config, &c.argv)?
            }
            Kind::Systemd(c) => {
                super::connection::command_spec(&self.machine, ssh_config, &c.argv())?
            }
        };
        spec.cwd = Some(root.into());
        spec.redacted_description = format!("{}: {}", self.machine.name, self.description());
        spec.stdout = StreamPolicy::CaptureAndForward;
        spec.stderr = StreamPolicy::CaptureAndForward;
        Ok(spec)
    }
    fn errors(&self, outcome: &ProcessOutcome, stdout: &[u8], stderr: &[u8]) -> Vec<String> {
        let mut errors = Vec::new();
        let expected = match &self.kind {
            Kind::Command(c) => c.exit_status,
            Kind::Systemd(_) => 0,
        };
        if outcome.code().and_then(|n| u64::try_from(n).ok()) != Some(expected) {
            errors.push(format!(
                "expected exit status {expected}, received {}",
                native_status(outcome)
            ));
        }
        match &self.kind {
            Kind::Command(c) => {
                if let Some(error) = c
                    .stdout
                    .error(stdout, "stdout")
                    .or_else(|| c.stderr.error(stderr, "stderr"))
                {
                    errors.push(error);
                }
            }
            Kind::Systemd(c) if outcome.status.success() => errors.extend(c.errors(stdout)),
            Kind::Systemd(_) => {}
        }
        errors
    }
}
fn native_status(outcome: &ProcessOutcome) -> String {
    outcome
        .code()
        .map(|n| format!("exit status {n}"))
        .unwrap_or_else(|| format!("signal {}", outcome.signal().unwrap_or(0)))
}
fn captured(capture: &Captured) -> io::Result<Vec<u8>> {
    match capture {
        Captured::Memory(bytes) => Ok(bytes.clone()),
        Captured::Spool { file, length } => {
            let mut bytes = Vec::new();
            file.reopen()?.take(*length).read_to_end(&mut bytes)?;
            if bytes.len() as u64 != *length {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "incomplete health capture spool",
                ));
            }
            Ok(bytes)
        }
        Captured::Unavailable => Err(io::Error::other("health stream was not captured")),
    }
}
fn compare(check: &Check, outcome: ProcessOutcome) -> Result<Vec<String>, ConfctlError> {
    let result = (|| {
        if outcome.termination == TerminationCause::Canceled {
            return Err(ConfctlError::Cancelled);
        }
        let read = || -> io::Result<Vec<String>> {
            if !outcome.stdout_complete || !outcome.stderr_complete {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "incomplete health check capture",
                ));
            }
            let stdout = captured(&outcome.stdout)?;
            let stderr = captured(&outcome.stderr)?;
            Ok(check.errors(&outcome, &stdout, &stderr))
        };
        read().map_err(|source| ConfctlError::Io {
            operation: "read health check capture",
            path: None,
            source,
        })
    })();
    result.map_err(|source| ConfctlError::ProcessFailure {
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

pub(super) struct CheckContext<'a, P> {
    pub runner: &'a P,
    pub events: &'a EventSink,
    pub cancellation: &'a CancellationToken,
    pub root: &'a Path,
    pub ssh_config: Option<&'a OsStr>,
    pub log: &'a Path,
    pub invocation: Option<&'a Context>,
}
#[derive(Debug)]
pub(super) struct Failure {
    pub index: usize,
    pub messages: Vec<String>,
}
fn canceled(token: &CancellationToken) -> Result<(), ConfctlError> {
    if token.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}
fn message(style: Style, text: String) -> UiEvent {
    UiEvent::Diagnostic {
        stream: OutputStream::Stdout,
        style,
        text,
    }
}
async fn emit(
    events: &EventSink,
    token: &CancellationToken,
    style: Style,
    text: String,
) -> Result<(), ConfctlError> {
    events.emit(message(style, text)).await?;
    canceled(token)
}
async fn finalize_unentered(
    events: &EventSink,
    first: TaskId,
    range: std::ops::Range<usize>,
    canceled: bool,
    error: ConfctlError,
) -> ConfctlError {
    let mut secondary = Vec::new();
    for i in range {
        if let Err(error) = events
            .emit(UiEvent::TaskFinished {
                task: TaskId(first.0 + i as u64),
                state: if canceled {
                    TaskState::Canceled
                } else {
                    TaskState::Skipped
                },
            })
            .await
        {
            secondary.push(error);
        }
    }
    error.with_secondary(secondary)
}
pub(super) async fn run_checks<P: ProcessRunner>(
    ctx: &CheckContext<'_, P>,
    checks: &[Check],
    max_jobs: usize,
    first: TaskId,
) -> Result<Vec<Failure>, ConfctlError> {
    run_check_refs(ctx, &checks.iter().collect::<Vec<_>>(), max_jobs, first).await
}
pub(super) async fn run_check_refs<P: ProcessRunner>(
    ctx: &CheckContext<'_, P>,
    checks: &[&Check],
    max_jobs: usize,
    first: TaskId,
) -> Result<Vec<Failure>, ConfctlError> {
    if max_jobs == 0 {
        return Err(ConfctlError::Usage("max-jobs must be positive".into()));
    }
    first
        .0
        .checked_add(checks.len() as u64)
        .ok_or_else(|| configuration("Health task IDs overflow"))?;
    let mut admitted = 0;
    let admission = async {
        for (i, check) in checks.iter().enumerate() {
            canceled(ctx.cancellation)?;
            let task = TaskId(first.0 + i as u64);
            ctx.events
                .emit(UiEvent::TaskPending {
                    task,
                    description: format!("{}: {}", check.machine.name, check.description()),
                })
                .await?;
            admitted += 1;
            canceled(ctx.cancellation)?;
            ctx.events
                .emit(UiEvent::LogAvailable {
                    task,
                    path: ctx.log.into(),
                })
                .await?;
            canceled(ctx.cancellation)?;
        }
        Ok::<_, ConfctlError>(())
    }
    .await;
    if let Err(error) = admission {
        return Err(finalize_unentered(
            ctx.events,
            first,
            0..admitted,
            ctx.cancellation.is_cancelled() || error.is_cancelled(),
            error,
        )
        .await);
    }
    let token = ctx.cancellation.child_token();
    let mut next = 0;
    let mut active = Vec::new();
    let mut failures = Vec::new();
    let mut errors = Vec::new();
    poll_fn(|cx| {
        while active.len() < max_jobs && next < checks.len() && !token.is_cancelled() {
            active.push((
                next,
                Box::pin(run_one(
                    ctx,
                    checks[next],
                    TaskId(first.0 + next as u64),
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
                    match result {
                        Ok(messages) if !messages.is_empty() => {
                            failures.push(Failure { index, messages })
                        }
                        Ok(_) => {}
                        Err(error) => {
                            errors.push(error);
                            token.cancel();
                        }
                    }
                }
            }
        }
        if active.is_empty() && (next == checks.len() || token.is_cancelled()) {
            Poll::Ready(())
        } else {
            if active.len() < max_jobs && next < checks.len() {
                cx.waker().wake_by_ref();
            }
            Poll::Pending
        }
    })
    .await;
    if !errors.is_empty() {
        let error = errors.remove(0).with_secondary(errors);
        return Err(finalize_unentered(
            ctx.events,
            first,
            next..checks.len(),
            token.is_cancelled() || error.is_cancelled(),
            error,
        )
        .await);
    }
    if let Err(error) = canceled(ctx.cancellation) {
        return Err(finalize_unentered(ctx.events, first, next..checks.len(), true, error).await);
    }
    failures.sort_by_key(|failure| failure.index);
    Ok(failures)
}
async fn run_one<P: ProcessRunner>(
    ctx: &CheckContext<'_, P>,
    check: &Check,
    task: TaskId,
    token: &CancellationToken,
) -> Result<Vec<String>, ConfctlError> {
    let label = format!("{}: {}", check.machine.name, check.description());
    let mut completed = None;
    let result=async {
        canceled(token)?;
        ctx.events.emit(UiEvent::TaskStarted {task,description:label.clone()}).await?;canceled(token)?;
        let started=Instant::now();let mut attempt=1;
        loop {
            emit(ctx.events,token,Style::Plain,format!("{label}: attempt {attempt}\n")).await?;
            let spec=check.spec(ctx.root,ctx.ssh_config)?;
            let mut scope=ProcessScope::new(token.clone(),task);scope.phase=Some(Phase::HealthChecks);
            let outcome=ctx.runner.run(spec,scope).await?;
            let native=native_status(&outcome);
            let errors=compare(check,outcome)?;
            if errors.is_empty() {
                completed = Some(TaskState::Succeeded);
                emit(ctx.events,token,Style::Success,format!("{label}: succeeded on attempt {attempt} ({native})\n")).await?;
                return Ok(errors);
            }
            if let Some(invocation)=ctx.invocation {invocation.keep_log();}
            emit(ctx.events,token,Style::Failure,format!("{label}: failed attempt {attempt} ({native}): {}\n",errors.join("; "))).await?;
            tokio::select! {biased; _=token.cancelled()=>return Err(ConfctlError::Cancelled),_=tokio::time::sleep(check.cooldown())=>{}}
            canceled(token)?;
            if started.elapsed()>check.timeout() {return Ok(errors);}
            attempt+=1;
        }
    }.await;
    let state = completed.unwrap_or_else(|| match &result {
        Ok(errors) if errors.is_empty() => TaskState::Succeeded,
        Ok(_) => TaskState::Failed,
        Err(error) if error.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    });
    let delivery = ctx.events.emit(UiEvent::TaskFinished { task, state }).await;
    match (result, delivery) {
        (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
        (_, Err(error)) | (Err(error), _) => Err(error),
        (Ok(errors), Ok(())) => {
            canceled(token)?;
            Ok(errors)
        }
    }
}
#[cfg(test)]
mod tests;
