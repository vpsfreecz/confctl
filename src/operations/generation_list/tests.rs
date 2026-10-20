use super::super::generation_selection::*;
use super::super::host_status::HostGeneration;
use super::*;
use crate::{domain::UnifiedGenerationSelector, events::UiEvent, generation::BuildGeneration};
use chrono::{DateTime, Utc};
use serde_json::json;
use std::os::unix::fs::symlink;

fn generation(name: &str, date: &str, info: Value) -> BuildGeneration {
    BuildGeneration::decode(
        name,
        json!({
            "mode":"flakes", "date":date, "toplevel":"/nonexistent", "inputs_info":info
        }),
    )
    .unwrap()
}

#[test]
fn offsets_are_global_and_preserve_out_of_range_pattern_spelling() {
    for (text, expected) in [
        ("0", Some(3)),
        ("-0", Some(3)),
        ("0000", Some(3)),
        ("1", Some(0)),
        ("0002", Some(1)),
        ("4", Some(3)),
        ("-1", Some(2)),
        ("-0002", Some(1)),
        ("-3", Some(0)),
        ("-4", None),
        ("5", None),
        ("9999999999999999999999999999999999", None),
    ] {
        assert_eq!(offset_index(text, 4), expected, "{text}");
        assert_eq!(offset_index(text, 0), None, "empty/{text}");
    }
    let saved = generation("0009", "2000-01-01T00:00:00Z", json!({}));
    let mut entries = vec![Entry::local("alpha", &saved, true, None)];
    filter(
        &mut entries,
        &UnifiedGenerationSelector::Offset("0009".into()),
        Utc::now(),
    );
    assert_eq!(entries.len(), 1);
}

#[test]
fn days_have_a_strict_shared_cutoff_and_arbitrary_magnitudes() {
    let before = generation("before", "2026-10-18T11:59:59Z", json!({}));
    let equal = generation("equal", "2026-10-18T12:00:00Z", json!({}));
    let after = generation("after", "2026-10-18T12:00:01Z", json!({}));
    let now = "2026-10-19T12:00:00Z".parse::<DateTime<Utc>>().unwrap();
    for (days, expected) in [
        ("1", vec!["before"]),
        ("0001", vec!["before"]),
        ("0", vec!["before", "equal", "after"]),
        ("99999999999999999999999999999", vec![]),
    ] {
        let mut entries = [&before, &equal, &after]
            .into_iter()
            .map(|generation| Entry::local("host", generation, false, None))
            .collect();
        filter(
            &mut entries,
            &UnifiedGenerationSelector::OlderThanDays(days.into()),
            now,
        );
        assert_eq!(
            entries.iter().map(|e| e.name.as_str()).collect::<Vec<_>>(),
            expected
        );
    }
}

#[test]
fn table_keeps_first_seen_roles_before_sorting_and_preserves_saved_values() {
    let later = generation(
        "z",
        "2000-01-01T00:00:00Z",
        json!({"zrole":{"short_rev":"short","unknown":[7]},"raw":"1234567890"}),
    );
    let earlier = generation(
        "a",
        "2000-01-02T00:00:00Z",
        json!({"arole":{"rev":"abcdef012345"},"raw":false}),
    );
    let snapshot = later.encode();
    let UiEvent::Table { columns, rows, .. } = table(&[
        Entry::local("zhost", &later, true, Some("6.12".into())),
        Entry::local("ahost", &earlier, false, None),
    ]) else {
        panic!()
    };
    assert_eq!(
        columns,
        [
            "HOST", "NAME", "ID", "PRESENCE", "CURRENT", "KERNEL", "ZROLE", "RAW", "AROLE"
        ]
    );
    assert_eq!(
        rows[0],
        ["ahost", "a", "", "build", "", "", "", "", "abcdef01"]
    );
    assert_eq!(
        rows[1],
        [
            "zhost", "z", "", "build", "build", "6.12", "short", "12345678", ""
        ]
    );
    assert_eq!(later.encode(), snapshot);
    for (value, expected) in [
        (json!({"shortRev":"","rev":"abcdefghi"}), ""),
        (json!({"shortRev":false,"rev":"abcdefghi"}), "abcdefgh"),
        (json!({"short_rev":"one","shortRev":"two"}), "two"),
        (
            json!({"shortRev":"one","short_rev":null,"rev":"abcdefghi"}),
            "abcdefgh",
        ),
        (json!({"shortRev":[7,"saved"]}), "[7, \"saved\"]"),
    ] {
        assert_eq!(input_revision(Some(&value)), expected);
    }
}

#[test]
fn kernel_links_follow_both_system_conventions_and_propagate_io_errors() {
    let temp = tempfile::tempdir().unwrap();
    assert_eq!(kernel_version(temp.path()).unwrap(), None);
    fs::write(temp.path().join("kernel"), "regular file").unwrap();
    symlink(
        "/nix/store/abc-linux-6.12.3/bzImage",
        temp.path().join("bzImage"),
    )
    .unwrap();
    assert_eq!(
        kernel_version(temp.path()).unwrap().as_deref(),
        Some("6.12.3")
    );
    fs::remove_file(temp.path().join("kernel")).unwrap();
    symlink(
        "/nix/store/xyz-linux-6.13/bzImage",
        temp.path().join("kernel"),
    )
    .unwrap();
    assert_eq!(
        kernel_version(temp.path()).unwrap().as_deref(),
        Some("6.13")
    );
    let loop_path = temp.path().join("loop");
    symlink(&loop_path, &loop_path).unwrap();
    assert!(kernel_version(&loop_path).is_err());
}

#[test]
fn remote_first_unification_retains_build_distinctions_and_current_memberships() {
    let remote = |id: &str, current, kernel: &str| HostGeneration {
        id: id.into(),
        toplevel: "/nonexistent".into(),
        date: "2026-10-19T12:00:00Z"
            .parse::<DateTime<chrono::Local>>()
            .unwrap(),
        kernel: Some(kernel.into()),
        current,
    };
    let first = remote("1", true, "remote-first");
    let duplicate = remote("2", false, "remote-later");
    let saved = generation(
        "local-name",
        "2000-01-01T00:00:00Z",
        json!({"nixpkgs":"abcdefghijk"}),
    );
    let mut distinct = saved.clone();
    distinct.name = "other-name".into();
    let mut different_inputs = saved.clone();
    different_inputs
        .inputs
        .insert("nixpkgs".into(), json!("/other-input"));
    let mut entries = Vec::new();
    add_remote(&mut entries, "host", &first);
    add_remote(&mut entries, "host", &duplicate);
    add_build(&mut entries, "host", &saved, true, Some("local".into()));
    add_build(&mut entries, "host", &distinct, false, None);
    add_build(&mut entries, "host", &different_inputs, false, None);
    assert_eq!(entries.len(), 3);
    assert!(entries[0].current);
    assert_eq!(entries[0].date, saved.date);
    let UiEvent::Table { rows, .. } = table(&entries) else {
        panic!()
    };
    assert_eq!(
        &rows[0][..6],
        [
            "host",
            "local-name",
            "2",
            "build+host",
            "build",
            "remote-first"
        ]
    );
    assert_eq!(&rows[2][..6], ["host", "other-name", "", "build", "", ""]);

    let mut different_path = remote("3", false, "other");
    different_path.toplevel = "/nonexistent/./".into();
    let mut entries = Vec::new();
    add_remote(&mut entries, "host", &different_path);
    add_build(&mut entries, "host", &saved, false, None);
    assert_eq!(
        entries.len(),
        2,
        "toplevel matching uses saved bytes, not normalized path components"
    );
}
