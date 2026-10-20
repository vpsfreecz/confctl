//! Actual build command path with temporary configuration and fake Nix outputs.
use super::*;
use crate::{
    configuration::ConfigRoot,
    generation::{BuildGeneration, GcRoots},
};
use serde_json::{Value, json};
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
    time::Instant,
};
const ROLE: &str = "CONFCTL_BUILD_APP_ROLE";
const ROOT: &str = "CONFCTL_BUILD_APP_ROOT";
const FIXTURE: &str = "app::build_tests::actual_build_application_fixture";
#[test]
fn actual_build_application_fixture() {
    let Some(role) = std::env::var_os(ROLE) else {
        return;
    };
    let root = PathBuf::from(std::env::var_os(ROOT).unwrap());
    let config = ConfigRoot::new(&root).unwrap();
    crate::operations::set_build_test_roots(
        GcRoots::new(&config, root.join("roots"), "test").unwrap(),
    );
    let argv = if let Ok(args) = std::env::var("CONFCTL_BUILD_APP_ARGS") {
        serde_json::from_str::<Vec<String>>(&args).unwrap()
    } else {
        let mut argv = vec![
            "--color=never",
            "build",
            "--show-trace",
            "--max-jobs",
            "2",
            "--cores",
            "3",
        ];
        if role != "decline" {
            argv.push("--yes");
        }
        if role == "empty" {
            argv.push("absent*");
        }
        argv.into_iter().map(str::to_owned).collect()
    };
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let code = runtime.block_on(execute(
        argv.into_iter().map(Into::into).collect(),
        Timing::start(),
        &crate::operations::CoreOperations,
        Ok(root),
        Arc::new(Mutex::new(Box::new(io::stdout()))),
        Arc::new(Mutex::new(Box::new(io::stderr()))),
    ));
    std::process::exit(code);
}
struct Fixture {
    _scratch: tempfile::TempDir,
    root: PathBuf,
    fake: PathBuf,
    lock: Vec<u8>,
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let fake = scratch.path().join("fake");
        fs::create_dir_all(root.join("replies")).unwrap();
        fs::create_dir(&fake).unwrap();
        fs::write(root.join("flake.nix"), "{ outputs = _: {}; }\n").unwrap();
        let lock = b"{\"version\":7,\"nodes\":{\"root\":{\"inputs\":{}}}}\n".to_vec();
        fs::write(root.join("flake.lock"), &lock).unwrap();
        for name in ["reused", "new", "rollback", "input"] {
            fs::create_dir_all(root.join("store").join(name)).unwrap();
        }
        let input = root.join("store/input");
        let machines = json!({"a":{"name":"alpha","key":"alpha-key","metaConfig":{"managed":true,"spin":"vpsadminos","host":{"target":"alpha.invalid"}}},"b":{"name":"carrier#beta","flakeKey":"beta/key","carrier":"carrier","metaConfig":{"managed":true,"spin":"nixos","host":{"target":null}}},"skip":{"name":"unmanaged","metaConfig":{"managed":false,"spin":"vpsadminos"}}});
        let plan = json!({"alpha":{"machineKey":"alpha-key","inputs":{"nixpkgs":input}},"carrier#beta":{"key":"beta/key","inputs":{"nixpkgs":input}}});
        let outputs = json!({"alpha-key":{"toplevel":root.join("store/reused"),"autoRollback":root.join("store/rollback")},"beta/key":{"toplevel":root.join("store/new"),"autoRollback":root.join("store/rollback")}});
        let defaults = json!({"list":{"columns":["name","spin"]},"nix":{"maxJobs":4,"impureEval":false,"legacyNixPath":false}});
        let build_settings = json!({"nix":{"maxJobs":99,"impureEval":true,"legacyNixPath":false}});
        for (name, value) in [
            ("machines", machines),
            ("plan", plan),
            ("outputs", outputs),
            ("defaults", defaults),
            ("build-settings", build_settings),
            (
                "infos",
                json!({"beta/key":{"nixpkgs":{"rev":"a".repeat(40),"unknown":false}}}),
            ),
        ] {
            fs::write(
                root.join(format!("replies/{name}.json")),
                serde_json::to_vec(&value).unwrap(),
            )
            .unwrap();
        }
        let host = root.join(".confctl/generations/alpha");
        for name in ["2000-01-01--00-00-00", "2001-01-01--00-00-00"] {
            fs::create_dir_all(host.join(name)).unwrap();
            let generation = BuildGeneration {
                name: name.into(),
                date: chrono::DateTime::parse_from_rfc3339(&format!("{}T00:00:00Z", &name[..10]))
                    .unwrap(),
                toplevel: root.join("store/reused"),
                auto_rollback: Some(root.join("store/rollback")),
                inputs: serde_json::from_value(json!({"nixpkgs":input})).unwrap(),
                inputs_info: Default::default(),
            };
            fs::write(
                host.join(name).join("generation.json"),
                serde_json::to_vec_pretty(&generation.encode()).unwrap(),
            )
            .unwrap();
        }
        std::os::unix::fs::symlink("2001-01-01--00-00-00", host.join("current")).unwrap();
        let script = r#"#!/bin/sh
printf 'CALL\000' >> "$CONFCTL_BUILD_APP_ROOT/argv"
printf '%s\000' "$@" >> "$CONFCTL_BUILD_APP_ROOT/argv"
last=
for arg do last="$arg"; done
case "$last" in
 '.#confctl.settings')
  count=0
  if [ -f "$CONFCTL_BUILD_APP_ROOT/settings-count" ]; then read -r count < "$CONFCTL_BUILD_APP_ROOT/settings-count"; fi
  count=$((count+1)); printf '%s\n' "$count" > "$CONFCTL_BUILD_APP_ROOT/settings-count"
  if [ "$count" -lt 3 ]; then cat "$CONFCTL_BUILD_APP_ROOT/replies/defaults.json"; else cat "$CONFCTL_BUILD_APP_ROOT/replies/build-settings.json"; fi
  ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/replies/machines.json"}}]\n' "$CONFCTL_BUILD_APP_ROOT" ;;
 '.#confctl.buildPlan') cat "$CONFCTL_BUILD_APP_ROOT/replies/plan.json" ;;
 '.#confctl.build') cat "$CONFCTL_BUILD_APP_ROOT/replies/outputs.json" ;;
 '.#confctl.inputsInfo') cat "$CONFCTL_BUILD_APP_ROOT/replies/infos.json" ;;
 *)
  if [ "$1" != build ]; then printf 'unexpected Nix eval: %s\n' "$last" >&2; exit 97; fi
  printf "these 2 derivations will be built:\nthis path will be fetched (1 MiB)\nbuilding '/nix/store/fixture.drv'...\nunknown build log\n" >&2
  if [ "$CONFCTL_BUILD_APP_ROLE" = failure ]; then printf 'fixture build failed\n' >&2; exit 7; fi
  printf '[{"outputs":{"out":"not-the-selected-output"}}]\n'
 ;;
