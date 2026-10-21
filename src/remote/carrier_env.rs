//! Carrier profile mutation and its one generated on-change executable.
//! The package supplies the executable; it is never interpreted as a command
//! string. A failed profile operation does not run the callback, and a failed
//! callback does not invent a rollback of the completed profile mutation.

include!("carrier_env/common.rs");

pub fn entry() -> i32 {
    let mut args = std::env::args_os();
    let program = args.next().unwrap_or_else(|| "carrier-env".into());
    let args = args.collect::<Vec<_>>();
    common_entry(&program, &args, &mut io::stdout(), &mut io::stderr())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::{fs::PermissionsExt, process::ExitStatusExt};

    fn executable(path: &Path, body: &str) {
        // Keep the writer outside this multithreaded process: a concurrent
        // fork can inherit a writable descriptor before its CLOEXEC takes effect.
        let status = Command::new("/bin/sh")
            .args(["-c", "set -C; printf '%s' \"$2\" > \"$1\"", "write-fixture"])
            .arg(path)
            .arg(format!("#!/bin/sh\n{body}\n"))
            .status()
            .unwrap();
        assert!(status.success(), "fixture writer failed: {status}");
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755)).unwrap();
    }

    #[test]
    fn helper_diag_01_command_primaries_keep_exact_lf_and_owned_native_status() {
        let original: serde_json::Value = serde_json::from_str(include_str!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/tests/compat/helpers/candidate/diagnostics-v1.json"
        )))
        .unwrap();
        let dir = tempfile::tempdir().unwrap();
        executable(&dir.path().join("nix-env"), "exit 3");
        executable(&dir.path().join("on-change"), "exit 9");
        let profile = dir.path().join("profile");
        for (row, code) in [("carrier-nix-failure", 3), ("carrier-callback-failure", 9)] {
            let mut command = if row == "carrier-nix-failure" {
                let mut command = Command::new("nix-env");
                command.env("PATH", dir.path()).args([
                    OsStr::new("-p"),
                    profile.as_os_str(),
                    OsStr::new("--set"),
                    OsStr::new("/nix/store/fixture"),
                ]);
                command
            } else {
                Command::new(dir.path().join("on-change"))
            };
            let error = checked(&mut command).unwrap_err();
            let source = error
                .get_ref()
                .unwrap()
                .downcast_ref::<CommandFailure>()
                .unwrap();
            assert_eq!(source.status.code(), Some(code));
            assert_eq!(source.status.signal(), None);
            assert!(!format!("{error:?}").contains(dir.path().to_str().unwrap()));
            let mut stderr = Vec::new();
            assert_eq!(finish(Err(error), &mut stderr), 1);
            assert_eq!(
                stderr,
                original["rows"][row]["expected"]["stderr"]
                    .as_str()
                    .unwrap()
                    .replace("@ROOT@", dir.path().to_str().unwrap())
                    .as_bytes()
            );
        }
    }

    #[test]
    fn native_set_delete_and_failures_preserve_callback_order_and_completed_effects() {
        let dir = tempfile::tempdir().unwrap();
        let record = dir.path().join("events");
        let record = format!("'{}'", record.to_str().unwrap().replace('\'', "'\\''"));
        // Every case publishes its own immutable executable paths.
        let fixture = |case: &str, nix_body: &str, callback_body: &str| {
            let case_dir = dir.path().join(case);
            std::fs::create_dir(&case_dir).unwrap();
            let nix = case_dir.join("nix-env");
            let callback = case_dir.join("on-change");
            executable(&nix, nix_body);
            executable(&callback, callback_body);
            (nix, callback)
        };
        let context = |nix: &Path, callback: &Path| {
            format!(
                "nix={nix:?}, bytes={:?}; callback={callback:?}, bytes={:?}; events={:?}",
                std::fs::read(nix),
                std::fs::read(callback),
                std::fs::read(dir.path().join("events"))
            )
        };
        for (action, flag, value) in [
            (
                Action::Set("/nix/store/system".into()),
                "--set",
                "/nix/store/system",
            ),
            (Action::Delete("3 4".into()), "--delete-generations", "3 4"),
        ] {
            let (nix, callback) = fixture(
                flag,
                &format!("printf '%s\\n' nix-env \"$@\" >> {record}"),
                &format!("printf '%s\\n' on-change >> {record}"),
            );
            assert_eq!(
                execute(
                    &CarrierEnvConfig {
                        on_change: callback.clone()
                    },
                    Options {
                        profile: "/fixture/profile".into(),
                        action
                    },
                    &nix
                )
                .unwrap_or_else(|error| panic!(
                    "{flag} {value}: {error:?}; {}",
                    context(&nix, &callback)
                )),
                0
            );
            assert_eq!(
                std::fs::read_to_string(dir.path().join("events")).unwrap(),
                format!("nix-env\n-p\n/fixture/profile\n{flag}\n{value}\non-change\n")
            );
            std::fs::remove_file(dir.path().join("events")).unwrap();
        }
        let options = || Options {
            profile: "/fixture/profile".into(),
            action: Action::Set("/system".into()),
        };
        let (nix, callback) = fixture(
            "profile-failure",
            &format!("printf '%s\\n' nix-env >> {record}\nexit 3"),
            &format!("printf '%s\\n' on-change >> {record}"),
        );
        let error = execute(
            &CarrierEnvConfig {
                on_change: callback.clone(),
            },
            options(),
            &nix,
        )
        .unwrap_err();
        assert!(
            error.to_string().contains("failed"),
            "profile-failure, set /system: {error:?}; {}",
            context(&nix, &callback)
        );
        assert_eq!(
            std::fs::read_to_string(dir.path().join("events")).unwrap(),
            "nix-env\n"
        );
        std::fs::remove_file(dir.path().join("events")).unwrap();
        let (nix, callback) = fixture(
            "callback-failure",
            &format!("printf '%s\\n' nix-env >> {record}"),
            &format!("printf '%s\\n' on-change >> {record}\nexit 9"),
        );
        let error = execute(
            &CarrierEnvConfig {
                on_change: callback.clone(),
            },
            options(),
            &nix,
        )
        .unwrap_err();
        assert!(
            error.to_string().contains("on-change failed"),
            "callback-failure, set /system: {error:?}; {}",
            context(&nix, &callback)
        );
        assert_eq!(
            std::fs::read_to_string(dir.path().join("events")).unwrap(),
            "nix-env\non-change\n"
        );
        // The configured empty lines value still generates a real executable.
        let (nix, callback) = fixture(
            "empty-callback",
            &format!("printf '%s\\n' nix-env >> {record}"),
            "",
        );
        assert_eq!(
            execute(
                &CarrierEnvConfig {
                    on_change: callback.clone()
                },
                options(),
                &nix
            )
            .unwrap_or_else(|error| panic!(
                "empty-callback, set /system: {error:?}; {}",
                context(&nix, &callback)
            )),
            0
        );
    }

    #[test]
    fn required_flags_last_action_and_generation_argument_match_ruby_oracle() {
        let args = [
            "-p",
            "/fixture/profile",
            "--set",
            "/nix/store/system",
            "--delete-generations",
            "3 4",
        ]
        .map(OsString::from);
        let Parsed::Run(options) = parse(&args).unwrap() else {
            panic!("valid carrier mutation")
        };
        assert_eq!(
            options,
            Options {
                profile: "/fixture/profile".into(),
                action: Action::Delete("3 4".into())
            }
        );
        for (args, message) in [
            (vec![], "No action specified"),
            (vec!["--set", "/nix/store/system"], "Profile not set"),
        ] {
            let (mut out, mut err) = (Vec::new(), Vec::new());
            assert_eq!(
                prepare(
                    OsStr::new("carrier-env"),
                    &args.into_iter().map(OsString::from).collect::<Vec<_>>(),
                    &mut out,
                    &mut err
                )
                .unwrap()
                .unwrap_err(),
                1
            );
            assert!(out.is_empty());
            assert_eq!(String::from_utf8(err).unwrap(), format!("{message}\n"));
        }
    }

    #[test]
    fn surplus_arguments_follow_carrier_01_without_any_child_effect() {
        let (mut out, mut err) = (Vec::new(), Vec::new());
        assert_eq!(
            prepare(
                OsStr::new("carrier-env"),
                &["-p", "/profile", "--set", "/system", "surplus"].map(OsString::from),
                &mut out,
                &mut err
            )
            .unwrap()
            .unwrap_err(),
            1
        );
        assert_eq!(err, b"Too many arguments\n");
        assert_eq!(out, help(OsStr::new("carrier-env")).as_bytes());
    }
}
