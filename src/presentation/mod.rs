//! One event consumer owns confctl output, inline painting and parked output.
//!
//! The model is independent of operations. Plain output keeps source bytes;
//! live text is sanitized and bounded without modifying captures or raw logs.

pub mod live;
pub mod model;
pub mod plain;

/// Owned deferred UI bytes survive replay errors until the app retains them.
/// Offsets count only bytes confirmed accepted by each original output writer.
/// A locator is present only when the complete pending data is verified durable.
#[derive(Debug)]
pub struct PendingOutput {
    pub stdout: crate::process::Captured,
    pub stderr: crate::process::Captured,
    pub stdout_replayed: u64,
    pub stderr_replayed: u64,
    pub verified_log: Option<std::path::PathBuf>,
    pub unspooled: Option<UnspooledOutput>,
}

/// A failed append may already have written a prefix after spool_offset. Keep
/// the full current chunk as evidence; never concatenate or replay it blindly.
#[derive(Debug)]
pub struct UnspooledOutput {
    pub stream: crate::events::OutputStream,
    pub bytes: crate::process::Captured,
    pub spool_offset: u64,
}

use crate::{domain::ColorMode, error::ConfctlError, events::OutputStream, process::Captured};
use std::{
    io::{self, IsTerminal, Read, Seek, SeekFrom, Write},
    os::fd::{AsFd, OwnedFd},
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};
use tempfile::NamedTempFile;

pub type OutputWriter = Arc<Mutex<Box<dyn Write + Send>>>;

pub struct OutputTargets {
    pub stdout: OutputWriter,
    pub stderr: OutputWriter,
}

impl OutputTargets {
    pub fn standard() -> Self {
        Self {
            stdout: Arc::new(Mutex::new(Box::new(io::stdout()))),
            stderr: Arc::new(Mutex::new(Box::new(io::stderr()))),
        }
    }
    fn writer(&self, stream: OutputStream) -> &OutputWriter {
        match stream {
            OutputStream::Stdout => &self.stdout,
            OutputStream::Stderr => &self.stderr,
        }
    }
}

fn lock_writer(
    writer: &OutputWriter,
) -> io::Result<std::sync::MutexGuard<'_, Box<dyn Write + Send>>> {
    writer
        .lock()
        .map_err(|_| io::Error::other("output writer lock poisoned"))
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct OutputPolicy {
    pub live: bool,
    pub stdout_color: bool,
    pub stderr_color: bool,
}

