//! Authenticated read-only API data, with one guarded publication per complete file.
mod renderer;
use crate::{
    app::Context,
    configuration::{
        ConfigEpoch, ConfigRelativePath, ConfigRoot, FilePolicy, MutationGuard, MutationLock,
        StateFiles,
    },
    domain::{Action, ruby_string},
    error::ConfctlError,
    events::{TaskId, TaskState, UiEvent},
    nix::{InvocationSettings, NixClient, NixOptions},
    vpsadmin::{Client, check},
};
use serde_json::{Map, Value, json};
pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    let guard = MutationLock::acquire(&root, &ctx.cancellation).await?;
    let task = TaskId(1);
    let result = async {
        ctx.events
            .emit(UiEvent::TaskPending {
                task,
                description: action.name().into(),
            })
            .await?;
        check(&ctx.cancellation)?;
        ctx.events
            .emit(UiEvent::TaskStarted {
                task,
                description: action.name().into(),
            })
            .await?;
        check(&ctx.cancellation)?;
        let mut api = Client::new(&ctx.ui, &ctx.events, &ctx.cancellation)?;
        api.authenticate().await.map_err(|e| e.with_secondary(vec![ConfctlError::State("gen-data authentication phase failed".into())]))?;
        if matches!(action, Action::GenDataAll | Action::GenDataContainers) {
            containers(&root, &guard, ctx, &mut api).await.map_err(|e| e.with_secondary(vec![ConfctlError::State("gen-data containers phase failed; inspect containers.nix before rerunning".into())]))?;
        }
        if matches!(action, Action::GenDataAll | Action::GenDataNetwork) {
            networks(&root, &guard, ctx, &mut api).await.map_err(|e| e.with_secondary(vec![ConfctlError::State("gen-data networks phase failed; an earlier containers.nix replacement can remain".into())]))?;
        }
        check(&ctx.cancellation)
    }
    .await;
    let state = match &result {
        Ok(()) => TaskState::Succeeded,
        Err(e) if e.is_cancelled() => TaskState::Canceled,
        Err(_) => TaskState::Failed,
    };
    let delivery = ctx.events.emit(UiEvent::TaskFinished { task, state }).await;
    let delivery = delivery.and_then(|()| check(&ctx.cancellation));
    match (result, delivery) {
        (Err(e), Err(later)) => Err(e.with_secondary(vec![later])),
        (_, Err(e)) => Err(e),
        (result, Ok(())) => result,
    }
}
async fn containers(
    root: &ConfigRoot,
    guard: &MutationGuard,
    ctx: &Context,
    api: &mut Client<'_>,
) -> Result<(), ConfctlError> {
    let defaults = InvocationSettings::new(
        root.clone(),
        ctx.processes.clone(),
        ctx.cancellation.clone(),
    );
    let nix = NixClient::configured(
        root.clone(),
        ctx.processes.clone(),
        NixOptions::default(),
        ctx.cancellation.clone(),
        &defaults,
        ConfigEpoch::default(),
    )
    .await?;
    let inventory = nix.inventory().await?;
    let inventory = inventory.require_epoch(nix.epoch())?;
    let mut data = Map::new();
    for machine in &inventory.machines {
        check(&ctx.cancellation)?;
        let container = match machine.metadata.get("container") {
            None | Some(Value::Null) => continue,
            Some(v) => v.as_object().ok_or_else(|| {
                ConfctlError::Configuration("gen-data container metadata must be an object".into())
            })?,
        };
        let node = api
            .vps_node(container.get("id").cloned().unwrap_or(Value::Null))
            .await?;
        let fqdn = ["name", "location", "domain"]
            .iter()
            .filter_map(|k| machine.metadata.get("host").and_then(|v| v.get(k)))
            .filter(|v| !v.is_null())
            .map(ruby_string)
            .collect::<Vec<_>>()
            .join(".");
        data.insert(fqdn, node);
    }
    publish(
        root,
        guard,
        ctx,
        "data/vpsadmin/containers.nix",
        &Value::Object(data),
    )
}
async fn networks(
    root: &ConfigRoot,
    guard: &MutationGuard,
    ctx: &Context,
    api: &mut Client<'_>,
) -> Result<(), ConfctlError> {
    let networks = api.networks().await?;
    let mut data = Map::new();
    for version in [4, 6] {
        data.insert(
            format!("ipv{version}"),
            Value::Array(
                networks
                    .iter()
                    .filter(|n| {
                        n["ip_version"].as_number().is_some_and(|n| {
                            n.to_string().parse::<f64>().ok() == Some(version as f64)
                        })
                    })
                    .map(|n| json!({"address":n["address"],"prefix":n["prefix"]}))
                    .collect(),
            ),
        );
    }
    publish(
        root,
        guard,
        ctx,
        "data/vpsadmin/networks/containers.nix",
        &Value::Object(data),
    )
}
fn publish(
    root: &ConfigRoot,
    guard: &MutationGuard,
    ctx: &Context,
    path: &str,
    value: &Value,
) -> Result<(), ConfctlError> {
    let bytes = renderer::render(value)?;
    check(&ctx.cancellation)?;
    StateFiles::new(root.clone()).replace(
        guard,
        &ConfigRelativePath::new(path)?,
        &bytes,
        FilePolicy::GeneratedNix,
    )?;
    check(&ctx.cancellation)
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{events::EventSink, process::Supervisor, terminal::Ui, timing::Timing};
    use std::time::Duration;
    #[tokio::test]
    async fn gen_data_cancellation_before_publication_and_after_task_ack_keeps_matching_lifecycle()
    {
        for after_ack in [false, true] {
            let scratch = tempfile::tempdir().unwrap();
            let root = ConfigRoot::new(scratch.path()).unwrap();
            std::fs::create_dir_all(root.path().join("data/vpsadmin")).unwrap();
            let file = root.path().join("data/vpsadmin/containers.nix");
            std::fs::write(&file, b"old").unwrap();
            let cancel = tokio_util::sync::CancellationToken::new();
            let guard = MutationLock::acquire(&root, &cancel).await.unwrap();
            let (events, mut receiver) = EventSink::channel(1);
            let ui = Ui::new(
                std::fs::File::open("/dev/null").unwrap(),
                events.clone(),
                cancel.clone(),
                None,
            )
            .unwrap();
            let processes = Supervisor::new(
                events.clone(),
                scratch.path().into(),
                64,
                Duration::from_millis(30),
                None,
                None,
            );
            let ctx = Context::for_test(
                root.path().into(),
                cancel.clone(),
                events,
                processes,
                ui,
                Timing::start(),
                root.path().join("test.log"),
            );
            if after_ack {
                drop(guard);
                let token = cancel.clone();
                let consumer = tokio::spawn(async move {
                    let mut ids = Vec::new();
                    while let Some(delivery) = receiver.receive().await {
                        let (event, ack) = delivery.into_parts();
                        match event {
                            UiEvent::TaskPending { task, .. } => {
                                ids.push(task);
                                token.cancel();
                            }
                            UiEvent::TaskFinished { task, state } => {
                                assert_eq!(ids, [task]);
                                assert_eq!(state, TaskState::Canceled);
                                ids.clear();
                            }
                            _ => panic!(
                                "no prompts or HTTP after pending acknowledgment cancellation"
                            ),
                        };
                        ack.acknowledge(Ok(()));
                    }
                    assert!(ids.is_empty());
                });
                let error = run(&Action::GenDataContainers, root.clone(), &ctx)
                    .await
                    .unwrap_err();
                assert!(error.is_cancelled());
                ctx.processes.shutdown().await.unwrap();
                ctx.ui.shutdown().unwrap();
                drop(ctx);
                consumer.await.unwrap();
            } else {
                cancel.cancel();
                assert!(
                    publish(
                        &root,
                        &guard,
                        &ctx,
                        "data/vpsadmin/containers.nix",
                        &json!({"new":true})
                    )
                    .unwrap_err()
                    .is_cancelled()
                );
                ctx.processes.shutdown().await.unwrap();
                ctx.ui.shutdown().unwrap();
            }
            assert_eq!(std::fs::read(file).unwrap(), b"old");
        }
    }
}
