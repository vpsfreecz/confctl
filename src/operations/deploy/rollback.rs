//! Caller of the existing watchdog protocol, not a witness of inner switch status.
use super::{Activation, Job, check_cancel, checked, diagnostic, process_error};
use crate::{
    domain::Machine,
    error::ConfctlError,
    events::TaskId,
    process::{
        Captured, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec, StreamPolicy,
        TerminationCause,
    },
    timing::Phase,
};
use std::{
    ffi::OsString,
    future::{Future, poll_fn},
    io::Read,
    path::{Path, PathBuf},
    task::Poll,
    time::{Duration, Instant},
};

struct Timeout {
    native: String,
    fallback_after: Option<Duration>,
    expired_before_start: bool,
}
impl Timeout {
    fn from_machine(machine: &Machine) -> Result<Self, ConfctlError> {
        let value = machine
            .metadata
            .get("autoRollback")
            .and_then(|v| v.get("timeout"));
        let native = value
            .and_then(|v| v.as_number())
            .map(ToString::to_string)
            .filter(|s| {
                s.strip_prefix('-')
                    .unwrap_or(s)
                    .bytes()
                    .all(|c| c.is_ascii_digit())
            })
            .ok_or_else(|| {
                ConfctlError::Configuration(format!(
                    "{}: autoRollback.timeout must be a signed integer",
                    machine.name
                ))
            })?;
        let magnitude = native.trim_start_matches('-').parse::<u64>().ok();
        let expired_before_start = native.starts_with('-') && magnitude.is_none_or(|n| n > 10);
        // JSON integers can exceed native widths. Such positive thresholds are
        // unreachable; huge negative timeouts already passed the fallback point.
        let seconds = if native.starts_with('-') {
            Some(magnitude.map_or(0, |n| 10u64.saturating_sub(n)))
        } else {
            magnitude.and_then(|n| n.checked_add(10))
        };
        Ok(Self {
            native,
            fallback_after: seconds.map(Duration::from_secs),
            expired_before_start,
        })
    }
    fn fallback(&self, elapsed: Duration) -> bool {
        self.expired_before_start
            || self
                .fallback_after
                .is_some_and(|threshold| elapsed > threshold)
    }
}

fn check_path() -> Result<PathBuf, ConfctlError> {
    let mut random = [0u8; 3];
    std::fs::File::open("/dev/urandom")
        .and_then(|mut file| file.read_exact(&mut random))
        .map_err(|source| ConfctlError::Io {
            operation: "read rollback confirmation randomness",
            path: None,
            source,
        })?;
    Ok(format!(
        "/run/confctl-confirm-{:02x}{:02x}{:02x}",
        random[0], random[1], random[2]
    )
    .into())
}

pub(super) async fn run<P: ProcessRunner>(
    activation: &Activation<'_, P>,
    job: &Job,
    task: TaskId,
    helper: &Path,
) -> Result<(), ConfctlError> {
    run_at(activation, job, task, helper, &check_path()?).await
}

fn spec<P>(
    activation: &Activation<'_, P>,
    job: &Job,
    argv: &[OsString],
    phase: &str,
) -> Result<ProcessSpec, ConfctlError> {
    let copies = activation.copies;
    let machine = copies.inventory.get(job.machine);
    let mut spec = super::super::connection::command_spec(machine, activation.ssh_config, argv)?;
    spec.cwd = Some(copies.root.path().into());
    spec.redacted_description = format!("{phase} on {}", machine.name);
    Ok(spec)
}
fn complete(outcome: ProcessOutcome, phase: &str) -> Result<ProcessOutcome, ConfctlError> {
    let error = if outcome.termination == TerminationCause::Canceled {
        Some(ConfctlError::Cancelled)
    } else if !outcome.stdout_complete
        || !outcome.stderr_complete
        || matches!(outcome.stdout, Captured::Unavailable)
        || matches!(outcome.stderr, Captured::Unavailable)
    {
        Some(ConfctlError::State(format!("incomplete {phase} capture")))
    } else {
        None
    };
    match error {
        Some(error) => Err(process_error(error, outcome)),
        None => Ok(outcome),
    }
}
fn native_failure(outcome: ProcessOutcome, phase: &str) -> ConfctlError {
    let error = ConfctlError::Child {
        description: phase.into(),
        code: outcome.code(),
        signal: outcome.signal(),
        log: None,
    };
    process_error(error, outcome)
}
fn helper_result(result: Result<ProcessOutcome, ConfctlError>) -> Result<(), ConfctlError> {
    let outcome = complete(result?, "rollback helper")?;
    if outcome.status.success() {
        Ok(())
    } else {
        Err(native_failure(outcome, "rollback helper"))
    }
}
fn marker(bytes: &[u8]) -> &[u8] {
    let strip = |byte: &u8| *byte == 0 || b" \t\r\n\x0b\x0c".contains(byte);
    let start = bytes.iter().position(|b| !strip(b)).unwrap_or(bytes.len());
    let end = bytes
        .iter()
        .rposition(|b| !strip(b))
        .map_or(start, |n| n + 1);
    &bytes[start..end]
}

