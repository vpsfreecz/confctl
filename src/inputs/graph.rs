//! Version-7 logical paths, independent of Nix's generated node identifiers.
use super::FlakeLock;
use crate::error::ConfctlError;
use serde_json::{Map, Value};
use std::collections::BTreeMap;

pub struct LockGraph<'a> {
    nodes: &'a Map<String, Value>,
    root: &'a str,
    pub entries: BTreeMap<String, GraphEntry<'a>>,
}
#[derive(Clone, Debug)]
pub struct GraphEntry<'a> {
    pub node_id: &'a str,
    pub node: &'a Value,
    pub terminal_path: String,
}
impl<'a> LockGraph<'a> {
    pub fn new(lock: &'a FlakeLock) -> Result<Self, ConfctlError> {
        if lock.data.get("version").and_then(Value::as_u64) != Some(7) {
            return Err(error("Expected a version-7 flake lock graph"));
        }
        let nodes = lock
            .data
            .get("nodes")
            .and_then(Value::as_object)
            .ok_or_else(|| error("Expected a version-7 flake lock graph"))?;
        let root = lock
            .data
            .get("root")
            .and_then(Value::as_str)
            .ok_or_else(|| error("Malformed or dangling lock graph node"))?;
        let mut graph = Self {
            nodes,
            root,
            entries: BTreeMap::new(),
        };
        graph.node(root)?;
        graph.walk(&[], root, &[root])?;
        Ok(graph)
    }
    fn node(&self, id: &str) -> Result<&'a Value, ConfctlError> {
        let node = self
            .nodes
            .get(id)
            .filter(|n| n.is_object())
            .ok_or_else(|| error("Malformed or dangling lock graph node"))?;
        if (id != self.root && !node.get("locked").is_some_and(Value::is_object))
            || node.get("original").is_some_and(|v| !v.is_object())
            || node.get("flake").is_some_and(|v| !v.is_boolean())
            || node.get("inputs").is_some_and(|v| !v.is_object())
        {
            return Err(error("Malformed or dangling lock graph node"));
        }
        if let Some(inputs) = node.get("inputs").and_then(Value::as_object) {
            validate(inputs.keys().map(String::as_str))?;
        }
        Ok(node)
    }
    pub fn resolve_path(&self, path: &str) -> Result<GraphEntry<'a>, ConfctlError> {
        let parts: Vec<_> = path.split('/').collect();
        validate(parts.iter().copied())?;
        self.resolve(&parts, &[])
    }
    fn resolve(
        &self,
        parts: &[&str],
        stack: &[(&str, &str)],
    ) -> Result<GraphEntry<'a>, ConfctlError> {
        let mut id = self.root;
        let mut terminal = Vec::new();
        for name in parts {
            let key = (id, *name);
            if stack.contains(&key) {
                return Err(error("Cyclic follows reference in lock graph"));
            }
            let edge = self
                .node(id)?
                .get("inputs")
                .and_then(|v| v.get(*name))
                .ok_or_else(|| error("Malformed or dangling lock input reference"))?;
            match edge {
                Value::String(next) => {
                    id = next;
                    self.node(id)?;
                    terminal.push((*name).to_owned());
                }
                Value::Array(path) => {
                    let path: Vec<_> = path
                        .iter()
                        .map(|v| v.as_str().ok_or_else(|| error("Malformed lock input path")))
                        .collect::<Result<_, _>>()?;
                    validate(path.iter().copied())?;
                    let mut next_stack = stack.to_vec();
                    next_stack.push(key);
                    let resolved = self.resolve(&path, &next_stack)?;
                    id = resolved.node_id;
                    terminal = if resolved.terminal_path.is_empty() {
                        vec![]
                    } else {
                        resolved
                            .terminal_path
                            .split('/')
                            .map(str::to_owned)
                            .collect()
                    };
                }
                _ => return Err(error("Malformed or dangling lock input reference")),
            }
        }
        Ok(GraphEntry {
            node_id: id,
            node: self.node(id)?,
            terminal_path: terminal.join("/"),
        })
    }
    fn walk(
        &mut self,
        prefix: &[String],
        id: &'a str,
        ancestors: &[&str],
    ) -> Result<(), ConfctlError> {
        let Some(inputs) = self.node(id)?.get("inputs").and_then(Value::as_object) else {
            return Ok(());
        };
        let mut names: Vec<_> = inputs.keys().cloned().collect();
        names.sort();
        for name in names {
            let mut path = prefix.to_vec();
            path.push(name);
            let logical = path.join("/");
            let entry = self.resolve_path(&logical)?;
            if ancestors.contains(&entry.node_id) {
                return Err(error("Cyclic dependency in lock graph"));
            }
            let mut next = ancestors.to_vec();
            next.push(entry.node_id);
            let target = entry.node_id;
            self.entries.insert(logical, entry);
            self.walk(&path, target, &next)?;
        }
        Ok(())
    }
    pub fn update_inputs(&self, names: &[String]) -> Result<Vec<String>, ConfctlError> {
        let mut targets = Vec::new();
        for name in names {
            let target = self.resolve_path(name)?.terminal_path;
            if !targets.contains(&target) {
                targets.push(target);
            }
        }
        Ok(targets)
    }
}
fn validate<'a>(parts: impl IntoIterator<Item = &'a str>) -> Result<(), ConfctlError> {
    if parts.into_iter().any(|s| s.is_empty() || s.contains('/')) {
        Err(error("Malformed lock input path"))
    } else {
        Ok(())
    }
}
fn error(message: &str) -> ConfctlError {
    ConfctlError::Configuration(message.into())
}
