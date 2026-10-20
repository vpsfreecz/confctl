use super::*;
use crate::native_tool;
use crate::{
    configuration::ConfigRoot,
    domain::{AutoRollback, Inventory, MachineId, SwitchAction},
    events::{EventReceiver, EventSink, UiEvent},
    process::Supervisor,
};
use serde_json::json;
use std::{
    ffi::OsStr,
    fs,
    os::unix::{
        ffi::{OsStrExt, OsStringExt},
        fs::PermissionsExt,
        process::ExitStatusExt,
    },
    sync::Mutex,
};
use tokio_util::sync::CancellationToken;

fn executable(path: &Path, script: &str) {
    fs::write(path, script).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}
struct Call {
    argv: Vec<OsString>,
    task: TaskId,
    phase: Option<Phase>,
}
struct Runner {
    supervisor: Supervisor,
    root: PathBuf,
    mode: &'static str,
    calls: Mutex<Vec<Call>>,
}
impl ProcessRunner for Runner {
    async fn run(
        &self,
        mut spec: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        self.calls.lock().unwrap().push(Call {
            argv: spec.args.clone(),
            task: scope.task,
            phase: scope.phase,
        });
        if (spec
            .redacted_description
            .starts_with("rollback marker read")
            && self.mode == "partial")
            || (spec
                .redacted_description
                .starts_with("rollback confirmation")
                && self.mode == "partial-confirm")
        {
            return Ok(ProcessOutcome {
                status: std::process::ExitStatus::from_raw(0),
                stdout: Captured::Memory(b"switched".into()),
                stderr: Captured::Memory(vec![]),
                stdout_complete: false,
                stderr_complete: true,
                started: Instant::now(),
                ended: Instant::now(),
                termination: TerminationCause::Exited,
            });
        }
        if spec
            .redacted_description
            .starts_with("rollback confirmation")
            && self.mode == "spawn-confirm"
        {
            return Err(ConfctlError::Spawn {
                description: "controlled confirmation".into(),
                source: std::io::Error::other("controlled spawn failure"),
            });
        }
        assert_eq!(spec.program, Path::new("ssh"));
        spec.program = self.root.join("ssh");
        let is_helper = spec
            .redacted_description
            .starts_with("rollback-helper protocol");
        spec.env.extend([
            ("RB_ROOT".into(), Some(self.root.clone().into_os_string())),
            ("RB_MODE".into(), Some(self.mode.into())),
            (
                "RB_HELPER_REQUEST".into(),
                Some(if is_helper { "1" } else { "0" }.into()),
            ),
            ("PATH".into(), Some(self.root.as_os_str().into())),
        ]);
        let mut outcome = self.supervisor.run(spec, scope).await?;
        if is_helper && self.mode == "partial-helper" {
            outcome.stderr_complete = false;
        }
        Ok(outcome)
    }
}
struct Fixture {
    _tmp: tempfile::TempDir,
    root: ConfigRoot,
    inventory: Inventory,
    job: Job,
    helper: PathBuf,
    path: PathBuf,
    runner: Runner,
    events: EventSink,
    cancel: CancellationToken,
}
impl Fixture {
    fn new(mode: &'static str) -> (Self, EventReceiver) {
        let tmp = tempfile::tempdir().unwrap();
        let dir = tmp.path();
        fs::create_dir(dir.join("native")).unwrap();
        for tool in ["cat", "sleep"] {
            std::os::unix::fs::symlink(native_tool::resolve(tool), dir.join("native").join(tool))
                .unwrap();
        }
        std::os::unix::fs::symlink(dir.join("native/cat"), dir.join("cat")).unwrap();

        let helper = dir.join("helper 'native");
        let path = dir.join(OsString::from_vec(b"confirm 'native\\\xff".to_vec()));
        fs::write(
            &path,
            if mode == "switching" {
                b"switching".as_slice()
            } else if mode == "unknown" {
                b"unknown".as_slice()
            } else {
                b"switched".as_slice()
            },
        )
        .unwrap();
        executable(
            &dir.join("ssh"),
            r#"#!/bin/sh
printf '%s\0' "$@" > "$RB_ROOT/ssh-$$"
if [ "$RB_HELPER_REQUEST" = 1 ]; then printf '%s' "$$" > "$RB_ROOT/helper-ssh-pid"; fi
while [ "$#" -gt 0 ]; do
 case "$1" in -F|-l|-p) shift; shift ;; *) break ;; esac
done
[ "$1" = remote.invalid ] || exit 91
shift
[ "$#" = 1 ] || exit 92
exec /bin/sh -c "exec $1"
"#,
        );
        executable(
            &dir.join("sh"),
            r#"#!/bin/sh
printf '%s\0' "$@" > "$RB_ROOT/control-$$"
case "$2" in
 'cat '*)
  if [ "$RB_MODE" = blocked ]; then printf '%s' "$$" > "$RB_ROOT/control-pid"; exec "$RB_ROOT/native/sleep" 60; fi
  if [ "$RB_MODE" = early ] || [ "$RB_MODE" = signal ] || [ "$RB_MODE" = exit143 ]; then "$RB_ROOT/native/sleep" 0.05; fi
  if [ "$RB_MODE" != early ] && [ "$RB_MODE" != signal ] && [ "$RB_MODE" != exit143 ]; then
   while [ ! -e "$RB_ROOT/ready" ]; do "$RB_ROOT/native/sleep" 0.01; done
  fi
  if [ "$RB_MODE" = switching ] && [ ! -e "$RB_ROOT/first-read" ]; then : > "$RB_ROOT/first-read"; printf 'switching'; exit 0; fi
  if [ "$RB_MODE" = read-retry ] && [ ! -e "$RB_ROOT/read-failed" ]; then : > "$RB_ROOT/read-failed"; exit 9; fi ;;
 'echo confirmed > '*)
  n=0
  if [ -r "$RB_ROOT/attempts" ]; then read -r n < "$RB_ROOT/attempts"; fi
  n=$((n+1)); printf '%s\n' "$n" > "$RB_ROOT/attempts"
  if [ "$RB_MODE" = retry ] && [ "$n" -lt 3 ]; then exit 7; fi
  if [ "$RB_MODE" = confirm-signal ] && [ "$n" -lt 3 ]; then kill -TERM "$$"; exit 94; fi
  if [ "$RB_MODE" = exhaust ]; then [ "$n" -lt 10 ] || : > "$RB_ROOT/exhausted"; exit 8; fi ;;