async fn run_at<P: ProcessRunner>(
    activation: &Activation<'_, P>,
    job: &Job,
    task: TaskId,
    helper: &Path,
    path: &Path,
) -> Result<(), ConfctlError> {
    let copies = activation.copies;
    let machine = copies.inventory.get(job.machine);
    let timeout = Timeout::from_machine(machine)?;
    check_cancel(copies.cancellation)?;
    let token = copies.cancellation.child_token();
    let mut helper_spec = spec(
        activation,
        job,
        &[
            helper.as_os_str().into(),
            "-t".into(),
            timeout.native.clone().into(),
            job.generation.toplevel.as_os_str().into(),
            super::action_name(activation.action).into(),
            path.as_os_str().into(),
        ],
        "rollback-helper protocol",
    )?;
    helper_spec.stdout = StreamPolicy::CaptureAndForward;
    helper_spec.stderr = StreamPolicy::CaptureAndForward;
    let scope = || {
        let mut scope = ProcessScope::new(token.clone(), task);
        scope.phase = Some(Phase::Deployment);
        scope
    };
    let helper = copies.runner.run(helper_spec, scope());
    tokio::pin!(helper);
    let start = Instant::now();
    let mut helper_done = false;
    let mut errors = Vec::new();
    let mut failed_requests = Vec::new();
    let mut confirming = false;
    let mut confirmed = false;
    let mut attempts = 0;
    let mut exhausted = false;
    let quoted = super::super::connection::shell_quote(path.as_os_str());
    let result: Result<(), ConfctlError> = async {
        loop {
            check_cancel(&token)?;
            let phase = if confirming { "rollback confirmation" } else { "rollback marker read" };
            let mut script = OsString::from(if confirming { "echo confirmed > " } else { "cat " });
            script.push(&quoted);
            if !confirming { script.push(" 2>/dev/null"); }
            let control = copies.runner.run(spec(activation, job, &["sh".into(), "-c".into(), script], phase)?, scope());
            tokio::pin!(control);
            let mut entered = false;
            // Poll the helper before admitting a request. Once entered, always
            // settle that request, including when the helper finishes first.
            let outcome = poll_fn(|cx| {
                if !helper_done && let Poll::Ready(result) = helper.as_mut().poll(cx) {
                    helper_done = true;
                    if let Err(error) = helper_result(result) {
                        errors.push(error);
                        token.cancel();
                    }
                }
                if helper_done && !entered { return Poll::Ready(None); }
                entered = true;
                control.as_mut().poll(cx).map(Some)
            }).await;
            let Some(outcome) = outcome else { break };
            let outcome = complete(outcome?, phase)?;
            check_cancel(&token)?;
            let marker = if outcome.status.success() {
                if confirming {
                    confirmed = true;
                    break;
                }
                match crate::nix::bytes(&outcome.stdout).await {
                    Ok(bytes) if bytes.len() as u64 == outcome.stdout.len() => bytes,
                    Ok(_) => return Err(process_error(ConfctlError::State("rollback marker capture length changed".into()), outcome)),
                    Err(error) => return Err(process_error(error, outcome)),
                }
            } else {
                let error = native_failure(outcome, phase);
                let message = format!("{}: {error}\n", machine.name);
                failed_requests.push(error);
                checked(copies.events, &token, diagnostic(&message)).await?;
                Vec::new()
            };
            if confirming {
                attempts += 1;
                if attempts == 10 { exhausted = true; break; }
            } else {
                let marker = self::marker(&marker);
                if marker == b"switched" { confirming = true; }
                else if marker != b"switching" && timeout.fallback(start.elapsed()) {
                    checked(copies.events, &token, diagnostic(&format!(
                        "{}: switched marker was not observed; using the elapsed rollback confirmation fallback.\n", machine.name
                    ))).await?;
                    confirming = true;
                }
            }
            if helper_done { break; }
            if !confirming {
                tokio::select! {
                    biased;
                    result = &mut helper => {
                        helper_done = true;
                        if let Err(error) = helper_result(result) { errors.push(error); token.cancel(); }
                        break;
                    }
                    _ = token.cancelled() => return Err(ConfctlError::Cancelled),
                    _ = tokio::time::sleep(Duration::from_secs(1)) => {}
                }
            }
        }
        Ok(())
    }.await;
    if let Err(error) = result {
        errors.push(error);
        token.cancel();
    }
    if exhausted {
        // Do not cancel the helper on confirmation exhaustion: its watchdog
        // must be allowed to reach its natural result.
        errors.push(ConfctlError::State(format!(
            "{}: all ten rollback confirmation commands failed",
            machine.name
        )));
    }
    if !helper_done && let Err(error) = helper_result(helper.await) {
        errors.push(error);
    }
    if errors.is_empty() {
        check_cancel(copies.cancellation)?;
        if !confirmed {
            errors.push(ConfctlError::State(format!(
                "{}: rollback helper finished without successful confirmation",
                machine.name
            )));
        }
    }
    if !errors.is_empty() {
        errors.extend(failed_requests);
        return Err(errors.remove(0).with_secondary(errors));
    }
    checked(copies.events, copies.cancellation, diagnostic(&format!(
        "{}: rollback-helper protocol confirmed and helper exited 0; inner switch and rollback results are not witnessed.\n", machine.name
    ))).await
}

#[cfg(test)]
mod tests;
