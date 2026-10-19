//! Actual no-build status route, using only temporary files and fake tools.
use serde_json::json;
use std::{
    fs,
    io::Write,
    os::unix::fs::{PermissionsExt, symlink},
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
    time::{Duration, Instant},
};

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
        fs::create_dir(&root).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::create_dir(root.join("native-tools")).unwrap();
        let cat = std::env::split_paths(&std::env::var_os("PATH").unwrap())
            .map(|p| p.join("cat"))
            .find(|p| p.is_file())
            .unwrap();
        symlink(cat, root.join("native-tools/cat")).unwrap();
        fs::write(root.join("flake.nix"), "{outputs=_:{};}\n").unwrap();
        fs::write(root.join("machines.json"),serde_json::to_vec(&json!({
            "a":{"name":"local","key":"local-key","metaConfig":{"managed":true,"host":{"target":"localhost"},"tags":["chosen"]}},
            "b":{"name":"carrier#guest","key":"guest.key/${literal}","carrier":"carrier","alias":"guest/one","metaConfig":{"managed":true,"host":{"target":null},"tags":["chosen"]}},
            "c":{"name":"remote","key":"remote-key","metaConfig":{"managed":true,"host":{"target":"remote.invalid"},"tags":["chosen"]}},
            "d":{"name":"carrier","metaConfig":{"managed":true,"host":{"target":"carrier.invalid","port":2222},"tags":["helper"]}},
            "e":{"name":"reference","metaConfig":{"managed":false,"host":{"target":"reference.invalid"},"tags":["excluded"]}},
            "f":{"name":"null","metaConfig":{"managed":true,"host":{"target":null},"tags":["excluded"]}},
            "g":{"name":"false","metaConfig":{"managed":true,"host":{"target":false},"tags":["excluded"]}},
            "h":{"name":"offline","metaConfig":{"managed":true,"host":{"target":"offline.invalid"},"tags":["offline"]}}
        })).unwrap()).unwrap();
        for (file, value) in [
            ("target-local", json!({"nixpkgs":"full-local"})),
            ("target-guest", json!({"nixpkgs":"full-guest"})),
            ("target-remote", json!({"nixpkgs":"full-remote"})),
            (
                "deployed-local",
                json!({"nixpkgs":{"rev":"full-local","short_rev":"local-rev"},"extra":"extra-rev"}),
            ),
            ("deployed-remote", json!({"nixpkgs":"different-remote"})),
            ("machine", json!({"inputs-info":{"nixpkgs":"full-guest"}})),
            ("fallback", json!({"nixpkgs":"full-guest"})),
        ] {
            fs::write(root.join(file), serde_json::to_vec(&value).unwrap()).unwrap();
        }
        executable(
            &bin.join("nix"),
            r#"#!/bin/sh
last=
for arg do last="$arg"; printf '%s\n' "$arg" >> "$STATUS_ROOT/nix-args"; done
case "$last" in
  '.#confctl.settings') printf '%s\n' '{"nix":{"maxJobs":"auto","impureEval":false,"legacyNixPath":false},"list":{"columns":["name"]}}' ;;
  '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$STATUS_ROOT" ;;
  *'.#confctl.inputsInfo.'*)
    printf '%s\n' "$last" >> "$STATUS_ROOT/input-evals"
    if [ -e "$STATUS_ROOT/eval-fail" ]; then printf 'fixture target unavailable\n' >&2; exit 42; fi
    if [ -e "$STATUS_ROOT/eval-garbage" ]; then printf '{broken'; exit 0; fi
    if [ -e "$STATUS_ROOT/eval-shape" ]; then printf '[]\n'; exit 0; fi
    case "$last" in *guest*) file=target-guest ;; *remote*) file=target-remote ;; *) file=target-local ;; esac
    exec "$STATUS_ROOT/native-tools/cat" "$STATUS_ROOT/$file"
    ;;
  *) printf 'unexpected Nix target: %s\n' "$last" >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &bin.join("cat"),
            r#"#!/bin/sh
printf '%s\n' "$*" >> "$STATUS_ROOT/local-calls"
case "$*" in
  /proc/uptime) printf '0.25 0\n' ;;
  /etc/confctl/inputs-info.json) exec "$STATUS_ROOT/native-tools/cat" "$STATUS_ROOT/deployed-local" ;;
  *) printf 'unexpected local read\n' >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &bin.join("bash"),
            r#"#!/bin/sh
