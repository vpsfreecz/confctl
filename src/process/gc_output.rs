//! GC-only bounded UI lines; captures and raw logging stay in the normal pumps.
use crate::{
    error::ConfctlError,
    events::{EventSink, OutputStream, TaskId, UiEvent},
};
use std::sync::Arc;
use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;
const LINE_LIMIT: usize = 8192;
#[derive(Default)]
struct Line {
    bytes: Vec<u8>,
    continued: bool,
}
#[derive(Default)]
struct State {
    lines: [Line; 2],
    completed: u64,
    summary: Option<String>,
}
#[derive(Clone)]
pub(crate) struct GcOutput {
    host: String,
    state: Arc<Mutex<State>>,
}
impl GcOutput {
    pub(crate) fn new(host: &str) -> Self {
        Self {
            host: host.into(),
            state: Arc::new(Mutex::new(State::default())),
        }
    }
    pub(crate) async fn summary(&self) -> Option<String> {
        self.state.lock().await.summary.clone()
    }
    pub(crate) async fn forward(
        &self,
        events: &EventSink,
        source: (TaskId, OutputStream),
        bytes: &[u8],
        eof: bool,
        cancel: &CancellationToken,
    ) -> Result<(), ConfctlError> {
        let (task, stream) = source;
        let index = usize::from(stream == OutputStream::Stderr);
        let mut state = tokio::select! { state = self.state.lock() => state, _ = cancel.cancelled() => return Err(ConfctlError::Cancelled) };
        for byte in bytes {
            if *byte == b'\n' {
                self.emit(&mut state, index, events, source, true, cancel)
                    .await?;
            } else {
                if state.lines[index].bytes.len() == LINE_LIMIT {
                    self.emit(&mut state, index, events, source, false, cancel)
                        .await?;
                    state.lines[index].continued = true;
                }
                state.lines[index].bytes.push(*byte);
            }
        }
        if eof && !state.lines[index].bytes.is_empty() {
            self.emit(&mut state, index, events, (task, stream), true, cancel)
                .await?;
        }
        Ok(())
    }
    async fn emit(
        &self,
        state: &mut State,
        index: usize,
        events: &EventSink,
        source: (TaskId, OutputStream),
        complete: bool,
        cancel: &CancellationToken,
    ) -> Result<(), ConfctlError> {
        let (task, stream) = source;
        let line = std::mem::take(&mut state.lines[index].bytes);
        let valid = complete && !state.lines[index].continued;
        let text = valid.then(|| std::str::from_utf8(&line).ok()).flatten();
        let deletion = text
            .and_then(|text| text.strip_prefix("deleting '/nix/store/"))
            .and_then(|text| text.strip_suffix('\''))
            .is_some_and(|path| !path.is_empty() && !path.contains('\''));
        if let Some(text) = text {
            let digits = text.bytes().take_while(u8::is_ascii_digit).count();
            if digits > 0 && text[digits..].starts_with(" store paths deleted") {
                state.summary = Some(text.into());
            }
        }
        let mut output = format!(
            "{}> {}",
            self.host,
            if state.lines[index].continued || !complete {
                "[continued] "
            } else {
                ""
            }
        )
        .into_bytes();
        output.extend(&line);
        output.push(b'\n');
        emit(
            events,
            cancel,
            UiEvent::ActivityOutput {
                task,
                stream,
                bytes: output,
            },
        )
        .await?;
        if deletion {
            state.completed = state
                .completed
                .checked_add(1)
                .ok_or_else(|| ConfctlError::State("GC activity count overflow".into()))?;
            emit(
                events,
                cancel,
                UiEvent::TaskProgress {
                    task,
                    completed: state.completed,
                    total: None,
                    detail: format!("{} store paths observed deleting", state.completed),
                },
            )
            .await?;
        }
        if complete {
            state.lines[index].continued = false;
        }
        Ok(())
    }
}
async fn emit(
    events: &EventSink,
    cancel: &CancellationToken,
    event: UiEvent,
) -> Result<(), ConfctlError> {
    tokio::select! { result = events.emit(event) => result?, _ = cancel.cancelled() => return Err(ConfctlError::Cancelled) }
    if cancel.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}
#[cfg(test)]
mod tests;
