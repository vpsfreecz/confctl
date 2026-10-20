//! Actual parser/app routes with confined fake tools. One explicitly isolated local Git case.
#![cfg(target_os = "linux")]
use serde_json::{Value, json};
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::{Child, Command, Output, Stdio},
    thread,
    time::{Duration, Instant},
};

struct Fixture {
    _tmp: tempfile::TempDir,
    root: PathBuf,
    bin: PathBuf,
    control: PathBuf,
    cache: PathBuf,
}
impl Fixture {
    fn new() -> Self {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("conf");
        let bin = tmp.path().join("bin");
        let control = tmp.path().join("control");
        let cache = tmp.path().join("cache");
        for dir in [&root, &bin, &control] {
            fs::create_dir(dir).unwrap();
        }
        fs::write(root.join("flake.nix"), "{outputs=_:{};}\n").unwrap();
        let f = Self {
            _tmp: tmp,
            root,
            bin,
            control,
            cache,
        };
        f.lock(lock());
        f.scenario(json!({}));
        let python = find("python3");
        let script=r#"#!PYTHON
import json,os,pathlib,sys,time,subprocess
c=pathlib.Path(os.environ['MUT_CONTROL']); args=sys.argv[1:]; kind=pathlib.Path(sys.argv[0]).name
s=json.loads((c/'scenario').read_text()); records=sorted(c.glob('call-*')); n=len(records)+1
(c/f'call-{n:03}').write_bytes(b'\0'.join(os.fsencode(a) for a in [kind,*args])+b'\0')
(c/f'cwd-{n:03}').write_text(os.getcwd())
def block(stage):
 if s.get('block')==stage:
  child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(120)'])
  (c/'native-pid').write_text(str(os.getpid())); (c/'descendant-pid').write_text(str(child.pid))
  while not (c/'release').exists(): time.sleep(.01)
if kind=='nix':
 last=args[-1]
 if last=='.#confctl.settings': print(json.dumps({'nix':{'maxJobs':'auto','impureEval':False,'legacyNixPath':False}})); sys.exit(0)
 if last=='.#confctl.channels': print(json.dumps(s.get('channels',{'base':{'main':'a'},'other':{'main':'b'}}))); sys.exit(0)
 if last=='.#confctl.machineKeys': print(json.dumps(s.get('keys',{'alpha':'a-key','odd':'a."${key}'}))); sys.exit(0)
 if last.startswith('.#confctl.inputsInfo.'):
  print(json.dumps(s.get('info',{'main':{'input':'b'}}))); sys.exit(0)
 if 'flake' not in args: sys.exit(97)
 operation=args[args.index('flake')+1]
 if operation not in ['update','lock']: sys.exit(97)
 if s.get('experimental') and '--extra-experimental-features' not in args:
  print('experimental feature flakes disabled',file=sys.stderr); sys.exit(1)
 data=json.loads(pathlib.Path('flake.lock').read_text()) if pathlib.Path('flake.lock').exists() else s['initial']
 if operation=='update': names=args[args.index('update')+1:]; target=pathlib.Path('flake.lock')
 else: names=[args[args.index('--update-input')+1]]; target=pathlib.Path(args[args.index('--output-lock-file')+1])
 for name in names:
  node=data['nodes']['root']['inputs'][name]; node=node[0] if isinstance(node,list) else node
  data['nodes'][node]['locked']['rev']=s.get('rev','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb')
  if s.get('metadata_only'): data['nodes'][node]['locked']['rev']='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
  if s.get('remove_url'):
   data['nodes'][node]['locked'].pop('url',None)
   data['nodes'][node].get('original',{}).pop('url',None)
 if operation=='lock':
  for node in data['nodes'].values():
   if 'original' in node: node['original']={'type':'git','url':'child-replaced'}
 data['unknown']={'preserved':[False,None,7]}
 target.write_text(json.dumps(data))
 block('nix')
 if s.get('fail_input') in names: print('setter native failed',file=sys.stderr); sys.exit(23)
 if s.get('cached'): print("unable to download 'https://api.github.com/repos/fixture/repo': using cached version",file=sys.stderr)
 print('NIX-STDOUT'); print('NIX-STDERR',file=sys.stderr)
 sys.exit(0)
if kind=='git':
 if args[0]=='commit':
  p=pathlib.Path(args[args.index('-F')+1]); (c/'message-path').write_text(str(p)); (c/'message').write_bytes(p.read_bytes())
  block('commit')
  assert p.exists()
  if '-e' in args: subprocess.run([os.environ['GIT_EDITOR'],str(p)],check=True)
  if s.get('real_git'): os.execv(os.environ['MUT_REAL_GIT'],[os.environ['MUT_REAL_GIT'],*args])
  sys.exit(s.get('commit_exit',0))
 block('git')
 if s.get('git_exit') and (not s.get('git_fail_at') or s['git_fail_at'] in args): print('GIT-NATIVE-FAIL',file=sys.stderr); sys.exit(s['git_exit'])
 if args[0]=='clone': pathlib.Path(args[-1],'mirror-receipt').write_text('clone')
 elif args[0]=='-C' and args[2]=='fetch': pathlib.Path(args[1],'mirror-receipt').write_text('fetch')
 elif args[0]=='-C' and args[2]=='log':
  sys.stdout.buffer.write(b'< entry\xff\n' if s.get('raw') else b''.join(f'> {i:08} line\n'.encode() for i in range(80)))
 else: sys.exit(97)
 sys.exit(0)
raise RuntimeError('forbidden native tool '+kind)
"#.replacen("PYTHON",python.to_str().unwrap(),1);
        for tool in ["nix", "git"] {
            executable(&f.bin.join(tool), &script);
        }
        for tool in [
            "ssh",
            "nix-env",
            "nix-build",
            "nix-copy-closure",
            "reboot",
            "systemctl",
        ] {
            executable(&f.bin.join(tool), "#!/bin/sh\nexit 97\n");
        }
        f
    }
    fn scenario(&self, v: Value) {
        fs::write(
            self.control.join("scenario"),
            serde_json::to_vec(&v).unwrap(),
        )
        .unwrap();
    }
    fn lock(&self, v: Value) {
        fs::write(
            self.root.join("flake.lock"),
            serde_json::to_vec_pretty(&v).unwrap(),
        )
        .unwrap();
    }
    fn command(&self, args: &[&str]) -> Command {
        let mut c = Command::new(env!("CARGO_BIN_EXE_confctl-rust"));
        c.current_dir(&self.root)
            .arg("--color=never")
            .args(args)
            .env("PATH", &self.bin)
            .env("MUT_CONTROL", &self.control)
            .env("HOME", &self.control)
            .env("XDG_CACHE_HOME", &self.cache)
            .env("XDG_CONFIG_HOME", &self.control)
            .env("CONFCTL_TTY", "0")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        c
    }
    fn run(&self, args: &[&str]) -> Output {
        wait(self.command(args).spawn().unwrap())
    }
    fn calls(&self) -> Vec<Vec<String>> {
        let mut files = fs::read_dir(&self.control)
            .unwrap()
            .map(|e| e.unwrap().path())
            .filter(|p| {
                p.file_name()
                    .unwrap()
                    .to_string_lossy()
                    .starts_with("call-")
            })
            .collect::<Vec<_>>();
        files.sort();
        for call in &files {
            let name = call
                .file_name()
                .unwrap()
                .to_string_lossy()
                .replace("call-", "cwd-");
            assert_eq!(
                fs::read_to_string(self.control.join(name)).unwrap(),
                self.root.to_str().unwrap()
            );
        }
        files
            .into_iter()
            .map(|p| {
                let raw = fs::read(&p).unwrap();
                assert_eq!(raw.last(), Some(&0), "{p:?}: {raw:?}");
                raw[..raw.len() - 1]
                    .split(|b| *b == 0)
                    .map(|b| String::from_utf8(b.to_vec()).unwrap())
                    .collect()
            })
            .collect()
    }
    fn changed(&self) -> Value {
        serde_json::from_slice(&fs::read(self.root.join("flake.lock")).unwrap()).unwrap()
    }
    fn message(&self) -> Vec<u8> {
        let p = fs::read_to_string(self.control.join("message-path")).unwrap();
        assert!(!Path::new(&p).exists(), "message leaked: {p}");
        fs::read(self.control.join("message")).unwrap()
    }
    fn no_temps(&self) {
        assert!(fs::read_dir(&self.root).unwrap().all(|e| {
            let e = e.unwrap();
            let n = e.file_name();
            let n = n.to_string_lossy();
            !n.starts_with("confctl-input-message-") && !n.starts_with("flake.lock.tmp-")
        }));
    }
}
fn lock() -> Value {
    json!({"version":7,"nodes":{"root":{"inputs":{"b":"b","a":"a","alias":["a","ignored"]}},"a":{"original":{"type":"git","url":"https://repo.invalid/a/","ref":"keep","unknown":4},"locked":{"type":"git","url":"https://repo.invalid/a/","rev":"a".repeat(40)}},"b":{"original":{"type":"github","owner":"fixture","repo":"b","dir":"nested"},"locked":{"type":"github","owner":"fixture","repo":"b","rev":"a".repeat(40)}}}})
}
fn executable(p: &Path, s: &str) {
    fs::write(p, s).unwrap();
    fs::set_permissions(p, fs::Permissions::from_mode(0o755)).unwrap();
}
fn find(name: &str) -> PathBuf {
    std::env::split_paths(&std::env::var_os("PATH").unwrap())
        .map(|p| p.join(name))
        .find(|p| p.is_file())
        .unwrap()
        .canonicalize()
        .unwrap()
}
fn wait(mut c: Child) -> Output {
    drop(c.stdin.take());
    let deadline = Instant::now() + Duration::from_secs(30);
    while c.try_wait().unwrap().is_none() {
        if Instant::now() > deadline {
            c.kill().unwrap();
            panic!("fixture timed out: {:?}", c.wait_with_output());
        }
        thread::sleep(Duration::from_millis(5));
    }
    c.wait_with_output().unwrap()
}
fn pass(o: &Output) {
    assert!(o.status.success(), "{o:?}");
}
fn fail(o: &Output, text: &str) {
    assert_eq!(o.status.code(), Some(1), "{o:?}");
    assert!(String::from_utf8_lossy(&o.stderr).contains(text), "{o:?}");
}
fn mutcalls(f: &Fixture) -> Vec<Vec<String>> {
    f.calls()
        .into_iter()
        .filter(|a| a[0] == "nix" && a.iter().any(|s| s == "flake"))
        .collect()
}

