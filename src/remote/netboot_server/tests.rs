use super::*;
use std::{
    collections::BTreeMap,
    os::unix::{fs::PermissionsExt, process::ExitStatusExt},
};

fn diagnostic_rows() -> Value {
    serde_json::from_str(include_str!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/compat/helpers/candidate/diagnostics-v1.json"
    )))
    .unwrap()
}

// Match the retained failure capture, including descendants reached through
// the two top-level published aliases. Do not discard any of its 52 entries.
fn captured_failure(root: &Path) -> BTreeMap<String, Value> {
    fn walk(root: &Path, path: &Path, files: &mut BTreeMap<String, Value>) {
        for entry in std::fs::read_dir(root.join(path)).unwrap() {
            let entry = entry.unwrap();
            let path = path.join(entry.file_name());
            let key = path.to_str().unwrap().to_owned();
            let kind = entry.file_type().unwrap();
            if kind.is_symlink() {
                files.insert(
                    key,
                    json!({"symlink":std::fs::read_link(entry.path()).unwrap().to_str().unwrap().replace(root.to_str().unwrap(),"@ROOT@")}),
                );
            } else if kind.is_dir() {
                walk(root, &path, files);
            } else {
                files.insert(
                    key,
                    json!({"text":std::fs::read_to_string(entry.path()).unwrap().replace(root.to_str().unwrap(),"@ROOT@")}),
                );
            }
        }
    }
    let mut files = BTreeMap::new();
    for entry in std::fs::read_dir(root).unwrap() {
        let entry = entry.unwrap();
        let name = entry.file_name();
        let name = name.to_str().unwrap();
        if !["http", "tftp"]
            .iter()
            .any(|prefix| name == *prefix || name.starts_with(&format!("{prefix}.")))
        {
            continue;
        }
        if entry.file_type().unwrap().is_symlink() {
            files.insert(
                name.to_owned(),
                json!({"symlink":std::fs::read_link(entry.path()).unwrap()}),
            );
        }
        if entry.path().is_dir() {
            walk(root, Path::new(name), &mut files);
        }
    }
    files
}

#[test]
fn helper_diag_01_metadata_and_current_enoent_keep_lexical_paths_and_all_old_files() {
    let rows = diagnostic_rows();
    for row in ["netboot-invalid-0", "netboot-invalid-1"] {
        let fixture = Fixture::new();
        let root = fixture.dir.path();
        fixture.profile("node", 1, row == "netboot-invalid-0", "vpsadminos");
        fixture.old_roots();
        if row == "netboot-invalid-0" {
            std::fs::write(root.join("store/node-1/machine.json"), "{bad").unwrap();
        }
        let mut out = Vec::new();
        let error = run(
            &fixture.config,
            &root.join("profiles"),
            &root.join("run/confctl/build-netboot-server.lock"),
            Path::new("/dev/urandom"),
            &mut out,
        )
        .unwrap_err();
        if row == "netboot-invalid-0" {
            let failure = error
                .get_ref()
                .unwrap()
                .downcast_ref::<MetadataJsonFailure>()
                .unwrap();
            assert_eq!(
                failure.path,
                root.join("profiles/confctl-node-1-link/machine.json")
            );
            assert_eq!(
                failure.source.classify(),
                serde_json::error::Category::Syntax
            );
            assert_eq!((failure.source.line(), failure.source.column()), (1, 2));
            assert!(std::error::Error::source(failure).is_some());
        } else {
            let failure = error
                .get_ref()
                .unwrap()
                .downcast_ref::<CurrentProfileMissing>()
                .unwrap();
            assert_eq!(failure.path, root.join("profiles/confctl-node"));
            assert_eq!(
                failure.source.raw_os_error(),
                Some(rustix::io::Errno::NOENT.raw_os_error())
            );
            assert!(std::error::Error::source(failure).is_some());
        }
        assert!(!format!("{error:?}").contains(root.to_str().unwrap()));
        assert!(!format!("{error:?}").contains("bad"));
        let mut stderr = Vec::new();
        assert_eq!(finish(Err(error), &mut stderr), 1);
        let expected = &rows["rows"][row]["expected"];
        assert_eq!(out, expected["stdout"].as_str().unwrap().as_bytes());
        assert_eq!(
            stderr,
            expected["stderr"]
                .as_str()
                .unwrap()
                .replace("@ROOT@", root.to_str().unwrap())
                .as_bytes()
        );
        assert_eq!(
            serde_json::to_value(captured_failure(root)).unwrap(),
            expected["observation"]["files"]
        );
        assert!(root.join("run/confctl/build-netboot-server.lock").is_file());
    }
}

