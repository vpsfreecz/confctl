use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot},
    domain::{pattern_matches, ruby_string},
    error::{ConfctlError, FlakeLockFailure},
    events::{EventSink, OutputStream, TableFormat, TableLayout, UiEvent},
    inputs::FlakeLock,
    nix::{InvocationSettings, NixClient, NixOptions},
};
use serde_json::{Map, Value};

pub(super) async fn list(
    root: &ConfigRoot,
    pattern: Option<&str>,
    events: &EventSink,
) -> Result<(), ConfctlError> {
    root.require_flake()?;
    let lock = FlakeLock::load(&root.path().join("flake.lock"))?;
    events.emit(table(&lock, pattern)).await
}

pub(super) async fn channels(
    root: &ConfigRoot,
    pattern: Option<&str>,
    has_extra_arguments: bool,
    context: &Context,
) -> Result<(), ConfctlError> {
    root.require_flake()?;
    // Ruby opens the operational log and checks the flake before rejecting
    // surplus arguments, but never reads the lock or constructs Nix for them.
    if has_extra_arguments {
        return Err(ConfctlError::Usage(
            "usage: confctl inputs channel ls [channel-pattern]".into(),
        ));
    }
    let path = root.path().join("flake.lock");
    let lock = FlakeLock::load(&path).map_err(|source| {
        ConfctlError::FlakeLockFailure(Box::new(FlakeLockFailure {
            path,
            source: Box::new(source),
        }))
    })?;
    let defaults = InvocationSettings::new(
        root.clone(),
        context.processes.clone(),
        context.cancellation.clone(),
    );
    let nix = NixClient::configured(
        root.clone(),
        context.processes.clone(),
        NixOptions::default(),
        context.cancellation.clone(),
        &defaults,
        ConfigEpoch::default(),
    )
    .await?;
    let value = nix.eval_json(".#confctl.channels").await?;
    context
        .events
        .emit(channels_table(&lock, &value, pattern)?)
        .await
}

