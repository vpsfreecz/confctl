//! Saved test and switch through the real app with confined fake native tools.
#![cfg(target_os = "linux")]
use confctl::generation::BuildGeneration;
use serde_json::{Value, json};
use std::{
    fs,
    io::{Read, Write},
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::{Child, Command, Output, Stdio},
    thread,
    time::{Duration, Instant},
};
struct Fixture {
    _tmp: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
    saved: Vec<(PathBuf, Vec<u8>)>,
}
fn executable(path: &Path, text: &str) {
    fs::write(path, text).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}
fn command_check(label: &str) -> Value {
    json!({"description":label,"command":["check-tool",label,"{checks}","{replace}","","{name}"],"timeout":0,"cooldown":0,"standardOutput":{"include":["ok"]}})
}
impl Fixture {
    fn new() -> Self {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("conf");
        let bin = tmp.path().join("bin");
        fs::create_dir(&root).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::create_dir(root.join("virtual-run")).unwrap();
        fs::write(root.join("flake.nix"), "{}\n").unwrap();
        fs::write(root.join("flake.lock"), "retained lock\n").unwrap();
        let mut machines = serde_json::Map::new();
        for (key, target) in [
            ("a", "a.invalid"),
            ("b", "b.invalid"),
            ("local", "localhost"),
        ] {
            let checks = if key == "a" {
                json!({"builderCommands":[command_check("builder")],
                    "systemd":{"enable":true,"systemProperties":[{"property":"SystemState","value":"running","timeout":0,"cooldown":0}],"unitProperties":{}},
                    "machineCommands":[command_check("machine")]})
            } else {
                json!({"builderCommands":[command_check("builder")]})
            };
            machines.insert(key.into(),json!({"name":key,"metaConfig":{"managed":true,"spin":"nixos","tags":["selected"],
                "host":{"target":target,"port":2222},"replace":"{name}","autoRollback":{"enable":true,"timeout":30},"healthChecks":checks}}));
        }
        machines.insert("guest".into(),json!({"name":"carrier#guest","carrier":"a","clusterName":"guest","metaConfig":{"managed":true,"host":{"target":null},"healthChecks":{"not-valid":true}}}));
        fs::write(
            root.join("machines.json"),
            serde_json::to_vec(&Value::Object(machines)).unwrap(),
        )
        .unwrap();
        let helper = root.join("fake 'rollback");
        executable(
            &helper,
            r#"#!/bin/sh
printf '%s\0' "$@" > "$DT_ROOT/helper-$$"
printf 'helper-start %s\n' "$DT_HOST" >> "$DT_ROOT/sequence"
[ "$1" = -t ] || exit 91
path="$DT_ROOT/virtual-run/${5##*/}"
printf 'switching' > "$path"
"$3/bin/switch-to-configuration" "$4"
printf 'switched' > "$path"
while :; do
 state=; read -r state < "$path"
 [ "$state" != confirmed ] || break
 /run/current-system/sw/bin/sleep 0.01
done
printf 'helper-end %s\n' "$DT_HOST" >> "$DT_ROOT/sequence"
if [ "$DT_MODE" = helper-fail ]; then exit 8; fi
exit 0
"#,
        );
        let mut saved = Vec::new();
        for host in ["a", "b", "local", "carrier#guest"] {
            let top = root.join("toplevels").join(format!("{host} 'saved"));
            fs::create_dir_all(top.join("bin")).unwrap();
            executable(
                &top.join("bin/switch-to-configuration"),
                r#"#!/bin/sh
printf '%s\0' "$DT_HOST" "$@" > "$DT_ROOT/activation-$$"
printf '%s %s\n' "$1" "$DT_HOST" >> "$DT_ROOT/sequence"
case "$1" in test|switch|dry-activate) ;; *) exit 92 ;; esac
if [ "$DT_MODE" = activation-fail ] && [ "$DT_HOST" = b ]; then exit 7; fi
if [ "$DT_MODE" = hidden ]; then exit 9; fi
if [ "$DT_MODE" = activation-slow ] && [ "$1" = switch ]; then printf '%s' "$$" > "$DT_ROOT/activation-pid"; exec /run/current-system/sw/bin/sleep 60; fi
printf 'test native %s\n' "$DT_HOST"
"#,
            );
            let dir = root.join(".confctl/generations").join(host).join("saved");
            fs::create_dir_all(&dir).unwrap();
            let generation = BuildGeneration {
                name: "saved".into(),
                date: chrono::DateTime::parse_from_rfc3339("2020-01-01T00:00:00Z").unwrap(),
                toplevel: top,
                auto_rollback: Some(helper.clone()),
                inputs: Default::default(),
                inputs_info: Default::default(),
            };
            let path = dir.join("generation.json");
            let bytes = serde_json::to_vec(&generation.encode()).unwrap();
            fs::write(&path, &bytes).unwrap();
            saved.push((path, bytes));
            std::os::unix::fs::symlink("saved", dir.parent().unwrap().join("current")).unwrap();
        }
        executable(
            &bin.join("nix"),
            r#"#!/bin/sh
printf '%s\0' "$@" > "$DT_ROOT/nix-$$"
last=; for arg do last=$arg; done
case "$last" in
 '.#confctl.settings') printf '{}\n' ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$DT_ROOT" ;;
 *) printf 'forbidden nix build\n' > "$DT_ROOT/forbidden"; exit 93 ;;
