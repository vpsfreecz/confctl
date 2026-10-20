//! Joined local process ownership, byte captures and reliable stream forwarding.
//!
//! Each pipe drains to disk independently of the acknowledged UI relay. A slow
//! renderer can delay completion, but cannot fill both child pipes and prevent
//! the child from exiting. Capture/log failures cancel the owned process group.
//! Ordinary nonzero exits remain outcomes; callers decide their command policy.

use crate::{
    error::ConfctlError,
    events::{EventSink, OutputStream, TaskId, UiEvent},
    terminal::{ForegroundLease, foreground::ForegroundAttachment},
    timing::{Phase, Timing},
};
use std::{
    ffi::OsString,
    fmt,
    future::Future,
    io,
    os::fd::{AsFd, OwnedFd},
    path::{Path, PathBuf},
    process::{ExitStatus, Stdio},
    sync::{
        Arc, Mutex as StdMutex,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};
use tempfile::NamedTempFile;
use tokio::{
    fs::File,
    io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt},
    process::Command,
    sync::{Mutex, mpsc, oneshot, watch},
    task::{JoinHandle, JoinSet},
};
use tokio_util::sync::CancellationToken;

const CHUNK: usize = 16 * 1024;

#[derive(Debug)]
pub enum StdinPolicy {
    Null,
    Bytes(Vec<u8>),
    File(PathBuf),
    /// Report chunks acknowledged only after the pager pipe write completes.
    Pager(PagerInput),
    Foreground,
    /// Reserved for the audited no-read GNU logname probe, never arbitrary
    /// interactive inheritance. The resolver owns its pinned executable path.
    LoginProbe(LoginProbeStdin),
}

/// The result describes pipe acceptance, not whether a human saw the bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WrittenOrClosed {
    Written,
    Closed,
}

#[derive(Debug)]
struct PagerChunk {
    bytes: Vec<u8>,
    response: oneshot::Sender<io::Result<WrittenOrClosed>>,
}

#[derive(Debug)]
pub struct PagerInput(mpsc::Receiver<PagerChunk>);

pub struct PagerWriter(mpsc::Sender<PagerChunk>);

impl PagerInput {
    pub fn channel() -> (PagerWriter, Self) {
        let (sender, receive) = mpsc::channel(1);
        (PagerWriter(sender), Self(receive))
    }
}

impl PagerWriter {
    pub async fn write(
        &mut self,
        bytes: &[u8],
        cancel: &CancellationToken,
    ) -> Result<WrittenOrClosed, ConfctlError> {
        for bytes in bytes.chunks(CHUNK) {
            let (response, receive) = oneshot::channel();
            let chunk = PagerChunk {
                bytes: bytes.to_vec(),
                response,
            };
            tokio::select! {
                result = self.0.send(chunk) => result.map_err(|_| ConfctlError::EventDelivery("pager input owner closed without a pipe-write result".into()))?,
                _ = cancel.cancelled() => return Err(ConfctlError::Cancelled),
            }
            let result = tokio::select! {
                result = receive => result.map_err(|_| ConfctlError::EventDelivery("pager pipe write was not acknowledged".into()))?,
                _ = cancel.cancelled() => return Err(ConfctlError::Cancelled),
            }.map_err(|source| io_error("write pager stdin", None, source))?;
            if cancel.is_cancelled() {
                return Err(ConfctlError::Cancelled);
            }
            if result == WrittenOrClosed::Closed {
                return Ok(result);
            }
        }
        if cancel.is_cancelled() {
            return Err(ConfctlError::Cancelled);
        }
        Ok(WrittenOrClosed::Written)
    }
}

async fn pager_input(
    stdin: &mut (impl AsyncWrite + Unpin),
    mut input: PagerInput,
) -> io::Result<()> {
    while let Some(chunk) = input.0.recv().await {
        match stdin.write_all(&chunk.bytes).await {
            Ok(()) => {
                let _ = chunk.response.send(Ok(WrittenOrClosed::Written));
            }
            Err(error) if error.raw_os_error() == Some(::nix::libc::EPIPE) => {
                let _ = chunk.response.send(Ok(WrittenOrClosed::Closed));
                return Ok(());
            }
            Err(error) => {
                let copy = error
                    .raw_os_error()
                    .map(io::Error::from_raw_os_error)
                    .unwrap_or_else(|| io::Error::new(error.kind(), error.to_string()));
                let _ = chunk.response.send(Err(copy));
                return Err(error);
            }
        }
    }
    stdin.shutdown().await
}

/// A duplicate of the original fd0 open-file description, not a reopened
/// /proc path or an input-broker read. Only the qualified login resolver uses
/// this private implementation seam with no args and capture-only outputs.
#[derive(Debug)]
pub struct LoginProbeStdin(OwnedFd);

