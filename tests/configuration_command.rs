//! Actual CLI with temporary configuration trees and fake Nix, without network.
#![cfg(target_os = "linux")]

use serde_json::{Value, json};
use std::{
    ffi::{OsStr, OsString},
    fs,
    os::{
        fd::OwnedFd,
        unix::{
            ffi::OsStringExt,
            fs::{PermissionsExt, symlink},
            net::UnixStream,
            process::CommandExt,
        },
    },
    path::{Path, PathBuf},
    process::{Child, Command, Output, Stdio},
    thread,
    time::{Duration, Instant},
};

struct Fixture {
    scratch: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
    control: PathBuf,
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("conf");
        let bin = scratch.path().join("bin");
        let control = scratch.path().join("control");
        for path in [&root, &bin, &control] {
            fs::create_dir(path).unwrap();
        }
        fs::create_dir(root.join("cluster")).unwrap();
        fs::create_dir_all(root.join(".confctl/generations/alpha")).unwrap();
        fs::write(
            root.join(".confctl/generations/alpha/saved"),
            b"retained generation\n",
        )
        .unwrap();
        symlink("saved", root.join(".confctl/generations/alpha/current")).unwrap();
        fs::write(root.join("flake.nix"), b"{ outputs = _: {}; }\n").unwrap();
        fs::write(root.join("flake.lock"), b"retained input lock\n").unwrap();
        fs::write(root.join("cluster/cluster.nix"), b"old core\n").unwrap();
        fs::write(root.join("cluster/netbootable.nix"), b"old feature\n").unwrap();
        fs::write(root.join("cluster/kernels.json"), b"retained kernels\n").unwrap();
        let fixture = Self {
            scratch,
            root,
            bin,
            control,
        };
        fixture.host("alpha");
        fixture.host("nested/beta");
        fixture.scenario(json!({"bootstrap":{},"client":{}}));
        let python = std::env::split_paths(&std::env::var_os("PATH").unwrap())
            .map(|directory| directory.join("python3"))
            .find(|path| path.is_file())
            .expect("declared test environment provides Python")
            .canonicalize()
            .unwrap();
        executable(
            &fixture.bin.join("nix"),
            &r#"#!PYTHON
import json, os, pathlib, subprocess, sys, time
control = pathlib.Path(os.environ['CONFIGURATION_CONTROL'])
scenario = json.loads((control / 'scenario.json').read_text())
core = pathlib.Path('cluster/cluster.nix').read_bytes()
record = {'args': sys.argv[1:], 'core': core.hex()}
with (control / 'calls').open('a') as f: f.write(json.dumps(record) + '\n')
calls = [json.loads(line) for line in (control / 'calls').read_text().splitlines()]
number = len(calls)
if scenario.get('slow_at') == number:
    (control / 'pid').write_text(str(os.getpid()) + '\n')
    if scenario.get('descendant'):
        child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
        (control / 'child-pid').write_text(str(child.pid) + '\n')
    while not (control / 'release').exists(): time.sleep(0.01)
if scenario.get('fail_at') == number:
    os.write(2, b'native configuration failure\xff\n')
    sys.exit(7)
if scenario.get('bad_at') == number:
    print('not json'); sys.exit(0)
if sys.argv[-1] == '.#confctl.settings':
    if scenario.get('policy'):
        enabled = b'./nested/renamed/module.nix' in core
        settings = {'netbootInventory': {'enable': enabled, 'outputFile': 'data/new-list.nix'}, 'nix': {'maxJobs': 4}}
    else: settings = scenario.get('bootstrap' if number == 1 else 'client', {})
    print(json.dumps(settings))
elif sys.argv[-1] == '.#confctl.machinesJson':
    inventory = scenario.get('inventory', {})
    if scenario.get('dynamic_inventory'):
        names = [line.strip()[2:-len('/module.nix')] for line in core.decode().splitlines() if line.startswith('  ./')]
        inventory = {str(i): {'name':name, 'metaConfig':{'netboot':{'enable':True}}} for i,name in enumerate(names)}
    (control / 'inventory.json').write_text(json.dumps(inventory))
    if scenario.get('publication_conflict'):
        path = pathlib.Path(scenario['bootstrap']['netbootInventory']['outputFile'])
        path.unlink(missing_ok=True); path.mkdir()
    print(json.dumps([{'outputs':{'out':str(control / 'inventory.json')}}]))
else:
    os.write(2,b'unexpected Nix target\n'); sys.exit(98)
"#.replacen("PYTHON", python.to_str().expect("Nix store interpreter path is UTF-8"), 1),
        );
        for name in [
            "ssh",
            "git",
            "nix-build",
            "nix-instantiate",
            "nix-shell",
            "nixfmt",
        ] {
            executable(
                &fixture.bin.join(name),
                "#!/bin/sh\nprintf '%s\\n' \"$0\" >> \"$CONFIGURATION_CONTROL/unexpected\"\nexit 99\n",
            );
        }
        fixture
    }
    fn host(&self, name: impl AsRef<Path>) {
        let path = self.root.join("cluster").join(name);
        fs::create_dir_all(&path).unwrap();
        for file in ["module.nix", "config.nix"] {
            fs::write(path.join(file), b"{}\n").unwrap();
        }
    }
    fn scenario(&self, scenario: Value) {
        fs::write(
            self.control.join("scenario.json"),
            serde_json::to_vec(&scenario).unwrap(),
        )
        .unwrap();
    }
    fn command(&self, args: &[&OsStr], mask: u32) -> Command {
        let mut command = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        command
            .current_dir(&self.root)
            .arg("--color=never")
            .args(args)
            .env("PATH", &self.bin)
            .env("CONFIGURATION_CONTROL", &self.control)
            .env("CONFCTL_TTY", "0")
            .env("NO_COLOR", "1")
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        // The mask changes only in this child, never the concurrent test process.
        unsafe {
            command.pre_exec(move || {
                rustix::process::umask(rustix::fs::Mode::from_raw_mode(mask));
                Ok(())
            });
        }
        command
    }
    fn run(&self, args: &[&str]) -> Output {
        self.command(&args.iter().map(OsStr::new).collect::<Vec<_>>(), 0o022)
            .output()
            .unwrap()
    }
    fn calls(&self) -> Vec<Value> {
        match fs::read_to_string(self.control.join("calls")) {
            Ok(text) => text
                .lines()
                .map(|line| serde_json::from_str(line).unwrap())
                .collect(),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => vec![],
            Err(e) => panic!("{e}"),
        }
    }
    fn unchanged_state(&self) {
        for (path, bytes) in [
            ("flake.lock", b"retained input lock\n".as_slice()),
            (".confctl/generations/alpha/saved", b"retained generation\n"),
            ("cluster/kernels.json", b"retained kernels\n"),
        ] {
            assert_eq!(fs::read(self.root.join(path)).unwrap(), bytes);
        }
        assert_eq!(
            fs::read_link(self.root.join(".confctl/generations/alpha/current")).unwrap(),
            Path::new("saved")
        );
        assert!(!self.control.join("unexpected").exists());
        assert!(!self.root.join(".git").exists());
    }
    fn old_feature(&self) {
        assert_eq!(
            fs::read(self.root.join("cluster/netbootable.nix")).unwrap(),
            b"old feature\n"
        );
    }
}
fn executable(path: &Path, source: &str) {
    fs::write(path, source).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
}
fn core(names: &[&str]) -> Vec<u8> {
    format!(
        "# This file is generated by confctl, changes will be lost\n[\n{}]\n",
        names
            .iter()
            .map(|name| format!("  ./{name}/module.nix\n"))
            .collect::<String>()
    )
    .into_bytes()
}
fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}
fn args(build: bool, jobs: &str, impure: bool) -> Value {
    let mut result = if build {
        vec!["build", "--json", "--no-link"]
    } else {
        vec!["eval", "--json"]
    };
    if impure {
        result.push("--impure");
    }
    result.extend([
        "--no-write-lock-file",
        "--no-update-lock-file",
        "--option",
        "max-jobs",
        jobs,
        if build {
            ".#confctl.machinesJson"
        } else {
            ".#confctl.settings"
        },
    ]);
    json!(result)
}
fn enabled() -> Value {
    json!({"netbootInventory":{"enable":true,"outputFile":"cluster/netbootable.nix"}, "nix":{"maxJobs":3, "impureEval":true}})
}

