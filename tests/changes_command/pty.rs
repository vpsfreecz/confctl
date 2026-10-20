//! Reexecute one test in an isolated controlling session, as existing PTY tests do.
use super::*;
use std::{io, os::fd::OwnedFd};

const TEST: &str = "pty::actual_comparison_pager_tty_policy_and_early_quit";
const CASE: &str = "CONFCTL_CHANGES_PTY_CASE";

#[test]
fn actual_comparison_pager_tty_policy_and_early_quit() {
    if let Ok(case) = std::env::var(CASE) {
        child(&case);
        return;
    }
    for case in [
        "000", "001", "010", "011", "100", "101", "110", "111", "disabled", "blank", "missing",
        "priority", "empty", "exit7", "invalid", "early",
    ] {
        let (master, slave) = open_pty();
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", TEST, "--nocapture"])
            .env(CASE, case)
            .stdin(Stdio::from(slave.try_clone().unwrap()))
            .stdout(Stdio::from(slave.try_clone().unwrap()))
            .stderr(Stdio::from(slave))
            .spawn()
            .unwrap();
        let flags = rustix::fs::fcntl_getfl(&master).unwrap();
        rustix::fs::fcntl_setfl(&master, flags | rustix::fs::OFlags::NONBLOCK).unwrap();
        let deadline = Instant::now() + Duration::from_secs(25);
        let mut bytes = Vec::new();
        let status = loop {
            drain(&master, &mut bytes);
            if let Some(status) = child.try_wait().unwrap() {
                drain(&master, &mut bytes);
                break status;
            }
            if Instant::now() >= deadline {
                child.kill().unwrap();
                let _ = child.wait();
                panic!("PTY {case} hung: {}", String::from_utf8_lossy(&bytes));
            }
            thread::sleep(Duration::from_millis(5));
        };
        assert!(
            status.success(),
            "PTY {case}: {}",
            String::from_utf8_lossy(&bytes)
        );
        assert!(String::from_utf8_lossy(&bytes).contains("COMPARISON_PTY_OK"));
    }
}