esac
"#;
        fs::write(fake.join("nix"), script).unwrap();
        fs::set_permissions(fake.join("nix"), fs::Permissions::from_mode(0o755)).unwrap();
        Self {
            _scratch: scratch,
            root,
            fake,
            lock,
        }
    }
    fn run(&self, role: &str) -> Output {
        self.run_args(role, None, if role == "decline" { b"n\n" } else { b"" })
    }
    fn command_args(&self, role: &str, args: Option<&[&str]>) -> Command {
        let inherited = std::env::var_os("PATH").unwrap_or_default();
        let path = std::env::join_paths(
            std::iter::once(self.fake.clone()).chain(std::env::split_paths(&inherited)),
        )
        .unwrap();
        let mut command = Command::new(std::env::current_exe().unwrap());
        if let Some(args) = args {
            command.env(
                "CONFCTL_BUILD_APP_ARGS",
                serde_json::to_string(args).unwrap(),
            );
        }
        command
            .args(["--exact", FIXTURE, "--nocapture"])
            .env(ROLE, role)
            .env(ROOT, &self.root)
            .env("PATH", path)
            .env("NIX_SSHOPTS", "-o Existing=yes")
            .env("CONFCTL_SSH_CONFIG", self.root.join("ssh config"))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        command
    }
    fn run_args(&self, role: &str, args: Option<&[&str]>, input: &[u8]) -> Output {
        let mut app = self.command_args(role, args).spawn().unwrap();
        app.stdin.take().unwrap().write_all(input).unwrap();
        let deadline = Instant::now() + Duration::from_secs(15);
        loop {
            if app.try_wait().unwrap().is_some() {
                return app.wait_with_output().unwrap();
            }
            if Instant::now() >= deadline {
                let _ = app.kill();
                let output = app.wait_with_output().unwrap();
                panic!("build command fixture watchdog: {output:?}");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }
    fn composed(&self, legacy: bool, refresh: bool) {
        let write = |name: &str, value: Value| {
            fs::write(
                self.root.join(format!("replies/{name}.json")),
                serde_json::to_vec(&value).unwrap(),
            )
            .unwrap()
        };
        let input = self.root.join("store/input");
        let second = self.root.join("store/second");
        fs::create_dir(&second).unwrap();
        fs::create_dir(self.root.join("store/refreshed")).unwrap();
        let mut machines: Value =
            serde_json::from_slice(&fs::read(self.root.join("replies/machines.json")).unwrap())
                .unwrap();
        for name in ["a", "b"] {
            machines[name]["metaConfig"]["healthChecks"] = if name == "a" {
                json!({"builderCommands":[{"command":["health-tool","{name}"],"timeout":0,"cooldown":0,"standardOutput":{"include":["ok"]}}]})
            } else {
                json!({"invalid":true})
            };
            machines[name]["metaConfig"]["host"]["port"] = 2222.into();
            machines[name]["metaConfig"]["autoRollback"] = json!({"enable":true,"timeout":30});
        }
        machines["0"] = json!({"name":"ahead","key":"ahead-key","metaConfig":{"managed":true,"spin":"nixos","host":{"target":"ahead.invalid"},"healthChecks":{"builderCommands":[{"command":["health-tool","{name}"],"timeout":0,"cooldown":0}]}}});
        machines["z"] = json!({"name":"zed","key":"zed-key","metaConfig":{"managed":true,"spin":"nixos","host":{"target":"zed.invalid"},"healthChecks":{"builderCommands":[{"command":["health-tool","{name}"],"timeout":0,"cooldown":0}]}}});
        machines["carrier"] = json!({"name":"carrier","metaConfig":{"managed":false,"host":{"target":"carrier.invalid","port":2233}}});
        for key in ["0", "z"] {
            machines[key]["metaConfig"]["autoRollback"] = json!({"enable":true,"timeout":30});
        }
        // Confirmed order differs from the group's reused-before-new return order.
        let mut ordered = serde_json::Map::new();
        let entries = machines.as_object_mut().unwrap();
        ordered.insert("0".into(), entries.remove("0").unwrap());
        ordered.extend(std::mem::take(entries));
        write("machines", Value::Object(ordered));
        let plan = json!({"ahead":{"key":"ahead-key","inputs":{"nixpkgs":input}},"alpha":{"key":"alpha-key","inputs":{"nixpkgs":input}},"carrier#beta":{"key":"beta/key","inputs":{"nixpkgs":input}},"zed":{"key":"zed-key","inputs":{"nixpkgs":second}}});
        write("plan", plan.clone());
        let mut refreshed = plan;
        refreshed["alpha"]["inputs"]["nixpkgs"] =
            self.root.join("store/refreshed").to_str().unwrap().into();
        write("plan-after", refreshed);
        let mut outputs: Value =
            serde_json::from_slice(&fs::read(self.root.join("replies/outputs.json")).unwrap())
                .unwrap();
        for name in ["ahead", "zed"] {
            fs::create_dir_all(self.root.join(format!("store/{name}/bin"))).unwrap();
            outputs[format!("{name}-key")] = json!({"toplevel":self.root.join(format!("store/{name}")),"autoRollback":self.root.join("store/rollback")});
        }
        write("outputs", outputs);
        write(
            "infos",
            json!({"alpha-key":{"nixpkgs":"CURRENT"},"ahead-key":{"nixpkgs":"CURRENT"},"beta/key":{"nixpkgs":"CURRENT"},"zed-key":{"nixpkgs":"CURRENT"}}),
        );
        write(
            "defaults",
            json!({"list":{"columns":["name"]},"nix":{"maxJobs":4,"impureEval":true,"legacyNixPath":legacy,"legacyNixPathMap":["nixpkgs"]},"runtimeKernels":{"enable":true,"refreshBeforeDeploy":refresh,"stateFile":"runtime-kernels.json"}}),
        );
        for saved in ["2000-01-01--00-00-00", "2001-01-01--00-00-00"] {
            let path = self.root.join(format!(
                ".confctl/generations/alpha/{saved}/generation.json"
            ));
            let mut value: Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
            value["inputs_info"] = json!({"nixpkgs":"OLD"});
            value["auto_rollback"] = self
                .root
                .join("store/old-rollback")
                .to_str()
                .unwrap()
                .into();
            fs::write(path, serde_json::to_vec(&value).unwrap()).unwrap();
        }
        for top in ["reused", "new", "ahead", "zed"] {
            fs::create_dir_all(self.root.join(format!("store/{top}/bin"))).unwrap();
            native(
                &self
                    .root
                    .join(format!("store/{top}/bin/switch-to-configuration")),
                r#"printf '%s\0' "$NB_HOST" "$@" > "$CONFCTL_BUILD_APP_ROOT/activate-$$"
printf 'activate %s %s\n' "$NB_HOST" "$1" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
if [ "$CONFCTL_BUILD_APP_ROLE" = activation-fail ]; then exit 8; fi"#,
            );
        }
        fs::remove_dir(self.root.join("store/rollback")).unwrap();
        for helper in ["rollback", "old-rollback"] {
            native(
                &self.root.join(format!("store/{helper}")),
                r#"printf '%s\0' "$NB_HOST" "$0" "$@" > "$CONFCTL_BUILD_APP_ROOT/helper-$$"
printf 'helper-start %s\n' "$NB_HOST" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
"$3/bin/switch-to-configuration" "$4"
path="$CONFCTL_BUILD_APP_ROOT/confirm-${5##*/}"
printf switched > "$path"
while :; do state=; read -r state < "$path"; [ "$state" != confirmed ] || break; /run/current-system/sw/bin/sleep 0.01; done
printf 'helper-end %s\n' "$NB_HOST" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
if [ "$CONFCTL_BUILD_APP_ROLE" = helper-fail ]; then exit 9; fi"#,
            );
        }
        native(
            &self.fake.join("nix"),
            r#"printf '%s\0' "$@" > "$CONFCTL_BUILD_APP_ROOT/nix-call-$$"
last=; for arg do last=$arg; done
case "$last" in
 '.#confctl.settings') exec /run/current-system/sw/bin/cat "$CONFCTL_BUILD_APP_ROOT/replies/defaults.json" ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/replies/machines.json"}}]\n' "$CONFCTL_BUILD_APP_ROOT" ;;
 '.#confctl.buildPlan')
  file=plan; if [ -e "$CONFCTL_BUILD_APP_ROOT/runtime-kernels.json" ]; then file=plan-after; fi
  printf 'plan %s\n' "$file" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
  exec /run/current-system/sw/bin/cat "$CONFCTL_BUILD_APP_ROOT/replies/$file.json" ;;
 '.#confctl.machineKeys') printf '{"ahead":"ahead-key","alpha":"alpha-key","carrier#beta":"beta/key","zed":"zed-key"}\n' ;;
 '.#confctl.build') exec /run/current-system/sw/bin/cat "$CONFCTL_BUILD_APP_ROOT/replies/outputs.json" ;;
 '.#confctl.inputsInfo') exec /run/current-system/sw/bin/cat "$CONFCTL_BUILD_APP_ROOT/replies/infos.json" ;;
 '.#confctl.inputsInfo.'*) printf 'current-inputs %s\n' "$last" >> "$CONFCTL_BUILD_APP_ROOT/sequence"; printf '{"nixpkgs":"CURRENT"}\n' ;;
 '.#confctl.inputs')
  input=input; if [ -e "$CONFCTL_BUILD_APP_ROOT/runtime-kernels.json" ]; then input=refreshed; fi
  printf '{"ahead-key":{"nixpkgs":"%s/store/input"},"alpha-key":{"nixpkgs":"%s/store/%s"},"beta/key":{"nixpkgs":"%s/store/input"},"zed-key":{"nixpkgs":"%s/store/second"}}\n' "$CONFCTL_BUILD_APP_ROOT" "$CONFCTL_BUILD_APP_ROOT" "$input" "$CONFCTL_BUILD_APP_ROOT" "$CONFCTL_BUILD_APP_ROOT" ;;
 *)
  [ "$1" = build ] || exit 97
  n=0; if [ -f "$CONFCTL_BUILD_APP_ROOT/build-count" ]; then read -r n < "$CONFCTL_BUILD_APP_ROOT/build-count"; fi
  n=$((n+1)); printf '%s\n' "$n" > "$CONFCTL_BUILD_APP_ROOT/build-count"
  printf 'build %s\n' "$n" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
  if [ "$CONFCTL_BUILD_APP_ROLE" = later-fail ] && [ "$n" = 2 ]; then exit 7; fi
  if [ "$CONFCTL_BUILD_APP_ROLE" = slow-build ]; then printf '%s' "$$" > "$CONFCTL_BUILD_APP_ROOT/build-pid"; exec /run/current-system/sw/bin/sleep 60; fi
  printf "building '/nix/store/fake.drv'...\n" >&2
  printf '[{"outputs":{"out":"not-selected"}}]\n' ;;
