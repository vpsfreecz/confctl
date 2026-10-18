//! One input broker and exclusive prompt/foreground ownership.
//!
//! The broker polls original input plus a wake socket; it never changes shared
//! descriptor O_NONBLOCK flags and reads one byte at a time only for a request.
//! Cancellation wakes and joins the read before ownership can pass elsewhere.
//! Presentation parks its sole writer before prompt or foreground ownership.

use crate::{
    error::ConfctlError,
    events::{EventSink, UiEvent},
    timing::Timing,
};
use std::{
    io::{self, Read, Write},
    os::{
        fd::{AsFd, OwnedFd},
        unix::net::UnixStream,
    },
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
        mpsc,
    },
    thread,
};
use tokio::sync::{oneshot, watch};
use tokio_util::sync::CancellationToken;

pub mod foreground;

/// Includes the line terminator when present. Never applies to child stdin.
pub const PROMPT_LINE_LIMIT: usize = 1024 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TerminalState {
    Idle,
    Live,
    Prompt,
    ExternalChild,
}

pub struct ConfirmPrompt {
    pub preview: Vec<UiEvent>,
    pub message: String,
    pub assume_yes: bool,
    pub always: bool,
}

impl Default for ConfirmPrompt {
    fn default() -> Self {
        Self {
            preview: Vec::new(),
            message: "\nContinue? [y/N]: ".into(),
            assume_yes: false,
            always: false,
        }
    }
}

struct Session {
    broker: InputBroker,
    state: watch::Sender<TerminalState>,
    owned: AtomicBool,
    controlling_tty: bool,
    terminal: OwnedFd,
    suspend: Mutex<Option<tokio::sync::mpsc::Sender<foreground::ForegroundSuspendRequest>>>,
}

#[derive(Clone)]
pub struct Ui {
    session: Arc<Session>,
    events: EventSink,
    cancellation: CancellationToken,
    timing: Option<Timing>,
}

struct Lease {
    session: Arc<Session>,
    previous: TerminalState,
}

impl Drop for Lease {
    fn drop(&mut self) {
        self.session.state.send_replace(self.previous);
        self.session.owned.store(false, Ordering::Release);
    }
}

pub struct ForegroundLease {
    _lease: Lease,
}

impl ForegroundLease {
    /// Validate real handoff prerequisites before a child exists. Non-TTY
    /// inheritance needs no job control; controlling TTYs need the app's live
    /// suspend owner and current foreground ownership.
    pub fn validate_process_handoff(&self) -> Result<(), ConfctlError> {
        self._lease.session.broker.check_available()?;
        let session = &self._lease.session;
        if session.controlling_tty {
            let ready = session
                .suspend
                .lock()
                .unwrap_or_else(|error| error.into_inner())
                .as_ref()
                .is_some_and(|sender| !sender.is_closed());
            if !ready {
                return Err(ConfctlError::ForegroundJobControlUnavailable);
            }
            let foreground = rustix::termios::tcgetpgrp(&session.terminal).map_err(|source| {
                ConfctlError::Io {
                    operation: "validate foreground terminal ownership",
                    path: None,
                    source: source.into(),
                }
            })?;
            if foreground != rustix::process::getpgrp() {
                return Err(ConfctlError::TerminalBusy);
            }
        }
        Ok(())
    }
}

impl Ui {
    pub fn new(
        input: impl AsFd,
        events: EventSink,
        cancellation: CancellationToken,
        timing: Option<Timing>,
    ) -> Result<Self, ConfctlError> {
        let terminal = input.as_fd().try_clone_to_owned().map_err(input_error)?;
        let controlling_tty = rustix::termios::tcgetpgrp(&terminal).is_ok();
        let broker = InputBroker::new(&terminal)?;
        let (state, _) = watch::channel(TerminalState::Idle);
        Ok(Self {
            session: Arc::new(Session {
                broker,
                state,
                owned: AtomicBool::new(false),
                controlling_tty,
                terminal,
                suspend: Mutex::new(None),
            }),
            events,
            cancellation,
            timing,
        })
    }