fn child(case: &str) {
    rustix::process::setsid().unwrap();
    rustix::process::ioctl_tiocsctty(io::stdin()).unwrap();
    let modes = rustix::termios::tcgetattr(io::stdin()).unwrap();
    let group = rustix::process::getpgrp();
    let f = Fixture::new();
    symlink("/bin/sh", f.bin.join("sh")).unwrap();
    executable(
        &f.bin.join("fake-pager"),
        r#"#!/bin/sh
printf '%s\0' "$@" > "$CHANGES_ROOT/pager-argv"
if [ "$CHANGES_PAGER_CASE" = early ]; then
 IFS= read -r first
 while [ ! -e "$CHANGES_ROOT/git-log-entered" ]; do /run/current-system/sw/bin/sleep 0.01; done
 exit 0
fi
"$CHANGES_ROOT/native/cat" > "$CHANGES_ROOT/pager-report"
if [ "$CHANGES_PAGER_CASE" = exit7 ]; then exit 7; fi
"#,
    );
    let all = matches!(
        case,
        "disabled" | "blank" | "missing" | "priority" | "empty" | "exit7" | "invalid" | "early"
    );
    let (stdout_tty, stderr_tty, stdin_tty) = if all {
        (true, true, case != "early")
    } else {
        let bytes = case.as_bytes();
        (bytes[0] == b'1', bytes[1] == b'1', bytes[2] == b'1')
    };
    let args = if case == "early" {
        vec!["changelog", "--yes", "--tag=chosen"]
    } else if case == "empty" {
        vec!["diff", "--yes", "alpha", "no-matching-role"]
    } else {
        vec!["diff", "--yes", "alpha"]
    };
    if case == "early" {
        fs::write(f.root.join("large-log"), vec![b'x'; 3 * 1024 * 1024]).unwrap();
        f.json(
            "target-a",
            json!({"a":{"url":"repo","rev":"new-a"},"z":{"url":"repo","rev":"new-z"}}),
        );
        f.json("deployed-a", json!({"a":"old-a","z":"old-z"}));
    }
    let mut command = f.command(&args);
    command
        .env("CONFCTL_TTY", if case == "disabled" { "0" } else { "1" })
        .env("PAGER", "fake-pager --preserved-argument")
        .env_remove("GIT_PAGER")
        .env("CHANGES_PAGER_CASE", case)
        .stdin(if stdin_tty {
            Stdio::inherit()
        } else {
            Stdio::piped()
        })
        .stdout(if stdout_tty {
            Stdio::inherit()
        } else {
            Stdio::piped()
        })
        .stderr(if stderr_tty {
            Stdio::inherit()
        } else {
            Stdio::piped()
        });
    if case == "blank" {
        command.env("PAGER", " \t\r\n");
    }
    if case == "missing" {
        command.env_remove("PAGER");
    }
    if case == "priority" {
        command.env("GIT_PAGER", "fake-pager --git-priority");
    }
    if case == "invalid" {
        command.env("PAGER", "missing-pager --never-strip");
    }
    let output = command.spawn().unwrap().wait_with_output().unwrap();
    assert_eq!(output.status.code(), Some(0), "{case}: {output:?}");
    let expected = stdout_tty
        && stderr_tty
        && !matches!(case, "disabled" | "blank" | "missing" | "empty" | "invalid");
    assert_eq!(f.root.join("pager-argv").exists(), expected, "{case}");
    if expected && case != "early" {
        let report = fs::read_to_string(f.root.join("pager-report")).unwrap();
        assert!(report.contains("alpha @ main"), "{report}");
        let args = fs::read(f.root.join("pager-argv")).unwrap();
        assert_eq!(
            args,
            if case == "priority" {
                b"--git-priority\0".as_slice()
            } else {
                b"--preserved-argument\0".as_slice()
            }
        );
    }
    if !stdout_tty {
        assert!(
            String::from_utf8_lossy(&output.stdout).contains("alpha @ main"),
            "{case}: {output:?}"
        );
    }
    if case == "early" {
        let calls = f.calls();
        assert!(!calls.iter().any(|s| s.starts_with("ssh:")), "{calls:?}");
        assert_eq!(
            calls.iter().filter(|s| s.starts_with("git:")).count(),
            2,
            "{calls:?}"
        );
        assert!(f.mirror("repo").join("fixture-cache").exists());
    } else {
        assert!(!f.calls().iter().any(|s| s.starts_with("git:")));
        assert!(!f.cache.exists());
    }
    f.assert_unchanged();
    assert_eq!(rustix::termios::tcgetpgrp(io::stdin()).unwrap(), group);
    let restored = rustix::termios::tcgetattr(io::stdin()).unwrap();
    assert_eq!(modes.local_modes, restored.local_modes);
    assert_eq!(modes.input_modes, restored.input_modes);
    assert_eq!(modes.output_modes, restored.output_modes);
    println!("COMPARISON_PTY_OK");
}

fn open_pty() -> (OwnedFd, OwnedFd) {
    use rustix::pty::{OpenptFlags, grantpt, ioctl_tiocgptpeer, openpt, unlockpt};
    let flags = OpenptFlags::RDWR | OpenptFlags::NOCTTY | OpenptFlags::CLOEXEC;
    let master = openpt(flags).unwrap();
    grantpt(&master).unwrap();
    unlockpt(&master).unwrap();
    let slave = ioctl_tiocgptpeer(&master, flags).unwrap();
    (master, slave)
}
fn drain(master: &OwnedFd, bytes: &mut Vec<u8>) {
    loop {
        let mut chunk = [0; 4096];
        match rustix::io::read(master, &mut chunk) {
            Ok(0) | Err(rustix::io::Errno::AGAIN) | Err(rustix::io::Errno::IO) => break,
            Ok(count) => bytes.extend_from_slice(&chunk[..count]),
            Err(rustix::io::Errno::INTR) => {}
            Err(error) => panic!("PTY read: {error}"),
        }
    }
}
