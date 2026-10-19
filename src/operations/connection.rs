//! Serial, read-only uptime probes with ordinary per-host failure results.

use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{Action, Machine, SelectionPolicy, select},
    error::ConfctlError,
    events::{EventSink, OutputStream, Style, TaskId, TaskState, UiEvent},
    nix::{InvocationSettings, NixClient, NixOptions},
    process::{
        Captured, PartialProcessEvidence, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec,
        TerminationCause,
    },
    terminal::ConfirmPrompt,
    timing::Phase,
};
use serde_json::Value;
use std::{
    ffi::{OsStr, OsString},
    io,
    os::unix::ffi::{OsStrExt, OsStringExt},
    path::Path,
};
use tokio_util::sync::CancellationToken;

pub(super) async fn test(
    action: &Action,
    root: ConfigRoot,
    context: &Context,
) -> Result<(), ConfctlError> {
    let Action::TestConnection { targets, managed } = action else {
        unreachable!("connection adapter receives its typed action")
    };
    let defaults = InvocationSettings::new(
        root.clone(),
        context.processes.clone(),
        context.cancellation.clone(),
    );
    let nix = NixClient::configured(
        root.clone(),
        context.processes.clone(),
        NixOptions::default(),
        context.cancellation.clone(),
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
            managed: *managed,
            runnable: true,
            ..SelectionPolicy::ALL
        },
    )?;
    if selected.machines.is_empty() {
        return Err(ConfctlError::Selection("No machines to test".into()));
    }
    let settings = defaults.read().await?;
    if !context
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: "Test SSH connection to the following machines:\n".into(),
                },
                super::machines::table(inventory, &selected, &settings.list_columns, true),
            ],
            assume_yes: targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    let machines = selected
        .machines
        .iter()
        .map(|id| inventory.get(*id))
        .collect::<Vec<_>>();
    let ssh_config = std::env::var_os("CONFCTL_SSH_CONFIG");
    check_hosts(
        &context.processes,
        &context.events,
        &context.cancellation,
        root.path(),
        &machines,
        ssh_config.as_deref(),
    )
    .await
}

