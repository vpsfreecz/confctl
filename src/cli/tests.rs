use super::*;
use crate::domain::*;

fn parse_words(words: &[&str]) -> Result<ParseOutcome, CliFailure> {
    parse(words.iter().map(OsString::from).collect())
}
fn action(words: &[&str]) -> Action {
    let ParseOutcome::Run(invocation) = parse_words(words).unwrap() else {
        panic!("expected action")
    };
    invocation.action
}

#[test]
fn channel_list_retains_operational_surplus_arguments_and_configuration_free_help() {
    assert_eq!(
        action(&["inputs", "channel", "ls", "base", "unexpected"]),
        Action::ChannelList {
            pattern: Some("base".into()),
            has_extra_arguments: true,
        }
    );
    assert_eq!(
        action(&["inputs", "channel", "ls"]),
        Action::ChannelList {
            pattern: None,
            has_extra_arguments: false,
        }
    );
    let ParseOutcome::Display(help) = parse_words(&["inputs", "channel", "ls", "--help"]).unwrap()
    else {
        panic!("help must not admit the operation")
    };
    assert_eq!(help, display_help("inputs channel ls"));
}

#[test]
fn every_frozen_route_help_and_option_probe_matches_static_baseline() {
    let manifest: serde_json::Value =
        serde_json::from_str(include_str!("../../tests/compat/core/coverage.json")).unwrap();
    for case in manifest["cases"].as_array().unwrap() {
        let id = case["id"].as_str().unwrap();
        if !id.starts_with("help-") && !id.starts_with("option-") {
            continue;
        }
        let argv = case["argv"]
            .as_array()
            .unwrap()
            .iter()
            .map(|v| OsString::from(v.as_str().unwrap()))
            .collect();
        let ParseOutcome::Display(actual) =
            parse(argv).unwrap_or_else(|error| panic!("{id}: {error:?}"))
        else {
            panic!("{id}: help did not display")
        };
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join(format!("tests/compat/core/expected/{id}.json"));
        let expected: serde_json::Value =
            serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert_eq!(
            String::from_utf8(actual.stdout).unwrap(),
            expected["stdout"],
            "{id}"
        );
        assert_eq!(
            String::from_utf8(actual.stderr).unwrap(),
            expected["stderr"],
            "{id}"
        );
        assert_eq!(actual.code, expected["exit"], "{id}");
    }
}

#[test]
fn command_prefix_version_and_usage_streams_preserve_gli_contract() {
    assert!(matches!(action(&["redi"]), Action::Rediscover));
    assert_eq!(
        parse_words(&["c"]).unwrap_err().message,
        "Ambiguous command 'c'. It matches changelog,collect-garbage,cssh"
    );
    let ParseOutcome::Display(version) = parse_words(&["--version"]).unwrap() else {
        panic!()
    };
    assert_eq!(version.code, 1);
    assert!(version.stdout.is_empty());
    assert_eq!(
        version.stderr,
        b"confctl: version unknown\nerror: confctl: version unknown\n"
    );
    assert_eq!(
        parse_words(&["deploy", "--outdated"]).unwrap_err().message,
        "Unknown option --outdated"
    );
    let invalid = parse_words(&["add", "foo", "--yes"]).unwrap_err();
    assert_eq!(invalid.display.code, 64);
    assert!(invalid.requires_log);
    assert_eq!(
        invalid.message,
        "unknown argument: --yes (note that options must come before arguments)"
    );
    assert!(matches!(
        action(&["ls", "alpha", "ignored"]),
        Action::List { .. }
    ));
}

