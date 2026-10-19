//! Private Ruby Integer conversion shared by the two concrete helper arguments:
//! rollback timeout and kexec generation selection. No native overflow or float
//! conversion is used; String#to_i (runtime SSH port) has different rules.

pub(super) fn canonical(text: &str) -> Option<(bool, String)> {
    let text = text.trim_matches(['\t', '\n', '\x0b', '\x0c', '\r', ' ']);
    let negative = text.starts_with('-');
    let unsigned = text.strip_prefix(['+', '-']).unwrap_or(text);
    let (base, digits) = if unsigned.starts_with("0x") || unsigned.starts_with("0X") {
        (16, &unsigned[2..])
    } else if unsigned.starts_with("0b") || unsigned.starts_with("0B") {
        (2, &unsigned[2..])
    } else if unsigned.starts_with("0o") || unsigned.starts_with("0O") {
        (8, &unsigned[2..])
    } else if unsigned.starts_with("0d") || unsigned.starts_with("0D") {
        (10, &unsigned[2..])
    } else if unsigned.starts_with('0') && unsigned.len() > 1 {
        (8, unsigned)
    } else {
        (10, unsigned)
    };
    if digits.is_empty() {
        return None;
    }
    // Decimal digits, least significant first: Ruby's Integer has no native
    // integer overflow. Both argument parsers need exact radix conversion.
    let mut decimal = vec![0u8];
    let mut previous_digit = false;
    for ch in digits.chars() {
        if ch == '_' {
            if !previous_digit {
                return None;
            }
            previous_digit = false;
            continue;
        }
        let digit = ch.to_digit(base).filter(|_| ch.is_ascii())?;
        previous_digit = true;
        let mut carry = digit;
        for value in &mut decimal {
            let product = u32::from(*value) * base + carry;
            *value = (product % 10) as u8;
            carry = product / 10;
        }
        while carry > 0 {
            decimal.push((carry % 10) as u8);
            carry /= 10;
        }
    }
    if !previous_digit {
        return None;
    }
    while decimal.len() > 1 && decimal.last() == Some(&0) {
        decimal.pop();
    }
    let decimal: String = decimal
        .into_iter()
        .rev()
        .map(|digit| char::from(b'0' + digit))
        .collect();
    Some((negative && decimal != "0", decimal))
}

#[cfg(test)]
mod tests {
    #[test]
    fn generation_integer_conversion_matches_original_ruby_without_native_overflow() {
        use super::*;
        let fixture: serde_json::Value =
            serde_json::from_str(include_str!("ruby-integers.json")).unwrap();
        for case in fixture["cases"].as_array().unwrap() {
            let input = case["input"].as_str().unwrap();
            let value = canonical(input)
                .map(|(negative, decimal)| format!("{}{decimal}", if negative { "-" } else { "" }));
            assert_eq!(
                value.as_deref(),
                case.get("integer").and_then(serde_json::Value::as_str),
                "{input:?}"
            );
        }
    }
}
