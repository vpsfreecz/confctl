//! Shared configured and deployed input observations; no scheduling or presentation.
use crate::{
    app::Context,
    domain::{Inventory, Machine},
    error::ConfctlError,
    events::TaskId,
    nix::{NixClient, bytes, process_error},
    process::{Captured, ProcessOutcome, ProcessRunner, ProcessScope, TerminationCause},
};
use serde_json::{Map, Value};
use std::ffi::OsStr;
use tokio_util::sync::CancellationToken;

pub(super) type Inputs = Map<String, Value>;
#[derive(Default)]
pub(super) struct InputObservation {
    pub(super) value: Option<Inputs>,
    pub(super) failed: bool,
    pub(super) diagnostics: Vec<String>,
    pub(super) native_failures: Vec<ProcessOutcome>,
    pub(super) eval_failures: Vec<ConfctlError>,
}

pub(super) async fn configured<P: ProcessRunner>(
    nix: &NixClient<P>,
    machine: &Machine,
) -> Result<InputObservation, ConfctlError> {
    let mut observation = InputObservation::default();
    let key = nix.machine_key(&machine.name).await?;
    match nix
        .eval_json(&format!(".#confctl.inputsInfo.{}", attribute(&key)))
        .await
    {
        Ok(value) => {
            observation.value = normalize(&value);
            if observation.value.is_none() {
                observation.failed = true;
                observation.diagnostics.push(format!(
                    "{}: target inputs info is not an object",
                    machine.name
                ));
            } else if observation.value.as_ref().is_some_and(Inputs::is_empty) {
                observation.diagnostics.push(format!(
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
            observation.diagnostics.push(format!(
                "{}: target inputs evaluation failed: {error}\n{}",
                machine.name,
                String::from_utf8_lossy(&stderr)
            ));
            observation.failed = true;
            observation.eval_failures.push(error);
        }
        Err(error) => return Err(error),
    }
    Ok(observation)
}

pub(super) async fn deployed(
    context: &Context,
    inventory: &Inventory,
    machine: &Machine,
    cancellation: &CancellationToken,
    task: TaskId,
    ssh_config: Option<&OsStr>,
) -> Result<InputObservation, ConfctlError> {
    let mut observation = InputObservation::default();
    observation.value = deployed_inputs(
        inventory,
        machine,
        context,
        cancellation,
        task,
        ssh_config,
        &mut observation,
    )
    .await?;
    Ok(observation)
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
    row: &mut InputObservation,
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
            Err(outcome) => record_failure(row, &machine.name, "machine.json", outcome).await?,
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
            record_failure(row, &machine.name, "deployed inputs info", outcome).await?;
            Ok(None)
        }
    }
}

async fn record_failure(
    row: &mut InputObservation,
    host: &str,
    phase: &str,
    outcome: ProcessOutcome,
) -> Result<(), ConfctlError> {
    let stderr = match bytes(&outcome.stderr).await {
        Ok(bytes) => bytes,
        Err(e) => return Err(process_error(e, outcome)),
    };
    row.diagnostics.push(format!(
        "{}: {phase} failed (exit {:?}, signal {:?})\n{}",
        host,
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
pub(super) fn normalize(value: &Value) -> Option<Inputs> {
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
pub(super) enum Comparison {
    Same,
    Changed,
    Unknown,
}
pub(super) fn comparison(target: Option<&Value>, deployed: Option<&Value>) -> Comparison {
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
pub(super) fn attribute(key: &str) -> String {
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

fn check_cancel(cancellation: &CancellationToken) -> Result<(), ConfctlError> {
    if cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests;
