//! Compatible flake generation records and GC roots. Reading rejected records
//! never modifies them; publication installs current only after every root.

use crate::{
    configuration::{ConfigRoot, MutationGuard},
    error::ConfctlError,
};
mod login;
use chrono::{DateTime, FixedOffset, SecondsFormat};
pub use login::LoginProbe;
use serde_json::{Map, Value};
use std::{
    collections::BTreeMap,
    ffi::{OsStr, OsString},
    fs,
    io::Write,
    path::{Path, PathBuf},
};

#[derive(Clone, Debug)]
pub struct BuildGeneration {
    pub name: String,
    pub date: DateTime<FixedOffset>,
    pub toplevel: PathBuf,
    pub auto_rollback: Option<PathBuf>,
    pub inputs: Map<String, Value>,
    pub inputs_info: Map<String, Value>,
}
impl BuildGeneration {
    pub fn decode(name: &str, value: Value) -> Result<Self, ConfctlError> {
        let invalid = |message: &str| ConfctlError::State(message.into());
        let object = value
            .as_object()
            .ok_or_else(|| invalid("expected a JSON object"))?;
        if object.get("mode").and_then(Value::as_str) != Some("flakes") {
            return Err(invalid(&format!(
                "unsupported generation mode {:?}; only explicit flakes mode is supported",
                object.get("mode")
            )));
        }
        let toplevel = object
            .get("toplevel")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| invalid("invalid flake generation payload"))?;
        let auto_rollback = match object.get("auto_rollback") {
            None | Some(Value::Null) => None,
            Some(Value::String(path)) => Some(path.into()),
            _ => return Err(invalid("invalid flake generation payload")),
        };
        let inputs = object
            .get("inputs")
            .cloned()
            .unwrap_or_else(|| Value::Object(Map::new()))
            .as_object()
            .cloned()
            .ok_or_else(|| invalid("invalid flake generation payload"))?;
        if inputs.values().any(|value| !value.is_string()) {
            return Err(invalid("invalid flake generation payload"));
        }
        for role in inputs.keys() {
            component(role, "input role")?;
        }
        let inputs_info = object
            .get("inputs_info")
            .filter(|v| !v.is_null())
            .or_else(|| object.get("inputsInfo"))
            .cloned()
            .unwrap_or_else(|| Value::Object(Map::new()))
            .as_object()
            .cloned()
            .ok_or_else(|| invalid("invalid flake generation payload"))?;
        let date = object
            .get("date")
            .and_then(Value::as_str)
            .ok_or_else(|| invalid("invalid generation date"))?;
        let date = DateTime::parse_from_rfc3339(date)
            .map_err(|error| invalid(&format!("invalid generation date: {error}")))?;
        component(name, "generation name")?;
        Ok(Self {
            name: name.into(),
            date,
            toplevel: toplevel.into(),
            auto_rollback,
            inputs,
            inputs_info,
        })
    }
    pub fn encode(&self) -> Value {
        serde_json::json!({"mode":"flakes","date":self.date.to_rfc3339_opts(SecondsFormat::Secs,true),"toplevel":self.toplevel,"auto_rollback":self.auto_rollback,"inputs":self.inputs,"inputs_info":self.inputs_info})
    }
}

#[derive(Clone, Debug)]
pub struct RejectedGeneration {
    pub path: PathBuf,
    pub reason: String,
}
#[derive(Clone, Debug, Default)]
pub struct GenerationInventory {
    pub generations: Vec<BuildGeneration>,
    pub rejected: BTreeMap<String, RejectedGeneration>,
    pub current: Option<String>,
    pub current_error: Option<String>,
    pub diagnostics: Vec<String>,
}
impl GenerationInventory {
    pub fn require_resolved_current(&self, host: &str) -> Result<(), ConfctlError> {
        if let Some(error) = &self.current_error {
            Err(ConfctlError::State(format!(
                "Cannot select current generation for {host}: {error}"
            )))
        } else {
            Ok(())
        }
    }
    pub fn select(&self, host: &str, name: &str) -> Result<Option<&BuildGeneration>, ConfctlError> {
        let numeric = name.parse::<i64>().ok();
        if numeric.is_some() && !self.rejected.is_empty() {
            return Err(ConfctlError::State(format!(
                "Numeric generation selection for {host} is ambiguous because records were excluded; select an explicit supported generation name"
            )));
        }
        if name == "current" {
            self.require_resolved_current(host)?;
            return Ok(self.current.as_ref().and_then(|current| {
                self.generations
                    .iter()
                    .find(|generation| generation.name == *current)
            }));
        }
        if let Some(rejected) = self.rejected.get(name) {
            return Err(ConfctlError::State(format!(
                "Cannot select generation {name} for {host}: {}",
                rejected.reason
            )));
        }
        if let Some(offset) = numeric {
            let len = self.generations.len() as i64;
            let index = if offset == 0 {
                len - 1
            } else if offset > 0 {
                offset - 1
            } else {
                len + offset - 1
            };
            return Ok(usize::try_from(index)
                .ok()
                .and_then(|index| self.generations.get(index)));
        }
        Ok(self
            .generations
            .iter()
            .find(|generation| generation.name == name))
    }
}

