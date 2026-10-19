use super::{CurrentProfileMissing, MetadataJsonFailure, NetbootServerConfig, invalid, joined};
use chrono::{DateTime, Local};
use serde_json::{Value, json};
use std::{
    io,
    os::unix::fs::MetadataExt,
    path::{Path, PathBuf},
};

#[derive(Clone)]
pub(super) struct Variant {
    pub name: String,
    pub label: String,
    pub params: Vec<String>,
}

impl Variant {
    fn for_spin(spin: &str) -> Vec<Self> {
        if spin != "vpsadminos" {
            return Vec::new();
        }
        [
            ("default", "Default runlevel", vec!["runlevel=default"]),
            (
                "nopools",
                "Default runlevel without container imports",
                vec!["runlevel=default", "osctl.pools=0"],
            ),
            (
                "nostart",
                "Default runlevel without container autostart",
                vec!["runlevel=default", "osctl.autostart=0"],
            ),
            (
                "rescue",
                "Rescue runlevel (network and sshd)",
                vec!["runlevel=rescue"],
            ),
            (
                "single",
                "Single-user runlevel (console only)",
                vec!["runlevel=single"],
            ),
        ]
        .into_iter()
        .map(|(name, label, params)| Self {
            name: name.into(),
            label: label.into(),
            params: params.into_iter().map(str::to_owned).collect(),
        })
        .collect()
    }
    fn json(&self) -> Value {
        json!({"name":self.name,"label":self.label,"kernel_params":self.params})
    }
}

pub(super) struct BootFile {
    pub name: String,
    pub path: PathBuf,
    pub url: String,
}

pub(super) struct Generation {
    pub number: String,
    pub store_path: PathBuf,
    pub current: bool,
    pub timestamp: i64,
    pub time_s: String,
    pub toplevel: PathBuf,
    pub version: Option<String>,
    pub revision: Option<String>,
    pub shortrev: Option<String>,
    pub kernel_version: Option<String>,
    pub macs: Vec<String>,
    pub params: Vec<String>,
    pub boot_files: Vec<BootFile>,
    pub variants: Vec<Variant>,
    pub url: String,
    metadata: Value,
    link: PathBuf,
}

pub(super) struct Machine {
    pub name: String,
    pub spin: String,
    pub fqdn: String,
    pub label: String,
    pub short_label: String,
    pub url: String,
    pub generations: Vec<Generation>,
}

fn string(value: &Value, key: &str) -> io::Result<String> {
    value
        .get(key)
        .and_then(Value::as_str)
        .map(str::to_owned)
        .ok_or_else(|| invalid(format!("missing or invalid {key}")))
}
fn optional_string(value: &Value, key: &str) -> io::Result<Option<String>> {
    match value.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => Ok(Some(value.clone())),
        _ => Err(invalid(format!("invalid {key}"))),
    }
}
fn strings(value: &Value, key: &str) -> io::Result<Vec<String>> {
    match value.get(key) {
        None => Ok(Vec::new()),
        Some(Value::Array(values)) => values
            .iter()
            .map(|value| {
                value
                    .as_str()
                    .map(str::to_owned)
                    .ok_or_else(|| invalid(format!("invalid {key} element")))
            })
            .collect(),
        _ => Err(invalid(format!("invalid {key}"))),
    }
}

fn kernel_version(toplevel: &Path) -> io::Result<Option<String>> {
    let path = match std::fs::read_link(toplevel.join("kernel")) {
        Ok(path) => path,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(error),
    };
    let Some(path) = path
        .to_str()
        .and_then(|path| path.strip_prefix("/nix/store/"))
    else {
        return Ok(None);
    };
    let Some((hash, tail)) = path.split_once('-') else {
        return Ok(None);
    };
    if hash.is_empty() {
        return Ok(None);
    }
    Ok(tail
        .strip_prefix("linux-")
        .and_then(|tail| tail.split('/').next())
        .filter(|value| !value.is_empty())
        .map(str::to_owned))
}

