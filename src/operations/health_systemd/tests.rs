use super::*;
use serde_json::json;

fn machine(spin: &str) -> Machine {
    Machine::from_value(json!({"name": "host-a", "metaConfig": {"spin": spin}})).unwrap()
}

fn property(name: &str, value: &str, timeout: u64, cooldown: u64) -> Value {
    json!({"property":name, "value":value, "timeout":timeout, "cooldown":cooldown})
}

fn system(properties: Vec<Value>) -> Check {
    from_metadata(
        &machine("nixos"),
        &json!({"enable":true, "systemProperties":properties, "unitProperties":{}}),
    )
    .unwrap()
    .remove(0)
}

#[test]
fn groups_preserve_order_literal_argv_and_independent_minima() {
    let metadata = json!({
        "enable":true,
        "systemProperties":[
            property("SystemState", "running", 90, 2),
            property("NFailedUnits", "0", 10, 8)
        ],
        "unitProperties":{
            "z worker@*.service; 'quoted'": [
                property("ActiveState", "active", 30, 9),
                property("SubState", "running", 50, 1)
            ],
            "empty.service": [],
            "a.service": [property("LoadState", "loaded", 0, 0)]
        }
    });
    let checks = from_metadata(&machine("nixos"), &metadata).unwrap();
    assert_eq!(checks.len(), 3);
    assert_eq!(
        checks[0].description,
        "systemd: SystemState=running, NFailedUnits=0"
    );
    assert_eq!(checks[0].argv(), ["systemctl", "show"].map(OsString::from));
    assert_eq!(checks[0].timeout, Duration::from_secs(10));
    assert_eq!(checks[0].cooldown, Duration::from_secs(2));
    assert_eq!(
        checks[1].argv(),
        ["systemctl", "show", "z worker@*.service; 'quoted'"].map(OsString::from)
    );
    assert_eq!(
        checks[1].description,
        "z worker@*.service; 'quoted': ActiveState=active, SubState=running"
    );
    assert_eq!(checks[1].timeout, Duration::from_secs(30));
    assert_eq!(checks[1].cooldown, Duration::from_secs(1));
    assert_eq!(
        checks[2].argv(),
        ["systemctl", "show", "a.service"].map(OsString::from)
    );
    assert_eq!(checks[2].timeout, Duration::ZERO);
    assert_eq!(checks[2].cooldown, Duration::ZERO);
    assert!(
        checks[1]
            .errors(b"ActiveState=active\nSubState=running\n")
            .is_empty()
    );
}

#[test]
fn empty_groups_are_zero_checks_without_fictitious_success() {
    let metadata = json!({
        "enable":true, "systemProperties":[],
        "unitProperties":{"empty.service":[], "another.service":[]}
    });
    assert!(
        from_metadata(&machine("nixos"), &metadata)
            .unwrap()
            .is_empty()
    );
    let unit_only = json!({
        "enable":true, "systemProperties":[],
        "unitProperties":{"worker.service":[property("ActiveState", "active", 60, 3)]}
    });
    let checks = from_metadata(&machine("nixos"), &unit_only).unwrap();
    assert_eq!(checks.len(), 1);
    assert_eq!(
        checks[0].argv(),
        ["systemctl", "show", "worker.service"].map(OsString::from)
    );
}

#[test]
fn disabled_and_other_spins_do_not_validate_inactive_groups() {
    let disabled = json!({"enable":false, "systemProperties":"bad", "unitProperties":null});
    assert!(
        from_metadata(&machine("nixos"), &disabled)
            .unwrap()
            .is_empty()
    );
    for spin in ["vpsadminos", "other", "openvz", ""] {
        for value in [
            json!({"enable":true, "systemProperties":false}),
            Value::Null,
        ] {
            assert!(from_metadata(&machine(spin), &value).unwrap().is_empty());
        }
    }
}

