//! Network-free actual diff/changelog routes; every external tool is a fake.
#![cfg(target_os = "linux")]
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::{
    fs,
    io::Write,
    os::unix::fs::{PermissionsExt, symlink},
    path::{Path, PathBuf},
    process::{Child, Command, Output, Stdio},
    thread,
    time::{Duration, Instant},
};

struct Fixture {
    _scratch: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
    cache: PathBuf,
    home: PathBuf,
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let bin = scratch.path().join("bin");
        let cache = scratch.path().join("cache");
        let home = scratch.path().join("home");
        fs::create_dir_all(root.join("native")).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::create_dir(&home).unwrap();
        let cat = std::env::split_paths(&std::env::var_os("PATH").unwrap())
            .map(|p| p.join("cat"))
            .find(|p| p.is_file())
            .unwrap();
        symlink(cat, root.join("native/cat")).unwrap();
        fs::write(root.join("flake.nix"), "{outputs=_:{};}\n").unwrap();
        fs::write(root.join("flake.lock"), "retained lock\n").unwrap();
        let fixture = Self {
            _scratch: scratch,
            root,
            bin,
            cache,
            home,
        };
        fixture.json("machines.json", json!({
            "a":{"name":"alpha","key":"a-key","metaConfig":{"managed":true,"host":{"target":"localhost"},"tags":["chosen"]}},
            "b":{"name":"beta","key":"b-key","metaConfig":{"managed":true,"host":{"target":"beta.invalid"},"tags":["chosen"]}},
            "guest":{"name":"carrier#guest","key":"g-key","carrier":"carrier","alias":"guest/one","metaConfig":{"managed":true,"host":{"target":null},"tags":["guest"]}},
            "carrier":{"name":"carrier","metaConfig":{"managed":false,"host":{"target":"carrier.invalid","port":2222}}},
            "reference":{"name":"reference","metaConfig":{"managed":false,"host":{"target":"reference.invalid"},"tags":["chosen"]}},
            "null":{"name":"null","metaConfig":{"managed":true,"host":{"target":null},"tags":["null"]}}
        }));
        for host in ["a", "b", "g"] {
            fixture.json(&format!("target-{host}"), json!({"main":{"rev":"abcdefgh-new","shortRev":"abcdefgh","url":"https://example.invalid/repo"}}));
            fixture.json(&format!("deployed-{host}"), json!({"main":{"rev":"abcdefgh-old","short_rev":"abcdefgh","url":"https://old.invalid/repo"}}));
        }
        fixture.json("machine", json!({"inputs-info":{"main":"deployed"}}));
        fixture.json("fallback", json!({"main":"deployed"}));
        executable(
            &fixture.bin.join("nix"),
            r#"#!/bin/sh
last=
for arg do last="$arg"; done
printf 'nix:%s\n' "$last" >> "$CHANGES_ROOT/calls"
printf '%s\0' "$@" >> "$CHANGES_ROOT/nix-args"
case "$last" in
 '.#confctl.settings') printf '%s\n' '{"list":{"columns":["name"]},"nix":{"maxJobs":"auto","impureEval":false,"legacyNixPath":false}}' ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$CHANGES_ROOT" ;;
 '.#confctl.inputsInfo.'*)
   if [ -e "$CHANGES_ROOT/eval-fail" ]; then printf 'target failed\n' >&2; exit 42; fi
   if [ -e "$CHANGES_ROOT/eval-json" ]; then printf '{broken'; exit 0; fi
   case "$last" in *b-key*) host=b ;; *g-key*) host=g ;; *) host=a ;; esac
   exec "$CHANGES_ROOT/native/cat" "$CHANGES_ROOT/target-$host" ;;
 *) printf 'forbidden Nix action: %s\n' "$last" >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &fixture.bin.join("cat"),
            r#"#!/bin/sh
