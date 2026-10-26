//! Deployment's health choices borrow the single configured-check runner.
use super::{check_cancel, checked, diagnostic_style};
use crate::{
    app::Context,
    configuration::ConfigRoot,
    domain::{DeployRequest, Inventory, MachineId},
    error::ConfctlError,
    events::{Style, TaskId, TaskState},
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
/// Final check qualification plus every attempt's task association for one host.
pub(super) struct HostResult {
    pub machine: MachineId,
    pub tasks: Vec<TaskId>,
    pub passed: usize,
    pub failed: usize,
    pub pending: usize,
}
impl HostResult {
    pub fn state(&self) -> TaskState {
        if self.failed > 0 {
            TaskState::Failed
        } else if self.pending > 0 || self.passed == 0 {
            TaskState::Incomplete
        } else {
            TaskState::Succeeded
        }
    }
    pub fn label(&self) -> String {
        format!(
            "health: {} passed, {} failed, {} unverified",
            self.passed, self.failed, self.pending
        )
    }
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

pub(super) struct Round {
    pub outcome: Result<Outcome, ConfctlError>,
    pub hosts: Vec<HostResult>,
}

pub(super) async fn run(
    ctx: &Context,
    request: &DeployRequest,
    inventory: &Inventory,
    root: &ConfigRoot,
    hosts: &[MachineId],
    next_id: &mut u64,
) -> Round {
    let mut details = Vec::new();
    let outcome = async {
    details.extend(hosts.iter().map(|machine| HostResult {
        machine: *machine,
        tasks: Vec::new(),
        passed: 0,
        failed: 0,
        pending: 0,
    }));
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
    for detail in details.iter_mut() {
        detail.pending = all
            .iter()
            .filter(|check| check.host_name() == inventory.get(detail.machine).name)
            .count();
    }
    if all.is_empty() {
        return Ok(Outcome::Empty);
    }
    if request.interactive {
        let accepted = ctx
            .ui
            .confirm(ConfirmPrompt {
                always: true,
                preview: vec![diagnostic_style(Style::Machine, &format!(
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
                diagnostic_style(Style::Incomplete, "Health checks declined; this deployment round is unverified.\n"),
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
    let mut final_results = vec![None; all.len()];
    let mut indices = (0..all.len()).collect::<Vec<_>>();
    loop {
        check_cancel(&ctx.cancellation)?;
        let active = indices.iter().map(|index| &all[*index]).collect::<Vec<_>>();
        let first = TaskId(*next_id);
        *next_id = next_id
            .checked_add(
                u64::try_from(active.len())
                    .map_err(|_| ConfctlError::State("Health task IDs overflow".into()))?,
            )
            .ok_or_else(|| ConfctlError::State("Health task IDs overflow".into()))?;
        // Retrying all invalidates earlier passes until this attempt completes.
        for index in &indices {
            final_results[*index] = None;
        }
        update_details(&mut details, &all, &final_results, inventory);
        let failures = run_check_refs(&execution, &active, 5, first).await?;
        for (offset, index) in indices.iter().enumerate() {
            let detail = details
                .iter_mut()
                .find(|detail| inventory.get(detail.machine).name == all[*index].host_name())
                .expect("selected health host");
            detail.tasks.push(TaskId(first.0 + offset as u64));
        }
        for (offset, index) in indices.iter().enumerate() {
            final_results[*index] = Some(!failures.iter().any(|failure| failure.index == offset));
        }
        update_details(&mut details, &all, &final_results, inventory);
        checked(
            &ctx.events,
            &ctx.cancellation,
            diagnostic_style(if failures.is_empty() { Style::Success } else { Style::Failure }, &format!(
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
                diagnostic_style(Style::Failure, &format!(
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
                checked(&ctx.events, &ctx.cancellation, diagnostic_style(Style::Incomplete, "Continuing with failed health checks explicitly accepted; failed tasks and raw logs are retained.\n")).await?;
                return Ok(Outcome::Continued);
            }
            Choice::All => indices = (0..all.len()).collect(),
            Choice::Failed => {
                indices = failures
                    .iter()
                    .map(|failure| indices[failure.index])
                    .collect();
            }
            Choice::Abort => {
                return Err(ConfctlError::Health(
                    "Health checks failed; deployment stopped".into(),
                ));
            }
        }
    }
    }.await;
    Round {
        outcome,
        hosts: details,
    }
}
fn update_details(
    details: &mut [HostResult],
    checks: &[crate::operations::health_check::Check],
    results: &[Option<bool>],
    inventory: &Inventory,
) {
    for detail in details {
        detail.passed = 0;
        detail.failed = 0;
        detail.pending = 0;
        for (check, result) in checks.iter().zip(results) {
            if check.host_name() == inventory.get(detail.machine).name {
                match result {
                    Some(true) => detail.passed += 1,
                    Some(false) => detail.failed += 1,
                    None => detail.pending += 1,
                }
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
    #[test]
    fn health_final_results_replace_retry_all_but_preserve_other_failed_retry_passes() {
        let inventory = Inventory::from_value(serde_json::json!({
            "a":{"name":"a","metaConfig":{"healthChecks":{"builderCommands":[{"command":["true"],"timeout":0,"cooldown":0}]}}},
            "b":{"name":"b","metaConfig":{"healthChecks":{"builderCommands":[{"command":["true"],"timeout":0,"cooldown":0}]}}}
        })).unwrap();
        let checks = inventory
            .machines
            .iter()
            .flat_map(|machine| from_metadata(machine).unwrap())
            .collect::<Vec<_>>();
        assert_eq!(checks.len(), 2);
        let mut details = (0..2)
            .map(|id| HostResult {
                machine: MachineId(id),
                tasks: vec![TaskId(id as u64 + 1)],
                passed: 0,
                failed: 0,
                pending: 1,
            })
            .collect::<Vec<_>>();
        update_details(
            &mut details,
            &checks,
            &[Some(true), Some(false)],
            &inventory,
        );
        assert_eq!(details[0].state(), TaskState::Succeeded);
        assert_eq!(details[1].state(), TaskState::Failed);
        // Retry-failed retains the successful host while the failed host is pending.
        update_details(&mut details, &checks, &[Some(true), None], &inventory);
        assert_eq!(details[0].state(), TaskState::Succeeded);
        assert_eq!(details[1].state(), TaskState::Incomplete);
        update_details(&mut details, &checks, &[None, None], &inventory);
        assert!(
            details
                .iter()
                .all(|detail| detail.state() == TaskState::Incomplete),
            "retry-all cannot reuse earlier green state"
        );
        update_details(
            &mut details,
            &checks,
            &[Some(false), Some(true)],
            &inventory,
        );
        assert_eq!(details[0].state(), TaskState::Failed);
        assert_eq!(details[1].state(), TaskState::Succeeded);
        assert_eq!(
            details[0].tasks,
            [TaskId(1)],
            "summarizing never drops prior attempt associations"
        );
    }
}