esac
exec /bin/sh "$@"
"#,
        );
        executable(
            &helper,
            r#"#!/bin/sh
printf '%s\0' "$@" > "$RB_ROOT/helper-args"
printf '%s' "$$" > "$RB_ROOT/helper-pid"
if [ "$RB_MODE" = early ]; then exit 0; fi
if [ "$RB_MODE" = signal ]; then trap - TERM; kill -TERM "$$"; exit 93; fi
if [ "$RB_MODE" = exit143 ]; then exit 143; fi
path=$5
if [ "$RB_MODE" = blocked ]; then
 while [ ! -s "$RB_ROOT/control-pid" ]; do "$RB_ROOT/native/sleep" 0.01; done
 printf 'both entered\n'; exec "$RB_ROOT/native/sleep" 60
fi
if [ "$RB_MODE" = switching ]; then
 printf 'switching' > "$path"; : > "$RB_ROOT/ready"
 while [ ! -e "$RB_ROOT/first-read" ]; do "$RB_ROOT/native/sleep" 0.01; done
fi
if [ "$RB_MODE" = unknown ]; then printf 'unknown' > "$path"; else printf '\000 \tswitched\r\n\000' > "$path"; fi
: > "$RB_ROOT/ready"
if [ "$RB_MODE" = exhaust ]; then
 while [ ! -e "$RB_ROOT/exhausted" ]; do "$RB_ROOT/native/sleep" 0.01; done
 "$RB_ROOT/native/sleep" 0.05
 printf 'natural helper completion\n'; exit 0
fi
while :; do
 state=; read -r state < "$path"
 [ "$state" != confirmed ] || break
 "$RB_ROOT/native/sleep" 0.01
done
printf 'helper stdout\377\n'; printf 'helper stderr\033\n' >&2
if [ "$RB_MODE" = hidden ]; then printf 'simulated inner exit 7 ignored\n'; fi
if [ "$RB_MODE" = helper-fail ]; then exit 6; fi
exit 0
"#,
        );
        let (events, receiver) = EventSink::channel(2);
        let supervisor = Supervisor::new(
            events.clone(),
            dir.into(),
            64,
            Duration::from_millis(50),
            None,
            None,
        );
        let root = ConfigRoot::new(dir).unwrap();
        let inventory = Inventory::from_value(json!({"remote":{"name":"remote","metaConfig":{"host":{"target":"remote.invalid","port":"02_222suffix"},"autoRollback":{"enable":true,"timeout":-11}}}})).unwrap();
        let job = Job {
            machine: MachineId(0),
            generation: crate::generation::BuildGeneration {
                name: "saved".into(),
                date: chrono::DateTime::parse_from_rfc3339("2020-01-01T00:00:00Z").unwrap(),
                toplevel: "/store/system".into(),
                auto_rollback: Some(helper.clone()),
                inputs: Default::default(),
                inputs_info: Default::default(),
            },
            task: TaskId(1),
        };
        (
            Self {
                root,
                inventory,
                job,
                helper,
                path,
                runner: Runner {
                    supervisor,
                    root: dir.into(),
                    mode,
                    calls: Mutex::new(Vec::new()),
                },
                events,
                cancel: CancellationToken::new(),
                _tmp: tmp,
            },
            receiver,
        )
    }
    async fn run(&self) -> Result<(), ConfctlError> {
        let copies = super::super::Copies {
            runner: &self.runner,
            events: &self.events,
            cancellation: &self.cancel,
            inventory: &self.inventory,
            root: &self.root,
            rollback: AutoRollback::Configured,
            ssh_opts: None,
        };
        let ui = crate::terminal::Ui::new(
            fs::File::open("/dev/null").unwrap(),
            self.events.clone(),
            self.cancel.clone(),
            None,
        )
        .unwrap();
        let activation = Activation {
            copies: &copies,
            ui: &ui,
            ssh_config: Some(OsStr::from_bytes(b"config '\xff")),
            interactive: false,
            dry_first: false,
            action: if self.runner.mode == "hidden" {
                SwitchAction::Switch
            } else {
                SwitchAction::Test
            },
            log: Path::new("log"),
        };
        let call = run_at(&activation, &self.job, TaskId(17), &self.helper, &self.path);
        tokio::pin!(call);
        let result = tokio::select! {
            result = &mut call => result,
            _ = tokio::time::sleep(Duration::from_secs(10)) => {
                self.cancel.cancel();
                let result = call.await;
                panic!("confined rollback caller did not finish: {result:?}");
            }
        };
        ui.shutdown().unwrap();
        result
    }
    fn confirmations(&self) -> usize {
        fs::read_to_string(self.root.path().join("attempts"))
            .unwrap_or_default()
            .trim()
            .parse()
            .unwrap_or(0)
    }
    fn assert_joined(&self) {
        for name in ["helper-ssh-pid", "helper-pid", "control-pid"] {
            if let Ok(text) = fs::read_to_string(self.root.path().join(name)) {
                let pid: u32 = text.parse().unwrap();
                assert!(
                    pid > 0 && !Path::new("/proc").join(pid.to_string()).exists(),
                    "{name}: {pid}"
                );
            }
        }
    }
}
fn consume(mut receiver: EventReceiver) -> tokio::task::JoinHandle<Vec<UiEvent>> {
    tokio::spawn(async move {
        let mut events = Vec::new();
        while let Some(delivery) = receiver.receive().await {
            let (event, ack) = delivery.into_parts();
            events.push(event);
            ack.acknowledge(Ok(()));
        }
        events
    })
}

