//! The lock probe occurs in TERM cleanup while the owned child is still alive.
use super::*;
use crate::{
    configuration::{ConfigRoot, MutationLock},
    events::{OutputStream, Style, TaskId, UiEvent},
    process::{ProcessRunner, ProcessScope, ProcessSpec},
};
use serde_json::{Value, json};
use std::{
    fs,
    future::{pending, poll_fn},
    process::Stdio,
    sync::atomic::AtomicUsize,
    task::Poll,
};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    net::{TcpListener, TcpStream},
    process::Command,
};

const CONTENDER: &str = r#"
import fcntl,json,os,pathlib,socket,sys
r=pathlib.Path(sys.argv[1])
s=socket.socket();s.bind(('127.0.0.1',0));s.listen(1);s.settimeout(10)
print(json.dumps({'phase':'contender-ready','pid':os.getpid(),'pgid':os.getpgrp(),'port':s.getsockname()[1]}),flush=True)
for phase in ['term','after']:
 c,_=s.accept();c.settimeout(1)
 request=json.loads(c.makefile('rb').readline());assert request['phase']==phase,request
 pid=request['pid']
 def alive():
  try: os.kill(pid,0);return True
  except ProcessLookupError: return False
 before=alive()
 with open(r/'.confctl/mutation.lock','r+') as f:
  st=os.fstat(f.fileno());acquired=False;error=0
  try: fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB);acquired=True
  except BlockingIOError as e: error=e.errno
  result={'phase':phase,'pid':os.getpid(),'pgid':os.getpgrp(),'child_pid':pid,
   'alive_before':before,'alive_after':alive(),'acquired':acquired,'errno':error,
   'device':st.st_dev,'inode':st.st_ino}
  if acquired: fcntl.flock(f,fcntl.LOCK_UN)
 c.sendall((json.dumps(result)+'\n').encode());c.close()
 print(json.dumps(result),flush=True)
"#;

const CHILD: &str = r#"
import json,os,pathlib,signal,socket,sys
r=pathlib.Path(sys.argv[1])
def term(signum,frame):
 s=socket.socket();s.settimeout(1);s.connect(('127.0.0.1',int(sys.argv[3])))
 s.sendall((json.dumps({'phase':'term','pid':os.getpid()})+'\n').encode())
 result=json.loads(s.makefile('rb').readline());s.close()
 result.update({'signal':signum,'child_pgid':os.getpgrp(),'exit_requested':23})
 (r/'term.json').write_text(json.dumps(result))
 sys.exit(23)
signal.signal(signal.SIGTERM,term)
s=socket.socket();s.settimeout(5);s.connect(('127.0.0.1',int(sys.argv[2])))
s.sendall((json.dumps({'phase':'child-ready','pid':os.getpid(),'pgid':os.getpgrp(),
 'term_handler':signal.getsignal(signal.SIGTERM)==term})+'\n').encode());s.close()
while True: signal.pause()
"#;

fn fixture_error(message: impl Into<String>) -> ConfctlError {
    ConfctlError::State(message.into())
}

#[derive(Clone, Default)]
struct Bytes(Arc<Mutex<Vec<u8>>>);
impl Write for Bytes {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(bytes);
        Ok(bytes.len())
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}
struct FailedWriter;
impl Write for FailedWriter {
    fn write(&mut self, _: &[u8]) -> io::Result<usize> {
        Err(io::Error::new(
            io::ErrorKind::BrokenPipe,
            "lifetime renderer",
        ))
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

struct Operation {
    ready: TcpListener,
    contender_port: u16,
    acknowledgement: bool,
    ack_polls: AtomicUsize,
    resumed: AtomicBool,
    dropped: AtomicBool,
    dropped_after_join: AtomicBool,
    child_pid: AtomicUsize,
}
struct Dropped<'a>(&'a Operation);
impl Drop for Dropped<'_> {
    fn drop(&mut self) {
        let pid = self.0.child_pid.load(Ordering::SeqCst);
        self.0.dropped_after_join.store(
            pid > 0 && !std::path::Path::new("/proc").join(pid.to_string()).exists(),
            Ordering::SeqCst,
        );
        self.0.dropped.store(true, Ordering::SeqCst);
    }
}

