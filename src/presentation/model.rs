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

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TaskPhase {
    Pending,
    Executing,
    /// The display scope ended. None retains a missing authoritative result.
    Final {
        state: Option<TaskState>,
        entered: bool,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskRow {
    pub id: TaskId,
    pub description: String,
    pub completed: u64,
    pub total: Option<u64>,
    pub detail: String,
    pub phase: TaskPhase,
    original_description: String,
}

impl TaskRow {
    /// A zero or unknown total is activity, never a fabricated percentage.
    pub fn fraction(&self) -> Option<f64> {
        self.total
            .filter(|total| *total > 0)
            .map(|total| self.completed.min(total) as f64 / total as f64)
    }
    fn priority(&self) -> u8 {
        match self.phase {
            TaskPhase::Final {
                state: None | Some(TaskState::Failed | TaskState::Incomplete | TaskState::Canceled),
                ..
            } => 0,
            TaskPhase::Pending | TaskPhase::Executing => 1,
            TaskPhase::Final {
                state: Some(TaskState::Skipped),
                ..
            } => 2,
            TaskPhase::Final {
                state: Some(TaskState::Succeeded),
                ..
            } => 3,
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
    pub(super) fn tail_sources(&self) -> impl Iterator<Item = (TaskId, OutputStream)> + '_ {
        self.tail
            .sources
            .iter()
            .copied()
            .chain(self.tail.source.filter(|_| !self.tail.current.is_empty()))
    }

    pub fn apply(&mut self, event: &UiEvent) -> Result<(), ConfctlError> {
        match event {
            UiEvent::TaskPending { task, description } => {
                if self.rows.iter().any(|row| row.id == *task) {
                    return Err(invalid("task registered twice"));
                }
                self.register(*task, description, TaskPhase::Pending);
            }
            UiEvent::TaskStarted { task, description } => {
                if let Some(row) = self.rows.iter_mut().find(|row| row.id == *task) {
                    if row.phase != TaskPhase::Pending || row.original_description != *description {
                        return Err(invalid("task start does not match pending work"));
                    }
                    row.phase = TaskPhase::Executing;
                } else {
                    self.register(*task, description, TaskPhase::Executing);
                }
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
                if row.phase != TaskPhase::Executing {
                    return Err(invalid("progress requires an executing task"));
                }
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
                let entered = match row.phase {
                    TaskPhase::Pending if *state == TaskState::Succeeded => {
                        return Err(invalid("pending task cannot succeed before execution"));
                    }
                    TaskPhase::Pending => false,
                    TaskPhase::Executing => true,
                    TaskPhase::Final {
                        state: Some(previous),
                        ..
                    } if previous == *state => {
                        return Ok(());
                    }
                    TaskPhase::Final { .. } => {
                        return Err(invalid("task has conflicting final results"));
                    }
                };
                row.phase = TaskPhase::Final {
                    state: Some(*state),
                    entered,
                };
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

    fn register(&mut self, id: TaskId, description: &str, phase: TaskPhase) {
        self.rows.push(TaskRow {
            id,
            description: sanitize_text(description),
            original_description: description.to_owned(),
            completed: 0,
            total: None,
            detail: String::new(),
            phase,
        });
    }

    /// End only the observation scope; a missing result stays unknown.
    pub fn finalize(&mut self) {
        for row in &mut self.rows {
            let entered = match row.phase {
                TaskPhase::Pending => false,
                TaskPhase::Executing => true,
                TaskPhase::Final { .. } => continue,
            };
            row.phase = TaskPhase::Final {
                state: None,
                entered,
            };
        }
    }

    /// The same incremental sanitizer feeds reliable live output and the tail.
    /// Captures/logs keep their original input; only this returned view changes.
    pub fn output(&mut self, task: TaskId, stream: OutputStream, bytes: &[u8]) -> String {
        let text = self
            .parsers
            .entry((task.0, stream == OutputStream::Stderr))
            .or_default()
            .feed(bytes);
        self.tail.append(task, stream, &text);
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

/// A reversible, group-independent display key, not a host or registry lookup.
pub(super) fn source_label(task: TaskId) -> String {
    let mut value = task.0;
    let mut digits = Vec::with_capacity(13);
    loop {
        digits.push(char::from(
            b"0123456789abcdefghijklmnopqrstuvwxyz"[(value % 36) as usize],
        ));
        value /= 36;
        if value == 0 {
            break;
        }
    }
    format!("@{}", digits.into_iter().rev().collect::<String>())
}

struct Tail {
    lines: VecDeque<String>,
    sources: VecDeque<(TaskId, OutputStream)>,
    current: String,
    content: String,
    source: Option<(TaskId, OutputStream)>,
    prefix: String,
    rows: usize,
    line_bytes: usize,
}
impl Tail {
    fn new(rows: usize, line_bytes: usize) -> Self {
        Self {
            lines: VecDeque::new(),
            sources: VecDeque::new(),
            current: String::new(),
            content: String::new(),
            source: None,
            prefix: String::new(),
            rows,
            line_bytes,
        }
    }
    fn append(&mut self, task: TaskId, stream: OutputStream, text: &str) {
        if text.is_empty() {
            return;
        }
        if self.source != Some((task, stream)) {
            // Keep interleaved partial lines distinct without changing output bytes.
            if !self.current.is_empty() {
                self.finish_line();
            }
            self.source = Some((task, stream));
            self.prefix = format!(
                "{}/{} ",
                source_label(task),
                if stream == OutputStream::Stderr {
                    "e"
                } else {
                    "o"
                }
            );
        }
        for part in text.split_inclusive(['\n', '\r']) {
            let (value, ending) = part
                .strip_suffix(['\n', '\r'])
                .map_or((part, None), |value| {
                    (value, part.as_bytes().last().copied())
                });
            self.content.push_str(value);
            trim_front(&mut self.content, self.line_bytes);
            self.current.clone_from(&self.prefix);
            self.current.push_str(&self.content);
            match ending {
                Some(b'\n') => self.finish_line(),
                Some(b'\r') => {
                    self.current.clear();
                    self.content.clear();
                }
                _ => {}
            }
        }
    }
    fn finish_line(&mut self) {
        if self.rows > 0 {
            self.lines.push_back(std::mem::take(&mut self.current));
            self.sources
                .push_back(self.source.expect("display fragment has a source"));
        } else {
            self.current.clear();
        }
        self.content.clear();
        while self.lines.len() > self.rows {
            self.lines.pop_front();
            self.sources.pop_front();
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
    fn pending(id: u64) -> UiEvent {
        UiEvent::TaskPending {
            task: TaskId(id),
            description: format!("task{id}"),
        }
    }
    #[test]
    fn pending_promotion_keeps_identity_and_order_beside_direct_execution() {
        let mut model = Model::new(2, 40);
        model.apply(&pending(9)).unwrap();
        model.apply(&start(2)).unwrap();
        assert_eq!(model.rows()[0].phase, TaskPhase::Pending);
        assert_eq!(model.rows()[1].phase, TaskPhase::Executing);
        assert_eq!(model.rows()[0].fraction(), None);
        model.output(TaskId(9), OutputStream::Stderr, b"notice\n");
        assert_eq!(model.rows()[0].phase, TaskPhase::Pending);
        model.apply(&start(9)).unwrap();
        assert_eq!(model.rows()[0].phase, TaskPhase::Executing);
        assert_eq!(model.rows()[0].id, TaskId(9));
        assert_eq!(model.rows()[1].id, TaskId(2));
    }
    #[test]
    fn preentry_results_are_explicit_and_identical_results_are_idempotent() {
        for state in [
            TaskState::Skipped,
            TaskState::Canceled,
            TaskState::Failed,
            TaskState::Incomplete,
        ] {
            let mut model = Model::new(2, 40);
            model.apply(&pending(1)).unwrap();
            let result = UiEvent::TaskFinished {
                task: TaskId(1),
                state,
            };
            model.apply(&result).unwrap();
            model.apply(&result).unwrap();
            assert_eq!(model.rows().len(), 1);
            assert_eq!(
                model.rows()[0].phase,
                TaskPhase::Final {
                    state: Some(state),
                    entered: false
                }
            );
        }
        let mut model = Model::new(2, 40);
        model.apply(&start(1)).unwrap();
        let result = UiEvent::TaskFinished {
            task: TaskId(1),
            state: TaskState::Succeeded,
        };
        model.apply(&result).unwrap();
        model.apply(&result).unwrap();
        assert_eq!(
            model.rows()[0].phase,
            TaskPhase::Final {
                state: Some(TaskState::Succeeded),
                entered: true
            }
        );
    }
    #[test]
    fn invalid_lifecycle_events_do_not_reset_or_advance_a_task() {
        let mut model = Model::new(2, 40);
        model.apply(&pending(1)).unwrap();
        let before = model.rows().to_vec();
        for event in [
            pending(1),
            UiEvent::TaskStarted {
                task: TaskId(1),
                description: "\x1b[31mtask1".into(),
            },
            UiEvent::TaskProgress {
                task: TaskId(1),
                completed: 0,
                total: Some(5),
                detail: "waiting".into(),
            },
            UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Succeeded,
            },
        ] {
            assert!(model.apply(&event).is_err(), "{event:?}");
            assert_eq!(model.rows(), before);
        }
        model.apply(&start(1)).unwrap();
        assert!(model.apply(&start(1)).is_err());
        assert!(model.apply(&pending(1)).is_err());
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Failed,
            })
            .unwrap();
        let final_row = model.rows().to_vec();
        for event in [
            start(1),
            pending(1),
            UiEvent::TaskProgress {
                task: TaskId(1),
                completed: 5,
                total: Some(5),
                detail: "done".into(),
            },
            UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Succeeded,
            },
        ] {
            assert!(model.apply(&event).is_err(), "{event:?}");
            assert_eq!(model.rows(), final_row);
        }
    }
    #[test]
    fn scope_end_keeps_missing_results_unknown_with_observed_entry_phase() {
        let mut model = Model::new(2, 40);
        model.apply(&pending(1)).unwrap();
        model.apply(&start(2)).unwrap();
        model.finalize();
        assert_eq!(
            model.rows()[0].phase,
            TaskPhase::Final {
                state: None,
                entered: false
            }
        );
        assert_eq!(
            model.rows()[1].phase,
            TaskPhase::Final {
                state: None,
                entered: true
            }
        );
        let final_rows = model.rows().to_vec();
        model.finalize();
        assert_eq!(model.rows(), final_rows);
        assert!(
            model
                .apply(&UiEvent::TaskFinished {
                    task: TaskId(2),
                    state: TaskState::Succeeded
                })
                .is_err()
        );
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
            model.rows()[1].phase == TaskPhase::Executing,
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
            ["@1/o red 界", "@1/o last"]
        );
        assert_eq!(bytes[4], 0x1b, "raw source is unchanged");
        model
            .apply(&UiEvent::Output {
                task: TaskId(1),
                stream: OutputStream::Stdout,
                bytes: vec![b'x'; 10_000],
            })
            .expect("long line");
        assert_eq!(model.current_tail_line(), "@1/o xxxxxxxxxxxxxxxx");
    }
    #[test]
    fn interleaved_partial_lines_keep_task_and_stream_without_changing_output() {
        let mut model = Model::new(3, 16);
        for (task, stream, bytes, returned) in [
            (1, OutputStream::Stdout, &b"node-a "[..], "node-a "),
            (2, OutputStream::Stderr, &b"\x1b[31mfailed"[..], "failed"),
            (1, OutputStream::Stderr, &b"warn\n"[..], "warn\n"),
            (1, OutputStream::Stdout, &b"continued"[..], "continued"),
        ] {
            assert_eq!(model.output(TaskId(task), stream, bytes), returned);
        }
        assert_eq!(
            model.tail().iter().map(String::as_str).collect::<Vec<_>>(),
            ["@1/o node-a ", "@2/e failed", "@1/e warn",]
        );
        assert_eq!(model.current_tail_line(), "@1/o continued");
        assert_eq!(
            model.tail_sources().collect::<Vec<_>>(),
            [
                (TaskId(1), OutputStream::Stdout),
                (TaskId(2), OutputStream::Stderr),
                (TaskId(1), OutputStream::Stderr),
                (TaskId(1), OutputStream::Stdout),
            ]
        );
        assert_eq!(
            model.output(TaskId(2), OutputStream::Stderr, b"\x1b[0m"),
            ""
        );
        assert_eq!(model.current_tail_line(), "@1/o continued");
        assert_eq!(
            model.output(TaskId(1), OutputStream::Stdout, b"\rreplacement\n"),
            "\rreplacement\n"
        );
        assert_eq!(model.tail().len(), 3);
        assert_eq!(
            model.tail().back().map(String::as_str),
            Some("@1/o replacement")
        );
        assert_eq!(
            model.tail_sources().collect::<Vec<_>>(),
            [
                (TaskId(2), OutputStream::Stderr),
                (TaskId(1), OutputStream::Stderr),
                (TaskId(1), OutputStream::Stdout),
            ]
        );
    }
    #[test]
    fn compact_source_keys_retain_the_entire_u64_identity() {
        for (id, key) in [
            (0, "@0"),
            (35, "@z"),
            (36, "@10"),
            (u64::MAX - 1, "@3w5e11264sgse"),
            (u64::MAX, "@3w5e11264sgsf"),
        ] {
            assert_eq!(source_label(TaskId(id)), key);
        }
    }
    #[test]
    fn narrow_clipping_keeps_graphemes_and_display_width() {
        assert_eq!(clip("e\u{301}界tail", 2), "e\u{301}");
        assert_eq!(clip("e\u{301}界tail", 3), "e\u{301}界");
        assert_eq!(clip("界", 1), "");
        assert_eq!(clip("anything", 0), "");
    }
}