#[test]
fn signed_timeout_marker_edges_and_random_path_are_exact() {
    for (text, fallback) in [
        ("-11", true),
        ("-10", true),
        ("-9", false),
        ("0", false),
        ("9223372036854775807", false),
        ("999999999999999999999999999999", false),
        ("-999999999999999999999999999999", true),
    ] {
        let value: serde_json::Value = serde_json::from_str(text).unwrap();
        let inv = Inventory::from_value(
            json!({"a":{"name":"a","metaConfig":{"autoRollback":{"timeout":value}}}}),
        )
        .unwrap();
        let timeout = Timeout::from_machine(&inv.machines[0]).unwrap();
        assert_eq!(timeout.native, text);
        assert_eq!(
            timeout.fallback(Duration::from_millis(1)),
            fallback,
            "{text}"
        );
        assert_eq!(
            timeout.fallback(Duration::ZERO),
            text.starts_with('-') && text != "-10" && text != "-9",
            "{text}"
        );
    }
    for value in [
        json!(null),
        json!("60"),
        json!(1.5),
        json!(true),
        json!(1e20),
    ] {
        let inv = Inventory::from_value(
            json!({"a":{"name":"a","metaConfig":{"autoRollback":{"timeout":value}}}}),
        )
        .unwrap();
        assert!(Timeout::from_machine(&inv.machines[0]).is_err());
    }
    assert_eq!(marker(b"\0 \t\r\nswitched\0\x0b\x0c "), b"switched");
    assert_ne!(marker(b"switch\0ed"), b"switched");
    let path = check_path().unwrap();
    let suffix = path
        .to_str()
        .unwrap()
        .strip_prefix("/run/confctl-confirm-")
        .unwrap();
    assert_eq!(suffix.len(), 6);
    assert!(suffix.bytes().all(|c| c.is_ascii_hexdigit()));
}