esac
"#,
        );
        executable(
            &bin.join("ssh"),
            r#"#!/bin/sh
printf '%s\0' "$@" > "$DT_ROOT/ssh-$$"
while [ "$#" -gt 0 ]; do
 case "$1" in -F|-l|-p) shift; shift ;; *) break ;; esac
done
case "$1" in a.invalid) DT_HOST=a ;; b.invalid) DT_HOST=b ;; *) exit 94 ;; esac
export DT_HOST
shift
[ "$#" = 1 ] || exit 95
exec /bin/sh -c "$1"
"#,
        );
        executable(
            &bin.join("cat"),
            r#"#!/bin/sh
[ "$1" = /proc/uptime ] || exit 96
printf '123.5 0\n'
"#,
        );
        executable(
            &bin.join("realpath"),
            r#"#!/bin/sh
if [ "$DT_MODE" = unknown ]; then exit 1; fi
case "$DT_MODE" in known|profile-fail|both-known|carried-known-fail)
 case "$3" in /nix/var/nix/profiles/confctl-*) DT_HOST='carrier#guest' ;; esac
 printf "%s/toplevels/%s 'saved\n" "$DT_ROOT" "$DT_HOST" ;;
 *) printf '/other-running\n' ;;
esac
"#,
        );
        executable(
            &bin.join("bash"),
            r#"#!/bin/sh
script=$(/run/current-system/sw/bin/cat)
if [ "$DT_MODE" = profile-fail ] || [ "$DT_MODE" = carried-known-fail ]; then exit 7; fi
case "$script" in *confctl-guest*) profile=/nix/var/nix/profiles/confctl-guest ;; *) profile=/nix/var/nix/profiles/system ;; esac
case "$DT_MODE" in both-known|profile-only) current="$DT_ROOT/toplevels/$DT_HOST 'saved" ;; *) current=/other-profile ;; esac
printf '%s\n' "$current"
printf '%s-1-link;%s;1700000000;\n' "$profile" "$current"
"#,
        );
        executable(
            &bin.join("sh"),
            r#"#!/bin/sh
case "$2" in
 'cat /run/confctl-confirm-'*' 2>/dev/null')
  path=${2#cat }; path=${path% 2>/dev/null}
  exec /run/current-system/sw/bin/cat "$DT_ROOT/virtual-run/${path##*/}" ;;
 'echo confirmed > /run/confctl-confirm-'*)
  path=${2#echo confirmed > }
  printf 'confirmed\n' > "$DT_ROOT/virtual-run/${path##*/}"
  printf 'confirm %s\n' "$DT_HOST" >> "$DT_ROOT/sequence" ;;
 *) exec /bin/sh "$@" ;;
esac
"#,
        );
        executable(
            &bin.join("nix-copy-closure"),
            r#"#!/bin/sh
printf '%s\0' "$NIX_SSHOPTS" "$@" > "$DT_ROOT/copy-$$"
printf 'copy %s\n' "$2" >> "$DT_ROOT/sequence"
if [ "$DT_MODE" = copy-fail ] && [ "$2" = root@b.invalid ]; then exit 7; fi
"#,
        );
        executable(
            &bin.join("check-tool"),
            r#"#!/bin/sh
label=$1; host=$5
printf '%s\0' "$@" > "$DT_ROOT/check-call-$$"
printf 'health %s %s\n' "$host" "$label" >> "$DT_ROOT/sequence"
printf 'start %s\n' "$$" >> "$DT_ROOT/check-events"
n=0; file="$DT_ROOT/attempt-$host-$label"
if [ -r "$file" ]; then read -r n < "$file"; fi
n=$((n+1)); printf '%s\n' "$n" > "$file"
if [ "$DT_MODE" = slow ]; then printf '%s' "$$" > "$DT_ROOT/check-pid"; exec /run/current-system/sw/bin/sleep 60; fi
/run/current-system/sw/bin/sleep 0.10
printf 'end %s\n' "$$" >> "$DT_ROOT/check-events"
failed=0
case "$DT_MODE" in
 fail|cooldown) failed=1 ;;
 subsets) if { [ "$label" = c1 ] && [ "$n" -lt 2 ]; } || { [ "$label" = c3 ] && [ "$n" -lt 3 ]; }; then failed=1; fi ;;
 retry-all) if [ "$label" = c1 ] && [ "$n" -lt 2 ]; then failed=1; fi ;;
