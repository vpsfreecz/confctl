//! Private formatting for the two generated gen-data shapes.
use crate::error::ConfctlError;
use serde_json::Value;
pub(super) fn render(value: &Value) -> Result<Vec<u8>, ConfctlError> {
    let mut out = String::new();
    value_to_nix(value, 0, &mut out)?;
    out.push('\n');
    Ok(out.into_bytes())
}
fn string(s: &str) -> String {
    let mut out = String::from("\"");
    let mut chars = s.chars().peekable();
    while let Some(c) = chars.next() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '$' if chars.peek() == Some(&'{') => out.push_str("\\$"),
            _ => out.push(c),
        }
    }
    out.push('"');
    out
}
fn key(s: &str) -> bool {
    if [
        "if", "then", "else", "assert", "with", "let", "in", "rec", "inherit", "or",
    ]
    .contains(&s)
    {
        return false;
    }
    let mut c = s.chars();
    c.next()
        .is_some_and(|c| c.is_ascii_alphabetic() || c == '_')
        && c.all(|c| c.is_ascii_alphabetic() || "_-'".contains(c))
}
fn value_to_nix(v: &Value, indent: usize, out: &mut String) -> Result<(), ConfctlError> {
    let pad = " ".repeat(indent);
    match v {
        Value::Object(m) => {
            if m.keys().any(|s| s.contains('\0')) {
                return Err(ConfctlError::State(
                    "gen-data cannot render a NUL in Nix keys".into(),
                ));
            }
            out.push_str("{\n");
            let quote = !m.keys().all(|k| key(k));
            let mut entries = m.iter().collect::<Vec<_>>();
            entries.sort_by_key(|(k, _)| *k);
            for (k, v) in entries {
                out.push_str(&format!(
                    "{pad}  {} = ",
                    if quote { string(k) } else { k.clone() }
                ));
                value_to_nix(v, indent + 2, out)?;
                out.push_str(";\n");
            }
            out.push_str(&format!("{pad}}}"));
        }
        Value::Array(a) => {
            out.push_str("[\n");
            for v in a {
                out.push_str(&format!("{pad}  "));
                value_to_nix(v, indent + 2, out)?;
                out.push('\n');
            }
            out.push_str(&format!("{pad}]"));
        }
        Value::String(s) => {
            if s.chars().any(|c| c == '\0') {
                return Err(ConfctlError::State(
                    "gen-data cannot render a NUL in Nix text".into(),
                ));
            }
            out.push_str(&string(s));
        }
        _ => out.push_str(&v.to_string()),
    }
    Ok(())
}
#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    #[test]
    fn gen_data_renderer_preserves_unicode_literals_keys_order_and_interpolation_safety() {
        let value = json!({"z":"žluťoučký ${host} \\\"\n\t", "a1":[null,1,true,{"if":"value"}]});
        let bytes = String::from_utf8(render(&value).unwrap()).unwrap();
        assert!(bytes.starts_with("{\n  \"a1\" = [\n"));
        assert!(bytes.contains("žluťoučký \\${host}"));
        assert!(bytes.contains("\"if\" = \"value\";"));
        assert!(bytes.ends_with("}\n"));
        let bytes = String::from_utf8(render(&json!({"${key}\"":"x"})).unwrap()).unwrap();
        assert!(bytes.contains("\"\\${key}\\\"\" = \"x\";"));
    }
}