esac"#,
        );
        native(
            &self.fake.join("ssh"),
            r#"printf '%s\0' "$@" > "$CONFCTL_BUILD_APP_ROOT/ssh-call-$$"
while [ "$#" -gt 0 ]; do case "$1" in -F|-l|-p) shift; shift ;; *) break ;; esac; done
NB_HOST=${1%.invalid}; export NB_HOST; shift
[ "$#" = 1 ] || exit 97
printf 'observe-or-command %s %s\n' "$NB_HOST" "$1" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
exec /bin/sh -c "$1""#,
        );
        native(
            &self.fake.join("sh"),
            r#"case "$2" in
 'cat /run/confctl-confirm-'*' 2>/dev/null') path=${2#cat }; path=${path% 2>/dev/null}; exec /run/current-system/sw/bin/cat "$CONFCTL_BUILD_APP_ROOT/confirm-${path##*/}" ;;
 'echo confirmed > /run/confctl-confirm-'*) path=${2#echo confirmed > }; printf 'confirmed\n' > "$CONFCTL_BUILD_APP_ROOT/confirm-${path##*/}"; printf 'confirm %s\n' "$NB_HOST" >> "$CONFCTL_BUILD_APP_ROOT/sequence" ;;
 *) exec /bin/sh "$@" ;;
