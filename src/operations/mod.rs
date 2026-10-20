//! Command dispatch admits only concrete adapters. Unsupported development
//! routes stay explicit failures before timing/UI/state construction.

mod build;
mod changes;
mod collect_garbage;
mod config_init;
mod configuration;
mod connection;
mod cssh;
mod deploy;
mod gen_data;
mod generation_cleanup;
mod generation_list;
mod generation_selection;
mod health_check;
mod health_systemd;
mod host_status;
mod input_git;
mod input_mutation;
mod input_observation;
mod inputs;
mod machines;
mod pager;
mod runtime_kernels;
mod ssh;
mod status;

use crate::{
    app::{Context, Operations},
    configuration::ConfigRoot,
    domain::{Action, ExitDisposition, Invocation},
    error::ConfctlError,
};

pub struct CoreOperations;
impl Operations for CoreOperations {
    fn available(&self, action: &Action) -> bool {
        matches!(action, Action::Deploy(_))
            || matches!(
                action,
                Action::GenerationList { .. }
                    | Action::GenerationRemove { .. }
                    | Action::GenerationRotate { .. }
                    | Action::CollectGarbage { .. }
                    | Action::Status { .. }
            )
            || matches!(
                action,
                Action::GenDataAll
                    | Action::GenDataContainers
                    | Action::GenDataNetwork
                    | Action::InputsList { .. }
                    | Action::InputsUpdate { .. }
                    | Action::InputsSet { .. }
                    | Action::ChannelUpdate { .. }
                    | Action::ChannelSet { .. }
                    | Action::MachineUpdate { .. }
                    | Action::MachineSet { .. }
                    | Action::Init
                    | Action::Add { .. }
                    | Action::Rename { .. }
                    | Action::Rediscover
                    | Action::List { .. }
                    | Action::ChannelList { .. }
                    | Action::RuntimeKernelsUpdate { .. }
                    | Action::Build(_)
                    | Action::TestConnection { .. }
                    | Action::Ssh(_)
                    | Action::Cssh { .. }
                    | Action::HealthCheck { .. }
                    | Action::Diff(_)
                    | Action::Changelog { .. }
            )
    }
    async fn run(
        &self,
        invocation: &Invocation,
        context: &Context,
    ) -> Result<ExitDisposition, ConfctlError> {
        let root = ConfigRoot::new(&context.cwd)?;
        match &invocation.action {
            Action::GenDataAll | Action::GenDataContainers | Action::GenDataNetwork => {
                gen_data::run(&invocation.action, root, context).await?
            }
            Action::InputsUpdate { .. }
            | Action::InputsSet { .. }
            | Action::ChannelUpdate { .. }
            | Action::ChannelSet { .. }
            | Action::MachineUpdate { .. }
            | Action::MachineSet { .. } => {
                input_mutation::run(&invocation.action, root, context).await?
            }
            Action::Init => config_init::run(root, context).await?,
            Action::Add { .. } | Action::Rename { .. } | Action::Rediscover => {
                configuration::run(&invocation.action, root, context).await?
            }
            Action::InputsList { pattern } => {
                inputs::list(&root, pattern.as_deref(), &context.events).await?
            }
            Action::ChannelList {
                pattern,
                has_extra_arguments,
            } => inputs::channels(&root, pattern.as_deref(), *has_extra_arguments, context).await?,
            Action::List { .. } => machines::list(&invocation.action, root, context).await?,
            Action::Status { .. } => status::run(&invocation.action, root, context).await?,
            Action::Diff(_) | Action::Changelog { .. } => {
                changes::run(&invocation.action, root, context).await?
            }
            Action::Build(request) => build::run(request, root, context).await?,
            Action::Deploy(request) => deploy::run(request, root, context).await?,
            Action::TestConnection { .. } => {
                connection::test(&invocation.action, root, context).await?
            }
            Action::GenerationRemove { .. } | Action::GenerationRotate { .. } => {
                generation_cleanup::run(&invocation.action, root, context).await?
            }
            Action::CollectGarbage { .. } => {
                collect_garbage::run(&invocation.action, root, context).await?
            }
            Action::GenerationList { .. } => {
                generation_list::list(&invocation.action, root, context).await?
            }
            Action::Ssh(_) => ssh::run(&invocation.action, root, context).await?,
            Action::Cssh { .. } => cssh::run(&invocation.action, root, context).await?,
            Action::HealthCheck { .. } => {
                health_check::run(&invocation.action, root, context).await?
            }
            Action::RuntimeKernelsUpdate { .. } => {
                runtime_kernels::update(&invocation.action, root, context).await?
            }
        }
        Ok(ExitDisposition::completed())
    }
}

#[cfg(test)]
pub(crate) fn set_build_test_roots(roots: crate::generation::GcRoots) {
    build::set_test_roots(roots);
}
