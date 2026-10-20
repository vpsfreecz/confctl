use super::*;
use serde_json::json;
#[test]
fn gcroot_login_join_matches_ten_immutable_native_byte_observations() {
    use std::os::unix::ffi::{OsStrExt, OsStringExt};
    let path =
        Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/compat/core/expected-login-join.json");
    let fixture: Value = serde_json::from_slice(&fs::read(path).unwrap()).unwrap();
    assert_eq!(
        fixture["baseline"],
        "cc40679d267165aecfa569128438bb55fe910268"
    );
    for (name, login) in [
        ("empty", b"".as_slice()),
        ("relative", b"fixture"),
        ("leading", b"/fixture"),
        ("embedded", b"fixture/middle"),
        ("trailing", b"fixture/"),
        ("repeated", b"//fixture///middle//"),
        ("dots", b"./fixture/../login"),
        ("root", b"/"),
        ("non-utf8", b"f\xffx"),
    ] {
        let result = login_directory(
            OsStr::new("<ROOT>"),
            &OsString::from_vec(login.to_vec()),
            "confctl-fixture",
        );
        let hex = result
            .as_os_str()
            .as_bytes()
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        assert_eq!(
            hex,
            fixture["cases"][name]["path_hex"].as_str().unwrap(),
            "{name}"
        );
    }
    assert_eq!(
        fixture["cases"]["nil"]["error"]["message"],
        "no implicit conversion of nil into String"
    );
}
fn fixture() -> (tempfile::TempDir, GenerationStore) {
    let dir = tempfile::tempdir().unwrap();
    let root = ConfigRoot::new(dir.path()).unwrap();
    let roots = GcRoots::new(&root, dir.path().join("test-gcroots"), "fixture").unwrap();
    (dir, GenerationStore::new(root, roots))
}
fn generation(name: &str) -> BuildGeneration {
    BuildGeneration::decode(name,json!({"mode":"flakes","date":"2000-01-01T00:00:00Z","toplevel":"/nix/store/system","auto_rollback":"/nix/store/rollback","inputs":{"nixpkgs":"/nix/store/input"},"inputsInfo":{"nixpkgs":{"unknown":{"field":7}}}})).unwrap()
}
#[tokio::test]
async fn canonical_round_trip_roots_and_current_published_last() {
    let (_dir, store) = fixture();
    let generation = generation("2000-01-01--00-00-00");
    let guard = crate::configuration::MutationLock::acquire(
        &store.root,
        &tokio_util::sync::CancellationToken::new(),
    )
    .await
    .unwrap();
    store.publish(&guard, "nested/alpha", &generation).unwrap();
    let inventory = store.load("nested/alpha").unwrap();
    let saved = inventory
        .select("nested/alpha", "current")
        .unwrap()
        .unwrap();
    assert_eq!(saved.inputs_info, generation.inputs_info);
    assert_eq!(
        fs::read_link(
            store
                .host_directory("nested/alpha")
                .unwrap()
                .join("current")
        )
        .unwrap(),
        Path::new(&generation.name)
    );
    assert!(
        store
            .roots
            .directory()
            .join("nested:alpha-generation-2000-01-01--00-00-00-input.nixpkgs")
            .is_symlink()
    );
    let value: Value = serde_json::from_slice(
        &fs::read(
            store
                .host_directory("nested/alpha")
                .unwrap()
                .join(&generation.name)
                .join("generation.json"),
        )
        .unwrap(),
    )
    .unwrap();
    assert!(value.get("inputsInfo").is_none());
    assert!(value.get("inputs_info").is_some());
}
#[test]
fn excluded_records_and_invalid_current_are_never_removed() {
    let (_dir, store) = fixture();
    let host = store.host_directory("alpha").unwrap();
    fs::create_dir_all(host.join("old")).unwrap();
    let raw = b"{\"mode\":\"swpins\"}";
    fs::write(host.join("old/generation.json"), raw).unwrap();
    std::os::unix::fs::symlink("old", host.join("current")).unwrap();
    let inventory = store.load("alpha").unwrap();
    assert!(inventory.select("alpha", "current").is_err());
    assert!(inventory.select("alpha", "0").is_err());
    assert_eq!(fs::read(host.join("old/generation.json")).unwrap(), raw);
    assert_eq!(
        fs::read_link(host.join("current")).unwrap(),
        Path::new("old")
    );
}
#[tokio::test]
async fn gcroot_failure_preserves_previous_current_and_collision_does_not_overwrite() {
    let (dir, store) = fixture();
    let host = store.host_directory("alpha").unwrap();
    fs::create_dir_all(&host).unwrap();
    std::os::unix::fs::symlink("previous", host.join("current")).unwrap();
    fs::write(dir.path().join("test-gcroots"), b"fault").unwrap();
    let generation = generation("new");
    let guard = crate::configuration::MutationLock::acquire(
        &store.root,
        &tokio_util::sync::CancellationToken::new(),
    )
    .await
    .unwrap();
    assert!(store.publish(&guard, "alpha", &generation).is_err());
    assert_eq!(
        fs::read_link(host.join("current")).unwrap(),
        Path::new("previous")
    );
    assert!(host.join("new/generation.json").is_file());
    assert!(store.publish(&guard, "alpha", &generation).is_err());
    assert_eq!(
        fs::read_link(host.join("current")).unwrap(),
        Path::new("previous")
    );
}
#[test]
fn missing_optional_rollback_and_inputs_info_alias_remain_readable() {
    let value = json!({"mode":"flakes","date":"2000-01-01T00:00:00Z","toplevel":"/nix/store/system","inputsInfo":{"x":{"raw":true}}});
    let generation = BuildGeneration::decode("old", value).unwrap();
    assert!(generation.auto_rollback.is_none());
    assert_eq!(generation.inputs_info["x"], json!({"raw":true}));
    assert!(
        BuildGeneration::decode(
            "bad",
            json!({"date":"2000-01-01T00:00:00Z","toplevel":"/nix/store/system"})
        )
        .is_err()
    );
}