esac"#,
        );
        native(
            &self.fake.join("cat"),
            r#"case "$1" in
 /proc/uptime) printf '1 0\n' ;;
 /etc/confctl/inputs-info.json) printf '{"nixpkgs":"CURRENT"}\n' ;;
 *machine.json) printf '{"inputs-info":{"nixpkgs":"CURRENT"}}\n' ;;
 *) exec /run/current-system/sw/bin/cat "$@" ;;
esac"#,
        );
        native(
            &self.fake.join("uname"),
            "printf 'refresh %s\\n' \"$NB_HOST\" >> \"$CONFCTL_BUILD_APP_ROOT/sequence\"\nprintf '6.12-fixture\\n'",
        );
        native(
            &self.fake.join("realpath"),
            r#"if [ "$CONFCTL_BUILD_APP_ROLE" = status-current ]; then printf '%s/store/reused\n' "$CONFCTL_BUILD_APP_ROOT"; else printf '/other-running\n'; fi"#,
        );
        native(
            &self.fake.join("bash"),
            r#"/run/current-system/sw/bin/cat > "$CONFCTL_BUILD_APP_ROOT/observed-profile-script-$$"
printf '/other-profile\n'"#,
        );
        native(
            &self.fake.join("nix-copy-closure"),
            r#"printf '%s\0' "$NIX_SSHOPTS" "$@" > "$CONFCTL_BUILD_APP_ROOT/copy-$$"
printf 'copy %s\n' "$2" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
if [ "$CONFCTL_BUILD_APP_ROLE" = copy-fail ]; then exit 7; fi"#,
        );
        native(
            &self.fake.join("nix-env"),
            r#"printf '%s\0' "$NB_HOST" "$@" > "$CONFCTL_BUILD_APP_ROOT/profile-$$"
printf 'profile %s\n' "$NB_HOST" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
if [ "$CONFCTL_BUILD_APP_ROLE" = profile-fail ]; then exit 7; fi"#,
        );
        native(
            &self.fake.join("carrier-env"),
            r#"printf '%s\0' "$NB_HOST" "$@" > "$CONFCTL_BUILD_APP_ROOT/carrier-$$"
printf 'carrier-profile %s\n' "$NB_HOST" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
if [ "$CONFCTL_BUILD_APP_ROLE" = carrier-fail ]; then exit 7; fi"#,
        );
        native(
            &self.fake.join("health-tool"),
            r#"printf 'health %s\n' "$1" >> "$CONFCTL_BUILD_APP_ROOT/sequence"
if [ "$CONFCTL_BUILD_APP_ROLE" = health-fail ]; then exit 7; fi
printf 'ok\n'"#,
        );
        for tool in ["reboot", "nix-store", "nix-collect-garbage", "logname"] {
            native(
                &self.fake.join(tool),
                "printf forbidden > \"$CONFCTL_BUILD_APP_ROOT/forbidden\"\nexit 98",
            );
        }
    }
    fn sequence(&self) -> Vec<String> {
        fs::read_to_string(self.root.join("sequence"))
            .unwrap_or_default()
            .lines()
            .map(str::to_owned)
            .collect()
    }
    fn native_records(&self, prefix: &str) -> Vec<Vec<Vec<u8>>> {
        let raw = fs::read_dir(&self.root)
            .unwrap()
            .map(|entry| entry.unwrap().path())
            .filter(|path| {
                path.file_name()
                    .unwrap()
                    .as_encoded_bytes()
                    .starts_with(prefix.as_bytes())
            })
            .map(|path| {
                let bytes = fs::read(&path).unwrap();
                (path, bytes)
            })
            .collect::<Vec<_>>();
        raw.iter()
            .map(|(_, bytes)| {
                bytes
                    .strip_suffix(&[0])
                    .unwrap_or_else(|| panic!("{raw:?}"))
                    .split(|b| *b == 0)
                    .map(Vec::from)
                    .collect()
            })
            .collect()
    }
    fn calls(&self) -> Vec<Vec<String>> {
        let bytes = fs::read(self.root.join("argv")).unwrap();
        let mut calls: Vec<Vec<String>> = Vec::new();
        for word in bytes.split(|b| *b == 0).filter(|b| !b.is_empty()) {
            if word == b"CALL" {
                calls.push(Vec::new());
            } else {
                calls
                    .last_mut()
                    .unwrap()
                    .push(String::from_utf8(word.to_vec()).unwrap());
            }
        }
        calls
    }
    fn unchanged(&self) {
        assert_eq!(fs::read(self.root.join("flake.lock")).unwrap(), self.lock);
    }
}
fn native(path: &Path, body: &str) {
    fs::write(path, format!("#!/bin/sh\n{body}\n")).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}

