//! Exercise the actual Rust binary with a temporary local configuration.
use serde_json::{Value, json};
use std::{
    collections::BTreeMap,
    fs,
    os::unix::fs::{PermissionsExt, symlink},
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
    time::{Duration, Instant},
};

struct Fixture {
    scratch: tempfile::TempDir,
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
        fs::write(root.join("flake.nix"), "{ outputs = _: {}; }\n").unwrap();
        fs::write(root.join("machines.json"), serde_json::to_vec(&json!({
            "a": {"name":"nodes/alpha","key":"a-key","metaConfig":{
                "managed":true,"spin":"nixos","host":{"target":"localhost"},"tags":["chosen"]}},
            "b": {"name":"beta","key":"b-key","metaConfig":{
                "managed":false,"spin":"nixos","host":{"target":"beta.invalid"},"tags":["chosen"]}},
            "c": {"name":"carrier#guest","key":"c-key","carrier":"carrier","clusterName":"nested/guest","alias":"alias/guest 'literal'","metaConfig":{
                "managed":true,"spin":"vpsadminos","host":{"target":null},"tags":["other"]}},
            "d": {"name":"carrier","metaConfig":{"managed":true,"spin":"nixos","host":{"target":"carrier.invalid","port":2222},"tags":["carrier"]}}
        })).unwrap()).unwrap();
        executable(
            &bin.join("nix"),
            r#"#!/bin/sh
printf '%s\n' "$*" >> "$GENERATION_FIXTURE_ROOT/nix-calls"
if [ "$GENERATION_FIXTURE_WAIT" = 1 ]; then
  : > "$GENERATION_FIXTURE_ROOT/nix-started"
  exec sleep 30
fi
last=
for arg do last="$arg"; done
case "$last" in
  '.#confctl.settings') printf '%s\n' '{"nix":{"maxJobs":"auto","impureEval":false,"legacyNixPath":false}}' ;;
  '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$GENERATION_FIXTURE_ROOT" ;;
  *) printf 'unexpected Nix target: %s\n' "$last" >&2; exit 97 ;;
