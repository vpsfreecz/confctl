use super::*;
use crate::configuration::{ConfigRelativePath, ConfigRoot, MutationLock};
use serde_json::json;
use std::{
    collections::VecDeque,
    os::unix::process::ExitStatusExt,
    process::ExitStatus,
    sync::{
        Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Instant,
};

type ProbeReply = (i32, Vec<u8>, Vec<u8>);

struct Probe {
    replies: Mutex<VecDeque<ProbeReply>>,
    calls: Mutex<Vec<ProcessSpec>>,
}
impl Probe {
    fn new(replies: Vec<ProbeReply>) -> Self {
        Self {
            replies: Mutex::new(replies.into()),
            calls: Mutex::new(Vec::new()),
        }
    }
}
fn outcome(code: i32, out: Vec<u8>, err: Vec<u8>) -> ProcessOutcome {
    ProcessOutcome {
        status: ExitStatus::from_raw(code << 8),
        stdout: Captured::Memory(out),
        stderr: Captured::Memory(err),
        stdout_complete: true,
        stderr_complete: true,
        started: Instant::now(),
        ended: Instant::now(),
        termination: TerminationCause::Exited,
    }
}
impl ProcessRunner for Probe {
    async fn run(
        &self,
        spec: ProcessSpec,
        _: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        self.calls.lock().unwrap().push(spec);
        let (code, out, err) = self
            .replies
            .lock()
            .unwrap()
            .pop_front()
            .expect("only expected probes");
        Ok(outcome(code, out, err))
    }
}
fn machine(name: &str, carrier: Option<&str>) -> Machine {
    Machine::from_value(json!({"name":name,"carrier":carrier,"metaConfig":{"spin":"vpsadminos","managed":false,"host":{"target":format!("{name}.invalid"),"port":22}}})).unwrap()
}
fn events() -> (EventSink, tokio::task::JoinHandle<String>) {
    let (events, mut receiver) = EventSink::channel(1);
    let task = tokio::spawn(async move {
        let mut output = String::new();
        while let Some(delivery) = receiver.receive().await {
            let (event, ack) = delivery.into_parts();
            let UiEvent::Diagnostic {
                stream: OutputStream::Stdout,
                text,
                ..
            } = event
            else {
                panic!("feature output remains on original stdout")
            };
            output.push_str(&text);
            ack.acknowledge(Ok(()));
        }
        output
    });
    (events, task)
}
struct Fixture {
    dir: tempfile::TempDir,
    root: ConfigRoot,
    files: StateFiles,
    settings: RuntimeKernelSettings,
    inventory: Inventory,
}
impl Fixture {
    fn new(names: &[&str], initial: Option<&str>) -> Self {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("configs/node")).unwrap();
        if let Some(initial) = initial {
            std::fs::write(dir.path().join("configs/node/kernels.json"), initial).unwrap();
        }
        let root = ConfigRoot::new(dir.path()).unwrap();
        Self {
            files: StateFiles::new(root.clone()),
            root,
            dir,
            settings: RuntimeKernelSettings {
                enable: true,
                state_file: Some(ConfigRelativePath::new("configs/node/kernels.json").unwrap()),
                refresh_before_deploy: false,
            },
            inventory: Inventory {
                machines: names
                    .iter()
                    .map(|name| {
                        machine(
                            name,
                            if name.contains('#') {
                                Some("carrier")
                            } else {
                                None
                            },
                        )
                    })
                    .collect(),
            },
        }
    }
    fn state(&self) -> Option<String> {
        std::fs::read_to_string(self.dir.path().join("configs/node/kernels.json")).ok()
    }
    fn context<'a, P>(
        &'a self,
        runner: &'a P,
        events: &'a EventSink,
        cancel: &'a CancellationToken,
        guard: &'a MutationGuard,
    ) -> FeatureContext<'a, P> {
        FeatureContext {
            runner,
            events,
            files: &self.files,
            inventory: &self.inventory,
            settings: &self.settings,
            cancellation: cancel,
            mutation: guard,
            ssh_config: None,
            task: TaskId(1),
        }
    }
}