#[test]
fn build_application_reuses_then_publishes_actual_selected_outputs_with_exact_options() {
    let fixture = Fixture::new();
    let output = fixture.run("yes");
    assert_eq!(output.status.code(), Some(0), "{output:?}");
    fixture.unchanged();
    let calls = fixture.calls();
    assert_eq!(calls.len(), 8, "{calls:?}");
    assert_eq!(
        calls[0],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--option",
            "max-jobs",
            "auto",
            ".#confctl.settings"
        ]
    );
    assert_eq!(
        calls[1],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--option",
            "max-jobs",
            "4",
            ".#confctl.settings"
        ]
    );
    assert_eq!(
        calls[2],
        [
            "build",
            "--json",
            "--no-link",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--option",
            "max-jobs",
            "4",
            ".#confctl.machinesJson"
        ]
    );
    assert_eq!(
        calls[3],
        [
            "eval",
            "--json",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--show-trace",
            "--option",
            "max-jobs",
            "2",
            "--option",
            "cores",
            "3",
            ".#confctl.settings"
        ]
    );
    assert_eq!(calls[4].last().unwrap(), ".#confctl.buildPlan");
    assert_eq!(
        calls[5],
        [
            "build",
            "--json",
            "--no-link",
            "--impure",
            "--no-write-lock-file",
            "--no-update-lock-file",
            "--show-trace",
            "--option",
            "max-jobs",
            "2",
            "--option",
            "cores",
            "3",
            ".#confctl.build.alpha-key.toplevel",
            ".#confctl.build.alpha-key.autoRollback",
            ".#confctl.build.\"beta/key\".toplevel",
            ".#confctl.build.\"beta/key\".autoRollback"
        ]
    );
    assert!(
        calls[6]
            .iter()
            .any(|s| s.contains("[ \"alpha-key\" \"beta/key\" ]"))
    );
    assert!(calls[7].iter().any(|s| s.contains("[ \"beta/key\" ]")));
    let host = fixture.root.join(".confctl/generations");
    assert_eq!(
        fs::read_link(host.join("alpha/current")).unwrap(),
        Path::new("2000-01-01--00-00-00")
    );
    let name = fs::read_link(host.join("carrier#beta/current")).unwrap();
    let dir = host.join("carrier#beta").join(&name);
    let record: Value =
        serde_json::from_slice(&fs::read(dir.join("generation.json")).unwrap()).unwrap();
    assert_eq!(
        record["toplevel"],
        fixture.root.join("store/new").to_str().unwrap()
    );
    assert_eq!(
        fs::read_link(dir.join("auto_rollback")).unwrap(),
        fixture.root.join("store/rollback")
    );
    assert_eq!(
        fs::read_link(dir.join("nixpkgs.input")).unwrap(),
        fixture.root.join("store/input")
    );
    let roots = GcRoots::new(
        &ConfigRoot::new(&fixture.root).unwrap(),
        fixture.root.join("roots"),
        "test",
    )
    .unwrap();
    assert_eq!(fs::read_dir(roots.directory()).unwrap().count(), 3);
    assert!(!host.join("unmanaged").exists());
    assert!(fixture.root.join(".confctl/mutation.lock").is_file());
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(
        stdout.contains("alpha: 2000-01-01--00-00-00 (reused)")
            && stdout.contains("carrier#beta:")
            && stdout.contains("Built generations:"),
        "{stdout}"
    );
    assert!(String::from_utf8_lossy(&output.stderr).contains("unknown build log"));
    assert_eq!(
        fs::read_dir(fixture.root.join(".confctl/logs"))
            .unwrap()
            .count(),
        0
    );
}
#[test]
fn build_application_decline_and_empty_selection_never_admit_build_or_mutate_generations() {
    for role in ["decline", "empty"] {
        let fixture = Fixture::new();
        let output = fixture.run(role);
        assert_eq!(output.status.code(), Some(1), "{output:?}");
        fixture.unchanged();
        assert_eq!(fixture.calls().len(), 3);
        assert!(
            String::from_utf8_lossy(&output.stderr).contains(if role == "decline" {
                "Aborted"
            } else {
                "No machines to build"
            })
        );
        assert_eq!(
            fs::read_link(fixture.root.join(".confctl/generations/alpha/current")).unwrap(),
            Path::new("2001-01-01--00-00-00")
        );
        assert!(
            !fixture.root.join("roots").exists()
                && !fixture.root.join(".confctl/mutation.lock").exists()
        );
    }
}
#[test]
fn build_application_child_failure_retains_log_and_publishes_no_generations() {
    let fixture = Fixture::new();
    let output = fixture.run("failure");
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    fixture.unchanged();
    assert_eq!(fixture.calls().len(), 6);
    assert!(
        !fixture
            .root
            .join(".confctl/generations/carrier#beta")
            .exists()
    );
    assert!(!fixture.root.join("roots").exists());
    assert_eq!(
        fs::read_link(fixture.root.join(".confctl/generations/alpha/current")).unwrap(),
        Path::new("2001-01-01--00-00-00")
    );
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stderr.contains("fixture build failed"), "{stderr}");
    assert!(stderr.contains("exit status: 7"), "{stderr}");
    let locator = stderr
        .lines()
        .find_map(|line| line.strip_prefix("Log file: "))
        .unwrap();
    let log = fs::read(locator).unwrap();
    assert!(
        log.windows(b"unknown build log".len())
            .any(|w| w == b"unknown build log")
    );
}