impl OutputPolicy {
    pub fn resolve(
        color: ColorMode,
        stdout_tty: bool,
        stderr_tty: bool,
        usable: bool,
        tty_enabled: bool,
        no_color: bool,
    ) -> Self {
        let colored = |tty| match color {
            ColorMode::Always => true,
            ColorMode::Never => false,
            ColorMode::Auto => tty && !no_color,
        };
        Self {
            live: stdout_tty && stderr_tty && usable && tty_enabled,
            stdout_color: colored(stdout_tty),
            stderr_color: colored(stderr_tty),
        }
    }
    fn color(self, stream: OutputStream) -> bool {
        match stream {
            OutputStream::Stdout => self.stdout_color,
            OutputStream::Stderr => self.stderr_color,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PresentationMode {
    Full,
    CommandSpinner,
    Plain,
}

impl PresentationMode {
    fn resolve(full: bool, compact: bool, requested_full: bool) -> Self {
        if requested_full && full {
            Self::Full
        } else if compact {
            Self::CommandSpinner
        } else {
            Self::Plain
        }
    }
}

pub struct PresentationConfig {
    pub command: String,
    pub operation: String,
    pub spool_dir: PathBuf,
    pub configuration_dir: Option<PathBuf>,
    pub invocation_log: Option<PathBuf>,
    pub policy: OutputPolicy,
    pub(crate) mode: PresentationMode,
    pub initial_size: (u16, u16),
    pub terminal: Option<OwnedFd>,
    pub max_tasks: usize,
    pub tail_rows: usize,
    pub tail_line_bytes: usize,
}

impl PresentationConfig {
    pub fn for_process(
        command: String,
        operation: String,
        color: ColorMode,
        spool_dir: PathBuf,
    ) -> Self {
        let stdout = io::stdout();
        let stderr = io::stderr();
        let terminal = if stdout.is_terminal() {
            stdout.as_fd()
        } else {
            stderr.as_fd()
        }
        .try_clone_to_owned()
        .ok();
        let dimensions = terminal
            .as_ref()
            .and_then(|fd| rustix::termios::tcgetwinsize(fd).ok())
            .map(|size| (size.ws_col, size.ws_row));
        let usable = dimensions.is_some_and(|(cols, rows)| cols > 1 && rows > 0)
            && std::env::var_os("TERM").as_deref() != Some(std::ffi::OsStr::new("dumb"));
        let no_color = std::env::var_os("NO_COLOR").is_some_and(|value| {
            value
                .as_encoded_bytes()
                .iter()
                .any(|byte| !matches!(byte, 0 | b' ' | b'\t' | b'\n' | b'\r' | 11 | 12))
        });
        let policy = OutputPolicy::resolve(
            color,
            stdout.is_terminal(),
            stderr.is_terminal(),
            usable,
            std::env::var_os("CONFCTL_TTY").as_deref() != Some(std::ffi::OsStr::new("0")),
            no_color,
        );
        let mode = PresentationMode::resolve(
            policy.live,
            stderr.is_terminal()
                && usable
                && std::env::var_os("CONFCTL_TTY").as_deref() != Some(std::ffi::OsStr::new("0")),
            true,
        );
        Self {
            command,
            operation,
            spool_dir,
            configuration_dir: None,
            invocation_log: None,
            policy,
            mode,
            initial_size: dimensions.unwrap_or((80, 24)),
            terminal,
            max_tasks: 5,
            tail_rows: 100,
            tail_line_bytes: 4096,
        }
    }
    pub(crate) fn select_mode(&mut self, full: bool) {
        if !full && self.mode == PresentationMode::Full {
            self.mode = PresentationMode::CommandSpinner;
        }
    }
}

/// A transfer spool is required-output ownership, not a second command log.
/// Clear only after successful destination flush; errors retain the full file
/// and conservative confirmed replay offset, including any possible overlap.
struct DeferredStream {
    file: Option<NamedTempFile>,
    length: u64,
    replayed: u64,
}

impl DeferredStream {
    fn new(directory: &Path) -> Result<Self, ConfctlError> {
        let file = NamedTempFile::new_in(directory)
            .map_err(|source| output_io("create deferred output spool", source))?;
        Ok(Self {
            file: Some(file),
            length: 0,
            replayed: 0,
        })
    }

    fn append(&mut self, bytes: &[u8]) -> io::Result<()> {
        self.append_using(bytes, |file, bytes| file.write(bytes))
    }

    fn append_using(
        &mut self,
        mut bytes: &[u8],
        mut write: impl FnMut(&mut std::fs::File, &[u8]) -> io::Result<usize>,
    ) -> io::Result<()> {
        let file = self
            .file
            .as_mut()
            .expect("owned transfer spool")
            .as_file_mut();
        file.seek(SeekFrom::End(0))?;
        while !bytes.is_empty() {
            match write(file, bytes) {
                Ok(0) => return Err(io::Error::from(io::ErrorKind::WriteZero)),
                Ok(count) => {
                    self.length += count as u64;
                    bytes = &bytes[count..];
                }
                Err(error) if error.kind() == io::ErrorKind::Interrupted => continue,
                Err(error) => return Err(error),
            }
        }
        file.flush()
    }

    fn replay(&mut self, writer: &OutputWriter) -> io::Result<Option<u8>> {
        if self.replayed == self.length {
            return Ok(None);
        }
        let file = self
            .file
            .as_mut()
            .expect("owned transfer spool")
            .as_file_mut();
        file.seek(SeekFrom::Start(self.replayed))?;
        let mut output = lock_writer(writer)?;
        let mut bytes = [0; 16 * 1024];
        let mut last = None;
        while self.replayed < self.length {
            let remaining = (self.length - self.replayed).min(bytes.len() as u64) as usize;
            let read = file.read(&mut bytes[..remaining])?;
            if read == 0 {
                return Err(io::Error::from(io::ErrorKind::UnexpectedEof));
            }
            let mut chunk = &bytes[..read];
            while !chunk.is_empty() {
                match output.write(chunk) {
                    Ok(0) => return Err(io::Error::from(io::ErrorKind::WriteZero)),
                    Ok(count) => {
                        self.replayed += count as u64;
                        last = Some(chunk[count - 1]);
                        chunk = &chunk[count..];
                    }
                    Err(error) if error.kind() == io::ErrorKind::Interrupted => continue,
                    Err(error) => return Err(error),
                }
            }
        }
        output.flush()?;
        // Retain evidence if truncate/seek fails; no ambiguous automatic replay.
        file.set_len(0)?;
        file.rewind()?;
        self.length = 0;
        self.replayed = 0;
        Ok(last)
    }

    fn capture(&mut self) -> Captured {
        self.file
            .take()
            .map_or(Captured::Unavailable, |file| Captured::Spool {
                file,
                length: self.length,
            })
    }
}

fn output_io(operation: &'static str, source: io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path: None,
        source,
    }
}

use crate::{
    events::{EventReceiver, UiEvent},
    terminal::TerminalState,
    timing::Timing,
};
use std::{
    collections::HashSet,
    sync::atomic::{AtomicBool, Ordering},
    time::{Duration, Instant},
};
use tokio::sync::watch;

struct Completion {
    error: Mutex<Option<ConfctlError>>,
    failed: AtomicBool,
    done: watch::Sender<bool>,
}

impl Completion {
    fn retain(&self, error: ConfctlError) {
        let mut slot = self
            .error
            .lock()
            .unwrap_or_else(|poison| poison.into_inner());
        *slot = Some(match slot.take() {
            Some(primary) => primary.with_secondary(vec![error]),
            None => error,
        });
        self.failed.store(true, Ordering::Release);
    }
    fn take(&self) -> Result<(), ConfctlError> {
        let error = self
            .error
            .lock()
            .unwrap_or_else(|poison| poison.into_inner())
            .take();
        match error {
            Some(error) => Err(error),
            None if self.failed.load(Ordering::Acquire) => Err(ConfctlError::EventDelivery(
                "presentation failure was already transferred by checked finish".into(),
            )),
            None => Ok(()),
        }
    }
}

/// This owns the checked failure/evidence slot, not a task registry. The app
/// requests finish before joining a normal loop; after failure/abort it joins
/// first, then consumes the retained source exactly once through finish.
#[derive(Clone)]
pub struct PresentationControl {
    finish: watch::Sender<bool>,
    completion: Arc<Completion>,
}

impl PresentationControl {
    pub async fn finish(&self) -> Result<(), ConfctlError> {
        let mut done = self.completion.done.subscribe();
        self.finish.send_replace(true);
        while !*done.borrow_and_update() {
            done.changed().await.map_err(|_| {
                ConfctlError::EventDelivery(
                    "presentation cleanup owner closed before completion".into(),
                )
            })?;
        }
        // No await after taking the sole owned evidence. Dropped/concurrent
        // finish waiters cannot erase it or turn a sticky failure into success.
        self.completion.take()
    }
}

struct SharedOutput(OutputWriter);
impl Write for SharedOutput {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        lock_writer(&self.0)?.write(bytes)
    }
    fn flush(&mut self) -> io::Result<()> {
        lock_writer(&self.0)?.flush()
    }
}

pub struct Presentation {
    receiver: EventReceiver,
    state: watch::Receiver<TerminalState>,
    current_state: TerminalState,
    outputs: OutputTargets,
    config: PresentationConfig,
    timing: Option<Timing>,
    started: String,
    origin: Instant,
    model: model::Model,
    stdout: DeferredStream,
    stderr: DeferredStream,
    unspooled: Option<UnspooledOutput>,
    painter: Option<live::RelativePainter<SharedOutput>>,
    spinner_painter: Option<live::RelativePainter<SharedOutput>>,
    mode: PresentationMode,
    commands: Vec<ActiveCommand>,
    compact_anchor: bool,
    build_scope: Option<BuildScope>,
    resize: Option<tokio::signal::unix::Signal>,
    finish: watch::Receiver<bool>,
    completion: Arc<Completion>,
    cleaned: bool,
    active_view: bool,
    seen_groups: HashSet<u64>,
    seen_checkpoints: HashSet<crate::events::TaskId>,
    last_geometry: Option<(u16, u16)>,
    last_paint: Option<Instant>,
    scrollback_logs: HashSet<PathBuf>,
}

struct ActiveCommand {
    id: crate::events::CommandId,
    task: crate::events::TaskId,
    display: String,
    suppressed: bool,
}

struct BuildScope {
    mode: PresentationMode,
    operation: String,
    first_row: usize,
}

impl Presentation {
    pub fn new(
        receiver: EventReceiver,
        state: watch::Receiver<TerminalState>,
        outputs: OutputTargets,
        config: PresentationConfig,
        timing: Option<Timing>,
    ) -> Result<(PresentationControl, Self), ConfctlError> {
        let stdout = DeferredStream::new(&config.spool_dir)?;
        let stderr = DeferredStream::new(&config.spool_dir)?;
        let painter = config.policy.live.then(|| {
            live::RelativePainter::new(
                SharedOutput(outputs.stdout.clone()),
                config.policy.stdout_color,
            )
        });
        let spinner_painter = (config.mode != PresentationMode::Plain).then(|| {
            live::RelativePainter::new(
                SharedOutput(outputs.stderr.clone()),
                config.policy.stderr_color,
            )
        });
        let resize = if config.mode != PresentationMode::Plain {
            Some(
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::window_change())
                    .map_err(|source| output_io("observe terminal resize", source))?,
            )
        } else {
            None
        };
        let started = timing
            .as_ref()
            .map(|timing| timing.started().with_timezone(&chrono::Local))
            .unwrap_or_else(chrono::Local::now)
            .format("%Y-%m-%d %H:%M:%S %:z")
            .to_string();
        let mut model = model::Model::new(config.tail_rows, config.tail_line_bytes);
        if let Some(path) = &config.invocation_log {
            model.apply(&UiEvent::LogAvailable {
                task: crate::events::TaskId(0),
                path: path.clone(),
            })?;
        }
        let (finish, finish_requests) = watch::channel(false);
        let (done, _) = watch::channel(false);
        let completion = Arc::new(Completion {
            error: Mutex::new(None),
            failed: AtomicBool::new(false),
            done,
        });
        let control = PresentationControl {
            finish,
            completion: completion.clone(),
        };
        let current_state = *state.borrow();
        Ok((
            control,
            Self {
                receiver,
                state,
                current_state,
                outputs,
                mode: config.mode,
                config,
                timing,
                started,
                origin: Instant::now(),
                model,
                stdout,
                stderr,
                unspooled: None,
                painter,
                spinner_painter,
                commands: Vec::new(),
                compact_anchor: true,
                build_scope: None,
                resize,
                finish: finish_requests,
                completion,
                cleaned: false,
                active_view: false,
                seen_groups: HashSet::new(),
                seen_checkpoints: HashSet::new(),
                last_geometry: None,
                last_paint: None,
                scrollback_logs: HashSet::new(),
            },
        ))
    }