    pub fn with_foreground_suspend(
        self,
        sender: tokio::sync::mpsc::Sender<foreground::ForegroundSuspendRequest>,
    ) -> Self {
        *self
            .session
            .suspend
            .lock()
            .unwrap_or_else(|error| error.into_inner()) = Some(sender);
        self
    }

    pub fn observe_state(&self) -> watch::Receiver<TerminalState> {
        self.session.state.subscribe()
    }
    pub fn state(&self) -> TerminalState {
        *self.session.state.borrow()
    }

    pub fn set_live(&self, live: bool) -> Result<(), ConfctlError> {
        if self
            .session
            .owned
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .is_err()
        {
            return Err(ConfctlError::TerminalBusy);
        }
        self.session.state.send_replace(if live {
            TerminalState::Live
        } else {
            TerminalState::Idle
        });
        self.session.owned.store(false, Ordering::Release);
        Ok(())
    }

    async fn acquire(&self, state: TerminalState) -> Result<Lease, ConfctlError> {
        if self
            .session
            .owned
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .is_err()
        {
            return Err(ConfctlError::TerminalBusy);
        }
        let previous = self.session.state.send_replace(state);
        let lease = Lease {
            session: self.session.clone(),
            previous,
        };
        self.emit(UiEvent::TerminalState(state)).await?;
        Ok(lease)
    }

    async fn emit(&self, event: UiEvent) -> Result<(), ConfctlError> {
        tokio::select! { result = self.events.emit(event) => result, _ = self.cancellation.cancelled() => Err(ConfctlError::Cancelled) }
    }

    pub async fn confirm(&self, prompt: ConfirmPrompt) -> Result<bool, ConfctlError> {
        if prompt.assume_yes && !prompt.always {
            return Ok(true);
        }
        let _lease = self.acquire(TerminalState::Prompt).await?;
        let mut preview = prompt.preview;
        loop {
            self.emit(UiEvent::PromptFrame {
                preview: std::mem::take(&mut preview),
                text: prompt.message.clone(),
            })
            .await?;
            let line = {
                let _wait = self.timing.as_ref().map(Timing::prompt);
                self.session.broker.line(false, &self.cancellation).await?
            };
            let answer = String::from_utf8(line).map_err(|error| {
                input_error(io::Error::new(
                    io::ErrorKind::InvalidData,
                    error.utf8_error(),
                ))
            })?;
            let answer = answer
                .trim_matches(|c| matches!(c, '\0' | '\t' | '\n' | '\r' | '\u{b}' | '\u{c}' | ' '))
                .to_ascii_lowercase();
            match answer.as_str() {
                "y" | "n" => {
                    self.emit(UiEvent::PromptFrame {
                        preview: Vec::new(),
                        text: "\n".into(),
                    })
                    .await?;
                    return Ok(answer == "y");
                }
                _ => {}
            }
        }
    }

    pub async fn protected_line(&self, prompt: String) -> Result<Vec<u8>, ConfctlError> {
        let _lease = self.acquire(TerminalState::Prompt).await?;
        self.emit(UiEvent::PromptFrame {
            preview: Vec::new(),
            text: prompt,
        })
        .await?;
        let _wait = self.timing.as_ref().map(Timing::prompt);
        self.session.broker.line(true, &self.cancellation).await
    }

    pub async fn foreground(&self) -> Result<ForegroundLease, ConfctlError> {
        self.session.broker.check_available()?;
        Ok(ForegroundLease {
            _lease: self.acquire(TerminalState::ExternalChild).await?,
        })
    }

    pub fn shutdown(&self) -> Result<(), ConfctlError> {
        self.session.broker.shutdown()
    }
}

fn input_error(source: io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation: "read terminal input",
        path: None,
        source,
    }
}

enum Request {
    Line {
        id: u64,
        protected: bool,
        response: oneshot::Sender<InputReply>,
    },
    Cancel {
        id: u64,
        restored: Option<mpsc::SyncSender<()>>,
    },
    Shutdown,
}