impl Generation {
    fn load(link: PathBuf, number: String) -> io::Result<Self> {
        let store_path = std::fs::canonicalize(&link)?;
        let stat = std::fs::symlink_metadata(&link)?;
        let time: DateTime<Local> = stat.modified()?.into();
        // Keep the exact lexical path used for the read; store_path is not
        // the attempted profile-link path in HELPER-DIAG-01.
        let metadata_path = link.join("machine.json");
        let metadata: Value =
            serde_json::from_slice(&std::fs::read(&metadata_path)?).map_err(|source| {
                io::Error::new(
                    io::ErrorKind::InvalidData,
                    MetadataJsonFailure {
                        path: metadata_path,
                        source,
                    },
                )
            })?;
        let toplevel = PathBuf::from(string(&metadata, "toplevel")?);
        let version = optional_string(&metadata, "version")?;
        let revision = optional_string(&metadata, "revision")?;
        let shortrev = revision
            .as_ref()
            .map(|value| value.chars().take(9).collect())
            .or_else(|| {
                version.as_ref().and_then(|value| {
                    value
                        .split('.')
                        .rfind(|part| !part.is_empty())
                        .map(str::to_owned)
                })
            });
        let params_file = store_path.join("kernel-params");
        let params = if params_file.exists() {
            std::fs::read_to_string(params_file)?
                .trim_matches(['\0', '\t', '\n', '\x0b', '\x0c', '\r', ' '])
                .split(['\t', '\n', '\x0b', '\x0c', '\r', ' '])
                .filter(|value| !value.is_empty())
                .map(str::to_owned)
                .collect()
        } else {
            strings(&metadata, "kernelParams")?
        };
        let macs = strings(&metadata, "macs")?;
        let kernel_version = kernel_version(&toplevel)?;
        Ok(Self {
            number,
            store_path,
            current: false,
            timestamp: stat.mtime(),
            time_s: time.format("%Y-%m-%d %H:%M:%S").to_string(),
            toplevel,
            version,
            revision,
            shortrev,
            kernel_version,
            macs,
            params,
            boot_files: Vec::new(),
            variants: Vec::new(),
            url: String::new(),
            metadata,
            link,
        })
    }

    fn resolve(&mut self, machine_url: &str, spin: &str) -> io::Result<()> {
        self.url = joined(machine_url, &self.number);
        for name in ["bzImage", "initrd", "root.squashfs"] {
            match std::fs::canonicalize(self.link.join(name)) {
                Ok(path) => self.boot_files.push(BootFile {
                    name: name.into(),
                    path,
                    url: joined(&self.url, name),
                }),
                Err(error) if error.kind() == io::ErrorKind::NotFound => {}
                Err(error) => return Err(error),
            }
        }
        self.variants = Variant::for_spin(spin);
        self.params.retain(|param| !param.starts_with("httproot="));
        if spin == "nixos" && !self.params.iter().any(|param| param.starts_with("init=")) {
            self.params
                .insert(0, format!("init={}/init", self.toplevel.display()));
        }
        let root = if spin == "vpsadminos" {
            self.boot_files
                .iter()
                .find(|file| file.name == "root.squashfs")
        } else {
            self.boot_files
                .iter()
                .find(|file| file.name == "initrd")
                .or_else(|| self.boot_files.iter().find(|file| file.name == "bzImage"))
        };
        if let Some(root) = root {
            self.params.insert(0, format!("httproot={}", root.url));
        }
        Ok(())
    }

    pub fn pxe_params(&self) -> Vec<String> {
        if self.params.is_empty() {
            vec![
                format!("init={}/init", self.toplevel.display()),
                "loglevel=7".into(),
            ]
        } else {
            self.params.clone()
        }
    }

    pub fn json(&self) -> Value {
        let mut files = serde_json::Map::new();
        for file in &self.boot_files {
            files.insert(file.name.clone(), Value::String(file.url.clone()));
        }
        let mut result = json!({"url":self.url,"store_path":self.store_path,"generation":serde_json::from_str::<Value>(&self.number).expect("decimal generation"),"time":self.timestamp,"time_s":self.time_s,"current":self.current,"toplevel":self.toplevel,"version":self.version,"revision":self.revision,"shortrev":self.shortrev,"macs":self.macs,"kernel_version":self.kernel_version,"kernel_params":self.params,"boot_files":files,"variants":self.variants.iter().map(Variant::json).collect::<Vec<_>>()});
        if let Some(info) = self
            .metadata
            .get("inputs-info")
            .filter(|value| !value.is_null() && *value != &Value::Bool(false))
        {
            result
                .as_object_mut()
                .expect("generation object")
                .insert("inputs_info".into(), info.clone());
        }
        result
    }
}

