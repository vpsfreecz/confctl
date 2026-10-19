use super::*;
use crate::{
    error::{NixCommandContext, NixCommandFailure},
    process::PartialProcessEvidence,
};
use serde_json::json;
use std::os::unix::process::ExitStatusExt;

fn row(target: Value, deployed: Value) -> Row {
    Row {
        host: "host".into(),
        observed: HostStatus {
            uptime: Some(0.25),
            generations: Some(Vec::new()),
            ..HostStatus::default()
        },
        target: normalize(&target),
        target_toplevel: None,
        deployed: normalize(&deployed),
        local_count: 0,
        failed: false,
        diagnostics: Vec::new(),
        native_failures: Vec::new(),
        eval_failures: Vec::new(),
    }
}
#[test]
fn normalized_revisions_keep_types_alias_order_and_full_equality() {
    let target=normalize(&json!({"string":"abcdefgh-one","typed":{"rev":7},"false":{"rev":false},"alias":{"shortRev":"one","short_rev":"two","rev":"full"},"unknown":{"extra":[7]}})).unwrap();
    assert_eq!(target["alias"]["shortRev"], "two");
    assert!(target["unknown"].as_object().unwrap().is_empty());
    assert_eq!(
        comparison(target.get("typed"), Some(&json!({"rev":"7"}))),
        Comparison::Changed
    );
    assert_eq!(
        comparison(target.get("false"), Some(&json!({"rev":false}))),
        Comparison::Same
    );
    assert_eq!(
        comparison(
            target.get("string"),
            Some(&json!({"rev":"abcdefgh-two","shortRev":"abcdefgh"}))
        ),
        Comparison::Changed
    );
    assert_eq!(
        comparison(target.get("unknown"), Some(&json!({"rev":null}))),
        Comparison::Unknown
    );
    assert!(payload(json!("{\"role\":\"revision\"}")).is_some());
    assert!(payload(json!("broken")).is_none());
    assert!(payload(json!([])).is_none());
}
#[test]
fn status_requires_nonempty_target_roles_and_online_but_allows_extra_deployed_roles() {
    for (target, deployed, expected) in [
        (
            json!({"role":"full-revision"}),
            json!({"role":"full-revision","extra":"other"}),
            "ok",
        ),
        (json!({}), json!({}), "outdated"),
        (json!({"role":"one"}), json!({"role":"two"}), "outdated"),
        (json!({"role":"one"}), json!({}), "outdated"),
    ] {
        let rows = [row(target, deployed)];
        let UiEvent::Table {
            rows: cells,
            columns,
            ..
        } = table(&rows)
        else {
            panic!()
        };
        assert_eq!(cells[0][3], expected);
        assert_eq!(cells[0][2], "0.250s");
        assert_eq!(cells[0][4], "0:0");
        let mut roles = columns[5..].to_vec();
        roles.sort();
        assert_eq!(roles, columns[5..]);
    }
    let mut r = row(json!({"role":"rev"}), json!({"role":"rev"}));
    r.observed.uptime = None;
    r.observed.generations = None;
    let UiEvent::Table { rows, .. } = table(&[r]) else {
        panic!()
    };
    assert_eq!(
        &rows[0][1..5],
        ["unknown", "unknown", "outdated", "0:unknown"]
    );
    assert!(rows[0][5].contains("same"));
}
#[test]
fn durations_include_zero_subsecond_and_reject_invalid_values() {
    for (uptime, expected) in [
        (0.0, "0.000s"),
        (0.5, "0.500s"),
        (90061.0, "1d 1h 1m 1s"),
        (-1.0, "unknown"),
        (f64::NAN, "unknown"),
        (f64::INFINITY, "unknown"),
    ] {
        assert_eq!(duration(uptime), expected);
    }
}
#[test]
fn attribute_quoting_and_target_selection_follow_existing_contract() {
    assert_eq!(attribute("native-key"), "native-key");
    assert_eq!(attribute("nested/key.${x}\""), "\"nested/key.\\${x}\\\"\"");
    for (value, expected) in [
        (json!({"name":"default","metaConfig":{}}), true),
        (
            json!({"name":"null","metaConfig":{"host":{"target":null}}}),
            false,
        ),
        (
            json!({"name":"false","metaConfig":{"host":{"target":false}}}),
            false,
        ),
        (
            json!({"name":"guest","carrier":"carrier","metaConfig":{"host":{"target":null}}}),
            true,
        ),
    ] {
        assert_eq!(eligible(&Machine::from_value(value).unwrap()), expected);
    }
}
#[test]
fn only_complete_failed_native_nix_eval_is_an_unknown_target() {
    let wrapped = |source, code, complete| {
        ConfctlError::NixCommandFailure(Box::new(NixCommandFailure {
            context: NixCommandContext {
                cwd: "/fixture".into(),
                program: "nix".into(),
                args: Vec::new(),
            },
            source: Box::new(ConfctlError::ProcessFailure {
                source: Box::new(source),
                evidence: Box::new(PartialProcessEvidence {
                    status: Some(std::process::ExitStatus::from_raw(code << 8)),
                    stdout: Captured::Memory(Vec::new()),
                    stderr: Captured::Memory(Vec::new()),
                    stdout_complete: complete,
                    stderr_complete: true,
                    log: None,
                }),
            }),
        }))
    };
    let child = || ConfctlError::Child {
        description: "nix eval".into(),
        code: Some(1),
        signal: None,
        log: None,
    };
    assert!(native_eval_failure(&wrapped(child(), 1, true)));
    assert!(!native_eval_failure(&wrapped(child(), 0, true)));
    assert!(!native_eval_failure(&wrapped(child(), 1, false)));
    assert!(!native_eval_failure(&wrapped(
        ConfctlError::Cancelled,
        1,
        true
    )));
    assert!(!native_eval_failure(&ConfctlError::Configuration(
        "bad configuration".into()
    )));
}

#[test]
fn saved_status_requires_known_exact_native_current_path() {
    use std::os::unix::ffi::OsStringExt;
    let expected = PathBuf::from(std::ffi::OsString::from_vec(b"/saved/\xff/system".to_vec()));
    for (current, status) in [
        (Some(expected.clone()), "ok"),
        (Some("/saved/other".into()), "outdated"),
        (Some("/saved/./system".into()), "outdated"),
        (None, "outdated"),
    ] {
        let mut row = row(json!({"role":"rev"}), json!({"role":"rev"}));
        row.target_toplevel = Some(expected.clone());
        row.observed.current_toplevel = current;
        let UiEvent::Table { rows, .. } = table(&[row]) else {
            panic!()
        };
        assert_eq!(rows[0][3], status);
    }
    let mut row = row(json!({"role":"rev"}), json!({"role":"rev"}));
    row.target_toplevel = Some("/saved/system".into());
    row.observed.current_toplevel = Some("/saved/./system".into());
    let UiEvent::Table { rows, .. } = table(&[row]) else {
        panic!()
    };
    assert_eq!(rows[0][3], "outdated");
}
