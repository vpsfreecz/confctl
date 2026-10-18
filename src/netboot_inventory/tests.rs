use super::*;
use crate::{
    configuration::{ConfigRelativePath, ConfigRoot, MutationLock},
    domain::Machine,
};
use serde_json::json;
use tokio_util::sync::CancellationToken;

fn machine(name: &str, spin: &str, managed: bool, carrier: Option<&str>, netboot: bool) -> Machine {
    Machine::from_value(json!({"name":name,"carrier":carrier,"metaConfig":{"spin":spin,"managed":managed,"netboot":{"enable":netboot}}})).unwrap()
}
#[test]
fn rendered_list_matches_ruby_order_and_includes_unmanaged_nixos_without_targets() {
    let inventory = Inventory {
        machines: vec![
            machine("nodes/a", "vpsadminos", true, None, true),
            machine("nodes/b", "vpsadminos", false, None, true),
            machine("carrier#b", "vpsadminos", true, Some("carrier"), true),
            machine("nixos/live", "nixos", false, None, true),
            machine("nodes/off", "vpsadminos", true, None, false),
        ],
    };
    // Accepted M0 site-netboot-{rediscover,add,rename} generated file bytes.
    assert_eq!(render(&inventory).unwrap(),b"# This file is auto-generated on confctl rediscover, changes will be lost\n[\n  \"nodes/a\"\n  \"nodes/b\"\n  \"nixos/live\"\n]\n");
    assert_eq!(
        render(&Inventory::default()).unwrap(),
        b"# This file is auto-generated on confctl rediscover, changes will be lost\n[\n]\n"
    );
}
#[test]
fn literal_dump_preserves_control_interpolation_and_unicode_source_semantics() {
    assert_eq!(
        ruby_dump("a\"b\\c\n\t\0\u{010d}\u{1f600}#{x}"),
        "\"a\\\"b\\\\c\\n\\t\\x00\\u010D\\u{1F600}\\#{x}\""
    );
}
#[tokio::test]
async fn publication_requires_enabled_matching_lock_and_preserves_prior_core_write() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir(dir.path().join("cluster")).unwrap();
    std::fs::write(dir.path().join("cluster/cluster.nix"), "new core list").unwrap();
    let root = ConfigRoot::new(dir.path()).unwrap();
    let files = StateFiles::new(root.clone());
    let cancel = CancellationToken::new();
    let guard = MutationLock::acquire(&root, &cancel).await.unwrap();
    let mut settings = NetbootInventorySettings {
        enable: false,
        output_file: Some(ConfigRelativePath::new("cluster/netbootable.nix").unwrap()),
    };
    assert!(
        publish(
            &FeatureFiles {
                files: &files,
                settings: &settings,
                mutation: &guard
            },
            b"new"
        )
        .is_err()
    );
    assert!(!dir.path().join("cluster/netbootable.nix").exists());
    settings.enable = true;
    let content = render(&Inventory::default()).unwrap();
    let receipt = publish(
        &FeatureFiles {
            files: &files,
            settings: &settings,
            mutation: &guard,
        },
        &content,
    )
    .unwrap();
    assert_eq!(receipt.bytes, content.len());
    assert_eq!(
        std::fs::read(dir.path().join("cluster/netbootable.nix")).unwrap(),
        content
    );
    let other = tempfile::tempdir().unwrap();
    let otherroot = ConfigRoot::new(other.path()).unwrap();
    let otherguard = MutationLock::acquire(&otherroot, &cancel).await.unwrap();
    assert!(
        publish(
            &FeatureFiles {
                files: &files,
                settings: &settings,
                mutation: &otherguard
            },
            b"invalid"
        )
        .is_err()
    );
    assert_eq!(
        std::fs::read_to_string(dir.path().join("cluster/cluster.nix")).unwrap(),
        "new core list"
    );
    assert_eq!(
        std::fs::read(dir.path().join("cluster/netbootable.nix")).unwrap(),
        content
    );
}