impl Machine {
    pub fn current(&self) -> &Generation {
        self.generations
            .iter()
            .find(|generation| generation.current)
            .expect("resolved current generation")
    }
    fn resolve(&mut self, config: &NetbootServerConfig) -> io::Result<()> {
        self.generations.sort_by(|a, b| {
            b.number
                .len()
                .cmp(&a.number.len())
                .then_with(|| b.number.cmp(&a.number))
        });
        let current = self
            .generations
            .iter()
            .find(|generation| generation.current)
            .ok_or_else(|| {
                invalid(format!(
                    "Unable to find current generation of machine {}",
                    self.name
                ))
            })?;
        self.spin = match current.metadata.get("spin") {
            None => "nixos".into(),
            Some(Value::String(value)) => value.clone(),
            _ => return Err(invalid("invalid machine spin")),
        };
        self.fqdn = match current.metadata.get("fqdn") {
            None => self.name.clone(),
            Some(Value::String(value)) => value.clone(),
            _ => return Err(invalid("invalid machine FQDN")),
        };
        self.label = optional_string(&current.metadata, "label")?
            .or(optional_string(&current.metadata, "fqdn")?)
            .unwrap_or_else(|| self.name.clone());
        let mut parts = self.label.split('.').collect::<Vec<_>>();
        while parts.last() == Some(&"") {
            parts.pop();
        }
        self.short_label = parts.into_iter().take(2).collect::<Vec<_>>().join(".");
        self.url = joined(&config.http_url, &self.fqdn);
        for generation in &mut self.generations {
            generation.resolve(&self.url, &self.spin)?;
        }
        Ok(())
    }
    pub fn json(&self) -> Value {
        json!({"url":self.url,"name":self.name,"spin":self.spin,"fqdn":self.fqdn,"label":self.label,"generations":self.generations.iter().map(Generation::json).collect::<Vec<_>>()})
    }
}

pub(super) fn load(config: &NetbootServerConfig, profiles: &Path) -> io::Result<Vec<Machine>> {
    let mut machines: Vec<Machine> = Vec::new();
    for entry in std::fs::read_dir(profiles)? {
        let entry = entry?;
        let filename = entry.file_name();
        let Some(filename) = filename.to_str() else {
            continue;
        };
        let Some(name) = filename
            .strip_prefix("confctl-")
            .and_then(|name| name.strip_suffix("-link"))
        else {
            continue;
        };
        let Some((name, number)) = name.rsplit_once('-') else {
            continue;
        };
        if name.is_empty() || number.is_empty() || !number.bytes().all(|byte| byte.is_ascii_digit())
        {
            continue;
        }
        let number = number.trim_start_matches('0');
        let number = if number.is_empty() { "0" } else { number }.to_owned();
        let generation = Generation::load(entry.path(), number)?;
        if let Some(machine) = machines.iter_mut().find(|machine| machine.name == name) {
            machine.generations.push(generation);
        } else {
            machines.push(Machine {
                name: name.into(),
                spin: "nixos".into(),
                fqdn: name.into(),
                label: name.into(),
                short_label: name.chars().take(15).collect(),
                url: String::new(),
                generations: vec![generation],
            });
        }
    }
    for machine in &mut machines {
        let current_path = profiles.join(format!("confctl-{}", machine.name));
        let current = std::fs::canonicalize(&current_path).map_err(|source| {
            if source.raw_os_error() == Some(rustix::io::Errno::NOENT.raw_os_error()) {
                io::Error::new(
                    source.kind(),
                    CurrentProfileMissing {
                        path: current_path,
                        source,
                    },
                )
            } else {
                source
            }
        })?;
        if let Some(generation) = machine
            .generations
            .iter_mut()
            .find(|generation| generation.store_path == current)
        {
            generation.current = true;
        }
        machine.resolve(config)?;
    }
    Ok(machines)
}