#[test]
fn root_all_and_explicit_order_duplicates_absent_lock_and_no_commit_effects() {
    let f = Fixture::new();
    let o = f.run(&["inputs", "update", "b", "a", "b"]);
    pass(&o);
    assert!(String::from_utf8_lossy(&o.stdout).contains("Updated inputs (3)"));
    assert_eq!(
        mutcalls(&f),
        [vec!["nix", "flake", "update", "b", "a", "b"]]
    );
    assert_eq!(f.calls().len(), 1);
    let f = Fixture::new();
    pass(&f.run(&["inputs", "update", "--all", "ignored"]));
    assert_eq!(
        mutcalls(&f),
        [vec!["nix", "flake", "update", "a", "alias", "b"]]
    );
    let f = Fixture::new();
    fs::remove_file(f.root.join("flake.lock")).unwrap();
    fail(&f.run(&["inputs", "update", "--all"]), "flake.lock missing");
    assert!(f.calls().is_empty());
    f.scenario(json!({"initial":lock()}));
    pass(&f.run(&["inputs", "update", "a"]));
    assert_eq!(f.calls().len(), 1);
}
#[test]
fn setters_native_urls_serial_original_restoration_preplan_and_partial_failure() {
    let f = Fixture::new();
    let old = lock();
    f.scenario(json!({"channels":{"base":{"first":"b","second":"a"}},"fail_input":"a"}));
    let o = f.run(&["inputs", "channel", "set", "base", "first", "new"]);
    pass(&o);
    let calls = mutcalls(&f);
    assert_eq!(
        &calls[0][..8],
        [
            "nix",
            "flake",
            "lock",
            "--update-input",
            "b",
            "--override-input",
            "b",
            "github:fixture/b/new?dir=nested"
        ]
    );
    assert_eq!(
        f.changed()["nodes"]["b"]["original"],
        old["nodes"]["b"]["original"]
    );
    f.no_temps();
}
#[test]
fn channel_selection_shared_only_set_and_summaries_from_actual_lock() {
    let f = Fixture::new();
    f.scenario(json!({"channels":{"z":{"x":"b"},"a":{"x":"a"},"other":{"second":"a"}}}));
    fail(
        &f.run(&["inputs", "channel", "set", "a", "x", "new"]),
        "shared outside",
    );
    assert!(mutcalls(&f).is_empty());
    let o = f.run(&[
        "inputs",
        "channel",
        "set",
        "--allow_shared",
        "a",
        "x",
        "new",
    ]);
    pass(&o);
    assert!(String::from_utf8_lossy(&o.stdout).contains("Warning:"));
    assert!(String::from_utf8_lossy(&o.stdout).contains("Configuring x in a -> bbbbbbbb"));
    let f = Fixture::new();
    f.scenario(json!({"channels":{"z":{"x":"b"},"a":{"x":"a"},"empty":false}}));
    pass(&f.run(&["inputs", "channel", "update", "{z,a,z,missing}"]));
    assert_eq!(mutcalls(&f), [vec!["nix", "flake", "update", "b", "a"]]);
}
#[test]
fn selected_mapping_validation_null_false_empty_and_machine_native_key() {
    for value in [json!([]), json!({"main":8})] {
        let f = Fixture::new();
        f.scenario(json!({"channels":{"base":value}}));
        fail(&f.run(&["inputs", "channel", "update", "base"]), "not");
        assert!(mutcalls(&f).is_empty());
    }
    for value in [
        Value::Null,
        json!(false),
        json!({"main":null}),
        json!({"main":false}),
    ] {
        let f = Fixture::new();
        f.scenario(json!({"channels":{"base":value}}));
        fail(
            &f.run(&["inputs", "channel", "update", "base", "main"]),
            "no inputs",
        );
        assert!(mutcalls(&f).is_empty());
    }
    let f = Fixture::new();
    f.scenario(json!({"keys":{"odd":"a.\"${key}"}}));
    pass(&f.run(&["inputs", "machine", "set", "odd", "main", "next"]));
    let calls = f.calls();
    assert!(
        calls
            .iter()
            .any(|c| c.last().unwrap() == ".#confctl.inputsInfo.\"a.\\\"\\${key}\""),
        "{calls:?}"
    );
    assert_eq!(mutcalls(&f)[0][4], "b");
    let f = Fixture::new();
    pass(&f.run(&["inputs", "machine", "update", "a-key", "main"]));
    assert_eq!(mutcalls(&f), [vec!["nix", "flake", "update", "b"]]);
    let f = Fixture::new();
    f.scenario(json!({"info":{"main":{"input":9}}}));
    fail(
        &f.run(&["inputs", "machine", "update", "alpha", "main"]),
        "not a string",
    );
    assert!(mutcalls(&f).is_empty());
}
#[test]
fn metadata_only_suppresses_git_cached_warning_fails_and_recognized_retry_once() {
    let f = Fixture::new();
    f.scenario(json!({"metadata_only":true}));
    let o = f.run(&["inputs", "update", "--commit", "a"]);
    pass(&o);
    assert!(String::from_utf8_lossy(&o.stdout).contains("No changes."));
    assert_eq!(f.calls().len(), 1);
    let f = Fixture::new();
    f.scenario(json!({"cached":true}));
    fail(
        &f.run(&["inputs", "update", "--commit", "a"]),
        "cached version",
    );
    assert_eq!(f.calls().len(), 1);
    assert_eq!(f.changed()["nodes"]["a"]["locked"]["rev"], "b".repeat(40));
    let f = Fixture::new();
    f.scenario(json!({"experimental":true}));
    pass(&f.run(&["inputs", "update", "a"]));
    let calls = mutcalls(&f);
    assert_eq!(calls.len(), 2);
    assert_eq!(
        &calls[1][1..5],
        [
            "--extra-experimental-features",
            "nix-command",
            "--extra-experimental-features",
            "flakes"
        ]
    );
}
#[test]
fn actual_message_file_unlimited_log_raw_url_cache_fetch_downgrade_and_flags() {
    let f = Fixture::new();
    let o = f.run(&["inputs", "update", "--commit", "--downgrade", "a"]);
    pass(&o);
    let calls = f.calls();
    assert_eq!(calls.len(), 4);
    assert_eq!(
        &calls[1][..5],
        ["git", "clone", "--quiet", "--mirror", "--"]
    );
    assert_eq!(calls[1][5], "https://repo.invalid/a/");
    let log = &calls[2];
    assert!(!log.iter().any(|a| a.contains("max-count")));
    assert_eq!(
        log[log.len() - 2],
        format!("{}..{}", "b".repeat(40), "a".repeat(40))
    );
    assert_eq!(log.last().unwrap(), "--");
    let message = String::from_utf8(f.message()).unwrap();
    assert!(
        message
            .starts_with("inputs: update a to bbbbbbbb\n\na: aaaaaaaa -> bbbbbbbb\ngit log for ")
    );
    assert!(message.contains("> 00000079 line"));
    assert_eq!(calls[3][..3], ["git", "commit", "-F"]);
    assert_eq!(&calls[3][4..], ["--", "flake.lock"]);
    f.no_temps();
    f.scenario(json!({"rev":"c".repeat(40),"raw":true}));
    pass(&f.run(&["inputs", "update", "--commit", "a"]));
    assert!(
        f.calls()
            .iter()
            .any(|a| a.get(3).map(String::as_str) == Some("fetch"))
    );
    assert!(f.message().ends_with(b"< entry\xff"));
    let f = Fixture::new();
    pass(&f.run(&["inputs", "set", "--commit", "--no-changelog", "a", "branch"]));
    assert_eq!(f.calls().len(), 2);
    assert!(
        String::from_utf8(f.message())
            .unwrap()
            .starts_with("inputs: set a to bbbbbbbb")
    );
}
#[test]
fn complete_native_and_cache_fallback_commit_failure_and_spawn_is_fatal() {
    for exit in [17, 0] {
        let f = Fixture::new();
        if exit == 0 {
            fs::write(&f.cache, b"not a directory").unwrap();
        }
        f.scenario(json!({"git_exit":exit}));
        pass(&f.run(&["inputs", "update", "--commit", "a"]));
        assert!(
            String::from_utf8(f.message())
                .unwrap()
                .contains("changelog unavailable")
        );
    }
    let f = Fixture::new();
    f.scenario(json!({"commit_exit":19}));
    let o = f.run(&["inputs", "update", "--commit", "--no-changelog", "a"]);
    fail(&o, "Git commit");
    assert!(!String::from_utf8_lossy(&o.stdout).contains("Updated inputs"));
    f.message();
    let f = Fixture::new();
    fs::remove_file(f.bin.join("git")).unwrap();
    fail(
        &f.run(&["inputs", "update", "--commit", "a"]),
        "unable to start",
    );
    assert!(!f.control.join("message").exists());
}
#[test]
fn cancellation_joins_mutation_history_and_commit_descendants_retains_lock_and_cleans_temp() {
    for stage in ["nix", "git", "commit"] {
        let f = Fixture::new();
        f.scenario(json!({"block":stage}));
        let mut c = f
            .command(&["inputs", "update", "--commit", "a"])
            .spawn()
            .unwrap();
        drop(c.stdin.take());
        let native = pid(&f.control.join("native-pid"));
        let descendant = pid(&f.control.join("descendant-pid"));
        nix::sys::signal::kill(
            nix::unistd::Pid::from_raw(c.id() as i32),
            nix::sys::signal::Signal::SIGINT,
        )
        .unwrap();
        let o = wait(c);
        assert_eq!(o.status.code(), Some(130), "{stage}: {o:?}");
        for p in [native, descendant] {
            assert!(
                !Path::new(&format!("/proc/{p}")).exists(),
                "{stage} child {p} alive"
            );
        }
        f.no_temps();
        assert!(!String::from_utf8_lossy(&o.stdout).contains("Updated inputs"));
    }
}
fn pid(path: &Path) -> u32 {
    let until = Instant::now() + Duration::from_secs(10);
    loop {
        if let Ok(text) = fs::read_to_string(path)
            && let Ok(n) = text.parse::<u32>()
            && n > 0
        {
            return n;
        }
        assert!(Instant::now() < until, "no PID at {}", path.display());
        thread::sleep(Duration::from_millis(5));
    }
}