    /// Errors here are notifications by task provenance. Checked finish owns
    /// the actual source/evidence; the app substitutes it after this task joins.
    pub async fn run(mut self) -> Result<(), ConfctlError> {
        let result = self.run_loop().await;
        let result = match result {
            Ok(()) => Ok(()),
            Err(primary) => {
                self.receiver.close();
                let secondary = self.release_painter().err().into_iter().collect();
                let error = primary.with_secondary(secondary);
                let pending = self.pending();
                self.completion.retain(ConfctlError::OutputFailure {
                    source: Box::new(error),
                    pending: Box::new(pending),
                });
                Err(ConfctlError::EventDelivery(
                    "presentation loop failed; checked finish retains its source and evidence"
                        .into(),
                ))
            }
        };
        self.cleaned = true;
        self.completion.done.send_replace(true);
        result
    }

    async fn run_loop(&mut self) -> Result<(), ConfctlError> {
        let mut timer = tokio::time::interval(Duration::from_millis(100));
        timer.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        let mut events_open = true;
        let mut state_open = true;
        loop {
            if *self.finish.borrow_and_update() {
                break;
            }
            tokio::select! {
                delivery = self.receiver.receive(), if events_open => {
                    if let Some(delivery) = delivery { self.deliver(delivery).await?; }
                    else { events_open = false; }
                },
                change = self.state.changed(), if state_open => {
                    if change.is_ok() { let state = *self.state.borrow_and_update(); self.transition(state)?; }
                    else { state_open = false; }
                },
                change = self.finish.changed() => {
                    if change.is_err() { return Err(ConfctlError::EventDelivery("presentation finish owner dropped".into())); }
                },
                _ = resize_notification(&mut self.resize) => {
                    if let Some(painter) = &mut self.painter { painter.invalidate(); }
                    self.paint_now()?;
                },
                _ = timer.tick(), if !self.parked() && self.transient_active() => {
                    self.paint()?;

                },
            }
        }
        // Explicit closure rejects blocked/new senders while queued required
        // events are still processed, rather than waiting for every clone.
        self.receiver.close();
        while let Some(delivery) = self.receiver.receive().await {
            self.deliver(delivery).await?;
        }
        self.current_state = *self.state.borrow();
        if self.parked() {
            return Err(ConfctlError::TerminalBusy);
        }
        self.replay()?;
        self.finalize_group()?;
        self.flush_outputs()
    }

