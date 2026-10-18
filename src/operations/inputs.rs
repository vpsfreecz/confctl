use crate::{
    configuration::ConfigRoot,
    domain::pattern_matches,
    error::ConfctlError,
    events::{EventSink, OutputStream, TableFormat, TableLayout, UiEvent},
    inputs::FlakeLock,
};

pub(super) async fn list(
    root: &ConfigRoot,
    pattern: Option<&str>,
    events: &EventSink,
) -> Result<(), ConfctlError> {
    root.require_flake()?;
    let lock = FlakeLock::load(&root.path().join("flake.lock"))?;
    events.emit(table(&lock, pattern)).await
}
fn table(lock: &FlakeLock, pattern: Option<&str>) -> UiEvent {
    let rows = lock
        .root_inputs()
        .into_iter()
        .filter(|input| pattern.is_none_or(|pattern| pattern_matches(pattern, input)))
        .map(|input| {
            let info = lock.input_info(&input);
            vec![
                input,
                info.kind,
                info.reference.unwrap_or_default(),
                info.short_rev.unwrap_or_default(),
                info.url.unwrap_or_default(),
            ]
        })
        .collect();
    UiEvent::Table {
        stream: OutputStream::Stdout,
        columns: ["INPUT", "TYPE", "REF", "REV", "URL"]
            .map(str::to_owned)
            .to_vec(),
        rows,
        cell_styles: Vec::new(),
        format: TableFormat {
            layout: TableLayout::Columns,
            ..TableFormat::default()
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn input_list_output_matches_all_four_immutable_follows_variations() {
        for variation in ["single", "nested", "different-suffix", "missing-first"] {
            let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(format!(
                "tests/compat/core/expected-follows/follows-{variation}-ls.json"
            ));
            let expected: serde_json::Value =
                serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
            let lock = FlakeLock::decode(
                expected["files"]["flake.lock"]["text"]
                    .as_str()
                    .unwrap()
                    .as_bytes(),
            )
            .unwrap();
            let UiEvent::Table {
                columns,
                rows,
                format,
                ..
            } = table(&lock, None)
            else {
                panic!()
            };
            assert_eq!(
                crate::presentation::plain::table(&columns, &rows, &format),
                expected["stdout"].as_str().unwrap().as_bytes(),
                "{variation}"
            );
            let UiEvent::Table { rows, .. } = table(&lock, Some("does-not-match")) else {
                panic!()
            };
            assert!(rows.is_empty());
        }
    }
}
