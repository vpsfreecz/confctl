//! Existing Nix installables and argv are the interface. Only the two narrowly
//! recognized old-Nix diagnostics permit a retry; failed builds are not retried
//! as a general availability policy.

use crate::{
    configuration::{ConfigEpoch, ConfigRoot, EpochValue, Settings, demodulify},
    domain::Inventory,
    error::{ConfctlError, NixCommandContext, NixCommandFailure},
    events::TaskId,
    process::{
        Captured, PartialProcessEvidence, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec,
        TerminationCause,
    },
    timing::Phase,
};
use serde_json::Value;
use std::{collections::HashMap, sync::Mutex};
use tokio::sync::Mutex as AsyncMutex;
use tokio_util::sync::CancellationToken;

pub(crate) mod progress;

struct BuildInputs<'a> {
    installables: &'a [String],
    legacy_args: &'a [String],
    task: Option<TaskId>,
}

#[derive(Clone, Debug, Default)]
pub struct NixOptions {
    pub show_trace: bool,
    pub max_jobs: Option<String>,
    pub cores: Option<String>,
}
#[derive(Default)]
struct Cache {
    inventory: Option<Inventory>,
    json: HashMap<String, Value>,
}

/// Ruby's invocation-wide Settings snapshot, independent of every Nix object.
/// Construction performs no I/O; even concurrent first readers share one
/// successful stateless evaluation. Failed reads remain retryable.
pub struct InvocationSettings<P> {
    bootstrap: NixClient<P>,
}
impl<P: ProcessRunner> InvocationSettings<P> {
    pub fn new(root: ConfigRoot, runner: P, cancellation: CancellationToken) -> Self {
        Self {
            bootstrap: NixClient::stateless(
                root,
                runner,
                NixOptions::default(),
                cancellation,
                ConfigEpoch::default(),
            ),
        }
    }

    pub async fn read(&self) -> Result<Settings, ConfctlError> {
        Ok(self.bootstrap.settings().await?.value)
    }
}

pub struct NixClient<P> {
    root: ConfigRoot,
    runner: P,
    options: NixOptions,
    cancellation: CancellationToken,
    epoch: ConfigEpoch,
    settings: AsyncMutex<Option<Settings>>,
    cache: Mutex<Cache>,
}
impl<P: ProcessRunner> NixClient<P> {
    /// Capture constructor defaults at the matching Ruby Nix.new boundary.
    /// Explicit jobs, including an empty string, bypass invocation settings.
    pub async fn configured<Q: ProcessRunner>(
        root: ConfigRoot,
        runner: P,
        mut options: NixOptions,
        cancellation: CancellationToken,
        defaults: &InvocationSettings<Q>,
        epoch: ConfigEpoch,
    ) -> Result<Self, ConfctlError> {
        if root != defaults.bootstrap.root {
            return Err(ConfctlError::Configuration(
                "Nix client and invocation settings have different configuration roots".into(),
            ));
        }
        if options.max_jobs.is_none() {
            options.max_jobs = defaults.read().await?.nix.max_jobs;
        }
        Ok(Self::captured(root, runner, options, cancellation, epoch))
    }

    /// Explicit stateless calls have their own flags and default jobs to auto.
    /// This never consults invocation defaults or constructs a configured client.
    pub fn stateless(
        root: ConfigRoot,
        runner: P,
        mut options: NixOptions,
        cancellation: CancellationToken,
        epoch: ConfigEpoch,
    ) -> Self {
        options.max_jobs.get_or_insert_with(|| "auto".into());
        Self::captured(root, runner, options, cancellation, epoch)
    }