    async fn deliver(&mut self, delivery: crate::events::Delivery) -> Result<(), ConfctlError> {
        let (event, acknowledgement) = delivery.into_parts();
        let result = match event {
            UiEvent::CommandStarting {
                command,
                task,
                display,
            } => {
                let state = *self.state.borrow_and_update();
                self.transition(state)?;
                self.admit_command(command, task, &display);
                if self.mode == PresentationMode::Full {
                    self.command_starting(task, display).await
                } else {
                    self.paint_now()
                }
            }
            event => self.event(event),
        };
        acknowledgement.acknowledge(result.as_ref().map(|_| ()).map_err(ToString::to_string));
        result
    }

    fn admit_command(
        &mut self,
        id: crate::events::CommandId,
        task: crate::events::TaskId,
        display: &str,
    ) {
        self.commands.retain(|command| command.id != id);
        self.commands.push(ActiveCommand {
            id,
            task,
            display: model::sanitize_text(display).replace(['\n', '\r'], " "),
            suppressed: !self.compact_anchor || self.parked(),
        });
    }

    fn suppress_commands(&mut self, task: Option<crate::events::TaskId>) {
        for command in &mut self.commands {
            if task.is_none_or(|task| command.task == task) {
                command.suppressed = true;
            }
        }
    }

    fn transient_active(&self) -> bool {
        match self.mode {
            PresentationMode::Full => self.active_view,
            PresentationMode::CommandSpinner => {
                self.compact_anchor && self.commands.iter().any(|command| !command.suppressed)
            }
            PresentationMode::Plain => false,
        }
    }

    fn build_phase(&mut self, phase: crate::events::BuildPhase) -> Result<(), ConfctlError> {
        use crate::events::BuildPhase;
        match phase {
            BuildPhase::Begin => {
                if self.build_scope.is_some() || !self.config.policy.live {
                    return Err(ConfctlError::EventDelivery(
                        "invalid nested or unavailable build phase".into(),
                    ));
                }
                self.park_painter()?;
                self.build_scope = Some(BuildScope {
                    mode: self.mode,
                    operation: self.config.operation.clone(),
                    first_row: self.model.rows().len(),
                });
                self.mode = PresentationMode::Full;
                self.config.operation = "build".into();
                self.active_view = true;
                self.paint_now()
            }
            BuildPhase::End => {
                let scope = self.build_scope.take().ok_or_else(|| {
                    ConfctlError::EventDelivery("build phase ended without begin".into())
                })?;
                self.park_painter()?;
                let tasks = self.model.finish_scope(scope.first_row);
                let mut parts = Vec::new();
                for row in self.model.checkpoint_rows(&tasks)? {
                    parts.extend(live::summary_parts(
                        row,
                        usize::from(self.dimensions().0.saturating_sub(1)),
                    ));
                }
                self.required(
                    OutputStream::Stdout,
                    plain::styled_diagnostic(&parts, self.config.policy.stdout_color),
                )?;
                self.model.mark_checkpoint(&tasks);
                self.mode = scope.mode;
                self.config.operation = scope.operation;
                self.active_view =
                    self.mode == PresentationMode::Full && self.model.has_active_tasks();
                Ok(())
            }
        }
    }

    async fn command_starting(
        &mut self,
        task: crate::events::TaskId,
        display: String,
    ) -> Result<(), ConfctlError> {
        if !self.parked()
            && let Some(last) = self.last_paint
        {
            // Admission provides bounded backpressure: flush this attempt's
            // announcement before acknowledging the producer's spawn barrier.
            tokio::time::sleep(Duration::from_millis(100).saturating_sub(last.elapsed())).await;
        }
        // A prompt/child may have acquired ownership during the paint-slot wait.
        let state = *self.state.borrow_and_update();
        self.transition(state)?;
        let display = model::sanitize_text(&display).replace(['\n', '\r'], " ");
        let announcement = format!("$ {display}");
        let (columns, rows) = self.dimensions();
        let width = columns.saturating_sub(1);
        let log = self.log_display();
        let wrapped = live::wrap(&announcement, usize::from(width));
        let fits = !self.parked()
            && width > 0
            && wrapped.concat() == announcement
            && announcement.len() <= self.config.tail_line_bytes
            && self.config.tail_rows > 0
            && wrapped.len()
                <= live::tail_capacity(&self.model, &self.frame_info(log.as_deref()), width, rows);
        self.model.command_starting(task, &display, fits);
        self.active_view = true;
        if !fits {
            // Parked/tiny views transfer the safe announcement to existing
            // required-output ownership, without duplicating it in the tail.
            self.required(
                OutputStream::Stdout,
                format!("{announcement}\n").into_bytes(),
            )?;
        }
        self.paint_now()
    }

