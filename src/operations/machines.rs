use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{Action, Inventory, Selection, SelectionPolicy, ruby_string, select},
    error::ConfctlError,
    events::{EventSink, OutputStream, TableFormat, TableLayout, TaskId, UiEvent},
    nix::{InvocationSettings, NixClient, NixOptions},
};
use serde_json::Value;

pub(super) async fn list(
    action: &Action,
    root: ConfigRoot,
    context: &Context,
) -> Result<(), ConfctlError> {
    let Action::List {
        selection,
        managed,
        list_attributes,
        output,
        hide_header,
        ..
    } = action
    else {
        unreachable!("list adapter receives its typed action")
    };
    let defaults = InvocationSettings::new(
        root.clone(),
        context.processes.clone(),
        context.cancellation.clone(),
    );
    // Both MachineList#extract and the attributes branch use plain Nix.new;
    // ls --show-trace is parsed but does not reach either constructor in Ruby.
    let nix = NixClient::configured(
        root,
        context.processes.clone(),
        NixOptions::default(),
        context.cancellation.clone(),
        &defaults,
        ConfigEpoch::default(),
    )
    .await?;
    if *list_attributes {
        emit(b"name\n".to_vec(), &context.events).await?;
        let attributes = nix.eval_json(".#confctl.moduleOptions").await?;
        let attributes = attributes
            .as_array()
            .ok_or_else(|| ConfctlError::Configuration("module options are not an array".into()))?;
        for attribute in attributes {
            let name = attribute
                .get("name")
                .and_then(Value::as_str)
                .ok_or_else(|| ConfctlError::Configuration("module option has no name".into()))?;
            if let Some(name) = name.strip_prefix("cluster.<name>.") {
                emit(format!("{name}\n").into_bytes(), &context.events).await?;
            }
        }
    } else {
        let inventory = nix.inventory().await?;
        let inventory = inventory.require_epoch(nix.epoch())?;
        let selected = select(
            inventory,
            selection,
            SelectionPolicy {
                managed: *managed,
                ..SelectionPolicy::ALL
            },
        )?;
        let columns = match output {
            Some(columns) => columns.clone(),
            None => defaults.read().await?.list_columns,
        };
        context
            .events
            .emit(table(inventory, &selected, &columns, !hide_header))
            .await?;
    }
    Ok(())
}
pub(super) fn table(
    inventory: &Inventory,
    selected: &Selection,
    columns: &[String],
    header: bool,
) -> UiEvent {
    UiEvent::Table {
        stream: OutputStream::Stdout,
        columns: columns.iter().map(|name| name.to_uppercase()).collect(),
        rows: selected
            .machines
            .iter()
            .map(|id| {
                let machine = inventory.get(*id);
                columns
                    .iter()
                    .map(|name| {
                        let value = machine.attribute(name);
                        // OutputFormatter treats nil and false as empty; other objects
                        // retain Ruby to_s, including array/hash inspection.
                        if matches!(value, Value::Null | Value::Bool(false)) {
                            String::new()
                        } else {
                            ruby_string(&value)
                        }
                    })
                    .collect()
            })
            .collect(),
        cell_styles: Vec::new(),
        format: TableFormat {
            header,
            layout: TableLayout::Columns,
            ..TableFormat::default()
        },
    }
}
async fn emit(bytes: Vec<u8>, events: &EventSink) -> Result<(), ConfctlError> {
    events
        .emit(UiEvent::Output {
            task: TaskId(0),
            stream: OutputStream::Stdout,
            bytes,
        })
        .await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        cli::{ParseOutcome, parse},
        domain::SelectionRequest,
    };

    #[test]
    fn list_output_and_selection_match_immutable_observations() {
        let inventory = Inventory::from_value(serde_json::json!({
            "alpha": {"name":"alpha","key":"alpha-key","metaConfig":{
                "managed":true,"spin":"nixos","host":{"target":"alpha.invalid"},"tags":["prod"]}},
            "reference": {"name":"reference","key":"reference-key","metaConfig":{
                "managed":false,"spin":"nixos","host":{"target":"reference.invalid"},"tags":["other"]}},
            "no-target": {"name":"no-target","key":"no-target-key","metaConfig":{
                "managed":true,"spin":"nixos","host":{"target":null},"tags":[]}}
        })).unwrap();
        for (name, argv) in [
            ("ls-default", vec!["ls"]),
            ("ls-all", vec!["ls", "--managed", "all"]),
            (
                "ls-filter",
                vec![
                    "ls",
                    "-H",
                    "-o",
                    "name,host.target",
                    "-a",
                    "spin=nixos",
                    "-t",
                    "prod",
                    "a*",
                ],
            ),
            (
                "ls-negative-filters",
                vec![
                    "ls",
                    "--no-hide-header",
                    "-a",
                    "spin!=vpsadminos",
                    "-t",
                    "^other",
                    "alpha-key",
                ],
            ),
            ("ls-surplus", vec!["ls", "alpha", "ignored"]),
        ] {
            let ParseOutcome::Run(invocation) =
                parse(argv.into_iter().map(Into::into).collect()).unwrap()
            else {
                panic!()
            };
            let Action::List {
                selection,
                managed,
                output,
                hide_header,
                ..
            } = invocation.action
            else {
                panic!()
            };
            let selected = select(
                &inventory,
                &selection,
                SelectionPolicy {
                    managed,
                    ..SelectionPolicy::ALL
                },
            )
            .unwrap();
            let columns =
                output.unwrap_or_else(|| vec!["name".into(), "spin".into(), "host.target".into()]);
            let UiEvent::Table {
                columns,
                rows,
                format,
                ..
            } = table(&inventory, &selected, &columns, !hide_header)
            else {
                panic!()
            };
            let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join(format!("tests/compat/core/expected/{name}.json"));
            let expected: Value = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
            assert_eq!(
                crate::presentation::plain::table(&columns, &rows, &format),
                expected["stdout"].as_str().unwrap().as_bytes(),
                "{name}"
            );
        }
        let empty = select(
            &inventory,
            &SelectionRequest {
                pattern: Some("absent".into()),
                ..SelectionRequest::default()
            },
            SelectionPolicy::ALL,
        )
        .unwrap();
        assert!(empty.machines.is_empty());
    }
}
