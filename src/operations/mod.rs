//! Command dispatch admits only concrete adapters. Unsupported development
//! routes stay explicit failures before timing/UI/state construction.

mod build;
mod changes;
mod connection;
mod cssh;
mod deploy;
mod generation_list;
mod health_check;
mod health_systemd;
mod host_status;
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
                Action::GenerationList { .. } | Action::Status { .. }
            )
            || matches!(
                action,
                Action::InputsList { .. }
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
            action => {
                return Err(ConfctlError::Configuration(format!(
                    "Rust operation '{}' is not implemented yet",
                    action.name()
                )));
            }
        }
        Ok(ExitDisposition::completed())
    }
}

#[cfg(test)]
pub(crate) fn set_build_test_roots(roots: crate::generation::GcRoots) {
    build::set_test_roots(roots);
}
