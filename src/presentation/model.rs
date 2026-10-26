//! Ordered task state and bounded display text. Neither decides child success.

use crate::{
    error::ConfctlError,
    events::{NixActivityProgress, OutputStream, TaskId, TaskState, UiEvent},
};
use ratatui::{style::Style, text::Span};
use std::{
    collections::{HashMap, HashSet, VecDeque},
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
    pub build: Option<(Option<NixActivityProgress>, Option<NixActivityProgress>)>,
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
    commands: Vec<(TaskId, String)>,
    finalized: bool,
    snapshotted: HashSet<TaskId>,
}

impl Model {
    pub fn new(tail_rows: usize, tail_line_bytes: usize) -> Self {
        Self {
            rows: Vec::new(),
            logs: Vec::new(),
            tail: Tail::new(tail_rows, tail_line_bytes),
            parsers: HashMap::new(),
            commands: Vec::new(),
            finalized: false,
            snapshotted: HashSet::new(),
        }
    }
    pub fn rows(&self) -> &[TaskRow] {
        &self.rows
    }
    pub(super) fn was_snapshotted(&self, task: TaskId) -> bool {
        self.snapshotted.contains(&task)
    }

    pub(super) fn checkpoint_rows(&self, tasks: &[TaskId]) -> Result<Vec<&TaskRow>, ConfctlError> {
        let mut selected = HashSet::new();
        let mut rows = Vec::new();
        for task in tasks {
            let row = self
                .rows
                .iter()
                .find(|row| row.id == *task)
                .ok_or_else(|| invalid("checkpoint names an unregistered task"))?;
            if !matches!(row.phase, TaskPhase::Final { .. }) {
                return Err(invalid("checkpoint requires terminal task results"));
            }
            if selected.insert(*task) && !self.was_snapshotted(*task) {
                rows.push(row);
            }
        }
        Ok(rows)
    }

    pub(super) fn has_active_tasks(&self) -> bool {
        self.rows.iter().any(|row| {
            !self.was_snapshotted(row.id)
                && matches!(row.phase, TaskPhase::Pending | TaskPhase::Executing)
        })
    }

    pub(super) fn mark_checkpoint(&mut self, tasks: &[TaskId]) {
        self.snapshotted.extend(tasks.iter().copied());
        self.tail.remove_tasks(tasks);
    }

    /// Close only tasks admitted by the explicit build scope. Other operation
    /// rows and the model's overall observation lifetime remain unchanged.
    pub(super) fn finish_scope(&mut self, first_row: usize) -> Vec<TaskId> {
        let tasks: Vec<_> = self
            .rows
            .iter_mut()
            .skip(first_row)
            .map(|row| {
                row.phase = match row.phase {
                    TaskPhase::Pending => TaskPhase::Final {
                        state: None,
                        entered: false,
                    },
                    TaskPhase::Executing => TaskPhase::Final {
                        state: None,
                        entered: true,
                    },
                    phase => phase,
                };
                row.id
            })
            .collect();
        tasks
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

    pub(super) fn command_starting(&mut self, task: TaskId, display: &str, in_tail: bool) {
        let display = sanitize_text(display).replace(['\n', '\r'], " ");
        self.commands.retain(|(id, _)| *id != task);
        if !self.tail.current.is_empty() {
            self.tail.finish_line();
        }
        if in_tail {
            self.tail.append(
                task,
                OutputStream::Stderr,
                &format!("$ {display}\n"),
                false,
                true,
            );
        }
        self.commands.push((task, display));
    }

    pub fn apply(&mut self, event: &UiEvent) -> Result<(), ConfctlError> {
        match event {
            UiEvent::CommandStarting { task, display, .. } => {
                self.command_starting(*task, display, true)
            }
            UiEvent::BuildProgress {
                task,
                building,
                fetching,
            } => {
                let row = self
                    .rows
                    .iter_mut()
                    .find(|row| row.id == *task)
                    .ok_or_else(|| invalid("build activity for unregistered task"))?;
                if row.phase != TaskPhase::Executing {
                    return Err(invalid("build activity requires an executing task"));
                }
                row.build = Some((*building, *fetching));
            }

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
                if self.tail.source.is_some_and(|(id, _)| id == *task)
                    && !self.tail.current.is_empty()
                {
                    self.tail.finish_line();
                }
            }
            UiEvent::Output {
                task,
                stream,
                bytes,
            }
            | UiEvent::ActivityOutput {
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
            build: None,
        });
    }

