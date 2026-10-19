use super::{CliFailure, Values, failure};
use crate::domain::*;
use std::path::PathBuf;

impl Values<'_> {
    fn selection(&self) -> Result<SelectionRequest, CliFailure> {
        let attributes = self
            .all("attr")?
            .iter()
            .map(|s| {
                AttributeFilter::parse(s)
                    .map_err(|error| failure(self.path, &error.to_string(), true))
            })
            .collect::<Result<_, _>>()?;
        Ok(SelectionRequest {
            pattern: self.arg(0)?,
            attributes,
            tags: self.all("tag")?,
        })
    }
    fn targets(&self) -> Result<Targets, CliFailure> {
        Ok(Targets {
            selection: self.selection()?,
            yes: self.flag("yes"),
        })
    }
    fn nix(&self) -> Result<NixBuildOptions, CliFailure> {
        Ok(NixBuildOptions {
            max_jobs: self.string("max-jobs")?,
            cores: self.string("cores")?,
        })
    }
    fn changes(&self) -> InputChanges {
        InputChanges {
            commit: self.flag("commit"),
            changelog: self.flag("changelog"),
            editor: self.flag("editor"),
            downgrade: self.flag("downgrade"),
        }
    }
    fn managed(&self) -> Result<ManagedFilter, CliFailure> {
        Ok(match self.string("managed")?.as_deref() {
            Some("n" | "no") => ManagedFilter::Unmanaged,
            Some("a" | "all") => ManagedFilter::All,
            _ => ManagedFilter::Managed,
        })
    }
    fn generation(&self) -> Result<Option<GenerationSelector>, CliFailure> {
        Ok(self.string("generation")?.map(GenerationSelector::parse))
    }
    fn locations(&self) -> GenerationLocations {
        GenerationLocations::new(self.flag("local"), self.flag("remote"))
    }
    fn unified(&self) -> Result<UnifiedGenerationSelector, CliFailure> {
        Ok(match self.arg(1)?.as_deref() {
            None => UnifiedGenerationSelector::All,
            Some("current") => UnifiedGenerationSelector::Current,
            Some("old") => UnifiedGenerationSelector::Old,
            Some(s)
                if {
                    let digits = s.strip_prefix('-').unwrap_or(s);
                    !digits.is_empty() && digits.bytes().all(|byte| byte.is_ascii_digit())
                } =>
            {
                UnifiedGenerationSelector::Offset(s.into())
            }
            Some(s)
                if s.strip_suffix('d').is_some_and(|days| {
                    !days.is_empty() && days.bytes().all(|c| c.is_ascii_digit())
                }) =>
            {
                UnifiedGenerationSelector::OlderThanDays(s[..s.len() - 1].into())
            }
            Some(s) => UnifiedGenerationSelector::Pattern(s.into()),
        })
    }
    fn gc(&self) -> GarbageCollectionOptions {
        GarbageCollectionOptions {
            enabled: self.flag("collect-garbage"),
            max_concurrent: self.integer("max-concurrent-gc", 5),
        }
    }
    fn build(&self) -> Result<BuildRequest, CliFailure> {
        Ok(BuildRequest {
            targets: self.targets()?,
            nix: self.nix()?,
            show_trace: self.flag("show-trace"),
        })
    }
    fn change(&self) -> Result<ChangeRequest, CliFailure> {
        Ok(ChangeRequest {
            targets: self.targets()?,
            nix: self.nix()?,
            generation: self.generation()?,
            role_pattern: self.arg(1)?,
            downgrade: self.flag("downgrade"),
        })
    }
    fn required(&self, index: usize) -> Result<String, CliFailure> {
        self.arg(index)?
            .ok_or_else(|| failure(self.path, "missing required argument", true))
    }
}