impl Operations for Operation {
    fn available(&self, _: &Action) -> bool {
        true
    }
    async fn run(
        &self,
        _: &Invocation,
        context: &Context,
    ) -> Result<ExitDisposition, ConfctlError> {
        let body = async {
            let root = ConfigRoot::new(&context.cwd)?;
            let _guard = MutationLock::acquire(&root, &context.cancellation).await?;
            let _dropped = Dropped(self);
            let scope = |id| ProcessScope::new(context.cancellation.clone(), TaskId(id));

            // One reply stays queued until operation drop, after the first
            // shutdown drain. The response case also makes shutdown return Err.
            let missing = || context.cwd.join("missing-native-fixture");
            let mut late = Box::pin(context.processes.run(
                ProcessSpec::new(missing(), "lifetime late reply"),
                scope(20),
            ));
            poll_fn(|cx| match late.as_mut().poll(cx) {
                Poll::Pending => Poll::Ready(Ok(())),
                Poll::Ready(_) => Poll::Ready(Err(fixture_error("late reply did not suspend"))),
            })
            .await?;
            let shutdown_failure = if !self.acknowledgement {
                let mut abandoned = Box::pin(context.processes.run(
                    ProcessSpec::new(missing(), "lifetime shutdown failure"),
                    scope(21),
                ));
                poll_fn(|cx| match abandoned.as_mut().poll(cx) {
                    Poll::Pending => Poll::Ready(Ok(())),
                    Poll::Ready(_) => {
                        Poll::Ready(Err(fixture_error("shutdown reply did not suspend")))
                    }
                })
                .await?;
                Some(abandoned)
            } else {
                None
            };

            let mut spec = ProcessSpec::new("python3", "lifetime owned child");
            spec.args = vec![
                "-c".into(),
                CHILD.into(),
                context.cwd.as_os_str().into(),
                self.ready.local_addr().unwrap().port().to_string().into(),
                self.contender_port.to_string().into(),
            ];
            let mut child = Box::pin(context.processes.run(spec, scope(22)));
            let (stream, _) = tokio::select! {
                result=tokio::time::timeout(Duration::from_secs(5),self.ready.accept()) =>
                    result.map_err(|_|fixture_error("child-ready deadline"))?
                        .map_err(|e|fixture_error(format!("child-ready accept: {e}")))?,
                result=&mut child => return Err(fixture_error(format!("child exited before ready: {result:?}"))),
            };
            let mut line = String::new();
            tokio::time::timeout(
                Duration::from_secs(5),
                BufReader::new(stream).read_line(&mut line),
            )
            .await
            .map_err(|_| fixture_error("child-ready frame deadline"))?
            .map_err(|e| fixture_error(format!("child-ready frame: {e}")))?;
            let ready: Value =
                serde_json::from_str(&line).map_err(|e| fixture_error(e.to_string()))?;
            let pid = ready["pid"]
                .as_u64()
                .ok_or_else(|| fixture_error("missing child PID"))?;
            if pid == 0 || ready["pgid"] != ready["pid"] || ready["term_handler"] != true {
                return Err(fixture_error(format!("child readiness: {ready}")));
            }
            self.child_pid.store(pid as usize, Ordering::SeqCst);
            fs::write(context.cwd.join("ready.json"), line)
                .map_err(|e| fixture_error(e.to_string()))?;
            // Retain this caller through startup so closing it does not race
            // its worker's initial cancellation check.
            drop(shutdown_failure);

            let mut ack = Box::pin(context.events.emit(UiEvent::Diagnostic {
                stream: OutputStream::Stdout,
                style: Style::Plain,
                text: "lifetime output\n".into(),
            }));
            poll_fn(|cx| {
                self.ack_polls.fetch_add(1, Ordering::SeqCst);
                match ack.as_mut().poll(cx) {
                    Poll::Pending => Poll::Ready(Ok(())),
                    Poll::Ready(result) => Poll::Ready(Err(fixture_error(format!(
                        "ack was not pending: {result:?}"
                    )))),
                }
            })
            .await?;
            fs::write(
                context.cwd.join("parked"),
                if self.acknowledgement {
                    "ack-pending"
                } else {
                    "process-response"
                },
            )
            .map_err(|e| fixture_error(e.to_string()))?;
            if self.acknowledgement {
                // Retain the REAL pending emit/ack future behind this private
                // gate; resuming it could let operation-first select complete.
                pending::<()>().await;
            } else {
                let _ = child.await;
            }
            self.resumed.store(true, Ordering::SeqCst);
            drop(ack);
            drop(late);
            Err(fixture_error("operation resumed after parking"))
        };
        tokio::pin!(body);
        poll_fn(|cx| {
            assert!(
                !context.cancellation.is_cancelled(),
                "abandoned operation polled during shutdown"
            );
            body.as_mut().poll(cx)
        })
        .await
    }
}

async fn probe(port: u16, pid: usize) -> Result<Value, String> {
    let mut stream = TcpStream::connect(("127.0.0.1", port))
        .await
        .map_err(|e| e.to_string())?;
    stream
        .write_all(format!("{}\n", json!({"phase":"after","pid":pid})).as_bytes())
        .await
        .map_err(|e| e.to_string())?;
    let mut line = String::new();
    tokio::time::timeout(
        Duration::from_secs(2),
        BufReader::new(stream).read_line(&mut line),
    )
    .await
    .map_err(|_| "post-join contender deadline".to_owned())?
    .map_err(|e| e.to_string())?;
    serde_json::from_str(&line).map_err(|e| e.to_string())
}