struct Pending {
    id: u64,
    bytes: Vec<u8>,
    response: oneshot::Sender<InputReply>,
    original: Option<rustix::termios::Termios>,
    unobserved: Arc<Mutex<Vec<ConfctlError>>>,
}

/// The reply owns its error until consumption, including while queued after a
/// successful send. Dropping it returns failure ownership to checked shutdown.
struct InputReply {
    result: Option<Result<Vec<u8>, ConfctlError>>,
    unobserved: Arc<Mutex<Vec<ConfctlError>>>,
}

impl InputReply {
    fn consume(mut self) -> Result<Vec<u8>, ConfctlError> {
        self.result.take().expect("input reply consumed once")
    }
}

impl Drop for InputReply {
    fn drop(&mut self) {
        if let Some(Err(error)) = self.result.take()
            && !matches!(error, ConfctlError::Cancelled)
        {
            self.unobserved
                .lock()
                .unwrap_or_else(|error| error.into_inner())
                .push(error);
        }
    }
}

fn reply(
    response: oneshot::Sender<InputReply>,
    result: Result<Vec<u8>, ConfctlError>,
    unobserved: &Arc<Mutex<Vec<ConfctlError>>>,
) {
    let _ = response.send(InputReply {
        result: Some(result),
        unobserved: unobserved.clone(),
    });
}

pub struct InputBroker {
    requests: mpsc::Sender<Request>,
    wake: Mutex<UnixStream>,
    worker: Mutex<Option<thread::JoinHandle<()>>>,
    next: AtomicU64,
    poisoned: Arc<AtomicBool>,
    unobserved: Arc<Mutex<Vec<ConfctlError>>>,
}

struct PendingRead<'a> {
    broker: &'a InputBroker,
    id: u64,
    armed: bool,
}

impl Drop for PendingRead<'_> {
    fn drop(&mut self) {
        if self.armed {
            let (restored, receive) = mpsc::sync_channel(1);
            if self
                .broker
                .send(Request::Cancel {
                    id: self.id,
                    restored: Some(restored),
                })
                .is_ok()
            {
                // Future drop must not release its lease before the broker has
                // stopped reading and restored protected input. The wake poll
                // makes this a joined cleanup, not a detached stdin reader.
                let _ = receive.recv();
            }
        }
    }
}

impl InputBroker {
    pub fn new(input: impl AsFd) -> Result<Self, ConfctlError> {
        let input = input.as_fd().try_clone_to_owned().map_err(input_error)?;
        let (wake, receiver) = UnixStream::pair().map_err(input_error)?;
        let (requests, messages) = mpsc::channel();
        let poisoned = Arc::new(AtomicBool::new(false));
        let worker_poisoned = poisoned.clone();
        let unobserved = Arc::new(Mutex::new(Vec::new()));
        let worker_unobserved = unobserved.clone();
        let worker = thread::Builder::new()
            .name("confctl-input".into())
            .spawn(move || {
                broker(
                    input,
                    receiver,
                    messages,
                    worker_poisoned,
                    worker_unobserved,
                )
            })
            .map_err(input_error)?;
        Ok(Self {
            requests,
            wake: Mutex::new(wake),
            worker: Mutex::new(Some(worker)),
            next: AtomicU64::new(0),
            poisoned,
            unobserved,
        })
    }

    fn check_available(&self) -> Result<(), ConfctlError> {
        if self.poisoned.load(Ordering::Acquire) {
            Err(ConfctlError::InputTooLong {
                limit: PROMPT_LINE_LIMIT,
            })
        } else {
            Ok(())
        }
    }

    fn send(&self, request: Request) -> Result<(), ConfctlError> {
        self.requests.send(request).map_err(|_| {
            input_error(io::Error::new(
                io::ErrorKind::BrokenPipe,
                "input broker stopped",
            ))
        })?;
        self.wake
            .lock()
            .unwrap_or_else(|error| error.into_inner())
            .write_all(&[1])
            .map_err(input_error)
    }

