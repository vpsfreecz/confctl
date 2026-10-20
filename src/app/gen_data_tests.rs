//! Actual parser/app/CoreOperations fixtures, confined to owned numeric loopback.
use super::*;
use serde_json::{Value, json};
use std::{
    fs,
    io::Read,
    net::{TcpListener, TcpStream},
    os::unix::fs::PermissionsExt,
    path::Path,
    process::{Command, Output, Stdio},
    sync::atomic::AtomicI64,
    thread,
    time::Instant,
};
const FIXTURE: &str = "app::gen_data_tests::actual_gen_data_application_fixture";
const ROOT: &str = "CONFCTL_GEN_DATA_TEST_ROOT";
const MODE: &str = "CONFCTL_GEN_DATA_TEST_MODE";
const USER: &str = "fixture-user-39281";
const PASSWORD: &str = " fixture-password-83641 ";
const CODE: &str = "fixture-code-81624";
const STEP: &str = "fixture-step-59321";
const TOKEN: &str = "fixture-session-78162";
#[test]
fn actual_gen_data_application_fixture() {
    let Some(root) = std::env::var_os(ROOT) else {
        return;
    };
    let root = PathBuf::from(root);
    let mode = std::env::var(MODE).unwrap();
    let original = if mode.starts_with("pty-") {
        crate::terminal::pty_tests::enter_controlling_session();
        Some(rustix::termios::tcgetattr(io::stdin()).unwrap())
    } else {
        None
    };
    crate::vpsadmin::configure_loopback(
        &std::env::var("CONFCTL_GEN_DATA_TEST_ENDPOINT").unwrap(),
        Arc::new(AtomicI64::new(100)),
    )
    .unwrap();
    let args: Vec<String> =
        serde_json::from_slice(&fs::read(root.join("argv.json")).unwrap()).unwrap();
    let stdout: SharedWriter = if mode == "output-fail" {
        Arc::new(Mutex::new(Box::new(FailingOutput)))
    } else {
        Arc::new(Mutex::new(Box::new(io::stdout())))
    };
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let code = runtime.block_on(execute(
        args.into_iter().map(Into::into).collect(),
        Timing::start(),
        &crate::operations::CoreOperations,
        Ok(root),
        stdout,
        Arc::new(Mutex::new(Box::new(io::stderr()))),
    ));
    if let Some(original) = original {
        crate::terminal::pty_tests::assert_modes(
            &original,
            &rustix::termios::tcgetattr(io::stdin()).unwrap(),
        );
    }
    std::process::exit(code);
}
struct FailingOutput;
impl Write for FailingOutput {
    fn write(&mut self, _: &[u8]) -> io::Result<usize> {
        Err(io::Error::other("fixture output failure"))
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}
fn parameter(kind: &str) -> Value {
    json!({"type":kind,"validators":{}})
}
fn action(path: &str, method: &str, ns: &str, params: Value, output: Value, layout: &str) -> Value {
    json!({"path":path,"method":method,"aliases":[],"blocking":false,"input":{"layout":"object","namespace":ns,"parameters":params},"output":{"layout":layout,"namespace":ns,"parameters":output}})
}
fn reference(resource: &str) -> Value {
    json!({"type":"Resource","resource":[resource],"value_id":"id","value_label":"id"})
}
fn resource(a: Value) -> Value {
    json!({"resources":{},"actions":{"show":a}})
}
fn description(mode: &str) -> Value {
    let scalar = parameter("String");
    let request = action(
        "/auth/start",
        "POST",
        "login",
        json!({"user":scalar,"password":parameter("String"),"lifetime":scalar,"interval":parameter("Integer")}),
        json!({}),
        "object",
    );
    let challenge = action(
        "/auth/next",
        "POST",
        "challenge",
        json!({"token":scalar,"code":{"type":"String","label":"Verification code","protected":true,"validators":{"presence":true}}}),
        json!({}),
        "object",
    );
    let mut d = json!({"authentication":{"token":{"http_header":"X-Gen-Token","resources":{"token":{"actions":{"request":request,"challenge":challenge,"empty":action("/auth/empty","POST","empty",json!({}),json!({}),"object")}}}}},"resources":{
        "vps":resource(action("/vps/{id}","GET","instance",json!({}),json!({"node":reference("node")}),"object")),
        "node":resource(action("/nodes/{id}","GET","node",json!({}),json!({"location":reference("location")}),"object")),
        "location":resource(action("/locations/{id}","GET","location",json!({}),json!({"environment":reference("environment")}),"object")),
        "environment":resource(action("/environments/{id}","GET","environment",json!({}),json!({}),"object")),
        "network":{"resources":{},"actions":{"fetch":action("/nets","GET","items",json!({}),json!({}),"object_list")}}
    }});
    d["resources"]["network"]["actions"]["fetch"]["aliases"] = json!(["list"]);
    if mode == "foreign-auth" {
        d["authentication"]["token"]["resources"]["token"]["actions"]["request"]["path"] =
            json!("https://example.invalid/stolen");
    }
    if mode == "userinfo" {
        d["authentication"]["token"]["resources"]["token"]["actions"]["request"]["path"] =
            json!("http://user@127.0.0.1/stolen");
    }
    if mode == "required" {
        d["authentication"]["token"]["resources"]["token"]["actions"]["request"]["input"]["parameters"]
            ["unsupported"] = json!({"type":"String","validators":{"required":true}});
    }
    if mode == "layout" {
        d["resources"]["network"]["actions"]["fetch"]["output"]["layout"] = json!("hash_list");
    }
    if mode == "blocking" {
        d["resources"]["network"]["actions"]["fetch"]["blocking"] = json!(true);
    }
    if mode.starts_with("types") {
        d["authentication"]["token"]["resources"]["token"]["actions"]["challenge"]["input"]["parameters"] = json!({"token":scalar,"text":{"type":"Text","label":"Public text"},"integer":parameter("Integer"),"float":parameter("Float"),"boolean":parameter("Boolean"),"datetime":parameter("Datetime"),"resource":parameter("Resource"),"nullable":{"type":"String","nullable":true},"delegated":{"type":"String","validators":{"format":"server-only"}}});
    }
    d
}
fn completed(expiry: Value) -> Value {
    json!({"complete":true,"token":TOKEN,"valid_to":expiry})
}
fn node(id: Value) -> Value {
    json!({"id":id,"name":"node","domain_name":"node.test","location":{"domain":"test","environment":{"domain":"example.invalid","_meta":{"resolved":true}},"_meta":{"resolved":true}},"_meta":{"resolved":true}})
}
fn token_expiry(mode: &str) -> Value {
    match mode {
        "expire" => json!("1970-01-01T00:00:50Z"),
        "expire-compact" => json!("1970-01-01T00:00:50+0000"),
        "expire-offset" => json!("1970-01-01T01:00:50.5+0100"),
        "expire-future-offset" => json!("1970-01-01T00:00:50-0100"),
        "expire-future-fraction" => json!("1970-01-01T00:01:40.5+0000"),
        "expire-malformed" => json!("1970-01-01T00:00:50+000"),
        _ => Value::Null,
    }
}
struct Server {
    endpoint: String,
    events: Arc<Mutex<Vec<String>>>,
    failed: Arc<AtomicBool>,
    stop: Arc<AtomicBool>,
    join: Option<thread::JoinHandle<()>>,
}
impl Server {
    fn new(mode: &str, root: &Path) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let endpoint = format!("http://{}/", listener.local_addr().unwrap());
        let events = Arc::new(Mutex::new(Vec::new()));
        let failed = Arc::new(AtomicBool::new(false));
        let stop = Arc::new(AtomicBool::new(false));
        let (e, f, s) = (events.clone(), failed.clone(), stop.clone());
        let mode = mode.to_owned();
        let root = root.to_owned();
        let join = thread::spawn(move || {
            let mut auth_count = 0;
            while !s.load(Ordering::SeqCst) {
                let (mut stream, _) = match listener.accept() {
                    Ok(v) => v,
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(5));
                        continue;
                    }
                    Err(_) => break,
                };
                stream
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                let Some((method, path, headers, body)) = read_request(&mut stream) else {
                    f.store(true, Ordering::SeqCst);
                    continue;
                };
                e.lock().unwrap().push(format!("{method} {path}"));
                let valid = headers.contains("user-agent: haveapi-client")
                    && headers.contains("accept: application/json");
                if !valid {
                    f.store(true, Ordering::SeqCst);
                }
                let mut status = 200;
                let mut raw = None;
                let mut redirect = false;
                let response = if method == "OPTIONS" {
                    if e.lock()
                        .unwrap()
                        .iter()
                        .filter(|v| v.starts_with("OPTIONS"))
                        .count()
                        > 1
                        && !headers.contains(&format!("x-gen-token: {TOKEN}"))
                    {
                        f.store(true, Ordering::SeqCst);
                    }
                    if e.lock()
                        .unwrap()
                        .iter()
                        .filter(|v| v.starts_with("OPTIONS"))
                        .count()
                        > 1
                    {
                        let argv: Vec<String> =
                            serde_json::from_slice(&fs::read(root.join("argv.json")).unwrap())
                                .unwrap();
                        if argv[2] != "network"
                            && fs::read_to_string(root.join("nix-calls"))
                                .map(|s| s.lines().count())
                                .unwrap_or(0)
                                != 3
                        {
                            f.store(true, Ordering::SeqCst);
                        }
                    }
                    json!({"version":if mode=="major"{"3.0"}else if mode=="minor"{"2.1"}else{"2.0"},"response":description(&mode)})
                } else if path == "/auth/start" {
                    auth_count += 1;
                    let valid = body["login"]["user"] == USER
                        && body["login"]["password"] == PASSWORD
                        && body["login"]["lifetime"] == "fixed"
                        && body["login"]["interval"] == 60;
                    if !valid {
                        f.store(true, Ordering::SeqCst);
                    }
                    if mode == "auth-error" {
                        status = 400;
                        json!({"status":false,"message":format!("bad {PASSWORD}"),"errors":{"password":[PASSWORD]}})
                    } else {
                        json!({"status":true,"response":{"login":if mode.starts_with("expire")&&auth_count>1{completed(Value::Null)}else if mode=="empty-step" || mode=="multi"{json!({"complete":false,"next_action":"empty","token":STEP})}else{json!({"complete":false,"next_action":"challenge","token":STEP})}}})
                    }
                } else if path == "/auth/next" {
                    let valid = if mode.starts_with("types") {
                        body["challenge"]["text"] == " public "
                            && body["challenge"]["integer"] == 7
                            && body["challenge"]["float"] == 1.25
                            && body["challenge"]["boolean"] == true
                            && body["challenge"]["datetime"]
                                == if mode == "types-compact" {
                                    "2026-10-20T00:00:00-04:30"
                                } else {
                                    "2026-10-20T00:00:00+00:00"
                                }
                            && body["challenge"]["resource"] == 9
                            && body["challenge"]["nullable"].is_null()
                            && body["challenge"]["delegated"] == "arbitrary"
                    } else {
                        body["challenge"]["code"] == CODE
                    };
                    if !valid || body["challenge"]["token"] != STEP {
                        f.store(true, Ordering::SeqCst);
                    }
                    json!({"status":true,"response":{"challenge":completed(token_expiry(&mode))}})
                } else if path == "/auth/empty" {
                    if body != json!({"empty":{}}) {
                        f.store(true, Ordering::SeqCst);
                    }
                    json!({"status":true,"response":{"empty":if mode=="multi"{json!({"complete":false,"next_action":"challenge","token":STEP})}else{completed(Value::Null)}}})
                } else {
                    if !headers.contains(&format!("x-gen-token: {TOKEN}")) {
                        f.store(true, Ordering::SeqCst);
                    }
                    if path.starts_with("/vps/") {
                        if !path.contains("_meta%5Bincludes%5D=node__location__environment") {
                            f.store(true, Ordering::SeqCst);
                        }
                        let id = if path.starts_with("/vps/2") {
                            json!(8)
                        } else {
                            json!(7)
                        };
                        let n = if mode == "lazy" {
                            json!({"_meta":{"resolved":false,"path_params":["node/7"]}})
                        } else {
                            node(id)
                        };
                        if mode == "missing-association" {
                            json!({"status":true,"response":{"instance":{"node":null}}})
                        } else {
                            json!({"status":true,"response":{"instance":{"node":n}}})
                        }
                    } else if path == "/nodes/node%2F7" {
                        let mut n = node(json!(7));
                        n["location"] = json!({"_meta":{"path_params":[4]}});
                        json!({"status":true,"response":{"node":n}})
                    } else if path == "/locations/4" {
                        json!({"status":true,"response":{"location":{"domain":"test","environment":{"_meta":{"path_params":[5]}}}}})
                    } else if path == "/environments/5" {
                        json!({"status":true,"response":{"environment":{"domain":"example.invalid"}}})
                    } else if path == "/nets" {
                        let argv: Vec<String> =
                            serde_json::from_slice(&fs::read(root.join("argv.json")).unwrap())
                                .unwrap();
                        if argv[2] == "all"
                            && !fs::read_to_string(root.join("data/vpsadmin/containers.nix"))
                                .unwrap_or_default()
                                .contains("id = 7")
                        {
                            f.store(true, Ordering::SeqCst);
                        }
                        match mode.as_str() {
                            "redirect" => {
                                status = 302;
                                redirect = true;
                            }
                            "bad-json" => raw = Some(b"{broken".to_vec()),
                            "http-error" | "partial-all" => status = 404,
                            "denied" => status = 403,
                            _ => {}
                        }
                        if mode == "empty" {
                            json!({"status":true,"response":{"items":[],"_meta":{"total_count":999}}})
                        } else if mode == "wrong-shape" {
                            json!({"status":true,"response":{"items":null}})
                        } else if matches!(mode.as_str(), "http-error" | "partial-all") {
                            json!({"status":false,"message":format!("not found {TOKEN}"),"errors":{"item":[PASSWORD,CODE]}})
                        } else {
                            json!({"status":true,"response":{"items":[{"ip_version":4,"address":"192.0.2.0","prefix":24},{"ip_version":"4","address":"omit","prefix":0},{"ip_version":6,"address":"2001:db8::","prefix":32},{"ip_version":4,"address":"192.0.2.0","prefix":24}],"_meta":{"total_count":999}}})
                        }
                    } else {
                        f.store(true, Ordering::SeqCst);
                        status = 500;
                        json!({})
                    }
                };
                let bytes = raw.unwrap_or_else(|| response.to_string().into_bytes());
                let header = format!(
                    "HTTP/1.1 {status} Fixture\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n{}\r\n",
                    bytes.len(),
                    if redirect {
                        "Location: https://api.vpsfree.cz/forbidden\r\n"
                    } else {
                        ""
                    }
                );
                let _ = stream.write_all(header.as_bytes());
                if mode == "body-cancel" && path == "/nets"
                    || mode == "auth-body-cancel" && path == "/auth/start"
                {
                    let _ = stream.write_all(&bytes[..1]);
                    fs::write(root.join("body-blocked"), b"").unwrap();
                    while !s.load(Ordering::SeqCst) && !root.join("release-body").exists() {
                        thread::sleep(Duration::from_millis(5));
                    }
                }
                let _ = stream.write_all(&bytes);
                let _ = stream.flush();
            }
        });
        Self {
            endpoint,
            events,
            failed,
            stop,
            join: Some(join),
        }
    }
    fn events(&self) -> Vec<String> {
        assert!(
            !self.failed.load(Ordering::SeqCst),
            "fixture observed an invalid request (values withheld)"
        );
        self.events.lock().unwrap().clone()
    }
}
impl Drop for Server {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        self.join.take().unwrap().join().unwrap();
    }
}
fn read_request(stream: &mut TcpStream) -> Option<(String, String, String, Value)> {
    let mut bytes = Vec::new();
    let mut byte = [0];
    while !bytes.ends_with(b"\r\n\r\n") {
        if stream.read(&mut byte).ok()? == 0 {
            return None;
        }
        bytes.push(byte[0]);
        if bytes.len() > 65536 {
            return None;
        }
    }
    let header = String::from_utf8(bytes).ok()?;
    let mut first = header.lines().next()?.split_whitespace();
    let method = first.next()?.into();
    let path = first.next()?.into();
    let lower = header.to_ascii_lowercase();
    let length = lower
        .lines()
        .find_map(|l| l.strip_prefix("content-length: "))
        .and_then(|v| v.parse::<usize>().ok())
        .unwrap_or(0);
    if length > 65536 {
        return None;
    }
    let mut body = vec![0; length];
    stream.read_exact(&mut body).ok()?;
    let body = if body.is_empty() {
        Value::Null
    } else {
        serde_json::from_slice(&body).ok()?
    };
    Some((method, path, lower, body))
}
struct Fixture {
    _scratch: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let bin = scratch.path().join("bin");
        fs::create_dir_all(root.join("data/vpsadmin/networks")).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::write(root.join("flake.nix"), b"{ outputs = _: {}; }\n").unwrap();
        fs::write(root.join("machines.json"),json!({"one":{"name":"alpha","metaConfig":{"managed":false,"host":{"name":"alpha","location":"test","domain":"example.invalid","target":null},"container":{"id":1}}},"absent":{"name":"absent","metaConfig":{}},"null":{"name":"null","metaConfig":{"container":null}}}).to_string()).unwrap();
        let nix = r#"#!/bin/sh
printf '%s\n' "$*" >> "$CONFCTL_GEN_DATA_TEST_ROOT/nix-calls"
last=
for arg do last="$arg"; done
if [ "$CONFCTL_GEN_DATA_TEST_MODE" = nix-cancel ] && [ "$last" = '.#confctl.machinesJson' ]; then
  printf '%s\n' "$$" > "$CONFCTL_GEN_DATA_TEST_ROOT/nix-blocked"
  exec sleep 120
fi
if [ "$CONFCTL_GEN_DATA_TEST_MODE" = nix-error ]; then printf 'native fixture failure' >&2; exit 7; fi
case "$last" in
  '.#confctl.settings') printf '%s\n' '{"nix":{}}' ;;
  '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$CONFCTL_GEN_DATA_TEST_ROOT" ;;
  *) exit 97 ;;