    fn event(&mut self, event: UiEvent) -> Result<(), ConfctlError> {
        match event {
            UiEvent::CommandFinished { command } => {
                if !self.commands.iter().any(|active| active.id == command) {
                    return Ok(());
                }
                self.commands.retain(|active| active.id != command);
                if self.mode == PresentationMode::CommandSpinner {
                    self.park_painter()?;
                    self.paint_now()?;
                }
                Ok(())
            }
            UiEvent::BuildPhase(phase) => self.build_phase(phase),
            UiEvent::Checkpoint {
                key,
                label,
                state,
                tasks,
            } => self.checkpoint(key, label, state, tasks),
            UiEvent::BuildProgress { .. } if self.mode != PresentationMode::Full => Ok(()),
            UiEvent::TerminalState(state) => {
                self.transition(state)?;
                // This FIFO barrier includes bytes received after watch-state
                // parking but before acquisition reached the event consumer.
                self.replay()?;
                self.flush_outputs()
            }
            UiEvent::PromptFrame { preview, text } => {
                if self.current_state != TerminalState::Prompt
                    || *self.state.borrow() != TerminalState::Prompt
                {
                    return Err(ConfctlError::TerminalBusy);
                }
                validate_frame(&preview)?;
                self.suppress_commands(None);
                for event in preview {
                    let (stream, bytes) = self.formatted(event)?;
                    self.prompt_write(stream, bytes)?;
                }
                let text = if self.mode == PresentationMode::Full {
                    model::sanitize_text(&text)
                } else {
                    text
                };
                self.prompt_write(OutputStream::Stdout, text.into_bytes())
            }
            UiEvent::OperationGroup {
                group,
                description,
                prelude,
            } => {
                validate_frame(&prelude)?;
                if self.seen_groups.contains(&group) {
                    return Ok(());
                }
                if !prelude.is_empty() {
                    self.suppress_commands(None);
                }
                if self.build_scope.is_none() {
                    self.finalize_group()?;
                } else {
                    self.park_painter()?;
                }
                for event in prelude {
                    let (stream, bytes) = self.formatted(event)?;
                    self.required(stream, bytes)?;
                }
                self.seen_groups.insert(group);
                self.config.operation =
                    model::sanitize_text(&description).replace(['\n', '\r'], " ");
                if self.build_scope.is_none() {
                    self.model =
                        model::Model::new(self.config.tail_rows, self.config.tail_line_bytes);
                }
                if let Some(path) = &self.config.invocation_log {
                    self.model.apply(&UiEvent::LogAvailable {
                        task: crate::events::TaskId(0),
                        path: path.clone(),
                    })?;
                }
                Ok(())
            }
            UiEvent::Output {
                task,
                stream,
                bytes,
            } => {
                if !bytes.is_empty() {
                    self.suppress_commands(Some(task));
                }
                let bytes = if self.mode == PresentationMode::Full {
                    self.model
                        .sanitize_output(task, stream, &bytes)
                        .into_bytes()
                } else {
                    bytes
                };
                self.required(stream, bytes)
            }
            UiEvent::PlainOutput { stream, bytes } => {
                if self.mode == PresentationMode::Full {
                    Ok(())
                } else {
                    if !bytes.is_empty() {
                        self.suppress_commands(None);
                    }
                    self.required(stream, bytes)
                }
            }
            UiEvent::ActivityOutput {
                task,
                stream,
                bytes,
            } => {
                if self.mode == PresentationMode::Full {
                    self.model.output(task, stream, &bytes);
                    self.active_view = (!self.model.finalized() && self.model.rows().is_empty())
                        || self.model.has_active_tasks();
                    Ok(())
                } else {
                    if !bytes.is_empty() {
                        self.suppress_commands(Some(task));
                    }
                    self.required(stream, bytes)
                }
            }
            event @ UiEvent::CommandStarting { command, task, .. } => {
                if let UiEvent::CommandStarting { display, .. } = &event {
                    self.admit_command(command, task, display);
                }
                if self.mode == PresentationMode::Full {
                    self.model.apply(&event)?;
                    self.active_view = true;
                    Ok(())
                } else {
                    self.paint_now()
                }
            }
            event @ (UiEvent::Table { .. }
            | UiEvent::Diagnostic { .. }
            | UiEvent::StyledDiagnostic { .. }) => {
                let (stream, bytes) = self.formatted(event)?;
                if !bytes.is_empty() {
                    self.suppress_commands(None);
                }
                self.required(stream, bytes)
            }
            event => {
                self.model.apply(&event)?;
                self.active_view = (!self.model.finalized() && self.model.rows().is_empty())
                    || self.model.has_active_tasks();
                Ok(())
            }
        }
    }