#[test]
fn helper_diag_01_current_profile_other_io_error_is_not_mapped_to_enoent() {
    let fixture = Fixture::new();
    fixture.profile("node", 1, false, "vpsadminos");
    symlink(
        "confctl-node",
        fixture.dir.path().join("profiles/confctl-node"),
    )
    .unwrap();
    let error = model::load(&fixture.config, &fixture.dir.path().join("profiles"))
        .err()
        .expect("looping current profile");
    assert_eq!(
        error.raw_os_error(),
        Some(rustix::io::Errno::LOOP.raw_os_error())
    );
    assert!(
        error
            .get_ref()
            .and_then(|source| source.downcast_ref::<CurrentProfileMissing>())
            .is_none()
    );
}

#[test]
fn helper_diag_01_first_publish_failure_keeps_exact_listing_and_all_52_entries() {
    // The original capture uses UTC. This native fixture keeps the real lock,
    // loader, listing and publisher; only entropy is replaced by a fixed suffix.
    let mut fixture = Fixture::new();
    fixture.config.memtest = None;
    fixture.config.iso_images.clear();
    fixture.profile("node", 9, true, "vpsadminos");
    fixture.profile("node", 7, false, "vpsadminos");
    fixture.profile("nixos", 3, true, "nixos");
    fixture.old_roots();
    let root = fixture.dir.path();
    let ln = root.join("tools/bin/ln");
    std::fs::remove_file(&ln).unwrap();
    let record = root.join("ln-argv");
    let quote = format!("'{}'", record.to_str().unwrap().replace('\'', "'\\''"));
    std::fs::write(
        &ln,
        format!("#!/bin/sh\nprintf '%s\\000' \"$@\" >> {quote}\nexit 3\n"),
    )
    .unwrap();
    std::fs::set_permissions(&ln, std::fs::Permissions::from_mode(0o755)).unwrap();
    let mut out = Vec::new();
    let (_lock, machines) = prepare(
        &fixture.config,
        &root.join("profiles"),
        &root.join("run/confctl/build-netboot-server.lock"),
        &mut out,
    )
    .unwrap();
    let error = publish(&fixture.config, &machines, "abcdef").unwrap_err();
    let source = error
        .get_ref()
        .unwrap()
        .downcast_ref::<InstallFailure>()
        .unwrap();
    assert_eq!(source.target, fixture.config.tftp_root);
    assert_eq!(source.status.code(), Some(3));
    assert_eq!(source.status.signal(), None);
    assert!(!format!("{error:?}").contains(root.to_str().unwrap()));
    let mut stderr = Vec::new();
    assert_eq!(finish(Err(error), &mut stderr), 1);
    let rows = diagnostic_rows();
    let expected = &rows["rows"]["netboot-publish-failure"]["expected"];
    assert_eq!(out, expected["stdout"].as_str().unwrap().as_bytes());
    assert_eq!(
        stderr,
        expected["stderr"]
            .as_str()
            .unwrap()
            .replace("@ROOT@", root.to_str().unwrap())
            .as_bytes()
    );
    let files = captured_failure(root);
    assert_eq!(files.len(), 52);
    assert_eq!(
        serde_json::to_value(files).unwrap(),
        expected["observation"]["files"]
    );
    let argv = expected["observation"]["events"][1]["argv"]
        .as_array()
        .unwrap()[1..]
        .iter()
        .map(|value| {
            value
                .as_str()
                .unwrap()
                .replace("@ROOT@", root.to_str().unwrap())
        })
        .collect::<Vec<_>>()
        .join("\0")
        + "\0";
    assert_eq!(std::fs::read(record).unwrap(), argv.as_bytes());
}

