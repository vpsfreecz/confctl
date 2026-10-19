use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot, MutationLock, StateFiles},
    domain::{Action, SelectionPolicy, select},
    error::ConfctlError,
    events::{OutputStream, Style, TaskId, UiEvent},
    nix::{InvocationSettings, NixClient, NixOptions},
    runtime_kernels::{FeatureContext, refresh},
    terminal::ConfirmPrompt,
};

pub(super) async fn update(
    action: &Action,
    root: ConfigRoot,
    context: &Context,
) -> Result<(), ConfctlError> {
    let Action::RuntimeKernelsUpdate {
        targets,
        show_trace,
    } = action
    else {
        unreachable!("runtime kernel adapter receives its typed action")
    };
    let defaults = InvocationSettings::new(
        root.clone(),
        context.processes.clone(),
        context.cancellation.clone(),
    );
    let nix = NixClient::configured(
        root.clone(),
        context.processes.clone(),
        NixOptions {
            show_trace: *show_trace,
            ..NixOptions::default()
        },
        context.cancellation.clone(),
        &defaults,
        ConfigEpoch::default(),
    )
    .await?;
    let inventory = nix.inventory().await?;
    let inventory = inventory.require_epoch(nix.epoch())?;
    let selected = select(
        inventory,
        &targets.selection,
        SelectionPolicy {
            vpsadminos_only: true,
            ..SelectionPolicy::ALL
        },
    )?;
    let settings = defaults.read().await?;
    if !context
        .ui
        .confirm(ConfirmPrompt {
            preview: vec![
                UiEvent::Diagnostic {
                    stream: OutputStream::Stdout,
                    style: Style::Plain,
                    text: "The following machines will be queried:\n".into(),
                },
                super::machines::table(inventory, &selected, &settings.list_columns, true),
            ],
            assume_yes: targets.yes,
            ..ConfirmPrompt::default()
        })
        .await?
    {
        return Err(ConfctlError::State("Aborted".into()));
    }
    let tasks = selected
        .machines
        .iter()
        .enumerate()
        .map(|(index, machine)| (*machine, TaskId(index as u64 + 1)))
        .collect::<Vec<_>>();
    let guard = MutationLock::acquire(&root, &context.cancellation).await?;
    let files = StateFiles::new(root);
    let ssh_config = std::env::var_os("CONFCTL_SSH_CONFIG");
    refresh(
        &FeatureContext {
            runner: &context.processes,
            events: &context.events,
            files: &files,
            inventory,
            settings: &settings.runtime_kernels,
            cancellation: &context.cancellation,
            mutation: &guard,
            ssh_config: ssh_config.as_deref(),
        },
        &tasks,
    )
    .await?;
    Ok(())
}