async fn check_hosts<P: ProcessRunner>(
    runner: &P,
    events: &EventSink,
    cancellation: &CancellationToken,
    root: &Path,
    machines: &[&Machine],
    ssh_config: Option<&OsStr>,
) -> Result<(), ConfctlError> {
    for (index, machine) in machines.iter().enumerate() {
        if let Err(error) = events
            .emit(UiEvent::TaskPending {
                task: TaskId(index as u64 + 1),
                description: description(machine),
            })
            .await
        {
            return Err(finish_unentered(error, events, 0..index, false).await);
        }
    }
    let mut failed = Vec::new();
    for (index, machine) in machines.iter().enumerate() {
        let task = TaskId(index as u64 + 1);
        let result = async {
            if cancellation.is_cancelled() {
                return Err(ConfctlError::Cancelled);
            }
            events
                .emit(UiEvent::TaskStarted {
                    task,
                    description: description(machine),
                })
                .await?;
            let mut spec = probe_spec(machine, ssh_config)?;
            spec.cwd = Some(root.into());
            let mut scope = ProcessScope::new(cancellation.clone(), task);
            scope.phase = Some(Phase::ConnectionTests);
            let outcome = runner.run(spec, scope).await?;
            let failure = decode_probe(outcome)?;
            if let Some(failure) = &failure {
                emit(
                    events,
                    Style::Failure,
                    format!("Unable to connect to {}: {failure}\n\n", machine.name),
                )
                .await?;
            }
            Ok(failure.is_some())
        }
        .await;
        let state = match &result {
            Ok(false) => TaskState::Succeeded,
            Ok(true) => TaskState::Failed,
            Err(error) if error.is_cancelled() => TaskState::Canceled,
            Err(_) => TaskState::Failed,
        };
        let finished = events.emit(UiEvent::TaskFinished { task, state }).await;
        let result = match (result, finished) {
            (result, Ok(())) => result,
            (Ok(_), Err(error)) => Err(error),
            (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
        };
        match result {
            Ok(true) => failed.push(machine.name.clone()),
            Ok(false) => {}
            Err(error) => {
                let canceled = error.is_cancelled();
                return Err(
                    finish_unentered(error, events, index + 1..machines.len(), canceled).await,
                );
            }
        }
        // The consumer may cancel while acknowledging the final host result.
        // Keep that observed result; cancellation belongs to the invocation.
        if cancellation.is_cancelled() {
            return Err(finish_unentered(
                ConfctlError::Cancelled,
                events,
                index + 1..machines.len(),
                true,
            )
            .await);
        }
    }
    emit(
        events,
        Style::Plain,
        format!(
            "\nResult: {} successful, {} failed\n\nFailed machines:\n{}",
            machines.len() - failed.len(),
            failed.len(),
            failed
                .iter()
                .map(|name| format!("  {name}\n"))
                .collect::<String>()
        ),
    )
    .await?;
    if cancellation.is_cancelled() {
        return Err(ConfctlError::Cancelled);
    }
    Ok(())
}

async fn finish_unentered(
    mut error: ConfctlError,
    events: &EventSink,
    indices: std::ops::Range<usize>,
    canceled: bool,
) -> ConfctlError {
    for index in indices {
        if let Err(later) = events
            .emit(UiEvent::TaskFinished {
                task: TaskId(index as u64 + 1),
                state: if canceled {
                    TaskState::Canceled
                } else {
                    TaskState::Skipped
                },
            })
            .await
        {
            error = error.with_secondary(vec![later]);
        }
    }
    error
}

fn description(machine: &Machine) -> String {
    format!("test connection to {}", machine.name)
}

fn probe_spec(machine: &Machine, ssh_config: Option<&OsStr>) -> Result<ProcessSpec, ConfctlError> {
    let mut spec = command_spec(machine, ssh_config, &["cat".into(), "/proc/uptime".into()])?;
    spec.redacted_description = description(machine);
    if spec.program == Path::new("ssh") {
        // Target and the one remote command follow these probe-only options.
        let before_target = spec.args.len() - 2;
        spec.args.splice(
            before_target..before_target,
            [
                "-o",
                "ConnectTimeout=3",
                "-o",
                "ServerAliveInterval=3",
                "-o",
                "ServerAliveCountMax=1",
            ]
            .map(OsString::from),
        );
    }
    Ok(spec)
}

/// Build only the supplied machine's route; callers choose any carrier first.
/// Remote argv becomes one Shellwords command argument. Empty remote argv
/// requests an interactive SSH session; localhost requires an executable.
pub(super) fn command_spec(
    machine: &Machine,
    ssh_config: Option<&OsStr>,
    argv: &[OsString],
) -> Result<ProcessSpec, ConfctlError> {
    let host = match machine.metadata.get("host") {
        None => None,
        Some(Value::Object(host)) => Some(host),
        _ => {
            return Err(ConfctlError::Configuration(
                "machine host metadata does not support key lookup".into(),
            ));
        }
    };
    let target = match host.and_then(|host| host.get("target")) {
        None => machine.name.as_str(),
        Some(Value::String(target)) => target,
        _ => {
            return Err(ConfctlError::Configuration(format!(
                "{} has no target host",
                machine.name
            )));
        }
    };
    if target == "localhost" {
        let (program, args) = argv.split_first().ok_or_else(|| {
            ConfctlError::Configuration("local command requires an executable".into())
        })?;
        let mut spec = ProcessSpec::new(program, format!("command on {}", machine.name));
        spec.args = args.to_vec();
        return Ok(spec);
    }
    let mut spec = ProcessSpec::new("ssh", format!("command on {}", machine.name));
    if let Some(config) = ssh_config.filter(|path| !path.is_empty()) {
        spec.args.extend(["-F".into(), config.into()]);
    }
    spec.args.extend(["-l".into(), "root".into()]);
    let port = host
        .and_then(|host| host.get("port"))
        .map(ruby_port)
        .transpose()?
        .unwrap_or_else(|| "22".into());
    if port != "22" {
        spec.args.extend(["-p".into(), port.into()]);
    }
    spec.args.push(target.into());
    if !argv.is_empty() {
        spec.args.push(shell_join(argv));
    }
    Ok(spec)
}

/// Native-byte Shellwords escaping: a newline needs quoted literal content,
/// since a backslash followed by LF would remove it in the remote shell.
pub(super) fn shell_quote(value: &OsStr) -> OsString {
    if value.is_empty() {
        return "''".into();
    }
    let mut escaped = Vec::new();
    for chunk in value.as_bytes().utf8_chunks() {
        for c in chunk.valid().chars() {
            if c == '\n' {
                escaped.extend_from_slice(b"'\n'");
            } else {
                if !c.is_ascii_alphanumeric() && !"_-.,:+/@".contains(c) {
                    escaped.push(b'\\');
                }
                escaped.extend_from_slice(c.encode_utf8(&mut [0; 4]).as_bytes());
            }
        }
        for byte in chunk.invalid() {
            escaped.extend_from_slice(&[b'\\', *byte]);
        }
    }
    OsString::from_vec(escaped)
}

pub(super) fn shell_join(argv: &[OsString]) -> OsString {
    let mut joined = OsString::new();
    for (index, arg) in argv.iter().enumerate() {
        if index != 0 {
            joined.push(" ");
        }
        joined.push(shell_quote(arg));
    }
    joined
}

// Machine#target_port uses Ruby to_i, including decimal prefixes and floats.
fn ruby_port(value: &Value) -> Result<String, ConfctlError> {
    let text = match value {
        Value::Null => return Ok("0".into()),
        Value::String(text) => text.clone(),
        Value::Number(number) => {
            let text = number.to_string();
            if text.contains(['.', 'e', 'E']) {
                let value = text
                    .parse::<f64>()
                    .map_err(|_| ConfctlError::Configuration("invalid target port Float".into()))?;
                if !value.is_finite() {
                    return Err(ConfctlError::Configuration(
                        "nonfinite target port Float cannot convert to Integer".into(),
                    ));
                }
                format!("{:.0}", value.trunc())
            } else {
                text
            }
        }
        _ => {
            return Err(ConfctlError::Configuration(
                "target port does not support to_i".into(),
            ));
        }
    };
    let text = text.trim_start_matches(['\t', '\n', '\x0b', '\x0c', '\r', ' ']);
    let negative = text.starts_with('-');
    let unsigned = text.strip_prefix(['+', '-']).unwrap_or(text);
    let bytes = unsigned.as_bytes();
    let mut digits = String::new();
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index].is_ascii_digit() {
            digits.push(bytes[index] as char);
            index += 1;
        } else if bytes[index] == b'_'
            && !digits.is_empty()
            && bytes.get(index + 1).is_some_and(u8::is_ascii_digit)
        {
            index += 1;
        } else {
            break;
        }
    }
    let digits = digits.trim_start_matches('0');
    if digits.is_empty() {
        Ok("0".into())
    } else {
        Ok(format!("{}{digits}", if negative { "-" } else { "" }))
    }
}