printf 'local:%s\n' "$*" >> "$CHANGES_ROOT/calls"
case "$*" in
 /proc/uptime) printf '1 0\n' ;;
 /etc/confctl/inputs-info.json) exec "$CHANGES_ROOT/native/cat" "$CHANGES_ROOT/deployed-a" ;;
 *) printf 'forbidden local read\n' >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &fixture.bin.join("ssh"),
            r#"#!/bin/sh
last=
for arg do last="$arg"; done
printf 'ssh:%s\n' "$last" >> "$CHANGES_ROOT/calls"
printf '%s\0' "$@" > "$CHANGES_ROOT/ssh-args-$$"
if [ -e "$CHANGES_ROOT/offline" ]; then printf 'offline fixture\n' >&2; exit 255; fi
case "$last" in
 'cat /proc/uptime') printf '2 0\n' ;;
 'cat /etc/confctl/inputs-info.json') exec "$CHANGES_ROOT/native/cat" "$CHANGES_ROOT/deployed-b" ;;
 *machine.json)
   if [ -e "$CHANGES_ROOT/machine-fail" ]; then printf 'no machine.json\n' >&2; exit 1; fi
   exec "$CHANGES_ROOT/native/cat" "$CHANGES_ROOT/machine" ;;
 *confctl-guest:one/etc/confctl/inputs-info.json) exec "$CHANGES_ROOT/native/cat" "$CHANGES_ROOT/fallback" ;;
 *) printf 'forbidden SSH command: %s\n' "$last" >&2; exit 97 ;;
esac
"#,
        );
        executable(
            &fixture.bin.join("git"),
            r#"#!/bin/sh
printf 'git:%s\n' "$*" >> "$CHANGES_ROOT/calls"
printf '%s\0' "$@" > "$CHANGES_ROOT/git-args-$$"
case "$1" in
 clone) last=; for arg do last="$arg"; done; printf 'partial clone\n' > "$last/fixture-cache" ;;
 -C) printf 'updated\n' >> "$2/fixture-cache" ;;
 *) exit 97 ;;
esac
if [ -e "$CHANGES_ROOT/git-slow" ]; then printf '%s\n' "$$" > "$CHANGES_ROOT/git-pid"; exec /run/current-system/sw/bin/sleep 60; fi
if [ -e "$CHANGES_ROOT/git-fail" ]; then printf 'Git native failure\377\n' >&2; exit 17; fi
case "$*" in *' log '*)
 : > "$CHANGES_ROOT/git-log-entered"
 if [ -e "$CHANGES_ROOT/large-log" ]; then exec "$CHANGES_ROOT/native/cat" "$CHANGES_ROOT/large-log"; fi
 printf '< commit bytes\377\n'
 ;;