/// The caller supplies the login identity and per-user root, permitting owned
/// fixture roots. Production login resolution belongs to application setup.
#[derive(Clone, Debug)]
pub struct GcRoots {
    directory: PathBuf,
}
impl GcRoots {
    pub fn new(
        root: &ConfigRoot,
        per_user: impl AsRef<Path>,
        login: impl AsRef<OsStr>,
    ) -> Result<Self, ConfctlError> {
        let login = login.as_ref();
        Ok(Self {
            directory: login_directory(
                per_user.as_ref().as_os_str(),
                login,
                &format!("confctl-{}", root.short_hash()),
            ),
        })
    }
    pub async fn for_process<P: crate::process::ProcessRunner>(
        root: &ConfigRoot,
        runner: &P,
        cancellation: tokio_util::sync::CancellationToken,
    ) -> Result<Self, ConfctlError> {
        let login = LoginProbe::packaged()?
            .resolve(runner, cancellation)
            .await?
            .ok_or_else(|| {
                ConfctlError::Configuration("no implicit conversion of nil into String".into())
            })?;
        Self::new(root, "/nix/var/nix/gcroots/per-user", login)
    }
    pub fn directory(&self) -> &Path {
        &self.directory
    }
    fn add(&self, name: &str, target: &Path) -> Result<(), ConfctlError> {
        fs::create_dir_all(&self.directory)
            .map_err(|source| io("create GC-root directory", &self.directory, source))?;
        std::os::unix::fs::symlink(target, self.directory.join(name)).map_err(|source| {
            io(
                "create generation GC root",
                &self.directory.join(name),
                source,
            )
        })
    }
}
/// Ruby File.join retains native bytes, dot segments and repeated separators;
/// an absolute-looking USER does not discard the per-user base directory.
fn login_directory(base: &OsStr, login: &OsStr, suffix: &str) -> PathBuf {
    use std::os::unix::ffi::{OsStrExt, OsStringExt};
    let mut joined = base.as_bytes().to_vec();
    for part in [login.as_bytes(), suffix.as_bytes()] {
        if part.first() == Some(&b'/') {
            while joined.last() == Some(&b'/') {
                joined.pop();
            }
        } else if joined.last() != Some(&b'/') {
            joined.push(b'/');
        }
        joined.extend_from_slice(part);
    }
    PathBuf::from(OsString::from_vec(joined))
}