fn channel_names<'a>(channels: &'a Map<String, Value>, pattern: Option<&str>) -> Vec<&'a str> {
    let Some(pattern) = pattern else {
        let mut names: Vec<_> = channels.keys().map(String::as_str).collect();
        names.sort();
        return names;
    };
    if pattern.contains(['{', ',']) {
        let value = ruby_strip(pattern);
        let value = value
            .strip_prefix('{')
            .and_then(|value| value.strip_suffix('}'))
            .unwrap_or(value);
        return value
            .split(',')
            .map(ruby_strip)
            .filter(|name| !name.is_empty())
            .filter_map(|name| channels.get_key_value(name).map(|(key, _)| key.as_str()))
            .collect();
    }
    let mut names: Vec<_> = channels
        .keys()
        .filter(|name| pattern_matches(pattern, name))
        .map(String::as_str)
        .collect();
    names.sort();
    names
}
fn ruby_strip(value: &str) -> &str {
    value.trim_matches(['\0', ' ', '\t', '\r', '\n', '\u{b}', '\u{c}'])
}
fn channels_table(
    lock: &FlakeLock,
    value: &Value,
    pattern: Option<&str>,
) -> Result<UiEvent, ConfctlError> {
    let mut rows = Vec::new();
    let absent = Value::Null;
    if let Some(channels) = value.as_object() {
        for channel in channel_names(channels, pattern) {
            match &channels[channel] {
                Value::Null | Value::Bool(false) => continue,
                Value::Object(roles) => {
                    for (role, input) in roles {
                        rows.push(channel_row(lock, channel, role.clone(), input));
                    }
                }
                Value::Array(roles) => {
                    // Array#each yields one element; Ruby destructures an
                    // Array element into the block's role/input parameters.
                    for entry in roles {
                        let (role, input) = if let Value::Array(fields) = entry {
                            (
                                fields.first().unwrap_or(&absent),
                                fields.get(1).unwrap_or(&absent),
                            )
                        } else {
                            (entry, &absent)
                        };
                        rows.push(channel_row(lock, channel, table_cell(role), input));
                    }
                }
                Value::Number(number) if !number.as_str().contains(['.', 'e', 'E']) => {
                    return Err(ConfctlError::Configuration(
                        "undefined method 'each' for an instance of Integer".into(),
                    ));
                }
                _ => {
                    return Err(ConfctlError::Configuration(format!(
                        "channel '{channel}' mapping is not an object"
                    )));
                }
            }
        }
    }
    Ok(UiEvent::Table {
        stream: OutputStream::Stdout,
        columns: ["CHANNEL", "ROLE", "INPUT", "REV", "URL"]
            .map(str::to_owned)
            .to_vec(),
        rows,
        cell_styles: Vec::new(),
        format: TableFormat {
            layout: TableLayout::Columns,
            ..TableFormat::default()
        },
    })
}
fn table_cell(value: &Value) -> String {
    if matches!(value, Value::Null | Value::Bool(false)) {
        String::new()
    } else {
        ruby_string(value)
    }
}
fn channel_row(lock: &FlakeLock, channel: &str, role: String, input: &Value) -> Vec<String> {
    let info = input.as_str().map(|input| lock.input_info(input));
    let field = |value: Option<String>| value.unwrap_or_else(|| "-".into());
    vec![
        channel.into(),
        role,
        table_cell(input),
        field(info.as_ref().and_then(|info| info.short_rev.clone())),
        field(info.and_then(|info| info.url)),
    ]
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
    fn channel_list_table_matches_frozen_default_and_keeps_explicit_selection_order() {
        let expected: Value = serde_json::from_str(include_str!(
            "../../tests/compat/core/expected/channel-ls.json"
        ))
        .unwrap();
        let lock = FlakeLock::decode(
            expected["files"]["flake.lock"]["text"]
                .as_str()
                .unwrap()
                .as_bytes(),
        )
        .unwrap();
        let channels = serde_json::json!({
            "other": {"nixpkgs": "nixpkgs"},
            "base": {"nixpkgs": "nixpkgs"}
        });
        let UiEvent::Table {
            columns,
            rows,
            format,
            ..
        } = channels_table(&lock, &channels, None).unwrap()
        else {
            panic!()
        };
        assert_eq!(
            crate::presentation::plain::table(&columns, &rows, &format),
            expected["stdout"].as_str().unwrap().as_bytes()
        );
        let channels = channels.as_object().unwrap();
        assert_eq!(
            channel_names(channels, Some(" { other, base, other,, absent } ")),
            ["other", "base", "other"]
        );
        assert_eq!(
            channel_names(channels, Some("other,base")),
            ["other", "base"]
        );
        assert_eq!(channel_names(channels, Some("b*")), ["base"]);
        assert!(channel_names(channels, Some("absent*")).is_empty());
    }

    #[test]
    fn channel_rows_keep_role_order_missing_fields_empty_strings_and_first_segment_follows() {
        let lock = FlakeLock::decode(
            br#"{
            "nodes": {
                "root": {"inputs": {"alias": ["target", "ignored"], "empty": "empty"}},
                "target": {"locked": {"rev": "abcdefgh-more", "url": "git+https://target.invalid"}},
                "empty": {"locked": {"rev": "", "url": ""}}
            }
        }"#,
        )
        .unwrap();
        let value = serde_json::json!({
            "channel": {"second": "alias", "first": "missing", "empty": "empty"},
            "null": null, "false": false
        });
        let UiEvent::Table { rows, .. } = channels_table(&lock, &value, None).unwrap() else {
            panic!()
        };
        assert_eq!(
            rows,
            vec![
                vec![
                    "channel",
                    "second",
                    "alias",
                    "abcdefgh",
                    "https://target.invalid"
                ],
                vec!["channel", "first", "missing", "-", "-"],
                vec!["channel", "empty", "empty", "", ""],
            ]
        );
        for value in [Value::Null, serde_json::json!([]), Value::Bool(true)] {
            let UiEvent::Table { rows, .. } = channels_table(&lock, &value, None).unwrap() else {
                panic!()
            };
            assert!(rows.is_empty());
        }
    }

    #[test]
    fn channel_array_mapping_destructures_roles_and_does_not_coerce_input_lookup_keys() {
        let lock = FlakeLock::decode(
            br#"{
            "nodes": {
                "root": {"inputs": {"22": "target"}},
                "target": {"locked": {"rev": "abcdefgh-more", "url": "https://target.invalid"}}
            }
        }"#,
        )
        .unwrap();
        let value = serde_json::json!({"base": [
            ["number", 22], ["string", "22"], ["nil", null],
            ["false", false], ["array", []], ["object", {}], ["one"], "standalone",
            ["duplicate", "22", "ignored"], ["duplicate", 22], [], [false, "22"]
        ]});
        let UiEvent::Table { rows, .. } = channels_table(&lock, &value, None).unwrap() else {
            panic!()
        };
        assert_eq!(
            rows,
            vec![
                vec!["base", "number", "22", "-", "-"],
                vec!["base", "string", "22", "abcdefgh", "https://target.invalid"],
                vec!["base", "nil", "", "-", "-"],
                vec!["base", "false", "", "-", "-"],
                vec!["base", "array", "[]", "-", "-"],
                vec!["base", "object", "{}", "-", "-"],
                vec!["base", "one", "", "-", "-"],
                vec!["base", "standalone", "", "-", "-"],
                vec![
                    "base",
                    "duplicate",
                    "22",
                    "abcdefgh",
                    "https://target.invalid"
                ],
                vec!["base", "duplicate", "22", "-", "-"],
                vec!["base", "", "", "-", "-"],
                vec!["base", "", "22", "abcdefgh", "https://target.invalid"],
            ]
        );
    }

    #[test]
    fn channel_integer_mapping_fails_before_table_publication_for_every_integer_size() {
        let lock = FlakeLock::decode(br#"{}"#).unwrap();
        for number in ["0", "-22", "184467440737095516160000000"] {
            let value =
                serde_json::from_str(&format!(r#"{{"a":{{"valid":"missing"}},"z":{number}}}"#))
                    .unwrap();
            let error = channels_table(&lock, &value, None).unwrap_err();
            assert_eq!(
                error.to_string(),
                "undefined method 'each' for an instance of Integer"
            );
        }
        // Arbitrary-precision JSON retains the numeric lexeme: an overflowing
        // float must not be classified as Integer merely because f64 fails.
        for number in ["7.0", "1e999"] {
            let value = serde_json::from_str(&format!(r#"{{"z":{number}}}"#)).unwrap();
            assert!(
                !channels_table(&lock, &value, None)
                    .unwrap_err()
                    .to_string()
                    .contains("Integer")
            );
        }
    }

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
