//! Ordered task state and bounded display text. Neither decides child success.

use crate::{
    error::ConfctlError,
    events::{OutputStream, TaskId, TaskState, UiEvent},
};
use ratatui::{style::Style, text::Span};
use std::{
    collections::{HashMap, VecDeque},
    path::PathBuf,
};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskRow {
    pub id: TaskId,
    pub description: String,
    pub completed: u64,
    pub total: Option<u64>,
    pub detail: String,
    pub state: Option<TaskState>,
}

impl TaskRow {
    /// A zero or unknown total is activity, never a fabricated percentage.
    pub fn fraction(&self) -> Option<f64> {
        self.total
            .filter(|total| *total > 0)
            .map(|total| self.completed.min(total) as f64 / total as f64)
    }
    fn priority(&self) -> u8 {
        match self.state {
            Some(TaskState::Failed | TaskState::Incomplete | TaskState::Canceled) => 0,
            None => 1,
            Some(TaskState::Skipped) => 2,
            Some(TaskState::Succeeded) => 3,
        }
    }
}

pub struct Model {
    rows: Vec<TaskRow>,
    logs: Vec<(TaskId, PathBuf)>,
    tail: Tail,
    parsers: HashMap<(u64, bool), Sanitizer>,
}

impl Model {
    pub fn new(tail_rows: usize, tail_line_bytes: usize) -> Self {
        Self {
            rows: Vec::new(),
            logs: Vec::new(),
            tail: Tail::new(tail_rows, tail_line_bytes),
            parsers: HashMap::new(),
        }
    }
    pub fn rows(&self) -> &[TaskRow] {
        &self.rows
    }
    pub fn logs(&self) -> &[(TaskId, PathBuf)] {
        &self.logs
    }
    pub fn tail(&self) -> &VecDeque<String> {
        &self.tail.lines
    }
    pub fn current_tail_line(&self) -> &str {
        &self.tail.current
    }

    pub fn apply(&mut self, event: &UiEvent) -> Result<(), ConfctlError> {
        match event {
            UiEvent::TaskStarted { task, description } => {
                if self.rows.iter().any(|row| row.id == *task) {
                    return Err(invalid("task registered twice"));
                }
                self.rows.push(TaskRow {
                    id: *task,
                    description: sanitize_text(description),
                    completed: 0,
                    total: None,
                    detail: String::new(),
                    state: None,
                });
            }
            UiEvent::TaskProgress {
                task,
                completed,
                total,
                detail,
            } => {
                let row = self
                    .rows
                    .iter_mut()
                    .find(|row| row.id == *task)
                    .ok_or_else(|| invalid("progress for unregistered task"))?;
                row.completed = *completed;
                row.total = *total;
                row.detail = sanitize_text(detail);
            }
            UiEvent::TaskFinished { task, state } => {
                let row = self
                    .rows
                    .iter_mut()
                    .find(|row| row.id == *task)
                    .ok_or_else(|| invalid("result for unregistered task"))?;
                if row.state.is_some_and(|previous| previous != *state) {
                    return Err(invalid("task has conflicting final results"));
                }
                row.state = Some(*state);
            }
            UiEvent::Output {
                task,
                stream,
                bytes,
            } => {
                self.output(*task, *stream, bytes);
            }
            UiEvent::LogAvailable { task, path } => {
                if let Some((_, old)) = self.logs.iter_mut().find(|(id, _)| id == task) {
                    *old = path.clone();
                } else {
                    self.logs.push((*task, path.clone()));
                }
            }
            _ => {}
        }
        Ok(())
    }

    /// The same incremental sanitizer feeds reliable live output and the tail.
    /// Captures/logs keep their original input; only this returned view changes.
    pub fn output(&mut self, task: TaskId, stream: OutputStream, bytes: &[u8]) -> String {
        let text = self
            .parsers
            .entry((task.0, stream == OutputStream::Stderr))
            .or_default()
            .feed(bytes);
        self.tail.append(&text);
        text
    }

