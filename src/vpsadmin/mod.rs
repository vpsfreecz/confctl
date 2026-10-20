//! The described token and read-only resource subset used by gen-data.
//! No request, response or credential-bearing type implements Debug.
mod auth;
use crate::{
    error::ConfctlError,
    events::{EventSink, OutputStream, Style, UiEvent},
    terminal::Ui,
};
use reqwest::{
    Client as Http, Method, Url,
    header::{HeaderName, HeaderValue},
};
use serde_json::{Map, Value, json};
use tokio_util::sync::CancellationToken;

pub(crate) struct Client<'a> {
    http: Http,
    base: Url,
    ui: &'a Ui,
    events: &'a EventSink,
    cancel: &'a CancellationToken,
    description: Option<Value>,
    auth_description: Value,
    user: String,
    password: String,
    token: Option<String>,
    valid_to: Option<chrono::DateTime<chrono::FixedOffset>>,
    header: Option<HeaderName>,
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
fn truthy(v: Option<&Value>) -> bool {
    !matches!(v, None | Some(Value::Null | Value::Bool(false)))
}
impl<'a> Client<'a> {
    pub(crate) fn new(
        ui: &'a Ui,
        events: &'a EventSink,
        cancel: &'a CancellationToken,
    ) -> Result<Self, ConfctlError> {
        #[cfg(test)]
        if let Some(settings) = TEST_SETTINGS.lock().unwrap().clone() {
            return Self::loopback(ui, events, cancel, &settings.endpoint, settings.clock);
        }
        Self::construct(
            ui,
            events,
            cancel,
            Url::parse("https://api.vpsfree.cz/").unwrap(),
            false,
        )
    }
    fn construct(
        ui: &'a Ui,
        events: &'a EventSink,
        cancel: &'a CancellationToken,
        base: Url,
        no_proxy: bool,
    ) -> Result<Self, ConfctlError> {
        let mut builder = Http::builder()
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .user_agent("haveapi-client");
        if no_proxy {
            builder = builder.no_proxy();
        }
        let http = builder
            .build()
            .map_err(|_| error("unable to initialize HTTP transport"))?;
        Ok(Self {
            http,
            base,
            ui,
            events,
            cancel,
            description: None,
            auth_description: Value::Null,
            user: String::new(),
            password: String::new(),
            token: None,
            valid_to: None,
            header: None,
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
    fn diagnostic(&self, value: &Value) -> Value {
        match value {
            Value::String(s) => Value::String(self.redact(s)),
            Value::Array(a) => Value::Array(a.iter().map(|v| self.diagnostic(v)).collect()),
            Value::Object(m) => Value::Object(
                m.iter()
                    .map(|(k, v)| (self.redact(k), self.diagnostic(v)))
                    .collect(),
            ),
            _ => {
                let s = value.to_string();
                if self.secrets.iter().any(|secret| s.contains(secret)) {
                    Value::String("[redacted]".into())
                } else {
                    value.clone()
                }
            }
        }
    }
    async fn request(
        &self,
        method: Method,
        url: Url,
        body: Option<Value>,
        authenticated: bool,
    ) -> Result<Value, ConfctlError> {
        check(self.cancel)?;
        self.validate(&url)?;
        let mut request = self
            .http
            .request(method, url)
            .header("Accept", "application/json");
        if authenticated {
            let header = self
                .header
                .as_ref()
                .ok_or_else(|| error("missing token header"))?;
            let mut token = HeaderValue::from_str(
                self.token
                    .as_deref()
                    .ok_or_else(|| error("missing token"))?,
            )
            .map_err(|_| error("invalid token header value"))?;
            token.set_sensitive(true);
            request = request.header(header, token);
        }
        if let Some(body) = body {
            request = request
                .header("Content-Type", "application/json")
                .body(body.to_string());
        }
        let response = tokio::select! { r=request.send() => r.map_err(|_| error("HTTP transport failed"))?, _=self.cancel.cancelled()=>return Err(ConfctlError::Cancelled) };
        let status = response.status();
        if status.is_redirection() {
            return Err(error("HTTP redirects are refused"));
        }
        if status == reqwest::StatusCode::FORBIDDEN {
            return Err(error("access denied (HTTP 403)"));
        }
        let bytes = tokio::select! { r=response.bytes()=>r.map_err(|_| error("HTTP body read failed"))?, _=self.cancel.cancelled()=>return Err(ConfctlError::Cancelled) };
        check(self.cancel)?;
        // JSON parser excerpts can contain secrets; never retain its raw error.
        let value: Value =
            serde_json::from_slice(&bytes).map_err(|_| error("invalid JSON response"))?;
        if !status.is_success() || value.get("status").is_some_and(|v| !truthy(Some(v))) {
            let detail = json!({"message": self.diagnostic(&value["message"]), "errors": self.diagnostic(&value["errors"])});
            return Err(error(&format!(
                "request failed (HTTP {}): {detail}",
                status.as_u16()
            )));
        }
        Ok(value)
    }
    fn validate(&self, url: &Url) -> Result<(), ConfctlError> {
        if url.origin() != self.base.origin()
            || !url.username().is_empty()
            || url.password().is_some()
            || url.fragment().is_some()
            || !url.path().starts_with(self.base.path())
        {
            return Err(error("described URL escapes API origin or base"));
        }
        Ok(())
    }
    fn action_url(&self, spec: &Value, args: &[Value]) -> Result<Url, ConfctlError> {
        let path = spec["path"]
            .as_str()
            .ok_or_else(|| error("missing action path"))?;
        if path.starts_with("//") || path.chars().any(|c| c.is_control() || c == '\\') {
            return Err(error("invalid described action URL"));
        }
        // Reject traversal before URL normalization can erase its evidence.
        let decoded = percent_decode(path)?;
        if decoded
            .split(['/', '\\', '?', '#'])
            .any(|s| s == "." || s == "..")
        {
            return Err(error("escaping described action URL"));
        }
        let mut path = path.to_owned();
        for arg in args {
            let start = path
                .find('{')
                .ok_or_else(|| error("too many action path arguments"))?;
            let end = path[start..]
                .find('}')
                .ok_or_else(|| error("invalid action placeholder"))?
                + start;
            let id = crate::domain::ruby_string(arg);
            if id == "." || id == ".." {
                return Err(error("escaping action path argument"));
            }
            path.replace_range(start..=end, &encode_segment(&id));
        }
        if path.contains(['{', '}']) {
            return Err(error("unresolved action path arguments"));
        }
        let url = self
            .base
            .join(&path)
            .map_err(|_| error("invalid described action URL"))?;
        self.validate(&url)?;
        Ok(url)
    }
    async fn describe(&mut self, authenticated: bool) -> Result<Value, ConfctlError> {
        if authenticated {
            self.ensure_token().await?;
        }
        let mut url = self.base.clone();
        url.query_pairs_mut().append_pair("describe", "default");
        let response = self
            .request(Method::OPTIONS, url, None, authenticated)
            .await?;
        let version = response["version"]
            .as_str()
            .ok_or_else(|| error("missing protocol version"))?;
        let mut parts = version.split('.');
        if parts.next() != Some("2") {
            return Err(error("incompatible API protocol major version"));
        }
        if version != "2.0" {
            self.events
                .emit(UiEvent::Diagnostic {
                    stream: OutputStream::Stderr,
                    style: Style::Plain,
                    text: "vpsAdmin: server uses a different protocol minor version\n".into(),
                })
                .await?;
            check(self.cancel)?;
        }
        response["response"]
            .as_object()
            .ok_or_else(|| error("invalid API description"))?;
        Ok(response["response"].clone())
    }
    async fn resources(&mut self) -> Result<(), ConfctlError> {
        if self.description.is_none() {
            self.description = Some(self.describe(true).await?);
        }
        Ok(())
    }
    async fn data_action(
        &mut self,
        spec: &Value,
        args: &[Value],
        includes: bool,
        layout: &str,
    ) -> Result<Value, ConfctlError> {
        if truthy(spec.get("blocking")) {
            return Err(error("blocking data actions are unsupported"));
        }
        self.ensure_token().await?;
        self.call(
            spec,
            args,
            auth::inputs(spec, &Map::new())?,
            includes,
            true,
            layout,
        )
        .await
    }
    async fn call(
        &self,
        spec: &Value,
        args: &[Value],
        input: Map<String, Value>,
        includes: bool,
        authenticated: bool,
        layout: &str,
    ) -> Result<Value, ConfctlError> {
        if spec["output"]["layout"].as_str() != Some(layout) {
            return Err(error("unsupported action output layout"));
        }
        let ns = spec["output"]["namespace"]
            .as_str()
            .ok_or_else(|| error("missing output namespace"))?;
        let mut url = self.action_url(spec, args)?;
        let method = match spec["method"].as_str() {
            Some("GET") => Method::GET,
            Some("POST") => Method::POST,
            Some("PUT") => Method::PUT,
            Some("DELETE") => Method::DELETE,
            _ => return Err(error("unsupported described HTTP method")),
        };
        let body = if method == Method::POST || method == Method::PUT {
            let mut body = Map::new();
            if !spec["input"].is_null() {
                let n = spec["input"]["namespace"]
                    .as_str()
                    .ok_or_else(|| error("missing input namespace"))?;
                body.insert(n.into(), Value::Object(input));
            }
            if includes {
                body.insert(
                    "_meta".into(),
                    json!({"includes":"node__location__environment"}),
                );
            }
            Some(Value::Object(body))
        } else {
            if !input.is_empty() {
                let n = spec["input"]["namespace"]
                    .as_str()
                    .ok_or_else(|| error("missing input namespace"))?;
                for (k, v) in input {
                    url.query_pairs_mut()
                        .append_pair(&format!("{n}[{k}]"), &crate::domain::ruby_string(&v));
                }
            }
            if includes {
                url.query_pairs_mut()
                    .append_pair("_meta[includes]", "node__location__environment");
            }
            None
        };
        let response = self.request(method, url, body, authenticated).await?;
        if response.get("status").and_then(Value::as_bool) != Some(true) {
            return Err(error("response is missing successful API status"));
        }
        let output = response
            .get("response")
            .and_then(|v| v.get(ns))
            .ok_or_else(|| error("missing namespaced action response"))?;
        if layout == "object" && !output.is_object()
            || layout == "object_list" && !output.is_array()
        {
            return Err(error("invalid namespaced action response shape"));
        }
        Ok(output.clone())
    }
    pub(crate) async fn vps_node(&mut self, id: Value) -> Result<Value, ConfctlError> {
        self.resources().await?;
        let description = self.description.as_ref().unwrap().clone();
        let vps = action(&description, &["vps".into()], "show")?;
        let ct = self.data_action(&vps, &[id], true, "object").await?;
        let (node, node_spec) = self.association(&description, &vps, &ct, "node").await?;
        let (location, location_spec) = self
            .association(&description, &node_spec, &node, "location")
            .await?;
        let (environment, _) = self
            .association(&description, &location_spec, &location, "environment")
            .await?;
        for value in [
            &node["id"],
            &node["name"],
            &node["domain_name"],
            &location["domain"],
            &environment["domain"],
        ] {
            if value.is_object() || value.is_array() {
                return Err(error("expected scalar node data"));
            }
        }
        Ok(
            json!({"node":{"id":node["id"],"name":node["name"],"location":location["domain"],"domain":environment["domain"],"fqdn":format!("{}.{}",crate::domain::ruby_string(&node["domain_name"]),crate::domain::ruby_string(&environment["domain"]))}}),
        )
    }
    async fn association(
        &mut self,
        description: &Value,
        parent_spec: &Value,
        parent: &Value,
        field: &str,
    ) -> Result<(Value, Value), ConfctlError> {
        let parameter = &parent_spec["output"]["parameters"][field];
        if parameter["type"] != "Resource" {
            return Err(error("required association is not described"));
        }
        let path = parameter["resource"]
            .as_array()
            .ok_or_else(|| error("missing association resource path"))?
            .iter()
            .map(|v| {
                v.as_str()
                    .map(String::from)
                    .ok_or_else(|| error("invalid association resource path"))
            })
            .collect::<Result<Vec<_>, _>>()?;
        let spec = action(description, &path, "show")?;
        let value = parent
            .get(field)
            .filter(|v| v.is_object())
            .ok_or_else(|| error("missing required association"))?;
        if truthy(value.pointer("/_meta/resolved")) {
            return Ok((value.clone(), spec));
        }
        let args = value
            .pointer("/_meta/path_params")
            .and_then(Value::as_array)
            .ok_or_else(|| error("missing association path parameters"))?;
        let resolved = self.data_action(&spec, args, false, "object").await?;
        Ok((resolved, spec))
    }
    pub(crate) async fn networks(&mut self) -> Result<Vec<Value>, ConfctlError> {
        self.resources().await?;
        let spec = action(
            self.description.as_ref().unwrap(),
            &["network".into()],
            "list",
        )?;
        let values = self.data_action(&spec, &[], false, "object_list").await?;
        let values = values.as_array().unwrap();
        if values.iter().any(|v| !v.is_object()) {
            return Err(error("invalid network list entry"));
        }
        for v in values {
            for k in ["ip_version", "address", "prefix"] {
                if v[k].is_object() || v[k].is_array() {
                    return Err(error("expected scalar network data"));
                }
            }
        }
        Ok(values.clone())
    }
}
fn action(description: &Value, path: &[String], name: &str) -> Result<Value, ConfctlError> {
    let mut resource = description;
    for part in path {
        resource = resource
            .get("resources")
            .and_then(|v| v.get(part))
            .ok_or_else(|| error("described resource unavailable"))?;
    }
    let actions = resource["actions"]
        .as_object()
        .ok_or_else(|| error("missing described actions"))?;
    actions
        .get(name)
        .or_else(|| {
            actions.values().find(|v| {
                v["aliases"]
                    .as_array()
                    .is_some_and(|a| a.iter().any(|v| v == name))
            })
        })
        .cloned()
        .ok_or_else(|| error("described action unavailable"))
}
fn encode_segment(s: &str) -> String {
    let mut out = String::new();
    for byte in s.bytes() {
        if byte.is_ascii_alphanumeric() || b"-._~".contains(&byte) {
            out.push(byte as char);
        } else {
            out.push_str(&format!("%{byte:02X}"));
        }
    }
    out
}
fn percent_decode(s: &str) -> Result<String, ConfctlError> {
    let b = s.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' {
            if i + 2 >= b.len() {
                return Err(error("invalid described URL escape"));
            }
            let h = std::str::from_utf8(&b[i + 1..i + 3])
                .ok()
                .and_then(|s| u8::from_str_radix(s, 16).ok())
                .ok_or_else(|| error("invalid described URL escape"))?;
            out.push(h);
            i += 3;
        } else {
            out.push(b[i]);
            i += 1;
        }
    }
    String::from_utf8(out).map_err(|_| error("invalid described URL encoding"))
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
fn test_base(endpoint: &str) -> Result<Url, ConfctlError> {
    let url = Url::parse(endpoint).map_err(|_| error("invalid fixture endpoint"))?;
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
impl<'a> Client<'a> {
    fn loopback(
        ui: &'a Ui,
        events: &'a EventSink,
        cancel: &'a CancellationToken,
        endpoint: &str,
        clock: std::sync::Arc<std::sync::atomic::AtomicI64>,
    ) -> Result<Self, ConfctlError> {
        let mut client = Self::construct(ui, events, cancel, test_base(endpoint)?, true)?;
        client.clock = Some(clock);
        Ok(client)
    }
}
#[cfg(test)]
mod tests;
