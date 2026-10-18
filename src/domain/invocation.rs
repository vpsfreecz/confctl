//! Parsed requests: operations consume these values without reparsing argv.

use std::{ffi::OsString, path::PathBuf};

use super::SelectionRequest;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum ColorMode {
    Always,
    Never,
    #[default]
    Auto,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct GlobalOptions {
    pub color: ColorMode,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Invocation {
    pub global: GlobalOptions,
    pub action: Action,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum ManagedFilter {
    #[default]
    Managed,
    Unmanaged,
    All,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Targets {
    pub selection: SelectionRequest,
    pub yes: bool,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct NixBuildOptions {
    /// Opaque Nix values, including "auto"; these are not confctl integers.
    pub max_jobs: Option<String>,
    pub cores: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct InputChanges {
    pub commit: bool,
    pub changelog: bool,
    pub editor: bool,
    pub downgrade: bool,
}

impl Default for InputChanges {
    fn default() -> Self {
        Self {
            commit: false,
            changelog: true,
            editor: true,
            downgrade: false,
        }
    }
}

/// Per-host resolution used by deploy/status/changelog, not unified listing.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum GenerationSelector {
    Current,
    Offset(i64),
    Name(String),
}

impl GenerationSelector {
    pub fn parse(value: String) -> Self {
        if value == "current" {
            Self::Current
        } else if let Ok(number) = value.parse() {
            Self::Offset(number)
        } else {
            Self::Name(value)
        }
    }
}

/// Listing/removal operates on a unified inventory with a different ordering.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum UnifiedGenerationSelector {
    All,
    Current,
    Old,
    Offset(i64),
    OlderThanDays(String),
    Pattern(String),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct GenerationLocations {
    pub local: bool,
    pub remote: bool,
}

impl GenerationLocations {
    pub fn new(local: bool, remote: bool) -> Self {
        Self {
            local: local || !remote,
            remote,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SwitchAction {
    Switch,
    Boot,
    Test,
    DryActivate,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WaitOnline {
    Seconds(String),
    Wait,
    NoWait,
    Unused(String),
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum AutoRollback {
    #[default]
    Configured,
    Enabled,
    Disabled,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BuildRequest {
    pub targets: Targets,
    pub nix: NixBuildOptions,
    pub show_trace: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeployRequest {
    pub build: BuildRequest,
    pub generation: Option<GenerationSelector>,
    pub action: SwitchAction,
    pub interactive: bool,
    pub dry_activate_first: bool,
    pub one_by_one: bool,
    pub max_concurrent_copy: i64,
    pub copy_only: bool,
    pub auto_rollback: AutoRollback,
    pub reboot: bool,
    pub wait_online: WaitOnline,
    pub health_checks: bool,
    pub keep_going: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ChangeRequest {
    pub targets: Targets,
    pub nix: NixBuildOptions,
    pub generation: Option<GenerationSelector>,
    pub role_pattern: Option<String>,
    pub downgrade: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum SshInput {
    None,
    Bytes(OsString),
    File(PathBuf),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SshRequest {
    pub targets: Targets,
    pub managed: ManagedFilter,
    pub command: Vec<OsString>,
    pub parallel: bool,
    pub aggregate: bool,
    pub input: SshInput,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GarbageCollectionOptions {
    pub enabled: bool,
    pub max_concurrent: i64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Action {
    Init,
    Add {
        name: PathBuf,
    },
    Rename {
        old: PathBuf,
        new: PathBuf,
    },
    Rediscover,
    InputsList {
        pattern: Option<String>,
    },
    InputsUpdate {
        inputs: Vec<String>,
        all: bool,
        changes: InputChanges,
    },
    InputsSet {
        input: String,
        revision: String,
        changes: InputChanges,
    },
    ChannelList {
        pattern: Option<String>,
        has_extra_arguments: bool,
    },
    ChannelUpdate {
        channels: String,
        role: Option<String>,
        changes: InputChanges,
    },
    ChannelSet {
        channels: String,
        role: String,
        revision: String,
        allow_shared: bool,
        changes: InputChanges,
    },
    MachineUpdate {
        machine: String,
        role: String,
        changes: InputChanges,
    },
    MachineSet {
        machine: String,
        role: String,
        revision: String,
        changes: InputChanges,
    },
    List {
        selection: SelectionRequest,
        managed: ManagedFilter,
        show_trace: bool,
        list_attributes: bool,
        output: Option<Vec<String>>,
        hide_header: bool,
    },
    Build(BuildRequest),
    Deploy(DeployRequest),
    HealthCheck {
        targets: Targets,
        max_jobs: i64,
    },
    Status {
        targets: Targets,
        generation: Option<GenerationSelector>,
        skip_toplevel: bool,
    },
    Changelog {
        change: ChangeRequest,
        verbose: bool,
        patch: bool,
    },
    Diff(ChangeRequest),
    TestConnection {
        targets: Targets,
        managed: ManagedFilter,
    },
    Ssh(SshRequest),
    Cssh {
        targets: Targets,
        managed: ManagedFilter,
    },
    GenerationList {
        selection: SelectionRequest,
        locations: GenerationLocations,
        selector: UnifiedGenerationSelector,
    },
    GenerationRemove {
        targets: Targets,
        locations: GenerationLocations,
        selector: UnifiedGenerationSelector,
        gc: GarbageCollectionOptions,
    },
    GenerationRotate {
        targets: Targets,
        locations: GenerationLocations,
        gc: GarbageCollectionOptions,
    },
    CollectGarbage {
        targets: Targets,
        max_concurrent: i64,
    },
    GenDataAll,
    GenDataContainers,
    GenDataNetwork,
    RuntimeKernelsUpdate {
        targets: Targets,
        show_trace: bool,
    },
}

impl Action {
    pub fn timing_admitted(&self) -> bool {
        match self {
            Self::Build(_)
            | Self::Deploy(_)
            | Self::HealthCheck { .. }
            | Self::Status { .. }
            | Self::TestConnection { .. }
            | Self::GenerationRemove { .. }
            | Self::GenerationRotate { .. }
            | Self::CollectGarbage { .. } => true,
            Self::Ssh(request) => !request.command.is_empty(),
            _ => false,
        }
    }

    pub fn name(&self) -> &'static str {
        match self {
            Self::Init => "init",
            Self::Add { .. } => "add",
            Self::Rename { .. } => "rename",
            Self::Rediscover => "rediscover",
            Self::InputsList { .. } => "inputs ls",
            Self::InputsUpdate { .. } => "inputs update",
            Self::InputsSet { .. } => "inputs set",
            Self::ChannelList { .. } => "inputs channel ls",
            Self::ChannelUpdate { .. } => "inputs channel update",
            Self::ChannelSet { .. } => "inputs channel set",
            Self::MachineUpdate { .. } => "inputs machine update",
            Self::MachineSet { .. } => "inputs machine set",
            Self::List { .. } => "ls",
            Self::Build(_) => "build",
            Self::Deploy(_) => "deploy",
            Self::HealthCheck { .. } => "health-check",
            Self::Status { .. } => "status",
            Self::Changelog { .. } => "changelog",
            Self::Diff(_) => "diff",
            Self::TestConnection { .. } => "test-connection",
            Self::Ssh(_) => "ssh",
            Self::Cssh { .. } => "cssh",
            Self::GenerationList { .. } => "generation ls",
            Self::GenerationRemove { .. } => "generation rm",
            Self::GenerationRotate { .. } => "generation rotate",
            Self::CollectGarbage { .. } => "collect-garbage",
            Self::GenDataAll => "gen-data vpsadmin all",
            Self::GenDataContainers => "gen-data vpsadmin containers",
            Self::GenDataNetwork => "gen-data vpsadmin network",
            Self::RuntimeKernelsUpdate { .. } => "runtime-kernels update",
        }
    }
}