#[test]
fn typed_defaults_negations_and_forwarded_nix_values_are_distinct() {
    let Action::Deploy(request) =
        action(&["deploy", "--yes", "--no-yes", "--cores", "auto", "alpha"])
    else {
        panic!()
    };
    assert!(!request.build.targets.yes);
    assert_eq!(request.action, SwitchAction::Switch);
    assert_eq!(request.max_concurrent_copy, 5);
    assert!(request.health_checks);
    assert_eq!(request.build.nix.cores.as_deref(), Some("auto"));
    for spelling in ["--allow_shared", "--allow-shared"] {
        let Action::ChannelSet {
            allow_shared,
            changes,
            ..
        } = action(&[
            "inputs", "channel", "set", spelling, "base", "nixpkgs", "deadbeef",
        ])
        else {
            panic!()
        };
        assert!(allow_shared);
        assert!(changes.changelog && changes.editor);
        assert!(!changes.commit);
    }
    assert!(matches!(
        action(&["deploy", "--wait-online", "invalid", "alpha"]),
        Action::Deploy(_)
    ));
    assert!(parse_words(&["deploy", "--reboot", "alpha"]).is_err());
    assert!(
        parse_words(&[
            "deploy",
            "--reboot",
            "--wait-online",
            "invalid",
            "alpha",
            "boot"
        ])
        .is_err()
    );
    let Action::GenerationRemove { locations, gc, .. } =
        action(&["generation", "rm", "--no-gc", "alpha"])
    else {
        panic!()
    };
    assert!(locations.local && !locations.remote);
    assert!(!gc.enabled);
}

#[test]
fn ssh_native_argv_is_not_reparsed_and_timing_admission_is_closed() {
    use std::os::unix::ffi::OsStringExt;
    let native = OsString::from_vec(vec![0xff, b';', b'$']);
    let args = vec![
        "ssh".into(),
        "alpha".into(),
        "printf".into(),
        native.clone(),
        "--yes".into(),
    ];
    let ParseOutcome::Run(invocation) = parse(args).unwrap() else {
        panic!()
    };
    let Action::Ssh(ref ssh) = invocation.action else {
        panic!()
    };
    assert_eq!(
        ssh.command,
        vec![OsString::from("printf"), native, OsString::from("--yes")]
    );
    assert!(!ssh.targets.yes);
    assert!(invocation.action.timing_admitted());
    assert!(!action(&["ssh", "alpha"]).timing_admitted());
    assert!(!action(&["cssh", "alpha"]).timing_admitted());
    assert!(!action(&["runtime-kernels", "update"]).timing_admitted());
    assert!(action(&["generation", "rotate", "--no-gc"]).timing_admitted());
}

#[test]
fn frozen_non_help_cases_classify_parser_usage_without_running_operations() {
    let manifest: serde_json::Value =
        serde_json::from_str(include_str!("../../tests/compat/core/coverage.json")).unwrap();
    for case in manifest["cases"].as_array().unwrap() {
        let id = case["id"].as_str().unwrap();
        if id.starts_with("help-")
            || id.starts_with("option-")
            || case["mode"].as_str().unwrap().starts_with("api")
        {
            continue;
        }
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join(format!("tests/compat/core/expected/{id}.json"));
        if !path.exists() {
            continue;
        }
        let expected: serde_json::Value =
            serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        let argv = case["argv"]
            .as_array()
            .unwrap()
            .iter()
            .map(|v| OsString::from(v.as_str().unwrap()))
            .collect();
        let actual = parse(argv);
        if expected["exit"] == 64 {
            assert!(actual.is_err(), "{id}: expected usage");
        } else {
            assert!(actual.is_ok(), "{id}: {actual:?}");
        }
    }
}

#[test]
fn unified_generation_offsets_preserve_exact_ruby_integer_spelling() {
    for value in [
        "0",
        "01",
        "-00",
        "-007",
        "9223372036854775808",
        "-99999999999999999999999999999999999999",
    ] {
        let Action::GenerationList { selector, .. } =
            action(&["generation", "ls", "--", "*", value])
        else {
            panic!("generation list")
        };
        assert_eq!(selector, UnifiedGenerationSelector::Offset(value.into()));
    }
    for value in ["+1", "-", "1.0", "1e3", "--1", "１２"] {
        let Action::GenerationList { selector, .. } =
            action(&["generation", "ls", "--", "*", value])
        else {
            panic!("generation list")
        };
        assert_eq!(selector, UnifiedGenerationSelector::Pattern(value.into()));
    }
}