    fn captured(
        root: ConfigRoot,
        runner: P,
        options: NixOptions,
        cancellation: CancellationToken,
        epoch: ConfigEpoch,
    ) -> Self {
        Self {
            root,
            runner,
            options,
            cancellation,
            epoch,
            settings: AsyncMutex::new(None),
            cache: Mutex::new(Cache::default()),
        }
    }
    pub fn root(&self) -> &ConfigRoot {
        &self.root
    }
    pub fn epoch(&self) -> ConfigEpoch {
        self.epoch
    }
    pub fn invalidate_configuration(&mut self) -> ConfigEpoch {
        self.epoch.0 = self
            .epoch
            .0
            .checked_add(1)
            .expect("configuration epoch exhausted");
        *self.cache.get_mut().unwrap_or_else(|e| e.into_inner()) = Cache::default();
        *self.settings.get_mut() = None;
        self.epoch
    }
    pub async fn settings(&self) -> Result<EpochValue<Settings>, ConfctlError> {
        let mut cached = self.settings.lock().await;
        if let Some(value) = cached.as_ref() {
            return Ok(EpochValue {
                epoch: self.epoch,
                value: value.clone(),
            });
        }
        self.root.require_flake()?;
        // Each client evaluates pure settings using its captured arguments.
        // The invocation's separate stateless client supplies auto/no trace/cores.
        let raw = self
            .query(
                (".#confctl.settings", None),
                false,
                self.options.max_jobs.as_deref(),
                false,
                None,
                &self.options,
            )
            .await?;
        let value = Settings::decode(raw, &self.root)?;
        *cached = Some(value.clone());
        Ok(EpochValue {
            epoch: self.epoch,
            value,
        })
    }
    pub async fn inventory(&self) -> Result<EpochValue<Inventory>, ConfctlError> {
        if let Some(value) = self
            .cache
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .inventory
            .clone()
        {
            return Ok(EpochValue {
                epoch: self.epoch,
                value,
            });
        }
        let output = self
            .build_json(&[".#confctl.machinesJson".into()], &[])
            .await?;
        let path = output
            .pointer("/0/outputs/out")
            .and_then(Value::as_str)
            .ok_or_else(|| {
                ConfctlError::State("missing output path for .#confctl.machinesJson".into())
            })?;
        let bytes = tokio::fs::read(path)
            .await
            .map_err(|source| ConfctlError::Io {
                operation: "read Nix machine inventory",
                path: Some(path.into()),
                source,
            })?;
        let mut raw: Value =
            serde_json::from_slice(&bytes).map_err(|source| ConfctlError::Decode {
                context: "Nix machine inventory".into(),
                source,
            })?;
        demodulify(&mut raw);
        let value = Inventory::from_value(raw)?;
        self.cache
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .inventory = Some(value.clone());
        Ok(EpochValue {
            epoch: self.epoch,
            value,
        })
    }
    pub async fn eval_json(&self, installable: &str) -> Result<Value, ConfctlError> {
        self.eval_apply(installable, None).await
    }
    pub async fn eval_apply(
        &self,
        installable: &str,
        apply: Option<&str>,
    ) -> Result<Value, ConfctlError> {
        self.root.require_flake()?;
        let settings = self.settings().await?.value;
        self.query(
            (installable, apply),
            settings.nix.impure_eval,
            self.options.max_jobs.as_deref(),
            false,
            None,
            &self.options,
        )
        .await
    }
    pub async fn cached_json(&self, installable: &str) -> Result<EpochValue<Value>, ConfctlError> {
        if let Some(value) = self
            .cache
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .json
            .get(installable)
            .cloned()
        {
            return Ok(EpochValue {
                epoch: self.epoch,
                value,
            });
        }
        let value = self.eval_json(installable).await?;
        self.cache
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .json
            .insert(installable.into(), value.clone());
        Ok(EpochValue {
            epoch: self.epoch,
            value,
        })
    }
    pub async fn machine_key(&self, name: &str) -> Result<String, ConfctlError> {
        if let Some(inventory) = self
            .cache
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .inventory
            .as_ref()
            && let Some(machine) = inventory
                .machines
                .iter()
                .find(|machine| machine.name == name || machine.key.as_deref() == Some(name))
        {
            return Ok(machine.key.clone().unwrap_or_else(|| machine.name.clone()));
        }
        let mapping = self.cached_json(".#confctl.machineKeys").await?.value;
        if let Some(key) = mapping.get(name).and_then(Value::as_str) {
            return Ok(key.into());
        }
        if mapping
            .as_object()
            .is_some_and(|map| map.values().any(|key| key.as_str() == Some(name)))
        {
            return Ok(name.into());
        }
        Err(ConfctlError::Selection(format!("Unknown machine {name:?}")))
    }
    pub async fn build_json(
        &self,
        installables: &[String],
        legacy_args: &[String],
    ) -> Result<Value, ConfctlError> {
        self.build_json_task(installables, legacy_args, None).await
    }
    pub async fn build_json_for_task(
        &self,
        installables: &[String],
        legacy_args: &[String],
        task: TaskId,
    ) -> Result<Value, ConfctlError> {
        self.build_json_task(installables, legacy_args, Some(task))
            .await
    }
    async fn build_json_task(
        &self,
        installables: &[String],
        legacy_args: &[String],
        task: Option<TaskId>,
    ) -> Result<Value, ConfctlError> {
        self.root.require_flake()?;
        let settings = self.settings().await?.value;
        self.query(
            ("", None),
            settings.nix.impure_eval,
            self.options.max_jobs.as_deref(),
            true,
            Some(BuildInputs {
                installables,
                legacy_args,
                task,
            }),
            &self.options,
        )
        .await
    }
    async fn query(
        &self,
        target: (&str, Option<&str>),
        impure: bool,
        max_jobs: Option<&str>,
        build: bool,
        build_inputs: Option<BuildInputs<'_>>,
        options: &NixOptions,
    ) -> Result<Value, ConfctlError> {
        let (installable, apply) = target;
        let mut experimental = false;
        let mut no_update = true;
        loop {
            let mut args = if build {
                vec!["build".into(), "--json".into(), "--no-link".into()]
            } else {
                vec!["eval".into(), "--json".into()]
            };
            args.extend(common_args(
                options,
                impure,
                max_jobs,
                experimental,
                no_update,
            ));
            if build {
                if let Some(inputs) = &build_inputs {
                    args.extend(inputs.legacy_args.iter().cloned());
                    args.extend(inputs.installables.iter().cloned());
                }
            } else {
                if let Some(apply) = apply {
                    args.extend(["--apply".into(), apply.into()]);
                }
                args.push(installable.into());
            }
            let mut spec = ProcessSpec::new("nix", if build { "nix build" } else { "nix eval" });
            spec.args = args.into_iter().map(Into::into).collect();
            spec.cwd = Some(self.root.path().into());
            let context = NixCommandContext {
                cwd: self.root.path().into(),
                program: spec.program.as_os_str().into(),
                args: spec.args.clone(),
            };
            let build_task = build_inputs.as_ref().and_then(|inputs| inputs.task);
            let mut scope =
                ProcessScope::new(self.cancellation.clone(), build_task.unwrap_or(TaskId(0)));
            if build_task.is_some() {
                spec.stderr = crate::process::StreamPolicy::CaptureAndForward;
                scope.nix_progress = Some(crate::nix::progress::Kind::Build);
            }
            scope.phase = Some(if build {
                Phase::Build
            } else {
                Phase::Evaluation
            });
            let result = match self.runner.run(spec, scope).await {
                Ok(result) => result,
                Err(source) => return Err(nix_failure(context, source)),
            };
            if result.termination == TerminationCause::Canceled {
                return Err(nix_failure(
                    context,
                    process_error(ConfctlError::Cancelled, result),
                ));
            }
            if result.status.success() {
                let out = match bytes(&result.stdout).await {
                    Ok(bytes) => bytes,
                    Err(error) => return Err(nix_failure(context, process_error(error, result))),
                };
                return serde_json::from_slice(&out).map_err(|source| {
                    nix_failure(
                        context,
                        process_error(
                            ConfctlError::Decode {
                                context: "Nix JSON output".into(),
                                source,
                            },
                            result,
                        ),
                    )
                });
            }
            let mut diagnostics = match bytes(&result.stdout).await {
                Ok(bytes) => bytes,
                Err(error) => return Err(nix_failure(context, process_error(error, result))),
            };
            match bytes(&result.stderr).await {
                Ok(bytes) => diagnostics.extend(bytes),
                Err(error) => return Err(nix_failure(context, process_error(error, result))),
            }
            let message = String::from_utf8_lossy(&diagnostics);
            if no_update && no_update_error(&message) {
                no_update = false;
                continue;
            }
            if !experimental && experimental_error(&message) {
                experimental = true;
                continue;
            }
            return Err(nix_failure(
                context,
                process_error(
                    ConfctlError::Child {
                        description: if build { "nix build" } else { "nix eval" }.into(),
                        code: result.code(),
                        signal: result.signal(),
                        log: None,
                    },
                    result,
                ),
            ));
        }
    }
}
fn nix_failure(context: NixCommandContext, source: ConfctlError) -> ConfctlError {
    ConfctlError::NixCommandFailure(Box::new(NixCommandFailure {
        context,
        source: Box::new(source),
    }))
}
pub fn common_args(
    options: &NixOptions,
    impure: bool,
    max_jobs: Option<&str>,
    experimental: bool,
    no_update: bool,
) -> Vec<String> {
    let mut args = Vec::new();
    if experimental {
        args.extend(
            [
                "--extra-experimental-features",
                "nix-command",
                "--extra-experimental-features",
                "flakes",
            ]
            .map(str::to_owned),
        );
    }
    if impure {
        args.push("--impure".into());
    }
    args.push("--no-write-lock-file".into());
    if no_update {
        args.push("--no-update-lock-file".into());
    }
    if options.show_trace {
        args.push("--show-trace".into());
    }
    if let Some(jobs) = max_jobs {
        args.extend(["--option".into(), "max-jobs".into(), jobs.into()]);
    }
    if let Some(cores) = &options.cores {
        args.extend(["--option".into(), "cores".into(), cores.clone()]);
    }
    args
}
pub fn selected_apply(keys: &[String]) -> String {
    let mut unique = Vec::new();
    for key in keys {
        if !unique.contains(key) {
            unique.push(key.clone());
        }
    }
    let list = unique
        .iter()
        .map(|key| {
            format!(
                "\"{}\"",
                key.replace('\\', "\\\\")
                    .replace('"', "\\\"")
                    .replace("${", "\\${")
            )
        })
        .collect::<Vec<_>>()
        .join(" ");
    format!("x: builtins.listToAttrs (map (k: {{ name = k; value = x.${{k}}; }}) [ {list} ])")
}
fn no_update_error(message: &str) -> bool {
    let lower = message.to_lowercase();
    message.contains("--no-update-lock-file")
        && ["unknown", "unrecognized", "invalid", "unsupported"]
            .iter()
            .any(|word| lower.contains(word))
}
fn experimental_error(message: &str) -> bool {
    let lower = message.to_lowercase();
    lower.contains("experimental") && (lower.contains("nix-command") || lower.contains("flakes"))
}
pub async fn bytes(capture: &Captured) -> Result<Vec<u8>, ConfctlError> {
    match capture {
        Captured::Unavailable => Ok(vec![]),
        Captured::Memory(bytes) => Ok(bytes.clone()),
        Captured::Spool { file, .. } => {
            tokio::fs::read(file.path())
                .await
                .map_err(|source| ConfctlError::Io {
                    operation: "read process capture",
                    path: Some(file.path().into()),
                    source,
                })
        }
    }
}
pub(crate) fn process_error(source: ConfctlError, result: ProcessOutcome) -> ConfctlError {
    ConfctlError::ProcessFailure {
        source: Box::new(source),
        evidence: Box::new(PartialProcessEvidence {
            status: Some(result.status),
            stdout: result.stdout,
            stderr: result.stderr,
            stdout_complete: result.stdout_complete,
            stderr_complete: result.stderr_complete,
            log: None,
        }),
    }
}

#[cfg(test)]
mod tests;
