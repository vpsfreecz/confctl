use super::*;
use serde_json::json;

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
fn a_successful_deployed_fallback_does_not_clear_an_earlier_target_failure() {
    let mut row = row(json!({}), json!({}));
    row.failed = true;
    row.diagnostics.push("target evaluation failed".into());
    merge_observation(
        &mut row,
        super::super::input_observation::InputObservation {
            value: normalize(&json!({"role":"deployed"})),
            diagnostics: vec!["deployed inputs read successfully from profile fallback".into()],
            ..Default::default()
        },
        false,
    );
    assert!(row.failed);
    assert_eq!(row.diagnostics.len(), 2);
    assert_eq!(row.deployed.unwrap()["role"]["rev"], "deployed");
}
#[test]
fn attribute_quoting_and_target_selection_follow_existing_contract() {
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
