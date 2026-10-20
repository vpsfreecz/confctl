//! One reboot request and serial uptime observations, without a remote retry.
use super::{check_cancel, checked, diagnostic, process_error};
use crate::{
    app::Context,
    domain::{Machine, WaitOnline},
    error::ConfctlError,
    events::{OutputStream, Style, TaskId, TaskState, UiEvent},
    operations::{connection, host_status},
    process::{
        Captured, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec, StreamPolicy,
        TerminationCause,
    },
    timing::Phase,
};
use std::{
    ffi::OsStr,
    future::Future,
    time::{Duration, Instant},
};
use tokio_util::sync::CancellationToken;

#[derive(Debug, PartialEq, Eq)]
pub(super) enum Outcome {
    Skipped,
    NoWait { native_success: bool },
    Online,
}

struct Budget<'a> {
    limit: Option<Duration>,
    label: &'a str,
}
impl<'a> Budget<'a> {
    fn new(wait: &'a WaitOnline) -> Result<Self, ConfctlError> {
        match wait {
            WaitOnline::Wait => Ok(Self {
                limit: None,
                label: "unbounded",
            }),
            WaitOnline::Seconds(text) => Ok(Self {
                // A larger valid decimal cannot expire within Instant's range.
                limit: text.parse::<u64>().ok().map(Duration::from_secs),
                label: text,
            }),
            _ => Err(ConfctlError::State(
                "reboot wait budget is unavailable".into(),
            )),
        }
    }
    fn expired(&self, elapsed: Duration) -> bool {
        self.limit.is_some_and(|limit| elapsed >= limit)
    }
    fn remaining(&self, elapsed: Duration) -> String {
        self.limit.map_or_else(
            || format!("{} budget", self.label),
            |limit| {
                format!(
                    "{:.1}s remaining",
                    limit.saturating_sub(elapsed).as_secs_f64()
                )
            },
        )
    }
}

pub(super) async fn run(
    ctx: &Context,
    machine: &Machine,
    ssh_config: Option<&OsStr>,
    task: TaskId,
    wait: &WaitOnline,
    interactive: bool,
) -> Result<Outcome, ConfctlError> {
    check_cancel(&ctx.cancellation)?;
    // Carried targets can have a null direct address; never resolve it here.
    let reason = if machine.carrier.is_some() {
        Some("carried machines are not rebooted")
    } else if connection::target_host(machine)? == "localhost" {
        Some("localhost is not rebooted")
    } else {
        None
    };
    if let Some(reason) = reason {
        checked(
            &ctx.events,
            &ctx.cancellation,
            diagnostic(&format!("{}: {reason}.\n", machine.name)),
        )
        .await?;
        return Ok(Outcome::Skipped);
    }
    if interactive {
        let accepted = loop {
            let line = ctx
                .ui
                .line(format!("Reboot {}? [y/N]: ", machine.name))
                .await?;
            check_cancel(&ctx.cancellation)?;
            let answer = line.trim_ascii().to_ascii_lowercase();
            match answer.as_slice() {
                b"y" => break true,
                b"" | b"n" => break false,
                _ => {}
            }
        };
        checked(&ctx.events, &ctx.cancellation, diagnostic("\n")).await?;
        if !accepted {
            checked(
                &ctx.events,
                &ctx.cancellation,
                diagnostic(&format!("{}: reboot declined.\n", machine.name)),
            )
            .await?;
            return Ok(Outcome::Skipped);
        }
    }
    let description = format!("reboot/wait on {}", machine.name);
    ctx.events
        .emit(UiEvent::TaskPending {
            task,
            description: description.clone(),
        })
        .await?;
    let result = async {
        checked(
            &ctx.events,
            &ctx.cancellation,
            UiEvent::LogAvailable {
                task,
                path: ctx.log_path().into(),
            },
        )
        .await?;
        checked(
            &ctx.events,
            &ctx.cancellation,
            UiEvent::TaskStarted { task, description },
        )
        .await?;
        let _phase = ctx.timing.phase(Phase::RebootWait);
        execute(ctx, machine, ssh_config, task, wait).await
    }
    .await;
    let state = match &result {
        Ok(Outcome::NoWait {
            native_success: false,
        }) => TaskState::Failed,
        Ok(_) => TaskState::Succeeded,
        Err(error) if error.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    };
    let delivery = ctx.events.emit(UiEvent::TaskFinished { task, state }).await;
    match (result, delivery) {
        (Err(error), Err(later)) => Err(error.with_secondary(vec![later])),
        (Err(error), _) | (_, Err(error)) => Err(error),
        (Ok(outcome), Ok(())) => {
            check_cancel(&ctx.cancellation)?;
            Ok(outcome)
        }
    }
}