    fn checkpoint(
        &mut self,
        key: crate::events::TaskId,
        label: String,
        state: crate::events::TaskState,
        tasks: Vec<crate::events::TaskId>,
    ) -> Result<(), ConfctlError> {
        if self.mode != PresentationMode::Full || self.seen_checkpoints.contains(&key) {
            return Ok(());
        }
        let rows = self.model.checkpoint_rows(&tasks)?;
        let mut parts = vec![
            (
                crate::events::Style::Heading,
                model::sanitize_text(&label).replace(['\n', '\r'], " "),
            ),
            (crate::events::Style::Plain, ": ".into()),
            (
                live::result_style(state),
                format!("{}\n", live::state_label(state)),
            ),
        ];
        let columns = usize::from(self.dimensions().0.saturating_sub(1));
        for row in rows {
            parts.extend(live::summary_parts(row, columns));
        }
        let bytes = plain::styled_diagnostic(&parts, self.config.policy.stdout_color);
        self.required(OutputStream::Stdout, bytes)?;
        // Delivery has transferred to the normal deferred-output owner. Keep
        // authoritative results, but retire only this unit's repeated detail.
        self.seen_checkpoints.insert(key);
        self.model.mark_checkpoint(&tasks);
        self.active_view = self.model.has_active_tasks();
        self.paint_now()
    }

    fn formatted(&self, event: UiEvent) -> Result<(OutputStream, Vec<u8>), ConfctlError> {
        match event {
            UiEvent::Diagnostic {
                stream,
                style,
                text,
            } => {
                let text = if self.mode == PresentationMode::Full {
                    model::sanitize_text(&text)
                } else {
                    text
                };
                Ok((
                    stream,
                    plain::diagnostic(&text, style, self.config.policy.color(stream)),
                ))
            }
            UiEvent::StyledDiagnostic { stream, mut parts } => {
                if self.mode == PresentationMode::Full {
                    for (_, text) in &mut parts {
                        *text = model::sanitize_text(text);
                    }
                }
                Ok((
                    stream,
                    plain::styled_diagnostic(&parts, self.config.policy.color(stream)),
                ))
            }
            UiEvent::Table {
                stream,
                mut columns,
                mut rows,
                cell_styles,
                format,
            } => {
                if self.mode == PresentationMode::Full {
                    columns = columns
                        .into_iter()
                        .map(|value| model::sanitize_text(&value))
                        .collect();
                    rows = rows
                        .into_iter()
                        .map(|row| {
                            row.into_iter()
                                .map(|value| model::sanitize_text(&value))
                                .collect()
                        })
                        .collect();
                }
                let bytes = plain::styled_table(
                    &columns,
                    &rows,
                    &cell_styles,
                    &format,
                    self.config.policy.color(stream),
                )?;
                Ok((stream, bytes))
            }
            _ => Err(ConfctlError::EventDelivery(
                "prompt/group frame contains a non-presentation event".into(),
            )),
        }
    }

    fn stream(&mut self, stream: OutputStream) -> &mut DeferredStream {
        match stream {
            OutputStream::Stdout => &mut self.stdout,
            OutputStream::Stderr => &mut self.stderr,
        }
    }
    fn parked(&self) -> bool {
        matches!(
            self.current_state,
            TerminalState::Prompt | TerminalState::ExternalChild
        )
    }

    fn required(&mut self, stream: OutputStream, bytes: Vec<u8>) -> Result<(), ConfctlError> {
        if bytes.is_empty() {
            return Ok(());
        }
        if !self.parked() {
            self.park_painter()?;
        }
        let spool_offset = self.stream(stream).length;
        if let Err(source) = self.stream(stream).append(&bytes) {
            self.unspooled = Some(UnspooledOutput {
                stream,
                bytes: Captured::Memory(bytes),
                spool_offset,
            });
            return Err(output_io("append deferred output", source));
        }
        if !self.parked() {
            self.replay()?;
        }
        Ok(())
    }

    fn prompt_write(&mut self, stream: OutputStream, bytes: Vec<u8>) -> Result<(), ConfctlError> {
        self.suppress_commands(None);
        self.compact_anchor = false;
        let result = lock_writer(self.outputs.writer(stream)).and_then(|mut writer| {
            writer.write_all(&bytes)?;
            writer.flush()
        });
        if let Err(source) = result {
            let spool_offset = self.stream(stream).length;
            self.unspooled = Some(UnspooledOutput {
                stream,
                bytes: Captured::Memory(bytes),
                spool_offset,
            });
            return Err(output_io("write prompt frame", source));
        }
        Ok(())
    }

