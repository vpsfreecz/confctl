//! Inventory order and Ruby-compatible name/key/filter selection.

use super::ManagedFilter;
use crate::error::ConfctlError;
use serde_json::Value;

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct MachineId(pub usize);

#[derive(Clone, Debug)]
pub struct Machine {
    pub name: String,
    pub key: Option<String>,
    pub cluster_name: String,
    pub carrier: Option<String>,
    pub metadata: Value,
    /// Preserve unrecognized top-level fields for adapters and round trips.
    pub extra: Value,
}

impl Machine {
    pub fn from_value(value: Value) -> Result<Self, ConfctlError> {
        let name = value
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(|| ConfctlError::Configuration("machine has no name".into()))?
            .to_owned();
        let key = ["key", "machineKey", "flakeKey"]
            .into_iter()
            .find_map(|key| value.get(key).and_then(Value::as_str).map(str::to_owned));
        let metadata = value.get("metaConfig").cloned().unwrap_or(Value::Null);
        Ok(Self {
            cluster_name: value
                .get("clusterName")
                .and_then(Value::as_str)
                .unwrap_or(&name)
                .to_owned(),
            carrier: value
                .get("carrier")
                .and_then(Value::as_str)
                .map(str::to_owned),
            name,
            key,
            metadata,
            extra: value,
        })
    }

    pub fn managed(&self) -> bool {
        ruby_truthy(self.metadata.get("managed"))
    }
    pub fn runnable(&self) -> bool {
        self.carrier.is_none()
            && self
                .metadata
                .get("host")
                .and_then(|v| v.get("target"))
                .is_none_or(|v| !v.is_null())
    }
    pub fn spin(&self) -> Option<&str> {
        self.metadata.get("spin").and_then(Value::as_str)
    }
    pub fn attribute(&self, key: &str) -> Value {
        match key {
            "name" => Value::String(self.name.clone()),
            "key" | "machineKey" | "flakeKey" => {
                self.key.clone().map(Value::String).unwrap_or(Value::Null)
            }
            _ => {
                let mut value = &self.metadata;
                for part in key.split('.') {
                    let Some(next) = value.get(part) else {
                        return Value::Null;
                    };
                    value = next;
                }
                value.clone()
            }
        }
    }
}

#[derive(Clone, Debug, Default)]
pub struct Inventory {
    pub machines: Vec<Machine>,
}