pub struct GenerationStore {
    root: ConfigRoot,
    roots: GcRoots,
}
impl GenerationStore {
    pub fn new(root: ConfigRoot, roots: GcRoots) -> Self {
        Self { root, roots }
    }
    pub fn host_directory(&self, host: &str) -> Result<PathBuf, ConfctlError> {
        let host = safe_host(host)?;
        Ok(self.root.path().join(".confctl/generations").join(host))
    }
    pub fn load(&self, host: &str) -> Result<GenerationInventory, ConfctlError> {
        let directory = self.host_directory(host)?;
        let mut inventory = GenerationInventory::default();
        let entries = match fs::read_dir(&directory) {
            Ok(entries) => entries,
            Err(source) if source.kind() == std::io::ErrorKind::NotFound => return Ok(inventory),
            Err(source) => return Err(io("list generation directory", &directory, source)),
        };
        for entry in entries {
            let entry = entry
                .map_err(|source| io("read generation directory entry", &directory, source))?;
            if !entry
                .file_type()
                .map_err(|source| io("inspect generation entry", &entry.path(), source))?
                .is_dir()
            {
                continue;
            }
            let name = entry
                .file_name()
                .into_string()
                .map_err(|_| ConfctlError::State("generation name is not UTF-8".into()))?;
            let path = entry.path().join("generation.json");
            let parsed = fs::read(&path)
                .map_err(|source| io("read generation record", &path, source))
                .and_then(|bytes| {
                    serde_json::from_slice(&bytes).map_err(|source| ConfctlError::Decode {
                        context: path.display().to_string(),
                        source,
                    })
                })
                .and_then(|value| BuildGeneration::decode(&name, value));
            match parsed {
                Ok(generation) => inventory.generations.push(generation),
                Err(error) => {
                    let reason = format!("{}: {error}", path.display());
                    inventory.diagnostics.push(format!("Skipping generation {}: {reason}. Records and GC roots were left untouched; use confctl v3 for old software-pin generations.",entry.path().display()));
                    inventory.rejected.insert(
                        name,
                        RejectedGeneration {
                            path: entry.path(),
                            reason,
                        },
                    );
                }
            }
        }
        inventory
            .generations
            .sort_by_key(|generation| generation.date);
        let current = directory.join("current");
        match fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                let target = fs::read_link(&current)
                    .map_err(|source| io("read current generation link", &current, source))?;
                let name = target.file_name().and_then(OsStr::to_str).unwrap_or("");
                if inventory
                    .generations
                    .iter()
                    .any(|generation| generation.name == name)
                {
                    match (current.canonicalize(), directory.join(name).canonicalize()) {
                        (Ok(actual), Ok(expected)) if actual == expected => {
                            inventory.current = Some(name.into())
                        }
                        _ => {
                            inventory.current_error = Some(format!(
                                "{}: target does not resolve to local generation {name}",
                                current.display()
                            ))
                        }
                    }
                } else {
                    inventory.current_error = Some(
                        inventory
                            .rejected
                            .get(name)
                            .map(|record| record.reason.clone())
                            .unwrap_or_else(|| {
                                format!("{}: missing generation {name}", current.display())
                            }),
                    );
                }
            }
            Ok(_) => {
                inventory.current_error = Some(format!("{}: expected a symlink", current.display()))
            }
            Err(source) if source.kind() == std::io::ErrorKind::NotFound => {
                inventory.current = inventory
                    .generations
                    .last()
                    .map(|generation| generation.name.clone());
                if !inventory.rejected.is_empty()
                    && let Some(name) = &inventory.current
                {
                    inventory.diagnostics.push(format!("No current link for {host}; selecting newest supported generation {name}. Excluded records and GC roots were left untouched."));
                }
            }
            Err(source) => return Err(io("inspect current generation", &current, source)),
        }
        Ok(inventory)
    }
    pub fn publish(
        &self,
        guard: &MutationGuard,
        host: &str,
        generation: &BuildGeneration,
    ) -> Result<(), ConfctlError> {
        guard.require_root(&self.root)?;
        component(&generation.name, "generation name")?;
        let host_dir = self.host_directory(host)?;
        fs::create_dir_all(&host_dir)
            .map_err(|source| io("create host generation directory", &host_dir, source))?;
        let dir = host_dir.join(&generation.name);
        fs::create_dir(&dir).map_err(|source| {
            io(
                "create generation without overwriting collision",
                &dir,
                source,
            )
        })?;
        let mut links = vec![(OsString::from("toplevel"), generation.toplevel.clone())];
        if let Some(rollback) = &generation.auto_rollback {
            links.push(("auto_rollback".into(), rollback.clone()));
        }
        for (role, path) in &generation.inputs {
            component(role, "input role")?;
            let path = path
                .as_str()
                .ok_or_else(|| ConfctlError::State("input path must be a string".into()))?;
            links.push((format!("{role}.input").into(), path.into()));
        }
        for (name, target) in &links {
            std::os::unix::fs::symlink(target, dir.join(name))
                .map_err(|source| io("create generation store link", &dir.join(name), source))?;
        }
        let record = dir.join("generation.json");
        let mut file = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&record)
            .map_err(|source| io("create generation record", &record, source))?;
        let mut encoded =
            serde_json::to_vec_pretty(&generation.encode()).expect("generation JSON serializable");
        encoded.push(b'\n');
        file.write_all(&encoded)
            .and_then(|()| file.sync_all())
            .map_err(|source| io("write generation record", &record, source))?;
        for (name, _) in &links {
            let suffix = if name == "toplevel" {
                "toplevel".into()
            } else if name == "auto_rollback" {
                "auto_rollback".into()
            } else {
                format!(
                    "input.{}",
                    name.to_string_lossy().trim_end_matches(".input")
                )
            };
            self.roots.add(
                &format!(
                    "{}-generation-{}-{suffix}",
                    safe_host(host)?,
                    generation.name
                ),
                &dir.join(name),
            )?;
        }
        // A fault before this point deliberately leaves the previous current
        // target intact, including a deliberately unresolved target.
        self.set_current(guard, host, &generation.name)
    }
    pub fn set_current(
        &self,
        guard: &MutationGuard,
        host: &str,
        name: &str,
    ) -> Result<(), ConfctlError> {
        guard.require_root(&self.root)?;
        component(name, "generation name")?;
        let dir = self.host_directory(host)?;
        let temporary = tempfile::Builder::new()
            .prefix("current.new-")
            .tempfile_in(&dir)
            .map_err(|source| io("reserve current replacement", &dir, source))?;
        let path = temporary.path().to_owned();
        temporary
            .close()
            .map_err(|source| io("prepare current replacement", &path, source))?;
        std::os::unix::fs::symlink(name, &path)
            .map_err(|source| io("write current replacement", &path, source))?;
        fs::rename(&path, dir.join("current"))
            .map_err(|source| io("publish current generation", &dir.join("current"), source))
    }
}
fn safe_host(host: &str) -> Result<String, ConfctlError> {
    let host = host.replace('/', ":");
    component(&host, "machine name")?;
    Ok(host)
}
fn component(name: &str, what: &str) -> Result<(), ConfctlError> {
    if name.is_empty() || name == "." || name == ".." || name.contains('/') || name.contains('\0') {
        Err(ConfctlError::State(format!("invalid {what}")))
    } else {
        Ok(())
    }
}
fn io(operation: &'static str, path: &Path, source: std::io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path: Some(path.into()),
        source,
    }
}

#[cfg(test)]
mod tests;