esac
"#,
        );
        for tool in [
            "logname",
            "ssh",
            "nix-store",
            "nix-env",
            "nix-collect-garbage",
        ] {
            executable(
                &bin.join(tool),
                "#!/bin/sh\nprintf 'forbidden tool\n' >&2\nexit 98\n",
            );
        }
        Self { scratch, root, bin }
    }
    fn host(&self, host: &str) -> PathBuf {
        self.root
            .join(".confctl/generations")
            .join(host.replace('/', ":"))
    }
    fn save(&self, host: &str, name: &str, date: &str, info: Value) {
        let dir = self.host(host).join(name);
        let system = self
            .scratch
            .path()
            .join("systems")
            .join(host.replace('/', ":"))
            .join(name);
        fs::create_dir_all(&dir).unwrap();
        fs::create_dir_all(&system).unwrap();
        symlink("/nix/store/abc-linux-6.12.3/bzImage", system.join("kernel")).unwrap();
        fs::write(
            dir.join("generation.json"),
            serde_json::to_vec_pretty(&json!({
                "mode":"flakes","date":date,"toplevel":system,"inputs":{},"inputsInfo":info,
                "unknown":{"saved":[false,7]}
            }))
            .unwrap(),
        )
        .unwrap();
        symlink(&system, dir.join("toplevel")).unwrap();
    }
    fn populated() -> Self {
        let f = Self::new();
        f.save(
            "nodes/alpha",
            "z-old",
            "2000-01-01T00:00:00Z",
            json!({"zrole":{"short_rev":"z-short","future":[7]},"nixpkgs":"1234567890"}),
        );
        f.save(
            "nodes/alpha",
            "a-current",
            "2001-01-01T00:00:00Z",
            json!({"nixpkgs":{"rev":"abcdef012345"}}),
        );
        symlink("a-current", f.host("nodes/alpha").join("current")).unwrap();
        f.save(
            "beta",
            "b-unmanaged",
            "2002-01-01T00:00:00Z",
            json!({"arole":{"shortRev":"a-short"}}),
        );
        f.save(
            "carrier#guest",
            "c-carried",
            "2003-01-01T00:00:00Z",
            json!({}),
        );
        f
    }
    fn enable_remote(&self) {
        let cat = std::env::split_paths(&std::env::var_os("PATH").unwrap())
            .map(|p| p.join("cat"))
            .find(|p| p.is_file())
            .unwrap();
        fs::create_dir(self.root.join("native-tools")).unwrap();
        symlink(cat, self.root.join("native-tools/cat")).unwrap();
        executable(
            &self.bin.join("cat"),
            r#"#!/bin/sh
[ "$*" = '/proc/uptime' ] || exit 97
printf '123.5 0\n'
"#,
        );
        executable(
            &self.bin.join("bash"),
            r#"#!/bin/sh
printf '%s\n' "$*" >> "$GENERATION_FIXTURE_ROOT/bash-args"
[ "$*" = '--norc' ] || exit 97
"$GENERATION_FIXTURE_ROOT/native-tools/cat" > "$GENERATION_FIXTURE_ROOT/local-script"
exec "$GENERATION_FIXTURE_ROOT/native-tools/cat" "$GENERATION_FIXTURE_ROOT/local-profile"
"#,
        );
        executable(
            &self.bin.join("ssh"),
            r#"#!/bin/sh
last=
for arg do
  printf '%s\n' "$arg" >> "$GENERATION_FIXTURE_ROOT/ssh-args"
  last="$arg"
done
if [ -e "$GENERATION_FIXTURE_ROOT/ssh-unavailable" ]; then
  printf 'fixture SSH unavailable\n' >&2
  exit 255
fi
case "$last" in
  'cat /proc/uptime') printf '456.5 0\n' ;;
  'bash --norc')
    "$GENERATION_FIXTURE_ROOT/native-tools/cat" > "$GENERATION_FIXTURE_ROOT/remote-script"
    if [ -e "$GENERATION_FIXTURE_ROOT/ssh-profile-failure" ]; then
      printf 'profile is missing\n' >&2
      exit 1
    fi
    if [ -e "$GENERATION_FIXTURE_ROOT/ssh-profile-wait" ]; then
      : > "$GENERATION_FIXTURE_ROOT/profile-started"
      exec sleep 30
    fi
    exec "$GENERATION_FIXTURE_ROOT/native-tools/cat" "$GENERATION_FIXTURE_ROOT/remote-profile"
    ;;
  *) printf 'unexpected remote command\n' >&2; exit 97 ;;
esac
"#,
        );
        fs::write(self.root.join("local-profile"),"/remote/local\n/nix/var/nix/profiles/system-3-link;/remote/local;1700000000;/nix/store/hash-linux-6.12/bzImage\n").unwrap();
        fs::write(self.root.join("remote-profile"),"/remote/beta\n/nix/var/nix/profiles/system-8-link;/remote/beta;1700000000;/nix/store/hash-linux-6.13/bzImage\n").unwrap();
    }
    fn command(&self, args: &[&str]) -> Command {
        let inherited = std::env::var_os("PATH").unwrap_or_default();
        let path = std::env::join_paths(
            std::iter::once(self.bin.clone()).chain(std::env::split_paths(&inherited)),
        )
        .unwrap();
        let mut command = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        command
            .arg("--color=never")
            .args(args)
            .current_dir(&self.root)
            .env("PATH", path)
            .env("GENERATION_FIXTURE_ROOT", &self.root)
            .env("CONFCTL_LOGNAME", self.bin.join("logname"))
            .env("CONFCTL_SSH_CONFIG", self.root.join("ssh config"))
            .env_remove("GENERATION_FIXTURE_WAIT")
            .env("TZ", "UTC")
            .env("NO_COLOR", "1")
            .stdin(Stdio::null());
        command
    }
    fn run(&self, args: &[&str]) -> Output {
        self.command(args).output().unwrap()
    }
    fn list(&self, args: &[&str]) -> Output {
        let mut all = vec!["generation", "ls"];
        all.extend_from_slice(args);
        self.run(&all)
    }
    fn preserved(&self, before: &BTreeMap<PathBuf, Vec<u8>>) {
        assert_eq!(&snapshot(&self.root.join(".confctl/generations")), before);
        assert!(!self.root.join(".confctl/mutation.lock").exists());
        let state = fs::read_dir(self.root.join(".confctl")).unwrap();
        for entry in state {
            let name = entry.unwrap().file_name();
            assert!(
                name == "generations" || name == "logs",
                "unexpected state {name:?}"
            );
        }
    }
}
fn executable(path: &Path, source: &str) {
    fs::write(path, source).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}