    fn replay(&mut self) -> Result<(), ConfctlError> {
        let mut errors = Vec::new();
        for (spool, writer, operation, stream) in [
            (
                &mut self.stdout,
                &self.outputs.stdout,
                "replay deferred stdout",
                OutputStream::Stdout,
            ),
            (
                &mut self.stderr,
                &self.outputs.stderr,
                "replay deferred stderr",
                OutputStream::Stderr,
            ),
        ] {
            match spool.replay(writer) {
                Ok(Some(last)) => {
                    // Only stderr can establish the spinner's stderr anchor.
                    // A stdout LF may have gone to a file; unfinished stdout
                    // still conservatively invalidates a possibly shared TTY.
                    if stream == OutputStream::Stderr || last != b'\n' {
                        self.compact_anchor = last == b'\n';
                    }
                    if let Some(painter) = &mut self.painter {
                        painter.output_ended(last);
                    }
                }
                Ok(None) => {}
                Err(source) => errors.push(output_io(operation, source)),
            }
        }
        combine(errors)
    }
    fn flush_outputs(&self) -> Result<(), ConfctlError> {
        let mut errors = Vec::new();
        for stream in [OutputStream::Stdout, OutputStream::Stderr] {
            if let Err(source) =
                lock_writer(self.outputs.writer(stream)).and_then(|mut output| output.flush())
            {
                errors.push(output_io("flush presentation output", source));
            }
        }
        combine(errors)
    }
    fn transition(&mut self, state: TerminalState) -> Result<(), ConfctlError> {
        let was_parked = self.parked();
        self.current_state = state;
        if self.parked() {
            self.suppress_commands(None);
            self.park_painter()?;
            self.flush_outputs()?;
        } else if was_parked {
            self.compact_anchor = false;
            if let Some(painter) = &mut self.painter {
                painter.foreign_output();
            }
            self.replay()?;
        }
        Ok(())
    }
    fn park_painter(&mut self) -> Result<(), ConfctlError> {
        self.refresh_geometry();
        if let Some(painter) = &mut self.painter {
            painter
                .park()
                .map_err(|source| output_io("park live presentation", source))?;
        }
        if let Some(painter) = &mut self.spinner_painter {
            painter
                .park()
                .map_err(|source| output_io("park command spinner", source))?;
        }
        Ok(())
    }
    fn release_painter(&mut self) -> Result<(), ConfctlError> {
        let mut errors = Vec::new();
        if let Some(mut painter) = self.spinner_painter.take()
            && let Err(source) = painter.finish()
        {
            errors.push(output_io("release command spinner", source));
        }
        if let Some(mut painter) = self.painter.take()
            && let Err(source) = painter.finish()
        {
            errors.push(output_io("release live presentation", source));
        }
        combine(errors)
    }
    fn dimensions(&self) -> (u16, u16) {
        self.config
            .terminal
            .as_ref()
            .and_then(|fd| rustix::termios::tcgetwinsize(fd).ok())
            .map(|size| (size.ws_col, size.ws_row))
            .unwrap_or(self.config.initial_size)
    }
    fn refresh_geometry(&mut self) {
        let size = self.dimensions();
        if self.last_geometry.is_some_and(|previous| previous != size)
            && let Some(painter) = &mut self.painter
        {
            painter.invalidate();
        }
        if self.last_geometry.is_some_and(|previous| previous != size)
            && self.mode == PresentationMode::CommandSpinner
        {
            self.suppress_commands(None);
            self.compact_anchor = false;
        }
        self.last_geometry = Some(size);
    }
    fn log_display(&self) -> Option<String> {
        self.model.logs().last().map(|(_, path)| {
            let relative = self
                .config
                .configuration_dir
                .as_ref()
                .and_then(|root| path.strip_prefix(root).ok());
            display_path(relative.unwrap_or(path))
        })
    }
    fn frame_info<'a>(&'a self, log: Option<&'a str>) -> live::FrameInfo<'a> {
        live::FrameInfo {
            command: &self.config.command,
            operation: &self.config.operation,
            started: &self.started,
            log,
            elapsed: self
                .timing
                .as_ref()
                .map_or_else(|| self.origin.elapsed(), Timing::elapsed),
            color: self.config.policy.stdout_color,
            max_tasks: self.config.max_tasks,
        }
    }
    fn frame(&self, log: Option<&str>) -> ratatui::buffer::Buffer {
        let (columns, rows) = self.dimensions();
        live::render(
            &self.model,
            &self.frame_info(log),
            columns.saturating_sub(1),
            rows,
        )
    }
    fn paint(&mut self) -> Result<(), ConfctlError> {
        if self
            .last_paint
            .is_some_and(|last| last.elapsed() < Duration::from_millis(100))
        {
            return Ok(());
        }
        self.paint_now()
    }
    fn paint_now(&mut self) -> Result<(), ConfctlError> {
        if self.mode == PresentationMode::CommandSpinner {
            self.refresh_geometry();
            if !self.parked() && self.transient_active() {
                let selected = self
                    .commands
                    .iter()
                    .find(|command| !command.suppressed)
                    .expect("active spinner command");
                let count = self.commands.len();
                let suffix = if count > 1 {
                    format!(
                        " (+{}{} running)",
                        (count - 1).min(999),
                        if count > 1000 { "+" } else { "" }
                    )
                } else {
                    String::new()
                };
                let columns = usize::from(self.dimensions().0.saturating_sub(1));
                let line = format!(
                    "{} {}{}",
                    live::spinner(self.origin.elapsed()),
                    selected.display,
                    suffix
                );
                let frame = ratatui::buffer::Buffer::with_lines([model::clip(&line, columns)]);
                if let Some(painter) = &mut self.spinner_painter {
                    painter
                        .paint(&frame)
                        .map_err(|source| output_io("paint command spinner", source))?;
                }
                self.last_paint = Some(Instant::now());
            } else if !self.parked()
                && let Some(painter) = &mut self.spinner_painter
            {
                painter
                    .park()
                    .map_err(|source| output_io("clear suppressed command spinner", source))?;
            }
            return Ok(());
        }
        if self.mode != PresentationMode::Full || self.parked() || !self.active_view {
            return Ok(());
        }
        self.refresh_geometry();
        let mut log = if self.model.finalized() {
            None
        } else {
            self.log_display()
        };
        if let Some(display) = &log {
            let (columns, rows) = self.dimensions();
            // Tiny panels cannot hold an entire wrapped locator plus useful
            // status. Publish it once in normal scrollback, never an ellipsis.
            if columns <= 2
                || !live::headers_fit(
                    &self.model,
                    &self.frame_info(log.as_deref()),
                    columns.saturating_sub(1),
                    rows,
                )
            {
                let path = self
                    .model
                    .logs()
                    .last()
                    .map(|(_, p)| p.clone())
                    .expect("log display has path");
                if !self.scrollback_logs.contains(&path) {
                    self.required(
                        OutputStream::Stdout,
                        plain::styled_diagnostic(
                            &[
                                (crate::events::Style::Heading, "Full log: ".into()),
                                (crate::events::Style::Generation, format!("{display}\n")),
                            ],
                            self.config.policy.stdout_color,
                        ),
                    )?;
                    self.scrollback_logs.insert(path);
                }
                log = None;
            }
        }
        let frame = self.frame(log.as_deref());
        if let Some(painter) = &mut self.painter {
            painter
                .paint(&frame)
                .map_err(|source| output_io("paint live presentation", source))?;
        }
        self.last_paint = Some(Instant::now());
        Ok(())
    }
    fn finalize_group(&mut self) -> Result<(), ConfctlError> {
        self.model.finalize();
        self.park_painter()?;
        if self.mode != PresentationMode::Full {
            return Ok(());
        }
        let columns = usize::from(self.dimensions().0.saturating_sub(1));
        let tasks: Vec<_> = self
            .model
            .rows()
            .iter()
            .filter(|row| !self.model.was_snapshotted(row.id))
            .map(|row| row.id)
            .collect();
        let mut parts = Vec::new();
        for row in self.model.checkpoint_rows(&tasks)? {
            parts.extend(live::summary_parts(row, columns));
        }
        let bytes = plain::styled_diagnostic(&parts, self.config.policy.stdout_color);
        self.required(OutputStream::Stdout, bytes)?;
        self.model.mark_checkpoint(&tasks);
        self.active_view = false;
        // Native IDs and log ownership remain in the model/log/evidence. Final
        // retained locators are printed by the app only after retention succeeds.
        Ok(())
    }
    fn pending(&mut self) -> PendingOutput {
        PendingOutput {
            stdout_replayed: self.stdout.replayed,
            stderr_replayed: self.stderr.replayed,
            stdout: self.stdout.capture(),
            stderr: self.stderr.capture(),
            verified_log: None,
            unspooled: self.unspooled.take(),
        }
    }
}