#[tokio::test]
async fn normalization_and_insertion_order_match_preserved_ruby_site_capture() {
    // Exact bytes: accepted M0 site-normalization observation, unchanged oracle.
    let fixture = Fixture::new(
        &["nodes/a", "nodes/b", "carrier#b", "nodes/off"],
        Some("{\"carrier#b\":\"keep\"}"),
    );
    let runner = Probe::new(vec![
        (
            0,
            b" 6.9..extra.more\n".to_vec(),
            b"ignored stderr".to_vec(),
        ),
        (0, Vec::new(), Vec::new()),
        (0, b"error\n".to_vec(), Vec::new()),
        (0, b"1.2...\n".to_vec(), Vec::new()),
    ]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output) = events();
    let report = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[MachineId(0), MachineId(1), MachineId(2), MachineId(3)],
    )
    .await
    .unwrap();
    assert_eq!(
        fixture.state().unwrap(),
        "{\n  \"carrier#b\": \"keep\",\n  \"nodes/a\": \"6.9.\",\n  \"nodes/b\": \"\",\n  \"nodes/off\": \"1.2\"\n}"
    );
    assert_eq!(report.updated, ["nodes/a", "nodes/b", "nodes/off"]);
    assert_eq!(report.unchanged, ["carrier#b"]);
    assert!(report.deleted.is_empty());
    assert_eq!(
        runner.calls.lock().unwrap().len(),
        4,
        "unmanaged/carried are not silently filtered"
    );
    drop(events);
    let output = output.await.unwrap();
    assert!(output.starts_with(&format!("{:<50} KERNEL\n", "HOST")));
    assert!(!output.contains("ignored stderr"));
}

#[tokio::test]
async fn nonfatal_hosts_delete_only_selected_keys_and_keep_unknown_values_ordered() {
    let fixture = Fixture::new(
        &["nodes/a", "nodes/b"],
        Some("{\"nodes/a\":\"old\",\"nodes/b\":\"old\",\"unselected\":{\"z\":false,\"a\":[1,2]}}"),
    );
    let runner = Probe::new(vec![
        (255, Vec::new(), b"fixture SSH failure".to_vec()),
        (0, b"6.12.3-extra.more\n".to_vec(), Vec::new()),
    ]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output) = events();
    let report = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[MachineId(0), MachineId(1)],
    )
    .await
    .unwrap();
    assert_eq!(report.deleted, ["nodes/a"]);
    assert_eq!(report.rows[0].kernel, "error");
    assert!(
        report.rows[0]
            .error
            .as_ref()
            .unwrap()
            .contains("exit status: 255")
    );
    assert_eq!(
        fixture.state().unwrap(),
        "{\n  \"nodes/b\": \"6.12.3-extra\",\n  \"unselected\": {\n    \"z\": false,\n    \"a\": [\n      1,\n      2\n    ]\n  }\n}"
    );
    drop(events);
    assert!(output.await.unwrap().contains("Error on nodes/a:"));
}

#[tokio::test]
async fn zero_selected_preserves_scalar_and_literal_error_keeps_old_state() {
    let fixture = Fixture::new(&[], Some("17"));
    let runner = Probe::new(vec![]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output) = events();
    let report = refresh(&fixture.context(&runner, &events, &cancel, &guard), &[])
        .await
        .unwrap();
    assert!(report.rows.is_empty());
    assert_eq!(fixture.state().as_deref(), Some("17"));
    assert!(runner.calls.lock().unwrap().is_empty());
    drop(events);
    assert_eq!(output.await.unwrap(), format!("{:<50} KERNEL\n", "HOST"));
    assert_eq!(normalize_kernel("\0 \t1.2...\r\n"), "1.2");
    assert_eq!(normalize_kernel("\u{a0}1.2.3"), "\u{a0}1.2.3");
    let mut scalar = json!(17);
    assert!(assign(&mut scalar, "host", "1.2.3").is_err());
    assert_eq!(scalar, json!(17));
    let mut array = json!(["host", 7, "host", false]);
    assert!(delete(&mut array, "host").unwrap());
    assert_eq!(array, json!([7, false]));
    let mut string = json!("prefix host suffix");
    assert!(assign(&mut string, "host", "6.12").unwrap());
    assert_eq!(string, json!("prefix 6.12 suffix"));
    assert!(!delete(&mut string, "x").unwrap());
}

#[tokio::test]
async fn malformed_state_disabled_and_wrong_guard_fail_before_any_probe_or_output() {
    let mut fixture = Fixture::new(&["nodes/a"], Some("{bad"));
    let runner = Probe::new(vec![]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output) = events();
    assert!(
        refresh(
            &fixture.context(&runner, &events, &cancel, &guard),
            &[MachineId(0)]
        )
        .await
        .is_err()
    );
    assert_eq!(fixture.state().as_deref(), Some("{bad"));
    fixture.settings.enable = false;
    assert!(
        refresh(&fixture.context(&runner, &events, &cancel, &guard), &[])
            .await
            .is_err()
    );
    fixture.settings.enable = true;
    let other = Fixture::new(&[], None);
    let other_guard = MutationLock::acquire(&other.root, &cancel).await.unwrap();
    assert!(
        refresh(
            &fixture.context(&runner, &events, &cancel, &other_guard),
            &[]
        )
        .await
        .is_err()
    );
    assert!(runner.calls.lock().unwrap().is_empty());
    drop(events);
    assert_eq!(output.await.unwrap(), "");
}