struct Fixture {
    dir: tempfile::TempDir,
    config: NetbootServerConfig,
}
impl Fixture {
    fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        std::fs::create_dir_all(root.join("profiles")).unwrap();
        std::fs::create_dir_all(root.join("tools/share/syslinux")).unwrap();
        std::fs::create_dir_all(root.join("tools/bin")).unwrap();
        for name in [
            "pxelinux.0",
            "ldlinux.c32",
            "libcom32.c32",
            "libutil.c32",
            "memdisk",
            "menu.c32",
            "reboot.c32",
        ] {
            std::fs::write(root.join("tools/share/syslinux").join(name), name).unwrap();
        }
        let ln = std::env::split_paths(&std::env::var_os("PATH").unwrap())
            .map(|dir| dir.join("ln"))
            .find(|path| path.is_file())
            .expect("declared coreutils ln");
        symlink(ln, root.join("tools/bin/ln")).unwrap();
        std::fs::write(root.join("memtest.bin"), "memtest").unwrap();
        std::fs::write(root.join("installer.iso"), "iso").unwrap();
        let config = NetbootServerConfig {
            coreutils: root.join("tools"),
            syslinux: root.join("tools"),
            tftp_root: root.join("tftp"),
            http_root: root.join("http"),
            host_name: "boot.fixture.invalid".into(),
            http_url: "http://boot.fixture.invalid".into(),
            memtest: Some(Memtest {
                package: root.into(),
                params: vec!["console=ttyS0".into()],
            }),
            iso_images: vec![IsoImage {
                file: root.join("installer.iso"),
                label: "installer.iso".into(),
                name: "00-installer.iso".into(),
            }],
        };
        Self { dir, config }
    }
    fn profile(&self, name: &str, number: u32, current: bool, spin: &str) {
        let root = self.dir.path();
        let store = root.join(format!("store/{name}-{number}"));
        let toplevel = root.join(format!("systems/{name}-{number}"));
        std::fs::create_dir_all(&store).unwrap();
        std::fs::create_dir_all(&toplevel).unwrap();
        symlink(
            "/nix/store/fixture-linux-6.12.3/bzImage",
            toplevel.join("kernel"),
        )
        .unwrap();
        let metadata = json!({"toplevel":toplevel,"spin":spin,"fqdn":format!("{name}.invalid"),"label":format!("{name}.fixture.invalid"),"version":"25.11.test","revision":"0123456789abcdef","kernelParams":["console=ttyS0","httproot=old"],"macs":[if name=="nixos"{"02:00:00:00:00:02"}else{"02:00:00:00:00:01"}],"inputs-info":{"vpsadminos":{"rev":"abcdef"}}});
        std::fs::write(
            store.join("machine.json"),
            serde_json::to_vec(&metadata).unwrap(),
        )
        .unwrap();
        for name in if spin == "nixos" {
            &["bzImage", "initrd"][..]
        } else {
            &["bzImage", "initrd", "root.squashfs"][..]
        } {
            std::fs::write(store.join(name), name).unwrap();
        }
        let link = root.join(format!("profiles/confctl-{name}-{number}-link"));
        symlink(&store, &link).unwrap();
        let stamp = rustix::fs::Timespec {
            tv_sec: 1_700_000_000,
            tv_nsec: 0,
        };
        rustix::fs::utimensat(
            rustix::fs::CWD,
            &link,
            &rustix::fs::Timestamps {
                last_access: stamp,
                last_modification: stamp,
            },
            rustix::fs::AtFlags::SYMLINK_NOFOLLOW,
        )
        .unwrap();
        if current {
            symlink(&store, root.join(format!("profiles/confctl-{name}"))).unwrap();
        }
    }
    fn old_roots(&self) {
        for name in ["tftp", "http"] {
            let old = self.dir.path().join(format!("{name}.old"));
            std::fs::create_dir(&old).unwrap();
            std::fs::write(old.join("sentinel"), "old").unwrap();
            symlink(format!("{name}.old"), self.dir.path().join(name)).unwrap();
        }
    }
    fn machines(&self) -> Vec<model::Machine> {
        model::load(&self.config, &self.dir.path().join("profiles")).unwrap()
    }
}

