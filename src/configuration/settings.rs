use super::{ConfigRelativePath, ConfigRoot, StateFiles};
use crate::error::ConfctlError;
use serde_json::Value;

#[derive(Clone, Debug, Default)]
pub struct RuntimeKernelSettings {
    pub enable: bool,
    pub state_file: Option<ConfigRelativePath>,
    pub refresh_before_deploy: bool,
}
#[derive(Clone, Debug, Default)]
pub struct NetbootInventorySettings {
    pub enable: bool,
    pub output_file: Option<ConfigRelativePath>,
}
#[derive(Clone, Debug)]
pub struct NixSettings {
    pub max_jobs: Option<String>,
    pub impure_eval: bool,
    pub legacy_nix_path: bool,
    pub legacy_names: Vec<String>,
}
#[derive(Clone, Debug)]
pub struct Settings {
    pub list_columns: Vec<String>,
    pub nix: NixSettings,
    pub runtime_kernels: RuntimeKernelSettings,
    pub netboot_inventory: NetbootInventorySettings,
    pub build_generations: Value,
    pub host_generations: Value,
    /// Includes all unknown settings; typed fields do not erase extension metadata.
    pub raw: Value,
}
impl Settings {
    pub fn decode(mut raw: Value, root: &ConfigRoot) -> Result<Self, ConfctlError> {
        demodulify(&mut raw);
        if !raw.is_object() {
            return Err(ConfctlError::Configuration(
                "confctl settings must be an object".into(),
            ));
        }
        let kernel = raw.get("runtimeKernels").cloned().unwrap_or_default();
        let netboot = raw.get("netbootInventory").cloned().unwrap_or_default();
        let runtime_kernels = RuntimeKernelSettings {
            enable: boolean(&kernel, "enable")?,
            state_file: relative(&kernel, "stateFile")?,
            refresh_before_deploy: boolean(&kernel, "refreshBeforeDeploy")?,
        };
        let netboot_inventory = NetbootInventorySettings {
            enable: boolean(&netboot, "enable")?,
            output_file: relative(&netboot, "outputFile")?,
        };
        if runtime_kernels.refresh_before_deploy && !runtime_kernels.enable {
            return Err(ConfctlError::Configuration(
                "runtimeKernels.refreshBeforeDeploy requires runtimeKernels.enable".into(),
            ));
        }
        let state = StateFiles::new(root.clone());
        if runtime_kernels.enable {
            state.validate(runtime_kernels.state_file.as_ref().ok_or_else(|| {
                ConfctlError::Configuration(
                    "runtimeKernels.stateFile is required when enabled".into(),
                )
            })?)?;
        }
        if netboot_inventory.enable {
            state.validate(netboot_inventory.output_file.as_ref().ok_or_else(|| {
                ConfctlError::Configuration(
                    "netbootInventory.outputFile is required when enabled".into(),
                )
            })?)?;
        }
        if runtime_kernels.state_file.is_some()
            && runtime_kernels.state_file == netboot_inventory.output_file
        {
            return Err(ConfctlError::Configuration(
                "runtime kernel and netboot destinations must differ".into(),
            ));
        }
        let nix = raw.get("nix").cloned().unwrap_or_default();
        let max_jobs = match nix.get("maxJobs") {
            None | Some(Value::Null) => None,
            Some(Value::String(s)) if s == "auto" => Some(s.clone()),
            Some(Value::Number(n)) if n.is_i64() || n.is_u64() => Some(n.to_string()),
            _ => {
                return Err(ConfctlError::Configuration(
                    "nix.maxJobs must be an integer, auto or null".into(),
                ));
            }
        };
        let legacy_names = strings(nix.get("legacyNixPathMap"))?
            .unwrap_or_else(|| vec!["nixpkgs".into(), "vpsadminos".into(), "vpsadmin".into()]);
        Ok(Self {
            list_columns: strings(raw.pointer("/list/columns"))?
                .unwrap_or_else(|| vec!["name".into(), "spin".into(), "host.target".into()]),
            nix: NixSettings {
                max_jobs,
                impure_eval: boolean(&nix, "impureEval")?,
                legacy_nix_path: boolean(&nix, "legacyNixPath")?,
                legacy_names,
            },
            build_generations: raw.get("buildGenerations").cloned().unwrap_or_default(),
            host_generations: raw.get("hostGenerations").cloned().unwrap_or_default(),
            runtime_kernels,
            netboot_inventory,
            raw,
        })
    }
}
fn boolean(value: &Value, key: &str) -> Result<bool, ConfctlError> {
    match value.get(key) {
        None => Ok(false),
        Some(Value::Bool(v)) => Ok(*v),
        _ => Err(ConfctlError::Configuration(format!(
            "{key} must be a boolean"
        ))),
    }
}
fn relative(value: &Value, key: &str) -> Result<Option<ConfigRelativePath>, ConfctlError> {
    match value.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(path)) => ConfigRelativePath::new(path).map(Some),
        _ => Err(ConfctlError::Configuration(format!(
            "{key} must be a relative path or null"
        ))),
    }
}
fn strings(value: Option<&Value>) -> Result<Option<Vec<String>>, ConfctlError> {
    match value {
        None => Ok(None),
        Some(Value::Array(items)) => items
            .iter()
            .map(|item| {
                item.as_str().map(str::to_owned).ok_or_else(|| {
                    ConfctlError::Configuration("expected string array in settings".into())
                })
            })
            .collect::<Result<_, _>>()
            .map(Some),
        _ => Err(ConfctlError::Configuration(
            "expected string array in settings".into(),
        )),
    }
}
pub fn demodulify(value: &mut Value) {
    match value {
        Value::Object(object) => {
            object.remove("_module");
            for value in object.values_mut() {
                demodulify(value);
            }
        }
        Value::Array(values) => {
            for value in values {
                demodulify(value);
            }
        }
        _ => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    #[test]
    fn old_settings_disable_features_and_preserve_unknown_metadata() {
        let dir = tempfile::tempdir().unwrap();
        let settings = Settings::decode(
            json!({"_module":{},"custom":{"flag":true,"_module":{}},"nix":{"maxJobs":"auto"}}),
            &ConfigRoot::new(dir.path()).unwrap(),
        )
        .unwrap();
        assert!(!settings.runtime_kernels.enable && !settings.netboot_inventory.enable);
        assert_eq!(settings.raw["custom"], json!({"flag":true}));
        assert_eq!(settings.nix.max_jobs.as_deref(), Some("auto"));
    }
    #[test]
    fn enabled_features_require_paths_and_refresh_requires_enable() {
        let dir = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(dir.path()).unwrap();
        for value in [
            json!({"runtimeKernels":{"enable":true}}),
            json!({"runtimeKernels":{"refreshBeforeDeploy":true}}),
            json!({"netbootInventory":{"enable":true,"outputFile":"flake.nix"}}),
        ] {
            assert!(Settings::decode(value, &root).is_err());
        }
    }
}