    pub async fn line(
        &self,
        protected: bool,
        cancel: &CancellationToken,
    ) -> Result<Vec<u8>, ConfctlError> {
        self.check_available()?;
        if cancel.is_cancelled() {
            return Err(ConfctlError::Cancelled);
        }
        let id = self.next.fetch_add(1, Ordering::Relaxed);
        let (response, mut receive) = oneshot::channel();
        self.send(Request::Line {
            id,
            protected,
            response,
        })?;
        let mut pending = PendingRead {
            broker: self,
            id,
            armed: true,
        };
        let result = tokio::select! {
            result = &mut receive => result.map_err(|_| input_error(io::Error::other("input broker lost response")))?.consume(),
            _ = cancel.cancelled() => {
                self.send(Request::Cancel { id, restored: None })?;
                // Await restoration/ack before releasing terminal ownership.
                let result = receive.await.map_err(|_| input_error(io::Error::other("input cancellation lost response")))?.consume();
                match result { Err(error) if !matches!(error, ConfctlError::Cancelled) => Err(error), _ => Err(ConfctlError::Cancelled) }
            },
        };
        pending.armed = false;
        result
    }

    /// Call after joining/dropping prompt futures. Joins the input worker and
    /// returns discarded errors, then wake/join failures, without duplicating a
    /// consumed input error. Clean canceled reads do not create sink failures.
    pub fn shutdown(&self) -> Result<(), ConfctlError> {
        let mut worker = self
            .worker
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        let mut transport_errors = Vec::new();
        if let Some(handle) = worker.take() {
            if let Err(error) = self.send(Request::Shutdown) {
                transport_errors.push(error);
            }
            if handle.join().is_err() {
                transport_errors.push(input_error(io::Error::other("input broker panicked")));
            }
        }
        let mut failures = std::mem::take(
            &mut *self
                .unobserved
                .lock()
                .unwrap_or_else(|error| error.into_inner()),
        );
        failures.extend(transport_errors);
        if failures.is_empty() {
            Ok(())
        } else {
            Err(failures.remove(0).with_secondary(failures))
        }
    }
}

impl Drop for InputBroker {
    fn drop(&mut self) {
        let _ = self.shutdown();
    }
}

fn complete(input: &OwnedFd, pending: Pending, result: Result<Vec<u8>, ConfctlError>) {
    let mut cleanup = Vec::new();
    if let Some(original) = pending.original {
        // An aborted protected answer must not become later shell/child input.
        // Do this before re-enabling echo, without waiting for a newline.
        if result.is_err()
            && let Err(error) =
                rustix::termios::tcflush(input, rustix::termios::QueueSelector::IFlush)
        {
            cleanup.push(ConfctlError::Io {
                operation: "flush protected input",
                path: None,
                source: error.into(),
            });
        }
        if let Err(error) =
            rustix::termios::tcsetattr(input, rustix::termios::OptionalActions::Now, &original)
        {
            cleanup.push(ConfctlError::Io {
                operation: "restore terminal input",
                path: None,
                source: error.into(),
            });
        }
    }
    let result = match result {
        Err(primary) => Err(primary.with_secondary(cleanup)),
        Ok(bytes) if cleanup.is_empty() => Ok(bytes),
        Ok(_) => Err(cleanup.remove(0).with_secondary(cleanup)),
    };
    reply(pending.response, result, &pending.unobserved);
}

