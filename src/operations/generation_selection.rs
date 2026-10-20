//! Shared source observation and existing unified listing selection.
use super::generation_list::{input_revision, kernel_version};
use super::host_status::{HostGeneration, HostStatus};
use crate::{
    app::Context,
    configuration::ConfigRoot,
    domain::{
        GenerationLocations, Inventory, MachineId, UnifiedGenerationSelector, pattern_matches,
    },
    error::ConfctlError,
    events::{OutputStream, Style, TableFormat, TableLayout, TaskId, TaskState, UiEvent},
    generation::{BuildGeneration, GenerationInventory},
};
use chrono::{DateTime, Duration, FixedOffset, Utc};
use std::{
    future::{Future, poll_fn},
    path::Path,
    task::Poll,
};

pub(super) struct Sources {
    pub selected: Vec<MachineId>,
    pub local: Vec<(String, GenerationInventory)>,
    pub remote: Vec<(String, HostStatus)>,
}

pub(super) async fn read_sources(
    root: &ConfigRoot,
    inventory: &Inventory,
    selected: &[MachineId],
    locations: GenerationLocations,
    context: &Context,
    next_task: &mut u64,
) -> Result<Sources, ConfctlError> {
    let remote = if locations.remote {
        read_remote(inventory, selected, context, next_task).await?
    } else {
        Vec::new()
    };
    let mut local = Vec::new();
    for id in selected.iter().filter(|_| locations.local) {
        canceled(context)?;
        let host = &inventory.get(*id).name;
        let generations = GenerationInventory::load_local(root, host)?;
        for diagnostic in &generations.diagnostics {
            context
                .events
                .emit(UiEvent::Diagnostic {
                    stream: OutputStream::Stderr,
                    style: Style::Plain,
                    text: format!("{diagnostic}\n"),
                })
                .await?;
            canceled(context)?;
        }
        local.push((host.clone(), generations));
    }
    Ok(Sources {
        selected: selected.to_vec(),
        local,
        remote: selected
            .iter()
            .zip(remote)
            .map(|(id, status)| (inventory.get(*id).name.clone(), status))
            .collect(),
    })
}

pub(super) fn unified<'a>(
    sources: &'a Sources,
    selector: &UnifiedGenerationSelector,
    now: DateTime<Utc>,
) -> Result<Vec<Entry<'a>>, ConfctlError> {
    let mut entries = Vec::new();
    for (host, status) in &sources.remote {
        for generation in status.generations.iter().flatten() {
            add_remote(&mut entries, host, generation);
        }
    }
    for (host, inventory) in &sources.local {
        check_selection(host, inventory, selector)?;
        for generation in &inventory.generations {
            add_build(
                &mut entries,
                host,
                generation,
                inventory.current.as_deref() == Some(&generation.name),
                kernel_version(&generation.toplevel)?,
            );
        }
    }
    filter(&mut entries, selector, now);
    Ok(entries)
}

