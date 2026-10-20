//! Deployment's health choices borrow the single configured-check runner.
use super::{check_cancel, checked, diagnostic};
use crate::{
    app::Context,
    configuration::ConfigRoot,
    domain::{DeployRequest, Inventory, MachineId},
    error::ConfctlError,
    events::TaskId,
    operations::health_check::{CheckContext, from_metadata, run_check_refs},
    terminal::ConfirmPrompt,
};

pub(super) enum Outcome {
    Passed,
    Declined,
    Continued,
    Empty,
    Disabled,
}
#[derive(Debug, PartialEq, Eq)]
enum Choice {
    Continue,
    All,
    Failed,
    Abort,
}
fn choice(line: &[u8]) -> Option<Choice> {
    match line.trim_ascii().to_ascii_lowercase().as_slice() {
        b"c" => Some(Choice::Continue),
        b"r" => Some(Choice::All),
        b"f" => Some(Choice::Failed),
        b"" | b"a" => Some(Choice::Abort),
        _ => None,
    }
}
pub(super) fn summary(outcomes: &[Outcome]) -> String {
    let count = |kind: fn(&Outcome) -> bool| outcomes.iter().filter(|v| kind(v)).count();
    format!(
        "Health rounds: {} passed, {} declined/unverified, {} continued with failed checks, {} empty or disabled.",
        count(|v| matches!(v, Outcome::Passed)),
        count(|v| matches!(v, Outcome::Declined)),
        count(|v| matches!(v, Outcome::Continued)),
        count(|v| matches!(v, Outcome::Empty | Outcome::Disabled))
    )
}

pub(super) async fn run(
    ctx: &Context,
    request: &DeployRequest,
    inventory: &Inventory,
    root: &ConfigRoot,
    hosts: &[MachineId],
    next_id: &mut u64,
) -> Result<Outcome, ConfctlError> {
    if !request.health_checks {
        return Ok(Outcome::Disabled);
    }
    let mut all = Vec::new();
    for id in hosts {
        check_cancel(&ctx.cancellation)?;
        let machine = inventory.get(*id);
        if machine.runnable() {
            all.extend(from_metadata(machine)?);
        }
    }
    if all.is_empty() {
        return Ok(Outcome::Empty);
    }
    if request.interactive {
        let accepted = ctx
            .ui
            .confirm(ConfirmPrompt {
                always: true,
                preview: vec![diagnostic(&format!(
                    "Run {} configured health checks for completed activation actions.\n",
                    all.len()
                ))],
                ..ConfirmPrompt::default()
            })
            .await?;
        check_cancel(&ctx.cancellation)?;
        if !accepted {
            checked(
                &ctx.events,
                &ctx.cancellation,
                diagnostic("Health checks declined; this deployment round is unverified.\n"),
            )
            .await?;
            return Ok(Outcome::Declined);
        }
    }
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
    let mut active = all.iter().collect::<Vec<_>>();
    loop {
        check_cancel(&ctx.cancellation)?;
        let first = TaskId(*next_id);
        *next_id = next_id
            .checked_add(
                u64::try_from(active.len())
                    .map_err(|_| ConfctlError::State("Health task IDs overflow".into()))?,
            )
            .ok_or_else(|| ConfctlError::State("Health task IDs overflow".into()))?;
        let failures = run_check_refs(&execution, &active, 5, first).await?;
        checked(
            &ctx.events,
            &ctx.cancellation,
            diagnostic(&format!(
                "{} checks passed, {} failed in deployment health batch.\n",
                active.len() - failures.len(),
                failures.len()
            )),
        )
        .await?;
        for failure in &failures {
            let check = active[failure.index];
            checked(
                &ctx.events,
                &ctx.cancellation,
                diagnostic(&format!(
                    "{}: {}: {}\n",
                    check.host_name(),
                    check.description(),
                    failure.messages.join("; ")
                )),
            )
            .await?;
        }
        if failures.is_empty() {
            return Ok(Outcome::Passed);
        }
        let selected = if request.interactive {
            loop {
                let line = ctx.ui.line("Health checks failed. [c] Continue, [r] Retry all, [f] Retry failed, [a] Abort (default): ".into()).await?;
                check_cancel(&ctx.cancellation)?;
                if let Some(choice) = choice(&line) {
                    break choice;
                }
            }
        } else if request.keep_going {
            Choice::Continue
        } else {
            Choice::Abort
        };
        match selected {
            Choice::Continue => {
                checked(&ctx.events, &ctx.cancellation, diagnostic("Continuing with failed health checks explicitly accepted; failed tasks and raw logs are retained.\n")).await?;
                return Ok(Outcome::Continued);
            }
            Choice::All => active = all.iter().collect(),
            Choice::Failed => {
                active = failures
                    .iter()
                    .map(|failure| active[failure.index])
                    .collect()
            }
            Choice::Abort => {
                return Err(ConfctlError::Health(
                    "Health checks failed; deployment stopped".into(),
                ));
            }
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn health_choices_default_to_abort_and_invalid_input_repeats() {
        assert_eq!(choice(b"\r\n"), Some(Choice::Abort));
        assert_eq!(choice(b""), Some(Choice::Abort));
        for (text, expected) in [
            (b"C".as_slice(), Choice::Continue),
            (b" r ", Choice::All),
            (b"f", Choice::Failed),
            (b"a", Choice::Abort),
        ] {
            assert_eq!(choice(text), Some(expected));
        }
        for text in [b"yes".as_slice(), b"?", b"\xff"] {
            assert_eq!(choice(text), None);
        }
    }
}
