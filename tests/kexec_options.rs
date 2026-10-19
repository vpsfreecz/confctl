use std::{os::unix::fs::PermissionsExt, process::Command};

#[test]
fn invalid_kexec_options_never_reach_the_executable() {
    let dir = tempfile::tempdir().unwrap();
    let executable = dir.path().join("kexec");
    let sentinel = dir.path().join("kexec.ran");
    std::fs::write(&executable, b"#!/bin/sh\nprintf ran > \"$0.ran\"\nexit 0\n").unwrap();
    std::fs::set_permissions(&executable, std::fs::Permissions::from_mode(0o755)).unwrap();

    for (argument, diagnostic) in [
        ("-eu", "use either --unload or --exec, not both"),
        ("-eZ", "invalid option: -Z"),
        ("--exec=garbage", "option takes no argument: --exec"),
    ] {
        let output = Command::new(env!("CARGO_BIN_EXE_kexec-netboot"))
            .arg(argument)
            .env("CONFCTL_INTERNAL_KEXEC_MACHINE_FQDN", "node.invalid")
            .env("CONFCTL_INTERNAL_KEXEC_EXECUTABLE", &executable)
            .output()
            .unwrap();
        assert_eq!(output.status.code(), Some(1), "{argument}");
        assert!(output.stdout.is_empty(), "{argument}");
        assert!(
            String::from_utf8_lossy(&output.stderr).contains(diagnostic),
            "{argument}: {:?}",
            output.stderr
        );
        assert!(!sentinel.exists(), "{argument} executed kexec");
    }
}
