use crate::error::ConfctlError;
use serde_json::Value;

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