impl LoginProbeStdin {
    pub fn original() -> io::Result<Self> {
        std::io::stdin().as_fd().try_clone_to_owned().map(Self)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StreamPolicy {
    Capture,
    Forward,
    CaptureAndForward,
    Foreground,
    /// Stderr only: share stdout's native pipe and its capture/forward policy.
    /// Stdout must be piped. There is no independent stderr capture or EOF:
    /// outcomes/evidence use stdout, with stderr Unavailable and incomplete.
    MergeToStdout,
}

impl StreamPolicy {
    fn forwards(self) -> bool {
        matches!(self, Self::Forward | Self::CaptureAndForward)
    }
    fn captures(self) -> bool {
        matches!(self, Self::Capture | Self::CaptureAndForward)
    }
}

#[derive(Debug)]
pub struct ProcessSpec {
    pub program: PathBuf,
    pub args: Vec<OsString>,
    pub cwd: Option<PathBuf>,
    pub env: Vec<(OsString, Option<OsString>)>,
    pub clear_env: bool,
    pub stdin: StdinPolicy,
    pub stdout: StreamPolicy,
    pub stderr: StreamPolicy,
    pub redacted_description: String,
    // Native fixtures may wait for an actual queued kernel stop before attach.
    // This observes with NOWAIT in the sole worker; it skips no route policy.
    #[cfg(test)]
    before_attach_stop: Option<rustix::process::Signal>,
    #[cfg(test)]
    before_attach_exit: bool,
}

impl ProcessSpec {
    pub fn new(program: impl Into<PathBuf>, redacted_description: impl Into<String>) -> Self {
        Self {
            program: program.into(),
            args: Vec::new(),
            cwd: None,
            env: Vec::new(),
            clear_env: false,
            stdin: StdinPolicy::Null,
            stdout: StreamPolicy::Capture,
            stderr: StreamPolicy::Capture,
            redacted_description: redacted_description.into(),
            #[cfg(test)]
            before_attach_stop: None,
            #[cfg(test)]
            before_attach_exit: false,
        }
    }
}

pub struct ProcessScope {
    pub cancellation: CancellationToken,
    pub task: TaskId,
    pub phase: Option<Phase>,
    pub foreground: Option<ForegroundLease>,
    pub(crate) nix_progress: Option<crate::nix::progress::Kind>,
}

impl ProcessScope {
    pub fn new(cancellation: CancellationToken, task: TaskId) -> Self {
        Self {
            cancellation,
            task,
            phase: None,
            foreground: None,
            nix_progress: None,
        }
    }
}

pub enum Captured {
    /// The stream was inherited, or forwarding succeeded without capture.
    Unavailable,
    Memory(Vec<u8>),
    Spool {
        file: NamedTempFile,
        length: u64,
    },
}

impl fmt::Debug for Captured {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Unavailable => formatter.write_str("Unavailable"),
            Self::Memory(bytes) => formatter
                .debug_struct("Memory")
                .field("length", &bytes.len())
                .finish(),
            Self::Spool { file, length } => formatter
                .debug_struct("Spool")
                .field("path", &file.path())
                .field("length", length)
                .finish(),
        }
    }
}

/// Owned evidence survives infrastructure errors until the caller retains it
/// or verifies a durable raw log. Dropping it releases temporary spool files.
/// Completeness means EOF and a successful spool flush, not log/UI delivery.
#[derive(Debug)]
pub struct PartialProcessEvidence {
    pub status: Option<ExitStatus>,
    pub stdout: Captured,
    pub stderr: Captured,
    pub stdout_complete: bool,
    pub stderr_complete: bool,
    pub log: Option<PathBuf>,
}