fn spec(
    ctx: &Context,
    machine: &Machine,
    config: Option<&OsStr>,
    poll: bool,
) -> Result<ProcessSpec, ConfctlError> {
    let mut spec = if poll {
        connection::probe_spec(machine, config)?
    } else {
        connection::command_spec(machine, config, &["cat".into(), "/proc/uptime".into()])?
    };
    spec.cwd = Some(ctx.cwd.clone());
    Ok(spec)
}

fn valid_uptime(status: &host_status::HostStatus, host: &str) -> Result<f64, ConfctlError> {
    status
        .uptime
        .filter(|value| value.is_finite() && *value >= 0.0)
        .ok_or_else(|| {
            ConfctlError::State(format!("{host}: uptime must be finite and nonnegative"))
        })
}

async fn execute(
    ctx: &Context,
    machine: &Machine,
    config: Option<&OsStr>,
    task: TaskId,
    wait: &WaitOnline,
) -> Result<Outcome, ConfctlError> {
    let token = ctx.cancellation.child_token();
    let baseline = if *wait == WaitOnline::NoWait {
        None
    } else {
        let status = host_status::uptime_spec(
            &ctx.processes,
            &token,
            task,
            spec(ctx, machine, config, false)?,
        )
        .await?;
        if let Some(failure) = status.failure {
            return Err(process_error(
                ConfctlError::State(failure.diagnostic(&machine.name)),
                failure.outcome,
            ));
        }
        Some(valid_uptime(&status, &machine.name)?)
    };
    let start = Instant::now();
    let mut command = connection::command_spec(machine, config, &["reboot".into()])?;
    command.cwd = Some(ctx.cwd.clone());
    command.redacted_description = format!("reboot {}", machine.name);
    command.stdout = StreamPolicy::CaptureAndForward;
    command.stderr = StreamPolicy::CaptureAndForward;
    check_cancel(&ctx.cancellation)?;
    let outcome = ctx
        .processes
        .run(command, ProcessScope::new(token.clone(), task))
        .await?;
    let outcome = complete(outcome)?;
    let native_success = outcome.status.success();
    if !native_success {
        ctx.keep_log();
        let delivery = checked(&ctx.events, &ctx.cancellation, UiEvent::Diagnostic {
            stream: OutputStream::Stderr, style: Style::Failure,
            text: format!("{}: reboot command failed with {}; effect unknown, continuing the requested wait/health policy.\n", machine.name, outcome.status),
        }).await;
        if let Err(error) = delivery {
            return Err(process_error(error, outcome));
        }
    }
    check_cancel(&ctx.cancellation)?;
    let Some(baseline) = baseline else {
        checked(
            &ctx.events,
            &ctx.cancellation,
            diagnostic(&format!(
                "{}: reboot command completed with {}; online state unverified (nowait).\n",
                machine.name, outcome.status
            )),
        )
        .await
        .map_err(|error| process_error(error, outcome))?;
        return Ok(Outcome::NoWait { native_success });
    };
    let waiting = Waiting {
        ctx,
        machine,
        task,
        token: &token,
        start,
        budget: Budget::new(wait)?,
    };
    let result = waiting.poll(config, baseline).await;
    // Retain the original reboot failure as secondary evidence if waiting fails.
    match result {
        Err(error) if !native_success => Err(error.with_secondary(vec![process_error(
            ConfctlError::State("earlier reboot command failed; remote effect unknown".into()),
            outcome,
        )])),
        other => other,
    }
}