fn seq_position(sequence: &[String], prefix: &str) -> usize {
    sequence
        .iter()
        .position(|line| line.starts_with(prefix))
        .unwrap_or_else(|| panic!("missing {prefix:?}: {sequence:?}"))
}
#[test]
fn new_build_deploy_all_actions_default_and_copy_only_use_returned_records() {
    for action in ["boot", "dry-activate", "test", "switch", "default", "copy"] {
        let f = Fixture::new();
        f.composed(false, false);
        let mut args = vec![
            "--color=never",
            "deploy",
            "--yes",
            "--show-trace",
            "--max-jobs=2",
            "--cores=3",
        ];
        if action == "copy" {
            args.extend(["--copy-only", "--reboot"]);
        }
        args.push("alpha");
        if action != "default" {
            args.push(if action == "copy" { "boot" } else { action });
        }
        let out = f.run_args("composed", Some(&args), b"");
        assert_eq!(out.status.code(), Some(0), "{action}: {out:?}");
        let sequence = f.sequence();
        assert!(
            seq_position(&sequence, "build 1") < seq_position(&sequence, "copy root@alpha.invalid")
        );
        let copies = f.native_records("copy-");
        assert_eq!(copies.len(), 1, "{copies:?}; {out:?}");
        assert_eq!(
            &copies[0][..3],
            [
                b"-o Existing=yes -p 2222".to_vec(),
                b"--to".to_vec(),
                b"root@alpha.invalid".to_vec()
            ]
        );
        assert_eq!(
            copies[0][3],
            f.root.join("store/reused").as_os_str().as_encoded_bytes()
        );
        assert_eq!(
            copies[0][4],
            f.root
                .join("store/old-rollback")
                .as_os_str()
                .as_encoded_bytes()
        );
        let nix = f.native_records("nix-call-");
        let build = nix
            .iter()
            .find(|args| {
                args.iter()
                    .any(|arg| arg == b".#confctl.build.alpha-key.toplevel")
            })
            .unwrap();
        for flag in [b"--show-trace".as_slice(), b"2", b"3"] {
            assert!(build.iter().any(|arg| arg == flag), "{build:?}");
        }
        let text = String::from_utf8_lossy(&out.stdout);
        assert_eq!(text.matches("Built generations:").count(), 1, "{text}");
        assert!(!text.contains("Resolved host generations"), "{text}");
        if action == "copy" {
            assert!(f.native_records("ssh-call-").is_empty());
            assert!(f.native_records("activate-").is_empty());
            assert!(!sequence.iter().any(|line| line.starts_with("health ")));
        } else {
            let requested = if action == "default" {
                "switch"
            } else {
                action
            };
            let activation = format!("activate alpha {requested}");
            assert!(seq_position(&sequence, "copy ") < seq_position(&sequence, &activation));
            if matches!(action, "test" | "switch" | "default") {
                let helpers = f.native_records("helper-");
                assert_eq!(helpers.len(), 1);
                assert_eq!(
                    helpers[0][1],
                    f.root
                        .join("store/old-rollback")
                        .as_os_str()
                        .as_encoded_bytes()
                );
                assert!(
                    seq_position(&sequence, "helper-end alpha")
                        < seq_position(&sequence, "health alpha")
                );
                if action != "test" {
                    assert!(
                        seq_position(&sequence, "helper-end alpha")
                            < seq_position(&sequence, "profile alpha")
                    );
                    assert!(
                        seq_position(&sequence, "profile alpha")
                            < seq_position(&sequence, "health alpha")
                    );
                }
            } else {
                assert!(f.native_records("helper-").is_empty());
            }
            assert_eq!(
                f.native_records("profile-").len(),
                usize::from(matches!(action, "boot" | "switch" | "default"))
            );
        }
        assert_eq!(
            fs::read_link(f.root.join(".confctl/generations/alpha/current")).unwrap(),
            Path::new("2000-01-01--00-00-00")
        );
        assert!(!f.root.join("forbidden").exists());
        f.unchanged();
    }
}

