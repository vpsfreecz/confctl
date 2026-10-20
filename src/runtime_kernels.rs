//! Opt-in runtime kernel refresh. Selection, confirmation and cache invalidation
//! belong to the calling operation. The borrowed mutation guard spans the whole
//! read/probe/merge/publication; an ordinary failed host is still a normal row.

use crate::{
    configuration::{FilePolicy, MutationGuard, RuntimeKernelSettings, StateFiles, WriteReceipt},
    domain::{Inventory, Machine, MachineId},
    error::ConfctlError,
    events::{EventSink, OutputStream, Style, TaskId, TaskState, UiEvent},
    machine_endpoint::{target_host, target_port},
    process::{
        Captured, PartialProcessEvidence, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec,
        TerminationCause,
    },
};
use serde_json::Value;
use std::{
    ffi::OsStr,
    future::{Future, poll_fn},
    io,
    task::Poll,
};
use tokio_util::sync::CancellationToken;

pub struct FeatureContext<'a, P> {
    pub runner: &'a P,
    pub events: &'a EventSink,
    pub files: &'a StateFiles,
    pub inventory: &'a Inventory,
    pub settings: &'a RuntimeKernelSettings,
    pub cancellation: &'a CancellationToken,
    pub mutation: &'a MutationGuard,
    pub ssh_config: Option<&'a OsStr>,
}

#[derive(Debug)]
pub struct KernelRow {
    pub machine: MachineId,
    pub kernel: String,
    pub error: Option<String>,
}

#[derive(Debug)]
pub struct KernelRefreshReport {
    pub rows: Vec<KernelRow>,
    pub updated: Vec<String>,
    pub deleted: Vec<String>,
    pub unchanged: Vec<String>,
    pub publication: WriteReceipt,
}

/// Ruby String#strip uses ASCII whitespace and NUL, then split('.') drops
/// trailing empty fields. This intentionally is neither Unicode trim nor semver.
pub fn normalize_kernel(stdout: &str) -> String {
    let stripped =
        stdout.trim_matches(|c| matches!(c, '\0' | '\t' | '\n' | '\x0b' | '\x0c' | '\r' | ' '));
    let mut components: Vec<_> = stripped.split('.').collect();
    while components.last() == Some(&"") {
        components.pop();
    }
    components.into_iter().take(3).collect::<Vec<_>>().join(".")
}

/// Purpose-specific compatibility with MachineControl.execute('uname','-r').
/// No connection-test timeout, carrier redirection or runnable filter applies.
fn probe_spec(machine: &Machine, ssh_config: Option<&OsStr>) -> Result<ProcessSpec, ConfctlError> {
    let target = target_host(machine)?;
    let mut spec = if target == "localhost" {
        let mut spec =
            ProcessSpec::new("uname", format!("query runtime kernel on {}", machine.name));
        spec.args = vec!["-r".into()];
        spec
    } else {
        let mut spec = ProcessSpec::new("ssh", format!("query runtime kernel on {}", machine.name));
        if let Some(config) = ssh_config.filter(|path| !path.is_empty()) {
            spec.args.extend(["-F".into(), config.into()]);
        }
        spec.args.extend(["-l".into(), "root".into()]);
        let port = target_port(machine)?;
        if port != "22" {
            spec.args.extend(["-p".into(), port.into()]);
        }
        spec.args.extend([target.into(), "uname -r".into()]);
        spec
    };
    // Only stdout defines a successful kernel. Both streams remain captured
    // and logged by the existing supervisor, with no direct display forwarding.
    spec.stdout = crate::process::StreamPolicy::Capture;
    spec.stderr = crate::process::StreamPolicy::Capture;
    Ok(spec)
}

fn captured_bytes(captured: &Captured) -> io::Result<Vec<u8>> {
    match captured {
        Captured::Memory(bytes) => Ok(bytes.clone()),
        Captured::Spool { file, length } => {
            use std::io::Read;
            let mut bytes = Vec::new();
            file.reopen()?.take(*length).read_to_end(&mut bytes)?;
            Ok(bytes)
        }
        Captured::Unavailable => Err(io::Error::other("kernel probe did not capture its stream")),
    }
}