    /// Choose failures/incomplete work first, then restore registration order.
    pub fn visible_rows(&self, limit: usize) -> Vec<&TaskRow> {
        let mut selected: Vec<_> = self.rows.iter().enumerate().collect();
        selected.sort_by_key(|(index, row)| (row.priority(), *index));
        selected.truncate(limit);
        selected.sort_by_key(|(index, _)| *index);
        selected.into_iter().map(|(_, row)| row).collect()
    }
}

fn invalid(message: &str) -> ConfctlError {
    ConfctlError::EventDelivery(message.into())
}

#[derive(Clone, Copy, Default)]
enum Escape {
    #[default]
    Ground,
    Esc,
    Csi,
    Osc,
    OscTerminator,
}

/// Stateful per task/stream so split UTF-8 and escape sequences cannot leak
/// terminal controls. At most three unfinished UTF-8 bytes are retained.
#[derive(Default)]
struct Sanitizer {
    escape: Escape,
    utf8: Vec<u8>,
}

impl Sanitizer {
    fn feed(&mut self, bytes: &[u8]) -> String {
        let mut printable = std::mem::take(&mut self.utf8);
        for byte in bytes {
            match self.escape {
                Escape::Ground => match byte {
                    0x1b => self.escape = Escape::Esc,
                    b'\n' | b'\r' => printable.push(*byte),
                    b'\t' => printable.extend_from_slice(b"    "),
                    0x00..=0x1f | 0x7f => {}
                    _ => printable.push(*byte),
                },
                Escape::Esc => {
                    self.escape = match byte {
                        b'[' => Escape::Csi,
                        b']' => Escape::Osc,
                        _ => Escape::Ground,
                    }
                }
                Escape::Csi => {
                    if (0x40..=0x7e).contains(byte) {
                        self.escape = Escape::Ground;
                    }
                }
                Escape::Osc => match byte {
                    0x07 => self.escape = Escape::Ground,
                    0x1b => self.escape = Escape::OscTerminator,
                    _ => {}
                },
                Escape::OscTerminator => {
                    self.escape = if *byte == b'\\' {
                        Escape::Ground
                    } else {
                        Escape::Osc
                    }
                }
            }
        }
        let mut result = String::new();
        let mut rest = printable.as_slice();
        while !rest.is_empty() {
            match std::str::from_utf8(rest) {
                Ok(text) => {
                    result.push_str(text);
                    break;
                }
                Err(error) => {
                    let valid = error.valid_up_to();
                    result.push_str(
                        std::str::from_utf8(&rest[..valid]).expect("validated UTF-8 prefix"),
                    );
                    if let Some(length) = error.error_len() {
                        result.push('\u{fffd}');
                        rest = &rest[valid + length..];
                    } else {
                        self.utf8.extend_from_slice(&rest[valid..]);
                        break;
                    }
                }
            }
        }
        result.retain(|ch| !ch.is_control() || matches!(ch, '\n' | '\r'));
        result
    }
}

pub fn sanitize_text(text: &str) -> String {
    Sanitizer::default().feed(text.as_bytes())
}

struct Tail {
    lines: VecDeque<String>,
    current: String,
    rows: usize,
    line_bytes: usize,
}
impl Tail {
    fn new(rows: usize, line_bytes: usize) -> Self {
        Self {
            lines: VecDeque::new(),
            current: String::new(),
            rows,
            line_bytes,
        }
    }
    fn append(&mut self, text: &str) {
        for part in text.split_inclusive(['\n', '\r']) {
            let (value, ending) = part
                .strip_suffix(['\n', '\r'])
                .map_or((part, None), |value| {
                    (value, part.as_bytes().last().copied())
                });
            self.current.push_str(value);
            trim_front(&mut self.current, self.line_bytes);
            match ending {
                Some(b'\n') => {
                    if self.rows > 0 {
                        self.lines.push_back(std::mem::take(&mut self.current));
                    } else {
                        self.current.clear();
                    }
                    while self.lines.len() > self.rows {
                        self.lines.pop_front();
                    }
                }
                Some(b'\r') => self.current.clear(),
                _ => {}
            }
        }
    }
}