fn captured(root: &Path) -> BTreeMap<String, Value> {
    fn walk(root: &Path, relative: &Path, files: &mut BTreeMap<String, Value>) {
        for entry in std::fs::read_dir(root.join(relative)).unwrap() {
            let entry = entry.unwrap();
            let path = relative.join(entry.file_name());
            let stat = entry.file_type().unwrap();
            let key = path.to_str().unwrap().to_owned();
            if stat.is_symlink() {
                files.insert(key,json!({"symlink":std::fs::read_link(entry.path()).unwrap().to_str().unwrap().replace(root.to_str().unwrap(),"@ROOT@")}));
            } else if stat.is_dir() {
                walk(root, &path, files);
            } else {
                files.insert(key,json!({"text":std::fs::read_to_string(entry.path()).unwrap().replace(root.to_str().unwrap(),"@ROOT@")}));
            }
        }
    }
    let mut files = BTreeMap::new();
    for name in ["tftp.abcdef", "http.abcdef"] {
        walk(root, Path::new(name), &mut files);
    }
    for name in ["tftp", "http"] {
        files.insert(
            name.into(),
            json!({"symlink":std::fs::read_link(root.join(name)).unwrap()}),
        );
    }
    files
}

#[test]
fn full_http_tftp_tree_menus_variants_json_and_cleanup_match_original_m0_bytes() {
    // Run this fixture under TZ=UTC, matching the retained original observation.
    let fixture = Fixture::new();
    fixture.profile("node", 9, true, "vpsadminos");
    fixture.profile("node", 7, false, "vpsadminos");
    fixture.profile("nixos", 3, true, "nixos");
    fixture.old_roots();
    let machines = fixture.machines();
    publish(&fixture.config, &machines, "abcdef").unwrap();
    let original: Value = serde_json::from_str(include_str!("ruby-full-tree.json")).unwrap();
    let expected = original["files"]
        .as_object()
        .unwrap()
        .iter()
        .map(|(key, value)| (key.clone(), value.clone()))
        .collect::<BTreeMap<_, _>>();
    assert_eq!(captured(fixture.dir.path()), expected);
    assert!(!fixture.dir.path().join("tftp.old").exists());
    assert!(!fixture.dir.path().join("http.old").exists());
}

#[test]
fn older_current_params_file_and_empty_inventory_keep_original_boundaries() {
    let fixture = Fixture::new();
    fixture.profile("node", 9, false, "vpsadminos");
    fixture.profile("node", 7, true, "vpsadminos");
    std::fs::write(
        fixture.dir.path().join("store/node-7/kernel-params"),
        "init=keep custom=1 httproot=old\n",
    )
    .unwrap();
    let machines = fixture.machines();
    assert_eq!(machines[0].current().number, "7");
    assert_eq!(
        machines[0].current().params,
        [
            "httproot=http://boot.fixture.invalid/node.invalid/7/root.squashfs",
            "init=keep",
            "custom=1"
        ]
    );
    let mut plain = Fixture::new();
    plain.config.memtest = None;
    plain.config.iso_images.clear();
    publish(&plain.config, &[], "abcdef").unwrap();
    assert_eq!(
        std::fs::read_to_string(plain.dir.path().join("http.abcdef/machines.json")).unwrap(),
        "{\n  \"machines\": []\n}"
    );
}

