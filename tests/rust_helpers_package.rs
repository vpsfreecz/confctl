//! Native Cargo helper contracts; evaluated Nix wrappers need separate qualification.
use std::{
    ffi::{OsStr, OsString},
    fs,
    io::Write,
    os::unix::{ffi::OsStringExt, fs::PermissionsExt, process::CommandExt},
    path::Path,
    process::{Command, Output, Stdio},
};

fn executable(path: &Path, body: &str) {
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(path)
        .unwrap();
    file.write_all(format!("#!/bin/sh\n{body}\n").as_bytes())
        .unwrap();
    file.set_permissions(fs::Permissions::from_mode(0o755))
        .unwrap();
    drop(file);
}

fn helper(program: &str) -> Command {
    let mut command = Command::new(program);
    for field in [
        "CONFCTL_INTERNAL_CARRIER_ON_CHANGE",
        "CONFCTL_INTERNAL_NETBOOT_CONFIG",
        "CONFCTL_INTERNAL_KEXEC_MACHINE_FQDN",
        "CONFCTL_INTERNAL_KEXEC_EXECUTABLE",
    ] {
        command.env_remove(field);
    }
    command
}

fn with_input(command: &mut Command, input: &[u8]) -> Output {
    let mut child = command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child.stdin.take().unwrap().write_all(input).unwrap();
    child.wait_with_output().unwrap()
}

#[test]
fn parser_help_keeps_argv0_and_precedes_private_package_data() {
    for program in [
        env!("CARGO_BIN_EXE_carrier-env"),
        env!("CARGO_BIN_EXE_kexec-netboot"),
        env!("CARGO_BIN_EXE_confctl-auto-rollback"),
    ] {
        let output = helper(program)
            .arg0("literal helper name")
            .arg("--help")
            .output()
            .unwrap();
        assert!(output.status.success(), "{output:?}");
        let usage = b"Usage: literal helper name ";
        assert!(
            output.stdout.windows(usage.len()).any(|part| part == usage),
            "{output:?}"
        );
        assert!(output.stderr.is_empty(), "{output:?}");
    }
    let output = helper(env!("CARGO_BIN_EXE_build-netboot-server"))
        .output()
        .unwrap();
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    assert!(String::from_utf8_lossy(&output.stderr).contains("CONFCTL_INTERNAL_NETBOOT_CONFIG"));
}

#[test]
fn one_carrier_binary_accepts_distinct_native_callbacks_and_keeps_context_and_order() {
    let dir = tempfile::tempdir().unwrap();
    executable(
        &dir.path().join("nix-env"),
        "printf 'nix\\0'; printf '%s\\0' \"$@\"; exit \"${NATIVE_EXIT:-0}\"",
    );
    let logical = dir.path().join("logical working directory");
    std::os::unix::fs::symlink(".", &logical).unwrap();
    for (number, label) in [(1, "first"), (2, "second")] {
        let callback = dir.path().join(OsString::from_vec(
            format!("callback {number}'")
                .into_bytes()
                .into_iter()
                .chain([0xff])
                .collect(),
        ));
        executable(
            &callback,
            &format!(
                "printf '{label}\\0%s\\0' \"$PWD\"; IFS= read -r line; printf '%s\\0' \"$line\""
            ),
        );
        let mut command = helper(env!("CARGO_BIN_EXE_carrier-env"));
        command
            .current_dir(dir.path())
            .env("PWD", &logical)
            .env("PATH", dir.path())
            .env("NATIVE_EXIT", "0")
            .env("CONFCTL_INTERNAL_CARRIER_ON_CHANGE", &callback)
            .args(["-p", "profile ' $ space", "--set", ""]);
        let output = with_input(&mut command, b"stdin byte marker\n");
        assert!(output.status.success(), "{output:?}");
        assert_eq!(
            output.stdout,
            format!(
                "nix\0-p\0profile ' $ space\0--set\0\0{label}\0{}\0stdin byte marker\0",
                logical.display()
            )
            .as_bytes(),
            "{output:?}"
        );
        assert!(output.stderr.is_empty(), "{output:?}");
        let output = command.env("NATIVE_EXIT", "7").output().unwrap();
        assert_eq!(output.status.code(), Some(1), "{output:?}");
        assert_eq!(output.stdout, b"nix\0-p\0profile ' $ space\0--set\0\0");
        assert!(String::from_utf8_lossy(&output.stderr).contains("nix-env"));
    }
}

#[test]
fn empty_kexec_fqdn_and_literal_absolute_backend_keep_native_arguments_and_failure() {
    let dir = tempfile::tempdir().unwrap();
    let backend = dir.path().join("kexec ' native");
    executable(
        &backend,
        "printf '%s\\0' \"$@\" > \"$RECORD\"; exit \"${NATIVE_EXIT:-0}\"",
    );
    let record = dir.path().join("argv");
    let mut command = helper(env!("CARGO_BIN_EXE_kexec-netboot"));
    command
        .env("CONFCTL_INTERNAL_KEXEC_MACHINE_FQDN", "")
        .env("CONFCTL_INTERNAL_KEXEC_EXECUTABLE", &backend)
        .env("RECORD", &record)
        .env("NATIVE_EXIT", "0")
        .arg("--unload");
    let output = command.output().unwrap();
    assert!(output.status.success(), "{output:?}");
    assert_eq!(fs::read(&record).unwrap(), b"--unload\0--no-sync\0");
    let output = command.env("NATIVE_EXIT", "9").output().unwrap();
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    assert!(String::from_utf8_lossy(&output.stderr).contains("kexec --unload failed"));
    let output = command
        .env("CONFCTL_INTERNAL_KEXEC_EXECUTABLE", OsStr::new("relative"))
        .output()
        .unwrap();
    assert_eq!(output.status.code(), Some(1), "{output:?}");
    assert!(String::from_utf8_lossy(&output.stderr).contains("CONFCTL_INTERNAL_KEXEC_EXECUTABLE"));
}
