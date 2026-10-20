//! Ruby Machine host and port interpretation, without command or transport policy.

use crate::{domain::Machine, error::ConfctlError};
use serde_json::Value;

pub(crate) fn target_host(machine: &Machine) -> Result<&str, ConfctlError> {
    let host = match machine.metadata.get("host") {
        None => None,
        Some(Value::Object(host)) => Some(host),
        _ => {
            return Err(ConfctlError::Configuration(
                "machine host metadata does not support key lookup".into(),
            ));
        }
    };
    let target = match host.and_then(|host| host.get("target")) {
        None => machine.name.as_str(),
        Some(Value::String(target)) => target,
        _ => {
            return Err(ConfctlError::Configuration(format!(
                "{} has no target host",
                machine.name
            )));
        }
    };
    Ok(target)
}

pub(crate) fn target_port(machine: &Machine) -> Result<String, ConfctlError> {
    machine
        .metadata
        .get("host")
        .and_then(|host| host.get("port"))
        .map(ruby_port)
        .transpose()
        .map(|port| port.unwrap_or_else(|| "22".into()))
}

// Machine#target_port uses Ruby to_i, including decimal prefixes and floats.
fn ruby_port(value: &Value) -> Result<String, ConfctlError> {
    let text = match value {
        Value::Null => return Ok("0".into()),
        Value::String(text) => text.clone(),
        Value::Number(number) => {
            let text = number.to_string();
            if text.contains(['.', 'e', 'E']) {
                let value = text
                    .parse::<f64>()
                    .map_err(|_| ConfctlError::Configuration("invalid target port Float".into()))?;
                if !value.is_finite() {
                    return Err(ConfctlError::Configuration(
                        "nonfinite target port Float cannot convert to Integer".into(),
                    ));
                }
                format!("{:.0}", value.trunc())
            } else {
                text
            }
        }
        _ => {
            return Err(ConfctlError::Configuration(
                "target port does not support to_i".into(),
            ));
        }
    };
    let text = text.trim_start_matches(['\t', '\n', '\x0b', '\x0c', '\r', ' ']);
    let negative = text.starts_with('-');
    let unsigned = text.strip_prefix(['+', '-']).unwrap_or(text);
    // Keep arbitrary-size decimal prefixes; one underscore may separate digits.
    let bytes = unsigned.as_bytes();
    let mut digits = String::new();
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index].is_ascii_digit() {
            digits.push(bytes[index] as char);
            index += 1;
        } else if bytes[index] == b'_'
            && !digits.is_empty()
            && bytes.get(index + 1).is_some_and(u8::is_ascii_digit)
        {
            index += 1;
        } else {
            break;
        }
    }
    let digits = digits.trim_start_matches('0');
    if digits.is_empty() {
        Ok("0".into())
    } else {
        Ok(format!("{}{digits}", if negative { "-" } else { "" }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn machine(metadata: Value) -> Machine {
        Machine::from_value(json!({"name":"fallback", "metaConfig":metadata})).unwrap()
    }

    #[test]
    fn host_defaults_and_rejections_keep_their_metadata_diagnostics() {
        for metadata in [json!({}), json!({"host":{}})] {
            assert_eq!(target_host(&machine(metadata)).unwrap(), "fallback");
        }
        assert_eq!(
            target_host(&machine(json!({"host":{"target":""}}))).unwrap(),
            ""
        );
        for host in [Value::Null, json!(false), json!([]), json!("host")] {
            let error = target_host(&machine(json!({"host":host}))).unwrap_err();
            assert!(matches!(error, ConfctlError::Configuration(message)
                if message == "machine host metadata does not support key lookup"));
        }
        for target in [Value::Null, json!(false), json!([]), json!({})] {
            let error = target_host(&machine(json!({"host":{"target":target}}))).unwrap_err();
            assert!(matches!(error, ConfctlError::Configuration(message)
                if message == "fallback has no target host"));
        }
    }

    #[test]
    fn ports_preserve_ruby_conversion_without_a_port_sized_integer() {
        for (value, expected) in [
            (Value::Null, "0"),
            (json!("nonsense"), "0"),
            (json!("\u{000b}-0002_222suffix"), "-2222"),
            (json!("22__33"), "22"),
            (json!(2222.9), "2222"),
            (
                json!("999999999999999999999999999"),
                "999999999999999999999999999",
            ),
        ] {
            let machine = machine(json!({"host":{"port":value}}));
            assert_eq!(target_port(&machine).unwrap(), expected);
        }
        assert_eq!(target_port(&machine(json!({}))).unwrap(), "22");
        assert_eq!(target_port(&machine(json!({"host":{}}))).unwrap(), "22");
        for value in [json!(false), json!([]), json!({})] {
            assert!(target_port(&machine(json!({"host":{"port":value}}))).is_err());
        }
        for token in ["1e400", "-1e400"] {
            let value: Value = serde_json::from_str(token).unwrap();
            let error = target_port(&machine(json!({"host":{"port":value}}))).unwrap_err();
            assert!(matches!(error, ConfctlError::Configuration(message)
                if message == "nonfinite target port Float cannot convert to Integer"));
        }
    }
}
