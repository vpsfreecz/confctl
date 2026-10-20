//! Opt-in real Nix source-inclusion observations. Never run by the default suite.
//! Preparation/compile review does not qualify Nix inputs or authorize execution.

use serde_json::{Value, json};
use std::{
    ffi::OsStr,
    fs,
    os::unix::{
        ffi::OsStrExt,
        fs::{PermissionsExt, symlink},
        process::CommandExt,
    },
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
};

struct Inputs {
    binary: PathBuf,
    source: PathBuf,
    nixpkgs: PathBuf,
    helper: PathBuf,
    nix: PathBuf,
    git: PathBuf,
    shell: PathBuf,
    root: PathBuf,
    system: String,
}
impl Inputs {
    fn read(phase: &str) -> Self {
        let this = Self {
            binary: required_path("CONFCTL_REAL_NIX_BIN"),
            source: required_path("CONFCTL_REAL_NIX_SOURCE"),
            nixpkgs: required_path("CONFCTL_REAL_NIX_NIXPKGS"),
            helper: required_path("CONFCTL_REAL_NIX_SITE_HELPER"),
            nix: required_path("CONFCTL_REAL_NIX_NIX"),
            git: required_path("CONFCTL_REAL_NIX_GIT"),
            shell: required_path("CONFCTL_REAL_NIX_SHELL"),
            root: PathBuf::from(std::env::var_os("CONFCTL_REAL_NIX_ROOT").expect("prerequisite: CONFCTL_REAL_NIX_ROOT is required (retained disposable absolute path outside Git)")),
            system: std::env::var("CONFCTL_REAL_NIX_SYSTEM").expect("prerequisite: CONFCTL_REAL_NIX_SYSTEM is required"),
        };
        for path in [&this.binary, &this.nix, &this.git, &this.shell] {
            assert!(
                path.is_file() && fs::metadata(path).unwrap().permissions().mode() & 0o111 != 0,
                "prerequisite: executable is unavailable: {}",
                path.display()
            );
        }
        for path in [
            this.source.join("nix/flake/mk-confctl-outputs.nix"),
            this.nixpkgs.join("default.nix"),
            this.nixpkgs.join("nixos/lib/eval-config.nix"),
            this.helper.clone(),
        ] {
            assert!(
                path.is_file(),
                "prerequisite: local source file unavailable: {}",
                path.display()
            );
        }
        require_native_system(&this.system);
        assert!(
            this.root.is_absolute(),
            "prerequisite: retained fixture root must be absolute"
        );
        let parent = this
            .root
            .parent()
            .expect("fixture root needs a parent")
            .canonicalize()
            .expect("prerequisite: fixture parent must already exist");
        outside_git(&parent);
        assert_eq!(
            this.root,
            parent.join(this.root.file_name().unwrap()),
            "prerequisite: use a canonical fixture parent without dot components"
        );
        if phase == "cases" {
            require_cases_root(&this.root);
        }
        this
    }
    fn base_command(&self, program: &Path, cwd: &Path) -> Command {
        let mut command = Command::new(program);
        command
            .current_dir(cwd)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        for key in [
            "GIT_DIR",
            "GIT_WORK_TREE",
            "GIT_INDEX_FILE",
            "GIT_COMMON_DIR",
        ] {
            command.env_remove(key);
        }
        command
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_CONFIG_GLOBAL", "/dev/null");
        command
    }
    fn native(&self, cwd: &Path, args: &[&str], label: &str) -> Output {
        let mut command = self.base_command(&self.nix, cwd);
        command
            .args([
                "--offline",
                "--option",
                "substitute",
                "false",
                "--option",
                "builders",
                "",
            ])
            .args(args);
        self.nix_environment(&mut command);
        let output = command
            .output()
            .unwrap_or_else(|error| panic!("prerequisite/native observer spawn {label}: {error}"));
        save(&self.root.join("observations"), label, &output);
        output
    }
    fn nix_environment(&self, command: &mut Command) {
        command.env("NIX_CONFIG", format!("experimental-features = nix-command flakes\nflake-registry = {}\naccept-flake-config = false\n", self.root.join("empty-registry.json").display()));
    }
    fn git(&self, cwd: &Path, args: &[&OsStr]) -> Vec<u8> {
        let output = self
            .base_command(&self.git, cwd)
            .args(args)
            .output()
            .expect("prerequisite: supplied Git must execute");
        assert!(
            output.status.success(),
            "fixture Git {args:?} failed: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        output.stdout
    }
}
fn require_native_system(system: &str) {
    assert!(
        !system.is_empty()
            && system
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_'),
        "prerequisite: explicit native Nix system is required"
    );
}
fn required_path(key: &str) -> PathBuf {
    let raw = std::env::var_os(key).unwrap_or_else(|| {
        panic!("prerequisite: {key} is required; no tool/source acquisition or automatic fallback")
    });
    let path = PathBuf::from(raw);
    assert!(path.is_absolute(), "prerequisite: {key} must be absolute");
    path.canonicalize()
        .unwrap_or_else(|error| panic!("prerequisite: {key}: {error}"))
}
fn outside_git(path: &Path) {
    for ancestor in path.ancestors() {
        assert!(
            !ancestor.join(".git").exists(),
            "prerequisite: fixture parent is inside Git ancestor {}",
            ancestor.display()
        );
    }
}
fn require_cases_root(root: &Path) {
    let metadata =
        fs::symlink_metadata(root).expect("prerequisite: retained cases root must already exist");
    assert!(
        metadata.is_dir(),
        "prerequisite: retained cases root must be a non-symlink directory"
    );
    let canonical = root
        .canonicalize()
        .expect("prerequisite: resolve cases root");
    assert_eq!(
        canonical.as_os_str(),
        root.as_os_str(),
        "prerequisite: retained cases root must be canonical"
    );
    outside_git(root);
}
fn write(root: &Path, relative: &str, bytes: impl AsRef<[u8]>) {
    let path = root.join(relative);
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(&path, bytes).unwrap();
    fs::set_permissions(path, fs::Permissions::from_mode(0o644)).unwrap();
}
fn save(directory: &Path, label: &str, output: &Output) {
    fs::create_dir_all(directory).unwrap();
    fs::write(directory.join(format!("{label}.stdout")), &output.stdout).unwrap();
    fs::write(directory.join(format!("{label}.stderr")), &output.stderr).unwrap();
    fs::write(
        directory.join(format!("{label}.status")),
        format!("{}\n", output.status),
    )
    .unwrap();
    eprintln!(
        "{label}: {}; native output retained in {}",
        output.status,
        directory.display()
    );
}
fn json_success(output: &Output, label: &str) -> Value {
    assert!(
        output.status.success(),
        "{label} failed; inspect retained native diagnostic: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    serde_json::from_slice(&output.stdout)
        .unwrap_or_else(|error| panic!("{label} returned invalid JSON: {error}"))
}
fn nix_path_url(value: &Path) -> String {
    serde_json::to_string(&format!(
        "path:{}",
        value
            .to_str()
            .expect("fixture input source paths must be UTF-8"),
    ))
    .unwrap()
    .replace("${", "\\${")
}

#[test]
#[ignore = "requires explicit local Nix/source/builder prerequisites and separate baseline/cases authorization; see docs/configuration-commands.md"]
fn configuration_source_inclusion_and_site_imports() {
    assert_eq!(
        std::env::consts::OS,
        "linux",
        "prerequisite: this native qualification requires Linux"
    );
    let phase = std::env::var("CONFCTL_REAL_NIX_PHASE")
        .expect("prerequisite: explicitly select CONFCTL_REAL_NIX_PHASE=baseline or cases");
    assert!(
        phase == "baseline" || phase == "cases",
        "select only baseline or cases"
    );
    let inputs = Inputs::read(&phase);
    eprintln!(
        "real-Nix fixture phase={phase}, retained root={}; this opt-in test never silently skips missing prerequisites",
        inputs.root.display()
    );
    if phase == "baseline" {
        fs::create_dir(&inputs.root).expect(
            "baseline requires a new retained root; do not overwrite or repair a previous fixture",
        );
        fs::write(
            inputs.root.join("empty-registry.json"),
            b"{\"flakes\":[],\"version\":2}\n",
        )
        .unwrap();
        let baseline = inputs.root.join("baseline");
        fs::create_dir(&baseline).unwrap();
        prepare(&inputs, &baseline);
        let lock = inputs.native(&baseline, &["flake", "lock"], "baseline-local-lock");
        assert!(
            lock.status.success(),
            "local-only lock prerequisite failed; no retry/acquisition: {}",
            String::from_utf8_lossy(&lock.stderr)
        );
        json_local_lock(&baseline);
        initial_git(&inputs, &baseline);
        let before = Snapshot::read(&inputs, &baseline, true);
        let dry_run = inputs.native(
            &baseline,
            &[
                "build",
                "--dry-run",
                "--json",
                "--no-link",
                "--impure",
                "--no-write-lock-file",
                "--no-update-lock-file",
                ".#confctl.machinesJson",
            ],
            "baseline-machines-dry-run",
        );
        before.unchanged(&inputs, &baseline, true);
        assert!(
            dry_run.status.success(),
            "baseline dry-run prerequisite failed; inspect retained output, do not realize missing dependencies"
        );
        fs::write(inputs.root.join("baseline-dry-run.exit"), b"0\n").unwrap();
        eprintln!(
            "STOP after the one dry-run: root must inspect its complete output. Only the small machine-list.json writeText output may need building; missing Bash/stdenv/compiler or other prerequisites block the later cases. No product invocation or actual build ran in this phase."
        );
        return;
    }
    assert_eq!(
        fs::read(inputs.root.join("baseline-dry-run.exit"))
            .expect("prerequisite: complete the separately authorized baseline phase first"),
        b"0\n"
    );
    let baseline = inputs.root.join("baseline");
    assert_eq!(
        fs::read(baseline.join("flake.nix")).unwrap(),
        flake(&inputs).as_bytes(),
        "prerequisite: source/system paths differ from the retained baseline"
    );
    assert_eq!(
        fs::read(baseline.join("lib/netboot-machines.nix")).unwrap(),
        fs::read(&inputs.helper).unwrap(),
        "prerequisite: copied site helper changed"
    );
    json_local_lock(&baseline);
    defaults(&inputs, &baseline);
    dirty_site(&inputs, &baseline);
    untracked_add(&inputs, &baseline);
    renamed(&inputs, &baseline);
    plain_path(&inputs, &baseline);
    eprintln!(
        "Five synthetic cases and explicit controls complete. Native source/index conditions and diagnostics are retained; root owns their disposition. This does not qualify the full site or system builds."
    );
}

fn flake(inputs: &Inputs) -> String {
    // Flake input URLs must be literals; concatenations are rejected as thunks.
    format!(
        r#"{{
  inputs.confctl = {{ url = {}; flake = false; }};
  inputs.nixpkgs = {{ url = {}; flake = false; }};
  outputs = inputs@{{ self, confctl, nixpkgs }}:
    let outputs = import (confctl + "/nix/flake/mk-confctl-outputs.nix") {{
      confDir = ./.;
      inherit inputs;
      system = "{}";
      channels.nixos-unstable.nixpkgs = "nixpkgs";
    }};
    in {{
      confctl = outputs // {{
        settings = outputs.settings // {{
          nix = outputs.settings.nix // {{
            impureEval = (import ./fixture-enabled.nix);
          }};
        }};
      }};
      fixtureNetbootList = import ./cluster/netbootable.nix;
    }};
}}
"#,
        nix_path_url(&inputs.source),
        nix_path_url(&inputs.nixpkgs),
        inputs.system
    )
}
fn prepare(inputs: &Inputs, root: &Path) {
    write(root, "flake.nix", flake(inputs));
    write(root, "fixture-enabled.nix", b"true\n");
    write(root, "configs/confctl.nix", b"{ ... }: { confctl.netbootInventory = { enable = true; outputFile = \"cluster/netbootable.nix\"; }; }\n");
    write(
        root,
        "cluster/module-list.nix",
        b"(import ./cluster.nix) ++ (import ./site/module-list.nix)\n",
    );
    write(root, "cluster/cluster.nix", core(&["alpha"]));
    write(root, "cluster/netbootable.nix", b"[ \"alpha\" ]\n");
    for (name, hostname) in [("alpha", "alpha"), ("nested/beta", "beta")] {
        write(
            root,
            &format!("cluster/{name}/module.nix"),
            format!(
                "{{ ... }}: {{ cluster.\"{name}\" = {{ spin = \"nixos\"; inputs.channels = [ \"nixos-unstable\" ]; netboot.enable = true; host = {{ name = \"{hostname}\"; location = \"fixture\"; domain = \"invalid\"; }}; }}; }}\n"
            ),
        );
        write(
            root,
            &format!("cluster/{name}/config.nix"),
            b"{ ... }: {}\n",
        );
    }
    write(root, "cluster/site/module-list.nix", b"[ ./module.nix ]\n");
    write(
        root,
        "cluster/site/module.nix",
        br#"{ config, ... }: {
  cluster.carrier = {
    spin = "nixos";
    host = { name = "carrier"; location = "fixture"; domain = "invalid"; };
    carrier = {
      enable = true;
      machines = import ../../lib/netboot-machines.nix {
        cluster = config.cluster;
        tags = [ "fixture" ];
        dynamicTags = [ "netboot" ];
        buildAttribute = [ "system" "build" "toplevel" ];
        buildGenerations = { min = 1; max = 2; maxAge = null; };
        hostGenerations = { min = 1; max = 2; maxAge = null; };
      };
    };
  };
}
"#,
    );
    write(
        root,
        "lib/netboot-machines.nix",
        fs::read(&inputs.helper).unwrap(),
    );
    write(root, "data/default.nix", b"{ lib }: {}\n");
    write(root, "environments/base.nix", b"{ ... }: {}\n");
    write(
        root,
        "modules/module-list.nix",
        b"{ nixos = []; vpsadminos = []; }\n",
    );
}
fn core(names: &[&str]) -> Vec<u8> {
    format!(
        "# This file is generated by confctl, changes will be lost\n[\n{}]\n",
        names
            .iter()
            .map(|name| format!("  ./{name}/module.nix\n"))
            .collect::<String>()
    )
    .into_bytes()
}
fn json_local_lock(root: &Path) {
    let lock: Value = serde_json::from_slice(
        &fs::read(root.join("flake.lock"))
            .expect("prerequisite: native local flake lock was not produced"),
    )
    .unwrap();
    let nodes = lock["nodes"].as_object().expect("local lock nodes");
    for (name, node) in nodes {
        if name == lock["root"].as_str().unwrap() {
            continue;
        }
        assert_eq!(
            node["flake"], false,
            "all fixture inputs must be flake=false: {name}"
        );
        for key in ["original", "locked"] {
            assert_eq!(
                node[key]["type"], "path",
                "remote/non-path source forbidden: {name}.{key}"
            );
            let path = node[key]["path"].as_str().expect("local locked path");
            assert!(
                Path::new(path).is_absolute() && Path::new(path).exists(),
                "prerequisite: locked source is not already local: {path}"
            );
        }
    }
    assert_eq!(
        nodes.len(),
        3,
        "fixture may contain only root and two local inputs"
    );
}
fn initial_git(inputs: &Inputs, root: &Path) {
    inputs.git(
        root,
        &[
            OsStr::new("init"),
            OsStr::new("--quiet"),
            OsStr::new("--initial-branch=fixture"),
        ],
    );
    inputs.git(
        root,
        &[OsStr::new("add"), OsStr::new("--"), OsStr::new(".")],
    );
    let message = inputs.root.join("initial-commit-message");
    fs::write(&message, b"Record disposable real Nix fixture baseline\n").unwrap();
    inputs.git(
        root,
        &[
            OsStr::new("-c"),
            OsStr::new("user.name=Configuration fixture"),
            OsStr::new("-c"),
            OsStr::new("user.email=fixture@example.invalid"),
            OsStr::new("-c"),
            OsStr::new("commit.gpgsign=false"),
            OsStr::new("commit"),
            OsStr::new("--quiet"),
            OsStr::new("-F"),
            message.as_os_str(),
        ],
    );
}