#[test]
fn metadata_and_first_install_failures_preserve_old_roots_but_second_install_is_not_transactional()
{
    for mode in [
        "corrupt",
        "missing-current",
        "first-install",
        "second-install",
    ] {
        let fixture = Fixture::new();
        fixture.profile("node", 9, mode != "missing-current", "vpsadminos");
        fixture.old_roots();
        if mode == "corrupt" {
            std::fs::write(fixture.dir.path().join("store/node-9/machine.json"), "{bad").unwrap();
        }
        if mode.ends_with("install") {
            let ln = fixture.dir.path().join("tools/bin/ln");
            let actual = std::fs::read_link(&ln).unwrap();
            std::fs::remove_file(&ln).unwrap();
            let quote = |value: &str| format!("'{}'", value.replace('\'', "'\\''"));
            let fail = if mode == "first-install" {
                "exit 3\n".into()
            } else {
                format!(
                    "test \"$3\" = {} && exit 3\nexec {} \"$@\"\n",
                    quote(fixture.config.http_root.to_str().unwrap()),
                    quote(actual.to_str().unwrap())
                )
            };
            std::fs::write(&ln, format!("#!/bin/sh\n{fail}")).unwrap();
            std::fs::set_permissions(&ln, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        let result = run(
            &fixture.config,
            &fixture.dir.path().join("profiles"),
            &fixture
                .dir
                .path()
                .join("run/confctl/build-netboot-server.lock"),
            Path::new("/dev/urandom"),
            &mut Vec::new(),
        );
        assert!(result.is_err(), "{mode}");
        let tftp = std::fs::read_link(&fixture.config.tftp_root).unwrap();
        let http = std::fs::read_link(&fixture.config.http_root).unwrap();
        assert_eq!(http, Path::new("http.old"));
        if mode == "second-install" {
            assert_ne!(tftp, Path::new("tftp.old"));
        } else {
            assert_eq!(tftp, Path::new("tftp.old"));
        }
        assert_eq!(
            std::fs::read_to_string(fixture.dir.path().join("http.old/sentinel")).unwrap(),
            "old"
        );
        assert_eq!(
            std::fs::read_to_string(fixture.dir.path().join("tftp.old/sentinel")).unwrap(),
            "old"
        );
    }
}

#[test]
fn config_accepts_old_unused_ruby_key_and_preserves_required_empty_data() {
    let fixture = Fixture::new();
    let root = fixture.dir.path();
    let mut value = json!({"ruby":"/unused/ruby","coreutils":root.join("tools"),"syslinux":root.join("tools"),"tftpRoot":root.join("tftp"),"httpRoot":root.join("http"),"hostName":"","httpUrl":"http://boot.invalid","memtest":null,"isoImages":[]});
    let path = root.join("config.json");
    std::fs::write(&path, serde_json::to_vec(&value).unwrap()).unwrap();
    let old = NetbootServerConfig::load(&path).unwrap();
    value.as_object_mut().unwrap().remove("ruby");
    std::fs::write(&path, serde_json::to_vec(&value).unwrap()).unwrap();
    let new = NetbootServerConfig::load(&path).unwrap();
    assert_eq!(old.host_name, "");
    assert_eq!(new.host_name, old.host_name);
    assert_eq!(old.http_root, new.http_root);
    assert!(new.memtest.is_none());
    assert!(new.iso_images.is_empty());
    value.as_object_mut().unwrap().remove("memtest");
    std::fs::write(&path, serde_json::to_vec(&value).unwrap()).unwrap();
    assert!(NetbootServerConfig::load(&path).is_err());
}