async fn lifetime_case(acknowledgement: bool) {
    let scratch = tempfile::tempdir().unwrap();
    let root = scratch.path();
    let ready = TcpListener::bind(("127.0.0.1", 0)).await.unwrap();
    let mut contender = Command::new("python3")
        .args(["-c", CONTENDER])
        .arg(root)
        .process_group(0)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .expect("independent contender");
    let contender_pid = contender.id().expect("contender PID");
    let mut lines = BufReader::new(contender.stdout.take().unwrap()).lines();
    let first = tokio::time::timeout(Duration::from_secs(5), lines.next_line()).await;
    if !matches!(&first, Ok(Ok(Some(_)))) {
        contender.kill().await.unwrap();
        let output = contender.wait_with_output().await.unwrap();
        panic!("contender ready precondition: {first:?}; {output:?}");
    }
    let first: Value = serde_json::from_str(&first.unwrap().unwrap().unwrap()).unwrap();
    let operation = Operation {
        ready,
        contender_port: first["port"].as_u64().unwrap().try_into().unwrap(),
        acknowledgement,
        ack_polls: AtomicUsize::new(0),
        resumed: AtomicBool::new(false),
        dropped: AtomicBool::new(false),
        dropped_after_join: AtomicBool::new(false),
        child_pid: AtomicUsize::new(0),
    };
    let stderr = Bytes::default();
    let code = execute(
        vec!["--color=never".into(), "build".into()],
        Timing::start(),
        &operation,
        Ok(root.into()),
        Arc::new(Mutex::new(Box::new(FailedWriter))),
        Arc::new(Mutex::new(Box::new(stderr.clone()))),
    )
    .await;
    let pid = operation.child_pid.load(Ordering::SeqCst);
    let after = probe(operation.contender_port, pid).await;
    let mut raw_receipts = Vec::new();
    loop {
        match tokio::time::timeout(Duration::from_secs(3), lines.next_line()).await {
            Ok(Ok(Some(line))) => raw_receipts.push(line),
            Ok(Ok(None)) => break,
            failure => {
                contender.kill().await.unwrap();
                let output = contender.wait_with_output().await.unwrap();
                panic!(
                    "contender finish precondition: {failure:?}; {raw_receipts:?}; {after:?}; {output:?}"
                );
            }
        }
    }
    let output = contender.wait_with_output().await.unwrap();
    let after = after.expect("post-join contender receipt");
    let receipts = raw_receipts
        .iter()
        .map(|s| serde_json::from_str::<Value>(s).unwrap())
        .collect::<Vec<_>>();
    let term: Value = serde_json::from_slice(
        &fs::read(root.join("term.json")).expect("TERM-phase precondition receipt"),
    )
    .unwrap();
    eprintln!(
        "ack={acknowledgement} app={code} contender={:?} ready={first} receipts={receipts:?} term={term} after={after}",
        output.status
    );
    assert!(output.status.success(), "{output:?}");
    assert_eq!(code, 1);
    assert_eq!(first["phase"], "contender-ready");
    assert_eq!(first["pid"], contender_pid);
    assert_eq!(first["pid"], first["pgid"]);
    assert_ne!(first["pid"].as_u64().unwrap(), pid as u64);
    assert!(first["pid"].as_u64().unwrap() > 0 && pid > 0);
    assert_eq!(term["phase"], "term");
    assert_eq!(term["child_pid"], pid);
    assert_eq!(term["child_pgid"], pid);
    assert_eq!(term["pid"], first["pid"]);
    assert_eq!(term["signal"], 15);
    assert_eq!(term["alive_before"], true);
    assert_eq!(term["alive_after"], true);
    assert_eq!(
        term["acquired"], false,
        "guard released during live TERM phase: {term}"
    );
    assert_eq!(term["errno"], rustix::io::Errno::AGAIN.raw_os_error());
    assert_eq!(after["acquired"], true);
    assert_eq!(after["alive_before"], false);
    assert_eq!(after["alive_after"], false);
    assert_eq!(after["device"], term["device"]);
    assert_eq!(after["inode"], term["inode"]);
    assert!(operation.dropped.load(Ordering::SeqCst));
    assert!(operation.dropped_after_join.load(Ordering::SeqCst));
    assert!(!operation.resumed.load(Ordering::SeqCst));
    assert_eq!(operation.ack_polls.load(Ordering::SeqCst), 1);
    assert_eq!(
        fs::read_to_string(root.join("parked")).unwrap(),
        if acknowledgement {
            "ack-pending"
        } else {
            "process-response"
        }
    );
    let stderr = String::from_utf8(stderr.0.lock().unwrap().clone()).unwrap();
    assert!(
        stderr.contains("error: replay deferred stdout: lifetime renderer"),
        "{stderr}"
    );
    assert_eq!(
        stderr
            .matches("cleanup: unable to start lifetime late reply")
            .count(),
        1,
        "{stderr}"
    );
    assert_eq!(
        stderr
            .matches("cleanup: unable to start lifetime shutdown failure")
            .count(),
        usize::from(!acknowledgement),
        "{stderr}"
    );
}

#[tokio::test]
async fn presentation_failure_retains_mutation_guard_while_ack_is_parked() {
    lifetime_case(true).await;
}

#[tokio::test]
async fn presentation_failure_retains_mutation_guard_through_failed_shutdown_and_reply_drop() {
    lifetime_case(false).await;
}