fn complete(outcome: ProcessOutcome) -> Result<ProcessOutcome, ConfctlError> {
    let error = if outcome.termination == TerminationCause::Canceled {
        Some(ConfctlError::Cancelled)
    } else if !outcome.stdout_complete
        || !outcome.stderr_complete
        || matches!(outcome.stdout, Captured::Unavailable)
        || matches!(outcome.stderr, Captured::Unavailable)
    {
        Some(ConfctlError::State("incomplete reboot capture".into()))
    } else {
        None
    };
    match error {
        Some(error) => Err(process_error(error, outcome)),
        None => Ok(outcome),
    }
}

struct Waiting<'a> {
    ctx: &'a Context,
    machine: &'a Machine,
    task: TaskId,
    token: &'a CancellationToken,
    start: Instant,
    budget: Budget<'a>,
}
impl Waiting<'_> {
    async fn progress(&self, state: &str) -> Result<(), ConfctlError> {
        let elapsed = self.start.elapsed();
        checked(
            &self.ctx.events,
            &self.ctx.cancellation,
            UiEvent::TaskProgress {
                task: self.task,
                completed: 0,
                total: None,
                detail: format!(
                    "{}: {state}; {:.1}s elapsed; {}",
                    self.machine.name,
                    elapsed.as_secs_f64(),
                    self.budget.remaining(elapsed)
                ),
            },
        )
        .await
    }
    async fn during<T>(
        &self,
        future: impl Future<Output = Result<T, ConfctlError>>,
    ) -> Result<T, ConfctlError> {
        tokio::pin!(future);
        let mut ticks = tokio::time::interval(Duration::from_millis(300));
        loop {
            tokio::select! {
                result = &mut future => return result,
                _ = ticks.tick() => {
                    if let Err(error) = self.progress("waiting for uptime observation").await {
                        self.token.cancel();
                        let joined = future.await;
                        return Err(error.with_secondary(joined.err().into_iter().collect()));
                    }
                }
            }
        }
    }
    async fn poll(&self, config: Option<&OsStr>, baseline: f64) -> Result<Outcome, ConfctlError> {
        let mut unavailable = false;
        loop {
            check_cancel(&self.ctx.cancellation)?;
            let status = self
                .during(host_status::uptime_spec(
                    &self.ctx.processes,
                    self.token,
                    self.task,
                    spec(self.ctx, self.machine, config, true)?,
                ))
                .await?;
            let state = if let Some(failure) = status.failure {
                self.ctx.keep_log();
                checked(
                    &self.ctx.events,
                    &self.ctx.cancellation,
                    UiEvent::Diagnostic {
                        stream: OutputStream::Stderr,
                        style: Style::Failure,
                        text: format!("{}\n", failure.diagnostic(&self.machine.name)),
                    },
                )
                .await
                .map_err(|error| process_error(error, failure.outcome))?;
                let state = if unavailable {
                    "still unavailable"
                } else {
                    "first unavailable observation"
                };
                unavailable = true;
                state
            } else {
                let uptime = valid_uptime(&status, &self.machine.name)?;
                if uptime < baseline {
                    self.progress("lower uptime observed").await?;
                    checked(&self.ctx.events, &self.ctx.cancellation, diagnostic(&format!("{}: lower uptime observed ({uptime} < {baseline}); command causality and target generation unverified.\n", self.machine.name))).await?;
                    return Ok(Outcome::Online);
                }
                "uptime unchanged or increasing"
            };
            self.progress(state).await?;
            checked(
                &self.ctx.events,
                &self.ctx.cancellation,
                diagnostic(&format!(
                    "{}: {state}; {:.1}s elapsed; {}.\n",
                    self.machine.name,
                    self.start.elapsed().as_secs_f64(),
                    self.budget.remaining(self.start.elapsed())
                )),
            )
            .await?;
            if self.budget.expired(self.start.elapsed()) {
                return Err(ConfctlError::State(format!(
                    "{}: timed out waiting for lower uptime; earlier reboot/profile effects may remain",
                    self.machine.name
                )));
            }
            tokio::select! {
                _ = self.ctx.cancellation.cancelled() => return Err(ConfctlError::Cancelled),
                _ = tokio::time::sleep(Duration::from_millis(300)) => {}
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::native_tool;
    #[tokio::test]
    async fn waiting_output_ack_failure_or_cancel_joins_the_entered_native_child() {
        use crate::{events::EventSink, process::Supervisor, terminal::Ui, timing::Timing};
        for cancel in [false, true] {
            let directory = tempfile::tempdir().unwrap();
            let pid_path = directory.path().join("pid");
            let (events, mut receiver) = EventSink::channel(1);
            let cancellation = CancellationToken::new();
            let ui = Ui::new(std::io::stdin(), events.clone(), cancellation.clone(), None).unwrap();
            let runner = Supervisor::new(
                events.clone(),
                directory.path().into(),
                64,
                Duration::from_millis(20),
                None,
                None,
            );
            let ctx = Context::for_test(
                directory.path().into(),
                cancellation.clone(),
                events,
                runner,
                ui,
                Timing::start(),
                directory.path().join("log"),
            );
            let machine = Machine::from_value(serde_json::json!({"name":"host"})).unwrap();
            let token = cancellation.child_token();
            let waiting = Waiting {
                ctx: &ctx,
                machine: &machine,
                task: TaskId(31),
                token: &token,
                start: Instant::now(),
                budget: Budget::new(&WaitOnline::Wait).unwrap(),
            };
            let mut spec = ProcessSpec::new("/bin/sh", "confined blocked uptime child");
            spec.args = vec![
                "-c".into(),
                "printf '%s' \"$$\" > \"$1\"; exec \"$2\" 60".into(),
                "fixture".into(),
                pid_path.as_os_str().into(),
                native_tool::resolve("sleep").into_os_string(),
            ];
            let entered_pid = std::cell::Cell::new(None);
            let consumer = async {
                let mut injected = false;
                while let Some(delivery) = receiver.receive().await {
                    let pid = std::fs::read_to_string(&pid_path)
                        .ok()
                        .and_then(|text| text.parse::<u32>().ok())
                        .filter(|pid| *pid > 0);
                    if !injected
                        && matches!(delivery.event, UiEvent::TaskProgress { .. })
                        && let Some(pid) = pid
                    {
                        injected = true;
                        entered_pid.set(Some(pid));
                        if cancel {
                            cancellation.cancel();
                            delivery.acknowledge(Ok(()));
                        } else {
                            delivery
                                .acknowledge(Err("reboot progress acknowledgement failed".into()));
                        }
                        break;
                    } else {
                        delivery.acknowledge(Ok(()));
                    }
                }
                assert!(injected);
            };
            let operation = async {
                let result = waiting
                    .during(
                        ctx.processes
                            .run(spec, ProcessScope::new(token.clone(), TaskId(31))),
                    )
                    .await;
                let error = result.unwrap_err();
                if cancel {
                    assert!(error.is_cancelled(), "{error}");
                } else {
                    assert!(
                        error
                            .to_string()
                            .contains("reboot progress acknowledgement failed"),
                        "{error}"
                    );
                }
                ctx.processes.shutdown().await.unwrap();
                ctx.ui.shutdown().unwrap();
            };
            tokio::join!(operation, consumer);
            assert!(
                !std::path::Path::new("/proc")
                    .join(entered_pid.get().unwrap().to_string())
                    .exists()
            );
            drop(ctx);
        }
    }

    #[test]
    fn budgets_preserve_zero_leading_zero_huge_and_unbounded() {
        for text in ["0", "0000"] {
            assert!(
                Budget::new(&WaitOnline::Seconds(text.into()))
                    .unwrap()
                    .expired(Duration::ZERO)
            );
        }
        let huge = WaitOnline::Seconds("9".repeat(100));
        assert!(
            !Budget::new(&huge)
                .unwrap()
                .expired(Duration::from_secs(u64::MAX))
        );
        assert!(
            !Budget::new(&WaitOnline::Wait)
                .unwrap()
                .expired(Duration::from_secs(u64::MAX))
        );
        for value in [f64::NAN, f64::INFINITY, -1.0] {
            assert!(
                valid_uptime(
                    &host_status::HostStatus {
                        uptime: Some(value),
                        ..Default::default()
                    },
                    "host"
                )
                .is_err()
            );
        }
    }
}