#[test]
fn new_build_group_return_order_controls_serial_deploy_but_bulk_copy_uses_confirmed_order() {
    for mode in ["one-by-one", "bulk", "interactive"] {
        let f = Fixture::new();
        f.composed(true, false);
        let mut args = vec![
            "--color=never",
            "deploy",
            "--yes",
            "--max-concurrent-copy=1",
            "--disable-auto-rollback",
        ];
        if mode == "one-by-one" {
            args.push("--one-by-one");
        }
        if mode == "interactive" {
            args.extend(["--interactive", "--no-health-checks"]);
        }
        args.extend(["*", "switch"]);
        let out = f.run_args(
            "composed",
            Some(&args),
            if mode == "interactive" {
                b"y\ny\ny\ny\ny\ny\ny\n"
            } else {
                b""
            },
        );
        assert_eq!(out.status.code(), Some(0), "{mode}: {out:?}");
        let sequence = f.sequence();
        assert!(
            seq_position(&sequence, "build 2") < seq_position(&sequence, "copy "),
            "{sequence:?}"
        );
        let copies = sequence
            .iter()
            .filter_map(|line| line.strip_prefix("copy "))
            .collect::<Vec<_>>();
        assert_eq!(
            copies,
            if mode == "bulk" {
                vec![
                    "root@ahead.invalid",
                    "root@alpha.invalid",
                    "root@zed.invalid",
                    "root@carrier.invalid",
                ]
            } else {
                vec![
                    "root@alpha.invalid",
                    "root@ahead.invalid",
                    "root@carrier.invalid",
                    "root@zed.invalid",
                ]
            },
            "{sequence:?}"
        );
        let actions = sequence
            .iter()
            .filter(|line| line.starts_with("activate ") || line.starts_with("carrier-profile "))
            .map(String::as_str)
            .collect::<Vec<_>>();
        assert_eq!(
            actions,
            [
                "activate alpha switch",
                "activate ahead switch",
                "carrier-profile carrier",
                "activate zed switch"
            ]
        );
        if mode == "one-by-one" {
            assert!(
                seq_position(&sequence, "health alpha")
                    < seq_position(&sequence, "copy root@ahead.invalid")
            );
        } else if mode == "bulk" {
            assert!(
                seq_position(&sequence, "copy root@carrier.invalid")
                    < seq_position(&sequence, "activate alpha")
            );
            assert!(seq_position(&sequence, "profile zed") < seq_position(&sequence, "health "));
        }
        let new_names = ["ahead", "carrier#beta", "zed"].map(|host| {
            fs::read_link(
                f.root
                    .join(".confctl/generations")
                    .join(host)
                    .join("current"),
            )
            .unwrap()
        });
        assert!(new_names.iter().all(|name| *name == new_names[0]));
        assert_eq!(f.native_records("carrier-").len(), 1);
        assert!(!sequence.iter().any(|line| line == "health carrier#beta"));
        f.unchanged();
        assert!(!f.root.join("forbidden").exists());
    }
}

#[test]
fn new_build_later_group_or_publication_failure_never_consumes_successful_prefix() {
    for consumer in ["deploy", "status"] {
        for failure in ["later-fail", "publication-fail"] {
            let f = Fixture::new();
            f.composed(true, false);
            if failure == "publication-fail" {
                fs::create_dir_all(f.root.join(".confctl/generations/carrier#beta/current"))
                    .unwrap();
            }
            let args = ["--color=never", consumer, "--yes", "*"];
            let out = f.run_args(failure, Some(&args), b"");
            assert_eq!(out.status.code(), Some(1), "{consumer}/{failure}: {out:?}");
            assert!(f.native_records("copy-").is_empty());
            assert!(f.native_records("ssh-call-").is_empty());
            assert!(f.native_records("activate-").is_empty());
            assert!(!f.sequence().iter().any(|line| line.starts_with("health ")));
            assert_eq!(
                fs::read_link(f.root.join(".confctl/generations/alpha/current")).unwrap(),
                Path::new("2000-01-01--00-00-00")
            );
            assert!(
                f.root
                    .join(".confctl/generations/ahead/current")
                    .is_symlink()
            );
            assert!(!f.root.join(".confctl/generations/zed/current").exists());
            assert!(
                String::from_utf8_lossy(&out.stderr).contains("retained"),
                "{out:?}"
            );
            f.unchanged();
        }
    }
}