fn snapshot(root: &Path) -> BTreeMap<PathBuf, Vec<u8>> {
    fn walk(path: &Path, result: &mut BTreeMap<PathBuf, Vec<u8>>) {
        let metadata = fs::symlink_metadata(path).unwrap();
        if metadata.file_type().is_symlink() {
            result.insert(
                path.into(),
                fs::read_link(path)
                    .unwrap()
                    .as_os_str()
                    .as_encoded_bytes()
                    .to_vec(),
            );
        } else if metadata.is_file() {
            result.insert(path.into(), fs::read(path).unwrap());
        } else {
            result.insert(path.into(), Vec::new());
            for entry in fs::read_dir(path).unwrap() {
                walk(&entry.unwrap().path(), result);
            }
        }
    }
    let mut result = BTreeMap::new();
    if root.exists() {
        walk(root, &mut result);
    }
    result
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
fn names(output: &Output) -> Vec<String> {
    success(output)
        .lines()
        .skip_while(|line| !line.starts_with("HOST"))
        .skip(1)
        .filter(|line| !line.trim().is_empty())
        .map(|line| line.split_whitespace().nth(1).unwrap().to_owned())
        .collect()
}

#[test]
fn actual_local_route_lists_all_hosts_columns_and_preserves_records() {
    let f = Fixture::populated();
    let before = snapshot(&f.root.join(".confctl/generations"));
    let output = f.list(&[]);
    let text = success(&output);
    assert_eq!(
        names(&output),
        ["a-current", "b-unmanaged", "c-carried", "z-old"]
    );
    assert_eq!(
        text.lines()
            .next()
            .unwrap()
            .split_whitespace()
            .collect::<Vec<_>>(),
        [
            "HOST", "NAME", "ID", "PRESENCE", "CURRENT", "KERNEL", "ZROLE", "NIXPKGS", "AROLE"
        ]
    );
    for value in [
        "nodes/alpha",
        "beta",
        "carrier#guest",
        "6.12.3",
        "z-short",
        "12345678",
        "abcdef01",
        "a-short",
    ] {
        assert!(text.contains(value), "missing {value}: {text}");
    }
    assert_eq!(names(&f.list(&["--local"])), names(&output));
    let calls = fs::read_to_string(f.root.join("nix-calls")).unwrap();
    assert!(!calls.contains("--show-trace"));
    assert!(
        calls
            .lines()
            .all(|line| line.contains(".#confctl.settings")
                || line.contains(".#confctl.machinesJson"))
    );
    f.preserved(&before);
}

#[test]
fn actual_selectors_use_flattened_inventory_order_then_display_sort() {
    let f = Fixture::populated();
    let huge = "99999999999999999999999999999999999999999999999999999";
    for (selector, expected) in [
        ("0", vec!["c-carried"]),
        ("000", vec!["c-carried"]),
        ("-0", vec!["c-carried"]),
        ("1", vec!["z-old"]),
        ("0002", vec!["a-current"]),
        ("-1", vec!["b-unmanaged"]),
        ("-0002", vec!["a-current"]),
        ("-3", vec!["z-old"]),
        ("-4", vec![]),
        ("5", vec![]),
        (huge, vec![]),
        ("current", vec!["a-current", "b-unmanaged", "c-carried"]),
        ("old", vec!["z-old"]),
        (
            "*-*",
            vec!["a-current", "b-unmanaged", "c-carried", "z-old"],
        ),
        ("1d", vec!["a-current", "b-unmanaged", "c-carried", "z-old"]),
        ("999999999999999999999999999999999999d", vec![]),
    ] {
        assert_eq!(names(&f.list(&["*", selector])), expected, "{selector}");
    }
    for (selector, expected) in [
        ("c-key", vec!["c-carried"]),
        ("beta", vec!["b-unmanaged"]),
        ("nodes/*", vec!["a-current", "z-old"]),
    ] {
        assert_eq!(names(&f.list(&[selector])), expected, "{selector}");
    }
    assert_eq!(
        names(&f.list(&["--tag=chosen", "--attr=managed=false"])),
        ["b-unmanaged"]
    );
}

#[test]
fn original_numeric_spelling_is_the_out_of_range_pattern() {
    for name in [
        "0009",
        "-0009",
        "999999999999999999999999999999999999999",
        "+1",
    ] {
        let f = Fixture::new();
        f.save("beta", name, "2000-01-01T00:00:00Z", json!({}));
        assert_eq!(names(&f.list(&["beta", name])), [name]);
    }
}

#[test]
fn exclusions_and_unresolved_current_are_read_only_and_fail_unsafe_selections() {
    let f = Fixture::populated();
    let bad = f.host("nodes/alpha").join("bad");
    fs::create_dir(&bad).unwrap();
    fs::write(bad.join("generation.json"), b"{not JSON\n").unwrap();
    fs::remove_file(f.host("nodes/alpha").join("current")).unwrap();
    symlink("bad", f.host("nodes/alpha").join("current")).unwrap();
    let before = snapshot(&f.root.join(".confctl/generations"));
    let output = f.list(&[]);
    assert_eq!(
        names(&output),
        ["a-current", "b-unmanaged", "c-carried", "z-old"]
    );
    let diagnostic = String::from_utf8_lossy(&output.stderr);
    assert!(
        diagnostic.contains("Skipping generation") && diagnostic.contains("bad/generation.json")
    );
    for selector in [
        "0",
        "999999999999999999999999999999",
        "bad",
        "current",
        "old",
    ] {
        let output = f.list(&["*", selector]);
        assert!(!output.status.success(), "{selector}");
        assert!(!String::from_utf8_lossy(&output.stderr).contains("not implemented yet"));
    }
    assert_eq!(names(&f.list(&["beta", "0"])), ["b-unmanaged"]);
    f.preserved(&before);
}

#[test]
fn empty_local_success_does_not_create_generation_storage() {
    let f = Fixture::new();
    assert!(names(&f.list(&[])).is_empty());
    assert!(names(&f.list(&["absent*"])).is_empty());
    assert!(!f.root.join(".confctl/generations").exists());
}

#[test]
fn actual_remote_profiles_use_local_or_root_ssh_without_local_record_reads() {
    let f = Fixture::new();
    f.enable_remote();
    // A remote-only request must never attempt the local generation reader.
    fs::create_dir_all(f.root.join(".confctl/generations")).unwrap();
    fs::write(f.host("beta"), "invalid local path").unwrap();
    let before = snapshot(&f.root.join(".confctl/generations"));
    for host in ["a-key", "b-key"] {
        let output = f.list(&["--remote", host]);
        let text = success(&output);
        assert_eq!(
            names(&output),
            ["2023-11-14--22-13-20"],
            "stdout={text}\nstderr={}",
            String::from_utf8_lossy(&output.stderr)
        );
        assert!(text.contains("host") && text.contains("6.1"));
    }
    assert_eq!(
        fs::read_to_string(f.root.join("bash-args")).unwrap(),
        "--norc\n"
    );
    let args = fs::read_to_string(f.root.join("ssh-args")).unwrap();
    assert!(args.contains("-F\n") && args.contains("ssh config\n") && args.contains("-l\nroot\n"));
    assert!(
        args.contains("beta.invalid\ncat /proc/uptime\n")
            && args.contains("beta.invalid\nbash --norc\n")
    );
    assert!(!args.contains("Timeout") && !args.contains("realpath"));
    let script = fs::read_to_string(f.root.join("remote-script")).unwrap();
    assert!(script.contains("realpath -e --") && script.contains("/nix/var/nix/profiles/system"));
    assert!(!script.contains("chmod") && !script.contains("nix-env"));
    // Queries may finish in any order; selectors still see host selection order.
    assert_eq!(
        names(&f.list(&["--remote", "--tag=chosen", "*", "1"])),
        ["2023-11-14--22-13-20"]
    );
    let output = f.list(&["--remote", "--tag=chosen", "*", "0"]);
    assert!(
        success(&output)
            .lines()
            .any(|line| line.starts_with("beta ") && line.contains("2023-11-14"))
    );
    f.preserved(&before);
}

#[test]
fn carried_remote_profiles_use_exact_carrier_target_port_and_quoted_alias() {
    let f = Fixture::new();
    f.enable_remote();
    let profile = "/nix/var/nix/profiles/confctl-alias:guest 'literal'";
    fs::write(f.root.join("remote-profile"),format!("/remote/guest\n{profile}-2-link;/remote/guest;1700000000;/nix/store/hash-linux-6.14/bzImage\nwrong-link;/ignore;0;\n")).unwrap();
    let output = f.list(&["--remote", "c-key"]);
    let text = success(&output);
    assert!(
        text.contains("carrier#guest") && text.contains("6.14"),
        "stdout={text}\nstderr={}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(
        String::from_utf8_lossy(&output.stderr)
            .contains("Invalid profile generation link 'wrong-link'")
    );
    let args = fs::read_to_string(f.root.join("ssh-args")).unwrap();
    assert!(args.contains("-p\n2222\ncarrier.invalid\n"));
    assert!(!args.contains("guest\n"));
    let script = fs::read_to_string(f.root.join("remote-script")).unwrap();
    assert!(script.contains("confctl-alias:guest"));
    assert!(script.contains("\\'") || script.contains("'\\''"));
    assert!(!f.root.join(".confctl/generations").exists());
}

#[test]
fn combined_remote_first_unification_keeps_local_names_metadata_and_distinct_builds() {
    let f = Fixture::populated();
    f.enable_remote();
    let record = f.host("nodes/alpha").join("a-current/generation.json");
    let value: Value = serde_json::from_slice(&fs::read(record).unwrap()).unwrap();
    let top = value["toplevel"].as_str().unwrap();
    fs::write(f.root.join("local-profile"),format!("{top}\n/nix/var/nix/profiles/system-8-link;{top};1700000000;/nix/store/hash-linux-remote-first/bzImage\n/nix/var/nix/profiles/system-10-link;{top};1700000001;/nix/store/hash-linux-remote-later/bzImage\n")).unwrap();
    let before = snapshot(&f.root.join(".confctl/generations"));
    let output = f.list(&["--local", "--remote", "a-key"]);
    let text = success(&output);
    assert_eq!(names(&output), ["a-current", "z-old"]);
    let merged = text
        .lines()
        .find(|line| line.contains("a-current"))
        .unwrap();
    assert!(
        merged.contains("10")
            && merged.contains("build+host")
            && merged.contains("remote-first")
            && merged.contains("abcdef01"),
        "stdout={text}\nstderr={}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        names(&f.list(&["--local", "--remote", "a-key", "1"])),
        ["a-current"]
    );
    assert_eq!(
        names(&f.list(&["--local", "--remote", "a-key", "0"])),
        ["z-old"]
    );
    f.preserved(&before);
}

#[test]
fn unavailable_profiles_have_failed_tasks_and_diagnostics_but_listing_succeeds() {
    let f = Fixture::new();
    f.enable_remote();
    fs::write(f.root.join("ssh-unavailable"), "").unwrap();
    let output = f.list(&["--remote", "b-key"]);
    assert!(names(&output).is_empty());
    let diagnostic = String::from_utf8_lossy(&output.stderr);
    assert!(
        diagnostic.contains("uptime for beta")
            && diagnostic.contains("255")
            && diagnostic.contains("fixture SSH unavailable")
    );
    assert!(!f.root.join("remote-script").exists());
    fs::remove_file(f.root.join("ssh-unavailable")).unwrap();
    fs::write(f.root.join("ssh-profile-failure"), "").unwrap();
    let output = f.list(&["--remote", "b-key"]);
    assert!(names(&output).is_empty());
    assert!(String::from_utf8_lossy(&output.stderr).contains("profile generations for beta"));
    assert!(!f.root.join(".confctl/generations").exists());
}

#[test]
fn remote_profile_cancellation_joins_active_query_without_touching_local_records() {
    use nix::{
        sys::signal::{Signal, kill},
        unistd::Pid,
    };
    let f = Fixture::populated();
    f.enable_remote();
    fs::write(f.root.join("ssh-profile-wait"), "").unwrap();
    let before = snapshot(&f.root.join(".confctl/generations"));
    let mut child = f
        .command(&["generation", "ls", "--remote", "b-key"])
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while !f.root.join("profile-started").exists() {
        if Instant::now() >= deadline {
            child.kill().unwrap();
            let _ = child.wait();
            panic!("profile query did not start");
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
            panic!("profile query did not join after cancellation");
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    let output = child.wait_with_output().unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("canceled"));
    f.preserved(&before);
}

#[test]
fn remote_output_failure_and_missing_carrier_are_not_successful_empty_lists() {
    let f = Fixture::populated();
    f.enable_remote();
    let before = snapshot(&f.root.join(".confctl/generations"));
    let output = f
        .command(&["generation", "ls", "--remote", "b-key"])
        .stdout(fs::File::create("/dev/full").unwrap())
        .output()
        .unwrap();
    assert!(!output.status.success());
    f.preserved(&before);
    let path = f.root.join("machines.json");
    let mut machines: Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    machines.as_object_mut().unwrap().remove("d");
    fs::write(path, serde_json::to_vec(&machines).unwrap()).unwrap();
    let output = f.list(&["--remote", "c-key"]);
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("Carrier carrier not found"));
    f.preserved(&before);
}

#[test]
fn generation_directory_io_and_output_failures_do_not_report_success() {
    let f = Fixture::populated();
    let before = snapshot(&f.root.join(".confctl/generations"));
    let output = f
        .command(&["generation", "ls"])
        .stdout(fs::File::create("/dev/full").unwrap())
        .output()
        .unwrap();
    assert!(!output.status.success());
    f.preserved(&before);

    let f = Fixture::new();
    fs::create_dir_all(f.host("nodes/alpha")).unwrap();
    fs::write(f.host("beta"), "not a directory\n").unwrap();
    let before = snapshot(&f.root.join(".confctl/generations"));
    let output = f.list(&[]);
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("list generation directory"));
    f.preserved(&before);
}

