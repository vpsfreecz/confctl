use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{
        Action, SelectionPolicy, UnifiedGenerationSelector, pattern_matches, ruby_string, select,
    },
    error::ConfctlError,
    events::{OutputStream, Style, TableFormat, TableLayout, UiEvent},
    generation::{BuildGeneration, GenerationInventory},
    nix::{InvocationSettings, NixClient, NixOptions},
};
use chrono::{DateTime, Duration, Utc};
use serde_json::Value;
use std::{fs, path::Path};

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
    // CoreOperations rejects remote requests before application setup as well.
    if locations.remote {
        return Err(ConfctlError::Configuration(
            "Remote generation listing is not implemented yet".into(),
        ));
    }
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
    let mut hosts = Vec::new();
    for id in selected.machines {
        canceled(context)?;
        let host = &inventory.get(id).name;
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
        }
        check_selection(host, &generations, selector)?;
        hosts.push((host.as_str(), generations));
    }
    let mut entries = Vec::new();
    for (host, inventory) in &hosts {
        for generation in &inventory.generations {
            canceled(context)?;
            entries.push(Entry {
                host,
                generation,
                current: inventory.current.as_deref() == Some(&generation.name),
                kernel: kernel_version(&generation.toplevel)?,
            });
        }
    }
    filter(&mut entries, selector, now);
    canceled(context)?;
    context.events.emit(table(entries)).await?;
    canceled(context)
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
    generation: &'a BuildGeneration,
    current: bool,
    kernel: Option<String>,
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
                || pattern_matches(text, &entry.generation.name),
                |selected| index == selected,
            ),
            OlderThanDays(_) => cutoff.is_some_and(|cutoff| entry.generation.date < cutoff),
            Pattern(pattern) => pattern_matches(pattern, &entry.generation.name),
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
        for role in entry.generation.inputs_info.keys() {
            if !roles.contains(role) {
                roles.push(role.clone());
            }
        }
    }
    entries.sort_by(|a, b| (&a.generation.name, a.host).cmp(&(&b.generation.name, b.host)));
    let mut columns = ["HOST", "NAME", "ID", "PRESENCE", "CURRENT", "KERNEL"]
        .map(str::to_owned)
        .to_vec();
    columns.extend(roles.iter().map(|role| role.to_uppercase()));
    let rows = entries
        .into_iter()
        .map(|entry| {
            let mut row = vec![
                entry.host.into(),
                entry.generation.name.clone(),
                String::new(),
                "build".into(),
                if entry.current {
                    "build".into()
                } else {
                    String::new()
                },
                entry.kernel.unwrap_or_default(),
            ];
            row.extend(
                roles
                    .iter()
                    .map(|role| input_revision(entry.generation.inputs_info.get(role))),
            );
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
        if let Some(target) = link.to_str().and_then(|s| s.strip_prefix("/nix/store/"))
            && let Some((hash, rest)) = target.split_once('-')
            && !hash.is_empty()
            && !hash.contains('/')
            && let Some(version) = rest.strip_prefix("linux-")
            && let Some(version) = version.split('/').next().filter(|s| !s.is_empty())
        {
            return Ok(Some(version.into()));
        }
    }
    Ok(None)
}

#[cfg(test)]
mod tests;