pub(super) fn build(v: &Values<'_>) -> Result<Action, CliFailure> {
    if v.path == "inputs update" && v.positional.is_empty() && !v.flag("all") {
        return Err(failure(v.path, "missing input name (or pass --all)", true));
    }
    Ok(match v.path {
        "init" => Action::Init,
        "add" => Action::Add {
            name: PathBuf::from(&v.positional[0]),
        },
        "rename" => Action::Rename {
            old: PathBuf::from(&v.positional[0]),
            new: PathBuf::from(&v.positional[1]),
        },
        "rediscover" => Action::Rediscover,
        "inputs ls" => Action::InputsList { pattern: v.arg(0)? },
        "inputs update" => Action::InputsUpdate {
            inputs: (0..v.positional.len())
                .map(|i| v.required(i))
                .collect::<Result<_, _>>()?,
            all: v.flag("all"),
            changes: v.changes(),
        },
        "inputs set" => Action::InputsSet {
            input: v.required(0)?,
            revision: v.required(1)?,
            changes: v.changes(),
        },
        "inputs channel ls" => Action::ChannelList {
            pattern: v.arg(0)?,
            has_extra_arguments: v.positional.len() > 1,
        },
        "inputs channel update" => Action::ChannelUpdate {
            channels: v.required(0)?,
            role: v.arg(1)?,
            changes: v.changes(),
        },
        "inputs channel set" => Action::ChannelSet {
            channels: v.required(0)?,
            role: v.required(1)?,
            revision: v.required(2)?,
            allow_shared: v.flag("allow_shared"),
            changes: v.changes(),
        },
        "inputs machine update" => Action::MachineUpdate {
            machine: v.required(0)?,
            role: v.required(1)?,
            changes: v.changes(),
        },
        "inputs machine set" => Action::MachineSet {
            machine: v.required(0)?,
            role: v.required(1)?,
            revision: v.required(2)?,
            changes: v.changes(),
        },
        "ls" => Action::List {
            selection: v.selection()?,
            managed: v.managed()?,
            show_trace: v.flag("show-trace"),
            list_attributes: v.flag("list"),
            output: v
                .string("output")?
                .map(|s| s.split(',').map(str::to_owned).collect()),
            hide_header: v.flag("hide-header"),
        },
        "build" => Action::Build(v.build()?),
        "deploy" => {
            let action = match v.arg(1)?.as_deref().unwrap_or("switch") {
                "switch" => SwitchAction::Switch,
                "boot" => SwitchAction::Boot,
                "test" => SwitchAction::Test,
                "dry-activate" => SwitchAction::DryActivate,
                other => return Err(failure(v.path, &format!("invalid action '{other}'"), true)),
            };
            let reboot = v.flag("reboot");
            if reboot && action != SwitchAction::Boot {
                return Err(failure(
                    v.path,
                    "--reboot can be used only with switch-action boot",
                    true,
                ));
            }
            let wait = v.string("wait-online")?.unwrap_or_else(|| "600".into());
            let wait_online = if !reboot {
                WaitOnline::Unused(wait)
            } else {
                match wait.as_str() {
                    "wait" => WaitOnline::Wait,
                    "nowait" => WaitOnline::NoWait,
                    _ if !wait.is_empty() && wait.bytes().all(|c| c.is_ascii_digit()) => {
                        WaitOnline::Seconds(wait)
                    }
                    _ => return Err(failure(v.path, "invalid value of --wait-online", true)),
                }
            };
            // Ruby's disable option takes priority when both switches are set.
            let auto_rollback = if v.flag("disable-auto-rollback") {
                AutoRollback::Disabled
            } else if v.flag("enable-auto-rollback") {
                AutoRollback::Enabled
            } else {
                AutoRollback::Configured
            };
            Action::Deploy(DeployRequest {
                build: v.build()?,
                generation: v.generation()?,
                action,
                interactive: v.flag("interactive"),
                dry_activate_first: v.flag("dry-activate-first"),
                one_by_one: v.flag("one-by-one"),
                max_concurrent_copy: v.integer("max-concurrent-copy", 5),
                copy_only: v.flag("copy-only"),
                auto_rollback,
                reboot,
                wait_online,
                health_checks: v.flag("health-checks"),
                keep_going: v.flag("keep-going"),
            })
        }
        "health-check" => Action::HealthCheck {
            targets: v.targets()?,
            max_jobs: v.integer("max-jobs", 5),
        },
        "status" => Action::Status {
            targets: v.targets()?,
            skip_toplevel: v.string("generation")?.as_deref() == Some("none"),
            generation: v.generation()?,
        },
        "changelog" => Action::Changelog {
            change: v.change()?,
            verbose: v.flag("verbose"),
            patch: v.flag("patch"),
        },
        "diff" => Action::Diff(v.change()?),
        "test-connection" => Action::TestConnection {
            targets: v.targets()?,
            managed: v.managed()?,
        },
        "ssh" => {
            let input = match (v.value("input-string"), v.value("input-file")) {
                (Some(_), Some(_)) => {
                    return Err(failure(
                        v.path,
                        "use one of --input-string or --input-file",
                        true,
                    ));
                }
                (Some(bytes), None) => SshInput::Bytes(bytes.to_owned()),
                (None, Some(file)) => SshInput::File(file.into()),
                _ => SshInput::None,
            };
            Action::Ssh(SshRequest {
                targets: v.targets()?,
                managed: v.managed()?,
                command: v.positional.get(1..).unwrap_or_default().to_vec(),
                parallel: v.flag("parallel"),
                aggregate: v.flag("aggregate"),
                input,
            })
        }
        "cssh" => Action::Cssh {
            targets: v.targets()?,
            managed: v.managed()?,
        },
        "generation ls" => Action::GenerationList {
            selection: v.selection()?,
            locations: v.locations(),
            selector: v.unified()?,
        },
        "generation rm" => Action::GenerationRemove {
            targets: v.targets()?,
            locations: v.locations(),
            selector: v.unified()?,
            gc: v.gc(),
        },
        "generation rotate" => Action::GenerationRotate {
            targets: v.targets()?,
            locations: v.locations(),
            gc: v.gc(),
        },
        "collect-garbage" => Action::CollectGarbage {
            targets: v.targets()?,
            max_concurrent: v.integer("max-concurrent-gc", 5),
        },
        "gen-data vpsadmin all" => Action::GenDataAll,
        "gen-data vpsadmin containers" => Action::GenDataContainers,
        "gen-data vpsadmin network" => Action::GenDataNetwork,
        "runtime-kernels update" => Action::RuntimeKernelsUpdate {
            targets: v.targets()?,
            show_trace: v.flag("show-trace"),
        },
        _ => return Err(failure(v.path, "unregistered action", false)),
    })
}