[ "$*" = --norc ] || exit 97
"$STATUS_ROOT/native-tools/cat" > "$STATUS_ROOT/local-script"
printf '/remote/current\n'
"#,
        );
        executable(
            &bin.join("ssh"),
            r#"#!/bin/sh
last=
for arg do last="$arg"; printf '%s\n' "$arg" >> "$STATUS_ROOT/ssh-args"; done
case "$*" in *offline.invalid*) printf 'fixture offline\n' >&2; exit 255 ;; esac
case "$last" in
  'cat /proc/uptime') printf '0 0\n' ;;
  'bash --norc')
    "$STATUS_ROOT/native-tools/cat" > "$STATUS_ROOT/remote-script"
    if [ -e "$STATUS_ROOT/profile-wait" ]; then : > "$STATUS_ROOT/profile-started"; exec sleep 30; fi
    printf '/remote/current\n'
    ;;
  *machine.json)
    if [ -e "$STATUS_ROOT/machine-fail" ]; then printf 'machine metadata missing\n' >&2; exit 1; fi
    exec "$STATUS_ROOT/native-tools/cat" "$STATUS_ROOT/machine"
    ;;
  *confctl-guest:one/etc/confctl/inputs-info.json)
    : > "$STATUS_ROOT/fallback-read"
    if [ -e "$STATUS_ROOT/fallback-fail" ]; then printf 'profile inputs missing\n' >&2; exit 1; fi
    exec "$STATUS_ROOT/native-tools/cat" "$STATUS_ROOT/fallback"
    ;;
  'cat /etc/confctl/inputs-info.json') exec "$STATUS_ROOT/native-tools/cat" "$STATUS_ROOT/deployed-remote" ;;
  *) printf 'unexpected remote command: %s\n' "$last" >&2; exit 97 ;;
esac
"#,
        );
        for tool in [
            "realpath",
            "logname",
            "nix-env",
            "nix-store",
            "nix-copy-closure",
            "nix-collect-garbage",
            "switch-to-configuration",
            "uname",
        ] {
            executable(
                &bin.join(tool),
                "#!/bin/sh\nprintf 'forbidden command\n' >&2\nexit 98\n",
            );
        }
        Self {
            _scratch: scratch,
            root,
            bin,
        }
    }
    fn command(&self, args: &[&str]) -> Command {
        let inherited = std::env::var_os("PATH").unwrap();
        let path = std::env::join_paths(
            std::iter::once(self.bin.clone()).chain(std::env::split_paths(&inherited)),
        )
        .unwrap();
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        cmd.arg("--color=never")
            .args(args)
            .current_dir(&self.root)
            .env("PATH", path)
            .env("STATUS_ROOT", &self.root)
            .env("CONFCTL_LOGNAME", self.bin.join("logname"))
            .env("CONFCTL_SSH_CONFIG", self.root.join("ssh config"))
            .env("TZ", "UTC")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        cmd
    }
    fn run(&self, args: &[&str], answer: &[u8]) -> Output {
        let mut child = self.command(args).spawn().unwrap();
        child.stdin.take().unwrap().write_all(answer).unwrap();
        child.wait_with_output().unwrap()
    }
    fn status(&self, args: &[&str]) -> Output {
        let mut all = vec!["status", "--generation", "none", "--yes"];
        all.extend_from_slice(args);
        self.run(&all, &[])
    }
    fn readonly(&self) {
        assert!(!self.root.join(".confctl/mutation.lock").exists());
        assert!(!self.root.join(".confctl/generations").exists());
        for name in ["gcroots", "roots", "result"] {
            assert!(!self.root.join(name).exists());
        }
    }
}
fn executable(path: &Path, source: &str) {
    fs::write(path, source).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}