#[test]
fn isolated_real_git_path_commit_preserves_unrelated_index_and_runs_hook() {
    let f = Fixture::new();
    let git = find("git");
    let run = |args: &[&str]| {
        let o = Command::new(&git)
            .current_dir(&f.root)
            .args(args)
            .env("HOME", &f.control)
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .output()
            .unwrap();
        pass(&o);
        o
    };
    run(&["init", "-q"]);
    run(&["config", "user.name", "Confctl fixture"]);
    run(&["config", "user.email", "fixture@example.invalid"]);
    run(&["config", "commit.gpgsign", "false"]);
    run(&["add", "--", "flake.lock", "flake.nix"]);
    run(&["commit", "-q", "-m", "fixture baseline"]);
    fs::write(f.root.join("unrelated"), "staged bytes\n").unwrap();
    run(&["add", "--", "unrelated"]);
    executable(
        &f.root.join(".git/hooks/pre-commit"),
        "#!/bin/sh\nprintf hook > hook-ran\n",
    );
    executable(&f.bin.join("fake-editor"), "#!/bin/sh\nexit 97\n");
    f.scenario(json!({"real_git":true}));
    let o = f
        .command(&["inputs", "update", "--commit", "--no-changelog", "a"])
        .env("MUT_REAL_GIT", &git)
        .env("GIT_EDITOR", f.bin.join("fake-editor"))
        .output()
        .unwrap();
    pass(&o);
    let head = run(&["show", "HEAD:flake.lock"]);
    assert_eq!(
        serde_json::from_slice::<Value>(&head.stdout).unwrap(),
        f.changed()
    );
    assert_eq!(
        run(&["diff", "--cached", "--name-only"]).stdout,
        b"unrelated\n"
    );
    assert!(
        !run(&["ls-tree", "--name-only", "HEAD"])
            .stdout
            .windows(9)
            .any(|b| b == b"unrelated")
    );
    assert!(f.root.join("hook-ran").exists());
    f.message();
    f.no_temps();
}

