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

pub struct PresentationConfig {
    pub command: String,
    pub operation: String,
    pub spool_dir: PathBuf,
    pub policy: OutputPolicy,
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
        let terminal = stdout.as_fd().try_clone_to_owned().ok();
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
        Self {
            command,
            operation,
            spool_dir,
            policy,
            initial_size: dimensions.unwrap_or((80, 24)),
            terminal,
            max_tasks: 5,
            tail_rows: 100,
            tail_line_bytes: 4096,
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

    fn replay(&mut self, writer: &OutputWriter) -> io::Result<()> {
        if self.replayed == self.length {
            return Ok(());
        }
        let file = self
            .file
            .as_mut()
            .expect("owned transfer spool")
            .as_file_mut();
        file.seek(SeekFrom::Start(self.replayed))?;
        let mut output = lock_writer(writer)?;
        let mut bytes = [0; 16 * 1024];
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
        Ok(())
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
    resize: Option<tokio::signal::unix::Signal>,
    finish: watch::Receiver<bool>,
    completion: Arc<Completion>,
    cleaned: bool,
    active_view: bool,
    seen_groups: HashSet<u64>,
    last_geometry: Option<(u16, u16)>,
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
        let resize = if config.policy.live {
            Some(
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::window_change())
                    .map_err(|source| output_io("observe terminal resize", source))?,
            )
        } else {
            None
        };
        let started = timing
            .as_ref()
            .map(|timing| timing.started().to_rfc3339())
            .unwrap_or_else(|| chrono::Local::now().to_rfc3339());
        let model = model::Model::new(config.tail_rows, config.tail_line_bytes);
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
                config,
                timing,
                started,
                origin: Instant::now(),
                model,
                stdout,
                stderr,
                unspooled: None,
                painter,
                resize,
                finish: finish_requests,
                completion,
                cleaned: false,
                active_view: false,
                seen_groups: HashSet::new(),
                last_geometry: None,
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
                    if let Some(delivery) = delivery { self.deliver(delivery)?; }
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
                    self.paint()?;
                },
                _ = timer.tick(), if self.config.policy.live && !self.parked() && self.active_view => { self.paint()?; },
            }
        }
        // Explicit closure rejects blocked/new senders while queued required
        // events are still processed, rather than waiting for every clone.
        self.receiver.close();
        while let Some(delivery) = self.receiver.receive().await {
            self.deliver(delivery)?;
        }
        self.current_state = *self.state.borrow();
        if self.parked() {
            return Err(ConfctlError::TerminalBusy);
        }
        self.replay()?;
        self.finalize_group()?;
        self.flush_outputs()
    }

    fn deliver(&mut self, delivery: crate::events::Delivery) -> Result<(), ConfctlError> {
        let (event, acknowledgement) = delivery.into_parts();
        let result = self.event(event);
        acknowledgement.acknowledge(result.as_ref().map(|_| ()).map_err(ToString::to_string));
        result
    }

    fn event(&mut self, event: UiEvent) -> Result<(), ConfctlError> {
        match event {
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
                for event in preview {
                    let (stream, bytes) = self.formatted(event)?;
                    self.prompt_write(stream, bytes)?;
                }
                let text = if self.config.policy.live {
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
                self.finalize_group()?;
                for event in prelude {
                    let (stream, bytes) = self.formatted(event)?;
                    self.required(stream, bytes)?;
                }
                self.seen_groups.insert(group);
                self.config.operation =
                    model::sanitize_text(&description).replace(['\n', '\r'], " ");
                self.model = model::Model::new(self.config.tail_rows, self.config.tail_line_bytes);
                Ok(())
            }
            UiEvent::Output {
                task,
                stream,
                bytes,
            } => {
                let bytes = if self.config.policy.live {
                    self.model.output(task, stream, &bytes).into_bytes()
                } else {
                    bytes
                };
                self.active_view = true;
                self.required(stream, bytes)
            }
            event @ (UiEvent::Table { .. } | UiEvent::Diagnostic { .. }) => {
                let (stream, bytes) = self.formatted(event)?;
                self.required(stream, bytes)
            }
            event => {
                self.model.apply(&event)?;
                self.active_view = true;
                Ok(())
            }
        }
    }

    fn formatted(&self, event: UiEvent) -> Result<(OutputStream, Vec<u8>), ConfctlError> {
        match event {
            UiEvent::Diagnostic {
                stream,
                style,
                text,
            } => {
                let text = if self.config.policy.live {
                    model::sanitize_text(&text)
                } else {
                    text
                };
                Ok((
                    stream,
                    plain::diagnostic(&text, style, self.config.policy.color(stream)),
                ))
            }
            UiEvent::Table {
                stream,
                mut columns,
                mut rows,
                cell_styles,
                format,
            } => {
                if self.config.policy.live {
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
        let had_bytes =
            self.stdout.replayed < self.stdout.length || self.stderr.replayed < self.stderr.length;
        let mut errors = Vec::new();
        if let Err(source) = self.stdout.replay(&self.outputs.stdout) {
            errors.push(output_io("replay deferred stdout", source));
        }
        if let Err(source) = self.stderr.replay(&self.outputs.stderr) {
            errors.push(output_io("replay deferred stderr", source));
        }
        if had_bytes && let Some(painter) = &mut self.painter {
            painter.invalidate();
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
            self.park_painter()?;
            self.flush_outputs()?;
        } else if was_parked {
            if let Some(painter) = &mut self.painter {
                painter.invalidate();
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
        Ok(())
    }
    fn release_painter(&mut self) -> Result<(), ConfctlError> {
        if let Some(mut painter) = self.painter.take() {
            painter
                .finish()
                .map_err(|source| output_io("release live presentation", source))?;
        }
        Ok(())
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
        self.last_geometry = Some(size);
    }
    fn frame(&self) -> ratatui::buffer::Buffer {
        let (columns, rows) = self.dimensions();
        let log =
            self.model.logs().last().map(|(task, path)| {
                format!("{}: {}", model::source_label(*task), display_path(path))
            });
        let info = live::FrameInfo {
            command: &self.config.command,
            operation: &self.config.operation,
            started: &self.started,
            log: log.as_deref(),
            elapsed: self
                .timing
                .as_ref()
                .map_or_else(|| self.origin.elapsed(), Timing::elapsed),
            color: self.config.policy.stdout_color,
            max_tasks: self.config.max_tasks,
        };
        // A spare terminal column avoids implicit wrapping, including wide cells.
        live::render(
            &self.model,
            &info,
            columns.saturating_sub(1),
            rows.saturating_sub(2),
        )
    }
    fn paint(&mut self) -> Result<(), ConfctlError> {
        if !self.config.policy.live || self.parked() || !self.active_view {
            return Ok(());
        }
        self.refresh_geometry();
        let frame = self.frame();
        if let Some(painter) = &mut self.painter {
            painter
                .paint(&frame)
                .map_err(|source| output_io("paint live presentation", source))?;
        }
        Ok(())
    }
    fn finalize_group(&mut self) -> Result<(), ConfctlError> {
        self.model.finalize();
        if !self.config.policy.live || !self.active_view {
            return Ok(());
        }
        self.paint()?;
        let (columns, rows) = self.dimensions();
        let columns = usize::from(columns.saturating_sub(1));
        let height = rows.saturating_sub(2);
        let headers = usize::from(height.saturating_sub(1)).min(3);
        let minimum_tail = if height >= 8 { 3 } else { 0 };
        let capacity = self
            .config
            .max_tasks
            .min(self.model.rows().len())
            .min(usize::from(height).saturating_sub(headers + minimum_tail));
        let visible: HashSet<_> = if self.parked() {
            HashSet::new()
        } else {
            self.model
                .visible_rows(capacity)
                .iter()
                .map(|row| row.id)
                .collect()
        };
        let omitted: Vec<_> = self
            .model
            .rows()
            .iter()
            .filter(|row| !visible.contains(&row.id) || live::task_label(row, columns).1)
            .map(live::task_summary)
            .collect();
        // Leave final panel in scrollback without destroying the painter object.
        if !self.parked()
            && let Some(painter) = &mut self.painter
        {
            painter
                .finish()
                .map_err(|source| output_io("retain final live panel", source))?;
        }
        self.active_view = false;
        if !omitted.is_empty() {
            self.required(
                OutputStream::Stdout,
                format!("{} tasks not shown in full\n", omitted.len()).into_bytes(),
            )?;
        }
        for line in omitted {
            self.required(OutputStream::Stdout, line.into_bytes())?;
        }
        // Output may precede registration; retain its source without inventing a task.
        let mut sources_seen = HashSet::new();
        let sources: Vec<_> = self
            .model
            .tail_sources()
            .filter(|(task, stream)| {
                !self.model.rows().iter().any(|row| row.id == *task)
                    && sources_seen.insert((*task, *stream == OutputStream::Stderr))
            })
            .map(|(task, stream)| {
                format!(
                    "Output source {}/{} (task {}, {}): no registered task description\n",
                    model::source_label(task),
                    if stream == OutputStream::Stderr {
                        "e"
                    } else {
                        "o"
                    },
                    task.0,
                    stream.name()
                )
            })
            .collect();
        for source in sources {
            self.required(OutputStream::Stdout, source.into_bytes())?;
        }
        let logs: Vec<_> = self
            .model
            .logs()
            .iter()
            .map(|(task, path)| {
                format!(
                    "Log: {} (task {}): {}\n",
                    model::source_label(*task),
                    task.0,
                    display_path(path)
                )
            })
            .collect();
        for log in logs {
            self.required(OutputStream::Stdout, log.into_bytes())?;
        }
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
fn display_path(path: &Path) -> String {
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
            UiEvent::Diagnostic { .. } => {}
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