fn captured_bytes(captured: &Captured) -> io::Result<Vec<u8>> {
    match captured {
        Captured::Memory(bytes) => Ok(bytes.clone()),
        Captured::Spool { file, length } => {
            use io::Read;
            let mut bytes = Vec::new();
            file.reopen()?.take(*length).read_to_end(&mut bytes)?;
            if bytes.len() as u64 != *length {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "short connection probe capture",
                ));
            }
            Ok(bytes)
        }
        Captured::Unavailable => Err(io::Error::other(
            "connection probe did not capture its stream",
        )),
    }
}

fn decode_probe(outcome: ProcessOutcome) -> Result<Option<String>, ConfctlError> {
    let decode = || {
        if outcome.termination == TerminationCause::Canceled {
            return Err(ConfctlError::Cancelled);
        }
        let capture = || -> io::Result<Option<String>> {
            if !outcome.stdout_complete || !outcome.stderr_complete {
                return Err(io::Error::other("incomplete connection probe capture"));
            }
            let stdout = captured_bytes(&outcome.stdout)?;
            let stderr = captured_bytes(&outcome.stderr)?;
            if outcome.status.success() {
                return Ok(None);
            }
            let status = outcome
                .code()
                .map(|code| format!("exit status: {code}"))
                .unwrap_or_else(|| format!("signal: {}", outcome.signal().unwrap_or(0)));
            Ok(Some(format!(
                "cat /proc/uptime failed with {status}\n  stdout: {}\n  stderr: {}",
                String::from_utf8_lossy(&stdout),
                String::from_utf8_lossy(&stderr)
            )))
        };
        capture().map_err(|source| ConfctlError::Io {
            operation: "read connection probe capture",
            path: None,
            source,
        })
    };
    decode().map_err(|source| ConfctlError::ProcessFailure {
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

async fn emit(events: &EventSink, style: Style, text: String) -> Result<(), ConfctlError> {
    events
        .emit(UiEvent::Diagnostic {
            stream: OutputStream::Stdout,
            style,
            text,
        })
        .await
}

#[cfg(test)]
mod tests;