#[test]
fn setter_preplans_all_inputs_and_retains_only_completed_publications() {
    let f = Fixture::new();
    let old = lock();
    f.scenario(json!({"channels":{"first":{"main":"b"},"second":{"main":"a"}},"fail_input":"a"}));
    fail(
        &f.run(&["inputs", "channel", "set", "{first,second}", "main", "next"]),
        "nix input lock mutation",
    );
    assert_eq!(mutcalls(&f).len(), 2);
    assert_eq!(f.changed()["nodes"]["b"]["locked"]["rev"], "b".repeat(40));
    assert_eq!(f.changed()["nodes"]["a"]["locked"]["rev"], "a".repeat(40));
    for name in ["a", "b"] {
        assert_eq!(
            f.changed()["nodes"][name]["original"],
            old["nodes"][name]["original"]
        );
    }
    f.no_temps();
    let f = Fixture::new();
    let mut invalid = lock();
    invalid["nodes"]["a"]["locked"]["type"] = json!("path");
    f.lock(invalid.clone());
    f.scenario(json!({"channels":{"first":{"main":"b"},"second":{"main":"a"}}}));
    fail(
        &f.run(&["inputs", "channel", "set", "{first,second}", "main", "next"]),
        "unsupported type",
    );
    assert!(mutcalls(&f).is_empty());
    assert_eq!(f.changed(), invalid);
}
#[test]
fn empty_literal_input_role_filters_globs_and_shared_scan_validation() {
    let f = Fixture::new();
    let mut data = lock();
    data["nodes"]["root"]["inputs"][""] = json!("a");
    f.lock(data);
    f.scenario(json!({"channels":{"base":{"main":""}}}));
    pass(&f.run(&["inputs", "channel", "update", "b*", "main"]));
    assert_eq!(mutcalls(&f), [vec!["nix", "flake", "update", ""]]);
    let f = Fixture::new();
    f.scenario(json!({"channels":{"base":{"main":"a"},"outside":[8]}}));
    fail(
        &f.run(&["inputs", "channel", "set", "base", "main", "next"]),
        "not an object",
    );
    assert!(mutcalls(&f).is_empty());
    let f = Fixture::new();
    f.scenario(json!({"metadata_only":true}));
    let o = f.run(&["inputs", "machine", "update", "alpha", "main"]);
    pass(&o);
    assert!(String::from_utf8_lossy(&o.stdout).contains("Updating main in alpha -> aaaaaaaa"));
}