fn success(output: &Output) -> String {
    assert!(
        output.status.success(),
        "stdout={}\nstderr={}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout.clone()).unwrap()
}
fn status_rows(text: &str) -> Vec<&str> {
    text.lines()
        .skip_while(|line| !line.starts_with("HOST") || !line.contains("ONLINE"))
        .skip(1)
        .filter(|line| !line.is_empty())
        .collect()
}

#[test]
fn actual_none_status_reads_per_host_inputs_and_preserves_comparison_without_color() {
    let f = Fixture::new();
    let output = f.status(&["--tag=chosen"]);
    let text = success(&output);
    let rows = status_rows(&text);
    assert_eq!(rows.len(), 3, "{text}");
    assert!(
        rows[0].starts_with("local")
            && rows[0].contains("0.250s")
            && rows[0].contains("ok")
            && rows[0].contains("same")
    );
    assert!(
        rows[1].starts_with("carrier#guest")
            && rows[1].contains("ok")
            && rows[1].contains("0.000s")
    );
    assert!(
        rows[2].starts_with("remote")
            && rows[2].contains("outdated")
            && rows[2].contains("changed")
    );
    assert!(rows.iter().all(|r| r.contains("0:0")));
    assert!(!text.contains('\x1b'));
    let calls = fs::read_to_string(f.root.join("input-evals")).unwrap();
    assert!(calls.contains(r#".#confctl.inputsInfo."guest.key/\${literal}""#));
    assert!(calls.contains(".#confctl.inputsInfo.local-key"));
    let ssh = fs::read_to_string(f.root.join("ssh-args")).unwrap();
    assert!(ssh.contains("-p\n2222\ncarrier.invalid\n"));
    assert!(ssh.contains("confctl-guest:one/machine.json"));
    assert!(!ssh.contains("/run/current-system"));
    let nix = fs::read_to_string(f.root.join("nix-args")).unwrap();
    assert!(!nix.contains("toplevel") && !nix.contains("buildPlan") && !nix.contains("flake.lock"));
    f.readonly();
}

#[test]
fn selection_consent_and_unavailable_routes_stop_before_observations() {
    for pattern in ["reference", "null", "false", "absent"] {
        let f = Fixture::new();
        let out = f.status(&[pattern]);
        assert!(!out.status.success());
        assert!(!f.root.join("input-evals").exists());
        assert!(!f.root.join("ssh-args").exists());
        f.readonly();
    }
    let f = Fixture::new();
    let out = f.run(&["status", "--generation=none", "local"], b"n\n");
    assert!(!out.status.success());
    assert!(!f.root.join("input-evals").exists());
    assert!(!f.root.join("local-calls").exists());
    for args in [vec!["status"], vec!["status", "--generation=current"]] {
        let f = Fixture::new();
        fs::remove_file(f.root.join("flake.nix")).unwrap();
        let out = f.run(&args, &[]);
        assert!(!out.status.success());
        assert!(!f.root.join(".confctl").exists());
        assert!(!f.root.join("nix-args").exists());
    }
}

#[test]
fn carried_fallback_recovers_native_and_json_failures_without_hiding_attempts() {
    let f = Fixture::new();
    fs::write(f.root.join("machine"),serde_json::to_vec(&json!({"inputs-info":r#"{"nixpkgs":{"rev":"full-guest","shortRev":"first","short_rev":"guest-alias"}}"#})).unwrap()).unwrap();
    let output = f.status(&["carrier#guest"]);
    let text = success(&output);
    assert!(
        status_rows(&text)[0].contains("ok")
            && status_rows(&text)[0].contains("guest-alias (same)")
    );
    assert!(!f.root.join("fallback-read").exists());
    for mode in ["machine-fail", "invalid-json", "null"] {
        let f = Fixture::new();
        match mode {
            "machine-fail" => fs::write(f.root.join(mode), "").unwrap(),
            "invalid-json" => fs::write(f.root.join("machine"), "{broken").unwrap(),
            _ => fs::write(f.root.join("machine"), r#"{"inputs-info":null}"#).unwrap(),
        };
        let output = f.status(&["carrier#guest"]);
        let text = success(&output);
        assert!(status_rows(&text)[0].contains("ok"));
        assert!(f.root.join("fallback-read").exists());
        let diagnostic = String::from_utf8_lossy(&output.stderr);
        assert!(diagnostic.contains("profile fallback"));
        if mode == "machine-fail" {
            assert!(
                diagnostic.contains("machine.json failed") && diagnostic.contains("exit Some(1)")
            );
        }
        if mode == "invalid-json" {
            assert!(diagnostic.contains("invalid machine.json"));
        }
        f.readonly();
    }
    for value in [json!({}), json!(7), json!("broken")] {
        let f = Fixture::new();
        fs::write(
            f.root.join("machine"),
            serde_json::to_vec(&json!({"inputs-info":value})).unwrap(),
        )
        .unwrap();
        let output = f.status(&["carrier#guest"]);
        assert!(status_rows(&success(&output))[0].contains("outdated"));
        assert!(!f.root.join("fallback-read").exists());
    }
    let f = Fixture::new();
    fs::write(f.root.join("machine-fail"), "").unwrap();
    fs::write(f.root.join("fallback-fail"), "").unwrap();
    let output = f.status(&["carrier#guest"]);
    assert!(status_rows(&success(&output))[0].contains("outdated"));
    assert!(String::from_utf8_lossy(&output.stderr).contains("profile inputs missing"));
}

#[test]
fn target_native_failure_and_domain_shape_are_unknown_but_protocol_decode_fails() {
    for mode in ["eval-fail", "eval-shape"] {
        let f = Fixture::new();
        fs::write(f.root.join(mode), "").unwrap();
        let out = f.status(&["local"]);
        let text = success(&out);
        assert!(status_rows(&text)[0].contains("outdated"));
        assert!(String::from_utf8_lossy(&out.stderr).contains("target inputs"));
        f.readonly();
    }
    let f = Fixture::new();
    fs::write(f.root.join("eval-garbage"), "").unwrap();
    let out = f.status(&["local"]);
    assert!(!out.status.success());
    assert!(!f.root.join("local-calls").exists());
    f.readonly();
    let f = Fixture::new();
    let out = f.status(&["offline"]);
    let text = success(&out);
    let row = status_rows(&text)[0];
    assert!(row.contains("unknown") && row.contains("0:unknown") && row.contains("outdated"));
    assert!(String::from_utf8_lossy(&out.stderr).contains("uptime for offline"));
    f.readonly();
}

#[test]
fn cancellation_joins_the_profile_child_and_output_failure_is_not_success() {
    use nix::{
        sys::signal::{Signal, kill},
        unistd::Pid,
    };
    let f = Fixture::new();
    fs::write(f.root.join("profile-wait"), "").unwrap();
    let mut child = f
        .command(&["status", "--generation=none", "--yes", "remote"])
        .spawn()
        .unwrap();
    drop(child.stdin.take());
    let deadline = Instant::now() + Duration::from_secs(5);
    while !f.root.join("profile-started").exists() {
        if Instant::now() >= deadline {
            child.kill().unwrap();
            let _ = child.wait();
            panic!("profile child did not start");
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    kill(
        Pid::from_raw(child.id().try_into().unwrap()),
        Signal::SIGINT,
    )
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while child.try_wait().unwrap().is_none() {
        if Instant::now() >= deadline {
            child.kill().unwrap();
            let _ = child.wait();
            panic!("profile child did not join");
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    let out = child.wait_with_output().unwrap();
    assert!(!out.status.success());
    assert!(String::from_utf8_lossy(&out.stderr).contains("canceled"));
    f.readonly();
    let f = Fixture::new();
    let out = f
        .command(&["status", "--generation=none", "--yes", "local"])
        .stdout(fs::File::create("/dev/full").unwrap())
        .output()
        .unwrap();
    assert!(!out.status.success());
    f.readonly();
}

#[test]
fn local_counts_report_exclusions_without_resolving_or_rewriting_current() {
    let f = Fixture::new();
    let dir = f.root.join(".confctl/generations/local");
    fs::create_dir_all(dir.join("supported")).unwrap();
    fs::create_dir(dir.join("rejected")).unwrap();
    let supported=serde_json::to_vec(&json!({"mode":"flakes","date":"2000-01-01T00:00:00Z","toplevel":"/saved/unchanged","inputs_info":{"keep":{"future":[false,7]}}})).unwrap();
    fs::write(dir.join("supported/generation.json"), &supported).unwrap();
    let rejected = b"{invalid saved record\n";
    fs::write(dir.join("rejected/generation.json"), rejected).unwrap();
    symlink("missing-current", dir.join("current")).unwrap();
    let output = f.status(&["local"]);
    let text = success(&output);
    assert!(status_rows(&text)[0].contains("1:0"));
    assert!(String::from_utf8_lossy(&output.stderr).contains("Skipping generation"));
    assert_eq!(
        fs::read(dir.join("supported/generation.json")).unwrap(),
        supported
    );
    assert_eq!(
        fs::read(dir.join("rejected/generation.json")).unwrap(),
        rejected
    );
    assert_eq!(
        fs::read_link(dir.join("current")).unwrap(),
        Path::new("missing-current")
    );
    assert!(!f.root.join(".confctl/mutation.lock").exists());
}