#[tokio::test]
async fn supervisor_protocol_observes_markers_fallback_retry_and_only_helper_success() {
    for mode in [
        "switching",
        "unknown",
        "read-retry",
        "retry",
        "confirm-signal",
        "hidden",
    ] {
        let (f, rx) = Fixture::new(mode);
        let consumer = consume(rx);
        let start = Instant::now();
        let result = f.run().await;
        assert!(result.is_ok(), "{mode}: {result:?}");
        if mode == "switching" {
            assert!(start.elapsed() >= Duration::from_secs(1));
        }
        assert_eq!(
            f.confirmations(),
            if mode == "retry" || mode == "confirm-signal" {
                3
            } else {
                1
            }
        );
        let args = fs::read(f.root.path().join("helper-args")).unwrap();
        let expected = [
            b"-t".as_slice(),
            b"-11",
            b"/store/system",
            if mode == "hidden" {
                b"switch".as_slice()
            } else {
                b"test".as_slice()
            },
            f.path.as_os_str().as_bytes(),
        ];
        assert_eq!(
            args.strip_suffix(&[0])
                .unwrap()
                .split(|b| *b == 0)
                .collect::<Vec<_>>(),
            expected
        );
        let expected_ssh = [
            "-F".into(),
            OsString::from_vec(b"config '\xff".to_vec()),
            "-l".into(),
            "root".into(),
            "-p".into(),
            "2222".into(),
            "remote.invalid".into(),
        ];
        for call in f.runner.calls.lock().unwrap().iter() {
            assert_eq!(&call.argv[..7], expected_ssh);
            assert_eq!(call.task, TaskId(17));
            assert_eq!(call.phase, Some(Phase::Deployment));
        }
        f.assert_joined();
        f.runner.supervisor.shutdown().await.unwrap();
        drop(f);
        let events = consumer.await.unwrap();
        let text = events
            .iter()
            .filter_map(|e| {
                if let UiEvent::Diagnostic { text, .. } = e {
                    Some(text.as_str())
                } else {
                    None
                }
            })
            .collect::<String>();
        assert!(text.contains("inner switch and rollback results are not witnessed"));
        if mode == "unknown" {
            assert!(text.contains("switched marker was not observed"));
        }
        if mode == "retry" || mode == "read-retry" {
            assert!(text.contains("failed (exit"), "{text}");
        }
        let mut stdout = Vec::new();
        let mut stderr = Vec::new();
        for event in &events {
            if let UiEvent::Output { stream, bytes, .. } = event {
                match stream {
                    crate::events::OutputStream::Stdout => stdout.extend_from_slice(bytes),
                    crate::events::OutputStream::Stderr => stderr.extend_from_slice(bytes),
                }
            }
        }
        assert!(
            stdout
                .windows(b"helper stdout\xff\n".len())
                .any(|bytes| bytes == b"helper stdout\xff\n")
        );
        assert_eq!(stderr, b"helper stderr\x1b\n");
    }
}