    /// End only the observation scope; a missing result stays unknown.
    pub fn finalize(&mut self) {
        self.finalized = true;
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

    pub fn finalized(&self) -> bool {
        self.finalized
    }
    pub fn description(&self, task: TaskId) -> &str {
        self.rows
            .iter()
            .find(|row| row.id == task)
            .map(|row| row.description.as_str())
            .or_else(|| {
                self.commands
                    .iter()
                    .find(|(id, _)| *id == task)
                    .map(|(_, display)| display.as_str())
            })
            .unwrap_or("command")
    }
    /// Source identity stays separate from text. Pending serial work does not
    /// create prefixes; mixed retained fragments remain attributable afterward.
    pub fn display_tail(&self) -> Vec<String> {
        let label = self.tail.labels.iter().any(|value| *value) || self.tail.needs_label;
        self.tail
            .lines
            .iter()
            .map(String::as_str)
            .chain((!self.tail.current.is_empty()).then_some(self.tail.current.as_str()))
            .zip(self.tail_sources())
            .zip(
                self.tail
                    .announcements
                    .iter()
                    .copied()
                    .chain((!self.tail.current.is_empty()).then_some(self.tail.announcement)),
            )
            .map(|((text, (task, _)), announcement)| {
                let description = self.description(task);
                let already_labelled = text
                    .split_once("> ")
                    .is_some_and(|(prefix, _)| !prefix.is_empty() && description.ends_with(prefix));
                if label && !announcement && !already_labelled {
                    format!("{description}> {text}")
                } else {
                    text.to_owned()
                }
            })
            .collect()
    }
    /// Required output and activity share incremental sanitization, but only
    /// activity enters the tail. Captures and native logs remain untouched.
    pub fn sanitize_output(&mut self, task: TaskId, stream: OutputStream, bytes: &[u8]) -> String {
        self.parsers
            .entry((task.0, stream == OutputStream::Stderr))
            .or_default()
            .feed(bytes)
    }
    pub fn output(&mut self, task: TaskId, stream: OutputStream, bytes: &[u8]) -> String {
        let text = self.sanitize_output(task, stream, bytes);
        let concurrent = self
            .rows
            .iter()
            .filter(|row| row.phase == TaskPhase::Executing)
            .count()
            > 1;
        self.tail.append(task, stream, &text, concurrent, false);
        text
    }

    /// Choose failures/incomplete work first, then restore registration order.
    pub fn visible_rows(&self, limit: usize) -> Vec<&TaskRow> {
        let mut selected: Vec<_> = self
            .rows
            .iter()
            .enumerate()
            .filter(|(_, row)| !self.was_snapshotted(row.id))
            .collect();
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
    sources: VecDeque<(TaskId, OutputStream)>,
    labels: VecDeque<bool>,
    announcements: VecDeque<bool>,
    needs_label: bool,
    announcement: bool,
    current: String,
    content: String,
    source: Option<(TaskId, OutputStream)>,
    rows: usize,
    line_bytes: usize,
}
impl Tail {
    fn remove_tasks(&mut self, tasks: &[TaskId]) {
        let mut keep = Self::new(self.rows, self.line_bytes);
        for (((line, source), label), announcement) in self
            .lines
            .drain(..)
            .zip(self.sources.drain(..))
            .zip(self.labels.drain(..))
            .zip(self.announcements.drain(..))
        {
            if !tasks.contains(&source.0) {
                keep.lines.push_back(line);
                keep.sources.push_back(source);
                keep.labels.push_back(label);
                keep.announcements.push_back(announcement);
            }
        }
        if !self.source.is_some_and(|(task, _)| tasks.contains(&task)) {
            keep.source = self.source;
            keep.current = std::mem::take(&mut self.current);
            keep.content = std::mem::take(&mut self.content);
            keep.needs_label = self.needs_label;
            keep.announcement = self.announcement;
        }
        *self = keep;
    }

    fn new(rows: usize, line_bytes: usize) -> Self {
        Self {
            lines: VecDeque::new(),
            sources: VecDeque::new(),
            labels: VecDeque::new(),
            announcements: VecDeque::new(),
            needs_label: false,
            announcement: false,
            current: String::new(),
            content: String::new(),
            source: None,
            rows,
            line_bytes,
        }
    }
    fn append(
        &mut self,
        task: TaskId,
        stream: OutputStream,
        text: &str,
        concurrent: bool,
        announcement: bool,
    ) {
        if text.is_empty() {
            return;
        }
        if self.source != Some((task, stream)) {
            // Keep interleaved partial lines distinct without changing output bytes.
            if !self.current.is_empty() {
                if !announcement
                    && !self.announcement
                    && self.source.is_some_and(|(id, _)| id != task)
                {
                    self.needs_label = true;
                }
                self.finish_line();
            }
            self.source = Some((task, stream));
        }
        self.announcement = announcement;
        self.needs_label |= concurrent;
        for part in text.split_inclusive(['\n', '\r']) {
            self.needs_label |= concurrent;
            let (value, ending) = part
                .strip_suffix(['\n', '\r'])
                .map_or((part, None), |value| {
                    (value, part.as_bytes().last().copied())
                });
            self.content.push_str(value);
            trim_front(&mut self.content, self.line_bytes);
            self.current.clone_from(&self.content);
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
            self.labels.push_back(self.needs_label);
            self.announcements.push_back(self.announcement);
        } else {
            self.current.clear();
        }
        self.content.clear();
        self.needs_label = false;
        while self.lines.len() > self.rows {
            self.lines.pop_front();
            self.sources.pop_front();
            self.labels.pop_front();
            self.announcements.pop_front();
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
    fn checkpoint_retires_only_named_terminal_rows_and_preserves_foreign_fragments() {
        let mut model = Model::new(12, 100);
        for id in [1, 2] {
            model.apply(&start(id)).unwrap();
        }
        model.output(
            TaskId(1),
            OutputStream::Stderr,
            b"completed native activity\n",
        );
        model.output(TaskId(2), OutputStream::Stderr, b"still running fragment");
        assert!(model.checkpoint_rows(&[TaskId(1)]).is_err());
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Succeeded,
            })
            .unwrap();
        let before = model.current_tail_line().to_owned();
        let selected = model.checkpoint_rows(&[TaskId(1), TaskId(1)]).unwrap();
        assert_eq!(selected.len(), 1);
        assert!(
            model
                .display_tail()
                .iter()
                .any(|line| line.contains("completed native activity"))
        );
        assert!(model.has_active_tasks());
        model.mark_checkpoint(&[TaskId(1)]);
        assert_eq!(model.current_tail_line(), before);
        assert_eq!(model.rows()[1].phase, TaskPhase::Executing);
        assert!(!model.finalized());
        assert_eq!(
            model
                .visible_rows(10)
                .iter()
                .map(|row| row.id)
                .collect::<Vec<_>>(),
            vec![TaskId(2)]
        );
        assert!(model.checkpoint_rows(&[TaskId(1)]).unwrap().is_empty());
        assert!(
            !model
                .display_tail()
                .iter()
                .any(|line| line.contains("completed native activity"))
        );
        model.output(TaskId(2), OutputStream::Stderr, b" completed\n");
        assert!(
            model
                .display_tail()
                .iter()
                .any(|line| line.contains("still running fragment completed"))
        );
        assert!(matches!(
            model.rows()[0].phase,
            TaskPhase::Final {
                state: Some(TaskState::Succeeded),
                ..
            }
        ));
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
        assert_eq!(model.current_tail_line(), "xxxxxxxxxxxxxxxx");
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
            ["node-a ", "failed", "warn",]
        );
        assert_eq!(model.current_tail_line(), "continued");
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
        assert_eq!(model.current_tail_line(), "continued");
        assert_eq!(
            model.output(TaskId(1), OutputStream::Stdout, b"\rreplacement\n"),
            "\rreplacement\n"
        );
        assert_eq!(model.tail().len(), 3);
        assert_eq!(model.tail().back().map(String::as_str), Some("replacement"));
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
    fn startup_announcements_and_completed_serial_groups_do_not_force_prefixes() {
        let mut model = Model::new(12, 128);
        model
            .apply(&UiEvent::CommandStarting {
                command: crate::events::CommandId(10001),
                task: TaskId(0),
                display: "nix eval --json settings".into(),
            })
            .unwrap();
        model.apply(&start(1)).unwrap();
        model.apply(&pending(2)).unwrap();
        model
            .apply(&UiEvent::CommandStarting {
                command: crate::events::CommandId(10002),
                task: TaskId(1),
                display: "nix build group-one".into(),
            })
            .unwrap();
        model.output(TaskId(1), OutputStream::Stderr, b"first build\n");
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Succeeded,
            })
            .unwrap();
        model.apply(&start(2)).unwrap();
        model
            .apply(&UiEvent::CommandStarting {
                command: crate::events::CommandId(10003),
                task: TaskId(2),
                display: "nix build group-two".into(),
            })
            .unwrap();
        model.output(TaskId(2), OutputStream::Stderr, b"second build\n");
        assert_eq!(
            model.display_tail(),
            [
                "$ nix eval --json settings",
                "$ nix build group-one",
                "first build",
                "$ nix build group-two",
                "second build"
            ]
        );
    }
    #[test]
    fn serial_pending_tasks_do_not_prefix_activity_but_mixed_sources_do() {
        let mut model = Model::new(4, 80);
        model.apply(&start(1)).unwrap();
        model.apply(&pending(2)).unwrap();
        model.output(TaskId(1), OutputStream::Stderr, b"first fragment");
        assert_eq!(model.display_tail(), ["first fragment"]);
        model.apply(&start(2)).unwrap();
        model.output(TaskId(2), OutputStream::Stderr, b"second fragment");
        assert_eq!(
            model.display_tail(),
            ["task1> first fragment", "task2> second fragment"]
        );
        assert_eq!(model.rows()[0].id, TaskId(1));
    }
    #[test]
    fn command_only_context_never_registers_work_or_loses_native_log_identity() {
        let mut model = Model::new(4, 80);
        model
            .apply(&UiEvent::CommandStarting {
                command: crate::events::CommandId(10004),
                task: TaskId(0),
                display: "nix eval [arguments redacted]".into(),
            })
            .unwrap();
        model.output(TaskId(0), OutputStream::Stderr, b"notice\n");
        assert!(model.rows().is_empty());
        assert_eq!(
            model.display_tail(),
            ["$ nix eval [arguments redacted]", "notice"]
        );
        model.finalize();
        assert!(model.rows().is_empty());
    }
    #[test]
    fn narrow_clipping_keeps_graphemes_and_display_width() {
        assert_eq!(clip("e\u{301}界tail", 2), "e\u{301}");
        assert_eq!(clip("e\u{301}界tail", 3), "e\u{301}界");
        assert_eq!(clip("界", 1), "");
        assert_eq!(clip("anything", 0), "");
    }
}