#[test]
fn active_malformed_fields_and_durations_fail_configuration() {
    let valid = json!({
        "enable":true,
        "systemProperties":[property("SystemState", "running", 60, 3)],
        "unitProperties":{}
    });
    let host = machine("nixos");
    let mut invalid = vec![Value::Null, json!({}), json!({"enable":"true"})];
    for field in ["systemProperties", "unitProperties"] {
        let mut value = valid.clone();
        value.as_object_mut().unwrap().remove(field);
        invalid.push(value);
        let mut value = valid.clone();
        value[field] = Value::Null;
        invalid.push(value);
    }
    for group in [
        json!({}),
        json!("bad"),
        json!(false),
        json!([null]),
        json!([["SystemState", "running", 60, 3]]),
    ] {
        let mut value = valid.clone();
        value["systemProperties"] = group;
        invalid.push(value);
    }
    for field in ["property", "value", "timeout", "cooldown"] {
        let mut value = valid.clone();
        value["systemProperties"][0]
            .as_object_mut()
            .unwrap()
            .remove(field);
        invalid.push(value);
    }
    for field in ["property", "value"] {
        let mut value = valid.clone();
        value["systemProperties"][0][field] = json!(7);
        invalid.push(value);
    }
    for field in ["timeout", "cooldown"] {
        for duration in [
            json!(-1),
            json!(0.5),
            json!("3"),
            Value::Null,
            serde_json::from_str("18446744073709551616").unwrap(),
        ] {
            let mut value = valid.clone();
            value["systemProperties"][0][field] = duration;
            invalid.push(value);
        }
    }
    for value in invalid {
        let error = from_metadata(&host, &value).expect_err("invalid active metadata");
        assert!(matches!(error, ConfctlError::Configuration(_)), "{error:?}");
        assert!(error.to_string().contains("host-a"));
    }
    let error = from_metadata(
        &host,
        &json!({
            "enable":true, "systemProperties":[], "unitProperties":{"bad.service":null}
        }),
    )
    .expect_err("invalid unit list");
    assert!(
        error
            .to_string()
            .contains("unitProperties[\"bad.service\"]")
    );
}

#[test]
fn property_records_preserve_values_and_last_duplicate() {
    let check = system(vec![
        property("SystemState", "running", 60, 3),
        property("Environment", " A=b=c  ", 60, 3),
        property("Empty", "", 60, 3),
        property("Description", " pracovní služba ", 60, 3),
    ]);
    let stdout = "Not a property\nSystemState=starting\n\nEnvironment= A=b=c  \nEmpty=\r\nDescription= pracovní služba \nSystemState=running\n".as_bytes();
    assert!(check.errors(stdout).is_empty());
}

#[test]
fn missing_and_mismatched_properties_report_in_configured_order() {
    let check = system(vec![
        property("Missing", "wanted", 60, 3),
        property("SystemState", "running", 60, 3),
        property("Empty", "", 60, 3),
        property("AlsoMissing", "", 60, 3),
        property("SystemState", "degraded", 60, 3),
    ]);
    assert_eq!(
        check.errors(b"SystemState=running\nSystemState=degraded\nEmpty= \n"),
        [
            "property \"Missing\" not found",
            "SystemState=degraded, expected running",
            "Empty= , expected ",
            "property \"AlsoMissing\" not found"
        ]
    );
}

#[test]
fn line_endings_are_removed_without_trimming_records() {
    let check = system(vec![
        property("CRLF", "yes", 60, 3),
        property("LF", "yes", 60, 3),
        property("NoNewline", "tail\r", 60, 3),
        property(" PaddedKey ", "  value \t", 60, 3),
    ]);
    assert!(
        check
            .errors(b"CRLF=yes\r\nLF=yes\n PaddedKey =  value \t\nNoNewline=tail\r")
            .is_empty()
    );
    assert_eq!(
        system(vec![property("State", "running", 0, 0)]).errors(b"State=running \n"),
        ["State=running , expected running"]
    );
}

#[test]
fn invalid_bytes_and_controls_are_compared_raw_and_diagnosed_safely() {
    let check = system(vec![
        property("ActiveState", "active", 60, 3),
        property("Unicode", "�", 60, 3),
        property("Missing\n\u{1b}", "", 60, 3),
    ]);
    let stdout = b"ActiveState=bad\xff\x1b[31m\nUnicode=\xff\n";
    assert_eq!(
        check.errors(stdout),
        [
            r"ActiveState=bad\xff\x1b[31m, expected active",
            r"Unicode=\xff, expected \xef\xbf\xbd",
            r#"property "Missing\n\u{1b}" not found"#,
        ]
    );
}