#[test]
fn local_inventory_reader_needs_no_gc_roots_or_mutation_and_keeps_rejected_bytes() {
    let scratch = tempfile::tempdir().unwrap();
    let root = ConfigRoot::new(scratch.path()).unwrap();
    assert!(
        GenerationInventory::load_local(&root, "absent")
            .unwrap()
            .generations
            .is_empty()
    );
    assert!(!root.path().join(".confctl").exists());
    let host = root.path().join(".confctl/generations/nested:host");
    let good = generation("supported");
    for name in ["supported", "rejected"] {
        fs::create_dir_all(host.join(name)).unwrap();
    }
    let bytes = serde_json::to_vec(&good.encode()).unwrap();
    fs::write(host.join("supported/generation.json"), &bytes).unwrap();
    fs::write(host.join("rejected/generation.json"), b"{bad").unwrap();
    std::os::unix::fs::symlink("rejected", host.join("current")).unwrap();
    let inventory = GenerationInventory::load_local(&root, "nested/host").unwrap();
    assert_eq!(inventory.generations.len(), 1);
    assert_eq!(inventory.generations[0].name, "supported");
    assert!(inventory.rejected.contains_key("rejected") && inventory.current_error.is_some());
    assert!(!root.path().join(".confctl/mutation.lock").exists());
    assert_eq!(
        fs::read(host.join("supported/generation.json")).unwrap(),
        bytes
    );
    assert_eq!(
        fs::read(host.join("rejected/generation.json")).unwrap(),
        b"{bad"
    );
    assert_eq!(
        fs::read_link(host.join("current")).unwrap(),
        Path::new("rejected")
    );
    assert!(GenerationInventory::load_local(&root, "..").is_err());
}

#[test]
fn saved_selector_owns_typed_offsets_names_and_exclusion_refusals() {
    let mut saved = GenerationInventory {
        generations: ["old", "middle", "+1", "99999999999999999999999999999"]
            .map(generation)
            .to_vec(),
        current: Some("old".into()),
        ..Default::default()
    };
    for (text, expected) in [
        ("current", Some("old")),
        ("0", Some("99999999999999999999999999999")),
        ("-0", Some("99999999999999999999999999999")),
        ("0001", Some("old")),
        ("2", Some("middle")),
        ("-1", Some("+1")),
        ("-0002", Some("middle")),
        ("+1", Some("+1")),
        ("middle", Some("middle")),
        ("5", None),
        ("-4", None),
        ("99999999999999999999999999999", None),
        ("-99999999999999999999999999999", None),
        ("missing", None),
    ] {
        assert_eq!(
            saved
                .select_saved("host", &GenerationSelector::parse(text.into()))
                .unwrap()
                .map(|g| g.name.as_str()),
            expected,
            "{text}"
        );
        assert_eq!(
            saved.select("host", text).unwrap().map(|g| g.name.as_str()),
            expected,
            "string {text}"
        );
    }
    saved.rejected.insert(
        "bad".into(),
        RejectedGeneration {
            path: "bad".into(),
            reason: "unsupported".into(),
        },
    );
    for text in ["bad", "0", "-0", "0001", "99999999999999999999999999999"] {
        assert!(
            saved
                .select_saved("host", &GenerationSelector::parse(text.into()))
                .is_err(),
            "{text}"
        );
    }
    assert_eq!(saved.select("host", "+1").unwrap().unwrap().name, "+1");
    saved.current_error = Some("broken current".into());
    assert!(
        saved
            .select_saved("host", &GenerationSelector::Current)
            .is_err()
    );
    assert_eq!(
        saved.select("host", "middle").unwrap().unwrap().name,
        "middle"
    );
    assert!(
        GenerationInventory::default()
            .select("host", "0")
            .unwrap()
            .is_none()
    );
}

