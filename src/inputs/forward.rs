//! Complete logical-graph policy. Transport evidence is supplied by the shared mirror.
use super::{FlakeLock, InputChange, LockGraph};
use crate::error::ConfctlError;
use serde_json::{Map, Value};
use std::collections::{BTreeMap, BTreeSet};
use std::future::Future;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Ancestry {
    Ahead,
    Behind,
    Diverged,
}
#[derive(Clone, Debug)]
pub struct GitComparison {
    pub owner: String,
    pub repo: String,
    pub old_rev: String,
    pub new_rev: String,
}
pub fn full_sha(value: &str) -> bool {
    value.len() == 40
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
fn name(value: &str) -> bool {
    !value.is_empty()
        && value != "."
        && value != ".."
        && value
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"_.-".contains(&c))
}
fn source(node: &Value) -> Result<Map<String, Value>, &'static str> {
    let locked = node["locked"].as_object().expect("validated graph node");
    if locked.get("type").and_then(Value::as_str) != Some("github")
        || !matches!(locked.get("host"), None | Some(Value::Null))
            && locked.get("host").and_then(Value::as_str) != Some("github.com")
        || !["owner", "repo"]
            .iter()
            .all(|k| locked.get(*k).and_then(Value::as_str).is_some_and(name))
    {
        return Err("Changed dependency is not a supported github.com source");
    }
    if !locked
        .get("rev")
        .and_then(Value::as_str)
        .is_some_and(full_sha)
    {
        return Err("Changed dependency lacks a full immutable Git revision");
    }
    if locked.keys().any(|k| {
        ![
            "type",
            "owner",
            "repo",
            "host",
            "rev",
            "narHash",
            "lastModified",
            "dir",
        ]
        .contains(&k.as_str())
    }) || ["dir", "narHash"]
        .iter()
        .any(|k| locked.get(*k).is_some_and(|v| !v.is_string()))
        || locked
            .get("lastModified")
            .is_some_and(|v| !v.is_i64() && !v.is_u64())
    {
        return Err("Unsupported GitHub source attributes");
    }
    let mut identity = locked.clone();
    for key in ["rev", "narHash", "lastModified"] {
        identity.shift_remove(key);
    }
    for key in ["owner", "repo"] {
        identity.insert(
            key.into(),
            Value::String(locked[key].as_str().unwrap().to_ascii_lowercase()),
        );
    }
    identity.insert("host".into(), Value::String("github.com".into()));
    identity.sort_keys();
    Ok(identity)
}
fn flake(node: &Value) -> bool {
    node.get("flake").and_then(Value::as_bool).unwrap_or(true)
}

pub async fn validate_forward<F, Fut>(
    old: &FlakeLock,
    new: &FlakeLock,
    mut compare: F,
) -> Result<Vec<InputChange>, ConfctlError>
where
    F: FnMut(GitComparison) -> Fut,
    Fut: Future<Output = Result<Ancestry, ConfctlError>>,
{
    let before = LockGraph::new(old)?;
    let after = LockGraph::new(new)?;
    let paths: BTreeSet<_> = before.entries.keys().chain(after.entries.keys()).collect();
    let mut changes = Vec::new();
    let mut rejected = Vec::new();
    let mut errors = Vec::new();
    // Cache ownership indices, not Display strings: aliases share one typed
    // failure until the app's existing evidence finalization consumes it.
    let mut failures = Vec::new();
    let mut cache: BTreeMap<(String, String, String), Result<Ancestry, usize>> = BTreeMap::new();
    for path in paths {
        let previous = before.entries.get(path).map(|e| e.node);
        let candidate = after.entries.get(path).map(|e| e.node);
        if let (Some(a), Some(b)) = (previous, candidate)
            && a["locked"] == b["locked"]
            && flake(a) == flake(b)
        {
            continue;
        }
        changes.push(InputChange {
            name: path.clone(),
            old_info: previous.map(|n| FlakeLock::node_info(Some(n))),
            new_info: FlakeLock::node_info(candidate),
        });
        let Some(candidate) = candidate else {
            rejected.push(format!(
                "{path}: previously reachable dependency was removed"
            ));
            continue;
        };
        let next_source = match source(candidate) {
            Ok(s) => s,
            Err(e) => {
                errors.push(format!("{path}: {e}"));
                continue;
            }
        };
        let Some(previous) = previous else {
            continue;
        };
        let old_source = match source(previous) {
            Ok(s) => s,
            Err(e) => {
                errors.push(format!("{path}: {e}"));
                continue;
            }
        };
        if old_source != next_source || flake(previous) != flake(candidate) {
            rejected.push(format!(
                "{path}: source or extraction attributes changed; manual review required"
            ));
            continue;
        }
        let old_rev = previous["locked"]["rev"].as_str().unwrap();
        let new_rev = candidate["locked"]["rev"].as_str().unwrap();
        if old_rev == new_rev {
            rejected.push(format!(
                "{path}: locked attributes changed at the same revision"
            ));
            continue;
        }
        let key = (
            serde_json::to_string(&old_source).expect("JSON identity"),
            old_rev.to_owned(),
            new_rev.to_owned(),
        );
        let outcome = if let Some(cached) = cache.get(&key) {
            *cached
        } else {
            let result = compare(GitComparison {
                owner: candidate["locked"]["owner"].as_str().unwrap().into(),
                repo: candidate["locked"]["repo"].as_str().unwrap().into(),
                old_rev: old_rev.into(),
                new_rev: new_rev.into(),
            })
            .await;
            let result = match result {
                Ok(v) => Ok(v),
                Err(e) if e.is_cancelled() => return Err(e.with_secondary(failures)),
                Err(e) => {
                    let index = failures.len();
                    failures.push(e);
                    Err(index)
                }
            };
            cache.insert(key, result);
            result
        };
        match outcome {
            Ok(Ancestry::Ahead) => {}
            Ok(direction) => rejected.push(format!(
                "{path}: {old_rev} -> {new_rev} is {}",
                if direction == Ancestry::Behind {
                    "behind"
                } else {
                    "diverged"
                }
            )),
            Err(index) => errors.push(format!("{path}: {}", failures[index])),
        }
    }
    if !errors.is_empty() {
        errors.extend(rejected);
        Err(ConfctlError::Configuration(format!(
            "Forward-only update could not be verified:\n{}",
            errors.join("\n")
        ))
        .with_secondary(failures))
    } else if !rejected.is_empty() {
        Err(ConfctlError::InputPolicyRejected(format!(
            "Forward-only update rejected:\n{}",
            rejected.join("\n")
        )))
    } else {
        Ok(changes)
    }
}