impl Inventory {
    pub fn from_value(value: Value) -> Result<Self, ConfctlError> {
        let object = value.as_object().ok_or_else(|| {
            ConfctlError::Configuration("machine inventory is not an object".into())
        })?;
        Ok(Self {
            machines: object
                .values()
                .cloned()
                .map(Machine::from_value)
                .collect::<Result<_, _>>()?,
        })
    }
    pub fn get(&self, id: MachineId) -> &Machine {
        &self.machines[id.0]
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AttributeFilter {
    pub attribute: String,
    pub value: String,
    pub negate: bool,
}

impl AttributeFilter {
    pub fn parse(text: &str) -> Result<Self, ConfctlError> {
        let found = text
            .find("!=")
            .map(|i| (i, 2, true))
            .or_else(|| text.find('=').map(|i| (i, 1, false)));
        let Some((i, len, negate)) = found else {
            return Err(ConfctlError::Usage("Invalid filter ''".into()));
        };
        // Ruby's 0..-1 slice makes an empty key span the original filter.
        let attribute = if i == 0 { text } else { &text[..i] }.to_owned();
        Ok(Self {
            attribute,
            value: text[i + len..].into(),
            negate,
        })
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct SelectionRequest {
    pub pattern: Option<String>,
    pub attributes: Vec<AttributeFilter>,
    pub tags: Vec<String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SelectionPolicy {
    pub managed: ManagedFilter,
    pub runnable: bool,
    pub vpsadminos_only: bool,
}

impl SelectionPolicy {
    pub const ALL: Self = Self {
        managed: ManagedFilter::All,
        runnable: false,
        vpsadminos_only: false,
    };
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Selection {
    pub machines: Vec<MachineId>,
}

pub fn select(
    inventory: &Inventory,
    request: &SelectionRequest,
    policy: SelectionPolicy,
) -> Result<Selection, ConfctlError> {
    let passes = |machine: &Machine| {
        request.attributes.iter().all(|filter| {
            (ruby_string(&machine.attribute(&filter.attribute)) == filter.value) != filter.negate
        }) && request.tags.iter().all(|tag| {
            let (tag, negate) = tag
                .strip_prefix('^')
                .map_or((tag.as_str(), false), |t| (t, true));
            machine
                .metadata
                .get("tags")
                .and_then(Value::as_array)
                .is_some_and(|tags| tags.iter().any(|v| v.as_str() == Some(tag)))
                != negate
        })
    };
    let candidates: Vec<_> = inventory
        .machines
        .iter()
        .enumerate()
        .filter(|(_, m)| passes(m))
        .collect();
    // Name existence takes precedence even when its attribute filter rejects it.
    let exact = request
        .pattern
        .as_ref()
        .is_some_and(|p| inventory.machines.iter().any(|m| m.name == *p));
    let keys = request
        .pattern
        .as_ref()
        .is_some_and(|p| candidates.iter().any(|(_, m)| m.key.as_ref() == Some(p)));
    let machines = candidates
        .into_iter()
        .filter(|(_, m)| {
            request.pattern.as_ref().is_none_or(|p| {
                if exact {
                    m.name == *p
                } else if keys {
                    m.key.as_ref() == Some(p)
                } else {
                    pattern_matches(p, &m.name)
                }
            }) && match policy.managed {
                ManagedFilter::All => true,
                ManagedFilter::Managed => m.managed(),
                ManagedFilter::Unmanaged => !m.managed(),
            } && (!policy.runnable || m.runnable())
                && (!policy.vpsadminos_only || m.spin() == Some("vpsadminos"))
        })
        .map(|(i, _)| MachineId(i))
        .collect();
    Ok(Selection { machines })
}

fn ruby_truthy(value: Option<&Value>) -> bool {
    !matches!(value, None | Some(Value::Null) | Some(Value::Bool(false)))
}

pub fn ruby_string(value: &Value) -> String {
    match value {
        Value::Null => String::new(),
        Value::String(text) => text.clone(),
        Value::Array(items) => format!(
            "[{}]",
            items
                .iter()
                .map(ruby_inspect)
                .collect::<Vec<_>>()
                .join(", ")
        ),
        Value::Object(items) => format!(
            "{{{}}}",
            items
                .iter()
                .map(|(k, v)| format!(
                    "{}=>{}",
                    ruby_inspect(&Value::String(k.clone())),
                    ruby_inspect(v)
                ))
                .collect::<Vec<_>>()
                .join(", ")
        ),
        _ => value.to_string(),
    }
}
fn ruby_inspect(value: &Value) -> String {
    match value {
        Value::Null => "nil".into(),
        Value::String(_) => value.to_string(),
        _ => ruby_string(value),
    }
}

/// File.fnmatch?(pattern, name, FNM_EXTGLOB): '*' crosses '/', and only an
/// initial dot is protected because FNM_PATHNAME is not enabled.
pub fn pattern_matches(pattern: &str, name: &str) -> bool {
    if let Some((start, end, choices)) = brace_group(pattern) {
        return choices.into_iter().any(|choice| {
            pattern_matches(
                &format!("{}{}{}", &pattern[..start], choice, &pattern[end + 1..]),
                name,
            )
        });
    }
    let pattern: Vec<char> = pattern.chars().collect();
    let name: Vec<char> = name.chars().collect();
    if name.first() == Some(&'.')
        && pattern.first() != Some(&'.')
        && !pattern.starts_with(&['\\', '.'])
    {
        return false;
    }
    let mut memo = std::collections::HashMap::new();
    glob(&pattern, &name, 0, 0, &mut memo)
}
fn brace_group(pattern: &str) -> Option<(usize, usize, Vec<&str>)> {
    let mut escaped = false;
    let mut start = None;
    let mut depth = 0;
    let mut choices = Vec::new();
    let mut part = 0;
    for (i, c) in pattern.char_indices() {
        if escaped {
            escaped = false;
            continue;
        }
        if c == '\\' {
            escaped = true;
            continue;
        }
        match c {
            '{' => {
                if depth == 0 {
                    start = Some(i);
                    part = i + 1;
                }
                depth += 1;
            }
            ',' if depth == 1 => {
                choices.push(&pattern[part..i]);
                part = i + 1;
            }
            '}' if depth > 0 => {
                depth -= 1;
                if depth == 0 {
                    choices.push(&pattern[part..i]);
                    return Some((start?, i, choices));
                }
            }
            _ => {}
        }
    }
    None
}
fn glob(
    p: &[char],
    n: &[char],
    i: usize,
    j: usize,
    memo: &mut std::collections::HashMap<(usize, usize), bool>,
) -> bool {
    if let Some(result) = memo.get(&(i, j)) {
        return *result;
    }
    let result = if i == p.len() {
        j == n.len()
    } else {
        match p[i] {
            '*' => glob(p, n, i + 1, j, memo) || (j < n.len() && glob(p, n, i, j + 1, memo)),
            '?' => j < n.len() && glob(p, n, i + 1, j + 1, memo),
            '\\' => {
                i + 1 < p.len() && j < n.len() && p[i + 1] == n[j] && glob(p, n, i + 2, j + 1, memo)
            }
            '[' => {
                if let Some(end) = p[i + 1..]
                    .iter()
                    .position(|c| *c == ']')
                    .map(|end| end + i + 1)
                {
                    let mut k = i + 1;
                    let negate = matches!(p.get(k), Some('!' | '^'));
                    if negate {
                        k += 1;
                    }
                    let mut matches = false;
                    while k < end && j < n.len() {
                        if k + 2 < end && p[k + 1] == '-' {
                            matches |= p[k] <= n[j] && n[j] <= p[k + 2];
                            k += 3;
                        } else {
                            matches |= p[k] == n[j];
                            k += 1;
                        }
                    }
                    j < n.len() && (matches != negate) && glob(p, n, end + 1, j + 1, memo)
                } else {
                    j < n.len() && n[j] == '[' && glob(p, n, i + 1, j + 1, memo)
                }
            }
            c => j < n.len() && c == n[j] && glob(p, n, i + 1, j + 1, memo),
        }
    };
    memo.insert((i, j), result);
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    #[test]
    fn exact_name_precedes_key_and_filtered_key_precedes_glob() {
        let inventory=Inventory::from_value(json!({"a":{"name":"prod","key":"other","metaConfig":{"managed":false,"tags":[]}},"b":{"name":"other","key":"prod","metaConfig":{"managed":true,"tags":[]}}})).unwrap();
        let request = SelectionRequest {
            pattern: Some("prod".into()),
            ..Default::default()
        };
        let selection = select(&inventory, &request, SelectionPolicy::ALL).unwrap();
        assert_eq!(inventory.get(selection.machines[0]).name, "prod");
        let filtered = select(
            &inventory,
            &request,
            SelectionPolicy {
                managed: ManagedFilter::Managed,
                ..SelectionPolicy::ALL
            },
        )
        .unwrap();
        assert!(filtered.machines.is_empty());
    }
    #[test]
    fn ruby_globs_cross_slashes_protect_initial_dot_and_expand_nested_braces() {
        for (pattern, name, expected) in [
            ("*", "a/b/c", true),
            ("*", "a/.hidden", true),
            ("*", ".hidden", false),
            (".*", ".hidden", true),
            ("{a,{b,c}}", "c", true),
            ("[!a]*", "foo", true),
            (r"a\*", "a*", true),
        ] {
            assert_eq!(pattern_matches(pattern, name), expected, "{pattern} {name}");
        }
    }
    #[test]
    fn filters_use_ruby_values_and_negated_tags() {
        assert_eq!(
            ruby_string(&json!(["a", true, null])),
            r#"["a", true, nil]"#
        );
        let inventory=Inventory::from_value(json!({"a":{"name":"a","metaConfig":{"managed":true,"host":{"target":null},"tags":["prod"],"nested":{"value":false}}}})).unwrap();
        let request = SelectionRequest {
            attributes: vec![
                AttributeFilter::parse("nested.value=false").unwrap(),
                AttributeFilter::parse("missing=").unwrap(),
            ],
            tags: vec!["prod".into(), "^other".into()],
            ..Default::default()
        };
        assert_eq!(
            select(&inventory, &request, SelectionPolicy::ALL)
                .unwrap()
                .machines
                .len(),
            1
        );
        assert!(
            select(
                &inventory,
                &request,
                SelectionPolicy {
                    runnable: true,
                    ..SelectionPolicy::ALL
                }
            )
            .unwrap()
            .machines
            .is_empty()
        );
    }
}