esac
"#,
        );
        fixture
    }
    fn json(&self, file: &str, value: Value) {
        fs::write(self.root.join(file), serde_json::to_vec(&value).unwrap()).unwrap();
    }
    fn command(&self, args: &[&str]) -> Command {
        let mut command = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        command
            .current_dir(&self.root)
            .arg("--color=never")
            .args(args)
            .env("PATH", &self.bin)
            .env("CHANGES_ROOT", &self.root)
            .env("HOME", &self.home)
            .env("XDG_CACHE_HOME", &self.cache)
            .env("CONFCTL_TTY", "0")
            .env("PAGER", "must-not-run")
            .env("GIT_PAGER", "must-not-run")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        command
    }
    fn run(&self, args: &[&str], input: &[u8]) -> Output {
        let mut child = self.command(args).spawn().unwrap();
        child.stdin.take().unwrap().write_all(input).unwrap();
        wait(child)
    }
    fn calls(&self) -> Vec<String> {
        fs::read_to_string(self.root.join("calls"))
            .unwrap_or_default()
            .lines()
            .map(str::to_owned)
            .collect()
    }
    fn mirror(&self, url: &str) -> PathBuf {
        self.cache.join("confctl/git-mirrors").join(
            Sha256::digest(url.as_bytes())
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<String>(),
        )
    }
    fn saved(&self, host: &str, name: &str, inputs: Value) -> Vec<u8> {
        let path = self
            .root
            .join(".confctl/generations")
            .join(host.replace('/', ":"))
            .join(name);
        fs::create_dir_all(&path).unwrap();
        let bytes=serde_json::to_vec(&json!({"mode":"flakes","date":"2000-01-01T00:00:00Z","toplevel":"/saved/system","inputs_info":inputs})).unwrap();
        fs::write(path.join("generation.json"), &bytes).unwrap();
        bytes
    }
    fn assert_unchanged(&self) {
        assert_eq!(
            fs::read(self.root.join("flake.lock")).unwrap(),
            b"retained lock\n"
        );
        assert!(!self.root.join(".confctl/mutation.lock").exists());
        assert!(!self.root.join("configs/node/kernels.json").exists());
        for call in self.calls().iter().filter(|call| call.starts_with("nix:")) {
            assert!(
                call.contains("confctl.settings")
                    || call.contains("confctl.machinesJson")
                    || call.contains("confctl.inputsInfo"),
                "{call}"
            );
        }
        assert!(!self.calls().iter().any(|call| call.contains("realpath")
            || call.contains("bash")
            || call.contains("profile generations")));
    }
}
fn executable(path: &Path, script: &str) {
    fs::write(path, script).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}
fn wait(mut child: Child) -> Output {
    let end = Instant::now() + Duration::from_secs(15);
    loop {
        if child.try_wait().unwrap().is_some() {
            return child.wait_with_output().unwrap();
        }
        if Instant::now() >= end {
            let _ = child.kill();
            panic!("comparison hung: {:?}", child.wait_with_output().unwrap());
        }
        thread::sleep(Duration::from_millis(10));
    }
}

