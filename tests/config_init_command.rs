//! Real init executable; external commands are fail-on-call fakes.
#![cfg(target_os = "linux")]

use serde_json::Value;
use std::{
    fs,
    os::{
        fd::OwnedFd,
        unix::{
            fs::{PermissionsExt, symlink},
            net::UnixStream,
            process::CommandExt,
        },
    },
    path::PathBuf,
    process::{Command, Output, Stdio},
};

struct Fixture {
    _scratch: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
    calls: PathBuf,
}
impl Fixture {
    fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("configuration");
        let bin = scratch.path().join("bin");
        let calls = scratch.path().join("external-calls");
        fs::create_dir(&root).unwrap();
        fs::create_dir(&bin).unwrap();
        for name in [
            "nix",
            "nix-build",
            "nix-instantiate",
            "nix-shell",
            "ssh",
            "git",
            "nixfmt",
            "confctl",
        ] {
            let path = bin.join(name);
            fs::write(
                &path,
                b"#!/bin/sh\nprintf '%s\\n' \"$0\" >> \"$INIT_CALLS\"\nexit 97\n",
            )
            .unwrap();
            fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
        }
        Self {
            _scratch: scratch,
            root,
            bin,
            calls,
        }
    }
    fn command(&self, umask: u32) -> Command {
        let mut command = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        command
            .current_dir(&self.root)
            .args(["--color=never", "init"])
            .env("PATH", &self.bin)
            .env("INIT_CALLS", &self.calls)
            .env("CONFCTL_TTY", "0")
            .env("NO_COLOR", "1")
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        // Only the child changes its process-wide mask, before exec.
        unsafe {
            command.pre_exec(move || {
                rustix::process::umask(rustix::fs::Mode::from_raw_mode(umask));
                Ok(())
            });
        }
        command
    }
    fn run(&self, umask: u32) -> Output {
        self.command(umask).output().unwrap()
    }
    fn no_tools(&self) {
        assert!(!self.calls.exists(), "init called an external command");
        assert!(!self.root.join("flake.lock").exists());
        assert!(!self.root.join(".git").exists());
    }
}

fn expected() -> Value {
    serde_json::from_str(include_str!("compat/core/expected/init.json")).unwrap()
}

fn assert_templates(fixture: &Fixture, mask: u32) {
    for (path, value) in expected()["files"].as_object().unwrap() {
        let path = fixture.root.join(path);
        assert_eq!(
            fs::read(&path).unwrap(),
            value["text"].as_str().unwrap().as_bytes()
        );
        assert_eq!(
            fs::metadata(path).unwrap().permissions().mode() & 0o777,
            0o644 & !mask
        );
    }
    for directory in ["cluster", "configs", "data", "environments", "modules"] {
        assert_eq!(
            fs::metadata(fixture.root.join(directory))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o755 & !mask
        );
    }
    let mut entries = fs::read_dir(&fixture.root)
        .unwrap()
        .map(|entry| entry.unwrap().file_name())
        .collect::<Vec<_>>();
    entries.sort();
    assert_eq!(
        entries,
        [
            ".confctl",
            "cluster",
            "configs",
            "data",
            "environments",
            "flake.nix",
            "modules"
        ]
    );
    fixture.no_tools();
}

#[test]
fn empty_directory_initializes_exact_ruby_bytes_order_modes_without_consent_or_children() {
    let fixture = Fixture::new();
    let result = fixture.run(0o022);
    assert!(result.status.success(), "{result:?}");
    assert_eq!(
        result.stdout,
        expected()["stdout"].as_str().unwrap().as_bytes()
    );
    assert!(result.stderr.is_empty(), "{result:?}");
    assert_templates(&fixture, 0o022);
    assert!(fixture.root.join(".confctl/mutation.lock").is_file());
    assert!(
        fs::read_dir(fixture.root.join(".confctl/logs"))
            .unwrap()
            .next()
            .is_none()
    );
}