#[test]
fn add_matches_ruby_templates_notices_modes_and_one_postwrite_disabled_query() {
    for mask in [0o022, 0o077, 0] {
        let fixture = Fixture::new();
        fs::set_permissions(
            fixture.root.join("cluster/cluster.nix"),
            fs::Permissions::from_mode(0o600),
        )
        .unwrap();
        let result = fixture
            .command(&[OsStr::new("add"), OsStr::new("nested/new")], mask)
            .output()
            .unwrap();
        assert!(result.status.success(), "{result:?}");
        let expected: Value =
            serde_json::from_str(include_str!("compat/core/expected/add.json")).unwrap();
        assert_eq!(
            result.stdout,
            expected["stdout"].as_str().unwrap().as_bytes()
        );
        for path in [
            "cluster/nested/new/module.nix",
            "cluster/nested/new/config.nix",
            "cluster/cluster.nix",
        ] {
            assert_eq!(
                fs::read(fixture.root.join(path)).unwrap(),
                expected["files"][path]["text"].as_str().unwrap().as_bytes()
            );
            assert_eq!(
                fs::metadata(fixture.root.join(path))
                    .unwrap()
                    .permissions()
                    .mode()
                    & 0o777,
                0o644 & !mask
            );
        }
        assert_eq!(
            fs::metadata(fixture.root.join("cluster/nested/new"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o755 & !mask
        );
        assert_eq!(
            fixture.calls(),
            [
                json!({"args":args(false,"auto",false),"core":hex(&core(&["alpha","nested/beta","nested/new"]))})
            ]
        );
        fixture.old_feature();
        fixture.unchanged_state();
    }
}

#[test]
fn rename_moves_once_without_embedded_name_state_or_generation_rewrites() {
    let fixture = Fixture::new();
    let original =
        b"cluster.\"alpha\" = { hostname = \"alpha\"; import = ../../environments/base.nix; };\n";
    fs::write(fixture.root.join("cluster/alpha/module.nix"), original).unwrap();
    let result = fixture.run(&["rename", "alpha", "nested/renamed"]);
    assert!(result.status.success(), "{result:?}");
    let expected: Value =
        serde_json::from_str(include_str!("compat/core/expected/rename.json")).unwrap();
    assert_eq!(
        result.stdout,
        expected["stdout"].as_str().unwrap().as_bytes()
    );
    assert_eq!(
        fs::read(fixture.root.join("cluster/nested/renamed/module.nix")).unwrap(),
        original
    );
    assert!(!fixture.root.join("cluster/alpha").exists());
    assert_eq!(
        fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
        core(&["nested/beta", "nested/renamed"])
    );
    assert_eq!(fixture.calls().len(), 1);
    let moved = fixture
        .command(
            &[
                OsStr::new("rename"),
                OsStr::new("nested/renamed"),
                OsStr::new("new-parent/deep/renamed"),
            ],
            0o077,
        )
        .output()
        .unwrap();
    assert!(moved.status.success(), "{moved:?}");
    for path in ["new-parent", "new-parent/deep"] {
        assert_eq!(
            fs::metadata(fixture.root.join("cluster").join(path))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o700
        );
    }
    assert_eq!(
        fs::read(
            fixture
                .root
                .join("cluster/new-parent/deep/renamed/module.nix")
        )
        .unwrap(),
        original
    );
    fixture.old_feature();
    fixture.unchanged_state();
}

#[test]
fn discovery_is_sorted_hidden_nested_and_internal_aliases_are_not_deduplicated() {
    let fixture = Fixture::new();
    fixture.host("alpha/child");
    fixture.host("a");
    fixture.host("a-z");
    fixture.host("a/child");
    fixture.host(".hidden");
    fixture.host("z");
    symlink("alpha", fixture.root.join("cluster/alias")).unwrap();
    fs::create_dir(fixture.root.join("cluster/not-a-host")).unwrap();
    fs::write(fixture.root.join("cluster/not-a-host/module.nix"), b"{}").unwrap();
    let result = fixture.run(&["rediscover"]);
    assert!(result.status.success(), "{result:?}");
    assert_eq!(result.stdout, b"replace cluster/cluster.nix\n");
    let expected = core(&[
        ".hidden",
        "a",
        "a-z",
        "a/child",
        "alias",
        "alias/child",
        "alpha",
        "alpha/child",
        "nested/beta",
        "z",
    ]);
    assert_eq!(
        fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
        expected
    );
    assert_eq!(fixture.calls()[0]["core"], hex(&expected));
    fixture.unchanged_state();
    let fixture = Fixture::new();
    fs::remove_dir_all(fixture.root.join("cluster/alpha")).unwrap();
    fs::remove_dir_all(fixture.root.join("cluster/nested")).unwrap();
    assert!(fixture.run(&["rediscover"]).status.success());
    assert_eq!(
        fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
        core(&[])
    );
}

#[test]
fn enabled_routes_query_fresh_core_twice_then_inventory_and_publish_in_inventory_order() {
    for command in [
        vec!["add", "nested/new"],
        vec!["rename", "alpha", "nested/renamed"],
        vec!["rediscover"],
    ] {
        let fixture = Fixture::new();
        fs::set_permissions(
            fixture.root.join("cluster/netbootable.nix"),
            fs::Permissions::from_mode(0o640),
        )
        .unwrap();
        fixture.scenario(json!({"bootstrap":enabled(),"client":{"nix":{"maxJobs":99,"impureEval":true}},"inventory":{
            "z":{"name":"z-first","metaConfig":{"spin":"nixos","managed":false,"host":{"target":null},"netboot":{"enable":true}}},
            "a":{"name":"a-next","metaConfig":{"spin":"vpsadminos","netboot":{"enable":true}}},
            "carried":{"name":"carrier#guest","carrier":"carrier","metaConfig":{"netboot":{"enable":true}}},
            "false":{"name":"off","metaConfig":{"netboot":{"enable":false}}},
            "truthy":{"name":"integer","metaConfig":{"netboot":{"enable":1}}}
        }}));
        let result = fixture.run(&command);
        assert!(result.status.success(), "{result:?}");
        let written = fs::read(fixture.root.join("cluster/cluster.nix")).unwrap();
        let calls = fixture.calls();
        assert_eq!(calls.len(), 3);
        for call in &calls {
            assert_eq!(call["core"], hex(&written));
        }
        assert_eq!(calls[0]["args"], args(false, "auto", false));
        assert_eq!(calls[1]["args"], args(false, "3", false));
        assert_eq!(calls[2]["args"], args(true, "3", true));
        assert!(
            result
                .stdout
                .ends_with(b"replace cluster/netbootable.nix\n")
        );
        assert_eq!(fs::read(fixture.root.join("cluster/netbootable.nix")).unwrap(), b"# This file is auto-generated on confctl rediscover, changes will be lost\n[\n  \"z-first\"\n  \"a-next\"\n]\n");
        assert_eq!(
            fs::metadata(fixture.root.join("cluster/netbootable.nix"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o640
        );
        fixture.unchanged_state();
    }
}

#[test]
fn renamed_source_changes_postwrite_policy_output_path_and_inventory_across_invocations() {
    let fixture = Fixture::new();
    fs::create_dir(fixture.root.join("data")).unwrap();
    fixture.scenario(json!({"policy":true,"dynamic_inventory":true}));
    let first = fixture.run(&["rediscover"]);
    assert!(first.status.success(), "{first:?}");
    assert_eq!(fixture.calls().len(), 1);
    assert!(!fixture.root.join("data/new-list.nix").exists());
    fs::remove_file(fixture.control.join("calls")).unwrap();
    let second = fixture.run(&["rename", "alpha", "nested/renamed"]);
    assert!(second.status.success(), "{second:?}");
    assert_eq!(fixture.calls().len(), 3);
    assert_eq!(fs::read(fixture.root.join("data/new-list.nix")).unwrap(), b"# This file is auto-generated on confctl rediscover, changes will be lost\n[\n  \"nested/beta\"\n  \"nested/renamed\"\n]\n");
    fixture.old_feature();
    fs::remove_file(fixture.control.join("calls")).unwrap();
    let third = fixture.run(&["rename", "nested/renamed", "alpha"]);
    assert!(third.status.success(), "{third:?}");
    assert_eq!(fixture.calls().len(), 1);
    assert!(
        !third
            .stdout
            .windows(b"replace data/".len())
            .any(|b| b == b"replace data/")
    );
    assert!(
        fs::read(fixture.root.join("data/new-list.nix"))
            .unwrap()
            .windows(b"nested/renamed\"".len())
            .any(|b| b == b"nested/renamed\"")
    );
    fixture.unchanged_state();
}

#[test]
fn enabled_empty_inventory_writes_empty_list_and_new_output_respects_umask() {
    let fixture = Fixture::new();
    fs::create_dir(fixture.root.join("data")).unwrap();
    let settings = json!({"netbootInventory":{"enable":true,"outputFile":"data/empty.nix"}});
    fixture.scenario(json!({"bootstrap":settings,"client":{},"inventory":{}}));
    let result = fixture
        .command(&[OsStr::new("rediscover")], 0o077)
        .output()
        .unwrap();
    assert!(result.status.success(), "{result:?}");
    assert_eq!(
        fs::read(fixture.root.join("data/empty.nix")).unwrap(),
        b"# This file is auto-generated on confctl rediscover, changes will be lost\n[\n]\n"
    );
    assert_eq!(
        fs::metadata(fixture.root.join("data/empty.nix"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
    fixture.old_feature();
    fixture.unchanged_state();
}

#[test]
fn path_refusals_collisions_flake_and_legacy_scripts_precede_effects_and_queries() {
    for command in [
        vec!["add", ""],
        vec!["add", "/absolute"],
        vec!["add", "."],
        vec!["add", "nested/../new"],
        vec!["add", "nested/./new"],
        vec!["add", "nested//new"],
        vec!["add", "alpha"],
        vec!["rename", "missing", "new"],
        vec!["rename", "alpha", "nested/beta"],
        vec!["rename", "alpha", "alpha/child"],
    ] {
        let fixture = Fixture::new();
        let result = fixture.run(&command);
        assert_eq!(result.status.code(), Some(1), "{command:?}: {result:?}");
        assert!(result.stdout.is_empty());
        assert!(fixture.calls().is_empty());
        assert!(!fixture.root.join(".confctl/mutation.lock").exists());
        assert_eq!(
            fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
            b"old core\n"
        );
        fixture.unchanged_state();
    }
    for kind in [
        "missing-flake",
        "directory-flake",
        "script",
        "link-script",
        "invalid-scripts",
    ] {
        let fixture = Fixture::new();
        match kind {
            "missing-flake" => fs::remove_file(fixture.root.join("flake.nix")).unwrap(),
            "directory-flake" => {
                fs::remove_file(fixture.root.join("flake.nix")).unwrap();
                fs::create_dir(fixture.root.join("flake.nix")).unwrap();
            }
            "invalid-scripts" => {
                fs::write(fixture.root.join("scripts"), b"not a directory").unwrap()
            }
            _ => {
                fs::create_dir(fixture.root.join("scripts")).unwrap();
                let path = fixture.root.join("scripts/any-extension.txt");
                if kind == "link-script" {
                    symlink(fixture.root.join("flake.nix"), path).unwrap();
                } else {
                    fs::write(path, b"raise 'must not execute'\n").unwrap();
                }
            }
        }
        let result = fixture.run(&["add", "new"]);
        assert_eq!(result.status.code(), Some(1), "{kind}: {result:?}");
        assert!(result.stdout.is_empty());
        assert!(fixture.calls().is_empty());
        assert!(!fixture.root.join("cluster/new").exists());
        if kind == "script" || kind == "link-script" {
            assert!(String::from_utf8_lossy(&result.stderr).contains("any-extension.txt"));
        }
    }
    let fixture = Fixture::new();
    fs::create_dir_all(fixture.root.join("scripts/nested")).unwrap();
    fs::write(
        fixture.root.join("scripts/nested/ignored.rb"),
        b"not a direct script",
    )
    .unwrap();
    symlink("absent", fixture.root.join("scripts/dangling")).unwrap();
    assert!(fixture.run(&["rediscover"]).status.success());
    assert_eq!(fixture.calls().len(), 1);
}

#[test]
fn internal_parent_links_native_names_and_missing_cluster_are_supported_without_template_repair() {
    let fixture = Fixture::new();
    symlink("nested", fixture.root.join("cluster/internal")).unwrap();
    let native = OsString::from_vec(b"internal/native\xff\"$\n".to_vec());
    let result = fixture
        .command(&[OsStr::new("add"), &native], 0o022)
        .output()
        .unwrap();
    assert!(result.status.success(), "{result:?}");
    let path = fixture.root.join("cluster").join(&native);
    assert!(
        fs::read(path.join("module.nix"))
            .unwrap()
            .windows(b"internal/native\xff\"$\n".len())
            .any(|b| b == b"internal/native\xff\"$\n")
    );
    assert!(
        result
            .stdout
            .windows(b"native\xff\"$\n".len())
            .any(|b| b == b"native\xff\"$\n")
    );
    let fixture = Fixture::new();
    fs::remove_dir_all(fixture.root.join("cluster")).unwrap();
    let result = fixture.run(&["add", "new"]);
    assert!(result.status.success(), "{result:?}");
    assert_eq!(
        fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
        core(&["new"])
    );
}

#[test]
fn escaping_and_cyclic_directory_links_refuse_core_publication_but_keep_prior_add() {
    let fixture = Fixture::new();
    fs::rename(
        fixture.root.join("cluster"),
        fixture.root.join("sibling-tree"),
    )
    .unwrap();
    symlink("sibling-tree", fixture.root.join("cluster")).unwrap();
    let refused = fixture.run(&["add", "new"]);
    assert_eq!(refused.status.code(), Some(1), "{refused:?}");
    assert!(refused.stdout.is_empty());
    assert!(fixture.calls().is_empty());
    assert!(!fixture.root.join("sibling-tree/new").exists());
    assert_eq!(
        fs::read(fixture.root.join("sibling-tree/cluster.nix")).unwrap(),
        b"old core\n"
    );
    for kind in ["escape", "cycle"] {
        let fixture = Fixture::new();
        let outside = fixture.scratch.path().join("outside");
        fs::create_dir(&outside).unwrap();
        let target = if kind == "escape" {
            outside.as_path()
        } else {
            Path::new(".")
        };
        symlink(target, fixture.root.join("cluster/link")).unwrap();
        let refused = fixture.run(&["rediscover"]);
        assert_eq!(refused.status.code(), Some(1), "{refused:?}");
        assert!(refused.stdout.is_empty());
        assert!(fixture.calls().is_empty());
        let added = fixture.run(&["add", "new"]);
        assert_eq!(added.status.code(), Some(1), "{added:?}");
        assert!(fixture.root.join("cluster/new/config.nix").is_file());
        assert_eq!(
            fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
            b"old core\n"
        );
        assert!(fixture.calls().is_empty());
        fixture.old_feature();
        let escape = fixture.run(&["add", "link/child"]);
        if kind == "escape" {
            assert!(escape.stdout.is_empty());
            assert!(!outside.join("child").exists());
        }
    }
}

#[test]
fn settings_inventory_and_feature_errors_keep_completed_core_and_primary_failure() {
    for stage in 1..=3 {
        let fixture = Fixture::new();
        fixture.scenario(json!({"bootstrap":enabled(),"client":{},"fail_at":stage}));
        let result = fixture.run(&["add", "new"]);
        assert_eq!(result.status.code(), Some(1), "stage{stage}: {result:?}");
        assert_eq!(fixture.calls().len(), stage);
        assert_eq!(
            fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
            core(&["alpha", "nested/beta", "new"])
        );
        assert!(
            result
                .stderr
                .windows(b"native configuration failure\xff".len())
                .any(|b| b == b"native configuration failure\xff")
        );
        assert!(String::from_utf8_lossy(&result.stderr).contains("core cluster list replaced"));
        assert!(
            !result
                .stdout
                .ends_with(b"replace cluster/netbootable.nix\n")
        );
        fixture.old_feature();
        fixture.unchanged_state();
    }
    for bootstrap in [
        Value::Null,
        json!({"netbootInventory":{"enable":"invalid"}}),
        json!({"netbootInventory":{"enable":true,"outputFile":"missing/output.nix"}}),
    ] {
        let fixture = Fixture::new();
        fixture.scenario(json!({"bootstrap":bootstrap}));
        let result = fixture.run(&["rediscover"]);
        assert_eq!(result.status.code(), Some(1), "{result:?}");
        assert_eq!(fixture.calls().len(), 1);
        assert_eq!(
            fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
            core(&["alpha", "nested/beta"])
        );
        fixture.old_feature();
    }
    let fixture = Fixture::new();
    fixture.scenario(json!({"bootstrap":enabled(),"client":{},"publication_conflict":true}));
    let result = fixture.run(&["rediscover"]);
    assert_eq!(result.status.code(), Some(1), "{result:?}");
    assert_eq!(fixture.calls().len(), 3);
    assert!(fixture.root.join("cluster/netbootable.nix").is_dir());
    assert!(
        result
            .stdout
            .ends_with(b"replace cluster/netbootable.nix\n")
    );
    let fixture = Fixture::new();
    fs::remove_file(fixture.root.join("cluster/cluster.nix")).unwrap();
    fs::create_dir(fixture.root.join("cluster/cluster.nix")).unwrap();
    let result = fixture.run(&["add", "new"]);
    assert_eq!(result.status.code(), Some(1));
    assert!(fixture.root.join("cluster/new/config.nix").is_file());
    assert!(fixture.calls().is_empty());
}

struct Running(Option<Child>);
impl Running {
    fn start(command: &mut Command) -> Self {
        Self(Some(command.spawn().unwrap()))
    }
    fn finish(mut self) -> Output {
        let started = Instant::now();
        loop {
            if self.0.as_mut().unwrap().try_wait().unwrap().is_some() {
                return self.0.take().unwrap().wait_with_output().unwrap();
            }
            assert!(
                started.elapsed() < Duration::from_secs(10),
                "owned CLI did not join"
            );
            thread::sleep(Duration::from_millis(5));
        }
    }
    fn pid(&self) -> u32 {
        self.0.as_ref().unwrap().id()
    }
}
impl Drop for Running {
    fn drop(&mut self) {
        if let Some(child) = self.0.as_mut() {
            let _ = nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(child.id() as i32),
                nix::sys::signal::Signal::SIGINT,
            );
            let started = Instant::now();
            while child.try_wait().ok().flatten().is_none()
                && started.elapsed() < Duration::from_secs(3)
            {
                thread::sleep(Duration::from_millis(5));
            }
            if child.try_wait().ok().flatten().is_none() {
                let _ = child.kill();
            }
            let _ = child.wait();
        }
    }
}
fn read_pid(path: &Path) -> i32 {
    let started = Instant::now();
    loop {
        if let Ok(bytes) = fs::read_to_string(path)
            && let Ok(pid) = bytes.trim().parse::<i32>()
            && pid > 1
        {
            return pid;
        }
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "no valid retained PID in {}",
            path.display()
        );
        thread::sleep(Duration::from_millis(5));
    }
}
fn gone(pid: i32) {
    let started = Instant::now();
    loop {
        let status = fs::read_to_string(format!("/proc/{pid}/stat"));
        if status.is_err()
            || status.is_ok_and(|s| {
                s.split(')')
                    .nth(1)
                    .is_some_and(|s| s.trim_start().starts_with('Z'))
            })
        {
            return;
        }
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "native child {pid} remained running"
        );
        thread::sleep(Duration::from_millis(5));
    }
}

#[test]
fn cancellation_joins_each_evaluation_stage_and_keeps_core_without_later_publication() {
    for stage in 1..=3 {
        let fixture = Fixture::new();
        fixture
            .scenario(json!({"bootstrap":enabled(),"client":{},"slow_at":stage,"descendant":true}));
        let cli =
            Running::start(&mut fixture.command(&[OsStr::new("add"), OsStr::new("new")], 0o022));
        let native = read_pid(&fixture.control.join("pid"));
        let descendant = read_pid(&fixture.control.join("child-pid"));
        nix::sys::signal::kill(
            nix::unistd::Pid::from_raw(cli.pid() as i32),
            nix::sys::signal::Signal::SIGINT,
        )
        .unwrap();
        let result = cli.finish();
        assert_eq!(result.status.code(), Some(130), "{result:?}");
        gone(native);
        gone(descendant);
        assert_eq!(fixture.calls().len(), stage);
        assert_eq!(
            fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
            core(&["alpha", "nested/beta", "new"])
        );
        fixture.old_feature();
        fixture.unchanged_state();
    }
}

#[test]
fn cooperating_writer_remains_excluded_through_postwrite_child_join() {
    let fixture = Fixture::new();
    fixture.scenario(json!({"bootstrap":{},"slow_at":1}));
    let first = Running::start(&mut fixture.command(&[OsStr::new("rediscover")], 0o022));
    let _native = read_pid(&fixture.control.join("pid"));
    let second =
        Running::start(&mut fixture.command(&[OsStr::new("add"), OsStr::new("second")], 0o022));
    thread::sleep(Duration::from_millis(80));
    assert!(!fixture.root.join("cluster/second").exists());
    assert_eq!(fixture.calls().len(), 1);
    fs::write(fixture.control.join("release"), b"release").unwrap();
    assert!(first.finish().status.success());
    let result = second.finish();
    assert!(result.status.success(), "{result:?}");
    assert_eq!(fixture.calls().len(), 2);
    assert!(fixture.root.join("cluster/second/config.nix").exists());
    fixture.unchanged_state();
}

#[test]
fn output_failure_before_first_notice_has_no_core_effect_or_child() {
    let fixture = Fixture::new();
    let (writer, reader) = UnixStream::pair().unwrap();
    drop(reader);
    let result = fixture
        .command(&[OsStr::new("add"), OsStr::new("new")], 0o022)
        .stdout(Stdio::from(OwnedFd::from(writer)))
        .output()
        .unwrap();
    assert_eq!(result.status.code(), Some(1));
    assert!(!fixture.root.join("cluster/new").exists());
    assert!(fixture.calls().is_empty());
    assert_eq!(
        fs::read(fixture.root.join("cluster/cluster.nix")).unwrap(),
        b"old core\n"
    );
    fixture.old_feature();
    fixture.unchanged_state();
}