#[derive(Debug, Eq, PartialEq)]
struct IndexEntry {
    path: Vec<u8>,
    mode: Vec<u8>,
    content: Vec<u8>,
}
#[derive(Debug, Eq, PartialEq)]
struct Snapshot {
    lock: Vec<u8>,
    mode: u32,
    staged: Vec<IndexEntry>,
}
impl Snapshot {
    fn read(inputs: &Inputs, root: &Path, git: bool) -> Self {
        let mut staged = Vec::new();
        if git {
            let paths = inputs.git(
                root,
                &[
                    OsStr::new("ls-files"),
                    OsStr::new("--stage"),
                    OsStr::new("-z"),
                ],
            );
            for entry in paths.split(|b| *b == 0).filter(|part| !part.is_empty()) {
                let tab = entry.iter().position(|b| *b == b'\t').unwrap();
                let header = entry[..tab].split(|b| *b == b' ').collect::<Vec<_>>();
                assert_eq!(header[2], b"0", "fixture index must not have conflicts");
                let content = inputs.git(
                    root,
                    &[
                        OsStr::new("cat-file"),
                        OsStr::new("blob"),
                        OsStr::from_bytes(header[1]),
                    ],
                );
                staged.push(IndexEntry {
                    path: entry[tab + 1..].to_vec(),
                    mode: header[0].to_vec(),
                    content,
                });
            }
        } else {
            outside_git(root);
        }
        Self {
            lock: fs::read(root.join("flake.lock")).unwrap(),
            mode: fs::metadata(root.join("flake.lock"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            staged,
        }
    }
    fn unchanged(&self, inputs: &Inputs, root: &Path, git: bool) {
        assert_eq!(
            *self,
            Self::read(inputs, root, git),
            "product changed lock or staged paths/content/modes; index stat-cache bytes are deliberately excluded"
        );
    }
}
fn copy_tree(source: &Path, destination: &Path) {
    fs::create_dir(destination).unwrap();
    for entry in fs::read_dir(source).unwrap() {
        let entry = entry.unwrap();
        if entry.file_name() == ".git" || entry.file_name() == ".confctl" {
            continue;
        }
        let path = destination.join(entry.file_name());
        if entry.file_type().unwrap().is_dir() {
            copy_tree(&entry.path(), &path);
        } else {
            assert!(entry.file_type().unwrap().is_file());
            fs::copy(entry.path(), path).unwrap();
        }
    }
}
struct Case<'a> {
    inputs: &'a Inputs,
    root: PathBuf,
    logs: PathBuf,
    bin: PathBuf,
    git: bool,
}
impl<'a> Case<'a> {
    fn new(inputs: &'a Inputs, baseline: &Path, name: &str, enabled: bool, git: bool) -> Self {
        require_cases_root(&inputs.root);
        let root = inputs.root.join(name);
        outside_git(&root);
        copy_tree(baseline, &root);
        if !enabled {
            write(&root, "fixture-enabled.nix", b"false\n");
            write(&root, "configs/confctl.nix", b"{ ... }: {}\n");
        }
        if git {
            initial_git(inputs, &root);
        }
        let logs = inputs.root.join(format!("{name}-observations"));
        fs::create_dir(&logs).unwrap();
        let bin = logs.join("bin");
        fs::create_dir(&bin).unwrap();
        symlink(&inputs.git, bin.join("git")).unwrap();
        write(
            &bin,
            "nix",
            format!(
                "#!{}\n{{ printf '%s\\0' \"$PWD\" \"$#\"; printf '%s\\0' \"$@\"; }} >> \"$CONFCTL_REAL_NIX_CALLS\"\nexec \"$CONFCTL_REAL_NIX_NIX\" --offline --option substitute false --option builders \"\" \"$@\"\n",
                inputs.shell.to_str().unwrap()
            ),
        );
        fs::set_permissions(bin.join("nix"), fs::Permissions::from_mode(0o755)).unwrap();
        Self {
            inputs,
            root,
            logs,
            bin,
            git,
        }
    }
    fn observe(&self, target: &str, label: &str) -> Output {
        self.inputs.native(
            &self.root,
            &[
                "eval",
                "--json",
                "--no-write-lock-file",
                "--no-update-lock-file",
                target,
            ],
            label,
        )
    }
    fn product(&self, args: &[&str], label: &str, enabled: bool) -> Output {
        let before = Snapshot::read(self.inputs, &self.root, self.git);
        let record = self.logs.join(format!("{label}.argv"));
        assert!(!record.exists(), "never reuse a product observation label");
        let mut command = self.inputs.base_command(&self.inputs.binary, &self.root);
        let mut path = vec![self.bin.clone()];
        path.extend(std::env::split_paths(
            &std::env::var_os("PATH").unwrap_or_default(),
        ));
        command
            .arg("--color=never")
            .args(args)
            .env("PATH", std::env::join_paths(path).unwrap())
            .env("CONFCTL_REAL_NIX_NIX", &self.inputs.nix)
            .env("CONFCTL_REAL_NIX_CALLS", &record)
            .env("NO_COLOR", "1")
            .env("CONFCTL_TTY", "0");
        self.inputs.nix_environment(&mut command);
        // Only the designated product child changes its process-wide umask.
        unsafe {
            command.pre_exec(|| {
                rustix::process::umask(rustix::fs::Mode::from_raw_mode(0o022));
                Ok(())
            });
        }
        let output = command
            .output()
            .expect("prerequisite: designated integrated binary must execute");
        save(&self.logs, label, &output);
        before.unchanged(self.inputs, &self.root, self.git);
        let calls = parse_calls(&record);
        let stages = sequence(&calls, &self.root, enabled);
        eprintln!(
            "{label}: product reached {stages} native stage(s), actual calls={} (compatibility retries are additional recorded calls)",
            calls.len()
        );
        if output.status.success() {
            assert_eq!(stages, if enabled { 3 } else { 1 });
        } else {
            assert_eq!(
                output.status.code(),
                Some(1),
                "unexpected product termination; retained output is authoritative"
            );
        }
        if self.git {
            fs::write(
                self.logs.join(format!("{label}.source-index")),
                self.inputs.git(
                    &self.root,
                    &[
                        OsStr::new("status"),
                        OsStr::new("--short"),
                        OsStr::new("--untracked-files=all"),
                        OsStr::new("--"),
                        OsStr::new("cluster"),
                    ],
                ),
            )
            .unwrap();
        }
        output
    }
    fn machines(&self, label: &str) -> Value {
        json_success(&self.observe(".#confctl.machines", label), label)
    }
    fn assert_feature(&self, machines: &Value, label: &str) {
        let names = machines
            .as_object()
            .unwrap()
            .iter()
            .filter(|(_, machine)| {
                machine["carrier"].is_null() && machine["metaConfig"]["netboot"]["enable"] == true
            })
            .map(|(name, _)| name.clone())
            .collect::<Vec<_>>();
        let actual = json_success(&self.observe(".#fixtureNetbootList", label), label);
        assert_eq!(
            actual,
            json!(names),
            "published list differs from actual inventory order/filter"
        );
    }
    fn index(&self, paths: &[&str]) {
        assert!(self.git);
        let mut args = vec![OsStr::new("add"), OsStr::new("-A"), OsStr::new("--")];
        args.extend(paths.iter().map(OsStr::new));
        self.inputs.git(&self.root, &args);
    }
}
fn parse_calls(path: &Path) -> Vec<(PathBuf, Vec<String>)> {
    let bytes = fs::read(path).expect("product did not invoke its literal Nix program");
    let mut fields = bytes.split(|b| *b == 0).peekable();
    let mut calls = Vec::new();
    while let Some(cwd) = fields.next() {
        if cwd.is_empty() && fields.peek().is_none() {
            break;
        }
        let count: usize = std::str::from_utf8(fields.next().unwrap())
            .unwrap()
            .parse()
            .unwrap();
        let args = (0..count)
            .map(|_| String::from_utf8(fields.next().unwrap().to_vec()).unwrap())
            .collect();
        calls.push((PathBuf::from(OsStr::from_bytes(cwd)), args));
    }
    calls
}
fn expected(build: bool, auto: bool) -> Vec<String> {
    let mut args = if build {
        vec!["build", "--json", "--no-link", "--impure"]
    } else {
        vec!["eval", "--json"]
    };
    args.extend(["--no-write-lock-file", "--no-update-lock-file"]);
    if auto {
        args.extend(["--option", "max-jobs", "auto"]);
    }
    args.push(if build {
        ".#confctl.machinesJson"
    } else {
        ".#confctl.settings"
    });
    args.into_iter().map(String::from).collect()
}
fn sequence(calls: &[(PathBuf, Vec<String>)], root: &Path, enabled: bool) -> usize {
    let normal = if enabled {
        vec![
            expected(false, true),
            expected(false, false),
            expected(true, false),
        ]
    } else {
        vec![expected(false, true)]
    };
    let mut stage = 0;
    for (cwd, args) in calls {
        assert_eq!(cwd.canonicalize().unwrap(), root.canonicalize().unwrap());
        if normal.get(stage) == Some(args) {
            stage += 1;
            continue;
        }
        // Retain, label and validate only the two existing old-Nix fallbacks.
        let mut stripped = Vec::new();
        let mut index = 0;
        let mut features = false;
        while index < args.len() {
            if args[index] == "--extra-experimental-features" {
                assert!(matches!(
                    args.get(index + 1).map(String::as_str),
                    Some("nix-command" | "flakes")
                ));
                features = true;
                index += 2;
            } else {
                stripped.push(args[index].clone());
                index += 1;
            }
        }
        assert!(
            stage > 0,
            "first call must use the normal literal argv: {args:?}"
        );
        let previous = &normal[stage - 1];
        assert!(
            stripped == *previous
                || stripped
                    == previous
                        .iter()
                        .filter(|arg| *arg != "--no-update-lock-file")
                        .cloned()
                        .collect::<Vec<_>>(),
            "unexpected product argv: {args:?}"
        );
        assert!(
            features || !stripped.iter().any(|arg| arg == "--no-update-lock-file"),
            "duplicate normal query is not a compatibility retry"
        );
        eprintln!("recognized native compatibility retry, retained separately: {args:?}");
    }
    stage
}
fn assert_new(root: &Path) {
    let expected: Value =
        serde_json::from_str(include_str!("compat/core/expected/add.json")).unwrap();
    for path in [
        "cluster/nested/new/module.nix",
        "cluster/nested/new/config.nix",
    ] {
        assert_eq!(
            fs::read(root.join(path)).unwrap(),
            expected["files"][path]["text"].as_str().unwrap().as_bytes()
        );
        assert_eq!(
            fs::metadata(root.join(path)).unwrap().permissions().mode() & 0o777,
            0o644
        );
    }
    assert_eq!(
        fs::read(root.join("cluster/cluster.nix")).unwrap(),
        core(&["alpha", "nested/beta", "nested/new"])
    );
}
fn defaults(inputs: &Inputs, baseline: &Path) {
    let case = Case::new(inputs, baseline, "defaults", false, true);
    let value = json_success(
        &case.observe(".#confctl.settings", "defaults-before-settings"),
        "defaults-before-settings",
    );
    assert_eq!(value["netbootInventory"]["enable"], false);
    assert!(value["netbootInventory"]["outputFile"].is_null());
    assert!(value["nix"]["maxJobs"].is_null());
    assert_eq!(value["nix"]["impureEval"], false);
    let result = case.product(&["add", "nested/new"], "defaults-add", false);
    assert!(
        result.status.success(),
        "default settings add must succeed without forcing inventory"
    );
    assert_new(&case.root);
    assert_eq!(
        fs::read(case.root.join("cluster/netbootable.nix")).unwrap(),
        b"[ \"alpha\" ]\n"
    );
    let staged = Snapshot::read(inputs, &case.root, true).staged;
    assert!(
        !staged
            .iter()
            .any(|entry| entry.path.starts_with(b"cluster/nested/new/"))
    );
}
fn dirty_site(inputs: &Inputs, baseline: &Path) {
    let case = Case::new(inputs, baseline, "dirty-site", true, true);
    let before = case.machines("site-before-machines");
    assert!(before.get("alpha").is_some());
    assert!(before.get("carrier#fixture/alpha").is_some());
    assert!(before.get("nested/beta").is_none());
    assert!(before.get("carrier#fixture/beta").is_none());
    let result = case.product(&["rediscover"], "site-rediscover", true);
    assert!(
        result.status.success(),
        "dirty tracked core/site imports must succeed"
    );
    assert_eq!(
        fs::read(case.root.join("cluster/cluster.nix")).unwrap(),
        core(&["alpha", "nested/beta"])
    );
    let after = case.machines("site-after-machines");
    for name in [
        "alpha",
        "nested/beta",
        "carrier#fixture/alpha",
        "carrier#fixture/beta",
    ] {
        assert!(
            after.get(name).is_some(),
            "missing post-write base/carrier {name}"
        );
    }
    case.assert_feature(&after, "site-after-generated-list");
}
fn untracked_add(inputs: &Inputs, baseline: &Path) {
    let case = Case::new(inputs, baseline, "untracked-add", true, true);
    let old_feature = fs::read(case.root.join("cluster/netbootable.nix")).unwrap();
    let result = case.product(&["add", "nested/new"], "untracked-add", true);
    assert_new(&case.root);
    let index = Snapshot::read(inputs, &case.root, true);
    assert!(
        !index
            .staged
            .iter()
            .any(|entry| entry.path.starts_with(b"cluster/nested/new/"))
    );
    if result.status.success() {
        let machines = case.machines("untracked-add-after-machines");
        assert!(
            machines.get("nested/new").is_some(),
            "successful add must expose new module"
        );
        case.assert_feature(&machines, "untracked-add-generated-list");
    } else {
        assert_eq!(
            fs::read(case.root.join("cluster/netbootable.nix")).unwrap(),
            old_feature
        );
    }
    // Explicit fixture-index control, never an application inclusion workaround.
    case.index(&[
        "cluster/nested/new/module.nix",
        "cluster/nested/new/config.nix",
    ]);
    let control = case.product(&["rediscover"], "indexed-add-control", true);
    assert!(
        control.status.success(),
        "explicitly indexed add control must succeed"
    );
    let machines = case.machines("indexed-add-after-machines");
    assert!(machines.get("nested/new").is_some());
    case.assert_feature(&machines, "indexed-add-generated-list");
}
fn renamed(inputs: &Inputs, baseline: &Path) {
    let case = Case::new(inputs, baseline, "rename", true, true);
    let module = fs::read(case.root.join("cluster/nested/beta/module.nix")).unwrap();
    let config = fs::read(case.root.join("cluster/nested/beta/config.nix")).unwrap();
    let old_feature = fs::read(case.root.join("cluster/netbootable.nix")).unwrap();
    let result = case.product(
        &["rename", "nested/beta", "nested/moved"],
        "rename-untracked-destination",
        true,
    );
    assert!(!case.root.join("cluster/nested/beta").exists());
    assert_eq!(
        fs::read(case.root.join("cluster/nested/moved/module.nix")).unwrap(),
        module
    );
    assert_eq!(
        fs::read(case.root.join("cluster/nested/moved/config.nix")).unwrap(),
        config
    );
    assert_eq!(
        fs::read(case.root.join("cluster/cluster.nix")).unwrap(),
        core(&["alpha", "nested/moved"])
    );
    let index = Snapshot::read(inputs, &case.root, true);
    assert!(
        index
            .staged
            .iter()
            .any(|entry| entry.path == b"cluster/nested/beta/module.nix")
    );
    assert!(
        !index
            .staged
            .iter()
            .any(|entry| entry.path.starts_with(b"cluster/nested/moved/"))
    );
    if result.status.success() {
        let machines = case.machines("rename-untracked-after-machines");
        assert!(machines.get("nested/beta").is_some());
        assert!(machines.get("nested/moved").is_none());
    } else {
        assert_eq!(
            fs::read(case.root.join("cluster/netbootable.nix")).unwrap(),
            old_feature
        );
    }
    case.index(&[
        "cluster/nested/beta/module.nix",
        "cluster/nested/beta/config.nix",
        "cluster/nested/moved/module.nix",
        "cluster/nested/moved/config.nix",
    ]);
    assert!(
        case.product(&["rediscover"], "indexed-rename-control", true)
            .status
            .success(),
        "indexed same-depth rename control must succeed"
    );
    let machines = case.machines("indexed-rename-after-machines");
    assert!(machines.get("nested/beta").is_some());
    assert!(machines.get("nested/moved").is_none());
    case.assert_feature(&machines, "indexed-rename-generated-list");
}
fn plain_path(inputs: &Inputs, baseline: &Path) {
    let case = Case::new(inputs, baseline, "plain-path", true, false);
    assert!(
        case.product(&["add", "nested/new"], "plain-add", true)
            .status
            .success(),
        "non-Git source inclusion control must succeed"
    );
    assert_new(&case.root);
    let machines = case.machines("plain-add-after-machines");
    assert!(machines.get("nested/new").is_some());
    case.assert_feature(&machines, "plain-add-generated-list");
    assert!(!case.root.join(".git").exists());
}

#[test]
fn flake_input_urls_are_complete_escaped_literals() {
    let inputs = Inputs {
        binary: PathBuf::new(),
        source: PathBuf::from(r#"/fixture/confctl "quote"/${literal}"#),
        nixpkgs: PathBuf::from(r"/fixture/nixpkgs\source"),
        helper: PathBuf::new(),
        nix: PathBuf::new(),
        git: PathBuf::new(),
        shell: PathBuf::new(),
        root: PathBuf::new(),
        system: "x86_64-linux".into(),
    };
    let generated = flake(&inputs);
    let mut lines = generated.lines().skip(1);
    assert_eq!(
        lines.next(),
        Some(
            r#"  inputs.confctl = { url = "path:/fixture/confctl \"quote\"/\${literal}"; flake = false; };"#
        )
    );
    assert_eq!(
        lines.next(),
        Some(r#"  inputs.nixpkgs = { url = "path:/fixture/nixpkgs\\source"; flake = false; };"#)
    );
}

#[test]
fn native_system_preflight_accepts_designated_system() {
    require_native_system("x86_64-linux");
    for invalid in [
        "",
        "x86_64/linux",
        "x86_64-linux;true",
        "x86_64-linux\n",
        "é-linux",
    ] {
        assert!(std::panic::catch_unwind(|| require_native_system(invalid)).is_err());
    }
}

#[test]
fn cases_root_preflight_rejects_git_and_symlink_before_copy() {
    // This regression uses filesystem operations only: no native tools, lock or
    // real-Nix phase. Mock baseline bytes must not authorize a redirected root.
    let scratch = tempfile::Builder::new()
        .prefix("confctl-root-preflight-")
        .tempdir_in("/var/tmp")
        .unwrap();
    let scratch = scratch.path().canonicalize().unwrap();
    outside_git(&scratch);
    let baseline = scratch.join("mock-baseline");
    fs::create_dir(&baseline).unwrap();
    fs::write(baseline.join("sentinel"), b"unchanged mock source\n").unwrap();
    let tool = scratch.join("never-execute-this-tool");
    let inputs = |root| Inputs {
        binary: tool.clone(),
        source: baseline.clone(),
        nixpkgs: baseline.clone(),
        helper: baseline.join("sentinel"),
        nix: tool.clone(),
        git: tool.clone(),
        shell: tool.clone(),
        root,
        system: "x86_64-linux".into(),
    };
    let valid = scratch.join("valid");
    fs::create_dir(&valid).unwrap();
    let valid_inputs = inputs(valid.clone());
    require_cases_root(&valid);
    let valid_case = Case::new(&valid_inputs, &baseline, "defaults", true, false);
    assert_eq!(
        fs::read(valid_case.root.join("sentinel")).unwrap(),
        b"unchanged mock source\n"
    );

    for git_file in [false, true] {
        let root = scratch.join(if git_file {
            "git-file-root"
        } else {
            "git-directory-root"
        });
        fs::create_dir(&root).unwrap();
        // Represent a retained root that acquired .git between the two phases.
        fs::write(root.join("baseline-dry-run.exit"), b"0\n").unwrap();
        if git_file {
            fs::write(root.join(".git"), b"gitdir: elsewhere\n").unwrap();
        } else {
            fs::create_dir(root.join(".git")).unwrap();
        }
        let invalid = inputs(root.clone());
        assert!(std::panic::catch_unwind(|| require_cases_root(&root)).is_err());
        assert!(
            std::panic::catch_unwind(|| {
                Case::new(&invalid, &baseline, "defaults", true, false);
            })
            .is_err()
        );
        assert!(!root.join("defaults").exists());
        assert!(!root.join("defaults-observations").exists());
        assert_eq!(
            fs::read(root.join("baseline-dry-run.exit")).unwrap(),
            b"0\n"
        );
    }
    let target = scratch.join("redirected-target");
    fs::create_dir(&target).unwrap();
    fs::write(target.join("baseline-dry-run.exit"), b"0\n").unwrap();
    let redirected = scratch.join("redirected-root");
    symlink(&target, &redirected).unwrap();
    let invalid = inputs(redirected.clone());
    assert!(std::panic::catch_unwind(|| require_cases_root(&redirected)).is_err());
    assert!(
        std::panic::catch_unwind(|| {
            Case::new(&invalid, &baseline, "defaults", true, false);
        })
        .is_err()
    );
    assert!(!target.join("defaults").exists());
    assert!(!target.join("defaults-observations").exists());
    assert_eq!(
        fs::read(target.join("baseline-dry-run.exit")).unwrap(),
        b"0\n"
    );
    assert_eq!(
        fs::read(baseline.join("sentinel")).unwrap(),
        b"unchanged mock source\n"
    );
}