esac
"#;
        fs::write(bin.join("nix"), nix).unwrap();
        fs::set_permissions(bin.join("nix"), fs::Permissions::from_mode(0o755)).unwrap();
        Self {
            _scratch: scratch,
            root,
            bin,
        }
    }
    fn command(&self, server: &Server, mode: &str, route: &str) -> Command {
        let args = vec!["gen-data", "vpsadmin", route, "ignored-surplus"];
        fs::write(
            self.root.join("argv.json"),
            serde_json::to_vec(&args).unwrap(),
        )
        .unwrap();
        let path = std::env::join_paths(
            std::iter::once(self.bin.clone())
                .chain(std::env::split_paths(&std::env::var_os("PATH").unwrap())),
        )
        .unwrap();
        let mut c = Command::new(std::env::current_exe().unwrap());
        c.args(["--exact", FIXTURE, "--nocapture"])
            .env(ROOT, &self.root)
            .env(MODE, mode)
            .env("CONFCTL_GEN_DATA_TEST_ENDPOINT", &server.endpoint)
            .env("PATH", path)
            .env("HTTP_PROXY", "http://192.0.2.1:1")
            .env("HTTPS_PROXY", "http://192.0.2.1:1")
            .env("ALL_PROXY", "http://192.0.2.1:1")
            .env("NO_PROXY", "")
            .env("CONFCTL_TTY", "0")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        c
    }
    fn run(&self, server: &Server, mode: &str, route: &str, input: &str) -> Output {
        let mut child = self.command(server, mode, route).spawn().unwrap();
        child
            .stdin
            .take()
            .unwrap()
            .write_all(input.as_bytes())
            .unwrap();
        child.wait_with_output().unwrap()
    }
    fn secret_free(&self, out: &Output) {
        let mut bytes = out.stdout.clone();
        bytes.extend(&out.stderr);
        if let Ok(entries) = fs::read_dir(self.root.join(".confctl/logs")) {
            for e in entries {
                bytes.extend(fs::read(e.unwrap().path()).unwrap());
            }
        }
        for secret in [USER, PASSWORD, CODE, STEP, TOKEN] {
            assert!(
                !bytes.windows(secret.len()).any(|b| b == secret.as_bytes()),
                "synthetic secret leaked (value withheld)"
            );
        }
    }
    fn containers(&self) -> PathBuf {
        self.root.join("data/vpsadmin/containers.nix")
    }
    fn networks(&self) -> PathBuf {
        self.root.join("data/vpsadmin/networks/containers.nix")
    }
}
fn credentials() -> String {
    format!("{USER}\n{PASSWORD}\n{CODE}\n")
}
fn succeeded(out: &Output) {
    assert!(
        out.status.success(),
        "{}",
        String::from_utf8_lossy(&out.stderr)
    );
}
#[test]
fn gen_data_all_publishes_fixture_bytes_once_preserving_modes_and_native_order() {
    let f = Fixture::new();
    fs::write(f.containers(), b"old").unwrap();
    fs::set_permissions(f.containers(), fs::Permissions::from_mode(0o640)).unwrap();
    let server = Server::new("normal", &f.root);
    let out = f.run(&server, "normal", "all", &credentials());
    succeeded(&out);
    f.secret_free(&out);
    assert_eq!(
        fs::read_to_string(f.containers()).unwrap(),
        "{\n  \"alpha.test.example.invalid\" = {\n    node = {\n      domain = \"example.invalid\";\n      fqdn = \"node.test.example.invalid\";\n      id = 7;\n      location = \"test\";\n      name = \"node\";\n    };\n  };\n}\n"
    );
    let text = fs::read_to_string(f.networks()).unwrap();
    assert!(text.starts_with("{\n  \"ipv4\" = [\n"));
    assert_eq!(text.matches("192.0.2.0").count(), 2);
    assert!(!text.contains("omit"));
    assert_eq!(
        fs::metadata(f.containers()).unwrap().permissions().mode() & 0o777,
        0o640
    );
    let calls = server.events();
    assert_eq!(calls.iter().filter(|s| s.starts_with("OPTIONS")).count(), 2);
    assert_eq!(calls.iter().filter(|s| s.ends_with("/nets")).count(), 1);
    assert!(calls.last().unwrap().ends_with("/nets"));
    assert!(!String::from_utf8_lossy(&out.stderr).contains("Timing:"));
    let nix = fs::read_to_string(f.root.join("nix-calls")).unwrap();
    assert_eq!(nix.lines().count(), 3);
    assert_eq!(
        nix.lines()
            .filter(|l| l.ends_with(".#confctl.settings"))
            .count(),
        2
    );
    assert!(
        nix.lines()
            .last()
            .unwrap()
            .ends_with(".#confctl.machinesJson")
    );
    assert!(!nix.contains("system.build"));
}
#[test]
fn gen_data_network_has_no_flake_nix_pagination_or_credential_replay() {
    for mode in [
        "normal",
        "empty",
        "empty-step",
        "multi",
        "types",
        "expire",
        "minor",
    ] {
        let f = Fixture::new();
        fs::remove_file(f.root.join("flake.nix")).unwrap();
        let server = Server::new(mode, &f.root);
        let input = if mode.starts_with("types") {
            format!(
                "{USER}\n{PASSWORD}\n public \n+007\n1.25\nYES\n2026-10-20\n009\n \narbitrary\n"
            )
        } else {
            credentials()
        };
        let out = f.run(&server, mode, "network", &input);
        succeeded(&out);
        f.secret_free(&out);
        assert!(f.networks().exists());
        assert!(!f.root.join("nix-calls").exists());
        assert_eq!(
            server
                .events()
                .iter()
                .filter(|s| s.ends_with("/nets"))
                .count(),
            1
        );
        assert_eq!(
            server
                .events()
                .iter()
                .filter(|s| s.ends_with("/auth/start"))
                .count(),
            if mode == "expire" { 2 } else { 1 }
        );
    }
}
#[test]
fn gen_data_containers_keep_inventory_filtering_duplicates_empty_and_lazy_associations() {
    for mode in ["normal", "lazy", "empty"] {
        let f = Fixture::new();
        let machines = if mode == "empty" {
            json!({"x":{"name":"x","metaConfig":{"container":null}}})
        } else {
            json!({"a":{"name":"one","carrier":"missing","metaConfig":{"managed":false,"host":{"name":"","location":null,"domain":"d"},"container":{"id":"a/b ?é"}}},"b":{"name":"two","metaConfig":{"host":{"name":"","domain":"d"},"container":{"id":2}}},"c":{"name":"repeated-id","metaConfig":{"host":{"name":"","domain":"d"},"container":{"id":2}}}})
        };
        fs::write(f.root.join("machines.json"), machines.to_string()).unwrap();
        let server = Server::new(mode, &f.root);
        let out = f.run(&server, mode, "containers", &credentials());
        succeeded(&out);
        f.secret_free(&out);
        let text = fs::read_to_string(f.containers()).unwrap();
        if mode == "empty" {
            assert_eq!(text, "{\n}\n");
            assert_eq!(
                server
                    .events()
                    .iter()
                    .filter(|s| s.starts_with("OPTIONS"))
                    .count(),
                1
            );
        } else {
            assert!(text.contains("\".d\""));
            assert_eq!(text.matches("node =").count(), 1);
            let calls = server.events();
            assert_eq!(
                calls.iter().filter(|s| s.starts_with("GET /vps/")).count(),
                3
            );
            assert!(calls.iter().any(|s| s.contains("a%2Fb%20%3F%C3%A9")));
            if mode == "lazy" {
                assert_eq!(
                    calls
                        .iter()
                        .filter(|s| s.starts_with("GET /nodes/"))
                        .count(),
                    3
                );
            } else {
                assert!(text.contains("id = 8"));
                assert!(!calls.iter().any(|s| s.starts_with("GET /nodes/")));
            }
        }
    }
}
#[test]
fn gen_data_errors_keep_old_files_and_partial_all_without_secret_diagnostics() {
    for mode in [
        "auth-error",
        "foreign-auth",
        "userinfo",
        "required",
        "major",
        "layout",
        "blocking",
        "bad-json",
        "wrong-shape",
        "http-error",
        "denied",
        "redirect",
        "nix-error",
        "missing-association",
        "bad-container",
        "missing-parent",
        "partial-all",
        "output-fail",
    ] {
        let f = Fixture::new();
        fs::write(f.networks(), b"old-network").unwrap();
        fs::write(f.containers(), b"old-container").unwrap();
        let route = if mode == "auth-error" {
            "all"
        } else if matches!(mode, "nix-error" | "missing-association" | "bad-container") {
            "containers"
        } else if matches!(mode, "missing-parent" | "partial-all") {
            "all"
        } else {
            "network"
        };
        if mode == "missing-parent" {
            fs::remove_file(f.networks()).unwrap();
            fs::remove_dir(f.root.join("data/vpsadmin/networks")).unwrap();
        }
        if mode == "bad-container" {
            fs::write(
                f.root.join("machines.json"),
                json!({"m":{"name":"bad","metaConfig":{"container":false}}}).to_string(),
            )
            .unwrap();
        }
        let server = Server::new(mode, &f.root);
        let out = f.run(&server, mode, route, &credentials());
        assert!(!out.status.success(), "{mode}");
        f.secret_free(&out);
        let calls = server.events();
        if matches!(mode, "missing-parent" | "partial-all") {
            assert!(
                fs::read_to_string(f.containers())
                    .unwrap()
                    .contains("id = 7")
            );
            if mode == "missing-parent" {
                assert!(!f.networks().exists());
            } else {
                assert_eq!(fs::read(f.networks()).unwrap(), b"old-network");
            }
        } else {
            assert_eq!(fs::read(f.networks()).unwrap(), b"old-network");
            assert_eq!(fs::read(f.containers()).unwrap(), b"old-container");
        }
        if matches!(
            mode,
            "foreign-auth" | "userinfo" | "required" | "major" | "output-fail"
        ) {
            assert!(!calls.iter().any(|s| s.starts_with("POST")));
        }
        assert!(calls.iter().filter(|s| s.ends_with("/nets")).count() <= 1);
        if mode == "auth-error" {
            assert!(!f.root.join("nix-calls").exists());
        }
        if mode == "nix-error" {
            let text = String::from_utf8_lossy(&out.stderr);
            assert!(text.contains("native fixture failure") && text.contains('7'));
        }
    }
}
#[test]
fn gen_data_body_and_native_nix_cancellation_join_and_prevent_publication() {
    for mode in ["body-cancel", "auth-body-cancel", "nix-cancel"] {
        let f = Fixture::new();
        fs::write(f.networks(), b"old").unwrap();
        fs::write(f.containers(), b"old").unwrap();
        let server = Server::new(mode, &f.root);
        let mut child = f
            .command(
                &server,
                mode,
                if mode != "nix-cancel" {
                    "network"
                } else {
                    "containers"
                },
            )
            .spawn()
            .unwrap();
        child
            .stdin
            .take()
            .unwrap()
            .write_all(credentials().as_bytes())
            .unwrap();
        let marker = f.root.join(if mode != "nix-cancel" {
            "body-blocked"
        } else {
            "nix-blocked"
        });
        let deadline = Instant::now();
        while !marker.exists() {
            assert!(deadline.elapsed() < Duration::from_secs(8));
            thread::sleep(Duration::from_millis(5));
        }
        let pid = if mode == "nix-cancel" {
            Some(
                fs::read_to_string(&marker)
                    .unwrap()
                    .trim()
                    .parse::<i32>()
                    .unwrap(),
            )
        } else {
            None
        };
        nix::sys::signal::kill(
            nix::unistd::Pid::from_raw(child.id() as i32),
            nix::sys::signal::Signal::SIGINT,
        )
        .unwrap();
        let out = child.wait_with_output().unwrap();
        assert_eq!(out.status.code(), Some(130));
        f.secret_free(&out);
        assert_eq!(fs::read(f.networks()).unwrap(), b"old");
        assert_eq!(fs::read(f.containers()).unwrap(), b"old");
        if let Some(pid) = pid {
            assert!(!Path::new(&format!("/proc/{pid}")).exists());
        }
        server.events();
    }
}
#[test]
fn gen_data_protected_password_challenge_cancel_eof_restore_existing_pty_owner() {
    use crate::terminal::pty_tests::open_pty;
    for mode in ["pty-success", "pty-cancel", "pty-eof"] {
        let f = Fixture::new();
        let server = Server::new(mode, &f.root);
        let (master, slave) = open_pty();
        let mut child = f
            .command(&server, mode, "network")
            .env("CONFCTL_TTY", "1")
            .env("TERM", "xterm-256color")
            .stdin(Stdio::from(slave.try_clone().unwrap()))
            .stdout(Stdio::from(slave.try_clone().unwrap()))
            .stderr(Stdio::from(slave))
            .spawn()
            .unwrap();
        let flags = rustix::fs::fcntl_getfl(&master).unwrap();
        rustix::fs::fcntl_setfl(&master, flags | rustix::fs::OFlags::NONBLOCK).unwrap();
        let mut bytes = Vec::new();
        let mut stage = 0;
        let deadline = Instant::now();
        let status = loop {
            let mut buf = [0; 8192];
            match rustix::io::read(&master, &mut buf) {
                Ok(n) => bytes.extend_from_slice(&buf[..n]),
                Err(rustix::io::Errno::AGAIN | rustix::io::Errno::IO) => {}
                Err(e) => panic!("PTY read: {e}"),
            }
            let text = String::from_utf8_lossy(&bytes);
            let send = if stage == 0 && text.contains("User name: ") {
                stage = 1;
                Some(format!("{USER}\n").into_bytes())
            } else if stage == 1 && text.contains("Password: ") {
                stage = 2;
                if mode == "pty-cancel" {
                    nix::sys::signal::kill(
                        nix::unistd::Pid::from_raw(child.id() as i32),
                        nix::sys::signal::Signal::SIGINT,
                    )
                    .unwrap();
                    None
                } else if mode == "pty-eof" {
                    Some(vec![4])
                } else {
                    Some(format!("{PASSWORD}\n").into_bytes())
                }
            } else if stage == 2 && mode == "pty-success" && text.contains("Verification code: ") {
                stage = 3;
                Some(format!("{CODE}\n").into_bytes())
            } else {
                None
            };
            if let Some(send) = send {
                rustix::io::write(&master, &send).unwrap();
            }
            if let Some(status) = child.try_wait().unwrap() {
                break status;
            }
            if deadline.elapsed() > Duration::from_secs(10) {
                child.kill().unwrap();
                let _ = child.wait();
                panic!("PTY fixture watchdog (transcript withheld)");
            }
            thread::sleep(Duration::from_millis(5));
        };
        assert_eq!(status.success(), mode == "pty-success");
        if mode == "pty-cancel" {
            assert_eq!(status.code(), Some(130));
        }
        for secret in [PASSWORD, CODE, STEP, TOKEN] {
            assert!(
                !bytes.windows(secret.len()).any(|b| b == secret.as_bytes()),
                "protected value echoed (withheld)"
            );
        }
        let calls = server.events();
        if mode != "pty-success" {
            assert!(!calls.iter().any(|s| s.starts_with("OPTIONS")));
            assert!(!f.networks().exists());
        }
    }
}

