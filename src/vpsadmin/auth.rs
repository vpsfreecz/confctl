use super::*;
use chrono::{DateTime, FixedOffset, NaiveDate, NaiveDateTime, SecondsFormat, Utc};
impl Client<'_> {
    pub(crate) async fn authenticate(&mut self) -> Result<(), ConfctlError> {
        self.user = self.read_line("User name: ".into(), false).await?;
        self.password = self.read_line("Password: ".into(), true).await?;
        self.auth_description = self.describe(false).await?;
        let header = self
            .auth_description
            .pointer("/authentication/token/http_header")
            .and_then(Value::as_str)
            .ok_or_else(|| error("missing token authentication header"))?;
        self.header = Some(
            HeaderName::from_bytes(header.as_bytes())
                .map_err(|_| error("invalid token authentication header"))?,
        );
        self.login().await
    }
    async fn read_line(&mut self, prompt: String, protected: bool) -> Result<String, ConfctlError> {
        check(self.cancel)?;
        let prompt = self.redact(&prompt);
        let mut value = if protected {
            self.ui.protected_line(prompt).await?
        } else {
            self.ui.line(prompt).await?
        };
        check(self.cancel)?;
        if value.last() == Some(&b'\n') {
            value.pop();
            if value.last() == Some(&b'\r') {
                value.pop();
            }
        }
        let value =
            String::from_utf8(value).map_err(|_| error("authentication input must be UTF-8"))?;
        self.secret(&value);
        Ok(value)
    }
    async fn login(&mut self) -> Result<(), ConfctlError> {
        let actions = self
            .auth_description
            .pointer("/authentication/token/resources/token/actions")
            .and_then(Value::as_object)
            .ok_or_else(|| error("missing token actions"))?
            .clone();
        let mut name = "request".to_owned();
        let mut supplied =
            json!({"lifetime":"fixed","interval":60,"user":self.user,"password":self.password})
                .as_object()
                .unwrap()
                .clone();
        loop {
            check(self.cancel)?;
            let spec = actions
                .get(&name)
                .ok_or_else(|| error("missing authentication continuation action"))?;
            let input = inputs(spec, &supplied)?;
            for value in input.values() {
                self.secret(&crate::domain::ruby_string(value));
            }
            let result = self.call(spec, &[], input, false, false, "object").await?;
            if let Some(token) = result["token"].as_str() {
                self.secret(token);
            }
            if truthy(result.get("complete")) {
                let token = result["token"]
                    .as_str()
                    .filter(|s| !s.is_empty())
                    .ok_or_else(|| error("missing completed authentication token"))?;
                HeaderValue::from_str(token).map_err(|_| error("invalid token header value"))?;
                self.token = Some(token.into());
                self.valid_to = match result.get("valid_to") {
                    None | Some(Value::Null | Value::Bool(false)) => None,
                    Some(Value::String(s)) => {
                        Some(iso8601(s).ok_or_else(|| error("invalid token expiry"))?)
                    }
                    _ => return Err(error("invalid token expiry")),
                };
                return check(self.cancel);
            }
            name = result["next_action"]
                .as_str()
                .ok_or_else(|| error("missing authentication continuation"))?
                .into();
            let token = result["token"].clone();
            if !token.is_string() {
                return Err(error("missing authentication continuation token"));
            }
            let next = actions
                .get(&name)
                .ok_or_else(|| error("unknown authentication continuation"))?;
            let parameters = parameters(next)?;
            supplied = Map::new();
            supplied.insert("token".into(), token);
            for (name, desc) in parameters {
                if name == "token" {
                    continue;
                }
                let label = desc["label"].as_str().unwrap_or(name);
                let value = self
                    .read_line(format!("{label}: "), truthy(desc.get("protected")))
                    .await?;
                supplied.insert(name.clone(), Value::String(value));
            }
        }
    }
    pub(super) async fn ensure_token(&mut self) -> Result<(), ConfctlError> {
        check(self.cancel)?;
        #[cfg(test)]
        let now = self
            .clock
            .as_ref()
            .map(|clock| {
                DateTime::from_timestamp(clock.load(std::sync::atomic::Ordering::SeqCst), 0)
                    .unwrap()
            })
            .unwrap_or_else(Utc::now);
        #[cfg(not(test))]
        let now = Utc::now();
        if self.valid_to.is_some_and(|v| v < now) {
            self.login().await?;
        }
        Ok(())
    }
}
fn parameters(spec: &Value) -> Result<&Map<String, Value>, ConfctlError> {
    if spec["input"]["layout"] != "object" {
        return Err(error("unsupported action input layout"));
    }
    spec["input"]["parameters"]
        .as_object()
        .ok_or_else(|| error("missing described input parameters"))
}
pub(super) fn inputs(
    spec: &Value,
    supplied: &Map<String, Value>,
) -> Result<Map<String, Value>, ConfctlError> {
    if spec["input"].is_null() {
        return Ok(Map::new());
    }
    let mut result = Map::new();
    for (name, desc) in parameters(spec)? {
        let validators = &desc["validators"];
        let presence = ["presence", "present", "required"]
            .iter()
            .filter_map(|k| validators.get(k))
            .find(|v| truthy(Some(v)));
        let Some(value) = supplied.get(name) else {
            if presence.is_some() {
                return Err(error("required authentication/action parameter is missing"));
            }
            continue;
        };
        let value = scalar(desc, value)?;
        let allow_empty = presence
            .and_then(|v| v.get("empty"))
            .is_some_and(|v| truthy(Some(v)));
        if presence.is_some()
            && (value.is_null()
                || !allow_empty && value.as_str().is_some_and(|s| s.trim().is_empty()))
        {
            return Err(error("required authentication/action parameter is empty"));
        }
        result.insert(name.clone(), value);
    }
    Ok(result)
}
fn scalar(desc: &Value, raw: &Value) -> Result<Value, ConfctlError> {
    let nullable = truthy(desc.get("nullable"));
    if raw.is_null() || nullable && raw.as_str().is_some_and(|s| s.trim().is_empty()) {
        return if nullable {
            Ok(Value::Null)
        } else {
            Err(error("authentication parameter cannot be null"))
        };
    }
    let invalid = || error("invalid authentication scalar input");
    match desc["type"].as_str() {
        Some("String" | "Text") => {
            if raw.is_array() || raw.is_object() {
                Err(invalid())
            } else {
                Ok(Value::String(crate::domain::ruby_string(raw)))
            }
        }
        Some("Integer") => {
            if raw.is_number() && raw.to_string().contains(['.', 'e', 'E']) {
                let number = raw
                    .as_f64()
                    .filter(|n| n.is_finite() && n.fract() == 0.0)
                    .ok_or_else(invalid)?;
                return serde_json::from_str(&if number == 0.0 {
                    "0".into()
                } else {
                    format!("{number:.0}")
                })
                .map_err(|_| invalid());
            }
            let text = raw
                .as_str()
                .map(str::trim)
                .map(String::from)
                .unwrap_or_else(|| raw.to_string());
            let digits = text.strip_prefix(['+', '-']).unwrap_or(&text);
            if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
                return Err(invalid());
            }
            let negative = text.starts_with('-');
            let digits = digits.trim_start_matches('0');
            let normalized = if digits.is_empty() {
                "0".into()
            } else {
                format!("{}{digits}", if negative { "-" } else { "" })
            };
            serde_json::from_str(&normalized).map_err(|_| invalid())
        }
        Some("Float") => {
            let value = float(&crate::domain::ruby_string(raw)).ok_or_else(invalid)?;
            serde_json::Number::from_f64(value)
                .map(Value::Number)
                .ok_or_else(invalid)
        }
        Some("Boolean") => match raw {
            Value::Bool(_) => Ok(raw.clone()),
            _ => match crate::domain::ruby_string(raw)
                .trim()
                .to_ascii_lowercase()
                .as_str()
            {
                "true" | "t" | "yes" | "y" | "1" => Ok(json!(true)),
                "false" | "f" | "no" | "n" | "0" => Ok(json!(false)),
                _ => Err(invalid()),
            },
        },
        Some("Datetime") => {
            let text = raw.as_str().ok_or_else(invalid)?;
            let date = iso8601(text).ok_or_else(invalid)?;
            Ok(Value::String(
                date.to_rfc3339_opts(SecondsFormat::Secs, false),
            ))
        }
        Some("Resource") => {
            if raw.is_number() && !raw.to_string().contains(['.', 'e', 'E']) {
                return Ok(raw.clone());
            }
            let text = raw.as_str().ok_or_else(invalid)?;
            if text.is_empty() || !text.bytes().all(|b| b.is_ascii_digit()) {
                return Err(invalid());
            }
            let digits = text.trim_start_matches('0');
            serde_json::from_str(if digits.is_empty() { "0" } else { digits })
                .map_err(|_| invalid())
        }
        _ => Err(error("unsupported authentication parameter type")),
    }
}
// Both pinned client consumers use DateTime.iso8601. Normalize its compact
// numeric offsets without losing the parsed offset or fractional instant.
fn iso8601(text: &str) -> Option<DateTime<FixedOffset>> {
    let normalized = [3, 5].into_iter().find_map(|length| {
        let start = text.len().checked_sub(length)?;
        let suffix = text.get(start..)?;
        let bytes = suffix.as_bytes();
        if !matches!(bytes[0], b'+' | b'-')
            || !bytes[1..].iter().all(u8::is_ascii_digit)
            || !text[..start].contains(['T', 't'])
        {
            return None;
        }
        let minutes = if length == 3 { "00" } else { &suffix[3..] };
        Some(format!("{}{}:{minutes}", &text[..start], &suffix[..3]))
    });
    DateTime::parse_from_rfc3339(normalized.as_deref().unwrap_or(text))
        .ok()
        .or_else(|| {
            NaiveDateTime::parse_from_str(text, "%Y-%m-%dT%H:%M:%S")
                .ok()
                .map(|date| date.and_utc().fixed_offset())
        })
        .or_else(|| {
            NaiveDate::parse_from_str(text, "%Y-%m-%d")
                .ok()
                .map(|date| date.and_hms_opt(0, 0, 0).unwrap().and_utc().fixed_offset())
        })
}
// Ruby Float accepts underscore-separated digits and hexadecimal mantissas.
fn float(raw: &str) -> Option<f64> {
    let raw = raw.trim();
    let hex = raw.trim_start_matches(['+', '-']).starts_with("0x")
        || raw.trim_start_matches(['+', '-']).starts_with("0X");
    let bytes = raw.as_bytes();
    for (i, b) in bytes.iter().enumerate() {
        if *b == b'_'
            && (i == 0
                || i + 1 == bytes.len()
                || !(if hex {
                    bytes[i - 1].is_ascii_hexdigit()
                } else {
                    bytes[i - 1].is_ascii_digit()
                })
                || !(if hex {
                    bytes[i + 1].is_ascii_hexdigit()
                } else {
                    bytes[i + 1].is_ascii_digit()
                }))
        {
            return None;
        }
    }
    let text = raw.replace('_', "");
    let (sign, text) = if let Some(s) = text.strip_prefix('-') {
        (-1.0, s)
    } else {
        (1.0, text.strip_prefix('+').unwrap_or(&text))
    };
    let number = if let Some(hex) = text.strip_prefix("0x").or_else(|| text.strip_prefix("0X")) {
        let (mantissa, exponent) = hex
            .split_once(['p', 'P'])
            .map_or(Some((hex, 0)), |(m, e)| Some((m, e.parse::<i32>().ok()?)))?;
        let mut value = 0.0;
        let mut scale = 1.0;
        let mut fractional = false;
        let mut digits = 0;
        for c in mantissa.chars() {
            if c == '.' && !fractional {
                fractional = true;
                continue;
            }
            let digit = c.to_digit(16)? as f64;
            digits += 1;
            if fractional {
                scale /= 16.0;
                value += digit * scale;
            } else {
                value = value * 16.0 + digit;
            }
        }
        if digits == 0 {
            return None;
        }
        value * 2f64.powi(exponent)
    } else {
        text.parse::<f64>().ok()?
    };
    (number * sign).is_finite().then_some(number * sign)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn iso8601_offsets_agree_for_challenge_conversion_and_expiry_instants() {
        for (input, expected, instant, fraction) in [
            (
                "2026-10-20T00:00:00Z",
                "2026-10-20T00:00:00+00:00",
                1792454400,
                0,
            ),
            (
                "2026-10-20T00:00:00+00:00",
                "2026-10-20T00:00:00+00:00",
                1792454400,
                0,
            ),
            (
                "2026-10-20T00:00:00+0000",
                "2026-10-20T00:00:00+00:00",
                1792454400,
                0,
            ),
            (
                "2026-10-20T00:00:00+02",
                "2026-10-20T00:00:00+02:00",
                1792447200,
                0,
            ),
            (
                "2026-10-20T00:00:00+0230",
                "2026-10-20T00:00:00+02:30",
                1792445400,
                0,
            ),
            (
                "2026-10-20T00:00:00.123456789-0430",
                "2026-10-20T00:00:00-04:30",
                1792470600,
                123456789,
            ),
        ] {
            let parsed = iso8601(input).unwrap();
            assert_eq!(parsed.timestamp(), instant);
            assert_eq!(parsed.timestamp_subsec_nanos(), fraction);
            assert_eq!(
                scalar(&json!({"type":"Datetime"}), &json!(input)).unwrap(),
                json!(expected)
            );
        }
        for input in [
            "2026-10-20T00:00:00+000",
            "2026-10-20T00:00:00+02:",
            "2026-10-20T00:00:00+02:300",
            "2026-10-20T00:00:00Ztail",
            "not-an-iso8601-value",
        ] {
            assert!(iso8601(input).is_none());
            let error = scalar(&json!({"type":"Datetime"}), &json!(input)).unwrap_err();
            assert!(!format!("{error:?}").contains(input));
        }
    }
    #[test]
    fn scalar_null_omission_presence_and_delegated_validators() {
        for (kind, raw, expected) in [
            ("String", json!(" pass "), json!(" pass ")),
            ("Text", json!(4), json!("4")),
            ("Integer", json!(" +0007 "), json!(7)),
            ("Integer", json!("-0"), json!(0)),
            ("Float", json!(" 1.5 "), json!(1.5)),
            ("Float", json!("1_000.25"), json!(1000.25)),
            ("Float", json!("0x1.8p1"), json!(3.0)),
            ("Integer", json!(2.0), json!(2)),
            ("Boolean", json!(" YES "), json!(true)),
            ("Boolean", json!("n"), json!(false)),
            ("Resource", json!("007"), json!(7)),
            (
                "Datetime",
                json!("2026-10-20"),
                json!("2026-10-20T00:00:00+00:00"),
            ),
        ] {
            assert_eq!(scalar(&json!({"type":kind}), &raw).unwrap(), expected);
        }
        for (kind, raw) in [
            ("Integer", json!("1x")),
            ("Float", json!("NaN")),
            ("Boolean", json!("2")),
            ("Datetime", json!("no")),
            ("Resource", json!("-1")),
            ("String", json!([])),
            ("Array", json!("[]")),
        ] {
            assert!(scalar(&json!({"type":kind}), &raw).is_err());
        }
        assert_eq!(
            scalar(&json!({"type":"Integer","nullable":true}), &json!("  ")).unwrap(),
            Value::Null
        );
        assert!(scalar(&json!({"type":"String"}), &Value::Null).is_err());
        let spec = json!({"input":{"layout":"object","parameters":{"optional":{"type":"String"},"needed":{"type":"String","validators":{"presence":true}},"server":{"type":"String","validators":{"format":"never matches"}}}}});
        assert!(inputs(&spec, &Map::new()).is_err());
        assert!(inputs(&spec, json!({"needed":"  "}).as_object().unwrap()).is_err());
        let got = inputs(
            &spec,
            json!({"needed":"value","server":"anything","extra":1})
                .as_object()
                .unwrap(),
        )
        .unwrap();
        assert_eq!(
            got,
            json!({"needed":"value","server":"anything"})
                .as_object()
                .unwrap()
                .clone()
        );
    }
}