struct FailingAndBlocked {
    started: AtomicUsize,
    joined: AtomicUsize,
}
impl ProcessRunner for FailingAndBlocked {
    async fn run(
        &self,
        _: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        let index = self.started.fetch_add(1, Ordering::SeqCst);
        let result = if index == 0 {
            Err(ConfctlError::Io {
                operation: "fixture spawn fault",
                path: None,
                source: io::Error::other("fixture"),
            })
        } else {
            scope.cancellation.cancelled().await;
            Err(ConfctlError::Cancelled)
        };
        self.joined.fetch_add(1, Ordering::SeqCst);
        result
    }
}
#[tokio::test]
async fn infrastructure_failure_cancels_and_joins_every_probe_without_publication() {
    let fixture = Fixture::new(&["a", "b"], Some("{\"old\":true}"));
    let runner = FailingAndBlocked {
        started: AtomicUsize::new(0),
        joined: AtomicUsize::new(0),
    };
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output) = events();
    let error = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[MachineId(0), MachineId(1)],
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("fixture spawn fault"));
    assert_eq!(runner.started.load(Ordering::SeqCst), 2);
    assert_eq!(runner.joined.load(Ordering::SeqCst), 2);
    assert_eq!(fixture.state().as_deref(), Some("{\"old\":true}"));
    drop(events);
    assert_eq!(output.await.unwrap(), "");
}

#[tokio::test]
async fn failed_output_ack_never_publishes_a_completed_merge() {
    let fixture = Fixture::new(&["a"], Some("{\"a\":\"old\"}"));
    let runner = Probe::new(vec![(0, b"6.12.3\n".to_vec(), Vec::new())]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, mut receiver) = EventSink::channel(1);
    let consumer = tokio::spawn(async move {
        receiver
            .receive()
            .await
            .unwrap()
            .acknowledge(Err("output fault".into()));
    });
    assert!(
        refresh(
            &fixture.context(&runner, &events, &cancel, &guard),
            &[MachineId(0)]
        )
        .await
        .is_err()
    );
    consumer.await.unwrap();
    assert_eq!(fixture.state().as_deref(), Some("{\"a\":\"old\"}"));
}

struct ReverseProbe {
    second: tokio::sync::Notify,
    completed: Mutex<Vec<String>>,
}
impl ProcessRunner for ReverseProbe {
    async fn run(
        &self,
        spec: ProcessSpec,
        _: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        let name = spec.args[2].to_str().unwrap();
        if name == "a.invalid" {
            self.second.notified().await;
        } else {
            self.second.notify_one();
        }
        self.completed.lock().unwrap().push(name.into());
        Ok(outcome(0, b"6.12.3\n".to_vec(), Vec::new()))
    }
}
#[tokio::test]
async fn concurrent_reverse_completion_still_renders_and_saves_selection_order() {
    let fixture = Fixture::new(&["a", "b"], None);
    let runner = ReverseProbe {
        second: tokio::sync::Notify::new(),
        completed: Mutex::new(Vec::new()),
    };
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output) = events();
    let report = refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[MachineId(0), MachineId(1)],
    )
    .await
    .unwrap();
    assert_eq!(
        *runner.completed.lock().unwrap(),
        ["b.invalid", "a.invalid"]
    );
    assert_eq!(
        report
            .rows
            .iter()
            .map(|row| row.machine)
            .collect::<Vec<_>>(),
        [MachineId(0), MachineId(1)]
    );
    assert_eq!(
        fixture.state().unwrap(),
        "{\n  \"a\": \"6.12.3\",\n  \"b\": \"6.12.3\"\n}"
    );
    drop(events);
    assert_eq!(
        output
            .await
            .unwrap()
            .lines()
            .skip(1)
            .map(|line| line.split_whitespace().next().unwrap().to_string())
            .collect::<Vec<_>>(),
        ["a", "b"]
    );
}