/// A display locator only: native path bytes remain in the model unchanged.
pub(crate) fn display_path(path: &Path) -> String {
    use std::os::unix::ffi::OsStrExt;
    let mut display = String::from("\"");
    let mut bytes = path.as_os_str().as_bytes();
    while !bytes.is_empty() {
        let (text, invalid) = match std::str::from_utf8(bytes) {
            Ok(text) => (text, 0),
            Err(error) => (
                std::str::from_utf8(&bytes[..error.valid_up_to()])
                    .expect("validated path UTF-8 prefix"),
                error
                    .error_len()
                    .unwrap_or(bytes.len() - error.valid_up_to()),
            ),
        };
        for character in text.chars() {
            match character {
                '"' => display.push_str("\\\""),
                '\\' => display.push_str("\\\\"),
                '\n' => display.push_str("\\n"),
                '\r' => display.push_str("\\r"),
                '\t' => display.push_str("\\t"),
                character if character.is_ascii_control() => {
                    display.push_str(&format!("\\x{:02x}", u32::from(character)));
                }
                character
                    if character.is_control()
                        || matches!(character, '\u{061c}' | '\u{200e}' | '\u{200f}'
                            | '\u{2028}'..='\u{202e}' | '\u{2066}'..='\u{2069}') =>
                {
                    display.extend(character.escape_unicode());
                }
                character => display.push(character),
            }
        }
        bytes = &bytes[text.len()..];
        for byte in &bytes[..invalid] {
            display.push_str(&format!("\\x{byte:02x}"));
        }
        bytes = &bytes[invalid..];
    }
    display.push('"');
    display
}

impl Drop for Presentation {
    fn drop(&mut self) {
        if !self.cleaned {
            self.receiver.close();
            let primary = ConfctlError::EventDelivery(
                "presentation loop dropped before checked cleanup".into(),
            );
            let secondary = self.release_painter().err().into_iter().collect();
            let pending = self.pending();
            self.completion.retain(ConfctlError::OutputFailure {
                source: Box::new(primary.with_secondary(secondary)),
                pending: Box::new(pending),
            });
            self.completion.done.send_replace(true);
        }
    }
}

async fn resize_notification(resize: &mut Option<tokio::signal::unix::Signal>) {
    match resize {
        Some(signal) => {
            signal.recv().await;
        }
        None => std::future::pending::<()>().await,
    }
}
fn validate_frame(events: &[UiEvent]) -> Result<(), ConfctlError> {
    for event in events {
        match event {
            UiEvent::Table {
                columns,
                rows,
                cell_styles,
                ..
            } => plain::validate_cell_styles(columns, rows, cell_styles)?,
            UiEvent::Diagnostic { .. } | UiEvent::StyledDiagnostic { .. } => {}
            _ => {
                return Err(ConfctlError::EventDelivery(
                    "prompt/group frame contains a non-presentation event".into(),
                ));
            }
        }
    }
    Ok(())
}
fn combine(mut errors: Vec<ConfctlError>) -> Result<(), ConfctlError> {
    if errors.is_empty() {
        Ok(())
    } else {
        Err(errors.remove(0).with_secondary(errors))
    }
}

#[cfg(test)]
mod tests;