#[test]
fn new_build_refresh_precedes_fresh_plan_and_default_status_uses_current_inputs_without_refresh() {
    let f = Fixture::new();
    f.composed(false, true);
    let out = f.run_args(
        "composed",
        Some(&["--color=never", "deploy", "--yes", "--copy-only", "alpha"]),
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let sequence = f.sequence();
    assert!(seq_position(&sequence, "refresh alpha") < seq_position(&sequence, "plan plan-after"));
    let name = fs::read_link(f.root.join(".confctl/generations/alpha/current")).unwrap();
    assert_ne!(name, Path::new("2000-01-01--00-00-00"));
    let record: Value = serde_json::from_slice(
        &fs::read(
            f.root
                .join(".confctl/generations/alpha")
                .join(name)
                .join("generation.json"),
        )
        .unwrap(),
    )
    .unwrap();
    assert_eq!(
        record["inputs"]["nixpkgs"],
        f.root.join("store/refreshed").to_str().unwrap()
    );
    assert_eq!(f.native_records("ssh-call-").len(), 1); // refresh only, copy-only makes no observations
    f.unchanged();

    let f = Fixture::new();
    f.composed(false, true);
    let out = f.run_args(
        "status-current",
        Some(&["--color=never", "status", "--yes", "{alpha,ahead}"]),
        b"",
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let text = String::from_utf8_lossy(&out.stdout);
    let alpha = text
        .lines()
        .find(|line| line.starts_with("alpha "))
        .unwrap();
    assert!(
        alpha.contains("ok") && alpha.contains("same") && alpha.contains("2:0"),
        "{text}"
    );
    assert!(text.contains("CURRENT") && !text.contains("OLD"), "{text}");
    let sequence = f.sequence();
    assert!(seq_position(&sequence, "build 1") < seq_position(&sequence, "observe-or-command "));
    assert!(
        sequence
            .iter()
            .any(|line| line == "current-inputs .#confctl.inputsInfo.alpha-key")
    );
    assert!(!f.root.join("runtime-kernels.json").exists());
    assert!(f.native_records("copy-").is_empty());
    assert!(f.native_records("activate-").is_empty());
    assert!(
        !sequence
            .iter()
            .any(|line| line.starts_with("health ") || line.starts_with("refresh "))
    );
    assert_eq!(
        fs::read_link(f.root.join(".confctl/generations/alpha/current")).unwrap(),
        Path::new("2000-01-01--00-00-00")
    );
    assert!(
        f.root
            .join(".confctl/generations/ahead/current")
            .is_symlink()
    );
    assert!(f.root.join("roots").is_dir());
    assert!(f.root.join(".confctl/mutation.lock").is_file());
    f.unchanged();
    assert!(!f.root.join("forbidden").exists());
}

#[test]
fn new_build_initial_decline_has_one_consent_and_no_build_lock_or_later_phases() {
    for consumer in ["deploy", "status"] {
        let f = Fixture::new();
        f.composed(false, true);
        let out = f.run_args(
            "composed",
            Some(&["--color=never", consumer, "alpha"]),
            b"n\n",
        );
        assert_eq!(out.status.code(), Some(1), "{out:?}");
        assert!(String::from_utf8_lossy(&out.stdout).contains("BUILT"));
        assert!(!f.root.join("build-count").exists());
        assert!(!f.root.join(".confctl/mutation.lock").exists());
        assert!(!f.root.join("roots").exists());
        assert!(!f.root.join("runtime-kernels.json").exists());
        assert!(f.native_records("ssh-call-").is_empty());
        assert!(f.native_records("copy-").is_empty());
        f.unchanged();
    }
}

#[test]
fn new_build_action_failures_retain_publications_and_stop_later_effects() {
    for role in [
        "copy-fail",
        "activation-fail",
        "helper-fail",
        "profile-fail",
        "health-fail",
        "carrier-fail",
    ] {
        let f = Fixture::new();
        f.composed(false, false);
        let mut args = vec!["--color=never", "deploy", "--yes"];
        if role != "helper-fail" {
            args.push("--disable-auto-rollback");
        }
        args.extend([
            if role == "carrier-fail" {
                "carrier#beta"
            } else {
                "alpha"
            },
            "switch",
        ]);
        let out = f.run_args(role, Some(&args), b"");
        assert_eq!(out.status.code(), Some(1), "{role}: {out:?}");
        assert!(f.root.join(".confctl/mutation.lock").is_file());
        let sequence = f.sequence();
        assert!(seq_position(&sequence, "build 1") < seq_position(&sequence, "copy "));
        if ["copy-fail", "activation-fail", "helper-fail"].contains(&role) {
            assert!(f.native_records("profile-").is_empty());
        }
        if role != "health-fail" {
            assert!(
                !sequence.iter().any(|line| line.starts_with("health ")),
                "{sequence:?}"
            );
        }
        assert!(
            String::from_utf8_lossy(&out.stderr).contains("retained"),
            "{out:?}"
        );
        assert!(!f.root.join("forbidden").exists());
        f.unchanged();
    }
}

#[test]
fn new_build_cancellation_joins_entered_build_before_any_deployment_or_status_query() {
    for consumer in ["deploy", "status"] {
        let f = Fixture::new();
        f.composed(false, false);
        let mut child = f
            .command_args(
                "slow-build",
                Some(&["--color=never", consumer, "--yes", "alpha"]),
            )
            .spawn()
            .unwrap();
        drop(child.stdin.take());
        let until = Instant::now() + Duration::from_secs(5);
        let pid = loop {
            if let Ok(text) = fs::read_to_string(f.root.join("build-pid"))
                && let Ok(pid) = text.parse::<u32>()
                && pid > 0
            {
                break pid;
            }
            assert!(Instant::now() < until, "{consumer}: {:?}", f.sequence());
            std::thread::sleep(Duration::from_millis(10));
        };
        rustix::process::kill_process(
            rustix::process::Pid::from_raw(child.id() as i32).unwrap(),
            rustix::process::Signal::INT,
        )
        .unwrap();
        let until = Instant::now() + Duration::from_secs(10);
        while child.try_wait().unwrap().is_none() {
            assert!(Instant::now() < until, "{consumer}: {:?}", f.sequence());
            std::thread::sleep(Duration::from_millis(10));
        }
        let out = child.wait_with_output().unwrap();
        assert_eq!(out.status.code(), Some(130), "{out:?}");
        assert!(!Path::new("/proc").join(pid.to_string()).exists());
        assert!(f.native_records("copy-").is_empty());
        assert!(f.native_records("ssh-call-").is_empty());
        assert!(!f.root.join(".confctl/generations/ahead").exists());
        f.unchanged();
    }
}
