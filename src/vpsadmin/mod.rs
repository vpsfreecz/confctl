//! Prompting, cancellation and gen-data projection around the API 7.0 client.
//! Credentials and authentication responses are never logged.
mod auth;
use crate::{error::ConfctlError, events::EventSink, terminal::Ui};
use serde::de::DeserializeOwned;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;
use vpsadmin_client::{Association, Client as Api, Error as ApiError, resources};

pub(crate) struct Client<'a> {
    api: Api,
    base: String,
    ui: &'a Ui,
    cancel: &'a CancellationToken,
    user: String,
    password: String,
    valid_to: Option<chrono::DateTime<chrono::FixedOffset>>,
    secrets: Vec<String>,
    #[cfg(test)]
    clock: Option<std::sync::Arc<std::sync::atomic::AtomicI64>>,
}
fn error(message: &str) -> ConfctlError {
    ConfctlError::State(format!("vpsAdmin: {message}"))
}
pub(crate) fn check(cancel: &CancellationToken) -> Result<(), ConfctlError> {
    if cancel.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}
fn api(base: &str, token: Option<&str>) -> Result<Api, ApiError> {
    let builder = Api::builder(base).user_agent("haveapi-client");
    match token {
        Some(token) => builder.token(token),
        None => builder,
    }
    .build()
}
impl<'a> Client<'a> {
    pub(crate) fn new(
        ui: &'a Ui,
        _events: &'a EventSink,
        cancel: &'a CancellationToken,
    ) -> Result<Self, ConfctlError> {
        #[cfg(test)]
        if let Some(settings) = TEST_SETTINGS.lock().unwrap().clone() {
            let mut client = Self::construct(ui, cancel, test_base(&settings.endpoint)?.as_str())?;
            client.clock = Some(settings.clock);
            return Ok(client);
        }
        Self::construct(ui, cancel, "https://api.vpsfree.cz/")
    }
    fn construct(
        ui: &'a Ui,
        cancel: &'a CancellationToken,
        base: &str,
    ) -> Result<Self, ConfctlError> {
        Ok(Self {
            api: api(base, None).map_err(|_| error("unable to initialize API client"))?,
            base: base.into(),
            ui,
            cancel,
            user: String::new(),
            password: String::new(),
            valid_to: None,
            secrets: Vec::new(),
            #[cfg(test)]
            clock: None,
        })
    }
    fn secret(&mut self, value: &str) {
        if !value.is_empty() && !self.secrets.iter().any(|v| v == value) {
            self.secrets.push(value.into());
        }
    }
    fn redact(&self, text: &str) -> String {
        let mut secrets = self.secrets.iter().collect::<Vec<_>>();
        secrets.sort_by_key(|s| std::cmp::Reverse(s.len()));
        let mut text = text.to_owned();
        for secret in secrets {
            text = text.replace(secret, "[redacted]");
        }
        text
    }
    fn api_error(&self, failure: ApiError) -> ConfctlError {
        // SDK Display/Debug omit response values. Explicit server details must
        // additionally pass through the session's known-secret redactor.
        let mut message = failure.to_string();
        if let Some(detail) = failure.server_message() {
            message.push_str(": ");
            message.push_str(detail);
        }
        if let Some(details) = failure
            .validation_errors()
            .filter(|details| !details.is_empty())
        {
            message.push_str(": ");
            let redacted: serde_json::Map<String, Value> = details
                .iter()
                .map(|(name, messages)| {
                    (
                        self.redact(name),
                        Value::Array(
                            messages
                                .iter()
                                .map(|message| Value::String(self.redact(message)))
                                .collect(),
                        ),
                    )
                })
                .collect();
            message.push_str(&Value::Object(redacted).to_string());
        }
        error(&self.redact(&message))
    }
    async fn send<T>(
        &self,
        request: impl std::future::Future<Output = Result<T, ApiError>>,
    ) -> Result<T, ConfctlError> {
        check(self.cancel)?;
        let result = tokio::select! {
            biased;
            _ = self.cancel.cancelled() => return Err(ConfctlError::Cancelled),
            result = request => result.map_err(|failure| self.api_error(failure))?,
        };
        check(self.cancel)?;
        Ok(result)
    }
    pub(crate) async fn vps_node(&mut self, id: Value) -> Result<Value, ConfctlError> {
        self.ensure_token().await?;
        let vps = self
            .send(
                self.api
                    .vps()
                    .show(crate::domain::ruby_string(&id))
                    .global_meta(
                        json!({"includes":"node__location__environment"})
                            .as_object()
                            .unwrap()
                            .clone(),
                    )
                    .send(),
            )
            .await?
            .output;
        let node = self.node(vps.node).await?;
        let location = self.location(node.location.clone()).await?;
        let environment = self.environment(location.environment.clone()).await?;
        Ok(
            json!({"node":{"id":node.id,"name":node.name,"location":location.domain,
            "domain":environment.domain,"fqdn":format!("{}.{}",
                node.domain_name.as_deref().unwrap_or(""), environment.domain.as_deref().unwrap_or(""))}}),
        )
    }
    async fn node(
        &mut self,
        association: Option<Association>,
    ) -> Result<resources::node::show::Output, ConfctlError> {
        let association = association.ok_or_else(|| error("missing required node association"))?;
        if resolved(&association) {
            return included(&association);
        }
        self.ensure_token().await?;
        Ok(self
            .send(self.api.node().show(association_id(&association)?).send())
            .await?
            .output)
    }
    async fn location(
        &mut self,
        association: Option<Association>,
    ) -> Result<resources::location::show::Output, ConfctlError> {
        let association =
            association.ok_or_else(|| error("missing required location association"))?;
        if resolved(&association) {
            return included(&association);
        }
        self.ensure_token().await?;
        Ok(self
            .send(
                self.api
                    .location()
                    .show(association_id(&association)?)
                    .send(),
            )
            .await?
            .output)
    }
    async fn environment(
        &mut self,
        association: Option<Association>,
    ) -> Result<resources::environment::show::Output, ConfctlError> {
        let association =
            association.ok_or_else(|| error("missing required environment association"))?;
        if resolved(&association) {
            return included(&association);
        }
        self.ensure_token().await?;
        Ok(self
            .send(
                self.api
                    .environment()
                    .show(association_id(&association)?)
                    .send(),
            )
            .await?
            .output)
    }
    pub(crate) async fn networks(&mut self) -> Result<Vec<Value>, ConfctlError> {
        self.ensure_token().await?;
        let response = self.send(self.api.network().list().send()).await?;
        Ok(response
            .output
            .into_iter()
            .map(|network| {
                json!({"ip_version":network.ip_version,
            "address":network.address,"prefix":network.prefix})
            })
            .collect())
    }
}
fn resolved(association: &Association) -> bool {
    !matches!(
        association
            .as_map()
            .get("_meta")
            .and_then(|meta| meta.get("resolved")),
        None | Some(Value::Null | Value::Bool(false))
    )
}
fn included<T: DeserializeOwned>(association: &Association) -> Result<T, ConfctlError> {
    serde_json::from_value(Value::Object(association.as_map().clone()))
        .map_err(|_| error("invalid included association data"))
}
fn association_id(association: &Association) -> Result<String, ConfctlError> {
    let parameters = association
        .as_map()
        .get("_meta")
        .and_then(|meta| meta.get("path_params"))
        .and_then(Value::as_array)
        .filter(|parameters| parameters.len() == 1)
        .ok_or_else(|| error("missing association path parameters"))?;
    let id = &parameters[0];
    if id.is_array() || id.is_object() {
        return Err(error("invalid association path parameter"));
    }
    Ok(crate::domain::ruby_string(id))
}
#[cfg(test)]
#[derive(Clone)]
struct TestSettings {
    endpoint: String,
    clock: std::sync::Arc<std::sync::atomic::AtomicI64>,
}
#[cfg(test)]
static TEST_SETTINGS: std::sync::Mutex<Option<TestSettings>> = std::sync::Mutex::new(None);
#[cfg(test)]
pub(crate) fn configure_loopback(
    endpoint: &str,
    clock: std::sync::Arc<std::sync::atomic::AtomicI64>,
) -> Result<(), ConfctlError> {
    test_base(endpoint)?;
    *TEST_SETTINGS.lock().unwrap() = Some(TestSettings {
        endpoint: endpoint.into(),
        clock,
    });
    Ok(())
}
#[cfg(test)]
fn test_base(endpoint: &str) -> Result<reqwest::Url, ConfctlError> {
    let url = reqwest::Url::parse(endpoint).map_err(|_| error("invalid fixture endpoint"))?;
    let ip = url
        .host_str()
        .unwrap_or("")
        .trim_matches(['[', ']'])
        .parse::<std::net::IpAddr>()
        .map_err(|_| error("fixture endpoint must use a numeric loopback address"))?;
    if !ip.is_loopback()
        || url.scheme() != "http"
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || !url.path().ends_with('/')
    {
        return Err(error("invalid fixture loopback endpoint"));
    }
    Ok(url)
}
#[cfg(test)]
mod tests;