esac
if [ "$failed" = 1 ]; then printf 'failed-out\377\n'; printf 'failed-err\033\n' >&2; exit 5; fi
printf 'ok\377 %s %s\n' "$host" "$label"; printf 'later-err\376\n' >&2
"#,
        );
        executable(
            &bin.join("systemctl"),
            r#"#!/bin/sh
printf '%s\0' "$DT_HOST" "$@" > "$DT_ROOT/systemctl-$$"
printf 'health %s systemd\n' "$DT_HOST" >> "$DT_ROOT/sequence"
printf 'SystemState=running\n'
"#,
        );
        executable(
            &bin.join("nix-env"),
            r#"#!/bin/sh
[ "$DT_ACTION" = switch ] || { printf 'forbidden profile' > "$DT_ROOT/forbidden"; exit 97; }
printf '%s\0' "$DT_HOST" "$@" > "$DT_ROOT/profile-$$"
printf 'profile %s\n' "$DT_HOST" >> "$DT_ROOT/sequence"
[ "$#" = 4 ] && [ "$1" = -p ] && [ "$2" = /nix/var/nix/profiles/system ] && [ "$3" = --set ] || exit 98
if [ "$DT_MODE" = profile-slow ]; then printf '%s' "$$" > "$DT_ROOT/profile-pid"; exec /run/current-system/sw/bin/sleep 60; fi
if [ "$DT_MODE" = set-profile-fail ]; then exit 6; fi
printf '%s' "$4" > "$DT_ROOT/system-profile-$DT_HOST"
"#,
        );
        executable(
            &bin.join("carrier-env"),
            r#"#!/bin/sh
[ "$DT_ACTION" = switch ] || { printf 'forbidden carrier profile' > "$DT_ROOT/forbidden"; exit 97; }
printf '%s\0' "$DT_HOST" "$@" > "$DT_ROOT/carrier-profile-$$"
printf 'carrier-profile %s\n' "$DT_HOST" >> "$DT_ROOT/sequence"
[ "$#" = 4 ] && [ "$1" = -p ] && [ "$3" = --set ] || exit 98
if [ "$DT_MODE" = carrier-profile-fail ]; then exit 6; fi
printf '%s' "$4" > "$DT_ROOT/simulated-carrier-profile"
printf 'callback %s\n' "$DT_HOST" >> "$DT_ROOT/sequence"
if [ "$DT_MODE" = callback-fail ]; then exit 8; fi
"#,
        );
        for name in ["reboot", "nix-store", "gc"] {
            executable(
                &bin.join(name),
                "#!/bin/sh\nprintf 'forbidden' > \"$DT_ROOT/forbidden\"\nexit 97\n",
            );
        }
        Self {
            _tmp: tmp,
            root,
            bin,
            saved,
        }
    }
    fn edit(&self, edit: impl FnOnce(&mut Value)) {
        let path = self.root.join("machines.json");
        let mut value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        edit(&mut value);
        fs::write(path, serde_json::to_vec(&value).unwrap()).unwrap();
    }
    fn direct(&self) {
        self.edit(|v| {
            for name in ["a", "b", "local"] {
                v[name]["metaConfig"]["autoRollback"]["enable"] = false.into();
            }
        });
    }
    fn builders(&self, count: usize) {
        self.edit(|v|v["a"]["metaConfig"]["healthChecks"]=json!({"builderCommands":(0..count).map(|i|command_check(&format!("c{i}"))).collect::<Vec<_>>()}));
    }
    fn command(&self, args: &[&str], mode: &str) -> Command {
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        cmd.current_dir(&self.root)
            .args(["--color=never", "deploy", "--generation=saved"])
            .args(args)
            .env("PATH", &self.bin)
            .env("CONFCTL_TTY", "0")
            .env("DT_ROOT", &self.root)
            .env("DT_HOST", "local")
            .env("DT_MODE", mode)
            .env(
                "DT_ACTION",
                if args.contains(&"test") {
                    "test"
                } else {
                    "switch"
                },
            )
            .env("CONFCTL_SSH_CONFIG", "native ssh config")
            .env("NIX_SSHOPTS", "-o Existing=yes")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        cmd
    }
    fn run(&self, args: &[&str], mode: &str, input: &[u8]) -> Output {
        let mut child = self.command(args, mode).spawn().unwrap();
        child.stdin.take().unwrap().write_all(input).unwrap();
        wait(child)
    }
    fn records(&self, prefix: &str) -> Vec<Vec<Vec<u8>>> {
        let mut raw = fs::read_dir(&self.root)
            .unwrap()
            .map(|e| e.unwrap().path())
            .filter(|p| p.file_name().unwrap().to_str().unwrap().starts_with(prefix))
            .map(|p| {
                let b = fs::read(&p).unwrap();
                (p, b)
            })
            .collect::<Vec<_>>();
        raw.sort_by(|a, b| a.0.cmp(&b.0)); // Diagnostics only, not native execution order.
        raw.iter()
            .map(|(_, b)| {
                let b = b
                    .strip_suffix(&[0])
                    .unwrap_or_else(|| panic!("unterminated native records: {raw:?}"));
                b.split(|c| *c == 0).map(Vec::from).collect()
            })
            .collect()
    }
    fn sequence(&self) -> Vec<String> {
        fs::read_to_string(self.root.join("sequence"))
            .unwrap_or_default()
            .lines()
            .map(str::to_owned)
            .collect()
    }
    fn logs(&self) -> Vec<PathBuf> {
        fs::read_dir(self.root.join(".confctl/logs"))
            .unwrap()
            .map(|e| e.unwrap().path())
            .collect()
    }
    fn unchanged(&self) {
        for (p, b) in &self.saved {
            assert_eq!(fs::read(p).unwrap(), *b);
        }
        for host in ["a", "b", "local", "carrier#guest"] {
            assert_eq!(
                fs::read_link(
                    self.root
                        .join(".confctl/generations")
                        .join(host)
                        .join("current")
                )
                .unwrap(),
                Path::new("saved")
            );
        }
        assert_eq!(
            fs::read(self.root.join("flake.lock")).unwrap(),
            b"retained lock\n"
        );
        assert!(!self.root.join("roots").exists());
        assert!(
            !self.root.join("forbidden").exists(),
            "{:?}",
            self.sequence()
        );
    }
}
fn wait(mut child: Child) -> Output {
    let until = Instant::now() + Duration::from_secs(30);
    loop {
        if child.try_wait().unwrap().is_some() {
            return child.wait_with_output().unwrap();
        }
        if Instant::now() > until {
            let _ = child.kill();
            panic!(
                "saved-test fixture timeout: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        thread::sleep(Duration::from_millis(10));
    }
}
fn position(sequence: &[String], prefix: &str) -> usize {
    sequence
        .iter()
        .position(|s| s.starts_with(prefix))
        .unwrap_or_else(|| panic!("missing {prefix:?}: {sequence:?}"))
}

fn interrupt_on_output(mut child: Child, needle: &'static [u8]) -> Output {
    let mut stdout = child.stdout.take().unwrap();
    let (tx, rx) = std::sync::mpsc::channel();
    let reader = thread::spawn(move || {
        let mut all = Vec::new();
        let mut buf = [0; 1024];
        let mut notified = false;
        loop {
            let n = stdout.read(&mut buf).unwrap();
            if n == 0 {
                break;
            }
            all.extend_from_slice(&buf[..n]);
            if !notified && all.windows(needle.len()).any(|s| s == needle) {
                tx.send(()).unwrap();
                notified = true;
            }
        }
        all
    });
    rx.recv_timeout(Duration::from_secs(5)).unwrap();
    rustix::process::kill_process(
        rustix::process::Pid::from_raw(child.id() as i32).unwrap(),
        rustix::process::Signal::INT,
    )
    .unwrap();
    // Keep the input writer open until SIGINT has been observed by the app;
    // racing EOF would exercise an input failure instead of prompt cancellation.
    let mut out = wait(child);
    out.stdout = reader.join().unwrap();
    out
}

#[test]
fn defaults_join_confirmed_helper_before_all_three_health_kinds() {
    let f = Fixture::new();
    let out = f.run(&["--yes", "a", "test"], "", b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let s = f.sequence();
    assert!(position(&s, "copy ") < position(&s, "helper-start a"));
    assert!(position(&s, "test a") < position(&s, "confirm a"));
    assert!(position(&s, "confirm a") < position(&s, "helper-end a"));
    assert!(
        s.iter()
            .enumerate()
            .filter(|(_, v)| v.starts_with("health "))
            .all(|(i, _)| i > position(&s, "helper-end a"))
    );
    assert_eq!(f.records("check-call-").len(), 2);
    assert_eq!(
        f.records("systemctl-"),
        [vec![b"a".to_vec(), b"show".to_vec()]]
    );
    let helper = f.records("helper-");
    assert_eq!(helper.len(), 1, "{helper:?}");
    assert_eq!(helper[0][..2], [b"-t".to_vec(), b"30".to_vec()]);
    assert_eq!(helper[0][3], b"test");
    assert!(helper[0][4].starts_with(b"/run/confctl-confirm-"));
    let copy = f.records("copy-");
    assert_eq!(copy.len(), 1, "{copy:?}");
    assert_eq!(
        &copy[0][..3],
        [
            b"-o Existing=yes -p 2222".to_vec(),
            b"--to".to_vec(),
            b"root@a.invalid".to_vec()
        ]
    );
    assert_eq!(copy[0].len(), 5); // toplevel plus saved helper
    assert!(String::from_utf8_lossy(&out.stdout).contains("1 confirmed rollback-helper protocols"));
    assert!(f.logs().is_empty());
    f.unchanged();
}
#[test]
fn direct_fallbacks_and_helper_failure_keep_real_health_order_and_partial_effects() {
    for kind in [
        "disabled",
        "configured-off",
        "missing",
        "helper-fail",
        "hidden",
    ] {
        let mut f = Fixture::new();
        let mut args = vec!["--yes"];
        if kind == "disabled" {
            args.push("--disable-auto-rollback");
        }
        if kind == "configured-off" {
            f.direct();
        }
        if kind == "missing" {
            for (path, bytes) in &mut f.saved {
                let mut v: Value = serde_json::from_slice(bytes).unwrap();
                v["auto_rollback"] = Value::Null;
                *bytes = serde_json::to_vec(&v).unwrap();
                fs::write(path, bytes).unwrap();
            }
        }
        args.extend(["a", "test"]);
        let out = f.run(
            &args,
            if kind == "helper-fail" || kind == "hidden" {
                kind
            } else {
                ""
            },
            b"",
        );
        assert_eq!(
            out.status.code(),
            Some(if kind == "helper-fail" { 1 } else { 0 }),
            "{kind}: {out:?}"
        );
        if kind == "helper-fail" {
            assert!(f.records("check-call-").is_empty());
        } else {
            assert_eq!(f.records("check-call-").len(), 2);
        }
        if ["disabled", "configured-off", "missing"].contains(&kind) {
            assert!(f.records("helper-").is_empty());
        }
        if kind == "missing" {
            assert!(String::from_utf8_lossy(&out.stdout).contains("without rollback protection"));
        }
        if kind == "hidden" {
            assert!(String::from_utf8_lossy(&out.stdout).contains("do not witness hidden switch"));
        }
        f.unchanged();
    }
}
#[test]
fn bulk_and_one_by_one_keep_copy_activation_health_order_and_abort_boundaries() {
    for serial in [false, true] {
        for failed in [false, true] {
            let f = Fixture::new();
            f.direct();
            let mut args = vec!["--yes", "--max-concurrent-copy=1"];
            if serial {
                args.push("--one-by-one");
            }
            args.extend(["{a,b}", "test"]);
            let out = f.run(&args, if failed { "activation-fail" } else { "" }, b"");
            assert_eq!(
                out.status.code(),
                Some(if failed { 1 } else { 0 }),
                "{out:?}"
            );
            let s = f.sequence();
            if serial {
                assert!(position(&s, "health a") < position(&s, "copy root@b.invalid"));
            } else {
                assert!(position(&s, "copy root@b.invalid") < position(&s, "test a"));
                if failed {
                    assert!(!s.iter().any(|s| s.starts_with("health ")));
                } else {
                    assert!(position(&s, "test b") < position(&s, "health a"));
                }
            }
            f.unchanged();
        }
    }
    let f = Fixture::new();
    f.direct();
    let out = f.run(
        &["--yes", "--interactive", "{a,b}", "test"],
        "",
        b"y\ny\ny\ny\ny\ny\n",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let sequence = f.sequence();
    assert!(position(&sequence, "copy root@b.invalid") < position(&sequence, "test a"));
    assert!(position(&sequence, "test b") < position(&sequence, "health a"));
    f.unchanged();
    let f = Fixture::new();
    f.direct();
    let out = f.run(
        &["--yes", "--max-concurrent-copy=1", "{a,b}", "test"],
        "copy-fail",
        b"",
    );
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert!(f.records("activation-").is_empty());
    assert!(f.records("check-call-").is_empty());
    assert_eq!(f.records("copy-").len(), 2);
    f.unchanged();
}
#[test]
fn known_running_carried_empty_disabled_and_interactive_skips_do_not_compile_checks() {
    for mode in ["known", "profile-fail"] {
        let f = Fixture::new();
        f.edit(|v| v["a"]["metaConfig"]["healthChecks"] = json!({"invalid":true}));
        let out = f.run(&["--yes", "a", "test"], mode, b"");
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        assert!(f.records("copy-").is_empty());
        assert!(f.records("check-call-").is_empty());
        f.unchanged();
    }
    for (kind, input) in [
        ("carried", b"".as_slice()),
        ("empty", b""),
        ("disabled", b"y\ny\ny\n"),
        ("copy-skip", b"y\nn\n"),
        ("activation-skip", b"y\ny\ns\n"),
    ] {
        let f = Fixture::new();
        f.direct();
        if kind == "empty" {
            f.edit(|v| v["a"]["metaConfig"]["healthChecks"] = json!({}));
        }
        if ["disabled", "copy-skip", "activation-skip"].contains(&kind) {
            f.edit(|v| v["a"]["metaConfig"]["healthChecks"] = json!({"invalid":true}));
        }
        let mut args = vec!["--yes"];
        if !input.is_empty() {
            args.push("--interactive");
        }
        if kind == "disabled" {
            args.push("--no-health-checks");
        }
        args.extend([
            if kind == "carried" {
                "carrier#guest"
            } else {
                "a"
            },
            "test",
        ]);
        let out = f.run(&args, "", input);
        assert_eq!(out.status.code(), Some(0), "{kind}: {out:?}");
        assert!(f.records("check-call-").is_empty());
        assert!(f.records("systemctl-").is_empty());
        f.unchanged();
    }
    let f = Fixture::new();
    f.direct();
    let out = f.run(&["--yes", "a", "test"], "unknown", b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(!f.records("activation-").is_empty());
    assert_eq!(f.records("check-call-").len(), 2);
    f.unchanged();
}
#[test]
fn health_choices_retry_borrowed_subsets_twice_or_all_with_original_argv_and_logs() {
    for all in [false, true] {
        let f = Fixture::new();
        f.direct();
        f.builders(4);
        let out = f.run(
            &["--yes", "--interactive", "a", "test"],
            if all { "retry-all" } else { "subsets" },
            if all {
                b"y\ny\ny\ny\nr\n"
            } else {
                b"y\ny\ny\ny\nf\nf\n"
            },
        );
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        let records = f.records("check-call-");
        for i in 0..4 {
            let expected = if all { 2 } else { [1, 2, 1, 3][i] };
            let calls = records
                .iter()
                .filter(|r| r[0] == format!("c{i}").as_bytes())
                .collect::<Vec<_>>();
            assert_eq!(calls.len(), expected, "{records:?}");
            for r in calls {
                assert_eq!(r.len(), 5, "{r:?}");
                assert_eq!(r[1], i.to_string().as_bytes());
                assert_eq!(r[2], b"{name}");
                assert!(r[3].is_empty());
                assert_eq!(r[4], b"a");
            }
        }
        assert_eq!(f.records("activation-").len(), 1);
        let logs = f.logs();
        assert_eq!(logs.len(), 1);
        let raw = fs::read(&logs[0]).unwrap();
        for needle in [
            b"failed-out\xff\n".as_slice(),
            b"failed-err\x1b\n",
            b"ok\xff",
            b"later-err\xfe\n",
        ] {
            assert!(raw.windows(needle.len()).any(|b| b == needle), "{needle:?}");
        }
        f.unchanged();
    }
}
#[test]
fn consent_choices_keep_going_default_abort_and_active_configuration_errors_are_honest() {
    for (input, keep, code, decline) in [
        (b"y\ny\ny\nn\n".as_slice(), false, 0, true),
        (b"y\ny\ny\ny\n?\nc\n", false, 0, false),
        (b"y\ny\ny\ny\na\n", true, 1, false),
        (b"y\ny\ny\ny\n\n", false, 1, false),
        (b"y\ny\ny\ny\n", true, 1, false),
    ] {
        let f = Fixture::new();
        f.direct();
        f.builders(1);
        let mut args = vec!["--yes", "--interactive"];
        if keep {
            args.push("--keep-going");
        }
        args.extend(["a", "test"]);
        let out = f.run(&args, "fail", input);
        assert_eq!(out.status.code(), Some(code), "{out:?}");
        let text = String::from_utf8_lossy(&out.stdout);
        if decline {
            assert!(f.records("check-call-").is_empty());
            assert!(text.contains("declined") && text.contains("unverified"));
        } else {
            assert_eq!(f.records("check-call-").len(), 1);
            if code == 0 {
                assert!(text.contains("failed health checks explicitly accepted"));
                assert_eq!(f.logs().len(), 1);
                let raw = fs::read(&f.logs()[0]).unwrap();
                for marker in [b"failed-out\xff\n".as_slice(), b"failed-err\x1b\n"] {
                    assert!(raw.windows(marker.len()).any(|bytes| bytes == marker));
                }
            }
        }
        f.unchanged();
    }
    for keep in [false, true] {
        let f = Fixture::new();
        f.direct();
        f.builders(1);
        let mut args = vec!["--yes"];
        if keep {
            args.push("--keep-going");
        }
        args.extend(["a", "test"]);
        let out = f.run(&args, "fail", b"");
        assert_eq!(out.status.code(), Some(if keep { 0 } else { 1 }), "{out:?}");
        f.unchanged();
    }
    let f = Fixture::new();
    f.direct();
    f.edit(|v| v["a"]["metaConfig"]["healthChecks"] = json!({"bad":true}));
    let out = f.run(&["--yes", "--keep-going", "a", "test"], "", b"");
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert_eq!(f.records("activation-").len(), 1);
    assert!(f.records("check-call-").is_empty());
    f.unchanged();
    let f = Fixture::new();
    f.direct();
    f.builders(1);
    f.edit(|v| {
        v["a"]["metaConfig"]["healthChecks"]["builderCommands"][0]["command"][0] =
            "missing-health-program".into()
    });
    let out = f.run(&["--yes", "--keep-going", "a", "test"], "", b"");
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert_eq!(f.records("activation-").len(), 1);
    assert!(f.records("check-call-").is_empty());
    f.unchanged();
}
#[test]
fn five_health_workers_ignore_nix_max_jobs_and_one_by_one_keeps_later_phase_ids_reserved() {
    for serial in [false, true] {
        let f = Fixture::new();
        f.direct();
        f.builders(7);
        let mut args = vec!["--yes", "--max-jobs=1"];
        if serial {
            args.push("--one-by-one");
        }
        args.extend(["{a,b}", "test"]);
        let out = f.run(&args, "", b"");
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        let events = fs::read_to_string(f.root.join("check-events")).unwrap();
        let (mut active, mut peak) = (0, 0);
        for line in events.lines() {
            if line.starts_with("start ") {
                active += 1;
                peak = peak.max(active);
            } else {
                active -= 1;
            }
            assert!((0..=5).contains(&active), "{events}");
        }
        assert_eq!(active, 0);
        assert!((2..=5).contains(&peak), "{events}");
        assert_eq!(f.records("check-call-").len(), 8);
        f.unchanged();
    }
}
#[test]
fn cancel_cooldown_or_native_child_joins_before_later_host_copy() {
    for mode in ["cooldown", "slow"] {
        let f = Fixture::new();
        f.direct();
        f.builders(1);
        if mode == "cooldown" {
            f.edit(|v| {
                v["a"]["metaConfig"]["healthChecks"]["builderCommands"][0]["timeout"] = 30.into();
                v["a"]["metaConfig"]["healthChecks"]["builderCommands"][0]["cooldown"] = 30.into();
            });
        }
        let mut child = f
            .command(&["--yes", "--one-by-one", "{a,b}", "test"], mode)
            .spawn()
            .unwrap();
        drop(child.stdin.take());
        let (out, pid) = if mode == "cooldown" {
            (interrupt_on_output(child, b"failed attempt 1"), None)
        } else {
            let until = Instant::now() + Duration::from_secs(5);
            let pid = loop {
                if let Ok(s) = fs::read_to_string(f.root.join("check-pid"))
                    && let Ok(p) = s.parse::<u32>()
                    && p > 0
                {
                    break p;
                }
                assert!(Instant::now() < until, "{:?}", f.sequence());
                thread::sleep(Duration::from_millis(10));
            };
            rustix::process::kill_process(
                rustix::process::Pid::from_raw(child.id() as i32).unwrap(),
                rustix::process::Signal::INT,
            )
            .unwrap();
            (wait(child), Some(pid))
        };
        assert_eq!(out.status.code(), Some(130), "{out:?}");
        if let Some(pid) = pid {
            assert!(!Path::new("/proc").join(pid.to_string()).exists());
        }
        assert!(!f.sequence().iter().any(|s| s == "copy root@b.invalid"));
        assert_eq!(f.records("check-call-").len(), 1);
        f.unchanged();
    }
}
#[test]
fn cancellation_at_acknowledged_health_prompt_starts_no_checks() {
    let f = Fixture::new();
    f.direct();
    let mut child = f
        .command(&["--yes", "--interactive", "a", "test"], "")
        .spawn()
        .unwrap();
    child
        .stdin
        .as_mut()
        .unwrap()
        .write_all(b"y\ny\ny\n")
        .unwrap();
    let out = interrupt_on_output(child, b"configured health checks");
    assert_eq!(out.status.code(), Some(130), "{out:?}");
    assert!(f.records("check-call-").is_empty());
    f.unchanged();
}

#[test]
fn saved_switch_explicit_or_default_joins_helper_then_sets_profile_before_health() {
    for explicit in [false, true] {
        let f = Fixture::new();
        let mut args = vec!["--yes", "a"];
        if explicit {
            args.push("switch");
        }
        let out = f.run(&args, "", b"");
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        let s = f.sequence();
        assert!(position(&s, "copy ") < position(&s, "helper-start a"));
        assert!(position(&s, "switch a") < position(&s, "confirm a"));
        assert!(position(&s, "confirm a") < position(&s, "helper-end a"));
        assert!(position(&s, "helper-end a") < position(&s, "profile a"));
        assert!(
            s.iter()
                .enumerate()
                .filter(|(_, line)| line.starts_with("health "))
                .all(|(i, _)| i > position(&s, "profile a"))
        );
        let top = f.root.join("toplevels/a 'saved");
        assert_eq!(f.records("helper-")[0][3], b"switch");
        assert_eq!(
            f.records("profile-"),
            [vec![
                b"a".to_vec(),
                b"-p".to_vec(),
                b"/nix/var/nix/profiles/system".to_vec(),
                b"--set".to_vec(),
                top.as_os_str().as_encoded_bytes().to_vec()
            ]]
        );
        assert_eq!(
            fs::read(f.root.join("system-profile-a")).unwrap(),
            top.as_os_str().as_encoded_bytes()
        );
        assert_eq!(f.records("check-call-").len(), 2);
        assert_eq!(f.records("systemctl-").len(), 1);
        let calls = f.records("ssh-");
        assert!(calls.iter().all(|r| r[..7]
            == [
                b"-F".to_vec(),
                b"native ssh config".to_vec(),
                b"-l".to_vec(),
                b"root".to_vec(),
                b"-p".to_vec(),
                b"2222".to_vec(),
                b"a.invalid".to_vec()
            ]));
        let summary = String::from_utf8_lossy(&out.stdout);
        assert!(
            summary.contains("0 direct switch actions, 1 confirmed rollback-helper protocols"),
            "{summary}"
        );
        assert!(summary.contains("0 carried profile updates"));
        f.unchanged();
    }
}

#[test]
fn switch_direct_fallbacks_and_activation_or_profile_failure_keep_partial_effects() {
    for kind in [
        "disabled",
        "configured-off",
        "missing",
        "helper-fail",
        "hidden",
        "activation-fail",
        "set-profile-fail",
    ] {
        let mut f = Fixture::new();
        let mut args = vec!["--yes", "--max-concurrent-copy=1"];
        if kind == "disabled" {
            args.push("--disable-auto-rollback");
        }
        if ["configured-off", "activation-fail", "set-profile-fail"].contains(&kind) {
            f.direct();
        }
        if kind == "missing" {
            for (path, bytes) in &mut f.saved {
                let mut value: Value = serde_json::from_slice(bytes).unwrap();
                value["auto_rollback"] = Value::Null;
                *bytes = serde_json::to_vec(&value).unwrap();
                fs::write(path, bytes).unwrap();
            }
        }
        args.extend([
            if kind == "activation-fail" {
                "b"
            } else {
                "{a,b}"
            },
            "switch",
        ]);
        let failed = ["helper-fail", "activation-fail", "set-profile-fail"].contains(&kind);
        let out = f.run(&args, kind, b"");
        assert_eq!(
            out.status.code(),
            Some(if failed { 1 } else { 0 }),
            "{kind}: {out:?}"
        );
        if failed {
            assert!(f.records("check-call-").is_empty());
            assert!(!f.root.join("system-profile-b").exists());
            if kind != "set-profile-fail" {
                assert!(f.records("profile-").is_empty());
            }
            assert!(!f.sequence().iter().any(|s| s == "switch b") || kind == "activation-fail");
        } else {
            assert_eq!(f.records("profile-").len(), 2);
            assert_eq!(f.records("check-call-").len(), 3);
        }
        if ["disabled", "configured-off", "missing"].contains(&kind) {
            assert!(f.records("helper-").is_empty());
            assert!(
                String::from_utf8_lossy(&out.stdout)
                    .contains("2 direct switch actions, 0 confirmed rollback-helper protocols")
            );
        }
        f.unchanged();
    }
}

#[test]
fn switch_requires_both_known_paths_and_preserves_carried_partial_read_knowledge() {
    for mode in [
        "both-known",
        "known",
        "profile-only",
        "profile-fail",
        "unknown",
    ] {
        let f = Fixture::new();
        f.direct();
        let out = f.run(&["--yes", "a", "switch"], mode, b"");
        assert_eq!(out.status.code(), Some(0), "{mode}: {out:?}");
        assert_eq!(
            f.records("profile-").len(),
            usize::from(mode != "both-known")
        );
        assert_eq!(f.records("copy-").len(), usize::from(mode != "both-known"));
        f.unchanged();
    }
    let f = Fixture::new();
    let out = f.run(
        &["--yes", "carrier#guest", "switch"],
        "carried-known-fail",
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(f.records("copy-").is_empty());
    assert!(f.records("carrier-profile-").is_empty());
    f.unchanged();
}

#[test]
fn carried_switch_routes_profile_and_callback_once_without_guest_actions_or_health() {
    for alias in [None, Some("rack/guest 'alias")] {
        let f = Fixture::new();
        f.edit(|v| {
            v["a"]["metaConfig"]["managed"] = false.into();
            if let Some(alias) = alias {
                v["guest"]["alias"] = alias.into();
            }
        });
        let out = f.run(
            &[
                "--yes",
                "--interactive",
                "--dry-activate-first",
                "carrier#guest",
                "switch",
            ],
            "",
            b"y\ny\n",
        );
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        let records = f.records("carrier-profile-");
        let expected_profile = format!(
            "/nix/var/nix/profiles/confctl-{}",
            alias.unwrap_or("guest").replace('/', ":")
        );
        assert_eq!(
            records,
            [vec![
                b"a".to_vec(),
                b"-p".to_vec(),
                expected_profile.as_bytes().to_vec(),
                b"--set".to_vec(),
                f.root
                    .join("toplevels/carrier#guest 'saved")
                    .as_os_str()
                    .as_encoded_bytes()
                    .to_vec()
            ]]
        );
        assert_eq!(
            f.records("copy-")[0][..3],
            [
                b"-o Existing=yes -p 2222".to_vec(),
                b"--to".to_vec(),
                b"root@a.invalid".to_vec()
            ]
        );
        assert!(f.records("activation-").is_empty());
        assert!(f.records("helper-").is_empty());
        assert!(f.records("profile-").is_empty());
        assert!(f.records("check-call-").is_empty());
        assert!(f.records("systemctl-").is_empty());
        assert_eq!(
            f.sequence().iter().filter(|s| *s == "callback a").count(),
            1
        );
        let text = String::from_utf8_lossy(&out.stdout);
        assert!(text.contains("carrier#guest on a"), "{text}");
        assert!(text.contains("0 direct switch actions, 0 confirmed rollback-helper protocols"));
        assert!(text.contains("1 carried profile updates"));
        assert!(!text.contains("[y] Continue"));
        f.unchanged();
    }
}

#[test]
fn switch_bulk_and_one_by_one_order_include_profiles_and_preserve_failed_callback_effect() {
    for serial in [false, true] {
        let f = Fixture::new();
        f.direct();
        let mut args = vec!["--yes", "--max-concurrent-copy=1"];
        if serial {
            args.push("--one-by-one");
        }
        args.extend(["{a,b}", "switch"]);
        let out = f.run(&args, "", b"");
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        let s = f.sequence();
        assert!(position(&s, "switch a") < position(&s, "profile a"));
        if serial {
            assert!(position(&s, "health a") < position(&s, "copy root@b.invalid"));
        } else {
            assert!(position(&s, "copy root@b.invalid") < position(&s, "switch a"));
            assert!(position(&s, "profile b") < position(&s, "health a"));
        }
        f.unchanged();
    }
    for mode in ["carrier-profile-fail", "callback-fail"] {
        let f = Fixture::new();
        f.edit(|v| {
            let mut last = v.as_object_mut().unwrap().remove("b").unwrap();
            last["name"] = "z".into();
            v["z"] = last;
        });
        let out = f.run(
            &["--yes", "--one-by-one", "{carrier#guest,z}", "switch"],
            mode,
            b"",
        );
        assert_eq!(out.status.code(), Some(1), "{mode}: {out:?}");
        assert_eq!(
            f.root.join("simulated-carrier-profile").exists(),
            mode == "callback-fail"
        );
        assert_eq!(
            f.sequence().iter().filter(|s| *s == "callback a").count(),
            usize::from(mode == "callback-fail")
        );
        assert_eq!(f.records("carrier-profile-").len(), 1);
        assert_eq!(f.records("copy-").len(), 1);
        assert!(f.records("activation-").is_empty());
        assert!(f.records("check-call-").is_empty());
        f.unchanged();
    }
}

#[test]
fn switch_dry_first_choices_and_interactive_bulk_keep_copy_barrier_and_profile_boundary() {
    for skip in [false, true] {
        let f = Fixture::new();
        f.direct();
        let out = f.run(
            &[
                "--yes",
                "--interactive",
                "--dry-activate-first",
                "{a,b}",
                "switch",
            ],
            "",
            if skip {
                b"y\ny\ny\ns\ny\ny\n"
            } else {
                b"y\ny\ny\nr\ny\ny\ny\n"
            },
        );
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        let s = f.sequence();
        assert!(position(&s, "copy root@b.invalid") < position(&s, "dry-activate a"));
        assert_eq!(
            s.iter().filter(|s| *s == "dry-activate a").count(),
            if skip { 1 } else { 2 }
        );
        assert_eq!(
            s.iter().filter(|s| *s == "profile a").count(),
            usize::from(!skip)
        );
        assert!(position(&s, "profile b") < position(&s, "health b"));
        f.unchanged();
    }
}

#[test]
fn switch_cancel_during_activation_or_profile_joins_and_suppresses_later_hosts_and_health() {
    for mode in ["activation-slow", "profile-slow"] {
        let f = Fixture::new();
        f.direct();
        let child = f
            .command(&["--yes", "--one-by-one", "{a,b}", "switch"], mode)
            .spawn()
            .unwrap();
        let path = f.root.join(if mode == "activation-slow" {
            "activation-pid"
        } else {
            "profile-pid"
        });
        let until = Instant::now() + Duration::from_secs(5);
        let pid = loop {
            if let Ok(text) = fs::read_to_string(&path)
                && let Ok(pid) = text.parse::<u32>()
                && pid > 0
            {
                break pid;
            }
            assert!(Instant::now() < until, "{mode}: {:?}", f.sequence());
            thread::sleep(Duration::from_millis(10));
        };
        rustix::process::kill_process(
            rustix::process::Pid::from_raw(child.id() as i32).unwrap(),
            rustix::process::Signal::INT,
        )
        .unwrap();
        let out = wait(child);
        assert_eq!(out.status.code(), Some(130), "{out:?}");
        assert!(!Path::new("/proc").join(pid.to_string()).exists());
        assert_eq!(f.records("copy-").len(), 1);
        assert!(f.records("check-call-").is_empty());
        assert!(!f.root.join("system-profile-a").exists());
        if mode == "activation-slow" {
            assert!(f.records("profile-").is_empty());
        }
        f.unchanged();
    }
}
