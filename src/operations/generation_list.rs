use super::host_status::HostGeneration;
use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{
        Action, Inventory, Selection, SelectionPolicy, UnifiedGenerationSelector, pattern_matches,
        ruby_string, select,
    },
    error::ConfctlError,
    events::{OutputStream, Style, TableFormat, TableLayout, TaskId, TaskState, UiEvent},
    generation::{BuildGeneration, GenerationInventory},
    nix::{InvocationSettings, NixClient, NixOptions},
};
use chrono::{DateTime, Duration, FixedOffset, Utc};
use serde_json::Value;
use std::{
    fs,
    future::{Future, poll_fn},
    path::Path,
    task::Poll,
};

pub(super) async fn list(
    action: &Action,
    root: ConfigRoot,
    context: &Context,
) -> Result<(), ConfctlError> {
    let Action::GenerationList {
        selection,
        locations,
        selector,
    } = action
    else {
        unreachable!("generation listing adapter receives its typed action")
    };
    let now = Utc::now();
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
    let selected = select(inventory, selection, SelectionPolicy::ALL)?;
    let remote = if locations.remote {
        read_remote(inventory, &selected, context).await?
    } else {
        Vec::new()
    };
    let mut hosts = Vec::new();
    for id in selected.machines.iter().filter(|_| locations.local) {
        canceled(context)?;
        let host = &inventory.get(*id).name;
        let generations = GenerationInventory::load_local(&root, host)?;
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
        check_selection(host, &generations, selector)?;
        hosts.push((host.as_str(), generations));
    }
    let mut entries = Vec::new();
    for (id, status) in selected.machines.iter().zip(&remote) {
        for generation in status.generations.iter().flatten() {
            add_remote(&mut entries, &inventory.get(*id).name, generation);
        }
    }
    for (host, inventory) in &hosts {
        for generation in &inventory.generations {
            canceled(context)?;
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
    canceled(context)?;
    context.events.emit(table(entries)).await?;
    canceled(context)
}

async fn read_remote(
    inventory: &Inventory,
    selected: &Selection,
    context: &Context,
) -> Result<Vec<super::host_status::HostStatus>, ConfctlError> {
    let cancellation = context.cancellation.child_token();
    let ssh_config = std::env::var_os("CONFCTL_SSH_CONFIG");
    // Resolve every carrier before starting any host observation.
    for id in &selected.machines {
        super::host_status::routing(inventory, inventory.get(*id))?;
    }
    let mut queries = selected
        .machines
        .iter()
        .enumerate()
        .map(|(index, id)| {
            let cancellation = &cancellation;
            let ssh_config = ssh_config.as_deref();
            Some(Box::pin(async move {
                let machine = inventory.get(*id);
                let task = TaskId(index as u64 + 1);
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

fn canceled(context: &Context) -> Result<(), ConfctlError> {
    if context.cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}

fn check_selection(
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

struct Entry<'a> {
    host: &'a str,
    name: String,
    date: DateTime<FixedOffset>,
    toplevel: &'a Path,
    build: Option<(&'a BuildGeneration, bool)>,
    remote: Option<&'a HostGeneration>,
    current: bool,
    kernel: Option<String>,
}

impl<'a> Entry<'a> {
    fn local(
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

fn add_remote<'a>(entries: &mut Vec<Entry<'a>>, host: &'a str, generation: &'a HostGeneration) {
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

fn add_build<'a>(
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

fn filter(entries: &mut Vec<Entry<'_>>, selector: &UnifiedGenerationSelector, now: DateTime<Utc>) {
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
fn offset_index(text: &str, len: usize) -> Option<usize> {
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

fn table(mut entries: Vec<Entry<'_>>) -> UiEvent {
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
                entry.name,
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
                entry.kernel.unwrap_or_default(),
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

fn membership(build: bool, host: bool) -> &'static str {
    match (build, host) {
        (true, true) => "build+host",
        (true, false) => "build",
        (false, true) => "host",
        (false, false) => "",
    }
}

pub(super) fn input_revision(value: Option<&Value>) -> String {
    let (short, rev) = match value {
        Some(Value::String(rev)) => (None, Some(Value::String(rev.clone()))),
        Some(Value::Object(info)) => {
            // InputsInfo normalizes aliases in insertion order, including null.
            let short = info
                .iter()
                .filter(|(key, _)| matches!(key.as_str(), "shortRev" | "short_rev"))
                .map(|(_, value)| value.clone())
                .next_back();
            (short, info.get("rev").cloned())
        }
        _ => return String::new(),
    };
    if let Some(value) = short.filter(|value| !matches!(value, Value::Null | Value::Bool(false))) {
        return ruby_string(&value);
    }
    match rev {
        Some(Value::String(rev)) => rev.chars().take(8).collect(),
        _ => String::new(),
    }
}

pub(super) fn kernel_version(toplevel: &Path) -> Result<Option<String>, ConfctlError> {
    for name in ["kernel", "bzImage"] {
        let path = toplevel.join(name);
        let link = match fs::read_link(&path) {
            Ok(link) => link,
            Err(error)
                if matches!(
                    error.kind(),
                    std::io::ErrorKind::NotFound | std::io::ErrorKind::InvalidInput
                ) =>
            {
                continue;
            }
            Err(source) => {
                return Err(ConfctlError::Io {
                    operation: "read generation kernel link",
                    path: Some(path),
                    source,
                });
            }
        };
        if let Some(version) = link.to_str().and_then(super::host_status::kernel_version) {
            return Ok(Some(version));
        }
    }
    Ok(None)
}

#[cfg(test)]
mod tests;