#[test]
fn gen_data_iso8601_compact_challenges_and_token_expiry_use_the_same_offset_rule() {
    for (mode, requests) in [
        ("types-compact", 1),
        ("expire-compact", 2),
        ("expire-offset", 2),
        ("expire-future-offset", 1),
        ("expire-future-fraction", 1),
        ("expire-malformed", 1),
    ] {
        let fixture = Fixture::new();
        let server = Server::new(mode, &fixture.root);
        let input = if mode == "types-compact" {
            format!(
                "{USER}\n{PASSWORD}\n public \n+007\n1.25\nYES\n2026-10-20T00:00:00.123456789-0430\n009\n \narbitrary\n"
            )
        } else {
            credentials()
        };
        let output = fixture.run(&server, mode, "network", &input);
        fixture.secret_free(&output);
        let events = server.events();
        assert_eq!(
            events
                .iter()
                .filter(|event| event.ends_with("/auth/start"))
                .count(),
            requests
        );
        if mode == "expire-malformed" {
            assert!(!output.status.success());
            assert!(!fixture.networks().exists());
            assert!(!events.iter().any(|event| event.ends_with("/nets")));
            assert!(String::from_utf8_lossy(&output.stderr).contains("invalid token expiry"));
        } else {
            succeeded(&output);
            assert!(fixture.networks().exists());
        }
    }
}
