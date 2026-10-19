//! Compile NixOS systemd property groups without owning execution or retries.

use crate::{domain::Machine, error::ConfctlError};
use serde::Deserialize;
use serde_json::Value;
use std::{collections::HashMap, ffi::OsString, time::Duration};

#[derive(Debug)]
pub(super) struct Check {
    pub(super) description: String,
    pub(super) timeout: Duration,
    pub(super) cooldown: Duration,
    unit: Option<String>,
    properties: Vec<Property>,
}

#[derive(Debug, Deserialize)]
struct Property {
    property: String,
    value: String,
    timeout: u64,
    cooldown: u64,
}

impl Check {
    /// Pass these native arguments through the existing machine command route.
    pub(super) fn argv(&self) -> Vec<OsString> {
        let mut argv = vec!["systemctl".into(), "show".into()];
        if let Some(unit) = &self.unit {
            argv.push(unit.into());
        }
        argv
    }

    /// Compare only complete stdout from a successful native command. Execution
    /// status, capture completeness and attempt scheduling belong to the caller.
    pub(super) fn errors(&self, stdout: &[u8]) -> Vec<String> {
        let mut observed = HashMap::new();
        for line in stdout.split_inclusive(|byte| *byte == b'\n') {
            let record = match line.strip_suffix(b"\n") {
                Some(record) => record.strip_suffix(b"\r").unwrap_or(record),
                None => line,
            };
            if let Some(equals) = record.iter().position(|byte| *byte == b'=') {
                observed.insert(&record[..equals], &record[equals + 1..]);
            }
        }
        self.properties
            .iter()
            .filter_map(
                |property| match observed.get(property.property.as_bytes()) {
                    None => Some(format!("property {:?} not found", property.property)),
                    Some(actual) if *actual != property.value.as_bytes() => Some(format!(
                        "{}={}, expected {}",
                        display_bytes(property.property.as_bytes()),
                        display_bytes(actual),
                        display_bytes(property.value.as_bytes())
                    )),
                    Some(_) => None,
                },
            )
            .collect()
    }
}

/// Only enabled NixOS groups apply. Empty system and unit groups produce no
/// checks; each remaining group uses its shortest timeout and cooldown.
pub(super) fn from_metadata(
    machine: &Machine,
    systemd_value: &Value,
) -> Result<Vec<Check>, ConfctlError> {
    if machine.spin() != Some("nixos") {
        return Ok(Vec::new());
    }
    match systemd_value.get("enable").and_then(Value::as_bool) {
        Some(false) => return Ok(Vec::new()),
        Some(true) => {}
        None => return Err(invalid(machine, "enable must be a boolean")),
    }
    let mut checks = Vec::new();
    let system = systemd_value
        .get("systemProperties")
        .ok_or_else(|| invalid(machine, "systemProperties must be a list"))?;
    if let Some(check) = group(machine, None, system)? {
        checks.push(check);
    }
    let units = systemd_value
        .get("unitProperties")
        .and_then(Value::as_object)
        .ok_or_else(|| invalid(machine, "unitProperties must be an object"))?;
    for (unit, properties) in units {
        if let Some(check) = group(machine, Some(unit), properties)? {
            checks.push(check);
        }
    }
    Ok(checks)
}

fn group(
    machine: &Machine,
    unit: Option<&str>,
    value: &Value,
) -> Result<Option<Check>, ConfctlError> {
    let field = unit.map_or_else(
        || "systemProperties".into(),
        |unit| format!("unitProperties[{unit:?}]"),
    );
    let entries = value
        .as_array()
        .ok_or_else(|| invalid(machine, &format!("{field} must be a list")))?;
    if entries.iter().any(|entry| !entry.is_object()) {
        return Err(invalid(
            machine,
            &format!("{field} entries must be objects"),
        ));
    }
    let properties: Vec<Property> = serde_json::from_value(value.clone())
        .map_err(|error| invalid(machine, &format!("{field}: {error}")))?;
    let Some(timeout) = properties.iter().map(|property| property.timeout).min() else {
        return Ok(None);
    };
    let cooldown = properties
        .iter()
        .map(|property| property.cooldown)
        .min()
        .expect("nonempty property group");
    let description = format!(
        "{}: {}",
        unit.unwrap_or("systemd"),
        properties
            .iter()
            .map(|property| format!("{}={}", property.property, property.value))
            .collect::<Vec<_>>()
            .join(", ")
    );
    Ok(Some(Check {
        description,
        timeout: Duration::from_secs(timeout),
        cooldown: Duration::from_secs(cooldown),
        unit: unit.map(str::to_owned),
        properties,
    }))
}

fn invalid(machine: &Machine, detail: &str) -> ConfctlError {
    ConfctlError::Configuration(format!(
        "invalid systemd health checks for {:?}: {detail}",
        machine.name
    ))
}

fn display_bytes(bytes: &[u8]) -> String {
    bytes
        .iter()
        .flat_map(|byte| std::ascii::escape_default(*byte))
        .map(char::from)
        .collect()
}

#[cfg(test)]
mod tests;
