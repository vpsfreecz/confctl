//! Command dispatch admits only concrete adapters. Unsupported development
//! routes stay explicit failures before timing/UI/state construction.

mod inputs;
mod machines;

use crate::{
    app::{Context, Operations},
    configuration::ConfigRoot,
    domain::{Action, ExitDisposition, Invocation},
    error::ConfctlError,
};

pub struct CoreOperations;
impl Operations for CoreOperations {
    fn available(&self, action: &Action) -> bool {
        matches!(action, Action::InputsList { .. } | Action::List { .. })
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
            Action::List { .. } => machines::list(&invocation.action, root, context).await?,
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
