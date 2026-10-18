use crate::{domain::ruby_string, error::ConfctlError};
use serde_json::{Map, Value};
use std::{fs, path::Path};

#[derive(Clone, Debug)]
pub struct FlakeLock {
    pub data: Value,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct InputInfo {
    pub kind: String,
    pub rev: Option<String>,
    pub short_rev: Option<String>,
    pub reference: Option<String>,
    pub url: Option<String>,
    pub locked: Map<String, Value>,
    pub original: Map<String, Value>,
}

impl FlakeLock {
    pub fn load(path: &Path) -> Result<Self, ConfctlError> {
        let bytes = fs::read(path).map_err(|source| ConfctlError::Io {
            operation: "read flake.lock",
            path: Some(path.into()),
            source,
        })?;
        Self::decode(&bytes)
    }
    pub fn decode(bytes: &[u8]) -> Result<Self, ConfctlError> {
        serde_json::from_slice(bytes)
            .map(|data| Self { data })
            .map_err(|source| ConfctlError::Decode {
                context: "flake.lock".into(),
                source,
            })
    }
    pub fn root_inputs(&self) -> Vec<String> {
        let mut names = self
            .root_map()
            .map(|map| map.keys().cloned().collect::<Vec<_>>())
            .unwrap_or_default();
        names.sort();
        names
    }
    fn root_map(&self) -> Option<&Map<String, Value>> {
        // Ruby reads the literal root node, independently of data["root"].
        self.data
            .get("nodes")?
            .get("root")?
            .get("inputs")?
            .as_object()
    }
    pub fn node_id(&self, input: &str) -> Option<&str> {
        match self.root_map()?.get(input)? {
            Value::String(id) => Some(id),
            Value::Array(segments) => segments.first()?.as_str(),
            _ => None,
        }
    }
    pub fn node_for_input(&self, input: &str) -> Option<&Value> {
        self.data.get("nodes")?.get(self.node_id(input)?)
    }
    pub fn input_info(&self, input: &str) -> InputInfo {
        let node = self.node_for_input(input);
        let object = |field| {
            node.and_then(|node| node.get(field))
                .and_then(Value::as_object)
                .cloned()
                .unwrap_or_default()
        };
        let locked = object("locked");
        let original = object("original");
        let kind = first(&locked, &original, "type")
            .map(ruby_string)
            .unwrap_or_else(|| "-".into());
        let rev = locked.get("rev").filter(|v| truthy(v)).map(ruby_string);
        let short_rev = rev.as_ref().map(|rev| rev.chars().take(8).collect());
        let reference = first(&original, &locked, "ref").map(ruby_string);
        let mut url = first(&locked, &original, "url")
            .map(ruby_string)
            .map(|url| url.strip_prefix("git+").unwrap_or(&url).to_owned());
        if locked.get("type").and_then(Value::as_str) == Some("github")
            || original.get("type").and_then(Value::as_str) == Some("github")
        {
            let owner = first(&locked, &original, "owner");
            let repo = first(&locked, &original, "repo");
            if let (Some(owner), Some(repo)) = (owner, repo) {
                url = Some(format!(
                    "https://github.com/{}/{}",
                    ruby_string(owner),
                    ruby_string(repo)
                ));
            }
        }
        InputInfo {
            kind,
            rev,
            short_rev,
            reference,
            url,
            locked,
            original,
        }
    }
    pub fn override_url(&self, input: &str, revision: &str) -> Result<String, ConfctlError> {
        self.node_id(input)
            .ok_or_else(|| ConfctlError::Configuration(format!("unknown input '{input}'")))?;
        let info = self.input_info(input);
        match info.kind.as_str() {
            "github" => {
                let owner = first(&info.locked, &info.original, "owner");
                let repo = first(&info.locked, &info.original, "repo");
                let (Some(owner), Some(repo)) = (owner, repo) else {
                    return Err(ConfctlError::Configuration(
                        "missing github owner or repo".into(),
                    ));
                };
                let mut url = format!(
                    "github:{}/{}/{revision}",
                    ruby_string(owner),
                    ruby_string(repo)
                );
                if let Some(dir) = first(&info.locked, &info.original, "dir") {
                    url.push_str(&format!("?dir={}", ruby_string(dir)));
                }
                Ok(url)
            }
            "git" => {
                let url = first(&info.locked, &info.original, "url")
                    .map(ruby_string)
                    .ok_or_else(|| ConfctlError::Configuration("missing git url".into()))?;
                let url = url.strip_prefix("git+").unwrap_or(&url);
                Ok(format!(
                    "git+{url}{}rev={revision}",
                    if url.contains('?') { '&' } else { '?' }
                ))
            }
            kind => Err(ConfctlError::Configuration(format!(
                "input '{input}' has unsupported type '{kind}' for set"
            ))),
        }
    }
    pub fn original(&self, input: &str) -> Option<Value> {
        self.node_for_input(input)?
            .get("original")
            .filter(|v| !v.is_null())
            .cloned()
    }
    /// Restore metadata on the temporary result, using that result's first
    /// segment mapping exactly as Ruby does before the atomic lock replacement.
    pub fn restore_original(
        &mut self,
        input: &str,
        original: Option<&Value>,
    ) -> Result<(), ConfctlError> {
        let id = self
            .node_id(input)
            .ok_or_else(|| ConfctlError::Configuration(format!("unknown input '{input}'")))?
            .to_owned();
        let nodes = self
            .data
            .get_mut("nodes")
            .and_then(Value::as_object_mut)
            .ok_or_else(|| ConfctlError::State("flake.lock nodes is not an object".into()))?;
        let node = nodes.entry(id).or_insert_with(|| Value::Object(Map::new()));
        let node = node
            .as_object_mut()
            .ok_or_else(|| ConfctlError::State("flake.lock input node is not an object".into()))?;
        if let Some(original) = original {
            node.insert("original".into(), original.clone());
        } else {
            node.shift_remove("original");
        }
        Ok(())
    }
    pub fn pretty(&self) -> Vec<u8> {
        let mut bytes = serde_json::to_vec_pretty(&self.data).expect("JSON value serializes");
        bytes.push(b'\n');
        bytes
    }
}

fn truthy(value: &Value) -> bool {
    !matches!(value, Value::Null | Value::Bool(false))
}
fn first<'a>(a: &'a Map<String, Value>, b: &'a Map<String, Value>, key: &str) -> Option<&'a Value> {
    a.get(key)
        .filter(|v| truthy(v))
        .or_else(|| b.get(key).filter(|v| truthy(v)))
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct InputChange {
    pub name: String,
    pub old_info: Option<InputInfo>,
    pub new_info: InputInfo,
}
impl InputChange {
    pub fn between(old: Option<&FlakeLock>, new: &FlakeLock, names: &[String]) -> Vec<Self> {
        names
            .iter()
            .filter_map(|name| {
                let old_info = old.map(|old| old.input_info(name));
                let new_info = new.input_info(name);
                if old_info.as_ref().and_then(|info| info.rev.as_ref()) == new_info.rev.as_ref() {
                    None
                } else {
                    Some(Self {
                        name: name.clone(),
                        old_info,
                        new_info,
                    })
                }
            })
            .collect()
    }
}