#[test]
fn current_diff_evaluates_all_targets_before_ordered_read_only_comparisons() {
    let f = Fixture::new();
    fs::create_dir_all(f.root.join(".confctl")).unwrap();
    fs::write(
        f.root.join(".confctl/generations"),
        b"current comparisons must not walk generations",
    )
    .unwrap();
    let out = f.run(&["diff", "--yes", "--tag=chosen"], b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(
        text.contains("alpha @ main (https://example.invalid/repo): abcdefgh -> abcdefgh"),
        "{out:?}"
    );
    assert!(text.contains("beta @ main"), "{out:?}");
    let calls = f.calls();
    let eval_a = calls
        .iter()
        .position(|s| s == "nix:.#confctl.inputsInfo.a-key")
        .unwrap();
    let eval_b = calls
        .iter()
        .position(|s| s == "nix:.#confctl.inputsInfo.b-key")
        .unwrap();
    assert!(
        eval_a < eval_b
            && eval_b
                < calls
                    .iter()
                    .position(|s| s == "local:/proc/uptime")
                    .unwrap()
    );
    assert_eq!(
        &calls[calls
            .iter()
            .position(|s| s == "local:/proc/uptime")
            .unwrap()..],
        [
            "local:/proc/uptime",
            "local:/etc/confctl/inputs-info.json",
            "ssh:cat /proc/uptime",
            "ssh:cat /etc/confctl/inputs-info.json"
        ]
    );
    assert!(!f.cache.exists());
    assert!(!calls.iter().any(|s| s.starts_with("git:")));
    f.assert_unchanged();
}

#[test]
fn saved_none_is_literal_missing_consent_and_empty_current_selection_are_distinct() {
    let f = Fixture::new();
    let bytes = f.saved("alpha", "none", json!({"main":"saved-revision"}));
    let out = f.run(&["diff", "--generation=none", "--yes", "alpha"], b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(String::from_utf8_lossy(&out.stdout).contains("saved-re"));
    assert!(!f.calls().iter().any(|s| s.contains("inputsInfo")));
    assert_eq!(
        fs::read(
            f.root
                .join(".confctl/generations/alpha/none/generation.json")
        )
        .unwrap(),
        bytes
    );
    f.assert_unchanged();
    for (args, input, code) in [
        (vec!["diff", "--yes", "absent*"], b"".as_slice(), 0),
        (
            vec!["diff", "--yes", "--generation=missing", "alpha"],
            b"".as_slice(),
            1,
        ),
        (
            vec!["diff", "--generation=none", "--tag=chosen"],
            b"y\nn\n".as_slice(),
            1,
        ),
        (vec!["diff", "--tag=chosen"], b"n\n".as_slice(), 1),
    ] {
        let f = Fixture::new();
        f.saved("alpha", "none", json!({"main":"saved"}));
        let out = f.run(&args, input);
        assert_eq!(out.status.code(), Some(code), "{out:?}");
        assert!(
            !f.calls()
                .iter()
                .any(|s| s.starts_with("local:") || s.starts_with("ssh:") || s.starts_with("git:"))
        );
        f.assert_unchanged();
    }
}

#[test]
fn roles_globs_unknown_no_changes_no_inputs_downgrade_and_truthy_url_fallback() {
    for (target, deployed, pattern, expected) in [
        (json!({}), json!({}), "*", "alpha: no inputs info"),
        (
            json!({"main":"same"}),
            json!({"main":"same"}),
            "*",
            "alpha: no changes",
        ),
        (
            json!({"main":{"rev":7}}),
            json!({"main":{"rev":"7"}}),
            "*",
            "unknown -> 7",
        ),
        (
            json!({"main":null}),
            json!({"main":"old"}),
            "*",
            "alpha @ main in unknown state",
        ),
        (
            json!({"main":{"rev":"new","url":false}}),
            json!({"main":{"rev":"old","url":"fallback"}}),
            "m{ain,isc}",
            "alpha @ main (fallback): new -> old",
        ),
    ] {
        let f = Fixture::new();
        f.json("target-a", target);
        f.json("deployed-a", deployed);
        let out = f.run(&["diff", "--yes", "--downgrade", "alpha", pattern], b"");
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        assert!(
            String::from_utf8_lossy(&out.stdout).contains(expected),
            "{out:?}"
        );
        f.assert_unchanged();
    }
    let f = Fixture::new();
    let out = f.run(&["diff", "--yes", "alpha", "absent*"], b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(out.stdout.is_empty(), "{out:?}");
    assert!(!f.cache.exists());
}

#[test]
fn ordinary_offline_and_native_target_failures_are_distinct_from_protocol_and_null_target_errors() {
    let f = Fixture::new();
    fs::write(f.root.join("offline"), b"").unwrap();
    let out = f.run(&["diff", "--yes", "beta"], b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(String::from_utf8_lossy(&out.stdout).contains("beta is offline"));
    assert!(
        !f.calls()
            .iter()
            .any(|s| s == "ssh:cat /etc/confctl/inputs-info.json")
    );
    f.assert_unchanged();
    for (flag, code) in [("eval-fail", 0), ("eval-json", 1)] {
        let f = Fixture::new();
        fs::write(f.root.join(flag), b"").unwrap();
        let out = f.run(&["diff", "--yes", "alpha"], b"");
        assert_eq!(out.status.code(), Some(code), "{out:?}");
        if code == 0 {
            assert!(String::from_utf8_lossy(&out.stdout).contains("unknown state"));
        }
        f.assert_unchanged();
    }
    let f = Fixture::new();
    let out = f.run(&["diff", "--yes", "null"], b"");
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    assert!(String::from_utf8_lossy(&out.stderr).contains("no target host"));
}

#[test]
fn carrier_selected_json_and_profile_fallback_keep_the_shared_status_semantics() {
    for (value, fallback) in [
        (json!({"inputs-info":{"main":"deployed"}}), false),
        (json!({"inputs-info":false}), true),
        (json!({"inputs-info":"{\"main\":\"deployed\"}"}), false),
        (json!({"inputs-info":[]}), false),
    ] {
        let f = Fixture::new();
        f.json("machine", value);
        let out = f.run(&["diff", "--yes", "carrier#guest"], b"");
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        assert_eq!(
            f.calls()
                .iter()
                .any(|s| s.contains("confctl-guest:one/etc/confctl/inputs-info.json")),
            fallback
        );
        f.assert_unchanged();
    }
    let f = Fixture::new();
    fs::write(f.root.join("machine-fail"), b"").unwrap();
    let out = f.run(&["diff", "--yes", "carrier#guest"], b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(
        f.calls()
            .iter()
            .any(|s| s.contains("confctl-guest:one/etc/confctl/inputs-info.json"))
    );
}

#[test]
fn changelog_clones_fetches_each_repeated_role_and_keeps_full_direction_and_flags() {
    let f = Fixture::new();
    let url = "ssh://example.invalid/a b'\"$x;repo";
    f.json("target-a",json!({"z":{"url":url,"rev":"--new","shortRev":"equal"},"a":{"url":url,"rev":"new-full","shortRev":"equal"}}));
    f.json("deployed-a",json!({"z":{"url":"ignored","rev":"old-z","shortRev":"equal"},"a":{"rev":"old-full","shortRev":"equal"}}));
    let out = f.run(
        &["changelog", "--yes", "--patch", "--downgrade", "alpha"],
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let calls = f
        .calls()
        .into_iter()
        .filter(|s| s.starts_with("git:"))
        .collect::<Vec<_>>();
    assert_eq!(calls.len(), 4, "{calls:?}");
    assert!(calls[0].contains("clone --quiet --mirror -- ssh://"));
    assert!(calls[1].contains("--oneline -p --end-of-options new-full..old-full --"));
    assert!(calls[2].contains("fetch --no-show-forced-updates --quiet"));
    assert!(calls[3].contains("--end-of-options --new..old-z --"));
    assert!(!calls.iter().any(|s| s.contains("--max-count")));
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(text.find("alpha @ a:").unwrap() < text.find("alpha @ z:").unwrap());
    assert!(out.stdout.contains(&255));
    assert!(f.mirror(url).join("fixture-cache").exists());
    f.assert_unchanged();
    let f = Fixture::new();
    let out = f.run(&["changelog", "--yes", "--verbose", "alpha"], b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(
        !f.calls()
            .iter()
            .any(|s| s.contains("--oneline") || s.contains("--max-count"))
    );
}

#[test]
fn git_native_failures_continue_roles_and_leave_partial_cache_while_spawn_errors_abort() {
    let f = Fixture::new();
    f.json(
        "target-a",
        json!({"a":{"url":"repo","rev":"new"},"b":{"url":"repo","rev":"new"}}),
    );
    f.json("deployed-a", json!({"a":"old","b":"old"}));
    fs::write(f.root.join("git-fail"), b"").unwrap();
    let out = f.run(&["changelog", "--yes", "alpha"], b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(
        f.calls().iter().filter(|s| s.starts_with("git:")).count(),
        2
    );
    assert!(String::from_utf8_lossy(&out.stdout).contains("Git failed with exit status: 17"));
    assert!(f.mirror("repo").join("fixture-cache").exists());
    f.assert_unchanged();
    let f = Fixture::new();
    fs::remove_file(f.bin.join("git")).unwrap();
    let out = f.run(&["changelog", "--yes", "alpha"], b"");
    assert_eq!(out.status.code(), Some(1), "{out:?}");
    let f = Fixture::new();
    let leaf = f.mirror("https://example.invalid/repo");
    fs::create_dir_all(leaf.parent().unwrap()).unwrap();
    fs::write(&leaf, b"do not repair").unwrap();
    let out = f.run(&["changelog", "--yes", "alpha"], b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(String::from_utf8_lossy(&out.stdout).contains("not a real directory"));
    assert_eq!(fs::read(leaf).unwrap(), b"do not repair");
    assert!(!f.calls().iter().any(|s| s.starts_with("git:")));
}

#[test]
fn malformed_role_metadata_and_symlink_cache_leaves_never_start_git() {
    for role in [
        json!({"rev":"new"}),
        json!({"rev":7,"url":"repo"}),
        json!({"rev":"new","url":true}),
        json!({"rev":"new","url":""}),
        json!({"rev":"nul\u{0000}revision","url":"repo"}),
    ] {
        let f = Fixture::new();
        f.json("target-a", json!({"main":role}));
        f.json("deployed-a", json!({"main":"old"}));
        let out = f.run(&["changelog", "--yes", "alpha"], b"");
        assert_eq!(out.status.code(), Some(0), "{out:?}");
        assert!(
            String::from_utf8_lossy(&out.stdout).contains("changelog unavailable"),
            "{out:?}"
        );
        assert!(!f.calls().iter().any(|s| s.starts_with("git:")));
        assert!(!f.cache.exists());
    }
    let f = Fixture::new();
    let leaf = f.mirror("https://example.invalid/repo");
    fs::create_dir_all(leaf.parent().unwrap()).unwrap();
    symlink(&f.home, &leaf).unwrap();
    let out = f.run(&["changelog", "--yes", "alpha"], b"");
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(String::from_utf8_lossy(&out.stdout).contains("not a real directory"));
    assert!(fs::symlink_metadata(leaf).unwrap().file_type().is_symlink());
    assert!(!f.calls().iter().any(|s| s.starts_with("git:")));
}

#[test]
fn unset_xdg_uses_isolated_home_and_large_raw_logs_stream_without_new_transcripts() {
    let f = Fixture::new();
    let bytes = b"log bytes \xff\n".repeat(150000);
    fs::write(f.root.join("large-log"), &bytes).unwrap();
    let child = f
        .command(&["changelog", "--yes", "alpha"])
        .env_remove("XDG_CACHE_HOME")
        .spawn()
        .unwrap();
    // Read pipes concurrently for output larger than their native capacity.
    let out = child.wait_with_output().unwrap();
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert!(out.stdout.windows(bytes.len()).any(|part| part == bytes));
    assert!(f.home.join(".cache/confctl/git-mirrors").exists());
    assert!(!f.cache.exists());
    f.assert_unchanged();
}

#[test]
fn cancellation_joins_native_git_and_stops_later_roles_and_hosts() {
    let f = Fixture::new();
    fs::write(f.root.join("git-slow"), b"").unwrap();
    let mut child = f
        .command(&["changelog", "--yes", "--tag=chosen"])
        .spawn()
        .unwrap();
    drop(child.stdin.take());
    let end = Instant::now() + Duration::from_secs(10);
    let pid = loop {
        if let Ok(text) = fs::read_to_string(f.root.join("git-pid"))
            && let Ok(pid) = text.trim().parse::<i32>()
            && pid > 1
        {
            break pid;
        }
        if child.try_wait().unwrap().is_some() {
            panic!(
                "early comparison exit: {:?}",
                child.wait_with_output().unwrap()
            );
        }
        assert!(Instant::now() < end);
        thread::sleep(Duration::from_millis(10));
    };
    nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(child.id() as i32),
        nix::sys::signal::Signal::SIGINT,
    )
    .unwrap();
    let out = wait(child);
    assert_eq!(out.status.code(), Some(130), "{out:?}");
    assert!(!Path::new(&format!("/proc/{pid}")).exists());
    assert!(!f.calls().iter().any(|s| s.starts_with("ssh:")));
    f.assert_unchanged();
}

#[path = "changes_command/pty.rs"]
mod pty;