#[tokio::test]
async fn exhaustion_and_early_zero_or_signal_cannot_report_protocol_success() {
    for mode in [
        "exhaust",
        "early",
        "signal",
        "exit143",
        "partial",
        "partial-confirm",
        "partial-helper",
        "helper-fail",
        "spawn-confirm",
    ] {
        let (f, rx) = Fixture::new(mode);
        let consumer = consume(rx);
        let error = f.run().await.unwrap_err();
        let text = error.to_string();
        match mode {
            "exhaust" => {
                assert!(text.contains("all ten"), "{text}");
                assert_eq!(f.confirmations(), 10);
            }
            "early" => {
                assert!(text.contains("without successful confirmation"), "{text}");
                assert_eq!(f.confirmations(), 0);
            }
            "signal" | "exit143" => {
                let expected = if mode == "signal" {
                    (None, Some(15))
                } else {
                    (Some(143), None)
                };
                let mut primary = &error;
                while let ConfctlError::Cleanup { primary: inner, .. } = primary {
                    primary = inner;
                }
                let ConfctlError::ProcessFailure { source, evidence } = primary else {
                    panic!("{mode}: missing native failure evidence: {error:?}");
                };
                let ConfctlError::Child {
                    description,
                    code,
                    signal,
                    ..
                } = source.as_ref()
                else {
                    panic!("{mode}: missing typed child failure: {error:?}");
                };
                assert_eq!(description, "rollback helper");
                assert_eq!((*code, *signal), expected, "{mode}");
                let status = evidence.status.expect("retained helper status");
                assert_eq!((status.code(), status.signal()), expected, "{mode}");
                assert!(evidence.stdout_complete && evidence.stderr_complete);
                let ssh_pid = fs::read_to_string(f.root.path().join("helper-ssh-pid"))
                    .unwrap()
                    .parse::<u32>()
                    .unwrap();
                let helper_pid = fs::read_to_string(f.root.path().join("helper-pid"))
                    .unwrap()
                    .parse::<u32>()
                    .unwrap();
                assert!(ssh_pid > 0);
                assert_eq!(
                    ssh_pid, helper_pid,
                    "{mode}: helper must exec as SSH leader"
                );
                assert_eq!(f.confirmations(), 0);
            }
            "partial" => {
                assert!(text.contains("incomplete rollback marker"), "{text}");
                assert_eq!(f.confirmations(), 0);
            }
            "partial-confirm" => {
                assert!(text.contains("incomplete rollback confirmation"), "{text}");
                assert_eq!(f.confirmations(), 0);
            }
            "partial-helper" => {
                assert!(text.contains("incomplete rollback helper"), "{text}");
                assert_eq!(f.confirmations(), 1);
            }
            "helper-fail" => {
                assert!(text.contains("exit Some(6)"), "{text}");
                assert_eq!(f.confirmations(), 1);
            }
            "spawn-confirm" => assert!(text.contains("controlled spawn failure"), "{text}"),
            _ => unreachable!(),
        }
        f.assert_joined();
        f.runner.supervisor.shutdown().await.unwrap();
        drop(f);
        let events = consumer.await.unwrap();
        assert!(
            !events.iter().any(|event| matches!(event, UiEvent::Diagnostic { text, .. } if text.contains("protocol confirmed"))),
            "{mode}: failed helper must not report protocol success"
        );
        if mode == "exhaust" {
            assert!(events.iter().any(|e| matches!(e, UiEvent::Output { bytes, .. } if bytes.windows(b"natural helper completion".len()).any(|b| b == b"natural helper completion"))));
        }
    }
}

#[tokio::test]
async fn cancellation_or_helper_delivery_failure_joins_both_entered_requests() {
    for reject in [false, true] {
        let (f, mut receiver) = Fixture::new("blocked");
        let cancel = f.cancel.clone();
        let consumer = tokio::spawn(async move {
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                if matches!(event, UiEvent::Output { .. }) {
                    if reject {
                        ack.acknowledge(Err("controlled helper output rejection".into()));
                        continue;
                    }
                    cancel.cancel();
                }
                ack.acknowledge(Ok(()));
            }
        });
        let error = f.run().await.unwrap_err();
        if reject {
            assert!(
                error
                    .to_string()
                    .contains("controlled helper output rejection"),
                "{error:?}"
            );
        } else {
            assert!(error.is_cancelled(), "{error:?}");
        }
        assert!(f.root.path().join("helper-pid").is_file());
        assert!(f.root.path().join("control-pid").is_file());
        assert_eq!(f.confirmations(), 0);
        f.assert_joined();
        f.runner.supervisor.shutdown().await.unwrap();
        drop(f);
        consumer.await.unwrap();
    }
}

#[tokio::test]
async fn last_protocol_acknowledgement_cancellation_keeps_confirmation_and_delivery_priority() {
    for reject in [false, true] {
        let (f, mut receiver) = Fixture::new("hidden");
        let cancel = f.cancel.clone();
        let consumer = tokio::spawn(async move {
            while let Some(delivery) = receiver.receive().await {
                let (event, ack) = delivery.into_parts();
                if matches!(event, UiEvent::Diagnostic { text, .. } if text.contains("protocol confirmed"))
                {
                    cancel.cancel();
                    if reject {
                        ack.acknowledge(Err("controlled final protocol delivery".into()));
                        continue;
                    }
                }
                ack.acknowledge(Ok(()));
            }
        });
        let error = f.run().await.unwrap_err();
        if reject {
            assert!(
                error
                    .to_string()
                    .contains("controlled final protocol delivery"),
                "{error:?}"
            );
        } else {
            assert!(error.is_cancelled(), "{error:?}");
        }
        assert_eq!(f.confirmations(), 1);
        assert_eq!(fs::read(&f.path).unwrap(), b"confirmed\n");
        f.assert_joined();
        f.runner.supervisor.shutdown().await.unwrap();
        drop(f);
        consumer.await.unwrap();
    }
}