fn broker(
    input: OwnedFd,
    mut wake: UnixStream,
    messages: mpsc::Receiver<Request>,
    poisoned: Arc<AtomicBool>,
    unobserved: Arc<Mutex<Vec<ConfctlError>>>,
) {
    let mut pending: Option<Pending> = None;
    loop {
        let mut descriptors = vec![rustix::event::PollFd::new(
            &wake,
            rustix::event::PollFlags::IN,
        )];
        if pending.is_some() {
            descriptors.push(rustix::event::PollFd::new(
                &input,
                rustix::event::PollFlags::IN,
            ));
        }
        match rustix::event::poll(&mut descriptors, None) {
            Ok(_) => {}
            Err(rustix::io::Errno::INTR) => continue,
            Err(error) => {
                if let Some(pending) = pending.take() {
                    complete(&input, pending, Err(input_error(error.into())));
                }
                return;
            }
        }
        let wake_ready = !descriptors[0].revents().is_empty();
        let input_ready = descriptors
            .get(1)
            .is_some_and(|fd| !fd.revents().is_empty());
        drop(descriptors);
        if wake_ready {
            let mut byte = [0];
            if wake.read(&mut byte).unwrap_or(0) == 0 {
                return;
            }
            while let Ok(request) = messages.try_recv() {
                match request {
                    Request::Shutdown => {
                        if let Some(pending) = pending.take() {
                            complete(&input, pending, Err(ConfctlError::Cancelled));
                        }
                        return;
                    }
                    Request::Cancel { id, restored } => {
                        if pending.as_ref().is_some_and(|pending| pending.id == id)
                            && let Some(pending) = pending.take()
                        {
                            complete(&input, pending, Err(ConfctlError::Cancelled));
                        }
                        if let Some(restored) = restored {
                            let _ = restored.send(());
                        }
                    }
                    Request::Line {
                        id,
                        protected,
                        response,
                    } => {
                        if poisoned.load(Ordering::Acquire) {
                            reply(
                                response,
                                Err(ConfctlError::InputTooLong {
                                    limit: PROMPT_LINE_LIMIT,
                                }),
                                &unobserved,
                            );
                            continue;
                        }
                        if pending.is_some() {
                            reply(response, Err(ConfctlError::TerminalBusy), &unobserved);
                            continue;
                        }
                        let mut original = None;
                        if protected && rustix::termios::isatty(&input) {
                            match rustix::termios::tcgetattr(&input) {
                                Ok(mut termios) => {
                                    original = Some(termios.clone());
                                    termios
                                        .local_modes
                                        .remove(rustix::termios::LocalModes::ECHO);
                                    if let Err(error) = rustix::termios::tcsetattr(
                                        &input,
                                        rustix::termios::OptionalActions::Now,
                                        &termios,
                                    ) {
                                        reply(
                                            response,
                                            Err(input_error(error.into())),
                                            &unobserved,
                                        );
                                        continue;
                                    }
                                }
                                Err(error) => {
                                    reply(response, Err(input_error(error.into())), &unobserved);
                                    continue;
                                }
                            }
                        }
                        pending = Some(Pending {
                            id,
                            bytes: Vec::new(),
                            response,
                            original,
                            unobserved: unobserved.clone(),
                        });
                    }
                }
            }
        }
        if input_ready && pending.is_some() {
            let mut byte = [0];
            match rustix::io::read(&input, &mut byte) {
                Ok(0) => {
                    if let Some(pending) = pending.take() {
                        let result = if pending.bytes.is_empty() {
                            Err(ConfctlError::InputEof)
                        } else {
                            Ok(pending.bytes.clone())
                        };
                        complete(&input, pending, result);
                    }
                }
                Ok(_) => {
                    if let Some(active) = &mut pending {
                        active.bytes.push(byte[0]);
                    }
                    if pending
                        .as_ref()
                        .is_some_and(|active| active.bytes.len() > PROMPT_LINE_LIMIT)
                    {
                        // Do not drain or reuse the unread suffix as an answer.
                        // Every later prompt and foreground handoff is refused.
                        poisoned.store(true, Ordering::Release);
                        if let Some(pending) = pending.take() {
                            complete(
                                &input,
                                pending,
                                Err(ConfctlError::InputTooLong {
                                    limit: PROMPT_LINE_LIMIT,
                                }),
                            );
                        }
                    } else if byte[0] == b'\n'
                        && let Some(pending) = pending.take()
                    {
                        let bytes = pending.bytes.clone();
                        complete(&input, pending, Ok(bytes));
                    }
                }
                Err(rustix::io::Errno::INTR) => {}
                Err(error) => {
                    if let Some(pending) = pending.take() {
                        complete(&input, pending, Err(input_error(error.into())));
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests;

#[cfg(test)]
pub(crate) mod pty_tests;