#[test]
fn uname_route_is_local_or_exact_root_ssh_without_hidden_filters_or_timeout() {
    let mut machine = machine("carrier#child", Some("carrier"));
    machine.metadata["host"]["port"] = json!(2222);
    let spec = probe_spec(&machine, Some(OsStr::new("/fixture/ssh.conf"))).unwrap();
    assert_eq!(spec.program, std::path::Path::new("ssh"));
    assert_eq!(
        spec.args,
        [
            "-F",
            "/fixture/ssh.conf",
            "-l",
            "root",
            "-p",
            "2222",
            "carrier#child.invalid",
            "uname -r"
        ]
        .map(std::ffi::OsString::from)
    );
    machine.metadata["host"]["target"] = json!("localhost");
    let spec = probe_spec(&machine, None).unwrap();
    assert_eq!(spec.program, std::path::Path::new("uname"));
    assert_eq!(spec.args, [std::ffi::OsString::from("-r")]);
    machine.metadata = json!({"spin":"vpsadminos"});
    let spec = probe_spec(&machine, None).unwrap();
    assert_eq!(spec.args.last().unwrap(), "uname -r");
    assert_eq!(spec.args[2], "carrier#child");
}

#[test]
fn uname_port_conversion_matches_original_ruby_and_local_target_ignores_unused_port() {
    let fixture: Value =
        serde_json::from_str(include_str!("../remote/json/ruby-conversions.json")).unwrap();
    for case in fixture["port_cases"].as_array().unwrap() {
        let converted = ruby_port(&case["value"]);
        match case.get("converted") {
            Some(expected) => assert_eq!(
                converted.unwrap(),
                expected.as_str().unwrap(),
                "{}",
                case["value"]
            ),
            None => assert!(converted.is_err(), "{}", case["value"]),
        }
    }
    let mut machine = machine("carrier#child", Some("carrier"));
    for bad in [Value::Null, json!(false), json!([]), json!("host")] {
        machine.metadata["host"] = bad;
        assert!(probe_spec(&machine, None).is_err());
    }
    for bad in [Value::Null, json!(false), json!([]), json!({})] {
        machine.metadata["host"] = json!({"target":bad});
        assert!(probe_spec(&machine, None).is_err());
    }
    machine.metadata["host"] = json!({"target":"localhost","port":{}});
    assert_eq!(
        probe_spec(&machine, None).unwrap().args,
        [std::ffi::OsString::from("-r")]
    );
    machine.metadata["host"] = json!({"target":"remote","port":"2_2"});
    let spec = probe_spec(&machine, Some(OsStr::new(""))).unwrap();
    assert_eq!(
        spec.args,
        ["-l", "root", "remote", "uname -r"].map(std::ffi::OsString::from)
    );
}

#[tokio::test]
async fn refresh_keeps_unselected_huge_integers_float_bytes_nested_empties_and_order() {
    let fixture = Fixture::new(
        &["a"],
        Some(
            "{\"unselected\":{\"z\":184467440737095516160001,\"a\":-184467440737095516160001,\"empty\":[[],{}],\"f\":[1e15,-0.0,5e-324]},\"a\":\"old\"}",
        ),
    );
    let runner = Probe::new(vec![(0, b"6.12.3\n".to_vec(), Vec::new())]);
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
    let (events, output) = events();
    refresh(
        &fixture.context(&runner, &events, &cancel, &guard),
        &[MachineId(0)],
    )
    .await
    .unwrap();
    let original: Value =
        serde_json::from_str(include_str!("../remote/json/ruby-conversions.json")).unwrap();
    let expected = original["json_cases"].as_array().unwrap().last().unwrap()["pretty"]
        .as_str()
        .unwrap();
    let expected = format!(
        "{},\n  \"a\": \"6.12.3\"\n}}",
        expected.strip_suffix("\n}").unwrap()
    );
    assert_eq!(fixture.state().unwrap(), expected);
    drop(events);
    output.await.unwrap();
}

#[tokio::test]
async fn encoding_failure_after_joined_probe_or_empty_selection_never_publishes() {
    for selected in [false, true] {
        let initial = "{\"unselected\":1e400,\"a\":\"old\"}";
        let fixture = Fixture::new(&["a"], Some(initial));
        let runner = Probe::new(if selected {
            vec![(0, b"6.12.3\n".to_vec(), Vec::new())]
        } else {
            vec![]
        });
        let cancel = CancellationToken::new();
        let guard = MutationLock::acquire(&fixture.root, &cancel).await.unwrap();
        let (events, output) = events();
        let ids = if selected { &[MachineId(0)][..] } else { &[] };
        let error = refresh(&fixture.context(&runner, &events, &cancel, &guard), ids)
            .await
            .unwrap_err();
        assert!(error.to_string().contains("nonfinite"));
        assert_eq!(fixture.state().as_deref(), Some(initial));
        assert_eq!(runner.calls.lock().unwrap().len(), usize::from(selected));
        drop(events);
        output.await.unwrap();
    }
}
