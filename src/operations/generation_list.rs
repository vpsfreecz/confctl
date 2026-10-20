use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{Action, SelectionPolicy, ruby_string, select},
    error::ConfctlError,
    nix::{InvocationSettings, NixClient, NixOptions},
};
use chrono::Utc;
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
    let sources = super::generation_selection::read_sources(
        &root,
        inventory,
        &selected.machines,
        *locations,
        context,
        &mut 1,
    )
    .await?;
    let entries = super::generation_selection::unified(&sources, selector, now)?;
    canceled(context)?;
    context
        .events
        .emit(super::generation_selection::table(&entries))
        .await?;
    canceled(context)
}

fn canceled(context: &Context) -> Result<(), ConfctlError> {
    super::generation_selection::canceled(context)
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