#[test]
fn cancellation_during_inventory_query_fails_and_preserves_local_generations() {
    use nix::{
        sys::signal::{Signal, kill},
        unistd::Pid,
    };
    let f = Fixture::populated();
    let before = snapshot(&f.root.join(".confctl/generations"));
    let mut child = f
        .command(&["generation", "ls"])
        .env("GENERATION_FIXTURE_WAIT", "1")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while !f.root.join("nix-started").exists() {
        if Instant::now() >= deadline {
            child.kill().unwrap();
            let _ = child.wait();
            panic!("inventory fixture did not start");
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
            panic!("generation listing did not stop after SIGINT");
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    let output = child.wait_with_output().unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("canceled"));
    f.preserved(&before);
}

#[test]
fn cancellation_during_blocked_table_delivery_fails_after_output_drains() {
    use nix::{
        sys::signal::{Signal, kill},
        unistd::Pid,
    };
    use rustix::{
        event::{PollFd, PollFlags, Timespec, poll},
        pipe::{fcntl_setpipe_size, pipe},
    };
    use std::io::Read;

    let f = Fixture::new();
    let revision = "r".repeat(64 * 1024);
    f.save(
        "beta",
        "saved",
        "2000-01-01T00:00:00Z",
        json!({"nixpkgs":{"shortRev":revision}}),
    );
    let before = snapshot(&f.root.join(".confctl/generations"));
    let (read, write) = pipe().unwrap();
    assert!(fcntl_setpipe_size(&read, 4096).unwrap() < revision.len());
    let mut child = f
        .command(&["generation", "ls", "beta"])
        .stdout(Stdio::from(write))
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let mut stdout = fs::File::from(read);
    let mut ready = [PollFd::new(&stdout, PollFlags::IN)];
    assert_eq!(
        poll(
            &mut ready,
            Some(&Timespec {
                tv_sec: 5,
                tv_nsec: 0
            })
        )
        .unwrap(),
        1
    );
    let mut prefix = [0; 4];
    stdout.read_exact(&mut prefix).unwrap();
    assert_eq!(&prefix, b"HOST");
    assert!(child.try_wait().unwrap().is_none());
    kill(
        Pid::from_raw(child.id().try_into().unwrap()),
        Signal::SIGINT,
    )
    .unwrap();
    // Keep output blocked while the application's signal owner handles SIGINT.
    std::thread::sleep(Duration::from_millis(100));
    assert!(child.try_wait().unwrap().is_none());
    let drain = std::thread::spawn(move || {
        let mut bytes = Vec::new();
        stdout.read_to_end(&mut bytes).unwrap();
        bytes
    });
    let deadline = Instant::now() + Duration::from_secs(5);
    while child.try_wait().unwrap().is_none() {
        if Instant::now() >= deadline {
            child.kill().unwrap();
            let _ = child.wait();
            panic!("generation listing did not finish after table output drained");
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    let output = child.wait_with_output().unwrap();
    let delivered = drain.join().unwrap();
    assert!(String::from_utf8(delivered).unwrap().contains(&revision));
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("canceled"));
    f.preserved(&before);
}

#[test]
fn missing_current_link_and_all_exclusion_kinds_leave_saved_bytes_unchanged() {
    let f = Fixture::populated();
    fs::remove_file(f.host("nodes/alpha").join("current")).unwrap();
    for (name, record) in [
        (
            "unsupported",
            json!({"mode":"swpins","date":"2004-01-01T00:00:00Z","toplevel":"/saved"}),
        ),
        (
            "invalid-date",
            json!({"mode":"flakes","date":"broken","toplevel":"/saved"}),
        ),
        (
            "missing-mode",
            json!({"date":"2004-01-01T00:00:00Z","toplevel":"/saved"}),
        ),
    ] {
        let dir = f.host("nodes/alpha").join(name);
        fs::create_dir(&dir).unwrap();
        fs::write(
            dir.join("generation.json"),
            serde_json::to_vec(&record).unwrap(),
        )
        .unwrap();
    }
    let before = snapshot(&f.root.join(".confctl/generations"));
    let output = f.list(&["nodes/alpha", "current"]);
    assert_eq!(names(&output), ["a-current"]);
    assert!(String::from_utf8_lossy(&output.stderr).contains("No current link"));
    for name in ["unsupported", "invalid-date", "missing-mode"] {
        let output = f.list(&["nodes/alpha", name]);
        assert!(!output.status.success());
        assert!(String::from_utf8_lossy(&output.stderr).contains("Cannot select generation"));
    }
    f.preserved(&before);
}