#[tokio::test]
async fn destroy_keeps_namespace_current_targets_and_unlinks_only_owned_entries() {
    let (scratch, store) = fixture();
    let cancel = tokio_util::sync::CancellationToken::new();
    let guard = crate::configuration::MutationLock::acquire(&store.root, &cancel)
        .await
        .unwrap();
    let mut saved = generation("saved");
    let target = scratch.path().join("store-target");
    fs::write(&target, b"keep store bytes").unwrap();
    saved.toplevel = target.clone();
    saved.auto_rollback = None;
    saved.inputs.insert("z".into(), json!(target));
    store.publish(&guard, "nested/host", &saved).unwrap();
    let dir = store.host_directory("nested/host").unwrap();
    let root = store.roots.directory();
    assert!(
        root.join("nested:host-generation-saved-toplevel")
            .is_symlink()
    );
    assert!(
        root.join("nested:host-generation-saved-input.z")
            .is_symlink()
    );
    // Both missing optional links and dangling input links are supported.
    fs::remove_file(dir.join("saved/nixpkgs.input")).unwrap();
    store
        .destroy(&guard, "nested/host", &saved, &cancel)
        .unwrap();
    assert!(!dir.join("saved").exists());
    assert_eq!(
        fs::read_link(dir.join("current")).unwrap(),
        Path::new("saved")
    );
    assert!(dir.join("current").is_symlink());
    assert_eq!(fs::read(target).unwrap(), b"keep store bytes");
    assert!(dir.is_dir() && root.is_dir());
    assert!(fs::read_dir(root).unwrap().next().is_none());
    assert!(store.root.path().join(".confctl/mutation.lock").exists());
}

#[tokio::test]
async fn destroy_validates_guard_record_and_parent_before_effects_then_stops_at_partial_fault() {
    let (scratch, store) = fixture();
    let cancel = tokio_util::sync::CancellationToken::new();
    let guard = crate::configuration::MutationLock::acquire(&store.root, &cancel)
        .await
        .unwrap();
    let saved = generation("saved");
    store.publish(&guard, "host", &saved).unwrap();
    let dir = store.host_directory("host").unwrap().join("saved");
    let roots = store.roots.directory();
    let other = tempfile::tempdir().unwrap();
    let other_root = ConfigRoot::new(other.path()).unwrap();
    let wrong = crate::configuration::MutationLock::acquire(&other_root, &cancel)
        .await
        .unwrap();
    assert!(store.destroy(&wrong, "host", &saved, &cancel).is_err());
    assert!(roots.join("host-generation-saved-toplevel").is_symlink());
    let mut changed = saved.clone();
    changed.toplevel = "/changed".into();
    assert!(store.destroy(&guard, "host", &changed, &cancel).is_err());
    let moved = scratch.path().join("moved");
    fs::rename(&dir, &moved).unwrap();
    std::os::unix::fs::symlink(&moved, &dir).unwrap();
    assert!(store.destroy(&guard, "host", &saved, &cancel).is_err());
    assert!(roots.join("host-generation-saved-toplevel").is_symlink());
    fs::remove_file(&dir).unwrap();
    fs::rename(&moved, &dir).unwrap();
    // Input root fails after the two earlier roots, before any record entry.
    let bad = roots.join("host-generation-saved-input.nixpkgs");
    fs::remove_file(&bad).unwrap();
    fs::create_dir(&bad).unwrap();
    assert!(store.destroy(&guard, "host", &saved, &cancel).is_err());
    assert!(!roots.join("host-generation-saved-toplevel").is_symlink());
    assert!(
        !roots
            .join("host-generation-saved-auto_rollback")
            .is_symlink()
    );
    assert!(dir.join("toplevel").is_symlink() && dir.join("generation.json").is_file());
    fs::remove_dir(bad).unwrap();
    fs::write(dir.join("unknown"), b"retained").unwrap();
    assert!(store.destroy(&guard, "host", &saved, &cancel).is_err());
    assert_eq!(fs::read(dir.join("unknown")).unwrap(), b"retained");
    assert!(!dir.join("generation.json").exists());
    assert!(
        store
            .host_directory("host")
            .unwrap()
            .join("current")
            .is_symlink()
    );
}

#[tokio::test]
async fn destroy_cancel_and_cross_configuration_roots_never_unlink() {
    let (_scratch, store) = fixture();
    let cancel = tokio_util::sync::CancellationToken::new();
    let guard = crate::configuration::MutationLock::acquire(&store.root, &cancel)
        .await
        .unwrap();
    let saved = generation("saved");
    store.publish(&guard, "host", &saved).unwrap();
    let other = tempfile::tempdir().unwrap();
    let other_root = ConfigRoot::new(other.path()).unwrap();
    let wrong_store = GenerationStore::new(
        store.root.clone(),
        GcRoots::new(&other_root, other.path(), "login").unwrap(),
    );
    assert!(
        wrong_store
            .destroy(&guard, "host", &saved, &cancel)
            .is_err()
    );
    cancel.cancel();
    assert!(
        store
            .destroy(&guard, "host", &saved, &cancel)
            .unwrap_err()
            .is_cancelled()
    );
    assert!(
        store
            .roots
            .directory()
            .join("host-generation-saved-toplevel")
            .is_symlink()
    );
}
