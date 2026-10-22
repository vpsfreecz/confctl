use serde_json::{Value, json};
use std::{
    fs,
    io::Write,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
};
pub struct Fixture {
    _scratch: std::cell::RefCell<Option<tempfile::TempDir>>,
    pub root: PathBuf,
    pub bin: PathBuf,
}
impl Default for Fixture {
    fn default() -> Self {
        Self::new()
    }
}
impl Fixture {
    pub fn new() -> Self {
        let scratch = tempfile::tempdir().unwrap();
        let root = scratch.path().join("config");
        let bin = scratch.path().join("bin");
        fs::create_dir(&root).unwrap();
        fs::create_dir(&bin).unwrap();
        fs::write(root.join("flake.nix"), "{outputs=_:{};}\n").unwrap();
        let retention = json!({"min":1,"max":2,"maxAge":9999999999_i64,"collectGarbage":true});
        fs::write(root.join("settings.json"), serde_json::to_vec(&json!({"list":{"columns":["name"]},"nix":{},"buildGenerations":retention,"hostGenerations":retention})).unwrap()).unwrap();
        fs::write(root.join("machines.json"), serde_json::to_vec(&json!({
            "a":{"name":"alpha","metaConfig":{"managed":false,"host":{"target":"a.invalid"},"tags":["chosen"]}},
            "b":{"name":"beta","metaConfig":{"managed":true,"host":{"target":"b.invalid"},"tags":["chosen"]}},
            "c":{"name":"carrier#one","carrier":"carrier","alias":"guest/one","metaConfig":{"host":{"target":null},"tags":["guest"]}},
            "d":{"name":"carrier#two","carrier":"carrier","alias":"guest/two","metaConfig":{"host":{"target":null},"tags":["guest"]}},
            "e":{"name":"carrier","metaConfig":{"host":{"target":"carrier.invalid","port":2222},"tags":["helper"]}},
            "f":{"name":"null","metaConfig":{"host":{"target":null}}}
        })).unwrap()).unwrap();
        fs::write(root.join("profiles.json"), b"{}").unwrap();
        // Nix sandboxes provide /bin/sh but need not provide /usr/bin/env.
        // The shell trampoline keeps Python on PATH and preserves this tool's argv.
        let source = r#"#!/bin/sh
""":"
exec python3 "$0" "$@"
":"""
import os,sys,json,shlex,pathlib,time,signal,select
r=pathlib.Path(os.environ['CLEANUP_ROOT'])
tool=pathlib.Path(sys.argv[0]).name
args=sys.argv[1:]
with open(r/'calls','ab',buffering=0) as f: f.write((json.dumps([tool,args])+'\n').encode())
if tool=='nix':
 target=args[-1]
 if target=='.#confctl.settings': print((r/'settings.json').read_text())
 elif target=='.#confctl.machinesJson': print(json.dumps([{'outputs':{'out':str(r/'machines.json')}}]))
 else: sys.stderr.write('forbidden Nix target '+target);sys.exit(97)
 sys.exit(0)
host='local'; command=args
if tool=='ssh':
 host=next((a.split('.')[0] for a in args if a.endswith('.invalid')),'missing')
 command=shlex.split(args[-1])
 if command==['cat','/proc/uptime']:
  if (r/(host+'.offline')).exists(): sys.stderr.write('offline fixture');sys.exit(255)
  print('10 0');sys.exit(0)
 if command==['bash','--norc']:
  script=sys.stdin.read();profile=shlex.split(script.splitlines()[0][8:])[0]
  data=json.loads((r/'profiles.json').read_text()).get(profile,[])
  print(data[0]['path'] if data else '/empty/profile')
  for entry in data: print(profile+'-'+str(entry['id'])+'-link;'+entry['path']+';'+str(entry['time'])+';')
  sys.exit(0)
 if command and command[0] in ['nix-env','carrier-env']:
  with open(r/'effects','a') as f: f.write(host+' '+json.dumps(command)+'\n')
  if (r/'check-local-removed').exists():
   assert not (r/'.confctl/generations/alpha/saved').exists(), 'local must precede remote'
  if (r/'check-rotation-local').exists(): assert (r/'.confctl/generations/alpha/old').exists(), 'remote rotation must precede local'
  if (r/'delete-fail').exists(): sys.stderr.write('callback failed after profile deletion');sys.exit(9)
  sys.exit(0)
if command!=['nix-collect-garbage'] and tool!='nix-collect-garbage': sys.stderr.write('forbidden native command');sys.exit(97)
# Publish active/PID records under a blocked TERM, then install the handler and
# close every startup write before restoring the mask. Moving signal.signal
# alone would still allow interruption in the middle of active.write_text.
old_mask=signal.pthread_sigmask(signal.SIG_BLOCK,{signal.SIGTERM})
active=r/('active-'+host)
record={'host':host,'pid':os.getpid(),'pgid':os.getpgrp()}
active.write_text(json.dumps(record))
if (r/'startup-term').exists(): os.kill(os.getpid(),signal.SIGTERM)
record['pending_term']=signal.SIGTERM in signal.sigpending()
(r/('started-'+host)).write_text(json.dumps(record))
with open(r/'concurrency','a') as f: f.write(str(len(list(r.glob('active-*'))))+'\n')
def stopped(signum,frame):
 if (r/'ignore-term').exists():
  (r/('ignored-term-'+host)).write_text(str(signum));return
 active.unlink(missing_ok=True)
 (r/('stopped-'+host)).write_text(json.dumps(dict(record,signal=signum)))
 sys.exit(128+signum)
signal.signal(signal.SIGTERM,stopped)
signal.pthread_sigmask(signal.SIG_SETMASK,old_mask)
record['ready']=True
(r/('ready-'+host)).write_text(json.dumps(record))
if (r/'direct-wait-release').exists():
 release=os.open(r/'direct-wait-release',os.O_RDONLY|os.O_NONBLOCK)
 ready=os.open(r/'ready-events',os.O_WRONLY|os.O_NONBLOCK)
 os.write(ready,(json.dumps(record)+'\n').encode());os.close(ready)
 polling=select.poll();polling.register(release,select.POLLIN)
 if not polling.poll(8000): raise TimeoutError('direct child release deadline')
 if os.read(release,1)!=b'1': raise RuntimeError('direct child invalid release')
 os.close(release);active.unlink(missing_ok=True);sys.exit(0)
# Only the output-failure test creates these FIFOs. Announce after unmasking;
# neither child emits output until the parent has checked both ready records.
if (r/('release-'+host)).exists():
 release=os.open(r/('release-'+host),os.O_RDONLY|os.O_NONBLOCK)
 ready=os.open(r/'ready-events',os.O_WRONLY|os.O_NONBLOCK)
 os.write(ready,(json.dumps(record)+'\n').encode());os.close(ready)
 polling=select.poll();polling.register(release,select.POLLIN)
 if not polling.poll(8000): raise TimeoutError('GC fixture release deadline')
 if os.read(release,1)!=b'1': raise RuntimeError('GC fixture invalid release')
 os.close(release)
sys.stdout.buffer.write(b"deleting '/nix/store/stdout-path'\nordinary\xff output\n");sys.stdout.flush()
sys.stderr.buffer.write(b"deleting '/nix/store/stderr-path'\n");sys.stderr.flush()
while (r/'gate').exists(): time.sleep(.01)
if (r/(host+'.signal')).exists():
 active.unlink(missing_ok=True);signal.signal(signal.SIGTERM,signal.SIG_DFL);os.kill(os.getpid(),signal.SIGTERM)
sys.stdout.write('2 store paths deleted, 1 MiB freed\n');sys.stdout.flush()
active.unlink(missing_ok=True)
sys.exit(7 if (r/(host+'.fail')).exists() else 0)
"#;
        for tool in ["nix", "ssh", "nix-collect-garbage"] {
            // Only the writer child owns the writable executable descriptor, so
            // concurrent parent forks cannot inherit it.
            let path = bin.join(tool);
            let status = Command::new("/bin/sh")
                .args(["-c", "set -C; printf '%s' \"$2\" > \"$1\"", "write-fixture"])
                .arg(&path)
                .arg(source)
                .status()
                .unwrap();
            assert!(status.success(), "fixture writer failed: {status}");
            fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
        }
        Self {
            _scratch: std::cell::RefCell::new(Some(scratch)),
            root,
            bin,
        }
    }
    // Keep the exact fixture only when bounded cleanup cannot establish terminal
    // evidence. Retention is diagnostic storage, not transfer of process ownership.
    // Only GC tests use retention; the shared generation-cleanup library and
    // command tests intentionally leave this optional capability unused.
    #[allow(dead_code)]
    pub fn preserve(&self) -> Option<PathBuf> {
        self._scratch
            .borrow_mut()
            .take()
            .map(tempfile::TempDir::keep)
    }
    pub fn command(&self, binary: &Path, args: &[&str]) -> Command {
        let path = std::env::join_paths(
            std::iter::once(self.bin.clone())
                .chain(std::env::split_paths(&std::env::var_os("PATH").unwrap())),
        )
        .unwrap();
        let mut command = Command::new(binary);
        command
            .args(args)
            .current_dir(&self.root)
            .env("PATH", path)
            .env("CLEANUP_ROOT", &self.root)
            .env("TZ", "UTC")
            .env("CONFCTL_SSH_CONFIG", self.root.join("ssh config"))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        command
    }
    pub fn run(&self, binary: &Path, args: &[&str], input: &[u8]) -> Output {
        let mut child = self.command(binary, args).spawn().unwrap();
        child.stdin.take().unwrap().write_all(input).unwrap();
        child.wait_with_output().unwrap()
    }
    pub fn profiles(&self, entries: Value) {
        fs::write(
            self.root.join("profiles.json"),
            serde_json::to_vec(&entries).unwrap(),
        )
        .unwrap();
    }
    pub fn calls(&self) -> String {
        fs::read_to_string(self.root.join("calls")).unwrap_or_default()
    }
    pub fn no_deletions(&self) {
        assert!(!self.root.join("effects").exists());
        assert!(!self.root.join("concurrency").exists());
    }
    pub fn save(&self, host: &str, name: &str, day: u32, path: &str) -> Vec<u8> {
        let dir = self
            .root
            .join(".confctl/generations")
            .join(host.replace('/', ":"))
            .join(name);
        fs::create_dir_all(&dir).unwrap();
        let bytes=serde_json::to_vec(&json!({"mode":"flakes","date":format!("2000-01-{day:02}T00:00:00Z"),"toplevel":path,"inputs_info":{"role":"revision"}})).unwrap();
        fs::write(dir.join("generation.json"), &bytes).unwrap();
        std::os::unix::fs::symlink(path, dir.join("toplevel")).unwrap();
        bytes
    }
}
pub fn success(output: &Output) -> String {
    assert!(
        output.status.success(),
        "stdout={}\nstderr={}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8_lossy(&output.stdout).into_owned()
}
