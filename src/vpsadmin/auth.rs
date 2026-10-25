use super::*;
use chrono::{DateTime, FixedOffset, NaiveDate, NaiveDateTime, Utc};
use vpsadmin_client::TokenStep;
impl Client<'_> {
    pub(crate) async fn authenticate(&mut self) -> Result<(), ConfctlError> {
        self.user = self.read_line("User name: ", false).await?;
        self.password = self.read_line("Password: ", true).await?;
        self.login().await
    }
    async fn read_line(&mut self, prompt: &str, protected: bool) -> Result<String, ConfctlError> {
        check(self.cancel)?;
        let prompt = self.redact(prompt);
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
        let response = self
            .send(
                self.api
                    .authentication__token__token()
                    .request()
                    .user(&self.user)
                    .password(&self.password)
                    .lifetime("fixed")
                    .interval(60)
                    .send(),
            )
            .await?
            .output;
        if let Some(token) = &response.token {
            self.secret(token);
        }
        let mut expiry = response.valid_to.clone();
        let mut step = response
            .token_step()
            .map_err(|failure| self.api_error(failure))?;
        loop {
            check(self.cancel)?;
            match step {
                TokenStep::Complete { token } => {
                    self.valid_to = expiry
                        .as_deref()
                        .map(|text| iso8601(text).ok_or_else(|| error("invalid token expiry")))
                        .transpose()?;
                    self.api = api(&self.base, Some(token.expose_secret()))
                        .map_err(|failure| self.api_error(failure))?;
                    return check(self.cancel);
                }
                TokenStep::Challenge { token, next_action } => match next_action.as_str() {
                    "totp" => {
                        let code = self.read_line("Verification code: ", true).await?;
                        let response = self
                            .send(
                                self.api
                                    .authentication__token__token()
                                    .totp()
                                    .token(token.expose_secret())
                                    .code(code)
                                    .send(),
                            )
                            .await?
                            .output;
                        if let Some(token) = &response.token {
                            self.secret(token);
                        }
                        expiry = response.valid_to.clone();
                        step = response
                            .token_step()
                            .map_err(|failure| self.api_error(failure))?;
                    }
                    "reset_password" => {
                        let first = self.read_line("New password: ", true).await?;
                        let second = self.read_line("Confirm new password: ", true).await?;
                        let response = self
                            .send(
                                self.api
                                    .authentication__token__token()
                                    .reset_password()
                                    .token(token.expose_secret())
                                    .new_password1(&first)
                                    .new_password2(second)
                                    .send(),
                            )
                            .await?
                            .output;
                        // A successful reset changes credentials used by a later
                        // expiry-driven login; retain both values for redaction.
                        self.password = first;
                        if let Some(token) = &response.token {
                            self.secret(token);
                        }
                        expiry = response.valid_to.clone();
                        step = response
                            .token_step()
                            .map_err(|failure| self.api_error(failure))?;
                    }
                    "request" => {
                        let response = self
                            .send(
                                self.api
                                    .authentication__token__token()
                                    .request()
                                    .user(&self.user)
                                    .password(&self.password)
                                    .lifetime("fixed")
                                    .interval(60)
                                    .send(),
                            )
                            .await?
                            .output;
                        if let Some(token) = &response.token {
                            self.secret(token);
                        }
                        expiry = response.valid_to.clone();
                        step = response
                            .token_step()
                            .map_err(|failure| self.api_error(failure))?;
                    }
                    _ => return Err(error("unsupported API 7.0 authentication continuation")),
                },
                _ => return Err(error("unsupported API 7.0 authentication step")),
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
        if self.valid_to.is_some_and(|expiry| expiry < now) {
            self.login().await?;
        }
        Ok(())
    }
}
// Preserve the ISO8601 token-expiry contract, including compact
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
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn iso8601_token_expiry_preserves_offsets_and_fractional_instants() {
        for (input, instant, fraction) in [
            ("2026-10-20T00:00:00Z", 1792454400, 0),
            ("2026-10-20T00:00:00+00:00", 1792454400, 0),
            ("2026-10-20T00:00:00+0000", 1792454400, 0),
            ("2026-10-20T00:00:00+02", 1792447200, 0),
            ("2026-10-20T00:00:00+0230", 1792445400, 0),
            ("2026-10-20T00:00:00.123456789-0430", 1792470600, 123456789),
        ] {
            let parsed = iso8601(input).unwrap();
            assert_eq!(parsed.timestamp(), instant);
            assert_eq!(parsed.timestamp_subsec_nanos(), fraction);
        }
        for input in [
            "2026-10-20T00:00:00+000",
            "2026-10-20T00:00:00+02:",
            "2026-10-20T00:00:00+02:300",
            "2026-10-20T00:00:00Ztail",
            "not-an-iso8601-value",
        ] {
            assert!(iso8601(input).is_none());
        }
    }
}