async fn read_remote(
    inventory: &Inventory,
    selected: &[crate::domain::MachineId],
    context: &Context,
    next_task: &mut u64,
) -> Result<Vec<super::host_status::HostStatus>, ConfctlError> {
    let cancellation = context.cancellation.child_token();
    let ssh_config = std::env::var_os("CONFCTL_SSH_CONFIG");
    // Resolve every carrier before starting any host observation.
    for id in selected {
        super::host_status::routing(inventory, inventory.get(*id))?;
    }
    let first_task = *next_task;
    *next_task = next_task
        .checked_add(selected.len() as u64)
        .ok_or_else(|| ConfctlError::State("generation task IDs overflow".into()))?;
    let mut queries = selected
        .iter()
        .enumerate()
        .map(|(index, id)| {
            let cancellation = &cancellation;
            let ssh_config = ssh_config.as_deref();
            Some(Box::pin(async move {
                let machine = inventory.get(*id);
                let task = TaskId(first_task + index as u64);
                let description = format!("query generations on {}", machine.name);
                let result = async {
                    context
                        .events
                        .emit(UiEvent::TaskPending {
                            task,
                            description: description.clone(),
                        })
                        .await?;
                    if cancellation.is_cancelled() {
                        return Err(ConfctlError::Cancelled);
                    }
                    context
                        .events
                        .emit(UiEvent::TaskStarted { task, description })
                        .await?;
                    if cancellation.is_cancelled() {
                        return Err(ConfctlError::Cancelled);
                    }
                    let status = super::host_status::query(
                        inventory,
                        machine,
                        &context.processes,
                        cancellation,
                        task,
                        ssh_config,
                        false,
                    )
                    .await?;
                    for diagnostic in &status.diagnostics {
                        context
                            .events
                            .emit(UiEvent::Diagnostic {
                                stream: OutputStream::Stderr,
                                style: Style::Plain,
                                text: format!("{diagnostic}\n"),
                            })
                            .await?;
                        if cancellation.is_cancelled() {
                            return Err(ConfctlError::Cancelled);
                        }
                    }
                    if let Some(failure) = &status.failure {
                        context
                            .events
                            .emit(UiEvent::Diagnostic {
                                stream: OutputStream::Stderr,
                                style: Style::Failure,
                                text: format!("{}\n", failure.diagnostic(&machine.name)),
                            })
                            .await?;
                        if cancellation.is_cancelled() {
                            return Err(ConfctlError::Cancelled);
                        }
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
                let delivery = context
                    .events
                    .emit(UiEvent::TaskFinished { task, state })
                    .await;
                match (result, delivery) {
                    (Err(error), Err(delivery)) => Err(error.with_secondary(vec![delivery])),
                    (_, Err(error)) => Err(error),
                    (Err(error), Ok(())) => Err(error),
                    (Ok(_), Ok(())) if cancellation.is_cancelled() => Err(ConfctlError::Cancelled),
                    (Ok(status), Ok(())) => Ok(status),
                }
            }))
        })
        .collect::<Vec<_>>();
    let mut statuses = std::iter::repeat_with(|| None)
        .take(queries.len())
        .collect::<Vec<_>>();
    let mut failures = Vec::new();
    poll_fn(|cx| {
        let mut pending = false;
        for (index, query) in queries.iter_mut().enumerate() {
            let Some(future) = query else {
                continue;
            };
            match future.as_mut().poll(cx) {
                Poll::Pending => pending = true,
                Poll::Ready(result) => {
                    *query = None;
                    match result {
                        Ok(status) => statuses[index] = Some(status),
                        Err(error) => {
                            failures.push(error);
                            cancellation.cancel();
                        }
                    }
                }
            }
        }
        if pending {
            Poll::Pending
        } else {
            Poll::Ready(())
        }
    })
    .await;
    if !failures.is_empty() {
        return Err(failures.remove(0).with_secondary(failures));
    }
    canceled(context)?;
    Ok(statuses
        .into_iter()
        .map(|s| s.expect("joined host observation"))
        .collect())
}

pub(super) fn canceled(context: &Context) -> Result<(), ConfctlError> {
    if context.cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}

pub(super) fn check_selection(
    host: &str,
    inventory: &GenerationInventory,
    selector: &UnifiedGenerationSelector,
) -> Result<(), ConfctlError> {
    use UnifiedGenerationSelector::*;
    let name = match selector {
        All => return Ok(()),
        Current | Old => {
            inventory.require_resolved_current(host)?;
            if matches!(selector, Current) {
                "current"
            } else {
                "old"
            }
        }
        Offset(name) => {
            if !inventory.rejected.is_empty() {
                return Err(ConfctlError::State(format!(
                    "Numeric generation selection for {host} is ambiguous because records were excluded; select an explicit supported generation name"
                )));
            }
            name
        }
        OlderThanDays(days) => return check_rejected(host, inventory, &format!("{days}d")),
        Pattern(name) => name,
    };
    check_rejected(host, inventory, name)
}

fn check_rejected(
    host: &str,
    inventory: &GenerationInventory,
    name: &str,
) -> Result<(), ConfctlError> {
    if let Some(rejected) = inventory.rejected.get(name) {
        return Err(ConfctlError::State(format!(
            "Cannot select generation {name} for {host}: {}",
            rejected.reason
        )));
    }
    Ok(())
}

pub(super) struct Entry<'a> {
    pub(super) host: &'a str,
    pub(super) name: String,
    pub(super) date: DateTime<FixedOffset>,
    pub(super) toplevel: &'a Path,
    pub(super) build: Option<(&'a BuildGeneration, bool)>,
    pub(super) remote: Option<&'a HostGeneration>,
    pub(super) current: bool,
    pub(super) kernel: Option<String>,
}

impl<'a> Entry<'a> {
    pub(super) fn local(
        host: &'a str,
        generation: &'a BuildGeneration,
        current: bool,
        kernel: Option<String>,
    ) -> Self {
        Self {
            host,
            name: generation.name.clone(),
            date: generation.date,
            toplevel: &generation.toplevel,
            build: Some((generation, current)),
            remote: None,
            current,
            kernel,
        }
    }
}

pub(super) fn add_remote<'a>(
    entries: &mut Vec<Entry<'a>>,
    host: &'a str,
    generation: &'a HostGeneration,
) {
    if let Some(entry) = entries.iter_mut().find(|entry| {
        entry.host == host && entry.toplevel.as_os_str() == generation.toplevel.as_os_str()
    }) {
        entry.remote = Some(generation);
        entry.current |= generation.current;
    } else {
        entries.push(Entry {
            host,
            name: generation.date.format("%Y-%m-%d--%H-%M-%S").to_string(),
            date: generation.date.fixed_offset(),
            toplevel: &generation.toplevel,
            build: None,
            remote: Some(generation),
            current: generation.current,
            kernel: generation.kernel.clone(),
        });
    }
}

pub(super) fn add_build<'a>(
    entries: &mut Vec<Entry<'a>>,
    host: &'a str,
    generation: &'a BuildGeneration,
    current: bool,
    kernel: Option<String>,
) {
    let existing = entries.iter_mut().find(|entry| {
        entry.host == host
            && entry.toplevel.as_os_str() == generation.toplevel.as_os_str()
            && entry.build.is_none_or(|(saved, _)| {
                saved.name == generation.name && saved.inputs == generation.inputs
            })
    });
    if let Some(entry) = existing {
        entry.name = generation.name.clone();
        entry.date = generation.date;
        entry.build = Some((generation, current));
        entry.current |= current;
    } else {
        entries.push(Entry::local(host, generation, current, kernel));
    }
}