mod pty {
    use super::*;
    use std::{io, os::fd::OwnedFd};
    const TEST: &str = "pty::git_editor_uses_actual_stdout_tty_and_restores_terminal";
    #[test]
    fn git_editor_uses_actual_stdout_tty_and_restores_terminal() {
        if let Ok(case) = std::env::var("MUT_PTY") {
            child(&case);
            return;
        }
        for case in [
            "all",
            "plain",
            "piped-input",
            "piped-output",
            "no-editor",
            "cancel",
        ] {
            let (master, slave) = open();
            let mut c = Command::new(std::env::current_exe().unwrap())
                .args(["--exact", TEST, "--nocapture"])
                .env("MUT_PTY", case)
                .stdin(Stdio::from(slave.try_clone().unwrap()))
                .stdout(Stdio::from(slave.try_clone().unwrap()))
                .stderr(Stdio::from(slave))
                .spawn()
                .unwrap();
            let flags = rustix::fs::fcntl_getfl(&master).unwrap();
            rustix::fs::fcntl_setfl(&master, flags | rustix::fs::OFlags::NONBLOCK).unwrap();
            let until = Instant::now() + Duration::from_secs(30);
            let mut bytes = Vec::new();
            let status = loop {
                drain(&master, &mut bytes);
                if let Some(s) = c.try_wait().unwrap() {
                    drain(&master, &mut bytes);
                    break s;
                }
                if Instant::now() > until {
                    c.kill().unwrap();
                    let _ = c.wait();
                    panic!("{case} timed out: {}", String::from_utf8_lossy(&bytes));
                }
                thread::sleep(Duration::from_millis(5));
            };
            assert!(
                status.success(),
                "{case}: {}",
                String::from_utf8_lossy(&bytes)
            );
            assert!(String::from_utf8_lossy(&bytes).contains("INPUT_PTY_OK"));
        }
    }
    fn child(case: &str) {
        rustix::process::setsid().unwrap();
        rustix::process::ioctl_tiocsctty(io::stdin()).unwrap();
        let modes = rustix::termios::tcgetattr(io::stdin()).unwrap();
        let group = rustix::process::getpgrp();
        let f = Fixture::new();
        executable(
            &f.bin.join("editor"),
            &format!(
                "#!{}\nimport os,pathlib,sys\np=pathlib.Path(os.environ['MUT_CONTROL'])\n(p/'editor-ran').write_bytes(pathlib.Path(sys.argv[1]).read_bytes())\n",
                find("python3").display()
            ),
        );
        let mut args = vec!["inputs", "update", "--commit", "--no-changelog", "a"];
        if case == "no-editor" {
            args.insert(args.len() - 1, "--no-editor");
        }
        let mut command = f.command(&args);
        command
            .env("GIT_EDITOR", f.bin.join("editor"))
            .env("CONFCTL_TTY", if case == "plain" { "0" } else { "1" });
        if case != "piped-input" {
            command.stdin(Stdio::inherit());
        }
        if case != "piped-output" {
            command.stdout(Stdio::inherit());
        }
        command.stderr(Stdio::inherit());
        if case == "cancel" {
            f.scenario(json!({"block":"commit"}));
            let mut c = command.spawn().unwrap();
            let native = pid(&f.control.join("native-pid"));
            let descendant = pid(&f.control.join("descendant-pid"));
            nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(c.id() as i32),
                nix::sys::signal::Signal::SIGINT,
            )
            .unwrap();
            assert_eq!(c.wait().unwrap().code(), Some(130));
            for p in [native, descendant] {
                assert!(!Path::new(&format!("/proc/{p}")).exists());
            }
        } else {
            let o = command.spawn().unwrap().wait_with_output().unwrap();
            pass(&o);
            let calls = f.calls();
            let commit = calls
                .iter()
                .find(|c| c.get(1).map(String::as_str) == Some("commit"))
                .unwrap();
            let expected = !matches!(case, "piped-output" | "no-editor");
            assert_eq!(
                commit.iter().any(|a| a == "-e"),
                expected,
                "{case}: {calls:?}"
            );
            assert_eq!(f.control.join("editor-ran").exists(), expected);
            if case == "piped-output" {
                assert!(String::from_utf8_lossy(&o.stdout).contains("Updated inputs"));
            }
        }
        let restored = rustix::termios::tcgetattr(io::stdin()).unwrap();
        assert_eq!(restored.local_modes, modes.local_modes);
        assert_eq!(restored.input_modes, modes.input_modes);
        assert_eq!(restored.output_modes, modes.output_modes);
        assert_eq!(restored.control_modes, modes.control_modes);
        assert_eq!(rustix::termios::tcgetpgrp(io::stdin()).unwrap(), group);
        f.no_temps();
        println!("INPUT_PTY_OK");
    }
    fn open() -> (OwnedFd, OwnedFd) {
        use rustix::pty::{OpenptFlags, grantpt, ioctl_tiocgptpeer, openpt, unlockpt};
        let flags = OpenptFlags::RDWR | OpenptFlags::NOCTTY | OpenptFlags::CLOEXEC;
        let master = openpt(flags).unwrap();
        grantpt(&master).unwrap();
        unlockpt(&master).unwrap();
        let slave = ioctl_tiocgptpeer(&master, flags).unwrap();
        (master, slave)
    }
    fn drain(fd: &OwnedFd, bytes: &mut Vec<u8>) {
        let mut buf = [0; 8192];
        loop {
            match rustix::io::read(fd, &mut buf) {
                Ok(0) | Err(rustix::io::Errno::AGAIN) | Err(rustix::io::Errno::IO) => break,
                Ok(n) => bytes.extend_from_slice(&buf[..n]),
                Err(e) => panic!("{e}"),
            }
        }
    }
}