impl Captured {
    pub fn len(&self) -> u64 {
        match self {
            Self::Unavailable => 0,
            Self::Memory(bytes) => bytes.len() as u64,
            Self::Spool { length, .. } => *length,
        }
    }
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
    pub fn memory(&self) -> Option<&[u8]> {
        if let Self::Memory(bytes) = self {
            Some(bytes)
        } else {
            None
        }
    }
    pub fn spool_path(&self) -> Option<&Path> {
        if let Self::Spool { file, .. } = self {
            Some(file.path())
        } else {
            None
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TerminationCause {
    Exited,
    Canceled,
}

#[derive(Debug)]
pub struct ProcessOutcome {
    pub status: ExitStatus,
    pub stdout: Captured,
    pub stderr: Captured,
    /// EOF observed and capture finalized; cancellation may leave a prefix.
    pub stdout_complete: bool,
    pub stderr_complete: bool,
    pub started: Instant,
    pub ended: Instant,
    pub termination: TerminationCause,
}

impl ProcessOutcome {
    pub fn code(&self) -> Option<i32> {
        self.status.code()
    }
    #[cfg(unix)]
    pub fn signal(&self) -> Option<i32> {
        use std::os::unix::process::ExitStatusExt;
        self.status.signal()
    }
}

pub trait ProcessRunner: Send + Sync {
    fn run(
        &self,
        spec: ProcessSpec,
        scope: ProcessScope,
    ) -> impl Future<Output = Result<ProcessOutcome, ConfctlError>> + Send;
}

#[derive(Clone)]
pub struct ProcessLog {
    writer: Arc<Mutex<Box<dyn AsyncWrite + Send + Unpin>>>,
    locator: Option<PathBuf>,
}

impl ProcessLog {
    pub fn new(writer: impl AsyncWrite + Send + Unpin + 'static) -> Self {
        Self {
            writer: Arc::new(Mutex::new(Box::new(writer))),
            locator: None,
        }
    }
    /// A known destination, not a promise that a failed write was durable.
    pub fn with_locator(mut self, locator: PathBuf) -> Self {
        self.locator = Some(locator);
        self
    }
    pub fn locator(&self) -> Option<&Path> {
        self.locator.as_deref()
    }
    async fn write(&self, bytes: &[u8]) -> io::Result<()> {
        let mut writer = self.writer.lock().await;
        writer.write_all(bytes).await?;
        writer.flush().await
    }
}

struct Owner {
    events: EventSink,
    log: Option<ProcessLog>,
    spool_dir: PathBuf,
    memory_limit: usize,
    grace: Duration,
    timing: Option<Timing>,
    cancellation: CancellationToken,
    closing: AtomicBool,
    workers: Mutex<JoinSet<()>>,
    unobserved: StdMutex<Vec<ConfctlError>>,
}

#[derive(Clone)]
pub struct Supervisor {
    owner: Arc<Owner>,
}

/// Closing first makes queued and future replies exclusive ownership paths.
/// A queued error transfers synchronously on Drop; a later send fails and its
/// worker retains the same error. Successful consumption disarms the guard.
struct ProcessResponse {
    receive: oneshot::Receiver<Result<ProcessOutcome, ConfctlError>>,
    owner: Arc<Owner>,
    armed: bool,
}

impl ProcessResponse {
    async fn receive(mut self) -> Result<ProcessOutcome, ConfctlError> {
        let result = (&mut self.receive).await.map_err(|_| {
            ConfctlError::EventDelivery("process owner terminated without a result".into())
        });
        self.armed = false;
        result?
    }
}

impl Drop for ProcessResponse {
    fn drop(&mut self) {
        if self.armed {
            self.receive.close();
            if let Ok(Err(error)) = self.receive.try_recv() {
                self.owner
                    .unobserved
                    .lock()
                    .unwrap_or_else(|error| error.into_inner())
                    .push(error);
            }
        }
    }
}

fn deliver_result(
    owner: &Owner,
    response: oneshot::Sender<Result<ProcessOutcome, ConfctlError>>,
    result: Result<ProcessOutcome, ConfctlError>,
) {
    if let Err(Err(error)) = response.send(result) {
        owner
            .unobserved
            .lock()
            .unwrap_or_else(|error| error.into_inner())
            .push(error);
    }
}

impl Supervisor {
    pub fn new(
        events: EventSink,
        spool_dir: PathBuf,
        memory_limit: usize,
        grace: Duration,
        log: Option<ProcessLog>,
        timing: Option<Timing>,
    ) -> Self {
        Self {
            owner: Arc::new(Owner {
                events,
                log,
                spool_dir,
                memory_limit,
                grace,
                timing,
                cancellation: CancellationToken::new(),
                closing: AtomicBool::new(false),
                workers: Mutex::new(JoinSet::new()),
                unobserved: StdMutex::new(Vec::new()),
            }),
        }
    }

    /// Stops admission, cancels owned work, then joins workers and every pipe.
    /// Join/drop operation futures first, then drain retained replies here.
    /// Do not select/drop this cleanup future as a response to cancellation.
    pub async fn shutdown(&self) -> Result<(), ConfctlError> {
        self.owner.closing.store(true, Ordering::SeqCst);
        self.owner.cancellation.cancel();
        let mut workers = self.owner.workers.lock().await;
        let mut failures = Vec::new();
        while let Some(result) = workers.join_next().await {
            if let Err(error) = result {
                failures.push(join_error(error));
            }
        }
        failures.append(
            &mut *self
                .owner
                .unobserved
                .lock()
                .unwrap_or_else(|error| error.into_inner()),
        );
        if failures.is_empty() {
            Ok(())
        } else {
            let first = failures.remove(0);
            Err(first.with_secondary(failures))
        }
    }
}

impl ProcessRunner for Supervisor {
    async fn run(
        &self,
        spec: ProcessSpec,
        scope: ProcessScope,
    ) -> Result<ProcessOutcome, ConfctlError> {
        let mut workers = self.owner.workers.lock().await;
        if self.owner.closing.load(Ordering::SeqCst) {
            return Err(ConfctlError::Cancelled);
        }
        while let Some(result) = workers.try_join_next() {
            result.map_err(join_error)?;
        }
        let (response, receive) = oneshot::channel();
        let owner = self.owner.clone();
        workers.spawn(async move {
            owned_run(owner, spec, scope, response).await;
        });
        drop(workers);
        ProcessResponse {
            receive,
            owner: self.owner.clone(),
            armed: true,
        }
        .receive()
        .await
    }
}

async fn owned_run(
    owner: Arc<Owner>,
    spec: ProcessSpec,
    scope: ProcessScope,
    mut response: oneshot::Sender<Result<ProcessOutcome, ConfctlError>>,
) {
    let local = CancellationToken::new();
    let root = owner.cancellation.clone();
    let parent = scope.cancellation.clone();
    // Receiver closure also cancels: dropping a run future cannot detach work.
    let execution = execute(&owner, spec, scope, local.clone());
    tokio::pin!(execution);
    let result = tokio::select! {
        result = &mut execution => result,
        _ = root.cancelled() => { local.cancel(); execution.await },
        _ = parent.cancelled() => { local.cancel(); execution.await },
        _ = response.closed() => { local.cancel(); execution.await },
    };
    deliver_result(&owner, response, result);
}

fn io_error(operation: &'static str, path: Option<PathBuf>, source: io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path,
        source,
    }
}
fn join_error(error: tokio::task::JoinError) -> ConfctlError {
    io_error("join process worker", None, io::Error::other(error))
}

// The unpolled Tokio Child exclusively owns this leader until final reap.
// NOWAIT leaves its PID/PGID reserved even after it exits. Every numeric signal
// rechecks that ownership; ECHILD or any uncertain probe disables future signals.
#[cfg(unix)]
struct Group {
    pid: rustix::process::Pid,
    active: bool,
    #[cfg(test)]
    transitions: Vec<&'static str>,
}

#[cfg(unix)]
impl Group {
    fn new(pid: rustix::process::Pid) -> Self {
        Self {
            pid,
            active: true,
            #[cfg(test)]
            transitions: Vec::new(),
        }
    }
    fn probe(&mut self) -> io::Result<Option<ExitStatus>> {
        use rustix::process::{WaitId, WaitIdOptions, waitid};
        use std::os::unix::process::ExitStatusExt;
        loop {
            match waitid(
                WaitId::Pid(self.pid),
                WaitIdOptions::EXITED | WaitIdOptions::NOWAIT | WaitIdOptions::NOHANG,
            ) {
                Ok(status) => {
                    return Ok(status.map(|status| {
                        let raw = if let Some(code) = status.exit_status() {
                            code << 8
                        } else {
                            status.terminating_signal().unwrap_or(0)
                                | if status.dumped() { 0x80 } else { 0 }
                        };
                        ExitStatus::from_raw(raw)
                    }));
                }
                Err(rustix::io::Errno::INTR) => {}
                Err(error) => {
                    self.disarm();
                    return Err(error.into());
                }
            }
        }
    }
    fn disarm(&mut self) {
        self.active = false;
        #[cfg(test)]
        self.transitions.push("disarm");
    }
    /// Stop/continue notification consumption excludes EXITED and never reaps
    /// the leader. The same worker owns this and the WNOWAIT exit observation.
    fn transition(&mut self) -> io::Result<Option<ChildTransition>> {
        use rustix::process::{WaitId, WaitIdOptions, waitid};
        loop {
            match waitid(
                WaitId::Pid(self.pid),
                WaitIdOptions::STOPPED | WaitIdOptions::CONTINUED | WaitIdOptions::NOHANG,
            ) {
                Ok(Some(status)) if status.stopped() => {
                    return Ok(Some(ChildTransition::Stopped(
                        status.stopping_signal().expect("stopped signal"),
                    )));
                }
                Ok(Some(_)) => return Ok(Some(ChildTransition::Continued)),
                Ok(None) => return Ok(None),
                Err(rustix::io::Errno::INTR) => {}
                Err(rustix::io::Errno::CHILD) if self.active => {
                    // Notification eligibility excludes exited children. Linux
                    // may report ECHILD for our unreaped zombie; only the exit
                    // ownership probe can confirm that its identity is retained.
                    if self.probe()?.is_some() {
                        return Ok(None);
                    }
                    self.disarm();
                    return Err(rustix::io::Errno::CHILD.into());
                }
                Err(error) => {
                    self.disarm();
                    return Err(error.into());
                }
            }
        }
    }
    fn signal(&mut self, signal: rustix::process::Signal) -> io::Result<()> {
        self.signal_using(signal, rustix::process::kill_process_group)
    }
    fn signal_using(
        &mut self,
        signal: rustix::process::Signal,
        send: impl FnOnce(rustix::process::Pid, rustix::process::Signal) -> rustix::io::Result<()>,
    ) -> io::Result<()> {
        if !self.active {
            return Ok(());
        }
        self.probe()?;
        #[cfg(test)]
        self.transitions.push("numeric-signal");
        match send(self.pid, signal) {
            Ok(()) | Err(rustix::io::Errno::SRCH) => Ok(()),
            Err(error) => Err(error.into()),
        }
    }
}

enum ChildTransition {
    Stopped(i32),
    Continued,
}

async fn final_wait(
    child: &mut tokio::process::Child,
    group: &mut Group,
) -> io::Result<ExitStatus> {
    group.disarm();
    #[cfg(test)]
    group.transitions.push("wait");
    child.wait().await
}

#[cfg(unix)]
impl Drop for Group {
    fn drop(&mut self) {
        // This guard precedes Child drop and is disarmed before explicit reap.
        let _ = self.signal(rustix::process::Signal::KILL);
    }
}

enum WaitReason {
    Exited(io::Result<ExitStatus>),
    Canceled,
    Failed(ConfctlError),
}

#[cfg(unix)]
async fn observe_exit(group: &mut Group) -> io::Result<ExitStatus> {
    loop {
        if let Some(status) = group.probe()? {
            return Ok(status);
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

/// Exactly one nonblocking observation before the first yield. Only an already
/// reportable initial TTIN/TTOU gets one automatic continuation; a later startup
/// stop is indistinguishable from a deliberate stop and follows app suspension.
fn initial_attachment_transition(
    group: &mut Group,
    attachment: &ForegroundAttachment,
) -> Result<Option<i32>, ConfctlError> {
    let transition = group
        .transition()
        .map_err(|source| io_error("observe initial child stop", None, source))?;
    match transition {
        Some(ChildTransition::Stopped(signal))
            if signal == rustix::process::Signal::TTIN.as_raw()
                || signal == rustix::process::Signal::TTOU.as_raw() =>
        {
            group
                .probe()
                .map_err(|source| io_error("verify initial child ownership", None, source))?;
            if !attachment.owns_terminal()? {
                return Err(ConfctlError::TerminalBusy);
            }
            group
                .signal(rustix::process::Signal::CONT)
                .map_err(|source| io_error("resume initial terminal I/O stop", None, source))?;
            Ok(None)
        }
        Some(ChildTransition::Stopped(signal)) => Ok(Some(signal)),
        _ => Ok(None),
    }
}

async fn observe_foreground_exit(
    group: &mut Group,
    attachment: &mut ForegroundAttachment,
    mut pending_stop: Option<i32>,
    cancel: &CancellationToken,
) -> Result<ExitStatus, ConfctlError> {
    loop {
        if let Some(status) = group
            .probe()
            .map_err(|source| io_error("observe child exit", None, source))?
        {
            return Ok(status);
        }
        let stopped = if let Some(signal) = pending_stop.take() {
            Some(signal)
        } else {
            match group
                .transition()
                .map_err(|source| io_error("observe child stop/continue", None, source))?
            {
                Some(ChildTransition::Stopped(signal)) => Some(signal),
                _ => None,
            }
        };
        if stopped.is_some() {
            attachment.request_suspend(cancel).await?;
            // The app grants only its own fresh foreground ownership. This
            // worker separately owns leader/PGID reservation and child resume.
            if cancel.is_cancelled() {
                return Err(ConfctlError::Cancelled);
            }
            if let Some(status) = group
                .probe()
                .map_err(|source| io_error("verify resumed child ownership", None, source))?
            {
                return Ok(status);
            }
            attachment.reattach()?;
            group
                .signal(rustix::process::Signal::CONT)
                .map_err(|source| io_error("resume foreground child group", None, source))?;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

struct RunState {
    group: Group,
    stopped: bool,
    termination: TerminationCause,
    errors: Vec<ConfctlError>,
}

impl RunState {
    async fn stop(&mut self, cancel: &CancellationToken, grace: Duration) {
        if self.stopped {
            return;
        }
        self.stopped = true;
        cancel.cancel();
        if let Err(error) = self.group.signal(rustix::process::Signal::TERM) {
            self.errors
                .push(io_error("terminate process group", None, error));
        }
        // Leader exit does not establish descendant exit. On a failure/cancel
        // only, grant the complete group its grace, then stop remaining members.
        tokio::time::sleep(grace).await;
        if let Err(error) = self.group.signal(rustix::process::Signal::KILL) {
            self.errors
                .push(io_error("kill process group", None, error));
        }
    }
}

async fn execute(
    owner: &Owner,
    mut spec: ProcessSpec,
    mut scope: ProcessScope,
    cancel: CancellationToken,
) -> Result<ProcessOutcome, ConfctlError> {
    if cancel.is_cancelled()
        || scope.cancellation.is_cancelled()
        || owner.cancellation.is_cancelled()
    {
        return Err(ConfctlError::Cancelled);
    }
    let merge_stderr = spec.stderr == StreamPolicy::MergeToStdout;
    if spec.stdout == StreamPolicy::MergeToStdout
        || (merge_stderr && spec.stdout == StreamPolicy::Foreground)
    {
        return Err(ConfctlError::Configuration(
            "stderr merging requires piped stdout and is valid only for stderr".into(),
        ));
    }
    let foreground = matches!(spec.stdin, StdinPolicy::Foreground)
        || spec.stdout == StreamPolicy::Foreground
        || spec.stderr == StreamPolicy::Foreground;
    if foreground {
        let lease = scope
            .foreground
            .as_ref()
            .ok_or(ConfctlError::TerminalBusy)?;
        lease.validate_process_handoff()?;
    }
    // Allocate every fallible stream resource before a child exists. A second
    // stream setup failure must never detach the first stream's drain task.
    let stdout_stream = if spec.stdout == StreamPolicy::Foreground {
        None
    } else {
        Some(prepare_stream(owner, spec.stdout)?)
    };
    let stderr_stream = if merge_stderr || spec.stderr == StreamPolicy::Foreground {
        None
    } else {
        Some(prepare_stream(owner, spec.stderr)?)
    };
    let _phase = scope
        .phase
        .and_then(|phase| owner.timing.as_ref().map(|timing| timing.phase(phase)));
    let started = Instant::now();
    let mut command = Command::new(&spec.program);
    // Group owns the kill fallback and verifies unreaped-child ownership. A
    // Tokio numeric-PID kill-on-drop could signal after an ECHILD wait error.
    command.args(&spec.args).kill_on_drop(false);
    if let Some(cwd) = &spec.cwd {
        command.current_dir(cwd);
    }
    if spec.clear_env {
        command.env_clear();
    }
    for (key, value) in &spec.env {
        if let Some(value) = value {
            command.env(key, value);
        } else {
            command.env_remove(key);
        }
    }
    let stdin_work = match std::mem::replace(&mut spec.stdin, StdinPolicy::Null) {
        StdinPolicy::Null => {
            command.stdin(Stdio::null());
            StdinPolicy::Null
        }
        StdinPolicy::Foreground => {
            command.stdin(Stdio::inherit());
            StdinPolicy::Null
        }
        StdinPolicy::LoginProbe(probe) => {
            command.stdin(Stdio::from(probe.0));
            StdinPolicy::Null
        }
        policy => {
            command.stdin(Stdio::piped());
            policy
        }
    };
    let merged_stdout = if merge_stderr {
        let (read, write) = rustix::pipe::pipe_with(rustix::pipe::PipeFlags::CLOEXEC)
            .map_err(|error| io_error("create merged output pipe", None, error.into()))?;
        let other_write = write
            .try_clone()
            .map_err(|error| io_error("duplicate merged output writer", None, error))?;
        let reader = tokio::process::ChildStdout::from_std(std::process::ChildStdout::from(read))
            .map_err(|error| io_error("prepare merged output reader", None, error))?;
        command.stdout(Stdio::from(write));
        command.stderr(Stdio::from(other_write));
        Some(reader)
    } else {
        command.stdout(if spec.stdout == StreamPolicy::Foreground {
            Stdio::inherit()
        } else {
            Stdio::piped()
        });
        command.stderr(if spec.stderr == StreamPolicy::Foreground {
            Stdio::inherit()
        } else {
            Stdio::piped()
        });
        None
    };
    #[cfg(unix)]
    command.process_group(0);
    // Installing SIGCHLD ownership before spawn also replaces inherited IGN,
    // which otherwise auto-reaps leaders and destroys the WNOWAIT reservation.
    // Keep Child unpolled even when Tokio uses its pidfd implementation.
    let _child_signal = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::child())
        .map_err(|error| io_error("own child notifications", None, error))?;
    let spawned = command.spawn();
    // Command retains supplied Stdio handles for reuse. Close the parent's
    // merged-pipe writers now so the existing drain can observe real EOF.
    drop(command);
    let mut child = spawned.map_err(|source| ConfctlError::Spawn {
        description: spec.redacted_description.clone(),
        source,
    })?;
    #[cfg(unix)]
    let mut group = {
        let pid = child
            .id()
            .and_then(|id| i32::try_from(id).ok())
            .and_then(rustix::process::Pid::from_raw);
        let Some(pid) = pid else {
            let _ = child.start_kill();
            let _ = child.wait().await;
            return Err(io_error(
                "identify owned process group",
                None,
                io::Error::other("missing child PID"),
            ));
        };
        Group::new(pid)
    };
    let mut attachment = None;
    let mut initial_stop = None;
    let mut setup_errors = Vec::new();
    if foreground {
        let setup = (|| {
            #[cfg(test)]
            if let Some(signal) = spec.before_attach_stop {
                foreground_tests::wait_before_attachment(group.pid, signal)?;
            }
            #[cfg(test)]
            if spec.before_attach_exit {
                foreground_tests::wait_for_exit_before_attachment(&mut group)?;
            }
            group
                .probe()
                .map_err(|source| io_error("verify foreground child ownership", None, source))?;
            let attached = scope
                .foreground
                .as_mut()
                .expect("validated foreground lease")
                .attach(group.pid)?;
            if attached.controlling_tty() {
                initial_stop = initial_attachment_transition(&mut group, &attached)?;
            }
            attachment = Some(attached);
            Ok::<(), ConfctlError>(())
        })();
        if let Err(error) = setup {
            setup_errors.push(error);
        }
    }
    let (errors, mut failures) = mpsc::unbounded_channel();
    let mut pumps = Vec::new();
    let mut relays = Vec::new();
    if let (Some(stdout), Some(prepared)) =
        (merged_stdout.or_else(|| child.stdout.take()), stdout_stream)
    {
        let (pump, relay) = start_stream(
            owner,
            prepared,
            stdout,
            (OutputStream::Stdout, scope.task),
            None,
            cancel.clone(),
            errors.clone(),
        );
        pumps.push((OutputStream::Stdout, pump));
        if let Some(relay) = relay {
            relays.push(relay);
        }
    }
    if let (Some(stderr), Some(prepared)) = (child.stderr.take(), stderr_stream) {
        let (pump, relay) = start_stream(
            owner,
            prepared,
            stderr,
            (OutputStream::Stderr, scope.task),
            scope.nix_progress,
            cancel.clone(),
            errors.clone(),
        );
        pumps.push((OutputStream::Stderr, pump));
        if let Some(relay) = relay {
            relays.push(relay);
        }
    }
    let stdin = child.stdin.take();
    let stdin_cancel = cancel.clone();
    let stdin_errors = errors.clone();
    let input = tokio::spawn(async move {
        let Some(mut stdin) = stdin else {
            return;
        };
        let write = async {
            match stdin_work {
                StdinPolicy::Bytes(bytes) => stdin.write_all(&bytes).await?,
                StdinPolicy::File(path) => {
                    let mut source = File::open(path).await?;
                    tokio::io::copy(&mut source, &mut stdin).await?;
                }
                StdinPolicy::Pager(input) => return pager_input(&mut stdin, input).await,
                _ => {}
            }
            stdin.shutdown().await
        };
        tokio::select! {
            result = write => if let Err(error) = result { let _ = stdin_errors.send(io_error("write child stdin", None, error)); },
            _ = stdin_cancel.cancelled() => {},
        }
    });
    drop(errors);
    let reason = if let Some(primary) = setup_errors.pop() {
        WaitReason::Failed(primary)
    } else {
        let observe = async {
            if let Some(attached) = attachment
                .as_mut()
                .filter(|attached| attached.controlling_tty())
            {
                observe_foreground_exit(&mut group, attached, initial_stop, &cancel).await
            } else {
                observe_exit(&mut group)
                    .await
                    .map_err(|source| io_error("observe child exit", None, source))
            }
        };
        tokio::select! {
            status = observe => match status { Ok(status) => WaitReason::Exited(Ok(status)), Err(ConfctlError::Cancelled) => WaitReason::Canceled, Err(error) => WaitReason::Failed(error) },
            _ = cancel.cancelled() => WaitReason::Canceled,
            Some(error) = failures.recv() => WaitReason::Failed(error),
        }
    };
    let child_finished_first = matches!(reason, WaitReason::Exited(Ok(_)));
    let mut observed = None;
    let mut state = RunState {
        group,
        stopped: false,
        termination: TerminationCause::Exited,
        errors: Vec::new(),
    };
    match reason {
        WaitReason::Exited(Ok(status)) => observed = Some(status),
        WaitReason::Exited(Err(error)) => {
            state
                .errors
                .push(io_error("observe child exit", None, error));
            state.stop(&cancel, owner.grace).await;
        }
        WaitReason::Canceled => {
            state.termination = TerminationCause::Canceled;
            state.stop(&cancel, owner.grace).await;
        }
        WaitReason::Failed(error) => {
            state.errors.push(error);
            state.stop(&cancel, owner.grace).await;
        }
    }
    // An ordinary exited leader may have legitimate tail-writing descendants.
    // Leave it unreaped through every input/drain/relay join and observe failures
    // and cancellation during those joins. No Child polling occurs here.
    if let Err(error) = join_noticing(input, &mut failures, &cancel, &mut state, owner.grace).await
    {
        state.errors.push(join_error(error));
        state.stop(&cancel, owner.grace).await;
    }
    let mut stdout = Captured::Unavailable;
    let mut stderr = Captured::Unavailable;
    let mut stdout_complete = false;
    let mut stderr_complete = false;
    let mut joined = Vec::new();
    for (stream, pump) in pumps {
        let complete =
            match join_noticing(pump.handle, &mut failures, &cancel, &mut state, owner.grace).await
            {
                Ok(complete) => complete,
                Err(error) => {
                    state.errors.push(join_error(error));
                    state.stop(&cancel, owner.grace).await;
                    false
                }
            };
        joined.push((stream, pump.spool, complete));
    }
    for relay in relays {
        if let Err(error) =
            join_noticing(relay, &mut failures, &cancel, &mut state, owner.grace).await
        {
            state.errors.push(join_error(error));
            state.stop(&cancel, owner.grace).await;
        }
    }
    while let Ok(error) = failures.try_recv() {
        state.errors.push(error);
    }
    if !state.errors.is_empty() {
        state.stop(&cancel, owner.grace).await;
    }
    if cancel.is_cancelled() && !state.stopped {
        state.termination = TerminationCause::Canceled;
        state.stop(&cancel, owner.grace).await;
    }
    // Disarm even if a prior signal failed. No numeric PGID signal can occur
    // after this single final reap, including the Group destructor path.
    match final_wait(&mut child, &mut state.group).await {
        Ok(status) => observed = Some(status),
        Err(error) => state.errors.push(io_error("reap owned child", None, error)),
    }
    if let Some(attached) = attachment
        && let Err(error) = attached.restore()
    {
        state.errors.push(error);
    }
    for (stream, spool, complete) in joined {
        let prior_errors = state.errors.len();
        let capture = finish_capture(spool, owner.memory_limit, &mut state.errors).await;
        let complete = complete && state.errors.len() == prior_errors;
        match stream {
            OutputStream::Stdout => {
                stdout = capture;
                stdout_complete = complete;
            }
            OutputStream::Stderr => {
                stderr = capture;
                stderr_complete = complete;
            }
        }
    }
    let log = owner.log.as_ref().and_then(|log| log.locator.clone());
    if child_finished_first
        && observed.is_some_and(|status| !status.success())
        && !state.errors.is_empty()
    {
        use std::os::unix::process::ExitStatusExt;
        state.errors.insert(
            0,
            ConfctlError::Child {
                description: spec.redacted_description,
                code: observed.and_then(|status| status.code()),
                signal: observed.and_then(|status| status.signal()),
                log: log.clone(),
            },
        );
    }
    if !state.errors.is_empty() {
        let primary = state.errors.remove(0).with_secondary(state.errors);
        return Err(ConfctlError::ProcessFailure {
            source: Box::new(primary),
            evidence: Box::new(PartialProcessEvidence {
                status: observed,
                stdout,
                stderr,
                stdout_complete,
                stderr_complete,
                log,
            }),
        });
    }
    let status = observed.expect("successful final wait provides a status");
    // Forward-only spools survive until all infrastructure outcomes are known.
    // Only successful forwarding may discard them.
    if !spec.stdout.captures() {
        stdout = Captured::Unavailable;
    }
    if !spec.stderr.captures() {
        stderr = Captured::Unavailable;
    }
    Ok(ProcessOutcome {
        status,
        stdout,
        stderr,
        stdout_complete,
        stderr_complete,
        started,
        ended: Instant::now(),
        termination: state.termination,
    })
}

struct Pump {
    // Keep ownership outside the task so a failed drain or task panic cannot
    // delete the available prefix before the caller receives its evidence.
    spool: NamedTempFile,
    handle: JoinHandle<bool>,
}
type Relay = JoinHandle<()>;

#[derive(Clone, Copy, Default)]
struct Published {
    length: u64,
    finished: bool,
}

struct PreparedStream {
    spool: NamedTempFile,
    writer: std::fs::File,
    relay_file: Option<std::fs::File>,
}

fn prepare_stream(owner: &Owner, policy: StreamPolicy) -> Result<PreparedStream, ConfctlError> {
    let spool = NamedTempFile::new_in(&owner.spool_dir)
        .map_err(|error| io_error("create stream spool", Some(owner.spool_dir.clone()), error))?;
    let writer = spool
        .reopen()
        .map_err(|error| io_error("open stream spool writer", None, error))?;
    let relay_file = if policy.forwards() {
        Some(
            spool
                .reopen()
                .map_err(|error| io_error("open stream spool reader", None, error))?,
        )
    } else {
        None
    };
    Ok(PreparedStream {
        spool,
        writer,
        relay_file,
    })
}

async fn join_noticing<T>(
    mut task: JoinHandle<T>,
    failures: &mut mpsc::UnboundedReceiver<ConfctlError>,
    cancel: &CancellationToken,
    state: &mut RunState,
    grace: Duration,
) -> Result<T, tokio::task::JoinError> {
    loop {
        tokio::select! {
            biased;
            Some(error) = failures.recv() => {
                state.errors.push(error);
                state.stop(cancel, grace).await;
            },
            _ = cancel.cancelled(), if !state.stopped => {
                state.termination = TerminationCause::Canceled;
                state.stop(cancel, grace).await;
            },
            result = &mut task => return result,
        }
    }
}

async fn finish_capture(
    spool: NamedTempFile,
    limit: usize,
    errors: &mut Vec<ConfctlError>,
) -> Captured {
    // Metadata is authoritative after the writer's final flush/join, including
    // a partial write followed by an error. Keep the spool if reading it fails.
    let length = match spool.as_file().metadata() {
        Ok(metadata) => metadata.len(),
        Err(error) => {
            errors.push(io_error(
                "measure stream capture",
                Some(spool.path().to_path_buf()),
                error,
            ));
            return Captured::Spool {
                file: spool,
                length: 0,
            };
        }
    };
    if length <= limit as u64 {
        match tokio::fs::read(spool.path()).await {
            Ok(bytes) => return Captured::Memory(bytes),
            Err(error) => errors.push(io_error(
                "read small stream capture",
                Some(spool.path().to_path_buf()),
                error,
            )),
        }
    }
    Captured::Spool {
        file: spool,
        length,
    }
}

fn start_stream(
    owner: &Owner,
    prepared: PreparedStream,
    mut reader: impl AsyncRead + Send + Unpin + 'static,
    source: (OutputStream, TaskId),
    nix_progress: Option<crate::nix::progress::Kind>,
    cancel: CancellationToken,
    errors: mpsc::UnboundedSender<ConfctlError>,
) -> (Pump, Option<Relay>) {
    let (stream, task) = source;
    let PreparedStream {
        spool,
        writer,
        relay_file,
    } = prepared;
    let (publish, observe) = watch::channel(Published::default());
    let log = owner.log.clone();
    let pump_errors = errors.clone();
    let pump_cancel = cancel.clone();
    let handle = tokio::spawn(async move {
        let mut writer = File::from_std(writer);
        let mut length = 0_u64;
        let mut log_failed = false;
        let drain = async {
            let mut bytes = vec![0; CHUNK];
            loop {
                let count = tokio::select! {
                    biased;
                    _ = pump_cancel.cancelled() => return Ok::<bool, ConfctlError>(false),
                    result = reader.read(&mut bytes) => result.map_err(|source| ConfctlError::Drain { stream: stream.name(), source })?,
                };
                if count == 0 { return Ok(true); }
                writer.write_all(&bytes[..count]).await.map_err(|error| io_error("write stream spool", None, error))?;
                // Tokio file writes can still be buffered. Publish only bytes
                // visible to the independent relay reader.
                writer.flush().await.map_err(|error| io_error("flush stream spool", None, error))?;
                length += count as u64;
                publish.send_replace(Published { length, finished: false });
                if let Some(log) = &log && !log_failed {
                        tokio::select! {
                            biased;
                            _ = pump_cancel.cancelled() => { log_failed = true; },
                            result = log.write(&bytes[..count]) => if let Err(error) = result {
                                let _ = pump_errors.send(io_error("write command log", log.locator.clone(), error));
                                log_failed = true;
                            },
                        }
                }
            }
        }.await;
        let mut complete = matches!(drain, Ok(true));
        if let Err(error) = drain {
            let _ = pump_errors.send(error);
        }
        if let Err(error) = writer.flush().await {
            complete = false;
            let _ = pump_errors.send(io_error("flush stream spool", None, error));
        }
        publish.send_replace(Published {
            length,
            finished: true,
        });
        complete
    });
    let relay = relay_file.map(|file| {
        let events = owner.events.clone();
        tokio::spawn(async move {
            relay_stream(
                File::from_std(file),
                observe,
                events,
                (task, stream),
                nix_progress,
                cancel,
                errors,
            )
            .await;
        })
    });
    (Pump { spool, handle }, relay)
}

async fn relay_stream(
    mut file: File,
    mut published: watch::Receiver<Published>,
    events: EventSink,
    source: (TaskId, OutputStream),
    nix_progress: Option<crate::nix::progress::Kind>,
    cancel: CancellationToken,
    errors: mpsc::UnboundedSender<ConfctlError>,
) {
    let (task, stream) = source;
    let mut progress = nix_progress.map(crate::nix::progress::Progress::new);
    let mut offset = 0;
    loop {
        let state = *published.borrow_and_update();
        while offset < state.length {
            let length =
                usize::try_from((state.length - offset).min(CHUNK as u64)).unwrap_or(CHUNK);
            let mut bytes = vec![0; length];
            if let Err(error) = file.read_exact(&mut bytes).await {
                let _ = errors.send(io_error("read forwarded spool", None, error));
                return;
            }
            offset += length as u64;
            let updates = progress
                .as_mut()
                .map(|decoder| decoder.feed(&bytes, task))
                .unwrap_or_default();
            tokio::select! {
                result = events.emit(UiEvent::Output { task, stream, bytes }) => if let Err(error) = result { let _ = errors.send(error); return; },
                _ = cancel.cancelled() => return,
            }
            let updates = updates.into_iter().flat_map(|event| {
                progress
                    .as_ref()
                    .and_then(|decoder| decoder.copy_line(&event))
                    .into_iter()
                    .chain(std::iter::once(event))
            });
            for event in updates {
                tokio::select! {
                    result = events.emit(event) => if let Err(error) = result { let _ = errors.send(error); return; },
                    _ = cancel.cancelled() => return,
                }
            }
        }
        if state.finished {
            if let Some(event) = progress.as_mut().and_then(|decoder| decoder.flush(task)) {
                let line = progress
                    .as_ref()
                    .and_then(|decoder| decoder.copy_line(&event));
                for event in line.into_iter().chain(std::iter::once(event)) {
                    tokio::select! {
                        result = events.emit(event) => if let Err(error) = result { let _ = errors.send(error); return; },
                        _ = cancel.cancelled() => return,
                    }
                }
            }
            return;
        }
        tokio::select! {
            result = published.changed() => if result.is_err() { return; },
            _ = cancel.cancelled() => return,
        }
    }
}

#[cfg(test)]
mod tests;

#[cfg(test)]
mod foreground_tests;