fn decode_probe(machine: MachineId, outcome: ProcessOutcome) -> Result<KernelRow, ConfctlError> {
    if outcome.termination == TerminationCause::Canceled {
        return Err(ConfctlError::Cancelled);
    }
    let decode = || -> Result<KernelRow, io::Error> {
        if outcome.status.success() {
            let bytes = captured_bytes(&outcome.stdout)?;
            let text = std::str::from_utf8(&bytes)
                .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))?;
            Ok(KernelRow {
                machine,
                kernel: normalize_kernel(text),
                error: None,
            })
        } else {
            let status = outcome
                .code()
                .map(|code| format!("exit status: {code}"))
                .unwrap_or_else(|| format!("signal: {}", outcome.signal().unwrap_or(0)));
            let stderr = captured_bytes(&outcome.stderr)?;
            Ok(KernelRow {
                machine,
                kernel: "error".into(),
                error: Some(format!(
                    "uname -r failed with {status}\n{}",
                    String::from_utf8_lossy(&stderr)
                )),
            })
        }
    };
    decode().map_err(|source| ConfctlError::ProcessFailure {
        source: Box::new(ConfctlError::Io {
            operation: "decode runtime kernel probe",
            path: None,
            source,
        }),
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

async fn emit(events: &EventSink, text: String) -> Result<(), ConfctlError> {
    events
        .emit(UiEvent::Diagnostic {
            stream: OutputStream::Stdout,
            style: Style::Plain,
            text,
        })
        .await
}

/// The exact already-confirmed IDs remain fixed throughout a refresh. All
/// borrowed probe futures are polled concurrently and joined, including after
/// an infrastructure failure cancels their shared child token. No detached task
/// or 'static/Clone runner requirement is introduced to implement concurrency.
pub async fn refresh<P: ProcessRunner>(
    ctx: &FeatureContext<'_, P>,
    tasks: &[(MachineId, TaskId)],
) -> Result<KernelRefreshReport, ConfctlError> {
    if !ctx.settings.enable {
        return Err(ConfctlError::Configuration(
            "runtime kernels are disabled; enable confctl.runtimeKernels.enable".into(),
        ));
    }
    ctx.files.require_mutation(ctx.mutation)?;
    let path = ctx.settings.state_file.as_ref().ok_or_else(|| {
        ConfctlError::Configuration("runtimeKernels.stateFile is required when enabled".into())
    })?;
    let mut selected = Vec::with_capacity(tasks.len());
    let mut task_ids = Vec::with_capacity(tasks.len());
    let mut names = Vec::with_capacity(tasks.len());
    for (id, task) in tasks {
        let machine = ctx.inventory.machines.get(id.0).ok_or_else(|| {
            ConfctlError::State("kernel refresh has an invalid selected machine ID".into())
        })?;
        if selected.contains(id) || names.contains(&machine.name.as_str()) {
            return Err(ConfctlError::State(
                "kernel refresh repeats a selected machine ID".into(),
            ));
        }
        if task.0 == 0 || task_ids.contains(task) {
            return Err(ConfctlError::State(
                "kernel refresh requires distinct nonzero task IDs".into(),
            ));
        }
        if machine.spin() != Some("vpsadminos") {
            return Err(ConfctlError::State(
                "kernel refresh requires an already-selected vpsAdminOS scope".into(),
            ));
        }
        selected.push(*id);
        task_ids.push(*task);
        names.push(machine.name.as_str());
    }
    let mut saved = match ctx.files.read_optional(path)? {
        None => Value::Object(Default::default()),
        Some(bytes) => crate::remote::json::parse(&bytes)?,
    };
    if ctx.cancellation.is_cancelled() {
        return Err(ConfctlError::Cancelled);
    }
    let local = ctx.cancellation.child_token();
    let specs = selected
        .iter()
        .map(|id| probe_spec(ctx.inventory.get(*id), ctx.ssh_config))
        .collect::<Result<Vec<_>, _>>()?;
    for ((_, task), spec) in tasks.iter().zip(&specs) {
        ctx.events
            .emit(UiEvent::TaskPending {
                task: *task,
                description: spec.redacted_description.clone(),
            })
            .await?;
    }
    let mut pending: Vec<_> = selected
        .iter()
        .copied()
        .zip(specs)
        .zip(task_ids)
        .map(|((id, spec), task)| {
            let cancellation = local.clone();
            Some(Box::pin(async move {
                let result = if cancellation.is_cancelled() {
                    Err(ConfctlError::Cancelled)
                } else {
                    match ctx
                        .events
                        .emit(UiEvent::TaskStarted {
                            task,
                            description: spec.redacted_description.clone(),
                        })
                        .await
                    {
                        Ok(()) => ctx
                            .runner
                            .run(spec, ProcessScope::new(cancellation.clone(), task))
                            .await
                            .and_then(|outcome| decode_probe(id, outcome)),
                        Err(error) => Err(error),
                    }
                };
                let state = match &result {
                    Ok(row) if row.error.is_none() => TaskState::Succeeded,
                    Ok(_) => TaskState::Failed,
                    Err(error) if error.is_cancelled() => TaskState::Canceled,
                    Err(_) => TaskState::Failed,
                };
                let final_event = ctx.events.emit(UiEvent::TaskFinished { task, state }).await;
                match (result, final_event) {
                    (result, Ok(())) => result,
                    (Ok(_), Err(error)) => Err(error),
                    (Err(error), Err(event_error)) => Err(error.with_secondary(vec![event_error])),
                }
            }))
        })
        .collect();
    let mut rows: Vec<Option<KernelRow>> = std::iter::repeat_with(|| None)
        .take(selected.len())
        .collect();
    let mut failures = Vec::new();
    poll_fn(|cx| {
        let mut remaining = false;
        for (index, slot) in pending.iter_mut().enumerate() {
            let Some(future) = slot else {
                continue;
            };
            match future.as_mut().poll(cx) {
                Poll::Pending => remaining = true,
                Poll::Ready(result) => {
                    *slot = None;
                    match result {
                        Ok(row) => rows[index] = Some(row),
                        Err(error) => {
                            failures.push(error);
                            local.cancel();
                        }
                    }
                }
            }
        }
        if remaining {
            Poll::Pending
        } else {
            Poll::Ready(())
        }
    })
    .await;
    if !failures.is_empty() {
        return Err(failures.remove(0).with_secondary(failures));
    }
    if ctx.cancellation.is_cancelled() {
        return Err(ConfctlError::Cancelled);
    }
    let rows: Vec<_> = rows
        .into_iter()
        .map(|row| row.expect("joined successful probe"))
        .collect();
    let mut updated = Vec::new();
    let mut deleted = Vec::new();
    let mut unchanged = Vec::new();
    emit(ctx.events, format!("{:<50} KERNEL\n", "HOST")).await?;
    for row in &rows {
        let name = &ctx.inventory.get(row.machine).name;
        if row.error.is_none() {
            if row.kernel == "error" {
                unchanged.push(name.clone());
            } else if assign(&mut saved, name, &row.kernel)? {
                updated.push(name.clone());
            } else {
                unchanged.push(name.clone());
            }
        }
        emit(ctx.events, format!("{name:<50} {}\n", row.kernel)).await?;
    }
    if rows.iter().any(|row| row.error.is_some()) {
        emit(ctx.events, "\n".into()).await?;
        for row in &rows {
            if let Some(error) = &row.error {
                let name = &ctx.inventory.get(row.machine).name;
                emit(ctx.events, format!("Error on {name}: {error}\n")).await?;
                if delete(&mut saved, name)? {
                    deleted.push(name.clone());
                } else {
                    unchanged.push(name.clone());
                }
            }
        }
    }
    let bytes = crate::remote::json::pretty(&saved)?;
    if ctx.cancellation.is_cancelled() {
        return Err(ConfctlError::Cancelled);
    }
    let publication = ctx
        .files
        .replace(ctx.mutation, path, &bytes, FilePolicy::KernelState)?;
    Ok(KernelRefreshReport {
        rows,
        updated,
        deleted,
        unchanged,
        publication,
    })
}

// Preserve Ruby's permissive parsed JSON rather than validating object<string>.
// Empty selections reserialize any JSON. Arrays support delete(value), and a
// String's nonmutating delete result was ignored by the original implementation.
fn assign(saved: &mut Value, name: &str, kernel: &str) -> Result<bool, ConfctlError> {
    match saved {
        Value::Object(values) => {
            let value = Value::String(kernel.into());
            if values.get(name) == Some(&value) {
                Ok(false)
            } else {
                values.insert(name.into(), value);
                Ok(true)
            }
        }
        Value::String(text) => {
            let Some(start) = text.find(name) else {
                return Err(ConfctlError::State(
                    "kernel state string does not contain the selected host".into(),
                ));
            };
            let changed = &text[start..start + name.len()] != kernel;
            text.replace_range(start..start + name.len(), kernel);
            Ok(changed)
        }
        _ => Err(ConfctlError::State(
            "kernel state does not support string-key assignment".into(),
        )),
    }
}
fn delete(saved: &mut Value, name: &str) -> Result<bool, ConfctlError> {
    match saved {
        Value::Object(values) => Ok(values.shift_remove(name).is_some()),
        Value::Array(values) => {
            let before = values.len();
            values.retain(|value| value.as_str() != Some(name));
            Ok(before != values.len())
        }
        Value::String(_) => Ok(false),
        _ => Err(ConfctlError::State(
            "kernel state does not support deletion".into(),
        )),
    }
}

#[cfg(test)]
mod tests;
