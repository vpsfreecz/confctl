//! Purpose-specific compatibility with JSON 2.21.2 pretty_generate. Parsed
//! integer tokens remain exact; float tokens are converted to binary64 before
//! formatting, as in Ruby. This is not a new application-wide JSON policy.

use crate::error::ConfctlError;
use serde_json::Value;

mod float;

pub(crate) fn parse(bytes: &[u8]) -> Result<Value, ConfctlError> {
    let value: Value = serde_json::from_slice(bytes).map_err(|error| {
        ConfctlError::Configuration(format!("invalid runtime kernel JSON: {error}"))
    })?;
    check_nesting(&value, 0)?;
    Ok(value)
}

fn check_nesting(value: &Value, depth: usize) -> Result<(), ConfctlError> {
    if depth == 100 && matches!(value, Value::Array(_) | Value::Object(_)) {
        return Err(ConfctlError::Configuration(
            "runtime kernel JSON nesting exceeds 100".into(),
        ));
    }
    match value {
        Value::Array(items) => {
            for item in items {
                check_nesting(item, depth + 1)?;
            }
        }
        Value::Object(items) => {
            for item in items.values() {
                check_nesting(item, depth + 1)?;
            }
        }
        _ => {}
    }
    Ok(())
}

pub(crate) fn pretty(value: &Value) -> Result<Vec<u8>, ConfctlError> {
    check_nesting(value, 0)?;
    let mut output = String::new();
    encode(value, 0, &mut output)?;
    Ok(output.into_bytes())
}

fn encode(value: &Value, depth: usize, output: &mut String) -> Result<(), ConfctlError> {
    match value {
        Value::Number(number) => {
            let token = number.to_string();
            if token.contains(['.', 'e', 'E']) {
                let value = token.parse::<f64>().map_err(|error| {
                    ConfctlError::State(format!("encode Ruby JSON float: {error}"))
                })?;
                if !value.is_finite() {
                    return Err(ConfctlError::State(
                        "nonfinite Float not allowed in JSON".into(),
                    ));
                }
                output.push_str(&float::format(value));
            } else {
                output.push_str(if token == "-0" { "0" } else { &token });
            }
        }
        Value::Array(items) if !items.is_empty() => {
            output.push_str("[\n");
            for (index, item) in items.iter().enumerate() {
                output.extend(std::iter::repeat_n(' ', (depth + 1) * 2));
                encode(item, depth + 1, output)?;
                output.push_str(if index + 1 == items.len() {
                    "\n"
                } else {
                    ",\n"
                });
            }
            output.extend(std::iter::repeat_n(' ', depth * 2));
            output.push(']');
        }
        Value::Object(items) if !items.is_empty() => {
            output.push_str("{\n");
            for (index, (name, item)) in items.iter().enumerate() {
                output.extend(std::iter::repeat_n(' ', (depth + 1) * 2));
                output.push_str(&serde_json::to_string(name).expect("JSON string encoding"));
                output.push_str(": ");
                encode(item, depth + 1, output)?;
                output.push_str(if index + 1 == items.len() {
                    "\n"
                } else {
                    ",\n"
                });
            }
            output.extend(std::iter::repeat_n(' ', depth * 2));
            output.push('}');
        }
        _ => output.push_str(
            &serde_json::to_string(value).expect("non-number JSON scalar or empty container"),
        ),
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pretty_matches_original_ruby_numeric_empty_and_permissive_root_probes() {
        let observations: Value =
            serde_json::from_str(include_str!("json/ruby-semantics.json")).unwrap();
        for case in observations["json_cases"].as_array().unwrap() {
            let value: Value = serde_json::from_str(case["input"].as_str().unwrap()).unwrap();
            match case.get("pretty") {
                Some(expected) => assert_eq!(
                    String::from_utf8(pretty(&value).unwrap()).unwrap(),
                    expected.as_str().unwrap(),
                    "{}",
                    case["input"]
                ),
                None => assert!(
                    pretty(&value).is_err(),
                    "encoding failure must remain a failure: {}",
                    case["input"]
                ),
            }
        }
    }

    #[test]
    fn finite_binary64_bytes_match_original_ruby_grisu2_patterns_and_thresholds() {
        let fixture: Value = serde_json::from_str(include_str!("json/ruby-floats.json")).unwrap();
        assert_eq!(fixture["ruby"], "3.4.9");
        assert_eq!(fixture["json"], "2.21.2");
        let cases = fixture["float_cases"].as_array().unwrap();
        assert_eq!(cases.len(), 2434);
        for case in cases {
            let bits = u64::from_str_radix(case["bits"].as_str().unwrap(), 16).unwrap();
            let value = f64::from_bits(bits);
            assert!(value.is_finite());
            let expected = case["pretty"].as_str().unwrap();
            assert_eq!(float::format(value), expected, "binary64 {bits:016x}");
            // Also exercise the persisted decimal-token -> binary64 boundary.
            assert_eq!(
                pretty(&parse(expected.as_bytes()).unwrap()).unwrap(),
                expected.as_bytes(),
                "parsed {bits:016x}"
            );
        }
    }

    #[test]
    fn overflow_underflow_giant_integers_nested_empties_and_nesting_match_ruby() {
        let fixture: Value =
            serde_json::from_str(include_str!("json/ruby-conversions.json")).unwrap();
        for case in fixture["json_cases"].as_array().unwrap() {
            let value = parse(case["input"].as_str().unwrap().as_bytes()).unwrap();
            if let Some(expected) = case.get("pretty") {
                assert_eq!(
                    pretty(&value).unwrap(),
                    expected.as_str().unwrap().as_bytes()
                );
            } else {
                assert!(pretty(&value).is_err());
            }
        }
        for case in fixture["nesting"].as_array().unwrap() {
            let depth = case["depth"].as_u64().unwrap() as usize;
            let input = format!("{}0{}", "[".repeat(depth), "]".repeat(depth));
            match case.get("pretty") {
                Some(expected) => assert_eq!(
                    pretty(&parse(input.as_bytes()).unwrap()).unwrap(),
                    expected.as_str().unwrap().as_bytes()
                ),
                None => assert!(parse(input.as_bytes()).is_err()),
            }
        }
    }
}
