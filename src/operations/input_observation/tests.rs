use super::*;
use crate::{
    error::{NixCommandContext, NixCommandFailure},
    process::PartialProcessEvidence,
};
use serde_json::json;
use std::os::unix::process::ExitStatusExt;
#[test]
fn normalized_revisions_keep_types_alias_order_and_full_equality() {
    let target=normalize(&json!({"string":"abcdefgh-one","typed":{"rev":7},"false":{"rev":false},"alias":{"shortRev":"one","short_rev":"two","rev":"full"},"unknown":{"extra":[7]}})).unwrap();
    assert_eq!(target["alias"]["shortRev"], "two");
    assert!(target["unknown"].as_object().unwrap().is_empty());
    assert_eq!(
        comparison(target.get("typed"), Some(&json!({"rev":"7"}))),
        Comparison::Changed
    );
    assert_eq!(
        comparison(target.get("false"), Some(&json!({"rev":false}))),
        Comparison::Same
    );
    assert_eq!(
        comparison(
            target.get("string"),
            Some(&json!({"rev":"abcdefgh-two","shortRev":"abcdefgh"}))
        ),
        Comparison::Changed
    );
    assert_eq!(
        comparison(target.get("unknown"), Some(&json!({"rev":null}))),
        Comparison::Unknown
    );
    assert!(payload(json!("{\"role\":\"revision\"}")).is_some());
    assert!(payload(json!("broken")).is_none());
    assert!(payload(json!([])).is_none());
}
#[test]
fn only_complete_failed_native_nix_eval_is_an_unknown_target() {
    let wrapped = |source, code, complete| {
        ConfctlError::NixCommandFailure(Box::new(NixCommandFailure {
            context: NixCommandContext {
                cwd: "/fixture".into(),
                program: "nix".into(),
                args: Vec::new(),
            },
            source: Box::new(ConfctlError::ProcessFailure {
                source: Box::new(source),
                evidence: Box::new(PartialProcessEvidence {
                    status: Some(std::process::ExitStatus::from_raw(code << 8)),
                    stdout: Captured::Memory(Vec::new()),
                    stderr: Captured::Memory(Vec::new()),
                    stdout_complete: complete,
                    stderr_complete: true,
                    log: None,
                }),
            }),
        }))
    };
    let child = || ConfctlError::Child {
        description: "nix eval".into(),
        code: Some(1),
        signal: None,
        log: None,
    };
    assert!(native_eval_failure(&wrapped(child(), 1, true)));
    assert!(!native_eval_failure(&wrapped(child(), 0, true)));
    assert!(!native_eval_failure(&wrapped(child(), 1, false)));
    assert!(!native_eval_failure(&wrapped(
        ConfctlError::Cancelled,
        1,
        true
    )));
    assert!(!native_eval_failure(&ConfctlError::Configuration(
        "bad configuration".into()
    )));
}

#[test]
fn machine_key_attribute_quoting_is_preserved() {
    assert_eq!(attribute("native-key"), "native-key");
    assert_eq!(attribute("nested/key.${x}\""), "\"nested/key.\\${x}\\\"\"");
}