fn trim_front(text: &mut String, bytes: usize) {
    if text.len() <= bytes {
        return;
    }
    let remove = {
        let span = Span::raw(text.as_str());
        let mut remaining = text.len();
        let mut remove = 0;
        for grapheme in span.styled_graphemes(Style::default()) {
            if remaining <= bytes {
                break;
            }
            remaining -= grapheme.symbol.len();
            remove += grapheme.symbol.len();
        }
        remove
    };
    text.drain(..remove);
}

/// Ratatui's own public grapheme/width API keeps clipping consistent with its
/// widgets without depending on an unqualified second Unicode implementation.
pub fn clip(text: &str, columns: usize) -> String {
    let span = Span::raw(text);
    let mut result = String::new();
    let mut width = 0;
    for grapheme in span.styled_graphemes(Style::default()) {
        let next = Span::raw(grapheme.symbol).width();
        if width + next > columns {
            break;
        }
        result.push_str(grapheme.symbol);
        width += next;
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    fn start(id: u64) -> UiEvent {
        UiEvent::TaskStarted {
            task: TaskId(id),
            description: format!("task{id}"),
        }
    }
    #[test]
    fn registered_order_and_explicit_results_survive_priority_selection() {
        let mut model = Model::new(2, 40);
        for id in 0..5 {
            model.apply(&start(id)).expect("registered");
        }
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(0),
                state: TaskState::Succeeded,
            })
            .expect("success");
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(4),
                state: TaskState::Failed,
            })
            .expect("failure");
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(2),
                state: TaskState::Incomplete,
            })
            .expect("incomplete");
        assert_eq!(
            model
                .visible_rows(3)
                .iter()
                .map(|row| row.id)
                .collect::<Vec<_>>(),
            [TaskId(1), TaskId(2), TaskId(4)]
        );
        assert!(
            model.rows()[1].state.is_none(),
            "zero activity cannot imply success"
        );
        assert!(
            model
                .apply(&UiEvent::TaskFinished {
                    task: TaskId(4),
                    state: TaskState::Succeeded
                })
                .is_err()
        );
    }
    #[test]
    fn positive_totals_clamp_bars_while_zero_and_unknown_remain_counts() {
        let mut model = Model::new(2, 40);
        model.apply(&start(1)).expect("registered");
        for (total, fraction) in [(None, None), (Some(0), None), (Some(5), Some(1.0))] {
            model
                .apply(&UiEvent::TaskProgress {
                    task: TaskId(1),
                    completed: 8,
                    total,
                    detail: "detail".into(),
                })
                .expect("progress");
            assert_eq!(model.rows()[0].fraction(), fraction);
            assert_eq!(model.rows()[0].completed, 8);
        }
    }
    #[test]
    fn split_controls_and_unicode_are_sanitized_only_in_bounded_live_text() {
        let mut model = Model::new(2, 16);
        let bytes = b"old\n\x1b]52;c;secret\x1b\\\x1b[31mred\x1b[0m\x07 \xe7\x95\x8c\nlast\n";
        for chunk in bytes.chunks(2) {
            model
                .apply(&UiEvent::Output {
                    task: TaskId(1),
                    stream: OutputStream::Stdout,
                    bytes: chunk.to_vec(),
                })
                .expect("output");
        }
        assert_eq!(
            model.tail().iter().map(String::as_str).collect::<Vec<_>>(),
            ["red 界", "last"]
        );
        assert_eq!(bytes[4], 0x1b, "raw source is unchanged");
        model
            .apply(&UiEvent::Output {
                task: TaskId(1),
                stream: OutputStream::Stdout,
                bytes: vec![b'x'; 10_000],
            })
            .expect("long line");
        assert_eq!(model.current_tail_line().len(), 16);
    }
    #[test]
    fn narrow_clipping_keeps_graphemes_and_display_width() {
        assert_eq!(clip("e\u{301}界tail", 2), "e\u{301}");
        assert_eq!(clip("e\u{301}界tail", 3), "e\u{301}界");
        assert_eq!(clip("界", 1), "");
        assert_eq!(clip("anything", 0), "");
    }
}