pub(super) fn filter(
    entries: &mut Vec<Entry<'_>>,
    selector: &UnifiedGenerationSelector,
    now: DateTime<Utc>,
) {
    use UnifiedGenerationSelector::*;
    let offset = match selector {
        Offset(text) => offset_index(text, entries.len()),
        _ => None,
    };
    let cutoff = match selector {
        OlderThanDays(days) => {
            let digits = days.trim_start_matches('0');
            let days = if digits.is_empty() {
                Some(0)
            } else {
                digits.parse().ok()
            };
            days.and_then(Duration::try_days)
                .and_then(|duration| now.checked_sub_signed(duration))
        }
        _ => None,
    };
    let mut index = 0;
    entries.retain(|entry| {
        let keep = match selector {
            All => true,
            Current => entry.current,
            Old => !entry.current,
            Offset(text) => offset.map_or_else(
                || pattern_matches(text, &entry.name),
                |selected| index == selected,
            ),
            OlderThanDays(_) => cutoff.is_some_and(|cutoff| entry.date < cutoff),
            Pattern(pattern) => pattern_matches(pattern, &entry.name),
        };
        index += 1;
        keep
    });
}

// Keep the original spelling for pattern fallback; a bounded magnitude is
// enough to decide whether any integer, including a huge one, is in range.
pub(super) fn offset_index(text: &str, len: usize) -> Option<usize> {
    let (negative, digits) = text.strip_prefix('-').map_or((false, text), |s| (true, s));
    let digits = digits.trim_start_matches('0');
    let magnitude = if digits.is_empty() {
        0
    } else {
        digits.parse::<usize>().ok()?
    };
    let index = if magnitude == 0 {
        len.checked_sub(1)?
    } else if negative {
        len.checked_sub(magnitude)?.checked_sub(1)?
    } else {
        magnitude - 1
    };
    (index < len).then_some(index)
}

pub(super) fn table(entries: &[Entry<'_>]) -> UiEvent {
    let mut entries = entries.iter().collect::<Vec<_>>();
    let mut roles = Vec::new();
    for entry in &entries {
        for role in entry
            .build
            .into_iter()
            .flat_map(|(generation, _)| generation.inputs_info.keys())
        {
            if !roles.contains(role) {
                roles.push(role.clone());
            }
        }
    }
    entries.sort_by(|a, b| (&a.name, a.host).cmp(&(&b.name, b.host)));
    let mut columns = ["HOST", "NAME", "ID", "PRESENCE", "CURRENT", "KERNEL"]
        .map(str::to_owned)
        .to_vec();
    columns.extend(roles.iter().map(|role| role.to_uppercase()));
    let rows = entries
        .into_iter()
        .map(|entry| {
            let mut row = vec![
                entry.host.into(),
                entry.name.clone(),
                entry
                    .remote
                    .map(|generation| generation.id.clone())
                    .unwrap_or_default(),
                membership(entry.build.is_some(), entry.remote.is_some()).into(),
                membership(
                    entry.build.is_some_and(|(_, current)| current),
                    entry.remote.is_some_and(|generation| generation.current),
                )
                .into(),
                entry.kernel.clone().unwrap_or_default(),
            ];
            row.extend(roles.iter().map(|role| {
                input_revision(
                    entry
                        .build
                        .and_then(|(generation, _)| generation.inputs_info.get(role)),
                )
            }));
            row
        })
        .collect();
    UiEvent::Table {
        stream: OutputStream::Stdout,
        columns,
        rows,
        cell_styles: Vec::new(),
        format: TableFormat {
            layout: TableLayout::Columns,
            ..TableFormat::default()
        },
    }
}

pub(super) fn membership(build: bool, host: bool) -> &'static str {
    match (build, host) {
        (true, true) => "build+host",
        (true, false) => "build",
        (false, true) => "host",
        (false, false) => "",
    }
}