#[test]
fn restrictive_and_permissive_umasks_apply_without_chmod() {
    for mask in [0o077, 0] {
        let fixture = Fixture::new();
        let result = fixture.run(mask);
        assert!(result.status.success(), "{result:?}");
        assert_templates(&fixture, mask);
    }
}

#[test]
fn permitted_entries_and_canonical_cwd_are_preserved() {
    let fixture = Fixture::new();
    fs::write(fixture.root.join("shell.nix"), b"retained shell\n").unwrap();
    fs::write(fixture.root.join(".gitignore"), b"retained ignore\n").unwrap();
    fs::set_permissions(
        fixture.root.join(".gitignore"),
        fs::Permissions::from_mode(0o600),
    )
    .unwrap();
    for path in [".gems", ".confctl"] {
        fs::create_dir(fixture.root.join(path)).unwrap();
        fs::set_permissions(fixture.root.join(path), fs::Permissions::from_mode(0o750)).unwrap();
        fs::write(fixture.root.join(path).join("retained"), b"retained bytes").unwrap();
    }
    let alias = fixture._scratch.path().join("alias");
    symlink(&fixture.root, &alias).unwrap();
    let result = fixture.command(0o022).current_dir(alias).output().unwrap();
    assert!(result.status.success(), "{result:?}");
    assert_eq!(
        result.stdout,
        expected()["stdout"].as_str().unwrap().as_bytes()
    );
    assert_eq!(
        fs::read(fixture.root.join("shell.nix")).unwrap(),
        b"retained shell\n"
    );
    assert_eq!(
        fs::read(fixture.root.join(".gitignore")).unwrap(),
        b"retained ignore\n"
    );
    assert_eq!(
        fs::metadata(fixture.root.join(".gitignore"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
    for path in [".gems/retained", ".confctl/retained"] {
        assert_eq!(
            fs::read(fixture.root.join(path)).unwrap(),
            b"retained bytes"
        );
    }
    for path in [".gems", ".confctl"] {
        assert_eq!(
            fs::metadata(fixture.root.join(path))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o750
        );
    }
    fixture.no_tools();
}

#[test]
fn nonempty_roots_and_repeated_init_fail_without_altering_templates() {
    for entry in [".git", "flake.nix", "cluster", "unrelated"] {
        let fixture = Fixture::new();
        let marker = if [".git", "cluster"].contains(&entry) {
            fs::create_dir(fixture.root.join(entry)).unwrap();
            fixture.root.join(entry).join("retained")
        } else {
            fixture.root.join(entry)
        };
        fs::write(&marker, b"existing entry").unwrap();
        let result = fixture.run(0o022);
        assert_eq!(result.status.code(), Some(1), "{result:?}");
        assert!(result.stdout.is_empty());
        assert!(
            String::from_utf8_lossy(&result.stderr)
                .contains("init must be called in an empty directory")
        );
        assert_eq!(fs::read(marker).unwrap(), b"existing entry");
        assert!(!fixture.calls.exists());
        assert!(!fixture.root.join("configs").exists());
        assert!(!fixture.root.join(".confctl/mutation.lock").exists());
    }
    let fixture = Fixture::new();
    assert!(fixture.run(0o022).status.success());
    let result = fixture.run(0o077);
    assert_eq!(result.status.code(), Some(1), "{result:?}");
    assert!(result.stdout.is_empty());
    assert_templates(&fixture, 0o022);
}

#[test]
fn failed_redirected_notice_prevents_its_filesystem_effect() {
    let fixture = Fixture::new();
    let (writer, reader) = UnixStream::pair().unwrap();
    drop(reader);
    let result = fixture
        .command(0o022)
        .stdout(Stdio::from(OwnedFd::from(writer)))
        .output()
        .unwrap();
    assert_eq!(result.status.code(), Some(1), "{result:?}");
    assert!(!fixture.root.join("cluster").exists());
    assert!(!fixture.root.join("flake.nix").exists());
    assert!(!result.stderr.is_empty());
    fixture.no_tools();
}