#[test]
fn message_grouping_raw_url_fallback_missing_url_and_complete_log_failure() {
    let f = Fixture::new();
    let mut data = lock();
    data["nodes"]["b"]["original"] = json!({"type":"git","url":"https://repo.invalid/a"});
    data["nodes"]["b"]["locked"] =
        json!({"type":"git","url":"https://repo.invalid/a","rev":"a".repeat(40)});
    f.lock(data);
    pass(&f.run(&["inputs", "update", "--commit", "b", "a"]));
    assert_eq!(
        f.calls()
            .iter()
            .filter(|c| c.get(1).map(String::as_str) == Some("clone"))
            .count(),
        1
    );
    let message = String::from_utf8(f.message()).unwrap();
    assert!(message.contains("b: aaaaaaaa -> bbbbbbbb\na: aaaaaaaa -> bbbbbbbb\ngit log"));
    assert!(message.starts_with("inputs: update a, b to bbbbbbbb"));
    assert_eq!(f.calls()[1][5], "https://repo.invalid/a");
    let f = Fixture::new();
    f.scenario(json!({"remove_url":true,"git_exit":17,"git_fail_at":"log"}));
    pass(&f.run(&["inputs", "update", "--commit", "a"]));
    let calls = f.calls();
    assert_eq!(calls[1][5], "https://repo.invalid/a/");
    assert!(calls[2].iter().any(|a| a == "log"));
    assert!(
        String::from_utf8(f.message())
            .unwrap()
            .contains("changelog unavailable")
    );
    let f = Fixture::new();
    let mut data = lock();
    data["nodes"]["a"]["original"]
        .as_object_mut()
        .unwrap()
        .remove("url");
    data["nodes"]["a"]["locked"]
        .as_object_mut()
        .unwrap()
        .remove("url");
    f.lock(data);
    pass(&f.run(&["inputs", "update", "--commit", "a"]));
    assert_eq!(f.calls().len(), 2);
    assert_eq!(
        f.message(),
        b"inputs: update a to bbbbbbbb\n\na: aaaaaaaa -> bbbbbbbb"
    );
}
#[test]
fn setter_cancellation_before_publication_keeps_original_lock() {
    let f = Fixture::new();
    let before = fs::read(f.root.join("flake.lock")).unwrap();
    f.scenario(json!({"block":"nix"}));
    let mut c = f
        .command(&["inputs", "set", "--commit", "a", "next"])
        .spawn()
        .unwrap();
    drop(c.stdin.take());
    let native = pid(&f.control.join("native-pid"));
    let descendant = pid(&f.control.join("descendant-pid"));
    nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(c.id() as i32),
        nix::sys::signal::Signal::SIGINT,
    )
    .unwrap();
    assert_eq!(wait(c).status.code(), Some(130));
    assert_eq!(fs::read(f.root.join("flake.lock")).unwrap(), before);
    f.no_temps();
    for p in [native, descendant] {
        assert!(!Path::new(&format!("/proc/{p}")).exists());
    }
    assert_eq!(f.calls().len(), 1);
}
#[test]
fn output_failure_stops_before_commit_and_retains_native_lock_effects() {
    use std::os::{fd::OwnedFd, unix::net::UnixStream};
    let f = Fixture::new();
    let (writer, reader) = UnixStream::pair().unwrap();
    drop(reader);
    let o = f
        .command(&["inputs", "update", "--commit", "a"])
        .stdout(Stdio::from(OwnedFd::from(writer)))
        .output()
        .unwrap();
    assert_eq!(o.status.code(), Some(1), "{o:?}");
    assert!(!f.control.join("message").exists());
    assert_eq!(f.changed()["nodes"]["a"]["locked"]["rev"], "b".repeat(40));
    assert!(f.calls().iter().all(|c| c[0] == "nix"));
    f.no_temps();
}
